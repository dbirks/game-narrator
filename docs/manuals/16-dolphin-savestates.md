# Dolphin savestates and their dev dependencies

## Purpose

Save and load Dolphin savestates from the dev bridge with a proven,
deterministic method — keyboard hotkeys injected over XTest — and know
exactly which dev-host packages make it work. Savestates are how the M6
drives reach later game regions and how the two blocked dolphin scenarios
get unblocked (15-dolphin-fixture-recorder.md).

## Why hotkeys on the keyboard device (not the controller pipe)

Hotkeys bound on the controller FIFO **cannot** work: Dolphin's SI
polling (~120 Hz) and its hotkey scheduler both read the **same** Pipes
FIFO, so a chord byte goes to whichever thread reads first — consumer
contention, nondeterministic firing. The working layout (ADR 0043
amendment 7) has one consumer each:

- the **controller FIFO** keeps a single consumer (the drive's input);
- the **savestate hotkeys are bound to the X11 keyboard device** — the
  device 2609's XInput2 backend resolves to, measured from the
  exit-rewritten spike user dirs (every keyboard controller resolves to
  that exact string) — and the keys are **injected with XTest**
  (`xdotool` keydown/keyup, 150 ms hold) on the bridge's private Xvfb
  display; the hotkey scheduler's own thread is the sole consumer of the
  key state.

## Preconditions

- Dev dependencies on PATH (the bootstrap's "virtual display and input
  automation" group installs the first three):
  | Package | Used for |
  | --- | --- |
  | `dolphin-emu` | the emulator itself |
  | `xorg-server-xvfb` | the private display the bridge renders on |
  | `xorg-xwininfo` (x11-utils) | window/frame inspection |
  | `xdotool` | XTest key injection for the hotkeys |
  | `ffmpeg` / `tesseract` | the frame source and the OCR oracle |
- The hotkey layout is **opt-in**: `LaunchSpec.Hotkeys=true` on the
  bridge's launch spec. The default single-pipe drive path is unchanged
  (no `Hotkeys.ini`, no focus gate) and stays proven by the same gate.

## Steps

1. **Launch with hotkeys on.** The bridge writes, into its isolated user
   dir:
   - `Config/Hotkeys.ini` — `Save State/Save State Slot 1 = F1`,
     `Load State/Load State Slot 1 = F2`, device = the measured X11
     keyboard device string;
   - `Config/Dolphin.ini` with `HotkeysRequireFocus = False` (the 2609
     default is `True`, which would gate the hotkeys on focus the headless
     bridge does not hold);
   - the normal `GCPadNew.ini` (Pipe/0/pipe1) — **no second FIFO is
     created**, which is the whole point of the layout.
2. **Save / load.** The bridge's `SaveState(ctx, slot)` / `LoadState(ctx,
   slot)` fire `F1` / `F2` over XTest on the Xvfb display. State files land
   in **`StateSaves/`** under the isolated user dir — *not* `states/`
   (the old glob watched the wrong directory and concluded, falsely, that
   the hotkey never fired).
3. **Verify a round trip the way the tests do — a double restore.** The
   restored scene is the animated opening legend, so a pixel compare
   against the pre-save frame would measure the timeline's own motion
   instead of the restore. The instrument is: save slot 1; advance
   (assert the frame moved, `rmse > 0.05`); load, settle, frame X1;
   advance again with the same input; load, settle, frame X2; assert
   `rmse(X1, X2) ≤ 0.10` and the same screen class for both. A fresh
   `--save_state` boot from the state file must land on the saved scene.
   Clean-tree measured result: `rmse(X1, X2) = 0.000` in consecutive runs
   (llvmpipe renders byte-identical frames), both frames class
   `storysub`, and the `--save_state` boot landed on the legend's
   narration subtitle within 30 s.

## Verification

- `TestRealDriveToFirstDialogue` (hotkeys on) PASSED twice in a row in the
  `make test-dolphin` gate (327 s / 344 s); `TestRealDolphinBridge`
  PASSED with the hotkey config present (66 s,
  `hotkey_device=XInput2/0/Virtual core pointer`).
- The bridge checks the exit-rewritten `Config/Hotkeys.ini` against the
  measured device string after a clean exit — a mismatch fails the test
  instead of silently losing the binding.
- `TestRealAbruptContextKill` (the default single-pipe path) passes with
  no leftover processes — the opt-in did not leak into the default path.

## What NOT to do

- Do not bind the savestate hotkeys to the controller pipe "to simplify"
  the layout: the consumer contention is measured, not theoretical —
  the chord byte is consumed by whichever thread reads the FIFO first.
- Do not watch `states/` for state files; the 2609 layout writes
  `StateSaves/`.
- Do not drop `HotkeysRequireFocus = False` or assume window focus on the
  Xvfb display — the headless bridge does not hold focus.
- Do not commit savestate files or state directories: they are game state
  (user data, AGENTS.md rule 5) and live in the gitignored isolated user
  dir / fixture dir.
- Do not present the opt-in hotkey path as the default: the default
  single-pipe drive path is what the runtime's admission logic and the
  default-path tests protect (15-dolphin-fixture-recorder.md).
