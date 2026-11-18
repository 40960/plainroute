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
// amplification invariant holds: it is called at most once per client request
// unless a fallback target is explicitly configured.
func (h *Handler) attempt(
	w http.ResponseWriter,
	r *http.Request,
	cfg *config.Config,
	pool *routing.Pool,
	t *routing.Target,
	body []byte,
	peek *peeked,
	id string,
	start time.Time,
	allowFallback bool,
) outcome {
	defer pool.Release(t)

	// Being selectable means this target is not cooling down, so clear its
	// cooldown gauge.
	h.metrics.CooldownActive.WithLabelValues(pool.Model, t.Name).Set(0)
	h.metrics.ActiveRequests.WithLabelValues(pool.Model, t.Name).Inc()
	defer h.metrics.ActiveRequests.WithLabelValues(pool.Model, t.Name).Dec()

	bodyToSend := peek.withModel(body, t.UpstreamModel)

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
		h.log.Error("build_upstream_request", "request_id", id, "model", pool.Model,
			"target", t.Name, "error", err)
		h.metrics.UpstreamErrors.WithLabelValues(pool.Model, t.Name, "build_request").Inc()
		if allowFallback {
			return outcome{kind: "transport", fallbackable: true}
		}
		h.reject(w, r, http.StatusInternalServerError, "internal_error", "internal_error",
			"could not build the upstream request", pool.Model, id)
		return outcome{delivered: true, kind: "transport"}
	}
	copyRequestHeaders(req.Header, r.Header, cfg, t, id)

	sendStart := h.now()
	res, err := h.client.Do(req)
	if err != nil {
		return h.reportTransportFailure(w, r, idleCtx, idleFired, cfg, pool, t, id, allowFallback, err)
	}
	defer func() {
		// A Close error after a clean read means the connection ended abnormally, which
		// is worth recording rather than throwing away.
		if cerr := res.Body.Close(); cerr != nil && !errors.Is(cerr, io.EOF) {
			h.metrics.StreamAborts.WithLabelValues(pool.Model, t.Name, "close_error").Inc()
			h.log.Warn("upstream_body_close_failed", "request_id", id, "model", pool.Model,
				"target", t.Name, "error", cerr)
		}
	}()

	stream := isEventStream(res.Header.Get("Content-Type"))

	if res.StatusCode >= 400 {
		h.metrics.UpstreamAttempts.WithLabelValues(pool.Model, t.Name, metrics.StatusClass(res.StatusCode)).Inc()
		def := cfg.Routing.Cooldown.Status[res.StatusCode].Std()
		ra := parseRetryAfter(res.Header.Get("Retry-After"), def, cfg.Routing.Cooldown.Max.Std())
		if ra > 0 {
			h.applyCooldown(cfg, pool, t, ra, http.StatusText(res.StatusCode), id)
		}
		h.log.Warn("upstream_error_status", "request_id", id, "model", pool.Model,
			"target", t.Name, "status", res.StatusCode,
			"ttft_ms", durationMs(h.now().Sub(sendStart)))

		if allowFallback {
			return outcome{kind: "status", fallbackable: true}
		}

		// The provider error body is forwarded as the upstream produced it. The
		// only value PlainRoute changes here is Retry-After: the gateway reports
		// the window it actually applied, so a hostile or broken upstream value
		// cannot be pushed on to the client.
		w.Header().Set("X-Request-Id", id)
		copyResponseHeaders(w.Header(), res.Header)
		if ra > 0 {
			w.Header().Set("Retry-After", retryAfterSeconds(ra))
		}
		w.WriteHeader(res.StatusCode)
		written := h.copyBody(w, res.Body, cfg, stream)
		h.metrics.Observe(pool.Model, t.Name, res.StatusCode, stream, h.now().Sub(start), h.now().Sub(sendStart), written)
		return outcome{delivered: true, kind: "status"}
	}

	h.metrics.UpstreamAttempts.WithLabelValues(pool.Model, t.Name, metrics.StatusClass(res.StatusCode)).Inc()

	buf := make([]byte, 32*1024)

	// Read the first chunk before committing, so a stream that fails immediately is
	// reported as an error instead of an empty successful response. The read is
	// clamped to the response cap, so one large chunk cannot overshoot it.
	n, err := readUpTo(res.Body, buf, cfg.Server.MaxResponseBytes)
	if n == 0 && err != nil && !errors.Is(err, io.EOF) {
		kind := classifyStreamError(r, idleCtx, idleFired, err)
		h.metrics.UpstreamErrors.WithLabelValues(pool.Model, t.Name, kind).Inc()
		if kind == "stream_idle" {
			h.metrics.StreamAborts.WithLabelValues(pool.Model, t.Name, kind).Inc()
		}
		if kind == "client_gone" {
			h.metrics.ClientDisconnects.WithLabelValues(pool.Model, t.Name).Inc()
			return outcome{delivered: true, kind: kind}
		}
		if dur := cfg.Routing.Cooldown.Timeout.Std(); dur > 0 {
			h.applyCooldown(cfg, pool, t, dur, kind, id)
		}
		if allowFallback {
			return outcome{kind: kind, fallbackable: true}
		}
		h.reject(w, r, http.StatusBadGateway, "upstream_empty_response", "upstream_error",
			"upstream closed before sending a response body", pool.Model, id)
		return outcome{delivered: true, kind: kind}
	}

	ttft := h.now().Sub(sendStart)

	w.Header().Set("X-Request-Id", id)
	copyResponseHeaders(w.Header(), res.Header)
	w.WriteHeader(res.StatusCode)

	var written int64
	if n > 0 {
		if _, werr := w.Write(buf[:n]); werr != nil {
			h.metrics.ClientDisconnects.WithLabelValues(pool.Model, t.Name).Inc()
			h.log.Warn("client_disconnect", "request_id", id, "model", pool.Model,
				"target", t.Name, "bytes", written, "error", werr)
			return outcome{delivered: true, kind: "client_gone"}
		}
		written += int64(n)
		h.flush(w, stream)
	}

	for {
		if written >= cfg.Server.MaxResponseBytes {
			h.metrics.StreamAborts.WithLabelValues(pool.Model, t.Name, "response_limit").Inc()
			h.log.Warn("response_limit_reached", "request_id", id, "model", pool.Model,
				"target", t.Name, "bytes", written)
			return outcome{delivered: true, kind: "response_limit"}
		}

		idleTimer.Reset(cfg.Timeouts.StreamIdle.Std())

		n, err := readUpTo(res.Body, buf, cfg.Server.MaxResponseBytes-written)
		if errors.Is(err, errResponseLimit) {
			h.metrics.StreamAborts.WithLabelValues(pool.Model, t.Name, "response_limit").Inc()
			h.log.Warn("response_limit_reached", "request_id", id, "model", pool.Model,
				"target", t.Name, "bytes", written)
			return outcome{delivered: true, kind: "response_limit"}
		}
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				h.metrics.ClientDisconnects.WithLabelValues(pool.Model, t.Name).Inc()
				h.log.Warn("client_disconnect", "request_id", id, "model", pool.Model,
					"target", t.Name, "bytes", written, "error", werr)
				return outcome{delivered: true, kind: "client_gone"}
			}
			written += int64(n)
			h.flush(w, stream)
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			kind := classifyStreamError(r, idleCtx, idleFired, err)
			h.metrics.UpstreamErrors.WithLabelValues(pool.Model, t.Name, kind).Inc()
			if kind == "stream_idle" {
				h.metrics.StreamAborts.WithLabelValues(pool.Model, t.Name, kind).Inc()
				if dur := cfg.Routing.Cooldown.Timeout.Std(); dur > 0 {
					h.applyCooldown(cfg, pool, t, dur, kind, id)
				}
			}
			if kind == "client_gone" {
				h.metrics.ClientDisconnects.WithLabelValues(pool.Model, t.Name).Inc()
			}
			h.log.Warn("stream_ended_early", "request_id", id, "model", pool.Model,
				"target", t.Name, "kind", kind, "bytes", written, "error", err)
			return outcome{delivered: true, kind: kind}
		}
	}

	duration := h.now().Sub(start)
	h.metrics.Observe(pool.Model, t.Name, res.StatusCode, stream, duration, ttft, written)
	h.log.Info("request_completed",
		"request_id", id,
		"model", pool.Model,
		"upstream_model", t.UpstreamModel,
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
		h.log.Warn("stream_idle", "request_id", id, "model", pool.Model,
			"target", t.Name, "error", err)
		h.metrics.UpstreamErrors.WithLabelValues(pool.Model, t.Name, "stream_idle").Inc()
		h.metrics.StreamAborts.WithLabelValues(pool.Model, t.Name, "stream_idle").Inc()
		if dur := cfg.Routing.Cooldown.Timeout.Std(); dur > 0 {
			h.applyCooldown(cfg, pool, t, dur, "stream_idle", id)
		}
		if allowFallback {
			return outcome{kind: "stream_idle", fallbackable: true}
		}
		h.reject(w, r, http.StatusGatewayTimeout, "stream_idle", "upstream_error",
			"upstream stopped sending data", pool.Model, id)
		return outcome{delivered: true, kind: "stream_idle"}
	}

	kind := "transport"
	if errors.Is(err, context.Canceled) || r.Context().Err() != nil {
		h.log.Warn("client_disconnect", "request_id", id, "model", pool.Model,
			"target", t.Name, "error", err)
		h.metrics.ClientDisconnects.WithLabelValues(pool.Model, t.Name).Inc()
		return outcome{delivered: true, kind: "client_gone"}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		kind = "timeout"
	}

	h.log.Warn("upstream_request_failed", "request_id", id, "model", pool.Model,
		"target", t.Name, "kind", kind, "error", err)
	h.metrics.UpstreamErrors.WithLabelValues(pool.Model, t.Name, kind).Inc()

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
	h.reject(w, r, status, "upstream_error", "upstream_error", "upstream request failed", pool.Model, id)
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
	h.metrics.CooldownEvents.WithLabelValues(pool.Model, t.Name, reason).Inc()
	h.metrics.CooldownActive.WithLabelValues(pool.Model, t.Name).Set(1)
	h.log.Info("cooldown", "request_id", id, "model", pool.Model, "target", t.Name,
		"reason", reason, "seconds", int64(dur/time.Second), "active", t.Active())
}

// copyBody copies without buffering the whole response. PlainRoute never holds a
// model response in memory.
func (h *Handler) copyBody(w http.ResponseWriter, body io.Reader, cfg *config.Config, stream bool) int64 {
	buf := make([]byte, 32*1024)
	var written int64
	for {
		n, err := readUpTo(body, buf, cfg.Server.MaxResponseBytes-written)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return written
			}
			written += int64(n)
			h.flush(w, stream)
		}
		if err != nil {
			return written
		}
	}
}

// readUpTo reads at most the remaining budget, so the configured response cap cannot
// be overshot by one large read.
func readUpTo(reader io.Reader, buf []byte, remaining int64) (int, error) {
	n := int64(len(buf))
	if remaining < n {
		n = remaining
	}
	if n <= 0 {
		return 0, errResponseLimit
	}
	return reader.Read(buf[:n])
}
