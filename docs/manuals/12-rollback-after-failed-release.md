# Rollback after failed release

## Purpose

Return a deployment to the previous working version after a failed
release/upgrade. The full procedure — backup-first ordering, per-artifact
rollback (.deb / binary / container image), the schema rule, and
verification — is in **docs/release.md, section "4. Rollback"**, and this
manual deliberately does not duplicate it.

## Preconditions

- The previous version's artifact is still available: apt history (one prior
  version), the previously recorded binary from the release evidence bundle
  (`artifacts/<run-id>/`), or the previous image digest (recorded in
  `artifacts/<run-id>/manifest`).
- You can stop the service.

## Steps

1. Read docs/release.md §4 and follow it in order:
   **backup the data directory first (always)** — stop the service, then
   copy the durable data dir (a stopped process leaves the WAL consistent) —
   then reinstall the previous `.deb`/binary or `docker pull
   <repo>@<previous-digest>` and recreate the container.
2. **Schema rule:** if the new version applied a migration the old binary
   cannot read, restore the DB backup from step 1 **before** starting the
   old binary. Migrations are forward-only and never auto-downgrade
   (docs/spec/02, docs/release.md §4).
3. Start the service and verify: `game-narrator doctor` plus
   `/api/v1/health/ready` (docs/release.md §4, step 5).

## Verification

- `doctor` reports no `FAIL`; `/api/v1/health/ready` is 200.
- The service behaves like the pre-release state (catalogue, sessions,
  audio) and the backup from step 1 still exists on disk until you decide
  it is no longer needed.

## What NOT to do

- Never skip the step-1 backup "because it is only a rollback" — the data
  directory is the one unit (catalogue + assets + voice references), and a
  schema-forwarded DB can make the old binary unusable without it.
- Do not `dpkg --purge` to roll back: purge removes package **and** config
  files; it is the purge-semantics section (docs/release.md §5), not a
  rollback. `/var/lib/game-narrator` is never deleted by the package
  either — deleting it is an explicit admin act, with a backup.
- Do not roll back only the binary and leave the new `.deb` state half
  installed; complete the sequence so `systemctl status` and the package
  state agree.
