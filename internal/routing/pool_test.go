// SPDX-License-Identifier: AGPL-3.0-only

package routing

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/40960/plainroute/internal/config"
)

// fakeClock lets a test move time without sleeping.
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

func testPool(strategy string, names []string, clock *fakeClock) *Pool {
	pool := &Pool{
		Route:    "demo",
		strategy: strategy,
		now:      clock.Now,
	}
	store := NewStateStore()
	for _, name := range names {
		pool.Targets = append(pool.Targets, &Target{
			Name:  name,
			Model: "demo",
			state: store.Get("demo", name),
		})
	}
	return pool
}

func TestLeastActivePrefersTheQuietestDeployment(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1700000000, 0)}
	pool := testPool(StrategyLeastActive, []string{"a", "b", "c"}, clock)

	// Nothing has been released yet, so each pick takes the quietest target;
	// ties resolve to configuration order, which keeps the sequence reproducible.
	want := []string{"a", "b", "c"}
	for i, expect := range want {
		d, ok := pool.Pick()
		if !ok {
			t.Fatalf("pick %d: not selectable", i)
		}
		if d.Name != expect {
			t.Errorf("pick %d: got %s, want %s", i, d.Name, expect)
		}
	}

	// a and b have been released, c still holds one request, so the quietest is
	// a again.
	for _, name := range []string{"a", "b"} {
		pool.Release(pool.Targets[indexOf(pool, name)])
	}
	d, _ := pool.Pick()
	if d.Name != "a" {
		t.Errorf("after releases: got %s, want a (c still holds a request)", d.Name)
	}
	pool.Release(d)
}

func TestRoundRobinRotatesHealthyDeployments(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1700000000, 0)}
	pool := testPool(StrategyRoundRobin, []string{"a", "b", "c"}, clock)

	for _, expect := range []string{"a", "b", "c"} {
		d, ok := pool.Pick()
		if !ok {
			t.Fatalf("pick: not selectable")
		}
		if d.Name != expect {
			t.Errorf("got %s, want %s", d.Name, expect)
		}
	}

	// Cooldown removes c from rotation; the remaining targets keep their
	// relative order instead of restarting from the top.
	pool.Cooldown(pool.Targets[indexOf(pool, "c")], 10*time.Second, "rate_limit")
	for _, expect := range []string{"a", "b"} {
		d, ok := pool.Pick()
		if !ok {
			t.Fatalf("pick: not selectable")
		}
		if d.Name != expect {
			t.Errorf("while c is cooling: got %s, want %s", d.Name, expect)
		}
	}
}

func TestCoolingDownDeploymentIsNotSelectedAndRejoinsAfterRecovery(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1700000000, 0)}
	pool := testPool(StrategyLeastActive, []string{"a", "b"}, clock)

	a := pool.Targets[0]
	pool.Cooldown(a, 10*time.Second, "rate_limit")

	d, ok := pool.Pick()
	if !ok {
		t.Fatal("expected b to be selectable")
	}
	if d.Name != "b" {
		t.Errorf("got %s, want b", d.Name)
	}
	pool.Release(d)

	remaining, cooling := a.state.Remaining(clock.Now())
	if !cooling || remaining != 10*time.Second {
		t.Errorf("cooldown: got %v remaining, cooling=%v", remaining, cooling)
	}

	// Just before recovery a is still out.
	clock.Advance(10*time.Second - time.Nanosecond)
	if _, ok := pool.Pick(); !ok {
		t.Fatal("b should still be selectable")
	}

	// Exactly at the cooldown boundary a is back in the pool.
	clock.Advance(time.Nanosecond)
	d, ok = pool.Pick()
	if !ok {
		t.Fatal("expected a to be selectable after recovery")
	}
	if d.Name != "a" {
		t.Errorf("after recovery: got %s, want a", d.Name)
	}
	pool.Release(d)
}

func TestEarliestRecoveryUsesTheSoonestDeployment(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1700000000, 0)}
	pool := testPool(StrategyLeastActive, []string{"a", "b", "c"}, clock)

	pool.Cooldown(pool.Targets[0], 30*time.Second, "rate_limit")
	pool.Cooldown(pool.Targets[1], 12*time.Second, "rate_limit")
	pool.Cooldown(pool.Targets[2], 25*time.Second, "rate_limit")

	remaining, ok := pool.EarliestRecovery()
	if !ok {
		t.Fatal("expected a target to be cooling down")
	}
	if remaining != 12*time.Second {
		t.Errorf("got %s, want 12s", remaining)
	}

	if _, ok := pool.Pick(); ok {
		t.Error("a pool where every target is cooling must not select one")
	}

	clock.Advance(12 * time.Second)
	remaining, ok = pool.EarliestRecovery()
	if !ok || remaining != 13*time.Second {
		t.Errorf("after b recovers: got %s ok=%v, want 13s", remaining, ok)
	}
}

func TestMaxActiveLimitsASingleDeployment(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1700000000, 0)}
	pool := testPool(StrategyLeastActive, []string{"a", "b"}, clock)
	pool.Targets[0].MaxActive = 1
	pool.Targets[1].MaxActive = 1

	d, ok := pool.Pick()
	if !ok || d.Name != "a" {
		t.Fatalf("first pick: got %v %v", d, ok)
	}
	d2, ok := pool.Pick()
	if !ok || d2.Name != "b" {
		t.Fatalf("second pick: got %v %v, want b", d2, ok)
	}
	if _, ok := pool.Pick(); ok {
		t.Error("a is at its limit, b is at its limit: nothing should be selectable")
	}

	pool.Release(d)
	d3, ok := pool.Pick()
	if !ok || d3.Name != "a" {
		t.Errorf("after release: got %v %v, want a", d3, ok)
	}
	pool.Release(d3)
	pool.Release(d2)
}

// TestConcurrentSelection is the race-detector coverage for the counter: many
// goroutines acquire at once, and no target may ever hold more requests
// than its limit.
func TestConcurrentSelection(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1700000000, 0)}
	pool := testPool(StrategyLeastActive, []string{"a", "b", "c"}, clock)
	for _, d := range pool.Targets {
		d.MaxActive = 4
	}

	var holders sync.WaitGroup
	var maxObserved atomic.Int64
	var overLimit atomic.Int64
	var successes atomic.Int64

	start := make(chan struct{})
	for i := 0; i < 120; i++ {
		holders.Add(1)
		go func() {
			defer holders.Done()
			<-start
			d, ok := pool.Pick()
			if !ok {
				return
			}
			successes.Add(1)
			if held := int64(d.state.Active()); held > int64(d.MaxActive) {
				overLimit.Add(1)
			}
			if held := int64(d.state.Active()); held > maxObserved.Load() {
				maxObserved.Store(held)
			}
			// Hold the slot briefly so several goroutines overlap.
			time.Sleep(1 * time.Millisecond)
			if held := int64(d.state.Active()); held > int64(d.MaxActive) {
				overLimit.Add(1)
			}
			pool.Release(d)
		}()
	}
	close(start)
	holders.Wait()

	if overLimit.Load() != 0 {
		t.Errorf("max_active violated %d times (max observed %d, limit 4)", overLimit.Load(), maxObserved.Load())
	}
	for _, d := range pool.Targets {
		if active := d.state.Active(); active != 0 {
			t.Errorf("%s leaked %d active requests", d.Name, active)
		}
	}
	if successes.Load() == 0 {
		t.Error("no concurrent selection happened")
	}
}

// TestStateSurvivesReload checks that a config reload does not reset a cooldown
// that is still in effect for a target that is still configured.
func TestStateSurvivesReload(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1700000000, 0)}
	store := NewStateStore()

	cfg, err := config.Parse([]byte(`
routes:
  demo:
    targets:
      - name: account-1
        model: provider/model-one
        base_url: http://127.0.0.1:8081
        api_key_env: K1
      - name: account-2
        model: provider/model-one
        base_url: http://127.0.0.1:8082
        api_key_env: K2
`), config.Env{"K1": "k1", "K2": "k2"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	first := New(cfg, store, clock.Now)
	pool := first.routes["demo"]
	pool.Cooldown(pool.Targets[0], 10*time.Second, "rate_limit")

	reloaded, err := config.Parse([]byte(`
routes:
  demo:
    targets:
      - name: account-1
        model: provider/model-one
        base_url: http://127.0.0.1:8081
        api_key_env: K1
      - name: account-2
        model: provider/model-one
        base_url: http://127.0.0.1:8082
        api_key_env: K2
      - name: account-3
        model: provider/model-one
        base_url: http://127.0.0.1:8083
        api_key_env: K3
`), config.Env{"K1": "k1", "K2": "k2", "K3": "k3"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	second := New(reloaded, store, clock.Now)

	remaining, cooling := second.routes["demo"].Targets[0].state.Remaining(clock.Now())
	if !cooling || remaining != 10*time.Second {
		t.Errorf("reload lost the cooldown: remaining=%v cooling=%v", remaining, cooling)
	}

	reloadedPool, ok := second.Lookup("demo")
	if !ok {
		t.Fatal("lookup after reload failed")
	}
	d, ok := reloadedPool.Pick()
	if !ok || d.Name != "account-2" {
		t.Errorf("reload: got %v %v, want account-2", d, ok)
	}
	reloadedPool.Release(d)
}

func TestLookupUsesRouteNamesOnly(t *testing.T) {
	cfg, err := config.Parse([]byte(`
routes:
  demo:
    targets:
      - name: a
        model: provider/model-one
        base_url: http://127.0.0.1:8081
        api_key_env: K
  two:
    targets:
      - name: b
        model: provider/model-two
        base_url: http://127.0.0.1:8081
        api_key_env: K
`), config.Env{"K": "k"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	r := New(cfg, nil, time.Now)

	for _, name := range []string{"demo", "two"} {
		pool, ok := r.Lookup(name)
		if !ok {
			t.Fatalf("lookup %q: not found", name)
		}
		if pool.Route != name {
			t.Errorf("lookup %q resolved to %q", name, pool.Route)
		}
	}

	// A model name is not a route name. Lookup used to accept it, which was possible only while
	// one alias mapped to exactly one model. A target carries its own model now, so the same model
	// name can sit behind several routes, and picking between them would need a tie-break a reader
	// cannot predict from the config.
	if _, ok := r.Lookup("provider/model-one"); ok {
		t.Error("a model name must not resolve to a route")
	}
	if _, ok := r.Lookup("unknown-model"); ok {
		t.Error("unknown route must not resolve")
	}
	if r.Strategy() != StrategyLeastActive {
		t.Errorf("strategy: got %q, want %s", r.Strategy(), StrategyLeastActive)
	}
}

func indexOf(pool *Pool, name string) int {
	for i, d := range pool.Targets {
		if d.Name == name {
			return i
		}
	}
	return -1
}
