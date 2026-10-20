---
name: uno-q-setup
description: Set up a stock Arduino UNO Q (Debian 13 arm64) as a working Game Narrator appliance over SSH. Covers reachability and USB host-role checks, the arm64 .deb install with SQLite migrate, boot enablement (ADR 0081), the /etc/game-narrator/env service env file (0600, never printed), HDMI capture preflight, still-grab and OCR smoke, USB DAC baseline or best-effort Bluetooth audio, first-run admin bootstrap, and upgrade/rollback/uninstall. Use when given SSH access to a UNO Q and asked to bring up, verify or repair the Game Narrator appliance.
compatibility: Dev host with the game-narrator repo checked out (the scripts call the repo's own hardware preflight); key-based ssh to the board (BatchMode works); passwordless sudo on the board.
metadata:
  repository: game-narrator
  references: docs/manuals/18-hdmi-capture-on-uno-q.md, docs/manuals/19-install-on-uno-q.md
---

# UNO Q setup — stock Debian 13 to a working Game Narrator appliance

You have SSH access to a **stock** Arduino UNO Q (`<user>@<uno-q-host`,
Debian 13 arm64, no custom image). Goal: a running, honestly
self-reporting appliance: package installed, service enabled at boot,
env file configured, `game-narrator doctor` green or explaining itself,
capture and audio verified or explicitly NOT VERIFIED.

Rules for the whole run (public repo, AGENTS.md rule 5):

- **Never touch** `sshd`, `authorized_keys`, `sudoers` or network
  configuration. The preconditions below are required, not something you
  fix on the device.
- **Never print** the contents of `/etc/game-narrator/env` (or any
  gateway token) into logs, tickets, issues or screenshots. The scripts
  report variable *names*, mode, owner and line count only.
- **Report honestly.** Capture/audio stay NOT VERIFIED until
  `scripts/hw/run-on-device.sh` (the `make test-hardware` gate) passes
  on the appliance; a mocked or partial run is never presented as
  end-to-end (AGENTS.md rule 3).
- Use the placeholders `<user>@<uno-q-host>` in anything you record.

## Preconditions

1. `ssh -o BatchMode=yes <user>@<uno-q-host> true` — key auth works
   non-interactively.
2. `ssh <user>@<uno-q-host> 'sudo -n true'` — passwordless `sudo`.
3. For capture/audio: the board sits on a **powered** USB-C hub with the
   UVC HDMI capture dongle (and USB DAC or BT speaker) attached — wiring
   in [docs/manuals/18](../../docs/manuals/18-hdmi-capture-on-uno-q.md)
   "Wiring" and "Power and the hub".

## Decision flow

Run every step from the repo root. All skill scripts are **read-only by
default**; add `--apply` to make the (idempotent) change.

### 1. Whole-appliance preflight (read-only)

```sh
skills/uno-q-setup/scripts/preflight.sh <user>@<uno-q-host>
```

Rows: `ssh`, `sudo`, `arch`, `os`, `package`, `service`, `env-file`,
plus the repo's HDMI rows (`usb-host`, `usb-role`, `capture-node`,
`audio`, `service`, doctor) via `scripts/hw/capture-preflight.sh`.

| Finding | Action |
| --- | --- |
| `usb-host` FAIL | Dongle/hub missing or unpowered — fix the physical chain (manual 18 "Wiring"), rerun. |
| `usb-role` WARN (device mode) | The board cannot enumerate USB devices — see manual 18 "Is my board affected?" (dwc3 host-mode quirk). |
| `package` FAIL | Step 2 (install). |
| `service` FAIL (static) | Installed build predates ADR 0081 — upgrade to a build with ADR 0081, or `sudo systemctl enable` after the upgrade. |
| `env-file` FAIL | Step 3 (env). |
| `env-file` WARN unreadable | No passwordless sudo for the ssh user — grant it (manual 19 Preconditions), rerun. |
| `capture-node` FAIL / `audio` WARN | Steps 4–5. |

### 2. Install the `.deb` + migrate + enable at boot

Build or fetch the arm64 `.deb` (`make release-snapshot` or a tagged
release artifact; verify `sha256sum -c checksums.txt` locally), then:

```sh
skills/uno-q-setup/scripts/install.sh <user>@<uno-q-host> dist/game-narrator_*_linux_arm64.deb
skills/uno-q-setup/scripts/install.sh <user>@<uno-q-host> dist/game-narrator_*_linux_arm64.deb --apply
```

`--apply` copies the deb to the board's `/tmp`, runs `apt-get install`,
prints `game-narrator version`, applies pending migrations as the
service user (`--data-dir /var/lib/game-narrator`), and enables
`game-narrator.service` at boot if it is not already enabled (fresh
installs get this from the package postinst; existing installs' unit was
`static` — ADR 0081). See [references/install.md](references/install.md).

### 3. Service env file

```sh
skills/uno-q-setup/scripts/env-setup.sh <user>@<uno-q-host>          # read-only report
skills/uno-q-setup/scripts/env-setup.sh <user>@<uno-q-host> --apply  # scaffold only if missing
```

`--apply` creates `/etc/game-narrator/env` from the packaged
`env.example` at `0600 root:root` and **never overwrites** an existing
file. You then fill the values on the board (gateway URL/token, the role
`_MODEL` lines incl. the decision role — protocol `systemone` is its
default — and `SPEECH_ASR`; full table and semantics in manual 19
step 5 + manual 04; a commented line records the chosen audio sink).
Then start the service and run doctor:

```sh
ssh <user>@<uno-q-host> 'sudo systemctl start game-narrator
  && sudo sh -c "set -a; . /etc/game-narrator/env; set +a; game-narrator doctor --data-dir /var/lib/game-narrator"'
```

Doctor exits 0 on a first-run host (with a "what to do next" list) and 1
only when a *configured* component is actually broken — read the rows
before blaming hardware. See [references/install.md](references/install.md).

### 4. HDMI capture

The preflight `usb-host`/`usb-role`/`capture-node` rows cover the
first-boot checks; details, measured rates (RYBOZEN V316B
`345f:2131`: 1080p MJPEG ≈49 fps, 720p YUYV ≈59 fps), the dwc3 quirk and
the node-selection rules (never assume `/dev/video0`) are in
[references/capture.md](references/capture.md). Once a mode is chosen
on a real picture, grab stills with the repo-documented `ffmpeg`
command, and run the OCR smoke against the still with the one-shot
`game-narrator ocr` command (in the current tree; when the board's
installed build ships it — check `game-narrator --help`):

```sh
ffmpeg -f v4l2 -i /dev/videoN -frames:v 30 /tmp/probe_%02d.png
game-narrator ocr --image /tmp/probe_01.png        # product pipeline on the still
game-narrator ocr --device <by-id/by-path>         # one live frame, same negotiation
```

A black or blank still is a *source* problem (converter/splitter/HDCP),
not a software bug.

### 5. Audio output

**USB DAC on the hub is the dependable baseline** (manual 18 "audio"
row). Bluetooth is **best-effort** with a known, unrepaired stutter on
A2DP SBC (issue #3); use it only when the operator accepts that:

```sh
skills/uno-q-setup/scripts/bluetooth-audio.sh <user>@<uno-q-host> [audio-user]          # read-only
skills/uno-q-setup/scripts/bluetooth-audio.sh <user>@<uno-q-host> [audio-user] --apply  # linger + headless override
```

The proven recipe (pair/trust/connect via `bluetoothctl`, the classic-
transport re-pair when a dual-mode headset returns LE-only, the
WirePlumber headless override
`monitor.bluez.seat-monitoring = disabled`, `loginctl enable-linger`,
`clock.force-quantum 256`) and the known limitations are in
[references/audio.md](references/audio.md). Verify with a chime and a
TTS line (`pw-play`).

### 6. Admin bootstrap and UI access

Create the first admin from the board (loopback-only setup; the CLI
runs straight against the database), then an operator token for the
web UI; the UI is reached from the LAN via an SSH tunnel (loopback bind
by design). Exact commands in [references/admin.md](references/admin.md).

### 7. Lifecycle

Upgrade in place with `scripts/upgrade.sh` (same shape as
`install.sh --apply`); the full verified clean-install/upgrade/rollback/
re-upgrade/purge cycle is `scripts/release/lifecycle-on-device.sh`.
Rollback order (backup data dir **before** downgrading) and uninstall /
purge semantics in [references/lifecycle.md](references/lifecycle.md).
The data directory `/var/lib/game-narrator` is **never deleted by the
package** — back it up first; removing it is an explicit admin act.

### 8. Final verification

- `scripts/hw/capture-preflight.sh <user>@<uno-q-host>` — no FAIL rows.
- `game-narrator doctor` (env sourced) — green or a self-explaining row.
- `scripts/hw/run-on-device.sh <user>@<uno-q-host>` — the hardware
  smoke gate; record its `artifacts/gates/test-hardware-device.{xml,rc}`
  as evidence. Only after it exits 0 may you claim capture/audio
  verified (rule 3, rule 12: a NOT RUN gate is reported as NOT RUN).
- A short sustained run (`scripts/hw/sustained-on-device.sh`) for the
  evidence bundle when the milestone asks for one.

## Layout

- `scripts/` — idempotent, read-only-by-default, `--apply` for changes:
  `preflight.sh`, `install.sh`, `env-setup.sh`, `bluetooth-audio.sh`,
  `upgrade.sh`.
- `references/` — `install.md`, `capture.md`, `audio.md`, `admin.md`,
  `lifecycle.md`. They point at docs/manuals/18 and 19 rather than
  duplicating them.
