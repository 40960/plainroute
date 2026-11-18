// SPDX-License-Identifier: AGPL-3.0-only

package proxy

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/40960/plainroute/internal/config"
	"github.com/40960/plainroute/internal/fakeupstream"
	"github.com/40960/plainroute/internal/metrics"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// The caller key decides who may use the gateway. It is not the target key, and these
// tests keep the two apart: the key that reaches the upstream is always the one the operator
// configured for the selected target.

const callerBody = `{"model":"m","messages":[{"role":"user","content":"prompt-content"}]}`

func TestCallerKeyGate(t *testing.T) {
	h := newHarness(t, `
auth:
  api_key_env: CALLER_KEY
routes:
  m:
    targets:
      - name: a
        model: m
        base_url: https://fake.invalid/v1
        api_key_env: DEPLOYMENT_KEY
`, config.Env{"CALLER_KEY": "caller-key", "DEPLOYMENT_KEY": "target-key"})

	// If the caller key were forwarded, the fake upstream would resolve the request by it
	// and answer with its own marker. It must not.
	h.fake.Set("caller-key", fakeupstream.Behavior{Status: 200, Body: `{"via":"caller-key"}`})
	h.fake.Set("target-key", fakeupstream.Behavior{Status: 200, Body: `{"via":"target-key"}`})

	cases := []struct {
		name    string
		header  string
		status  int
		needle  string
		message string
	}{
		{"no key presented", "", http.StatusUnauthorized, `"code":"invalid_api_key"`, "no request was sent to the upstream"},
		{"wrong key presented", "Bearer not-the-caller-key", http.StatusUnauthorized, `"type":"authentication_error"`, "the error shape is the OpenAI one"},
		{"correct key, Bearer form", "Bearer caller-key", http.StatusOK, `"via":"target-key"`, "the caller key is not forwarded upstream"},
		{"correct key, raw form", "caller-key", http.StatusOK, `"via":"target-key"`, "the raw form works the same way"},
	}

	for _, c := range cases {
		req, err := http.NewRequest(http.MethodPost, h.gateway.URL+"/v1/chat/completions", strings.NewReader(callerBody))
		if err != nil {
			t.Fatalf("%s: new request: %v", c.name, err)
		}
		if c.header != "" {
			req.Header.Set("Authorization", c.header)
		}

		resp, err := h.gateway.Client().Do(req)
		if err != nil {
			t.Fatalf("%s: request: %v", c.name, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != c.status {
			t.Errorf("%s: status got %d, want %d (%s)", c.name, resp.StatusCode, c.status, c.message)
		}
		if !strings.Contains(string(body), c.needle) {
			t.Errorf("%s: body does not contain %q: %s", c.name, c.needle, body)
		}
	}

	if h.fake.Attempts("caller-key") != 0 {
		t.Errorf("the caller key was used as an upstream key: %d attempts", h.fake.Attempts("caller-key"))
	}
	if h.fake.Attempts("target-key") != 2 {
		t.Errorf("upstream attempts: got %d, want 2", h.fake.Attempts("target-key"))
	}
	if got := testutil.ToFloat64(h.metrics.GatewayRejected.WithLabelValues(metrics.NoRoute, "invalid_api_key")); got != 2 {
		t.Errorf("gateway_rejected invalid_api_key: got %v, want 2", got)
	}
	if got := testutil.ToFloat64(h.metrics.RequestsTotal.WithLabelValues(metrics.NoRoute, metrics.NoTarget, "4xx")); got != 2 {
		t.Errorf("requests_total for the rejected requests: got %v, want 2", got)
	}

	// The gate counts what the gateway answered from its own state, so the two numbers do
	// not match, and that is the difference the metrics exist to show.
	if got := testutil.ToFloat64(h.metrics.UpstreamAttempts.WithLabelValues("m", "a", "2xx")); got != 2 {
		t.Errorf("upstream_attempts_total: got %v, want 2", got)
	}

	if strings.Contains(h.log.String(), "caller-key") {
		t.Errorf("the caller key appears in the log: %s", h.log.String())
	}
	if strings.Contains(h.log.String(), "not-the-caller-key") {
		t.Errorf("a presented key appears in the log: %s", h.log.String())
	}
}

// The health, readiness and metrics endpoints stay open while the gate is on, because a
// monitoring system that cannot authenticate is the ordinary case and those pages carry
// counts only.
func TestGateCoversTheAPIEndpointsOnly(t *testing.T) {
	h := newHarness(t, `
auth:
  api_key_env: CALLER_KEY
routes:
  m:
    targets:
      - name: a
        model: m
        base_url: https://fake.invalid/v1
        api_key_env: DEPLOYMENT_KEY
`, config.Env{"CALLER_KEY": "caller-key", "DEPLOYMENT_KEY": "target-key"})

	h.fake.Set("target-key", fakeupstream.Behavior{Status: 200, Body: `{"ok":true}`})

	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		_, body := h.get(path)
		if strings.Contains(body, "invalid_api_key") {
			t.Errorf("%s must not be gated: %s", path, body)
		}
	}

	// /v1/models is an API endpoint, so it is gated.
	_, body := h.get("/v1/models")
	if !strings.Contains(body, "invalid_api_key") {
		t.Errorf("/v1/models must be gated: %s", body)
	}

	req, err := http.NewRequest(http.MethodGet, h.gateway.URL+"/v1/models", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer caller-key")
	resp, err := h.gateway.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/v1/models with the key: got %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(out), `"object":"list"`) {
		t.Errorf("the model listing was not served: %s", out)
	}
}

// With nothing configured the gateway is open, which is what the documentation says, and a
// request that presents any key is served.
func TestGateIsOffWhenNothingIsConfigured(t *testing.T) {
	h := newHarness(t, `
routes:
  m:
    targets:
      - name: a
        model: m
        base_url: https://fake.invalid/v1
        api_key_env: DEPLOYMENT_KEY
`, config.Env{"DEPLOYMENT_KEY": "target-key"})

	h.fake.Set("target-key", fakeupstream.Behavior{Status: 200, Body: `{"via":"target-key"}`})

	req, err := http.NewRequest(http.MethodPost, h.gateway.URL+"/v1/chat/completions", strings.NewReader(callerBody))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer anything")

	resp, err := h.gateway.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d, want 200 with the gate off", resp.StatusCode)
	}
	if !strings.Contains(string(body), "target-key") {
		t.Errorf("the target key was not used upstream: %s", body)
	}
}

// A gate that is turned on by SIGHUP applies to new requests, and a request that was already
// handled is not retroactively gated.
func TestGateAppliesAfterReload(t *testing.T) {
	h := newHarness(t, `
routes:
  m:
    targets:
      - name: a
        model: m
        base_url: https://fake.invalid/v1
        api_key_env: DEPLOYMENT_KEY
`, config.Env{"DEPLOYMENT_KEY": "target-key"})

	h.fake.Set("target-key", fakeupstream.Behavior{Status: 200, Body: `{"ok":true}`})

	_, body := h.post(callerBody)
	if !strings.Contains(body, "ok") {
		t.Fatalf("before the reload: %s", body)
	}

	reloaded, err := config.Parse([]byte(`
auth:
  api_key_env: CALLER_KEY
routes:
  m:
    targets:
      - name: a
        model: m
        base_url: https://fake.invalid/v1
        api_key_env: DEPLOYMENT_KEY
`), config.Env{"CALLER_KEY": "caller-key", "DEPLOYMENT_KEY": "target-key"})
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	h.handler.Reload(reloaded)

	_, body = h.post(callerBody)
	if !strings.Contains(body, "invalid_api_key") {
		t.Errorf("after the reload the gate did not apply: %s", body)
	}
}
