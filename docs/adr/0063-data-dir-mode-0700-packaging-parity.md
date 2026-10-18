# ADR-0063: The data directory is 0700 everywhere — packaging conformed to the spec, not the other way round

Date: 2026-10-18
Status: accepted
Owners: agent worker
Related requirements/tests: docs/spec/02 ("Asset store contract": "Directory 0700 for sensitive assets"), docs/spec/10 (UNO Q deployment: state `/var/lib/game-narrator`), docs/spec/13 (doctor reports, no silent self-heal), docs/spec/16 (evidence bundles), ADR-0008 (store opens the parent dir 0700), ADR-0014 (asset store root/blobs/fan-out dirs 0700, files 0600), ADR-0053 (deb/Arch packaging parity; its 0750 lines are superseded by this ADR for the data dir), `internal/doctor` (`storage.data_dir` check), `scripts/release/smoke.sh` (packaging mode contract)

## Context and constraints

On the real UNO Q, `game-narrator doctor` reported `DEGRADED storage.data_dir — mode 0750, want 0700` **right after a clean .deb install** (recorded in `docs/status/acceptance.md`, M8 row, 2026-10-18 run): the product's own package installed a state the product's own diagnostics call degraded.

The disagreement:

- The spec is unambiguous: docs/spec/02 requires the asset-store directory at **0700** ("Directory 0700 for sensitive assets"); ADR-0008 creates the store's parent dir 0700; ADR-0014 keeps the whole tree 0700/0600 because the same tree holds the SQLite catalogue, voice references, the speech cache and user-curated profiles. The `storage.data_dir` doctor check enforces exactly that contract and reports any group/other read access as DEGRADED with a `chmod 700` remedy.
- The packaging shipped **0750** in every place: `packaging/deb/postinst.sh`, `packaging/arch/post_install.sh` / `post_upgrade.sh`, `packaging/tmpfiles.d/game-narrator.conf`, the systemd unit's `StateDirectoryMode`, and `packaging/Dockerfile`.

Facts established before choosing:

- `/var/lib/game-narrator` holds user data (catalogue + assets + caches): 0750 grants read access to every member of the `game-narrator` group and, on a single-user appliance, to no one in particular — the mode buys nothing, and on any multi-account host it leaks.
- `/etc/game-narrator` is a different contract: root-owned, group `game-narrator` *read* access for the 0640 `config.yaml` (docs/spec/10: "base config in `/etc/game-narrator/config.yaml` … env file protected at 0600"). It stays **0750** — this ADR changes only the data directory, and the doctor does not check the config directory.
- `install -d -o U -g G -m M` on an **existing** directory re-applies owner/mode without touching its contents (verified locally: a 0750 dir with a file in it became 0700, file intact). So "postinst fixes the mode on upgrade" is a one-mode-digit change, not a migration: the package scripts are idempotent and already run on `upgrade` (deb postinst) and on the Arch `post_upgrade` hook.
- systemd's `StateDirectoryMode` only applies when systemd *creates* the directory at service start; an existing 0750 dir from a pre-fix install is not re-chmodded by the unit. That is fine: the package scripts (postinst/post_upgrade hooks, and the tmpfiles `d` line at boot under systemd) carry the enforcement, and the doctor keeps reporting any leftover 0750 as DEGRADED with the one-line `chmod 700` remedy until an upgrade (or the admin) fixes it.

## Options considered

- **Change the doctor to accept 0750.** Rejected: the spec (02), the store ADR (0008) and the asset-store ADR (0014) all state 0700, and the data dir genuinely holds user-curated content; loosening the check to match the packaging bug would make the package's leak the contract.
- **Change the packaging to 0700.** Adopted — the spec wins; the change is six one-line edits plus doc/ADR corrections, and the upgrade path is already sound (idempotent `install -d` re-chmods existing dirs, contents untouched).
- **Add a migration script / one-shot systemd generator.** Rejected: overkill for a mode digit; the existing idempotent hooks and the boot-time tmpfiles `d` line already re-assert 0700 on every upgrade and boot, and the doctor's remedy line is the bridge for pre-fix installs that see neither.

## Decision

- Every packaging surface creates the data directory at **0700** `game-narrator:game-narrator`:
  - `packaging/deb/postinst.sh`: `install -d -o game-narrator -g game-narrator -m 0700 /var/lib/game-narrator` (runs on install *and* upgrade — re-applies the mode to the pre-fix 0750 dir without touching its contents).
  - `packaging/arch/post_install.sh`, `packaging/arch/post_upgrade.sh`: same line (pacman runs `post_upgrade` on upgrades, so the legacy mode is fixed there too).
  - `packaging/tmpfiles.d/game-narrator.conf`: `d /var/lib/game-narrator 0700 game-narrator:game-narrator -` (boot-time enforcer, both formats).
  - `packaging/systemd/game-narrator.service`: `StateDirectoryMode=0700` (for systemd-created dirs).
  - `packaging/Dockerfile`: `install -d … -m 0700` (the container path).
- `/etc/game-narrator` stays **0750** `root:game-narrator` (config dir, 0640 `config.yaml`, admin-writable; unchanged by this ADR).
- The doctor is unchanged: it already encodes the spec (0700 ⇒ OK; group/other-readable ⇒ DEGRADED with `chmod 700 <dir>` remedy; it reports and never self-heals an existing dir).
- `scripts/release/smoke.sh` now asserts the contract **from the package itself**: step 4 extracts the shipped unit and tmpfiles from the amd64 .deb and requires `StateDirectoryMode=0700` and the `d /var/lib/game-narrator 0700 …` line; step 5 (bookworm container install) additionally asserts the *installed* directory is `700 game-narrator` via `stat` — so a clean install can no longer ship a state doctor calls degraded.

## Tradeoffs

- Pre-fix installs that are neither upgraded nor boot-enforced (e.g. a manual install on a systemd-free box) keep 0750 until the admin runs the doctor's one-line `chmod 700` remedy. Accepted: the doctor is the documented post-install verifier (docs/spec/15) and says exactly what to run.
- A stricter-than-before mode could in principle break an external tool that reads the data dir as another user. None exists in the product contract: everything that touches the dir runs as `game-narrator` (service user, container user, doctor probe). The spec's own rationale ("sensitive assets") is the reason for 0700, not an accident.
- The 0750 lines recorded in ADR-0053 and `docs/release.md` now describe history, not current behaviour: both were updated in place with the correction (ADR-0053 cross-references this ADR), per the "update superseded ADRs rather than rewriting history" rule.

## Verification

- `go test -race -count=1 ./internal/doctor/` — new `TestDataDirGroupReadableIsDegraded` pins the legacy 0750 state: DEGRADED, correct summary, `chmod 700` remedy, and the directory left untouched (no silent self-heal). Existing `TestDataDirIsCreatedWithPrivateMode` (created ⇒ 0700) and `TestDataDirWorldReadableIsDegraded` (0755 ⇒ DEGRADED) still hold; the full-fake-host run asserts overall OK on a 0700 dir.
- `scripts/release/smoke.sh` (run after `goreleaser release --snapshot`): step 4 fails a deb whose unit or tmpfiles are not 0700; step 5 fails a container install whose `/var/lib/game-narrator` is not `700 game-narrator`.
- Upgrade path verified locally: `install -d -m 0700` on an existing 0750 dir with contents → mode 0700, contents intact.
- On the UNO Q: after upgrading to a build containing this ADR, `sudo game-narrator doctor` must show `storage.data_dir` OK (or `chmod 700 /var/lib/game-narrator` if the upgrade path did not run); the M8 row of `docs/status/acceptance.md` carries that as the known fix for the 2026-10-18 run.

## Consequences

- Packaging and diagnostics now state the same contract, and the release smoke enforces it from the artifact, so the "install ⇒ doctor DEGRADED" state cannot ship again.
- `docs/release.md` (Arch install section) now documents the data dir as 0700.
- Any future packaging that wants a different mode must argue against docs/spec/02 + ADR-0014 in a new ADR; there is no "packaging says otherwise" escape hatch.

## Rollback / migration path

- Rollback = revert the six packaging edits and the smoke assertions (one commit). No data, schema, protocol, or config change.
- Migration of an existing 0750 dir needs no code: the next package upgrade (deb postinst `upgrade`, Arch `post_upgrade`, or the boot tmpfiles `d` line) re-applies 0700, and the doctor's remedy line is the manual bridge. A dir that was 0750 and is chmodded to 0700 loses nothing — it is a pure permission narrowing.

## Links / references

- `packaging/deb/postinst.sh`, `packaging/arch/post_install.sh`, `packaging/arch/post_upgrade.sh`, `packaging/tmpfiles.d/game-narrator.conf`, `packaging/systemd/game-narrator.service`, `packaging/Dockerfile` — the six 0700 sites.
- `internal/doctor/checks.go` (`checkDataDir`), `internal/doctor/doctor_test.go` (`TestDataDirGroupReadableIsDegraded` et al.).
- `scripts/release/smoke.sh` (steps 4 and 5 mode assertions).
- `docs/spec/02-data-and-storage.md` (Asset store contract), `docs/spec/10-hardware-and-deployment.md`, `docs/release.md` (install/upgrade sections), `docs/status/acceptance.md` (M8 row: the field observation this ADR fixes).
- ADR-0008, ADR-0014 (the 0700 contract), ADR-0053 (packaging parity; superseded by this ADR for the data-dir mode).
