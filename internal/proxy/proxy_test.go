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
	// fakeURL is the address the fake upstream is listening on, so a test that builds a second
	// configuration — the one a reload swaps in — can point its targets at the same upstream. The
	// placeholder in the config text is only a stand-in for this address.
	fakeURL string
	handler *Handler
	metrics *metrics.Metrics
	log     *bytes.Buffer
	clock   *fakeClock
}

func newHarness(t *testing.T, cfgText string) *harness {
	t.Helper()

	fake := fakeupstream.New()
	fakeServer := httptest.NewServer(fake)
	t.Cleanup(fakeServer.Close)

	// Point the configured targets at the fake upstream. The placeholder in the
	// test config is only a stand-in for the httptest URL.
	cfgText = strings.ReplaceAll(cfgText, "https://fake.invalid/v1", fakeServer.URL+"/v1")

	cfg, err := config.Parse([]byte(cfgText))
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

	return &harness{t: t, gateway: gateway, fake: fake, fakeURL: fakeServer.URL, handler: handler, metrics: m, log: logBuf, clock: clock}
}

// parseAt is Parse with the placeholder replaced by the address the fake upstream is listening on,
// which is the same thing newHarness does for the config a handler starts with. A reload config built
// with the placeholder would point at a name that cannot resolve, and the test would measure nothing.
func (h *harness) parseAt(text string) *config.Config {
	h.t.Helper()

	cfg, err := config.Parse([]byte(strings.Replace(text, "https://fake.invalid/v1", h.fakeURL+"/v1", 1)))
	if err != nil {
		h.t.Fatalf("config: %v", err)
	}
	return cfg
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
routes:
  demo:
    targets:
      - name: a
        model: provider/model-one
        base_url: https://fake.invalid/v1
        api_key: key-a
`)

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
routes:
  demo:
    targets:
      - name: a
        model: provider/model-one
        base_url: https://fake.invalid/v1
        api_key: key-a
`)

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
routes:
  demo:
    targets:
      - name: account-1
        model: provider/model-one
        base_url: https://fake.invalid/v1
        api_key: key-a
      - name: account-2
        model: provider/model-one
        base_url: https://fake.invalid/v1
        api_key: key-b
`)

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
routing:
  cooldown:
    status: {429: 3s}
    max: 30s
`)

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

// TestUnmappedStatusDoesNotCoolDown checks the other half of the rule: the window for an
// error status comes from routing.cooldown.status, so a status with no entry has no window.
// The target stays selectable, and the gateway sends no Retry-After of its own, because it
// did not apply one.
func TestUnmappedStatusDoesNotCoolDown(t *testing.T) {
	h := newHarness(t, `
routes:
  m:
    targets:
      - name: a
        model: m
        base_url: https://fake.invalid/v1
        api_key: key-a
routing:
  cooldown:
    status: {429: 10s}
    max: 30s
`)

	h.fake.Set("key-a", fakeupstream.Behavior{Status: 500, Body: `{"error":{"message":"internal"}}`})

	resp, _ := h.post(chatBody("m"))
	if resp.StatusCode != 500 {
		t.Fatalf("status: got %d, want 500", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != "" {
		t.Errorf("no window was applied, but Retry-After says %q", got)
	}
	if strings.Contains(h.log.String(), "cooldown") {
		t.Errorf("an unmapped status cooled the target down: %s", h.log.String())
	}

	resp, _ = h.post(chatBody("m"))
	if resp.StatusCode != 500 {
		t.Fatalf("second request: got %d", resp.StatusCode)
	}
	if h.fake.Attempts("key-a") != 2 {
		t.Errorf("target a was not selectable after an unmapped status: %d attempts", h.fake.Attempts("key-a"))
	}
}

// Test503IsNotInTheDefaultCooldownMap writes no cooldown section at all, so what it reads is
// the map the code defaults to rather than a map the test set up. 429 alone is the default,
// because a window names a target and a 503 normally says something about the service behind a
// key, not about the key. The response still has to reach the client as the upstream produced
// it, the target has to stay selectable, and one client request must stay one upstream attempt.
func Test503IsNotInTheDefaultCooldownMap(t *testing.T) {
	h := newHarness(t, `
routes:
  m:
    targets:
      - name: a
        model: m
        base_url: https://fake.invalid/v1
        api_key: key-a
`)

	upstream := `{"error":{"message":"content safety screening is temporarily unavailable"}}`
	h.fake.Set("key-a", fakeupstream.Behavior{Status: 503, Body: upstream})

	resp, out := h.post(chatBody("m"))
	if resp.StatusCode != 503 {
		t.Fatalf("status: got %d, want 503: a transient service failure must reach the client as it arrived", resp.StatusCode)
	}
	if out != upstream {
		t.Errorf("error body was rewritten: got %q, want %q", out, upstream)
	}
	if got := resp.Header.Get("Retry-After"); got != "" {
		t.Errorf("Retry-After: got %q, want none because no window was applied", got)
	}
	if h.fake.Attempts("key-a") != 1 {
		t.Errorf("attempts: got %d, want 1: the gateway retried inside one request", h.fake.Attempts("key-a"))
	}
	if strings.Contains(h.log.String(), "cooldown") {
		t.Errorf("a 503 cooled the target down: %s", h.log.String())
	}

	// The second request is what makes this falsifiable: the first reaches the upstream
	// whatever the code does with the map, so only a request answered by a cooled target can
	// show that the target was or was not taken out of the pool.
	resp, out = h.post(chatBody("m"))
	if resp.StatusCode != 503 {
		t.Fatalf("second request: got %d, want 503 from the upstream, not from gateway state", resp.StatusCode)
	}
	if out != upstream {
		t.Errorf("second response body: got %q, want the upstream body again", out)
	}
	if h.fake.Attempts("key-a") != 2 {
		t.Errorf("the target was not selectable after a 503: %d attempts", h.fake.Attempts("key-a"))
	}
	if got := testutil.ToFloat64(h.metrics.CooldownEvents.WithLabelValues("m", "a", "Service Unavailable")); got != 0 {
		t.Errorf("cooldown_events_total: got %v, want 0", got)
	}
}

// TestZeroEntryMeansNoWindowFor503 is the same rule read from the file: an entry of 0 is not a
// window, so a generated config that writes 503: 0s behaves as if the entry were absent. The
// bug this guards against is a fallback that treats 0 as "nothing configured" and uses the
// default instead, which would make writing 0s mean something other than what it says.
func TestZeroEntryMeansNoWindowFor503(t *testing.T) {
	h := newHarness(t, `
routes:
  m:
    targets:
      - name: a
        model: m
        base_url: https://fake.invalid/v1
        api_key: key-a
routing:
  cooldown:
    status: {429: 10s, 503: 0s}
    max: 30s
`)

	h.fake.Set("key-a", fakeupstream.Behavior{Status: 503,
		Body: `{"error":{"message":"content safety screening is temporarily unavailable"}}`})

	resp, out := h.post(chatBody("m"))
	if resp.StatusCode != 503 {
		t.Fatalf("status: got %d, want 503", resp.StatusCode)
	}
	if !strings.Contains(out, "content safety") {
		t.Errorf("error body not forwarded: %s", out)
	}
	if got := resp.Header.Get("Retry-After"); got != "" {
		t.Errorf("Retry-After: got %q, want none for an entry of 0", got)
	}

	resp, _ = h.post(chatBody("m"))
	if resp.StatusCode != 503 {
		t.Fatalf("second request: got %d, want 503 from the upstream", resp.StatusCode)
	}
	if h.fake.Attempts("key-a") != 2 {
		t.Errorf("a 0 entry took the target out of the pool: %d attempts", h.fake.Attempts("key-a"))
	}
}

// TestUnmappedStatusStillHonoursAnUpstreamRetryAfter is the half that taking 503 out of the map
// does not change: the map only supplies the prior for the silent case. A 503 that names a
// window is a provider that said how long to wait, so that window applies, the client is told
// the same number, and the request that lands inside it is answered from gateway state with no
// upstream call.
func TestUnmappedStatusStillHonoursAnUpstreamRetryAfter(t *testing.T) {
	h := newHarness(t, `
routes:
  m:
    targets:
      - name: a
        model: m
        base_url: https://fake.invalid/v1
        api_key: key-a
routing:
  cooldown:
    max: 30s
`)

	h.fake.Set("key-a", fakeupstream.Behavior{Status: 503,
		Headers: map[string]string{"Retry-After": "2"},
		Body:    `{"error":{"message":"content safety screening is temporarily unavailable"}}`})

	resp, out := h.post(chatBody("m"))
	if resp.StatusCode != 503 {
		t.Fatalf("status: got %d, want 503", resp.StatusCode)
	}
	if !strings.Contains(out, "content safety") {
		t.Errorf("error body not forwarded: %s", out)
	}
	if resp.Header.Get("Retry-After") != "2" {
		t.Errorf("Retry-After: got %q, want 2 as the upstream sent it", resp.Header.Get("Retry-After"))
	}
	if h.fake.Attempts("key-a") != 1 {
		t.Errorf("attempts: got %d, want 1", h.fake.Attempts("key-a"))
	}

	resp, out = h.post(chatBody("m"))
	if resp.StatusCode != 429 {
		t.Fatalf("second request: got %d, want 429 answered from state", resp.StatusCode)
	}
	if !strings.Contains(out, "all_targets_cooling_down") {
		t.Errorf("the answer is not the one gateway state gives: %s", out)
	}
	if h.fake.Attempts("key-a") != 1 {
		t.Errorf("the cooled target was contacted again: %d attempts", h.fake.Attempts("key-a"))
	}

	h.clock.Advance(2 * time.Second)
	h.post(chatBody("m"))
	if h.fake.Attempts("key-a") != 2 {
		t.Errorf("the target should be selectable at the boundary: %d attempts", h.fake.Attempts("key-a"))
	}
}

func TestInvalidRetryAfterFallsBackToConfiguredCooldown(t *testing.T) {
	h := newHarness(t, `
routes:
  m:
    targets:
      - name: a
        model: m
        base_url: https://fake.invalid/v1
        api_key: key-a
routing:
  cooldown:
    status: {429: 5s}
    max: 30s
`)

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
routes:
  m:
    targets:
      - name: a
        model: m
        base_url: https://fake.invalid/v1
        api_key: key-a
routing:
  cooldown:
    status: {429: 10s}
    max: 30s
`)

	h.fake.Set("key-a", fakeupstream.Behavior{Status: 429,
		Headers: map[string]string{"Retry-After": "36000"}, Body: `{"error":{"message":"limited"}}`})

	resp, _ := h.post(chatBody("m"))
	if got := resp.Header.Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After not capped: got %q, want 30", got)
	}
}

// TestHugeRetryAfterPassesThroughWhenClampIsOff is the off switch for the clamp. A target is not
// contacted while it is cooling, so a Retry-After is never verified: the clamp is what makes
// verification happen at all, which is why a default exists. With 0 the received value governs,
// and the gateway forwards it because the window it applied is the window it received.
func TestHugeRetryAfterPassesThroughWhenClampIsOff(t *testing.T) {
	h := newHarness(t, `
routes:
  m:
    targets:
      - name: a
        model: m
        base_url: https://fake.invalid/v1
        api_key: key-a
routing:
  cooldown:
    status: {429: 10s}
    max: 0s
`)

	h.fake.Set("key-a", fakeupstream.Behavior{Sequence: []fakeupstream.Behavior{
		{Status: 429, Headers: map[string]string{"Retry-After": "3600"},
			Body: `{"error":{"message":"limited"}}`},
		{Status: 200, Body: `{"ok":true}`},
	}})

	resp, _ := h.post(chatBody("m"))
	if resp.StatusCode != 429 {
		t.Fatalf("status: got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != "3600" {
		t.Errorf("Retry-After: got %q, want the value as received", got)
	}
	if !strings.Contains(h.log.String(), `"seconds":3600`) {
		t.Errorf("the window applied is not the window received: %s", h.log.String())
	}

	// Inside the window the value is not verified, so it governs on its own.
	h.clock.Advance(3599 * time.Second)
	h.post(chatBody("m"))
	if h.fake.Attempts("key-a") != 1 {
		t.Error("the target was contacted inside its own window")
	}

	h.clock.Advance(time.Second)
	resp, _ = h.post(chatBody("m"))
	if resp.StatusCode != 200 {
		t.Fatalf("after the window: got %d, want 200", resp.StatusCode)
	}
	if h.fake.Attempts("key-a") != 2 {
		t.Errorf("the value was not verified when the window ended: %d attempts", h.fake.Attempts("key-a"))
	}
}

// TestAllCoolingDoesNotTouchUpstream: a fully cooled model is answered from the
// gateway's own state, and Retry-After comes from the target that recovers
// first.
func TestAllCoolingDoesNotTouchUpstream(t *testing.T) {
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
	if envelope.Error.Code != "all_targets_cooling_down" || envelope.Error.Type != "rate_limit_error" {
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

// TestNoRetryAmplification is the core regression test: 50 client requests must never
// become more than 50 upstream attempts. The bound is read against the requests this test
// issued, not against a counter that a disconnect or a gateway rejection could move
// independently.
func TestNoRetryAmplification(t *testing.T) {
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
routes:
  m:
    targets:
      - name: a
        model: m
        base_url: https://fake.invalid/v1
        api_key: key-a
routing:
  cooldown:
    status: {%d: 15s}
    max: 60s
`, code))

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
routes:
  demo:
    targets:
      - name: a
        model: provider/model-one
        base_url: https://fake.invalid/v1
        api_key: key-a
`)

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

func TestEachTargetSendsItsOwnModelName(t *testing.T) {
	h := newHarness(t, `
routing:
  strategy: round_robin

routes:
  one-name:
    targets:
      - name: a
        model: provider/model-one
        base_url: https://fake.invalid/v1
        api_key: key-a
      - name: b
        model: provider/model-two
        base_url: https://fake.invalid/v1
        api_key: key-b
`)

	h.fake.Set("key-a", fakeupstream.Behavior{Status: 200, Body: `{"ok":true}`})
	h.fake.Set("key-b", fakeupstream.Behavior{Status: 200, Body: `{"ok":true}`})

	// round_robin is what makes the two requests land on the two targets. Under
	// least_active, sequential requests never overlap, so every tie goes to config order
	// and one target answers all of them.
	for range 2 {
		resp, _ := h.post(`{"model":"one-name","messages":[{"role":"user","content":"hi"}]}`)
		if resp.StatusCode != 200 {
			t.Fatalf("status: got %d", resp.StatusCode)
		}
	}

	for key, want := range map[string]string{
		"key-a": "provider/model-one",
		"key-b": "provider/model-two",
	} {
		sent := h.fake.LastBody(key)
		var parsed map[string]json.RawMessage
		if err := json.Unmarshal([]byte(sent), &parsed); err != nil {
			t.Fatalf("upstream received invalid JSON: %s", sent)
		}
		if string(parsed["model"]) != `"`+want+`"` {
			t.Errorf("%s: the model sent upstream is %s, want %s", key, parsed["model"], want)
		}
	}
}

// The config file holds the key now, so this is the case the security model depends on: a value
// that is a secret in the file must not reach a log line. The key here is an obviously fake fixture
// value, and the check is that it is absent from every line the gateway wrote.
func TestLogsDoNotContainKeysOrPrompts(t *testing.T) {
	h := newHarness(t, `
routes:
  m:
    targets:
      - name: a
        model: m
        base_url: https://fake.invalid/v1
        api_key: super-secret-key-12345
`)

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
routes:
  m:
    targets:
      - name: a
        model: m
        base_url: https://fake.invalid/v1
        api_key: key-a
`)

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

	// The name a client sent is not a metric label. A label value is never removed, so a
	// client that sends a different string on every request would grow the registry
	// forever. The label is the constant, and the name is logged.
	if got := testutil.ToFloat64(h.metrics.GatewayRejected.WithLabelValues(metrics.NoRoute, "model_not_found")); got != 1 {
		t.Errorf("model_not_found at route=none: got %v, want 1", got)
	}
	if got := testutil.ToFloat64(h.metrics.GatewayRejected.WithLabelValues("nope", "model_not_found")); got != 0 {
		t.Errorf("the name a client sent became a label value: got %v", got)
	}
	if !strings.Contains(h.log.String(), `"requested":"nope"`) {
		t.Errorf("the attempted name is not in the log: %s", h.log.String())
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
routes:
  m:
    targets:
      - name: a
        model: m
        base_url: https://fake.invalid/v1
        api_key: key-a
`)

	w := &failingWriter{header: http.Header{}, err: errors.New("client went away")}
	req, err := http.NewRequest(http.MethodPost, h.gateway.URL+"/v1/chat/completions", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	h.handler.reject(w, req, http.StatusTooManyRequests, "all_targets_cooling_down",
		"rate_limit_error", "No healthy targets are currently available", "m", "test-id", true)

	if got := testutil.ToFloat64(h.metrics.ClientDisconnects.WithLabelValues("m", metrics.NoTarget)); got != 1 {
		t.Errorf("client_disconnects_total: got %v, want 1", got)
	}
	if !strings.Contains(h.log.String(), "client_disconnect") {
		t.Errorf("the failed write was not logged: %s", h.log.String())
	}
}

func TestHealthAndReadinessSurviveCooldown(t *testing.T) {
	h := newHarness(t, `
routes:
  m:
    targets:
      - name: a
        model: m
        base_url: https://fake.invalid/v1
        api_key: key-a
`)

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

func TestModelsListingReportsRoutesAndTheirModels(t *testing.T) {
	h := newHarness(t, `
routes:
  mixed:
    targets:
      - name: a
        model: provider/model-one
        base_url: https://fake.invalid/v1
        api_key: key-a
      - name: b
        model: provider/model-two
        base_url: https://fake.invalid/v1
        api_key: key-b
  qwen3:8b:
    targets:
      - name: local
        model: qwen3:8b
        base_url: https://fake.invalid/v1
        api_key: key-a
`)

	resp, body := h.get("/v1/models")
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d", resp.StatusCode)
	}
	var listing struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string   `json:"id"`
			Models  []string `json:"models"`
			Targets int      `json:"targets"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &listing); err != nil {
		t.Fatalf("listing is not JSON: %v (%s)", err, body)
	}
	if listing.Object != "list" || len(listing.Data) != 2 {
		t.Errorf("listing: got %s", body)
		return
	}

	// Route names in sorted order. mixed has two models behind one name, so the listing has to say
	// them; qwen3:8b has one model and that model is the route name, so models would only repeat
	// the id and is left out.
	if listing.Data[0].ID != "mixed" || len(listing.Data[0].Models) != 2 ||
		listing.Data[0].Models[0] != "provider/model-one" || listing.Data[0].Models[1] != "provider/model-two" ||
		listing.Data[0].Targets != 2 {
		t.Errorf("route with two models: got %s", body)
	}
	if listing.Data[1].ID != "qwen3:8b" || listing.Data[1].Models != nil || listing.Data[1].Targets != 1 {
		t.Errorf("route with one model: got %s", body)
	}

	// A model name is not a route name, so it is not a name a client can send.
	h.fake.Set("key-a", fakeupstream.Behavior{Status: 200, Body: `{"ok":true}`})
	resp, _ = h.post(chatBody("provider/model-one"))
	if resp.StatusCode != 404 {
		t.Errorf("model name request: got %d, want 404", resp.StatusCode)
	}
}
