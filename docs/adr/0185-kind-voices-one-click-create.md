# ADR-0185: The generic kind voices get their one-click create on the Cast page, with the cast audit's pitch check in the loop

Date: 2026-10-24
Status: accepted
Owners: agent worker (kind-voices-ui)
Related requirements/tests: docs/spec/07 (voice lifecycle), docs/spec/08 (APIs), docs/spec/09 (embedded SPA), docs/spec/14 (consent), AGENTS.md rule 6 (reviewable voice selection), rule 3 (no simulation theater). ADR-0158 (speaker-kind generic voices and their `kind:*` keys), ADR-0171 (cast audit: measured F0 against expected class), ADR-0162 (YIN pitch estimator and voice classes), ADR-0109 (auto-cast). Tests: `internal/cast` (kinds_test.go, the orchestration with synthetic 110/220/320 Hz tones), `internal/api` (voices_kinds_test.go, the two routes over the wired subsystem), `web/e2e/kind-voices.spec.ts` (the Cast section and the one-click run on the scripted-provider stack).

## Context and constraints

ADR-0158 routes an unnamed speaker of a confident apparent kind through the roster key `kind:<kind>`; seven kinds exist (`adult_male`, `adult_female`, `elder_male`, `elder_female`, `boy`, `girl`, `creature`). Until now those voices were created by hand with an operator script (audition → wait → approve → register per kind), and one bad take got through QA uncaught: the first `kind:adult_male` take measured ~219 Hz (a female or youthful range), the "villager with a woman's voice" complaint again. The cast audit (ADR-0171, `internal/cast`) already measures a take's median F0 with the ADR-0162 YIN estimator and classifies it — the missing piece is running that check *inside* the kind-voice creation itself, and giving the operator one click instead of a script.

Constraints: voice selection stays reviewable (AGENTS.md rule 6) — the run must never auto-approve; the existing audition/approve/register workflow and endpoints are reused, not duplicated; nothing game-derived enters the fixed briefs (rule 5).

## Options

1. **A cast-package orchestrator over the existing voice service, exposed as two API routes and one Cast page section.** Adopted.
2. **A durable `voice.kinds` job** that creates the missing kinds in the background. Rejected for this change: the run is one take per missing kind (up to three design calls per kind), bounded and click-scoped; a job would add polling, a payload schema and crash-recovery surface for work that finishes inside the request. The synchronous handler reports per-kind failures on the rows, which is what the operator needs on the click.
3. **Reuse the auto-cast job path per kind** (enqueue `voice.auditions` with the kind key, let the job auto-approve). Rejected: auto-approval is exactly what rule 6 forbids for these voices, and the job's fill path carries no pitch verdict — the check would have to be bolted onto the job's result, which duplicates the audit's job in the job's code.
4. **Duplicate the pitch logic in `internal/api`.** Rejected: the classifier already lives in `internal/cast` (ADR-0171); two copies of the class thresholds would drift.

## Decision

- `internal/cast` gains `kinds.go`: the fixed per-kind `Briefs` (each states the expected pitch in words), `BriefFor(kind, attempt)` (retries append the stronger pitch wording from attempt two on), the coverage read (`Coverage`: missing / pending / approved per kind, with the take's measured F0 and class — approved follows the runtime's selection rule, the highest approved revision with fingerprint and reference; pending is a draft/auditioning revision that carries a take), and `CreateMissing`: for each missing kind, one audition via the existing `voices.Service.CreateAuditions` seam, the first QA-passed (or QA-not-run) take measured with `speech.MeasurePitch`, judged with the audit's `KindToClass` + `Verdict`; an out-of-class take is re-auditioned up to 2 retries (3 attempts total) with the stronger brief, and the run leaves every passing — and even the still-out-of-class — take **PENDING**: the approve click is the operator's, an out-of-class final take is flagged on the row, never approved.
- `internal/api` gains `GET /api/v1/voices/kinds` (viewer: a read like the voice list) and `POST /api/v1/voices/kinds/create-missing` (curator: curation — it starts design work). Both build the orchestrator over the wired voice subsystem and answer 503 SUBSYSTEM_UNAVAILABLE when it is not wired.
- The Cast page gains the "Generic kind voices" section below the roster: one row per kind (state badge, measured pitch class + Hz), and the "Create missing (n)" button for curators when any kind is missing. The section degrades to nothing when the voice subsystem is unwired, the way the roster's voice column does.
- The OpenAPI contract documents both operations and the `KindRow` / `KindCoverage` schemas; the kind state vocabulary is pinned in `openapi_enums_test.go` against the `cast` constants.

## Tradeoffs

- The create-missing request is synchronous: with a slow design model, a fully missing appliance holds the request through up to 21 design calls. Accepted — the operator clicked and wants the answer on this click, the per-kind failures are reported on the rows, and the design calls are sequential short requests (no retries with backoff inside the request).
- A kind voice whose take is still out of class after 3 attempts is left pending and flagged, not discarded: discarding would burn three design calls for nothing on the next click without a stronger signal, and the operator's ear is the final gate (rule 6) — a flagged row is a decision, a discarded row is a guess.
- `girl` is classified as adult-female range and `boy` as child by the existing `KindToClass` (ADR-0171's mapping); this change reuses that mapping as-is rather than re-deciding it. If the mapping is wrong in product terms, that is ADR-0171's table to change, with one test and one ADR amendment.

## Validation

- `go test ./internal/cast/` — the orchestration on a fake voice service with synthetic 110/220/320 Hz tones: every missing kind audited once, the out-of-class take retried with the stronger brief, the give-up after 3 attempts keeping the flagged pending take, the skip of non-missing kinds, the design-failure row, the coverage read.
- `go test ./internal/api/ -run TestKindVoices` — the two routes over the wired subsystem with a scripted TTS (per-kind queued tones): the fresh-appliance coverage (all missing), the one-click run (all seven pending with the expected measured class), the retry on an out-of-class first take (220 Hz then 110 Hz), the curator-only gate (viewer gets 403), the 503 without the subsystem; both success bodies recorded against the OpenAPI contract.
- `go test ./internal/api/ -run 'TestOpenAPIDocMatchesTheRouter|TestOpenAPISchemaKeywords|TestRoleMatrixOverEveryRoute'` — route/documentation parity and the scope table.
- `make build`, then Playwright (scripted TTS/ASR, packaged binary, the mandated scope command): `playwright test kind-voices.spec.ts --project=voices --workers=1` — the section lists all seven kinds, the "Create missing" run lands every kind pending with the measured pitch class visible on the row, and the measured F0 matches the scripted tone within 2 Hz (the spec computes each brief's tone up front from the stub's djb2 tone law and asserts the in-class invariants at load).
- `make lint` over the touched packages.

## Reversal plan

Delete `internal/cast/kinds.go` (+ test), `internal/api/voices_kinds.go` (+ test), the two route registrations and policy grants, the two OpenAPI operations/schemas (and the enums-test pin), the Cast section and the web API helpers, and the spec. No schema migration is involved (the kind voices are ordinary voice revisions under `kind:*` character names); voices created by the run remain valid voice revisions and can be retired through the existing undo/retire flow.

## Consequences

- The seven kind voices are one click from the Cast page, with the pitch check in the loop: the 219 Hz "adult male" class of defect is caught by the run itself (retried, then flagged) instead of by a manual post-hoc measurement.
- The operator's approve click is the only approval path for a kind voice; the run's contract is "passing takes pending, out-of-class takes flagged".
- The kind coverage (missing / pending / approved + measured pitch) is a stable read for the doctor's `voices.kind_coverage` check (issue #51's third suggestion) — it answers from the same state.
