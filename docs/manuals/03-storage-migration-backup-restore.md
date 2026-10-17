# Storage migration, backup and restore

## Purpose

Move the durable data directory across schema versions and take/restore
consistent snapshots. The data directory is **one unit**: the SQLite
catalogue (`game-narrator.sqlite`, holding games/profiles/messages, jobs,
events, auth and voice state) plus the content-addressed assets beside it
(docs/spec/02). Restore both or neither.

## Preconditions

- You know where the data dir is: `--data-dir` flag, `GAME_NARRATOR_DATA_DIR`,
  else `$XDG_DATA_HOME/game-narrator` (or `$HOME/.local/share/game-narrator`).
  On the Debian service it is `/var/lib/game-narrator` (pinned by the unit).
- For a restore: the service stopped (`systemctl stop game-narrator` or kill
  the `serve` process) — a stopped process leaves the WAL consistent.

## Steps — migration (forward only)

```sh
bin/game-narrator migrate --data-dir <dir>     # idempotent; prints each applied migration + schema version
```

Migrations are embedded in the binary (`internal/store/migrations/`,
0001–0009 today) and recorded with checksums in `schema_migrations`, so a
binary and a database that disagree about what "version N" means fail loudly
instead of guessing. `doctor` opens and migrates the store the same way.
`serve` never migrates on boot — run `migrate` explicitly before starting a
new binary (docs/release.md §3).

## Steps — backup (today)

```sh
systemctl stop game-narrator                      # or stop `serve`
cp -a <data-dir> <backup-dir>                     # whole dir: DB + -wal/-shm if present + assets
```

A stopped process makes a plain copy consistent. **NOT YET IMPLEMENTED:** a
command-line online backup. The Go API `store.Backup` / `store.Restore`
exists and is tested (SQLite online backup API, 256-page steps, usable while
serving; `Restore` verifies the file is a migrated Game Narrator database
and reads the backup read-only) — tracked in docs/spec/02 "Migration and
backup acceptance" and docs/release.md §4 ("once exposed at the CLI").

## Steps — restore

```sh
systemctl stop game-narrator
# replace the data dir contents with the backup (DB + assets together),
# then start the matching binary version
systemctl start game-narrator
bin/game-narrator doctor                          # or /api/v1/health/ready
```

If the new version applied a migration and the old binary cannot read the
new schema, restore the DB backup **before** starting the old binary —
migrations do not auto-downgrade (docs/release.md §4).

## Verification

- `bin/game-narrator migrate` on the restored dir prints nothing pending and
  the expected schema version.
- `sqlite3 <data-dir>/game-narrator.sqlite 'PRAGMA integrity_check;'` → `ok`.
- The service starts, `/api/v1/health/ready` is 200, and the catalogue
  (games/profiles) is visible in `/library` or via the API.

## What NOT to do

- Never `cp` the database file while the service runs — a file copy misses or
  splits WAL content (docs/spec/02); stop it first, or use the online backup
  API once it is exposed at the CLI.
- Never restore the DB without its assets (or vice versa): approved voice
  references and cached media are content-addressed against this directory.
- Never run an old binary against a migrated schema without restoring the
  backup first.
- Never delete the data directory to "fix" a bad state (AGENTS.md rule 11) —
  back it up first, as in docs/release.md §4.
