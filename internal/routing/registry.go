// SPDX-License-Identifier: AGPL-3.0-only

package routing

import (
	"strings"
	"sync"
	"time"

	"github.com/40960/plainroute/internal/config"
)

// Registry maps the name a client sends in the model field to the route behind it.
//
// A route is found by its name only. Lookup used to accept the real model name as well, which
// worked while one name mapped to exactly one model. A target carries its own model now, so the
// same model name can sit behind several routes, and picking between them would need a rule a
// reader cannot predict from the config. The name a client sends is therefore a route name, and a
// route name is not necessarily a model name.
type Registry struct {
	mu       sync.Mutex
	routes   map[string]*Pool
	order    []string
	strategy string
	now      func() time.Time
	store    *StateStore
}

// New builds a registry from a validated config. Target state comes from the store, so rebuilding
// after a reload does not clear a cooldown for a target that is still configured.
func New(cfg *config.Config, store *StateStore, now func() time.Time) *Registry {
	if store == nil {
		store = NewStateStore()
	}
	if now == nil {
		now = time.Now
	}

	r := &Registry{
		routes:   map[string]*Pool{},
		order:    config.RouteNames(cfg.Routes),
		strategy: cfg.Routing.Strategy,
		now:      now,
		store:    store,
	}

	for _, name := range r.order {
		route := cfg.Routes[name]
		pool := &Pool{
			Route:     name,
			Fallbacks: append([]string(nil), route.Fallbacks...),
			strategy:  r.strategy,
			now:       now,
		}
		for i := range route.Targets {
			t := &route.Targets[i]
			endpoint := t.Path
			if endpoint == "" {
				endpoint = config.DefaultEndpoint
			}
			pool.Targets = append(pool.Targets, &Target{
				Name:      t.Name,
				Model:     t.Model,
				BaseURL:   strings.TrimRight(t.BaseURL, "/"),
				Endpoint:  endpoint,
				APIKey:    t.APIKey,
				Headers:   t.Headers,
				MaxActive: t.MaxActive,
				state:     store.Get(name, t.Name),
			})
		}
		r.routes[name] = pool
	}

	return r
}

// Lookup resolves the name a client sent.
func (r *Registry) Lookup(name string) (*Pool, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pool, ok := r.routes[name]
	return pool, ok
}

// Pools returns every pool in stable name order.
func (r *Registry) Pools() []*Pool {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Pool, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.routes[name])
	}
	return out
}

func (r *Registry) Strategy() string { return r.strategy }
