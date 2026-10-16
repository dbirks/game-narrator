# ADR-0015: Profile revision row version, publication pointer and publish audit as catalogue columns
Date: 2026-10-16
Status: accepted
Owners: project
Related requirements/tests: docs/spec/02 "Entity catalog" (`profiles`, `editions`, `publish_actions`) + "Transaction workflows" ("draft edit → new revision snapshot → validation → reviewer approval → atomic publication pointer swap") + "Migration and backup acceptance" (blank DB *and* prior DB), docs/spec/01 "Runtime and setup isolation" (invariants: only published revisions are used at runtime; active sessions pin a revision), docs/spec/08 "Conventions" (ETag/If-Match for profile revisions) + `PROFILE_REVISION_CONFLICT` + `CONTENT_RIGHTS_REVIEW_REQUIRED`, docs/spec/12 "Property" mode ("no published edit without valid revision token"), docs/spec/16 (evidence bundles from the database), docs/spec/14 (rights gate), AGENTS.md rules 7 and 10; builds on ADR-0008 (driver, migrations, splitter) and ADR-0010 (the HTTP layer that will render these as ETags); `go test -race -count=1 ./internal/store/`

## Context and constraints

- A `profiles` row is one **revision** of one edition, and revision semantics are
  load-bearing in three separate places: the immutability rule (a published or
  archived revision cannot be edited — derive a new one), concurrency control (two
  curators holding the same view must not silently overwrite each other), and
  publication (exactly one revision per edition is *what the appliance narrates*).
  docs/spec/12 states the property, not the mechanism: "no published edit without
  valid revision token." It is a storage property before it is an HTTP one.
- docs/spec/08 requires the token to travel as an `ETag` and be compared as
  `If-Match`, which means the store must hand out a value that (a) changes on every
  accepted write, (b) never changes on a refused one, and (c) **never comes back
  around**. A token that can be reused is worse than no token: the client believes
  it holds a lock.
- docs/spec/02 names `publish_actions` in the entity catalog as the reviewer / audit
  trail, and docs/spec/16 builds release evidence bundles from the database. An
  answer to "was this revision ever offered for publication, and why was it
  refused?" has to be readable from the file after the process that decided it is
  long gone.
- The publication gate refuses *most* drafts (docs/spec/14: nothing derived from
  material with an unresolved rights position may ship). A refusal that is written
  inside the failed transaction is a refusal that never gets recorded — so the
  audit write cannot simply live in the publication transaction.
- One writer at a time per database (ADR-0008's `BEGIN IMMEDIATE` +
  `busy_timeout=5000`), one process on a 4 GB appliance. Optimistic locking is a
  UI/consistency requirement, not a throughput optimization.
- **The situation this ADR exists to settle.** The first implementation of these
  three facts put them in the append-only `events` table (`repo_journal.go`),
  because an earlier working brief forbade touching tests that pinned "exactly
  three migrations". That worker documented the hazard itself; this ADR records the
  decision it was pushing against.

## Decision

`internal/store/migrations/0004_publication.sql` adds the three facts to the
catalogue tables docs/spec/02 names them in, and the repositories in
`internal/store` read and write them there and only there.

1. **`profiles.row_version` — the revision token.**
   `ALTER TABLE profiles ADD COLUMN row_version INTEGER NOT NULL DEFAULT 1 CHECK
   (row_version >= 1)`.
   - It is bumped **in the same UPDATE statement as the change it guards**, by the
     one helper every guarded write funnels through
     (`UPDATE profiles SET <change>, row_version = row_version + 1 WHERE id = ? AND
     row_version = ?`). Zero rows affected is the compare failing, reported as
     `ErrRevisionConflict` when the row exists and `ErrNotFound` when it does not.
     One statement means a mutation can never be observed with an unmoved version,
     and a rolled-back mutation rolls its bump back with it.
   - `DEFAULT 1`, not `0`: creating a revision is itself a write, so the first ETag
     a client receives is `1` and nothing special-cases "not written yet". A
     revision derived by `NewRevision` therefore starts at `1` too — it has never
     been edited.
   - It is *not* a field of `games.Profile` and not part of `profileColumns`: it is
     storage bookkeeping returned alongside the profile (`GetProfile`,
     `GetProfileContent.RowVersion`, `PublishedProfile`) for the API layer to
     render as an ETag. Folding it into the domain type would have put a mutable
     counter next to `ManifestDigest`, the value every audio cache key derives from.
   - Every content write moves it exactly once **per transaction**, not per row: a
     200-message import that bumped it per row would expire every other editor's
     token before that editor could save.

2. **`editions.published_profile_id` — what an edition serves.**
   Nullable TEXT, `REFERENCES profiles(id) ON DELETE RESTRICT`.
   - Nullable: an edition that has never shipped serves nothing, and docs/spec/13's
     degraded modes say a session refuses to start rather than improvise —
     `PublishedProfile` returns `ErrNotFound` (404), not a 200 with a null body.
   - `RESTRICT`, argued in the migration file: `CASCADE` would delete a curated,
     reviewed revision as a side effect of somebody tidying an edition row;
     `SET NULL` would un-publish the current release with no audit trail and pull
     the pinned revision out from under live sessions (forbidden by docs/spec/01).
     RESTRICT forces the honest order — retire first (an audited act through
     `Publish`/`SetProfileStatus`), delete second — and mirrors the reasoning
     `0001_init.sql` applies to `profiles.edition_id`.
   - **The pointer is authoritative and the status column is not a second source of
     truth about what is served.** `PublishedProfile` reads the pointer. A revision
     whose status says `published` but that the edition does not name does not
     enter service; a pointer naming a revision whose status was edited by hand is
     reported as `ErrConflict` rather than served.
   - `TestPublishedProfileFollowsThePointerNotTheStatus` pins exactly this, and it
     replaced a test that pinned the opposite (status-derived lookup).

3. **`profiles_one_published_idx` — the invariant in the schema.**
   `CREATE UNIQUE INDEX ... ON profiles (edition_id) WHERE status = 'published'`.
   Partial, so drafts and archived revisions are untouched. Its job is to make the
   one-live-revision rule true *of the file*, not merely of the code written today:
   a write that bypasses the repositories cannot produce two published revisions of
   one edition, and therefore cannot make "what is live?" a question with two
   answers. Applying it to a database that already holds such a state fails loudly
   instead of silently picking a winner — correct for state the repositories could
   not have created.
   The repositories keep pointer and index in agreement by construction, not by a
   reconciler: `Publish` retires the incumbent before promoting the successor
   inside one `BEGIN IMMEDIATE` transaction; archiving a live revision clears the
   pointer in the same transaction as its status write.

4. **`publish_actions` — the audit trail.** One row per publication-lifecycle
   decision: `action IN ('published','archived','rejected')`, `author NOT NULL`,
   `row_version` (the revision's version *after* the decision), `detail_json`
   (gate findings, manifest digest, the caller's expected token, the superseded
   revision — interpreted only by Go, like every JSON column in this schema),
   `occurred_at`, plus an `(edition_id, occurred_at)` index for "this edition's
   publication history".
   - `edition_id` is denormalised and carries **no foreign key**, for the same
     reason `0002_content.sql` gives `corrections.target_id`: the record must
     outlive what it describes in the one direction SQLite would otherwise allow.
     `profile_id` is `RESTRICT`: an audit row that outlived its subject would
     describe nothing and could not be cross-checked against the digest it recorded.
   - A **refusal is committed in its own short transaction** after the publication
     transaction has been rolled back. The refusal *is* the outcome, and its
     evidence must survive the rollback of the attempt that produced it. `Publish`
     never fails a caller's request because its own bookkeeping failed; a lost audit
     row is joined to the returned error rather than replacing it.
   - `author` is `NOT NULL` with no default. Where the store genuinely cannot name
     anybody (`SetProfileStatus` carries no identity), the row says `unknown` — the
     difference between a diagnostics gap and a silently unaccountable history.
   - Ordering on read is `occurred_at DESC, rowid DESC`: a second-resolution
     timestamp cannot separate the retirement from the promotion of one
     publication, and rowid is this table's insertion order.

5. **`events` may be written, never read, for catalogue facts.** An operational feed
   may append `profile.published`-style rows for the live UI. Nothing in
   `internal/store` may — and nothing now does — read `events` to decide a version,
   a pointer or an audit fact. `grep -rn 'repo_journal\|rowVersionJournal\|profile.revision.write' internal/`
   is part of the acceptance gate for exactly that reason.

6. **Tests state the schema version they depend on, instead of asserting a number.**
   `store.LatestSchemaVersion()` / `store.MigrationNames()` (plus the test-only
   `migrateTo(t, s, n)` helper) replace the hardcoded "three files, version 3" in
   `internal/store`, `cmd/game-narrator` and `internal/doctor`, and
   `TestCatalogTablesAreExactlyTheFirstFive` now asserts what **`0001_init.sql`**
   creates rather than what the whole database contains; the whole-database drift
   check is `TestMigratedDatabaseHoldsExactlyTheMigrationTables`, derived from the
   embedded migrations. This is what removed the pressure that produced the
   journal design.

## Alternatives considered

- **The `events`-journal approach (what shipped in the previous build).** Storing
  the counter as `profile.revision.write` rows, the pointer as `profile.published`
  rows and the audit as event payloads.
  Rejected, on the log's own documented properties:
  - docs/spec/02 calls `events` a "bounded operational store separated from profile
    data", pruned by age. A fact stored only there can disappear while the row it
    describes survives — so catalogue truth would depend on retention luck.
  - For the counter that is **fail-open**: prune a revision's journal rows and its
    version restarts at the bottom, so a months-old `If-Match` token matches again
    and an editor silently overwrites work they were told was stale. The direction
    docs/spec/12's property must never allow. (Pruning *every* row to zero is the
    worst case, and the old code reported version 0 for a revision it had not
    journaled — a token no legitimate writer would ever present, so the *first*
    edit after pruning succeeded regardless of what any client held.)
  - It made every publication read a scan of an append-only log instead of an
    indexed primary-key lookup, and gave the audit trail no per-edition index.
  - It could not express the one-published-revision invariant in the schema at all.
- **Deriving the pointer from `profiles.status` (no column).** One indexed lookup,
  no new state, and no second representation to disagree — but it answers "which
  rows *claim* to be published", which needs a tie-break the moment anything writes
  outside the repositories, and it cannot represent "retired, still pinned by live
  sessions" separately from "live". docs/spec/02 names the operation a *pointer
  swap*; the partial unique index is what makes pointer and status agree.
- **A separate `revisions` table of immutable snapshots** (edit rows vs. shipped
  rows). Truly immutable publication, but a second content model, a second copy of
  every message, and a digest that must reconcile two tables. docs/spec/02 already
  models a revision as a `profiles` row with the same ids the dialogue graph points
  at; immutability is enforced at the repository boundary (`ErrImmutable`) and is
  cheap to keep there.
- **`updated_at` or a hash of the row as the token.** Wall-clock collisions at
  second resolution (the schema's own timestamp granularity), and a content hash
  makes an unrelated cache-field edit invisible. A monotone per-row counter is
  comparable, cheap, and honest about "somebody wrote this".
- **Row-level `SELECT ... FOR UPDATE` / pessimistic locking.** Not a thing SQLite
  has; `BEGIN IMMEDIATE` already serializes writers, so the extra value of a
  compare-and-swap is telling the *loser* why they lost instead of timing them out.
- **Reusing `profiles.revision_num` as the token.** It identifies *which* revision,
  not *which state* of it: it must never move again once published, while the token
  must move on every draft edit.

## Consequences and risks

Positive: catalogue facts live in catalogue tables, so pruning, retention or a
replayed operational log cannot change what a revision's version is, what an
edition serves, or who shipped it; the one-live-revision rule is enforceable by a
reader with nothing but `sqlite3` access; `0004` is purely additive, so an upgrade
is one transaction; the audit answer to docs/spec/16's evidence questions is in the
same file the evidence bundle points at.

Costs and risks:

- **Two representations of publication remain** (pointer + status) and can be pulled
  apart by a hand edit of the file. Mitigated, not eliminated: the partial index,
  the single-transaction writes, and `PublishedProfile` returning `ErrConflict` for
  a pointer/status disagreement — which is a loud failure a session surfaces
  instead of narrating the wrong revision.
- `RESTRICT` on both publication foreign keys means **deleting a revision or an
  edition that ever shipped requires retiring and un-auditing it first**, and
  `publish_actions` rows accumulate forever by design. Retention for audit rows is
  an open operability question (docs/spec/13), deliberately not answered here — the
  wrong answer would be the same fail-open bug with a schedule on it.
- `row_version` grows monotonically per row and is never reset; a heavily edited
  revision can reach six figures. Integers are 64-bit; not a practical risk, but a
  *derived* version (e.g. from a timestamp) would have been, so the counter stays
  a counter.
- Every guarded write must go through `guardedProfileUpdate`. A future write path
  that issues a bare `UPDATE profiles` would move content without moving the token.
  `TestStaleRowVersionIsRejectedOnEveryWritePath` exercises the guard primitive
  directly for that reason, and `TestRowVersionIncrementsByExactlyOnePerWrite`
  enumerates the exported write paths.
- A rejected publication writes a row every time it is refused, so a client retrying
  a refused publish in a loop grows the audit trail. Bounded per row by
  `maxListedProblems` in `detail_json`; unbounded in count. Deduplicating refusals
  would need a caller identity the store does not have — an API-layer concern
  (idempotency keys) rather than a schema one.
- `0004` has no down-migration. SQLite supports `DROP COLUMN`/`DROP INDEX`, and the
  reversal sketch is below, but this build has no downgrade runner (ADR-0008) —
  restoring from backup remains the supported path.

## Evidence and test plan

Environment: Arch dev host, `CGO_ENABLED=0`, `modernc.org/sqlite` v1.60.1 (bundled
SQLite 3.53.4) per ADR-0008.

```sh
go vet ./... && go test -race -count=1 ./...
go test -race -count=1 -v -run 'RowVersion|Publish|Upgrade|Prune|Revision' ./internal/store/
D=$(mktemp -d); go run ./cmd/game-narrator migrate --data-dir "$D"
sqlite3 "$D/game-narrator.sqlite" '.schema publish_actions' 'PRAGMA integrity_check;'
grep -rn 'repo_journal\|rowVersionJournal\|profile.revision.write' internal/   # prints nothing
grep -rnE 'schema version [0-9]|v != [0-9]\b|SchemaVersion.*!= [0-9]' internal cmd --include=*_test.go   # prints nothing
```

Covering tests (`internal/store`), each naming the failure it exists to prevent:

| Test | Property |
| --- | --- |
| `TestRowVersionIncrementsByExactlyOnePerWrite` | one accepted write → exactly +1, across every exported write path; a refused write and `NewRevision` leave the parent alone; a fresh revision starts at the column default |
| `TestStaleRowVersionIsRejectedOnEveryWritePath` | stale token → `ErrRevisionConflict` on the token-bearing paths and on the shared guard primitive; guard on a deleted row → `ErrNotFound`; rolled-back bump disappears; concurrent tokenless writers lose nothing |
| `TestPartialUniqueIndexAllowsOnePublishedRevisionPerEdition` | raw `INSERT`/`UPDATE` producing a second published revision is refused by the partial index; drafts/archive coexist; another edition is unaffected; pointer and index agree |
| `TestPublishedProfileFollowsThePointerNotTheStatus` | the pointer decides service; a raw status edit elsewhere changes nothing; a raw status edit *of* the live revision is `ErrConflict`; retirement through the store clears the pointer and audits itself |
| `TestRejectedPublishLeavesAnAuditRowAndChangesNothing` | gate refusal leaves status, token and pointer untouched, yet commits a `rejected` row whose `detail_json` holds the findings, and survives a later successful publish |
| `TestUpgradeFromThePriorSchemaBackfillsThePointer` | a database at the previous version (built with `migrateTo`, holding a published revision and no pointer column) upgrades, backfills the pointer, and stays writable and auditable; an edition with nothing published stays `NULL`; `PRAGMA integrity_check` passes |
| `TestPruningTheEventLogChangesNothingAboutPublicationFacts` | `DELETE FROM events` (the fail-open case the journal design allowed): version, pointer and audit unchanged, next write continues from the stored value, the pre-pruning token is still refused |
| `TestConcurrentPublishLetsExactlyOneWin`, `TestPublishingAgainRetiresThePredecessor`, `TestInTxSerializesWriters`, `TestProfileLifecycleDraftReviewPublished` | the swap and the counter under real concurrency, plus `migrate → reopen → still consistent` (docs/spec/02 "prior database") |
| `TestCatalogTablesAreExactlyTheFirstFive`, `TestMigratedDatabaseHoldsExactlyTheMigrationTables` | the migration-split tests that no longer hardcode a version or a table list |

Migration acceptance is exercised twice over: `TestMigrateFromBlankDBIsRepeatable`
(blank DB) and `TestUpgradeFromThePriorSchemaBackfillsThePointer` (prior DB), and
`cmd/game-narrator`'s `TestRunMigrateEndToEnd` now derives its expected output from
`store.MigrationNames()` / `store.LatestSchemaVersion()` rather than from a typed
number, so it tests the CLI instead of testing last quarter's file count.

## Migration / rollback

Forward: `game-narrator migrate` applies `0004` in its own transaction (ADR-0008),
adding the column (DEFAULT-filled, CHECK not re-evaluated against existing rows —
which is correct, since every pre-existing revision has been written at least once),
the nullable pointer, the partial index, and the audit table, then backfills
`published_profile_id` from the row that currently claims `published` per edition.
The backfill is why the upgrade cannot silently un-publish a live appliance: a
session that cannot name a revision must refuse to start (docs/spec/13), and before
it, `PublishedProfile` would have found nothing.

Backward: `publish_actions` is `DROP TABLE`-able and the index `DROP INDEX`-able;
SQLite ≥3.35 drops `editions.published_profile_id` and `profiles.row_version`
in place (the CHECK travels with the column). Older binaries that derive
publication from `status` keep working against the data, because the backfill keeps
both representations in agreement — the pointer simply becomes unused by them.
The supported operational path stays what ADR-0008 defines: `Backup`, then `Restore`
from the pre-upgrade copy. Unwinding is *safe in one direction only*: the counter
values written after the upgrade are unrecoverable from an older schema, so
restoring backwards loses ETags, which is exactly why the token was never made
derivable from anything else.

## Follow-ups

- Render `row_version` as the HTTP `ETag` and compare `If-Match` in the profile
  endpoints (Milestone 1 API work, ADR-0010), mapping `ErrRevisionConflict` →
  `409 PROFILE_REVISION_CONFLICT` and `ErrImmutable` → 409 "derive a revision".
- Decide audit retention for `publish_actions` alongside docs/spec/13's backup /
  retention policy — and keep it out of the `events` retention job.
- The developer UI's publication history view reads `PublishActions`; bound and
  cursor it when the review screen ships (docs/spec/03).
- Revisit whether a `rejected` storm needs API-level idempotency-key deduplication
  before it needs a schema change.
- Supersedes nothing; refines ADR-0008's migration section (see its
  "Follow-up 2026-10-16" note on the trigger-aware splitter, which `0002` needed
  and `0004` relies on for the FTS5 objects it inherits).
