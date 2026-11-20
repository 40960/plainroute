// SPDX-License-Identifier: AGPL-3.0-only

package proxy

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/40960/plainroute/internal/metrics"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

const affinityConfig = `
fallbacks:
  enabled: true
  max_hops: 2
routes:
  demo:
    fallbacks: [other]
    targets:
      - name: a
        model: model-a
        base_url: https://fake.invalid/v1
        api_key: key-a
      - name: b
        model: model-b
        base_url: https://fake.invalid/v1
        api_key: key-b
      - name: c
        model: model-c
        base_url: https://fake.invalid/v1
        api_key: key-c
      - name: d
        model: model-d
        base_url: https://fake.invalid/v1
        api_key: key-d
  other:
    targets:
      - name: e
        model: model-e
        base_url: https://fake.invalid/v1
        api_key: key-e
`

func (h *harness) postWithHeaders(body string, headers map[string]string) (*http.Response, string) {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.gateway.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := h.gateway.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, string(out)
}

func TestSessionAffinityHeadersAreAcceptedWhenTheyAgree(t *testing.T) {
	h := newHarness(t, affinityConfig)
	const session = "ses_probe_affinity_1234"
	headers := map[string]string{
		"X-Session-Affinity": session,
		"X-Session-Id":       session,
		"X-Opencode-Session": session,
	}
	resp, _ := h.postWithHeaders(chatBody("demo"), headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	total := testutil.ToFloat64(h.metrics.AffinityTotal.WithLabelValues("demo", "sticky")) +
		testutil.ToFloat64(h.metrics.AffinityTotal.WithLabelValues("demo", "spill"))
	if total != 1 {
		t.Errorf("affinity decisions: got %v, want 1", total)
	}
	if n := testutil.ToFloat64(h.metrics.AffinityTotal.WithLabelValues("demo", "none")); n != 0 {
		t.Errorf("none decisions: got %v, want 0", n)
	}
	if strings.Contains(h.log.String(), session) {
		t.Error("the session value appeared in the log")
	}
	_, page := h.get("/metrics")
	if strings.Contains(page, session) {
		t.Error("the session value appeared on the metrics page")
	}
	if !strings.Contains(page, "plainroute_affinity_total{") {
		t.Error("the affinity counter is not on the metrics page")
	}
}

func TestConflictingAndInvalidAffinityAreRefusedLocally(t *testing.T) {
	h := newHarness(t, affinityConfig)
	cases := []struct {
		name    string
		headers map[string]string
		code    string
	}{
		{"conflicting", map[string]string{
			"X-Session-Affinity": "ses_one", "X-Session-Id": "ses_two",
		}, "conflicting_session_affinity"},
		{"oversized", map[string]string{
			"X-Session-Affinity": strings.Repeat("x", 129),
		}, "invalid_session_affinity"},
		{"invalid characters", map[string]string{
			"X-Session-Affinity": "has space",
		}, "invalid_session_affinity"},
	}
	for idx, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := testutil.ToFloat64(h.metrics.UpstreamAttempts.WithLabelValues("demo", "a", "2xx"))
			resp, body := h.postWithHeaders(chatBody("demo"), c.headers)
			if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, c.code) {
				t.Errorf("status=%d body=%s, want 400 %s", resp.StatusCode, body, c.code)
			}
			after := testutil.ToFloat64(h.metrics.UpstreamAttempts.WithLabelValues("demo", "a", "2xx"))
			if after != before {
				t.Error("a refused affinity header still contacted an upstream")
			}
			want := 1
			if c.code == "invalid_session_affinity" {
				// Two cases share this code; compare against the count so far.
				for j := 0; j <= idx; j++ {
					if cases[j].code == c.code {
						want++
					}
				}
				want--
			}
			if got := testutil.ToFloat64(h.metrics.GatewayRejected.WithLabelValues(metrics.NoRoute, c.code)); got != float64(want) {
				t.Errorf("gateway_rejected{%s}: got %v, want %d", c.code, got, want)
			}
		})
	}
}

func TestNoAffinityUsesExistingStrategyAndCountsNone(t *testing.T) {
	h := newHarness(t, affinityConfig)
	resp, _ := h.post(chatBody("demo"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if got := testutil.ToFloat64(h.metrics.AffinityTotal.WithLabelValues("demo", "none")); got != 1 {
		t.Errorf("none decisions: got %v, want 1", got)
	}
}

func TestSpillThenReturnWhenPreferredIsCooling(t *testing.T) {
	h := newHarness(t, affinityConfig)
	const session = "ses_spill_probe"
	pool := h.pool("demo")

	// Discover the preferred target for this session through the same API the
	// gateway uses, hold it, and make it unavailable by cooldown.
	preferred, _, ok := pool.PickWithAffinity(session)
	if !ok {
		t.Fatal("no preferred target")
	}
	pool.Release(preferred)

	_, _ = h.postWithHeaders(chatBody("demo"), map[string]string{"X-Session-Affinity": session})
	if got := testutil.ToFloat64(h.metrics.UpstreamAttempts.WithLabelValues("demo", "a", "2xx")) +
		testutil.ToFloat64(h.metrics.UpstreamAttempts.WithLabelValues("demo", "b", "2xx")) +
		testutil.ToFloat64(h.metrics.UpstreamAttempts.WithLabelValues("demo", "c", "2xx")) +
		testutil.ToFloat64(h.metrics.UpstreamAttempts.WithLabelValues("demo", "d", "2xx")); got != 1 {
		t.Fatalf("attempts: got %v, want 1", got)
	}

	pool.Cooldown(preferred, 10*time.Second, "rate_limit")
	_, _ = h.postWithHeaders(chatBody("demo"), map[string]string{"X-Session-Affinity": session})
	if got := testutil.ToFloat64(h.metrics.AffinityTotal.WithLabelValues("demo", "spill")); got != 1 {
		t.Errorf("spill decisions: got %v, want 1", got)
	}
	h.clock.Advance(11 * time.Second)
	_, _ = h.postWithHeaders(chatBody("demo"), map[string]string{"X-Session-Affinity": session})
	if got := testutil.ToFloat64(h.metrics.AffinityTotal.WithLabelValues("demo", "sticky")); got != 2 {
		t.Errorf("sticky decisions: got %v, want 2", got)
	}
}

func TestAffinityFallbackUsesItsOwnRouteScopedPreference(t *testing.T) {
	h := newHarness(t, affinityConfig)
	const session = "ses_fallback_probe"
	pool := h.pool("demo")
	for _, target := range pool.Targets {
		pool.Cooldown(target, 10*time.Second, "rate_limit")
	}

	resp, _ := h.postWithHeaders(chatBody("demo"), map[string]string{"X-Session-Affinity": session})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fallback status: %d", resp.StatusCode)
	}
	attempts := float64(0)
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		attempts += testutil.ToFloat64(h.metrics.UpstreamAttempts.WithLabelValues(
			map[string]string{"a": "demo", "b": "demo", "c": "demo", "d": "demo", "e": "other"}[name],
			name, "2xx"))
	}
	if attempts != 1 {
		t.Errorf("upstream attempts: got %v, want 1 (no internal retry)", attempts)
	}
	if got := testutil.ToFloat64(h.metrics.AffinityTotal.WithLabelValues("other", "sticky")) +
		testutil.ToFloat64(h.metrics.AffinityTotal.WithLabelValues("other", "spill")); got != 1 {
		t.Errorf("fallback route affinity decisions: got %v, want 1", got)
	}
}

func TestAffinityMappingSurvivesHotReloadAndReorder(t *testing.T) {
	h := newHarness(t, affinityConfig)
	const session = "ses_reload_probe"
	_, _ = h.postWithHeaders(chatBody("demo"), map[string]string{"X-Session-Affinity": session})
	if !strings.Contains(h.log.String(), "affinity_decision") {
		t.Fatal("no affinity decision was logged")
	}

	// Same names and order: a reload must keep the mapping. Target reordering is
	// covered at the pool level, where the hash is computed.
	h.handler.Reload(h.parseAt(affinityConfig))
	_, _ = h.postWithHeaders(chatBody("demo"), map[string]string{"X-Session-Affinity": session})
	if got := testutil.ToFloat64(h.metrics.AffinityTotal.WithLabelValues("demo", "sticky")); got != 2 {
		t.Errorf("sticky decisions across reload: got %v, want 2", got)
	}
}
