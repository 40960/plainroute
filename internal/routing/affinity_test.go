// SPDX-License-Identifier: AGPL-3.0-only

package routing

import (
	"strconv"
	"sync"
	"testing"
	"time"
)

func testSessionPool(strategy string) *Pool {
	return testPool(strategy, []string{"a", "b", "c", "d"}, &fakeClock{t: time.Unix(1700000000, 0)})
}

func TestAffinityMappingIsStableAndRouteScoped(t *testing.T) {
	const session = "ses_one"
	first := testSessionPool(StrategyRoundRobin)
	want := preferredTarget(first.Route, session, first.Targets)
	if want == nil {
		t.Fatal("no preferred target")
	}

	// Same route/target names, newly built pool (a process restart or reload).
	second := testSessionPool(StrategyRoundRobin)
	if got := preferredTarget(second.Route, session, second.Targets); got.Name != want.Name {
		t.Errorf("after recreation: got %s, want %s", got.Name, want.Name)
	}

	// Reordering the YAML must not change the mapping: the hash input is the name.
	second.Targets = []*Target{second.Targets[3], second.Targets[2], second.Targets[1], second.Targets[0]}
	if got := preferredTarget(second.Route, session, second.Targets); got.Name != want.Name {
		t.Errorf("after reorder: got %s, want %s", got.Name, want.Name)
	}

	// A different route is a different model/provider, so the preferred target is
	// computed in that route's own name space. With many sessions, at least some
	// must land on a different target; one session could coincide.
	other := testPool(StrategyRoundRobin, []string{"a", "b", "c", "d"}, &fakeClock{t: time.Unix(1700000000, 0)})
	other.Route = "another"
	different := 0
	for i := 0; i < 20; i++ {
		s := "ses_" + strconv.Itoa(i)
		if preferredTarget(first.Route, s, first.Targets).Name != preferredTarget(other.Route, s, other.Targets).Name {
			different++
		}
	}
	if different == 0 {
		t.Error("two routes produced identical mappings for all sessions")
	}
}

func TestAffinityMappingSpreadsSessions(t *testing.T) {
	pool := testSessionPool(StrategyRoundRobin)
	seen := map[string]int{}
	for i := 0; i < 100; i++ {
		name := preferredTarget(pool.Route, "ses_"+strconv.Itoa(i), pool.Targets).Name
		seen[name]++
	}
	if len(seen) != len(pool.Targets) {
		t.Errorf("sessions reached %d of %d targets: %v", len(seen), len(pool.Targets), seen)
	}
	for name, n := range seen {
		if n < 10 || n > 40 {
			t.Errorf("target %s got %d of 100 sessions", name, n)
		}
	}
}

func TestAffinityRendezvousRemapsOnlyAffectedSessions(t *testing.T) {
	pool := testSessionPool(StrategyRoundRobin)
	before := map[string]string{}
	for i := 0; i < 100; i++ {
		session := "ses_" + string(rune(i))
		before[session] = preferredTarget(pool.Route, session, pool.Targets).Name
	}

	// Adding a target changes only sessions for which it wins the hash; a plain
	// modulo over the target count would change nearly all of them.
	pool.Targets = append(pool.Targets, &Target{Name: "e", Model: "demo", state: NewStateStore().Get("demo", "e")})
	changed := 0
	for session, name := range before {
		if got := preferredTarget(pool.Route, session, pool.Targets).Name; got != name {
			changed++
		}
	}
	if changed > 30 {
		t.Errorf("adding one target remapped %d of 100 sessions", changed)
	}

	// Removing one changes only the sessions that preferred it.
	pool.Targets = pool.Targets[:4]
	changed = 0
	for session, name := range before {
		if got := preferredTarget(pool.Route, session, pool.Targets).Name; got != name {
			changed++
		}
	}
	if changed != 0 {
		t.Errorf("removing the added target changed %d sessions that never preferred it", changed)
	}
}

func TestAffinityStickyUntilUnavailableThenReturns(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1700000000, 0)}
	pool := testPool(StrategyRoundRobin, []string{"a", "b", "c", "d"}, clock)
	const session = "ses_stick"
	pref := preferredTarget(pool.Route, session, pool.Targets)

	for i := 0; i < 4; i++ {
		target, kind, ok := pool.PickWithAffinity(session)
		if !ok || kind != PickSticky || target.Name != pref.Name {
			t.Fatalf("pick %d: target=%v kind=%s ok=%v", i, target, kind, ok)
		}
		pool.Release(target)
	}

	// Capacity full: immediate spill, and the next request prefers the original
	// target again after the held slot is released.
	pref.MaxActive = 1
	held, _, ok := pool.PickWithAffinity(session)
	if !ok || held.Name != pref.Name {
		t.Fatalf("hold preferred: %v %v", held, ok)
	}
	target, kind, ok := pool.PickWithAffinity(session)
	if !ok || kind != PickSpill || target.Name == pref.Name {
		t.Fatalf("spill on capacity: target=%v kind=%s ok=%v", target, kind, ok)
	}
	pool.Release(target)
	pool.Release(held)
	target, kind, _ = pool.PickWithAffinity(session)
	if kind != PickSticky || target.Name != pref.Name {
		t.Errorf("after release: target=%v kind=%s", target, kind)
	}
	pool.Release(target)

	// Cooling: same immediate spill, then a return after real recovery.
	pool.Cooldown(pref, 10*time.Second, "rate_limit")
	target, kind, ok = pool.PickWithAffinity(session)
	if !ok || kind != PickSpill || target.Name == pref.Name {
		t.Fatalf("spill on cooldown: target=%v kind=%s ok=%v", target, kind, ok)
	}
	pool.Release(target)
	clock.Advance(11 * time.Second)
	target, kind, _ = pool.PickWithAffinity(session)
	if kind != PickSticky || target.Name != pref.Name {
		t.Errorf("after recovery: target=%v kind=%s", target, kind)
	}
	pool.Release(target)
}

func TestNoAffinityKeepsTheExistingStrategy(t *testing.T) {
	for _, strategy := range []string{StrategyRoundRobin, StrategyLeastActive} {
		plain := testPool(strategy, []string{"a", "b", "c", "d"}, &fakeClock{t: time.Unix(1700000000, 0)})
		withEmptyKey := testPool(strategy, []string{"a", "b", "c", "d"}, &fakeClock{t: time.Unix(1700000000, 0)})
		for i := 0; i < 8; i++ {
			want, ok := plain.Pick()
			got, kind, ok2 := withEmptyKey.PickWithAffinity("")
			if !ok || !ok2 || want.Name != got.Name || kind != PickNone {
				t.Fatalf("%s pick %d: %v/%v vs %v/%s/%v", strategy, i, want, ok, got, kind, ok2)
			}
			plain.Release(want)
			withEmptyKey.Release(got)
		}
	}
}

func TestConcurrentAffinityRespectsCapacity(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1700000000, 0)}
	pool := testPool(StrategyRoundRobin, []string{"a", "b", "c", "d"}, clock)
	const session = "ses_concurrent"
	pref := preferredTarget(pool.Route, session, pool.Targets)
	pref.MaxActive = 2

	const workers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	sticky, spill := 0, 0
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			target, kind, ok := pool.PickWithAffinity(session)
			if !ok {
				return
			}
			mu.Lock()
			if kind == PickSticky {
				sticky++
			} else {
				spill++
			}
			mu.Unlock()
			if active := target.state.Active(); active > target.MaxActive && target.MaxActive > 0 {
				t.Errorf("max_active violated on %s: %d", target.Name, active)
			}
			time.Sleep(time.Millisecond)
			pool.Release(target)
		}()
	}
	close(start)
	wg.Wait()
	if sticky != 2 || spill != workers-2 {
		t.Errorf("sticky=%d spill=%d, want 2 and %d", sticky, spill, workers-2)
	}
}
