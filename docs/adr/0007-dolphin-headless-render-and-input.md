# ADR-0007: Dolphin headless render and input path (Xvfb + OGL/llvmpipe, controller Pipes FIFO)

Date: 2026-10-16
Status: accepted (input path proven in-game on 2026-10-16; see "Evidence and test plan")
Owners: project (M0 Dolphin spike)
Related requirements/tests: FR-13, risk R-05, GN-T (docs/spec/12 "Emulator/hardware":
"Dolphin boot and screenshot, proper graphics setup", "live input pipe/controller
event accepted"); docs/spec/11-dolphin-automation.md modes B/C

## Context and constraints

`docs/spec/11` requires a coding agent on this Arch box to launch a user-supplied
GameCube image in Dolphin, capture real frames and inject real controller input,
without simulation theater. The development host has **no graphical session**
(`XDG_SESSION_TYPE=tty`, no `DISPLAY`), an Intel HD 630 GPU with `/dev/dri` render
node, `dolphin-emu 1:2609-1` (a Qt binary only — the package ships no
`dolphin-emu-nogui`/`dolphin-tool`), Mesa 26.2.4 + vulkan-intel, `Xvfb`, `ffmpeg`
`x11grab`, `ImageMagick`, `xdotool`/`xwininfo`. No `sudo`, no package installs, no
permission changes, no touching the user's real Dolphin directories, and the game
image may only be referenced by path (hash recorded).

Empirical results of `scripts/spikes/dolphin/{probe,boot,input}.sh`
(evidence in `artifacts/spikes/dolphin/`, findings in
`docs/spikes/dolphin-capability.md`):

* Xvfb's GLX is **llvmpipe** (`Accelerated: no`) — Xvfb has no DRI3, so GL is
  software-rasterised. Vulkan *enumerates* the Intel ANV device under Xvfb but
  Mesa reports `No DRI3 support detected - required for presentation`.
* Dolphin's GUI renders into Xvfb (root screenshot entropy 0.169) and `ffmpeg
  -f x11grab` reproduces it (20/20 frames, entropy 0.169).
* With `--batch --exec=<ciso> --video_backend=OGL` the real game boots
  (`Booting from disc: …`, `Active title: … (GZLE01)` in the isolated
  `user/Logs/dolphin.log`), presents a 640x480 render window whose title names the
  game, and produces measured game frames: entropy 0.277, differs from the bare
  GUI baseline, successive frames differ. First game frame at **t=17 s**.
* Throughput is **below real time** under llvmpipe (≈0.8–1.0x at 1x internal
  resolution, vsync off): the ~5-minute opening movie took ~5–6 minutes of wall
  clock.
* Input: Dolphin's compiled-in controller **Pipes** backend creates a device
  `Pipe/<index>/<filename>` for every file in `<user>/Pipes/`, with a plain-text
  protocol (`PRESS|RELEASE A|B|X|Y|Z|START|L|R|D_*`, `SET L|R|x`, `SET MAIN|C|x y`).
  The channel wires up (Dolphin opens the FIFO: `lr-x------ fd 24 -> user/Pipes/pipe1`; the saved `GCPadNew.ini` keeps `Device = Pipe/0/pipe1` unchanged). **In-game acceptance is now proven**: with `[Input] BackgroundInput = True`, one `PRESS START` / 0.5 s hold / `RELEASE START` on `Pipe/0/pipe1`, with no window focus and no keyboard injection, moved the game off its title screen and onto the "create a save file … Memory Card in Slot A?" prompt, while 15 no-input samples 1 s apart on the same screen never crossed the threshold (`run-20261016-134333/`, reproduced by `run-20261016-134555/`, `input.sh` exit 0).
  **Root cause of the three earlier failures:** Xvfb has no window manager, so the render window never receives X input focus, and Dolphin's `InputManager` does not poll *any* device — the Pipes FIFO included — while unfocused unless `BackgroundInput` is true. Every earlier press was simply never read.
  **Second trap:** this game's title screen is a slow camera orbit with the logo overlaid (RMSE to a fixed reference swings 0.00-0.40 by itself) and, if left alone, its attract mode starts the opening movie on its own ~50-90 s later — the very transition an input test looks for. Pixel-delta ratios and "the screen stopped changing" are both useless here; `input.sh` uses nearest-neighbour RMSE against 12 verified title framings plus a letterbox-band test (title 0.33-0.60, letterboxed movie 0.000), recomputes the crossing threshold from the measured control, and runs the control *before* the press inside one title visit.
* `uinput` is unusable here on two counts: `/dev/uinput` is `crw------- root root`
  (needs privileged setup we are not allowed to do) **and** this build contains no
  uinput input source at all.

## Decision

For Mode B (Dolphin + recorded provider responses) and Mode C (Dolphin + real
models) on hosts without a graphical session, standardise on:

1. **Display path:** `Xvfb :NN -screen 0 1280x1024x24` + `--video_backend=OGL`,
   with `--config=GPU.Settings.InternalResolution=1`,
   `--config=GPU.Hardware.VSync=False`, launched as
   `dolphin-emu --user=<repo>/artifacts/<run>/dolphin/user --batch --exec="$GAME_NARRATOR_GAME_IMAGE_PATH"`.
   Frame source = `ffmpeg -f x11grab` on that display, cropped to the render
   window geometry resolved through `xwininfo`/`xdotool` (never the whole root
   window). Rendered-ness is asserted with measured entropy +
   differs-from-baseline + successive-frame-differ, exactly as in this spike.
2. **Input path:** controller **Pipes** FIFO in the isolated user dir
   (`user/Pipes/pipe1`, device `Pipe/0/pipe1`, mappings
   ``Buttons/Start = `Button START` `` etc.), with one long-lived writer process
   holding the FIFO open so Dolphin never sees EOF. A single writer process owns
   the FIFO; commands are newline-terminated and idempotent per press/release
   pair, so a press is `PRESS <tok>` → hold ≥0.5 s → `RELEASE <tok>` (0.5 s spans
   several llvmpipe frames and was sufficient; the script also carries 2 s and
   `A` fallbacks, unused).
   **`[Input] BackgroundInput = True` is part of this contract**: set it in
   `<user>/Config/Dolphin.ini` before launch *and* pass
   `--config=Dolphin.Input.BackgroundInput=True`, because the emulator does not
   poll controller sources while the render window is unfocused and a headless X
   server has no window manager to focus it. Without it the FIFO is opened, the
   bytes are accepted by the OS, and nothing reaches the emulated pad — a silent
   no-op that looks like "input is broken".
   Input has no acknowledgement channel, so every driven scenario must assert a
   **scene predicate** (nearest title-framing RMSE + letterbox band test) against
   a **no-input control window taken immediately before the press**, at a true
   1 s sampling cadence — never a raw pixel-delta ratio and never "the screen
   went quiet".
3. **Isolation and provenance:** one `--user` dir per suite run under `artifacts/`
   (never `~/.local/share/dolphin-emu`), game image referenced by env var, only
   its SHA-256 recorded, `Logger.ini` forced on so `dolphin.log` proves the booted
   title and backend, and every PASS in the evidence bundle pointing at a real
   screenshot/log.
4. Timing assertions must be expressed against *emulated* progress (screen
   predicates, event text) with tolerances, never wall-clock sleeps, because
   llvmpipe throughput varies with host load.

## Alternatives considered

* **Vulkan backend under Xvfb** — no swapchain presentation without DRI3
  (three Dolphin attempts under Xvfb ended on a `Warning` dialog with `No DRI3 support detected - required for presentation` and permanently identical frames). Kept as a per-host option only where an accelerated display
  exists.
* **`Software Renderer` backend** — available in the CLI list, unmeasured; it
  removes the GPU from the loop entirely but was not needed since OGL/llvmpipe
  already boots the game. Revisit only if a host has no usable GL at all.
* **`--video_backend=Null`** — no presented pixels, so it cannot satisfy the
  "assert rendered game frame" rule; rejected as a *test* path.
* **`uinput` virtual gamepad** — blocked by root-only `/dev/uinput` *and* absent
  from this build; would additionally need a privileged helper and udev rule.
* **XTEST keyboard (`xdotool type`) into the emulated pad** — the default
  `GCPadNew.ini` maps GC buttons to keyboard keys, so this works, but it is
  focus-dependent and explicitly disfavoured by `docs/spec/11`; kept only as a
  manual fallback for emulator hotkeys (savestate, DTM record), never as the
  game-input channel. `xdotool windowactivate/windowfocus` on the render window is
  implemented as the *first* fallback in `input.sh`'s press ladder and turned out
  to be unnecessary once `BackgroundInput` was set — focus is not required for the
  Pipes channel with that setting.
* **DTM playback (`--movie`) as the primary driver** — deterministic and the right
  long-term fixture mechanism, but a DTM must first be authored against a real
  session; this spike establishes the live-input channel that makes authoring
  possible. Mode A (pure frame replay) remains the default for ordinary PRs.
* **Real X display / Xwayland / EGL headless** — would give GPU-accelerated
  frames, but there is no session on this host and no `sudo`; `VK_EXT_headless_
  surface` is exported by the driver but Dolphin does not present through it.

## Consequences and risks

Positive: one command reproduces a real booting game with captured frames and a
non-focus-dependent input channel on a plain TTY host; no privileged setup; the
same x11grab + entropy assertions become the `FrameSource` conformance test for
fixture replay.

Negative / risks: llvmpipe is ~sub-real-time, so Mode B/C scenarios need generous
budgets or an accelerated host, and latency claims (R-07) must not be measured on
this path; Dolphin has no headless CLI target (`dolphin-tool`) here, so the
rendered-frame assertion can never be replaced by "process existed"; the pipe
channel carries no acknowledgement back, so input acceptance must always be
proved by a scene predicate versus a no-input control window (proved feasible: the
spike's predicate costs ~0.3 s of ImageMagick work per 320x240 sample and still
fits a 1 s cadence); games whose attract mode performs the same transition the
test looks for need the control window taken *first*, inside one title visit; a
rename of `BackgroundInput` or a change to Dolphin's focus gating inside
`InputManager` re-breaks input *silently*, so the ini line is captured as evidence
on every run; Dolphin's config format
and the pipe protocol are upstream-version dependent (pinned 2609) — a version
bump re-runs this spike; `--batch` still needs an X server, so packaging must ship
`Xvfb`/`ffmpeg`/`ImageMagick` as dev dependencies.

## Evidence and test plan

Commands (all output captured in `artifacts/spikes/dolphin/`, one
`run-<timestamp>/` directory per `input.sh` run so a rerun can never overwrite the
frames it classifies against):
`scripts/spikes/dolphin/probe.sh`, `boot.sh OGL 240`, `boot.sh Vulkan 120`,
`input.sh 700`. Gate assertions mirror `docs/spec/12`: boot+screenshot
(`boot-OGL-*.png`, `boot-OGL-frames.tsv`, `boot-OGL-first-frame-seconds.txt`),
graphics setup (`glxinfo.txt`, `vulkaninfo-summary.txt`, `probe-result.txt`),
capture (`x11grab-*.png`), **live input accepted** (`run-20261016-134333/`:
`input-OGL-report.txt`, `input-OGL-timeline.tsv`, `input-OGL-calibration.tsv`,
`input-OGL-backgroundinput.txt`,
`input-OGL-{before,title-1,control-1-01..15,after,after-START-nofocus-01..15}.png`;
reproduced in `run-20261016-134555/`), plus the device-open proof `pipe-fd.txt`.
Acceptance rule for the input gate (as implemented): the game must be recognised
on its title screen by nearest-neighbour normalised RMSE against 12 retained
title framings (`< 0.228`; calibrated on retained evidence — title-vs-title max
0.1980, title-vs-movie min 0.4260); a 15-sample no-input control window at a true
1 s cadence must not cross `SCENE_TH` (midpoint between the measured control
maximum and the title-vs-movie minimum — 0.338 in that run; measured control max
0.2499, min letterbox band 0.3299) and must not be letterboxed; after
`PRESS START` / 0.5 s / `RELEASE START`, **two consecutive** samples must cross
`SCENE_TH` (measured 0.4073 with letterbox band 0.0614 — the letterboxed
transition, followed by the "create a save file in Slot A? Yes/No" prompt) — and
the before/after/control screenshots are retained for human review, because
numbers alone are not accepted.
Re-run whenever `dolphin-emu`, `mesa`, `xorg-server-xvfb` or `ffmpeg` versions
change, or when Dolphin's `Input` config keys change;
`docs/spikes/dolphin-capability.md` records the matrix per run.

## Migration / rollback

No persistence, protocol or dependency change to existing subsystems: the spike
scripts and this ADR are additive, and the Dolphin user dirs stay under
`artifacts/` (gitignored). To roll back, stop using the Xvfb+OGL+Pipes driver and
run Mode A frame replay instead (which needs no emulator) — the runtime pipeline
is unchanged either way. Switching a host to an accelerated display is a config
change (`--video_backend`, display provider) plus a re-run of the spike matrix.

## Follow-ups

* **Resolved 2026-10-16: a controller event is accepted in-game.** `input.sh` now
  exits 0 on a real scene change (title → letterboxed transition → the "create a
  save file in Slot A?" prompt) with a clean 15×1 s no-input control, given
  `BackgroundInput = True`. Mode B/C no longer has to be reported as
  `input_not_proven` when the pipe device is bound and that setting is on.
* **Blocker for repeatable fixture driving: the title/attract timer.** Screen
  predicates on this game must account for the opening movie starting by itself,
  so a Mode B scenario should start from a savestate placed *inside* the
  interactive window (next item) rather than from a cold boot.
* Prove `--save_state` round trip + checkpoint perceptual-hash validation.
* Drive a dialogue textbox into the real `FrameSource`/OCR path (Mode B E2E).
* Measure an accelerated host (DRI3/Xwayland or real session) and compare time to
  first game frame / scene throughput; record in the capability matrix.
* Decide whether to vendor `dolphin-tool` (needs a different package/build) for
  header/hash/convert helpers.
* Re-review when Dolphin 2609 → next Arch release lands.
