# ADR-0135: Cast redesign (ordering, filters, picker, bulk) and the shared voiced count

Date: 2026-11-21
Status: accepted
Related: docs/design/plum-peach (2d, UX-03, UX-03d), ADR 0093 (Cast screen
merge), ADR 0109 (auto-cast and one-step undo), ADR 0070 (speaker-link),
ADR 0101 (narrator fallback, portable voice mappings), ADR 0005 (voice
approval immutability), ADR 0061 (ASR QA gate), docs/spec/07 (voice
lifecycle), docs/spec/14 (voice consent)

## Context

The Wind Waker Cast page (`/games/:id/characters` on the real board) showed a
62-row roster that was unusable for its job:

1. **Order.** ~45 unnamed placeholder rows (`ww:spk:N`, proposed, inferred,
   "no voice") and ~12 retired rows (0 lines) filled the list; the 10 named,
   voiced characters sat at the very bottom, after every placeholder.
2. **Count.** The Library card and the status bar read "0 of 62 voiced" even
   though 10 characters had approved takes. Root cause: the one store method
   behind both reads, `Store.ProfileCharacterVoiceCounts`, joined
   `voice_profiles` to `characters` on `c.id = vp.character` — but
   `voice_profiles.character` is the character's **name**, not its row id, so
   the join matched nothing and every profile's voiced count was 0. The
   denominator also counted retired rows and 0-line rows.
3. **Picking a voice.** A narrow 380px side panel; the list rows were long
   and repetitive (the same attribution sentence on every placeholder row);
   no obvious way to hear, compare and approve takes, and no way to voice
   many characters at once.

## Decision

### The shared voiced count (root fix)

One definition, used by the Library read (`GET /api/v1/games/{id}/library`),
the status bar's setup-run progress and the Cast page header:

- **A character counts when it is an active speaking character:** its
  `characters.status` is not `retired` **and** at least one message is
  assigned to it (`messages.speaker_id` points at its row). Retired rows
  never count, and a roster slot with no lines is not a speaker.
- **It counts as voiced** when it holds at least one `voice_profiles`
  revision in a state other than `retired`, matched by the character's
  **name** (the voice surface's own identity, `UNIQUE (character, revision)`).
  A draft/auditioning take counts — the line has a voice the runtime will
  use once it lands; only `retired` means "no voice".

`ProfileCharacterVoiceCounts` now answers exactly this pair (two indexed
COUNT statements; the `messages` side rides
`messages_profile_speaker_idx`, the voice side
`idx_voice_profiles_character`). The Cast page mirrors the same predicate in
`web/src/pages/cast/castShared.ts` (`castCounts`) so the header chip, the
filter counts and the Library read cannot disagree; both sides carry unit
tests against the same fixture shape (named voiced, named unvoiced, unnamed
placeholder, retired with lines, active 0-line, retired voice revision).

### The Cast screen

- **Order.** Named and voiced first (line count descending, then name), then
  named needs-a-voice (line count descending), then unnamed voiced, then
  unnamed needs-a-voice. Retired rows are **hidden by default** behind the
  `Retired (n)` filter; `All` means "everything that is not retired".
- **Filters.** Chips with counts: `All · Needs a voice · Voiced · Unnamed ·
  Retired`, plus a name search. "Unnamed" is the data-driven predicate the
  old speaker-placeholder section used: proposed rows whose assigned lines
  carry `source_field` evidence — the import's own linkage. A name pattern
  here would be a game-specific fact (docs/spec/04 keeps those out of the
  UI), so the roster name (`ww:spk:3` on the Wind Waker board) is carried in
  the row's tooltip, and the display name of an unnamed row is
  "Unnamed speaker".
- **Compact rows.** Avatar · name (or "Unnamed speaker") · line count ·
  voice-state chip · one primary action per row: **Listen** (voiced),
  **Pick a voice** (named, unvoiced), **Name** (unnamed). The repeated
  attribution sentence leaves the cell and moves to a tooltip (the badge's
  `title`) and to the detail panel's facts.
- **Picker.** Selecting a row opens the roomy detail panel (full-height on
  wide layouts, full-screen sheet at ≤600px, closable by the ✕, the Escape
  key or the browser back): the facts, the editable brief, the character's
  own sample lines (runtime data, never fixtures), the generated takes —
  each with its own player, the kit's waveform and duration and the
  transcript (the audition cards) — **Approve this take**, **Generate more**,
  and **Use narrator voice** (the per-profile narrator fallback, ADR 0101:
  with it on, this character's lines speak through the narrator voice until
  a take is approved). The same dial sits in the page header beside the
  auto-cast dial — both switches are the profile's one flag, so the panel
  and the header stay in agreement. Keyboard accessible: every control is a
  real interactive element, and Escape closes the sheet.
- **Bulk.** A cast bar over the roster: **Voice everyone with auto-cast**
  enqueues the existing one-take `voice.auditions` jobs for every named,
  unvoiced, speaking character (the ADR 0109 jobs; the page polls each job
  and shows `n of m` progress), and **Name speakers with the agent** starts
  the existing onboarding agent run for the profile (`POST
  /api/v1/onboarding/runs` with `profile_id`) to identify the unnamed
  speakers. When the `agent_reasoning` model role is not configured, the
  button is not dead: the settings read's role row names the exact setting
  (`NOT CONFIGURED: no model configured (set
  GAME_NARRATOR_ROLE_AGENT_REASONING_MODEL)`), which the bar renders verbatim
  beside the disabled action.

### Naming a speaker: the speaker-link's `name`

The existing speaker-link action (`POST
/api/v1/profiles/{id}/characters/{cid}/speaker-link`) identified a
placeholder with an *existing* roster row. "Name this speaker" needs the
create half as well, so the request gains one optional field:

- `name` (1–200 chars): the character the placeholder is. The server
  resolves it to the profile's non-retired character with that exact name —
  the existing link when one exists, or a **new confirmed character**
  (curator-confirmed, not parser-proposed: the curation decision is the
  confirmation) created in the same transaction when it does not. When both
  `name` and `target_character_id` are given, they must name the same row;
  a mismatch is a 400 (the caller said two different things).
- The response gains `target_name` and `created` (the create half of the
  answer). Everything else — draft-only, If-Match, line re-pointing,
  assertions, alias folding, retirement of the placeholder — is unchanged.

The picker's **Name this speaker** block (visible for unnamed speakers)
shows a few of the speaker's own lines, the name field, and the save action;
the placeholder's `ww:spk:` key stays in the target's aliases, so a
re-import binds the key straight to the named character (the existing
re-import binding, ADR 0070/0101).

### What is deliberately not built

- No new voice endpoints: audition creation, approval, undo, register and
  preview are ADR 0109/0005/0061 as they stand; the picker is a re-organisation
  of the same calls. "Approve this take" is the existing approve with the
  profile's consent default prefilled (docs/spec/14's record is unchanged).
- No per-character narrator mapping: the narrator fallback is a per-profile
  editorial setting (ADR 0101), so the panel's "Use narrator voice" toggles
  the profile flag and says so.
- No new ordering in the store: the server returns the roster by id (the
  content write's rule); the presentation order is the screen's job.

## Tradeoffs

- **The name-based voice join** means two roster rows that share a display
  name share one voice (both count voiced). That is already the runtime's
  behaviour — the voice surface is keyed by name — so the count describes
  what the box actually speaks, not an idealised per-row ownership.
- **`name` on speaker-link** widens one endpoint instead of adding a
  `POST /characters` that nobody else needs. The alternative (a character
  create endpoint) would be a second write path over the same roster with
  the same draft-only/If-Match contract, and a third idempotency surface;
  the create only ever happens as the tail of a link, so it lives in the
  link's transaction.
- **Retired hidden by default** changes the visible roster of existing
  screens (the speaker-link walk now opens the `Retired` filter to verify a
  placeholder's retirement; the affected spec walks the filter instead of
  asserting the row under `All`). The information is one click away and the
  chip carries its count, so nothing is lost — only the 12 dead rows at the
  bottom of a 62-row list.

## Validation

- `internal/store/status_counts_test.go` — the count definition: the join is
  by name, retired characters never count (with or without lines, voiced or
  not), 0-line active characters are out of the denominator, draft/
  auditioning takes count, a retired-only voice does not.
- `internal/api/speakerlink_test.go` — the `name` extension: link to an
  existing row by name, create a new confirmed character by name (the lines
  move to it, the placeholder retires, the key folds into its aliases), the
  mismatch 400, the missing-name 400, draft-only and If-Match unchanged; the
  new body validates against the OpenAPI schema
  (`openapi_validate_test.go`'s clause c).
- `web/src/pages/cast/castShared.test.ts` — the client mirror of the count
  (the same fixture shape as the Go test, both directions: retired never
  counted, 0-line excluded, the name join, the retired-voice case).
- `web/e2e/cast-redesign.spec.ts` — on the synthetic 62-row roster (10 named
  + 45 unnamed placeholders + 12 retired, seeded through
  `import-manifest`): voiced named rows first, retired hidden under `All`
  and listed under `Retired (n)`, the `n of m voiced` header agrees with the
  Library read, the picker opens, a take plays (scripted TTS, AGENTS.md rule
  3 boundary) and is approved through the panel, and the bulk auto-cast bar
  shows `n of m` progress as the jobs land.
- `make build`, `make lint`, `go test -race` for the touched packages, the
  full Playwright suite (workers=2), and the width/theme screenshots with
  axe (zero serious/critical).

## Consequences and reversal

- The Library and status-bar voiced counts move from 0 to the true number on
  every existing appliance at first start after the upgrade (the read is
  stateless; no migration — `voice_profiles` and `characters` are untouched).
- The `SpeakerLinkRequest` schema keeps `target_character_id` required-only
  when `name` is absent: old clients keep working unchanged; `additionalProperties`
  stays false with the new field documented.
- Reversal: restore the old `ProfileCharacterVoiceCounts` statement (the
  join fix is one clause), drop the `name` field from the request and
  response (additive; nothing depends on the create half until the Cast
  picker ships), and the Cast screen falls back to the ADR 0093 layout from
  its pre-redesign commit. No stored data changes in either direction.
