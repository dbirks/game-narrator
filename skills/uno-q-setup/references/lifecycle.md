# Upgrade, rollback, uninstall

Manual 19 step 11 and docs/release.md §4–§6 are the operator runbooks;
this is the agent view. **Back up the data directory first** whenever
the outcome of the operation is not certain — `/var/lib/game-narrator`
is user data and is **never deleted by the package**.

## Upgrade in place

Copy the new `.deb` + `checksums.txt` to the board, verify, then
`scripts/upgrade.sh` (same shape as `install.sh --apply`: install,
migrate as the service user, keep boot enablement, doctor):

```sh
skills/uno-q-setup/scripts/upgrade.sh <user>@<uno-q-host> dist/game-narrator_<new>_linux_arm64.deb --apply
```

Then re-verify:

```sh
ssh <user>@<uno-q-host> 'sudo systemctl start game-narrator
  && sudo sh -c "set -a; . /etc/game-narrator/env; set +a; game-narrator doctor --data-dir /var/lib/game-narrator"'
```

`/etc/game-narrator/env` and `config.yaml` survive upgrades
(`conffile`/admin-created); the data directory always survives.
Upgrades never change boot enablement state (ADR 0081: postinst
enables on `install` only).

## Full verified cycle (clean install / upgrade / rollback / re-upgrade / purge)

From the dev host, with two arm64 `.debs` from clean committed trees
(different versions — the runner refuses same-version pairs):

```sh
scripts/release/lifecycle-on-device.sh <user>@<uno-q-host> \
  dist-prev/game-narrator_*_linux_arm64.deb \
  dist-cur/game-narrator_*_linux_arm64.deb
```

Requires key-based SSH + passwordless `sudo` on the board; writes only
`/tmp/gn-lc`, the pre-flight backup and package-managed paths; never
prints the env file; exits 0 only when every step PASSed and the device
ends on the current package. Docs: docs/release.md §6. Use this as the
evidence-grade proof of the lifecycle, and `upgrade.sh` for day-to-day
in-place upgrades.

## Rollback (to an older version)

Follow docs/release.md §4 **in order** — the order is the point:

1. **Back up the data directory first** (stop the service; a stopped
   process leaves the WAL consistent): copy `/var/lib/game-narrator`
   aside (e.g. `/home/arduino/gn-data.bak-<ts>`).
2. Reinstall the previous `.deb`. Newer → older is a **downgrade**:
   `apt-get install --allow-downgrades ./game-narrator_<old>_linux_arm64.deb`.
3. If the newer version applied a migration the old binary cannot read,
   **restore the DB backup before starting the old service**.
4. Verify with `doctor` + `/api/v1/health/ready`.

Manual 12 is the operator-facing runbook for a failed release.

## Uninstall / purge

```sh
sudo dpkg -r game-narrator        # remove: package files gone, /etc/game-narrator kept
sudo dpkg --purge game-narrator   # purge: package + config files gone
```

- `/var/lib/game-narrator` is **never deleted by the package** (user
  data: catalogue, assets, approved voice references). Back it up
  first; removing it afterwards is an explicit admin act, and
  `userdel game-narrator` is yours to decide (docs/release.md §5).
- If manual 19 step 2's bind mount was used, disable and unmount it
  afterwards: `sudo systemctl disable --now gn-data.mount`.
- After any of these, re-run the preflight
  (`skills/uno-q-setup/scripts/preflight.sh`) to record the resulting
  honest state.
