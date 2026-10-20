# ADR-0104: The global status bar's snapshot and stream (`/api/v1/status`)

Date: 2026-10-20
Status: accepted
Owners: agent worker (plum-peach global chrome, status backend); owner (review)
Related: docs/design/plum-peach "Global chrome" (the 28px status bar),
docs/spec/08 "APIs" and "Event stream reliability", ADR-0087 (the
diagnostics run store the health rollup reads), ADR-0097 (the readiness
seam the storage tile reads through), `internal/api/status.go` (the
snapshot, the renderer, the push engine, the two routes),
`internal/runtime/status.go` (the commit→voice sampler),
`internal/store/status.go` (the three indexed reads),
`internal/auth/policy.go` (the viewer grants)

## Context and constraints

The redesign puts a 28px status bar on every screen. The bar is a single
live string — `● live · capture 30fps · 412ms frame→voice`,
`● agent running · 9 of 12 voiced · 18m 04s`, `5 ok · 1 slow` — fed by one
live status stream across all pages (UX-07), composed of the active
session (game, mode, capture fps actual vs target, dropped frames, the
frame→voice p50, lines spoken), the active onboarding runs (state, stage,
characters voiced vs total, elapsed), a six-component health rollup
(capture, OCR, decision, TTS, agent, storage) and the device (version,
uptime, listen address). The redesign's hard rule: the bar is never the
thing that starts a heavy probe, and non-ok health must surface on every
page, not just a Health page.

The constraints:

- One string, everywhere: a bar that renders its own variant per client is
  a client bug nobody can name, so the server renders the canonical string
  and ships it inside the snapshot.
- The stream must be the documented SSE machinery (Last-Event-ID resume
  from the bounded log, heartbeats, resync-required gaps), not a second
  implementation.
- Up to two snapshots a second on a busy box, bounded on the push side
  (a chatty session must not push per line), and cheap on the read side
  (a viewer polling with GET must not trigger probes either).
- The box has one hub: the event stream already owns subscriber state,
  replay and fan-out, so the status stream rides on it.

## Decision

- **One typed snapshot, one canonical string.** `GET /api/v1/status`
  answers `StatusSnapshot` (session, setup_runs, health, device) whose
  `status` field is the bar's string, rendered by the server
  (`RenderStatus`, a pure function of the snapshot). Push and read share
  the same shape: a client that holds the stream sees exactly the GET's
  bytes, move for move. Priority in the string: a live session beats a
  setup run (narration is the box's foreground), and non-ok health
  appends in both cases (the slow/fail tiles, canonical order; "unknown"
  appends nothing — the rollup's "2 unknown" says the rest).
- **frame→voice is two measured halves, summed.** The pipeline's
  frame→committed histogram is whole-run (stable but cold-start-tinted);
  the session's commit→voice window is last-N (32 lines, current but blind
  to the frame half). The bar's number is each half's p50, added when both
  have samples. The sum is an *estimate* of the total's p50 (medians of
  halves are not the median of the join); the error is bounded by the
  variance of the two halves, and a status line is not the place to chase
  it. The commit→voice half samples per line by pairing
  `message.committed`'s dialogue occurrence with `speech.started`'s
  utterance occurrence (the ids the speech scheduler already carries),
  under one mutex, with the pending table capped (64) and reset on
  overflow: a run that commits without speaking sheds its anchors rather
  than grow a map for its whole life.
- **The health rollup reads cached answers only.** Storage reads the
  readiness probe (the same seam `/health/ready` answers from, ADR-0097)
  through a 30 s TTL cache — the probe runs at most twice a minute at a 2
  Hz poll. Decision, TTS and agent read the newest *answered* diagnostics
  run (ADR-0087's in-memory run store): the probe already happened when
  the run did, and the status bar touches its snapshot. The session's own
  live decision-degraded flag beats the last doctor run (it is newer).
  Capture and OCR read the live session's in-memory counters (fps actual
  vs target, dropped frames, OCR error rate). A component nobody has
  probed is **unknown**, never ok: a "5 ok" on a box that was never
  checked is the lie the level exists to prevent.
- **One push engine per process, ≤2 frames a second.** The engine
  assembles at 500 ms and publishes `status.changed` only when a coarse
  compare key (the displayed numbers: the rendered string, the session
  counters, the setup counts at 5 s buckets, the health levels) changed,
  coalesced to at most one publish per 500 ms. Uptime and `updated_at`
  are deliberately out of the key: a new timestamp for a box that did
  nothing is the churn "push on change" exists to stop. The engine is
  process-lived and started lazily by the stream route (a process whose
  clients poll by GET pays no push at all), and exits if the hub refuses
  publishes (the shutdown signal, after which the stream answers 503 like
  the event stream does).
- **The stream is a name-filtered subscription on the existing hub.**
  `GET /api/v1/status/stream` subscribes with the filter
  `Names: [status.changed]` and reuses the event stream's pump, cursor
  parsing and failure answers; `status.changed` joins the hub's closed
  name vocabulary through `events.Register` (the same seam the jobs
  runner and the region preview use), so replay, heartbeats and gap
  notices are the documented machinery. The stream is **viewer** tier —
  the bar sits on every screen, kiosk included, and it carries no
  gameplay text (a status string, counters, health levels).
- **The assembly's seam set is the whole set.** In-memory counters, the
  diagnostics run store, the TTL-cached probe, and three indexed reads
  per active setup run (`ListActiveAgentRuns`, `GameTitleForProfile`,
  `ProfileCharacterVoiceCounts`, `LatestAgentToolCall` — the last two
  cached per profile id for the session's and the run's game name). A
  seam that is absent degrades to the honest shape: no session → "no
  active session"; no hub → 503; no probe → unknown.

## Options considered

1. **Push per change, no throttle.** A session committing a line every
   200 ms would push 5 frames a second; the bar cannot use a frame it
   renders once a second, and the hub's ring would fill with frames no
   client displays. Rejected in favour of the coarse-key coalescing.
2. **The status as its own hub (a second fan-out).** A second subscriber
   registry, replay log and heartbeat clock for one event name.
   Rejected: the hub already does all of it, and a second registry is
   where "two event streams" bugs are born.
3. **A fresh doctor probe for the health tiles.** The rollup would be
   exact, at the cost of turning the bar into the heaviest probe on the
   box — the redesign's explicit anti-goal — and of racing the
   /diagnostics page. Rejected: the cached answers (the last run, the
   readiness seam) are what the operator already trusts elsewhere.
4. **p50 of the joined frame→voice samples.** Exact, but it needs the
   frame half sampled per line at the frame's wall clock (a change to the
   pipeline's histogram) for a number whose error bound the bar cannot
   display anyway. Rejected: the two-half sum is honest about what each
   half measures and costs the pipeline nothing.
5. **Client-rendered string.** The snapshot ships, the UI composes.
   Rejected: every client (and every test, and the future mobile surface)
   would own the format, and UX-07's "the string is not assembled per
   page" wants one owner.

## Positive consequences

- Every screen shows the same bar text from the same bytes; the format is
  a server contract a test can pin (`TestStatusStringPerState` pins all
  three design strings).
- The bar costs the box nothing it does not already pay: counters it
  keeps anyway, a probe it runs at most twice a minute, a doctor run
  that already happened.
- A reconnecting client is whole by construction (the hub's replay); a
  fresh client gets the baseline frame immediately, not after a tick.
- "unknown" makes an unprobed box say so instead of showing "6 ok".

## Negative consequences / risks

- The frame→voice number is a sum of two p50s, not the p50 of the join:
  it drifts from a per-line joined measurement by the variance of the
  two halves. Accepted: it is a status line, and both halves are named on
  the wire (`frame_to_voice_samples` says how many lines went into the
  commit→voice half).
- The health tiles lag reality by the cache's age (readiness 30 s;
  doctor run since its last run). The rollup is a rollup of *last
  answers*, and the /diagnostics page is where a fresh probe belongs.
- The coarse compare key can coalesce two distinct displayed states into
  one frame when both land inside one 500 ms floor. Bounded by the floor;
  the frame that arrives is the latest shape.

## How to verify (commands and evidence)

- `go test -race -count=1 ./internal/runtime/ -run FrameVoiceSampler` —
  the window bound (last 32 lines), the median, occurrence pairing, the
  pending-cap reset, the negative clamp and the unknown-voice drop.
- `go test -race -count=1 ./internal/api/ -run 'TestStatus|TestFrameToVoiceTotal|TestCaptureHealth|TestOCRHealth|TestReadinessFailure|TestFormatElapsed'`
  — the three design strings plus every other state; the setup-run
  aggregation against a real store (9 of 12 voiced, the stage, the
  pre-profile plan title, the terminal run excluded); the health rollup
  from the last diagnostics run with the readiness probe counted exactly
  once across two in-TTL snapshots; the stream's name filter, baseline
  frame and Last-Event-ID replay; the 503 without a hub.
- `TestStatusEnginePublishesOnChangeAtMostTwiceASecond` — two changes
  inside one floor publish nothing, the coalesced frame carries the
  latest shape, and an unchanged snapshot publishes nothing when the
  floor is clear.
- The OpenAPI contract gate records the GET's 200 body against the
  documented schema (`recordContractResponse`), and the enum gate pins
  the level / component / run-state vocabularies to their sources.

## Rollback/migration path

Delete the two route registrations and the `status.changed` registration
in `New()`, then `internal/api/status.go`; drop the sampler wiring
(`voiceLat`) from `internal/runtime/speech.go` and the `CaptureFPSTarget`
/ `CommitToVoice` fields from `Info`. Nothing is persisted (the run
store and the caches are process-local), so there is no migration; the
event log's `status.changed` frames are replayable noise at most and
age out of the ring.

## Supersedes / superseded by

None. The bar's front half (the 28px chrome, the font, the dither rows)
is the web redesign's; this ADR owns the backend it feeds.
