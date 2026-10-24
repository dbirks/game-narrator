# Install and run on an Arduino UNO Q

## Purpose

Bring a **fresh UNO Q appliance** (Debian 13 arm64) to a running, honestly
self-reporting Game Narrator: space freed on the small root partition,
durable data on the large partition, the arm64 `.deb` installed and
configured, `game-narrator doctor` clean, the UI reachable from another
machine, a first game imported, a voice registered and a first session run.
Every command here is a real command with a real flag (verified against the
installed binary's `--help` and the packaging scripts); nothing is
abbreviated into a made-up shortcut.

What this manual does **not** claim: the on-device install/upgrade/rollback
path of the `.deb` is proven on a real UNO Q (docs/release.md §6,
`scripts/release/lifecycle-on-device.sh`, 9/9 PASS), but live **capture and
audio playback remain NOT VERIFIED** until the UVC HDMI dongle and a USB DAC
are attached and `make test-hardware` passes on the appliance (AGENTS.md
rule 12, docs/spec/16 M8).

## Preconditions

- A UNO Q with power, on a network you can reach. For the capture/audio
  path (not needed for install), wire the chain per docs/spec/10
  "Physical connection": GameCube → RCA-to-HDMI converter → powered HDMI
  splitter → UVC HDMI capture dongle on the powered hub → UNO Q, plus a USB
  audio DAC/speaker on the hub (5 V / 3 A power distribution per that spec).
- **SSH key access** from a dev host: `ssh <user>@<uno-q-host>` must work
  non-interactively (`ssh -o BatchMode=yes <user>@<uno-q-host> true`). The
  device lifecycle and hardware-smoke runners use `BatchMode` and never
  touch the device's sshd, `authorized_keys` or network configuration.
- **Passwordless `sudo`** for your user on the board
  (`sudo -n true` — the lifecycle pre-flight requires exactly this).
- A dev host with the repository checked out (for building the `.deb` and
  running the on-device scripts).
- Out of band (AGENTS.md rule 5): your inference gateway URL and token, and
  the model ids for the roles you will configure — manual
  [04-inference-gateway-connection.md](04-inference-gateway-connection.md).
  This manual shows **placeholders only**; never put real tokens in a file
  that is copied into the repo, a log or a screenshot.

## The two partitions

A 16 GB UNO Q ships with a small root partition and a large data partition
(measured on an appliance, `df -h`):

```text
/dev/mmcblk0p68   9.8G   /                 # root — fills up fast
/dev/mmcblk0p69  18G     /home/arduino     # large data partition
/dev/mmcblk0p67 488M     /boot/efi
```

`df -h` shows your board's actual numbers. Game Narrator's durable state
(SQLite catalogue + content-addressed assets) lives in
`/var/lib/game-narrator`, which is **on the root partition by default**.
Step 2 moves it to the large partition with a bind mount so the 10 GB root
never fills up because of game data.

## Step 1 — (optional) Debloat the root partition

Each sub-step is **optional**; do them in the order shown and re-check
`df -h /` afterwards. Only remove what you do not need — this manual does
not ship a package allowlist for the appliance, so discover first.

```sh
# See what is using the root partition (package size in KiB).
dpkg-query -Wf '${Installed-Size}\t${Package}\n' | sort -rn | head -20
```

- **Remove Arduino App Lab** (optional — only if you will not use the
  Arduino IDE / container tooling on the board):

  ```sh
  apt list --installed 2>/dev/null | grep -i -E 'arduino|lab'   # discover the names
  sudo apt-get remove --purge -y <app-lab-packages>
  ```

- **Remove Docker** (optional — Game Narrator does not use containers):

  ```sh
  sudo apt-get remove --purge -y docker.io docker-compose   # discover via apt list
  sudo systemctl disable --now docker docker.socket 2>/dev/null
  ```

- **Drop the desktop** (optional — the board runs headless; the UI is a web
  page). Switch the default target so the GUI stack does not start at boot:

  ```sh
  sudo systemctl set-default multi-user.target
  ```

- **Trim the journal** (optional — journald can grow large on a 4 GB box):

  ```sh
  sudo sed -ri 's/^#?SystemMaxUse=.*/SystemMaxUse=200M/' /etc/systemd/journald.conf
  sudo systemctl restart systemd-journald
  ```

Finish the round with `sudo apt-get autoremove --purge -y` and verify:

```sh
df -h /
```

A reboot is only needed if you changed the default target or removed
services you had running.

## Step 2 — (optional, recommended) Put durable data on the large partition

Do this **before installing the `.deb`** (the unit is not running yet, so
nothing writes into the directory). The mount is transparent to the service:
`postinst` applies owner/mode to `/var/lib/game-narrator` with
`install -d`, which passes through the bind mount to the backing directory.

```sh
sudo mkdir -p /home/arduino/gn-data
sudo mkdir -p /var/lib/game-narrator
sudo mount --bind /home/arduino/gn-data /var/lib/game-narrator
```

Make it survive reboots with a systemd mount unit:

```sh
sudo tee /etc/systemd/system/gn-data.mount >/dev/null <<'UNIT'
[Mount]
What=/home/arduino/gn-data
Where=/var/lib/game-narrator
Type=bind
[Install]
WantedBy=multi-user.target
UNIT
sudo systemctl daemon-reload
sudo systemctl enable gn-data.mount
```

And make sure the service never starts before the mount (an override is
enough; it is `config|noreplace` territory, not a package file):

```sh
sudo systemctl edit game-narrator
# add these lines under [Unit], then save:
#
# [Unit]
# Requires=gn-data.mount
# After=gn-data.mount
```

(`systemctl edit` fails harmlessly before the unit exists — you can run it
again after Step 4 if you skipped it now.) Verify with `df -h /var/lib/game-narrator`
(it must report the large partition, not `/`).

## Step 3 — Get the arm64 `.deb`

Two sources, in preference order:

1. **A published release** (a `v*` GitHub release, docs/release.md §2.2):
   download `game-narrator_<version>_linux_arm64.deb` and `checksums.txt`
   from the release assets on the dev host, then check the signature of the
   matching GHCR image and the checksums before consuming anything
   (docs/manuals/17, docs/release.md §2.4):

   ```sh
   cosign verify \
     ghcr.io/dbirks/game-narrator:<version> \
     --certificate-identity-regexp 'https://github.com/dbirks/game-narrator/.github/workflows/publish.yml@.*' \
     --certificate-oidc-issuer https://token.actions.githubusercontent.com
   sha256sum -c checksums.txt
   ```

   Signed images carry keyless Sigstore signatures + an SPDX SBOM
   attestation; the `.deb` itself is integrity-checked by
   `checksums.txt` (archives are not individually signed —
   docs/release.md §2.2).
2. **A local snapshot build** on the dev host (always available):

   ```sh
   make release-snapshot
   (cd dist && sha256sum -c checksums.txt)   # every line OK
   ```

   The artifact is `dist/game-narrator_<version>_linux_arm64.deb`. Snapshot
   versions are monotonic in the commit timestamp (ADR 0060), so a newer
   snapshot is an ordinary `apt` upgrade over an older one.

Transfer to the board:

```sh
scp dist/game-narrator_<version>_linux_arm64.deb dist/checksums.txt <user>@<uno-q-host>:~/
```

## Step 4 — Install

On the board:

```sh
cd ~
sha256sum -c checksums.txt        # every line OK
sudo apt install ./game-narrator_<version>_linux_arm64.deb
```

`apt` resolves the declared dependencies (`ffmpeg`, `tesseract-ocr`,
`tesseract-ocr-eng`, `v4l-utils`). `postinst` then creates the
`game-narrator` system user, the data directory
(`/var/lib/game-narrator`, mode 0700 — if you did Step 2 this lands on the
large partition through the bind mount) and `/etc/game-narrator` (0750)
with `env.example` (0640). The systemd unit ships in
`/lib/systemd/system/game-narrator.service`.

## Step 5 — Configure the environment file

The runtime is configured through `/etc/game-narrator/env` (the unit's
`EnvironmentFile`), mode 0600. It is **never shipped** in the package — you
create it from the shipped `env.example`:

```sh
sudo cp /etc/game-narrator/env.example /etc/game-narrator/env
sudo chmod 0600 /etc/game-narrator/env
```

Fill it in with your real values (placeholders shown; roles and semantics
in manual [04](04-inference-gateway-connection.md) — a role is enabled only
when its `_MODEL` variable is set):

```ini
GAME_NARRATOR_GATEWAY_URL=<gateway-base-url>
GAME_NARRATOR_GATEWAY_TOKEN=replace-with-real-secret-outside-git

# one _MODEL line per role you will use (see manual 04 for the full table)
GAME_NARRATOR_ROLE_AGENT_REASONING_MODEL=<model-id>
GAME_NARRATOR_ROLE_FAST_REASONING_MODEL=<model-id>
GAME_NARRATOR_ROLE_VISION_OCR_FALLBACK_MODEL=<model-id>
GAME_NARRATOR_ROLE_DECISION_MODEL=<model-id>
GAME_NARRATOR_ROLE_VOICE_DESIGN_MODEL=<model-id>
GAME_NARRATOR_ROLE_VOICE_CLONE_MODEL=<model-id>
GAME_NARRATOR_ROLE_SPEECH_ASR_MODEL=<model-id>

# optional, set per this manual's later steps:
GAME_NARRATOR_IMPORT_DIR=/home/arduino/disc-images      # Step 8
GAME_NARRATOR_LISTEN=127.0.0.1:8080                     # default; 0.0.0.0:8080 only per Step 7
GAME_NARRATOR_REPLAY_ROOTS=/var/lib/game-narrator/replay # Step 10
```

Keep the file 0600 root:root; systemd (root) reads it into the unit's
environment — the service process never opens the file itself. Never print
its contents into logs, tickets or screenshots (the lifecycle runner
verifies the env file by mode, owner and line count only, for this reason).

**User-level runs must carry `GAME_NARRATOR_DATA_DIR`.** The systemd unit
pins the data directory itself (`Environment=GAME_NARRATOR_DATA_DIR=/var/lib/game-narrator`),
so the unit never needs the variable in the env file. A run **outside** the
unit (env file plus `setsid nohup game-narrator serve`) resolves the data
directory from the environment it was started with, and without
`GAME_NARRATOR_DATA_DIR=<data-dir>` in that env file it silently falls back
to the platform default (`$XDG_DATA_HOME/game-narrator`, else
`~/.local/share/game-narrator`) — where a stale database from an older build
may sit, which is how an upgrade dies on a checksum mismatch naming a
database nobody meant to open (issue #31). `game-narrator starting` now
logs the resolved data directory, the database path and the setting that
won (flag, env or default), and `doctor` reports `DEGRADED` on the
`storage.data_dir` row when a non-empty default data directory exists next
to a different, explicitly configured one (two databases on one host).

## Step 6 — Start the service and run doctor

```sh
sudo systemctl enable --now game-narrator
systemctl status game-narrator
```

The service binds `127.0.0.1:8080` by default (docs/spec/10: bind localhost
initially, expose the LAN explicitly). Doctor is the honest self-report —
it exits 0 with a "what to do next" list on a first-run host and exits 1
only when a **configured** component is actually broken:

```sh
sudo sh -c 'set -a; . /etc/game-narrator/env; set +a; game-narrator doctor --data-dir /var/lib/game-narrator'
```

(The env file is sourced so the gateway/role probes see the same variables
the service sees; this is the same pattern the lifecycle runner uses.)
`--json` gives the machine-readable form; `--dev` adds developer-host
checks (Dolphin/Xvfb) that do not apply to the appliance.

Expected on a fresh install with no hardware attached: capture/audio rows
`NOT CONFIGURED`/`BLOCKED` with the attach instruction, data directory
`OK`, configured model roles `OK` (or the specific probe failure), no
`FAIL` on the storage stack.

**What does the device read?** Doctor reports component health; the
one-shot OCR command answers "what would the pipeline read right now" —
the session's own capture negotiation, crop, preprocessing, Tesseract and
matcher over one live frame (issue #1, ADR-0085). The UVC bus is
single-consumer, so it runs with the service stopped:

```sh
sudo systemctl stop game-narrator
sudo sh -c 'set -a; . /etc/game-narrator/env; set +a; game-narrator ocr --device /dev/videoN --data-dir /var/lib/game-narrator'
sudo systemctl start game-narrator
```

`--json` for the report, `--game <profile-id>` to rank reads against the
published profile, `--save-debug DIR` for the source/crop/preprocess
images, and `--image frame.png` re-runs the pipeline over a saved frame
(no device needed). Manual 18's "What does the device read right now?" is
the same check from the hardware side, with the node discovery.

## Step 7 — Open the UI from another machine

The API is loopback-bound by design, so the safe way in from your laptop is
an SSH tunnel (no configuration change, the loopback-only first-run
endpoint stays loopback-only):

```sh
ssh -N -L 8080:127.0.0.1:8080 <user>@<uno-q-host>
```

then open <http://127.0.0.1:8080> in your browser.

Alternatively, bind the LAN and accept the exposure: set
`GAME_NARRATOR_LISTEN=0.0.0.0:8080` in the env file,
`sudo systemctl daemon-reload && sudo systemctl restart game-narrator`,
and open `http://<uno-q-host>:8080`. docs/spec/10 "Security defaults for LAN
deployment" recommends a TLS reverse proxy / Tailscale rather than a bare
LAN bind (set `GAME_NARRATOR_TRUSTED_PROXY_CIDRS` for the proxy that
terminates TLS — see `game-narrator serve --help`).

**First-run admin setup.** Until an admin account exists,
`GET /api/v1/auth/status` reports `setup_required`. Create the first admin
from the board (the API's setup endpoint is loopback-only, and the CLI
runs directly against the database — no running server needed):

```sh
printf '%s' '<password>' | sudo -u game-narrator \
  game-narrator admin create --data-dir /var/lib/game-narrator --username admin --role admin
```

(As the `game-narrator` user so the SQLite file it touches is owned by the
service user.) Or, with the SSH tunnel from above, create it in the browser
at `/setup`. Afterwards log in at `/login`; the health/diagnostics page is
`/health` (the old `/diagnostics` URL redirects there).

## Step 8 — Import a game

The reference adapter is the user's own Wind Waker disc (game id `GZL`),
`.gcm`/`.ciso`. The disc image is read **in place and never copied** into
the data directory; only its SHA-256 and the extracted BMG's SHA-256 are
recorded (AGENTS.md rule 5, ADR-0066).

**In the UI (allowlisted directory).** Put the disc image in a directory
you control, name that directory in the env file
(`GAME_NARRATOR_IMPORT_DIR=/home/arduino/disc-images`, then
`sudo systemctl daemon-reload && sudo systemctl restart game-narrator`).
The API may read **only** regular files inside that directory, addressed by
basename — anything path-shaped is refused. The game's Overview page lists
the allowlisted images; choose one and the import runs as a background job
(`import.disc`, visible under the jobs API). Real disc images take minutes
(hash + parse); the job is resumable and idempotent by content hash.

**CLI alternative** (e.g. to import before the UI is up). The image must be
readable by the service user, and run **as** the service user so the asset
store stays its owner:

```sh
sudo -u game-narrator game-narrator import-disc \
  --data-dir /var/lib/game-narrator \
  --image /home/arduino/disc-images/<your-disc>.iso \
  --game-slug wind-waker
```

`--publish` (off by default) moves the draft profile through review and
publishes it; review the imported text in the UI first. Verify with
`GET /api/v1/health/ready` staying 200 during the job and the job ending
`succeeded`.

## Step 9 — Voices

With the TTS roles configured (manual 04) and a voice workflow available,
register voices from the UI: game page → **Voices**
(`/games/<id>/voices`) — audition (`POST /api/v1/voices/auditions`),
approve, then register (operator role; a consent string is mandatory).
Names are canonical (`gn_<…>_v<rev>_<digest8>`) and the registry is
persistent state on the direct Base service — maintenance rules and
re-registration semantics in manual
[05-voice-registry-maintenance.md](05-voice-registry-maintenance.md).
`doctor` probes the `voice_clone` role (registry list + a synthesis with
the first `gn_*` voice).

Audio **playback** needs the physical path: a USB DAC/speaker on the hub,
the PipeWire sink the service uses (`GAME_NARRATOR_AUDIO_DEVICE` to pin
one). Until the DAC is attached, sessions run with speech disabled and the
report says so — that is an honest state, not a failure.

### Audio over the hub's HDMI port

If an AV receiver is on the hub's HDMI port (USB-C DisplayPort → ANX7625
bridge chip → hub HDMI), a second, receiver-driven output exists: the
PipeWire sink `Built-in Audio HDMI Digital Stereo Output`
(`alsa_output.platform-sound.HDMI__HDMI__sink`, `hw:ArduinoImolaHPH,3`).
Out of the box it is **silent**: PipeWire negotiates a PCM config
(S24_LE, MMAP, 10 ms periods, tsched) the bridge does not honor, while
`aplay -D hw:1,3` on the same PCM is audible. The working ALSA route is
the **default** one — `SEC_MI2S_RX Audio Mixer MultiMedia4` on (numid
207), `DISPLAY_PORT_RX Audio Mixer MultiMedia4` off (numid 135);
**do not change the mixer routes** (the DisplayPort route is a dead end:
no DP backend in the device tree, prepare fails with `-22`).

Fix (user-level, survives reboot) — on the audio-owning user create
`~/.config/wireplumber/wireplumber.conf.d/82-hdmi-bridge.conf` pinning
the sink node to the aplay-equivalent config (issue #5 and
`skills/uno-q-setup/references/audio.md`, "Audio over the hub HDMI
(ANX7625 bridge)", have the full measurement):

```ini
monitor.alsa.rules = [
  {
    matches = [
      {
        node.name = "~alsa_output.platform-sound.HDMI.*"
      }
    ]
    actions = {
      update-props = {
        audio.format = "S16LE"
        audio.rate = "48000"
        audio.channels = "2"
        api.alsa.period-size = 6240
        api.alsa.period-num   = 4
        api.alsa.headroom = 0
        api.alsa.disable-mmap = true
        api.alsa.disable-tsched = true
        api.alsa.disable-batch = true
        session.suspend-timeout-seconds = 0
      }
    }
  }
]
```

then `systemctl --user restart wireplumber` (the HDMI sink stays the
default; the no-suspend setting keeps the receiver's ELD lock alive,
~520 ms buffer latency is the cost). Verify: while `pw-play tone.wav`
is running, `/proc/asound/card1/pcm3p/sub0/hw_params` must read
`RW_INTERLEAVED / S16_LE / 2ch / 48000 / period_size 6240 /
buffer_size 24960`, state RUNNING, `hw_ptr` advancing, and `dmesg`
shows `q6asm_dai_prepare: pcm size 99840 period 4 period_sz 6240 vs
24960 rate 48000 ch 2`. If all of that holds, the user hears the tone
on the receiver — ask them; it is the acceptance step.

### Audio over the hub HDMI from the service

The narrator service (`game-narrator`, the system user without a user
PipeWire session) plays the same hub HDMI **without** the WirePlumber
rule and without any user session: it writes the ALSA device node
in-process at the exact proven config (ADR-0114, issue #5). The sink is
`alsahw`, the device the stable identity `hw:ArduinoImolaHPH,3` — the
Settings page's audio section lists it as the "UNO Q hub HDMI
(ANX7625)" row, kind **hdmi**, and the selection is sink `alsahw` +
that device (or `GAME_NARRATOR_AUDIO_SINK=alsahw` and
`GAME_NARRATOR_AUDIO_DEVICE=hw:ArduinoImolaHPH,3` in
`/etc/game-narrator/env`). The sink keeps the stream open between
narrations — one silence period every ~200 ms — which is what holds the
receiver's ELD lock in place of the user session's
`session.suspend-timeout-seconds = 0` rule; the mixer routes stay exactly
as above (SEC_MI2S_RX on, DISPLAY_PORT_RX off).

Verify the service path the same way the skill verifies the user path:

1. `game-narrator doctor` — the `audio.hdmi_bridge` row must be OK. It
   is read-only: on a mis-routed board it is FAIL and names the exact
   `amixer -c ArduinoImolaHPH set …` command to run.
2. The test tone: Settings → Audio → the "UNO Q hub HDMI (ANX7625)"
   device with sink `alsahw` → play tone. The receiver should reproduce
   the 440 Hz tone; while it plays, the kernel's own record must read
   the proven config:

   ```sh
   cat /proc/asound/card1/pcm3p/sub0/hw_params
   # RW_INTERLEAVED / S16_LE / 2ch / 48000 / period_size 6240 /
   # buffer_size 24960, state RUNNING, hw_ptr advancing
   ```

3. The hardware gate (the machine-checked form of the same proof, under
   consent): with a receiver on the hub HDMI, from the **dev host**
   (the appliance has no repo checkout, so `make` runs there, not on the
   board — 13-uno-q-hardware-smoke.md), with
   `GAME_NARRATOR_HW_AUDIO_CONSENT=1` in `/etc/game-narrator/env` (the
   script loads the service env and sets `GAME_NARRATOR_HW_SMOKE=1`):

   ```sh
   scripts/hw/run-on-device.sh <user>@<uno-q-host>
   ```

   plays a 1 s 440 Hz tone into `hw:ArduinoImolaHPH,3` through the
   sink and asserts the DMA consumed it — `hw_ptr` advances and the
   negotiated `hw_params` match the proven config (the
   `TestHWALSAHubHDMI` row; NOT RUN without the board or the consent
   variable, and reported NOT RUN — not passed — until it has run).

A silent receiver while `hw_ptr` advances is the receiver side (volume,
input selection, the receiver's own ELD); a frozen `hw_ptr` is the
bridge side (the `audio.hdmi_bridge` doctor row and the mixer routes
above are the first read).

## Step 10 — First session

The Play page (`/play`) starts sessions against a published game revision;
one active session at a time. Frame sources:

- **Replay (works now, no hardware).** The installed appliance ships
  without `GAME_NARRATOR_REPLAY_ROOTS` (the production default admits no
  replay dir). Add it per the documented operator step — set it in
  `/etc/game-narrator/env` (e.g.
  `GAME_NARRATOR_REPLAY_ROOTS=/var/lib/game-narrator/replay`), place a clip
  under it, `sudo systemctl daemon-reload && sudo systemctl restart
  game-narrator` (manual 13 "Sustained play" documents the same step,
  including the restore). The Play page's source picker starts the replay
  session. **This is replay evidence, not capture evidence** (AGENTS.md
  rule 3).
- **Capture (once the dongle is attached).** Wire the chain per
  docs/spec/10 "Physical connection" (GameCube → RCA-to-HDMI converter →
  HDMI splitter → UVC capture dongle on the powered hub). Verify with the
  hardware smoke from the dev host,
  `scripts/hw/run-on-device.sh <user>@<uno-q-host>` (manual 13), then run
  a capture session per the capture runbook (manual 18, in flight). Until
  then capture and audio stay **NOT VERIFIED**.

## Step 11 — Upgrade, rollback, uninstall

**Upgrade (in place).** Copy the new `.deb` + `checksums.txt` to the board,
`sha256sum -c`, then

```sh
sudo apt install ./game-narrator_<new>_linux_arm64.deb
sudo systemctl start game-narrator      # prerm stopped the unit; start it again
sudo sh -c 'set -a; . /etc/game-narrator/env; set +a; game-narrator doctor --data-dir /var/lib/game-narrator'
```

`/etc/game-narrator/env` and `config.yaml` survive upgrades
(`config|noreplace` / admin-created); the data directory always survives.

**Full verified cycle (clean install / upgrade / rollback / re-upgrade /
purge, with a pre-flight data backup to `/home/arduino/gn-data.bak-<ts>`).**
From the dev host, with two arm64 `.deb`s built from clean committed trees
(different versions — `lifecycle-on-device.sh` refuses same-version
pairs):

```sh
scripts/release/lifecycle-on-device.sh <user>@<uno-q-host> \
  dist-prev/game-narrator_*_linux_arm64.deb \
  dist-cur/game-narrator_*_linux_arm64.deb
```

It requires key-based SSH + passwordless `sudo` on the board (the same
preconditions as this manual), writes only `/tmp/gn-lc`, the pre-flight
backup and package-managed paths, never prints the env file, and exits 0
only when every step PASSed and the device ends on the current package.
Docs: docs/release.md §6.

**Rollback.** Follow docs/release.md §4 in order: **backup the data
directory first** (stop the service; a stopped process leaves the WAL
consistent), reinstall the previous `.deb` — from a newer snapshot to an
older one is a downgrade: `apt-get install --allow-downgrades
./game-narrator_<old>_linux_arm64.deb` — and if the newer version applied a
migration the old binary cannot read, restore the DB backup **before**
starting it. Verify with `doctor` + `/api/v1/health/ready`. Manual 12 is
the operator-facing runbook.

**Uninstall / purge.**

```sh
sudo dpkg -r game-narrator       # remove: package files gone, /etc/game-narrator kept
sudo dpkg --purge game-narrator  # purge: package + config files gone
```

`/var/lib/game-narrator` is **never deleted by the package** (user data:
catalogue, assets, approved voice references). Back it up first, then
removing it is an explicit admin act; `userdel game-narrator` afterwards is
yours to decide (docs/release.md §5). If you used Step 2's bind mount,
disable and unmount it afterwards (`sudo systemctl disable --now
gn-data.mount`).

## CPU frequency tuning (issue #58)

The UNO Q (QRB2210, 4× Cortex-A53, max 2.016 GHz) captures around 7.5 fps
against the 10 fps target. Community characterization of the board shows the
cause is clock, not compute: a light periodic workload (our ~10 fps tick with
short OCR bursts) leaves the frequency policy modal at its ~614 MHz floor, while
free-running work holds 2.016 GHz — a ~6x step-time difference between the two
regimes, mostly clock. The board has thermal headroom (10-minute soak: 80.2 C
peak, no throttling), so raising the clock floor is safe.

### Read the current state (read-only)

The doctor's `system.cpu_governor` row reports governor, min/max/current
frequency per cpufreq policy and a thermal sample, and is DEGRADED (with the
remedy below) when a non-performance governor can park the clock at the
policy's floor:

```sh
ssh <board-user>@<uno-q-host> "sudo game-narrator doctor | grep -A3 cpu_governor"
```

### Tune, one variable at a time

`scripts/board/cpu-tune.sh` writes the tuning into a systemd oneshot unit
(`game-narrator-cpu.service`, applied at boot) and is **dry-run by default**:

```sh
# copy the script to the board
scp scripts/board/cpu-tune.sh <board-user>@<uno-q-host>:$HOME/

# 1) dry run (shows before-state, target, unit path; writes nothing)
ssh <board-user>@<uno-q-host> "bash $HOME/cpu-tune.sh"

# 2) apply: schedutil with the floor raised above 614 MHz
ssh <board-user>@<uno-q-host> "sudo bash $HOME/cpu-tune.sh --min-freq 1600000 --apply"
#   — or, more aggressive: pin the max clock outright
# ssh <board-user>@<uno-q-host> "sudo bash $HOME/cpu-tune.sh --performance --apply"
```

Verify the after-state it prints, then re-run the measurement protocol below
**before and after each single change** — change one thing (governor *or*
floor *or* OCR threads), measure, record, then move on.

Reverting the governor/floor tuning is the same script with `--apply` and the
previous values (e.g. `--governor powersave` with no `--min-freq` restores the
default policy behaviour); the oneshot unit stays installed and idempotent.

### Per-call OCR threads

The Tesseract child runs OpenMP when its build has it, and the service env on
the board pins `OMP_THREAD_LIMIT=1`, which bounds that parallelism. To test a
per-process thread budget for one tesseract child only, set
`GAME_NARRATOR_OCR_THREADS=<n>` in the environment file (Step 5) — the wrapper
sets `OMP_NUM_THREADS`/`OMP_THREAD_LIMIT` to `<n>` on the child, overriding the
inherited pin; unset means the current behaviour (inherit). The board's 4 A53
cores are a reasonable upper bound to test (n = 2 and n = 4); one variable at
a time, measured per the protocol. Revert by removing the line.

### Measurement protocol (run before/after each single change)

```sh
# 1) fps — sustained capture over 60 s, from the doctor/UI capture stats or a
#    session log; record the steady-state mean, not the first-second spike.
# 2) per-call OCR latency — from the session log / developer UI timeline:
#    min, median, p95 of the OCR call duration over the same 60 s.
# 3) temperature — sample the thermal zones every 5 s during the run and
#    record the peak:
# ssh <board-user>@<uno-q-host> "for i in \$(seq 12); do cat /sys/class/thermal/thermal_zone*/temp; sleep 5; done"
# 4) record: governor/min/cur as printed by cpu-tune.sh "after" state, plus
#    the three numbers above, into the issue — before/after, one row per change.
```

Pass criteria are recorded in issue #58 (the 10 fps capture/OCR target and
the per-call OCR latency baseline from the dev-host bench). Do not stack
changes: fps and OCR latency are measured after *each single* change so the
effect of the governor floor and of the OCR thread budget are separated.

## Starting at boot (user-level install)

A hand-started `setsid` narrator does not survive a reboot. On 2026-10-22 the board power-cycled, and nothing was narrating until it was started by hand. For the `~/gn-live` user-level install, install the user unit once:

```sh
loginctl enable-linger "$USER"            # start the user's systemd at boot, without a login
mkdir -p ~/.config/systemd/user
install -m 0644 packaging/systemd/user/game-narrator.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now game-narrator.service
```

The unit runs in the same user session as PipeWire, so the hub-HDMI sink stays reachable. It retries every 5 s while a tailnet listen address is still coming up at boot. `scripts/board/deploy.sh --user` stops and starts through it whenever it is enabled. A live capture session still has to be started after a reboot, from Play or with `POST /api/v1/sessions`.

**Cold boot without sound.** The `ArduinoImolaHPH` card is missing (#24) when the kernel logs `Failed to get clk 'audio'` about 40 s after boot. The recovery unit now waits up to 60 s for that line (ADR 0118 amendment) and restarts the ADSP. To recover by hand:

```sh
echo stop | sudo tee /sys/class/remoteproc/remoteproc1/state
echo start | sudo tee /sys/class/remoteproc/remoteproc1/state
systemctl --user restart pipewire wireplumber
```

## One install only (systemd unit vs user-level run)

A board can carry **two** game-narrator installs at once, and both layouts
really exist in the field:

| | the packaged unit | the hand-run user-level process |
|---|---|---|
| what starts it | `systemctl start game-narrator` | `setsid nohup /usr/bin/game-narrator serve >>serve.log 2>&1 </dev/null &` |
| runs as | the `game-narrator` service user | the ssh login user |
| env file | `/etc/game-narrator/env` | `~/gn-live/env` |
| data directory | `/var/lib/game-narrator` | `~/gn-live/data` (`GAME_NARRATOR_DATA_DIR`) |
| logs | `journalctl -u game-narrator` | `~/gn-live/serve.log` |

Both bind the same listen address by default. The 2026-10-21 incident: a
deploy installed a new binary and restarted the **unit** while the
**user-level process** still held port 8080 — the unit crash-looped (503
restarts) on `listen tcp <addr>:8080: bind: address already in use`,
burning CPU on a CPU-bound board, the new build never served, and the unit
was pointed at a different, empty data directory. The rule is therefore:
**one active install per board**, and check the other before starting or
deploying either:

```sh
systemctl status game-narrator   # the unit (active means it holds the port)
pgrep -x game-narrator           # the process(es); matches BOTH installs
```

`pgrep -x` is exact-name only, so it never matches an ssh shell. The two
"stop the other one" commands are `sudo systemctl disable --now
game-narrator` (the unit) and `kill $(pgrep -x game-narrator)` (the
user-level process). **Never** `pkill -f "game-narrator serve"`: the pattern
matches the ssh shell running the command and kills your own session.

`game-narrator doctor` now catches the collision before a crash loop starts:
run it with the target install's env, and the `network.listen` row probes the
configured address. Because you normally run doctor **while the narrator is
serving**, the row disambiguates the holder (ADR-0150, amendment) instead of
failing on the healthy install: if the holder answers as a game-narrator
(`GET /api/v1/system/info` on the loopback, 2 s budget) and there is no
second-install evidence (a second `pgrep -x game-narrator` process, or the
unit restarting while a non-unit process holds the port), the row is `OK`
("held by a running game-narrator <version>"). If something that is **not** a
game-narrator owns the address, the row is `FAIL` with "another program holds
<addr>: the holder does not answer as a game-narrator"; with a game-narrator
holder **plus** second-install evidence it is `FAIL` with "another process
holds <addr>; is a second game-narrator install running?" (exit code 1
in both FAIL cases).

## Deploying a dev build

`scripts/board/deploy.sh` deploys a locally built binary to the board in one
of the two layouts above (ADR-0150). It takes the target only from
`GN_BOARD_SSH` — it is never defaulted:

```sh
make web-build   # stage the UI into internal/webassets/dist (the binary embeds it)
GN_BOARD_SSH=<user>@<uno-q-host> ./scripts/board/deploy.sh --systemd   # the packaged unit
GN_BOARD_SSH=<user>@<uno-q-host> ./scripts/board/deploy.sh --user      # the ~/gn-live layout
GN_BOARD_SSH=<user>@<uno-q-host> ./scripts/board/deploy.sh --user --dry-run   # print the steps, touch nothing
```

The script, in order: cross-builds the arm64 binary stamped with
`0.1.0-<short commit>`

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath \
  -ldflags "-X github.com/dbirks/game-narrator/internal/bootstrap.version=0.1.0-$SHA \
            -X github.com/dbirks/game-narrator/internal/bootstrap.commit=$SHA" \
  -o <out> ./cmd/game-narrator
```

— `scp`s it to the board, **backs up the data directory first** (the
`--user` backup lands at `~/gn-live/data-backup-<ts>.tar.gz`, the `--systemd`
backup at `/var/lib/game-narrator-data-backup-<ts>.tar.gz`; only the newest
2 timestamped backups are kept (and, when free space is under twice the
newest backup's size, older ones are pruned before the new one is written) —
each is 2–3 GB once the speech cache is warm, and unpruned they filled the board's home partition to 99% on
2026-10-22; hand-named `data-backup-pre-*` archives are never pruned), keeps the
previous binary at `/usr/bin/game-narrator.prev`, stops the old process by
exact PID, installs with `sudo -n install`, starts, waits up to 30 s for
`GET /api/v1/health/live`, and prints `game-narrator version` (there is no
`--version` flag).

Gotchas the script encodes (each one cost an incident on a real board):

- **The data backup is tolerant of a live service.** `tar czf` exits 1 when a
  file changes while being read — and `dev-trace` blobs are rewritten while
  the service runs, so a `set -e` script aborts at the backup. The recipe
  excludes `data/dev-trace` from the archive and treats tar exit 1 as a
  warning ("file changed as we read it", backup still usable) and exit >1 as
  a stop. Manual equivalent:
  `tar czf data-backup-<ts>.tar.gz --exclude=data/dev-trace data`.
- **Stop by exact PID, never by pattern.** `pkill -f "game-narrator serve"`
  matches the ssh shell running the deploy. The script kills the PIDs from
  `pgrep -x game-narrator` (SIGTERM, 15 s grace, SIGKILL fallback).
- **It refuses while the OTHER install is active.** Deploying `--user` while
  the unit is active (or `--systemd` while a user-level process runs) stops
  before touching anything and prints the one command that disables the other
  install ("One install only" above). That refusal is the incident fix.
- **Rollback:** `sudo -n install -m 755 /usr/bin/game-narrator.prev
  /usr/bin/game-narrator`, then restart the install; the data backup from the
  failed deploy is the restore path (manual 12 / docs/release.md §4 if the
  schema moved under you).

## Verification

- `systemctl status game-narrator` is active; `GET /api/v1/health/ready` is
  200 and `GET /api/v1/system/info` answers.
- `game-narrator doctor` (env sourced, `--data-dir /var/lib/game-narrator`)
  reports no `FAIL`; every `NOT CONFIGURED`/`BLOCKED` row names the missing
  piece (dongle, DAC, role model) and how to attach/configure it. On a
  board with a receiver on the hub HDMI the `audio.hdmi_bridge` row is
  part of that check (the mixer route the ANX7625 bridge needs; a
  misroute is a `FAIL` naming the exact `amixer` fix).
- The UI is reachable from another machine: the login page renders, the
  admin account logs in, `/health` (the old `/diagnostics` URL redirects
  there) shows build version/commit and live
  readiness.
- A disc import job ends `succeeded` and the profile's messages are visible
  in the UI; a replay session runs end-to-end with frames/OCR/commits
  visible in the session view.
- `df -h /var/lib/game-narrator` reports the large partition (when Step 2
  was done).

## Troubleshooting

### No sound card after a cold boot (issue #24, ADR 0118)

After the first power-on following a power cut, the board can come up
without its built-in sound card: `cat /proc/asound/cards` has no
`ArduinoImolaHPH` (the hub HDMI audio path via the ANX7625 bridge), the
ADSP booted ("adsp is now up" in `dmesg`), but its APR services never
registered, so the lpass pinctrl → soundwire → codecs → sound probe
chain stayed deferred. The `dmesg` signature is the lpass deferral line
`qcom-sm6115-lpass-lpi-pinctrl: Failed to get clk 'audio'` (it can also
appear transiently on healthy boots — the card's absence is the fault,
the signature is the cause).

- **Automatic fix:** the package ships the oneshot
  `game-narrator-audiodsp-recover.service` (enabled on first install),
  which runs once per boot: on a UNO Q with the card absent and the
  signature present it restarts the ADSP remoteproc once and waits up to
  20 s for the card. Healthy boots and other hardware are logged no-ops.
  Check the outcome: `journalctl -u
  game-narrator-audiodsp-recover.service`.
- **Manual fix (the proven one):** find the ADSP remoteproc — the entry
  whose `/sys/class/remoteproc/<n>/name` is `adsp` (on the board it is
  `remoteproc1`; never `remoteproc0`, the modem) — and restart it:

  ```sh
  echo stop  > /sys/class/remoteproc/remoteproc1/state
  echo start > /sys/class/remoteproc/remoteproc1/state
  ```

  The APR services register at once and card 1 `ArduinoImolaHPH`
  appears; verify with `cat /proc/asound/cards` and the `audio.dsp`
  doctor row (OK).
- **Diagnostics:** `sudo game-narrator doctor` — the `audio.dsp` row is
  FAIL with the cause and this remedy when the signature is present,
  BLOCKED when the card is absent without it (a different fault: check
  `dmesg` for the deferral chain, then reboot). The skill preflight's
  `audiodsp` row reports the same three-way state read-only.
- **Upstream:** likely a cold-boot race between the in-kernel PDR
  notification for the ADSP audio PD and APR; if it reproduces, report
  it to the Arduino image / kernel (issue #24, item 4).

## What NOT to do

- Do not report the install as hardware-verified: capture and audio are
  **NOT VERIFIED** until the dongle + DAC are attached and
  `make test-hardware` passes on the appliance (AGENTS.md rule 12).
- Do not write real tokens, the gateway's private hostname or the device's
  IP/hostname into the repo, this manual, logs or screenshots — the env
  file is 0600 on the board and the placeholders above are the only values
  that belong in documentation.
- Do not `dpkg --purge` to "roll back" — purge is the §5 semantics;
  rollback is §4 (backup first).
- Do not delete `/var/lib/game-narrator` while the service is running, or
  without a backup you have tested restoring (docs/manuals/03).
- Do not raise the RSS/temperature budgets or edit `internal/hwsmoke`
  thresholds to make a flaky dongle pass (manual 13).
- Do not address the disc import API with paths: basenames inside
  `GAME_NARRATOR_IMPORT_DIR` only (ADR-0066).
