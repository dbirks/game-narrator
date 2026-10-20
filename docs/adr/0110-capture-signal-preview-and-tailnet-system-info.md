# ADR-0110: HDMI signal and active-picture preview (UX-11), tailnet address in system info (UX-13)

Date: 2026-10-20
Status: accepted
Owners: ux11 worker
Related requirements/tests: docs/design/plum-peach UX-11 ("HDMI signal and active-picture preview (Health and Play)"), UX-13 ("Tailnet URL shown in settings"), docs/spec/08 (GET /api/v1/capture/preview; rate-limit preview streaming), docs/spec/10 ("never assume /dev/video0"; last-frame freshness; preview rate limit), docs/spec/13 (operator action on every fault), ADR-0078 (capture sessions/devices: the one-shot snapshot/probe paths, the global preview throttle, the per-device busy rule), ADR-0082 (the active-picture tracker and its single-frame `Settle` seam), ADR-0100 (status-bar snapshot: the live-status class the signal state joins)

## Context and constraints

UX-11: the UNO Q captures through an external UVC HDMI path (GameCube → RCA-to-HDMI → splitter → UVC dongle), so "card connected" is not enough — Health and Play must show **HDMI signal present** and an **active-picture** preview, so a dead cable or sleeping converter is visible immediately. With a black/static feed the surfaces must say "no active picture"; with a changing feed they must say signal present. ADR-0078 already ships `GET /api/v1/capture/preview` (one throttled JPEG through a throwaway source, capped 1280 px wide) but it answers the *device's* picture, never the *session's*: while a v4l2 session runs, the route 503s `capture-busy` instead of showing what the running session is actually seeing, and there is no JSON answer for "is there a signal, in what format, at what rate, where is the picture, and how old is the last frame" — the data the Health tile and the live status stream (UX-07) need.

UX-13: the settings page must show the device's **tailnet URL** (the Tailscale address for reaching the web UI over the tailnet), copied from the running system so it always matches the appliance; when Tailscale is absent an explicit "not configured" state, not a blank field. The system-info surface (`GET /api/v1/system/info`, the settings backend's machine row) already reports the bound listen address on the settings/security section and the status bar, but nowhere does the box say how to reach it over the tailnet.

Constraints: the UVC bus is single-consumer (a throwaway preview source and a running session must not both open the node); the appliance is four A53 cores (previews are a decode budget, so the existing global 500 ms throttle stays); frames are never stored (AGENTS.md rule 5 and the preview's "served from memory, never stored" requirement); the one-active-session invariant means at most one capture session exists; the tailnet address must be detected **at startup**, never by shelling out at request time, and it is never sent to any external service (the update check, provider calls and support bundles all stay tailnet-free).

## Decision

### 1. `GET /api/v1/capture/signal` — the JSON sibling of the preview (operator)

A new route under the existing `/api/v1/capture/{rest...}` operator grant answers the signal question from two sources of evidence, in order:

- **A running v4l2 session** (`runtime.Manager.CaptureLive`): the session snapshot names the device, the negotiated format (`capture_mode`) and the target fps; the source's self-report gives the measured fps, the last-frame time and the lifecycle state; the session's one active-picture tracker (ADR-0082, the pipeline's `CropOptions.Tracker`) reports the accepted window and whether it has settled; the pipeline's ROI stage reports the last crop rectangle it computed for the dialogue region (the first region whose text-kind hint is dialogue — the textbox); and the pipeline's newest in-memory frame gives the active-picture judgment and the preview's pixels. **The session path answers v4l2-source sessions only**: a replay or test-pattern run is not the card's source and holds no card, so it never answers "signal present" — for those the one-shot path answers against the card instead.
- **Else, a one-shot grab** (the ADR-0078 throwaway path, now `stack.Frame`: build, start, one frame, close, the frame returned in memory): the negotiated mode from the resolution, the source's stats, the window from `Tracker.Settle` on that one frame (the ADR-0082 amendment-1 single-frame seam — a one-frame input has no stream to hysteresise over), and the active-picture judgment from the same frame. A single frame cannot measure a rate, so `fps_actual` is null in this mode.

The answer:

```json
{
  "device": "usb-…", "mode": "640x480 MJPEG @ 10 fps", "source": "session|oneshot",
  "session_id": "…", "signal": true,
  "fps_actual": 9.8, "fps_target": 10,
  "picture": { "x": 72, "y": 136, "w": 1764, "h": 854, "settled": true, "active": true },
  "dialogue_crop": { "x": 62, "y": 363, "w": 518, "h": 76 },
  "age_ms": 120, "synthetic": true
}
```

Semantics: `signal` is the source's aliveness (session: `Stats.State == running`; one-shot: a frame arrived within the grab budget) — a card delivering a black pattern is *signal present* (frames arrive), while `picture.active` is the "is the picture there" half (a bar-framed, non-black picture was found in the newest frame via the same `ActiveArea` scan the tracker runs per frame; a black/static feed answers `active: false`, which the UI renders as "no active picture"). `picture` carries the accepted window (the whole frame plus `settled: false` until the tracker settles one, the ADR-0082 fresh-start behaviour) and `dialogue_crop` is absent until the session has computed one (the one-shot path has no profile regions, so it stays absent there). When **no session is running and the one-shot grab cannot run** (no device attached, the device not delivering, the device held) the route answers **409** `capture-signal-unavailable` with the operator action in the detail — the "no session" state the spec names — while a client-named device that is not in the enumeration stays a 400 (the enumeration is the allowlist, ADR-0078). The route shares the preview's global throttle (one one-shot in flight, 500 ms minimum interval, 429 + Retry-After) because the one-shot mode opens the same single-consumer node; session mode answers from memory and is only throttled against an in-flight one-shot.

### 2. The preview serves the session's newest frame while a session runs

`GET /api/v1/capture/preview` keeps its contract (operator, global 500 ms throttle, 429, JPEG, `no-store`) but changes two things:

- The width cap moves from 1280 px to **640 px** (UX-11): the Health tile and the Play frame are small; a 720p-class JPEG on a 4 GB box is decode budget the OCR and speech pipelines would rather have. The `stack`'s one-shot path and the session path share one encoder (`stack.EncodeJPEGDownscaled`, quality 60, nearest-neighbour downscale, the existing `downscale` helper).
- While a v4l2 session runs on the resolved device, the JPEG is the session's **newest in-memory frame** — served from memory, never stored, never a second device grab. The UVC bus stays with the session, so the old 503 `capture-busy` answer for the same-device preview is replaced by the picture the operator asked for; the 503 `capture-busy` survives only for the one-shot path racing a session start (the window where the hold is not yet visible). The one-shot path (no session, or a different device named) is unchanged. A session that has not delivered a frame yet answers 409 `capture-preview-unavailable` with the reason (no frame in memory yet / the source's own last error), which is the honest state for a session that is starting but not yet streaming.

### 3. The pipeline keeps one newest frame and the last per-region crops

Two small, bounded, mutex-guarded observations join `pipeline.Runner`, written only on the frame-ordered stage:

- `noteLatestFrame` once per frame in `roiLoop` (before the regions analyse it): a single-slot mailbox that overwrites, so the pipeline holds exactly one extra reference to the newest frame (an 1080p RGBA frame is ~8 MB; the ADR-0082 bridge's LatestSlot already holds one). `LatestFrame()` reports it for concurrent readers.
- `noteCrop` in `analyze` (per region, per frame): the crop's pixel rectangle, one entry per region id — the region set is the profile's, bounded. `LastCrop(id)` and `DialogueCrop()` (the first region whose kind hint is dialogue) report the last computed crop in frame coordinates — the box the developer inspector and the signal route draw, through the same `MapRect` the runtime crops with, so they cannot disagree.

Memory is bounded by construction: one frame + one rectangle per region, overwritten in place, no history, no accumulation. A nil tracker / nil image / empty crop record nothing; a finished session's runner is unreferenced with the session, so the frame's pixels are released with it.

### 4. Tailnet detection: at startup, cached, never at request time

`internal/api` gains `TailnetDeps` / `DetectTailnet` (a pure, injectable function) and a `WithTailnet` option. Detection order: (1) scan the machine's interface addresses for an IPv4 in **100.64.0.0/10** (the Tailscale default CGNAT range) — first match wins, source `"interface"`; (2) else, when the `tailscale` binary is available, run `tailscale ip` **once** (bounded 2 s context) and accept its IPv4 answer, source `"tailscale"`. The result is cached on the Server: `cmd/game-narrator` detects at startup — after config, before the listener — and injects the value, so no request ever touches the network namespace or the subprocess again. The answer: `present`, `address`, `port` (the bound listen port), `url` (`http://addr:port`) and `source`; absent, `present: false` with an explicit `note` ("Tailscale is not detected on this machine…" — the UX-13 "not configured" state, never a blank field).

The value is exposed in two places and nowhere else: the **settings security section** (the operator's page: the listen address it already renders, plus the tailnet block) and `GET /api/v1/system/info` (`listen_addresses` — the bound address(es) the host wired — plus the tailnet block), so a monitoring client and the settings page read one spelling. `main` also wires the previously-unwired `api.WithListen(cfg.Listen)`, so the bound address is the real bind, not the environment fallback. The tailnet address crosses no other boundary: the update checker, provider requests and the support bundle do not read it (they are tailnet-free by construction — none of their seams receives the value), which is the "never sent to an external service" rule made structural rather than promised.

### 5. OpenAPI and contract

The document gains `/api/v1/capture/signal` (GET, operator, the `CaptureSignal` schema; 200/400/409/429/503 + default problem), updates the preview's description and documents the 409, and adds `listen_addresses` + `tailnet` to `SystemInfo` (the `Tailnet` schema). `CaptureSignal.fps_actual` is the null-union type form the validator enforces (null in the one-shot answer); `dialogue_crop` follows the document's omission convention for nil pointers (absent until computed, `omitempty` — the validator's `$ref` resolution would reject an explicit `null` against the object schema). The authz matrix gains the signal row (operator), and the success bodies land in `recordContractResponse` like every other capture answer.

## Alternatives considered

1. **An MJPEG multipart stream for the preview.** Rejected: ADR-0078 already rejected a stream for the same surface; the browser's `<img>` polling at the 2/s throttle is the loop, and a stream would need its own state, backpressure and a second route to tear down. The snapshot contract stays.
2. **Answer the signal from the no-signal monitor's verdict only.** Rejected: the monitor (ADR-0076) judges loss (black/static/frozen) for the hold-back behaviour, but UX-11 also asks *where* the picture is and *how old* the frame is — the tracker's window and the source's stats are the evidence, and the monitor's private state would need its own plumbing to reach the API. The source stats + `ActiveArea` on the newest frame answer both halves with what the pipeline already computes per frame.
3. **Run the one-shot grab on every signal request with no session (no throttle).** Rejected: the one-shot mode opens the single-consumer node and decodes a frame — that is the preview's CPU/USB budget, and an unthrottled poller would starve OCR. The shared throttle keeps the budget global (ADR-0078's tradeoff, unchanged).
4. **Report the tailnet address only on the admin settings page.** Rejected: UX-13's acceptance asserts the settings page's value against *the API value*, and the system-info surface is the machine row a monitoring client (and the settings backend) reads; keeping one spelling on both surfaces is what "copied from the running system" means. The address is CGNAT-space (unreachable off the tailnet) and the UI still requires authentication, so the public system-info exposure is the tailnet membership boundary, not a credential leak — and the value is never forwarded anywhere.
5. **Detect the tailnet by shelling out to `tailscale ip` per request.** Rejected: a request-time subprocess is the exact cost class the spec forbids, and a wedged CLI would put latency on every settings render. Startup detection + cache is the whole design.
6. **A second, per-client preview throttle.** Rejected: ADR-0078's tradeoff still holds — the budget is the decode cores and the USB bus, not the caller; the appliance is one operator at a time.

## Consequences and risks

- Health and Play can say "no active picture" on a black/static feed and "signal present" on a changing one, from one JSON route plus the existing preview; the live status stream (UX-07/ADR-0100) can carry the signal state without a device grab.
- The preview no longer 503s while its own session runs: the operator sees the picture the session is reading. Risk accepted: the newest-frame read is a cross-goroutine snapshot (mutex-guarded single slot, the same class as the tracker's `Box` reads, ADR-0082 amendment 1), so it is one frame old at worst — correct for a preview.
- One extra frame held per running session (~8 MB at 1080p RGBA, overwritten per frame): bounded, released with the session. The per-region last-crop map is one rectangle per profile region.
- The one-shot signal mode reports `fps_actual: null` (one frame cannot measure a rate): the JSON is honest about the evidence class rather than inventing a number.
- The 1280 px preview cap becomes 640 px: a regression for anyone who used the preview as a high-res still. Accepted — the probe (200 frames, OCR) and the dev-trace frames are the high-fidelity surfaces; the preview is a tile.
- Tailnet detection is cached for the process lifetime: a tailnet IP acquired after boot is invisible until the next restart. The `tailscale ip` fallback covers the case where the interface scan misses (e.g. a non-default CGNAT range), and the note names both halves of the detection so an operator can see why "not configured" is the answer.
- Hardware path (the real RYBOZEN V316B, a real dongle, a real tailnet) stays NOT RUN here; every answer on the synthetic seam carries `synthetic: true`, and the tailnet tests inject synthetic interfaces rather than reading the host's.

## Evidence and test plan

- `go test ./internal/api/... ./internal/capture/... ./internal/pipeline/... ./internal/runtime/...` (race):
  - the signal's one-shot mode on a **synthetic bordered frame** (the ADR-0082 tracker shape: black borders, a picture inside) — the window's x/y/w/h against the expected active area, `settled: true`, `signal: true`, `synthetic: true`; the black-frame variant — `signal: true`, `picture.active: false` (the "no active picture" answer); the no-device variant — 409 with the operator action; the unknown-device variant — 400.
  - the signal's session mode on a running v4l2 session (push source, the runtime's own capture wiring): `source: "session"`, the session id, the negotiated format and target fps, the measured fps, the tracker window, the **dialogue crop** equal to the textbox region's mapped rectangle, and the preview's JPEG decoded to ≤ 640 px wide **without the preview's source seam being opened** (the in-memory proof: the device is never a second consumer).
  - the preview's 429 (second request inside 500 ms, with Retry-After) and its size bound (the decoded JPEG is ≤ 640 px wide).
  - tailnet detection on **injected interfaces**: a 100.64.0.0/10 address → present with the url and the `interface` source; no /10 interface + an injected `tailscale ip` answer → present via `tailscale`; neither → `present: false` with the explicit note. `system/info` and the settings security section carry the bound listen address(es) and the tailnet value (contract-validated).
  - the OpenAPI/contract tests (doc↔router parity, role grants, success-body validation) over the new route and the changed schemas.
- NOT RUN (hardware absent): the real dongle's enumeration/modes/link, a real signal loss across a running session, and a real tailnet address — the hwsmoke and device scripts carry them.

## Migration / rollback

All additive. Revert: remove the `/api/v1/capture/signal` route, its OpenAPI entry, its matrix row and the 409/width changes in the preview handler; drop `CaptureLive` from the runtime and the `LatestFrame`/`LastCrop`/`DialogueCrop` seams from the pipeline (they are pure observers — no other caller); delete `internal/api/tailnet.go`, the `WithTailnet`/`WithListen` wiring in `main` and the two schema additions. No protocol, event-name or store-schema change is involved; the preview's contract keeps its route, role and media type in every variant of this decision.

## Follow-ups

- UX-07: carry the signal state in the live status stream (ADR-0100's snapshot) once this route exists — the route's JSON is the field source.
- The settings page (frontend) renders the tailnet block and the "not configured" state from the new fields; that is the paired UI change, not this one (backend-only task).
- If a non-default Tailscale range is ever configured (the CGNAT range is operator-mutable), the `tailscale ip` fallback already answers; the interface scan's range is then the only half to extend.
