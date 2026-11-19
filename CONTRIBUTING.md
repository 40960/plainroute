# Contributing

<!-- SPDX-License-Identifier: AGPL-3.0-only -->

PlainRoute has a boundary, and contributions that cross it are not accepted. That is not
a style preference; it is what makes the project worth using.

## What will not be accepted

- Retries on by default, or anything that turns one client request into more than one
  upstream request.
- Transforming a response: re-serialising JSON, re-forming SSE frames, merging tool call
  deltas, adding a closing frame the upstream did not send.
- Prompt logging, response logging, summarisation, guardrails, enrichment.
- Caches, queues, rate limiters of its own, budgets, billing, circuit breakers beyond the
  cooldown map.
- Per-provider SDK layers or provider-specific error normalisation.
- A feature that increases streaming pipeline risk. If it is genuinely wanted, it goes in
  the README limitations section as future work, not in the request path.

An objective statement is fine in this project. Attacking other open-source projects is not.

## Priority order

1. Correctness
2. Streaming reliability
3. Simplicity
4. Testability
5. Observability
6. Performance
7. Feature count

When two of these conflict, the earlier one wins.

## Setting up

Go 1.27 or newer. The race detector needs cgo, so run it as `CGO_ENABLED=1 go test -race ./...`.

```bash
make fmt vet test race build
./scripts/e2e.sh
```

`scripts/e2e.sh` starts the fake upstream and the gateway as two real processes and checks
the behaviours this project exists for. It is the check that a unit test cannot replace,
and CI runs it on every push. A startup check uses a timeout, because a build that stops
reporting the problem leaves a process listening, and the script would hang instead of
failing.

## Tests

- A change to the request path needs a test.
- The two tests that matter most are the byte-for-byte streaming test and the no
  amplification test. They must keep passing.
- Do not weaken an assertion to make a test green. If a test fails, find out why: routing
  picks the quietest target, so an expected target is often not the one you
  guessed.
- A check that cannot fail is not a check. Before an assertion is committed, break the
  code it reads once and show that the check fails, then put it back. A needle that
  matches whatever the implementation prints passes without proving anything.
- A check that can only pass is not an assertion either. When a claim cannot be made to
  fail without inventing a path the implementation does not have, write it as an invariant
  and say that, the way `SECURITY.md` writes the reload claim about a stream.
- A check has to read something the implementation decides, not something the fixture
  wrote before the implementation was consulted. A check that looks for bytes the fake
  upstream writes before the gateway reads its own limits can only pass, because the bytes
  arrive either way. Ask for the thing the decision can deny, such as the terminal frame
  of a stream that a cap read late would cut off.
- A check that counts what the upstream saw passes for the wrong reason when the break stops
  the request before it reaches the upstream. The break has to reach the read the check is
  about: a stale key, not a frozen generation, is what makes a check about which key answered
  able to fail, and a check that needs both halves of a decision needs both halves broken.
- A claim about which fields a running process can act on is checked by a test that
  reads the code, not by the table that states it:
  `internal/config/hotreload_table_test.go` compares the table in `README.md` with the
  set in `internal/config/boundary.go`. A document cannot check itself, and a table that
  a reviewer maintains can say a field is hot while the code reads it only on the
  startup path.
- A check about something that happens while the process runs has to be able to fail on a
  process that never looked. Drive the watcher with `Tick()` in a unit test, or in the
  end-to-end script wait with a deadline instead of sleeping a fixed time, and make a run
  that checks a swap long enough for the swap to land inside it. A check that only slept
  passes on a watcher that never read the file.
- The fake upstream is keyed by the API key value, because that is what identifies a
  target.

## What CI can and cannot check

CI runs the unit tests, the end-to-end script and the licence-header check, so a claim
those run check is a fact on every push. A claim CI cannot check is written as a
measurement, with the command that takes it, and says so.

A workflow file cannot check itself: a file that fails to parse produces a run with no
jobs and no logs, so no step inside it can report the parse failure. That is why the
workflow definition is read by hand, and why the checks it runs are inside the job
rather than assumed by it.

## Style

- Conventional commits in English, one commit per logical change: `feat:`, `fix:`,
  `test:`, `docs:`, `ci:`, `chore:`. A commit message says what was actually checked.
- Files and comments in English.
- Every file in the repository carries `SPDX-License-Identifier: AGPL-3.0-only` in its
  header, apart from `LICENSE`, which is the licence text, and `go.sum`, which is machine
  generated and cannot carry a comment. `./scripts/check-headers.sh` is the check and CI runs
  it, so the rule is not left to a reviewer. A file that only mentions the string in prose is
  not a file that carries it.
- A comment explains why, not what. There is no comment in this project that restates the
  code.
- A config field that nothing reads is worse than no config field. If a config value has no
  effect, delete it.
- Do not use a plain bool default when "not written" and "written as false" are different
  things.

## Before a pull request

```bash
gofmt -l .            # must print nothing
go vet ./...
CGO_ENABLED=1 go test -race ./...
golangci-lint run     # optional, but keep it clean
./scripts/e2e.sh
```

Report what failed and what passed. A pull request that changes a documented behaviour must
change the documentation in the same branch.

[Code of conduct](CODE_OF_CONDUCT.md) applies. To report a security problem, see
[SECURITY.md](SECURITY.md).
