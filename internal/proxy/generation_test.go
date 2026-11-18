// SPDX-License-Identifier: AGPL-3.0-only

package proxy

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/40960/plainroute/internal/fakeupstream"
)

// A generation is one whole configuration: the config, the registry built from it, and the transport
// built from it, as one value that a reload swaps. A request takes it once at its start and keeps it
// for its whole life, so a reload that happens while a request is running cannot change what that
// request is doing. Target state lives in the store, keyed by route and target name, so a target that
// is still configured keeps its cooldown and its count of requests in flight across a reload.
//
// What makes these tests able to answer the question is the pairing. Every generation has its own key
// and its own endpoint, so a record at the upstream pairs a key with an endpoint that belong together;
// a handler that read the generation twice can only produce a pairing that no configuration wrote.

const generationA = `
server:
  max_response_bytes: 4096
routes:
  gen:
    targets:
      - name: t
        model: gen
        base_url: https://fake.invalid/v1
        path: /gen-a
        api_key: key-a
        max_active: 2
`

// The same target name, with a different key, a different endpoint and a response limit small enough to
// cut a stream. The same name is the point: state is keyed by route and target name, so this is the
// same target under two configurations, and the only things that differ are the ones a generation
// carries.
const generationB = `
server:
  max_response_bytes: 10
routes:
  gen:
    targets:
      - name: t
        model: gen
        base_url: https://fake.invalid/v1
        path: /gen-b
        api_key: key-b
        max_active: 2
`

// TestARequestUsesTheGenerationItStartedWith puts a reload inside a request that is already running,
// and the upstream delay is what makes that order certain: the request has reached the upstream and
// the response has not come back yet, so every decision the request still has to make — how much body
// it may read, which key and endpoint it used, which state object it releases its slot on — happens
// after the swap. A request that took one generation is unaffected by the swap; a handler that read
// the generation again would cut this one at 10 bytes, the limit of the generation that is now in
// force, and that is the difference these checks look at.
func TestARequestUsesTheGenerationItStartedWith(t *testing.T) {
	h := newHarness(t, generationA)

	h.fake.Set("key-a", fakeupstream.Behavior{Status: 200, Body: "served-by-generation-a", Delay: 300 * time.Millisecond})
	h.fake.Set("key-b", fakeupstream.Behavior{Status: 200, Body: "served-by-generation-b"})

	started := h.handler.Generation()

	type answer struct {
		status int
		body   string
		err    error
	}
	answers := make(chan answer, 1)
	go func() {
		resp, err := h.gateway.Client().Post(h.gateway.URL+"/v1/chat/completions", "application/json",
			strings.NewReader(chatBody("gen")))
		if err != nil {
			answers <- answer{err: err}
			return
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		answers <- answer{status: resp.StatusCode, body: string(out)}
	}()

	// The poll is the point the request reached, not a wait chosen in advance. Once the upstream has
	// the request, the generation was already taken and the response has not been read yet, so the
	// swap below is inside the request.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && h.fake.Attempts("key-a") == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if h.fake.Attempts("key-a") == 0 {
		t.Fatal("the request never reached the upstream, so the reload was not inside the request")
	}

	reloaded := h.parseAt(generationB)
	h.handler.Reload(reloaded)

	if h.handler.Generation() == started {
		t.Fatal("the generation in force did not change, so this test did not swap anything")
	}
	if h.fake.Cancelled("key-a") {
		t.Fatal("the swap cancelled a request that had already started")
	}

	got := <-answers
	if got.err != nil {
		t.Fatalf("request: %v", got.err)
	}

	if got.status != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (%s)", got.status, got.body)
	}
	if got.body != "served-by-generation-a" {
		t.Errorf("the body the request served is the body of generation %s: the limit that ended it was the one in force at the read, not the one the request started with: %q",
			got.body, got.body)
	}

	record := h.fake.Last("key-a")
	if !strings.HasSuffix(record.Path, "/gen-a") {
		t.Errorf("the endpoint the request used is not the endpoint of the generation it started with: %s", record.Path)
	}
	if record.Headers["Authorization"] != "Bearer key-a" {
		t.Errorf("the key the request used is not the key of the generation it started with: %q", record.Headers["Authorization"])
	}
	if h.fake.Attempts("key-b") != 0 {
		t.Errorf("the request touched the key of the generation that came after it: %d attempts on key-b", h.fake.Attempts("key-b"))
	}

	// The slot was taken on the state object the request started with, and it is released on that same
	// object. A generation that carried its own store would leave this one holding a request that is
	// already finished, which is the leak this check is for.
	if active := started.registry.Pools()[0].Snapshot()[0].Active; active != 0 {
		t.Errorf("the state the request took its slot on still holds it: %d active", active)
	}
	if h.fake.Cancelled("key-a") {
		t.Errorf("the request was cancelled by the reload rather than completing: %s", h.log.String())
	}
}

// TestTargetStateSurvivesAReloadForTheSameTarget is the rule that a reload changes the configuration
// and not the state: a target that is still configured under the new generation keeps its cooldown,
// so the next request is answered from state with no upstream call. State is keyed by route and target
// name, which is why the same name under a different key is still the same target.
func TestTargetStateSurvivesAReloadForTheSameTarget(t *testing.T) {
	h := newHarness(t, generationA)

	h.fake.Set("key-a", fakeupstream.Behavior{Status: 429,
		Headers: map[string]string{"Retry-After": "30"},
		Body:    `{"error":{"message":"rate limited"}}`})
	h.fake.Set("key-b", fakeupstream.Behavior{Status: 200, Body: "served-by-generation-b"})

	resp, out := h.post(chatBody("gen"))
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("first request: got %d, want 429 (%s)", resp.StatusCode, out)
	}

	h.handler.Reload(h.parseAt(generationB))

	resp, out = h.post(chatBody("gen"))
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("the cooled target was contacted again after the reload: got %d (%s)", resp.StatusCode, out)
	}
	if !strings.Contains(out, "all_targets_cooling_down") {
		t.Errorf("the answer is not the one gateway state gives: %s", out)
	}
	if h.fake.Attempts("key-a") != 1 || h.fake.Attempts("key-b") != 0 {
		t.Errorf("attempts after a reload that only changed the key: key-a %d, key-b %d",
			h.fake.Attempts("key-a"), h.fake.Attempts("key-b"))
	}
}

// TestConcurrentRequestsDoNotMixGenerations is the invariant over many requests while the generation is
// being swapped. Every request pairs the key with the endpoint of the generation it started with, so a
// record that pairs key-b with the endpoint of generation A is a request that read the generation twice.
//
// The first half is deterministic: the test sets the generation, sends one request, and checks the
// pairing against the generation it set, which is what says a swapped generation is the one new
// requests use. The second half is the invariant: a handler that takes one snapshot cannot be caught by
// a test that forces an interleaving, because there is no window to force, so what this half can do is
// run enough requests across enough swaps that a handler that read the generation twice would show a
// pairing no configuration wrote.
func TestConcurrentRequestsDoNotMixGenerations(t *testing.T) {
	h := newHarness(t, generationA)

	h.fake.Set("key-a", fakeupstream.Behavior{Status: 200, Body: "served-by-generation-a"})
	h.fake.Set("key-b", fakeupstream.Behavior{Status: 200, Body: "served-by-generation-b"})

	a := h.parseAt(generationA)
	b := h.parseAt(generationB)

	// pairing is what the fake recorded for the key that answered, and each key belongs to one
	// generation, so the endpoint in that record has to be the endpoint of the same generation.
	pairing := func(key string) bool {
		return strings.HasSuffix(h.fake.Last(key).Path, "/gen-"+strings.TrimPrefix(key, "key-"))
	}

	// Which key a request reached is what the upstream recorded, so a request that answered with the
	// pairing of the generation in force at its start is not a coincidence: the test knows which pairing
	// to expect, and it can tell them apart because each key belongs to one generation.
	h.post(chatBody("gen"))
	if h.fake.Attempts("key-a") != 1 || !pairing("key-a") {
		t.Fatalf("the first request did not use the generation that was in force at its start: %s", h.fake.LastText("key-a"))
	}

	h.handler.Reload(b)
	h.post(chatBody("gen"))
	if h.fake.Attempts("key-b") != 1 || !pairing("key-b") {
		t.Fatalf("the request after the swap did not use the generation that is in force: %s", h.fake.LastText("key-b"))
	}

	stop := make(chan struct{})
	go func() {
		// The reloader has to be running while the requests run, otherwise there is no overlap and
		// the test measures a queue of single requests rather than concurrency.
		for {
			select {
			case <-stop:
				return
			default:
			}
			h.handler.Reload(b)
			h.handler.Reload(a)
		}
	}()

	mixed := 0
	used := map[string]int{}
	for i := 0; i < 200; i++ {
		before := map[string]int{"key-a": h.fake.Attempts("key-a"), "key-b": h.fake.Attempts("key-b")}
		h.post(chatBody("gen"))

		key := ""
		for name, count := range before {
			if h.fake.Attempts(name) > count {
				key = name
			}
		}
		if key == "" {
			t.Fatal("a request reached neither key, so it never left the gateway")
		}
		used[key]++

		// The record for that key is this request's, because the requests here are one at a time.
		if !pairing(key) {
			mixed++
		}
	}
	close(stop)

	if mixed != 0 {
		t.Errorf("%d of 200 requests paired a key with the endpoint of the other generation, so a request read the generation twice", mixed)
	}
	// Both generations have to be in use while the swap is running. A test that only ever used one of
	// them would pass the pairing check without having tested anything about concurrency.
	if used["key-a"] < 40 || used["key-b"] < 40 {
		t.Errorf("the requests did not span both generations while the swap was running: key-a %d, key-b %d",
			used["key-a"], used["key-b"])
	}
	for _, pool := range h.handler.Generation().registry.Pools() {
		for _, target := range pool.Snapshot() {
			if target.Active != 0 {
				t.Errorf("active counter leaked on %s: %d", target.Target, target.Active)
			}
		}
	}
}
