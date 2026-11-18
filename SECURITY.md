# Security

<!-- SPDX-License-Identifier: AGPL-3.0-only -->

## Reporting a problem

Use a private security advisory on the repository rather than a public issue, so a problem
is not announced before it is fixed. Include the version, the config you ran, and what
happened. If a reproduction exists, include it.

A fix lands as an ordinary commit, and the advisory is published when the fix is out.

## Authentication

The gate is one optional key, and off is the default. `auth.api_key_env` names an environment
variable; when it resolves to a value, the api key a caller presents in the ordinary
`Authorization: Bearer` form is compared with it in constant time, on the API endpoints,
before any byte of the request body is read. Nothing configured means no gate, and that is the
documented default: anyone who can reach the listening port can then use the configured
targets, which is why the compose file maps the port on loopback only.

What the gate is not, and what is not a bug to be fixed by adding complexity:

- It is not a table of keys. One key decides who may call; it cannot say which caller is
  which. Accounts, authorisation, per-caller quotas and multi-tenancy belong in front of the
  gateway, not inside it.
- It does not cover the health, readiness or metrics endpoints. Those carry counts only, and a
  monitoring system that cannot authenticate is the ordinary case.
- A key that is named in the config but is not present in the environment is a startup
  failure. A gate that reads as on while checking nothing is the failure mode this rule exists
  to prevent.
- The variable name should carry this project's prefix (`PLAINROUTE_API_KEY`, not `API_KEY`). A
  generic name can be exported by another program in the same shell or set by a shared `.env`.
  The process then starts and compares a value nobody chose for it, which reads as a working gate
  holding the wrong key. This is the one gate failure the config check cannot catch, because the
  variable is set. The startup line names the variable it read, never its value, so a collision
  shows up in the log rather than in the behaviour of a request.

The caller key is never logged, never written to a metric label, and never forwarded upstream.
The key a provider sees is always the target key the operator configured, so a caller
cannot aim a request at a key it was not given.

## What it does do

- **Keys stay in the environment.** A config file never contains a key, and the gateway
  never writes one to a log line, to a metric label, or to an image layer.
- **Nothing sensitive is logged.** No key, no `Authorization` header, no prompt content,
  no response content. The logging package has no field that could carry a secret, so a
  log line cannot leak one by accident.
- **Upstream URLs come only from configuration.** The gateway does not follow redirects, so
  a response cannot send the proxy to a host that was not configured while carrying the
  request body. `base_url` is checked at load: no credentials, no query, http or https only.
- **A client cannot aim a request at a key it was not given.** `Authorization` is always the
  key configured for the target the gateway selected. When the gate is on, the caller key
  is compared in constant time before the request body is read, and then not used again.
- **The gate cannot be half-on.** `auth.api_key_env` is off when it is empty, and a variable
  that is named but not present in the environment is a startup failure.
- **Size limits are enforced before forwarding**, in both directions.
- **A `Retry-After` from an upstream is clamped**, so a hostile or broken value cannot make
  a target unavailable for an unreasonable time.
- **The metrics page contains no secrets.** Counts, gauges and histograms, no key names and
  no prompt content. It is not behind the gate, because monitoring is the ordinary case; if
  the page should not be reachable from outside, restrict it or set `metrics.enabled: false`.

## How to check these claims

Each claim above is either checked by a gate that runs on every push, or written as a
measurement you take yourself. A claim that no gate can check is not written as a fact.

Checked by the gates (`make fmt vet test race build headers`, and `./scripts/e2e.sh`):

- The gate compares the caller key in constant time, on the API endpoints only, before the
  body is read; a named variable that is not in the environment stops the process, and every
  missing name is reported at once.
- A request that the gate rejects never reaches an upstream, which is checked by counting
  what the fake upstream received rather than by reading a log line.
- Size limits are enforced before forwarding: exactly the limit is forwarded, one byte more is
  a 413 with `code: request_body_too_large` and no upstream call.
- `Retry-After` is clamped, checked with one target at three different clamp settings.
- No key, prompt or response body appears in a log line or a metric label. The logging
  package has no field that could carry one, so a line cannot leak one by accident.
- A provider error is forwarded with the upstream's status and the upstream's bytes, which is
  what a client that branches on the status depends on.

Each of those was shown to fail once by breaking the code it reads. A check that cannot
fail is not a check, so a green run is not treated as proof until the same check is seen
to report a failure against a deliberate break.

Measured on a machine, not by CI:

- The image and the container settings: the commands in the README's Development section, run
  against a container you started.
- The log and the metrics page: send a request with a key value you can recognise, then search
  the log lines and `GET /metrics` for it. The search returns nothing, and that is the check.
- A published image: `cosign verify` and `cosign verify-attestation --type sbom`, with the
  identity regexp naming this repository's release workflow. The signature is bound to the run
  that built the image, so a build from anywhere else does not verify against it.
- The smoke script (`scripts/smoke-openai-compatible.sh`): CI runs it against the project's own
  fake upstream, never against a provider, and checks both that its checks pass and that each
  can fail against an answer that should not satisfy it. Its output prints status, timings and
  structural facts, and CI asserts that the key value and the prompt are not in that output.

## In and out of scope

In scope: request handling, routing and cooldown state, header handling, redirect handling,
size limits, what gets logged, the optional api key gate, and the metrics exposition page.

Out of scope: the provider you point at, the network in front of the gateway, your secrets
manager, accounts and per-caller authorisation, and anything you build on top of the gateway.
A vulnerability in the OpenAI compatible API of a provider is not a PlainRoute vulnerability.

## Supported versions

Only the current `main` branch is supported. There are no backported security fixes, because
there are no release lines yet.
