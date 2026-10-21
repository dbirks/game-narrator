# ADR-0133: Keep the read-only settings contract — the spec-08 `GET/PUT /api/v1/config` deviation is decided (keep)

Date: 2026-10-21
Status: accepted
Owners: agent (small-35-39 task)
Related requirements/tests: docs/spec/08 (API table row `GET/PUT /api/v1/config` — "redacted settings and validated changes"), docs/spec/14 (secrets server-side; masked labels), docs/spec/16 (gate list), ADR-0047 (`/settings` read-only; `PUT /api/v1/settings` 405 by design), ADR-0074 (audio output + master volume — the one runtime-mutable setting, persisted, env-override precedence), gap audit row G18 (docs/status/gap-audit-2026-10-19.md "Runtime config changes"), acceptance row G18 (docs/status/acceptance-2026-10-20.md, **closed with deviation** by this ADR), issue #35 (decision issue, closed completed).
Tests: `internal/api/settings_test.go` (405 PUT contract: Allow header, problem+json, the env list in the detail; redaction assertions — token/host absence), `internal/api/audio_test.go` (the one mutable setting: validation, consent gate, selection precedence), `internal/store/settings_store_test.go` (migration backup/restore), Playwright `web/e2e/m5-settings-tests.spec.ts` (settings page: NOT CONFIGURED roles, no secrets in the DOM).

## Context

docs/spec/08's minimum API table names `GET/PUT /api/v1/config` with the purpose "redacted settings and validated changes". The implementation, since ADR-0047, does not carry that route pair:

- `GET /api/v1/settings` answers with the full redacted effective configuration (model roles with tokens as presence booleans, gateway hosts as `"<configured>"`, storage, audio sink/device, security, import policies, version, the mutable-settings note) — `internal/api/settings.go`.
- `PUT /api/v1/settings` answers **405 Method Not Allowed by design** (`handleSettingsPut`), with `Allow: GET` and a problem+json detail that names the environment variables a change would have to go through. The 405 body doubles as documentation.
- Exactly one setting is runtime-mutable: the audio output selection and master volume, persisted via `PUT /api/v1/settings/audio` with env-override precedence (ADR-0074).

The 2026-10-21 acceptance audit (gap row G18, "polish" severity) recorded the contract gap as a named row and filed issue #35 with the decision: *keep the deviation and record it* or *implement validated runtime config changes*. No owner had taken it; this ADR is the record.

## Decision

**Keep the deviation. The spec-08 `GET/PUT /api/v1/config` contract is superseded by the implemented `/api/v1/settings` contract (this ADR), and G18 is closed with deviation.**

Rationale, for the form factor this product is:

1. **This is a single-operator LAN appliance.** One operator, on one box, with console access to the environment and a service restart. "Validated runtime config changes" (schema-validated writes, per-setting restart-vs-live semantics, redacted change responses, audit events) buys a multi-tenant control plane for a product that has one operator. It would add write-path attack surface — a settings mutation endpoint is a standing privilege for every authenticated admin session — for zero user benefit: the operator can edit the environment variables and restart, and the 405 answer names exactly which variables to change.
2. **The env-plus-restart model is honest.** ADR-0047's constraint 1 stands: every setting of this build is environment- or build-constant, and "a save button that saves nothing is worse than its absence" (AGENTS.md rule 3 — no simulation theater). The 405-with-the-env-list is the answer the operator gets in one round trip instead of a save that lies.
3. **The one mutable setting already proves the pattern.** ADR-0074 (audio output + master volume) is the documented, persisted, validated exception — admin-gated, consent-gated where it plays audio, env-override precedence, store-backed with a migration and a backup/restore test. If a future UI or multi-operator flow needs more runtime-mutable settings, the audio route is the template to copy (schema-validated partial update, redacted response, audit via the events log), and the decision of which setting crosses the line is made per setting, with an ADR — not by opening a general config write endpoint up front.

docs/spec/08's API table row is updated in place to point at `/api/v1/settings` with a pointer to this ADR; the `PUT .../config` semantics described there are retired, not deferred.

## Alternatives considered

- **Implement `PUT /api/v1/config` now** (validated changes, per-setting restart-vs-live semantics, audit event). Rejected on the form-factor ground above: one operator with console access has a strictly better path (env + restart) for every setting, and the endpoint's only user would be a future UI that does not exist. The audio route already carries the proven pattern for the day it is needed.
- **Close G18 as verified against the spec as written.** Rejected: the route pair the spec names does not exist and PUT declines the method; AGENTS.md rule 3 forbids presenting that as a pass. The row is closed *with deviation*, with the deviation named in both the spec and the acceptance report.
- **Mark the spec row "future work" and leave G18 PARTIAL.** Rejected: that is the pre-decision state this ADR exists to end — the gap audit's objection was that the deviation had "no owner and no record of when it was considered".

## Consequences

- The spec-08 API table row for `GET/PUT /api/v1/config` reads as the implemented `/api/v1/settings` contract with the ADR pointer (done in the same change as this ADR).
- G18 is **closed with deviation** in `docs/status/acceptance-2026-10-20.md`; issue #35 is closed `completed`.
- ADR-0047 stays accepted and unmodified in its decision: the 405 contract it documents is precisely what this decision keeps. ADR-0074 stays the sole runtime-mutable setting until a future ADR says otherwise.
- If the form factor ever changes (multi-operator, unattended remote configuration), the re-decision is: implement the validated-changes contract with the audio-settings route as the template — and this ADR is the point to re-open, superseded then.

## Verification

- Grep-verified at decision time (2026-10-21 working tree): `PUT /api/v1/settings` 405 by design in `internal/api/settings.go` (`handleSettingsPut`); the 405 detail names the env list (pinned by `internal/api/settings_test.go`); `PUT /api/v1/settings/audio` is the only settings write (ADR-0074); `GET /api/v1/settings` returns the redacted config plus the mutable-settings note.
- Spec cross-reference updated: docs/spec/08 row points at `/api/v1/settings` + this ADR.
- Acceptance report updated: G18 row **CLOSED WITH DEVIATION** (ADR-0133), done-criteria list de-PARTIALs G18.

## Rollback / reversal plan

No code, schema or migration changes: this ADR only records a decision and retires a spec row. Reversal is the alternative branch of issue #35: implement `PUT /api/v1/config` (schema-validated, redacted responses, per-setting restart-vs-live semantics, audit event), restore the spec row, open a new ADR superseding this one, and re-open G18 as a normal row. Nothing here is load-bearing on the running system.

## Links / references

- docs/spec/08-api-and-events.md — "APIs (minimum complete set)" table, settings row
- docs/adr/0047-settings-tests-pages.md — the read-only settings contract and the 405-by-design decision
- docs/adr/0074-audio-device-selection-and-gain.md — the one runtime-mutable setting
- docs/status/gap-audit-2026-10-19.md — G18 "Runtime config changes"
- docs/status/acceptance-2026-10-20.md — G18 row (closed with deviation by this ADR)
- issue #35 (closed completed)

Supersedes: the docs/spec/08 `GET/PUT /api/v1/config` contract row (as the deviation's deciding record). Superseded by: —.
