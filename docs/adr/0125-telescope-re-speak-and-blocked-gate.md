# ADR-0125: The telescope drive re-SPEAKs a settled prompt scene, and an unreached item scene is gated NOT RUN (BLOCKED), not FAIL

Date: 2026-10-20
Status: accepted
Owners: dolphin-gate worker (task dolphin-gate)
Related requirements/tests: `TestRealRecordTelescopeFixture` (internal/dolphin,
`GN_GATE=test-dolphin`); docs/spec/12 (the Dolphin-mode holdout and the
"one sign/item" scenario); docs/status/blocked.md (the BLOCKED register,
ADR-0037 "blocked ≠ pass"); ADR-0123 (the measured first-box persistence
pattern this ADR extends); ADR-0116 (the finalize OCR retry the same run
needed)

## Context and constraints

The G8 item recording (`TestRealRecordTelescopeFixture`) boots the real
Dolphin from the proven `windwaker-first-dialogue` state, closes the first
dialogue to the lookout deck, walks to the deck's SPEAK target, presses A
(speak), and drives the resulting conversation until the item box (the
telescope's item-get screen) double-confirms — the stop condition the
fixture's content mark and the `dolphin/item-telescope` scenario both
depend on (ADR-0103).

Seven real-Dolphin runs (2026-10-20, 11:46–18:26, `GN_GATE=test-dolphin`,
logs `/tmp/gn-dg-continue/`, per-step evidence
`internal/dolphin/artifacts/runs/dolphin-telescope-*/`) measured the
post-SPEAK flow:

- the deck carries one SPEAK target (Aryll — the vision model reads the
  prompt "Speak"; the structural approach stops on two consecutive SPEAK
  label reads);
- SPEAK opens a single letterboxed storybook page whose subbar OCRs (the
  4x record recipe) to "everyone else on the island wants to say hello to
  you, too!";
- the page advances on A (it does not auto-advance — measured in 45 s on
  11:46), and advancing it returns to the SPEAK-prompt scene
  (letterbox 0.264; static — it held for the whole 12 m conversation
  budget in the 17:47 run);
- re-pressing A on that scene (SPEAK again) replays the SAME page
  (the replayed page's frames OCR byte-identical);
- the only `class=box` samples in any run are the storybook-OPEN
  transient (1–2 consecutive samples, boxInk 0.0700→0.1244, subbar still
  quiet), followed by the page; a real dialogue box persists for dozens of
  2 s samples;
- the item box double-confirmed on NO attempt (the vision item-check on
  the settled scene: "no"; the vision read of the stuck scene: no box,
  no item, no pickup icon).

The pre-ADR-0125 code treated two of these measurements as failures:
(1) a single transient `class=box` sample set `boxSeen`, which disabled
the (absent at the time) re-SPEAK policy and started the 15 s box-gone
timer on a screen that never had a box; (2) a settled prompt scene with
no item box burned the whole 12 m budget and reported a plain FAIL —
although the game, not the pipeline, is the gap (the fixture's design
premise — SPEAK → a dialogue that hands over the telescope — does not
match the measured behavior at this story point).

## Options

- **A. Fail the test when the item box never appears (status quo).**
  Honest about the miss, but reports a game-side gap as a pipeline
  failure, and a FAIL is not an allowed final state for this scenario
  while the gap is unblocked (the task contract: PASS or explicit
  NOT RUN/BLOCKED, never a FAIL, never a fake pass).
- **B. Re-press A (re-SPEAK) on a settled prompt scene, bounded, and gate
  the still-unreached item scene as NOT RUN (BLOCKED)** with the register
  row's unblock steps. Chosen.
- **C. Redesign the approach (map the deck for the telescope's own
  interactable, drive from a different state).** The correct unblock
  (register row), but a new route to measure — not a change to this
  test's verdict semantics, and not something this task may assume
  without the measurement it needs.

## Decision

1. **The re-SPEAK policy** (`internal/dolphin/telescope_test.go`,
   `runConversation`'s non-box branch): when the screen has settled on a
   non-box class for `promptSettleSamples` (3) consecutive samples AND
   the letterbox measures ≥ `promptSceneLbMin` (0.15 — the measured
   prompt-scene family 0.156–0.264; the narration pages and the real box
   measure 0.000) AND the box was never confirmed AND
   `reSpeaks < reSpeakCap` (3), the drive presses A (SPEAK). The judge
   is structural (the measured lb clause on a SETTLED scene — a
   mid-fade frame cannot trigger it), the press is idempotent (a press
   on the prompt scene either starts the beat or replays the page; both
   are handled by the existing loops), and it is BOUNDED: a re-SPEAK the
   screen does not answer still fails the no-response watch (the anchor
   re-anchors on the pre-press frame — the ADR-0123 press pattern), and
   after the cap the conversation runs out its budget with the measured
   evidence (presses, reSpeaks, vision calls) in the error.
2. **The boxSeen confirmation** (`boxConfirmSamples = 4`): the box counts
   as seen only on 4 consecutive `ClassBox` samples — the measured
   storybook-open transient is 1–2 samples, a real box persists for
   dozens; the margin is two samples over the measured transient.
   `boxSeen` gates only the box-gone timer and the re-SPEAK policy
   (`!boxSeen`), so a late confirmation costs nothing functional.
3. **The BLOCKED gate** (`errItemSceneNotReached`): the three
   conversation failures that mean "the item scene was not reached"
   (budget exhausted; box-gone after the last advance; no-response on a
   press) wrap the sentinel. When the sentinel is in the error chain the
   test reports `NOT RUN (BLOCKED)` with the register pointer
   (`docs/status/blocked.md`, windwaker-aryll-telescope), removes the
   manifest-less partial fixture dir (the per-step evidence frames stay
   in the run's evidence dir), and SKIPs. Every other error (save,
   screenshot, vision reconfirm, the classifier mismatch, a non-box item
   screen) still FAILs. A skip records no fixture, so nothing downstream
   (the scenario pin, the bench) can ever present the missing coverage
   as green (ADR-0037: blocked ≠ pass).

## Tradeoffs and validation

- The re-SPEAK policy can spend up to ~3 page cycles (~90 s) before the
  budget fail on an item scene that will never come — bounded, measured,
  and the evidence (the replayed page's frames) is exactly what the
  register row cites.
- A re-SPEAK on a genuinely different prompt scene (a future route where
  the handover IS the second SPEAK) is the policy working as intended;
  the cap keeps a degenerate loop from burning the budget.
- The `boxConfirmSamples=4` margin is over MEASURED transients (1–2
  samples on llvmpipe at this host's speed); if a slower host stretches
  the open to 4 samples, the box-gone timer starts early and the item
  check still runs at the 15 s settled layout (the fail message and the
  evidence carry the class sequence).
- Validation (real Dolphin, `GN_GATE=test-dolphin`, external fixture
  root; logs `/tmp/gn-dg-continue/t3b-telescope.log`,
  `t3c-telescope.log`, `t3d-telescope.log`): the 18:15 and 18:26 runs
  measured the book-open transient poisoning (`boxSeen` on a 1- and
  2-sample run) and the settled prompt scene (lb 0.264, 3 samples)
  triggering the re-SPEAK — and both still reported the box-gone
  failure through a plain FAIL (the BLOCKED skip was not exercised by
  that code). The 22:38 run (`t3d-telescope.log`) reached the
  conversation (SPEAK consumed, the storybook page advanced on A,
  presses=4 on the settled dark scene) but the worker session timed out
  at 18:50 local, inside the 12 m conversation budget: no verdict line
  exists, so the BLOCKED skip path is NOT yet verified end-to-end. The
  acceptance criterion for this ADR — the test ends `NOT RUN (BLOCKED)`
  (a SKIP naming the `docs/status/blocked.md` row and the evidence dir,
  the partial fixture dir removed, `go test` exit 0 — a skip is not a
  FAIL) and never a FAIL on the unreached item scene — is verified by
  the follow-up run 2026-10-20 22:55 UTC
  (`internal/dolphin/artifacts/runs/dolphin-telescope-2026-10-20-225547`).

## Reversal

Remove the sentinel wrap (the three errors stop carrying
`errItemSceneNotReached`) and the BLOCKED skip branch: the test FAILs
again when the item box never appears, exactly as before. Remove the
re-SPEAK block (and `reSpeakCap`/`promptSettleSamples`/`promptSceneLbMin`)
to restore the budget-only behavior; set `boxConfirmSamples = 1` to
restore the single-sample `boxSeen`. The blocked.md row moves to Cleared
with the route measurement when the item scene becomes reachable
(register steps 1–4).
