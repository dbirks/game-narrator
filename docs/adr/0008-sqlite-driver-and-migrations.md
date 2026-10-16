# ADR-0008: Pure-Go SQLite driver and embedded transactional migrations
Date: 2026-10-16
Status: accepted
Owners: project
Related requirements/tests: docs/spec/02 "SQLite schema principles" + "Migration and backup acceptance", docs/spec/12 Contract mode (SQLite migrations), docs/spec/15 "GoReleaser v2 plan" (CGO rule), docs/spec/16 Milestone 0; `go test -race ./internal/store/`, `TestRunMigrateEndToEnd`

## Context and constraints

- Releases must build with `CGO_ENABLED=0` and cross-compile from the Arch
  amd64 dev host to the Debian ARM64 UNO Q target (docs/spec/15).
- Milestone 2 retrieval needs **FTS5** and the **trigram tokenizer**
  (docs/spec/02 "SQLite schema principles"); a driver whose SQLite build lacks
  them would force an app-level n-gram index and a schema rewrite later.
- Backups must go through SQLite's online backup API, not file copies, because
  the database runs in WAL mode (docs/spec/02).
- Migrations must be applied transactionally, be idempotent, survive restarts,
  and make a mismatch between *shipped* schema definition and *applied* schema
  visible instead of guessable.
- Target hardware is a 4 GB appliance: one process, bounded connections, no
  external database server.

## Decision

Use **`modernc.org/sqlite` v1.60.1** (bundled SQLite 3.53.4, registered as
driver name `sqlite`) and implement persistence in `internal/store`:

1. `store.Open(ctx, path)` creates the parent directory `0700`, opens the file
   with the required pragmas expressed as **DSN parameters**
   (`_foreign_keys=1&_journal_mode=WAL&_busy_timeout=5000&_synchronous=NORMAL`)
   so every pooled connection gets them at connect time, then reads each pragma
   back from a live connection and fails `Open` if any is not in effect.
2. Migrations are numbered `.sql` files embedded from
   `internal/store/migrations` with `//go:embed`, applied in version order,
   **one transaction per file**, recorded in
   `schema_migrations(version INTEGER PRIMARY KEY, name TEXT, checksum TEXT, applied_at TEXT)`.
   `checksum` is the SHA-256 of the file bytes. `0001_init.sql` creates only
   `games`, `editions`, `profiles`, `sources`, `source_blobs`
   (docs/spec/02 "Entity catalog").
3. `Migrate` refuses to proceed when a recorded checksum no longer matches the
   embedded file (`ErrChecksumMismatch`) or when the database records a version
   this binary has no file for. Applied migrations are immutable history.
4. `Backup`/`Restore` use `sqlite3_backup_*` through `database/sql`'s
   `Conn.Raw`; `IntegrityCheck` requires `PRAGMA integrity_check` to report
   `ok`.
5. `game-narrator migrate [--data-dir DIR]` is the only schema-mutating CLI
   entry point. `serve` does not migrate yet, so a binary swap can never
   silently upgrade a running appliance's data; the ready check will call
   `SchemaVersion` in a later milestone. Data directory precedence is
   `--data-dir` > `GAME_NARRATOR_DATA_DIR` > `$XDG_DATA_HOME/game-narrator` >
   `$HOME/.local/share/game-narrator`, and the path must be absolute.

## Alternatives considered

- **`github.com/ncruces/go-sqlite3` v0.35.6** — same pure-Go property, a
  cleaner `driver.Conn.Raw()` accessor, and its backup API worked. Rejected:
  its embedded SQLite build has **no FTS5 at all**, which is a hard requirement
  for message/alias retrieval; adopting it would mean shipping a retrieval
  subsystem that cannot be built.
- **`github.com/mattn/go-sqlite3`** — feature-complete and fast, but requires
  CGO, which contradicts docs/spec/15 and removes the simple static
  cross-compile to ARM64.
- **Raw file copy for backup** — rejected by docs/spec/02: copying a live WAL
  database misses or splits `-wal` content.
- **Golang-migrate-style external tooling / `migrations/` at repo root** —
  rejected for now: embedding keeps the binary self-contained (single static
  artifact on the appliance) and avoids a second version-tracking format; a
  later ADR may supersede this if migration authoring needs up/down pairs.

## Evidence and test plan

Probes run on the Arch dev host (`go1.27.2-X:nodwarf5 linux/amd64`), literal
output. `modernc.org/sqlite` v1.60.1, `CGO_ENABLED=0 go run .`:

```text
fts5_plain: err=<nil>
fts5_trigram: err=<nil>
fts5_trigram_match: rows=1
backup_api: err=<nil> dest_exists=true
backup_readable: body="the ship sails north"
compile_option: ENABLE_FTS5
sqlite_version: 3.53.4        # from SELECT sqlite_version()
CGO0_OK_PROBE
```

`github.com/ncruces/go-sqlite3` v0.35.6 (driver package), same probe, same
`CGO_ENABLED=0`:

```text
fts5_plain: err=sqlite3: SQL logic error: no such module: fts5
fts5_trigram: err=sqlite3: SQL logic error: no such module: fts5
backup_api: err=<nil> dest_exists=true
backup_readable: body="the ship sails north"
```

A `PRAGMA compile_options` dump of that build (60 options) contains no FTS
entry at all, so this is a missing compile-time feature, not a usage error.

Repo-level verification:

```sh
go vet ./... && go test -race ./...
CGO_ENABLED=0 go build ./...                        # CGO0_OK
CGO_ENABLED=0 GOARCH=arm64 go build ./cmd/game-narrator   # static ELF aarch64, "not a dynamic executable"
D=$(mktemp -d); go run ./cmd/game-narrator migrate --data-dir "$D"; go run ./cmd/game-narrator migrate --data-dir "$D"
sqlite3 "$D/game-narrator.sqlite" '.tables' 'PRAGMA journal_mode;' 'SELECT version,name FROM schema_migrations;'
```

which prints `applied 0001_init.sql` then `schema version 1` on the first run
and only `schema version 1` on the second, and lists
`editions games profiles schema_migrations source_blobs sources` with `wal`.

Covering tests (`internal/store`, `cmd/game-narrator`): pragma read-back, `0700`
directory creation, blank-DB migrate + idempotent re-migrate + `SchemaVersion`,
foreign-key and CHECK/UNIQUE enforcement, delete-cascade versus RESTRICT
behaviour matching the SQL comments, checksum tamper refusal, unknown recorded
version refusal, interrupted-migration rollback, `splitSQL` quoting/comment
edge cases, backup → reopen → `IntegrityCheck` + identical rows, and
corrupt-then-restore, where zeroing page 1 of a closed copy is caught at open
time:

```text
Open detected the corruption: store: connect /tmp/.../corrupt.sqlite: file is not a database (26)
```

## Consequences and risks

Positive: one static binary with no system SQLite dependency; checksums make
"which schema is this file?" answerable; migration failure is atomic; backups
are consistent while the service runs; corruption is detected at `Open`.

Costs and risks:

- `modernc.org/sqlite` drags `modernc.org/libc`, `mathutil`, `memory` and a few
  small indirects (`go-humanize`, `google/uuid`, `go-isatty`, `go-strftime`,
  `bigfft`, `golang.org/x/sys`) into `go.mod`.
- Transpiled SQLite is slower than CGO SQLite. Not yet measured on target;
  Milestone 1 must benchmark 10k-message retrieval and the FTS5/trigram path on
  the UNO Q before thresholds in docs/spec/12 are trusted.
- `synchronous=NORMAL` trades a fsync per commit for write latency; on a
  device that can be unplugged this tolerates an OS-crash-safe but
  last-transaction-loss window. Revisit with power-loss measurements before
  release (docs/spec/13).
- `splitSQL` deliberately does not understand `CREATE TRIGGER ... BEGIN ... END`
  bodies; a migration that needs one must extend the splitter, and
  `TestSplitSQL` must fail first to prove the limitation was noticed.
- Backups and restores need a dedicated `*sql.Conn`; a busy database can make
  `Backup` wait up to `busy_timeout`.
- `foreign_keys`/`busy_timeout`/`synchronous` are per-connection in SQLite: any
  future code that opens the file outside `store.Open` silently loses them.

## Migration / rollback

The file is ordinary SQLite, so the driver is swappable without data loss:
`Restore`/backup files stay readable by any SQLite build. Replacing the driver
requires a new ADR with FTS5/trigram + `CGO_ENABLED=0` probe evidence, then
re-running `go test -race ./...`. Removing the checksum gate would mean deleting
the `checksum` comparison in `Migrate`, which is exactly the protection this ADR
exists to add, so it is not offered as an option. To unwind the schema itself:
`game-narrator migrate` on a fresh data directory plus `Restore` from a backup —
`0001_init.sql` is additive and no data exists below version 1.

## Follow-ups

- Benchmark modernc vs. CGO SQLite on UNO Q hardware (M1 search 10k messages);
  record numbers here when measured.
- Confirm on Debian ARM64 UNO Q hardware that the pure-Go build behaves (no
  hardware in the current dev environment: NOT RUN).
- Decide backup scheduling, retention and `-wal` checkpoint policy in the
  operability milestone (docs/spec/13).
- Wire `serve` readiness to `SchemaVersion`/`IntegrityCheck` (next milestone).
- When FTS5 tables arrive (M2), record here whether
  `tokenize='trigram'` or an app-side prefix index wins on 4 GB RAM.
- Review when Milestone 1 schema work starts.

## Follow-up 2026-10-16: the splitter now understands trigger bodies

The risk recorded above — "`splitSQL` deliberately does not understand
`CREATE TRIGGER ... BEGIN ... END` bodies; a migration that needs one must extend
the splitter, and `TestSplitSQL` must fail first to prove the limitation was
noticed" — is closed. `0002_content.sql` needed the FTS5 synchronisation triggers,
which is precisely the case the note named, so `splitSQL` tracks a trigger body:
a semicolon inside `BEGIN ... END` separates the body's statements and does not
terminate the `CREATE TRIGGER`. Detection runs over *words*, not bytes (a trigger
body full of string literals must not be fooled by the text inside them), and the
handling is deliberately narrow — only `BEGIN`/`END` nesting is tracked, with a
`CASE ... END` inside a body counted separately. A trigger whose `END` never
arrives fails the whole migration loudly and atomically (the remaining file
becomes one invalid statement, the transaction rolls back) rather than
half-applying: `TestSplitSQLUnderstandsTriggerBodies` and
`TestSplitSQLRejectsAnUnterminatedTriggerBody` cover both, and
`TestMigrationAppliesContentFileToBlankDatabase` proves SQLite itself accepts the
resulting FTS5 triggers, which no amount of splitting on its own can show. The
rest of this ADR stands unchanged; the migration-count claims in "Evidence and
test plan" describe version 1, and schema-version expectations in tests and in the
CLI now derive from `store.Migrations()` / `store.LatestSchemaVersion()` rather
than a typed number (ADR-0015).
