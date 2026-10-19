# ADR-0071: The fixture capture's StopCapture joins the worker drain on the done channel, and the capture-loop tests ride the injectable clock

Date: 2026-10-19
Status: accepted
Owners: agent worker (capture-join)
Related requirements/tests: docs/spec/11 (Fixture specification), docs/spec/12 (Frame replay), ADR-0054 amendment 2 (parallel adaptive capture, schema 2), ADR-0058 amendment 1 (capture block in the manifest), ADR-0064 (wall-clock determinism of bench replay), AGENTS.md rules 1/7 (bounded queues, graceful degraded modes). Tests: `internal/dolphin/record_capture_test.go` (TestCaptureSelfStopDrainJoin, TestCaptureMinGap, TestCapturePlanByteBudget), `internal/dolphin/real_test.go` (TestRealRecordLegendDense, the end-to-end proof).

## Context and constraints

The M6 fixture recorder's parallel adaptive capture (ADR-0054 amendment 2) grabs frames on the loop's own clock and hands each to four workers (encode + write + classify); a frame number is reserved at grab and published when the worker completes it, so the manifest's frame list is the recording.

`StopCapture` is the join point: `Finalize` calls it before reading the frame list, so no frame may land after the manifest is assembled. The join was keyed on the `captureOn` flag — but the loop clears that flag ITSELF when it stops on its own duration/byte budget (`captureGate`/`captureReserve`). A later `StopCapture` then early-returned without cancelling and joining the worker drain, so `Finalize` could read an empty or partial frame list while frame files were still being written — a manifest shorter than the files on disk (the fixture-loss shape). Production wall-clock drain times masked it: the workers finish in the wall time between the early return and the first manifest read, but the ordering was never guaranteed.

The loop's two stop paths (self-stop on budget, context cancel) both end in the same exit defers: `close(jobs)` (workers drain the bounded in-flight channel), `wg.Wait()` (nothing in flight), `close(done)`. Waiting on `done` is therefore the one join that is correct no matter who stopped the loop.

## Options

1. **Key the join on the loop's done channel** (adopted): `StopCapture` reads `captureDone` under the mutex and waits on it after cancelling; the `captureOn` flag no longer gates the join.
2. Make the loop not clear `captureOn` on self-stop (a StopCapture sentinel instead) — rejected: it would make the flag lie about the loop's state to the gate (which checks it every tick) and hide the real invariant (whoever stops, the defers close `done`).
3. Keep the flag-keyed join and document the budget race — rejected: it is a fixture-loss race, not a documentation gap; AGENTS.md rule 1 requires the outcome (no frame lost), not the happy path.

## Decision

- `StopCapture` (record.go) reads `captureDone`/`captureCancel` under `r.mu` and waits on `done` after cancelling. It is idempotent (the second call receives from the already-closed channel and returns after the drain, like the first) and safe before `StartCapture` (`captureDone == nil` → no-op, no deadlock). It no longer clears `captureCancel`/`captureDone`: `StartCapture` may re-arm with a fresh channel, and both concurrent `StopCapture` calls join the same drain.
- The regression test `TestCaptureSelfStopDrainJoin` rides the injectable capture clock (the `captureClock`/`SetCaptureClock` hook from the previous commit): the loop's 400 ms duration budget closes on its OWN gate while 3-4 heavy 2560×1440 frames are still draining, and the assertions are by event — every reserved frame's file exists on disk immediately after `StopCapture` returns, and `Finalize`'s manifest count equals the files on disk. It fails deterministically against the flag-keyed join (5/5 runs: `StopCapture returned before the drain finished`, frame file absent) and passes 10/10 against the done-channel join under `taskset -c 0 -race`.
- `TestCapturePlanByteBudget` moved off the wall clock onto the same injectable-clock pattern as `TestCaptureMinGap`: the test advances the loop's virtual clock by event until the loop stops itself on the byte budget and then joins the drain. The old version slept 1.5 s of wall time and asserted the stamped span stayed under 0.8 s — an assertion about the scheduler, which fails under single-core `-race` load with or without this change (measured at clean HEAD: 5/5 runs, span 1.3-1.5 s, throwaway worktree, `taskset -c 0 -race -count=30`). The virtual-clock version bounds the span by the budget's own mechanics (gap-limited grabs until the published bytes reach the budget) and adds the same manifest==disk join assertion.

## Validation

- Pre-fix failure: `taskset -c 0 go test -race -count=5 -run TestCaptureSelfStopDrainJoin ./internal/dolphin/` — 5/5 FAIL on the flag-keyed join.
- Post-fix: `taskset -c 0 go test -race -count=10 -run TestCaptureSelfStopDrainJoin ./internal/dolphin/` — ok (10/10).
- Flake predates the change: throwaway worktree at clean HEAD, `taskset -c 0 go test -race -count=30 -run TestCapturePlanByteBudget ./internal/dolphin/` — 5/5 FAIL with the old wall-sleep span assertion (span 1.3-1.5 s > 0.8 s); the dolphin package at HEAD is byte-identical to the working tree's pre-fix code (comment-only diff), confirming the span flake is the pre-existing wall-clock shape, not a regression from the join change.
- Gates: `go vet ./internal/dolphin/`, `taskset -c 0 go test -race -count=30 -run 'Capture' ./internal/dolphin/`, `GN_GATE=test-unit go test -race -count=1 ./internal/dolphin/`.

## Consequences

- A capture that stops itself on its budget can no longer orphan its in-flight frames: the manifest always lists exactly the frames whose files were written, on both stop paths.
- The capture-loop unit tests assert bookkeeping by event on the loop's own clock; none of them asserts a frame count or a span inside a wall-clock sleep window (the failure class ADR-0064 closed for the bench replay, now closed for the capture unit tests).
- `StopCapture` holds `r.mu` only while reading the pair and never waits under the lock, so it cannot deadlock the drive's synchronous `Screenshot`/`recordFrame` path.

## Reversal plan

Restore the flag-keyed early return in `StopCapture` (the pre-fix body is in the git history of this change) and delete `TestCaptureSelfStopDrainJoin`; the byte-budget test reverts to its wall-sleep form only if the host-scheduler starvation is fixed out-of-band — it is load-sensitive by construction and will flake again under `taskset`/`-race` load (measured at HEAD).
