# ADR-0108 — Setup board: typed SSE stage events and a durable board snapshot (UX-06)

## Context

The plum-peach redesign's setup page (`/games/new`, docs/design/plum-peach
README §2c, product-notes UX-06) shows each character in one of four
columns — FINDING · BRIEF · VOICING · DONE — plus a six-cell stage rail
(Find game · Dialogue · Characters · Briefs · Voices · Publish) and three
stat tiles (lines read · characters · voiced). UX-06's rule is that cards
move **only** on server SSE stage events; the page never polls board state.
ADR-0093 made the API/SSE a first-class shared surface; this ADR is the
backend half of that page.

Three properties have to hold at once:

1. **Real transitions only.** A stage event is emitted when the underlying
   durable fact changed — a character row was staged, a brief tool call
   succeeded, a TTS take was generated, a voice revision was approved —
   never on a timer or a UI guess.
2. **Replayable.** A browser that loads the page mid-run (or reconnects with
   `Last-Event-ID`) must be able to reconstruct the board from the event
   stream. That requires the events to be typed, orderable and idempotently
   applicable, and the same stream to survive the hub's ring and the
   durable `events` table (SSE Last-Event-ID already replays from them).
3. **Snapshot ≡ replay.** A page load that cannot trust the stream (fresh
   load, pruned log) asks for `GET /api/v1/onboarding/runs/{id}/board`. The
   snapshot is computed from the same durable facts the events describe, so
   it is equal to the board a client would assemble by replaying the events
   — the test asserts exactly that equality.

## Options

- **A. Polling board endpoint.** One `GET /board` and a 5-second interval.
  Rejected: UX-06's acceptance explicitly forbids polling requests while the
  board updates, and the SSE hub already exists and already replays via
  Last-Event-ID.
- **B. Free-form `agent.step` payloads.** Reuse the agent loop's existing
  step events and let the UI pattern-match tool results. Rejected: the event
  vocabulary in internal/events is closed and typed by name (an unknown name
  is a 400 at subscribe time), and pattern-matching on untyped payloads is
  exactly the drift the typed-envelope rule exists to prevent.
- **C. Two typed `setup.*` events, emitted at the durable transition points,
  plus a derived snapshot.** (Chosen.)

## Decision

### Event vocabulary

Two new event names join the events registry (`internal/setupboard`
registers them at package init, the same idempotent pattern the assistant
dock uses for its card events):

- `setup.character_stage` — one character moved stage.
  Data: `{run_id, character_id, name, stage, lines, at}` with
  `stage ∈ {finding, brief, voicing, done}` and `lines` the character's
  cited-line count.
- `setup.progress` — the run-level tile/rail state.
  Data: `{run_id, lines_read, characters, voiced, total, elapsed,
  stage_rail}`. `stage_rail` is the six cells `{id, label, state, value}`
  with `id ∈ {find_game, dialogue, characters, briefs, voices, publish}` and
  `state ∈ {pending, active, done}`; `voiced` counts characters at stage
  `voicing` or later (a take exists; `done` means the take is approved),
  `total` the character count, `elapsed` seconds since the run was created.

`run_id` rides in both payloads: the SSE stream is global (one hub for
every run and session), so a payload that cannot name its run is unboardable
by a client.

### Transitions (who emits what)

| Durable fact                                   | Stage    | Emitter                                            |
| ---------------------------------------------- | -------- | -------------------------------------------------- |
| `stage_character` tool succeeds                | finding  | onboarding agent loop hook                         |
| `analyze_character` tool succeeds (brief)      | brief    | onboarding agent loop hook                         |
| `design_voice` tool succeeds (TTS takes)       | voicing  | onboarding agent loop hook                         |
| `voice.auditions` job fills auditions          | voicing  | voices service observer (the durable job path)     |
| voice revision approved (human or auto-cast)   | done     | voices service observer (UX-03 auto-cast re-uses the same `Approve` consent gate with the profile default, so it lands here too) |

The agent hook is a new `SetupStages func(runID, profileID, toolName string,
res agent.Result)` field on `agent.Options`/`agent.JobConfig`, called by the
loop after a **succeeded** tool call (before the review-boundary stop, so a
`design_voice` that stops the run for approval still reports its stage). The
loop stays agnostic: it hands the tool name and the result envelope, and the
observer decides what is a transition. A nil hook is a no-op, so the agent
core and every other agent consumer (assistant, tests) are unchanged.

### Monotonic, state-derived stages

Stage emission is guarded by the **derived** stage, not by the event
history: before emitting, the emitter recomputes each character's stage from
durable rows —

- `done`: an approved voice revision is current (no newer take supersedes
  it; regeneration opens a newer draft, which reads as `voicing` again —
  UX-03d);
- `voicing`: a succeeded `design_voice` call for the character id, or a
  voice revision with at least one audition for the character name;
- `brief`: a succeeded `analyze_character` call for the character id;
- `finding`: the character row exists.

and emits only when the new stage advances the derived one. The same
derivation serves the board snapshot, which is what makes "snapshot equals
replayed events" a property of the code, not of a particular test run.
Regression is refused (a stale retry or a re-run of an old call cannot move
a card backwards), and the event stream is therefore idempotently
re-applicable.

### Run resolution for the voices path

The `voice.auditions` job and the approve route know the character **name**
(voice revisions are keyed by name, matching the existing
`/api/v1/voices?character=` surface), not a run. The emitter resolves the
board the event belongs to by finding the most recently updated non-failed
onboarding run whose profile contains a character of that name (new store
queries `AgentStore.LatestAgentRunByCharacter` + `Store.ProfileIDsForCharacter`).
Voices created outside any setup run (a Cast page on a published game) have
no board: the transition is skipped, which is correct — the board is the
setup page, and the Cast page reads its state from the voices API.

### Snapshot

`GET /api/v1/onboarding/runs/{id}/board` (curator, inside the existing
`/api/v1/onboarding` prefix grant) returns
`{run_id, state, created_at, characters:[{character_id, name, stage, lines,
at}], progress:{…setup.progress payload…}}`. `at` is the `occurred_at` of
the character's newest `setup.character_stage` event read from the durable
event log (the same rows the SSE replay serves); a character with no
recorded event (hub unwired) carries no `at`. All other fields are derived
from the durable rows above, so every field the snapshot shows is a field
some `setup.*` event also carries.

### Replay

No new machinery: both names register in the events vocabulary, travel the
hub's ring and the durable `events` table, and are therefore replayed by the
existing `Last-Event-ID` path (`GET /api/v1/events?names=setup.character_stage,
setup.progress`). `setup.*` events are **not droppable**: a slow subscriber
that lost one must get a resync, never a silently moved card.

## Tradeoffs

- **Per-character store reads on every stage-relevant tool call.** Bounded:
  the onboarding loop is sequential, the reads are indexed point lookups,
  and the run's tool budget (48 calls) caps the count. Simpler and more
  honest than an in-memory stage cache, which would disagree with the
  snapshot after a resume.
- **Name-keyed voice revisions mean name-matched run resolution.** Two
  profiles with the same character name can collide; the latest non-failed
  run wins. The collision is a pre-existing property of the voices surface
  (ADR-0101) and the setup page is the only consumer that needs it.
- **`setup.progress` is emitted once per character-stage transition**, not
  on a timer: the rail's `elapsed` value is as fresh as the last transition.
  A run that idles in review shows a slightly stale `elapsed` until the next
  transition or the snapshot read — acceptable for a setup page, and it
  avoids hub churn the status push loop specifically exists to prevent.

## Validation

- `internal/setupboard` unit tests: stage derivation over real store rows
  (staging, brief call, design call, approved + superseding revisions),
  rail state computation, and the monotonic guard.
- `internal/api` integration test (category (a), scripted model + real
  tools): a scripted onboarding run asserts the `setup.character_stage`
  order `finding → brief → voicing` per character and a `setup.progress`
  after each; the board snapshot is then compared field-for-field against
  the board assembled by replaying the durable events (the Last-Event-ID
  path) — snapshot ≡ replay.
- OpenAPI contract: the new route is documented and its 200 body validates
  against the contract in the package's contract tests.

## Consequences

- The setup page (a later UI task) consumes two typed events and one
  snapshot; its "no card moves without an event" acceptance is testable
  server-side.
- Auto-cast (UX-03, not yet implemented) needs no new board surface: its
  per-character auto-approval is the same consent-gated `Approve` with the
  profile-level default consent, and it therefore emits `done` through this
  ADR's observer.
- `agent.Options`/`JobConfig` gained one optional hook; nothing else in the
  agent core changed.

## Reversal plan

Remove the `internal/setupboard` package, the `SetupStages` field, the
voices `StageObserver` field, the board route and its OpenAPI operation, and
the two store queries. The event names stay registered in any log that
already wrote them (names are permanent), but nothing publishes or reads
them; no schema, migration or durable state is added by this ADR, so the
reversal leaves no orphaned rows.
