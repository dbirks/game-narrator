# ADR-0030: Voice workflow — audition, consent-gated approval, per-instance registration, cached preview

Date: 2026-10-17
Status: accepted
Owners: agent (m4-voice-workflow)
Related requirements/tests: docs/spec/07 "Voice lifecycle" (1–8) and "Identity vs emotion vs
random seed", docs/spec/02 (`voice_profiles`, `voice_registrations`, `audio_assets`), docs/spec/06
"VoiceRegistryManager", docs/spec/08 voice endpoints, docs/spec/14 "Voice consent", docs/spec/16
M4 acceptance; ADR-0005 (app-owned reference), ADR-0022 (TTS wire contract), ADR-0023 (cache key);
tests `TestVoiceWorkflow`/`TestRestart`/`TestLiveVoiceWorkflow` (internal/voices),
`TestVoice*` (internal/api), `internal/store` voice tests, `web/e2e/voices.spec.ts`.

(Numbering: the milestone plan assigned 0029, but ADR-0029 was taken by the BMG parser; this
workflow takes the next free number, 0030.)

## Context and constraints

A narrator's voice must be set up once, per character: audition candidates, pick one with a
meaningful consent record, register it on each provider instance that will later speak the
character's lines, and preview lines through a cache that is keyed on the voice's identity so a
new approval changes the key (old cache entries stay attributed to the old voice, docs/spec/07
step 8).

Hard constraints from the verified deployment (ADR-0022): VoiceDesign and Base are different
checkpoints reached through the gateway; the voice registry exists **only** on the direct Base
service; registered names are immutable once chosen, and this app may only create/remove names in
its own namespace (`gn_`, tests `gn_test_`). The reference clip is app-owned and immutable
(ADR-0005): the registry is a cache of the app's content, not the source of truth.

## Decision

### The revision is the unit of immutability

`voice_profiles` holds one row per revision (`0009_voices.sql`); the state machine is one-way:
`draft → auditioning → approved → retired` (and `draft → auditioning` only). A changed brief or
re-audition creates a new revision; it never edits a frozen one. `Approve` re-reads the chosen
clip from the asset store, re-hashes it (the content address is only as good as the bytes behind
it), freezes the bytes + exact transcript + design model + consent record on the row, and computes
the fingerprint `sha256(refSHA | transcript | model)` — the identity that goes into every TTS
cache key (ADR-0023). The registry name is precomputed at approval
(`gn_` + digest8(profile id) + character + revision + digest8(reference bytes)) so a name
collision surfaces at approval, not mid-`Ensure`.

### Audition creation is 202 + a durable job

`POST /api/v1/voices/auditions` creates the draft row synchronously (immediately addressable)
and enqueues a `voice.auditions` job that runs the N VoiceDesign calls (N capped at the number of
fixed synthetic lines — docs/spec/03: never game text). A gateway stall must not hold an HTTP
request open; the job's report (`audition_count`, ids) is the durable verdict. The fill is
idempotent over a crash (a draft that already has clips is left untouched). A build without a
queue answers 503 on that one route only.

### Registration is a per-instance fact, reconciled, not assumed

`Register` runs one `Registry.Ensure` pass for the revision and records
`voice_registrations{instance, registered_name, remote_digest, state, last_verified_at}`. The
store's state set is `pending | ready | missing | invalid` — a superset of docs/spec/08's
`ready|missing|invalid`: `pending` is the precomputed-name row approval opens before any instance
has seen the voice. `Reconcile` re-registers every approved voice whose row is not `ready`,
covering the "voice missing on a replica" failure injection (AGENTS.md minimum quality policy).
A `ready` voice that later vanishes from `Registry.List` degrades to `missing`, not `invalid`.

### Preview: the cache decision is part of the API contract

`POST /api/v1/voices/{id}/preview` speaks through `speech.Cache.GetOrGenerate` with the revision
fingerprint in the key and `OriginalMessageID="voice-preview:"+revisionID`. The response is the
WAV; the cache decision rides `X-Cache: HIT|MISS` so the UI shows "cache hit" without a second
call, and `ETag` is the content SHA-256. With no `ready` registration the preview falls back to
the inline-reference path (docs/spec/06 fallback), which is what makes preview usable before the
first `Register`.

### Honest degradation and role boundaries

No speech endpoint configured ⇒ the voice subsystem is not wired and `/api/v1/voices*` answers
503 (an empty list would read as "no voices configured" instead of "this build does not do
voices"); malformed role environment fails boot. Roles come from the scope table, not the
handlers: reads are viewer, auditions/approve/preview/registrations are curator, `register` is
operator (touching the provider registry is an operator act).

## Alternatives considered

- **Register at approval, not on demand.** Rejected: approval is a curation act on this
  instance; the replicas are the point of the registry, and forcing every approval through a
  registry round-trip couples the consent record to an external failure.
- **Edit the brief on an auditioning revision.** Rejected: the audition's provenance (brief text
  per clip) must match what produced the clip; a changed brief is a new design attempt.
- **Store preview audio with the voice row.** Rejected: previews are content-addressed speech
  cache entries (ADR-0023); the row would bloat and the cache would be bypassed.

## Validation

- Domain: consent required, immutability (second approval refused), fingerprint change moves the
  cache key, reconcile re-registers a removed voice, preview miss→hit with identical bytes,
  restart persistence (close/reopen: voice, reference and `Reconcile`→ready survive) —
  `internal/voices` with fake TTS/registry, race-enabled.
- API: full workflow over HTTP with role enforcement (viewer 403 on writes, operator-only
  register), error mapping (400 CONSENT_REQUIRED / 409 state / 404 / 410 asset-gone / 503
  unwired) — `internal/api` contract + OpenAPI scope-table + enum tests.
- Live (env-gated, `TestLiveVoiceWorkflow`): three real VoiceDesign auditions, real approval,
  `gn_test_` registration + removal on the direct Base registry, real preview MISS (TTS request)
  then HIT (no TTS request, cache hit counter).
- UI: `web/e2e/voices.spec.ts` against the packaged binary with a scripted TTS instance
  (deterministic WAVs, honest 400 for unregistered voices): generate → 3 clips with transcripts →
  consent-gated approval → registration `ready` + name visible in the registry → preview
  MISS→HIT, and a different line misses again.

## Consequences

- The registry is a cache: a fresh instance with an old database answers preview via the
  inline-reference fallback until `Register`/`Reconcile` runs; dialogue sessions reconcile on
  startup.
- One row per revision means the history of a character's voice (every attempt, every consent
  record) is queryable and auditable; cleanup is by `retired`, not delete.
- The fingerprint couples the cache to the reference bytes: any re-approval invalidates preview
  reuse by design (docs/spec/07 step 8).

## Reversal

Drop `internal/voices`, the voice handlers and the migration (down-migration deletes the three
tables); the TTS/speech/registry packages are untouched and revert with their own ADRs. A
fingerprint-scheme change is additive: old cache entries stay valid under the old key.
