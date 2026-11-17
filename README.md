# PlainRoute

<!-- SPDX-License-Identifier: AGPL-3.0-only -->

> **A small, boring, transparent LLM gateway.**

PlainRoute is an OpenAI-compatible HTTP gateway in front of one or more deployments of
the same model. It routes a request to a deployment, sends the body it received, copies
the response bytes back, and reports what happened. It does not rewrite the response,
does not retry on its own, and does not grow features that touch the streaming path.

This project intentionally has a smaller scope and avoids modifying streaming responses
whenever possible.

PlainRoute is not affiliated with OpenAI, NVIDIA, LiteLLM, or any other company or
project. The OpenAI-compatible shape it serves is an interface it talks to, not an
endorsement or an affiliation.

## What it does

- One upstream attempt per client request. Never more.
- Per-deployment cooldown: a deployment that returned a status code is taken out of the
  pool for the configured time, or for the upstream's own `Retry-After`, whichever is
  shorter than the configured maximum.
- Model alias mapping: the name a client sends can map to the real name the upstream
  expects. Only that one field changes, and it changes by splicing bytes into the
  original document rather than re-serialising it.
- Streaming responses are copied chunk by chunk and flushed per chunk, so the bytes the
  upstream produced arrive as the bytes the client receives.
- Structured logs, Prometheus metrics, health and readiness endpoints, SIGHUP reload,
  graceful shutdown.
- An optional api key gate for the API endpoints: off when nothing is configured, one
  constant-time comparison when it is.

## What it does not do

No guardrails, no prompt logging, no response enrichment, no cache, no queue, no
budget, no rate limiter of its own, no provider SDK zoo, no template rendering. Every one
of those would either change the bytes on the wire or turn one client request into more
than one upstream request. That is the line this project does not cross.

## Why it exists

Most gateways in this space are large because they add behaviour: they retry, they
transform, they summarise, they decide what a model should say. A gateway that does those
things cannot promise what the client asked for, and it cannot promise that one request
did not become five requests.

PlainRoute exists for the case where you want the boring thing: many accounts or
deployments behind one name, a key that got rate limited is taken out of the pool, the
response is what the provider sent. If a gateway answers a request the provider answered
successfully, that is a bug in this project.

## Quick start

```bash
go build -o bin/plainroute ./cmd/plainroute
cp config.example.yaml config.yaml
export EXAMPLE_API_KEY_1=...
export EXAMPLE_API_KEY_2=...
./bin/plainroute -config config.yaml
```

```bash
curl -s http://127.0.0.1:8080/healthz
curl -s http://127.0.0.1:8080/v1/models
curl -s http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"demo","stream":true,"messages":[{"role":"user","content":"hi"}]}'
```

Keys are read from the environment. Nothing that looks like a secret belongs in a config
file, and the gateway never writes a key into a log line.

## Model name mapping example

```yaml
models:
  demo:
    upstream_model: provider/model-one
    deployments:
      - name: account-1
        base_url: https://api.example.com/v1
        api_key_env: EXAMPLE_API_KEY_1
        max_active: 4
      - name: account-2
        base_url: https://api.example.com/v1
        api_key_env: EXAMPLE_API_KEY_2
        max_active: 4
```

A client asks for `demo`. The gateway sends `provider/model-one` and the key from the
environment variable of the deployment it picked. The response comes back as the provider
produced it.

## Multiple accounts example

Two accounts for the same model, with a cooldown that keeps a limited account out of the
pool:

```yaml
routing:
  strategy: least_active          # ties keep the order they appear in the config
  cooldown:
    status:
      429: 10s
      401: 60s
      503: 15s
    max: 5m
    timeout: 30s                  # 0 disables cooling after a connection failure

models:
  chat:
    upstream_model: gpt-4o-mini
    deployments:
      - name: work
        base_url: https://api.openai.com/v1
        api_key_env: OPENAI_WORK_KEY
      - name: personal
        base_url: https://api.openai.com/v1
        api_key_env: OPENAI_PERSONAL_KEY

retry:
  enabled: false
```

Account A returns 429. That request ends with the upstream error, exactly as the provider
answered it, and account A is cooled down. The *next independent request* chooses account
B. Nothing inside the first request tried account B. This is the whole design in one
paragraph, and there is a test and an end-to-end script that check it.

## Local OpenAI-compatible endpoint example

Anything that speaks the OpenAI shape works, including a local server:

```yaml
models:
  local:
    upstream_model: qwen3:8b
    deployments:
      - name: ollama
        base_url: http://127.0.0.1:11434/v1
        api_key_env: LOCAL_KEY    # any value; the key is still what identifies the deployment
```

## Api key gate (optional)

Off by default, and it is one key rather than a table of keys. `auth.api_key_env` names one
environment variable; when it resolves to a value, the api key a caller presents is compared
with it in constant time, so a wrong key does not reveal how many bytes matched.

```yaml
auth:
  api_key_env: PLAINROUTE_API_KEY
```

- Nothing configured means no gate, which is the documented default. A variable named in the
  config but not present in the environment is a startup failure, not a quietly open gate,
  because a half-configured gate reads as a working one.
- Give the variable a name this project owns: `PLAINROUTE_API_KEY`, not `API_KEY`. A generic name
  can be exported by another program in the same shell, or set by a shared `.env`, and then the
  process starts and compares a value nobody chose for it. A name collision is not a startup
  failure, so this is the one gate failure mode the config check cannot catch — the prefix is what
  tells this project's secret from another program's. The startup line prints the gate state and
  the name it read, never the value, so a collision is at least visible in the log.
- The key is read the ordinary way: `Authorization: Bearer <key>`, which is what any
  OpenAI-compatible client already sends. No separate header, no second naming scheme.
- The caller key and the deployment key are different things. The caller key decides who may
  use the gateway; the key that goes upstream is always the one the operator configured for
  the selected deployment, so a caller cannot aim a request at a key it was not given. The
  caller key is never forwarded, never logged, and never appears in a metric label.
- The gate covers the API endpoints (`/v1/*`). Health, readiness and the metrics page stay
  open, because a monitoring system that cannot authenticate is the ordinary case and those
  pages carry counts only.
- A rejection is the OpenAI error shape: 401, `code: invalid_api_key`,
  `type: authentication_error`. Rejections are counted in
  `plainroute_gateway_rejected_total{model="none",code="invalid_api_key"}`, the family of
  requests the gateway answered from its own state.
- One key is not accounts. Knowing which caller is which would need a table of keys, and a
  table of keys is the point this project stays out of.

## Routing and cooldown semantics

- A deployment is selectable when it is not cooling down and has not reached
  `max_active`.
- Selection is `least_active` by default, with `round_robin` as the other option. Ties
  keep config order, so the choice is reproducible rather than random.
- The two strategies answer different questions, and this was measured rather than assumed:
  12 sequential requests with four deployments gave `round_robin` 3/3/3/3 and `least_active`
  12/0/0/0, because sequential traffic never overlaps and every request ties on "fewest in
  flight". If the limit you care about is per key, spread with `round_robin`; if it is per
  connection pool, `least_active` is the closer fit. Under real concurrency both converge.
- Cooldown length comes from `routing.cooldown.status`, and a `Retry-After` from the
  upstream wins when it is present. The value is clamped by `routing.cooldown.max` so a
  hostile or broken upstream value cannot make a deployment unavailable for an
  unreasonable time. An unparseable `Retry-After` falls back to the configured duration.
- When every deployment for a model is cooling, the gateway does not contact an upstream.
  It answers 429 with the OpenAI error shape and `code: all_deployments_cooling_down`,
  and `Retry-After` is the recovery time of the deployment that recovers first.
- The `Retry-After` the gateway sends is the window it actually applied, not whatever the
  upstream printed.
- The gateway does not follow redirects. Upstream URLs come from config only, so a
  response body cannot point the proxy at a host that was not configured.
- Hop-by-hop headers are dropped in both directions. `Authorization` is always the key
  configured for the selected deployment, so a client cannot aim a request at a key it
  was not given.

## Why retry is off by default

A gateway that retries changes the meaning of a request. One client request becomes two,
three, or ten upstream requests, and the party being rate limited is the one that told you
to slow down. Retrying also makes the numbers impossible to read: you cannot prove that
client requests and upstream attempts are the same, because they are not.

The client owns backoff, because the client is the one who knows whether a request is
worth repeating. `retry.enabled` exists for people who insist, and when it is on, a retry
only happens before any byte of the upstream response has reached the client. Once bytes
are on the wire, there is no taking them back.

There is a regression test for this: 50 client requests must never produce more than 50
upstream attempts, and the metrics must report the same numbers.

## Streaming transparency

- The response body is copied with no accumulation; the gateway never holds a model
  response in memory.
- The stream is flushed per read chunk, so frames reach the client in the order the
  upstream produced them.
- Frames are not re-formed. The opening role delta, tool call deltas as separate frames,
  and `[DONE]` all survive exactly.
- A stream that fails before the first byte is reported as an error, not as an empty
  successful response.
- A stream idle watchdog aborts a stream that has gone silent. The gateway does not invent
  a closing frame; the response ends with what it received.
- A client that goes away cancels the upstream request, and the two are told apart: an
  idle timeout is recorded as `stream_idle`, a client that left as `client_gone`.
- `timeouts.request` is 0 by default, so a long reasoning stream is not killed by an
  ordinary request timeout. Only the idle timeout applies.
- Compression is left to the client and the upstream. PlainRoute never decompresses, so
  the bytes it copies are the bytes the upstream sent.

## Metrics

`GET /metrics` (on by default):

| Metric | What it is |
| --- | --- |
| `plainroute_requests_total{model,deployment,status_class}` | Client requests handled |
| `plainroute_upstream_attempts_total{model,deployment,status_class}` | Upstream requests actually sent |
| `plainroute_active_requests{model,deployment}` | Requests in flight per deployment |
| `plainroute_deployment_in_cooldown{model,deployment}` | 1 while a deployment is cooling |
| `plainroute_cooldown_events_total{model,deployment,reason}` | Times a deployment was cooled down |
| `plainroute_request_duration_seconds{model,deployment,stream}` | End to end duration, per request |
| `plainroute_ttft_seconds{model,deployment}` | Time to the first upstream byte |
| `plainroute_stream_duration_seconds{model,deployment}` | Duration of a streamed response |
| `plainroute_bytes_streamed_total{model,deployment}` | Bytes copied to clients |
| `plainroute_upstream_errors_total{model,deployment,kind}` | Upstream failures by kind |
| `plainroute_client_disconnects_total{model,deployment}` | Clients that went away |
| `plainroute_stream_aborts_total{model,deployment,reason}` | Streams ended by the gateway |
| `plainroute_gateway_rejected_total{model,code}` | Requests the gateway answered from its own state |
| `plainroute_config_reloads_total{outcome}` | SIGHUP reloads, by outcome |
| `plainroute_in_flight_requests` | Requests being handled right now |
| `go_goroutines`, `go_memstats_heap_alloc_bytes`, `process_resident_memory_bytes` | The process itself |

`requests_total` and `upstream_attempts_total` are kept separately on purpose. With retry
disabled they should be the same number for proxied requests, and the difference is exactly
the requests the gateway answered itself. `cmd/bench` reads these numbers from the gateway
rather than counting them on the client, and fails if they disagree.

When the api key gate is on, its rejections are part of that difference: they are counted by
the gateway from its own state, with `model="none"` and `code="invalid_api_key"`, and no
upstream is contacted.

Logs carry routing facts only: request id, logical model, upstream model, deployment,
status, duration, time to first byte, bytes streamed, cooldown events, disconnects. There is
no field in the logging package that could hold a key, a prompt or a response body, so a
log line cannot leak one by accident.

## Docker

```bash
docker build -t plainroute .
docker run --rm -p 127.0.0.1:8080:8080 \
  --env-file .env \
  -v $PWD/config.yaml:/etc/plainroute/config.yaml:ro \
  plainroute
```

Pass the variables by the names the config uses. `api_key_env` names an environment
variable, and the gateway looks that name up in the environment, so every name the
config uses has to be set — in the example config that is `EXAMPLE_API_KEY_1`,
`EXAMPLE_API_KEY_2` and `LOCAL_KEY`. A container missing one of them does not start,
and the error names every missing variable at once, so a missing key is a startup
failure rather than a request that fails later.

The image is 28 MB, runs as a named non-root user, and has no write access anywhere. The
health check is the live endpoint. Readiness is not checked inside the image, because a
deployment that is cooling down is still a healthy container.

The compose file is the shape this project runs in production: loopback-only mapping,
read-only root filesystem, all capabilities dropped, `no-new-privileges`, a pid and memory
limit, a 30 second stop grace, and keys from a `.env` file with 600 permissions. Verified by
starting it for real: `health=healthy user=plainroute readonly=true caps=[ALL]`.

Inside a container, `127.0.0.1` is the container itself. If the upstream runs on the host,
write `host.docker.internal` in the config file (the compose file maps that name to the
gateway), not `127.0.0.1`.

The name a published image would carry is `ghcr.io/40960/plainroute`. Nothing is published
from this repository automatically; the CI job builds the image without pushing it.

## Security

- Keys come from environment variables and are never written into a config file, a log
  line, or an image layer.
- The gateway never logs a key, the `Authorization` header, prompt content, or response
  content.
- Upstream URLs come only from configuration; the gateway does not follow redirects, so a
  response cannot redirect the proxy to an arbitrary host with the request body.
- `base_url` is checked at load: no credentials, no query, http or https only.
- Request bodies and response bodies have configured size limits, checked before anything
  is forwarded.
- The metrics page reports counts, gauges and histograms. It contains no key names and no
  prompt content. If the page must not be public, put it behind your own restriction or set
  `metrics.enabled: false`.
- Authentication is optional and off by default. When `auth.api_key_env` resolves to a value,
  the api key a caller presents is compared in constant time, on the API endpoints only. With
  nothing configured there is no gate, and then any process that can reach the listening port
  can use the configured deployments — which is why the compose file maps the port on
  loopback only.
- The caller api key is never forwarded upstream, never logged, and never appears in a metric
  label. The key the provider sees is always the deployment key.
- There is no authorisation, no accounts and no multi-tenancy. One optional key decides who
  may call; it does not say who is calling. If you need that, it belongs in front of the
  gateway.
- See [SECURITY.md](SECURITY.md) for how to report a problem.

## Limitations

Known and deliberate. These are not bugs to be fixed by adding complexity:

- No queue, no backpressure beyond `max_active`, no per-model concurrency limit.
- No circuit breaker beyond the cooldown map.
- No provider-specific error normalisation: an upstream error body is forwarded as it
  arrived, which means different providers produce different error bodies.
- No retry, so a transient failure is a failed request. That is the point.
- No cache, no accounting, no budgets, no billing.
- No OpenTelemetry traces. Metrics and logs only.
- No accounts, no multi-tenancy, no per-key quotas. The api key gate is one optional key, not
  a table of keys, so it cannot say which caller is which.
- Model fallback is opt-in, bounded, and only possible before the first byte reaches the
  client.
- A response cap can stop copying mid-frame. The client keeps what arrived; the gateway
  does not close a frame it did not receive.
- Single process, single binary, no clustering. State is per process: two gateway
  processes do not share cooldowns.

## Troubleshooting

**Every request gets 429 from the gateway with `all_deployments_cooling_down`.** The
gateway is answering from its own state, not from a provider. Look at `Retry-After`: it is
the recovery time of the deployment that recovers first. If the window is longer than you
expected, the upstream sent a big `Retry-After`, and `routing.cooldown.max` clamped it.

**A stream ends without `[DONE]`.** The upstream stopped sending. The gateway does not
invent a closing frame. Check `plainroute_stream_aborts_total` and the log line with
`kind=stream_idle`.

**A stream is cut off earlier than expected.** `server.max_response_bytes` stopped copying.
The client keeps the bytes that arrived; the gateway counted the limit in
`plainroute_stream_aborts_total` with `response_limit`.

**A request fails with 502 or 504 while the provider looks healthy.** The gateway does not
retry. A connection failure or an idle timeout is reported as it happened. Check the log
line for `kind` (`transport`, `timeout`, `stream_idle`, `client_gone`).

**The gateway returns 500.** That should not happen for a request the upstream answered. If
it does, it is a bug in this project, and it should be reported with the request id.

**Keys are missing at startup.** The config reports every problem at once, naming the
environment variable. A config that references an unset variable is a startup failure, not
a runtime surprise. The same is true for the gate: an `auth.api_key_env` that names an unset
variable stops the process rather than leaving a gate that looks on but checks nothing.

**Every request gets 401 with `invalid_api_key`.** The gate is on, because
`auth.api_key_env` resolved to a value. The client has to present that key as
`Authorization: Bearer <key>`, the ordinary form. A gateway that is running either has no
gate or a working one, so this is a client-side mismatch, not a half-configured gate. The
key a caller presents is not the key the provider sees, so if the provider works when you
send the caller key directly, you are reading the wrong variable. If a key that is not yours is
accepted instead, the name is the problem: a generic name such as `API_KEY` reads whatever
another program exported, so the gate is holding that program's value.

**Reload did nothing.** A failed `SIGHUP` reload keeps the running config instead of
breaking a working gateway. The failure is logged and counted in
`plainroute_config_reloads_total{outcome="failed"}`. Deployment state, including cooldowns,
survives a successful reload.

## License

AGPL-3.0-only. See [LICENSE](LICENSE). Every file in the repository carries `SPDX-
License-Identifier: AGPL-3.0-only` in its header, apart from `LICENSE` itself and
`go.sum`, which cannot carry a comment. `./scripts/check-headers.sh` checks it, and
`make headers` runs the same check locally.

Modifications and distribution must follow AGPL-3.0. If you modify the program and serve
it to users over a network, the AGPL-3.0 network-interaction clause (section 13) may
require providing the corresponding source code to those users. This is a statement about
the licence text, not legal advice.

## Development

```bash
make fmt vet test race build headers
./scripts/e2e.sh          # seven end-to-end cases against two real processes
make bench                # load test, checked against the gateway's own counters
python3 scripts/generate-config.py --keys 4 --out config.yaml   # model list from the live endpoint, snapshot fallback
```

See [CONTRIBUTING.md](CONTRIBUTING.md), [docs/architecture.md](docs/architecture.md), and
[CHANGELOG.md](CHANGELOG.md).
