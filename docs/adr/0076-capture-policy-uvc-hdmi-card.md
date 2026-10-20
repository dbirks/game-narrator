# ADR-0076: Capture policy for the UVC HDMI card — mode selection, USB link, low-latency MJPEG, active area, no-signal handling, hotplug

Date: 2026-10-19
Status: accepted
Owners: agent worker (capture-hardening)
Related requirements/tests: docs/spec/10 (hardware discovery and preflight: enumerate modes, negotiate, record negotiated link, reselect by stable ID), docs/spec/05 (analysis at 5–10 Hz, latency budget), docs/spec/08 (session events), docs/spec/12 (test strategy: synthetic/captured/emulator/provider/hardware distinction; failure injection), docs/spec/13 (diagnostics page and CLI: operator action on every fault; reconnect with backoff), AGENTS.md rules 2/3/12. Tests: `internal/capture/mode` (pure policy, synthetic MS2130/MS2109 fixtures), `internal/capture/usb` (fake sysfs tree), `internal/capture/ffmpegpipe` (argv assertions + real-FFmpeg smoke), `internal/capture/signal` (synthetic frame patterns), `internal/capture/devices` (fake v4l2-ctl layer), `internal/runtime/signal_test.go` (v4l2 session, fake source), `internal/vision/roi_test.go`, `internal/doctor` (fixture host). Evidence: `artifacts/capture-hardening-20261019/mjpeg-bench-unoq-20261019T2318Z.txt`.

## Context and constraints

The appliance (UNO Q, 4× Cortex-A53, Debian 13, FFmpeg 7.1) will ingest a GameCube screen through a RYBOZEN V316B USB HDMI capture card (Macrosilicon MS2130/MS2109 class) into the live pipeline (docs/spec/10). The dongle is not on the bench yet, so nothing here may assume its modes: docs/spec/10 says the preflight must enumerate the real card's `--list-formats-ext` and never assume. Everything in this ADR is therefore proven on (a) synthetic FFmpeg sources, (b) committed text fixtures of the documented MS2130/MS2109 format set, explicitly labelled synthetic in the files, and (c) one real measurement on the UNO Q itself over SSH (the MJPEG decode budget).

Constraints: the pipeline analyses at 5–10 Hz (docs/spec/05), so the card need not — and on a shared USB link must not — stream 60 fps; the same four cores run OCR and speech, so the capture mode is a CPU budget, not just a link budget; a USB 3 card plugged into a USB 2 port is a real, silent fault (it works, at a capped speed); the HDMI input loses signal when the console powers off or changes mode, and the runtime must neither commit phantom lines nor flap the session.

## Decision

### 1. Mode selection is one pure function; the doctor reports the choice

`internal/capture/mode.Select(formats, policy)` picks the capture mode from the card's enumerated formats and the target policy. Criteria, in priority order: (1) the picture must carry the game's native canvas (640×480 GameCube default; a 320×240 card cannot show it no matter how fast it streams); (2) the rate is the *smallest offered* rate at or above the analysis rate (10 Hz default) — this is the pre-decode decimation: the card delivers at the analysis rate instead of 60 frames the pipeline would drop; (3) the format follows the USB link — raw YUYV 1080p60 is ~250 MB/s and only a SuperSpeed link carries it, so a HighSpeed-negotiated card gets MJPEG, which is the compressed form that fits; (4) the mode must fit the MJPEG decode budget (below). The zero-value `Policy` is the documented preflight policy, so the doctor and the runtime cannot drift apart.

The doctor's `capture.devices` check (internal/doctor) runs the policy per capable node with the real enumeration and the real link, and reports in its detail: the chosen mode, the reason string, the rejected candidate count, and the negotiated link class. A node whose formats cannot be read, or for which no mode fits the policy, is DEGRADED with the fault named.

### 2. The USB link is read from sysfs, and a downgraded card is DEGRADED

`internal/capture/usb.LinkSpeedFor` resolves the node's sysfs class entry to its USB device directory and reads the negotiated `speed` and the port ceiling `maxportspeed` (read-only, no privileges). A SuperSpeed-class card negotiating 480 Mb/s (`Downgraded()`) is the classic "plugged into a USB 2 hub port" fault: the doctor answers DEGRADED with the operator action (re-seat into a USB 3 port, drop the USB 2 hub), because the card works but its raw modes do not.

### 3. Low-latency MJPEG input, pre-decode decimation, and a measured decode budget

`ffmpegpipe`'s live-card defaults (set by the v4l2 source constructor): `-fflags nobuffer -flags low_delay` and `-thread_queue_size 2` — a demuxer that buffers and a decoder that queues both add delay to a live picture, and the supervision (stall watchdog, single-slot queue) is sized for frames that arrive when the card sends them. The policy's rate choice (item 1, criterion 2) is the decimation: the card streams 10 fps, so the decoder decodes 10 fps.

The MJPEG decode budget is **measured on the UNO Q**, not assumed. Evidence (`artifacts/capture-hardening-20261019/mjpeg-bench-unoq-20261019T2318Z.txt`, FFmpeg 7.1.5, 4 cores, synthetic testsrc2 MJPEG encoded once and decoded to null):

| input | sustained decode | pixels/second | CPU at mid-run |
|---|---|---|---|
| 1280×720@60 MJPEG | 140 fps | ≈ 129 Mpps | ≈ 104 % (one core) |
| 1920×1080@30 MJPEG | 66 fps | ≈ 137 Mpps | ≈ 102 % (one core) |

Sustained ceiling ≈ 130 Mpps at roughly one core of the four. `mode.DefaultMJPEGBudgetPPS` is set to **8 Mpps**, far below the ceiling, deliberately: the same four cores also run OCR and the speech pipeline, and real game content is harder to decode than testsrc2. At 8 Mpps the 480p-class picture at the 10 Hz analysis rate (3.1 Mpps) fits with wide headroom; 720p@10 (9.2 Mpps) and 1080p@10 (20.7 Mpps) are refused unless the operator sets a higher `DecodeBudgetPPS` from a measured run on the target. Raising the budget without a benchmark is how a mode gets requested that the A53 cannot hold. (A pre-existing comment in `mode.go` cited this measurement as "≈ 12 Mpps"; the arithmetic was off a factor of ten — the measured ceiling is ≈ 130 Mpps. The 8 Mpps budget is unchanged and remains ~6 % of the measured ceiling.)

### 4. Active picture area, deinterlace, colour range

- **Active area**: `internal/vision`'s ROI layer scans wide frames for black bars (pillarbox/letterbox) and crops to the active area. The scan is refused on 4:3 frames (4:3 content in a 4:3 frame rarely carries bars, and a scan risks eating a legitimately dark scene edge), a line counts as bar only when ≥ 98 % of it is below the luma floor (16, the bottom of legal video range), and the trim is capped so a frame that is mostly black is not "cropped" into a sliver.
- **Deinterlace**: optional `bwdif` (default for 480i converters — it deinterlaces and drops, halving the rate to the progressive rate) or `yadif=0:-1:0` (auto parity), applied after decode; a misspelled filter is a counted error, never a silently dropped argv.
- **Colour range**: `setparams=range=tv|full` declares the range into the filtergraph, so the YUV→RGB conversion normalises it; an unrecognised value is an error at argv assembly.

### 5. No-signal / signal-change detection, paused matching, auto-recovery

`internal/capture/signal` classifies frames (blue screen, "no signal" text patterns, near-misses) and gates state changes: a loss is declared only after a consecutive-bad gate, a recovery only after a consecutive-good one, and a size change is treated as a loss-then-recover unless a single-glitch window proves otherwise. On the live v4l2 input the runtime wraps the source (`internal/runtime/signal.go`): while the monitor reports loss the frames are **held back from the pipeline** — no frame reaches the recognition stage, so no line can commit — and `capture.signal` events are published on the session's chain (lost / recovered with the reason). Matching and the commit path are untouched, so a held window can never commit a phantom line; the fixture sources pass through unwrapped (their ID is not `v4l2:`). Auto-recovery is the monitor's good-gate: the same card, the same source, the same session — the session is never restarted for a signal blip.

### 6. Hotplug: stable identity, reselection, backoff

`internal/capture/devices.Watcher` polls the v4l2-ctl enumeration (the fake device layer in tests is the same seam) and reports transitions keyed by the udev by-id name, not the node path: a card back at a different node is `EventReselected` (the stable ID matches a bounded set of recent removals — a tombstone set capped at eight, so a flapping port cannot grow it), a stranger is `EventAdded`, an unplug is `EventRemoved`. An enumeration fault is **not** an unplug: the known set is kept and the failure surfaces as the poll's error. The reconnect side is the ffmpegpipe child's existing restart loop: exponential backoff capped at 10 s, permanent errors (no such device, permission) classified so the state names what backoff cannot fix.

## Options considered

1. **udev inotify for hotplug** — rejected: a udev client dependency (or raw netlink parsing) for a transition that matters once per session; v4l2-ctl polling at a two-second interval is one subprocess the doctor already runs, and the watcher's contract (transitions, not immediacy) is what the consumer needs.
2. **Ask the card for its "best mode" and take it** — rejected: docs/spec/10 forbids assuming; a card that advertises 1080p60 MJPEG on a 4-core A53 box that must also run OCR is a flapping pipeline by the time it is noticed.
3. **A fixed 1080p MJPEG mode for all cards** — rejected: it fails the link budget on USB 2, the decode budget on the A53, and the canvas criterion when the card is a 720p-class device; one policy function with four criteria beats one number.
4. **Drop frames in the pipeline instead of decimating at the card** — rejected: the decoder would still decode 60 fps to throw away 50; the decimation must happen where the card's rate is negotiable.
5. **Restart the session on signal loss** — rejected: a power-off of the console must not kill the operator's session; holding the frames and reporting the transition is the docs/spec/13 behaviour, and the recovery gate makes the restart the rare case (a card that dies, item 6).

## Tradeoffs

- **The 8 Mpps budget refuses 720p-class MJPEG on the UNO Q** even though the measured ceiling (≈ 130 Mpps) would allow it. That is the point: OCR and speech share the cores, and the budget is the part of the A53 budget the capture is allowed to spend. A mode that fits only at the ceiling is a mode that flaps under load.
- **Synthetic fixtures until the dongle lands**: the MS2130/MS2109 fixtures are labelled synthetic and the doctor's first real run on the appliance supersedes them. No claim in this ADR is a claim about the V316B's actual enumeration; that is the docs/spec/10 preflight's job, and the gap audit tracks it.
- **The polling watcher misses a plug-and-unplug inside one interval** — an acceptable loss for the fault class (a card yanked mid-session), and the enumeration-fault-is-not-an-unplug rule prevents the worse failure (a wedged v4l2-ctl read as a hotplug storm).
- **The blue-screen classifier is a luma/text heuristic, not a signal probe**: a game that displays a full-blue screen for long enough is, to the monitor, no signal. The gate (consecutive frames) bounds the damage to a held window, and the `capture.signal` event names the reason so the developer UI can show it.

## Validation

- `go test ./internal/capture/... ./internal/runtime/ ./internal/doctor/ ./internal/vision/` — policy table on the synthetic fixtures, fake-sysfs link reads (including the 480 Mb/s SuperSpeed-downgrade case), argv assertions plus real-FFmpeg smoke for the low-latency flags, synthetic-frame monitor tests (loss gate, recovery gate, frozen window, size change, glitch), the fake-device hotplug transitions (added / removed / reselected with the previous node named, enumeration fault not an unplug), the v4l2 session hold-and-recover test with a fake source, the ROI tests, and the doctor's fixture-host checks (mode + reason reported; downgraded link DEGRADED with the re-seat remedy; unreadable formats DEGRADED; unreadable link still selects conservatively).
- MJPEG decode budget: `sh /tmp/gn_bench.sh` on the UNO Q over SSH (<user>@<uno-q-host>, FFmpeg 7.1.5, 4 cores) — raw output at `artifacts/capture-hardening-20261019/mjpeg-bench-unoq-20261019T2318Z.txt`; the arithmetic (fps × pixels-per-frame → Mpps; `ps -o pcpu` mid-run) is shown in the evidence file.
- `go test -race` on the concurrency-heavy packages (`internal/capture/devices`, `internal/capture/signal`, `internal/runtime` signal tests) — clean.
- **NOT RUN (hardware absent)**: the real RYBOZEN V316B (its `--list-formats-ext`, its negotiated link, its blue screen); real HDMI signal loss and mode change; Dolphin live E2E through the card. The gap audit carries these as the real-hardware validation work; this ADR does not claim them.

## Consequences

- The doctor's capture check is now a preflight that answers "which mode will the runtime ask for, and why" before the first session, and names the two silent faults (USB 2 port, unreadable formats) with the operator action.
- The runtime can run a v4l2 session across a console power-off: the session holds, the events say so, and nothing commits in the held window.
- The decode budget is a number with a measurement behind it, and the one place it may be raised (a re-measured target run) is named.

## Rollback / migration path

Revert the ADR-0076 changes in reverse: the runtime signal wrapper (`src = m.newSignalSource(src, id)` in `session.go`), the doctor's mode-selection block, the `internal/capture/{mode,usb,signal,devices}` packages (all additive; `ffmpegpipe`'s low-latency defaults are the only change to an existing path — remove the `LowLatency`/`ThreadQueueSize` defaults from the v4l2 constructor to restore the pre-ADR argv). No persistence or protocol change is involved; the `capture.signal` event is additive on the session's chain.
