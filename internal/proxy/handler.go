// SPDX-License-Identifier: AGPL-3.0-only

// Package proxy is PlainRoute's request path.
//
// One client request produces at most one upstream attempt. The gateway selects a
// target, sends the body it received, copies the response bytes back and
// reports what happened. It does not parse model content, does not rebuild SSE
// frames, and does not decide to try another key on its own.
package proxy

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/40960/plainroute/internal/config"
	"github.com/40960/plainroute/internal/metrics"
	"github.com/40960/plainroute/internal/routing"
)

// generation is one whole configuration, and it is the unit a reload swaps: the config, the
// registry built from it, and the transport built from it, together as one value. Nothing inside a
// generation is replaced in place. A request takes one generation at its start and uses that
// generation for its whole life, so a reload cannot hand a request the timeouts of one configuration
// and the targets of another.
type generation struct {
	cfg      *config.Config
	registry *routing.Registry
	client   *http.Client
}

// Handler serves the gateway API.
type Handler struct {
	current atomic.Pointer[generation]
	metrics *metrics.Metrics
	log     *slog.Logger
	now     func() time.Time
	store   *routing.StateStore

	ready    atomic.Bool
	draining atomic.Bool
	inflight sync.WaitGroup

	// started is reported as the model listing creation time, so /v1/models is
	// stable instead of changing on every request.
	started time.Time
}

// New builds a handler. The clock is a parameter so a test can move time without
// sleeping.
func New(cfg *config.Config, m *metrics.Metrics, log *slog.Logger, store *routing.StateStore, now func() time.Time) *Handler {
	if now == nil {
		now = time.Now
	}
	h := &Handler{
		metrics: m,
		log:     log,
		now:     now,
		store:   store,
		started: now(),
	}
	h.Reload(cfg)
	return h
}

// Reload builds a generation from a config that has already been parsed and validated, and then
// swaps the pointer. The build happens before the swap, so a config that cannot be turned into a
// registry does not replace the one in force, and a caller that failed validation never gets here.
// Routing a generation is the whole build, and it cannot fail, which is why there is no error check
// here that could never fire: the step that can fail is the parse and validation, before this call.
//
// Target state comes from the store, so a target that is still configured keeps its cooldown and its
// count of requests in flight across a reload, including a reload that only changed its api key.
// Requests that started under the previous generation keep that generation until they finish, and
// they release the slot on the same state object they took it on.
func (h *Handler) Reload(cfg *config.Config) {
	h.current.Store(&generation{
		cfg:      cfg,
		registry: routing.New(cfg, h.store, h.now),
		client:   &http.Client{Transport: newTransport(cfg), CheckRedirect: noRedirect},
	})
}

// Generation returns the configuration in force. A request calls this once, at its start.
func (h *Handler) Generation() *generation { return h.current.Load() }

// Drain stops the gateway from accepting new work. Readiness reports unhealthy
// while draining.
func (h *Handler) Drain() { h.draining.Store(true) }

func (h *Handler) Ready() bool { return h.ready.Load() && !h.draining.Load() }

// Wait blocks until every handled request finished, up to the deadline.
func (h *Handler) Wait(deadline time.Time) bool {
	done := make(chan struct{})
	go func() {
		h.inflight.Wait()
		close(done)
	}()
	for {
		select {
		case <-done:
			return true
		case <-time.After(20 * time.Millisecond):
			if h.now().After(deadline) {
				return false
			}
		}
	}
}

// ServeHTTP routes a small API surface. There is no middleware chain and no
// request transformation beyond header filtering.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// One generation, taken once. Everything this request does reads this value, so a reload that
	// happens while it is running cannot change what it is doing.
	g := h.Generation()
	cfg := g.cfg

	// The api key a caller presents is checked on the API endpoints only. Health,
	// readiness and the metrics page stay open, because a monitoring system that cannot
	// authenticate is the ordinary case and nothing on those pages is secret.
	if strings.HasPrefix(r.URL.Path, "/v1/") && !h.authorize(w, r, g) {
		return
	}

	switch r.URL.Path {
	case "/v1/chat/completions":
		h.handleChat(w, r, g)
		return
	case "/v1/models":
		h.handleModels(w, r, g)
		return
	case "/healthz", "/health/live":
		h.status(w, http.StatusOK, "ok")
		return
	case "/readyz", "/health/ready":
		if h.Ready() {
			h.status(w, http.StatusOK, "ready")
		} else {
			h.status(w, http.StatusServiceUnavailable, "not ready")
		}
		return
	}

	if cfg.Metrics.On() && r.URL.Path == cfg.Metrics.Path {
		h.metrics.Handler().ServeHTTP(w, r)
		return
	}

	h.reject(w, r, http.StatusNotFound, "page_not_found", "invalid_request_error",
		"unknown path "+r.URL.Path, metrics.NoRoute, "")
}

func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request, g *generation) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.reject(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "invalid_request_error",
			"models listing requires GET", metrics.NoRoute, "")
		return
	}

	id := requestID(r)
	h.metrics.GatewayRejected.WithLabelValues(metrics.NoRoute, "models_list").Inc()

	// A route entry is the name a client can send. It is not a model: the models belong to the
	// targets, so the listing reports the name and the distinct models it can be served by. When a
	// route has one model and that model is the route name, models is left out because it would
	// only repeat the id.
	type routeEntry struct {
		ID      string   `json:"id"`
		Object  string   `json:"object"`
		Created int64    `json:"created"`
		OwnedBy string   `json:"owned_by"`
		Models  []string `json:"models,omitempty"`
		Targets int      `json:"targets"`
	}

	created := h.started.Unix()
	entries := make([]routeEntry, 0)

	for _, pool := range g.registry.Pools() {
		seenModel := map[string]bool{}
		models := make([]string, 0, len(pool.Targets))
		for _, t := range pool.Targets {
			if !seenModel[t.Model] {
				seenModel[t.Model] = true
				models = append(models, t.Model)
			}
		}
		sort.Strings(models)

		entry := routeEntry{
			ID:      pool.Route,
			Object:  "model",
			Created: created,
			OwnedBy: "plainroute",
			Models:  models,
			Targets: len(pool.Targets),
		}
		if len(models) == 1 && models[0] == pool.Route {
			entry.Models = nil
		}
		entries = append(entries, entry)
	}

	body, err := json.Marshal(map[string]any{"object": "list", "data": entries})
	if err != nil {
		h.reject(w, r, http.StatusInternalServerError, "internal_error", "internal_error",
			"model listing failed", metrics.NoRoute, id)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-Id", id)
	w.WriteHeader(http.StatusOK)
	h.writeToClient(w, body, metrics.NoRoute, metrics.NoTarget, id)
}

// handleChat is the whole gateway behaviour for one client request.
func (h *Handler) handleChat(w http.ResponseWriter, r *http.Request, g *generation) {
	cfg := g.cfg
	if r.Method != http.MethodPost {
		h.reject(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "invalid_request_error",
			"chat completions require POST", metrics.NoRoute, "")
		return
	}

	id := requestID(r)
	start := h.now()

	h.inflight.Add(1)
	defer h.inflight.Done()
	h.metrics.InFlight.Inc()
	defer h.metrics.InFlight.Dec()

	body, err := readBody(w, r, cfg)
	if err != nil {
		status, code := http.StatusBadRequest, "invalid_json"
		if errors.Is(err, errBodyTooLarge) {
			status, code = http.StatusRequestEntityTooLarge, "request_body_too_large"
		}
		h.metrics.GatewayRejected.WithLabelValues(metrics.NoRoute, code).Inc()
		h.reject(w, r, status, code, "invalid_request_error", err.Error(), metrics.NoRoute, id)
		return
	}

	peek, err := peekBody(body)
	if err != nil {
		h.metrics.GatewayRejected.WithLabelValues(metrics.NoRoute, "invalid_json").Inc()
		h.reject(w, r, http.StatusBadRequest, "invalid_json", "invalid_request_error",
			err.Error(), metrics.NoRoute, id)
		return
	}
	if peek.Model == "" {
		h.metrics.GatewayRejected.WithLabelValues(metrics.NoRoute, "missing_model").Inc()
		h.reject(w, r, http.StatusBadRequest, "missing_model", "invalid_request_error",
			`field "model" is required`, metrics.NoRoute, id)
		return
	}

	primary, ok := g.registry.Lookup(peek.Model)
	if !ok {
		// The name a client sent cannot be a metric label: a client can send any string, and a
		// label value is never removed. The name is logged instead, so it stays diagnosable.
		h.metrics.GatewayRejected.WithLabelValues(metrics.NoRoute, "model_not_found").Inc()
		h.log.Warn("route_not_found", "request_id", id, "requested", peek.Model)
		h.reject(w, r, http.StatusNotFound, "model_not_found", "invalid_request_error",
			"model "+peek.Model+" is not a configured route", metrics.NoRoute, id)
		return
	}

	// The fallback chain is only built when fallbacks are explicitly enabled, and
	// the cycle check at load time guarantees this walk cannot loop. It is a list
	// of routes, not of targets: a fallback is a different route, and the target is
	// picked inside that route.
	chain := []*routing.Pool{primary}
	if cfg.Fallback.Enabled {
		seen := map[string]bool{primary.Route: true}
		current := primary
		for len(chain) <= cfg.Fallback.MaxHops && len(current.Fallbacks) > 0 {
			next, ok := g.registry.Lookup(current.Fallbacks[0])
			if !ok || seen[next.Route] {
				break
			}
			chain = append(chain, next)
			seen[next.Route] = true
			current = next
		}
	}

	var lastUnavailable *routing.Pool
	for i, pool := range chain {
		target, ok := pool.Pick()
		if !ok {
			lastUnavailable = pool
			continue
		}

		// A fallback is only possible while nothing has been written to the
		// client. Once a stream has started the gateway cannot take the response
		// back, so it reports what it got.
		result := h.attempt(w, r, g, pool, target, body, peek, id, start, i < len(chain)-1)
		if result.delivered {
			return
		}

		h.log.Warn("model_fallback",
			"request_id", id,
			"from_route", pool.Route,
			"to_route", chain[i+1].Route,
			"target", target.Name,
			"reason", result.kind,
		)
	}

	// Nothing was reachable. The gateway does not contact an upstream here: it
	// answers from its own routing state, and Retry-After comes from the
	// target that recovers first.
	if lastUnavailable == nil {
		h.reject(w, r, http.StatusInternalServerError, "internal_error", "internal_error",
			"no target state", metrics.NoRoute, id)
		return
	}

	remaining, cooling := lastUnavailable.EarliestRecovery()
	if !cooling {
		remaining = cfg.Routing.Cooldown.Status[429].Std()
	}
	w.Header().Set("Retry-After", retryAfterSeconds(remaining))
	h.metrics.GatewayRejected.WithLabelValues(lastUnavailable.Route, "all_targets_cooling_down").Inc()
	h.reject(w, r, http.StatusTooManyRequests, "all_targets_cooling_down", "rate_limit_error",
		"No healthy targets are currently available", lastUnavailable.Route, id)
}

func (h *Handler) reject(w http.ResponseWriter, r *http.Request, status int, code, typ, message, route, id string) {
	h.metrics.RequestsTotal.WithLabelValues(route, metrics.NoTarget, metrics.StatusClass(status)).Inc()
	h.log.Warn("gateway_response", "request_id", id, "route", route, "status", status, "code", code)
	if id != "" {
		w.Header().Set("X-Request-Id", id)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	h.writeToClient(w, errorBody(message, typ, code), route, metrics.NoTarget, id)
}

// authorize checks the api key a caller presents, and is reached only for the API endpoints.
// The comparison is constant time, so a wrong key does not show how many bytes matched.
//
// The caller key and the target key are different things. The caller key decides who may
// use the gateway; the key that goes upstream is always the one configured for the selected
// target, so a caller cannot aim a request at a key the operator did not give it.
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, g *generation) bool {
	cfg := g.cfg
	if cfg.Auth.APIKey == "" {
		return true
	}

	id := requestID(r)
	presented := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(presented), []byte(cfg.Auth.APIKey)) == 1 {
		return true
	}

	h.metrics.GatewayRejected.WithLabelValues(metrics.NoRoute, "invalid_api_key").Inc()
	h.log.Warn("unauthenticated_request", "request_id", id, "path", r.URL.Path)
	h.reject(w, r, http.StatusUnauthorized, "invalid_api_key", "authentication_error",
		"invalid api key", metrics.NoRoute, id)
	return false
}

func (h *Handler) status(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	h.writeToClient(w, []byte(message+"\n"), metrics.NoRoute, metrics.NoTarget, "")
}

// writeToClient writes a body the gateway produced itself. A failed write means the
// client went away before the answer arrived, so it is recorded the same way as a
// disconnect anywhere else in the request path.
func (h *Handler) writeToClient(w http.ResponseWriter, body []byte, route, target, id string) {
	if _, err := w.Write(body); err != nil {
		h.metrics.ClientDisconnects.WithLabelValues(route, target).Inc()
		h.log.Warn("client_disconnect", "request_id", id, "route", route,
			"target", target, "error", err)
	}
}

var errBodyTooLarge = errors.New("request body too large")

func readBody(w http.ResponseWriter, r *http.Request, cfg *config.Config) ([]byte, error) {
	if r.Body == nil {
		return nil, errors.New("request body is required")
	}
	limited := http.MaxBytesReader(w, r.Body, cfg.Server.MaxRequestBytes)
	body, err := io.ReadAll(limited)
	if err != nil {
		// MaxBytesReader reports the limit as its own error type. Mapping it to the
		// gateway's error keeps a too-large body a 413 instead of a parse failure.
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			return nil, errBodyTooLarge
		}
		return nil, err
	}
	if int64(len(body)) > cfg.Server.MaxRequestBytes {
		return nil, errBodyTooLarge
	}
	if len(body) == 0 {
		return nil, errors.New("request body is empty")
	}
	return body, nil
}

func requestID(r *http.Request) string {
	if id := r.Header.Get("X-Request-Id"); id != "" && len(id) <= 128 {
		return id
	}
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(buf)
}

func isEventStream(contentType string) bool {
	return strings.HasPrefix(strings.ToLower(contentType), "text/event-stream")
}

func durationMs(d time.Duration) int64 { return int64(d / time.Millisecond) }
