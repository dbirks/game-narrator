# 12 — Test pyramid, regression fixture discipline and acceptance metrics

## Required test modes

| Mode | Scope | Provider types | When | Blocking |
|---|---|---|---|---|
| Unit | Go + TS logic, state reducer, parser, cache | fakes | every PR | yes |
| Contract | OpenAPI, SQLite migrations, tool outputs, provider schema | recorded and live probes | every PR / nightly | yes for recorded, conditional live |
| Frame replay | real OCR/matcher on captured screenshots + synthetic visual fixtures | recorded Clef/VLM | every PR | yes |
| UI | real Go service + React and browser | recorded providers | every PR | yes |
| Dolphin controlled | real emulator, controller, capture and Go runtime | recorded providers | Arch host/on-demand | yes when environment configured |
| Dolphin full inference | real emulator + gateway + real Clef, VoiceDesign/Base, ASR | real endpoints | on-demand/nightly/release | yes when resources provided |
| Hardware-in-loop | real HDMI/UVC/UNO Q/USB audio | live gateway | tagged release/board available | release-target hardware gate |
| Resilience/performance | chaos injection, replay at speed, memory/temperature | mixed | nightly/release | yes for published release |

No test may silently substitute synthetic frames or canned model replies for a real-model/Dolphin assertion. Reports explicitly record stage origins.

## Benchmarks and ground truth

Use curated ground truth JSON from screenshots/game states labeled with source text, speaker, text kind, scene and intended speech. At minimum create reference sets with a range of short/long dialogue, multiple font treatments, layouts, analog-converted noisy input and capture resolutions. Split into tuning vs holdout; do not tune thresholds on holdout. Report recognition precision/recall, CER/WER, named speaker accuracy and calibration (Brier/ECE), positive speech precision, missed narrations, duplicate speech, prediction misfires, stale speech after advance, p50/p95 start latency, end-to-end audio duration vs expectation and CPU/RSS.

Provisional acceptance objectives — must be calibrated on actual content and revised via ADR with benchmark evidence, **not represented as already achieved**:

- ≥95% recall for intentionally engaged conventional dialogue boxes on baseline selected scenario dataset; ≥99% precision for events that trigger speech.
- ≥95% top-1 exact canonical message selection for baseline held-out named dialogue, if script contains message; high ambiguity can abstain.
- ≥95% named speaker accuracy on *committed named-voice* utterances; unknown cases don't receive wrong forced speaker.
- No duplicated speech for repeated observations of same dialogue occurrence in recorded tests.
- Zero stale audio starts after a new committed dialogue event in deterministic delay-injection suite; local stop event target <200 ms (audio device buffering excluded and reported).
- ≥99% source text preservation under parse/normalize roundtrip for supported control tags, except explicitly recorded transformations.
- Voice reference retained across application restart, inference worker restart and re-registration; no audio cache collision across profiles/revisions.
- No unhandled panics/OOM during at least 1h replay load on UNO Q profile and 2h simulated network/provider chaos.

## Required test cases, organized by dimension

### Visual perception

- Game running without text; lower-third UI that resembles textbox; transparent/animated box edges; fade-in/out; typewriter progressive reveal; instant text; mixed-size font; outlined text over animated background; 4:3 frame letterboxed into 16:9; capture deinterlacing and motion blur; framebuffer size change; negative contrast; Zelda-style decorative glyphs; overlapping HUD icons; video loss/freeze; repeated identical frames; black/loading screen; on-screen choices; cursor movement; sign in open world vs sign actually opened; item pickup; button icons; custom player name; non-English script; textbox partly occluded.

### Dialogue and graph

- Exact script hit; Levenshtein near miss; short common phrase; same text spoken by two actors; two adjacent graph candidates; graph jump/revisit; branch selection; loop; no next-message ID; missing source; partial/incorrect dump; version mismatch; punctuation-only difference; manual speech rendering different from canonical text; very long multi-page utterance; player skips intermediate page; repeat NPC after menu; different scene same sentence; new profile publication while session active; predictive prefix unique vs two likely continuations; prediction misfire then correction.

### Decision/provider

- Clef choice answer and `unknown`; batched classification where returned fields contradict; high score on wrong option; image decode fail; provider 429/503; inconsistent answer schema; gateway load balances to new Clef build; request late after session reset; images too large; model content limits; too many choices; missing auth; TLS error; hallucinated VLM speaker; offline fallback; user revokes remote screenshot sharing; corrupted provider responses; per-role circuit breaker recovery.

### Voices/TTS/ASR

- Designed audition clips; speaker approval; voice reference exact text; Base model rejects CustomVoice instruction; seed unsupported gracefully; registry returns missing on replica; app restart maintains approved voice; TTS emits silence, clipping, short/truncated, duplicate segments, pronunciation errors; ASR false negative; max regeneration retries; cached clip retrieval; cache revision invalidation; identical text from two speakers; emotional variation does not mutate identity; streaming chunk reorder; sample-rate mismatch; USB DAC unplug; Bluetooth intentionally optional; audio device buffering drift; player page advances during streaming.

### Agent and sources

- Same game name cross-region; online search disabled; malicious prompt injection from source page; script imported by user; rights unclear; HTML scraper hitting redirect to private IP; oversized ZIP or zip-slip; parser crashes; partial import restart; source cross-edition mixup; exact/fuzzy text conflict; character merged incorrectly; user chat corrects a character and previews diff; voice approval workflow; auto-research doesn't imply auto-publication; profile export/import; no copyrighted bytes in default distribution.

### UI/operations

- UI refresh while session playing; SSE reconnect and Last-Event-ID; history gap; developer event inspect and correction; source preview; agent tool log; Playwright screenshot matching event; dashboard with offline provider; diagnostics actually detects wrong audio device; backup/restore; disk full; DB busy; 10k messages pagination; storage purge doesn't remove pinned references; auth/CSRF; large asset upload; non-ASCII titles; browser console errors; embedded SPA deep link after fresh binary deployment.

### Emulator/hardware

- Dolphin boot and screenshot, proper graphics setup; commandline `--movie`/`--save_state` proof; DTM desync after config change detected; live input pipe/controller event accepted; savestate checkpoint validates; simulator exits/crashes; window loses focus; framebuffer capture permission fails; renderer black frames; UNO Q UVC enumeration changes; splitter EDID change; 480i/480p incompatibility; powered hub undervoltage; USB DAC missing; device hotplug; power loss during profile publish; long-run thermal throttling.

## Test fixture quality and provenance

A golden fixture has an assertion plan, valid `source_type` label, frame/image hashes and source rights, expected text/speaker labels with human review, and deterministic provider-response sidecar if recorded. Live-model tests do *not* assert exact probabilities; use policy invariants and quality thresholds. Store output artifacts from failing runs, hash and version them; rerun fail-only case before opening a PR.

## Property-based and fuzz tests

Fuzz normalization with arbitrary Unicode and control tokens; no panics or illegal byte drops. Fuzz ZIP/parsers against traversals/decompression bombs; malformed BMG chunks should fail safely with offsets. Fuzz API JSON/envelopes, SSE client reconnect logic and candidate ranking's `NaN`/infinite values. Property: rank ordering deterministic under same inputs/settings; event reducer never accepts stale generation; asset export/import round-trip stable; no published edit without valid revision token; no duplicate clip for idempotent request.

## Failure injection/hypothesis tests

Model responses delayed 0–30s with reordering; fake network partition; wrong checkpoint returns 400; voice server instance rotation; frames skipped at varied fps; two OCR futures finish out of order; 20 rapid textbox page advances; duplicate SSE delivery; TTS partly streams before cancellation; disk insufficient mid-WAV write; process killed mid-transaction; local power cycle. Assert no inappropriate audible stale clips and explain degraded status.

## Evidence bundle and release test report

`manifest.json`, `events.jsonl`, `assertions.json`, `metrics.json`, `screenshots/`, `audio/`, `playwright-report/`, `traces/`, `model-calls.jsonl.redacted`, `commands.txt`, `report.html`. HTML includes system info; timeline with synchronized frame/recognized message/selected speaker/voice/waveform; link to raw redacted decision questions and probabilities; failure summaries and reproduction command. Maintain JUnit export and machine-readable quality dashboard.

## Release gates

- All unit/contract/frame replay/Playwright tests green, no unresolved high-risk security issues.
- Complete game onboarding/profile editing/voice approval and runtime modes exercised end-to-end.
- At least 12 diverse Dolphin scenarios recorded and passing in one supported, pinned host configuration; at least 5 scenarios with actual live model requests including one speech cache miss, one cancellation, one wrong-speaker correction and one sign/item.
- Synthetic second-game adapter pipeline passes entire suite without Wind Waker-dependent conditions.
- ARM64 package produces working binary on UNO Q (if unit provided); real hardware capture/audio preflight and long-run scenario pass for hardware release. If not available, explicitly label release as unverified on hardware.
- Build artifacts checksums verified, fresh install/uninstall/upgrade and backup/restore tested; security checks on default published images.
