# Install, migrate, boot enablement, env file

The full operator runbook (root-partition space, data partition bind
mount, every step with real flags) is
[docs/manuals/19-install-on-uno-q.md](../../../docs/manuals/19-install-on-uno-q.md)
— this file is the agent's condensed path and the reasoning behind each
step.

## The `.deb`

- Build on the dev host: `make release-snapshot` (writes
  `dist/game-narrator_*_linux_arm64.deb` + `checksums.txt`), or take the
  artifact from a tagged release (docs/release.md).
- Always verify before transfer: `sha256sum -c checksums.txt` (from the
  directory holding the deb).
- `scripts/uno-q-setup/scripts/install.sh` (this skill) copies the deb to
  the board's `/tmp`, installs with `apt-get`, and reports state.
  Re-running is a no-op when the same version is installed.

```sh
skills/uno-q-setup/scripts/install.sh <user>@<uno-q-host> dist/game-narrator_*_linux_arm64.deb
skills/uno-q-setup/scripts/install.sh <user>@<uno-q-host> dist/game-narrator_*_linux_arm64.deb --apply
```

## Migrate

After install, apply pending SQLite migrations **as the service user**
so the files it touches are owned by `game-narrator`:

```sh
sudo -u game-narrator game-narrator migrate --data-dir /var/lib/game-narrator
```

Idempotent: prints each newly applied migration; none pending → clean
run. The service itself never blocks on migration output (`serve` logs
it instead, docs/spec/02).

## Boot enablement (ADR 0081)

`docs/adr/0081-boot-enablement-for-uno-q-appliance.md`: the appliance's
whole product is the `game-narrator` service, so it must survive a
reboot.

- **Fresh installs**: the package `postinst` runs
  `systemctl enable game-narrator.service` exactly once (on `install`,
  never on `upgrade` — an admin-disabled unit stays disabled across
  upgrades, Debian convention).
- **Existing installs** (pre-ADR 0081 build, `is-enabled` = `static`):
  the unit file has no `[Install]` section, so a bare `systemctl enable`
  fails ("Unit is not loaded, or cannot be enabled"). Upgrade to a build
  with ADR 0081 first — `install.sh --apply` / `upgrade.sh --apply` with
  a newer .deb — then it runs `daemon-reload` + `systemctl enable`, which
  succeeds against the new unit file.
- The package deliberately does **not** *start* the service at install
  time: without `/etc/game-narrator/env` the service runs
  unconfigured. Start it after the env file exists (below).
- Verify: `systemctl is-enabled game-narrator` → `enabled`.

## The env file (never printed)

`/etc/game-narrator/env` is the unit's `EnvironmentFile`. It is never
shipped in the package; the admin creates it from the packaged
`/etc/game-narrator/env.example` at **0600 root:root**
(`env-setup.sh --apply` scaffolds it if and only if it is missing):

```sh
sudo install -m 0600 -o root -g root /etc/game-narrator/env.example /etc/game-narrator/env
```

Fill in (placeholders; roles/semantics in manual
[04](../../../docs/manuals/04-inference-gateway-connection.md), full
table in manual 19 step 5 — a role is enabled only when its `_MODEL`
variable is set):

```ini
GAME_NARRATOR_GATEWAY_URL=<gateway-base-url>
GAME_NARRATOR_GATEWAY_TOKEN=replace-with-real-secret-outside-git

# one _MODEL line per role you will use:
GAME_NARRATOR_ROLE_AGENT_REASONING_MODEL=<model-id>
GAME_NARRATOR_ROLE_FAST_REASONING_MODEL=<model-id>
GAME_NARRATOR_ROLE_VISION_OCR_FALLBACK_MODEL=<model-id>
GAME_NARRATOR_ROLE_DECISION_MODEL=<model-id>
GAME_NARRATOR_ROLE_VOICE_DESIGN_MODEL=<model-id>
GAME_NARRATOR_ROLE_VOICE_CLONE_MODEL=<model-id>
GAME_NARRATOR_ROLE_SPEECH_ASR_MODEL=<model-id>

# optional:
GAME_NARRATOR_IMPORT_DIR=/home/arduino/disc-images      # manual 19 step 8
GAME_NARRATOR_LISTEN=127.0.0.1:8080                     # 0.0.0.0:8080 only per manual 19 step 7
GAME_NARRATOR_REPLAY_ROOTS=/var/lib/game-narrator/replay
GAME_NARRATOR_CAPTURE_DEVICE=<udev by-id/by-path>       # manual 18 "Choosing the capture mode"
```

Protocol notes:

- The **decision role speaks System One** — `systemone` is its default
  protocol (internal/providers roles table), so setting
  `GAME_NARRATOR_ROLE_DECISION_MODEL` is enough; the runtime runs the
  role through the router's breaker/timeouts. An unconfigured decision
  role is a documented degraded mode (local commits), not an error.
- `SPEECH_ASR` (`GAME_NARRATOR_ROLE_SPEECH_ASR_MODEL`) uses the
  OpenAI-compatible transcriptions protocol (ADR 0061): it is the ASR
  endpoint the QA loop reads played audio back through.
- Add a **comment line** recording the chosen audio sink (e.g.
  `# audio sink: bluez_output.<MAC>.1 (A2DP SBC) — best-effort, issue #3`
  or the USB DAC card name). There is no app env variable for the sink:
  it is the OS default sink (references/audio.md).

Rules:

- Keep `0600 root:root`; systemd (root) reads it into the unit's
  environment — the service process never opens the file itself.
- **Never print its contents** into logs, tickets, issues or
  screenshots. Scripts and the lifecycle runner verify it by mode,
  owner and line count only, for this reason.
- Secrets stay out of band (AGENTS.md rule 5): no real tokens in the
  repo, in this skill, or in any recorded run notes.

## Start and doctor

```sh
sudo systemctl start game-narrator
sudo sh -c 'set -a; . /etc/game-narrator/env; set +a; \
  game-narrator doctor --data-dir /var/lib/game-narrator'
```

(Same pattern the lifecycle runner uses: the env is sourced so the
gateway/role probes see exactly what the service sees. `--json` gives
the machine-readable form; `--dev` is dev-host-only checks.)

Expected on a fresh, hardware-less install: capture/audio rows
`NOT CONFIGURED`/`BLOCKED` with the attach instruction, storage `OK`,
configured model roles `OK` (or the specific probe failure), no `FAIL`
on the storage stack. Doctor exits 0 with a "what to do next" list on a
first-run host; 1 only when a **configured** component is actually
broken. Read the rows before blaming hardware.
