# ADR-0052: Editor corrections record the regression fixture
Date: 2026-10-18
Status: accepted
Owners: agent worker
Related requirements/tests: docs/spec/08 (API table: `POST/GET /api/v1/corrections`, `GET /api/v1/dev/fixtures`), docs/spec/09 (developer correction actions; "make a correction, verify DB and API state, save a fixture"), docs/spec/12 (regression fixture discipline), docs/spec/16 M5 ("correction produces regression fixture"), ADR-0017 (contract tests), `TestRoleMatrixOverEveryRoute`, `TestOpenAPIDocMatchesTheRouter`

## Context and constraints

docs/spec/16 M5's acceptance names "correction produces regression fixture" alongside per-route Playwright coverage. The table already exists: `0002_content.sql` created `corrections` (kind, target, before/after values, evidence, actor, review state) with the rationale that "the label is training data and outlives the row", and `games.Correction` validates the same shape. What does not exist is any repository method that writes or reads the table, and any API surface: the scope table and the authz test matrix both carry placeholder rows for `POST /api/v1/corrections` and the `dev/fixtures` surface "so they cannot be born unauthenticated", and the message PATCH (the editor's one hand-typed edit) records an audit line but no correction.

The fixture question is where it goes and what it is. docs/spec/12's golden-fixture paragraph demands an assertion plan, expected labels, and provenance; docs/spec/09's inspector names the "save regression fixture" action. A correction of a message's speech text already carries all four: the context (profile revision + message id), what was wrong (before value), what it should be (after value = the assertion a re-run checks against), and who decided (actor). Duplicating that into a second "fixture" table would create two sources of truth for one user act; the spec's two routes (`/corrections`, `/dev/fixtures`) are two *views* of the same rows (curation label vs. developer re-run fixture), not two stores.

## Decision

1. **The `corrections` table is the regression fixture.** A correction row of kind `speech_text` targeting a message *is* the re-runnable label for that message in that revision; no separate fixture table, no fixture file.
2. **The editor's correction produces it automatically.** `PATCH /api/v1/profiles/{id}/messages/{mid}` (the only hand-typed edit, docs/spec/02) records a `corrections` row in the same write unit as the speech-text update: kind `speech_text`, target_type `message`, before/after = the old/new speech text, actor = the signed-in username, status `accepted` (the curator who typed the fix is the reviewer of that fix; nothing is pending). A save that leaves the text unchanged records nothing — a no-op is not a correction. A save that *clears* the rendering is still a valid edit (silence is a rendering choice, docs/spec/02) but also records no label: an accepted correction must carry the assertion a re-run checks (`games.Correction`'s validation), and an empty after-value asserts nothing. The row joins the profile's row-version bump, so an edit that lands is a correction that lands; the API composes the two store calls inside one `InTx` (the store's existing join-on-ctx pattern, the same way import → publish composes).
3. **Two read views over the same rows** (docs/spec/08's route table, both already granted by `auth.DefaultPolicy`):
   - `GET /api/v1/corrections?profile_id=&kind=&limit=` (curator): the label list — newest first, bounded (default 100, max 500), optional profile/kind filters. A named profile the store does not have is a 404 the read itself answers, the way every other profile-scoped read does (`ListCorrections` enforces the referential claim), and an unknown kind is a 400 naming the vocabulary rather than a silently dropped filter.
   - `GET /api/v1/dev/fixtures?profile_id=&kind=&limit=` (developer): the fixture browsing surface — the same rows, documented as the re-run inputs (docs/spec/09 "save a fixture and inspect a test report"; the `/tests` harness's future fail-only re-runs read these).
   The documented response is one schema (`CorrectionList`); the dev route's description states the fixture reading of it.
4. **`POST /api/v1/corrections`** (curator) records a correction explicitly — the inspector's "speaker incorrect / text incorrect / …" actions and anything a later milestone wires there. It records a label only; it never applies a fix (applying is the PATCH or the corresponding content write), and it validates the profile and, for message targets, the message row. `evidence` is a JSON array (frame/observation ids, asset hashes) defaulting to `[]`.

## Options considered

- **A separate `regression_fixtures` table plus a copy step.** Rejected: two sources of truth for one act; docs/spec/12's fixture properties are already the columns of `corrections` (assertion plan = kind + after_value, expected labels = before/after, provenance = actor/evidence/created_at), and the 0002 design deliberately lets labels outlive their rows for exactly this reason.
- **Recording the correction in the audit log only.** Rejected: the audit store is an operational trail (unfiltered, untyped), while the spec's correction rows are typed training labels with a review state; M5's acceptance asserts the label's existence through the API, not the log.
- **A fixture *file* under the data directory.** Rejected: appliance state belongs in the database — it then rides the backup/restore test (AGENTS.md rule 10) for free, whereas a sidecar file would need its own persistence story and would be orphaned by a restore.
- **`status = proposed` for editor-recorded rows.** Rejected: the fix is already applied by the same actor in the same request; a forever-proposed label would poison the review queue. The proposed→accepted lifecycle remains available to explicit `POST /corrections` rows and to a later reviewer surface.

## Consequences and risks

- Every successful speech-text edit grows one row; growth is bounded by curation activity (one row per human edit), and the list endpoint is bounded by `limit` with the `(profile_id, status, created_at)` index 0002 already created.
- A crash between the two store calls cannot happen: they join one transaction (AGENTS.md "Keep runtime predictable").
- `GET /corrections` returns before/after text to curator and above; that text is curated script the caller already reads in the dialogue editor, and developer-mode surfaces (docs/spec/08) are exactly this tier.
- Risk: an explicit POST with `after_value` set reads like an applied fix when it is only a label. The response shape and the ADR both say the write is record-only; the 422 path (unknown message/profile) keeps a mistyped label from pointing at nothing.

## Verification

- `internal/store` (repo_corrections_test.go): write + list round trip, profile filter, kind filter, newest-first order, limit bounds, validation refusals (bad kind, accepted-without-reviewer).
- `internal/api` (corrections_test.go): PATCH on a draft revision records a `speech_text` correction with the exact before/after values visible at `GET /corrections`; an unchanged save records none; a stale If-Match records none and changes nothing; `POST /corrections` validates, records and is visible to both read views; the list body validates against the documented `CorrectionList` schema; the authz matrix and the OpenAPI doc/router/scope-table drift tests pass with the three new operations.
- Playwright (`web/e2e`): the dialogue editor's correction shows up at `GET /api/v1/corrections` with both texts, inside the full curation walk.

## Rollback / migration

No schema change (0002 already created the table; databases created before this ADR already have it). Rollback: delete the three routes, the API handlers, the store methods and the recording call in the message PATCH; rows already recorded are inert data a future build ignores. No forward migration is needed; rows written by a newer build and then read by a rolled-back one are simply not surfaced.
