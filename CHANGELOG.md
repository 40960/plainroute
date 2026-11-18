# Changelog

<!-- SPDX-License-Identifier: AGPL-3.0-only -->

This file describes what a version actually does, in the order a reader would notice it.
Entries are checked against the code before they are written, and a version is not labelled
as done until the tests, the race detector and the end-to-end script pass.

PlainRoute does not promise an API beyond the OpenAI-compatible endpoints it serves and the
metric names it exposes. Changing a metric name is a breaking change and goes in the changelog.

## 0.1.0

Released for the first publish. Everything below was checked by tests, the race detector, or
the end-to-end script.

### Behaviour

- One upstream attempt per client request. No retry by default, and no internal retry inside
  a single request.
- Per-target cooldown from a status map, with `Retry-After` from the upstream winning,
  clamped by `routing.cooldown.max`, and an unparseable value falling back to the configured
  duration.
- A model with every target cooling is answered from the gateway's own state: 429,
  `code: all_deployments_cooling_down`, `Retry-After` from the target that recovers
  first, no upstream call.
- Routing strategies `least_active` (default) and `round_robin`. Selection is deterministic:
  ties keep config order. Measured on 12 sequential requests with four targets:
  round_robin 3/3/3/3, least_active 12/0/0/0 — sequential traffic never overlaps, so ties go
  to config order. The choice between them depends on whether the limit you care about is per
  key or per connection pool.
- `max_active` per target.
- Model alias mapping by byte splice, so only the model value changes and the rest of the
  document, including unknown fields and field order, survives.
- Streaming responses are copied chunk by chunk and flushed per chunk. Frames are not
  re-formed. Tool call deltas stay separate, and `[DONE]` survives.
- A stream idle watchdog aborts a silent stream. A client that goes away cancels the
  upstream request. An idle timeout and a client disconnect are recorded as different kinds.
- The gateway does not follow redirects. Hop-by-hop headers are dropped both ways.
  `Authorization` is always the configured target key.
- `timeouts.request` is 0 by default, so a long reasoning stream is not killed by an
  ordinary request timeout.
- Optional api key gate, off by default. `auth.api_key_env` names one environment variable;
  when it resolves, the api key a caller presents is compared in constant time on the API
  endpoints (`/v1/*`). Nothing configured means no gate, and a variable that is named but not
  set is a startup failure, so a gate cannot be half-on. Health, readiness and the metrics
  page stay open. The caller key is never forwarded upstream, never logged, and never appears
  in a metric label: the key the provider sees is always the target key. A rejection is
  401 with `code: invalid_api_key`, `type: authentication_error`, counted in
  `plainroute_gateway_rejected_total{model="none",code="invalid_api_key"}`, and it is one of
  the requests the gateway answered from its own state. The startup line reports the gate state and
  the name of the variable it reads, never the value, which is the only place a name collision can
  be seen: the config check cannot catch one, because a collided variable is set.
- The gate applies to new requests after a `SIGHUP` reload; a request that was already
  handled is not gated retroactively.

### Interfaces

- `/v1/chat/completions`, `/v1/models`, `/healthz`, `/readyz`, `/metrics`.
- Readiness is not affected by an upstream cooldown. It goes unhealthy only while draining.
- Structured JSON logs (text is also available) with routing facts only.
- Prometheus collectors, including the process memory and goroutine counts.
- SIGHUP reload through an atomic swap, keeping cooldown state; a failed reload keeps the
  running config.
- Graceful shutdown: no new requests, then the requests already streaming get the configured
  grace period.

### Tools

- `cmd/fakeupstream`: controllable upstream keyed by API key value, with per-key sequences
  and a `/stats` endpoint that reports what the server saw.
- `cmd/bench`: load generator that measures what the client sees and then reads the gateway's
  own counters, failing when upstream attempts exceed client requests or when a gauge is
  still non-zero after the load stopped.
- `scripts/e2e.sh`: the seven end-to-end cases against two real processes, including the api
  key gate. It stops before starting if something is already listening on the ports it needs,
  because a leftover process answers the checks instead of the one the script started.
- `scripts/generate-config.py`: builds a config for one upstream with N key slots from the live
  model endpoint, falling back to a snapshot file when that endpoint gives nothing. Keys are
  named by environment variable and never written into the generated file.
- `docker-compose.yml`: the production shape — loopback-only mapping, read-only root, all
  capabilities dropped, `no-new-privileges`, pid and memory limits, 30 second stop grace, keys
  from a 600-permission `.env`. Checked by starting it for real.

### Fixes found by the tests during this release

- The model value rewrite ate the separator between the key and the value, producing invalid
  JSON on the wire.
- Requests answered by the gateway itself were not counted in `requests_total`.
- The response cap could be overshot by one large read; reads are clamped to the remaining
  budget.
- An idle timeout was reported as a client disconnect, because a client disconnect also
  cancels the derived context.
- `http.MaxBytesReader` reported an oversized body as a JSON parse failure instead of a 413.
- Errors returned by `Write` and `Close` were thrown away.
- `TE` was forwarded to the upstream: `http.CanonicalHeaderKey` turns it into `Te`, so the
  hop-by-hop lookup never matched. A per-connection header must not cross the proxy.
- The fake upstream's configurable sequence did not advance: the entry just consumed became
  the standing behaviour, so a test that says "first 429, then 200" answered 429 forever.
- The fake upstream wrote an abrupt close without chunked framing, so a reader could not tell
  truncation from a clean end. It now frames a stream the way a provider does, and a test
  asserts that a stream the upstream ended abruptly is recorded as an early end, not as a
  completed request.

## Unreleased

Nothing is planned here that changes the request path. The list in the README limitations
section is the honest statement of what this project will not do.
