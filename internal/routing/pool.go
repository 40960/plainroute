// SPDX-License-Identifier: AGPL-3.0-only

// Package routing picks which deployment serves a request.
//
// The rule that matters most in this package: one client request produces at
// most one upstream attempt. A deployment that returns 429 is put into cooldown
// and the error is handed back to the client. PlainRoute never decides to try
// another key inside the same request; the caller owns retries and backoff.
package routing

import (
	"sort"
	"sync"
	"time"

	"github.com/40960/plainroute/internal/config"
)

// Deployment is one upstream endpoint with one key.
type Deployment struct {
	Name          string
	Model         string // logical model, the alias the client used
	UpstreamModel string // model name sent upstream
	BaseURL       string
	Endpoint      string
	APIKey        string
	Headers       map[string]string
	MaxActive     int

	state *State
}

// URL is the full upstream request URL for this deployment.
func (d *Deployment) URL() string { return config.JoinPath(d.BaseURL, d.Endpoint) }

// Active is the number of requests this deployment is holding.
func (d *Deployment) Active() int { return d.state.Active() }

// Available reports whether the deployment can be used right now.
func (d *Deployment) Available() bool { return d.state.Available(d.MaxActive, time.Now()) }

// State is the runtime state of a deployment: requests in flight and whether it
// is cooling down. It is keyed by model and deployment name, so a configuration
// reload keeps the cooldown of a deployment that is still configured.
type State struct {
	mu             sync.Mutex
	active         int
	cooldownUntil  time.Time
	cooldownReason string
}

// Available reports whether the deployment can be used right now, without
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

// CoolDown puts the deployment out of the pool for d. A longer cooldown wins
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

// Remaining reports how long the deployment is still unavailable.
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

// StateStore hands out the same *State for the same model and deployment name
// across configuration reloads.
type StateStore struct {
	mu     sync.Mutex
	states map[string]*State
}

func NewStateStore() *StateStore {
	return &StateStore{states: map[string]*State{}}
}

func (s *StateStore) Get(model, name string) *State {
	key := model + "\x00" + name
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.states[key]; ok {
		return st
	}
	st := &State{}
	s.states[key] = st
	return st
}

// Pool is the set of deployments behind one logical model.
type Pool struct {
	Model         string
	UpstreamModel string
	Deployments   []*Deployment
	Fallbacks     []string

	strategy string
	mu       sync.Mutex
	cursor   int
	now      func() time.Time
}

// Status is a snapshot used for logging and metrics.
type Status struct {
	Model      string
	Deployment string
	Active     int
	Cooling    bool
	Remaining  time.Duration
	Reason     string
}

// Pick selects a healthy deployment and reserves a slot on it. It returns false
// when nothing is selectable, which is the only decision the gateway makes for a
// request: the caller then reports the failure to the client instead of trying
// another deployment.
func (p *Pool) Pick() (*Deployment, bool) {
	if len(p.Deployments) == 0 {
		return nil, false
	}
	now := p.now()

	candidates := make([]*Deployment, 0, len(p.Deployments))
	for _, d := range p.Deployments {
		if d.state.Available(d.MaxActive, now) {
			candidates = append(candidates, d)
		}
	}
	if len(candidates) == 0 {
		return nil, false
	}

	switch p.strategy {
	case StrategyRoundRobin:
		p.mu.Lock()
		start := p.cursor
		p.mu.Unlock()
		// Rotate the candidate list so the next request starts where the last
		// one left off, while cooling deployments keep their position out of the
		// rotation until they recover.
		rotated := make([]*Deployment, 0, len(candidates))
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

	for _, d := range candidates {
		if d.state.TryAcquire(d.MaxActive, now) {
			if p.strategy == StrategyRoundRobin {
				p.mu.Lock()
				p.cursor = (p.cursor + 1) % len(p.Deployments)
				p.mu.Unlock()
			}
			return d, true
		}
	}
	return nil, false
}

// Release gives back the slot taken by Pick. Callers must always call it.
func (p *Pool) Release(d *Deployment) {
	d.state.Release()
}

// Cooldown marks a deployment unavailable for dur.
func (p *Pool) Cooldown(d *Deployment, dur time.Duration, reason string) {
	d.state.CoolDown(dur, reason, p.now())
}

// EarliestRecovery reports how long until the first unavailable deployment can
// be used again. ok is false when every deployment is currently usable.
func (p *Pool) EarliestRecovery() (time.Duration, bool) {
	now := p.now()
	var earliest time.Duration
	found := false
	for _, d := range p.Deployments {
		remaining, cooling := d.state.Remaining(now)
		if !cooling {
			continue
		}
		if !found || remaining < earliest {
			earliest, found = remaining, true
		}
	}
	return earliest, found
}

// Snapshot returns the current state of every deployment in configuration order.
func (p *Pool) Snapshot() []Status {
	now := p.now()
	out := make([]Status, 0, len(p.Deployments))
	for _, d := range p.Deployments {
		remaining, cooling := d.state.Remaining(now)
		out = append(out, Status{
			Model:      p.Model,
			Deployment: d.Name,
			Active:     d.state.Active(),
			Cooling:    cooling,
			Remaining:  remaining,
			Reason:     d.state.Reason(),
		})
	}
	return out
}

const (
	StrategyLeastActive = "least_active"
	StrategyRoundRobin  = "round_robin"
)
