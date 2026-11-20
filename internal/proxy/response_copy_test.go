// SPDX-License-Identifier: AGPL-3.0-only

package proxy

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/40960/plainroute/internal/fakeupstream"
	"github.com/40960/plainroute/internal/routing"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

const chatConfig = `
server:
  max_response_bytes: 200
timeouts:
  stream_idle: 10s
routes:
  m:
    targets:
      - name: a
        model: m
        base_url: https://fake.invalid/v1
        api_key: key-a
`

// bytesAndError is a legal io.Reader: it returns data and a transport error in the
// same call. The old success loop wrote the bytes and then continued, losing the error.
type bytesAndError struct {
	data []byte
	err  error
	done bool
}

func (r *bytesAndError) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	return copy(p, r.data), r.err
}

type stubTransport struct {
	body   io.ReadCloser
	err    error
	status int
}

func (t stubTransport) RoundTrip(*http.Request) (*http.Response, error) {
	if t.err != nil {
		return nil, t.err
	}
	status := t.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"text/plain"}},
		Body:       t.body,
	}, nil
}

func TestCapPlusOneIsRefusedWithoutLeakingTheExtraByte(t *testing.T) {
	h := newHarness(t, chatConfig)
	body := strings.Repeat("x", 201) // the cap is 200 in chatConfig
	h.fake.Set("key-a", fakeupstream.Behavior{Status: http.StatusOK, Body: body})

	resp, got := h.post(chatBody("m"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d", resp.StatusCode)
	}
	if len(got) != 200 {
		t.Errorf("client received %d bytes, want exactly the 200-byte cap", len(got))
	}
	if !strings.Contains(h.log.String(), "response_limit") {
		t.Errorf("over-limit response was not recorded: %s", h.log.String())
	}
	if n := testutil.ToFloat64(h.metrics.StreamAborts.WithLabelValues("m", "a", "response_limit")); n != 1 {
		t.Errorf("stream_aborts_total: got %v, want 1", n)
	}
}

func TestReadErrorArrivingWithBytesIsRecorded(t *testing.T) {
	h := newHarness(t, chatConfig)
	h.handler.Generation().client = &http.Client{Transport: stubTransport{
		body: io.NopCloser(&bytesAndError{data: []byte("partial"), err: io.ErrUnexpectedEOF}),
	}}

	_, got := h.post(chatBody("m"))
	if got != "partial" {
		t.Errorf("bytes: got %q, want the data returned with the error", got)
	}
	if n := testutil.ToFloat64(h.metrics.UpstreamErrors.WithLabelValues("m", "a", "transport")); n != 1 {
		t.Errorf("upstream_errors_total: got %v, want 1 for the swallowed error", n)
	}
	if !strings.Contains(h.log.String(), "stream_ended_early") {
		t.Errorf("the read error was not logged: %s", h.log.String())
	}
}

func TestNon2xxClientWriteErrorIsCounted(t *testing.T) {
	h := newHarness(t, chatConfig)
	w := &failingWriter{header: http.Header{}, err: errors.New("client left")}
	req, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	h.handler.copyBody(w, req, req.Context(), &atomic.Bool{}, h.handler.Generation().cfg,
		h.pool("m"), h.target("m", "a"), "id", strings.NewReader("some error body"), false, time.Now())
	if n := testutil.ToFloat64(h.metrics.ClientDisconnects.WithLabelValues("m", "a")); n != 1 {
		t.Errorf("client_disconnects_total: got %v, want 1 for a non-2xx write failure", n)
	}
}

func TestNon2xxReadErrorIsCounted(t *testing.T) {
	h := newHarness(t, chatConfig)
	req, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	h.handler.copyBody(httptestWriter(), req, req.Context(), &atomic.Bool{}, h.handler.Generation().cfg,
		h.pool("m"), h.target("m", "a"), "id",
		io.NopCloser(&bytesAndError{data: []byte("partial"), err: io.ErrUnexpectedEOF}), false, time.Now())
	if n := testutil.ToFloat64(h.metrics.UpstreamErrors.WithLabelValues("m", "a", "transport")); n != 1 {
		t.Errorf("upstream_errors_total: got %v, want 1 for a non-2xx read failure", n)
	}
}

func TestTransportFailureCountsAsAnUpstreamAttempt(t *testing.T) {
	h := newHarness(t, chatConfig)
	h.handler.Generation().client = &http.Client{Transport: stubTransport{err: errors.New("connection refused")}}

	resp, _ := h.post(chatBody("m"))
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status: got %d, want 502", resp.StatusCode)
	}
	if n := testutil.ToFloat64(h.metrics.UpstreamAttempts.WithLabelValues("m", "a", "other")); n != 1 {
		t.Errorf("upstream_attempts_total{other}: got %v, want 1 for a transport failure", n)
	}
}

func TestCooldownGaugeReflectsCurrentStateAtScrape(t *testing.T) {
	h := newHarness(t, chatConfig)
	pool := h.pool("m")
	target := h.target("m", "a")
	g := h.handler.Generation()
	h.handler.applyCooldown(g.cfg, pool, target, 10*time.Second, "rate_limit", "id")

	_, page := h.get("/metrics")
	if !strings.Contains(page, "plainroute_target_in_cooldown{route=\"m\",target=\"a\"} 1") {
		t.Errorf("cooling target is not 1 on the metrics page:\n%s", page)
	}

	h.clock.Advance(11 * time.Second)
	_, page = h.get("/metrics")
	if !strings.Contains(page, "plainroute_target_in_cooldown{route=\"m\",target=\"a\"} 0") {
		t.Errorf("expired cooldown still reads 1 after time moved:\n%s", page)
	}
}

func httptestWriter() http.ResponseWriter {
	return &plainWriter{header: http.Header{}, status: http.StatusOK}
}

type plainWriter struct {
	header http.Header
	status int
}

func (w *plainWriter) Header() http.Header         { return w.header }
func (w *plainWriter) WriteHeader(status int)      { w.status = status }
func (w *plainWriter) Write(p []byte) (int, error) { return len(p), nil }

func (h *harness) pool(route string) *routing.Pool {
	p, ok := h.handler.Generation().registry.Lookup(route)
	if !ok {
		h.t.Fatalf("route %q is not in the registry", route)
	}
	return p
}

func (h *harness) target(route, name string) *routing.Target {
	for _, t := range h.pool(route).Targets {
		if t.Name == name {
			return t
		}
	}
	h.t.Fatalf("target %q/%q not found", route, name)
	return nil
}
