# ADR-0080: Monotonic-within-a-millisecond ULIDs, and the `ORDER BY id` audit the contract now carries

Date: 2026-07-04
Status: accepted
Owners: agent worker (task ulid-order)
Related requirements/tests: docs/spec/02 ("SQLite schema principles": ULIDs for public IDs), docs/spec/08 (keyset pagination cursors), docs/spec/12 (pagination property), `internal/games/ids.go` + `ids_test.go`, `internal/profileio/import_order_test.go`, the speaker-link Playwright spec (`web/e2e/speaker-link.spec.ts`)

## Context and constraints

The speaker-link Playwright flake's root cause: two message rows minted in the
same millisecond at import got ULIDs whose 80-bit random suffixes sort
arbitrarily, and the UI pages by id. `internal/store` lists messages,
characters, scenes, regions, sources and speaker assertions `ORDER BY id`
(`repo_content.go`), plus games/editions keysets, the job list, the auth audit
and the publish problem enumerators. Any consumer that assumes id order =
creation/script order (editor listing, export, keyset cursors, the speaker
link cell, bench truth) could shuffle lines imported in one batch — and the
flake pinned a set rather than an order precisely because the order was
unstable.

`ids.go` had documented the non-monotonicity ("within one millisecond the
random suffix decides") as a deliberate contract, on the premise that tables
needing a total order carry their own sequence column. The premise held for
events (`seq`), message pages (`ordinal`) and profiles (`revision_num`) — but
not for the content tables, where creation order *is* the semantic order and
no ordinal column exists.

## Options

- **Add a numeric script-order column to `messages` (and the other content tables), migrate, and sort by it.** Rejected: it duplicates the ULID's timestamp prefix for every row, requires a schema migration plus backfill for every existing database, and would have to be maintained by every writer (imports, agents, corrections). The ordering gap it fills is the one a monotonic entropy already fills at the mint.
- **Sort messages by `source_message_id`.** Rejected on the schema: it is a *source-side identity* (row number, BMG index, dump offset), TEXT-typed and nullable, numbering restarts per source, and lexicographic order breaks numerics at ten (`"10" < "2"` — the windwaker adapter formats it `fmt.Sprintf("%d", id)`). It is the reconciliation key (docs/spec/02), not an ordinal.
- **Sort by `created_at`.** Rejected: the column is second-resolution text; a whole import batch shares one value, so the sort degenerates exactly where the flake happens.
- **Monotonic entropy within a millisecond, process-wide, mutex-serialised (chosen).** A 48-bit millisecond prefix plus a per-millisecond counter is the standard monotonic ULID (the oklog/ulid `Monotonic` behaviour): fresh 80-bit randomness on the first mint of a millisecond, then the previous entropy advanced by one. Strictly ascending in call order, concurrent-safe, no schema change, no new dependency (ADR-0008's rule: a new module is a decision; this is a fixed-width increment).

## Decision

1. `games.NewID()` is now monotonic: every id sorts strictly after every id
   minted before it in this process, even inside one millisecond. The first
   mint of a millisecond draws fresh crypto/rand entropy; later mints advance
   the stored entropy by one (big-endian) and panic — rather than wrap to a
   suffix that sorts before issued ids — on the unreachable 2^80-per-millisecond
   exhaustion.
2. Clock guard: if the wall clock steps backwards (NTP slew, VM resume), the
   generator keeps minting at its high-water mark so a prefix can never
   regress below ids already handed out.
3. No `ORDER BY` query changes (the audit below). No schema or migration
   changes. The in-memory id sorts that produce deterministic *set* orderings
   (clip indexes, devtrace, provider role lists) are unchanged and now also
   creation-order.

### `ORDER BY id` site audit

| Site | Semantic order | Decision |
| --- | --- | --- |
| `repo_content.go:740` `loadProfileMessages` (→ `GetProfileContent`, export) | script = import order | keep id; monotonic ULID makes id order the import order of the process that imported (reconciliation keeps re-imported rows on their old ids; appended rows mint later) |
| `repo_content.go:713,1156` `ListMessages` keyset cursor (→ `api/messages.go`, `api/speakerlink.go`, agent tools, import reconciliation) | creation order (cursor contract, docs/spec/12) | keep id; the cursor is a row identity, monotonicity preserves the no-skip/no-repeat window |
| `repo_content.go:190/301/391/496/1046` sources, characters, scenes, regions, speaker_assertions | creation order; no ordinal column exists | keep id |
| `repo_catalog.go:120/205` games keyset, editions of a game | creation order; no ordinal column | keep id |
| `jobs_store.go:675` job list keyset | creation order; the picker already orders `(priority, created_at, rowid)` | keep id |
| `auth_store.go:903` auth audit `ORDER BY id DESC` | creation order (audit trail) | keep id; same-millisecond bursts are now deterministic |
| `repo_publish.go:636/647/658/681/699` problem enumerators | deterministic enumeration only, no script-order claim | keep id |
| `repo_content.go:953` edges `ORDER BY e.from_id, e.to_id, e.edge_type` | deterministic enumeration | keep |
| in-memory: `clips/index.go:78/84`, `support/devtrace.go:112`, `providers/roles.go:445/485/499`, `providers/router/slots.go:211`, `matcher/index.go:178` | stable set output, not a script listing | keep |

What the contract does *not* buy: two separate processes minting in the same
millisecond still interleave arbitrarily, as any ULID would — the counter is
per-process by construction. Tables whose order must survive that case already
carry their own sequence (`events.seq`, `message_pages.ordinal`,
`profiles.revision_num`, jobs' `(priority, created_at, rowid)`); the audit
above confirms no `ORDER BY id` site relies on cross-process same-millisecond
order.

## Tradeoffs

- Ids are now less random within a millisecond (a 16-line batch looks like a
  sequence). Public ids expose a bit more about mint timing; the timestamp
  prefix already exposed that per millisecond, and no secret material is
  encoded.
- The global mutex serialises mints. A mint is a few nanoseconds of integer
  work plus (once per millisecond) a crypto/rand draw; measured contention is
  negligible next to any I/O a caller does, and the alternative — per-goroutine
  counters — breaks the process-wide strictness the store queries assume.
- Exhaustion panics on 2^80 mints in one millisecond instead of silently
  wrapping; unreachable, and the panic is the only honest failure.

## Validation

- `internal/games/ids_test.go`: `TestNewIDsStrictlyAscendingInTightLoop`
  (10,000 mints, strict string order), `TestNewIDsStrictlyAscendingConcurrent`
  (16×512 across goroutines, -race: per-goroutine strictness + global
  uniqueness), `TestNewIDAdvancesEntropyWithinAMillisecond` (pinned
  high-water mark, exact entropy+1 rendering), `TestNewIDHoldsTheHighWaterMark`
  (backwards clock step keeps the prefix at the mark), `TestNewIDRefusesExhaustedEntropy`
  (all-ones entropy panics). Pre-existing shape/round-trip/sort-by-time tests
  unchanged and passing.
- `internal/profileio/import_order_test.go`: real CSV import of a 16-line
  batch (same-millisecond mints through the real adapter and store); asserts
  the `ListMessages` listing and the `Export` manifest both preserve script
  order and are id-ascending.
- `gofmt -l internal`, `go vet ./...`, and the gate's
  `go test -race -count=1 ./internal/store/... ./internal/games/... ./internal/profileio/... ./internal/api/...`
  all clean.

## Consequences

- The speaker-link UI's "line order is the ULID order" assumption is now
  correct for a single appliance (the deployment shape); the flake's
  pinned-set assertion can be re-tightened to pin the order once the UI-side
  task lands.
- Every existing `ORDER BY id` read is silently more correct: same-millisecond
  bursts (imports, event storms, audit bursts) are deterministic instead of
  random-per-run.
- Existing databases are unaffected: old ids stay where they sort (they were
  already time-ordered across millisecond boundaries); only ids minted after
  the upgrade gain the within-millisecond property, and a mixed ordering can
  only differ on rows from the same millisecond of different processes — the
  case the contract explicitly does not cover.

## Reversal plan

Restore the pre-ADR mint (drop `monoMu`/`monoLastMS`/`monoEntropy` from
`NewID`, draw fresh entropy per call) — no data migration, since no schema or
id layout changed. The regression tests then document the old behaviour
instead of guarding the new contract.
