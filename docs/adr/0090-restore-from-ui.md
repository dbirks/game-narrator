# ADR-0090: Restore from the UI (backup/restore pair, restart-with-swap)

Date: 2026-10-20
Status: accepted
Owners: g6-g11 worker
Related requirements/tests: docs/spec/09 (/settings "backup/export" — a consistent backup and the way to get back from it), docs/spec/08 (admin scope on state-changing routes), docs/spec/13 (operational manuals: backup/restore is an operator act with an evidence trail), docs/status/gap-audit-2026-10-19.md (G6: "RESTORE not (documented CLI act only)… on a LAN appliance browsed from another machine, a user cannot restore without SSH/file access"), ADR-0047 (no auto-update posture, settings page contract), ADR-0088 (the sibling ADR for the updates check, same /settings section); tests `TestAdminRestore*` (internal/api), `TestValidateBackup*`/`TestApply*` (internal/restore), Playwright restore walk (web/e2e)

## Context and constraints

The settings page can download a consistent SQLite backup (`POST /api/v1/admin/backup`) but the only documented restore path was manual: stop the appliance, replace the file on the host, start again. On a LAN appliance browsed from another machine that path needs SSH or physical access, so the backup the UI offers is not actually usable by the UI's user (gap G6).

Constraints that bind the shape:

- A restore is a **process-lifecycle act**: the live database file must be closed, replaced and reopened. The API must never restore into an open store in place.
- The backup is **untrusted input**: it is an upload from a browser. Format (SQLite header), schema marker (`schema_migrations` — an ordinary database is refused), integrity (`PRAGMA integrity_check`) and schema version (a backup **newer** than the binary is refused; an older one is forward-migrated by the normal boot path) must all be validated before anything is touched.
- The operator's current state must survive even a failed restore: an **automatic pre-restore backup** is taken (online-backup API, live connection keeps serving) and kept as the rollback artifact.
- The decision and its evidence must be **durable**: the appliance restarts mid-flow, so the swap description (plan) lives on disk in the data directory, and the outcome (applied/failed with reason, hashes, schema journey) is recorded to disk and to the durable event log.
- Admin-only by the scope table; every step visible on the admin-scoped event stream (nothing here is silent).

## Options

- **In-process restore (open a second connection, copy pages over, continue).** Rejected: SQLite's own online-backup/restore API requires the destination not be open; faking it with raw page copies races the live writer and leaves no clean "the process is now the backup" moment. It would also skip the forward-migration step that a restored file needs.
- **CLI-only restore (the status quo, polished).** Rejected for the product path: it is the honest unattended/remote path and stays (it is the same `internal/restore` machinery), but it does not close G6 — the gap is specifically that the browser user cannot do it.
- **Restart-with-swap (chosen).** The API validates, takes the pre-restore copy, writes a durable one-shot plan and answers 202; the process runs its normal graceful shutdown; the replacement's boot applies the plan before the listener (open, swap via `store.Restore` with roles reversed, reopen, forward-migrate, record). The plan is one-shot: it is cleared after the attempt, applied or failed.

## Decision

- `POST /api/v1/admin/restore` (admin): multipart upload of one backup file (field `backup`, 256 MiB bound enforced twice — on the request body before the form parser can buffer it, and on the staged part). Validation order: multipart → size → SQLite format → schema marker → integrity → version guard (422 `backup-newer-than-binary` when the backup outruns this build). Then pre-restore copy (base name named in the 202), then the plan (written last, so a process that dies mid-flow leaves no pending restore), then the `admin.restore.scheduled` event, then the 202 with the plan's operator-facing projection (hashes, sizes, schema versions — base names only, absolute paths stay server-side per docs/spec/08).
- The process-replacement seam is `api.WithRestarter`: in `serve` it runs the normal graceful shutdown after a flush grace for the 202. Under the documented deployment (the systemd unit, `Restart=always`) the replacement comes back within seconds and its boot applies the plan; without a supervisor the plan stays **pending** and `GET /api/v1/admin/restore` says exactly that (with the plan's evidence) — a restore that cannot be applied is never pretended to be.
- `GET /api/v1/admin/restore` (admin): the post-restart status the settings page renders — `none` / `pending` / `applied` / `failed`, with the recorded outcome (when, error, schema before/after, migrations applied, recovered-from-pre-restore flag).
- `internal/restore.Apply` is the boot step (`cmd/game-narrator` calls it after migration, before ready). It re-validates the staged file against the plan (hash, size, version guard) — the plan is durable state the restart carries and boot does not trust it blindly — and on any failure after the live file was touched recovers the live database from the pre-restore copy and still serves; the only hard failure is one where even that cannot open.
- The settings page shows the restore section (upload with the hash-of-what-you-uploaded, the pending/applied/failed banner after the restart) and the manual replacement note for the binary itself (ADR-0047's no-auto-update posture is unchanged by this ADR: this restores *data*, not the program).

## Tradeoffs

- The happy path costs a process restart (a few seconds of availability under the supervisor). That is the honest price of "the running process becomes the backup"; the alternative (in-place) was rejected above.
- The 256 MiB bound and the single-file shape are generous for the appliance's catalogue (text and pointers; binary content lives in the asset store, never in SQLite) while bounding what a mis-pointed multi-GB upload can do to the data partition.
- `pending` is a real state the UI must render (a supervisor that never restarts, a dev box), not an error: it carries the plan's evidence so the operator can act (restart by hand, or re-request the restore).

## Validation

- Unit: `TestValidateBackup*` (format, marker, integrity, version), `TestApply*` (no-op, swap, failed-swap recovery, plan re-validation) in internal/restore; `TestAdminRestore*` in internal/api (admin gate, 503 seams, malformed/foreign/newer/oversize refusals, happy path driven through the real `restore.Apply` with the restart seam faked, the four status states); OpenAPI contract + enum tests updated (the route, `RestoreScheduled`/`RestoreStatus`, the four-state vocabulary pinned to `internal/restore`).
- Playwright: the restore walk (backup download → mutate → restore upload → 202 banner with hash/pre-restore name → process restart → applied banner and the reverted state visible on the page) in web/e2e.
- Live evidence owed (Milestone gate): a restore under the real systemd unit on the release host, with the event-stream rows (`admin.restore.scheduled` → `admin.restore.applied`) in the support bundle.

## Consequences

- The backup the settings page offers is now actually usable from the settings page; the manual file-replacement procedure remains the documented remote/CLI path on the same machinery.
- A pending restore plan in the data directory is a durable fact: any future boot applies it, so a crashed restart does not lose the restore.
- The data directory gains `backups/pre-restore-*.sqlite` (kept artifacts) and `restore/` (staged file + plan + result) — both part of the support bundle's data-directory snapshot.

## Reversal plan

Remove the two routes and the `WithRestarter`/boot-`Apply` wiring; the settings page falls back to its documented-manual note (the current wording). No data-format change is introduced (the plan/result files are new and self-contained), so removing the feature leaves existing data untouched.
