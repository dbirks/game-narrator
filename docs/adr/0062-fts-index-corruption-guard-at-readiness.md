# ADR-0062: Guard FTS5 index corruption at readiness — rebuild the derived index only when the damage is provably index-only

Date: 2026-10-18 (amended 2026-10-19, see follow-up)
Status: accepted
Owners: agent worker
Related requirements/tests: docs/spec/06 (messages and FTS search), docs/spec/13 (bounded autonomous troubleshooting, graceful degraded modes), docs/spec/16 (evidence bundles; no simulation theater), ADR-0002 (SQLite as the content database; WAL + synchronous=NORMAL + BEGIN IMMEDIATE serialization), AGENTS.md rules 3/4/11, `internal/store` (fts: triggers, sync, repair), `cmd/game-narrator/main.go` (the readiness probe), `web/e2e/serve.mjs` (fresh data dir per run)

## Context and constraints

A fresh-data-dir e2e run (`play` project, packaged binary) failed the startup `PRAGMA integrity_check` exactly once with `fts5: corruption found reading blob <n> from table "messages_fts"`. It did not reproduce in the next three runs. The host was under heavy load at the time (Go race suites and another Playwright run; `/tmp` is tmpfs, near quota). No operator-visible user data was at stake — the run was a synthetic smoke profile — but the failure class matters: **the FTS5 index is derived state**, and the content of truth is the `messages` table.

Facts established before any design:

- Every `messages` write goes through `putMessage` (upsert via `ON CONFLICT (id) DO UPDATE`), the re-import path, `UpdateMessageSpeechText`, or `deleteMessage` — all in a single transaction, and all inside `InTx`, which serializes writers on `BEGIN IMMEDIATE` over a pinned connection. FTS is maintained by the three `AFTER` triggers from migration 0002. `NewRevision` copies content by inserting new rows, so the triggers run. **No path writes `messages` (or `messages_fts`) outside the triggers.** A desync through a "path that updates without the trigger" is structurally excluded by review.
- The field error is a *structural* read failure of the inverted index (`corruption found reading blob <id> from table "messages_fts"`), i.e. a damaged page in the `messages_fts_data` shadow table — not a content/index desync. The FTS5 `integrity-check` command was verified (probe) **not** to check external-content consistency, so the field signature cannot come from that command; it is the `PRAGMA integrity_check` probe, as wired in `Store.IntegrityCheck`.
- WAL + `synchronous=NORMAL` + a checkpoint on clean close make the B-tree structure crash-safe: a process killed mid-transaction leaves the pre-transaction or the post-commit image, never a torn one.

## Investigation (reproduce first, per AGENTS.md rule 4)

Automated, race-enabled stress tests were added to `internal/store` (`fts_stress_test.go`) and run to the point of exhaustion:

1. **Concurrent writers through the pool** (eight goroutines: `PutMessages` inserts, full re-imports, `UpdateMessageSpeechText`, deletes) plus a FTS reader on a second store: no corruption in five `-race` runs.
2. **Process killed mid-transaction**: a child store binary SIGKILLed at eight varied delays during a 3,000-message import; every kill left a database that reopens clean.
3. **Disk full mid-write** (`PRAGMA max_page_count` capped): the write fails with the typed `ErrNoSpace`, the WAL rolls back, the file reopens clean, and search still works.
4. **A non-trigger path that desyncs FTS**: none exists (see the review above); the FTS5 command check additionally *cannot* surface a desync, so this class does not match the field signature anyway.

**None of the four hypotheses reproduced the field failure.** The probe suite (`/tmp/fts-probe`, driver `modernc.org/sqlite v1.60.1`) established the failure geometry instead: zeroing one byte in a small (unfilled) `messages_fts_data` page reproduces the exact field signature — `fts5: corruption found reading blob <id> from table "messages_fts"` — deterministically, and the FTS5 `'rebuild'` command restores a clean database with the content byte-identical. A full 4,096-byte page absorbs one zeroed byte in its slack, so the repro is page-geometry-dependent. The root cause of the single field event is therefore **unproven**: the candidate remains byte-level damage to a checkpointed page (OS/tmpfs under memory pressure, or an interaction in the packaged binary's driver build). Per the task and AGENTS.md rule 11, this ADR does not claim a root cause and no change touches user rows.

## Options considered

- **Do nothing (trust WAL).** Rejected: the field event shows the startup check *does* fire, and today it leaves the process down with no operator answer beyond the raw error.
- **Auto-rebuild on any integrity failure.** Rejected: rebuilding over damaged *content* would mask data loss — the index would be rebuilt from a damaged table and the failure would become opaque.
- **Classified guard: rebuild only when the damage is provably confined to the derived index; refuse otherwise.** Adopted.
- **Per-table checks plus `VACUUM` at startup.** Rejected: slower, broader blast radius, and `VACUUM` does not rebuild the FTS index from content.

## Decision

A classified self-heal, wired into the readiness probe (`storeReadiness` — the function `serve` polls and `/api/v1/health/ready` reports):

- `store.IntegrityProblems` runs `PRAGMA integrity_check` and returns the problem lines (a check that cannot even complete is an error, never a pass).
- `store.IsFTSOnlyDamage` classifies: every line must mention the derived index (`_fts` / `fts5`). **Any other line refuses** — the classifier errs toward "I don't know" and never toward "I can fix it".
- `store.RebuildMessagesFTS` runs the FTS5 `'rebuild'` command inside an explicit transaction. `'rebuild'` reconstructs the index from the content table and does not touch user rows.
- `store.RecoverDerivedFTS` serializes recovery behind a process-wide mutex, re-checks under the lock (a prior probe may already have repaired), refuses non-FTS-only damage with a message that names the backup/restore answer, runs the rebuild, and re-checks: it reports repaired **only if the post-rebuild check is clean**.
- `storeReadiness` calls the guard on an integrity failure: repaired ⇒ the probe reports ready and the recovery is logged at `Error` level ("rebuilt from messages and re-verified clean; no user rows touched"); refused ⇒ the probe reports not-ready with a check entry telling the operator to restore from the last good backup.

What the guard never does: modify or delete content rows, touch the migration journal, or accept a still-failing post-rebuild check.

## Tradeoffs

- A false "not FTS-only" means the operator restores from backup — a lossless path. A false "FTS-only" would be a data-loss path; the conservative classifier exists to make that direction impossible for anything the check can name.
- The rebuild runs at readiness-probe time, not at process start: the first probe after a repair costs one integrity check plus one `'rebuild'` (O(database)). Acceptable — both are one-shot and logged.
- The guard does not find the root cause. It converts the one field event that was observed into a self-healing, loudly-logged outcome for the known-recoverable class, and a loud failure for everything else. If the root cause is later identified (e.g. a driver bug in the packaged binary), the guard remains correct and harmless.
- `PRAGMA integrity_check` is the only reliable probe here: the FTS5 command's `integrity-check` silently skips external-content consistency, so it is never used by the guard or the tests.

## Verification

- `go test -race -count=1 ./internal/store/` — new `fts_stress_test.go` (three stress tests) and `fts_repair_test.go` (three regression tests, including a byte-for-byte repro of the field signature: a zeroed byte in `messages_fts_data` → `fts5: corruption found reading blob <id> from table "messages_fts"` → repair → clean check, content digest unchanged).
- `go test -race -count=1 ./cmd/game-narrator/` — `TestStoreReadinessRepairsOnlyADamagedFTSIndex` drives the real readiness function through both directions (index-only damage ⇒ ready after rebuild; content damage ⇒ fail entry) and asserts the operator-facing log lines.
- Stress suite re-run under `-count=5` to hold against the race detector.

## Consequences

- An operator who meets the field failure again gets one of two explicit outcomes: a logged self-heal (index rebuilt from content, verified clean) or a readiness failure that names the restore-from-backup answer. No silent data mutation in either case.
- The e2e harness is unchanged; fresh data dirs per run remain the correct behaviour.
- The root cause stays unproven and open. Any future repro that pins it (e.g. page geometry in a specific packaged build) gets a follow-up ADR; this guard is the containment, not the fix.

## Rollback / migration path

Remove the `RecoverDerivedFTS` call from `storeReadiness` (one call site) and delete `internal/store/fts_repair.go` plus its tests. No schema, migration, protocol, or data change: the guard is a code path in the readiness probe only.

## Follow-up (2026-10-19): the "corruption" was a stale per-connection view, not a damaged file

Status: accepted (amendment; supersedes the "root cause stays unproven" conclusion of the original entry)

### Field event

On the real UNO Q (eMMC/ext4, not tmpfs) the device service went not-ready at
11:05:48 with `database integrity check failed … fts5: corruption found reading
blob 1099511627777 from table "messages_fts"`, repeated on every readiness
probe for 7+ minutes while the device was **idle**. Preceding workload
(journal): an `import.file` job, several replay sessions, then API cleanup of
the run's profile/session. After a clean `systemctl stop`, the copied DB
passed `PRAGMA integrity_check` and an FTS5 `integrity-check` on disk, and a
restart came up ready 200. **The on-disk data was never corrupt: a running
process's view was.** (The build at the time, 0cbebce, predates the guard this
ADR introduced; had it carried dc29274, the guard would have rebuilt a
healthy index on the phantom report.)

### Reproduction (AGENTS.md rule 4: first reproduce)

Deterministic in-process repro on the installed driver (modernc.org/sqlite
v1.60.1, transpiled SQLite 3.53.4): with one Store and the real migrations,
a journal-shaped workload (import → FTS writes via the 0002 triggers on one
connection; reads on another; profile-delete cascade on a third) leaves the
following state matrix when `PRAGMA integrity_check` runs afterwards:

- **idle pooled connection that had previously read `messages_fts`** →
  `fts5: corruption found reading blob <n> from table "messages_fts"` with a
  garbage blob id (the field's 1099511627777; the repro produced
  2611340115969 / 1786706395137) — the exact field signature;
- the same connection's own FTS5 MATCH queries still return correct results,
  a connection holding a *stale read snapshot* reports clean, a connection
  with no prior FTS read reports clean, a fresh connection and a second
  `sql.Open` on the file report clean;
- no WAL checkpoint is needed for the phantom report (it appears as soon as
  the index is rewritten on another connection) and a PASSIVE checkpoint does
  not clear it;
- **any subsequent statement on the stale connection clears the phantom
  view** — including non-FTS statements;
- **`PRAGMA integrity_check` itself populates the stale view**: a dedicated
  long-lived connection that checked once, then idled across a rewrite, lies
  on its next check with the identical garbage blob id.

### Mechanism (driver internals, verified in the vendored driver source)

The FTS5 module caches the inverted-index structure per connection: the
per-connection index object (`TFts5Index`) carries a prepared
`PRAGMA <db>.data_version` statement (`_fts5IndexDataVersion`) and the
structure cache (`TFts5Structure`, cookie-validated via the `%_config` shadow
table; the per-connection cookie `TFts5Config.FiCookie` advances only on that
connection's own FTS writes). In this build the cache is **not invalidated by
commits made on other connections** — only by the next statement on the
connection itself. A pooled connection that read `messages_fts`, went idle,
and then watched the index get rewritten elsewhere keeps walking the old
structure: its shadow-table page rowids were deleted or replaced by the
rewrite, so the structure walk hits rowids that no longer exist and reports
"corruption found reading blob". The blob id in the message is the stale
structure's page rowid, which is why the field value is garbage, not a page
number. A fresh connection decodes the structure the last writer persisted
(the committed on-disk truth) and always reports clean — which is exactly
what the field restart proved. Whether this is a transpilation artifact or a
latent upstream bug in SQLite 3.53.4's FTS5/WAL interaction could not be
settled from here (the driver changelog v1.55.0–v1.60.1 carries no FTS5 fix,
and no verified upstream fix was available); the boundary fix below is
correct either way.

### Decision (amendment)

The original guard stands and is unchanged in behaviour for *real* damage.
What changes is **where the check runs**: `Store.IntegrityCheck` and
`Store.IntegrityProblems` no longer run `PRAGMA integrity_check` on the
shared pool. They open a dedicated throwaway connection — a fresh
`sql.Open` on the same DSN, `SetMaxOpenConns(1)`, closed when the check
finishes (plus a file-existence guard so an unlinked database file is
reported, not silently re-created as an empty one). Consequences:

- the readiness probe and the doctor can never surface a stale per-connection
  view as damage, so the field failure mode (idle device, phantom FTS
  corruption, not-ready for minutes) is closed at the boundary;
- the guard's `RecoverDerivedFTS` now **confirms damage on a fresh
  connection** before rebuilding: its pre-rebuild check, its re-check under
  the repair lock, and its post-rebuild verification all run on fresh
  connections, so a stale pooled view can never trigger a rebuild of a
  healthy index (pre-fix, the guard would have rebuilt and reported
  "repaired" on the phantom report);
- a dedicated long-lived connection is deliberately *not* used: the check
  itself populates the per-connection FTS5 view, so a reused dedicated
  connection poisons itself identically after idling across a rewrite
  (reproduced above); the only provably clean vantage point is a connection
  that has never run an FTS5 statement in this process.

The cost is one SQLite open per integrity check — milliseconds against the
O(database) walk the check takes, at readiness-probe and doctor frequency.

### Options considered (amendment)

- **Driver upgrade.** Rejected for now: v1.60.1 is the current release,
  its changelog carries no FTS5 fix, and no verified fix existed to adopt;
  the boundary fix is strictly safer and survives a later driver fix. If a
  driver release lands a fix, the stale-view regression test (which pins the
  mechanism and names the update path) is the place to notice it.
- **A dedicated long-lived check connection.** Rejected: the check itself
  populates the per-connection FTS5 view (reproduced), so it poisons itself
  after idling across a rewrite — the field failure with a different
  lifetime.
- **Refresh the pooled connection before checking (e.g. run a probe
  statement first).** Rejected: it relies on the quirk that *any* statement
  clears the view, which is undocumented driver behaviour and would leave
  the check racing the very connection it just "warmed".
- **Ban long-lived read transactions / checkpoint before the check.** Not
  the cause: the repro shows a stale *snapshot* connection reports clean and
  no checkpoint affects the phantom view; the cache, not the WAL snapshot,
  is the stale state.

### Verification

- `internal/store/fts_stale_view_test.go`, `TestIntegrityCheckIgnoresStalePooledFTSView`
  (failing-first: with the check on the pool it fails with the phantom line;
  with the fresh-connection check it passes; also pins the mechanism — the
  stale pooled connection reports the field signature while a fresh
  connection reports ok) and
  `TestRecoverDerivedFTSNeverRebuildsOnAStaleView` (failing-first: pre-fix
  the guard reported a repair of a healthy index; post-fix it reports
  nothing to repair and rebuilds nothing).
- `go test -race -count=3 -run 'FTS|Integrity|Stale|Conn' ./internal/store/`
  and the full `./internal/store/`, `./cmd/game-narrator/`,
  `./internal/doctor/` suites (the readiness guard tests
  `TestStoreReadinessTracksTheLiveDatabase` /
  `TestStoreReadinessRepairsOnlyADamagedFTSIndex` still drive both guard
  directions through the real readiness function).
- Note: `cmd/game-narrator` carries one pre-existing failure unrelated to
  this change — `TestRunSupportBundleEndToEnd` expects the ADR-0069
  pre-`bottlenecks.json` archive shape; it fails identically with this
  change reverted.

### Rollback (amendment)

Revert `integrityCheckConn` out of `IntegrityProblems` (and restore the
inline pragma loop in `IntegrityCheck`) — one function per file; delete
`internal/store/fts_stale_view_test.go`. No schema, migration, protocol, or
data change. The original guard's rollback path is unchanged.

## Links / references

- `internal/store/fts_repair.go`, `internal/store/fts_repair_test.go`, `internal/store/fts_stress_test.go` — the guard, its regression tests, and the stress suite.
- `cmd/game-narrator/main.go` — `storeReadiness` (the guard's wiring) and its readiness tests.
- `internal/store/migrations/0002_content.sql` — the FTS table and sync triggers; `internal/store/fts.go` — the content sync and `RebuildSearchIndex` (the manual, admin-facing rebuild this guard reuses in spirit).
- ADR-0002 (SQLite storage: WAL, `synchronous=NORMAL`, write serialization), ADR-0056 (the reimport surface whose stress test shaped the writer suite).
