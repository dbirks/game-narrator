# ADR-0109: Auto-cast by default (per-profile consent default) and one-step voice undo

Date: 2026-10-20
Status: accepted
Related: docs/design/plum-peach (UX-03, UX-03d), ADR 0093 (Cast screen),
ADR 0005 (voice approval immutability), ADR 0061 (ASR QA gate and degraded
mode), ADR 0101 (portable voice mappings / per-profile dials), docs/spec/07
("Voice lifecycle", "Cache"), docs/spec/14 ("Voice consent")

## Context

The Cast screen (ADR 0093) merges characters and voices into one place and
promises UX-03/UX-03d:

- **Auto-cast ON by default**: every character receives one take that is
  auto-approved, with no per-character prompt. The existing voice consent
  requirement (docs/spec/14, ADR 0005) must still be *satisfied and audited*:
  `voice_profiles.consent_provenance` is the frozen consent record.
- **Regenerate keeps the previous take**: a regeneration creates a new
  revision that becomes current; the previous approved take is not deleted and
  is one step away from being current again (a "keep old take / undo" action).

The voices workflow is otherwise unchanged: audition generation is the
durable `voice.auditions` job (docs/spec/08), approval freezes one audition as
the canonical reference (ADR 0005), the runtime speaks the highest approved
revision with a reference, and the TTS cache key is versioned over the
revision fingerprint (ADR 0023).

## Options

1. **Extend the voice job with a cast context (chosen).** `POST
   /api/v1/voices/auditions` accepts an optional `profile_id` (the
   catalogue profile the Cast screen belongs to); the job payload carries it,
   and after the fill (which already runs the ASR QA read-back, ADR 0061) the
   job reads the profile's `auto_cast` dial and, when on, auto-approves the
   first audition whose QA is not failed. The dial and the consent default are
   profile-level: no per-click prompt, and the recorded consent is attributed
   to the profile default, not to a user click.
2. **A separate cast job / endpoint per character.** A new `cast` surface
   that orchestrates the existing audition job and then approves. Rejected:
   two job kinds for one design attempt, a second idempotency surface, and a
   race window between "job done" and "approve" that option 1 does not have
   (approval runs inside the same job, on the same draft row, under the
   existing state machine).
3. **Make approval implicit (no consent record).** Rejected outright:
   docs/spec/14 forbids silent consent, and the acceptance criteria require
   the consent field to be non-null and attributed to the profile default.

## Decision

1. **Per-profile dial.** `profiles.auto_cast` (migration 0018):
   `INTEGER NOT NULL DEFAULT 1 CHECK (auto_cast IN (0,1))`. A fresh profile
   casts (on is the product default); `NewRevision` carries the dial from the
   parent like `narrator_fallback`; toggling is
   `POST /api/v1/profiles/{id}/auto-cast` with the profile-write token
   contract (428 absent / 412 stale, ADR 0101 pattern) and no row-version
   bump (an editorial setting, not content).
2. **Consent default.** The per-profile consent default is the constant
   `voices.ConsentDefault = "original_synthetic_design"` (docs/spec/14's
   default position for newly designed synthetic voices). It is not
   user-editable: the consent wording is a rights statement, and a free-text
   per-profile consent field would let a caller fabricate the very approval
   docs/spec/14 forbids. The *attribution* is per-profile: auto-approval
   records `consent_provenance = "original_synthetic_design:profile-default:<profile_id>"`
   and `approved_by = "auto-cast"`, so the frozen row says which profile's
   default consented, and that it was not a click.
3. **Auto-cast flow.** `POST /api/v1/voices/auditions` gains an optional
   `profile_id`. Given one: the profile must exist and the named character
   must be on its roster (or be the narrator key), so a consent can never be
   attributed to a profile that does not own the character. When the dial is
   on at enqueue time and the request leaves `count` unset, the default count
   is **one** take (the product promise: one take per character); an explicit
   `count` (1..3) is honoured and is how "3 more takes" is expressed. The job
   payload carries `cast_profile_id`; after the fill, the job re-reads the
   dial (a toggle after enqueue is respected: the take stays
   *auditioning*, i.e. pending review) and, when on, auto-approves the
   first audition (by index) whose ASR QA is **not failed**.
4. **QA gate semantics.** A *failed* QA (transcript beyond the bound, ADR
   0061) is the only auto-cast refusal: the revision stays `auditioning`
   (needs review) with the flagged evidence on the row. A *not-run* QA
   (ASR role unconfigured or the endpoint down) auto-approves: ADR 0061's
   degraded mode says neither playback nor approval ever blocks on ASR, and a
   fresh appliance without a speech_asr role must still get its default
   voices.
5. **One-step undo.** `POST /api/v1/voices/{id}/undo`: the named revision
   must be `approved`, must be the character's current revision (highest
   approved revision with a reference — the same rule the runtime resolves
   with), and a lower approved revision must exist. The current revision
   moves `approved -> retired` (a legal transition the state machine already
   allowed but nothing could reach); the previous revision becomes current
   again. The move is one-step by construction: a retired row cannot be
   un-retired, so the undo is not repeatable and an "undo the undo" does not
   exist. Neither revision is deleted: old cache entries stay attributed
   (docs/spec/07 step 8) and both takes' audio remains retrievable.
6. **Cache keys stay versioned.** No change: a regenerated voice is a new
   revision with a new fingerprint, and the cache key's key space is guarded
   by `keyVersion` (ADR 0023); undo re-points the runtime at the old
   revision's fingerprint, whose cache entries were never invalidated.

## Tradeoffs

- **The consent default is a constant, not a profile column.** Cheaper and
  safer (no editable rights wording), at the cost of one consent position per
  build. A per-profile consent *wording* column can be added later without
  touching the provenance format (the `:profile-default:` segment already
  names the source).
- **The dial is read twice** (enqueue for the count default, job end for the
  approval). The second read is authoritative: a dial turned off between
  enqueue and job end leaves the take pending review, which is the honest
  "toggled after the run" outcome.
- **Undo retires the new take rather than flagging a `current` column.** The
  existing "highest approved revision is current" rule (runtime and UI) keeps
  one definition of current; the cost is that a retired revision's *preview*
  endpoint (which requires `approved`) stops answering — its audition clips
  and the old take's playback do not.

## Validation

- Hermetic unit tests (fake TTS/ASR): auto-cast on + QA passed → exactly one
  approved revision with the attributed consent; QA failed → auditioning;
  QA not-run → approved (degraded mode); dial off at job end → auditioning;
  undo restores the previous take and refuses a second undo.
- API tests: the 202→job flow ends with one approved version and zero
  pending approvals; `profile_id` validation (unknown profile, character not
  on the roster); the undo endpoint's 404/409 surface; the auto-cast toggle's
  token contract.
- OpenAPI: the new operations (`setAutoCast`, `undoVoice`), the `profile_id`
  request field and the `auto_cast` profile field are documented and
  role-checked by the existing contract tests.
- Live test under `GN_GATE=test-models` only: one real design take with
  auto-cast through the real checkpoints.

## Consequences

- `voice.auditions` job results gain `cast_profile_id`, `auto_approved` and
  `auto_approval_note`; a crash-restarted job re-reads the dial and is
  idempotent (an already-approved revision is not re-approved: `Approve`
  requires `auditioning`).
- The profile export carries `publication.auto_cast` (absent on older
  documents → the reader applies the default, on, mirroring
  `narrator_fallback`); the publication section is outside the content
  digest, so the manifest digest is unaffected.

## Reversal plan

Drop the dial and the undo route, stop setting `cast_profile_id`, and the
workflow is exactly the manual flow of ADR 0005: auditions are created, a
human approves with a per-click consent. Migration 0018 is an additive column
with a default; a forward migration (0018) dropping it can be written if the
feature is removed, and existing approved rows are unaffected either way
because the consent record is frozen per row.
