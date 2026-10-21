# ADR-0126: Name the resolved data directory at startup and in every store migration error

Date: 2026-10-20
Status: accepted
Owners: issue #31 (dbirks)
Related requirements/tests: AGENTS.md rule 4 (first reproduce, then repair) and rule 5 (honest diagnostics), docs/spec/02 "Asset store contract" (data-dir precedence: flag > `GAME_NARRATOR_DATA_DIR` > `$XDG_DATA_HOME/game-narrator` > `$HOME/.local/share/game-narrator`), docs/spec/13 "Diagnostics page and CLI" (doctor states), `internal/bootstrap` (`TestLoadConfigResolvesDataDir`), `internal/store/migrate_test.go` (tamper gate), `internal/doctor/doctor_test.go` (storage rows)
Links/references: GitHub issue #31, ADR-0008 (SQLite driver and migrations), ADR-0063 (data dir 0700), ADR-0088 (update cache in the data dir)
Supersedes / superseded by: —

## Context

Upgrading the binary on the UNO Q in a **user-level run** (the service env file plus `setsid nohup game-narrator serve`, no systemd unit) where the previous launch had passed the data directory on the command line and the env file did not carry `GAME_NARRATOR_DATA_DIR`: the restart resolved the platform default (`$HOME/.local/share/game-narrator`), which held a stale database from an older build, and the process died at once with

```
fatal: migrating store: store: migration 15 (0015_….sql): checksum mismatch: database recorded <sha>, embedded file hashes <sha>
```

Neither the `game-narrator starting` line nor the error named *which* database file was opened, so it read like the real data was corrupted or a migration had been edited. A rollback to the previous binary failed the same way (its "unknown recorded version" error also had no path), which made it look worse. Finding the real cause meant copying the database off the device and comparing `schema_migrations` by hand.

The three gaps behind one incident:

1. **Startup logs** (and doctor's storage row) do not say where the durable data actually lives or which setting decided it (flag > env > XDG > HOME default, docs/spec/02).
2. **Store migration errors** (checksum mismatch, a recorded version this build has no file for, an embedded-sequence gap) name the migration but not the database file.
3. **doctor** says nothing when a non-empty default data directory exists next to a different, explicitly configured one — the "two databases on one host" state that caused the incident.

## Options

1. **Log the data directory only, at `serve` startup.** Rejected: the fatal migration error is exactly the moment an operator needs the path, and the `migrate`/`admin`/`support-bundle` commands open the same store through the same resolution.
2. **A `--verbose`/`--debug` flag that reveals the path.** Rejected: a diagnostic that is needed every time an upgrade goes wrong must not be opt-in; the path carries no secret (docs/spec/13: reports describe paths and modes, never credentials).
3. **Thread the resolved data directory *and its source* through `bootstrap.Config`, log both at startup, name the database file in every `Store.Migrate` error, and warn in doctor when a stale default directory sits next to an explicitly configured one.** Accepted: smallest change that removes all three gaps, no protocol, persistence or behaviour change — only what the existing failure paths print.

## Decision

- **`internal/bootstrap`**: a new `DataDirSource` (`flag` | `env` | `default`) is recorded on `Config.DataDirSource` by `LoadConfig`/`DefaultConfig` through `ResolveDataDirWithSource`, the precedence chain of `ResolveDataDir` plus the origin of the value. `ResolveDataDir` keeps its signature and delegates, so existing callers (the API's settings section, tests) are untouched.
- **`serve`**: the `game-narrator starting` line gains `data_dir` (the resolved directory), `database` (`store.DBPath(dataDir)`) and `data_dir_source`.
- **`internal/store`**: `Store.Migrate` names the database file it is open on (`Store.path`) in **every** error it returns: the checksum mismatch (still wrapping `ErrChecksumMismatch` with `%w`, so `errors.Is` keeps working), the recorded-version-newer-than-build error, and the embedded-migration errors (gap, unreadable file) from the loader. The embedded loader runs through a one-line package seam (`loadMigrations`) so the wrapped error path is testable.
- **`internal/doctor`**: `Options` gains `DataDirSource` (the CLI passes `cfg.DataDirSource`). The `storage.data_dir` row reports the resolved directory, the database path and the source. When the source is explicit (`flag`/`env`) and the default location (`$XDG_DATA_HOME/game-narrator`, else `$HOME/.local/share/game-narrator`) exists and is **non-empty**, the row is `DEGRADED` — a "two databases on one host" warning with a back-up-both remedy — never `FAIL`: the configured directory is the one the service uses, and the default's contents are user data, not a fault. An unknown source (no source recorded) or a source of `default` does not trigger the warning.
- **Documentation** (the incident's root cause): `docs/manuals/19-install-on-uno-q.md` and `skills/uno-q-setup` (SKILL.md + `references/install.md`) state that a **user-level run's env file must carry `GAME_NARRATOR_DATA_DIR`** — the systemd unit pins it itself, so only a run outside the unit falls through to the platform default.

## Consequences

- An upgrade that lands on the wrong data directory now names the database on its very first log line and in the fatal error; the "is this the real data or a stale default?" question is answerable from the log alone (issue #31's reproduction).
- `errors.Is(err, store.ErrChecksumMismatch)` is unchanged — existing callers (doctor's `storage.database` row, the restore path) are unaffected; only the message gains a path.
- doctor's new `DEGRADED` state can change an existing green report to a yellow one on hosts that genuinely have a stale default directory (that is the point), and a scripted `doctor` consumer that treats DEGRADED as fatal must already exist for the mode-0700 row, so no new exit-code class is introduced.
- `Config` grows one field; any out-of-tree consumer constructing `Config` literals compiles but reports an empty source (doctor's row then omits the source and skips the stale-default warning) — the documented degradation, not a fault.

## Validation

- `internal/bootstrap`: `TestResolveDataDirWithSourcePrecedence` — flag/env/XDG/HOME/blank fall-through each resolve the same directory as before **and** the right source.
- `internal/store`: `TestMigrateErrorsNameTheDatabaseFile` — the checksum error (via the real tamper path), the unknown-recorded-version error and the embedded-sequence gap error (via the loader seam) each contain the store's file path; the checksum error still satisfies `errors.Is(err, ErrChecksumMismatch)`.
- `internal/doctor`: `TestStaleDefaultDataDirIsDegraded` and the `storage.data_dir` source-reporting assertions — a non-empty `HOME/.local/share/game-narrator` next to an explicit `flag`/`env` directory is `DEGRADED` and names both paths; an empty or same-location default is not; a `default`/unknown source warns nothing.
- `cmd/game-narrator`: the `game-narrator starting` line is asserted to carry `data_dir`, `database` and `data_dir_source` for all three sources (with `t.TempDir` + `HOME`/`XDG_DATA_HOME` overrides), and a CLI-level doctor run reports the `DEGRADED` row end-to-end.
- `go test -race` on `internal/bootstrap`, `internal/store`, `internal/doctor`, `cmd/game-narrator`; `make lint`.

## Reversal plan

Delete the `DataDirSource` field and `ResolveDataDirWithSource` (restore `ResolveDataDir`'s inline chain), drop the three fields from the starting log line, remove the path prefixes from the three `Migrate` error sites (and the seam), delete `Options.DataDirSource` and the stale-default block in `checkDataDir`, and revert the three documentation notes. No schema, wire format or package behaviour changes; databases and callers are unaffected.
