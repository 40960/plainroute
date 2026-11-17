// SPDX-License-Identifier: AGPL-3.0-only

package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/40960/plainroute/internal/config"
	"github.com/40960/plainroute/internal/fakeupstream"
)

// TestRedirectIsNotFollowed proves the rule the README and SECURITY.md state: upstream
// URLs come only from configuration. A response that names another host must not cause
// the gateway to send the request body there.

func TestRedirectIsNotFollowed(t *testing.T) {
	h := newHarness(t, `
models:
  m:
    upstream_model: m
    deployments:
      - name: a
        base_url: https://fake.invalid/v1
        api_key_env: KEY_A
`, config.Env{"KEY_A": "key-a"})

	var mu sync.Mutex
	var hits int
	var hitBody string

	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		hitBody = string(readAllForTest(r.Body))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("the attacker host was contacted"))
	}))
	defer attacker.Close()

	h.fake.Set("key-a", fakeupstream.Behavior{
		Status:  http.StatusFound,
		Headers: map[string]string{"Location": attacker.URL + "/steal"},
		Body:    `{"note":"redirect"}`,
	})

	// The test client must not follow redirects either, otherwise the test would be
	// measuring the test client rather than the gateway.
	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	req, err := http.NewRequest(http.MethodPost, h.gateway.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"prompt-content"}]}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Errorf("status: got %d, want 302 as the upstream produced it", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != attacker.URL+"/steal" {
		t.Errorf("Location: got %q, want the upstream value forwarded unchanged", got)
	}
	if !strings.Contains(string(out), "redirect") {
		t.Errorf("the upstream body was not forwarded as it arrived: %s", out)
	}
	if h.fake.Attempts("key-a") != 1 {
		t.Errorf("upstream attempts: got %d, want 1", h.fake.Attempts("key-a"))
	}

	mu.Lock()
	defer mu.Unlock()
	if hits != 0 {
		t.Errorf("the gateway followed the redirect: %d requests reached %s with body %q", hits, attacker.URL, hitBody)
	}
}

// TestMetricsPageCarriesNoSecret checks the exposition page the way the log is checked:
// the labels are model, deployment and status class, so a key value must not appear
// anywhere on the page, and neither must prompt content.
func TestMetricsPageCarriesNoSecret(t *testing.T) {
	h := newHarness(t, `
models:
  m:
    upstream_model: m
    deployments:
      - name: a
        base_url: https://fake.invalid/v1
        api_key_env: KEY_A
`, config.Env{"KEY_A": "super-secret-key-value"})

	h.fake.Set("super-secret-key-value", fakeupstream.Behavior{Status: 200, Body: `{"ok":true}`})

	h.post(chatBody("m"))

	_, page := h.get("/metrics")
	if strings.Contains(page, "super-secret-key-value") {
		t.Errorf("the metrics page contains the key value: %s", page)
	}
	if strings.Contains(page, "prompt-content") {
		t.Errorf("the metrics page contains prompt content: %s", page)
	}
	if !strings.Contains(page, "plainroute_upstream_attempts_total") {
		t.Errorf("the metrics page does not report upstream attempts: %s", page)
	}
}

// TestBodyFieldsCannotChooseTheUpstreamHost is the behavioural half of the SSRF rule: a
// client may put a field named base_url, target or upstream in the request body, but the
// only place a URL comes from is configuration. The field is passed through as an unknown
// field, and the request still goes to the configured host.
func TestBodyFieldsCannotChooseTheUpstreamHost(t *testing.T) {
	h := newHarness(t, `
models:
  m:
    upstream_model: m
    deployments:
      - name: a
        base_url: https://fake.invalid/v1
        api_key_env: KEY_A
`, config.Env{"KEY_A": "key-a"})

	h.fake.Set("key-a", fakeupstream.Behavior{Status: 200, Body: `{"ok":true}`})

	body := `{"model":"m","base_url":"https://attacker.invalid/v1","upstream":"attacker","messages":[{"role":"user","content":"prompt-content"}]}`
	_, out := h.post(body)

	if !strings.Contains(out, `"ok":true`) {
		t.Errorf("response: got %s", out)
	}
	if h.fake.Attempts("key-a") != 1 {
		t.Errorf("upstream attempts: got %d, want 1", h.fake.Attempts("key-a"))
	}
	if got := h.fake.LastBody("key-a"); got != body {
		t.Errorf("the body was altered on the way upstream:\n got %s\nwant %s", got, body)
	}
}

func readAllForTest(r io.Reader) []byte {
	out, _ := io.ReadAll(r)
	return out
}
