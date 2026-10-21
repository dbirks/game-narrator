# ADR-0141: The Traces screen — line view, session-inspector reachability, and the UX-05 correction flow

Date: 2026-10-21
Status: accepted (plum-peach Phase 1 item 7)
Related: ADR 0093 (Plum & Peach design system and IA), ADR 0099
(context-aware assistant), ADR 0083 (developer actions), ADR 0052
(corrections as fixtures), `docs/design/plum-peach/README.md` screen 2h,
`docs/design/plum-peach/product-notes.md` UX-05, plan.md item 7

## Context

ADR-0093 §4 renames `/developer` to **Traces** at `/traces` (old path
redirects). Screen 2h specifies the new surface: a filterable line list
(speaker + time + line; filter pills spoken/skipped/slow), a 5-step strip
(Saw frame → Read OCR → Matched → Who → Spoke, each with a +ms offset),
"What it read" (crop + raw/normalised OCR), "Why this line" (candidate bars
+ a one-sentence reason), and the actions Replay audio · Wrong speaker ·
Save as test · Download evidence. It also says the existing
developer-dashboard inspection (candidates, Clef payloads, model inspector)
"remains reachable from here".

The legacy `/developer` inspector is a session-scoped surface (event
timeline, detail panel, bottleneck, session control) whose deep link
`/developer?session=<id>` is a live contract: the play walk arrives there
with a running session and developers share run URLs. A naive "replace the
route" would delete that contract; a naive "keep both routes" contradicts
the ADR-0093 redirect.

UX-05 (product-notes) requires that a "wrong speaker" mark from a trace
triggers the agent's similar-lines check: a suggestion naming the affected
lines, bulk application only after explicit confirmation. The ADR-0099
assistant already owns the wire (`POST /api/v1/assistant/messages`, the
`assistant.message`/`assistant.question` events, the confirmation gate) and
the real tools (`find_similar_lines`, `apply_speaker_fix`); what was missing
is the trace-screen trigger.

## Options

- **Two routes** (`/traces` new surface, `/developer` kept for the
  inspector): violates ADR-0093's rename (the old path must redirect) and
  leaves two homes for one screen.
- **Replace `/developer` entirely**: loses the session inspector, its
  deep-link contract and the existing inspector specs.
- **One route, two views**: `/traces` is the line view; the session
  inspector is reachable at `/traces?view=inspector` (and any
  `?session=<id>` deep link, which keeps tracking exactly as before);
  `/developer` becomes a query-preserving redirect to `/traces`. The
  inspector component, its testids and its behaviour are unchanged.

## Decision

1. **`/traces` is one page, two views.** The default view is the 2h line
   view. The session inspector (the existing `DeveloperInspector`,
   unmodified) renders when `view=inspector` is present or when a
   `session=` deep link is present. The line view links to it per session
   ("Open in session inspector").
2. **`/developer` redirects to `/traces`**, preserving the query string, so
   `/developer?session=<id>` still lands on the tracked session and shared
   run URLs keep working. Existing inspector specs point at the new URLs
   (ADR-0093's rename; the testids they assert are unchanged).
3. **The line view reads the durable developer log**
   (`GET /api/v1/dev/events`, bounded pages of ≤1000, at most five pages):
   one entry per committed occurrence, grouped by `trace_id`. The entry
   carries speaker, time and line text; the selected entry is
   `--peach`. This is the honest data source — the same records the
   inspector reads — with no new backend surface.
4. **The 5-step strip maps onto the trace's own events** (read via
   `GET /api/v1/dev/traces/{trace_id}`), each step with a +ms offset from
   the trace's first event:
   - *Saw frame* — the trace's stored `text.observed` (`frame_seq`);
   - *Read OCR* — the same observation's raw/normalised text and
     confidence;
   - *Matched* — the trace's `message.candidates` top probability and the
     matcher's reason code;
   - *Who* — the `message.committed` `speaker_id`, resolved to the
     character's name through the profile's characters (an unattributed
     line reads "narrator (unattributed)");
   - *Spoke* — the trace's `speech.ready` `from_cache` (cache hit/miss);
     when the line never spoke, the terminal `speech.ended` result and
     reason (e.g. `skipped: no_approved_voice`).
5. **The filter pills are spoken / skipped / slow**, independently
   toggleable (none active = all lines). *spoken* = the utterance reached
   playback (`speech.started`); *skipped* = the terminal result was not
   playback (skipped/muted/failed/cancelled, with the reason shown);
   *slow* = capture→commit latency above 1000 ms. The latency is the
   committed event's time minus the observation's `capture_timestamp_ns`
   (the source clock both events carry), falling back to the envelopes'
   `occurred_at` when the source clock is absent. The 1000 ms threshold is
   the page's own documented constant — a design decision of this screen,
   not a pipeline property.
6. **The actions.** *Replay audio* plays the utterance's stored WAV
   (the `speech.ready` cache key, the inspector's existing
   `/api/v1/dev/speech/audio` surface) with a dithered waveform. *Save as
   test* is the existing fixture recording (`POST /api/v1/dev/fixtures`).
   *Download evidence* is the existing redacted bundle
   (`GET /api/v1/dev/traces/{id}/evidence`).
7. **UX-05 from a trace.** "Wrong speaker" records the
   `speaker-incorrect` correction on the occurrence (the corrected line is
   marked immediately, the ADR-0083 shape with the target character in the
   evidence) and sends one assistant message (ADR-0099) with the screen
   context `{route: /traces, profile_id, selection: {type: "trace", id}}`.
   The agent runs the real `find_similar_lines` and
   `apply_speaker_fix` tools against the real store; the confirmation gate
   is the real gate. The page renders the agent's suggestion
   (`assistant.message`) and the NEEDS-YOU confirmation card
   (`assistant.question`) with confirm/cancel; cancel leaves the data
   untouched, confirm applies the bulk fix. The page rebuilds from the
   thread snapshot (`GET /api/v1/assistant/threads/{id}`) after a refresh.
8. **No backend changes.** Every surface (dev events, trace read, fixture
   recording, evidence, corrections, assistant threads/messages/answers,
   the confirmation-gated tools) already exists; this item is the screen,
   the redirect and the e2e wiring.

## E2E model boundary

The walk runs the real pipeline on the packaged binary (real replay
session, real OCR, the scripted TTS instance for the utterance's audio —
the established synthetic-stub class). The assistant's *model* runs
against a new scripted gateway (`web/e2e/fake-assistant.mjs`, labelled
synthetic in its header): it drives the conversation (find the similar
lines, stage the bulk fix, report) while the tools, the store and the
confirmation gate are the real thing. That is AGENTS.md rule 3 class (a):
a synthetic model stub, named as such; the suggestion, the tool timeline
and the DB assertions are not stubbed.

## Tradeoffs

- The line view reads up to five bounded pages of the durable log, which
  on a busy appliance is the tail of history, not all of it; the log is an
  appliance-scale surface (one appliance, few runs), and the per-trace
  read (`GET /api/v1/dev/traces/{id}`) is the complete record for the
  selected line. Bounded by design (AGENTS.md principle 7).
- The 1000 ms "slow" threshold is a page constant; making it a profile
  setting would be a backend change this item does not need.
- The session inspector behind a query parameter means its deep link is
  now `/traces?view=inspector&session=<id>`; the old `/developer?session=`
  URL still redirects onto it, so the contract is preserved, not moved
  silently.

## Validation

- `web/e2e/traces.spec.ts` (playwright project `traces`): for a seeded
  `trace_id` the 5-step strip values match the API record; the line list
  filters (spoken/skipped) behave; "Save as test" is asserted via the API
  (the 201 fixture body and the on-disk manifest); `/developer` redirects
  to `/traces`; the wrong-speaker correction records a correction, the
  agent's suggestion names the similar line, no speaker change lands
  before the confirmation, and the confirmed bulk fix is read back from
  the messages API. Screenshots at 1440/1024/390, axe at zero
  serious/critical, no horizontal scroll at 390.
- Unit tests for the pure line/strip model (`web/src/pages/
  tracesModel.test.ts`), including the slow classification and the
  cache-hit/miss mapping.
- The existing inspector specs keep their testids and pass against the
  new URLs (developer.spec.ts, developer-actions.spec.ts,
  developer-decision.spec.ts).

## Consequences

- `App.tsx`: `/traces` renders the new page; `/developer` is a
  query-preserving redirect.
- `DeveloperPage.tsx` exports the inspector component; the role gate
  moves to the Traces page (both views behind the developer role, as
  before).
- The a11y walk visits `/traces` instead of `/developer`.
- The shell's status-bar nav (Phase 0 seam) now points at the real screen.

## Reversal

Re-point the redirect (`/traces` → `/developer`) and restore the old
route entry; the inspector component is unchanged and the line view's
files delete cleanly. The query-parameter view seam is a one-line route
change either way.
