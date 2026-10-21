# 08 — Go service HTTP API, events, long-running jobs and versioning

## Conventions

Base `/api/v1`; JSON UTF-8; RFC3339 UTC timestamps; UUID/ULID identifiers; `Idempotency-Key` for create or expensive requests; RFC 9457 problem+json error objects (`type,title,status,detail,instance,request_id`); ETag/If-Match for profile revisions; pagination with cursor and hard upper bounds. Browser session secured with same-site httpOnly cookie; API token for integration CLI. State-changing calls require CSRF protection under cookie auth.

## APIs (minimum complete set)

| Method/path | Purpose |
|---|---|
| `GET /api/v1/health/live`, `GET /api/v1/health/ready` | process vs dependencies health |
| `GET /api/v1/system/info` | build/OS/device capacities, no secrets |
| `GET /api/v1/settings`, `PUT /api/v1/settings` (405 by design) | redacted settings; no general runtime-mutable setting — change the environment variables and restart (ADR-0047); the one runtime-mutable setting is the audio output selection and master volume via `PUT /api/v1/settings/audio` (ADR-0074). Replaces the original `GET/PUT /api/v1/config` row, whose deviation is decided and kept by ADR-0133 (single-operator LAN appliance; env plus restart) |
| `POST /api/v1/diagnostics/run`, `GET /api/v1/diagnostics/:id` | runnable preflight and results |
| `GET /api/v1/capture/devices`, `POST /api/v1/capture/probe` | UVC device list, mode negotiation |
| `GET /api/v1/capture/preview` | throttled MJPEG/snapshot, auth, privacy |
| `POST /api/v1/sessions`, `GET /api/v1/sessions/:id`, `POST .../stop` | session lifecycle |
| `POST .../pause`, `POST .../resume`, `PUT .../settings` | capture/speech mode controls |
| `GET /api/v1/games`, `POST /api/v1/games` | library and edition metadata |
| `GET/POST /api/v1/games/:id/profiles` | versioned drafts |
| `POST .../import`, `POST .../preview`, `POST .../publish` | staged source/profiles |
| `POST .../export`, `GET .../download` | manifest export with rights filters |
| `GET/PATCH /api/v1/profiles/:id/messages/:id` | original vs speech text edits, immutable source refs |
| `GET/POST /api/v1/profiles/:id/characters` | characters and speaker assignments |
| `GET/PUT /api/v1/profiles/:id/regions` | OCR layouts/region revisions |
| `GET /api/v1/voices`, `POST /api/v1/voices/auditions` | voices, design candidates |
| `POST /api/v1/voices/:id/approve`, `POST .../register`, `POST .../preview` | voice lifecycle |
| `GET /api/v1/voices/:id/registrations` | replica-level registry state |
| `POST /api/v1/onboarding/runs`, `POST .../messages` | start and chat with agent |
| `GET .../state`, `POST .../pause`, `POST .../resume`, `POST .../approve` | durable job and review |
| `GET /api/v1/jobs`, `GET /api/v1/jobs/:id`, `POST .../cancel`, `POST .../retry` | durable workflow queue |
| `POST /api/v1/corrections`, `GET /api/v1/corrections` | user fixes become labels |
| `GET /api/v1/dev/events`, `GET /api/v1/dev/traces/:id` | correlated dev inspection |
| `POST /api/v1/dev/fixtures`, `GET /api/v1/dev/fixtures` | fixture recording and browsing |
| `POST /api/v1/dev/test-runs`, `GET .../report` | run suite and inspect artifacts |
| `GET /api/v1/events` | SSE multiplex, Last-Event-ID |
| `GET /api/v1/status` | one snapshot of what the box is doing (session, setup runs, health, device) plus the canonical status string the bar renders (ADR-0104) |
| `GET /api/v1/status/stream` | the status snapshot's on-change push: SSE of `status.changed`, Last-Event-ID, ≤2 frames/s (ADR-0104) |
| `GET /api/v1/openapi.json` | generated OpenAPI contract |

Design nested resource URLs consistently; implementation may rationalize exact forms through ADR. Use document-level JSON schemas and generate TS clients from OpenAPI to avoid manual drift.

## Example session workflow

`POST /sessions {profile_id, frame_source_id, audio_sink_id, mode:"conservative"|"predictive"}` → `201 {session_id, state:"starting", event_cursor}`.  
`GET /events?session_id=...` (SSE) yields `frame.observed`, `text.observed`, `message.candidates`, `decision.resolved`, `speech.started` with common trace and chronological seq.  
`POST /sessions/{id}/stop` idempotently sets state to stopping, cancels speech, closes V4L2/audio handles and yields `session.stopped`.

## Event stream reliability

Emit `id: <session-or-global-cursor>` and JSON event envelope; heartbeat comments; Last-Event-ID replay from bounded event log, otherwise send `events.resync_required` and use REST snapshot. Partial data redacted when not in developer mode. Disconnecting browser never stops playback (unless user explicitly clicked stop). Backpressure policy drops redundant frame previews first, not state transitions or critical speech events.

## Agent chat interface

Messages can include user intent, semantic edits, tool results and approval request. Client receives streamed `agent.step`, `tool.started`, `tool.completed`, `review.required`, `source.proposed`, `profile.diff`, `voice.audition.ready`. The agent operates against draft `profile_revision`; optimistic locking prevents stale editor overwrites. Agent should ask for one specific missing asset/permission when blocked, with clear explanation and resume button.

## API version and user authentication

Token/session scopes: `viewer`, `curator`, `operator`, `developer`, `admin`. Developer mode can display sensitive gameplay text and provider traces; require explicit role. Default bind to localhost, or LAN with configured auth and TLS reverse proxy. Rate-limit agent web search, file import, inference calls and preview streaming. Store provider keys as sealed/env-sourced server-side config; return masked labels only.

## Error contract examples

`DEVICE_UNAVAILABLE` for removed capture, `VOICE_NOT_REGISTERED` with replica ID and retryable flag, `MODEL_UNSUPPORTED`, `PROFILE_REVISION_CONFLICT`, `CONTENT_RIGHTS_REVIEW_REQUIRED`, `UNKNOWN_GAME_EDITION`, `JOB_REQUIRES_INPUT`, `STALE_SESSION_EVENT`. Frontend presents recoverable action and correlation ID, not raw stack trace.
