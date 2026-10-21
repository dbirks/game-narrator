# ADR-0113: Load-sensitive -race unit tests — capture clock, channel synchronisation and race-scaled wedge guards

Date: 2026-10-20
Status: accepted
Owners: agent worker (issue #22)
Related requirements/tests: GitHub issue #22 (follow-up to #16 and #17), ADR-0064 (bench replay is load-invariant; amendment 2 removed the pipeline's wall-clock belts in a named mode), ADR-0037 (the capture clock is the clock), AGENTS.md rule 3 (no simulation theatre), the race-scale convention of `cmd/game-narrator/race_on_test.go` and `internal/pipeline/race_on_test.go` (since b14e24f)

## Context and constraints

Issue #22: at host load 12–20 (the full race unit gate run in parallel with the
other gates on a shared 8-core host), four -race unit tests fail that pass at
normal load:

- `internal/scenario` `TestScenarios`: the last three scenario subtests
  (`gn-t047-tts-partial-cancel`, `item-obtained-001`, `sign-opened-001`) die
  with `run: context deadline exceeded`.
- `internal/doctor`: `TestDatabaseCheckAppliesMigrationsOnAFreshDir` and
  `TestUnconfiguredRolesAreNotConfigured`.
- `internal/runtime`: `TestCaptureSessionCommitsSyntheticText` and
  `TestReplayPauseMutesSpeech`.
- `internal/pipeline`: `TestDeterministicReplaySlowRankReplyDoesNotFence`.

The issue's expected fix: correctness tests must not depend on wall-clock
budgets. Use the capture clock (as the bench does since #17), channel
synchronisation, or budgets scaled by the race build tag with generous
ceilings.

Measured root causes (the 8-core dev host reproduced `TestCaptureSessionCommitsSyntheticText`
at a measured load of ~12 with the issue's own parallel-api saturators; the
others were analysed from the code paths and the issue's load):

1. **`internal/scenario` — a shared 3-minute test context over wall-paced
   scenarios.** `TestScenarios` runs every scenario under one
   `context.WithTimeout(..., 3*time.Minute)`. Each scenario's wall time is
   floored by `runSidecar`'s wall-clock pacing — one `time.After` per truth
   row, summing to the clip's own duration — plus per-row processing that a
   saturated race host inflates. At the issue's load each scenario took
   ~15–25 s, so ten scenarios outran the shared 3-minute context and the last
   three (alphabetically: exactly the three named) reported the context's
   `deadline exceeded`. The pacing itself is semantic: the TTS interleaving
   the scenario assertions grade (playback start, `dialogue_advanced`
   cancel mid-playback, the evidence WAV written over the utterance's
   duration) needs the wall gap between commits. What is load-sensitive is
   the bound, and the bound is what the fix scales.
2. **`internal/doctor` — a 3 s wall budget around real SQLite work.**
   `newFakeHost` configures the doctor with `Timeout: 3 s`; the
   `storage.database` check runs real migrations (open, migrate, integrity,
   close) under that budget. Both failing tests run the full report through
   `h.run()`, so both carry that check. Under a race host at load 12–20 the
   migration work outlives 3 s, the check's context expires mid-migration,
   and the row reports FAIL (`rep.HasFail()` in the unconfigured-roles test,
   the `StatusOK` assertion in the migration test).
3. **`internal/runtime` `TestCaptureSessionCommitsSyntheticText` — the
   pusher outruns the pipeline, and the run's tail outruns the rank reply.**
   The v4l2 session runs `ModeLive`, whose frame queue is the bounded
   single-slot latest-frame bridge (docs/spec/01): a frame the pipeline has
   not consumed is overwritten by the next one. The test pushes the 24-frame
   clip at a fixed 100 ms pace — the dongle's 10 fps cadence. Under a race
   host at load 12–20 (and, measured on this host, already alongside a
   co-located Dolphin run at load ~3) the pipeline's per-frame wall cost
   (640×480 preprocessing, crop, hash, tracker) exceeds 100 ms, so the
   typewriter-reveal frames are overwritten before the ROI stage consumes
   them; the region never sees a stable full line and the 60 s commit
   ceiling expires (`no line committed within 60s of the stream`).
   Reproduced on this host. A second failure mode shows once every frame
   settles: the clip ends on empty-text frames, and the last line's rank
   reply (the local matcher round trip through the session) lands after the
   last observation that could re-trigger it — fenced stale (measured: 24
   observations in, `ranking_stale=1`, `rankings=0`, `commits=0`) — and a
   real dongle would have kept delivering the screen the line is still on
   while that reply was in flight; the test's clipped source does not.
4. **`internal/runtime` `TestReplayPauseMutesSpeech` — patience ceilings
   sized for a quiet race host.** The replay session is lossless (back
   pressure, no drops), so the run is correct under any load — it is just
   slower: every frame pays the 250 ms scripted OCR delay plus the race
   host's per-frame overhead, and the two 3 s tones play in real time. The
   test's own ceilings (60 s first speech, 30 s pause fence, 30 s resume,
   180 s terminal) are the only wall-clock element; at the issue's load the
   healthy-but-slow run can outrun them.
5. **`internal/pipeline` `TestDeterministicReplaySlowRankReplyDoesNotFence` —
   two test-owned wall constants around the plain arm's tail.** The plain
   ModeReplay counterfactual drives the product's wall belts against a 300 ms
   wall reply: the 60 ms retrieval-hold budget (`OCRTimeout`) and the run
   tail's end-of-stream grace, `OCRTimeout + 500 ms = 560 ms` of wall time.
   The reply is owed by a timer, so under load the distance between "the last
   frame drained" and "the timer fired plus the reply scheduled, dispatched
   and applied" can stretch past the 560 ms grace: the run then ends through
   the grace with the tail reply unapplied, a different observable state than
   the reply-applied run the assertion was written against. The assertion
   itself tolerates both states (it ORs the fence count and the commit
   delta), but the test's two wall constants — the 300 ms `slowDecider` delay
   and the 60 ms `OCRTimeout` — set the ratio the dependence is measured in,
   and that ratio, not the assertion, is what a race host at load 12–20
   stretches. The deterministic arm is structurally load-invariant (its hold
   has no wall budget — ADR-0064 amendment 2).

Constraints: the pipeline's own deadlines (docs/spec/01) and the bench's
load-invariance (ADR-0064) are unchanged; the scenario harness keeps running
the real provider clients over real loopback HTTP (AGENTS.md rule 3);
non-race builds keep their existing budgets, so a genuinely wedged probe or
provider still fails fast where it used to.

## Decision

Following the issue's three sanctioned levers, each named test gets the one
that matches its mechanism. All scaling uses the repo's existing race-scale
convention (b14e24f): a `*_test.go` constant pair, 5 under `//go:build race`,
1 otherwise, multiplied at the budget's use site.

1. **`internal/scenario`: keep the dialogue pacing; race-scale the suite's
   bound and the provider bound.** The suite's shared context becomes
   `3*time.Minute*budgetScale` and `providerTimeout` (10 s, the harness's
   per-provider-call and drain bound) becomes a package var the race build
   sets to `10*time.Second*budgetScale` (50 s under race), via new
   `race_on_test.go`/`race_off_test.go` (`budgetScale` 5 / 1) — the same
   pattern the real-Tesseract gates use to scale their engine budgets.
   `runSidecar`'s wall pacing is kept: a first attempt removed it, and four
   TTS scenarios (`black-water-001`, `gn-t032-rapid-advance`,
   `gn-t046-voice-missing`, `gn-t047-tts-partial-cancel`) failed — the
   playback interleaving the assertions grade (an utterance starting while
   the previous line is still playing, a `dialogue_advanced` cancel
   mid-playback, the evidence WAV written over the utterance's duration)
   needs the wall gap between commits a real session has while the previous
   line is spoken. The pacing is a simulation of the dialogue's cadence,
   not a deadline: what killed the test at the issue's load was the suite's
   shared 3-minute bound outrun by the paced runs (the issue's "per-scenario
   deadline about 15-25 s" was the measured per-scenario wall time, not a
   per-scenario budget), and the race-scaled 15-minute bound is a generous
   ceiling for a suite whose measured cost is ~10×(clip length +
   load-inflated processing) ≈ 5-8 minutes even at the issue's load.
2. **`internal/doctor`: race-scaled generous ceilings.** New
   `race_on_test.go`/`race_off_test.go` define `checkTimeoutScale` (5 / 1),
   and `newFakeHost`'s budgets become `Timeout: 3 s × scale` (15 s) and
   `ModelTimeout: 5 s × scale` (25 s) under race. The uninstrumented values
   keep today's behaviour (a check that ignores its 3 s deadline still
   reports FAIL); the race ceilings stay small against the production
   defaults (15 s/60 s, `doctor.go`) so a genuinely wedged check still fails
   the test — they buy the headroom a load-inflated SQLite migration needs,
   nothing more.
3. **`internal/runtime` capture test: channel synchronisation + the
   dongle's own held-screen tail.** After pushing frame `n`, the test waits
   (polling the session snapshot's live `pipeline.Stats`, which the manager
   already refreshes per snapshot) until the frame has settled —
   `Observations` (one per processed frame per region, plus the hash-reuse
   observations that skip a read) reaching `n`, with a quiet-count fallback
   for frames whose box is absent and emits nothing — before pushing `n+1`:
   the pusher can never outrun the pipeline, so the latest-slot bridge has
   nothing to overwrite and the next observation cannot outrun the settled
   frame's rank reply, at any host load. The clip's own tail is empty-text
   frames, which would re-trigger nothing after a fenced reply, so the test
   then holds the clip's last text frame (the screen the dongle is still
   delivering — hash-reuse observations, no new reads) at a sub-10-fps
   cadence — a dongle under load delivers late, never early — until the
   commit lands. The per-frame settled wait, the 60 s commit ceiling and the
   30 s terminal ceiling are wedge guards, each race-scaled by
   `patienceScale` (5 / 1): 150 s / 300 s / 150 s under race.
4. **`internal/runtime` pause test: race-scaled patience ceilings.** The
   event-poll structure (channel synchronisation) is unchanged; the four
   ceilings (60 s first speech, 30 s pause fence, 30 s resume, 180 s
   terminal) are multiplied by the package's `patienceScale` (5 / 1), so the
   instrumented build bounds a healthy run at 300/150/150/900 s — a wedge
   guard a realistic load cannot trip, and a genuinely dead run still fails
   inside the gate's package timeout.
5. **`internal/pipeline` slow-rank-reply test: scale the test's own wall
   constants, keep the ratio, keep the assertion.** The test's two wall
   constants — the 300 ms `slowDecider` delay and the 60 ms `OCRTimeout`
   (retrieval-hold budget and, through it, the 560 ms end-of-stream grace) —
   scale by the package's existing `ocrTimeoutScale` (5 / 1), as does the
   5 s `ReplayOCRTimeout` per-call deadline. The ratio the dependence is
   measured in — the reply 5× slower than the hold budget — survives the
   race build unchanged (1500 ms reply over a 300 ms hold budget, an 800 ms
   grace). Under race the plain arm's tail reply now deterministically
   outlives the tail grace, so the counterfactual lands in the same
   state run to run there (the commit delta carries the dependence) instead
   of a scheduling coin flip between the fence state and the commit-loss
   state; the assertion (OR of `st.RankingStale > 0` and the commit-timestamp
   delta) covers both and is unchanged. The deterministic arm and the
   no-drops assertion are structurally load-invariant and unchanged.

## Options considered

- **Scale every wall budget more aggressively and keep the mechanisms.**
  Rejected for the capture test: the fixed 100 ms push is the load
  sensitivity itself — scaling the outer ceiling leaves a timer that a
  slow-enough host trips, the exact ADR-0064 "rejected" class (the pusher
  is channel-synchronised instead; the ceilings scale). For the doctor and
  pause tests the budgets are the whole load sensitivity (the work they
  bound is already load-independent once it finishes), so scaling them is
  the right fix.
- **Remove `runSidecar`'s wall pacing (machine-speed truth feed).**
  Rejected — measured, not reasoned: with the pacing removed, four TTS
  scenarios failed on an unsaturated host (`speech.cancelled
  dialogue_advanced` 0× want 2; no terminal `speech.ended` result; 0 evidence
  WAVs for a started utterance). The playback interleaving is graded by the
  scenarios, and it needs the wall gap between commits. (The assertions grade
  the capture clock and the fences are generation-based, but the
  start/cancel/WAV events the speech assertions count are produced by the
  real-time scheduler over that gap.)
- **Have the capture test push through a replay-mode session instead of
  ModeLive.** Rejected: the test exists to run the v4l2 (live) session path
  — device identity, mode provenance, the live latest-slot queue — and
  swapping the queue policy would test a different product.

## Validation

- **Before (protocol: 4 parallel `go test -race ./internal/api/` saturators,
  ADR-0064's load-16 method, peak load measured ~12–19 on this 8-core host,
  plus the four packages at `-race -count=3`):**
  `TestCaptureSessionCommitsSyntheticText` failed both protocol runs with
  `no line committed within 60s of the stream` (69.6 s and 74.7 s runs), and
  failed the same way on an unsaturated run alongside a co-located Dolphin
  emulator (load ~3, 69.1 s and 70.9 s) — the load sensitivity is the
  mechanism, not the absolute load. `TestReplayPauseMutesSpeech`, the two
  doctor tests, the pipeline test and `TestScenarios` passed under this
  host's saturation — their failure mode (issue's host: parallel gates and
  workers at load 12–20) was analysed from the code paths above and is the
  mechanism each change removes.
- **After:** the same command matrix passes: the capture test commits in
  ~13 s under the co-located-Dolphin condition that took it 69 s before, and
  the four packages pass at `-race -count=3` with the four api saturators
  running in parallel (the capture test no longer reproduces under the
  saturation that reproduced it before).
- **Regressions:** the uninstrumented budgets are unchanged (5× scaling is
  `//go:build race`-gated), so a wedged probe or provider still fails fast in
  non-race builds; `go vet` and `gofmt` clean on the touched packages; the
  scenario suite's report lines (PASS/BLOCKED) are unchanged in content, only
  the wall time shrinks.

## Consequences and reversal

- The four named tests no longer depend on wall-clock budgets for their
  correctness: the capture test's pusher is channel-synchronised to the
  pipeline's consumption, and the remaining ceilings (the scenario suite's
  shared context, the harness's provider bound, the doctor's fake-host
  budgets, the runtime patience ceilings, the pipeline test's wall
  constants) are race-scaled wedge guards inside the gate's package
  timeout.
- The scenario suite's wall time is unchanged on a quiet host (the pacing
  is kept); only its bound moved (3 min → 15 min under race), so a
  genuinely wedged scenario still fails the suite inside the gate's
  package timeout.
- Reversal: restore the 3-minute shared context in
  `internal/scenario/scenarios_test.go` (and delete the new race files and
  the `providerTimeout` var in `run.go`, restoring the const); restore
  `Timeout: 3 s` / `ModelTimeout: 5 s` literals in `internal/doctor` and
  delete its race files; restore the fixed 100 ms push loop and the
  unscaled ceilings in `internal/runtime` `capture_test.go` and
  `controls_test.go` (and delete `patienceScale` from the runtime race
  files); restore the unscaled `OCRTimeout` / `ReplayOCRTimeout` /
  `slowDecider` constants in `internal/pipeline` `deterministic_test.go`.
  Nothing else reverts.

## Amendment 1 (2026-10-21): the real-Tesseract replay session's engine budget is race-scaled, and its terminal ceiling joins the package's

**Trigger.** CI red on `5fcf61e` (run 39542298642, 2026-10-21 04:07–04:22, the 2-core
GitHub runner, `go test -race`, tesseract 5.3): `TestSyntheticReplaySessionRealTesseract`
(`internal/runtime`) fails `commits = 1, want 2` with OCRCalls 13, OCRErrors 4,
ReplayDegradedFrames 4 (frame seqs 4 9 13 19), FrameToOCR P50 5 s / mean 6.8 s /
max 12.3 s. The test passes locally under `-race` on an unsaturated host. This is
issue #22's load-sensitivity class in a test #22's fix did not cover (filed as
issue #38 by the 2026-10-21 acceptance audit).

**Root cause.** In `ModeReplay` the pipeline's per-call budget is
`ReplayOCRTimeout` (30 s) and the replay retry policy (`ReplayOCRMaxAttempts` = 3,
`internal/pipeline`) gives every attempt a fresh 30 s deadline — but the
engine's own per-recognition deadline (`ocr.Tesseract.Timeout`, default
`ocr.DefaultTimeout` = 4 s) is the tighter bound, and it fires inside the
attempt: `Tesseract.Recognize` wraps the caller's context in
`context.WithTimeoutCause(ctx, t.deadline(), ErrTimeout)`. The session test's
`newEnv` injected the production default engine `ocr.Tesseract{}` (0 → 4 s), so
the pipeline's 30 s per-attempt budget could never outrun the engine's 4 s
kill. On the race-instrumented runner one crop takes 5–12.3 s: every attempt of
the retry policy times out at 4 s, the frame degrades
(`Stats.ReplayDegradedFrames`), and one line never commits. The degraded-frame
path itself is correct (bounded retries, then a named degrade — the ADR-0116
class of policy, the pipeline's own replay version); what was wrong was the
test's injected engine carrying an unscaled budget into a race build.

**Decision.** The ADR-0113 convention, applied to the two load-sensitive
bounds of this test; production defaults unchanged:

1. `newEnv`'s default engine becomes
   `ocr.Tesseract{Timeout: ocr.DefaultTimeout * ocrTimeoutScale}` — 20 s per
   recognition under `//go:build race`, 4 s in the uninstrumented build (the
   production default, tuned for the board). The package's existing
   `liveOCRTimeoutScale` constant is renamed `ocrTimeoutScale` (5 / 1) because
   it now covers the session's default engine and the live OCR fallback
   pre-check, not just the pre-check. The engine's scaled budget stays tighter
   than the pipeline's 30 s replay budget, so the retry policy's contract
   (fresh per-attempt deadline, bounded attempts, degrade after the budget)
   is unchanged — a single slow crop now fits inside one attempt instead of
   burning all three.
2. The test's 180 s terminal patience ceiling (the `waitState` bound) becomes
   `180 * time.Second * patienceScale`, the package's existing race-scaled
   wedge-guard convention (900 s under race) — the same shape as
   `controls_test.go`'s 180 s ceiling, which this test had overlooked.

**Reproduction and validation (this 8-core host).** Protocol: the compiled
`-race` test binary (`go test -race -c ./internal/runtime`), 6 busy-loop CPU
burners, `GOMAXPROCS=2` (the 2-core runner), the single test per run
(evidence: `artifacts/ci-ocr-timeout-race-2026-10-21/`).

- **Old code, N=6:** reproduces CI exactly — `commits = 1, want 2`,
  OCRErrors 15 (every timed-out attempt booked).
- **New code, N=6:** 10 consecutive PASS, 12–74 s wall each. Every run:
  `Commits: 2`, `OCRErrors: 0`, `ReplayDegradedFrames: 0`, `OCRCancelled: 0`,
  `FramesDropped: 0`. FrameToOCR max across the 10 runs: 0.79–14.76 s — in
  four of ten runs the slowest single read (14.76, 12.56, 12.38, 5.12 s)
  outlived the old 4 s engine deadline and is exactly what the unscaled
  budget converted into a degrade; the scaled 20 s budget carries it.
- `go test -race ./internal/runtime` and `make lint` pass on the final tree.

The assertions (2 commits, 24 delivered frames, the dev-trace evidence chain)
are unchanged; nothing in the pipeline, the retry policy, or the production
budgets moved. **Reversal:** restore `ocr: ocr.Tesseract{}` in `newEnv`
(`helpers_test.go`), the `liveOCRTimeoutScale` name in the runtime race files,
and the unscaled 180 s `waitState` bound in `manager_test.go`.
