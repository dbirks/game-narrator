# ADR-0051: Load-hardened test criteria — event-driven speech terminal test and resolution-aware soak pass criteria

Date: 2026-10-18
Status: accepted
Owners: agent (load-hardening)
Related requirements/tests: docs/spec/12 "Soak" (1h replay + 2h provider/network
chaos, resource budgets from docs/spec/10) and "deterministic delay-injection
suite", docs/spec/13 "did it speak, and why not" (every requested utterance
reaches a terminal event with a reason), ADR-0039 (runtime speech pipeline:
the terminal contract and its bounded stop drain), ADR-0045 (soak sidecar
pacing and the loop-aware per-pass voice-terminal check). Tests:
`TestSpeechSessionEndTerminatesPending` (race), `TestSoakShort` (race,
`make test-unit`), `TestSoakLong` (`make soak`).

## Context and constraints

A full `go test -race` sweep on a machine at load average ~12 (the shape of
GitHub's 2-core runners running the suite) failed exactly two tests, both
green when run alone:

1. `internal/runtime` `TestSpeechSessionEndTerminatesPending`:
   `2 requested utterance(s), only 1 reached their terminal session_ended
   event`. The event log of a failing run shows the shape: line a's
   `speech.ready` lands, then the 2000 ms operator delay (the API's
   documented maximum) **expires before line b's commit lands** on the
   starved machine — the pipeline's full-tilt commit pace stretched past
   2000 ms in wall time. Line a submits to the scheduler and is fenced stale
   by the advance (`speech.cancelled new_commit` + `speech.ended
   failed/stale_generation` — the correct product terminal for a stale
   utterance); only line b ends `failed/session_ended`. The test's asserted
   shape ("the maximum delay outruns the clip, so none starts and every
   requested utterance ends `session_ended`") rests on the wall-clock margin
   between the pipeline's commit pace and the 2000 ms delay, which a loaded
   machine inverts. The same test also raced its `SetAudio` against the
   pipeline's first commit ("commits land ~100 ms later").
2. `internal/soak` `TestSoakShort` (100 s chaos): `fault_recovery: N/77
   faults without recovery: http_5xx@<near run end>: K commits at fault, K
   by +30s`. The verdict measured every fault's 30 s recovery window against
   the run, **clamping the window to the run's end** when it ran past it. A
   fault fired inside the last 30 s therefore had an open window: the
   harness stops the pipeline at run end (the run's timer, not the fault)
   and the commit counter freezes, so "no commit in the remainder" is the
   run's stop, not a missing recovery. Under contention the pipeline's
   commit rate is also slower, so the flat tail is even more likely. The
   100 s window also made the `rss_slope` fit resolution-blind: its
   post-warm-up span (~70 s) is at the measurement's noise floor — a
   one-time Go-arena step or a GC sawtooth of tens of MiB reads as 30+
   MiB/min over it, at or past the 1 GiB/window bound — and its tail still
   carries warm-up under a loaded machine (the passing short run that
   printed 6.3 MiB/min was ratcheting, not leaking).

Constraint: the fix is test/harness-side only — `internal/runtime/
speech_test.go` (plus its test helpers) and `internal/soak/`. The product
behaviour both tests exercise — the fence's stale terminals, the wiring's
bounded stop drain, the run's stop at duration — is unchanged.

## Decision

1. **`TestSpeechSessionEndTerminatesPending` runs on the explicitly-driven
   manual frame source** (`Options.ManualSource`, the seam
   `TestSpeechRapidAdvanceCancelsStale` already uses) with the
   push–wait–push protocol: apply the maximum delay **before pushing any
   frame** (with the manual source no commit can exist before the first
   push, so "delay live before the first requested utterance" is a protocol
   fact); push line a's frames and wait on line a's commit **and** its
   `speech.requested`; push line b's frames and wait on the same; close the
   source (EOF) and assert on the event log.

   The assertion becomes the **terminal contract itself** (docs/spec/13),
   which no wall-clock margin can reorder:
   - both lines are requested (deterministic: manual replay mode processes
     every pushed frame in order);
   - every requested generation carries a terminal event
     (`speech.ended` or `speech.cancelled`) **before `session.stopped`**;
   - at least one utterance ends `failed/session_ended` (the source ends
     while the requested utterances are still pending — in the TTS call or
     the delay — so at least one owes it; on a fast machine both do, on a
     starved one the advance fences line a stale and line b carries it);
   - zero stale audio starts: no generation starts at or after its own
     cancellation, nothing starts after `session.stopped`, the current
     generation (line b) never starts (its delay outlasts the source end by
     construction), and the sink's start count agrees with the log.

   What is deliberately *not* asserted anymore: `started == 0` and
   `reason == "session_ended"` for every requested utterance. Those are the
   wall-clock shape, not the contract: which of the two 2000 ms delays
   expires first (or whether the advance's commit fences line a stale) is a
   fact of machine speed. The contract — every requested utterance
   terminates, with a reason, before the session's terminal event, and no
   stale start — is strictly the same guarantee the original test intended
   to pin, stated on the event stream where it can hold.

2. **Soak `fault_recovery` is resolution-aware.** A fault whose 30 s
   recovery window runs past the run's end has an **open window**: it is
   reported (`faultEvent.Judged=false`, the fault table renders "open
   window") and excluded from the verdict; every fault whose window closed
   inside the run is graded exactly as before — same 30 s bound, same
   sample-grid evidence. The verdict detail names both counts. This changes
   no threshold: it stops grading an observation the run itself made
   impossible (a 30 s window inside the last 30 s of a 100 s run).

3. **Soak `rss_slope` is graded only over resolvable windows.** The Theil–Sen
   fit is always computed and reported (summary line, report), but the
   verdict passes with an explanatory note when the post-warm-up span is
   under `slopeMinWindow` (5 min): over a 70 s window a one-time arena step
   or GC sawtooth is indistinguishable from a leak at the bound rate, and
   the `rss_growth` level bound (20 % / 1 GiB over the same window) still
   grades the run. The documented gates (1h replay, 2h chaos) post
   54/105 min of post-warm-up window and are graded on the slope exactly as
   before.

## Options considered

- **Widen the product's drain bound / make it event-driven**
  (`speechStopDrainBound`): rejected — the drain bound is a documented
  degradation for a wedged provider (ADR-0039), it is outside this task's
  file ownership, and the evidence showed the failing assertion was the
  test's shape assumption, not the drain.
- **Assert the exact terminal reason per utterance with a manual source**:
  rejected — the reason is a wall-clock fact (delay expiry vs advance vs
  source end) that no push–wait–push protocol can pin without also pinning
  machine speed; asserting it would re-create the flake on the reason
  instead of the shape.
- **Skip `fault_recovery` entirely in short mode**: rejected — the 100 s
  run still contains many faults with closed windows; those are graded.
  Only the tail (unclosable windows) is reported-not-graded.
- **Lengthen the short soak to make the tail window a smaller fraction**:
  rejected — doubles the CI cost and still leaves a tail; the window
  openness is a property of any finite run.
- **Grade the slope over the whole run including warm-up**: rejected — the
  warm-up exclusion exists because one-time warm-up allocations are not
  leaks (metrics.go); shortening the fit window further (post-warm-up) was
  the right direction, and the remaining problem is resolution, which the
  minimum window answers.

## Positive consequences

- `TestSoakShort` and `TestSpeechSessionEndTerminatesPending` stop failing
  on their wall-clock margins on 2-core runners; the same assertions hold
  on a loaded machine because they are stated on the event stream / on
  observation windows the run actually closed.
- The soak report is more honest: faults the run outran are "open window",
  not silently "NO"; the slope line stops reading a noise-floor fit as a
  leak signal.
- The terminal contract (docs/spec/13) is now pinned by the test named for
  it, on every requested generation, including the stale path the old form
  never saw.

## Negative consequences / risks

- The short soak's `fault_recovery` verdict covers fewer faults than the
  old form (the tail is ungraded). Mitigation: the tail is reported in the
  fault table and named in the verdict detail; the 1h/2h gates, where the
  tail is a negligible fraction, are unaffected.
- The slope is not graded in the short soak. Mitigation: `rss_growth`
  (level bound over the same post-warm-up window) still grades it, and the
  1h/2h gates grade the slope.
- `TestSpeechSessionEndTerminatesPending` no longer asserts "no playback
  started". Mitigation: it still asserts zero **stale** starts (the
  docs/spec/12 property), that the current generation never starts, and
  sink/log agreement — a regression that plays audio the fence should have
  refused is still caught.

## How to verify (commands and evidence)

- Reproduction (pre-fix), both failures, load ~12:
  `systemd-run --user --scope -q -p CPUQuota=100% -p MemoryMax=4G go test
  -race -count=5 -run TestSpeechSessionEndTerminatesPending ./internal/
  runtime/` → `2 requested utterance(s), only 1 reached their terminal
  session_ended event` (×5); same harness on `TestSoakShort` →
  `fault_recovery: … no commit by +30s` for faults fired inside the last
  ~30 s (×3).
- Post-fix proof: each test `go test -race -count=10` under
  `CPUQuota=100%` with 12 CPU burners running — all pass (see the task
  report).

## Rollback / migration path

Restore the pre-fix `TestSpeechSessionEndTerminatesPending` body
(replay-driven, `session_ended`-only count) and the clamped
`faultRecoveryVerdict` / un-gated `rssSlopeVerdict`; drop `faultEvent.Judged`
and `slopeMinWindow`. No schema or state is involved; report.json gains the
`judged` field on fault events (additive).

## Links / references

- ADR-0039 (runtime speech pipeline, terminal contract, bounded drain)
- ADR-0045 (soak sidecar pacing, per-pass voice-terminal check; the
  11/11 fault-recovery evidence on a 10 min run)
- docs/spec/12 "Soak" and "deterministic delay-injection suite";
  docs/spec/13 "did it speak, and why not"; docs/spec/10 resource budgets
- Commit 62b7d55 (`TestSpeechRapidAdvanceCancelsStale` on the manual
  source)
