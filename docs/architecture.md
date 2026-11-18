# Architecture

<!-- SPDX-License-Identifier: AGPL-3.0-only -->

PlainRoute is one process, one binary, and one request path. This document describes
that path, the packages it is built from, and the decisions that are load bearing enough
that changing them would change what the project is.

## The request path

```
client
  │
  ▼
ServeHTTP ── method and path check, request id
  │
  ├── api key gate, /v1/* only, off when nothing is configured
  │
  ├── /healthz, /readyz, /metrics, /v1/models       answered from gateway state only
  │
  ▼
handleChat
  1. readBody      size limit checked before anything is forwarded
  2. peekBody      read model and stream from the document, nothing else
  3. Registry.Lookup(route name)               → pool
  4. pool.Pick()                               → one target, or nothing
  5. attempt()                                 → exactly one upstream request
  6. copy the response bytes, flush per read chunk
  7. report what happened to metrics and the log
```

Step 3 resolves a name, and it resolves route names only. Step 4 is the only decision the
gateway makes, and the model name sent upstream is the model of the target it picked. If
nothing is selectable, the request ends with a 429 answered from routing state, and no
upstream is contacted. If a target is selectable, exactly one request goes to it.

`attempt` is the function that keeps the invariant true, so it is worth reading in full.
It has no loop over targets and no second call to itself. The only way a client request
can produce two upstream attempts is when a route fallback is explicitly configured, and then
the walk is bounded by `fallbacks.max_hops`.

## Packages

| Package | What it owns | What it must not do |
| --- | --- | --- |
| `internal/config` | Load YAML strictly (unknown keys are errors), read the keys as values in the file, including the optional caller key, validate everything at once, apply defaults | Print a value it read from the file, which is a secret |
| `internal/routing` | Pools, selection, per-target state (active count, cooldown), state that survives a reload | Try another target inside one request |
| `internal/proxy` | The request path: the optional api key gate, body read, model splice, headers, the single attempt, streaming copy, error shape | Hold a model response in memory, re-form an SSE frame |
| `internal/logging` | Structured logging with a fixed field set | Carry a key, a prompt, or response content |
| `internal/metrics` | Prometheus collectors, including the process itself | Report anything that is not a count, gauge, or histogram |
| `cmd/plainroute` | Wiring only: load, listen, signals, drain | Decide anything about a request |
| `internal/fakeupstream` | A controllable upstream for tests and the end-to-end script, keyed by API key value | Be a real provider |
| `cmd/bench` | Load generator that reads the gateway's own counters and fails when they disagree | Count its own successes as the truth |

## Invariants

These are the properties the tests exist to protect. A change that breaks one of them is a
behaviour change, not a refactor.

1. **One client request produces at most one upstream attempt** (default config). Verified
   by 50 requests against an upstream that always returns 429, and by `cmd/bench` comparing
   `requests_total` with `upstream_attempts_total` under load.
2. **A target that is cooling down is never selected.** A cooldown is recorded when a
   status code matches `routing.cooldown.status`, when a `Retry-After` arrives, or after a
   connection or idle failure when `routing.cooldown.timeout` is non-zero.
3. **When every target behind a route name is cooling, no upstream call happens.** The answer
   comes from the gateway's own state with `code: all_targets_cooling_down`.
4. **The bytes the upstream sent are the bytes the client receives**, apart from the model
   value in the request body. Verified byte for byte, including the opening role delta,
   separate tool call deltas, and `[DONE]`.
5. **A failed write or a failed close is recorded, not swallowed.** A write error in a
   response the gateway produced itself means the client left before the answer arrived.
6. **Nothing sensitive is in a log line, a metric label, or an image layer.** There is no
   field in the logging package that could hold a key, so a leak requires adding one.
7. **State is safe under concurrency.** Every counter, gauge and selection decision is
   touched from many goroutines; the race detector is the check that matters most.

## Load-bearing decisions

**The model name is changed by splicing bytes, not by re-serialising.** `peekBody` records
the byte range of the model value while it reads the document, and `withModel` replaces that
range with the model of the target that was picked. Re-serialising would reorder keys, drop
unknown fields, normalise numbers, and change the request the client sent. Only one field is
allowed to differ, and the test asserts the rest is identical. Since the response is never
rewritten, the model a client reads back is the one the upstream wrote, which is not
necessarily the name the client sent.

**Lookup accepts route names only.** It used to accept the real model name as well, which was
possible only while one name mapped to exactly one model. A target carries its own model now,
so the same model can sit behind several routes, and any rule for choosing between them would
not be predictable from the config.

**A route name is the level a client codes against.** A name such as `large` or `medium` is the
ordinary form of it: the client names a tier, the configuration decides which model and which
provider answer, and selection happens inside that name. That makes the listing report two
different things, and only one of them is sendable. `id` is the name a client can send;
`models` says what is inside the route. A model name in the listing is not a name a client can
send, and for a tier name the `model` field in a response changes from request to request,
because the response reports what the upstream wrote.

**The first read happens before the response is committed.** If the upstream fails before
sending a body, the client gets an error, not an empty 200. Once bytes are on the wire they
cannot be taken back, which is also why a fallback only happens before the first byte.

**Flush per read chunk, and nothing more.** Per-chunk flushing is what makes streaming work
through a proxy at all. Re-forming frames would mean parsing SSE, which means holding a
frame, which means the response is no longer the upstream's.

**The idle watchdog records whether it fired.** A client disconnect also cancels the derived
context, so checking the context alone reports an idle timeout as a client disconnect. The
two are different things in a log and in a metric, so a flag records which one happened, and
the check order is the watchdog first.

**`timeouts.request` is 0 by default.** A long reasoning stream is not killed by an ordinary
request timeout. Only the idle timeout applies to a stream, and `write_timeout` stays off.

**Compression is left alone** (`Transport.DisableCompression = true`). A proxy that
decompresses cannot promise the bytes it copies are the bytes the upstream sent.

**Redirects are not followed.** Upstream URLs come from configuration only, so an error body
cannot point the proxy at a host that was not configured while carrying the request body.

**`Authorization` is always the configured target key.** A client cannot aim a request at
a key it was not given. Hop-by-hop headers are dropped in both directions, and
`Content-Length` is dropped because the body the gateway sends may differ from the one the
client counted.

**The caller api key is one optional comparison before the request path, not a caller table.**
It is checked on the API endpoints only, before any byte of the request body is read, so it is
not part of the streaming path and cannot change what reaches the client. The caller key and
the target key are different things, and the tests keep them apart: a caller key that is
accepted is never the key the upstream sees. A rejected request is counted as one the gateway
answered from its own state, with `route="none"`. A gate that is named in the config but whose
variable is not set stops the process at load, because a half-configured gate reads as a
working one. Health, readiness and the metrics page stay open, because they carry counts and a
monitoring system that cannot authenticate is the ordinary case. A gate that can be configured
but is off by default does not change the invariants; a table of keys, per-caller quotas, or
accounts would.

**A metric label never holds client input.** The route label is a configured route name, or
the constant `none` when the gateway answered before any route was resolved. The name a client
sends in the `model` field is logged as `requested` when it is not a configured route, because
a label value is never removed from the registry: a client that sends a different string on
every request would grow it without bound. Logging a name is safe, counting one is not.

**The clamp on `Retry-After` is a re-check interval, not a way of disagreeing with the
upstream.** A target is not contacted while it is cooling, so a `Retry-After` is never
verified, and the clamp is the longest an unverified value can govern before that target
is asked again and reality answers. It applies only to the value the upstream sent,
never to a configured duration, and 0 turns it off, like the other two cooldown values,
so an operator who wants to act on the upstream's word exactly can. When it fires it
changes what the client is told to wait, which is the one place the gateway does not
forward the upstream's word, so the default (15 minutes) sits above what a rate limit in
force can plausibly state: it fires on values that cannot be true. With the clamp off, a
false value governs until it expires, and only a process restart clears it, because a
reload keeps target state.

**The default cooldown is a prior, not a measurement.** `429: 10s` applies only when the
upstream sent nothing usable, because when it sent a `Retry-After` that value wins. It cannot be
right for every provider, so the choice is between holding a target out longer than the provider
asked, and asking the same key again too soon: 10s sits between a per-second limiter and a
per-minute limiter, which is the narrowest band a default can cover, and it is long enough that
the same key is not asked again on every request. For a key that was told to slow down, the
window is also the interval at which that key is asked again. A larger default does not reduce the number of upstream requests, because one client
request is one upstream attempt whenever a target is selectable: it moves answers from the
provider to the gateway, which is what this project calls a bug when the provider would have
answered. An operator who knows their provider's window should set it explicitly rather than
rely on the prior.

**Cooldown state survives a reload.** Target state is keyed by route and target name
in a `StateStore`, so rebuilding the registry after a `SIGHUP` does not clear a cooldown for a
target that is still configured. A failed reload keeps the running config, because a bad
edit should not break a working gateway.

**Readiness is not affected by an upstream cooldown.** A target that is cooling down is
still a healthy process. Readiness goes false only while draining, which is what a load
balancer should act on.

**Selection is deterministic.** `least_active` with stable sort, ties keep config order, so a
test can assert which target was chosen. `round_robin` rotates the candidate list while
cooling targets keep their position out of the rotation until they recover.

**`TryAcquire` checks and increments under one lock**, so two goroutines cannot both believe
they own the same slot. Every path that acquires must release: normal completion, an upstream
error, a timeout, a client disconnect.

**The clock is a parameter.** `now func() time.Time` is passed into the handler and the pool,
which is how cooldown tests move time without sleeping. A test that sleeps for a cooldown is a
test that gets flaky.

## Testing shape

Two kinds of tests, and each answers a different question:

- **Unit tests** in `internal/proxy`, `internal/routing`, `internal/config`,
  `internal/metrics`, and `internal/fakeupstream`: the request path against `httptest`, with the fake upstream keyed
  by API key value. These check the invariants, including flush-per-chunk timing, a stream
  that goes silent, an upstream that drops the connection mid-stream, a client that leaves,
  header filtering, redirects not being followed, goroutine counts after load, the 50-request
  no-amplification check, the api key gate: accepted, rejected, off when nothing is
  configured, applied after a reload, and never forwarded upstream, and the metric label names
  as the page prints them: a test built on `WithLabelValues` matches values only, so it cannot
  see whether a label is named `route` or `model`. The fake upstream has its
  own tests, because every other test depends on it doing what it promises.
- **`scripts/e2e.sh`**: the fake upstream and the gateway as two real processes, checking the
  seven end-to-end cases. This is the check a unit test cannot replace, because it verifies the
  binary, the config loader, and the signals with nothing mocked.

`cmd/bench` is not a performance target. It is a consistency check: the numbers the client
measured must match the numbers the gateway reports, and every gauge must return to zero
after the load stops.

## What would change this document

Anything that adds behaviour to the request path: a retry, a cache, a queue, a guardrail, a
response transform, a provider SDK. Each of those either changes the bytes on the wire or
turns one client request into more than one upstream request, which is the line the project
does not cross. If such a feature is genuinely wanted, it belongs in the README limitations
section as future work.
