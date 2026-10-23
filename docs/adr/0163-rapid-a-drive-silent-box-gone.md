# ADR-0163: The rapid-A drive treats a silent box as gone (the conversation ended)

- Status: accepted
- Date: 2026-10-22
- Related requirements/tests: catch-up live E2E (issue #25), ADR-0122 (catch-up policy), ADR-0065 (live gates)

## Context and constraints

The catch-up rapid-A driver (`cuDriver.decidePress` in `internal/e2e/catchup_live_helpers_test.go`) advances the
conversation by pressing A: first on a commit (the rapid path), then on the stall fallback (a box whose text went
stable without committing) and the bounded lost-A retry.

Its "box text stable" signal comes from `lastText`/`textChanged`, folded only by `text.observed` events. Once the
conversation ends and the box closes, the observations stop; the stale last text therefore keeps looking stable, and
the stall fallback fired a press into gameplay — which started a new conversation. On 2026-10-22 one run failed with
"the rapid-A drive did not finish the conversation within 10m".

## Decision

The driver treats the box as gone — instead of pressing on the stale text — when any of:

1. the box region last read empty (`boxEmpty`, the pre-existing closing/between-box signal), or
2. the newest `text.observed` reported `region_present: false`, or
3. no fresh `text.observed` has arrived within the press-stability window (`cuPressStable`, 3 s).

The existing box-gone path then runs: with committed lines it marks the conversation over (`box gone: the conversation
ended`) and exits; without commits it reports the box as not yet visible.

## Alternatives considered

- **Only widen the empty-read clock.** Rejected: the conversation can end without a final empty read (the stream
  going quiet), and the region presence is the detector's own "textbox gone" verdict.
- **Widen the stall gap instead.** Rejected: a longer stall gap delays legitimate advances and does not cover the
  region-presence / quiet-stream signals.
- **Press on stale text until a commit arrives.** Rejected: that is the observed failure mode — the press fences the
  game into a new conversation.

## Consequences and risks

- The stall/lost-A fallbacks now require a fresh observation within the press window; a quiet box is the
  conversation's end, not a stable box.
- The between-box transition (reads empty ~1-9 s) keeps its own `cuConvGone` (30 s) gone threshold; the new signals do
  not shorten it.
- Live behaviour is unchanged while observations flow; the fix only engages once the stream goes quiet or reports the
  box absent.

## Evidence and test plan

- Unit tests: `TestCuDecidePressStaleTextNoPressAfterQuiet`, `TestCuDecidePressStallOnStablePresentText`,
  `TestCuDecidePressBoxAbsentRegionNoPress` in `internal/e2e/catchup_live_driver_test.go` over synthetic event
  pages. The stale-text test reproduces the 2026-10-22 press ("stall: … stable without a commit for 45s") against the
  pre-fix decision logic and passes with it.

## Migration / rollback

None — the change is harness-local (test helpers); no persisted state, protocol or release artefact changes.

## Follow-ups

None known. Re-validate with `make test-dolphin` (the real-Dolphin catch-up gate) when the fixture is available.

## Amendment 1 (2026-10-23): silence is ambiguous — look at the screen

Evidence: the `make test-av` run in the a83aa1b gate sweep (2026-10-23 04:36Z) ended the conversation after 2 presses
with Aryll's box still on screen for the remaining ~25 s of the recording. The rapid press had landed while the
typewriter was still running (it completed the text instead of advancing), and once the box sat fully typed the
session emitted no `text.observed` at all (it reads the region only when it changes). Rule 3 above treated that
silence as a closed box: the lost-A retry was suppressed and, after `cuConvGone`, the drive ended.

Decision: silence alone no longer means "gone". When the session has been silent for `cuPressStable`, the driver
grabs the Dolphin screen through the bridge (`Bridge.Screenshot`) and runs the drive's own `dolphin.BoxPresent`
checkpoint (at most every `cuPressStable/3`). A fresh check that sees a box makes the silence a static box: the
stall / lost-A fallbacks run and the conversation is not over. A check that sees no box (or a failed grab) keeps
rule 3's behaviour. Rules 1 and 2 (empty or absent readings) are unchanged.

Validation: `TestCuDecidePressSilentStaticBoxOnScreenStillPresses`, `TestCuDecidePressSilentNoBoxOnScreenEndsTheDrive`;
`BoxPresent` checked against frames of the failing run (static box at 30 s and 40 s → box; the opening scene → no
box); the next `make test-av` run.

Tradeoff: `BoxPresent` is tuned for the lower box band, so a silent top-of-screen box still reads as no box and
behaves as before this amendment (no fallback press, drive ends after `cuConvGone`). Reversal: drop the screen
check; rule 3 applies again.
