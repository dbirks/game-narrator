# ADR-0124: The windwaker-newgame bench holdout — grow the labelled samples, do not weaken the commit gate

Date: 2026-10-20
Status: accepted
Owners: dolphin-gate worker (task dolphin-gate)
Related requirements/tests: `make bench` (the captured holdout, class (b)
captured-frame replay, docs/spec/12); `TestRealRecordFixture`
(internal/dolphin, `GN_GATE=test-dolphin`); the windwaker-newgame and
windwaker-legend-dense fixtures under `$GAME_NARRATOR_FIXTURE_DIR`;
internal/recognition (the temporal-vote commit gate, docs/spec/05);
record_dense.go (the dense legend recording, ADR-0058 amendment 1);
ADR-0037 (blocked ≠ pass; fixture provenance)

## Context and constraints

The 2026-10-20 bench (fixture root `$GAME_NARRATOR_FIXTURE_DIR`, main
551723c) reported:

- **windwaker-legend-dense: recognition_recall 95.0% (n=20) — met.**
  The single missed sample, found by replaying the holdout through the
  real pipeline with per-sample dumps (internal/bench, the run's
  evidence): `cx0021`, the recording's LAST line — exactly ONE frame at
  the tail of the capture (first text 17:55:47, the recording's final
  second). The bench's own OCR read that frame as EMPTY (a mid-fade /
  mid-transition frame the recording cut), so the pipeline had no stable
  text to commit and committed nothing. The other 19 lines (25-110
  frames each at the ≥10 Hz capture) all committed with top-1 exact
  (top1_exact 100.0%, n=19). The miss is a recording-boundary truncation —
  the line's display was cut off by the recording's end (the legend
  watch's measured gameplay-screen boundary), not a recognition failure:
  the pipeline correctly abstained on one frame of empty OCR. The metric
  still meets its ≥95% bar.
- **windwaker-newgame: recognition_recall 50.0% (n=2) — not_met.** The
  fixture (re-recorded 2026-10-20 13:58 EDT by `TestRealRecordFixture`,
  the drive's ~2.9 s grid, 52 frames) holds two legend lines: line 1 in
  five frames (commits, top-1 exact) and line 2 in ONE frame — the
  recording's truncated tail. The recognition tracker cannot commit a
  line seen in one frame: ABSENT→APPEARING→REVEALING lands on the first
  text frame, REVEALING→STABILIZING requires the text to have held for
  the StableWindow (100 ms on the bench's 10 Hz clock, i.e. the NEXT
  frame), and STABILIZING→READY costs one more frame (the deliberately
  un-chained step, internal/recognition/tracker.go). One frame can never
  reach READY. record_dense.go's header already documents this: "the
  original windwaker-newgame fixture samples on the drive's ~2.1 s grid,
  so a legend line can appear in a single frame and the recognition
  tracker (which needs ≥2 observations) can never commit it; the bench's
  single-frame lines are then sampling artifacts, not pipeline failures."

Constraints: docs/spec/05 requires temporal voting over several adjacent
frames, and docs/spec/12's failure-injection set (rapid textbox advances,
delayed/duplicated model responses) is what the ≥2-observation commit
gate defends against in the product. The bench's tracker config
(VoteWindow 3, StableWindow 100 ms) is the pipeline's synthetic-replay
config (internal/pipeline) — loosening it in the bench would grade a
pipeline the product does not run. AGENTS.md rule 3: no simulation
theater — a green that buys itself by weakening the precision gate is a
false positive.

## Options

1. **"Fix recognition"**: lower the commit requirement so a one-frame
   line commits (StableWindow 0 / same-frame STABILIZING→READY /
   AppearFrames relaxation). Rejected: it converts a sampling artifact
   into a product-level false-positive risk (a flickering or half-read
   box could commit), contradicts the documented rationale above, and
   changes the pipeline the bench is supposed to grade. The dense
   fixture's tail (the same one-frame truncation) would then commit on
   EMPTY OCR — a commit with no text is worse than the abstention.
2. **"Grow the holdout with more labelled samples from the existing
   newgame recording"** (chosen): re-record the windwaker-newgame fixture
   the way the dense fixture records it — the same newgame scenario
   (cold boot, the screen-driven drive to the first legend subtitle) but
   with the parallel adaptive capture (denseCapturePlan, ≥10 Hz on the
   subtitle scenes) riding the WHOLE opening legend (WatchLegend) instead
   of stopping at the drive's observation window. The holdout grows from
   two labelled lines (one truncated to one frame) to the legend's full
   labelled line set (~20 lines, each with ≥2 frames — committable).
   `TestRealRecordFixture` is changed to record this way (the one-off is
   codified: the next re-record keeps the property), and the fixture is
   re-recorded against the real image.

## Decision

Option 2. The windwaker-newgame fixture is re-recorded dense (drive +
denseCapturePlan + WatchLegend, fixture id unchanged); its bench holdout
is then the legend's full labelled line set, graded one way by the same
measured clauses. The recognition tracker is unchanged.

The expected outcome, and why it is honest: the recording ends at the
measured gameplay screen (the legend watch's boundary), so its tail line
is cut at the boundary too — the same one-frame truncation the dense
fixture carries (cx0021). That one abstention on a truncated tail is the
correct pipeline behaviour, and with ~19-20 committable lines the recall
lands at or above the ≥95% bar, exactly like the dense fixture (95.0%,
n=20). Both fixtures then grade the same scenario at the dense rate —
the newgame fixture is the M6 scenario-referenced recording (the dolphin
scenarios' pins) and the dense fixture is the ADR-0058 dialogue-rate
recording; keeping both is the fixture provenance the scenarios need.

## Validation

- `TestRealRecordFixture` (real image, `GN_GATE=test-dolphin`): records
  the dense fixture, asserts the manifest's capture block (HighHz ≥ 10)
  and the line count; the printed manifest SHA-256 re-authorizes the
  dolphin scenarios' pins against the external fixture root only (the
  gate's scenario phase runs against the default artifacts/fixtures
  root, whose pins are untouched).
- `make bench` (fixture root `$GAME_NARRATOR_FIXTURE_DIR`):
  windwaker-newgame recognition_recall ≥95% (n ≈ 20), top-1 exact on the
  committed lines; windwaker-legend-dense unchanged (95.0%, n=20, the
  documented tail truncation).

## Reversal

Restore the original `TestRealRecordFixture` body (drive-only recording,
no capture plan, no WatchLegend) and re-record the fixture; the holdout
returns to the two-line shape and its 50% verdict. Nothing in the
pipeline or the bench grader is changed by this ADR, so no further
reversal is needed.
