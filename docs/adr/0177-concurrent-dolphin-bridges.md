# ADR-0177 — Concurrent Dolphin bridges

* Status: accepted
* Date: 2026-10-23
* Related: ADR-0007 (Dolphin bridge), ADR-0043 (readiness), ADR-0143 (A/V harness), ADR-0094 (E2E port base)

## Context

Every real-Dolphin test enforced a host-wide "no-parallel rule": it refused to start, or failed its leftover check, if any `dolphin-emu` or `Xvfb` process existed anywhere on the host. The stated reason dates from the bridge spike: "two emulators on one machine fight over the render-window lookup".

Since then, each bridge got its own private resources:
- an Xvfb on a free display (`freeDisplay`);
- an isolated `--user` directory (`isolateUserDir`);
- a window lookup scoped to that display;
- the A/V harness's PipeWire null sink is named per run (`gn_av_<run id>`);
- the E2E servers pick their ports automatically (ADR-0094).

The rule now only serializes work. The host has 8 cores, and ten agents queued behind one emulator.

## Options

1. Keep the rule and serialize every emulator run. This is simple, but slow.
2. Drop the rule entirely. Latency-gated runs (`make test-av`) would then measure CPU contention from other emulators, not the narrator.
3. Make exclusivity opt-in (`GN_DOLPHIN_EXCLUSIVE=1`), and scope the leftover assertions to the run's own display.

## Decision

Option 3.
- `strayDolphinProcesses` (internal/dolphin) and `strayDolphinProcs` (internal/e2e) check only when `GN_DOLPHIN_EXCLUSIVE=1`.
- The abrupt-kill test asserts that no process references *its own* display after the cancel (`countProcEntries(display)`), instead of "no Dolphin/Xvfb anywhere".
- Runs whose numbers are gates (the final sweep's `test-av` and `test-dolphin`) run exclusive. Development runs share the host, at most 2 at a time (the host-level semaphore lives in the operator's tooling, not the repo).

## Validation

Measured on the dev host (i7-7700: 4 cores, 8 threads, llvmpipe rendering):
- **3 concurrent** `TestRealDolphinBridge` + `TestRealAbruptContextKill` processes: each ran on its own display (:94/:95/:96), and all 3 abrupt-kill tests passed their display-scoped leftover check. All 3 bridge tests FAILED the input proof, though: "no usable title visit in 3 episodes". Three software-rendered emulators slow each one past the proof's timing windows.
- **2 concurrent** `TestRealDolphinBridge`: both PASS (100 s each, against about 80 s alone). Load average peaked at 7.9.
- So the host semaphore is 2 slots on this machine, and the timing-sensitive gates stay exclusive.

## Consequences and reversal

- A latency number measured without `GN_DOLPHIN_EXCLUSIVE=1` may include contention, so gate numbers are only taken from exclusive runs.
- Reversal: set `GN_DOLPHIN_EXCLUSIVE=1` everywhere (the old behaviour), or restore the unconditional check.
