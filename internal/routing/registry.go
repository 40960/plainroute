// SPDX-License-Identifier: AGPL-3.0-only

package routing

import (
	"strings"
	"sync"
	"time"

	"github.com/40960/plainroute/internal/config"
)

// Registry maps the model name a client sends to a pool of deployments.
//
// Lookup accepts the configured alias and the real upstream model name, so a
// client can use either. Aliases are checked first; if two models share the same
// upstream name, the model listed first in sorted alias order wins.
type Registry struct {
	mu       sync.Mutex
	pools    map[string]*Pool
	byName   map[string]*Pool
	order    []string
	strategy string
	now      func() time.Time
	store    *StateStore
}

// New builds a registry from a validated config. Deployment state comes from the
// store, so rebuilding after a reload does not clear a cooldown for a deployment
// that is still configured.
func New(cfg *config.Config, store *StateStore, now func() time.Time) *Registry {
	if store == nil {
		store = NewStateStore()
	}
	if now == nil {
		now = time.Now
	}

	r := &Registry{
		pools:    map[string]*Pool{},
		byName:   map[string]*Pool{},
		strategy: cfg.Routing.Strategy,
		now:      now,
		store:    store,
	}

	for _, alias := range config.ModelNames(cfg.Models) {
		model := cfg.Models[alias]
		pool := &Pool{
			Model:         alias,
			UpstreamModel: model.UpstreamModel,
			Fallbacks:     append([]string(nil), model.Fallbacks...),
			strategy:      r.strategy,
			now:           r.now,
		}
		for i := range model.Deployments {
			d := &model.Deployments[i]
			endpoint := d.Path
			if endpoint == "" {
				endpoint = config.DefaultEndpoint
			}
			pool.Deployments = append(pool.Deployments, &Deployment{
				Name:          d.Name,
				Model:         alias,
				UpstreamModel: model.UpstreamModel,
				BaseURL:       strings.TrimRight(d.BaseURL, "/"),
				Endpoint:      endpoint,
				APIKey:        d.APIKey,
				Headers:       d.Headers,
				MaxActive:     d.MaxActive,
				state:         store.Get(alias, d.Name),
			})
		}
		r.pools[alias] = pool
	}

	r.order = config.ModelNames(cfg.Models)

	// Register the real model names too, in sorted alias order so the mapping is
	// reproducible.
	for _, alias := range r.order {
		pool := r.pools[alias]
		if _, ok := r.byName[pool.UpstreamModel]; !ok {
			r.byName[pool.UpstreamModel] = pool
		}
	}
	for _, alias := range r.order {
		r.byName[alias] = r.pools[alias]
	}

	return r
}

// Lookup resolves the model name a client sent.
func (r *Registry) Lookup(model string) (*Pool, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pool, ok := r.byName[model]
	return pool, ok
}

// Pools returns every pool in stable alias order.
func (r *Registry) Pools() []*Pool {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Pool, 0, len(r.order))
	for _, alias := range r.order {
		out = append(out, r.pools[alias])
	}
	return out
}

func (r *Registry) Strategy() string { return r.strategy }
