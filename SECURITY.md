# Security

<!-- SPDX-License-Identifier: AGPL-3.0-only -->

## Reporting a problem

Use a private security advisory on the repository rather than a public issue, so a problem
is not announced before it is fixed. Include the version, the shape of the config you ran
with the values redacted, and what happened. If a reproduction exists, include it.

Redact `api_key` in a report. The config file is the secret, so a report should name the
field and the problem rather than paste the file: the code names the field it refused and
never prints the value it read, and a report should do the same. A fingerprint of the
content, which is the first five bytes of the SHA-256 of the file, identifies which
content a process acted on without revealing what it holds.

A fix lands as an ordinary commit, and the advisory is published when the fix is out.

## Authentication

The gate is one optional key, and off is the default. `auth.api_key` is a value in the
config file; when it is not empty, the api key a caller presents in the ordinary
`Authorization: Bearer` form is compared with it in constant time, on the API endpoints,
before any byte of the request body is read. Nothing configured means no gate, and an
empty value means the same thing: anyone who can reach the listening port can then use the
configured targets, which is why the compose file maps the port on loopback only.

What the gate is not, and what is not a bug to be fixed by adding complexity:

- It is not a table of keys. One key decides who may call; it cannot say which caller is
  which. Accounts, authorisation, per-caller quotas and multi-tenancy belong in front of the
  gateway, not inside it.
- It does not cover the health, readiness or metrics endpoints. Those carry counts only, and a
  monitoring system that cannot authenticate is the ordinary case.
- There is no half state. The value written in the file is the value in force, so a gate that reads as
  on in the file is on. Under the old form, `auth.api_key_env` named a variable, and a gate could read
  as on in the file while the process had nothing to compare against. It could also be on with a value
  nobody chose for it: a generic name such as `API_KEY` can be exported by another program in the same
  shell or set by a shared `.env`, so a name collision was a gate failure the config check could not
  catch, because the variable was set. Writing the value down removes that failure mode, and this is
  why the environment lookup went away with it.
- The startup line reports the gate as `on` or `off` and never the value, because printing the value
  would print a secret into a log line.

The caller key is never logged, never written to a metric label, and never forwarded upstream.
The key a provider sees is always the target key the operator configured, so a caller
cannot aim a request at a key it was not given.

## What it does do

- **The production config is a secret.** It holds the keys as values, so it needs 600 permissions on
  the host, the directory 700, it is gitignored, and a container mounts it read-only. What a gateway
  actually runs on is never in the repository; `config.example.yaml` holds placeholders, and the
  generator writes placeholders, so a script cannot put a key into a file. What is written in the file
  is what runs, which is the property the old two-source design did not have.
- **Nothing sensitive is logged.** No key, no `Authorization` header, no prompt content,
  no response content. The logging package has no field that could carry a secret, so a
  log line cannot leak one by accident. A config error names the field and the problem, and prints a
  value only when that value is what it is complaining about, which can be a duration or a mode name
  and never a key, because nothing can find a key wrong except that it is empty.
- **Upstream URLs come only from configuration.** The gateway does not follow redirects, so
  a response cannot send the proxy to a host that was not configured while carrying the
  request body. `base_url` is checked at load: no credentials, no query, http or https only.
- **A client cannot aim a request at a key it was not given.** `Authorization` is always the
  key configured for the target the gateway selected. When the gate is on, the caller key
  is compared in constant time before the request body is read, and then not used again.
- **The gate cannot be half-on.** `auth.api_key` is off when it is empty, and there is no second
  place a value can come from, so there is nothing for the file and the process to disagree about.
- **A change to the file cannot make the gateway run something it did not validate.** The
  file is watched, and a reload swaps one whole generation only after a complete read,
  parse and validation. A file that decodes but does not validate is reported and not
  applied: nothing is partly applied, the process does not exit, and the configuration in
  force stays the last one that was valid. A request keeps the generation it started with
  for its whole life, so a reload cannot hand a request the limits of one configuration
  and the targets of another.
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
  body is read; an empty value means off, and a target with no key is a startup failure that names
  the target, with every problem in the file reported at once.
- A request that the gate rejects never reaches an upstream, which is checked by counting
  what the fake upstream received rather than by reading a log line.
- Size limits are enforced before forwarding: exactly the limit is forwarded, one byte more is
  a 413 with `code: request_body_too_large` and no upstream call.
- `Retry-After` is clamped, checked with one target at three different clamp settings.
- No key, prompt or response body appears in a log line, a metric label or an API response. The
  logging package has no field that could carry one, so a line cannot leak one by accident. The check
  uses a value that must never come back out (`PLAINROUTE_TEST_SECRET_DO_NOT_LEAK_918273645`), as a
  key in a file the process is asked to reject, and asserts the needle is absent from the error text,
  from the log lines and from the metrics page: `internal/config/leak_test.go`, the log test in
  `internal/proxy/proxy_test.go`, and the startup case in `scripts/e2e.sh`.
- The file is watched, so a change is applied without a signal and without a restart, and
  the check reads what the fake upstream received rather than a log line: the key a
  request reaches is the key the file named at the time the request was taken, checked by
  rotating a key and then sending a request. A file that decodes but does not validate is
  reported and not applied, the process stays up, and the configuration that was in force
  keeps serving; the same bad content is reported once across three more looks. The caller
  key rotates the same way, checked by the status codes on the API endpoints, on the same
  process.
- A request pairs the key it reached with the path the same version of the file wrote,
  which is the observable form of taking one snapshot per request: a pairing no version of
  the file wrote is a request that read the file twice. The run that checks it is longer
  than one reload interval, so the swap lands inside it and requests on both sides are
  covered.
- A provider error is forwarded with the upstream's status and the upstream's bytes, which is
  what a client that branches on the status depends on.

Each of those was shown to fail once by breaking the code it reads. A check that cannot
fail is not a check, so a green run is not treated as proof until the same check is seen
to report a failure against a deliberate break.

A request can be shown to mix two generations, but only by a break that reads one value
twice, and that is worth stating because the first version of this document said it could
not be shown at all. The break has to be the kind this project refuses: a cache keyed by
target name, so one value comes from an older version of the file while the rest of the
request comes from the one in force. Caching the endpoint by name makes a request reach
the path of one version with the key of another, and the pairing check reports that as a
pairing no version wrote. Caching the key by name is the same kind of break, and it fails
the check that asks which key answered; it cannot fail the pairing check, because that
check first requires a request to reach each key, and with the key cached only one key is
ever reached. The same distinction runs through the stream checks: a check that looked
only for the frame the fake upstream had already written could not fail, because that
frame is written before the cap is consulted again, so the check that answers is the one
that asks for the terminal frame of the stream, and it fails against a cap read at each
read instead of once at the start of the request.

One claim stays an invariant, and the reason is worth keeping: a reload cannot be shown to
cancel an in-flight stream, because there is no cancel path to break. It is stated as a
property the design holds by taking one snapshot per request, and the closest thing to a
check is the unit test that holds a request open across the swap and reads the generation
it used (`internal/proxy/generation_test.go`).

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
