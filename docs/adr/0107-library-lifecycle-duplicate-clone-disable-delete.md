# ADR-0107: Profile library lifecycle — duplicate, clone, disable, delete (gap G14)

Date: 2026-10-21
Status: accepted
Owners: g14-library-lifecycle worker
Related requirements/tests: FR-02 (docs/spec/00-product-charter.md: "create,
import, export, duplicate, clone, disable, version, archive and delete"), gap
G14 (docs/status/gap-audit-2026-10-19.md); ADR-0015 (revision snapshots and
row_version), 0004_publication.sql's publish_actions RESTRICT rationale,
ADR-0041 (reference check before delete); tests
internal/store/repo_library_test.go, internal/api/library_test.go, migration
0017_library_lifecycle.sql

## Context

FR-02 names eight library operations. As of this change, create, import,
export, version (revision snapshot + ETag), and archive (status retirement)
exist. Duplicate, clone, disable and delete had store-side primitives at best
(profile-level cascade delete exists and is exercised by
`TestDeletingProfileCascadesContentAndFTS`) but no API surface, no retention
semantics, no session guard and no audit trail.

The constraints the shape has to respect:

* **0001/0004 FK discipline.** `profiles.edition_id` is RESTRICT (an edition
  with revisions cannot vanish), `editions.published_profile_id` is RESTRICT
  (a live release cannot be un-published as a side effect of a delete), and
  `publish_actions.profile_id` is RESTRICT with the explicit rationale: *"a
  revision with publication history cannot be deleted out from under its own
  trail; deleting either side is a deliberate act, never a cascade."*
* **0004 `profiles_one_published_idx`** — at most one `published` revision per
  edition, enforced at the schema level.
* **docs/spec/01** — active sessions pin a published revision; a revision a
  session names must survive the session's whole run.
* **AGENTS rule 10** — schema changes need forward migrations and
  backup/restore tests; cache keys are versioned.

## Decision

Four operations, five routes:

| Operation | Route | Role |
|---|---|---|
| Duplicate | `POST /api/v1/profiles/{id}/duplicate` | curator (existing `profiles/{rest...}` grant) |
| Clone | `POST /api/v1/profiles/{id}/clone` | curator |
| Disable / enable | `POST /api/v1/games/{id}/disable`, `POST /api/v1/games/{id}/enable` | curator (existing `games/{rest...}` grant) |
| Soft delete | `DELETE /api/v1/profiles/{id}` | curator |
| Purge | `POST /api/v1/profiles/{id}/purge` | **admin** (new grant, before the `profiles` wildcard) |

### Duplicate

`POST /profiles/{id}/duplicate` creates a new **draft** revision of the *same
edition* with the source revision's content copied — the same snapshot the
store already implements for `POST /profiles/{id}/revisions`
(`Store.NewRevision`, ADR-0015): every row gets a fresh id, all internal
references are rewritten, corrections and publication history are not copied,
and the parent link (`parent_revision`) records the derivation. The route
exists because FR-02 and the library UI ask for the *lifecycle verb*
"duplicate", and it is audited as `catalog.profile.duplicate` so the two
intents stay distinguishable in the audit trail even though they share the
store primitive. Duplicate takes no body (the source is the path), returns
201 with the new draft and its ETag.

### Clone

`POST /profiles/{id}/clone` copies a revision into a **new game** (optionally
with a caller-supplied `title`/`slug`; defaults are the source title and
`<source-slug>-copy`, suffixed `-2`, `-3`, … until unique), a matching
**edition** (region/language/revision/… copied), and a **draft** revision 1
whose content is the source's content snapshot. The provenance link is a new
immutable row in `clone_links` (source/target game, edition and profile ids,
actor, timestamp) — a table rather than a `sources` row because a clone is a
catalogue relationship between two revisions, not documentation of where one
revision's bytes came from, and the audit trail must be queryable in both
directions. All of it is one store transaction: a partial clone (new game
without the link) would be exactly the half-state the schema's FK discipline
exists to forbid.

### Disable

`games.disabled` (new column, default 0). A disabled game is **hidden from
Play and narration**: session start refuses any profile of that game with
`ErrGameDisabled` (409 `GAME_DISABLED`), which also removes the narration
path (narration only exists inside a session). It is **still editable**:
every catalog write (import, draft, publish, …) is untouched, and the game
stays visible in the library list with `disabled: true` so the UI can hide it
from the Play screen and keep it in the library. Game-level rather than
profile-level because "still editable" is only a meaningful statement at the
game level — the one profile a session can run is the published one, which is
frozen anyway.

### Soft delete

`DELETE /profiles/{id}` marks the revision `deleted_at` with `purge_after =
deleted_at + 72h` (the retention window is a store constant; it exists so a
mistake is visible and recoverable by support for a bounded time). While
marked:

* hidden from `GET /editions/{id}/profiles` (the library),
* hidden from `GET /profiles/{id}` (404 — the row exists, the surface does
  not),
* hidden from session start (`PublishedProfile` answers not-found for a
  soft-deleted live pointer, and `Start` refuses a deleted request target),
* **still on disk** — that is what the retention window is for.

Guards, each a 409 with its own problem code:

* **publication history.** A revision with any `publish_actions` row
  (published, archived or even rejected) cannot be soft-deleted. This is the
  0004 invariant kept intact: the audit trail outlives its subject, and the
  retirement path for a shipped revision is archive, not delete. Consequence
  stated plainly: soft delete applies to unreleased working revisions (drafts
  and reviews never offered to the publication gate). A draft that was
  attempted and rejected carries a `rejected` audit row and is retired the
  same way.
* **derived revisions.** A revision another revision descends from
  (`parent_revision`) cannot be deleted first; the schema would refuse it
  opaquely, so the API says so.
* **active session.** If this process's runtime has an active session
  (starting/running/stopping) pinned to the profile, the delete is refused.
  In practice a session can only pin a published revision — which the
  publication-history guard already excludes — so this check is the
  defense-in-depth half of the pair: it is the session-side statement of the
  same rule, at the layer that actually knows session state, and it keeps the
  guarantee true if the runtime ever runs a non-published revision.

### Purge

`POST /profiles/{id}/purge` is admin-only and audit-logged
(`catalog.profile.purge` via the auth audit trail). It requires `now >=
purge_after` (409 `RETENTION_NOT_ELAPSED` before the window closes),
re-checks the soft-delete guards, and then, in one store transaction:

1. deletes the profile's **speech-cache entries**: `speech_cache` gains an
   `original_message_id` column (migration 0017) recorded at cache-commit
   time (the key's identity already carries it; the column is the
   denormalised provenance that lets a purge address a profile's entries
   without inverting a hash). Legacy rows (and voice-preview entries, whose
   `original_message_id` is `voice-preview:<id>`) match nothing and survive;
   the cache key space itself is unchanged and stays versioned.
2. deletes the profile row; the 0002 FK CASCADEs remove its content rows and
   the FTS5 sync triggers drop the index entries with them (cascade delete
   is exactly what `TestDeletingProfileCascadesContentAndFTS` proves).

After the commit the API layer deletes the now-unreferenced **blobs** from
the asset store: for every hash the purge dropped (cache-entry blobs and the
profile's source/portrait references) it re-runs the catalogue reference
check (`AssetReferenced`) plus a speech-cache count — the same
reference-before-delete discipline as eviction (ADR-0041) — and only then
removes the bytes. Shared blobs survive, as the schema always intended.

## Schema (migration 0017)

* `games.disabled INTEGER NOT NULL DEFAULT 0 CHECK (disabled IN (0, 1))`
* `profiles.deleted_at TEXT` / `profiles.purge_after TEXT` (nullable,
  strftime-CHECKed, always set together)
* `clone_links` table + two lookup indexes (source profile, target game)
* `speech_cache.original_message_id TEXT` + index

All additive; no row rewrite, no key-space change. The 0004 RESTRICTs are
**not** weakened: the purge reaches them through the soft-delete guards
instead of through an FK change, so a database written by any older build
behaves identically until 0017 lands.

## Rejected alternatives

* **Duplicate as a new lineage (parent NULL).** The schema numbers revisions
  densely per edition and the lineage is the parent link; a "duplicate" that
  pretends to descend from nothing would make "what is this draft derived
  from" unanswerable for exactly the case the question exists for.
* **Clone as a `sources` row.** A clone is a catalogue relationship, not byte
  provenance; a sources row would name a URI the clone has none and hide the
  link from both-direction queries.
* **Profile-level disable.** The only profile a session runs is the published
  one, which is immutable; "hidden from Play but still editable" has no
  profile-level referent.
* **Changing `publish_actions.profile_id` to `SET NULL`.** It would let the
  purge delete shipped revisions, at the cost of the 0004 invariant the
  compliance review (docs/spec/14) leans on, via a table-rebuild migration.
  The audit trail outliving its subject is worth the narrower delete scope;
  shipped revisions retire via archive and stay addressable.
* **Purge as `DELETE` again (idempotent soft-then-hard).** Two verbs with two
  roles (curator soft, admin hard) and one audit row each is the shape docs/
  spec/14's "deliberate act" rule asks for; one idempotent verb would hide the
  admin-only boundary in the verb's semantics instead of in the scope table.

## Validation

* `internal/store/repo_library_test.go`: soft-delete guards (history, derived
  revisions, already deleted), retention window, purge cascade (profile +
  content + FTS + cache rows) and the restart property (re-opened store: the
  deleted profile is still deleted, the disabled game still disabled), clone
  transactionality and provenance row, backup/restore round trip of the new
  columns and tables (AGENTS rule 10).
* `internal/api/library_test.go`: success bodies for all five operations
  (validated against the OpenAPI document by the harness, so
  `TestEveryCatalogOperationWasExercised` sees them), role assertions
  (viewer/curator denied the admin purge; curator allowed the rest — and
  `TestRoleMatrixOverEveryRoute` walks them at every role), disabled game →
  session start 409, deleted profile hidden from lists and 404 on direct
  read, clone provenance readable back, and a restart test through the API:
  a deleted profile stays deleted and a disabled game stays disabled after
  the store is re-opened.
* `go test ./internal/store/... ./internal/games/... ./internal/api/...`
  with `-race`; `gofmt -l internal` and `go vet ./internal/...` clean.

## Consequences

* The library UI (a later task) gets five verbs with stable codes:
  `RESOURCE_NOT_FOUND`, `RESOURCE_CONFLICT` (+ the named 409 slugs),
  `RETENTION_NOT_ELAPSED`, `GAME_DISABLED`, `PROFILE_IN_USE`.
* Shipped revisions are not deletable by design; the archive → (never purged)
  path is permanent, which keeps evidence bundles (docs/spec/16) whole.
* Voice-preview cache entries and pre-0017 cache rows are never addressable
  by a profile purge; they age out through the existing eviction order.

## Reversal plan

Drop the routes and the admin grant (policy table), then the columns and the
`clone_links` table with a new migration (SQLite drops are one-way in this
codebase's forward-only discipline); `speech_cache.original_message_id` is
purely additive and safe to leave. No behaviour other library work depends on
changes: the soft-delete mark is only consulted by this ADR's readers.
