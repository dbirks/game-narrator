# HDMI capture on the UNO Q

## Purpose

Bring up the physical capture chain on the UNO Q — GameCube (or a PC
running Dolphin) → HDMI adapter/upscaler → HDMI → UVC capture dongle
(RYBOZEN V316B) → powered USB-C hub → UNO Q — and verify it honestly
before the hardware smoke gate ([manual 13](13-uno-q-hardware-smoke.md))
runs against a real source. Everything hardware-dependent below is
research-based until the dongle is actually attached and re-verified, and
is marked **NOT VERIFIED** (AGENTS.md rule 12: no hardware acceptance is
claimed without the run).

## What the card is (research, 2026-10-19)

- The RYBOZEN V316B listing (Amazon `B097DKNS1M`, rybozen.us) says:
  USB 3.0, HDMI in + 4K HDMI loop-out, records up to 1080p60,
  plug-and-play (UVC webcam class), 3.5 mm mic input. The listing does
  not name the chipset — **which chip this specific unit carries is NOT
  VERIFIED**; the first device step (`lsusb`) settles it.
- Cards in this class are typically Macrosilicon **MS2130**. The MS2130
  datasheet (UltraSemi) states: UVC 1.0 video, UAC 1.0 audio (default
  48 kHz stereo), USB 3.0 device with USB 2.0 fallback, input up to
  3840×2160@30, USB output **YUV422 and MJPEG**, default maximum output
  1920×1080@60.
- MS2130-class units on Linux enumerate as `345f:2130 MACROSILICON
  USB3.0 UHD`, bind to `uvcvideo`, and expose **two** V4L2 nodes (capture
  + metadata) plus one UAC sound card (the HDMI audio path).
- Observed MS2130 mode list (Linux, `yavta`): YUYV and MJPEG at
  1920×1080 @ 60/50/30/20/10 fps plus 1280×720 and smaller.
  `v4l2-ctl --list-formats-ext` has been observed to **under-enumerate**
  the discrete modes (1080p missing from its list while `ffplay
  -list_formats` and a live capture show it) — always cross-check with
  `yavta --enum-formats` or `ffplay -list_formats`.
- YUYV 1080p60 is ≈ 250 MB/s of raw payload: it only works on a real
  USB 3 (SuperSpeed, 5000 Mb/s) link. On a USB 2 link 1080p60 YUYV
  drops frames heavily; the fallbacks are 720p YUYV or MJPEG.
- Colour: this card class has documented colour-fidelity complaints
  (shifted hues, washed-out whites on wide-gamut displays, 4:2:0
  sub-sampling in some configurations) and the driver's colourspace
  quantization differs per format (YUYV ≈ limited range, MJPG ≈ full
  range on one observed unit). Treat colour as a **calibration step, not
  an assumption** (see *Region and colour calibration*).
- HDCP: this class of capture card **cannot** capture HDCP-protected
  content. GameCube (analog-out converter or GCVideo digital adapter) and
  Dolphin are not HDCP-protected, so this is not a constraint here;
  modern consoles (PS4/5, Switch) will be blank on this chain.
- A firmware-flashing tool for MS2130-class USB capture exists
  (UltraSemi "USB firmware flash" + EDID helper) — only if a specific
  unit is defective, not part of first boot.

## Wiring

Per docs/spec/10 "Physical connection" (the appliance topology; the
UNO Q's own USB-C is the data uplink, **not** a video input):

```
 GameCube ──component/RCA──> RCA-to-HDMI converter ──HDMI──> powered HDMI 1×2 splitter
 (Dolphin PC ──HDMI── also works in place of the left half)
                                                                 │
                                        out 1 ──> TV (operator picture)
                                        out 2 ──HDMI──> RYBOZEN V316B  (HDMI IN)
                                                          │  HDMI loop-out ──> optional second display
                                                          │  3.5 mm mic ──> optional
                                                          │  USB 3 (blue/SS) cable ──>
                                                          ▼
                                   powered USB-C hub  (externally powered, PD uplink)
                                                          │  USB-C ──> UNO Q
                                                          └  USB-A ──> USB audio DAC + speaker
```

What to plug where:

1. GameCube composite/component RCA → the RCA-to-HDMI converter (or:
   Dolphin PC HDMI out → the splitter directly).
2. Converter HDMI out → splitter in; splitter `out 1` → TV, `out 2` →
   the V316B's HDMI **in**. The V316B's HDMI **loop-out** is optional
   (a second display); leave it unconnected if unused.
3. V316B USB → the hub's **USB 3** port (blue/SS). Use the supplied USB 3
   cable; a USB 2 port or a charge-only cable silently degrades the
   capture (see *Troubleshooting*, "frame drops").
4. USB audio DAC (+ speaker) → the hub's USB-A port.
5. Hub uplink (USB-C) → the UNO Q's USB-C port.

## Power and the hub

- The UNO Q's USB-C port is a **PD sink**: it does not power peripherals.
  The hub must be **externally powered** (its own PSU) and must supply
  PD on the uplink — the board boots from PD on that port. A 5 V-only
  hub uplink may not boot the board (which hub works is **NOT
  VERIFIED**; the Arduino 8-in-1 hub is the reference part).
- docs/spec/10 budget: "UNO Q 5 V / 3 A compliant power distribution
  through suitable USB-C PD hub/supply". Budget: dongle ≤ 0.9 A (USB 3
  bus-power ceiling; actual draw of the V316B is **NOT VERIFIED** —
  typically a few hundred mA for this class), DAC ≤ ~0.5 A. A 5 V / 3 A
  hub covers both with headroom; `dmesg` over-current/port errors mean
  the hub is underpowered.
- **Consequence of the switch:** while the UNO Q is plugged into the dev
  PC it is in *device* mode (adb-over-USB; no host bus — there is no
  capture chain and the preflight's `usb-host` row FAILs by design).
  Attach the powered hub and the port flips to *host* mode;
  adb-over-USB disappears, Wi-Fi ssh remains.
- Known kernel quirk (older OS images): when nothing is negotiated on
  the USB-C CC lines at boot (notably the VIN-power scenario), the
  `dwc3-qcom` controller defaults to *device* mode and the port never
  becomes a host (arduino/linux-qcom issue #2, still open; Arduino
  staff: "not a supported scenario" for VIN power). Images **without**
  `/etc/buildinfo` (e.g. kernel `6.16.0-geffa8626771a`, Aug 2025) are the
  affected ones; Arduino staff (forum 1412062, 2025-12-11) confirmed
  the **latest images start automatically in host mode**. On this
  appliance's image the community debugfs knob
  `/sys/kernel/debug/usb/4e00000.usb/mode` is **absent** (the knob exists
  on later images, e.g. `BUILD_ID=20251229-457`), so the community patch
  does not apply as-is. In the powered-hub topology the board is the PD
  sink and takes host role when the hub is attached — Arduino staff
  (forum 1442207, 2026-07-29): the force-host jumper is "only required
  if you were attempting to connect a USB device directly", not with a
  standard (powered) USB-C hub. The preflight's `usb-host` and `usb-role`
  rows are the live checks.

## Is my board affected? (dwc3 host-mode quirk)

Quick read-only pass — run it before relying on the capture chain. The
`usb-role` row of the preflight does this automatically:

```sh
ssh <user>@<uno-q-host> 'cat /etc/buildinfo 2>/dev/null; uname -r;
  ls -l /sys/class/usb_role/4e00000.usb-role-switch/ | grep -E "host|device";
  cat /sys/class/typec/port0/data_role /sys/class/typec/port0/power_role 2>/dev/null;
  lsusb 2>/dev/null | head'
```

| Check | Affected (old image, e.g. no `/etc/buildinfo`) | Not affected (newer image) |
| --- | --- | --- |
| `/etc/buildinfo` | missing | present (e.g. `BUILD_ID=20251111-426`) |
| `data_role` | — | **unreliable: always prints `host [device]`** (Arduino staff) — never use it as the verdict |
| current role | `device` symlink under `/sys/class/usb_role/4e00000.usb-role-switch/` while a powered hub is attached | `device` while on the dev PC (expected); `host` symlink + non-empty `lsusb` once the powered hub is attached |
| debugfs knob `/sys/kernel/debug/usb/4e00000.usb/mode` | absent on this appliance's kernel (absent on `6.16.7-g0dd6551ae96b` / `BUILD_ID=20251111-426`; **present** on later images such as `20251229-457`) | as left |
| host bus | no root hubs in `lsusb` | `1d6b:0002/0003` root hubs appear with the hub attached |

Residual risks even on a newer image (forum 1442207 / 1447175 / 1458555,
linux-qcom #2):

1. **Booting with a USB device already attached** to the board's port can
   leave it in device mode (Arduino uses this to decide App-Lab vs SBC
   mode). Boot the board first, then attach the powered hub.
2. A separate open bug can **unbind the xhci host controller at runtime**
   (`xhci-hcd…auto: remove` in `dmesg`, seen with a Media Carrier DSI
   display on `BUILD_ID=20260528-558`); the host bus drops and must be
   restored.
3. On VIN power (not our topology) the port never supplies VBUS and the
   community debugfs workaround is the only fix — and it needs the knob,
   which this kernel lacks.

**If the port does not come up in host mode with the powered hub
attached**, remediation in order of preference (apply with user
approval; none of this has been applied on this appliance):

1. **Official image update** with `arduino-flasher-cli` to the latest
   release (staff: latest images auto-boot in host mode; later images
   also ship the documented force-host mechanism,
   `/opt/arduino/scripts/volumedown.sh`). Risk: the flasher needs the
   board in device mode (adb/USB); Wi-Fi ssh remains after.
2. **Official force-host jumper**: bridge `VOL_DOWN`–`GND` on the JCTL
   header while powering on (docs.arduino.cc
   `/tutorials/uno-q/host-mode/`). Only needed if the port still refuses
   host role; does not change the fact that the port does not source VBUS.
3. **Community patch** (`Psalmustrack/arduino-uno-q-usb-fix`, systemd
   service writing `host` to the debugfs knob) — only on images where the
   knob exists; it does **not** apply to this kernel as-is.

Risk of the role switch itself: adb-over-USB disappears, Wi-Fi ssh
remains (see *Power and the hub*). Verify afterwards: `lsusb` shows the
hub's root hubs, `dmesg | grep -i xhci` is clean, and the preflight's
`usb-host`/`usb-role` rows report host.

## First-boot checks

From the dev host, one read-only pass over ssh (it prints a
PASS/WARN/FAIL table; it never writes to the device):

```sh
scripts/hw/capture-preflight.sh <user>@<uno-q-host>
```

Expected rows **today** (dongle not yet attached, device in device mode):
`usb-host` FAIL (no host bus), `usb-role` WARN (device mode expected on
the dev PC; image is newer than the quirk — see *Is my board affected?*),
`capture-node` FAIL (no capture device), `audio` WARN (no sound cards) —
everything else PASS. Expected after wiring: all PASS, or a WARN naming
the exact remaining gap.

Then the raw commands (ssh or the appliance console) and what each
should show:

```sh
lsusb
# hub + the dongle. MS2130-class: "345f:2130 MACROSILICON USB3.0 UHD".
# If the VID:PID differs, that is this unit's identity — record it in the
# run notes (the card is otherwise a generic UVC webcam-class device).

lsusb -t
# the dongle's port must be SuperSpeed (5000 Mb/s), not high speed (480).

v4l2-ctl --list-devices
# the card's product string (e.g. "USB3.0 UHD") with TWO nodes:
# the capture node and a metadata node. /dev/video0/1 on this board are
# the Qualcomm Venus codec nodes (H.264/HEVC/VP9) — never assume a number.

v4l2-ctl -d /dev/videoN --list-formats-ext
# "YUYV 4:2:2" (and MJPEG on MS2130-class). v4l2-ctl may under-enumerate
# discrete 1080p modes on these chips; cross-check:
yavta --enum-formats /dev/videoN
ffplay -hide_banner -f v4l2 -list_formats all /dev/videoN

cat /proc/asound/cards; pactl list sources short
# the card's UAC sound card (HDMI audio arrives here) + the USB DAC.

game-narrator doctor
# every component OK/DEGRADED/NOT CONFIGURED (manual 01); a FAIL row here
# is the doctor's own finding — read it before blaming the dongle.
```

## Choosing the capture mode

Target: **YUYV (yuyv422) at the highest mode the chain sustains** —
lossless, no software-decode cost on the A53 cores, and what
`TestHWCaptureDevice` expects (manual 13: the node must offer
mjpeg/yuyv422; a frame is grabbed through the app's FFmpeg path and
checked fresh, non-blank, ≤ 10 % dropped over 10 s).

| Link / source | Mode | Why |
| --- | --- | --- |
| USB 3 hub port + progressive source (Dolphin 720p/1080p, GCVideo 480p) | YUYV at the source's fps (≤ 60) | lossless; 250 MB/s needs the SuperSpeed link |
| USB 3 hub port + 1080i/interlaced converter output | YUYV 1080i60 if listed, else 1080p30/50 | match the source; combing is a *source* problem (troubleshooting) |
| USB 2 port (or degraded link) | 720p YUYV or MJPEG 1080p30 | 1080p60 YUYV will drop frames on 480 Mb/s |
| MJPG only (no YUYV at 1080p) | MJPEG 1080p30 | MS2109-class (USB 2) units expose MJPEG instead — the unit is then *not* the researched MS2130 class; re-check `lsusb` |

Rules:

- Pick the mode on a **real picture** (title screen), not a black screen:
  grab a few frames (`ffmpeg -f v4l2 -i /dev/videoN -frames:v 30
  /tmp/probe_%02d.png`) and confirm they are stable, non-blank and
  current.
- UVC format settings **persist across replug** on these cards (observed
  on MS2130) — a re-plugged dongle comes back in its last mode; set it
  deliberately after any replug.
- Pin the device for the app with `GAME_NARRATOR_CAPTURE_DEVICE` in
  `/etc/game-narrator/env` using the udev **by-id/by-path** name (manual
  13 "Device selection") — never a bare `/dev/videoN`, which is
  reassigned on replug; on this board the Venus nodes hold the low
  numbers.
- Latency: the chain's latency (converter + dongle + hub) is not
  published for these parts — measure it once with a real source (count
  a fast on-screen change) and record it in the run notes; it affects
  how promptly narration follows gameplay, not correctness.

## Region and colour calibration with a real picture

The GameCube's picture is 4:3 (320×240 native) pillarboxed in whatever
16:9 canvas the chain delivers (converter output, typically 720p/1080p
scaled; spec/10: "negotiate the actual converter's HDMI output as
provided"). OCR and the speaker region are calibrated on **captured**
frames, so do this once per (converter × card × game video mode)
combination, with the game in its title/dialogue screen:

1. Grab stills at the chosen mode (the `ffmpeg` command above), from a
   scene with a known textbox.
2. **Frame geometry:** note where the 4:3 picture sits inside the canvas
   (pillarbox width, any overscan the converter crops). The region
   config is relative to the captured frame — a converter that outputs
   1080i vs 720p vs with/without letterboxing moves the textbox
   coordinates.
3. **Colour range:** GCVideo-based adapters default to **full-range RGB**
   (configurable to limited RGB / 4:4:4 / 4:2:2 YCbCr); cheap
   converters usually emit limited-range. A range mismatch shows up as
   washed-out (double-limited) or crushed blacks. Check against a known
   image (a TV test card or the title screen's reference colours) and,
   if the card exposes V4L2 colour controls, align them; otherwise note
   the observed behaviour in the run record. Do **not** assume the
   card's "default" colourspace equals the source's — one observed unit
   reported limited range for YUYV and full range for MJPG.
4. **Interlace:** if the converter passes 480i/1080i through (interlaced
   output), moving objects show combing in stills. If the source offers
   a progressive mode (GCVideo 480p, Dolphin 720p/1080p) prefer it;
   interlaced capture is a documented fallback, not the target.
5. Record the outcome (mode, fps, geometry, range handling, latency
   estimate) in the run notes; the hardware smoke run
   (`make test-hardware` via `scripts/hw/run-on-device.sh`) is the
   formal verification and must show the capture check PASS with real
   frames (manual 13).

## Troubleshooting

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| No `/dev/video*` for the card (`lsusb` sees it) | not bound to `uvcvideo`; missing `v4l-utils` | `dmesg \| tail` for the card; `apt install v4l-utils`; replug. If `lsusb` does **not** see it: the hub port / cable / power (below) |
| `lsusb` sees the hub but no external devices, or the card | host bus down: board in device mode, or hub not powered / PD uplink not passing | power the hub; check the uplink is the PD-capable port; older-image `dwc3` device-mode quirk (see *Power and the hub*) |
| Frame drops / `lsusb -t` shows 480 Mb/s (high speed) for the card | USB 2 link: hub USB 2 port, charge-only cable, or hub internal downgrade | dongle into a blue/SS USB 3 hub port, USB 3 cable, powered hub; else capture 720p/MJPG (table above) |
| No signal / black picture on capture (TV shows the picture) | EDID mismatch: source outputs a mode the card's EDID doesn't accept; or an intermediate device | bypass intermediates one at a time — a batch of four MS2130-class units was black only when fed through an AV receiver (HyperHDR #869); set the source/converter to a plain progressive mode (480p/720p/1080p), replug the card (its mode/EDID state persists), try loop-out connected/disconnected |
| No signal on the TV **and** the capture | the converter or splitter output, not the card | TV directly on the converter; TV directly on the splitter; then the card on the working output |
| Washed-out colours / wrong range | limited-vs-full range mismatch between converter, card EDID and app | *Region and colour calibration* §3; compare with a reference image; record the observed behaviour |
| Interlace combing on moving objects | interlaced source mode (480i/1080i) | force a progressive source mode if the game/adapter supports it; otherwise accept the mode and document it (region config must match the captured frame) |
| Latency: narration lags the picture | chain latency (converter + dongle + USB) | measure once with a fast on-screen change; it is additive and constant per chain — record it; do not chase it below one frame by changing capture format |
| No audio from the card | UAC card not enumerated, or wrong source; mic-in port conflicts | `cat /proc/asound/cards` for the card's UAC entry; `pactl list sources short`; select it with `GAME_NARRATOR_AUDIO_DEVICE` (manual 13); if the 3.5 mm mic is plugged in and silent, unplug it and retest |
| `dmesg`: over-current / port reset errors | underpowered hub | larger powered hub (5 V / 3 A), move the DAC to another port |

## What NOT to do

- Do not report the capture path as verified from this manual alone —
  it is research. The evidence is the preflight table plus a
  `make test-hardware` run with the `TestHWCaptureDevice` PASS line
  (manual 13, AGENTS.md rule 3).
- Do not assume `/dev/video0` (or any number): on this board the Venus
  codec nodes occupy the low numbers and the card's nodes move on
  replug. Use by-id/by-path names.
- Do not capture MJPEG as if it were lossless (it is not), and do not
  assume the card is the researched MS2130 class until `lsusb` says so.
- Do not feed HDCP-protected content through this chain expecting a
  picture; do not power the hub from the UNO Q's USB-C port.
- Do not raise thresholds or change the card's UVC settings to make a
  flaky run green; record the measured mode list and drop rate instead.

## Sources

- RYBOZEN V316B listings/specs: Amazon `B097DKNS1M`; rybozen.us product
  page; instruction manual (manuals.plus `asin/B097DKNS1M`). Chipset,
  USB IDs and Linux behaviour of *this unit*: NOT VERIFIED.
- MS2130 datasheet summary (UVC 1.0, UAC 1.0, YUV422/MJPEG, 4K30 in /
  1080p60 out): UltraSemi, doc.ultrasemi.com/en/ic/macrosilicon/ms2130;
  video-capture selection overview (doc.ultrasemi.com/
  en/ic/macrosilicon/video-capture).
- Linux enumeration (`345f:2130 MACROSILICON USB3.0 UHD`, `uvcvideo`,
  two V4L2 nodes, UAC card, `v4l2-ctl` under-enumeration, 1080p60
  YUYV capture test): Cin mailing list "ms2130 HDMI-USB3 UHD capture
  card" (lists.cinelerra-gg.org, 2023-02); linux-hardware.org
  `usb:345f-2130`; usb-ids.gowdy.us 345f/2130; Raspberry Pi forum
  (forums.raspberrypi.com/t/355911).
- Full MS2130 mode list via `yavta`/`v4l2-ctl`, colourspace quantization
  differences, settings persisting across replug: Hackaday.io project
  "Streamo encodo cheapo", log "UVC and UAC".
- Colour-fidelity complaints (shifted hue, wide-gamut wash-out, 4:2:0):
  ivonblog.com/posts/ms2130-usb-video-capture-card.
- Black-screen case study (four MS2130 units, AV receiver in the chain):
  awawa-dev/HyperHDR discussion #869.
- HDCP behaviour of this card class (loop-out protection; no capture of
  protected content): ecency.com non-branded 4K capture review; general
  UVC-capture-card behaviour.
- GameCube output: GCVideo firmware README (github.com/ikorb/gcvideo —
  default full-range RGB, limited/4:4:4/4:2:2 options); Retro-Bit
  Prism manual + Nintendo Life review (480i/480p, 16-bit PCM 48 kHz
  over HDMI, bob deinterlace option); retrorgb.com/gamecubeoutput.html;
  docs/spec/10 (converter "e.g. 720p/1080p", HDCP note).
- UNO Q: official datasheet ABX00162 (docs.arduino.cc — "USB 3.1 with
  Role-Switching Capabilities over USB Connector", QRB2210 4× A53);
  arduino.cc UNO Q product page; Arduino forum
  "Connecting USB Camera to Uno Q" (forum.arduino.cc/1412062 — PD-sink
  port, powered-hub requirement, latest images auto-boot in host mode
  per Arduino staff); github.com/arduino/linux-qcom issue #2 (VIN-power
  host-mode bug, open, assigned; root cause and debugfs workaround);
  github.com/Psalmustrack/arduino-uno-q-usb-fix (community systemd fix +
  its issue #2: knob present but host mode still flaky on newer images);
  Arduino forum 1442207 (staff: force-host jumper "only required if you
  were attempting to connect a USB device directly" — not with a
  standard powered hub) and 1447175 (final word on VIN power, 2026-06),
  1458555 (xhci unbind at runtime on `BUILD_ID=20260528-558`);
  docs.arduino.cc `/tutorials/uno-q/host-mode/` (JCTL VOL_DOWN–GND
  jumper); arduino/arduino-deb-images `volumedown.sh` (image-shipped
  force-host mechanism). First-boot host mode on *this* appliance's
  image: NOT VERIFIED until the preflight's `usb-host`/`usb-role` rows
  pass with the hub attached.
- Appliance topology and power budget: docs/spec/10 ("Physical
  connection", "5 V / 3 A compliant power distribution").
