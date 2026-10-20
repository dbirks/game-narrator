# ADR-0098: Global command-palette search (`GET /api/v1/search`)

Date: 2026-07-05
Status: accepted
Owners: agent worker (task search-api)
Related requirements/tests: docs/design/plum-peach/product-notes.md UX-02 (⌘K
general search), docs/design/plum-peach/plan.md Phase 1 item 2, docs/spec/08
("APIs", "role ACL"), docs/spec/14 ("Authorization"), ADR-0017 (hand-written
OpenAPI contract), ADR-0062 + cfd936f (FTS5 per-connection structure cache —
reads must run on the store's safe `inRead` path), `internal/store/repo_search.go`
(FTS5 phrase-escape contract), `internal/api/search.go`,
`internal/store/repo_search_global.go`, `internal/api/search_test.go`,
`internal/api/openapi/openapi.json` (`/api/v1/search`)

## Context and constraints

UX-02 opens a ⌘K overlay from any screen that searches **games, characters,
dialogue lines, traces, settings sections and actions**, grouped by kind;
↵ opens or executes a result, and Tab hands the query to the agent dock. The
plan assigns the backend to `internal/api`: one endpoint the palette calls on
every keystroke (debounced client-side), `GET /api/v1/search?q=&kinds=&limit=`.

Constraints:

- **The route is viewer+**, but its result kinds cross privilege tiers.
  Traces are the developer-evidence tier (docs/spec/08: "Developer mode can
  display sensitive gameplay text and provider traces; require explicit
  role"); the settings page and the settings API are admin; action verbs name
  the role they need. A single endpoint therefore cannot return one fixed
  shape for every caller: **result kinds are filtered by the caller's role**,
  the same rule the scope table applies to routes.
- **FTS5 is an injection surface** (`internal/store/repo_search.go`): the
  match string is *parsed*, so quotes, `OR`, `NEAR(`, `*` and unbalanced
  quotes must be neutralised by the existing `ftsPhrase` spelling, and the
  query must run through the store's safe read path. cfd936f / ADR-0062 show
  why the FTS reads must stay on the store's pooled `inRead` path (and, for
  integrity-style reads, on fresh connections) — a handler that opens its own
  connection would reintroduce the stale-view failure class.
- **Revision scope.** A `messages` row belongs to one profile row, and a
  profile row is one *revision* of an edition (`internal/store/repo_profiles.go`).
  A global line search that joins across every revision would list the same
  line once per revision of every edition. The scope this endpoint uses is
  *the latest revision per edition* — drafts included, because a game being
  set up is the most likely thing a curator is searching for.
- **Recency.** "Recent items get a boost" (UX-02 tasking) needs a time source;
  the store already mints `created_at` with `time.Now().UTC()`, so the boost
  uses the same wall clock and is a bounded, documented fraction of the score.
- **Bounded work on the appliance.** The event store has no text index; a
  trace lookup by line text or speaker is a bounded scan, shaped exactly like
  `handleDevTraces` (fixed page count × fixed page size), and runs only for
  callers who are allowed to see traces at all.
- **50 ms on the synthetic bench DB** (docs/spec/12's search-scale case is the
  10 000-message database; the benchmark below builds one and asserts the
  whole handler round trip is under budget). The bench texts are drawn from a
  100-phrase pool in which two phrases contain the bench query, so the query
  matches 200 of the 10 000 lines: bm25's honest cost is ranking the
  candidates that matched, and a bench in which every line matches the query
  would punish the handler for a match rate no real catalogue has.
  No game text in tests (AGENTS.md rule 5 — synthetic names only).

## Options considered

1. **Extend `GET /api/v1/profiles/{id}/messages/search` with a global mode.**
   Rejected: that route's contract is one revision — its ETag/If-Match
   semantics, its profile-scoped scope grant and its "a phrase that occurs
   nowhere is an empty array" shape are all per-revision. A global mode would
   conflate two contracts in one operation, and the palette needs five more
   kinds the route could not carry.
2. **One new FTS5 index over every surface (games, characters, lines).**
   Rejected: games and characters are small tables (a few dozen and a few
   hundred rows on an appliance); a `LIKE` scan with a documented score is the
   honest retrieval for them, and a new virtual table plus triggers plus a
   migration buys nothing at that scale. Messages already carry the required
   trigram FTS5 index (`0002_content.sql`), which is the expensive one.
3. **Chosen: one new endpoint with three retrieval shapes.** A new
   `GET /api/v1/search` (viewer) whose groups are, in order: `game` and
   `character` (bounded `LIKE` scans, ranked by match class and recency),
   `line` (the existing `messages_fts` trigram index, phrase-escaped, ranked
   by bm25 and recency, scoped to the latest revision per edition), `trace`
   (bounded event-store scan, developer-only), `setting` (a static registry of
   the /settings page's sections) and `action` (a typed registry of verbs,
   each carrying its target — SPA route or API call — and the role it needs,
   filtered by the caller). Each result is `{kind, id, title, subtitle,
   route, action?, score}` with `score` higher-is-better per group, and the
   response carries `agent_prompt`, the normalised query the Tab key hands to
   the agent dock.

## Decision

1. **Route and grants.** `GET /api/v1/search` is registered in
   `internal/api/api.go` and granted `Require(RoleViewer)` in
   `auth.DefaultPolicy` (viewer tier: the palette sits on every screen,
   including the kiosk viewer's, exactly like the status bar). Within the
   handler, kind availability is narrower than the route:
   - `trace` results are produced only for `RoleDeveloper` and above;
   - a `setting` section appears only if the caller's role opens the page it
     links to (admin, like `GET /api/v1/settings`);
   - an `action` verb appears only if the caller's role satisfies the verb's
     own `role` field (e.g. "start narrating" needs operator,
     "regenerate …'s voice" needs curator, "open settings" needs admin).
2. **Scoring.** One higher-is-better score per group:
   - name fields (games, characters) and the static registries: match class
     exact = 3.0, prefix = 2.0, substring = 1.0;
   - `line`: the FTS path scores `-bm25(messages_fts)` (higher-is-better
     direction of SQLite's own ranking; the per-revision endpoint keeps
     passing bm25 through unmodified — this endpoint is a different contract);
   - every kind adds a recency boost `max(0, 1 − age/90d)` ≤ 1.0, so a newer
     item can overtake within a match class but never across one (class gap is
     1.0, boost is bounded below 1.0);
   - an empty query is not an error: it is the palette's "recents" state, so
     each requested kind answers with its most recent items (class 0,
     recency-ordered).
3. **Limits.** `limit` is the per-kind ceiling (a palette shows at most one
   screen of any one kind): default 5, ceiling 20. `kinds` is a
   comma-separated subset of `game,character,line,trace,setting,action`;
   unknown kinds are a 400, because a silently-ignored kind is a client bug
   the response would hide.
4. **Injection safety.** User text reaches FTS5 only through `ftsPhrase`
   (the one spelling under which FTS5 reads it as text), reaches `LIKE` only
   through wildcard-escaped, lower-cased literals, and reaches nothing else —
   the store methods take one query string and a limit, nothing raw.
5. **Trace retrieval.** When the caller is developer+ and an event store is
   wired, the handler scans the durable log page by page (the
   `handleDevTraces` bound: fixed pages × fixed size) and matches, per
   envelope: trace id (exact, then prefix), then the event name, then the
   payload bytes (case-insensitive substring — the "line text or speaker"
   case, without parsing every event shape). One result per trace id (best
   match kept), score = class + recency from `seq`.
6. **Action verbs are data, not handlers.** The registry is a typed slice of
   `{id, label, role, route | api{method,path}, dynamic, titleOf}`; dynamic
   verbs ("start narrating <game>", "regenerate <character>'s voice") expand
   over the game/character matches the groups above already fetched — no
   extra queries. The one exception is the verb-prefix form: a query that
   starts with the verb's label ("start narrating zelda") reaches the entity
   through the query's *tail*, which the entity group (matched on the whole
   query) cannot see, so that one shape runs a single extra entity query, and
   only when the entity group came back empty. The registry lives beside the
   handler in `internal/api/search.go`, so a verb's route and its role are
   reviewable in one file.
7. **`agent_prompt`.** The response echoes the normalised query (trimmed,
   internal whitespace collapsed) so the UI's Tab handler has one source of
   truth instead of re-deriving it; an empty query echoes the empty string.
8. **Target routes and contract.** Target routes follow the plum-peach IA
   (ADR-0093) — the palette ships with the redesign, not before it: the
   library focuses a game (`/library?game=<id>`), character rows link to the
   Cast page (`/games/<game>/cast`), line rows open their trace
   (`/traces?line=<id>`), trace rows `/traces?trace=<id>`, and the
   health/diagnostics verbs point at `/health`. The `open health` verb is a
   route, and the `regenerate` verb is the registry's API-call target
   (`POST /api/v1/voices/auditions`), so both target shapes in the tasking
   are exercised by the shipped registry. In `openapi.json` the operation
   carries `x-required-role: viewer` plus a `SearchResponse` schema, so the
   standing contract tests (route ↔ document drift, role ↔ scope-table
   drift, body ↔ schema validation) cover the new route.

## Positive consequences

- The palette is one debounced call with one response shape; grouping,
  role-filtering and the Tab handoff are all in the body, so the UI (Phase 1
  `KSearch.tsx`) needs no per-kind endpoints and no client-side ranking.
- No schema change: lines ride the index `0002_content.sql` already builds
  and triggers already maintain, on the store's safe read path.
- Role filtering inside one endpoint is explicit and testable: a viewer gets
  no trace rows, no admin settings sections, no operator verbs — each
  absence is an assertion in `search_test.go`, not a hope.

## Negative consequences / risks

- A developer's trace search pages the durable event log per keystroke.
  Bounded (page count and size fixed, identical to the dev-trace route) and
  developer-gated, but it is the one O(history) shape in the endpoint; if it
  ever shows up in a bottleneck trace, the fix is a trace-id prefix index in
  the event store, not a widening of the scan.
- The latest-revision-per-edition scope is a `MAX(revision_num)` subquery per
  joined row; on the appliance's edition counts that is noise, but a catalog
  with thousands of editions would want a materialised "latest" column.
- `agent_prompt` duplicates `q` in the body on purpose (UI convenience);
  the OpenAPI description says the two are always equal after
  normalisation.

## How to verify (commands and evidence)

- `GN_GATE=test-unit go test -race ./internal/api ./internal/store` — the new
  `search_test.go` (per-kind results, role filtering, ranking, recents,
  FTS5 injection cases: quotes, `OR`, `NEAR(`, `*`, unbalanced quote) and the
  store tests for the three global retrieval methods.
- `go test ./internal/api -run TestOpenAPI` — route/document/role/schema
  drift checks include `GET /api/v1/search`.
- `go test ./internal/api -bench BenchmarkSearchHandler -benchtime 20x` —
  the synthetic bench DB (one game, one revision, 10 000 lines of which 200
  contain the bench query); the test asserts the median round trip is under
  50 ms.
- `make lint` (gofmt + vet) on the touched packages.

## Rollback / migration path

No schema or migration changes. Reversal is the deletion of the route
registration, the scope-table grant, the `openapi.json` entry,
`internal/api/search.go`, `internal/store/repo_search_global.go` and their
tests; nothing outside `internal/api` and `internal/store` references the
endpoint, and no client ships before the Phase 1 UI that calls it.

## Links / references

- docs/design/plum-peach/product-notes.md UX-02 and plan.md Phase 1 item 2
  (the ⌘K overlay and its acceptance)
- ADR-0017 (hand-written OpenAPI contract and its three test clauses),
  ADR-0016 (scope table), ADR-0024 (developer evidence tier), ADR-0062 and
  cfd936f (FTS5 connection-cache failure class)
- `internal/store/repo_search.go` (the per-revision search and the FTS5
  phrase contract this endpoint reuses)
