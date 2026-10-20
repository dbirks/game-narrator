# ADR-0075: Player controls — pause/resume, narrated classes and mid-session mode on the Play page

Date: 2026-10-19
Status: accepted
Owners: agent worker (g7-player-controls)
Related requirements/tests: docs/spec/08 (`POST …/pause`, `POST …/resume`, `PUT …/settings` session routes; `session.paused`/`session.resumed`/`session.settings_changed` events), docs/spec/09 (`/play`: "pause/mute, narrated classes, conservative/predictive toggle"; keyboard accessible, captions/text equivalents), docs/spec/00 (player can "pause/mute … choose conservative/predictive playback"; "signs and item text only on intentional selection"), docs/spec/05 (commitment policy, pause resets belief), docs/spec/12 (failure injection: rapid textbox advances; no duplicate speech), gap audit G7/G16 (docs/status/gap-audit-2026-10-19.md), AGENTS.md rules 3/5/9. Tests: `internal/session` (reducer: settings control, class veto, pause fence), `internal/runtime/controls_test.go` (replay pause mutes speech, resume bursts nothing, SetSettings snapshot+events), `internal/api/sessions_controls_test.go` (contract + authz matrix rows), `web/e2e/play-controls.spec.ts` (Playwright on the packaged binary, replay session, fake TTS, labelled synthetic; axe scan of the live-session state).

## Context and constraints

Gap audit G7: the session reducer already carries `ControlPause`/`ControlResume`/`ControlSettings` (`internal/session/messages.go`, `apply.go`) — pause stops committing and cancels in-flight work, the commit gate refuses while paused, resume changes no belief — and the runtime manager already exposes `Pause`/`Resume`/`SetSettings` as one control message to the session's single writer. What did not exist: the spec-08 routes that send those controls, the authz coverage for them, the operator's narrated-class and mode settings mid-session, and the Play-page surface (no buttons, no toggles, no shortcuts, no `aria-live` on the committed lines — G16).

Constraints: one session writer (all controls ride the same ordered chain as observations, so a pause pressed mid-line fences the audio on the same event stream); the answer to every control is the session's fresh snapshot, not an acknowledgement (the UI's button shows the server's state, not the click); the synthetic replay corpus is dialogue-only (the scenario loader admits only `text_kind: dialogue`), so a committed line's class cannot vary at the runtime layer on this corpus.

## Options

1. **Routes send the existing controls; the UI reads the snapshot** (adopted): `POST /api/v1/sessions/{id}/pause`, `POST …/resume`, `PUT …/settings` map 1:1 onto `Manager.Pause/Resume/SetSettings`. The settings body is partial (`mode?`, `narrated_classes?`); the manager merges it with the session's current policy, validates against the documented vocabulary, and sends one complete `ControlSettings` message (the reducer force-includes dialogue and dedupes). A request that names the state the session already has is a no-op — no event, because a settings event is an operator-visible change.
2. A single `PATCH …/state` with a `field` discriminator — rejected: three named spec-08 routes are the contract; one generic route would force the OpenAPI document to describe the same surface with a weaker vocabulary.
3. Client-side pause (the UI stops listening, the speech queue drains) — rejected: "pause stops new speech and fences pending utterances" (spec 08) is server-side semantics; a browser that closes the tab must not un-pause the session.

## Decision

### API

- `POST /api/v1/sessions/{id}/pause` and `…/resume` (operator): idempotent; the 200 body is the snapshot with `paused` moved. A terminal session answers 409 `session_not_running`; an unknown id 404; an unwired runtime 503. Each change is one event on the session's chain: `session.paused` (after the cancellations it caused) and `session.resumed`.
- `PUT /api/v1/sessions/{id}/settings` (operator): partial body, merged-and-normalized policy (dialogue first, always present), one `session.settings_changed` event carrying the applied mode and classes. Out-of-vocabulary values answer 400 `invalid_session_settings`.
- The commitment policy's eligibility filter reads the session's `NarratedKinds` at commit time: a committed line whose class is not narrated still commits (the box advances, the text machine knows) but carries `speech_eligible: false` — the speech layer's no-audio answer. The mode switch moves the commitment policy and the text machines together under one lock.
- Authz: all three routes are operator (the tier that starts and stops sessions); the role matrix asserts the grant and the fail-closed behaviour for lower roles.

### Play page

- A "Session controls" panel on the live view: a pause/resume button (`aria-pressed`, label from the snapshot's `paused`), the narrated-class toggles (Dialogue locked on — the product's default the reducer force-includes; +Signs, +Items), and the conservative/predictive mode select. Every control posts only its own field; the 200 snapshot installs into the query cache (the same read-your-writes rule as the audio controls).
- Keyboard shortcuts on the live view: `p` pause/resume, `s` signs, `i` items, `m` mode — inert while a form control has focus, so the shortcuts never swallow typing.
- `aria-live="polite"` on the current committed line and `role="log"` + `aria-live="polite"` on the recent-lines list (G16: committed lines are announced, not only rendered).

### Tests (AGENTS.md rule 3 — every layer named)

- Reducer (synthetic input, real logic): the settings control applies and emits `session.settings_changed`; a committed line of a non-narrated class carries `speech_eligible: false` and becomes eligible when the class is added; the pause fence and the post-resume single commit (no burst) hold.
- Runtime (b (fixture replay), real pipeline): pause during a replay with a wired dummy sink ends the in-flight utterance and admits no new speech while paused; resume after a paused window commits the waiting line exactly once; `SetSettings` answers the applied snapshot and emits the event (no-op sends nothing).
- API (contract): the three routes' 200/400/404/409/503 answers against a real replay session, validated against the OpenAPI schema; the authz matrix gains the three rows.
- Playwright (packaged binary, replay session, fake TTS — labelled synthetic): the walk seeds the profile and a narrator voice, starts the session from /play, exercises pause/resume (the speech counters prove the fence), the class toggles and the mode select through the real routes, the keyboard shortcuts, and scans the live-session state with axe-core (WCAG 2.1 A/AA).

## Tradeoffs

- **The corpus cannot exercise a sign/item commit at the runtime layer**: the eligibility filter is proven at the reducer (the layer that decides it) with a scripted `kindOf`, and the runtime test proves the settings control end to end on the dialogue corpus. A sign/item *scenario* is G8's, not this ADR's.
- **Partial settings body, complete control message**: the merge lives in the runtime (it owns the session's current policy); the reducer sees only complete target states, which keeps the duplicate-detection in one place.
- **No persistent per-session settings**: a restart loses the operator's mid-session choices (the session dies with the process anyway; spec 08 names no persistence).

## Validation

- `go test -race ./internal/session/... ./internal/runtime/... ./internal/api/...` (the new tests above; existing pause/lifecycle tests unchanged and green).
- `web`: typecheck, lint, fmt:check, unit suite; the `play-controls` Playwright project on `make build`'d binary.
- Secret scan of the new files (orchestrator step; no credentials or private endpoints appear in any of them).

## Consequences

- `session.settings_changed` is a new name on the session's event chain; the developer inspector and the event log pick it up with no further work (names are open in the envelope).
- The Play page becomes the operator's live policy surface; a future per-session settings persistence (G18's named `PUT /api/v1/config`) would extend this ADR's merge, not replace it.
- ADR-0073's managed clips and ADR-0039's audio controls are untouched; the controls panel sits beside the audio panel and shares its snapshot-install rule.

## Amendment 1 (2026-10-19): controls are identity-stamped at the writer; the
control channel closes with the run, so a late control is a refusal, not a
phantom success

The first green run of the runtime controls tests exposed the wiring bug the
reducer's fences had been built to catch: the runtime's `Pause`/`Resume`/`SetSettings`
sent `ControlMsg`s with an empty `Header`, and the session fence read that as
`other_session` — every live control was dropped without an event while the
runtime's optimistic snapshot answered "applied" on top of a session that kept
speaking. The fix is the same identity rule the pipeline already applies to its
own messages, at the same place: the pipeline's single writer stamps the control
with its own `SessionID`, the live `Generation()` read at the moment of the apply
(the writer is the only applier, so nothing can commit between the read and the
`Apply`), and the profile revision, just before it hands the control to `Apply`
(`internal/pipeline/reduce.go`'s controls case). A non-control shape on the
live-control channel fails the run rather than being applied through a door it
was never meant for.

Second half of the same rule: the run now closes the session's control channel
the moment the writer is done (`internal/runtime/session.go`'s `run`, guarded by
the session's mutex so `sendControl` can never send on a closing channel), and
`sendControl` answers `false` on a closed channel. A pause, resume or settings
that lands after the run has ended is therefore refused with `session_not_running`
(the 409 the operator deserves) instead of buffered into a dead session and
reported as applied — the snapshot never lies "paused" about a session that is
already over. The idempotent and no-op rules are unchanged: a duplicate is the
same snapshot with no second event.
