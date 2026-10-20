# HDMI capture on the UNO Q

Everything here is measured and documented in
[docs/manuals/18-hdmi-capture-on-uno-q.md](../../../docs/manuals/18-hdmi-capture-on-uno-q.md)
(sections "Wiring", "Power and the hub", "Is my board affected?",
"First-boot checks", "Choosing the capture mode", "Real feed
measurements", "Region and colour calibration"). This file is the agent
view: what to check, in what order, and what each failure means.

## The physical chain

GameCube → RCA-to-HDMI converter → **powered** HDMI splitter → UVC HDMI
capture dongle (USB 3 port of a **powered** USB-C hub) → UNO Q. A USB
audio DAC/speaker sits on the same hub. Power budget and hub choice are
in manual 18 "Power and the hub". If the board is not on a powered
hub, the capture card will enumerate unreliably or not at all.

## The dwc3 host-mode quirk ("Is my board affected?")

On some images the board boots in USB *device* mode and has no host
bus at all, so nothing can enumerate on the board's own port.
Manual 18 "Is my board affected?" has the quick read-only pass:

```sh
ssh <user>@<uno-q-host> 'cat /etc/buildinfo 2>/dev/null; uname -r;
  ls -l /sys/class/usb_role/4e00000.usb-role-switch/ | grep -E "host|device";
  cat /sys/class/typec/port0/data_role /sys/class/typec/port0/power_role 2>/dev/null;
  lsusb 2>/dev/null | head'
```

The preflight `usb-role` row automates this; affected boards (old
image, no `/etc/buildinfo`) need the host-mode workaround documented
there before any capture work is meaningful.

## First-boot checks (read-only)

```sh
scripts/hw/capture-preflight.sh <user>@<uno-q-host>
```

Expected rows **before** the dongle is attached: `usb-host` FAIL,
`usb-role` WARN, `capture-node` FAIL, `audio` WARN, everything else
PASS. **After** wiring: all PASS, or a WARN naming the exact remaining
gap. The raw commands behind each row and what each should show are in
manual 18 "First-boot checks".

## Choosing the mode

Target **YUYV (yuyv422) at the highest mode the chain sustains** —
lossless, no software decode on the A53 cores, and what the hardware
smoke test expects (the node must offer mjpeg/yuyv422). The decision
table (link speed × source fps → mode) and the rules are in manual 18
"Choosing the capture mode", including:

- Pick the mode on a **real picture** (title screen), never a black
  screen; a black/blank still is a source problem (converter, splitter,
  HDCP), not a software bug.
- UVC format settings **persist across replug** on MS2130-class cards —
  set the mode deliberately after any replug.
- Pin the device for the app with `GAME_NARRATOR_CAPTURE_DEVICE` in
  `/etc/game-narrator/env` using the udev **by-id/by-path** name —
  never a bare `/dev/videoN`, which is reassigned on replug (on this
  board the Venus codec nodes hold the low numbers).

## Measured rates (RYBOZEN V316B, MacroSilicon `345f:2131`)

- 1080p MJPEG ≈ **49 fps** decoded; 720p YUYV ≈ **59 fps** (manual 18
  "Real feed measurements" — full table, including the "MJPEG declared
  but raw YUY2 delivered" gotcha and the ffmpeg `-vf fps/-t` → 0 frames
  gotcha on these chips).
- Cross-check formats with `yavta --enum-formats` and
  `ffplay -hide_banner -f v4l2 -list_formats all`: `v4l2-ctl` can
  under-enumerate discrete 1080p modes on these chips.

## Still grab + OCR smoke

Grab stills with the repo-documented command (from the board or over
ssh), from a scene with a known textbox:

```sh
ffmpeg -f v4l2 -i /dev/videoN -frames:v 30 /tmp/probe_%02d.png
```

Confirm the stills are stable, non-blank and current. Then run the
product's own pipeline over the still instead of raw `tesseract` (raw
tesseract proves only that the hardware works, not the pipeline). The
one-shot command is in the current tree (issue #1) — use it when the
board's installed build ships it (`game-narrator --help`):

```sh
game-narrator ocr --image /tmp/probe_01.png                 # [--json], [--save-debug DIR], [--top N]
game-narrator ocr --device <udev by-id/by-path>             # one live frame (works while serve is stopped)
game-narrator ocr --image /tmp/probe_01.png --game <profile-id>   # matcher top-k vs a published profile
```

Until the board's build has it, the fallback evidence is the ffmpeg
stills plus `game-narrator doctor` — say exactly which you ran (no
simulation theater, AGENTS.md rule 3). Frames are **user content**:
keep them local (`/tmp`), never in the repo, support bundles, or issues;
`--save-debug` crops stay local for the same reason.

## Then

Region and colour calibration (textbox geometry, colour quantization)
per (converter × card × video-mode) combination is manual 18 "Region
and colour calibration"; the hardware smoke gate is
`scripts/hw/run-on-device.sh <user>@<uno-q-host>` (see SKILL.md step 8).
