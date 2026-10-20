# ADR-0114: narrator audio over the UNO Q hub HDMI via an in-process ALSA-hardware sink (issue #5)

Date: 2026-10-20
Status: accepted
Owners: issue-5 worker
Related requirements/tests: issue #5 (narrator audio out of the UNO Q hub HDMI, ANX7625 bridge, from the system service), docs/spec/07 (audio sink abstraction, stable device identity), docs/spec/10 (reselect by stable ID on reboot; hardware preflight), docs/spec/13 (consented playback; diagnostics), docs/spec/16 (hardware smoke gate), ADR-0039 (sink/device env precedence), ADR-0074 (audio device selection and gain: the device list, the kinds, the tone evidence), ADR-0091 (Bluetooth output as the user-session radio — the contrast case), the proven board facts of 2026-10-20 (skills/uno-q-setup/references/audio.md)

## Context and constraints

The UNO Q's only HDMI output is the hub's: USB-C DisplayPort through the ANX7625 bridge chip into the hub's HDMI port (verified into a Pioneer VSX-823, whose ELD the sink reports). The ALSA path that audibly reaches the receiver is the **default** route — MultiMedia4 → SEC_MI2S_RX → ANX7625 → USB-C DP → hub HDMI — exposed as `hw:ArduinoImolaHPH,3`. The proven, audible config is `S16_LE / 48000 / 2ch / period_size 6240 / buffer_size 24960` (aplay's config; PipeWire's own negotiation — S24_LE + MMAP + 10 ms periods + tsched — is **silent** on the same node: the bridge only reconfigures audio at stream start, and the receiver's ELD lock dies when the stream suspends between narrations).

Two product facts shape the design:

1. **The narrator runs as the system user `game-narrator` (SupplementaryGroups=audio), with no user PipeWire session.** The existing `pipewire` sink shells out to `pw-play`, which needs exactly the user session the service does not have. The USB-DAC path (ADR-0074's pipewire sink, the operator's session default) is not the service's path on the appliance.
2. **The receiver stays locked only while the DMA is fed.** The bridge keeps the ELD alive while the stream runs; a stream that suspends between utterances loses the lock and the next narration is silent. The WirePlumber fix for the user-session path (`session.suspend-timeout-seconds = 0`) is the session's answer; the service needs the same property in-process: **one stream, opened once, left RUNNING between utterances, fed silence when idle.**

Constraints: stable identity only (`hw:<card name>,<device>`, never a numeric index — docs/spec/10's reboot rule); the exact hw_params must be negotiated by the app (a clamped kernel answer is a failure, not a fallback); the device must be addressable from the device list, the selection, the test tone and the session runtime with one spelling of the proven config; diagnostics must distinguish "no UNO Q card" from "card present but mis-routed", and a misroute must name the exact fix command; the hardware proof (a tone the DMA actually consumed) runs only under the `make test-hardware` gate with explicit consent.

## Decision

### 1. `internal/speech/alsahw` — the ALSA-hardware sink (in-process, pure Go)

A new `speech.AudioSink` that talks to the kernel PCM node directly — the ALSA UAPI ioctls (`SNDRV_PCM_IOCTL_*`) over `/dev/snd/pcmC<card>D<dev>p`, via `golang.org/x/sys/unix`; **no cgo, no alsa-lib, no subprocess** (the doctor's `aplay`/`amixer` calls stay subprocess-based read-only probes; the sink's data path does not fork).

The ABI layer (`abi.go`) mirrors `include/uapi/sound/asound.h`: the `hw_params_t`/`sw_params_t`/`pcm_status_t`/`xferi` layouts with compile-time size asserts (1504/136/152/24 bytes on 64-bit) so a kernel ABI drift fails the build, not the board. Data plane: RW-interleaved writes through `SNDRV_PCM_IOCTL_WRITEI_FRAMES` — the correct data-plane ioctl (there is no `SNDRV_PCM_IOCTL_RW` in the UAPI).

The contract the sink enforces:

- **Exact hw_params, verified.** `Setup` sends `HW_PARAMS` with integer (min == max) intervals for rate/channels/period_size/buffer_size and masks for access (RW_INTERLEAVED) and format (S16_LE), then reads the kernel's answer back and **fails on any clamped value** (`hwParamsDrift` names the moved parameter). A device that does not take the proven config is an error at open, not a silent stream later.
- **Stream open between utterances.** One writer goroutine owns the device for the sink's lifetime: the stream is opened on the first `Play` and left RUNNING; `Close` (idempotent, bounded) releases it. While idle, one period of silence is written every **keep-alive** interval (default 200 ms, `audio.DefaultKeepAlive` — well inside the 520 ms buffer the proven period config carries) so `hw_ptr` keeps advancing and the receiver's ELD lock holds. This is the in-process form of the WirePlumber `session.suspend-timeout-seconds = 0` rule.
- **In-process conversion.** TTS WAVs (24 kHz mono, 16- or 32-bit) are converted to the device's exact stream in the writer goroutine: linear-interpolation resample to 48 kHz, mono→stereo duplication (no matrixing to invent), S16_LE packing. No `plug` plugin, no kernel-side conversion — the stream the device sees is byte-for-byte the proven hw_params.
- **Failure policy.** A write the kernel rejects with EPIPE/EIO is resolved through `PCM_STATUS`: XRUN → DROP + PREPARE + START (counted, reported as `Underruns()`, the utterance retries its chunk); DISCONNECTED/SUSPENDED → terminal failure of the in-flight utterance. `Stop(fade)` cuts at frame granularity (a positive fade scales the delivered tail 1→0 over the fade, capped at the fade's frame count); the request context and `Close` end in-flight playbacks with terminal results.

### 2. The device profile (issue #5's deliverable: the "hub HDMI (ANX7625)" entry)

`internal/audio/profiles.go` pins the proven config as a **profile**: `{CardName: "ArduinoImolaHPH", DeviceNumber: 3, Label: "UNO Q hub HDMI (ANX7625)", Format S16_LE, 48000, 2ch, period 6240 × 4}`. The profile is the single source of truth for the stable identity (`hw:ArduinoImolaHPH,3`), the UI label, the kind and the sink params.

- **Device list.** The ALSA backend upgrades a row whose ID matches a profile: kind `hdmi`, name the profile's label, the proven rate/channels filled in (the aplay -L comment does not carry the evidence on this board — the profile does). The UI renders it unchanged: the Settings page already lays out rows by `kind`, so the "hdmi" row appears without a frontend change.
- **Selection and sinks.** A new sink kind `alsahw` joins `KnownSinkKinds` (env `GAME_NARRATOR_AUDIO_SINK=alsahw`, or the store). The session runtime and the tone endpoint resolve a profiled device ID to `alsahw.New` with the profile's exact params; **a device without a profile is refused by the alsahw sink** — inventing params is how the receiver goes silent — and the runtime degrades to the filesink evidence with a warning (the pipewire sink's established precedent). `cmd/game-narrator` needs no change: it already passes the selected device as the sink target.

### 3. The doctor row: `audio.hdmi_bridge` (read-only)

The new row probes the one silent-but-present failure mode of the hub HDMI: a board whose mixer got re-routed still enumerates the card and still looks healthy in the device list, but plays nothing. Read-only probes, in order: `aplay -l` for card presence (absent → **NOT CONFIGURED** — the row applies on the UNO Q only, and NOT CONFIGURED never degrades the report), then `amixer -c ArduinoImolaHPH get` for the two route switches:

- `SEC_MI2S_RX Audio Mixer MultiMedia4` **on** and `DISPLAY_PORT_RX Audio Mixer MultiMedia4` **off** → **OK** (the proven route).
- SEC switch **off** → **FAIL**, remedy the exact command: `amixer -c ArduinoImolaHPH set 'SEC_MI2S_RX Audio Mixer MultiMedia4' on` (and the DP switch kept off).
- DP switch **on** → **FAIL**: the DisplayPort route is a dead end (no DP backend dai-link in the qcm2290 device tree; arming it gives `q6asm_dai_prepare: ret:-22`), remedy the exact `off` command.
- Card present but the route cannot be read (no amixer, control missing on a revision, unparseable output) → **DEGRADED** with the manual `amixer … get` command.

The doctor reports, it does not apply: the remedy is the command the operator runs.

### 4. Tests

- **Fake-ALSA-writer suite** (`internal/speech/alsahw`, the `PCM` seam): the hw_params contract (the sink negotiates exactly S16_LE/48000/2ch/6240×4, setup precedes any write, non-proven configs are refused at construction); the keep-alive cadence (one silence period per interval, gaps bounded, silence is silence); the resample (a 440 Hz 24 kHz mono tone comes out as 48 kHz stereo whose 440 Hz Goertzel bin carries the energy and the neighbours do not, RMS at the rendered amplitude; the 32-bit stereo source shape too); gain, stop/fade, xrun recovery (EPIPE → restart, counted, utterance completes), a disconnected stream is terminal, `Close` ends in-flight handles and releases the device; and the ABI encoding of `hw_params` (the mask bits and integer intervals the kernel sees, plus the drift report for a clamped answer). Race-clean.
- **Doctor row** (fake amixer): NOT CONFIGURED off-board (with **no** amixer probe issued — the read-only contract), OK, both misroutes with the exact remedy commands, no-amixer and missing-control degradation, no-aplay blocked.
- **Hardware gate** (`internal/hwsmoke.TestHWALSAHubHDMI`, the `GN_GATE=test-hardware` row): on the board, with `GAME_NARRATOR_HW_SMOKE=1` and `GAME_NARRATOR_HW_AUDIO_CONSENT=1`, the sink plays a 1 s 440 Hz tone into `hw:ArduinoImolaHPH,3` at the proven hw_params and the gate asserts the DMA consumed it — `hw_ptr` (the `/proc/asound/cardN/pcm3p/sub0` reading) advances at least 0.25 s of the stream, and the kernel's own `hw_params` record carries `RW_INTERLEAVED / S16_LE / 2ch / 48000 / period_size 6240 / buffer_size 24960`. **NOT RUN** without the board, without the gate, or without consent — the board proof is never claimed from the dev host.

### 5. Documentation

`skills/uno-q-setup/references/audio.md` carries the proven facts (already written from the 2026-10-20 board session: the route, the mixer switches, the aplay reference, the hw_params comparison); this ADR adds the service-side form. `docs/manuals/19` gains the "Audio over the hub HDMI from the service" section: the selection (sink `alsahw`, device `hw:ArduinoImolaHPH,3`), the doctor row, the tone verification and the gate run.

## Tradeoffs considered

- **WirePlumber rule + pipewire sink (the proven user-session fix):** right for a user session; the narrator service has no user session, and a user-level rule does not exist for a system service's daemon. The same property (`suspend-timeout 0` semantics) is implemented in the sink's keep-alive instead, where it also survives upgrades and restarts without a per-user config file.
- **`aplay` subprocess (the board's proven command) as the sink:** the app's data path would own a child process per utterance, with no in-process stop/fade, no xrun accounting and no single-stream guarantee (two aplay children on one node is a race the kernel loses). The kernel ioctls give the same stream with the lifecycle the scheduler needs. The aplay reference stays the *test* reference (the gate compares the kernel's `hw_params` against aplay's proven line).
- **cgo against alsa-lib:** one dependency and one build toolchain to carry for what is four ioctls; the UAPI is stable and the compile-time size asserts make drift loud. Revisit with evidence if the ABI surface grows.
- **Letting the kernel's `plug` plugin resample:** the plugin negotiates the format the bridge then refuses (that is the PipeWire failure mode); the in-process converter keeps the negotiated stream byte-identical to the proven config.
- **A per-profile keep-alive value:** the default (200 ms) sits well inside the proven 520 ms buffer; a profile-specific value is one field away if a future bridge needs it.

## Validation

- `go test ./internal/speech/alsahw/` (+ `-race`): the fake-writer suite — hw_params contract, keep-alive cadence, resample (Goertzel/RMS), gain, stop, xrun, close, ABI encoding.
- `go test ./internal/doctor/`: the `audio.hdmi_bridge` row (fake amixer) plus the unchanged-row suite.
- `go test ./internal/audio/`: the profiled device row in the ALSA list, the `alsahw` sink kind in the selection vocabulary.
- `go test ./internal/hwsmoke/`: the gate's NOT RUN behaviour without the board/consent (the hardware PASS itself runs only under `make test-hardware` on the UNO Q with consent — reported NOT RUN until then, per docs/spec/16).
- Board acceptance (separate from the dev-host validation): `make test-hardware` on the UNO Q with `GAME_NARRATOR_HW_SMOKE=1 GAME_NARRATOR_HW_AUDIO_CONSENT=1` and a receiver on the hub HDMI — the gate's hw_ptr and hw_params evidence plus the user hearing the tone.

## Consequences and reversal

- New: `internal/speech/alsahw` (ABI, params, converter, kernel node, sink, tests), `internal/audio/profiles.go`, the `audio.hdmi_bridge` doctor row (+ test), the `TestHWALSAHubHDMI` gate, the `alsahw` sink kind in the selection/API/runtime vocabulary.
- The alsahw sink is addressable only through a profile; adding a device is adding a profile entry (the proven config measured on that device, the way issue #5's measurement produced the first one).
- Reversal: dropping the `alsahw` case from the runtime/API wiring and the kind from `KnownSinkKinds` removes the surface without touching the selection store's shape (a stored `alsahw` selection on an old build is refused by the validation the ADR-0074 store already performs); the doctor row and the profile file delete cleanly. The in-process UAPI work is the only non-reversible investment, and it is reused by any future ALSA data path (including a future capture direction).
