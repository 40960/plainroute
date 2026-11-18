// SPDX-License-Identifier: AGPL-3.0-only

package proxy

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/40960/plainroute/internal/fakeupstream"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestFlushHappensPerChunk proves the gateway does not buffer a stream: the client
// must see the first frame before the upstream writes the second one. The timing is
// the proof, so the margins are wide enough not to fail on a loaded machine.
func TestFlushHappensPerChunk(t *testing.T) {
	h := newHarness(t, `
routes:
  m:
    targets:
      - name: a
        model: m
        base_url: https://fake.invalid/v1
        api_key: key-a
`)

	first := "data: {\"delta\":{\"role\":\"assistant\"}}\n\n"
	second := "data: {\"delta\":{\"content\":\"later\"}}\n\n"
	h.fake.Set("key-a", fakeupstream.Behavior{
		Status: 200,
		SSE:    []string{first, second},
		Delay:  600 * time.Millisecond,
	})

	resp, err := h.gateway.Client().Post(h.gateway.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m","stream":true,"messages":[]}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Errorf("content type: got %q", resp.Header.Get("Content-Type"))
	}

	reader := bufio.NewReader(resp.Body)
	start := time.Now()
	buf := make([]byte, len(first))
	if _, err := io.ReadFull(reader, buf); err != nil {
		t.Fatalf("first frame: %v", err)
	}
	arrival := time.Since(start)
	if string(buf) != first {
		t.Errorf("first frame: got %q, want %q", string(buf), first)
	}
	if arrival > 300*time.Millisecond {
		t.Errorf("the first frame arrived after %v, which means the gateway buffered it", arrival)
	}

	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("second frame: %v", err)
	}
	if string(rest) != second {
		t.Errorf("second frame: got %q, want %q", string(rest), second)
	}
}

// TestSlowStreamSurvives proves a long reasoning stream is not killed by an ordinary
// overall request timeout: the only streaming limit is the idle timeout.
func TestSlowStreamSurvives(t *testing.T) {
	h := newHarness(t, `
routes:
  m:
    targets:
      - name: a
        model: m
        base_url: https://fake.invalid/v1
        api_key: key-a
timeouts:
  connect: 1s
  response_header: 2s
  stream_idle: 2s
  request: 0s
`)

	// Six frames, 250ms apart: 1.5s of streaming, longer than the response header
	// timeout, so a short overall timeout would have killed it.
	frames := []string{
		"data: {\"delta\":{\"role\":\"assistant\"}}\n\n",
		"data: {\"delta\":{\"content\":\"thinking\"}}\n\n",
		"data: {\"delta\":{\"content\":\"about\"}}\n\n",
		"data: {\"delta\":{\"content\":\"a\"}}\n\n",
		"data: {\"delta\":{\"content\":\"long\"}}\n\n",
		"data: [DONE]\n\n",
	}
	h.fake.Set("key-a", fakeupstream.Behavior{Status: 200, Delay: 250 * time.Millisecond,
		SSE: frames, Gap: 250 * time.Millisecond})

	resp, err := h.gateway.Client().Post(h.gateway.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m","stream":true,"messages":[]}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d", resp.StatusCode)
	}
	if string(body) != strings.Join(frames, "") {
		t.Errorf("slow stream changed:\n got %q\nwant %q", string(body), strings.Join(frames, ""))
	}
	if !strings.Contains(h.log.String(), "request_completed") {
		t.Error("the completed stream was not logged")
	}
}

// TestStreamIdleTimeoutEndsTheRequest proves the idle watchdog cancels the upstream
// request when the stream goes silent in the middle. The gateway does not invent a
// frame to close the stream: the response simply ends with what it received.
func TestStreamIdleTimeoutEndsTheRequest(t *testing.T) {
	h := newHarness(t, `
routes:
  m:
    targets:
      - name: a
        model: m
        base_url: https://fake.invalid/v1
        api_key: key-a
timeouts:
  stream_idle: 200ms
routing:
  cooldown:
    status: {429: 10s}
    timeout: 1s
    max: 30s
`)

	h.fake.Set("key-a", fakeupstream.Behavior{Status: 200,
		SSE: []string{"data: {\"delta\":{\"content\":\"partial\"}}\n\n", "data: [DONE]\n\n"},
		Gap: 2 * time.Second,
	})

	resp, err := h.gateway.Client().Post(h.gateway.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m","stream":true,"messages":[]}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil && !strings.Contains(err.Error(), "connection") &&
		!strings.Contains(err.Error(), "unexpected") {
		t.Logf("read ended with: %v", err)
	}
	if !bytes.Contains(body, []byte("partial")) {
		t.Errorf("body: got %q, want the first frame only", string(body))
	}
	if bytes.Contains(body, []byte("[DONE]")) {
		t.Error("the gateway added a frame it did not receive")
	}
	if !strings.Contains(h.log.String(), "stream_idle") {
		t.Errorf("the idle timeout was not recorded in the log: %s", h.log.String())
	}
	if !strings.Contains(h.log.String(), "cooldown") {
		t.Error("the idle timeout did not cool the target down")
	}
}

// TestIdleBeforeFirstByteIsNotAClientDisconnect: a stream that never sends anything
// must be reported as an idle timeout, not as a client that went away, because the
// gateway itself cancelled the request.
func TestIdleBeforeFirstByteIsNotAClientDisconnect(t *testing.T) {
	h := newHarness(t, `
routes:
  m:
    targets:
      - name: a
        model: m
        base_url: https://fake.invalid/v1
        api_key: key-a
timeouts:
  stream_idle: 150ms
`)

	h.fake.Set("key-a", fakeupstream.Behavior{Status: 200, Delay: 3 * time.Second})

	resp, err := h.gateway.Client().Post(h.gateway.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m","stream":true,"messages":[]}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Errorf("status: got %d, want 504 (%s)", resp.StatusCode, string(body))
	}
	if !strings.Contains(h.log.String(), "stream_idle") {
		t.Errorf("log: got %s", h.log.String())
	}
	if strings.Contains(h.log.String(), "client_disconnect") {
		t.Errorf("an idle timeout was reported as a client disconnect: %s", h.log.String())
	}
}

// TestClientDisconnectCancelsUpstream checks that a client going away mid-stream
// cancels the upstream request and releases the active counter.
func TestClientDisconnectCancelsUpstream(t *testing.T) {
	h := newHarness(t, `
routes:
  m:
    targets:
      - name: a
        model: m
        base_url: https://fake.invalid/v1
        api_key: key-a
`)

	h.fake.Set("key-a", fakeupstream.Behavior{Status: 200,
		SSE:           []string{"data: {\"delta\":{\"content\":\"first\"}}\n\n"},
		WaitForCancel: true,
	})

	req, err := http.NewRequest(http.MethodPost, h.gateway.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true,"messages":[]}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := h.gateway.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}

	reader := bufio.NewReader(resp.Body)
	if _, err := io.ReadFull(reader, make([]byte, 32)); err != nil {
		t.Fatalf("first frame: %v", err)
	}

	// The client leaves now.
	resp.Body.Close()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !h.fake.Cancelled("key-a") {
		time.Sleep(10 * time.Millisecond)
	}
	if !h.fake.Cancelled("key-a") {
		t.Error("the upstream request was not cancelled when the client disconnected")
	}

	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && h.handler.Generation().registry.Pools()[0].Snapshot()[0].Active != 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if active := h.handler.Generation().registry.Pools()[0].Snapshot()[0].Active; active != 0 {
		t.Errorf("active counter leaked: %d", active)
	}
	if !strings.Contains(h.log.String(), "client_gone") {
		t.Errorf("the disconnect was not logged as a client going away: %s", h.log.String())
	}
}

// TestNoGoroutineLeak runs many requests of every shape and checks that nothing is
// left behind: no active counters, no goroutines still sitting in the request path.
func TestNoGoroutineLeak(t *testing.T) {
	h := newHarness(t, `
routes:
  m:
    targets:
      - name: a
        model: m
        base_url: https://fake.invalid/v1
        api_key: key-a
      - name: b
        model: m
        base_url: https://fake.invalid/v1
        api_key: key-b
timeouts:
  stream_idle: 100ms
`)

	h.fake.Set("key-a", fakeupstream.Behavior{Status: 200, Body: `{"ok":true}`})
	h.fake.Set("key-b", fakeupstream.Behavior{Status: 429, Body: `{"error":{"message":"limited"}}`})

	for i := 0; i < 200; i++ {
		h.post(chatBody("m"))
		h.clock.Advance(11 * time.Second)
	}

	for _, pool := range h.handler.Generation().registry.Pools() {
		for _, s := range pool.Snapshot() {
			if s.Active != 0 {
				t.Errorf("%s leaked %d active requests", s.Target, s.Active)
			}
		}
	}

	runtime.GC()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && goroutinesInRequestPath() > 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if n := goroutinesInRequestPath(); n > 0 {
		t.Errorf("goroutine leak: %d goroutines still in the request path", n)
	}
}

// goroutinesInRequestPath counts the goroutines whose stack still holds the request
// path, which is what a leak would look like after every response has finished.
func goroutinesInRequestPath() int {
	buf := make([]byte, 1<<16)
	n := runtime.Stack(buf, true)
	count := 0
	for _, line := range strings.Split(string(buf[:n]), "\n") {
		if strings.Contains(line, "proxy.(*Handler).attempt(") {
			count++
		}
	}
	return count
}

// TestFuzzLikeInputs covers the shapes a broken upstream produces. The gateway must
// not panic, and must not turn a response the upstream served successfully into an
// error of its own.
func TestFuzzLikeInputs(t *testing.T) {
	cases := []struct {
		name   string
		beh    fakeupstream.Behavior
		needle string
	}{
		{
			name:   "malformed SSE",
			beh:    fakeupstream.Behavior{Status: 200, SSE: []string{"data: not json at all\n\n", "data: [DONE]\n\n"}},
			needle: "not json at all",
		},
		{
			name:   "truncated frame",
			beh:    fakeupstream.Behavior{Status: 200, SSE: []string{"data: {\"delta\":{\"content\":\"half\"}}, "}},
			needle: "half",
		},
		{
			name:   "no frames at all",
			beh:    fakeupstream.Behavior{Status: 200, Body: ``},
			needle: "",
		},
		{
			name:   "malformed error body",
			beh:    fakeupstream.Behavior{Status: 500, Body: `<html>not json</html>`},
			needle: "not json",
		},
		{
			name:   "negative retry-after",
			beh:    fakeupstream.Behavior{Status: 429, Headers: map[string]string{"Retry-After": "-5"}, Body: `{"error":{}}`},
			needle: "",
		},
		{
			name:   "huge retry-after",
			beh:    fakeupstream.Behavior{Status: 429, Headers: map[string]string{"Retry-After": "999999999999999999999"}, Body: `{"error":{}}`},
			needle: "",
		},
		{
			name:   "upstream closes without a body",
			beh:    fakeupstream.Behavior{Status: 200, CloseEarly: true},
			needle: "",
		},
	}

	for _, c := range cases {
		h := newHarness(t, `
routes:
  m:
    targets:
      - name: a
        model: m
        base_url: https://fake.invalid/v1
        api_key: key-a
timeouts:
  stream_idle: 1s
`)

		h.fake.Set("key-a", c.beh)

		resp, err := h.gateway.Client().Post(h.gateway.URL+"/v1/chat/completions", "application/json",
			strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"x"}]}`))
		if err != nil {
			t.Errorf("%s: request: %v", c.name, err)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if c.needle != "" && !bytes.Contains(body, []byte(c.needle)) {
			t.Errorf("%s: body lost content: %q", c.name, string(body))
		}
		for _, pool := range h.handler.Generation().registry.Pools() {
			for _, s := range pool.Snapshot() {
				if s.Active != 0 {
					t.Errorf("%s: active counter leaked: %d", c.name, s.Active)
				}
			}
		}
	}
}

// TestUpstreamClosingMidStreamIsNotReportedAsSuccess is the failure mode this project
// exists to avoid: a stream the upstream served successfully must not become the gateway's
// own error, and a stream the upstream ended abruptly must not be reported as a clean
// completion. The frames that arrived reach the client, and the way it ended is recorded.
func TestUpstreamClosingMidStreamIsNotReportedAsSuccess(t *testing.T) {
	h := newHarness(t, `
routes:
  m:
    targets:
      - name: a
        model: m
        base_url: https://fake.invalid/v1
        api_key: key-a
timeouts:
  stream_idle: 1s
`)

	h.fake.Set("key-a", fakeupstream.Behavior{Status: 200, CloseEarly: true, SSE: []string{
		"data: {\"delta\":{\"role\":\"assistant\"}}\n\n",
		"data: {\"delta\":{\"content\":\"half\"}}\n\n",
	}})

	_, body := h.post(`{"model":"m","stream":true,"messages":[{"role":"user","content":"x"}]}`)

	if !strings.Contains(body, "half") {
		t.Errorf("the client lost the bytes that arrived: %q", body)
	}
	if strings.Contains(body, "[DONE]") {
		t.Errorf("the gateway invented a closing frame the upstream did not send: %q", body)
	}
	if strings.Contains(h.log.String(), "request_completed") {
		t.Errorf("an aborted stream was logged as a completed request: %s", h.log.String())
	}
	if !strings.Contains(h.log.String(), "stream_ended_early") {
		t.Errorf("the abrupt end was not recorded: %s", h.log.String())
	}
	if got := testutil.ToFloat64(h.metrics.UpstreamErrors.WithLabelValues("m", "a", "transport")); got != 1 {
		t.Errorf("upstream_errors transport: got %v, want 1", got)
	}
}

// TestResponseLimitStopsCopying: the gateway never holds a model response in memory,
// and a stream longer than the configured cap stops copying without becoming an
// internal error. The client keeps the bytes that already arrived.
func TestResponseLimitStopsCopying(t *testing.T) {
	h := newHarness(t, `
server:
  max_response_bytes: 200
routes:
  m:
    targets:
      - name: a
        model: m
        base_url: https://fake.invalid/v1
        api_key: key-a
`)

	chunk := "data: " + strings.Repeat("x", 60) + "\n\n"
	frames := make([]string, 10)
	for i := range frames {
		frames[i] = chunk
	}
	h.fake.Set("key-a", fakeupstream.Behavior{Status: 200, SSE: frames})

	resp, body := h.post(`{"model":"m","stream":true,"messages":[]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d, want 200: a response the upstream served must not become the gateway's own error", resp.StatusCode)
	}
	if len(body) >= 300 {
		t.Errorf("body length: got %d, want copying to stop near the 200 byte cap", len(body))
	}
	if len(body) < 200 {
		t.Errorf("body length: got %d, want at least the cap worth of bytes", len(body))
	}
	if !strings.Contains(h.log.String(), "response_limit") {
		t.Errorf("the cap was not logged: %s", h.log.String())
	}
	if got := testutil.ToFloat64(h.metrics.StreamAborts.WithLabelValues("m", "a", "response_limit")); got != 1 {
		t.Errorf("stream_aborts_total: got %v, want 1", got)
	}
}

// TestRequestBodyTooLarge: an oversized body is rejected before any upstream call, so
// the gateway never forwards a request it could not have answered.
func TestRequestBodyTooLarge(t *testing.T) {
	h := newHarness(t, `
server:
  max_request_bytes: 512
routes:
  m:
    targets:
      - name: a
        model: m
        base_url: https://fake.invalid/v1
        api_key: key-a
`)

	h.fake.Set("key-a", fakeupstream.Behavior{Status: 200, Body: `{"ok":true}`})

	body := `{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("a", 1000) + `"}]}`
	resp, out := h.post(body)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status: got %d, want 413 (%s)", resp.StatusCode, out)
	}
	if !strings.Contains(out, "request_body_too_large") {
		t.Errorf("error code: got %s", out)
	}
	if h.fake.TotalAttempts() != 0 {
		t.Error("an oversized body reached an upstream")
	}
}

// TestRequestBodyExactlyAtTheLimitIsForwarded: the limit is a limit, not a refusal at the
// boundary. A body of exactly the configured size reaches the upstream, so the check that
// rejects a larger body cannot also reject the one it was sized for.
func TestRequestBodyExactlyAtTheLimitIsForwarded(t *testing.T) {
	h := newHarness(t, `
server:
  max_request_bytes: 4096
routes:
  m:
    targets:
      - name: a
        model: provider/model-one
        base_url: https://fake.invalid/v1
        api_key: key-a
`)

	h.fake.Set("key-a", fakeupstream.Behavior{Status: 200, Body: `{"ok":true}`})

	prefix := `{"model":"m","messages":[{"role":"user","content":"prompt content `
	suffix := `"}]}`
	body := prefix + strings.Repeat("a", 4096-len(prefix)-len(suffix)) + suffix
	if len(body) != 4096 {
		t.Fatalf("fixture: body is %d bytes, want exactly the limit", len(body))
	}

	resp, _ := h.post(body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200 at the boundary (%s)", resp.StatusCode, body[:40])
	}
	if h.fake.TotalAttempts() != 1 {
		t.Errorf("attempts: got %d, want 1", h.fake.TotalAttempts())
	}
	want := strings.Replace(body, `"model":"m"`, `"model":"provider/model-one"`, 1)
	if got := h.fake.LastBody("key-a"); got != want {
		t.Errorf("forwarded body: got %d bytes, want the same bytes with only the model value changed", len(got))
	}
}

// TestLargeMultimodalBodyOnlyTheModelValueChanges is the shape a coding agent sends: a large
// body whose interesting bytes are a base64 blob the gateway does not understand. The point is
// that a big body is still handled by splicing the model value into the original document, not
// by parsing and re-serialising it, so the blob, the field order, the unknown field and the
// spaces in the text part all survive as they were written.
func TestLargeMultimodalBodyOnlyTheModelValueChanges(t *testing.T) {
	h := newHarness(t, `
server:
  max_request_bytes: 33554432
routes:
  vision:
    targets:
      - name: a
        model: provider/vision-model
        base_url: https://fake.invalid/v1
        api_key: key-a
`)

	h.fake.Set("key-a", fakeupstream.Behavior{Status: 200, Body: `{"ok":true}`})

	blob := strings.Repeat("A9b+c/=", 200000)
	prefix := `{"model":"vision","stream":true,"messages":[{"role":"user","content":[{"type":"text",` +
		`"text":"what is in this screenshot, agent?"},{"type":"image_url","image_url":{"url":` +
		`"data:image/png;base64,`
	suffix := `"}}]}],"unknown_field":{"nested":[1,2,3]}}`
	body := prefix + blob + suffix
	if len(body) < 1<<20 {
		t.Fatalf("fixture: body is %d bytes, which is not a body worth calling large", len(body))
	}
	want := strings.Replace(body, `"model":"vision"`, `"model":"provider/vision-model"`, 1)

	resp, out := h.post(body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (%s)", resp.StatusCode, out)
	}

	received := h.fake.LastBody("key-a")
	if received != want {
		t.Errorf("forwarded body is not the bytes the client sent, with only the model value changed")
	}
	if got := h.fake.Last("key-a").ContentLength; got != int64(len(want)) {
		t.Errorf("Content-Length the upstream was told: got %d, want %d for the body that arrived",
			got, len(want))
	}
	if !strings.Contains(received, "what is in this screenshot, agent?") {
		t.Errorf("the text part changed: the gateway is not supposed to read it")
	}
	if !strings.Contains(received, `"unknown_field":{"nested":[1,2,3]}`) {
		t.Errorf("an unknown field was dropped or rewritten")
	}
}

// TestResponseExactlyAtTheCapIsDeliveredWhole: a response of exactly the configured size is the
// whole response, not a truncated one. The test that checks copying stops past the cap cannot show
// this half on its own, because a body at the cap is the case where nothing is lost. It is still
// recorded as a cap hit, and this test says so rather than pretending otherwise: the gateway
// cannot know whether more bytes would have followed without waiting for them, and waiting is what
// an idle watchdog exists to stop, so a response that happens to end at the cap is counted as
// capped. What the client keeps is the whole response either way.
func TestResponseExactlyAtTheCapIsDeliveredWhole(t *testing.T) {
	h := newHarness(t, `
server:
  max_response_bytes: 200
routes:
  m:
    targets:
      - name: a
        model: m
        base_url: https://fake.invalid/v1
        api_key: key-a
`)

	frame := "data: " + strings.Repeat("x", 42) + "\n\n"
	if len(frame) != 50 {
		t.Fatalf("fixture: frame is %d bytes, want 50", len(frame))
	}
	frames := []string{frame, frame, frame, frame}
	h.fake.Set("key-a", fakeupstream.Behavior{Status: 200, SSE: frames})

	resp, body := h.post(`{"model":"m","stream":true,"messages":[]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	if len(body) != 200 {
		t.Errorf("body length: got %d, want the whole 200 bytes of the cap", len(body))
	}
	if !strings.Contains(h.log.String(), "response_limit") {
		t.Errorf("the cap was not recorded for a response that reached it: %s", h.log.String())
	}
	if got := testutil.ToFloat64(h.metrics.StreamAborts.WithLabelValues("m", "a", "response_limit")); got != 1 {
		t.Errorf("stream_aborts_total: got %v, want 1 for the cap that stopped copying", got)
	}
}

// TestConcurrentRequests checks the promise the project makes in its README: more
// than a hundred simultaneous requests, checked under the race detector.
func TestConcurrentRequests(t *testing.T) {
	h := newHarness(t, `
routes:
  demo:
    targets:
      - name: a
        model: provider/model-one
        base_url: https://fake.invalid/v1
        api_key: key-a
      - name: b
        model: provider/model-one
        base_url: https://fake.invalid/v1
        api_key: key-b
      - name: c
        model: provider/model-one
        base_url: https://fake.invalid/v1
        api_key: key-c
      - name: d
        model: provider/model-one
        base_url: https://fake.invalid/v1
        api_key: key-d
`)

	frames := []string{
		"data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n",
		"data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n",
		"data: [DONE]\n\n",
	}
	for _, key := range []string{"key-a", "key-b", "key-c", "key-d"} {
		h.fake.Set(key, fakeupstream.Behavior{Status: 200, SSE: frames})
	}

	const n = 120
	errs := make(chan string, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			resp, err := h.gateway.Client().Post(h.gateway.URL+"/v1/chat/completions", "application/json",
				strings.NewReader(fmt.Sprintf(`{"model":"demo","stream":true,"messages":[{"role":"user","content":"x%d"}]}`, i)))
			if err != nil {
				errs <- fmt.Sprintf("request %d: %v", i, err)
				return
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				errs <- fmt.Sprintf("read %d: %v", i, err)
				return
			}
			if resp.StatusCode != 200 {
				errs <- fmt.Sprintf("request %d: status %d", i, resp.StatusCode)
				return
			}
			if string(body) != strings.Join(frames, "") {
				errs <- fmt.Sprintf("stream changed on request %d: %q", i, string(body))
				return
			}
			errs <- ""
		}(i)
	}

	for i := 0; i < n; i++ {
		select {
		case msg := <-errs:
			if msg != "" {
				t.Error(msg)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("request %d did not finish", i)
		}
	}

	if h.fake.TotalAttempts() != n {
		t.Errorf("upstream attempts: got %d, want %d", h.fake.TotalAttempts(), n)
	}
	for _, pool := range h.handler.Generation().registry.Pools() {
		for _, s := range pool.Snapshot() {
			if s.Active != 0 {
				t.Errorf("%s leaked %d active requests", s.Target, s.Active)
			}
		}
	}
}

// TestGracefulShutdownDrains proves that draining stops new work while an in-flight
// stream gets the grace period to finish.
func TestGracefulShutdownDrains(t *testing.T) {
	h := newHarness(t, `
routes:
  m:
    targets:
      - name: a
        model: m
        base_url: https://fake.invalid/v1
        api_key: key-a
`)

	h.fake.Set("key-a", fakeupstream.Behavior{Status: 200,
		SSE:   []string{"data: {\"delta\":{\"content\":\"streaming\"}}\n\n", "data: [DONE]\n\n"},
		Gap:   100 * time.Millisecond,
		Delay: 50 * time.Millisecond,
	})

	done := make(chan string, 1)
	go func() {
		resp, err := h.gateway.Client().Post(h.gateway.URL+"/v1/chat/completions", "application/json",
			strings.NewReader(`{"model":"m","stream":true,"messages":[]}`))
		if err != nil {
			done <- fmt.Sprintf("request: %v", err)
			return
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		done <- string(body)
	}()

	time.Sleep(20 * time.Millisecond)
	h.handler.Drain()
	if h.handler.Ready() {
		t.Error("readiness must report unhealthy while draining")
	}

	select {
	case got := <-done:
		if !strings.Contains(got, "streaming") {
			t.Errorf("the in-flight stream was cut off: %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the in-flight request did not finish")
	}

	if !h.handler.Wait(time.Now().Add(2 * time.Second)) {
		t.Error("Wait did not see the request finish")
	}
	for _, pool := range h.handler.Generation().registry.Pools() {
		for _, s := range pool.Snapshot() {
			if s.Active != 0 {
				t.Errorf("active counter leaked after shutdown: %d", s.Active)
			}
		}
	}
}
