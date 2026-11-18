// SPDX-License-Identifier: AGPL-3.0-only

package proxy

import (
	"net/http"
	"testing"

	"github.com/40960/plainroute/internal/config"
	"github.com/40960/plainroute/internal/routing"
)

// These tests check the header rules as behaviour, not as a list of names. A header
// filter is only trustworthy if a test can show what it forwards and what it drops,
// including the cases a client controls.

func TestHopByHopHeadersAreDroppedInBothDirections(t *testing.T) {
	cfg, err := config.Parse([]byte("routes:\n  m:\n    targets:\n      - name: a\n        model: m\n        base_url: https://fake.invalid/v1\n        api_key: key-a\n"))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	d := &routing.Target{Name: "a", APIKey: "key-a"}

	src := http.Header{}
	src.Set("Connection", "keep-alive")
	src.Set("Keep-Alive", "timeout=5")
	src.Set("Upgrade", "websocket")
	src.Set("Transfer-Encoding", "chunked")
	src.Set("Proxy-Authorization", "Bearer client-proxy-key")
	src.Set("TE", "trailers")
	src.Set("Trailer", "X-Anything")
	src.Set("X-Forwarded-Host", "kept")

	dst := http.Header{}
	copyRequestHeaders(dst, src, cfg, d, "id-1")

	for _, name := range []string{"Connection", "Keep-Alive", "Upgrade", "Transfer-Encoding", "Proxy-Authorization", "TE", "Trailer"} {
		if got := dst.Get(name); got != "" {
			t.Errorf("request: %s was forwarded as %q; a per-connection header must not be", name, got)
		}
	}
	if dst.Get("X-Forwarded-Host") != "kept" {
		t.Errorf("request: a message header was dropped: %v", dst)
	}

	respSrc := http.Header{}
	respSrc.Set("Connection", "close")
	respSrc.Set("Transfer-Encoding", "chunked")
	respSrc.Set("X-RateLimit-Remaining", "3")

	respDst := http.Header{}
	copyResponseHeaders(respDst, respSrc)

	if respDst.Get("Connection") != "" || respDst.Get("Transfer-Encoding") != "" {
		t.Errorf("response: per-connection header leaked: %v", respDst)
	}
	if respDst.Get("X-RateLimit-Remaining") != "3" {
		t.Errorf("response: a message header was dropped: %v", respDst)
	}
}

// TestAuthorizationIsAlwaysTheConfiguredKey is the security rule that matters most:
// the client chooses the model, not the key. A client that sends its own Authorization
// must not be able to aim a request at a key the operator did not give it.
func TestAuthorizationIsAlwaysTheConfiguredKey(t *testing.T) {
	cfg, err := config.Parse([]byte("routes:\n  m:\n    targets:\n      - name: a\n        model: m\n        base_url: https://fake.invalid/v1\n        api_key: key-a\n"))
	if err != nil {
		t.Fatalf("config: %v", err)
	}

	src := http.Header{}
	src.Set("Authorization", "Bearer whatever-the-client-sent")

	dst := http.Header{}
	copyRequestHeaders(dst, src, cfg, &routing.Target{Name: "a", APIKey: "key-a"}, "id-1")

	if got := dst.Get("Authorization"); got != "Bearer key-a" {
		t.Errorf("Authorization: got %q, want the configured target key", got)
	}
}

func TestDenyModeForwardsEverythingExceptDeny(t *testing.T) {
	cfg, err := config.Parse([]byte(`headers:
  mode: deny
  deny: [Cookie, X-Forwarded-For]
routes:
  m:
    targets:
      - name: a
        model: m
        base_url: https://fake.invalid/v1
        api_key: key-a
`))
	if err != nil {
		t.Fatalf("config: %v", err)
	}

	src := http.Header{}
	src.Set("Cookie", "session=secret-value")
	src.Set("X-Forwarded-For", "10.0.0.1")
	src.Set("Accept-Encoding", "gzip")
	src.Set("Content-Length", "137")
	src.Set("Host", "gateway.example")

	dst := http.Header{}
	copyRequestHeaders(dst, src, cfg, &routing.Target{Name: "a", APIKey: "key-a"}, "id-2")

	if dst.Get("Cookie") != "" || dst.Get("X-Forwarded-For") != "" {
		t.Errorf("deny mode forwarded a denied header: %v", dst)
	}
	if dst.Get("Accept-Encoding") != "gzip" {
		t.Errorf("deny mode dropped a header it should forward: %v", dst)
	}
	if dst.Get("Content-Length") != "" {
		t.Errorf("Content-Length must not be forwarded: the body the gateway sends may differ from the one the client counted")
	}
	if dst.Get("Host") != "" {
		t.Errorf("Host must not be copied from the client request: %v", dst)
	}
}

func TestAllowModeForwardsOnlyWhatIsListed(t *testing.T) {
	cfg, err := config.Parse([]byte(`headers:
  mode: allow
  allow: [Accept, X-Request-Id]
routes:
  m:
    targets:
      - name: a
        model: m
        base_url: https://fake.invalid/v1
        api_key: key-a
`))
	if err != nil {
		t.Fatalf("config: %v", err)
	}

	src := http.Header{}
	src.Set("Accept", "application/json")
	src.Set("X-Request-Id", "id-3")
	src.Set("User-Agent", "something")

	dst := http.Header{}
	copyRequestHeaders(dst, src, cfg, &routing.Target{Name: "a", APIKey: "key-a"}, "id-3")

	if dst.Get("Accept") != "application/json" || dst.Get("X-Request-Id") != "id-3" {
		t.Errorf("allow mode dropped a listed header: %v", dst)
	}
	if dst.Get("User-Agent") != "" {
		t.Errorf("allow mode forwarded a header that was not listed: %v", dst)
	}
}

// A per-target header is what a provider requires that a client cannot control, for
// example an org id. It must survive, and it must not be able to smuggle a per-connection
// header through the back door.
func TestDeploymentAndExtraHeadersAreApplied(t *testing.T) {
	cfg, err := config.Parse([]byte(`headers:
  mode: deny
  extra:
    X-Plainroute-Test: gateway
routes:
  m:
    targets:
      - name: a
        model: m
        base_url: https://fake.invalid/v1
        api_key: key-a
        headers:
          X-Target-Header: target
          Connection: close
`))
	if err != nil {
		t.Fatalf("config: %v", err)
	}

	dst := http.Header{}
	copyRequestHeaders(dst, http.Header{}, cfg, &routing.Target{Name: "a", APIKey: "key-a", Headers: map[string]string{
		"X-Target-Header": "target",
		"Connection":      "close",
	}}, "id-4")

	if dst.Get("X-Target-Header") != "target" || dst.Get("X-Plainroute-Test") != "gateway" {
		t.Errorf("configured headers were not applied: %v", dst)
	}
	if dst.Get("Connection") != "" {
		t.Errorf("a configured header smuggled a per-connection header: %v", dst)
	}
}

func TestRequestIDForwardingCanBeTurnedOff(t *testing.T) {
	cfg, err := config.Parse([]byte(`headers:
  mode: deny
  forward_request_id: false
routes:
  m:
    targets:
      - name: a
        model: m
        base_url: https://fake.invalid/v1
        api_key: key-a
`))
	if err != nil {
		t.Fatalf("config: %v", err)
	}

	dst := http.Header{}
	copyRequestHeaders(dst, http.Header{}, cfg, &routing.Target{Name: "a", APIKey: "key-a"}, "id-5")
	if dst.Get("X-Request-Id") != "" {
		t.Errorf("request id forwarded while forwarding is off: %v", dst)
	}
}
