# ADR-0024: Session runtime manager, replay/test-pattern frame sources, and quota-bounded developer trace storage

Date: 2026-10-17
Status: accepted
Owners: project
Related requirements/tests: docs/spec/01 "Key invariants" (one active session at a time; only the published revision runs; the foreground session wins the CPU/audio budget), "Slow browsers cannot stall capture"; docs/spec/08 "APIs" (session lifecycle; dev/events; dev/traces), "Example session workflow", "Event stream reliability", "API version and user authentication" (developer mode displays sensitive gameplay text and provider traces; require explicit role); docs/spec/13 "Logging/privacy" (developer trace sample mode stores frame crops/audio under time/size quotas and an explicit toggle); docs/spec/12 (a replay must not depend on machine speed); docs/spec/14 (no arbitrary filesystem paths; role-gated dev surfaces); builds on ADR-0005 (pipeline and capture seams), ADR-0014 (content-addressed asset store), ADR-0016 (scope table)

## Context and constraints

- Milestone 2 needs the developer inspector and the live session runtime:
  `POST /api/v1/sessions`, `GET /api/v1/sessions/{id}`,
  `POST /api/v1/sessions/{id}/stop`, plus
  `GET /api/v1/dev/events`, `GET /api/v1/dev/traces/{trace_id}` and
  `GET /api/v1/dev/assets/{sha256}`.
- The pipeline (ADR-0005) already runs one recognition session end to end;
  what it does not own is *which* profile revision runs, the frame source,
  the one-active-session invariant, the lifecycle events, or the evidence a
  developer inspects afterwards. That ownership must live in exactly one
  place or the invariants become per-callite promises.
- A replay session is the M2 acceptance path: the synthetic clip in
  `testdata/synthetic/replay/dialogue-advance-001` plus its published
  profile must produce the two documented commits
  (`demo-page-1`, `demo-page-2`) without depending on machine speed
  (docs/spec/12). The pipeline's frame-replay gate already proves this for
  `ModeReplay` at `Speed: 0`; a session that re-paces at wall clock would
  reintroduce the race the gate exists to forbid.
- A session's run must outlive the HTTP request that started it (a browser
  closing its tab does not stop playback, docs/spec/08), and stopping must be
  idempotent and answer with the terminal snapshot (docs/spec/08 "Example
  session workflow").
- Developer trace storage is privacy-sensitive (docs/spec/13): sampled
  gameplay frames and provider traces may only exist behind an explicit
  toggle held by a developer role, under a per-session byte quota with
  oldest-first eviction. v4l2 and dolphin frame sources are not implemented
  in this build and must answer 501, not fake a device.

## Decision

### The runtime package owns the session

`internal/runtime` exposes a `Manager` — the only place a pipeline run is
started or stopped — over the existing seams (`store.Store`, `events.Hub`,
`jobs.Runner`, `ocr.Engine`):

- `Start` validates (mode, source kind, one-active-session), loads the
  profile and refuses it unless it *is* the edition's published revision,
  builds the matcher index from the published content rows, opens the frame
  source, and publishes `session.started` before the run goroutine begins.
  The returned snapshot is state `starting`; the run loop flips it to
  `running`, and the manager — not the pipeline — publishes the single
  terminal `session.stopped` (reason `requested` / `source_end` /
  `error` / `closed`) or `session.failed` before it. A second terminal
  event would make "did it end and why" ambiguous in the log.
- The run context is detached from the request context, so a disconnected
  client never stops the session; `Stop` cancels the run context and waits,
  bounded, for finalization, so the response is the terminal snapshot.
- While a session is starting/running/stopping, the `jobs.Runner`
  foreground-busy flag is set, so background work pauses between jobs for
  the lifetime of the session (docs/spec/01: the foreground session wins
  the CPU/audio budget).
- `Close` (the server's shutdown hook) stops live sessions with reason
  `closed` and is ordered *before* the hub closes in `main`, because a run
  finalising on shutdown still publishes its `session.stopped`.

The API layer (`internal/api/sessions.go`) is a thin mapping of one manager
call to one documented problem code; it re-interprets no free text. The
scope table keeps `POST /sessions` and `…/stop` at operator, and adds a
viewer grant for `GET /sessions/{id}` (placed before the generic operator
rule, since the table is first-match): the snapshot is profile, mode and
counters — no gameplay text, which stays on the developer-only surfaces.

### Frame sources and the replay allowlist

`StartRequest.FrameSource` names one of `replay`, `test_pattern`, `v4l2`,
`dolphin`. `v4l2` and `dolphin` answer 501 `unsupported-capability`: this
build does not implement live capture, and a 201 that claims it would be
the simulation theater AGENTS.md forbids. `replay` and `test_pattern` run
the real pipeline; replay uses `pipeline.ModeReplay` at `Speed: 0` (the
source clock drives the tracker, so the outcome is a pure function of the
frames and the engine), with the tracker tuned to the 10 Hz fixture cadence
(100 ms stability window, 3-frame vote) — documented settings, not fixture
branches. Live sources keep `recognition.DefaultConfig()`.

A replay directory is admitted only when, after full symlink resolution on
both sides, it sits at or below one of the configured roots
(`runtime.DefaultReplayRoots`): an explicit
`GAME_NARRATOR_REPLAY_ROOTS` (comma-separated, absolute) wins; otherwise
`<cwd>/testdata` when the process runs from a directory that has one (the
dev checkout); otherwise nothing (production). Traversal and symlink escape
both fail the prefix test, and existence is checked first, so a missing
path reads as `source-missing` and an existing-but-foreign one as
`source-not-allowed`.

### Developer trace mode

`dev_trace` on `POST /sessions` requires the caller to hold the developer
role (checked in the handler: the route is operator, the evidence is
developer evidence, and an operator must not silently switch on frame
storage). Only then is a `devTraceRecorder` constructed — per session, in
its own content-addressed store (`assets.Local`, private 0700 layout,
ADR-0014) under `<assets-root>/<session-id>`:

- Full frames are stored downscaled to at most 640 px wide as JPEG q80;
  ROI crops are stored as raw PNG.
- A time gate (≥250 ms between samples of a changing picture) bounds the
  rate, and a per-session byte quota (default 64 MiB) with oldest-first
  eviction bounds the size; evicted samples are pruned from the lookup
  table so `text.observed` never references bytes that no longer exist.
- `text.observed` payloads gain `frame_asset` and `crop_asset` sha256
  references resolved to the nearest stored sample at or before the
  observation's capture timestamp (the sampling gate makes "nearest kept
  sample" the honest answer, not a fabricated exact match).
- `GET /api/v1/dev/assets/{sha256}` is the only read path into those
  stores (served with the stored content type); the whole `/api/v1/dev`
  prefix is developer-role in the scope table, which is the privacy
  boundary (docs/spec/13). Without the toggle no recorder exists and the
  pipeline stores and references no images at all — privacy by absence,
  not by filtering on the way out.

`GET /api/v1/dev/events` pages the durable event log (the store the hub
persists into, not the ring) with `session_id` / `name` / `since_seq` /
`limit` (1..1000) filters; `GET /api/v1/dev/traces/{trace_id}` pages the
same store and selects by trace, bounded by 50 store pages per lookup, so
the inspector reads what already happened without holding the appliance.

## Tradeoffs

- A thin API layer over a fat manager means the HTTP semantics (status
  codes, problem slugs) are decided once in `sessions.go`'s mapping table,
  and every new sentinel error must get a row there — a new runtime error
  that the table cannot name falls through to a 500 `internal-error`,
  which is the honest answer.
- Trace lookup by "nearest stored sample at or before the timestamp" trades
  exactness for honesty under the sampling gate: a developer who wants the
  exact frame of one OCR read sees the closest one the recorder kept, and
  the sample's own timestamp is in the same event stream.
- The dev asset store is per-process (sessions live until `Close`), so a
  restart drops evidence for sessions that were not finished and read back
  before the restart. Durable trace archives are a later milestone's
  problem; the ADR records the boundary so a future archive cannot
  silently change the read path's promises.
- One `Manager` per process enforces the one-active-session invariant by
  construction; a future multi-session build must either relax the
  invariant in the manager (with an ADR) or run one process per session.

## Validation

- `internal/runtime`: real store + hub + manager over the synthetic clip —
  validation (unknown profile 404, unpublished 409, invalid mode 400,
  unknown source 400, v4l2/dolphin 501, disallowed/missing replay dir 400,
  dev-trace toggle with no assets root 503), one-session-at-a-time 409,
  idempotent stop, published-revision-only start, replay→two commits
  (`demo-page-1`, `demo-page-2`) with fake OCR and with real Tesseract,
  dev-trace on/off (no images stored or referenced without the toggle),
  quota eviction, and foreground-busy pausing a real `jobs.Runner`.
- `internal/api/sessions_test.go`: the same environment at the HTTP
  boundary — every status/problem slug above, the viewer-read ACL split,
  the 401/403 gates, and the inspector round trip (dev/events →
  trace → asset bytes with content type).
- `cmd/game-narrator`: `TestServeWiresTheSessionRuntime` runs `serve` as
  the process runs it — migrate, CLI-prepared credentials and profile,
  `GAME_NARRATOR_REPLAY_ROOTS`, a developer replay session with the
  production Tesseract engine, terminal state, dev-trace assets read back
  through the wire.
- Contract: the new operations are in `openapi.json` with `x-required-role`
  matching the scope table (the existing drift tests cover them); the
  enums are pinned to the runtime constants in the enum audit test.

## Consequences

- The developer inspector (docs/spec/09) has its backend: correlated
  events, traces and stored evidence behind the developer role gate.
- Live capture (`v4l2`) and emulator (`dolphin`) sources remain 501 until
  their milestone implements them; the API and the allowlist rules they
  must satisfy are already in place.
- Dev-trace evidence is private to the process and to the developer role;
  nothing leaves the box and nothing is stored without the explicit toggle.
- A later audio-archive or durable-trace milestone must reuse the
  content-addressed layout and the quota/eviction rules above rather than
  invent a parallel one.

## Reversal

Drop the three route registrations and the two scope grants, delete
`internal/runtime` and the `WithSessions` / `WithDevEvents` options. The
pipeline, store, event, asset and jobs packages are untouched by this ADR,
so no data migration is owed; stored dev-trace directories are disposable
by definition and may be removed with the feature.

## Amendment: pinned dev-trace evidence (2026-10-21)

Issue #46 amended the developer-trace retention rule this ADR set. The
rolling, byte-bounded store kept only the newest evidence, so the frames
and crops of committed and spoken lines were evicted within minutes by
idle frames — the "why did OCR read X" record the operator was holding
was gone before it could be opened.

**Decision.** A per-session pinned-evidence budget: when a `text.observed`
read led to a `message.committed` (which is also how an unmatched
read-through speaks), the dev-trace recorder pins the committing read's
frame and crops plus the last `devTracePinContextReads` retained reads
before it, and the rolling sweep skips pinned blobs. The pinned budget is
`DevTracePinnedQuotaFraction` (0.25) of the session quota, and the pinned
budget — not the rolling quota — owns the pinned bytes: once it overflows,
the oldest pinned evidence is evicted first, oldest-pinned-first, which
is the mirror image of the rolling oldest-first sweep. Everything else
about the store is unchanged (per-session quota, oldest-first idle
eviction, sampling gate, blob bound, `text.observed` enrichment).

**Wiring.** The `tracePublisher` (the runtime's dev-trace event boundary)
resolves a `text.observed` read to its stored sample, remembers the read a
later `message.committed` answers, and pins through the recorder on the
commit — the read that committed, and the last reads before it. The
recorder's `PinSample` books the frame and crops against the pinned
budget, evicts oldest-pinned-first on overflow and prunes the sample
ledger together with the store, so ledger, samples and on-disk blobs
cannot drift apart.

**Tradeoffs.** Pinned bytes stay inside the rolling total while pinned, so
the rolling quota is a ceiling on everything the session keeps — pinned
or not — and the pinned budget is the ceiling on how long pinning may
hold. The pin context window is a bounded cost: the committing read is
always pinned, and the last three reads before it ride along, so a
commit's context survives the same idle sweep without unbounding the
budget per commit.

**Validation.** `internal/runtime` dev-trace tests: `TestDevTracePinnedCommitSurvivesIdleStream`
(a long idle stream after a commit leaves the commit's crop retrievable
through the store's read while the rolling quota still holds), `TestDevTracePinnedBudgetBounds`
(the pinned budget bounds pinning; the oldest pinned evidence is
evicted first and the newest pinned survives), and `TestDevTraceIdleEvictionUnchangedAfterPin`
(the pin holds the pinned read out of the sweep without switching the
idle sweep off). The pre-existing `TestDevTraceQuotaEvictsOldest` keeps
covering the rolling idle eviction itself.

**Reversal.** Drop the `tracePublisher` pin hook and the recorder's pinned
state; the rolling sweep reverts to evicting the oldest retained blob
unconditionally and no pinned bytes are ever booked.
