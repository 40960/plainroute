// SPDX-License-Identifier: AGPL-3.0-only

package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/40960/plainroute/internal/config"
	"github.com/40960/plainroute/internal/fakeupstream"
	"github.com/40960/plainroute/internal/metrics"
	"github.com/40960/plainroute/internal/routing"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type harness struct {
	t       *testing.T
	gateway *httptest.Server
	fake    *fakeupstream.Server
	handler *Handler
	metrics *metrics.Metrics
	log     *bytes.Buffer
	clock   *fakeClock
}

func newHarness(t *testing.T, cfgText string, env config.Env) *harness {
	t.Helper()

	fake := fakeupstream.New()
	fakeServer := httptest.NewServer(fake)
	t.Cleanup(fakeServer.Close)

	// Point the configured targets at the fake upstream. The placeholder in the
	// test config is only a stand-in for the httptest URL.
	cfgText = strings.ReplaceAll(cfgText, "https://fake.invalid/v1", fakeServer.URL+"/v1")

	cfg, err := config.Parse([]byte(cfgText), env)
	if err != nil {
		t.Fatalf("config: %v", err)
	}

	clock := &fakeClock{t: time.Unix(1700000000, 0)}
	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	m := metrics.New()

	handler := New(cfg, m, logger, routing.NewStateStore(), clock.Now)
	handler.MarkReady()

	gateway := httptest.NewServer(handler)
	t.Cleanup(gateway.Close)

	return &harness{t: t, gateway: gateway, fake: fake, handler: handler, metrics: m, log: logBuf, clock: clock}
}

func (h *harness) post(body string) (*http.Response, string) {
	h.t.Helper()
	resp, err := h.gateway.Client().Post(h.gateway.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		h.t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, string(out)
}

func (h *harness) get(path string) (*http.Response, string) {
	h.t.Helper()
	resp, err := h.gateway.Client().Get(h.gateway.URL + path)
	if err != nil {
		h.t.Fatalf("request %s: %v", path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, string(out)
}

func chatBody(model string) string {
	return fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"prompt-content"}]}`, model)
}

func (h *harness) requests(model, target, class string) float64 {
	return testutil.ToFloat64(h.metrics.RequestsTotal.WithLabelValues(model, target, class))
}

func (h *harness) attemptsMetric(model, target, class string) float64 {
	return testutil.ToFloat64(h.metrics.UpstreamAttempts.WithLabelValues(model, target, class))
}

// TestStreamIsByteForByte is the streaming integrity test: the bytes the upstream
// produced must arrive unchanged, in order, including [DONE].
func TestStreamIsByteForByte(t *testing.T) {
	h := newHarness(t, `
models:
  demo:
    upstream_model: provider/model-one
    targets:
      - name: a
        base_url: https://fake.invalid/v1
        api_key_env: KEY_A
`, config.Env{"KEY_A": "key-a"})

	sequence := []string{
		"data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n",
		"data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n",
		"data: [DONE]\n\n",
	}
	h.fake.Set("key-a", fakeupstream.Behavior{Status: 200, SSE: sequence})

	resp, got := h.post(`{"model":"demo","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d, want 200 (%s)", resp.StatusCode, got)
	}
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Errorf("content type: got %q, want text/event-stream", resp.Header.Get("Content-Type"))
	}
	if got != strings.Join(sequence, "") {
		t.Errorf("stream is not byte for byte:\n got %q\nwant %q", got, strings.Join(sequence, ""))
	}
	if !strings.Contains(got, `"role":"assistant"`) {
		t.Error("the opening role delta was removed")
	}
	if !strings.Contains(got, "[DONE]") {
		t.Error("[DONE] was removed")
	}
}

func TestToolCallStreamPassesThroughUnchanged(t *testing.T) {
	h := newHarness(t, `
models:
  demo:
    upstream_model: provider/model-one
    targets:
      - name: a
        base_url: https://fake.invalid/v1
        api_key_env: KEY_A
`, config.Env{"KEY_A": "key-a"})

	sequence := []string{
		"data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"read_file\"}}}]}]}\n\n",
		"data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"path\\\":\\\"a.go\\\"}\"}}]}}]}\n\n",
		"data: [DONE]\n\n",
	}
	h.fake.Set("key-a", fakeupstream.Behavior{Status: 200, SSE: sequence})

	resp, got := h.post(`{"model":"demo","stream":true,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"read_file"}}],"tool_choice":"auto","reasoning":{"effort":"high"},"unknown_extension":{"x":1}}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d", resp.StatusCode)
	}
	if got != strings.Join(sequence, "") {
		t.Errorf("tool call stream changed:\n got %q\nwant %q", got, strings.Join(sequence, ""))
	}

	// The two tool call deltas must stay two separate frames.
	if strings.Count(got, "chat.completion.chunk") != 2 {
		t.Errorf("frames were merged or added: %q", got)
	}
}

// Test429CooldownAndNoInternalRetry is the behaviour the project exists for: the
// target that returned 429 is cooled down, this request ends with the upstream
// error, and only the next independent request uses another target.
func Test429CooldownAndNoInternalRetry(t *testing.T) {
	h := newHarness(t, `
models:
  demo:
    upstream_model: provider/model-one
    targets:
      - name: account-1
        base_url: https://fake.invalid/v1
        api_key_env: KEY_A
      - name: account-2
        base_url: https://fake.invalid/v1
        api_key_env: KEY_B
`, config.Env{"KEY_A": "key-a", "KEY_B": "key-b"})

	h.fake.Set("key-a", fakeupstream.Behavior{Status: 429, Body: `{"error":{"message":"rate limited","type":"rate_limit_error"}}`})
	h.fake.Set("key-b", fakeupstream.Behavior{Status: 200, Body: `{"ok":true}`})

	resp, body := h.post(chatBody("demo"))
	if resp.StatusCode != 429 {
		t.Fatalf("first request: got %d, want 429", resp.StatusCode)
	}
	if !strings.Contains(body, "rate limited") {
		t.Errorf("the upstream error body must be forwarded, got %q", body)
	}
	if h.fake.TotalAttempts() != 1 {
		t.Fatalf("attempts: got %d, want 1: the gateway retried inside one request", h.fake.TotalAttempts())
	}
	if h.fake.Attempts("key-b") != 0 {
		t.Error("the gateway retried to target B inside the same request")
	}

	resp, _ = h.post(chatBody("demo"))
	if resp.StatusCode != 200 {
		t.Fatalf("second request: got %d, want 200", resp.StatusCode)
	}
	if h.fake.Attempts("key-a") != 1 {
		t.Errorf("target A was used while cooling down: %d attempts", h.fake.Attempts("key-a"))
	}
	if h.fake.Attempts("key-b") != 1 {
		t.Errorf("target B attempts: got %d, want 1", h.fake.Attempts("key-b"))
	}

	h.clock.Advance(10 * time.Second)
	resp, _ = h.post(chatBody("demo"))
	if h.fake.Attempts("key-a") != 2 {
		t.Errorf("target A should be selectable again after recovery: %d", h.fake.Attempts("key-a"))
	}
	if resp.StatusCode != 429 && resp.StatusCode != 200 {
		t.Fatalf("after recovery: unexpected status %d", resp.StatusCode)
	}
	if !strings.Contains(h.log.String(), "cooldown") {
		t.Error("the cooldown event was not logged")
	}
}

func TestRetryAfterFromUpstreamWins(t *testing.T) {
	h := newHarness(t, `
models:
  m:
    upstream_model: m
    targets:
      - name: a
        base_url: https://fake.invalid/v1
        api_key_env: KEY_A
      - name: b
        base_url: https://fake.invalid/v1
        api_key_env: KEY_B
routing:
  cooldown:
    status: {429: 3s}
    max: 30s
`, config.Env{"KEY_A": "key-a", "KEY_B": "key-b"})

	h.fake.Set("key-a", fakeupstream.Behavior{Status: 429, Headers: map[string]string{"Retry-After": "2"},
		Body: `{"error":{"message":"limited"}}`})
	h.fake.Set("key-b", fakeupstream.Behavior{Status: 200, Body: `{"ok":true}`})

	resp, _ := h.post(chatBody("m"))
	if resp.StatusCode != 429 {
		t.Fatalf("status: got %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") != "2" {
		t.Errorf("Retry-After: got %q, want 2", resp.Header.Get("Retry-After"))
	}

	h.clock.Advance(time.Second)
	resp, _ = h.post(chatBody("m"))
	if resp.StatusCode != 200 {
		t.Fatalf("second request: got %d, want 200", resp.StatusCode)
	}
	if h.fake.Attempts("key-a") != 1 {
		t.Errorf("key-a used %d times inside its own Retry-After window", h.fake.Attempts("key-a"))
	}

	h.clock.Advance(time.Second)
	h.post(chatBody("m"))
	if h.fake.Attempts("key-a") != 2 {
		t.Errorf("key-a should be usable again at the boundary: %d", h.fake.Attempts("key-a"))
	}
}

func TestInvalidRetryAfterFallsBackToConfiguredCooldown(t *testing.T) {
	h := newHarness(t, `
models:
  m:
    upstream_model: m
    targets:
      - name: a
        base_url: https://fake.invalid/v1
        api_key_env: KEY_A
routing:
  cooldown:
    status: {429: 5s}
    max: 30s
`, config.Env{"KEY_A": "key-a"})

	h.fake.Set("key-a", fakeupstream.Behavior{Status: 429,
		Headers: map[string]string{"Retry-After": "not-a-number"}, Body: `{"error":{"message":"limited"}}`})

	resp, _ := h.post(chatBody("m"))
	if resp.StatusCode != 429 {
		t.Fatalf("status: got %d", resp.StatusCode)
	}

	h.clock.Advance(4 * time.Second)
	h.post(chatBody("m"))
	if h.fake.Attempts("key-a") != 1 {
		t.Error("target was used before the configured cooldown ended")
	}

	h.clock.Advance(time.Second)
	h.post(chatBody("m"))
	if h.fake.Attempts("key-a") != 2 {
		t.Error("target should be usable after the configured cooldown")
	}
}

func TestHugeRetryAfterIsCapped(t *testing.T) {
	h := newHarness(t, `
models:
  m:
    upstream_model: m
    targets:
      - name: a
        base_url: https://fake.invalid/v1
        api_key_env: KEY_A
routing:
  cooldown:
    status: {429: 10s}
    max: 30s
`, config.Env{"KEY_A": "key-a"})

	h.fake.Set("key-a", fakeupstream.Behavior{Status: 429,
		Headers: map[string]string{"Retry-After": "36000"}, Body: `{"error":{"message":"limited"}}`})

	resp, _ := h.post(chatBody("m"))
	if got := resp.Header.Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After not capped: got %q, want 30", got)
	}
}

// TestAllCoolingDoesNotTouchUpstream: a fully cooled model is answered from the
// gateway's own state, and Retry-After comes from the target that recovers
// first.
func TestAllCoolingDoesNotTouchUpstream(t *testing.T) {
	h := newHarness(t, `
models:
  demo:
    upstream_model: provider/model-one
    targets:
      - name: a
        base_url: https://fake.invalid/v1
        api_key_env: KEY_A
      - name: b
        base_url: https://fake.invalid/v1
        api_key_env: KEY_B
      - name: c
        base_url: https://fake.invalid/v1
        api_key_env: KEY_C
      - name: d
        base_url: https://fake.invalid/v1
        api_key_env: KEY_D
`, config.Env{"KEY_A": "key-a", "KEY_B": "key-b", "KEY_C": "key-c", "KEY_D": "key-d"})

	for _, key := range []string{"key-a", "key-b", "key-c", "key-d"} {
		h.fake.Set(key, fakeupstream.Behavior{Status: 429, Body: `{"error":{"message":"limited"}}`})
		resp, _ := h.post(chatBody("demo"))
		if resp.StatusCode != 429 {
			t.Fatalf("warm up %s: got %d", key, resp.StatusCode)
		}
	}
	if h.fake.TotalAttempts() != 4 {
		t.Fatalf("warm up attempts: got %d, want 4", h.fake.TotalAttempts())
	}

	before := h.fake.TotalAttempts()
	resp, body := h.post(chatBody("demo"))
	if resp.StatusCode != 429 {
		t.Fatalf("all cooling: got %d", resp.StatusCode)
	}
	if h.fake.TotalAttempts() != before {
		t.Error("the gateway contacted an upstream while every target was cooling")
	}
	if got := resp.Header.Get("Retry-After"); got != "10" {
		t.Errorf("Retry-After: got %q, want 10", got)
	}

	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("error body is not JSON: %v (%s)", err, body)
	}
	if envelope.Error.Code != "all_deployments_cooling_down" || envelope.Error.Type != "rate_limit_error" {
		t.Errorf("error shape: %+v", envelope.Error)
	}
	if envelope.Error.Message != "No healthy targets are currently available" {
		t.Errorf("message: got %q", envelope.Error.Message)
	}

	// The target that cools down last is still cooling; the one that recovers
	// first decides Retry-After once time moves.
	h.clock.Advance(4 * time.Second)
	resp, _ = h.post(chatBody("demo"))
	if h.fake.TotalAttempts() != before {
		t.Error("the gateway contacted an upstream while every target was cooling")
	}
	if got := resp.Header.Get("Retry-After"); got != "6" {
		t.Errorf("Retry-After after time moved: got %q, want 6", got)
	}
}

// TestNoRetryAmplification is the core regression test: 50 client requests must
// never become more than 50 upstream attempts.
func TestNoRetryAmplification(t *testing.T) {
	h := newHarness(t, `
models:
  demo:
    upstream_model: provider/model-one
    targets:
      - name: a
        base_url: https://fake.invalid/v1
        api_key_env: KEY_A
      - name: b
        base_url: https://fake.invalid/v1
        api_key_env: KEY_B
      - name: c
        base_url: https://fake.invalid/v1
        api_key_env: KEY_C
      - name: d
        base_url: https://fake.invalid/v1
        api_key_env: KEY_D
retry:
  enabled: false
`, config.Env{"KEY_A": "key-a", "KEY_B": "key-b", "KEY_C": "key-c", "KEY_D": "key-d"})

	for _, key := range []string{"key-a", "key-b", "key-c", "key-d"} {
		h.fake.Set(key, fakeupstream.Behavior{Status: 429, Body: `{"error":{"message":"limited"}}`})
	}

	for i := 0; i < 50; i++ {
		resp, _ := h.post(chatBody("demo"))
		if resp.StatusCode != 429 {
			t.Fatalf("request %d: got %d", i, resp.StatusCode)
		}
		// Each client request is independent; the client is the one that waits.
		h.clock.Advance(11 * time.Second)
	}

	attempts := h.fake.TotalAttempts()
	if attempts > 50 {
		t.Fatalf("retry amplification: 50 client requests produced %d upstream attempts", attempts)
	}

	var reported float64
	for _, target := range []string{"a", "b", "c", "d", metrics.NoTarget} {
		reported += h.requests("demo", target, "4xx")
	}
	if reported != 50 {
		t.Errorf("requests_total: got %v, want 50", reported)
	}

	var upstream float64
	for _, target := range []string{"a", "b", "c", "d"} {
		upstream += h.attemptsMetric("demo", target, "4xx")
	}
	if upstream > 50 {
		t.Errorf("upstream_attempts_total: got %v, want at most 50", upstream)
	}
	if upstream > reported {
		t.Errorf("upstream attempts (%v) exceed client requests (%v)", upstream, reported)
	}
}

func TestErrorStatusesAreForwardedWithoutRetry(t *testing.T) {
	for _, code := range []int{500, 503} {
		h := newHarness(t, fmt.Sprintf(`
models:
  m:
    upstream_model: m
    targets:
      - name: a
        base_url: https://fake.invalid/v1
        api_key_env: KEY_A
routing:
  cooldown:
    status: {%d: 15s}
    max: 60s
`, code), config.Env{"KEY_A": "key-a"})

		h.fake.Set("key-a", fakeupstream.Behavior{Status: code, Body: `{"error":{"message":"upstream said no"}}`})
		resp, body := h.post(chatBody("m"))
		if resp.StatusCode != code {
			t.Fatalf("status: got %d, want %d", resp.StatusCode, code)
		}
		if !strings.Contains(body, "upstream said no") {
			t.Errorf("error body not forwarded: %s", body)
		}
		if h.fake.TotalAttempts() != 1 {
			t.Errorf("attempts: got %d, want 1", h.fake.TotalAttempts())
		}
	}
}

func TestModelAliasAndRawBodyArePreserved(t *testing.T) {
	h := newHarness(t, `
models:
  demo:
    upstream_model: provider/model-one
    targets:
      - name: a
        base_url: https://fake.invalid/v1
        api_key_env: KEY_A
`, config.Env{"KEY_A": "key-a"})

	h.fake.Set("key-a", fakeupstream.Behavior{Status: 200, Body: `{"ok":true}`})

	body := `{"model":"demo","messages":[{"role":"user","content":"hi"}],"temperature":0.2,` +
		`"reasoning":{"effort":"high"},"stream_options":{"include_usage":true},"vendor_extra":{"keep":"me"}}`
	resp, _ := h.post(body)
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d", resp.StatusCode)
	}

	sent := h.fake.LastBody("key-a")
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal([]byte(sent), &parsed); err != nil {
		t.Fatalf("upstream received invalid JSON: %s", sent)
	}
	if string(parsed["model"]) != `"provider/model-one"` {
		t.Errorf("model not mapped: got %s", parsed["model"])
	}
	if string(parsed["temperature"]) != `0.2` {
		t.Errorf("temperature changed: got %s", parsed["temperature"])
	}
	if string(parsed["vendor_extra"]) != `{"keep":"me"}` {
		t.Errorf("unknown field dropped: got %s", parsed["vendor_extra"])
	}
	if string(parsed["stream_options"]) != `{"include_usage":true}` {
		t.Errorf("stream_options changed: got %s", parsed["stream_options"])
	}
	if strings.Index(sent, `"temperature"`) > strings.Index(sent, `"reasoning"`) {
		t.Errorf("field order was rebuilt: %s", sent)
	}
}

func TestLogsDoNotContainKeysOrPrompts(t *testing.T) {
	h := newHarness(t, `
models:
  m:
    upstream_model: m
    targets:
      - name: a
        base_url: https://fake.invalid/v1
        api_key_env: KEY_A
`, config.Env{"KEY_A": "super-secret-key-12345"})

	// Behaviour is keyed by the value of the API key, which is what the gateway
	// presents to the upstream.
	h.fake.Set("super-secret-key-12345", fakeupstream.Behavior{Status: 429, Body: `{"error":{"message":"limited"}}`})
	h.post(chatBody("m"))

	out := h.log.String()
	if strings.Contains(out, "super-secret-key-12345") {
		t.Errorf("a key appeared in the log: %s", out)
	}
	if strings.Contains(out, "Authorization") {
		t.Errorf("the Authorization header appeared in the log: %s", out)
	}
	if strings.Contains(out, "prompt-content") {
		t.Errorf("prompt content appeared in the log: %s", out)
	}
	if !strings.Contains(out, "cooldown") {
		t.Error("the cooldown event is missing from the log")
	}
}

func TestUnknownModelAndBadJSON(t *testing.T) {
	h := newHarness(t, `
models:
  m:
    upstream_model: m
    targets:
      - name: a
        base_url: https://fake.invalid/v1
        api_key_env: KEY_A
`, config.Env{"KEY_A": "key-a"})

	resp, body := h.post(chatBody("nope"))
	if resp.StatusCode != 404 {
		t.Fatalf("unknown model: got %d", resp.StatusCode)
	}
	if !strings.Contains(body, "model_not_found") {
		t.Errorf("error code: got %s", body)
	}
	if h.fake.TotalAttempts() != 0 {
		t.Error("an unknown model must not reach an upstream")
	}

	resp, body = h.post(`{ not json`)
	if resp.StatusCode != 400 {
		t.Fatalf("bad json: got %d", resp.StatusCode)
	}
	if !strings.Contains(body, "invalid_json") {
		t.Errorf("error code: got %s", body)
	}

	resp, body = h.post(`{"model":"m"}`)
	if resp.StatusCode != 200 {
		t.Errorf("a request with no messages should still be forwarded, got %d (%s)", resp.StatusCode, body)
	}

	resp, body = h.get("/nope")
	if resp.StatusCode != 404 {
		t.Errorf("unknown path: got %d", resp.StatusCode)
	}
	if !strings.Contains(body, "page_not_found") {
		t.Errorf("error code: got %s", body)
	}
}

// TestFailedWriteIsRecordedAsClientGone: the gateway answers some requests by itself,
// and a write that fails there means the client left before the answer arrived. That
// has to be recorded like any other disconnect, not swallowed.
type failingWriter struct {
	header http.Header
	err    error
}

func (f *failingWriter) Header() http.Header       { return f.header }
func (f *failingWriter) WriteHeader(status int)    {}
func (f *failingWriter) Write([]byte) (int, error) { return 0, f.err }

func TestFailedWriteIsRecordedAsClientGone(t *testing.T) {
	h := newHarness(t, `
models:
  m:
    upstream_model: m
    targets:
      - name: a
        base_url: https://fake.invalid/v1
        api_key_env: KEY_A
`, config.Env{"KEY_A": "key-a"})

	w := &failingWriter{header: http.Header{}, err: errors.New("client went away")}
	req, err := http.NewRequest(http.MethodPost, h.gateway.URL+"/v1/chat/completions", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	h.handler.reject(w, req, http.StatusTooManyRequests, "all_deployments_cooling_down",
		"rate_limit_error", "No healthy targets are currently available", "m", "test-id")

	if got := testutil.ToFloat64(h.metrics.ClientDisconnects.WithLabelValues("m", metrics.NoTarget)); got != 1 {
		t.Errorf("client_disconnects_total: got %v, want 1", got)
	}
	if !strings.Contains(h.log.String(), "client_disconnect") {
		t.Errorf("the failed write was not logged: %s", h.log.String())
	}
}

func TestHealthAndReadinessSurviveCooldown(t *testing.T) {
	h := newHarness(t, `
models:
  m:
    upstream_model: m
    targets:
      - name: a
        base_url: https://fake.invalid/v1
        api_key_env: KEY_A
`, config.Env{"KEY_A": "key-a"})

	h.fake.Set("key-a", fakeupstream.Behavior{Status: 429, Body: `{"error":{"message":"limited"}}`})
	h.post(chatBody("m"))

	if resp, _ := h.get("/healthz"); resp.StatusCode != 200 {
		t.Errorf("healthz: got %d", resp.StatusCode)
	}
	if resp, _ := h.get("/readyz"); resp.StatusCode != 200 {
		t.Errorf("readyz must not fail because one target is cooling, got %d", resp.StatusCode)
	}

	h.handler.Drain()
	if resp, _ := h.get("/readyz"); resp.StatusCode != 503 {
		t.Errorf("readyz while draining: got %d, want 503", resp.StatusCode)
	}
	if resp, _ := h.get("/healthz"); resp.StatusCode != 200 {
		t.Errorf("healthz while draining: got %d, want 200", resp.StatusCode)
	}
}

func TestModelsListingUsesAliasesAndRealNames(t *testing.T) {
	h := newHarness(t, `
models:
  demo:
    upstream_model: provider/model-one
    targets:
      - name: a
        base_url: https://fake.invalid/v1
        api_key_env: KEY_A
      - name: b
        base_url: https://fake.invalid/v1
        api_key_env: KEY_B
`, config.Env{"KEY_A": "key-a", "KEY_B": "key-b"})

	resp, body := h.get("/v1/models")
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d", resp.StatusCode)
	}
	var listing struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &listing); err != nil {
		t.Fatalf("listing is not JSON: %v (%s)", err, body)
	}
	if listing.Object != "list" || len(listing.Data) != 2 {
		t.Errorf("listing: got %s", body)
	}
	if listing.Data[0].ID != "demo" || listing.Data[1].ID != "provider/model-one" {
		t.Errorf("listing order: got %s", body)
	}

	// Requesting the real model name must work as well as the alias.
	h.fake.Set("key-a", fakeupstream.Behavior{Status: 200, Body: `{"ok":true}`})
	resp, _ = h.post(chatBody("provider/model-one"))
	if resp.StatusCode != 200 {
		t.Errorf("real model name request: got %d", resp.StatusCode)
	}
}
