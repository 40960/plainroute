// SPDX-License-Identifier: AGPL-3.0-only

package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/40960/plainroute/internal/config"
	"github.com/40960/plainroute/internal/metrics"
	"github.com/40960/plainroute/internal/routing"
)

var errResponseLimit = errors.New("response limit reached")

// outcome describes one upstream attempt.
type outcome struct {
	// delivered means the response was already written to the client, so no
	// further attempt is possible.
	delivered bool
	// kind labels the failure: status, transport, timeout, stream_idle,
	// client_gone, response_limit. An empty kind means the attempt succeeded.
	kind string
	// fallbackable means the attempt failed before any byte reached the client,
	// so a configured model fallback could still be tried.
	fallbackable bool
}

// attempt performs exactly one upstream request. This function is the reason the
// amplification invariant holds: it is called at most once per client request unless a
// fallback target is explicitly configured. A client that leaves during an attempt can still
// leave an upstream attempt counted without a completed client request, which is why the
// bound is checked against the requests the caller issued, not against requests_total.
func (h *Handler) attempt(
	w http.ResponseWriter,
	r *http.Request,
	g *generation,
	pool *routing.Pool,
	t *routing.Target,
	body []byte,
	peek *peeked,
	id string,
	start time.Time,
	allowFallback bool,
) outcome {
	// The generation this request started with, kept for the whole attempt.
	cfg := g.cfg

	defer pool.Release(t)

	// Being selectable means this target is not cooling down. The cooldown gauge is
	// refreshed from the pools at every scrape, so it does not need a latch here.
	h.metrics.ActiveRequests.WithLabelValues(pool.Route, t.Name).Inc()
	defer h.metrics.ActiveRequests.WithLabelValues(pool.Route, t.Name).Dec()

	bodyToSend := peek.withModel(body, t.Model)

	ctx := r.Context()
	if cfg.Timeouts.Request > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.Timeouts.Request.Std())
		defer cancel()
	}

	// The idle watchdog cancels this context when the upstream stops sending.
	// Cancelling the request context is what makes the transport abort the
	// in-flight response body read.
	//
	// The flag matters: a cancelled context can mean the watchdog fired, or that the
	// client went away and the parent was cancelled. Without recording which one, an
	// idle timeout and a client disconnect look identical in the logs and metrics.
	idleCtx, cancelIdle := context.WithCancel(ctx)
	defer cancelIdle()
	idleFired := &atomic.Bool{}
	idleTimer := time.AfterFunc(cfg.Timeouts.StreamIdle.Std(), func() {
		idleFired.Store(true)
		cancelIdle()
	})
	defer idleTimer.Stop()

	req, err := http.NewRequestWithContext(idleCtx, http.MethodPost, t.URL(), bytes.NewReader(bodyToSend))
	if err != nil {
		h.log.Error("build_upstream_request", "request_id", id, "route", pool.Route,
			"target", t.Name, "error", err)
		h.metrics.UpstreamErrors.WithLabelValues(pool.Route, t.Name, "build_request").Inc()
		if allowFallback {
			return outcome{kind: "transport", fallbackable: true}
		}
		h.reject(w, r, http.StatusInternalServerError, "internal_error", "internal_error",
			"could not build the upstream request", pool.Route, id, true)
		return outcome{delivered: true, kind: "transport"}
	}
	copyRequestHeaders(req.Header, r.Header, cfg, t, id)

	sendStart := h.now()
	res, err := g.client.Do(req)
	if err != nil {
		// The transport was reached, so this is an attempt even when no status came back.
		h.metrics.UpstreamAttempts.WithLabelValues(pool.Route, t.Name, "other").Inc()
		return h.reportTransportFailure(w, r, idleCtx, idleFired, cfg, pool, t, id, allowFallback, err)
	}
	defer func() {
		// A Close error after a clean read means the connection ended abnormally, which
		// is worth recording rather than throwing away.
		if cerr := res.Body.Close(); cerr != nil && !errors.Is(cerr, io.EOF) {
			h.metrics.StreamAborts.WithLabelValues(pool.Route, t.Name, "close_error").Inc()
			h.log.Warn("upstream_body_close_failed", "request_id", id, "route", pool.Route,
				"target", t.Name, "error", cerr)
		}
	}()

	stream := isEventStream(res.Header.Get("Content-Type"))

	if res.StatusCode >= 400 {
		h.metrics.UpstreamAttempts.WithLabelValues(pool.Route, t.Name, metrics.StatusClass(res.StatusCode)).Inc()
		def := cfg.Routing.Cooldown.Status[res.StatusCode].Std()
		ra := parseRetryAfter(res.Header.Get("Retry-After"), def, cfg.Routing.Cooldown.MaxStd())
		if ra > 0 {
			h.applyCooldown(cfg, pool, t, ra, http.StatusText(res.StatusCode), id)
		}
		if allowFallback {
			return outcome{kind: "status", fallbackable: true}
		}

		// The provider error body is forwarded as the upstream produced it. The only value
		// PlainRoute changes here is Retry-After: the gateway reports the window it actually
		// applied, which is the value it received when the clamp is off, and the clamp when the
		// received value exceeded it.
		copyResponseHeaders(w.Header(), res.Header)
		w.Header().Set("X-Request-Id", id)
		if ra > 0 {
			w.Header().Set("Retry-After", retryAfterSeconds(ra))
		}
		w.WriteHeader(res.StatusCode)
		written, ttft := h.copyBody(w, r, idleCtx, idleFired, cfg, pool, t, id, res.Body, stream, sendStart)
		h.metrics.Observe(pool.Route, t.Name, res.StatusCode, stream, h.now().Sub(start), ttft, written)
		h.log.Warn("upstream_error_status", "request_id", id, "route", pool.Route,
			"target", t.Name, "status", res.StatusCode,
			"ttft_ms", durationMs(ttft), "bytes", written)
		return outcome{delivered: true, kind: "status"}
	}

	h.metrics.UpstreamAttempts.WithLabelValues(pool.Route, t.Name, metrics.StatusClass(res.StatusCode)).Inc()

	buf := make([]byte, 32*1024)

	// One loop handles the first chunk and the rest. A read that returns bytes and an
	// error writes those bytes before the error is handled, so a transport error that
	// arrives with data is never swallowed. Headers are committed only after the first
	// byte, which keeps an immediate failure reportable as an error and lets a configured
	// fallback still run before anything is written.
	var written int64
	var ttft time.Duration
	first := true
	for {
		idleTimer.Reset(cfg.Timeouts.StreamIdle.Std())
		n, err := readBudget(res.Body, buf, cfg.Server.MaxResponseBytes-written)
		if first && n > 0 {
			ttft = h.now().Sub(sendStart)
			copyResponseHeaders(w.Header(), res.Header)
			w.Header().Set("X-Request-Id", id)
			w.WriteHeader(res.StatusCode)
			first = false
		}
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				h.noteClientGone(pool, t, id, written, werr)
				return outcome{delivered: true, kind: "client_gone"}
			}
			written += int64(n)
			h.flush(w, stream)
		}
		if errors.Is(err, errResponseLimit) {
			h.noteResponseLimit(pool, t, id, written)
			return outcome{delivered: true, kind: "response_limit"}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			kind := h.noteStreamError(r, idleCtx, idleFired, cfg, pool, t, id, err, written)
			if first {
				if kind == "client_gone" {
					return outcome{delivered: true, kind: kind}
				}
				if allowFallback {
					return outcome{kind: kind, fallbackable: true}
				}
				h.reject(w, r, http.StatusBadGateway, "upstream_empty_response", "upstream_error",
					"upstream closed before sending a response body", pool.Route, id, false)
				return outcome{delivered: true, kind: kind}
			}
			return outcome{delivered: true, kind: kind}
		}
	}
	if first {
		// A clean empty response still needs a status line and the gateway request id.
		copyResponseHeaders(w.Header(), res.Header)
		w.Header().Set("X-Request-Id", id)
		w.WriteHeader(res.StatusCode)
		ttft = h.now().Sub(sendStart)
	}

	duration := h.now().Sub(start)
	h.metrics.Observe(pool.Route, t.Name, res.StatusCode, stream, duration, ttft, written)
	h.log.Info("request_completed",
		"request_id", id,
		"route", pool.Route,
		"model", t.Model,
		"target", t.Name,
		"status", res.StatusCode,
		"stream", stream,
		"ttft_ms", durationMs(ttft),
		"duration_ms", durationMs(duration),
		"bytes", written,
		"active", t.Active(),
	)
	return outcome{delivered: true, kind: ""}
}

func (h *Handler) reportTransportFailure(w http.ResponseWriter, r *http.Request, idleCtx context.Context,
	idleFired *atomic.Bool, cfg *config.Config, pool *routing.Pool, t *routing.Target,
	id string, allowFallback bool, err error) outcome {

	// The watchdog first: a cancellation it caused is a stream that went silent, not
	// a client that went away. Checking the client first would report an idle timeout
	// as a disconnect, because a client disconnect also cancels the derived context.
	if idleFired.Load() {
		h.log.Warn("stream_idle", "request_id", id, "route", pool.Route,
			"target", t.Name, "error", err)
		h.metrics.UpstreamErrors.WithLabelValues(pool.Route, t.Name, "stream_idle").Inc()
		h.metrics.StreamAborts.WithLabelValues(pool.Route, t.Name, "stream_idle").Inc()
		if dur := cfg.Routing.Cooldown.Timeout.Std(); dur > 0 {
			h.applyCooldown(cfg, pool, t, dur, "stream_idle", id)
		}
		if allowFallback {
			return outcome{kind: "stream_idle", fallbackable: true}
		}
		h.reject(w, r, http.StatusGatewayTimeout, "stream_idle", "upstream_error",
			"upstream stopped sending data", pool.Route, id, false)
		return outcome{delivered: true, kind: "stream_idle"}
	}

	kind := "transport"
	if errors.Is(err, context.Canceled) || r.Context().Err() != nil {
		h.log.Warn("client_disconnect", "request_id", id, "route", pool.Route,
			"target", t.Name, "error", err)
		h.metrics.ClientDisconnects.WithLabelValues(pool.Route, t.Name).Inc()
		return outcome{delivered: true, kind: "client_gone"}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		kind = "timeout"
	}

	h.log.Warn("upstream_request_failed", "request_id", id, "route", pool.Route,
		"target", t.Name, "kind", kind, "error", err)
	h.metrics.UpstreamErrors.WithLabelValues(pool.Route, t.Name, kind).Inc()

	if dur := cfg.Routing.Cooldown.Timeout.Std(); dur > 0 {
		h.applyCooldown(cfg, pool, t, dur, kind, id)
	}

	if allowFallback {
		return outcome{kind: kind, fallbackable: true}
	}

	status := http.StatusBadGateway
	if kind == "timeout" {
		status = http.StatusGatewayTimeout
	}
	h.reject(w, r, status, "upstream_error", "upstream_error", "upstream request failed", pool.Route, id, false)
	return outcome{delivered: true, kind: kind}
}

func classifyStreamError(r *http.Request, idleCtx context.Context, idleFired *atomic.Bool, err error) string {
	if idleFired.Load() {
		return "stream_idle"
	}
	if idleCtx.Err() != nil || r.Context().Err() != nil || errors.Is(err, context.Canceled) {
		return "client_gone"
	}
	return "transport"
}

// flush writes when the response is a stream. PlainRoute flushes per read chunk,
// which keeps frames in order without re-forming them.
func (h *Handler) flush(w http.ResponseWriter, stream bool) {
	if !stream {
		return
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (h *Handler) applyCooldown(cfg *config.Config, pool *routing.Pool, t *routing.Target,
	dur time.Duration, reason, id string) {
	if dur <= 0 {
		return
	}
	pool.Cooldown(t, dur, reason)
	h.metrics.CooldownEvents.WithLabelValues(pool.Route, t.Name, reason).Inc()
	h.metrics.CooldownActive.WithLabelValues(pool.Route, t.Name).Set(1)
	h.log.Info("cooldown", "request_id", id, "route", pool.Route, "target", t.Name,
		"reason", reason, "seconds", int64(dur/time.Second), "active", t.Active())
}

// noteClientGone is shared by the success and error-body paths: a client that left
// is the same event whichever status the upstream answered with.
func (h *Handler) noteClientGone(pool *routing.Pool, t *routing.Target, id string, written int64, err error) {
	h.metrics.ClientDisconnects.WithLabelValues(pool.Route, t.Name).Inc()
	h.log.Warn("client_disconnect", "request_id", id, "route", pool.Route,
		"target", t.Name, "bytes", written, "error", err)
}

// noteResponseLimit is shared by both copy paths for the same reason.
func (h *Handler) noteResponseLimit(pool *routing.Pool, t *routing.Target, id string, written int64) {
	h.metrics.StreamAborts.WithLabelValues(pool.Route, t.Name, "response_limit").Inc()
	h.log.Warn("response_limit_reached", "request_id", id, "route", pool.Route,
		"target", t.Name, "bytes", written)
}

// noteStreamError classifies an upstream read failure, records it once for both copy
// paths, and returns the kind so the caller can decide about fallback.
func (h *Handler) noteStreamError(r *http.Request, idleCtx context.Context, idleFired *atomic.Bool,
	cfg *config.Config, pool *routing.Pool, t *routing.Target, id string, err error, written int64) string {
	kind := classifyStreamError(r, idleCtx, idleFired, err)
	h.metrics.UpstreamErrors.WithLabelValues(pool.Route, t.Name, kind).Inc()
	if kind == "stream_idle" {
		h.metrics.StreamAborts.WithLabelValues(pool.Route, t.Name, kind).Inc()
		if dur := cfg.Routing.Cooldown.Timeout.Std(); dur > 0 {
			h.applyCooldown(cfg, pool, t, dur, kind, id)
		}
	}
	if kind == "client_gone" {
		h.metrics.ClientDisconnects.WithLabelValues(pool.Route, t.Name).Inc()
	}
	h.log.Warn("stream_ended_early", "request_id", id, "route", pool.Route,
		"target", t.Name, "kind", kind, "bytes", written, "error", err)
	return kind
}

// copyBody copies a non-2xx response with the same rules as the success path: no
// buffering, byte-preserving, flush per read, and the same disconnect, limit and
// stream-error instrumentation. TTFT is taken at the first byte, not after the body.
func (h *Handler) copyBody(w http.ResponseWriter, r *http.Request, idleCtx context.Context, idleFired *atomic.Bool,
	cfg *config.Config, pool *routing.Pool, t *routing.Target, id string, body io.Reader,
	stream bool, sendStart time.Time) (int64, time.Duration) {
	buf := make([]byte, 32*1024)
	var written int64
	var ttft time.Duration
	for {
		n, err := readBudget(body, buf, cfg.Server.MaxResponseBytes-written)
		if n > 0 {
			if ttft == 0 {
				ttft = h.now().Sub(sendStart)
			}
			if _, werr := w.Write(buf[:n]); werr != nil {
				h.noteClientGone(pool, t, id, written, werr)
				return written, ttft
			}
			written += int64(n)
			h.flush(w, stream)
		}
		if errors.Is(err, errResponseLimit) {
			h.noteResponseLimit(pool, t, id, written)
			return written, ttft
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				h.noteStreamError(r, idleCtx, idleFired, cfg, pool, t, id, err, written)
			}
			return written, ttft
		}
	}
}

// readBudget reads at most the remaining budget into buf. At a zero budget it
// probes one byte without putting it in buf: over-limit is reported only when the
// upstream actually has another byte, so a response that ends exactly at the cap
// is complete and a cap+1 response is refused without sending that extra byte.
func readBudget(reader io.Reader, buf []byte, remaining int64) (int, error) {
	if remaining > 0 {
		n := int64(len(buf))
		if remaining < n {
			n = remaining
		}
		return reader.Read(buf[:n])
	}
	var probe [1]byte
	n, err := reader.Read(probe[:])
	if n > 0 {
		return 0, errResponseLimit
	}
	if err != nil {
		return 0, err
	}
	return 0, io.ErrNoProgress
}
