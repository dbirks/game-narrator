# 16 — Coding-agent implementation program and definition of done

## Scope of the implementation assignment

**Deliver the complete Game Narrator application described in parts 00–15**, not only architecture documents or a thin MVP. Work may be split into milestones to manage dependencies, but the ultimate assignment includes the functional product, strong tests, deployable images/packages, operational guides, and first-class dev experience. Do not stop upon proving one dialogue line. Working code is validated with `make test-full` and supported real Dolphin test runs. If the user has not yet provided game data or inference credentials, build/test every other feature with synthetic fixtures and record the blocked real gates; continue implementation in all unblocked areas.

## Milestone 0 — Environment/discovery and executable infrastructure

Outputs: reproducible `scripts/bootstrap-arch.sh` (reviewed actions, idempotent), dependency manifest, `.env.example` with logical model roles, Go/Vite skeleton, SQLite migrations, mocked and replayable providers, `make` targets and tests. Probe Dolphin (`--version`, graphics backend, game image path via config), UVC capture tools, audio sink, browser. Write ADRs for chosen Go router, SQLite driver, frontend UI kit, FFmpeg/OCI strategy. Acceptance: first-run dev environment starts, web UI served by Go or Vite, Go/TS tests green, no secrets in Git, diagnostic report lists configured/missing components accurately.

## Milestone 1 — Full foundation and portability

Core domain types; SQLite state and indexed retrieval; asset storage abstraction; full profile/version import/export, generic CSV/JSON parser, game adapters and parser interface; user login and role ACL; JSON-schema and OpenAPI, generated TS client, persistent jobs/event bus. Acceptance: create/edit/revise/publish/export/reimport two synthetic game profiles, verify migration/restart/backup, comprehensive API/UI tests, search 10k synthetic messages without memory blowup.

## Milestone 2 — Video + recognition engine

FrameSource implementations: V4L2/FFmpeg, replay and Dolphin; region plugin configuration; OCR process with temporal voting; text state machine; exact/prefix/trigram/fuzzy retrieval; scene/message graph; policy engine and safe abstention. Acceptance: synthetic variable-layout fixtures pass, captured Wind Waker screenshots analyzed once authorized, repeat observation doesn't create duplicate speech, out-of-order results discarded. Developer inspector displays frame, crop, normalized OCR and candidates.

## Milestone 3 — Model integration and knowledge agent

Capability-driven inference router, Clef adapter with bundled questions, real-model schema probes, VLM/LLM fallback, ASR client, web search provider, safe page fetcher, source discovery, parser job runner, character and scene graph builder, agent chat + editorial correction. Acceptance: agent can identify/import synthetic title using local sources, optionally browse verified external metadata, show attribution + rights and have a user review/publish it; prompt injection rejected; Clef real endpoint smoke if configured; provider faults correctly degrade.

## Milestone 4 — Complete voice and playback pipeline

Qwen3 VoiceDesign audition generator, Base registered voice lifecycle, instance reconciliation and inline fallback; versioned voice references; audio rendering, ASR QA, deterministic cache/prewarming and USB file/physical sinks; cancellation/streaming; narrator voice; player session. Acceptance: full voice setup audition/approval, cloned voice preserved after app/provider restart, real TTS+ASR tests when configured, correct TTS on cache miss followed by cache hit, rapid advance stops stale playback.

## Milestone 5 — Complete React product

Build onboarding wizard, library, message editor, graph viewer, characters, voice editor/preview, region calibration UI, publication review, player dashboard, developer inspector, diagnostics page, full test viewer, profile export/import, settings and role-based auth. Acceptance: Playwright test coverage of each route, correction produces regression fixture; SSE reconnect reflects accurate state; voice preview works; user can curate a full synthetic game with no CLI.

## Milestone 6 — Dolphin live driver and fixture factory

Implement/dev-probe real Dolphin launcher with isolated user profile, DTM/savestate loading, virtual/gamepad input, capture adapter (window or Vulkan/OpenGL capture), oracle and fixture recorder; use authorized game, build a variety of scenes and save/label test states; build replay reports, snapshots and audio evidence, integrate with `make test-dolphin`. Acceptance: real emulated game boot/frame/controlled input verified; at least 12 authored scenarios (or justified blocked count pending user assets), report links actual frames, decisions, voice, audio and React UI. Validate no simulator-only hacks in runtime code.

## Milestone 7 — Full live-model and adverse-condition validation

Run live Clef + Qwen3 Base/VoiceDesign + ASR and VLM where relevant on user gateway, test route compatibility, registry replica changes, calibration and thresholds. Exercise concurrency, cache and provider failure injection, malicious web content, long-running sessions, graph branches, unknown text, noisy analog video. Acceptance: benchmark targets in part 12 met or ADR adjusted with evidence; no silent voice mismatch or stale speech; diagnostics expose bottlenecks.

## Milestone 8 — UNO Q and distributable release

Install on UNO Q Debian arm64 with powered USB hub/capture/USB DAC, validate frames, UI responsiveness, 4 GB memory, audio, power, hotplug and network behavior. Create systemd/nFPM Debian package, optional Arch package, GoReleaser v2 AMD64/ARM64 archives and GHCR multiarch OCI images. Test clean install/upgrade/rollback, signed/checksummed artifacts, security scan and no copyrighted bundled bytes. Acceptance: hardware smoke and sustained play verified when actual hardware exists, otherwise explicitly `NOT VERIFIED` and user handoff instructions; release notes and complete readme.

## Milestone dependency graph

M0→M1→M2/M3 in parallel→M4→M5 (UI incrementally)→M6→M7→M8. Tests and ADRs are ongoing, not deferred. Use vertical demonstrations to validate integration early, but none constitute project completion on their own. Any gate unavailable due to private resources may be marked `blocked` with exact unblocking instructions while the agent continues other planned work.

## Implementation backlog by ownership

| Workstream | Deliverable(s) | Core verification |
|---|---|---|
| Data/storage | migrations, indexes, profile exporter, asset dedupe | migration+backup/restore+roundtrip |
| Agent/orchestration | safe tools, job workflows, review and approvals | injection, resume, agent edit tests |
| Game adapters | generic source+Wind Waker BMG importer after supplied assets | parser fixtures, dual-game test |
| Capture/OCR | V4L2 and Dolphin + replay, ROI, normalization | captured frames, parameter tests |
| Recognition | candidates, graph, belief, commit/cancel | golden fixture accuracy/chaos |
| Inference | gateway role config, Clef and VLM, ASR | schema probes, live/redacted traces |
| TTS | VoiceDesign, registry, Base, cache, QA | identity, restart, ASR, no stale playback |
| Browser UX | library, curation, player, dev, tests, diagnostics | Playwright + accessibility + SSE |
| Test kit | DTM driver, oracle, fixtures, simulator reports | reproducible scenario suite |
| Ops/release | security, telemetry, doctor, systemd, GoReleaser | packaging/hardware/artifact scanner |

## Agent autonomy and escalation

Allowed on Arch dev host with passwordless sudo: installing required official packages after previewing package manager commands, building app, debugging local emulator/test scripts, reading upstream docs, creating temporary isolated Dolphin profiles, generating synthetic fixtures, safe git changes/ADRs. Never read private files unrelated to configured game/test paths or send credentials in logs. Requires user: supplying legitimate ISO, choosing copyrighted material import approval, providing endpoint credentials or reachable gateway, confirming destructive storage removal, external public release, granting voice consent. If blocked, log `blocked_by`, evidence, proposed workaround and continued tasks; do not ask repeated already answered questions.

## Mandatory work tracking

Track issues by ID `GN-EPIC-xx` and tasks with owner, change set, contracts touched, acceptance assertions and ADR links. An increment is complete when functional code, tests, docs, UI (where relevant), error behavior and evidence bundle are committed together. Mark `incomplete` for stub routes, dummy data or skipped integration. Bug fixes must add a regression fixture before closing.

## Final definition of done

1. Functional player path on capture and emulator + cached/live speech, correct session lifecycle.
2. Autonomous setup path with source provenance, parser/graph creation, voice auditions, approval and export.
3. Complete React admin/player/developer interfaces with real backend functionality and Playwright coverage.
4. Real providers integrated with validated capability contracts and failure/degraded behavior.
5. Repeatable Dolphin test runner (with actual screenshot and input evidence), live-inference test and fixture suite; HTML reports and saved regression cases.
6. Multi-game generic adapter demonstrates portability without Zelda-specific core logic.
7. Security, backups, diagnostics, metrics, QoS, cancellation, performance and restart correctness demonstrably tested.
8. Installable arm64/amd64 releases, OCI images, GoReleaser snapshot/tag process and upgrade/rollback runbooks.
9. `docs/adr/` history current and referenced from code/tasks; no undocumented architecture reversal.
10. Final acceptance report: each requirement ID status, actual command run, pass/fail, artifacts, environment and unverified blockers. **Never claim a real Dolphin/UNO Q test passed unless the agent executed it in a connected environment.**
