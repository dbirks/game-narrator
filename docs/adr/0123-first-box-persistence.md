# ADR-0123: The first box is real only when it holds (drive success and the first_dialogue anchor)

Date: 2026-10-20
Status: accepted
Owners: dolphin-gate worker (task dolphin-gate)
Related requirements/tests: `TestRealDriveToFirstDialogue`,
`TestRealRecordSavestateFixtures`, `TestRealRecordTelescopeFixture`
(internal/dolphin, `GN_GATE=test-dolphin`); docs/spec/11 ("Avoiding false
positives"); docs/spec/12 (Dolphin emulator mode); ADR-0054 (the screen
classifier's measured clauses, amendments 1-7); ADR-0043 (the savestate
hotkey path); ADR-0053 (golden-file fixture semantics)

## Context and constraints

The 2026-10-20 `make test-dolphin` gate run (main 551723c, fixture root
`$GAME_NARRATOR_FIXTURE_DIR`) failed three real-Dolphin tests. The retained
evidence (the gate's `go test -json` stream, the m6-recheck run logs of
2026-10-18, the fixture manifests) measured three related facts about the
first in-game dialogue box:

1. **`TestRealDriveToFirstDialogue` stopped early.** The past-legend drive
   (the wait-only ride from the first legend subtitle to the first
   dialogue box) returned success at step 72 on a frame the classifier read
   as the box's dark form (boxband mean=57, edges=0.095). The frame was a
   storybook page in the between-subtitle gap: the narration subtitle's
   bottom-bar text had faded (the storysub clause failed), the page was a
   sparse illustrated one (centre-band ink below the 0.06 clause), and the
   band below it measured dark, flat and glyph-inked. The classifier's
   measured dark-box clauses all matched — the classifier was not wrong
   about the clauses, the clauses do not separate that legend frame from
   the box (the dense storybook pages are separated by the centre-band ink
   0.141+; the sparse ones are not). The drive's 120 s end-assertion
   recheck then polled only legend frames (storysub/dark, boxband 53-71)
   and expired: the real box was still minutes away. The same mode failed
   runs 1-3 of the 2026-10-18 m6-recheck and passed runs 4-5 with
   unchanged drive code — a timing flake, not the signature.go
   Amendment-2 diff (for the legend frame, lb=0.000, the old and new
   letterbox clauses agree).

2. **`TestRealRecordSavestateFixtures` re-recorded the
   `first_dialogue` checkpoint on a transient.** Its phase 1b re-launches
   from the `legend_start` checkpoint, rides the legend wait-only, and
   saves `windwaker-first-dialogue.state` on the **first** `ClassBox`
   sample the 2 s cadence catches. The measured flow carries two transients
   that match the class and do not persist: the sparse-storybook frames of
   (1) and the pre-box scene's **brief** box that auto-advances within
   1-2 s (m6-recheck runs 1-3 stop frames: boxband mean=62/59). The
   14:03 state saved on one of them restored to a screen whose box was
   gone in seconds; phase 3 (the first-dialogue fixture's recording, which
   needs the box's typewriters to settle for the press pace) failed with
   "no press landed: the box's typewriters never measured a settled state".

3. **The real first NPC dialogue box holds.** The m6-recheck evidence: the
   box A-HOLDS unchanged for 10+ minutes (the 64 s no-input sample: the
   signature steady at boxband 46/0.031); the dense fixture held its box 1
   across 50+ captured frames.

Constraints: the classifier (signature.go) is shared with the G8
telescope's bright-scene box (Amendment 2, 2026-10-20) and with every
checkpoint — re-tuning its measured clauses to reject sparse storybook
frames risks the deck-scene box with no retained measured evidence to
trade against. AGENTS.md rule 3 (no simulation theater): the fix must be
measured, not asserted.

## Options

1. **Re-tune the classifier's dark-box clauses** (e.g. a namegrid-ink
   ceiling: storybook pages measure 0.199-0.207, the held box 0.029).
   Separates the measured families with margin, but the clause is not
   measured against the G8 deck-scene box (bright outdoor scene: the
   namegrid could carry scene ink), and a classifier regression would
   break the telescope recording and every checkpoint at once.

2. **Persist the drive's terminal recheck** (lengthen the 120 s window to
   the measured legend remainder). The recheck accepts the *same* clauses,
   so a longer window only delays the same wrong stop; it also slows every
   legitimate run.

3. **Require the first real-form box to hold** (this ADR): a single-sample
   confirmation at the drive's 2 s cadence in the drive's success gate,
   and a 3-consecutive-sample hold for the `first_dialogue` anchor. The
   measured families separate on persistence alone: neither transient
   (seconds) survives two 2 s samples; the held box (minutes) costs one
   cadence. No classifier change; the success gate keeps the exact
   measured clauses (RealBoxForm).

## Decision

Option 3, in three places:

- **`internal/dolphin/drive.go`** (`RunDrive`): when a run that is still
  seeking the box (past-legend ride, or no success yet) sees a
  `ClassBox && RealBoxForm` frame, the first such sample is a *candidate*:
  it is recorded in the transition log, runs the table's no-op
  dialogue-box action, and the loop keeps observing. The candidate counts
  as the success only when the NEXT sample also measures the real form
  (`boxConfirmed`); the flag expires on the first non-box sample, so the
  requirement is consecutive samples. The loop detector (`driveLoopSameMin`
  = 4) cannot fire inside the 2-sample hold. Unit-test consequence (the
  synthetic box-arrival tests in `drive_unit_test.go`): a run whose frame
  sequence ends on the box now logs **8/10/11** transitions (one more than
  pre-hold), because the candidate sample is its own recorded transition
  (`dialogue_box --(done)--> dialogue_box`, the no-op action, no inject)
  before the confirmation closes the run at `<end>` — the fake bridge
  repeats the last queued frame, which is exactly the consecutive-sample
  hold the real 10+ minute box A-HOLDS. The inject counts are unchanged
  (the candidate runs the no-op action).
- **`internal/dolphin/savestate_fixture_test.go`** (`recordSavestates`
  phase 1b): `windwaker-first-dialogue.state` is saved only when
  `ClassBox && RealBoxForm()` holds for 3 consecutive 2 s samples (4+ s of
  hold) — the same measured gate as the drive, made strict enough that the
  brief box (1-2 s) cannot satisfy it.
- **`Makefile` (`test-dolphin`)**: the live-play-UI phase needs the
  packaged SPA go:embedded into the test binary (`webassets.Built()`), so
  the gate builds it (`web-build`) between the scenario phase and the
  liveui phase. Without pnpm the build is skipped and the phase stays
  NOT RUN (a host without it never turns the gate red); a failed build
  removes the staged dist so the phase reports NOT RUN instead of walking
  a half-built UI.

## Tradeoffs and validation

- The confirmation costs one cadence (~2 s) on every legitimate box
  arrival — against a measured 10+ minute hold, this is noise.
- A box that appears and is dismissed within one cadence is missed by the
  drive; the measured WOTW flow has no such screen (the brief box is the
  only transient, and it is the one the fix targets).
- The classifier is untouched; the G8 telescope's bright-scene box and all
  checkpoints keep their measured behaviour.
- Validation: `TestRealDriveToFirstDialogue`, `TestRealRecordSavestateFixtures`
  and `TestRealRecordTelescopeFixture` under `GN_GATE=test-dolphin` on the
  real image (one Dolphin at a time), plus the full `make test-dolphin`
  gate (its scenario phase re-plays the re-recorded fixtures, its
  liveui phase runs the Playwright walk on the built SPA).

## Reversal

Remove the `boxConfirmed` hold from `RunDrive` and the 3-sample anchor
from `recordSavestates` (both are additive bookkeeping; the success
clauses and the classifier are unchanged). Revert the Makefile by
deleting the `web-build` step (the liveui phase returns to its
run-`make web-build`-manually NOT RUN behaviour).
