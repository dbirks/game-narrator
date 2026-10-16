# 09 — React/Vite frontend, interaction contracts and developer mode

## Technology

React + TypeScript + Vite; lightweight UI component system (Radix-style accessible primitives or similar) and TanStack Query for API state, React Router for pages. Use a small shared application store for live session state and SSE event reducer. Generate typed REST client from OpenAPI. Build `web/dist` at compile time and embed through `//go:embed` in Go, with SPA fallback restricted to frontend paths. In development Vite proxy targets Go API; production has no Node runtime requirement.

## Routes and page requirements

- `/setup` first-run wizard: detect device, configure auth/storage/gateway roles, run test audio, review data/privacy settings.
- `/library`: games grid/list with sourced art or placeholder, edition/version badge, profile state, recent activity, actions.
- `/games/new`: conversational setup agent interface with streaming tool timeline, web-source candidate cards, permissions prompts, import upload, progress and resume.
- `/games/:id/overview`: edition/coverage report; source provenance, imported text count, missing actors, unresolved errors, version history.
- `/games/:id/dialogue`: searchable paginated message explorer; original/rendered/OCR example, control AST, neighbors and speaker candidates; manual correction and bulk annotate.
- `/games/:id/graph`: virtualized scene/message graph, branch/loop/repeat edges, confidence and source labels; filter by speaker, message kind and scene.
- `/games/:id/characters`: portrait, verified context, editable voice brief, pronunciations, assigned dialogues and conflict review.
- `/games/:id/voices`: candidate auditions side by side, waveform/audio player, voice approval, version comparison, registry health and generation queue.
- `/games/:id/regions`: capture image, drag/select bounding boxes, preview OCR variants and textbox events, aspect/resolution presets and test-on-fixture.
- `/games/:id/publish`: diff draft/published, provenance conflicts, source rights and voice approval gate, publish transaction and rollback to previous published revision.
- `/play`: active session view, capture status, selected profile, volume/speech delay, pause/mute, narrated classes, conservative/predictive toggle.
- `/developer`: rich real-time events and screenshot timeline, ranked candidates/Clef probability graphs, model payload inspector (redacted), audio waveform, decision/rule reasons, correction/fixture actions.
- `/tests`: scenario catalog/run controls, Arch Dolphin readiness, frame replay mode, live provider/hardware test status, HTML report viewer/download.
- `/diagnostics`: video input modes, power/environment warnings, CPU/RAM/storage, network and each model role, voice registry replication, speaker test, system logs (redacted).
- `/settings`: model endpoints, storage limits, security, profiles/import policies, audio sinks, backup/export, software updates.

## Developer dashboard layout

Desktop: live 4:3 preview with drawn ROI boxes on left; right side searchable time-sequenced event table. Selecting one frame shows OCR raw/normalized/speech text, top-N dialogue candidates with rank component bar/table, graph belief, speaker choice and `unknown`, Clef input question schema and returned probabilities, selected voice/profile version, asset hit/miss, pending TTS/ASR status, actual playback start/stop. Link everything using `trace_id` and `dialogue_occurrence_id`. Pin a particular event/frame despite stream advancement. Buttons: “speaker incorrect,” “text incorrect,” “wrong region,” “not narratable,” “duplicate,” “prediction misfire,” “save regression fixture,” “re-run against current model,” “download evidence.”

## E2E UI verification requirements

Playwright tests exercise all core routes; start a session, ingest a synthetic frame source, assert OCR/decision/speech event IDs appear in dev mode, make a correction, verify DB and API state, save a fixture and inspect a test report. Agent setup UI test starts background workflow, views multiple tool events, introduces ambiguous source, supplies approval, publishes profile and reopens it after restart. Voice test auditions alternatives, chooses a version, verifies provider registration and plays approved audio. Device page warns correctly when no UVC card attached rather than crashing.

## UX robustness

- Keyboard accessible tabs, dialogs, interactive graph alternative in table view, captions/text equivalents to voice clips.
- Correct empty/loading/error states; highly visible attribution provenance and “inferred” vs “verified” labels.
- Fast large text dump browsing: server pagination, virtualized rows, search indexing; don't load thousands of messages into React memory.
- Session survives browser refresh; SSE reconnect, event replay and state reconciliation; no duplicate speech due to UI remount.
- Privacy controls: disable screenshots in event history and browser caching, session export explicit.
- Long jobs can be left/rerun; dashboard shows queue, checkpoints, cost/call count, blocked reason and actionable continuation.

## Embedded binary test

CI builds Vite assets, bundles and compiles Go for Linux amd64/arm64, serves them using `embed.FS`. Test SPA deep links after server restart, MIME types, compression/cache-control, CSP, hashed asset integrity, and no dependence on dev Vite process in published binary. Vite-generated files need deterministic build ordering and no committed private environment vars.
