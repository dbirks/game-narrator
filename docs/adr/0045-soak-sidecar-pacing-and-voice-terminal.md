# ADR-0045: Soak — sidecar pacing on the clip's own span, and the loop-aware per-occurrence voice-terminal check

Date: 2026-10-17
Status: accepted
Owners: agent (m7-soak-finish)
Related requirements/tests: docs/spec/12 "Soak" (1h replay + 2h simulated
provider/network chaos, no unhandled panics/OOM, resource budgets from
docs/spec/10) and "Zero stale audio starts", docs/spec/06 "on provider error
the runtime proceeds without Clef" (abstain, not guess); ADR-0037 (two-layer
soak composition over the scenario harness), ADR-0039 (runtime speech
pipeline: every started utterance reaches a terminal — `speech.ended`
result `ended`/`failed` with reason, or `speech.cancelled` with reason).
Tests: `TestVoiceContractOffenders`, `TestSidecarPassPacingIsFlat`,
`TestSoakShort` (race), `TestSoakLong` (budgets via `GAME_NARRATOR_SOAK_*`,
`make soak`).

## Context and constraints

The 10-minute chaos soak `20261017-222119-chaos` (seed 7) failed exactly one
criterion — `pass_assertions`: `clip 0 pass 3 no_voice_substitution: no
terminal speech outcome for voice "synthetic-narrator" (want result ended, or
failed naming voice_missing)`. Every other criterion held: no panics, RSS
+18.3% (slope 0.49 MiB/min, below the 1 GiB/window bound), goroutines/FDs
back to baseline, 11/11 faults followed by a commit within 30 s.

Reproducing the first ~95 s of that run (same seed) and inspecting the
sidecar's per-pass slices showed the failing pass slice:

- `message.committed` for `demo-page-1` (generation 7) with **no voice
  events at all** — the sidecar's async decision lost the race to the next
  advance and was discarded by the session's generation fence. An abstain is
  the docs/spec/06 behaviour: no decision, no voice, no event owed.
- `message.committed` for `demo-page-2` (generation 8), then `speech.ready`,
  `speech.started`, then `speech.cancelled reason=region_vanished` — the
  clip's own box-dismissal rows (dialogue-advance-001 rows 22–23 carry
  `box_present: false`) dismissed the region before the utterance's 100 ms
  tone-end fired. A cancelled phase cannot be ended by the session's phase
  table; the cancellation is the "zero stale audio" property working.

Both facts are **correct product behaviour**: the session and the fences
satisfy the ADR-0039 terminal contract — every started utterance reached a
terminal (`speech.ended` ended/failed-with-reason or `speech.cancelled`
with-reason). The failure was in the harness's evaluation:

1. **Narrow pass-slice evaluation.** The soak reuses the scenario's
   `no_voice_substitution` evaluator per pass slice. That evaluator is sound
   for a single-clip run (the last line's audio completes last; GN-T046
   pins it there), but on a *looped* pass it is unsound: it requires a good
   `speech.ended` in the slice, which a looped pass does not owe — the
   clip's own geometry cancels the last line every pass, and a
   fault-delayed decision can commit a line with no voice at all. A pass
   where page-1 abstained and page-2 was cancelled by the dismissal is
   exactly the failing shape, and it is a healthy pass.
2. **Sidecar pacing grew per pass.** `sidecarPass` paced row *i* of pass *p*
   at `anchor + (t.TsNS + p·stride − first)` with the anchor reset per pass,
   so the `p·stride` term was dead sleep: pass *p* took clip-span + p·stride
   wall time (2.3 s + 2.4 p s). The 10-minute run therefore did **21 passes
   / 42 sidecar commits** instead of the intended ~260 passes / ~520
   commits — the ADR-0037 "one commit per line per pass" load was not the
   load it claimed to be, and each pass slice was a growing wall-time
   window (pass 3 ≈ 9.5 s) into which the seeded chaos faults bunched.

## Decision

1. **Pace on the in-clip offset only** (the scenario's `runSidecar` shape):
   the loop offset stays on the *capture* timestamp (the monotonic source
   clock the session fences and the pipeline tracker both see) and never
   enters the wall target. Every pass again takes the clip's own ~2.3 s span.
2. **Loop-aware per-occurrence voice-terminal check** for the pass slices
   (the soak-local `staleSpeechOffenders` pattern, already the precedent for
   the looped `no_stale_speech` check). A slice carrying speech events must
   (a) contain no speech event naming a voice other than the configured one
   (the substitution check itself, unchanged) and (b) terminate **every
   occurrence that started in the slice** within the slice —
   `speech.ended` (any result: `ended`, or `failed` carrying the voice and
   the reason in `error`) or `speech.cancelled` (carrying the fence's
   reason). This is strictly stronger than "some good end exists" in the
   directions that matter (an utterance that genuinely never terminates
   still fails the pass) and gives up only the requirement that was unsound.
   The scenario's shared evaluator is unchanged.
3. **No product change.** The session's occurrence/phase fences and the
   sidecar's `endOnce`/`finishUtterances` drain already deliver the terminal
   contract under the observed fault combinations (TTS connection reset,
   latency spike, slow body, voice missing on replica, decision delay); the
   probe showed every started utterance ending in `ended`/`failed`/
   `cancelled-with-reason`. Fixing the product instead of the harness would
   have meant inventing a terminal the fences already emit.
4. **`make soak` target**: 1 h replay then 2 h chaos (`TestSoakLong`
   budgets via `GAME_NARRATOR_SOAK_DURATION` / `GAME_NARRATOR_SOAK_CHAOS`),
   each with an explicit `-timeout` (1h15m / 2h15m) that outlives the run
   plus the bounded stop phase. Hours-long by design, so it is not part of
   `test-full`; the race-enabled coverage of the same composition is
   `TestSoakShort` in `test-unit`.

## RSS growth (docs/spec/10)

| Run (chaos, seed 7) | Growth (10 % mark → end) | Theil–Sen slope | Bound | Post-warm-up range |
| --- | --- | --- | --- | --- |
| 10 min (`20261017-225301-chaos`, post-fix) | +9.7 % (55952 → 61372 KiB) | 0.28 MiB/min | 113.8 MiB/min (1 GiB / 10 min) | 57–61 MiB |
| 30 min (`20261017-230630-chaos`) | +6.5 % | 0.20 MiB/min | 34.1 MiB/min (1 GiB / 30 min) | 58.0–65.6 MiB (median 62.6) |

The pre-fix 10-minute run (`20261017-222119-chaos`) measured +18.3 % / 0.49
MiB/min, but at the distorted load (21 passes instead of 261). With the
intended load the slope **falls** as the window lengthens (0.49 → 0.28 →
0.20 MiB/min) and the relative growth shrinks (18.3 % → 9.7 % → 6.5 %):
growth is the one-time Go arena step in the first ~90 s plus a ±4 MiB GC
oscillation around a flat level for the remaining 28 minutes — it
**plateaus**, it does not leak.

## Tradeoffs

- The per-pass voice contract no longer travels through the scenario
  vocabulary. Consequence: if the scenario evaluator's terminal semantics
  change, the soak-local copy must follow. `TestVoiceContractOffenders`
  pins the relationship (the old evaluator still fails the pass-3 shape, the
  new check passes it).
- Flat pacing raises the sidecar's commit rate ~12× (42 → ~520 commits per
  10 minutes), i.e. the chaos RNG is consumed ~12× faster: the fault mix of
  a seeded run is not comparable across this change. The seed and the fault
  vocabulary are unchanged; only the request count per wall time is what the
  docs/spec/12 gate actually names.
- A pass whose every utterance was cancelled (the clip's box dismissal) now
  passes the voice contract. That is the intended semantics — cancellation
  is the product's terminal for an interrupted utterance — and the
  cancelled events remain on the merged timeline, so the report still shows
  what happened and why.

## Validation

- `TestVoiceContractOffenders`: the pass-3 shape (abstain +
  cancelled-by-dismissal) is clean; the old `no_voice_substitution`
  evaluator fails the same slice (the bug, pinned); a stuck utterance
  (started, no terminal) still fails; a foreign voice still fails; a
  `failed`/`unavailable` end and a normal `ended` are clean.
- `TestSidecarPassPacingIsFlat`: ≥3 pass slices with commits (≥6 sidecar
  commits) in 12 s of wall time; the pre-fix pacing completes 2.
- `go test -race ./internal/soak/` (including `TestSoakShort`, ~100 s
  chaos): PASS — summary line:
  `soak: PASS run=20261017-230438-chaos mode=chaos wall=1m40s rss_growth=+14.1% slope=14.34MiB/min goroutines 4->2 fds 8->6 commits=70 faults=77 provider_errors=76 panics=0 passes_evaluated=43`
- `go test -race ./internal/speech/... ./internal/scenario/...` (unchanged,
  regression check): PASS.
- 10-minute chaos run (`GAME_NARRATOR_SOAK_DURATION=10m
  GAME_NARRATOR_SOAK_CHAOS=1 go test -timeout 30m -run TestSoakLong`): PASS —
  summary line:
  `soak: PASS run=20261017-225301-chaos mode=chaos wall=10m0s rss_growth=+9.7% slope=0.28MiB/min goroutines 4->2 fds 8->6 commits=5468 faults=486 provider_errors=480 panics=0 passes_evaluated=261`. All seven verdicts held: 261 pass slices clean under the voice contract, 486 faults (incl. 442 5xx, 29 TTS-endpoint-down, 5 voice-missing, 4 conn-reset) each followed by a commit within 30 s.
- 30-minute chaos run (RSS plateau check): PASS — summary line:
  `soak: PASS run=20261017-230630-chaos mode=chaos wall=30m0s rss_growth=+6.5% slope=0.20MiB/min goroutines 4->2 fds 8->6 commits=18241 faults=1529 provider_errors=1523 panics=0 passes_evaluated=782`.

## Consequences and reversal

- The soak's load is now the load it claims (one commit per line per 2.3 s
  pass); pass slices are again ~2.3 s of wall time; the seeded fault
  schedule of a given run is not comparable to pre-change runs.
- Reversal: both fixes are single-point — restore the `pass*stride` term in
  the pacing target and put the scenario `no_voice_substitution` assertion
  back into `perPassAssertions` — and the two unit tests above are the
  revert guards. The ADR-0039 terminal contract in the runtime is untouched.

## Amendment (2026-10-17, m7-leak): the 1-hour replay RSS growth was a harness leak — per-pass slices, now settled and dropped

**The leak.** The orchestrator's 1-hour replay soak on a clean HEAD
(`make soak`, run `20261017-235844-replay`) grew RSS monotonically:
+30.0 % / 0.32 MiB/min over the 54-minute post-warm-up window
(61428 → 79880 KiB; 35391 commits ≈ 0.5 KiB per committed line), while
goroutines (4→2) and FDs (8→6) sat flat — heap retention, not
goroutine or FD growth. The "plateau" reading in the RSS section above
was an artifact of window length: at 30 minutes the leak had only
accumulated ~7 MiB, still inside the ±4 MiB GC band this ADR measured.

**The cause (heap-profiled, not guessed).** An opt-in heap capture added
for this amendment (`GAME_NARRATOR_SOAK_HEAPPROFILE=dir`: `runtime.GC()`
then `pprof.WriteHeapProfile` at the 10 % warm-up mark and at teardown;
the leak diff is `go tool pprof -top -inuse_space -diff_base early
late`) ran on a 15-minute replay soak before the fix (run
`20261018-011115-replay`, +11.8 % / 0.49 MiB/min). The diff named the
harness's own sidecar timeline as the growth site:
`(*sidecarPub).addPassEvent` +1.5 MB flat over the 13.5-minute window,
with the envelope copies it retains (`events.Envelope.withDefaults`
retention via `Hub.Publish`, `games.newID` occurrence ids held only by
those slices). The harness kept every pass's event slice —
`passEvents map[passStamp][]events.Envelope` — until end of run, so
retention grew one slice per pass (~44 events per pass, ≈ 4 KiB unique
per pass: slice and Envelope headers; the Data bytes share backing
arrays with the ring copies) for the whole run: ≈ 1500 passes per hour.
That is the 0.32 MiB/min. Every other suspect was profiled or audited
and is already bounded: the hub ring (4096, docs/spec/01), the soak's
`ringStore` (8192, a fixed-capacity ring), the session reducer
(`committedKeys` ≤ `maxCommittedIdentities` with oldest-eviction,
`rankings` ≤ 8 per region, `Recent` ≤ `RecentMessages`, stale notices ≤
`maxStaleNotices`), the matcher index (static), and the replay-source
frames (transient — the profile shows them freed at teardown, −9.3 MB).
This is a harness-only leak: the runtime, session and hub behave as
designed.

**The bound (harness-only fix, no product change).** `internal/soak/`
(`passstate.go`, `settle.go`, `speak.go`): a pass slice is *settled*
when the pass stops being fed, the settle window
(`2×providerTimeout + toneEndDelay` = 20.1 s) has closed since its last
commit, and every utterance started in it has a terminal event. A
settled slice is graded immediately (the same per-pass assertions as
before) and dropped — the retained pending set is bounded at
`passBound = max(9, ceil(settleHorizon/clipSpan)+2)` (≈ 9 for the 2.3 s
clip), so retained state is the unsettled horizon, not the run's pass
count. When the pending set exceeds the bound the oldest unsettled
closed slice is force-graded (a stuck utterance fails its voice
contract — the bound never hides a verdict). `occPass` routing entries
are released with their slice, and the `spoken` double-speak map is
bounded at `spokenCap = 4096` (dead-entry sweep; the clear fallback is
safe because the session's occurrence fence is the backstop).
`TestPassTimelineBounded` (100 passes ≫ bound; the bound holds at every
step; every pass graded; residue drains to zero) and `TestSpokenBounded`
(4×spokenCap live occurrences; size never above spokenCap) pin the
bounds.

**Validation.**

- 15-minute replay with heap capture, pre-fix
  (`20261018-011115-replay`): +11.8 % / 0.49 MiB/min; diff top
  (inuse_space, early→late):
  `+4096 kB events.Envelope.withDefaults` (retention through
  `Hub.Publish`), `+1538 kB (*sidecarPub).addPassEvent` (the growth
  site), `+512 kB games.newID`, the rest ±512 kB GC noise and the
  teardown frame release (`image.NewRGBA` −9292 kB).
- 15-minute replay with heap capture, post-fix
  (`20261018-025946-replay`): **PASS** (+7.5 % / 0.13 MiB/min); diff
  top no longer names a soak growth site — `addPassEvent` is gone and
  `games.newID` flipped to −1024 kB (occurrence ids released with their
  slices). What remains is the one-time, saturating fill of the bounded
  ring (4096) + ringStore (8192): ≈ +2 MB of `withDefaults` envelope
  retention between the 1.5-minute early capture (rings not yet full)
  and the late capture (rings full), flat once the rings are full — a
  documented, bounded step, not a leak.
- 30-minute replay soak, post-fix (run `20261018-025948-replay`):
  **PASS** — `soak: PASS run=20261018-025948-replay mode=replay
  wall=30m0s rss_growth=+7.8% slope=0.08MiB/min goroutines 4->2
  fds 8->6 commits=17124 faults=0 provider_errors=0 panics=0
  passes_evaluated=782`. The growth level is the same one as the
  15-minute run's (+7.5 % over 13.5 min → +7.8 % over 27 min): the
  ring-fill step plus GC noise, reached once and held — the signature
  of a bounded fill, where a linear leak would have doubled the level
  and held its slope.
- `go test -race -count=1 ./internal/soak/` (TestSoakShort + the two
  new bound tests + the pacing contract): PASS.

Consequence for the orchestrator re-run: the 1-hour `make soak`
rss_growth verdict is expected to land at the ring-fill step plus GC
noise (single-digit percent), not the previous +30 %.
