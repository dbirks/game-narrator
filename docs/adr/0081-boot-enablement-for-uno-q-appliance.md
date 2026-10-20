# ADR-0081: First-install boot enablement of `game-narrator.service` (the appliance is the service)

Date: 2026-10-19
Status: accepted
Owners: agent worker (task hdmi-preflight-fix)
Related requirements/tests: docs/spec/10 ("UNO Q software deployment" — "Service managed by `systemd`, unit `game-narrator.service`"), docs/spec/15 ("Debian/systemd package"), docs/manuals/18-hdmi-capture-on-uno-q.md ("First-boot checks"), docs/manuals/19-install-on-uno-q.md (Step 6), `packaging/systemd/game-narrator.service`, `packaging/deb/postinst.sh`, `scripts/hw/capture-preflight.sh` (`service` row)

## Context and constraints

The UNO Q appliance's entire product is the `game-narrator` service: capture,
OCR, decisions, speech and the web UI all run inside it. docs/spec/10
mandates the unit ("Service managed by `systemd` … with dedicated user,
restart policy, …"), and the first-boot preflight (manual 18) expects the
service row to be PASS once the appliance is set up.

But the shipped unit had no `[Install]` section, so `systemctl is-enabled`
reports `static`: it cannot be enabled at all, and nothing starts it after a
reboot. This was observed on the real appliance (RYBOZEN V316B capture card
attached, board rebooted with the powered hub): the service was dead and had
to be started by hand. The documented workaround was an admin step (manual 19
Step 6: `sudo systemctl enable --now game-narrator`) — which, without an
`[Install]` section, would have failed even if run. An appliance that is dark
until a human sshes in does not meet the product's intent.

## Options

- **Keep enablement manual, fix the docs.** Rejected: the observed failure
  mode (service dead after every reboot) remains; the appliance is unusable
  for live narration until a human intervenes.
- **Add `[Install]` and `systemctl enable` unconditionally in postinst.**
  Rejected: on upgrade it would re-enable a unit an admin deliberately
  disabled — Debian policy is that upgrade must preserve the admin's
  enablement state.
- **Add `[Install]` and enable on first install only (postinst, `$1 = install`).**
  Chosen: matches Debian convention (`deb-systemd-invoke`-style semantics;
  upgrade/`replace`/`configure`-only invocations never touch enablement),
  and lands at the one place that runs exactly once per fresh install on the
  appliance.
- **Also `start` the service from postinst.** Rejected: the package never
  ships `/etc/game-narrator/env` (manual 19 Step 5); starting before the
  env file exists would run the service unconfigured. The admin starts it
  after configuration; the preflight `service` row tells them exactly what to
  run when it is not up.

## Decision

1. `packaging/systemd/game-narrator.service` gains
   `[Install] WantedBy=multi-user.target` (the unit's existing
   `After=`/`Wants=` ordering against `network-online.target` is unchanged).
2. `packaging/deb/postinst.sh` runs
   `systemctl enable game-narrator.service` (guarded on `command -v
   systemctl` and `/run/systemd/system`, silently tolerant like the existing
   `daemon-reload`) **only when invoked with `$1 = install`** — fresh
   installs. `upgrade`/`replace`/`configure` invocations leave the
   enablement state untouched, so an admin-disabled unit stays disabled
   across upgrades. The postinst message now says the service is enabled at
   boot and how to start it (`sudo systemctl start game-narrator`).
3. `scripts/hw/capture-preflight.sh` `service` row: an inactive service is a
   WARN (with the exact remediation hint), not a FAIL — the preflight is a
   read-only diagnostic, and the fix is a packaging/admin action. The hint
   is exact and per enablement state:
   - inactive + `static` (unit not enableable, e.g. old package):
     `sudo systemctl enable --now game-narrator`
   - inactive + `enabled`: `sudo systemctl status game-narrator`
   - inactive + `disabled`: `sudo systemctl start --now game-narrator`
4. The Arch package (dev host) is intentionally unchanged: the dev machine
   runs the service through the dev flow, not as an appliance service.

## Tradeoffs

- postinst gains a dependency on systemctl behaving well (a failing
  `enable` is swallowed by `|| true`, same posture as the existing
  `daemon-reload`; a fresh install on a system without systemd — e.g. the
  bookworm container smoke — is unaffected because the guard fails first).
- The unit is now enable-able on any host that installs the .deb. That is
  intended: the .deb is the appliance release target (docs/spec/15).
- An admin who installs and then disables the unit gets a silent appliance
  after the next reboot until they remember; the preflight `service` row
  (inactive + disabled WARN) is the detection path.

## Validation

- Verified on the real appliance (pre-fix): `systemctl is-active` =
  `inactive`, `systemctl is-enabled` = `static` (no `[Install]` section —
  the root cause); after reboot with the hub attached the service was dead.
- `systemctl enable` on a unit without `[Install]` fails with
  "Unit is not loaded, or cannot be enabled" — so `[Install]` is a
  precondition for the postinst step, not optional.
- `sh -n` + `shellcheck` on the preflight and postinst scripts.
- No automated test parses the unit file or postinst (checked
  `internal/*_test.go`); the first-boot behavior is covered by the
  preflight's `service` row against the appliance (manual 13/18 hardware
  smoke).

## Consequences

- A freshly installed appliance comes up at boot with `game-narrator`
  running (after the admin has created the env file and started it once).
- Manual 19 Step 6 (`sudo systemctl enable --now game-narrator`) becomes
  redundant but harmless (idempotent) on fresh installs; docs owners should
  reword it to "verify enabled: `systemctl is-enabled game-narrator`".
- `postrm` already disables the unit on purge — unchanged, still correct.
- The preflight `service` row no longer FAILs on an inactive service; it
  WARNs with the exact command to run.

## Reversal plan

Revert the three hunks (unit `[Install]` section, postinst enable block,
preflight `service` case) — no data or schema is involved. On appliances
already enabled, the enablement symlink survives a downgrade of the package
until an admin disables it or `postrm` (purge) runs; that is the intended
preserve-admin-state behavior, so a revert is not a forced state change.
