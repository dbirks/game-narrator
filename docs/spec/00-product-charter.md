# 00 — Product charter and experience specification

## Mission

**Game Narrator** is a self-hosted audiovisual companion for games that lack recorded voice acting or have text that users would like spoken. It never modifies the game or inserts latency into the display path. It creates durable, per-game understanding from user-imported material and synthesizes distinct, consistent character voices based on detected gameplay text.

## Supported usage modes

- **Physical-console companion:** GameCube analog-to-HDMI → powered 1×2 HDMI splitter; leg A to TV, leg B to UVC HDMI capture → UNO Q 4 GB Linux; speech output to USB sound device. App accessed via LAN browser.
- **Dolphin development mode:** Arch Linux machine running Dolphin with developer-controlled frame source and audio sink, no UNO Q required. Same runtime pipeline; emulator controls are dev/test only.
- **Recorded-media regression:** timestamped source frames or video + expected observation/decision/audio events, runnable headless in CI.
- **Generic game import:** game profile supplied through agent discovery, text dump, script archive, manual entry, or plugins; media-specific automation is optional.

## Personas and complete journeys

### Player

Connects console and HDMI splitter → opens Game Narrator → chooses profile → verifies audio/capture preview → plays normally → hears character voices, signs and item text only on intentional selection → can pause/mute, skip, adjust delay/volume, choose conservative/predictive playback, switch narrator voice. Inference outage degrades gracefully. No external PC is required once UNO Q is provisioned.

### Game curator

Enters title/version/language → agent researches sources and presents confidence/source previews → imports owned game data or approved web material → extracts dialogue, control codes and message graph → tags possible speakers → agent proposes personalities, voice designs and canonical references → auditions/edits/approves voices → generates/prefetches selected audio and publishes versioned profile → exports/reimports profile manifest without third-party copyrighted bytes by default.

### Developer / quality engineer

Runs Arch+emulator and tests → observes frame/crop/OCR/script match/speaker score/Clef answers/TTS/ASR/playback events in developer dashboard → fixes a wrong label → saves a testcase and optional DTM/savestate → runs replay and live Dolphin scenario automatically → views correlated Playwright report and profiler. Builds AMD64/ARM64 releases and installs on UNO Q; runs hardware preflight.

## Functional product requirements

`FR-01` Device enumeration and selection; previews, aspect ratio, signal-mode stats, screenshot and capture test.  
`FR-02` Profile library; create, import, export, duplicate, clone, disable, version, archive and delete.  
`FR-03` Agent-driven research with constrained web search, provenance, approval, resumable tasks, per-tool logs.  
`FR-04` User-owned dump import with format adapters, source hashes, raw bytes stored separately from parsed records, safe token handling.  
`FR-05` Exact, fuzzy, context-aware text match and graph progression; recover from arbitrary travel/skips.  
`FR-06` Overlay/text region discovery, configurable OCR pipelines, local processing and VLM fallback.  
`FR-07` Clef decision provider with typed batched questions, calibrated policies, abstention.  
`FR-08` Consistent per-character voices, narrator presets, voice audition/approval/versioning.  
`FR-09` Qwen3 VoiceDesign→approved reference→Base voice registry→cached/streamed speech.  
`FR-10` Qwen3-ASR quality checks for approved/generated speech; regenerate and flag.  
`FR-11` Generation cancellation, text page transitions, cache prewarming, audio output and mixer/delay configuration.  
`FR-12` Live player mode and fully featured developer inspector.  
`FR-13` Full Dolphin fixture creator/replayer, UI validation with Playwright, automated artifact packaging.  
`FR-14` Diagnostics, security/auth, backups, bounded storage, provenance and model resource controls.  
`FR-15` ARM64 device packaging, amd64 test packaging, OCI image, signed/checksummed tagged releases.

## UX-specific rules

- Audible default starts once text identity and utterance are sufficiently certain; short fade-and-stop on advance. Never block controller/game progression, and never send input to actual console.
- No open-world OCR spam. Dialogue/sign/item reading only when a present/selected textbox or explicit interaction can be detected. Narrator for confirmed non-character text; abstain if even speech eligibility is unclear.
- Voice style coherent across lines, scene-sensitive without mutating canonical timbre.
- Predictive mode optional; refuse early playback on ambiguous prefixes, conflicting scene evidence, or poor calibration.
- Show “uncertain” instead of fabricated character attribution; let curator correct and preserve training/eval artifact.
- All autonomous outputs are inspectable; never claim the setup agent found a complete game script when coverage is unverified.

## Nonfunctional requirements (initial targets, not measured capabilities)

- Console video should remain on direct splitter branch; app should add **zero inference-path display latency** (HDMI hardware can have its own measured delay).
- UI remains responsive under model outage/capture load; target p95 basic API latency < 250 ms on development host and < 500 ms on UNO Q, excluding long jobs.
- Queue policy shall favor **fresh** dialogue, dropping stale frames before older requests saturate the service.
- Conservative cached-line audio start target p95 < 800 ms from complete/committed visible text; predictive target depends on calibration. Record actual breakdown; thresholds remain tentative until measured.
- No stale speech after a dialogue-change grace window; target cancellation response < 200 ms for local playback (measured independently of audible device latency).
- Game profile operations should be atomic and recoverable after power loss. Target recovery point: last committed transaction + content-addressed assets.
- Unicode and multilingual text model ready; initial English validation, architecture should not assume Latin-only scripts.
- Handle 2–4 GB device memory without running local heavyweight foundation models; bounded OCR workers and image buffers.

## Explicit non-goals

- No reverse-engineered gameplay AI that controls physical consoles; no gameplay input manipulation in production.
- No synchronization of game clock or alteration of games' sound tracks in first product release. Optional external audio mixing can be a later enhancement.
- No public hosting of copyrighted scripts, original game files, or character art.
- No requirement for all imported games to have a BMG or exact dialogue graph. Game adapters degrade gracefully.
- No unsupervised public-user voice impersonation or scraping private voice samples.

## Product completion checklist

Player, curator and developer journeys work. At least one user-provided GameCube title passes live Dolphin full E2E; another structurally different synthetic/generic profile passes adapter tests; uploaded voice survives inference restart or re-registration; multi-game profile selection and edit/version work; portal supports full correction loop; packaging installs on fresh Arch development host and Debian UNO Q with documented native dependencies. If physical hardware or inference services are not present, mark their gates **blocked/unverified**, never passed.
