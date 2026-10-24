# ADR 0171: The cast audit — a character's voice must read its expected voice class

- Status: accepted (implemented)
- Date: 2026-10-23
- Owner task: cast-audit (a male voice on a female character and vice versa)

## Context and constraints

Some characters shipped with a voice that does not match their gender/age —
a female character spoken by a low adult-male voice, a child by an adult
female voice. There was no check for it: the voice-design brief could drift
from the character's profile data, and nothing measured the result. The
operator's remedy was manual: hear the voice, notice the mismatch, re-design
it by hand.

Constraints:

- The measurement must reuse the existing YIN estimator
  (`internal/speech.MeasurePitch`, ADR-0162) and its class vocabulary
  (`VoiceClass`: below `PitchLowAdultMaleHz` = 165.0 Hz is low adult male,
  165.0–255.0 Hz adult female, above 255.0 Hz child-high). Inventing a second
  pitch ladder would let the audit and the playback-side class disagree.
- AGENTS.md rule 5: no game text in logs, reports, or test fixtures beyond
  the character's own profile name. The audit output carries profile names,
  class labels and Hz numbers only.
- AGENTS.md rule 6: voice selection is reviewable. A fixed take must not be
  auto-approved into the speaking set; it stays PENDING unless the operator
  explicitly approves.
- The audit is read-only; the fix is a separate, explicit, idempotent,
  dry-run-by-default command leg.

## Options

1. **A pure classifier + a store-backed auditor + a fixer in one new package
   `internal/cast`.** The comparison leg (expected class from profile text /
   ADR-0158 `speaker_kind` id, verdict against measured F0) is a pure function
   unit-tested on synthetic tones; the measurement leg reads the assigned
   voice's reference asset and runs `speech.MeasurePitch`; the fix leg
   re-designs through the existing `voices.Service.CreateAuditions` path.
2. Put the logic in `internal/speech`. Rejected: that package owns pitch
   measurement and the speech cache; the cast audit's subject is the
   roster/profile relationship, not speech signal processing.
3. Put it in `internal/voices`. Rejected: the audit also reads roster rows
   and profile text it has no business knowing, and the voices package's
   lifecycle is the revision state machine, not cross-voice review.

## Decision

- **Expected class** (`cast.ExpectedClass`): keyword table over the
  character's profile text (description + role + the assigned voice's design
  brief, the brief last so it wins on conflict) — `male/man` → low adult
  male; `female/woman/girl/lady` → adult female; `boy/child/kid` → child;
  `creature` → no expectation. The descriptor at the latest position in the
  text wins ("… a child, grown now into an old man" → male). ADR-0158
  `speaker_kind` ids (`adult_male`, `elder_male`, `adult_female`,
  `elder_female`, `girl`, `boy`; `kind:` prefix tolerated) are the fallback
  when the text carries no gender word. No words → `unknown`.
- **Verdict** (`cast.Verdict`): `OK` when expected and measured classes
  agree; `MISMATCH` when the expected class is known and the measured F0
  reads a different class; `UNKNOWN` when there is no expected class or the
  F0 was unmeasurable (an unmeasurable clip is not evidence, not a failure).
- **Assigned voice** (`cast.Auditor.AssignedVoiceOf`): the highest approved
  revision with a fingerprint and a reference asset — the same selection
  rule the runtime speaks with (`internal/runtime.approvedVoiceForKey`).
  Characters without one get an `UNKNOWN` row: the audit reports absence,
  never invents a measurement.
- **Surfaces**:
  - CLI `game-narrator cast audit --profile <id> [--json] [--fix] [--apply]
    [--approve]`; exit 1 while mismatches remain (scriptable), 0 otherwise.
  - API `GET /api/v1/profiles/{id}/cast/audit` (viewer role, read-only; the
    OpenAPI `CastAuditReport`/`CastAuditRow` schemas).
  - Doctor row `cast.voice_gender` (read-only; `degraded` with the first
    five mismatch rows and a count when any exist; `not configured` when no
    profiles are imported).
  - Web: a small "Voice check" badge column on the Cast roster row
    (OK / mismatch / muted dash for UNKNOWN), refetched when the voice list
    moves.
- **Fix** (`cast.Fixer`, `--fix --apply`): for each MISMATCH, re-design
  through the existing voice-design path with the character's existing brief
  preserved plus an explicit gender/age sentence stating the expected class,
  prefixed with the marker `cast-audit-fix:`. The new take's F0 is measured;
  up to 2 retries (`FixMaxRetries`) while it still reads the wrong class.
  The winning take stays PENDING (auditioning) unless `--approve` is passed.
  Idempotence: a pending revision carrying the marker is re-measured on
  re-run instead of opening another draft; only a still-wrong pending take
  triggers more design. `--fix` without `--apply` is a dry run that prints
  what it would redesign.

## Tradeoffs

- Keyword classification is deliberately crude: it will miss characters
  whose gender is only implied, and a later descriptor overrides an earlier
  one rather than combining them. The audit answers "does this voice sound
  like the character claims to be" for the cases where the profile text
  says — it is not a natural-language understanding. `UNKNOWN` rows make the
  gap visible instead of hiding it behind a false OK.
- The fix re-designs the whole brief (existing + gender sentence) rather
  than patching a single instruction: the design model consumes the brief as
  one prompt, and the marker sentence is also the idempotence key.
- The doctor row audits every profile on every run; on large appliances that
  is one YIN pass per assigned voice. The measurement is on the reference
  asset (a short WAV), not per-line, so the cost is bounded by the cast size,
  not the script length.

## Validation

- Pure leg: `TestExpectedClass`, `TestKindToClass`, `TestVerdict`,
  `TestRowFor` (invented character descriptions; boundary cases at 165.0 /
  255.0 Hz).
- Measurement leg: `TestMeasuredClassesFromSyntheticTones` drives the real
  YIN estimator with synthetic 110 / 220 / 320 Hz tones and asserts each
  lands in the expected class.
- Wire: `TestCastAuditEndpoint` plants approved voices on two fixture-roster
  characters (a female brief with a 110 Hz reference → MISMATCH; a male
  brief with a 110 Hz reference → OK) and validates the 200 body against the
  OpenAPI schema; the operation-coverage gate records the hit.
- Web: `web/e2e/cast-voice-check.spec.ts` on the packaged binary with the
  scripted providers seeds a deliberate mismatch (the stub's tone law is a
  pure function of the brief, so the spec computes the take's F0 before it
  exists — 418 Hz child tone under a female brief) and an OK control row
  (468 Hz child tone under a child brief), and asserts the visible badge.
  Passed: `1 passed (4.2s)` under the voices project.
- Live (real provider, gate `GN_GATE=test-models`, model
  `qwen3-tts-voicedesign`, direct Base checkpoint): `TestLiveCastAuditFix`
  seeds a 110 Hz male reference on a profile described as "a young woman"
  (every string invented; throwaway store), runs the real CLI:
  - audit: `expected mid (adult female) measured 110.0 Hz (low (adult
    male)) verdict MISMATCH`;
  - `--fix --apply`: `110.0 Hz -> 176.8 Hz; pending review`, one take;
  - verified take F0 176.8 Hz → `mid (adult female)`, state `auditioning`
    (PENDING, not approved);
  - idempotent re-run: revision count stable, the pending take re-reported.
  Passed in 3.9 s.

## Consequences

- `internal/cast` is the new home for cast-level review; the doctor, the API
  and the CLI all read through `cast.Auditor`, so there is one definition of
  "the voice this character speaks".
- A fixed voice is a pending audition: the operator sees it in the voice
  panel and approves it like any other take. The mismatch keeps reporting
  (the old approved voice still speaks) until the take is approved and the
  old revision retires — by design, the audit measures what is actually
  spoken.
- The web badge is a small column, not a section: it must not alarm on
  `UNKNOWN` rows (most imported rosters have no gender words).

## Reversal plan

- Delete `internal/cast`, `cmd/game-narrator/cast.go`,
  `internal/api/cast_audit.go`, `internal/doctor/cast_voice.go`, the route
  registration and OpenAPI operation, the auth policy grant, the Cast page
  badge and query, `web/e2e/cast-voice-check.spec.ts`, and the
  `playwright.voices-only.config.ts` variant. No schema migration is added by
  this ADR (the fix reuses the existing voice revision rows), so the store is
  untouched; pending fix takes already created would remain as ordinary
  pending auditions (harmless; the marker in their brief is inert).
- The doctor row, CLI subcommand and API endpoint simply disappear; nothing
  else in the pipeline depends on them.
