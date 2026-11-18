// SPDX-License-Identifier: AGPL-3.0-only

package proxy

import (
	"strings"
	"testing"

	"github.com/40960/plainroute/internal/config"
	"github.com/40960/plainroute/internal/fakeupstream"
)

// PlainRoute does not normalise a provider error. These tests are the guard for that rule: an
// OpenAI-shaped error, an NVIDIA-shaped error, a JSON shape this project has never seen, an RFC
// problem document, and a body that is not JSON at all must reach the client as the upstream wrote
// them, with the upstream's status. The only value PlainRoute changes on an error response is
// Retry-After, and only in the way the README documents.
//
// Every case runs on a single-target route, so an attempt count is the count for that target and
// nothing else: an internal retry would show up as a second attempt on the same key.

const singleTarget = `
routes:
  m:
    targets:
      - name: a
        model: m
        base_url: https://fake.invalid/v1
        api_key_env: KEY_A
`

func TestProviderErrorShapesAreForwardedUnchanged(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		code        int
		contentType string
	}{
		{
			"the shape a rate limiter writes",
			`{"status":429,"title":"Too Many Requests"}`, 429,
			"application/json",
		},
		{
			"the shape a service writes for its safety screener",
			`{"error":{"message":"content safety screening is temporarily unavailable"}}`, 503,
			"application/json",
		},
		{
			"a JSON shape this project does not know",
			`{"detail":"qwen code 40001","request_id":"abc"}`, 400,
			"application/json",
		},
		{
			"an RFC 7807 problem document",
			`{"type":"https://example.com/errors/limited","title":"Too Many Requests","status":429,"instance":"/v1/chat/completions"}`, 429,
			"application/json",
		},
		{
			"a body that is not JSON at all",
			"upstream said no", 502,
			"text/plain",
		},
	}

	for _, c := range cases {
		h := newHarness(t, singleTarget, config.Env{"KEY_A": "key-a"})
		h.fake.Set("key-a", fakeupstream.Behavior{Status: c.code, Body: c.body,
			Headers: map[string]string{"Content-Type": c.contentType}})

		resp, out := h.post(chatBody("m"))
		if resp.StatusCode != c.code {
			t.Errorf("%s: status: got %d, want %d (%s)", c.name, resp.StatusCode, c.code, out)
			continue
		}
		if out != c.body {
			t.Errorf("%s: body was rewritten: got %q, want the bytes the upstream sent", c.name, out)
			continue
		}
		if got := resp.Header.Get("Content-Type"); got != c.contentType {
			t.Errorf("%s: Content-Type: got %q, want %q", c.name, got, c.contentType)
		}
		if h.fake.Attempts("key-a") != 1 {
			t.Errorf("%s: attempts: got %d, want 1: the gateway retried inside one request", c.name, h.fake.Attempts("key-a"))
		}
	}
}

// TestErrorStatusIsNeverChangedBecauseTheGatewayHasNoTableOfKnownErrors: the passthrough path is
// one status comparison, not a map of recognised provider error types, so an error shape nobody
// here knows cannot become a gateway error. A 500 that came from the upstream is the upstream's
// 500, and that is the case where the distinction is worth checking.
func TestErrorStatusIsNeverChangedBecauseTheGatewayHasNoTableOfKnownErrors(t *testing.T) {
	body := `{"detail":"a shape nobody here knows","code":40001}`

	for _, code := range []int{400, 401, 403, 404, 409, 413, 422, 429, 500, 502, 503, 504} {
		h := newHarness(t, singleTarget, config.Env{"KEY_A": "key-a"})
		h.fake.Set("key-a", fakeupstream.Behavior{Status: code, Body: body})

		resp, out := h.post(chatBody("m"))
		if resp.StatusCode != code {
			t.Errorf("status %d: got %d, want the upstream's status (%s)", code, resp.StatusCode, out)
		}
		if out != body {
			t.Errorf("status %d: body was rewritten: got %q", code, out)
		}
		if h.fake.Attempts("key-a") != 1 {
			t.Errorf("status %d: attempts: got %d, want 1", code, h.fake.Attempts("key-a"))
		}
	}
}

// TestErrorBodyPassesThroughAndOnlyRetryAfterChanges is the whole rule in one request shape: the
// same body through three configs that differ only in the clamp. The body is the same bytes in
// every answer; what differs is the window the gateway reports, which is the only value PlainRoute
// is allowed to change on an error response.
func TestErrorBodyPassesThroughAndOnlyRetryAfterChanges(t *testing.T) {
	body := `{"status":429,"title":"Too Many Requests"}`

	for _, c := range []struct {
		name string
		cfg  string
		want string
	}{
		{
			"a value already under the default clamp is used as written",
			"",
			"900",
		},
		{
			"a value over the clamp is reported as the clamp",
			"routing:\n  cooldown:\n    max: 30s\n",
			"30",
		},
		{
			"with the clamp off the value is used as written",
			"routing:\n  cooldown:\n    max: 0s\n",
			"900",
		},
	} {
		h := newHarness(t, singleTarget+c.cfg, config.Env{"KEY_A": "key-a"})
		h.fake.Set("key-a", fakeupstream.Behavior{Status: 429, Body: body,
			Headers: map[string]string{"Retry-After": "900"}})

		resp, out := h.post(chatBody("m"))
		if resp.StatusCode != 429 {
			t.Fatalf("%s: status: got %d, want 429", c.name, resp.StatusCode)
		}
		if out != body {
			t.Errorf("%s: body was rewritten: got %q", c.name, out)
		}
		if got := resp.Header.Get("Retry-After"); got != c.want {
			t.Errorf("%s: Retry-After: got %q, want %s", c.name, got, c.want)
		}
		if h.fake.Attempts("key-a") != 1 {
			t.Errorf("%s: attempts: got %d, want 1", c.name, h.fake.Attempts("key-a"))
		}
	}
}

// TestStreamErrorIsForwardedAsTheUpstreamProducedIt: a stream that answers with an error status
// before any frame is copied is the same passthrough, and the bytes of an SSE body are not reformed
// into a JSON error the client would have to recognise.
func TestStreamErrorIsForwardedAsTheUpstreamProducedIt(t *testing.T) {
	h := newHarness(t, singleTarget, config.Env{"KEY_A": "key-a"})

	frames := []string{
		"data: {\"status\":503,\"title\":\"content safety screening is temporarily unavailable\"}\n\n",
		"data: [DONE]\n\n",
	}
	h.fake.Set("key-a", fakeupstream.Behavior{Status: 503, SSE: frames})

	resp, out := h.post(`{"model":"m","stream":true,"messages":[{"role":"user","content":"prompt-content"}]}`)
	if resp.StatusCode != 503 {
		t.Fatalf("status: got %d, want 503", resp.StatusCode)
	}
	want := strings.Join(frames, "")
	if out != want {
		t.Errorf("stream body was rewritten: got %q, want the bytes the upstream wrote", out)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type: got %q, want text/event-stream", got)
	}
	if h.fake.Attempts("key-a") != 1 {
		t.Errorf("attempts: got %d, want 1", h.fake.Attempts("key-a"))
	}
}
