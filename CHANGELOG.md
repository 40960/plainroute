# Changelog

<!-- SPDX-License-Identifier: AGPL-3.0-only -->

This file describes what a version actually does, in the order a reader would notice it.
Entries are checked against the code before they are written, and a version is not labelled
as done until the tests, the race detector and the end-to-end script pass.

PlainRoute does not promise an API beyond the OpenAI-compatible endpoints it serves and the
metric names it exposes. Changing a metric name is a breaking change and goes in the changelog.

## Unreleased

### Breaking changes

- The top level key is `routes:` and not `models:`. A route is the name a client sends in the
  `model` field, and the model belongs to each target, so one route can be served by several
  models. `upstream_model` is gone: each target carries `model`.
- `deployments:` is `targets:`. A target is one upstream entry: a model, a base URL, one key,
  and its own path, headers and `max_active`. Two targets can differ by any of those, so
  "same model, different key" is one case of a target and not its definition.
- Metric labels are `{route,target}` where they were `{model,deployment}`, and
  `plainroute_deployment_in_cooldown` is `plainroute_target_in_cooldown`.
- Log fields: `model` is now `route` (the name the client sent), `upstream_model` is now
  `model` (the model of the target that was picked), and `deployment` is now `target`.
- The error code `all_deployments_cooling_down` is `all_targets_cooling_down`. The other
  names were renamed by word boundary, which cannot match this one because it is a single
  underscore-joined token, so it had to be applied separately.
- `/v1/models` reports route names, the distinct model names behind each one, and the target
  count. `aliased_of` is gone: a name can now stand for several models, so there is nothing
  for it to point at. When a route has one model and that model is the route name, `models`
  is left out because it would only repeat the id. Only `id` is a name a client can send:
  `models` says what is inside a route, and a route name is the level a client codes against,
  so a model name in the listing is not a sendable name.
- `plainroute_gateway_rejected_total` no longer counts a name a client sent as the route
  label. `model_not_found` is counted with `none`, which is the value the api key gate already
  used, and the four rejections that were counted with `all` are counted with `none`. The name
  the client sent is logged as `requested` instead. A label value is never removed from the
  registry, so a client that sends a different name on every request would grow it without
  bound.
- Lookup accepts route names only, so a request that names a model is a 404 with
  `code: model_not_found`. Looking up by upstream model name was possible only while one name
  mapped to exactly one model. A model can now sit behind several routes, and any rule for
  choosing between them would not be predictable from the config.
- `api_key_env` is gone, and environment substitution is gone with it. A key is a value in the
  config file, so the file a gateway actually runs on is a secret: `config.yaml` is gitignored, it
  needs 600 permissions with the directory 700, the example file holds placeholders, and the
  generator writes placeholders because it has no argument that can take a key. `.env.example` is
  deleted and the `--env-file` workflow went with it, because a value that can come from two places is
  the two-source problem this change removes: a generic name such as `API_KEY` can be exported by
  another program in the same shell or set by a shared `.env`, and the process then compares a value
  nobody chose for it. Writing the value down is the whole fix, and the startup rule that a named
  variable must be set went with the lookup that rule existed for. A field a reader reads is now a
  field that runs, and that is the property the tests pin.

### Behaviour

- The config file is watched, so a change is applied without a restart and without a
  signal. The process reads the file, hashes the content with SHA-256, and acts only on
  content it has not acted on before. `config_reload.interval` is one second when nothing
  is written, and a value under 100ms is a startup error rather than a silent clamp,
  because a watcher that polled every millisecond is the busy loop the guard exists for.
  Last-known-good is the rule: a file that decodes but does not validate is reported and
  not applied, nothing is partly applied, the process does not exit, and the next look
  tries again. Startup stays strict, because before the first configuration there is
  nothing good to keep, which is why the watcher never waits for a file to appear. A half-
  written file, and a file that is briefly gone, are reported once: one bad content is one
  report and one count, not a line every second. Both ways an editor writes a file are
  seen, the save in place and the temp file renamed over the inode, because the state is
  the content and not the inode. `SIGHUP` stays as the manual force for a look now, and a
  signal on a file that has not changed is not a reload, because there is nothing to
  reload. There is no filesystem event framework, on purpose: no events to lose and no
  events to debounce.
- A request now takes one whole generation at its start and keeps it for its whole life. A
  generation is the configuration, the registry built from it and the transport built from
  it as one value, swapped by one pointer, and nothing inside it is replaced in place, so
  a reload cannot hand a running request the limits of one configuration and the targets
  of another. Target state comes from the state store keyed by route and target name, so a
  target that is still configured keeps its cooldown and its count of requests in flight
  across a reload, including a reload that only changed its key.
- `plainroute_config_reloads_total{outcome}` counts reloads that acted on changed content,
  one count per distinct content. Before this, every `SIGHUP` was a reload; now a signal
  on a file that has not changed is not one, and a file that stays bad is counted once
  rather than once per look. A reader comparing two runs of this counter should not expect
  the number of signals.

- Load balancing now covers targets that can differ by more than the key: model, endpoint,
  path, headers and `max_active` are all per target. The set behind one route name is the set
  that gets balanced; two route names are never balanced against each other, and one reaches
  the other only through a configured fallback.

- `routing.cooldown.max` can be turned off by writing 0, like the other two cooldown
  values, and its default moved from 5 minutes to 15. A file that wrote `max: 0` meant
  the default before, so it now behaves differently: a `Retry-After` from the upstream
  is used as written. The reason for the clamp is that a target is never contacted while
  it is cooling, so a `Retry-After` is never verified, and the clamp is the longest an
  unverified value can govern before the target is asked again; the default is chosen so
  it fires only on a value that cannot be true.

- The example config no longer writes a window for 503, and the generator writes `503:
  0s`. The core default map is `429` alone and did not change, because a window names a
  target, and a code belongs in the map only when it says something about that target. A
  401 or a 403 says the credential is the problem; a 503 normally says the service
  behind the credential is temporarily unavailable, so a window on 503 holds a working
  key out of the pool for a failure that may not belong to it, and nothing in PlainRoute
  counts consecutive failures, so it cannot tell a transient 503 from a broken target.
  What the provider says is still honoured: the map is the prior for the silent case
  only, and a `Retry-After` carried by a 503 still wins. Checked with one target both
  ways, by the end-to-end script and by a test that writes no cooldown map at all, so
  what it reads is the code default.

- The example config and the generator write 32 MiB for a request body and 64 MiB for a
  response, where they wrote 2 MiB and 32 MiB. The library defaults are unchanged, and
  the two sets are now written as two different things: what a caller gets when nothing
  is written, and what this project recommends for a coding-agent workload whose body
  can hold a screenshot. The limits stay limits. Checked at the boundary against a real
  running gateway by `scripts/e2e.sh`: exactly the limit is forwarded, one byte more is
  a 413 with `code: request_body_too_large` and no upstream call, and the bytes that
  arrive are the bytes that were sent with only the bytes of the `model` value replaced.

- scripts/generate-config.py takes the production numbers as parameters:
  `--max-request-bytes`, `--max-response-bytes`, `--cooldown-max` and `--cooldown-503`,
  which write 33554432, 67108864, 30s and 0s when nothing is given. Those are what this
  script recommends for a coding-agent workload, not the library defaults, which stay
  8 MiB, 32 MiB and 15 minutes: what one provider does is a reason for a recommendation,
  not a reason to change the routing abstraction. CI runs the script against a snapshot
  file, so the check never contacts a provider, and it checks that a generated file is a
  config a real gateway loads and routes a request through.

### Fixes
- A negative size limit is a startup error instead of being quietly replaced by the
  default, which is what made the message in the config check a claim that could never
  print. 0 and absent are the same number in a config file, so 0 means the default: a
  limit someone wrote is not a number to substitute.

- `plainroute_upstream_errors_total` help text named `status`, which that collector can never
  record, and left out two values it can. An upstream that answered with an error status is
  counted by `upstream_attempts_total` with its status class, and a response cap is a reason on
  `plainroute_stream_aborts_total`. The help text now names the five kinds the collector can
  hold: `build_request`, `transport`, `timeout`, `stream_idle`, `client_gone`. The metrics test
  compares the collected text, so the help text and the label names are checked, not assumed.

### Tests

- Provider error passthrough is covered by named shapes: what a rate limiter writes,
  what a service writes for its content safety screener, a JSON shape this project has
  never seen, an RFC 7807 problem document, and a body that is not JSON at all. Each
  reaches the client with the upstream's status, the upstream's Content-Type, and the
  bytes the upstream wrote. The status path is one comparison rather than a table of
  known error types, so an unrecognised shape cannot become a gateway 500; 12 statuses
  are checked on a single-target route, where an attempt count can only be the count for
  that target.
- The fake upstream honours a Content-Type a behaviour names instead of forcing its own
  default. Without that, a test that checks the header the gateway forwards would be
  checking the fake's default and not the proxy.
- Every assertion was shown to fail once by breaking the code it reads: the status
  write, a normaliser added to the error branch, the response header copy, a retry
  inserted inside one request, the Retry-After clamp, and the frame copy. A check that
  only compares a status cannot fail when that status comes from the gateway's own
  reject branch, so the checks that matter read bytes.

### Tools

- `.github/workflows/release.yml`: builds for linux/amd64 and linux/arm64, pushes to GHCR,
  writes an SPDX SBOM, signs the image with cosign in keyless mode against the run's
  identity for both the tag and the immutable digest, and publishes a GitHub Release
  that names the version, the commit, the digest and the two verify commands a reader
  runs. There is no signing key in this repository, so the signature is bound to the run
  that made the build.
- A workflow file cannot check itself, so this one was read by hand and each reading was
  shown to fail once by breaking the file: the parse, the platform list, every reference
  against the step ids and job outputs the file declares, the permissions against what
  the steps claim to need, the quoting inside each run block, the duplicate-key rule,
  and the rule that the release cannot attach a file its own job never created. What the
  file cannot check — image size, the container settings, the health check answering
  /healthz — is written in the README as a measurement with the command that takes it.

- `scripts/smoke-openai-compatible.sh` checks a live OpenAI-compatible endpoint the way
  a client sees it: the model list, a non-stream answer, a streamed answer, a tool call,
  a top-level field this project does not define, an error answer, and one optional
  image. Each line reports the status, the time to the first byte and the total time,
  and prints structural facts only, so neither the key nor the prompt appears in the
  output.
- CI cannot use it to check somebody's provider, so CI runs it against the project's own
  fake upstream, where what is checked is that the checks can pass and that each can
  fail against an answer that should not satisfy it: a stream without the frame that
  ends it, a prose answer to a tool-call request, an error written as an HTML page, an
  endpoint that accepts an empty messages array. The timings are shown to be measured by
  a delayed answer, and the request count is one per check, so no check can pass on an
  answer another check received.

### Docs

- A section for a client that owns retry, such as OpenCode: what the gateway keeps for a
  client that branches on a status (the status, the body byte for byte, `Retry-After` as
  the window it applied, one attempt per request), and the two counters that make a
  client's retries readable — `requests_total` against `upstream_attempts_total`, which
  are equal while no fallback is configured.
- The verification commands, kept apart from the claims: what a gate checks on every
  push, and what only a machine with a docker daemon, cosign and a tag can measure.
  SECURITY.md and the README now say which claim is which kind. The claim about
  publishing was corrected: an image is published only by the release workflow, on a
  tag.

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
- Optional api key gate, off by default. `auth.api_key` is a value in the file; when it is not empty,
  the api key a caller presents is compared in constant time on the API endpoints (`/v1/*`). Nothing
  configured means no gate, and an empty value means the same thing, so a gate cannot be half-on and
  there is no second place for the value to come from. Health, readiness and the metrics page stay
  open. The caller key is never forwarded upstream, never logged, and never appears in a metric
  label: the key the provider sees is always the target key. A rejection is 401 with
  `code: invalid_api_key`, `type: authentication_error`, counted in
  `plainroute_gateway_rejected_total{route="none",code="invalid_api_key"}`, and it is one of the
  requests the gateway answered from its own state. The startup line reports the gate as `on` or
  `off` and never the value, because printing the value would print a secret into a log line. A gate
  that reads as on in the file is on, and that is the whole check.
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

- `cmd/fakeupstream`: controllable upstream keyed by API key value, with per-key sequences,
  a `/stats` endpoint that reports what the server saw, and `/last?key=` for the last request
  it received for one key, as one line per fact. That is the shape a script greps when it has
  to tell what the gateway forwarded from what it dropped.
- `cmd/bench`: load generator that measures what the client sees and then reads the gateway's
  own counters, failing when upstream attempts exceed client requests or when a gauge is
  still non-zero after the load stopped.
- `scripts/e2e.sh`: the end-to-end cases against two real processes — the api key gate,
  the request path, the startup errors, both distribution strategies and the cooldown
  window. It stops before starting if something is already listening on the ports it
  needs, because a leftover process answers the checks instead of the one the script
  started. A startup check runs under a timeout, so a build that stops reporting the
  problem fails the check instead of leaving a process that never exits.
- `scripts/generate-config.py`: builds a config for one upstream with N key slots from the live
  model endpoint, falling back to a snapshot file when that endpoint gives nothing. Each slot is
  written as `REPLACE_ME_ACCOUNT_n`, and `--key-prefix` is gone because the script has no argument
  that takes a key value: a real key gets into the file only when a person types it in. The generated
  file is the config a gateway runs on, so it is the secret file, and CI checks that the placeholder
  the script wrote is the key the gateway then presents, which is what says the file a reader reads is
  the file that runs.
- `docker-compose.yml`: the production shape — loopback-only mapping, read-only root, all
  capabilities dropped, `no-new-privileges`, pid and memory limits, 30 second stop grace, and the
  config file mounted read-only as a directory rather than as a single file, because a single-file
  bind mount pins the inode the container started with and a live reload would then see nothing. The
  host keeps the directory at 700 and the file at 600. Checked by starting it for real on a machine
  with a docker daemon; the bind-mount behaviour itself is stated as a deployment fact and confirmed
  where the daemon runs.

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
