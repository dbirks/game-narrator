# UNO Q hardware smoke

## Purpose

Run the M8 hardware smoke on a UNO Q (or any host) and read its result
honestly: `make test-hardware` executes the docs/spec/10 "Hardware
discovery and preflight" as real checks (`internal/hwsmoke`, tests named
`TestHW*`). Every PASS measured something on this host; every NOT RUN is a
missing capability with the attach/configure instruction printed in place;
the gate never prints success without running (docs/spec/16 M8: "hardware
smoke and sustained play verified when actual hardware exists, otherwise
explicitly NOT VERIFIED"; AGENTS.md rule 12).

## Preconditions

- The appliance wired per docs/spec/10 "Physical connection":
  GameCube → RCA-to-HDMI converter → HDMI splitter → UVC capture dongle
  through the USB hub into the UNO Q, plus a USB audio DAC/speaker on the
  hub. A dev host without a dongle or a DAC is fine: the affected checks
  report NOT RUN with the attach instruction, the rest still run.
- Dependencies the preflight itself verifies: `v4l-utils` (v4l2-ctl),
  FFmpeg, Tesseract + the English pack, PipeWire/Pulse tooling
  (`pactl`, `pw-play`, `aplay`). Missing tools are named by the NOT RUN
  lines, with the package to install.
- The built binary is produced by the gate itself (it compiles
  `./cmd/game-narrator` into a temp dir for the 60 s serve-session budget
  run), so no prior `make build` is needed.

## Running it

On the UNO Q (Debian, arm64):

```sh
sudo -u game-narrator make test-hardware   # as the service user, or any user with /dev access
```

On any other host (dev box, single-node box) the same command runs the
checks the host can run and NOT RUN-explains the rest — including the
platform verdict, which only passes on Debian arm64. Exit codes: `0`
everything ran and passed, `1` a check failed (read the FAIL line, fix,
rerun), `2` nothing failed but at least one check was NOT RUN (the setup
instructions are printed by the gate summariser). `artifacts/gates/
test-hardware.{rc,xml,json}` keep the machine-readable result.

## What each check proves

| Check | Proves | NOT RUN means |
| --- | --- | --- |
| `TestHWCaptureDevice` | The V4L2 enumeration finds a capture node (stable by-id/by-path names recorded, never an assumed `/dev/video0`); formats/sizes/fps are recorded; a frame grabbed through the app's supervised FFmpeg path (`internal/capture/ffmpegpipe`) is fresh (≤ 2 s old on the source's own monotonic clock), non-blank (luma entropy ≥ 1.0 bits, ≥ 8 distinct luma values), and the dropped-frame rate over 10 s is ≤ 10 %. | No capture node attached; v4l2-ctl missing; the node has no mjpeg/yuyv422 mode; `GAME_NARRATOR_CAPTURE_DEVICE` set to a name the host does not have. |
| `TestHWAudioOutput` | PipeWire sinks (and ALSA cards) are enumerated with their sample rate, and — only with consent (below) — a 1 s 440 Hz tone is played through the app's PipeWire sink (`internal/speech/pipewire`, `pw-play`) and the observed playback length is within [500 ms, 2 s]. | No sinks/cards found; no consent (see below); `pw-play` missing; the named sink does not exist. |
| `TestHWOCRControlledImage` | The real Tesseract engine path (`internal/vision/ocr`, the production subprocess wrapper with its TSV parsing and timeout bounds) reads the controlled textbox image (`NARRATOR 42`, synthetic, copyright-free) with character error rate < 5 % — i.e. the OCR dependency stack on this host is healthy before the device ships. | Tesseract binary or `eng` pack missing. |
| `TestHWResourceBudget` | RAM/CPU/temperature (`/sys/class/thermal` where present) and data-dir writability + free space (≥ 512 MiB) preflight; then the built `serve` binary runs a 60 s session (temp data dir, replay roots at the repo fixtures, free port) while its RSS is sampled from `/proc` every 2 s. On the UNO Q profile (arm64, < 5 GB RAM) the peak RSS must stay under the documented budget. | — (runs on every host). On non-UNO-Q hosts the *budget assertion* is NOT RUN-explained ("not the UNO Q profile") and the measured peak is logged as evidence; `/proc` unreadable also NOT RUN-explains the session. |
| `TestHWPlatform` | Kernel, distro and arch are reported; the "is this the UNO Q target" verdict passes only on Debian arm64. | This host is not Debian arm64 (the measured identity is in the NOT RUN text). |

## Consent for audible playback

`TestHWAudioOutput` enumerates sinks silently and always (read-only, like
`doctor`), but plays the 440 Hz tone **only** when

```sh
export GAME_NARRATOR_HW_AUDIO_CONSENT=1
```

is set for the run. docs/spec/13 makes playback a consented action: a gate
that hoots down a living room on every CI run is not a diagnostic. Without
the variable the check is NOT RUN and prints the exact export instruction
and the sink that would play. On a shared room, consent the run deliberately
(it plays ~1 s at the app's default volume through the sink named by
`GAME_NARRATOR_AUDIO_DEVICE`, or the first enumerated sink when unset).

## Resource budget

The UNO Q profile — arm64 with strictly less than 5 GB RAM (the appliance
ships 4 GB; docs/spec/10) — asserts the **serve process peak RSS over the
60 s session is ≤ 512 MiB**. The bound covers the whole process: HTTP
app, frame previews, the OCR worker and the speech cache — sized so the
rest of the 4 GB stays free for the OS and the GameCube-side USB stack
(docs/spec/10 "UNO Q 4 GB must remain stable …"). This is a smoke bound:
the sustained-play verification (long session, thermal behaviour,
drop-bound under load) is the separate M8 item tracked in
[docs/status/blocked.md](../status/blocked.md) and is still NOT VERIFIED
until it runs on the appliance. A budget FAIL on the appliance means
recheck the measured peak line, confirm the session actually ran (the
sample log is in the test output), then treat it as a real regression —
do not raise the bound to make the gate green.

## Device selection

| Variable | Meaning |
| --- | --- |
| `GAME_NARRATOR_CAPTURE_DEVICE` | Capture node to smoke: exact `/dev/video*` path, or the udev stable by-id/by-path symlink (by full path or base name). Unset: first capture-capable node. Set to a name the host does not have → NOT RUN with the found list. |
| `GAME_NARRATOR_AUDIO_DEVICE` | The app's named PipeWire sink (same name the serve process uses in pipewire mode). Unset: first enumerated sink. Set to a missing sink → hard FAIL with the found list — a silent fall-through to a different speaker is worse than an error. |
| `GAME_NARRATOR_HW_AUDIO_CONSENT` | `1` = consent the audible tone; anything else = NOT RUN. |

## What NOT to do

- Do not report `make test-hardware` as hardware-verified on a host that
  printed NOT RUN lines for the capture or audio checks — say which checks
  ran, which were NOT RUN, and what is needed to run them (docs/spec/16:
  unavailable hardware is `NOT RUN` with actionable setup, never green).
- Do not set `GAME_NARRATOR_HW_AUDIO_CONSENT=1` in a shared environment to
  "get the gate to 0".
- Do not edit the thresholds in `internal/hwsmoke` to match a flaky
  dongle; the measured values are logged with every run exactly so a
  threshold review has evidence.
- Do not run this against a data dir other than the app's resolved one for
  the preflight part — the budget session always uses a temp dir and never
  touches the operator's state.
