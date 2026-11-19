// SPDX-License-Identifier: AGPL-3.0-only

package proxy

import (
	"net/http"
	"testing"
	"time"

	"github.com/40960/plainroute/internal/fakeupstream"
)

// A change to a timeout has to be a new transport, not just a new configuration struct: the values a
// request uses are the ones the transport it was built with carries. The pair that makes this
// answerable is one upstream delay that is under the first timeout and over the second, so the same
// upstream answers on one side of the change and is cut off on the other. Reading the values off the
// two transport objects is what says the second set came from the new configuration rather than from
// a struct that was swapped while the transport stayed.
//
// The connect timeout is the one value a Transport does not keep readable, because the dialer is a
// closure inside it. That value is code-guaranteed: newTransport reads the generation's config and is
// called once per generation. It is not measured here, and this test says so instead of claiming it.
func TestATimeoutChangeBuildsANewTransportForNewRequests(t *testing.T) {
	h := newHarness(t, `
routes:
  gen:
    targets:
      - name: t
        model: gen
        base_url: https://fake.invalid/v1
        path: /gen-a
        api_key: key-a
timeouts:
  connect: 3s
  response_header: 5s
  stream_idle: 60s
  request: 0
`)

	// The same delay on both keys, so the only difference between the two requests is which
	// generation they started with.
	h.fake.Set("key-a", fakeupstream.Behavior{Status: 200, Body: "answered", Delay: 300 * time.Millisecond})
	h.fake.Set("key-b", fakeupstream.Behavior{Status: 200, Body: "answered", Delay: 300 * time.Millisecond})

	started := h.handler.Generation()
	startTransport, ok := started.client.Transport.(*http.Transport)
	if !ok {
		t.Fatal("the generation in force does not carry a transport")
	}
	if startTransport.ResponseHeaderTimeout != 5*time.Second {
		t.Fatalf("the transport this generation was built with: got %s, want 5s",
			startTransport.ResponseHeaderTimeout)
	}

	resp, body := h.post(chatBody("gen"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a request under the first transport: got %d (%s), want 200", resp.StatusCode, body)
	}

	reloaded := h.parseAt(`
routes:
  gen:
    targets:
      - name: t
        model: gen
        base_url: https://fake.invalid/v1
        path: /gen-a
        api_key: key-b
timeouts:
  connect: 1s
  response_header: 60ms
  stream_idle: 60s
  request: 0
`)
	h.handler.Reload(reloaded)

	after := h.handler.Generation()
	if after == started {
		t.Fatal("the generation in force did not change, so this test did not swap anything")
	}
	if after.client == started.client {
		t.Fatal("the client is the same object, so a timeout change cannot be a new transport")
	}
	if after.client.Transport == started.client.Transport {
		t.Fatal("the transport was reused, so the values a new request uses cannot be the new ones")
	}

	newTransport, ok := after.client.Transport.(*http.Transport)
	if !ok {
		t.Fatal("the new generation does not carry a transport")
	}
	if newTransport.ResponseHeaderTimeout != 60*time.Millisecond {
		t.Errorf("the new transport carries: got %s, want 60ms", newTransport.ResponseHeaderTimeout)
	}

	// The generation that a request already started with is not changed by the swap: the old transport
	// still holds the value it was built with, which is what rules out a single shared transport that
	// was updated in place.
	if startTransport.ResponseHeaderTimeout != 5*time.Second {
		t.Errorf("the transport a request already started with now carries: got %s, want 5s",
			startTransport.ResponseHeaderTimeout)
	}

	// And a new request answers from the transport of its own generation: the same upstream delay that
	// the first transport allowed is cut off by the second. This is the read that says the new values
	// are the ones in force, and it can only fail on a handler that read the transport of the old
	// generation.
	resp, body = h.post(chatBody("gen"))
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Errorf("a request after the change: got %d, want the timeout the new transport names (%s)",
			resp.StatusCode, body)
	}
	if h.fake.Attempts("key-b") != 1 {
		t.Errorf("the upstream recorded %d attempts on the key of the new generation, want 1",
			h.fake.Attempts("key-b"))
	}

	// A request that started before the change keeps its own generation, so a transport read after the
	// swap cannot be the one that decided this one.
	if h.fake.Attempts("key-a") != 1 {
		t.Errorf("the request under the first transport: got %d attempts, want 1", h.fake.Attempts("key-a"))
	}
}
