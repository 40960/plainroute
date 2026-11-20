// SPDX-License-Identifier: AGPL-3.0-only

package proxy

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/40960/plainroute/internal/metrics"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

const chainConfig = `
fallbacks:
  enabled: true
  max_hops: 2
routes:
  a:
    fallbacks: [b]
    targets:
      - name: a1
        model: a1
        base_url: https://fake.invalid/v1
        api_key: key-a
  b:
    targets:
      - name: b1
        model: b1
        base_url: https://fake.invalid/v1
        api_key: key-b
`

var errTransportForTest = errors.New("connection refused")

func TestModelsListingIsNotCountedAsARejection(t *testing.T) {
	h := newHarness(t, chainConfig)
	resp, _ := h.get("/v1/models")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("models status: %d", resp.StatusCode)
	}
	for _, pair := range [][2]string{{metrics.NoRoute, "models_list"}, {metrics.NoRoute, "method_not_allowed"}} {
		if got := testutil.ToFloat64(h.metrics.GatewayRejected.WithLabelValues(pair[0], pair[1])); got != 0 {
			t.Errorf("gateway_rejected{%s,%s}: got %v, want 0 for a successful list", pair[0], pair[1], got)
		}
	}

	resp, _ = h.post(chatBody("a"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status: %d", resp.StatusCode)
	}
}

func TestLocalRejectionsAreCountedAndSynthesizedUpstreamErrorsAreNot(t *testing.T) {
	h := newHarness(t, chainConfig)
	_, _ = h.post(`{"model":"a","messages":[]}garbage`)
	_, _ = h.post(`{"messages":[]}`)
	if got := testutil.ToFloat64(h.metrics.GatewayRejected.WithLabelValues(metrics.NoRoute, "invalid_json")); got != 1 {
		t.Errorf("invalid_json: got %v, want 1", got)
	}
	if got := testutil.ToFloat64(h.metrics.GatewayRejected.WithLabelValues(metrics.NoRoute, "missing_model")); got != 1 {
		t.Errorf("missing_model: got %v, want 1", got)
	}

	// An upstream transport failure produces a synthesized 502 after an upstream
	// call, so it must not be counted as a rejection made without one.
	h = newHarness(t, chainConfig)
	h.handler.Generation().client = &http.Client{Transport: stubTransport{err: errTransportForTest}}
	resp, _ := h.post(chatBody("a"))
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status: got %d, want 502", resp.StatusCode)
	}
	if got := testutil.ToFloat64(h.metrics.GatewayRejected.WithLabelValues("a", "upstream_error")); got != 0 {
		t.Errorf("gateway_rejected{upstream_error}: got %v, want 0", got)
	}
}

func TestChainUnavailabilityUsesEveryPool(t *testing.T) {
	h := newHarness(t, chainConfig)
	g := h.handler.Generation()
	a := h.pool("a")
	b := h.pool("b")

	// Hold a slot in the primary by capacity, and cool the fallback. Reporting only
	// the last pool would call this all_targets_cooling_down and invent a Retry-After.
	a.Targets[0].MaxActive = 1
	held, ok := a.Pick()
	if !ok {
		t.Fatal("primary pick: not selectable")
	}
	h.handler.applyCooldown(g.cfg, b, b.Targets[0], 3*time.Second, "rate_limit", "id")

	resp, body := h.post(chatBody("a"))
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status: got %d, want 429", resp.StatusCode)
	}
	if !strings.Contains(body, "all_targets_busy") {
		t.Errorf("chain with a busy pool was reported as only cooling: %s", body)
	}
	if got := resp.Header.Get("Retry-After"); got != "" {
		t.Errorf("a busy chain got an invented Retry-After: %q", got)
	}
	a.Release(held)

	// With both pools cooling, the code is cooling and Retry-After is the earliest
	// real recovery across the chain (3s from the fallback, not 10s from the primary).
	h.handler.applyCooldown(g.cfg, a, a.Targets[0], 10*time.Second, "rate_limit", "id")
	resp, body = h.post(chatBody("a"))
	if !strings.Contains(body, "all_targets_cooling_down") {
		t.Errorf("all-cooling chain: %s", body)
	}
	if got := resp.Header.Get("Retry-After"); got != "3" {
		t.Errorf("Retry-After: got %q, want 3 across the whole chain", got)
	}
}

func TestMetricsPathStaysUngatedEvenUnderV1(t *testing.T) {
	h := newHarness(t, `
auth:
  api_key: caller-key
metrics:
  path: /v1/metrics
routes:
  m:
    targets:
      - name: a
        model: m
        base_url: https://fake.invalid/v1
        api_key: key-a
`)
	resp, body := h.get("/v1/metrics")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics status: %d, want 200 without an API key", resp.StatusCode)
	}
	if !strings.Contains(body, "go_goroutines") {
		t.Errorf("the metrics page did not answer: %s", body)
	}
}
