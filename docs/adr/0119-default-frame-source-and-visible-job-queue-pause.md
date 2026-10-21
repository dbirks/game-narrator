# ADR-0119: The default frame source resolves to a ready capture card, and the job queue's foreground pause is visible

Date: 2026-10-21
Status: accepted
Related requirements/tests: issue #28 (the UI's Start narrating ran `test_pattern` while the capture card was ready and selected), issue #27 (background jobs silently sat `queued` for the lifetime of a live session), docs/spec/01 "Key invariants" 1 (the foreground session wins the CPU/audio budget; background work pauses), docs/spec/08 (the session workflow; the error contract), ADR-0078 (capture product wiring: the enumeration is the allowlist; an empty device id resolves the persisted selection, then the first usable device), ADR-0140 (the Library's ▶ and its `test_pattern` start choice — the UI half this ADR makes obsolete), docs/spec/13 (the operator action lives in the answer, not in a log)
Links/references: `internal/runtime/source.go` (`buildFrameSource`), `internal/jobs/runner.go` (`SetForegroundBusy`), `internal/api/status.go`, `internal/api/jobs.go`
Supersedes / superseded by: ADR-0140's `test_pattern`-as-Library-default decision (the UI stops sending an explicit source and lets the server resolve one; the ADR stands for the endpoint and the screen)

## Context

Two surfaces said nothing about what the box was actually doing:

1. **The frame source a "Start narrating" without an explicit source ran on.** `POST /api/v1/sessions` required a `frame_source`; the Library's ▶ sent `test_pattern` (ADR-0140's explicit choice), the Play start form's picker defaulted to `test_pattern`, and on the UNO Q a ready, selected HDMI card narrated a synthetic test pattern until a session was started by hand with `frame_source: {kind: "v4l2"}` (issue #28). The product rule docs/spec/08 implies — the live capture card is the appliance's input — was not the default anywhere: `test_pattern` and `replay` are developer choices, not the appliance default.
2. **The job queue's foreground pause.** `Runner.SetForegroundBusy(true)` (docs/spec/01 invariant 1) makes background kinds pause between jobs for the lifetime of a live session, but nothing surfaced the pause: queued rows carried `attempts: []` with no reason, the job list and the status snapshot were silent, and an operator or agent polling the queue could not tell "waiting in a normal queue" from "blocked for the whole lifetime of the session" (issue #27). "Never silently" is the rule this ADR applies.

## Options

1. **Resolve the default in the API layer** (the handler reads the capture device list, picks a kind, and calls the manager with a concrete `frame_source`). Rejected: it splits device resolution across two layers that can disagree — the handler's pre-read and the session's own resolution are separate enumeration reads that race a dongle unplug, and the snapshot would then name a device the handler chose rather than one the resolution settled on. The runtime's `buildFrameSource` already owns the ADR-0078 chain through the wired `CaptureOptions.Resolve` closure; the default belongs there.
2. **An `Options.DeveloperMode bool` wired from `cmd/game-narrator`.** Rejected for now: the process-level developer opt-ins in this codebase are environment-gated at the layer that acts on them (`GAME_NARRATOR_DEV_DOLPHIN` in `internal/dolphin`, the ADR-0074 fakes in their own packages), and the frame-source default is runtime behaviour — `buildFrameSource` already receives a `lookupEnv` seam (nil → `os.LookupEnv`) for the dolphin gate, so an env gate there needs no new wiring and cannot drift from the process that acts on it.
3. **Persist the pause reason on the job rows** (a new `paused_reason` column plus a migration, per issue #27's suggested shape). Rejected: the pause is process- and session-lifetime state of the worker pool, not a property of any row — every queued row shares it, and it disappears when the session ends, so a column would need writing and clearing on every pause transition of every row. A page-level object on the job list plus the status line carries the same information with no schema change (AGENTS.md rule 10: migrations only for schema changes that persist).

## Decision

### The default frame source (issue #28)

`POST /api/v1/sessions` accepts an omitted or empty `frame_source` (the OpenAPI schema stops requiring it); the request's `profile_id` stays the only required member. The runtime resolves the empty kind in `buildFrameSource`, in the source-resolution home, with this rule:

1. **A usable capture device wins.** The ADR-0078 chain runs through the process's capture wiring (persisted selection, then the first usable device) and the session runs `v4l2` on the device it settles on. The session snapshot's `frame_source` and `capture_device`/`capture_mode`, the `session.started` event's `frame_source` object, and the status bar's `source` line all carry the **resolved** kind and device — a default is visible in the run's own evidence, not guessed from the request (the ADR-0078 "visible, not guessed" rule, extended from device fallback to kind defaulting).
2. **No usable device (or no capture wiring) — developer mode only.** With `GAME_NARRATOR_DEV=1` the session runs `test_pattern` (the built-in synthetic source, real frames through the production consumer). The env is the process-level developer mode, in the family of `GAME_NARRATOR_DEV_DOLPHIN` and the ADR-0074 fake flags; on the appliance it is unset.
3. **No usable device, developer mode off — 409.** The answer is `409` with code `NO_FRAME_SOURCE` (type `no-frame-source`) and a detail that names the operator action: connect the capture card and start again, or name the frame source explicitly. A conflict, not a 503: the request is valid and the box is in a state that cannot satisfy the default. The **explicit** `kind: v4l2` answer with no device is unchanged (503 `DEVICE_UNAVAILABLE`) — an explicit choice names the failure; the default names the action.
4. **A device that is present but offers no policy-fitting mode** keeps its `503 capture-mode-unavailable` through the default path: a broken card is never hidden behind a silent synthetic session.
5. **An empty kind with a `dir` or `clip_id`** is a 400 (`incomplete-frame-source`): a path or a clip id without a kind is ambiguous, and the remedy is to name the kind.

The UI half is a follow-up (this change is backend only): the Library's ▶, the Play start form and the agent action omit the source instead of sending an explicit `test_pattern`, and the start form's "which source will run" line reads the resolved kind from the started session's snapshot.

### The visible foreground pause (issue #27)

- `Runner` records when the pause began: `SetForegroundBusy(true)` stamps `since` (idempotently — a repeated true does not move the clock) and `SetForegroundBusy(false)` clears it. `Runner.ForegroundPause(ctx)` reports `paused`, `since`, and `queued` — the count of `queued` rows read from the queue (`JobsStore.CountQueued`, a new read mirroring `QueueDepth`; the database is the authority on job state, so the count is a query, never a counter kept on the enqueue/complete paths).
- The API wires the runner through a small seam (`WithJobRunner`; `*jobs.Runner` satisfies it, a fake does the tests) and exposes the pause as one object, `{paused_by: "live_session", since, queued_count}`, in two places:
  - `GET /api/v1/status` (and the `status.changed` stream frames, which are the same snapshot): `jobs`, present only while the pause is active — the 2 Hz push engine publishes when the displayed shape changes, so the pause appears when the session starts and vanishes when it ends, and `RenderStatus` appends a `jobs paused` tail to the bar's string.
  - `GET /api/v1/jobs`: `jobs_paused` at the page level, so a poller of the queue sees the pause in the same round trip as the rows it paused.
- The pause's `paused_by` is `live_session` today because the session runtime is the only caller of `SetForegroundBusy`; the enum-shaped value keeps the door open for a second pausing subsystem without a shape change.
- Per-row `paused_reason` annotations and the onboarding-run view's hint (issue #27's fuller suggested shape) are a follow-up: the page-level object and the status line satisfy "an operator (or an agent) polling the queue cannot tell" — the information is on both surfaces an agent polls.

## Consequences

- A "Start narrating" with no source on a card-equipped appliance runs the card; the snapshot, the `session.started` event and the bar say `v4l2` plus the device. On a card-less appliance the start is refused with the operator action in the body — the dead synthetic session of issue #28's mirror case is gone from the default path.
- Developer hosts keep their card-less workflow via `GAME_NARRATOR_DEV=1`; an appliance operator who wants the pattern explicitly still sends `kind: test_pattern` (the explicit kind is unchanged by this ADR).
- The 2 Hz status engine now reads one count from the queue while a session is live (the pause is only then active) — a bounded indexed read, the same class the status bar's other seams pay.
- The `GAME_NARRATOR_DEV` flag is new vocabulary for the env surface: docs and the doctor surface should learn it when they next touch the env list.

## Validation

- Runtime unit tests, every branch of the default resolution: selected/only device → `v4l2` (the snapshot's `frame_source` is the resolved kind and `capture_device` names the device); no device + `GAME_NARRATOR_DEV=1` → `test_pattern`; no device + no developer mode → the 409 sentinel; an empty kind with a `dir`/`clip_id` → the 400 sentinel; the explicit-kind answers unchanged.
- Runner unit tests: the pause state and `since` stamp/clear, the queued count from the store, the not-paused shape.
- API tests: the 409 problem body (code `NO_FRAME_SOURCE`, the "connect your capture card" detail) and the 201 whose `frame_source` is `v4l2` with no source named; the status snapshot's `jobs` line and its absence when idle; the job list's `jobs_paused` object; all success bodies re-validated against the updated OpenAPI document by the contract gate.
- `gofmt`/`go vet` clean; `go test -race ./internal/api/... ./internal/jobs/... ./internal/runtime/...` (the API package's ADR-0105 leak gate included).

## Reversal plan

Drop the `case ""` branch and the two new sentinels from the runtime, the `jobs`/`jobs_paused` fields and the seam from the API, `ForegroundPause`/`CountQueued` from the jobs/store packages, and the one wiring line from `cmd/game-narrator`. The OpenAPI edits revert with the shapes. No data or schema change is involved (the store addition is a read), so the reversal touches no migration.
