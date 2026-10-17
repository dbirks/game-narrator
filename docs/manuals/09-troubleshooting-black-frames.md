# Troubleshooting black frames

## Purpose

Diagnose "the frame is black / the game frame never appears" for the dev
Dolphin bridge and the OCR pipeline, using the measured readiness predicate
instead of guessing. A process that exists is not a rendered game
(docs/spec/11 "Avoiding false positives": do not declare test passed because
the Dolphin process existed).

## Preconditions

- `make diagnostics` (`doctor --dev`) run, so `emulator.dolphin` and
  `game.image` have been probed: bare `dolphin-emu --version` aborts on a
  headless TTY host — run it under `xvfb-run -a`, as the bridge does.
- For replay/bench black frames, `tesseract` and `magick` on PATH (else the
  gates report NOT RUN, which is a missing capability, not a black frame).

## The readiness predicate (ADR 0034, `internal/dolphin/readiness.go`)

`WaitForGameFrame` accepts a frame only when **all three** clauses hold, at
the 1 s probe cadence, and the first sample is never accepted (it has no
predecessor):

| Clause | Threshold | What a black/blank failure looks like |
| --- | --- | --- |
| Luma entropy on the render-window crop | `> 0.1` | a blank or crashed render window measures ~0 (spike reference: bare GUI 0.169, live game 0.277) |
| Differs from the pre-Dolphin baseline (`vs_baseline`, full screen) | `> 0.01` | a boot-logo frame over the same background the bare GUI shows does not cross |
| Differs from the previous shot (`vs_prev`, window crop of both shots) | `> 0.001` | a frozen window measures `0.0000` — the observed false positive (t=6 s: entropy 0.138 / vs_baseline 0.0612 / vs_prev 0.0000 was rejected by exactly this clause) |

When the budget is exhausted the bridge logs the measured values against the
thresholds, e.g. `dolphin: no game frame within … (last measured:
entropy=… want >0.10, vs_baseline=… want >0.010, vs_prev=… want >0.001) —
see the bridge-emu.log in the user dir`.

## Steps

1. **Read the last-measured line** and map it to the failing clause:
   - `entropy≈0` → nothing is being rendered: check the window exists and
     the Xvfb display is up (the bridge's own supervision covers this;
     inspect `bridge-emu.log` in the run's isolated user dir for the
     emulator's own error).
   - `entropy` fine, `vs_prev=0.0000` → a **frozen** frame: the emulated GPU
     is not producing motion (llvmpipe stalls, a hung attract scene).
     Restart the run with a fresh isolated user dir
     (08-dolphin-profile-reset.md) before suspecting the predicate.
   - `vs_baseline` below threshold → the window still shows the bare GUI
     (Dolphin booted but the game did not start): check
     `GAME_NARRATOR_GAME_IMAGE_PATH` resolves to the user-owned image.
2. **Check the budgets, not the code.** On the llvmpipe dev box the render
   window arrives within the 60 s launch timeout, but the *game frame itself*
   can take ~20 s; `WaitForGameFrame` has its own longer budget. A "black
   frame" at t=6 s on this host is expected, not a regression.
3. **For replay/bench OCR** (not live Dolphin): run `doctor`'s
   `ocr.controlled_image` check on the controlled image; a black crop there
   means the fixture/normalisation path, not the emulator.

## Verification

- The next `WaitForGameFrame` passes with a logged sample satisfying all
  three clauses, and `ProveInput`'s control window is measured against the
  live (moving) scene.
- `make test-dolphin` reports its real-test results per the gate's usual
  NOT RUN / PASS / FAIL discipline.

## What NOT to do

- Never accept a frame on "the process is running" or on a single non-zero
  sample — the conjunction (entropy **and** baseline **and** motion, with
  the first sample rejected) is what makes each clause do work.
- Never lower the thresholds to hide a black frame: they are the spike's
  calibrated values (ADR 0007/0034); changing them needs an ADR with
  measured evidence.
- Do not copy the real `~/.local/share/dolphin-emu` into a run to "give it
  a profile" — isolation is a hard rule (08-dolphin-profile-reset.md).
