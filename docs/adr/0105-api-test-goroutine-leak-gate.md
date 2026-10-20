# ADR-0105: package-level goroutine leak gate in the api test suite (and the capturePush stop contract)

## Status

Accepted (2026-10-20, issue #18).

## Context

Issue #18: the `internal/api` package hangs or slows near the end of long
`-race` runs, at a different test each time (compounded by the load-sensitive
tests of #16). The capture push/preview path was the suspect.

Reproduction (before the fix): every `capturePush.Start` in
`internal/api/capture_test.go` spawns one watcher goroutine
(`go func() { <-ctx.Done(); c.Close() }()`) that only watches the context.
The preview/probe handlers pass the **request** context, and in the tests
that context is the `httptest` request's `context.Background()`-derived
context, which is never cancelled. The stack closes the source after every
grab (`defer src.Close()` in `stack.Snapshot`/`stack.Probe`), but the
watcher does not observe `Close` — it stays blocked on `ctx.Done()` forever.
Measured: one goroutine per `Start`; 20 Start/Close cycles left 20 goroutines
on the stack `(*capturePush).Start.func1` (`chan receive (nil chan)`), and
the same signature survived the real preview/probe request path (4 per full
package run: one each for the preview test and the three probe tests).

Two further pre-existing findings came out of the gate (filed as GitHub
issues, out of scope here):

- The `statusTestEnv` (status_test.go) never closes its events hub, so the
  process-lived `statusEngine` loop (which exits only when the hub refuses a
  publish) outlives its tests.
- The `scenarioWorker` (tests_worker.go) stops of its own accord when the
  store stops answering — bounded by one 10 s heartbeat tick plus ten
  250 ms claim errors after a test closes its store — a stop window that
  straddles any short gate grace.

## Decision

1. **The capturePush test double is tied to both cancel and stop.**
   `capturePush` gains a `stop` channel closed by `Close`; the watcher
   selects on `ctx.Done()` (which still triggers `Close`, the source-side
   stop) **or** `stop` (which exits the watcher when the source is closed
   without a context cancel). A source that the stack closes after every
   preview/probe grab therefore leaves no watcher behind, regardless of
   whether the request context is ever cancelled. This mirrors the real
   sources' contract: production request contexts are cancelled when the
   handler returns, but a source's `Close` is the stop that must always
   bound its goroutines.

2. **The api package's TestMain gains a goroutine leak gate.** No new
   dependency (goleak is not in go.mod): a hand-rolled check that
   (a) takes a pprof goroutine dump (legacy per-goroutine format,
   `WriteTo(_, 2)` — Go 1.24+ writes collapsed/binary formats for debug
   0/1, which carry no per-goroutine blocks or created-by line) before
   `m.Run()`, (b) takes another after, with a bounded 2 s grace for
   transient workers (a store's `database/sql` connectionOpener exiting its
   Close), and (c) fails the package with a dump of every stack whose
   **count grew** between the two dumps. The delta-of-stacks-multiset is
   the "ignore known background workers" mechanism: anything alive at
   baseline (test runner, runtime) cancels out.

3. **Two process-lived workers are acknowledged explicitly**
   (`isIgnoredGoroutine`), each named with its exit condition:
   `(*statusEngine).run` (process-lived by design, ADR-0104; the test
   fixture's hub-close gap is filed) and `(*scenarioWorker)` and its
   per-job children (stops when the store stops answering; the stop window
   straddles the grace, issue #20). The match is on the workers' own frames: a
   goroutine they spawned deeper (scenario/pipeline code) does not carry
   those frames and still fails the gate. The main goroutine (the runner
   executing the gate itself) is excluded because its own stack differs
   between the two dumps by construction.

## Options considered

- **go.uber.org/goleak**: rejected — not in go.mod; adding a dependency for
  one package's TestMain is a larger step than the hand-rolled delta check,
  and goleak's default options would still need the same ignore list for the
  two process-lived workers.
- **runtime.NumGoroutine before/after count**: rejected as the verdict — a
  count delta is masked by transient workers alive at sampling time (the
  template store's sql opener), which let a deliberate leak pass in the
  first prototype. The stack multiset cannot be masked by timing.
- **Fixing the status/scenarioWorker teardowns in this change**: rejected —
  out of scope (other work is landing in those files); both are filed as
  issues with the working recipe.

## Validation

- Repro (pre-fix): 20 Start/Close cycles → +20 goroutines on
  `(*capturePush).Start.func1`; preview/probe request path → +1 per Start;
  stacks dumped from the pprof goroutine profile.
- Gate proof: a temporary test leaking one `context.Background()` watcher
  fails the package with exactly that stack and its created-by line; the
  same gate run with the original capture code fails on the four
  `(*capturePush).Start.func1` stacks; with the fix the package passes.
- `go test -race -count=1 ./internal/api/` completes (148 s, ~151 s
  before; the gate adds ≤2 s at the end).

## Consequences

- A future capture push/preview watcher (or any other test goroutine) that
  outlives its test fails the api package's `-race` run with a dump naming
  the leaking start site, instead of silently accumulating and slowing the
  run (issue #18's symptom).
- The ignore list is the one place a real leak can hide: a leaked goroutine
  whose stack goes through `statusEngine.run` or `scenarioWorker` frames is
  acknowledged. Both are narrow (their own frames only) and each is tied to
  a filed issue with its exit condition documented above.
- `WriteTo(_, 2)` depends on the legacy per-goroutine format remaining in
  `runtime/pprof` (Go 1.27: yes, `debug >= 2`). A future Go that removes it
  breaks the gate visibly (it skips itself with a stderr note), not
  silently.

## Reversal plan

Delete the gate block in `TestMain` (keep the template build) and the
`goroutineStackCounts`/`isGoroutineHeader`/`normaliseGoroutineHeader`/
`isIgnoredGoroutine`/`leakedGoroutineBlocks` helpers in
`internal/api/fixture_test.go`; revert `capturePush` in
`internal/api/capture_test.go` to the ctx-only watcher. No schema, protocol
or production behaviour is touched; the revert is a pure test-file change.
