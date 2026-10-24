# ADR-0179: adopt `testing/synctest` bubbles for timer-based tests

- Status: accepted
- Date: 2026-10-23
- Issue: #64

## Context

Several unit tests exercise concurrency by advancing real wall-clock time
(poll loops with `time.Sleep`, `time.After` deadlines, 200 ms "nothing may
resurrect it" windows). On loaded CI hosts that is the classic flaky class,
and it also makes the test suite needlessly slow: the jobs runner tests
sleep through the store's real 2 s backoff base, so
`TestRetryWithBackoffThenSuccess` alone took 1.6–2 s of wall time per run.

Go 1.25 added `testing/synctest`; our go.mod declares `go 1.27` and the
toolchain (go1.27.2) ships the package, so no dependency or version change
is needed. `synctest.Test(t, f)` runs `f` in a bubble where `time.Now`,
`time.Sleep`, and `time.NewTimer` use a fake clock that only advances when
every goroutine in the bubble is durably blocked.

## Options

1. **Per-test bubbles where they pay.** Wrap a test body in
   `synctest.Test(t, func(t *testing.T) { ... })` when the goroutines under
   test coordinate only via channels/timers/context, so waits become
   deterministic in test time.
2. **Package-wide migration.** Convert every test in the package at once.
   Rejected: most tests (direct `Apply` calls, single-threaded store
   checks) have no clock to fake, and some tests must stay on the wall
   clock (below).
3. **Keep the status quo** (longer deadlines, `patienceScale` factors).
   Rejected: it hides real races instead of removing the dependence on the
   wall clock, and it keeps paying real wall time for every retry/backoff
   path exercised.

## Decision

Adopt option 1, per test, starting with the highest-value packages:

- `internal/jobs` — all 13 tests in `runner_test.go` plus
  `TestForegroundPauseState` in `foreground_pause_test.go`. The poll loops
  (`waitFor` at 10 ms) and the 200 ms "nothing may resurrect it" and 300 ms
  "~30 idle polls" windows now run in test time, while the assertions keep
  the real values they check (the store's 2 s backoff base is still
  asserted elsewhere verbatim).
- `internal/session` — the three tests that race the wall clock around the
  run loop: `TestWakeWakesTheRunLoop`, `TestRunDrainsTheInboxOnOneGoroutine`
  (`session_test.go`) and `TestConcurrentSendersRaceFree`
  (`property_test.go`). Their 5 s `time.After`/`time.Sleep(1ms)` polling
  deadlines are now fake-clock deadlines; the reducer itself already runs
  on an injected fake clock in the harness, so the bubble adds no real
  deadlines.

Explicitly **not** converted, and staying wall-clock:

- Tests that interact with external processes or devices with real
  deadlines (`internal/runtime/manager_test.go` polling for tesseract child
  processes, Dolphin/emulator tests, live provider tests, ffmpegpipe
  throughput tests). A synctest bubble must not block on external
  processes whose completion it cannot observe in test time.
- Single-threaded tests without timers; wrapping them would add bubble
  overhead without removing a race.

## Tradeoffs

- A bubble fails the test if any goroutine it created is still alive when
  the body returns; the converted tests already cancel their contexts and
  join their goroutines (runner `Stop`, session `Run` returns on
  `ctx.Done`), so the bubbles close cleanly. The `internal/jobs` runners'
  SQLite work is local file I/O with no external deadline — safe in a
  bubble.
- `t.Cleanup` registered inside a bubble runs after the bubble closes, so
  the 3 s stop timeout in `mustStart`'s cleanup uses the real clock
  there; that is intended, it is the teardown path.
- Fake-clock time makes the tests' timing assertions exact but only
  meaningful while the code under test uses the `time` package
  (it does: the runner polls with timers, the session loop uses
  `time.After`). Code that reads a monotonic source outside `time` would
  be invisible to the bubble; none does in the converted tests.

## Validation

- Per-test wall time before/after and `-count=50` pass rates: see
  `/tmp/synctest/REPORT.md` (evidence bundle for this change).
- `go test -race -count=20` on `internal/jobs` and `internal/session`;
  `make lint` — both green (details in the report).
- No assertion was weakened: the diff is wrapper-only
  (`synctest.Test(t, func(t *testing.T) { … })` plus indentation); all
  original Fatalf/Fatal conditions are byte-identical.

## Consequences

- New flaky-timing tests should be proposed as bubble tests when the code
  under test coordinates only via `time`-based waits; the converted tests
  are the template.
- Tests that legitimately measure wall-clock throughput must keep using the
  real clock and must say so in a comment.

## Reversal plan

Reverting is mechanical: remove the `synctest.Test(t, func(t *testing.T)
{` line, the matching `})`, and the `testing/synctest` import in
`internal/jobs/runner_test.go`, `internal/jobs/foreground_pause_test.go`,
`internal/session/session_test.go`, and
`internal/session/property_test.go` (equivalently, restore the pre-change
files). No production code changed, so there is no compatibility
consideration.
