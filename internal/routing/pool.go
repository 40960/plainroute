// SPDX-License-Identifier: AGPL-3.0-only

// Package routing picks which target serves a request.
//
// The rule that matters most in this package: one client request produces at
// most one upstream attempt. A target that returns 429 is put into cooldown
// and the error is handed back to the client. PlainRoute never decides to try
// another key inside the same request; the caller owns retries and backoff.
package routing

import (
	"sort"
	"sync"
	"time"

	"github.com/40960/plainroute/internal/config"
)

// Target is one upstream: a model name, an endpoint and one key.
type Target struct {
	Name      string
	Model     string // the model this target serves
	BaseURL   string
	Endpoint  string
	APIKey    string
	Headers   map[string]string
	MaxActive int

	state *State
}

// URL is the full upstream request URL for this target.
func (t *Target) URL() string { return config.JoinPath(t.BaseURL, t.Endpoint) }

// Active is the number of requests this target is holding.
func (t *Target) Active() int { return t.state.Active() }

// Available reports whether the target can be used right now.
func (t *Target) Available() bool { return t.state.Available(t.MaxActive, time.Now()) }

// State is the runtime state of a target: requests in flight and whether it
// is cooling down. It is keyed by route and target name, so a configuration
// reload keeps the cooldown of a target that is still configured.
type State struct {
	mu             sync.Mutex
	active         int
	cooldownUntil  time.Time
	cooldownReason string
}

// Available reports whether the target can be used right now, without
// reserving anything.
func (s *State) Available(maxActive int, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if now.Before(s.cooldownUntil) {
		return false
	}
	return maxActive <= 0 || s.active < maxActive
}

// TryAcquire reserves a slot. The check and the increment happen under one lock,
// so two goroutines cannot both believe they own the same slot.
func (s *State) TryAcquire(maxActive int, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if now.Before(s.cooldownUntil) {
		return false
	}
	if maxActive > 0 && s.active >= maxActive {
		return false
	}
	s.active++
	return true
}

// Release gives back a slot. Every request path must call it: normal completion,
// an upstream error, a client disconnect, a timeout.
func (s *State) Release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active > 0 {
		s.active--
	}
}

func (s *State) Active() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

// CoolDown puts the target out of the pool for t. A longer cooldown wins
// over a shorter one, so a long Retry-After is not overwritten by a later,
// smaller value.
func (s *State) CoolDown(dur time.Duration, reason string, now time.Time) {
	if dur <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	until := now.Add(dur)
	if until.After(s.cooldownUntil) {
		s.cooldownUntil = until
		s.cooldownReason = reason
	}
}

// Remaining reports how long the target is still unavailable.
func (s *State) Remaining(now time.Time) (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if now.Before(s.cooldownUntil) {
		return s.cooldownUntil.Sub(now), true
	}
	return 0, false
}

func (s *State) Reason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cooldownReason
}

// StateStore hands out the same *State for the same route and target name
// across configuration reloads.
type StateStore struct {
	mu     sync.Mutex
	states map[string]*State
}

func NewStateStore() *StateStore {
	return &StateStore{states: map[string]*State{}}
}

func (s *StateStore) Get(route, name string) *State {
	key := route + "\x00" + name
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.states[key]; ok {
		return st
	}
	st := &State{}
	s.states[key] = st
	return st
}

// Pool is the set of selectable targets behind one route name.
type Pool struct {
	Route     string
	Targets   []*Target
	Fallbacks []string

	strategy string
	mu       sync.Mutex
	cursor   int
	now      func() time.Time
}

// Status is a snapshot used for logging and metrics.
type Status struct {
	Route     string
	Target    string
	Active    int
	Cooling   bool
	Remaining time.Duration
	Reason    string
}

// PickKind says what decided a reservation. PickNone means the request had no
// usable session affinity and the configured strategy chose; the other two are
// only possible with an affinity key.
type PickKind string

const (
	PickNone   PickKind = "none"
	PickSticky PickKind = "sticky"
	PickSpill  PickKind = "spill"
)

// Pick selects a healthy target and reserves a slot on it. It returns false
// when nothing is selectable, which is the only decision the gateway makes for a
// request: the caller then reports the failure to the client instead of trying
// another target.
//
// The whole decision is one critical section: the selectable set, the cursor or
// active counts it reads, the reservation and the cursor advance are all done
// under p.mu. Release and Cooldown share the same boundary, so no caller can
// change a target's state between the snapshot and TryAcquire.
func (p *Pool) Pick() (*Target, bool) {
	t, _, ok := p.pick("")
	return t, ok
}

// PickWithAffinity first tries the deterministic preferred target for this
// route and session. If that target is selectable it is reserved (sticky); if
// it is cooling or at capacity the request spills immediately to the existing
// strategy, with no waiting and no local retry. The next request prefers the
// same target again: spilling does not rebind the session.
func (p *Pool) PickWithAffinity(session string) (*Target, PickKind, bool) {
	return p.pick(session)
}

func (p *Pool) pick(session string) (*Target, PickKind, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.Targets) == 0 {
		return nil, PickNone, false
	}
	now := p.now()

	candidates := make([]*Target, 0, len(p.Targets))
	for _, t := range p.Targets {
		if t.state.Available(t.MaxActive, now) {
			candidates = append(candidates, t)
		}
	}
	if len(candidates) == 0 {
		return nil, PickNone, false
	}

	kind := PickNone
	if session != "" {
		kind = PickSpill
		preferred := preferredTarget(p.Route, session, p.Targets)
		if preferred != nil && preferred.state.Available(preferred.MaxActive, now) &&
			preferred.state.TryAcquire(preferred.MaxActive, now) {
			if p.strategy == StrategyRoundRobin {
				p.cursor = (p.cursor + 1) % len(p.Targets)
			}
			return preferred, PickSticky, true
		}
	}

	switch p.strategy {
	case StrategyRoundRobin:
		start := p.cursor
		// Rotate the selectable candidates so the next request starts where the last one
		// left off. Cooling and unavailable targets are not candidates at all: they leave
		// the rotation while out and re-enter it when they become selectable again.
		rotated := make([]*Target, 0, len(candidates))
		for i := range candidates {
			rotated = append(rotated, candidates[(start+i)%len(candidates)])
		}
		candidates = rotated
	default:
		// least_active: fewest requests in flight. Ties keep configuration order,
		// which makes the choice reproducible in tests.
		sort.SliceStable(candidates, func(i, j int) bool {
			return candidates[i].state.Active() < candidates[j].state.Active()
		})
	}

	for _, t := range candidates {
		if t.state.TryAcquire(t.MaxActive, now) {
			if p.strategy == StrategyRoundRobin {
				p.cursor = (p.cursor + 1) % len(p.Targets)
			}
			return t, kind, true
		}
	}
	return nil, PickNone, false
}

// Release gives back the slot taken by Pick. Callers must always call it.
// It shares Pick's lock so a release cannot change an active count between
// Pick's snapshot and its reservation.
func (p *Pool) Release(t *Target) {
	p.mu.Lock()
	defer p.mu.Unlock()
	t.state.Release()
}

// Cooldown marks a target unavailable for dur. It shares Pick's lock for the
// same reason as Release.
func (p *Pool) Cooldown(t *Target, dur time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	t.state.CoolDown(dur, reason, p.now())
}

// EarliestRecovery reports how long until the first unavailable target can
// be used again. ok is false when every target is currently usable.
func (p *Pool) EarliestRecovery() (time.Duration, bool) {
	now := p.now()
	var earliest time.Duration
	found := false
	for _, t := range p.Targets {
		remaining, cooling := t.state.Remaining(now)
		if !cooling {
			continue
		}
		if !found || remaining < earliest {
			earliest, found = remaining, true
		}
	}
	return earliest, found
}

// Unavailability says why nothing in this pool can be picked right now.
// cooling is true when at least one target is in cooldown, busy is true when a
// target is out only because it is at max_active, and earliest is the soonest
// real cooldown recovery (zero when no target is cooling). A busy target has no
// computable recovery time: State does not record when its in-flight requests
// began, so the caller must not invent a Retry-After for it.
func (p *Pool) Unavailability() (cooling, busy bool, earliest time.Duration) {
	now := p.now()
	found := false
	for _, t := range p.Targets {
		if remaining, coolingNow := t.state.Remaining(now); coolingNow {
			cooling = true
			if !found || remaining < earliest {
				earliest, found = remaining, true
			}
			continue
		}
		if t.MaxActive > 0 && t.state.Active() >= t.MaxActive {
			busy = true
		}
	}
	return cooling, busy, earliest
}

// Snapshot returns the current state of every target in configuration order.
func (p *Pool) Snapshot() []Status {
	now := p.now()
	out := make([]Status, 0, len(p.Targets))
	for _, t := range p.Targets {
		remaining, cooling := t.state.Remaining(now)
		out = append(out, Status{
			Route:     p.Route,
			Target:    t.Name,
			Active:    t.state.Active(),
			Cooling:   cooling,
			Remaining: remaining,
			Reason:    t.state.Reason(),
		})
	}
	return out
}

const (
	StrategyLeastActive = "least_active"
	StrategyRoundRobin  = "round_robin"
)
