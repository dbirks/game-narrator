# ADR-0059: Scheduler `Stop` joins the active playback's terminal result

Date: 2026-10-18
Status: accepted
Owners: agent worker
Related requirements/tests: docs/spec/07 ("audio sink must never block session event loop", speech scheduler lifecycle), docs/spec/12 ("Zero stale audio starts", stop target <200 ms), ADR-0023 (speech cache and scheduler — the scheduler owns playback), ADR-0057 (TTS streamed synthesis; the scenario harness's `audio_evidence_clean` check and filesink evidence), `internal/speech` (scheduler, filesink), `internal/scenario` (TTS playback half)

## Context and constraints

ADR-0057 gave the scenario harness a real playback half: a generation-fenced `speech.Scheduler` over a `filesink` under a per-run temp dir, with the implicit `audio_evidence_clean` check asserting that the sink dir carries exactly one evidence WAV per started utterance and no `.tmp` leftover. The filesink writes its evidence WAV only at completion — staged as `.tmp`, renamed on finish, after the stop fade.

The scheduler's shutdown path did not wait for that completion. `Scheduler.Stop` cancels the loop's context and waits (bounded, 2 s) for the loop to exit; the loop's `ctx.Done` branch called `active.handle.Stop(stopFade)` on the in-flight playback and then left the loop — without consuming the playback's terminal `Done` result. The sink's delivery goroutine was still running (fade sleep + evidence write + rename) when `Stop` returned. Consequences, both observed:

1. **The evidence check raced the final write.** `TestScenarios`/`gn-t034-decision-delayed` flaked: the check read the sink dir while the last utterance's WAV was mid-write — `0 evidence WAV(s) for 1 started utterance(s)`, or a leftover `.tmp`. Reproduced 1–2 of 3 runs before the join (deterministic in a focused test, 4 of 10 iterations).
2. **The playback active at shutdown never got its terminal event.** The loop broke before `finishActive`, so the interrupted playback's `speech.ended` was never published: the session saw a playback that started and then vanished at shutdown.

Constraints: `Stop` must stay bounded (a pathological sink that never finishes a stopped playback cannot wedge shutdown); the scheduler must keep owning playback exclusively (the harness and the runtime do not hold handles); the 200 ms stop target (docs/spec/12) is about the *stop taking effect*, not about the loop joining.

## Decision

The loop joins the active playback's terminal result before it exits. In the `ctx.Done` branch, after `active.handle.Stop(stopFade)`, the loop waits on the playback's `Done` channel with a bounded `stopJoinBudget` (2 s) and, on a result, runs the normal `finishActive` (terminal `speech.ended` published, stop latency recorded). `Scheduler.Stop`'s wait budget grows by the same join budget (2 s loop-exit margin + 2 s join = 4 s worst case); its documented meaning tightens: when `Stop` returns nil, no delivery is in flight and the sink's terminal artifacts are on disk.

A sink that does not finish a stopped playback within the budget is a pathological shutdown: the loop exits without the terminal result (the bounded wait, not the sink, decides shutdown is done) and `Stop` reports via the existing `context.DeadlineExceeded` when the whole budget elapses.

## Options considered

- **The harness polls the sink dir until the count stabilizes.** Rejected: polling is a smell, it fixes only the one consumer, and the missing terminal event (consequence 2) stays in the runtime shutdown path.
- **Expose the active `PlayHandle` so the harness waits on `Done` itself.** Rejected: the scheduler owns playback exclusively (docs/spec/07); leaking the handle breaks that ownership and still leaves the runtime's shutdown with the same gap.
- **Make the filesink's final write synchronous inside `Stop(fade)`.** Rejected: `PlayHandle.Stop` is a fire-and-forget contract (`Done` reports completion); changing the sink contract to hide its completion behind `Stop` would break every other sink's semantics.

## Tradeoffs

- `Stop`'s worst case grows from 2 s to 4 s. It is a shutdown path; healthy sinks finish a stopped playback inside the stop fade (50 ms default) plus one small file write, so the join adds microseconds-to-milliseconds in practice. The budget stays explicit and bounded either way.
- The loop now does one bounded blocking wait in its shutdown path only — never in the steady-state select, so the "sink must never block the session event loop" rule (docs/spec/07) is untouched: `Submit`/`Cancel` paths and the fence ticker are unchanged.

## Verification

- `TestStopJoinsActivePlaybackTerminal` (`internal/speech/stop_join_test.go`): filesink + scheduler, one 300 ms clip, `Stop` mid-playback. Asserts the sink dir holds exactly one WAV and no `.tmp` after `Stop` returns, and that exactly one `speech.ended` was published for the stopped playback. Without the join the test fails 4 of 10 iterations with the same signature as the gn-t034 flake (leftover `.tmp` or 0 WAVs).
- `go test -race -count=1 ./internal/speech/...` (all scheduler/fence/resilience tests, including `TestSchedulerTwentyRapidAdvances` over the dummy sink) and `go test -count=1 -run TestScenarios ./internal/scenario/` ×3 (all seven synthetic scenarios, previously flaky on `gn-t034-decision-delayed`).
- `go test -race -count=1 ./internal/runtime/...` — runtime shutdown (the other `sched.Stop()` caller) passes with the tightened semantics.

## Consequences

- The `audio_evidence_clean` implicit check (ADR-0057) is no longer racing the sink's final write: a scenario run's evidence is complete by the time the check reads it.
- A playback that is active at scheduler shutdown now ends with a published terminal event (interrupted, with stop latency) instead of vanishing.
- `Stop`'s contract is stronger for every caller: "nil ⇒ quiescent" — no in-flight delivery, terminal artifacts written.

## Rollback / migration path

Remove the join select from the loop's `ctx.Done` branch (`internal/speech/run.go`) and restore the 2 s wait budget in `Scheduler.Stop` (`internal/speech/scheduler.go`); delete `stop_join_test.go`. No data migration: the change is lifecycle-only. The gn-t034 evidence flake and the missing-terminal-event gap return.

## Links / references

- `internal/speech/run.go` — the loop's `ctx.Done` join (`stopJoinBudget`).
- `internal/speech/scheduler.go` — `stopJoinBudget`, `Stop` (budget and contract).
- `internal/speech/stop_join_test.go` — the regression test.
- `internal/speech/filesink/sink.go` — the evidence write at completion (the artifact the join protects).
- `internal/scenario/run.go` — `closeAudio` + `audio_evidence_clean` (the consumer that motivated the fix).
- ADR-0057 (the `audio_evidence_clean` check and the scheduler/filesink playback half), ADR-0023 (scheduler ownership).
