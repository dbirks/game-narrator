# ADR-0084: Agent Skill for bringing up a stock UNO Q appliance (agentskills.io format)

Date: 2026-10-19
Status: accepted
Owners: agent worker (task uno-skill, issue #4)
Related requirements/tests: issue #4 (this ADR's motivation), issue #2/#3 comments (Bluetooth
findings codified), docs/manuals/18-hdmi-capture-on-uno-q.md (first-boot checks, dwc3 quirk,
measurements), docs/manuals/19-install-on-uno-q.md (install runbook, env file, lifecycle),
ADR-0081 (first-install boot enablement), `scripts/hw/capture-preflight.sh`,
`scripts/hw/run-on-device.sh`, `scripts/release/lifecycle-on-device.sh`,
`skills/uno-q-setup/SKILL.md`

## Context and constraints

The full manual bring-up of a stock Arduino UNO Q (Debian 13 arm64) to a
working Game Narrator appliance was done by hand on 2026-10-19 (issue #4).
A fresh coding agent given SSH access to a stock board has to rediscover
that path — including the non-obvious lessons: the `static` unit that is
dead after every reboot (ADR-0081), the dwc3 host-mode quirk, the
WirePlumber headless seat-monitoring gotcha, the Bluetooth LE-only
re-pair, and the env-file secrecy discipline (AGENTS.md rule 5).

The issue requests that path be a checked-in **Agent Skill** in the
agentskills.io format (agentskills.io/specification): a
`skills/uno-q-setup/SKILL.md` with `name`/`description` YAML
frontmatter, an instruction body under ~500 lines, and optional
`scripts/` and `references/` directories that agents load on demand
(progressive disclosure: metadata at startup, SKILL.md on activation,
files as needed). Constraints: public repo (rule 5: no IPs, hostnames,
tokens, game text or frames in the tree — placeholders
`<user>@<uno-q-host>` only); scripts idempotent and read-only by default
with an explicit `--apply`; the skill must reuse manuals 18/19 and the
existing repo hardware scripts rather than duplicate them; the
skill's read-only checks must be safely runnable against the live
appliance.

## Decision

1. **Location and layout.** `skills/uno-q-setup/` at the repo root:
   - `SKILL.md` — frontmatter (`name: uno-q-setup` matching the directory
     name, a keyword-rich `description`, `compatibility`, `metadata`),
     then a short decision flow (preflight → install → env → capture →
     audio → admin → lifecycle → verification) with a table mapping each
     preflight finding to its next action, and the standing rules
     (never touch sshd/authorized_keys/sudoers/network; never print the
     env file; report capture/audio as NOT VERIFIED until the hardware
     gate passes — AGENTS.md rules 3 and 12).
   - `references/` — five focused files (`install.md`, `capture.md`,
     `audio.md`, `admin.md`, `lifecycle.md`). They condense manuals 18/19
     and the issue #2/#3 findings and link back to the manuals rather
     than duplicating them (one level of file reference from SKILL.md,
     per the spec).
   - `scripts/` — five POSIX `sh` scripts, shellcheck-clean,
     read-only-by-default with `--apply` for the idempotent change:
     `preflight.sh` (whole-appliance rows + the repo's own
     `scripts/hw/capture-preflight.sh` for the HDMI chain), `install.sh`
     (deb verify/copy/apt-install/migrate-as-service-user/enable-at-boot
     per ADR-0081), `env-setup.sh` (env file report; scaffold from the
     packaged `env.example` at 0600 root **only if missing** — values are
     never written or printed by the script), `bluetooth-audio.sh`
     (state report; `--apply` = `loginctl enable-linger` + the user-level
     WirePlumber headless override `monitor.bluez.seat-monitoring =
     disabled` + wireplumber restart; pairing stays a documented manual
     `bluetoothctl` step with the classic-transport re-pair recipe), and
     `upgrade.sh` (in-place upgrade; automatic downgrades refused — the
     documented rollback path with data backup first is printed instead).
2. **Reuse, not duplication.** The skill scripts call the repo's own
   `scripts/hw/capture-preflight.sh` for the USB/capture/audio/doctor
   rows (resolved relative to the repo root, with a WARN row if the repo
   layout changed), and point at `scripts/hw/run-on-device.sh` and
   `scripts/release/lifecycle-on-device.sh` for the evidence-grade gates.
   The still-grab uses the manual-18-documented `ffmpeg -f v4l2` command;
   the one-shot `game-narrator ocr` smoke is referenced as **optional
   until the command exists** (issue #1).
3. **Honesty rules are in the skill itself.** The SKILL.md body states
   that a mocked or partial run is never presented as end-to-end and that
   a NOT RUN hardware gate is reported as NOT RUN (AGENTS.md rules 3 and
   12), and that frames/tokens stay out of the tree (rule 5).

## Alternatives considered

- **Duplicate manuals 18/19 into the skill.** Rejected: drift risk
  (two sources of truth) and it violates the issue's "reuse manuals
  18/19 rather than duplicating them".
- **A single fat `setup.sh --apply` doing everything.** Rejected:
  different steps have different blast radii (install vs. env vs.
  Bluetooth linger) and different preconditions; the skill's value is
  the decision flow, and coarse automation would hide failures the
  agent must interpret row by row.
- **Scripts that write secrets into `/etc/game-narrator/env`.**
  Rejected: rule 5 and the env-file discipline (manual 19 step 5) — the
  script scaffolds the 0600 skeleton; the agent/operator fills values
  out of band and the scripts only ever report names/mode/owner/line
  count.
- **Bash scripts with shared helpers.** Rejected: the board is dash;
  POSIX `sh` keeps every script self-contained (agentskills.io spec:
  scripts should be self-contained or clearly document dependencies) and
  matches the existing `scripts/hw/*.sh` style.

## Consequences and risks

- The skill is documentation + thin wrappers: it cannot go stale faster
  than manuals 18/19 and the hw scripts it links (they are the source
  of truth); a rename of those scripts shows up as a single WARN row in
  `preflight.sh` rather than silent breakage.
- `--apply` paths are only as safe as the board preconditions (key SSH,
  passwordless `sudo`); the scripts re-verify state before each change
  and refuse automatic downgrades. Residual risk: a future manual-19
  divergence is detected at run time by the agent, not at build time —
  mitigated by the links and by the read-only modes being the default
  (running them against the appliance is part of validation).
- The `bluetooth-audio.sh --apply` writes a user-level config file and
  restarts a user service: reversible (delete file + restart), scoped to
  the named audio user, and it does not touch the service user or the
  system-wide session.

## Evidence and test plan

- `shellcheck -s sh skills/uno-q-setup/scripts/*.sh` clean; `sh -n` on
  every script (dash-compatible).
- Read-only script modes executed against the real appliance
  (`ssh <user>@<uno-q-host>`) during the task run (results recorded in
  the task report, not the repo): `preflight.sh` (whole-appliance rows
  incl. the HDMI chain; it correctly surfaced the pre-0081 `static`
  unit and the missing env file on that board), `env-setup.sh`,
  `bluetooth-audio.sh` (headset paired, linger enabled, headless
  override present on that board), `install.sh` read-only state
  report, and `upgrade.sh --apply` correctly refusing a local artifact
  older than the installed build before touching the board.
- Still-grab smoke on the appliance (manual-18 ffmpeg command into the
  board's `/tmp`, deleted afterwards; no frames left on the device or in
  the tree): valid 1280x720 PNG from `/dev/video0` MJPEG. The OCR half
  of the smoke awaits the appliance's next build — the `game-narrator
  ocr` one-shot command (issue #1) landed in the same tree; the board's
  installed snapshot predates it (verified via `game-narrator --help`).
- Mock-board exercise (fake `ssh`/`scp`/`sudo`/`apt-get`/`dpkg-query`/
  `systemctl`/`game-narrator` on the dev host) exercised
  `install.sh --apply` end-to-end (apt install → `game-narrator
  version` → `migrate` as service user → unit enablement `static`→
  `enabled`) and `upgrade.sh` downgrade refusal (exit 1, data-backup
  instruction printed, nothing applied).
- `SKILL.md` validated against agentskills.io/specification: frontmatter
  fields (`name` matches the directory, lowercase/hyphens; `description`
  ≤ 1024 chars; optional `compatibility`/`metadata`), body well under
  500 lines, references one level deep.

## Migration / rollback

No schema, protocol or runtime change — the skill adds a directory and
a README pointer only. Rollback: delete `skills/uno-q-setup/` and the
README paragraph; nothing else references the tree.

## Follow-ups

- Run the OCR smoke row of `SKILL.md` step 4 on the appliance after the
  next upgrade ships the `game-narrator ocr` command (issue #1, in-tree
  at this ADR's date; the appliance's installed snapshot predates it),
  and fold the observed timings into `references/capture.md`.
- If the UI gains the Bluetooth flow (issue #2/#3), `references/audio.md`
  becomes the SSH-side counterpart and the skill should note the UI path
  as the operator-preferred route.
