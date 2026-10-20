# ADR-0078: Capture product wiring — v4l2 session sources, the device API, throttled preview and the capture probe

Date: 2026-10-19
Status: accepted
Owners: agent worker (gap audit G1+G2)
Related requirements/tests: docs/spec/00 FR-01 (device enumeration and selection; previews, aspect ratio, signal-mode stats, screenshot and capture test), docs/spec/08 (GET /api/v1/capture/devices, POST /api/v1/capture/probe, GET /api/v1/capture/preview; the DEVICE_UNAVAILABLE error), docs/spec/10 (never assume /dev/video0; record fps/pixel format; capture test with OCR on the user-selected region; last-frame freshness and dropped-frame rate; rate-limit preview streaming), docs/spec/12 (synthetic vs hardware evidence classes), docs/spec/13 (operator action on every fault), ADR-0076 (capture policy, no-signal, hotplug — the machinery this ADR wires into the product), AGENTS.md rules 2/3/9.

## Context and constraints

ADR-0076 built the capture machinery (mode policy, USB link, low-latency MJPEG, no-signal hold, hotplug watcher, ffmpegpipe supervision) but the product surface was still a stub: `POST /api/v1/sessions` with `frame_source.kind=v4l2` answered 501 (`buildFrameSource`), so no session could run from the HDMI dongle, and the device had no product surface — no device list, no persisted selection, no preview, no capture test. docs/spec/08 already names the routes (devices/probe/preview) and the scope table (`internal/auth/policy.go`) already grants `/api/v1/capture/{rest...}` to operator; the matrix test (`internal/api/authz_test.go`) already probes `GET /api/v1/capture/devices` and `GET /api/v1/capture/preview` at operator — the routes were promised, not built. The dongle is still not on the bench, so nothing here may assume its modes; validation is synthetic (labelled) plus the NOT RUN hardware path.

Constraints: the client must never be able to name an arbitrary device path (a session request that opened `/dev/whatever` is the replay-allowlist footgun repeated on the capture bus); the UVC device is a single physical consumer, so a preview and a running session must not grab it simultaneously; previews are a CPU/decode budget on the same four A53 cores as OCR and speech, so they are throttled (docs/spec/08 "Rate-limit … preview streaming"); and the fake-device test seam must be labelled synthetic in every answer, the same rule ADR-0074 applies to the fake audio list.

## Decision

### 1. One shared selection layer: `internal/capture/stack`

Both consumers of the capture device — the runtime (which builds the session's source) and the API (which lists, selects, previews and probes) — resolve "which device, in which mode" through one pure layer, `internal/capture/stack`, so the Settings page and the session can never disagree about what will be opened. `stack.Stack` holds seams, not hardware: a device enumerator (`devices.ListWith` in production; the fake seam in tests), a link reader (`usb.LinkSpeedFor`), a selection store (the new `app_settings` row, key `capture`), the `mode.Policy`, and a source builder (production: `ffmpegpipe.V4L2`; the fake seam: a `testsrc2` pipe).

`Resolve(ctx, requestedID)` is the one function the runtime calls at session start:

- **Device by stable ID.** `requestedID` (the session's `frame_source.dir`) must match a device currently in the enumeration — its stable by-id name, or by-path, or its node path, since that is what the API's `id` field is. A raw id that is not enumerated is `ErrDeviceUnknown` (400): the enumeration is the allowlist, the same discipline the replay roots apply to directories. Empty `requestedID` falls to the persisted selection (if it is still present in the enumeration), then to the first usable enumerated device — a single-dongle appliance should start a session without a second round-trip, and the session snapshot names the device actually used (`capture_device`), so the fallback is visible, not guessed.
- **Mode: saved choice, else the policy.** A persisted explicit mode is honoured only when it is exactly an enumerated mode of that device (same size, a rate the size offers, a supported pixel format); a saved mode the card no longer offers (replugged into a different card, kernel change) falls through to `mode.Select` (ADR-0076) with the device's live link, and the answer records which half won (`chosen.source` = `saved` or `policy`, plus the policy's reason and rejected count). No mode fits is `ErrModeUnavailable` (409): the doctor's remediation (re-seat into USB 3, or a higher decode budget from a measured run) is the operator action, not a flapping session.
- **No usable device** is `ErrDeviceAbsent` — the API answers it 503 `capture-device-unavailable` (docs/spec/08's DEVICE_UNAVAILABLE class), never a 200 with an empty list dressed up as success.

The persisted row (`store.CaptureSettings`, one `app_settings` key-value row — no schema migration, the same mechanism as the audio row): `{device_id, mode}` where an empty mode means "the policy decides". `PUT /api/v1/capture/devices/{id}` is the only writer; validation is against the live enumeration (an unknown device is a 400, a non-offered mode a 400), so the store cannot hold a selection the next `Resolve` would silently discard.

### 2. The session's v4l2 source is the production source

`buildFrameSource`'s v4l2 case calls the runtime's capture wiring: `Resolve` the device and mode, then build the source through the `ffmpegpipe.V4L2` constructor (the ADR-0076 low-latency defaults: nobuffer/low_delay demux, the small decode queue, the per-format colour range), with the source's `ID` of the form `v4l2:<node path>` — which is exactly the identity the existing no-signal wrapper (`internal/runtime/signal.go`) keys on, so a v4l2 session gets the ADR-0076 hold-and-recover behaviour with no new wrapper. The session snapshot gains `capture_device` (the stable id actually used) and `capture_mode` (e.g. `640x480 mjpeg @ 10 fps`), and the `session.started` event's `frame_source` object carries them, so the developer UI correlates the run with the physical device.

The runtime takes the wiring as closures (`Options.Capture`), mirroring the `DevDolphin`/`ManualSource` seams: a nil wiring answers `ErrCaptureUnavailable` (503), so a process without capture wiring cannot fake a device. Hotplug reconnection is the ffmpegpipe child's existing restart loop (ADR-0076 item 6): a yanked dongle is a device-lost child failure, the supervisor backs off (capped 10 s) and re-opens the device on the plug-in, and the session's source state (`reconnecting`/`failed`) is what the UI renders — the session is never restarted for a dongle blip, and no new goroutine owns the device outside the source.

### 3. The API surface (operator, per the existing scope-table grant)

- **`GET /api/v1/capture/devices`** — the enumeration with, per device: the stable `id`, name, node path, the negotiated USB link (speed, class, the ADR-0076 downgrade flag), the state (`ready` / `no-formats` / `enumeration-error` / `not-capture`), and the chosen mode with its source (saved vs policy) and reason. The answer carries `synthetic` and the currently selected device id, so the page shows what a session will use and says so when the list is the labelled test seam.
- **`PUT /api/v1/capture/devices/{id}`** — persist the selection (device always; mode optional — omitting it records "the policy decides"). The answer is the saved row plus the effective selection.
- **`GET /api/v1/capture/preview?device=`** — a JPEG snapshot through a throwaway source: build, start, one frame, encode (quality 60, capped to 1280 px wide so the 4 GB box never holds a 1080p frame in a browser), close. Authenticated (the scope grant) and throttled: one snapshot in flight and a 500 ms minimum interval, answered 429 with `Retry-After` when exceeded (docs/spec/08 "rate-limit … preview streaming"). A preview is also refused (503 `capture-busy`) while a running session holds the same device — the UVC bus is single-consumer, and a raw FFmpeg "device or resource busy" is not an operator action.
- **`POST /api/v1/capture/probe`** — the docs/spec/10 capture test: grab `frames` (default 20, capped 200) from a throwaway source, report `grabbed`/`dropped`/`fps`/`freshness_ms`/`state`/`last_error` (the freshness and drop-rate checks), and run OCR over the requested region (a pixel box; absent, the centre third) of the last frame with the region editor's Tesseract factory. OCR is degradable: a missing tesseract binary degrades the `ocr` block to `engine: "unavailable"` with the install action, never failing a probe that otherwise worked.

All four sit under the existing `/api/v1/capture/{rest...}` operator grant; `x-required-role` in the contract says operator, and the matrix test's two placeholder rows now hit live handlers.

### 4. The labelled-synthetic test seam

`GAME_NARRATOR_CAPTURE_FAKE=1` (the ADR-0074 `GAME_NARRATOR_AUDIO_FAKE_DEVICES` pattern, applied to capture) makes `stack.DefaultOptions` return the synthetic seam: one fixed device (stable id `usb-GameNarrator_SyntheticHDMI_0001`, node `/dev/video-fake0`), the documented MS2130-class format set (mjpeg + yuyv422, 640×480 and 1280×720, discrete rates), a fixed HighSpeed (480 Mb/s) link, and a source builder that opens a real FFmpeg `testsrc2` pipe at the resolved mode's geometry — loopback-free (no X11, no v4l2 node) but real frames through the production consumer, with the source `ID` prefixed `v4l2:` so the no-signal wrapper and every v4l2 code path runs on the fake exactly as on the card. Every API answer on the seam carries `synthetic: true`; the Settings page renders "Synthetic device list (test seam): this is not the hardware of this machine", and the Play page's capture option is labelled the same way. Playwright exercises the seam on the packaged binary (labelled synthetic in the report); real-dongle runs stay NOT RUN.

### 5. The device script takes the capture path when the dongle is attached

`scripts/hw/sustained-on-device.sh --source capture` probes the appliance over SSH (`v4l2-ctl --list-devices`) before running: no capture-capable node is still the NOT RUN (exit 2) with the physical-connection remediation; with the dongle attached it runs the same 10 s-sampled budget loop with `frame_source {"kind":"v4l2"}` (no clip install — a live source does not drain), and the report says the source is live capture, not replay dressed as capture.

## Options considered

1. **Let the session request name the device path directly** — rejected: a `dir` that reaches `ffmpegpipe.V4L2` verbatim is the replay-allowlist hole on the capture bus (the client could open any `/dev/video*`, including one the operator did not list). Stable-id resolution against the live enumeration is the allowlist.
2. **A dedicated capture-manager goroutine owning the device for the process lifetime** — rejected: a long-lived grab blocks the preview and the probe (single-consumer bus) and adds a lifecycle the product does not need; throwaway per-operation sources plus the session's supervised source cover every operation, and the ffmpegpipe restart loop is already the ADR-0076 reconnect story.
3. **MJPEG streaming preview (multipart/x-mixed-replace)** — rejected for now: the docs/spec/08 row says "throttled MJPEG/snapshot"; a snapshot endpoint is the smaller surface (one JPEG, no stream state, the browser's `<img>` polling is the loop), and the probe/preview share the one-frame grab. A stream can be added behind the same route without a protocol break.
4. **A new `app_settings` migration with a typed column** — rejected: the audio row (ADR-0074) proved the key-value row is the product pattern for runtime-mutable settings; a JSON row under a new key needs no migration and no backward-compatibility window.
5. **Defer the probe's OCR to the region editor only** — rejected: docs/spec/10's preflight is "run test OCR on user-selected region" *before* a session exists; the probe's pixel-box region is that step, and reusing the region editor's Tesseract factory keeps one engine spelling.

## Tradeoffs

- **A preview during a session on another device is allowed** — the single-consumer conflict is per-device, and on a two-device bench the other device's picture is legitimate. The `capture-busy` answer names the session id holding the device.
- **The 500 ms preview throttle is global, not per-client** — the budget is the decode cores and the USB bus, not the caller; a per-client limit would let two tabs double it. The appliance is one operator at a time.
- **The synthetic seam's `testsrc2` frames carry no game text** — a fake-device session on the Play page reaches "running" with frames advancing, but it cannot commit a line, and the e2e says so (it asserts the session state and the synthetic label, not a commit). The deterministic commit-through-v4l2 path is the Go session test's push-based fake source, which is labelled as such.
- **`Resolve` re-enumerates on every session start** — v4l2-ctl is a ~20 ms subprocess and a session starts once; caching the enumeration would let a replugged dongle's by-path name go stale, which is a worse failure than a 20 ms cost.

## Validation

- `go test ./internal/capture/stack/ ./internal/runtime/ ./internal/api/ ./internal/store/` — the selection layer's table (requested id / saved mode / policy fallback / absent device / unknown mode / the by-path and node-path acceptances, the fake seam's enumeration), the runtime's v4l2 session test (a full session commits the synthetic text through the capture source: fake enumeration, push-based `v4l2:` source, fake OCR; the no-signal hold still applies), the API routes (devices list, select persist, preview throttle 429 and busy 503, probe with stats and OCR, authz matrix, contract validation of every success body).
- `pnpm typecheck && pnpm lint` (the regenerated `schema.d.ts`), `make build`, and the Playwright `capture-device` project on the packaged binary with the labelled-synthetic seam: the Settings capture section (device list, selection, the synthetic label, the probe result) and the Play page's capture source option start a real v4l2 session that reaches running with the fake device's frames.
- **NOT RUN (hardware absent)**: the real RYBOZEN V316B enumeration, its negotiated link, its blue screen, a real HDMI signal loss across a running session, and `sustained-on-device.sh --source capture` on the dongle — the script's NOT RUN branch and the gap audit carry them until the card is attached.

## Consequences

- A session can run from the HDMI dongle: the operator selects the device and mode in Settings (persisted across restarts, env-free), previews before the first run, runs the capture test (freshness, drops, OCR on a region), and starts a v4l2 session from the Play page that holds across a console power-off (ADR-0076) and reconnects across a dongle reseat (the ffmpegpipe loop).
- The contract's capture rows are live: `x-required-role` operator on all four routes, the matrix placeholder rows hit handlers, and the TS client is generated from the same document.
- The one physical fact the software cannot fake (the dongle's real modes, its real link) stays on the NOT RUN list until the preflight (docs/spec/10) runs on the appliance.

## Rollback / migration path

All additive: remove the four routes from `Handler()` and their contract entries, delete `internal/api/capture.go`, `internal/capture/stack`, `internal/runtime/capture.go` and `internal/store/capture_settings.go`, revert `buildFrameSource`'s v4l2 case to the 501, and drop the two `Session` schema fields. The `app_settings` key `capture` row is inert (key-value table; no migration in either direction). No protocol, event-name or store-schema change is involved; the `frame_source` event object's two new fields are additive.
