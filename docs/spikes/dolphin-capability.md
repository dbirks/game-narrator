# Dolphin headless capability spike — findings

Milestone M0 spike for risk **R-05** ("Dolphin headless display/input brittle",
`docs/spec/18-decisions-and-risks.md`), feeding `docs/spec/11-dolphin-automation.md`
and the `docs/spec/12-test-strategy.md` "Emulator/hardware" gate ("Dolphin boot and
screenshot, proper graphics setup", "live input pipe/controller event accepted").
Decision record: `docs/adr/0007-dolphin-headless-render-and-input.md`.

Everything below was measured on this host by `scripts/spikes/dolphin/`. Evidence
files live in `artifacts/spikes/dolphin/` (gitignored — the screenshots contain
copyrighted game visuals and must never enter the repo or a release).

## Host / build under test

| Item | Value |
| --- | --- |
| OS / kernel | Arch Linux, `Linux 7.2.9-arch1-1 x86_64` |
| Session | **no graphical session** (`XDG_SESSION_TYPE=tty`, no `DISPLAY`, no Wayland) |
| GPU | Intel Kaby Lake-S GT2 [HD Graphics 630] (`/dev/dri/renderD128` present, world-writable) |
| Emulator | `dolphin-emu 1:2609-1` (only `/usr/bin/dolphin-emu`; **no** `dolphin-emu-nogui` / `dolphin-tool` binary in the package) |
| Mesa / Vulkan | `mesa 1:26.2.4-1`, `vulkan-intel 1:26.2.4-1` |
| Virtual display | `xorg-server-xvfb 21.1.25-2` (`Xvfb`, `xvfb-run`) |
| Capture / measure | `ffmpeg n9.0.2`, `ImageMagick 7.1.2-34` (`import`, `magick compare`) |
| Game image | user-supplied GameCube disc, CISO container, ID `GZLE01`; passed in as `$GAME_NARRATOR_GAME_IMAGE_PATH`; SHA-256 `677726191ba8cc0829e9baa731c14eeff95ba399e1fe40a4f2473e7b8e0c80ac`; referenced read-only, never copied into the repo or artifacts |
| Dolphin user dir | `artifacts/spikes/dolphin/user` (isolated via `--user`; the real `~/.local/share/dolphin-emu` was never created or touched) |

## Capability matrix

| Capability | Result | Evidence (file + command) | Notes |
| --- | --- | --- | --- |
| Dolphin version / binaries | PASS | `artifacts/spikes/dolphin/versions.txt` (`pacman -Q dolphin-emu mesa vulkan-intel xorg-server-xvfb`), `help.txt` (`xvfb-run -a dolphin-emu --help`) | Qt GUI binary only; this package ships no headless `dolphin-tool`/`-nogui` |
| CLI flags present | PASS | `help.txt` | `--user`, `--exec`, `--batch`, `--movie`, `--save_state`, `--config=<System>.<Section>.<Key>`, `--video_backend`, `--nand_title`, `--debugger`, `--logger`, `--confirm`, `--audio_emulation` all listed. `--config`/`--video_backend` proven in use by `input.sh` / `boot.sh` runs |
| Xvfb GL renderer | PASS | `glxinfo.txt` (`DISPLAY=:91 glxinfo -B`) | `llvmpipe (LLVM 23.1.1, 256 bits)`, `direct rendering: Yes`, `Accelerated: no`, GL 4.6 core / GLES 3.2 — software rasterisation only |
| Vulkan under Xvfb | PARTIAL | `vulkaninfo-summary.txt` (`DISPLAY=:91 vulkaninfo --summary`) | Intel HD 630 (ANV) enumerates, Vulkan 1.4.354, **but**: `MESA: info: vulkan: No DRI3 support detected - required for presentation` — Xvfb exposes no DRI3, so a swapchain cannot present |
| Dolphin GUI renders under Xvfb | PASS | `gui-root.png`, `probe.sh` output, `probe-result.txt` (`magick gui-root.png -format '%[entropy]' info:` → 0.1693) | window `Dolphin 2609`, 800x600, entropy 0.169 ≫ 0.1 threshold |
| x11grab capture | PASS | `x11grab-01.png` … `x11grab-20.png`, `probe-result.txt` (`ffmpeg -f x11grab -video_size 1280x1024 -framerate 10 -i :91 -frames:v 20`) | 20/20 frames written, first frame entropy 0.169 |
| OGL video backend | PASS | `boot-OGL-*.png`, `boot-OGL-frames.tsv`, `BOOT PASS (OGL)` in the `boot.sh OGL` log | window title during the run: `Dolphin 2609 \| JIT64 SC \| OpenGL \| HLE \| …` |
| Vulkan video backend | FAIL | `boot-Vulkan.log` (`dolphin-emu --user=<isolated> --batch --exec=… --video_backend=Vulkan`), `vulkan-warning-dialog.png`, `vulkan-warning-text.png`, `boot-Vulkan-frames.tsv` | Dolphin comes up but pops a **"Warning"** dialog; log shows `MESA: info: vulkan: No DRI3 support detected - required for presentation`; every screenshot is identical (`vs_prev=0`, entropy 0.024 — the dialog over an unpresented frame). Matches the `vulkaninfo` finding: the ANV device exists, Xvfb cannot present |
| Game boots and renders a game frame | PASS | `boot-OGL-04.png` (entropy 0.277), `boot-OGL-frames.tsv`, `boot-OGL-first-frame-seconds.txt` (=17) | `dolphin-emu --user=<isolated> --batch --exec="$GAME_NARRATOR_GAME_IMAGE_PATH" --video_backend=OGL`; window title names the game; frames show cel-shaded sky/sea and the opening story scroll with legible English subtitles (visuals kept out of the repo). Log proof: `user/Logs/dolphin.log` → `Booting from disc: …/<image>.ciso`, `Active title: … (GZLE01)` |
| CISO container readable by Dolphin | PASS | same as above (`boot-OGL.log`, `user/Logs/dolphin.log`); `CISOFileReader`/`.ciso` in `/usr/bin/dolphin-emu` strings | `DiscIO::CISOFileReader` is compiled in; no conversion of the user image was needed |
| Time to first game frame | PASS | `boot-OGL-first-frame-seconds.txt` | 17 s wall clock (llvmpipe, 1x internal resolution, vsync off); in the 13:43 `input.sh` run the interactive title screen was already recognised at t=17 s |
| Emulation speed under llvmpipe | OBSERVED | `boot-OGL-frames.tsv`, `run-20261016-134333/input-OGL-timeline.tsv` | Not real time: the opening "legend" movie (~5 min of content) needs ~3.5-5 min of wall clock, i.e. roughly 0.8-1.0x. Sufficient for fixture authoring, not for latency claims |
| Game state machine on this host | OBSERVED | `run-20261016-134333/input-OGL-{seek-*,title-1,control-1-*,after-START-nofocus-*}.png`, `boot-OGL-{04..43}.png` | `boot logos → title screen (ZELDA logo + "PRESS START" + "© 2002, 2003 Nintendo" over a slow camera orbit of Outset Island, interactive from t≈15-20 s) → if left alone for ~50-90 s the opening legend movie starts BY ITSELF (letterboxed parchment scroll, ~3.5-5 min) → back to the title (attract loop). A real START press instead goes to the "create a save file in Slot A? Yes/No" memory-card prompt. "The screen stopped changing" is therefore NOT a title-screen signal, and the title camera keeps moving, so a single reference frame cannot classify it |
| Controller Pipes device opened by Dolphin | PASS | `pipe-fd.txt` (`ls -l /proc/$(pgrep -x dolphin-emu)/fd \| grep pipe1`) | fd 24 `lr-x------` → `user/Pipes/pipe1` while the game runs; `user/Config/GCPadNew.ini` is re-written by Dolphin on exit **unchanged** (`Device = Pipe/0/pipe1`, ``Buttons/Start = `Button START```), i.e. the device string and control names resolved |
| Controller **Pipes** input accepted in-game (scene change) | **PASS** | `run-20261016-134333/` — `input-OGL-report.txt`, `input-OGL-timeline.tsv`, `input-OGL-calibration.tsv`, `input-OGL-before.png`, `input-OGL-control-1-{01..15}.png`, `input-OGL-after-START-nofocus-{01..15}.png`, `input-OGL.log`; command `scripts/spikes/dolphin/input.sh 700` (exit 0) | With `[Input] BackgroundInput = True`, one `PRESS START` / hold 0.5 s / `RELEASE START` on `Pipe/0/pipe1`, with **no window focus and no xdotool**, moved the game off its title screen: the 15 no-input control samples 1 s apart stayed inside the title band (RMSE to the closest of 12 known title framings 0.09-0.25, letterbox band brightness 0.33-0.47) and 2-3 s after the press the frames left it (0.41 at the fade, then a settled 0.345 with a bright full frame) — the game showed the Wind Waker "Would you like to create a save file … in Slot A? Yes/No" memory-card prompt, which only a real START press can bring up. Looked at by eye: `input-OGL-before.png` = ZELDA logo + sailing ship + clouds + Outset Island cliff tops; `input-OGL-after.png` = near-black blue fade with faint clouds (letterbox bands 0.061); `input-OGL-control-1-15.png` = same title orbit, different camera angle; `input-OGL-after-START-nofocus-05.png` = the tan parchment save-file question over sky and clouds. The run reproduced on the next two attempts (`run-20261016-134555`, `run-20261016-134712`: control max 0.2466 / 0.2494, crossing 2-5 s after the press, `input.sh` exit 0). The old FAIL (no BackgroundInput) numbers survive in `input-OGL-report.txt` |
| `[Input] BackgroundInput = True` required for headless input | PASS (root cause) | `run-20261016-134333/input-OGL-backgroundinput.txt` (`13:BackgroundInput = True`), `...-backgroundinput-before.txt`; `grep -n BackgroundInput artifacts/spikes/dolphin/user/Config/Dolphin.ini` | Xvfb has no window manager, so the render window never receives X input focus, and Dolphin's `InputManager` does not poll **any** device — the Pipes FIFO included — while unfocused. Three earlier input attempts (12:50-13:26) measured nothing at all because of this. Fix: set `BackgroundInput = True` under `[Input]` in `<user>/Config/Dolphin.ini` before launch **and** pass `--config=Dolphin.Input.BackgroundInput=True`; the first attempt of the fixed script then passed. No `xdotool windowfocus`, no longer hold and no `A` fallback was needed (they remain coded fallbacks, in that order) |
| Pipes device discovery format | PASS | `input.sh` log (`[GCPad1] Device = Pipe/0/pipe1`), `user/Pipes/pipe1` (mkfifo), upstream `InputCommon/ControllerInterface/Pipes/Pipes.cpp`; symbols `ciface::Pipes::PipeDevice/PipeInput/InputBackend`, `Pipes/` and `PRESS`/`RELEASE` strings in `/usr/bin/dolphin-emu` | Any file in `<user>/Pipes/` becomes `Pipe/<i>/<filename>`; commands `PRESS|RELEASE {A,B,X,Y,Z,START,L,R,D_UP,D_DOWN,D_LEFT,D_RIGHT}`, `SET {L,R} [0,1]`, `SET {MAIN,C} [0,1] [0,1]`; mappings ``Buttons/Start = `Button START` `` |
| uinput virtual gamepad | BLOCKED | `ls -l /dev/uinput` → `crw------- 1 root root 10, 223`; `groups` → `david wheel`; `strings /usr/bin/dolphin-emu \| grep -i uinput` → no matches | Two independent blockers: (1) the device node is root-only and fixing that needs `sudo`/udev changes (out of scope, forbidden); (2) this Dolphin build has **no** uinput input source at all — upstream only ships evdev/SDL/Xlib/Pipes/DualShockUDPClient, so a virtual pad would have to be created by a separate privileged helper |
| Live-input gate per `docs/spec/12` | **MET** | as above; `input.sh` exits 0 only when the press crosses the scene-change threshold and the identical no-input window does not | Predicate is scene-level, not a pixel-count ratio: RMSE against known title framings + a letterbox-band test, with the threshold recomputed from the measured control. Mode B/C may now drive input; the DTM/savestate items below are still owed |
| XTEST / `xdotool` keyboard to the emulator | NOT USED | — | Deliberately not adopted: `docs/spec/11` says to avoid focus-dependent keyboard automation as the only option. Also note the default `GCPadNew.ini` maps GC buttons to keyboard keys, so keyboard injection would work but is not a clean programmatic channel |
| DTM (`--movie`) playback | NOT TESTED | — | Flag exists (`help.txt`). Recording a DTM needs a completed interactive session; this spike only proved live pipe input. Owed before Mode B is called deterministic |
| `--save_state` / savestate round trip | NOT TESTED | — | Flag exists (`help.txt`). Owed; `docs/spec/11` requires savestate/DTM desync detection |
| Audio output (for reference) | NOT TESTED | — | Out of scope here; the pipeline only needs frames + input from Dolphin. `--audio_emulation=HLE|LLE` accepted |

## Reproduce

```sh
export GAME_NARRATOR_GAME_IMAGE_PATH='/path/to/your/game.ciso'   # read-only, never copied
scripts/spikes/dolphin/probe.sh            # steps 2/3/5: Xvfb GL+Vulkan, GUI render, x11grab
scripts/spikes/dolphin/boot.sh OGL 240     # step 6b: game boot + first game frame
scripts/spikes/dolphin/boot.sh Vulkan 120  # step 4: Vulkan backend attempt
scripts/spikes/dolphin/input.sh 700        # step 6c: boot, title-screen control window,
                                           #          then a Pipes START press (exit 0 = accepted)
```

`input.sh` writes a fresh evidence directory per run
(`artifacts/spikes/dolphin/run-<timestamp>/`, gitignored — screenshots contain
copyrighted visuals) so that a rerun can never overwrite the reference frames it
classifies scenes with; the reference frames themselves live in
`artifacts/spikes/dolphin/` (override with
`GAME_NARRATOR_SPIKE_REF_DIR=/your/path`). It logs the calibration table, the
per-sample scene distances, `grep -n BackgroundInput` of the isolated
`user/Config/Dolphin.ini`, and exits non-zero unless the press crossed the
scene-change threshold while the 15 no-input samples 1 s apart did not.
`probe.sh` and `input.sh` pick a free display (`:91`–`:110`), always pass
`--user=$PWD/artifacts/spikes/dolphin/user`, clean up with a `trap`, and exit
non-zero when the measured entropy or the scene predicates fail.

## Method notes and traps found

* **A render window exists even in `--batch`.** Batch mode suppresses the
  Dolphin *main* UI; the framebuffer still presents into a 640x480 X window, which
  is what makes `x11grab` work headless. Confirms the warning in `docs/spec/11`
  that `--batch` alone is not proof of rendering — the entropy check is the proof.
* **Measure the render window, not the root window.** Cropping the whole
  1280x1024 root diluted every difference (the idle root is 78% of the pixels) and
  `magick compare -metric AE` prints a *float* in IM7 (`58463.7 (0.04…)`) and exits
  non-zero when images differ — both had to be handled in `lib.sh`.
* **Unfocused means deaf: `BackgroundInput = True` is mandatory headless.** Three
  input attempts measured nothing because Xvfb has no window manager, the render
  window never gets X focus, and Dolphin's `InputManager` skips polling *all*
  controller sources — including the controller Pipes FIFO — while unfocused.
  Set `[Input] BackgroundInput = True` in the isolated `Config/Dolphin.ini`
  **and** `--config=Dolphin.Input.BackgroundInput=True`; then a `PRESS START` /
  0.5 s / `RELEASE START` on the FIFO produced the scene change with no `xdotool`
  focus and no keyboard injection. A longer hold, the `A` button and
  `xdotool windowfocus` fallbacks are implemented in `input.sh` and were not
  needed (attempt 1 passed).
* **The title screen is a moving camera, so "the title screen" is a set of
  framings, not one frame.** RMSE against a single reference swings 0.00-0.40 as
  the orbit passes different angles of Outset Island; `input.sh` therefore keeps
  12 verified interactive-title framings as references (nearest-neighbour RMSE)
  and recomputes its scene-change threshold as the midpoint between what the
  *measured control window* actually reached and the title-vs-movie distance, so
  the orbit can never fake a crossing. Calibration recorded per run in
  `input-OGL-calibration.tsv`: title-vs-title max **0.1980** / title-vs-movie min
  **0.4260** for two known title frames and two known legend-movie frames
  (thresholds `on title` 0.228, `scene change` 0.338 for that episode).
* **Letterbox bands are the cheap, camera-proof scene test.** The title screen
  fills the 4:3 frame (mean brightness of the top/bottom 30-row bands 0.33-0.60),
  the opening legend movie is letterboxed (0.000). `input.sh` records both numbers
  per sample and needs two consecutive crossed samples, so a single transition
  frame cannot pass.
* **Attract mode is the confounder.** Left alone, the game starts the opening
  movie by itself ~50-90 s after the title appears, which is exactly the
  transition an input test looks for. Hence the ordering: seek the title, run the
  15×1 s no-input control *first*, then press, all inside one title visit — which
  forces a true 1 s sampling cadence (`cadence` sleeps only the remainder of each
  period; a naive `sleep 1` per sample plus measurement time blows the window and
  the episode gets discarded and retried, up to 3 visits).
* **Never let a rerun write over its own reference frames.** An intermediate
  revision of `input.sh` wrote `input-OGL-before.png`/`-after.png` and thereby
  destroyed two reference frames it was classifying against, which silently moved
  the calibration (0.166 → 0.197) and caused a false title detection during the
  pre-title fly-in. Per-run `run-<timestamp>/` evidence dirs fixed this; the 13:01
  FAIL run's numbers survive only in `input-OGL-report.txt`.
* **Natural animation defeats naive pixel deltas.** The title screen animates on its own (2.3% of pixels in a 4 s window), so an input test needs either a scene-level predicate (title → file select) or a metric insensitive to the animation, not a raw pixel-count ratio.
* **Static ≠ interactive.** The opening movie holds single frames with a true
  pixel delta of 0.000 for seconds, so "the screen stopped changing" is not a
  reliable "the game is waiting for input" signal; `input.sh` refuses to treat a
  screen as idle before the opening movie has had its wall-clock budget.
* Dolphin writes `Config/*.ini` only on a clean exit and `Logs/dolphin.log` stays
  empty unless `Logger.ini` turns groups on and `WriteToFile = True` — that file is
  the only place the emulator records the booted title, so the spike writes a
  `Logger.ini` into the isolated user dir (`enable_emulator_logging` in `lib.sh`).
* The GUI renders tofu boxes for its Chinese-localised labels (no CJK font
  installed). Cosmetic for this spike (entropy and the game window are what
  matter), but it means OCR-style tests must target the *game* framebuffer, not
  emulator chrome.

## What is still owed before Mode B/C can be called green

1. DTM record + playback and a desync assertion (`--movie`).
2. `--save_state` checkpoint round trip and validation by perceptual hash.
3. OCR on a captured dialogue frame (needs a textbox ROI + frame feed into the
   real `FrameSource`).
4. Accelerated rendering path: Xvfb gives llvmpipe only. Either accept
   sub-real-time E2E runs, or capture on a real/Xwayland display with DRI3, or an
   EGL/headless Vulkan path (`VK_EXT_headless_surface` is exported by the driver,
   but Dolphin does not present through it) — see ADR-0007.
5. Frame-rate headroom measurement for Mode C (real providers + TTS in parallel).
