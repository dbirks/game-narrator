# Development environment — verified host inventory

Milestone 0 inventory of the actual Arch development host. Every value below was
produced by the command in the same row at the time of writing; anything not
observed is listed as `NOT CHECKED`. No URLs, hostnames, or credentials are
recorded here — model roles are configured through environment variables only
(see `examples/config/env.example`).

## Gates

`make test-full` runs the gate targets in order — `test-unit`, `test-ui`,
`diagnostics`, `release-snapshot`, `bench`, `test-replay`, `test-dolphin`,
`test-models`, `test-hardware` — continues past failures, and prints a final
table (`gate | result | detail`). It exits 0 only if every gate passed, 1 if
any gate FAILed, and 2 if nothing failed but at least one gate is NOT RUN.
Per-gate logs and the JUnit XML summaries land in `artifacts/gates/`
(gitignored).

**NOT RUN** is the gate convention for "this capability is not configured on
this host": the test (or the gate) skips itself and prints
`NOT RUN: <setup instruction>`. NOT RUN is neither a pass nor a fail — it is
a reported, missing capability (ADR-0009/0013; ADR-0037 "blocked ≠ pass").
A gate whose capabilities are not configured is never green, and
`make test-full` exits 2, not 0, while one remains.

| Gate | What it runs | Needs (env var names only) |
| --- | --- | --- |
| `test-unit` | `go vet` plus all unit tests with `-race` | — |
| `test-ui` | frontend typecheck/lint/format/tests, embedded build, Playwright e2e | — |
| `diagnostics` | `game-narrator doctor --dev` on a fresh build | — |
| `release-snapshot` | GoReleaser snapshot pipeline (no publishing) | `goreleaser` on PATH; docker optional |
| `bench` | benchmark holdout split through the real pipeline | `tesseract` and `magick` on PATH |
| `test-replay` | synthetic scenario suite (fault injection; BLOCKED reported, never passed) plus pipeline replay tests, `-race` | `tesseract` on PATH, else the replay test reports NOT RUN |
| `test-dolphin` | real Dolphin tests (`-run Real`), one emulator at a time (`-p 1`, 20m timeout) | `GAME_NARRATOR_GAME_IMAGE_PATH`; `dolphin-emu`, `Xvfb`, `ffmpeg`, `x11-utils` on PATH |
| `test-models` | live provider contract tests (`-run Live`), sequential (`-p 1`) | `GAME_NARRATOR_GATEWAY_URL` plus the token env (default `GAME_NARRATOR_GATEWAY_TOKEN`, name overridable via `GAME_NARRATOR_GATEWAY_TOKEN_ENV`); vision tests additionally `GAME_NARRATOR_LIVE_VISION=1`; TTS registry tests `GAME_NARRATOR_TTS_BASE_DIRECT_URL` (or `GAME_NARRATOR_ROLE_VOICE_CLONE_REGISTRY_URL`) |
| `test-hardware` | UNO Q hardware smoke — no hardware tests exist yet, so the gate itself reports NOT RUN | attach a UNO Q; see `docs/status/blocked.md` |

The go-test gates (`test-replay`, `test-dolphin`, `test-models`) run through
`scripts/gates/run-go-gate.sh`, which parses `go test -json`, prints one
`PASS`/`FAIL`/`SKIP`/`NOT RUN` line per test (NOT RUN lines carry the setup
instruction), and writes `artifacts/gates/<gate>.xml`. Its exit code is 0 =
every test ran and passed, 1 = any FAIL (or `go test` itself failed), 2 =
nothing failed but at least one test is NOT RUN; the gate records that code
in `artifacts/gates/<gate>.rc` because `make` maps any recipe failure to its
own exit status 2.

## Host

- OS: Arch Linux (`/etc/os-release`: `NAME="Arch Linux"`)
- Kernel / machine: `Linux 7.2.9-arch1-1 x86_64` (`uname -srm`)
- Shell: `/bin/bash` (`echo $SHELL`)
- Sudo: passwordless sudo available on this host (verified with `sudo -n true`)
- Session: `XDG_SESSION_TYPE=tty`; `DISPLAY` and `WAYLAND_DISPLAY` both unset

| Tool | Version | Command used |
|---|---|---|
| kernel / arch | `Linux 7.2.9-arch1-1 x86_64` | `uname -srm` |
| Go | `go1.27.2-X:nodwarf5 linux/amd64` | `go version` |
| Node.js | `v26.11.1` | `node --version` |
| pnpm | `12.14.0` | `pnpm --version` |
| FFmpeg | `n9.0.2` | `ffmpeg -version \| head -1` |
| v4l-utils | `v4l2-ctl 1.32.0` | `v4l2-ctl --version` |
| Tesseract | `5.5.3` | `tesseract --version \| head -1` |
| Tesseract languages | `eng`, `osd` (2 total) | `tesseract --list-langs` |
| SQLite CLI | `3.54.0` (64-bit) | `sqlite3 --version` |
| GoReleaser | `2.14.0` (`GitVersion`) | `goreleaser --version \| head -3` prints only the ASCII banner; version read from the full `goreleaser --version` output (`GitVersion:` line) |
| Docker | `29.9.1` | `docker --version` |
| buildx | `0.38.0` | `docker buildx version` |
| Dolphin (package) | `dolphin-emu 1:2609-1` | `pacman -Q dolphin-emu` |
| Dolphin (runtime) | `Dolphin 2609` | `dolphin-emu --version` **aborted (core dumped), exit 134** with no display; `xvfb-run -a dolphin-emu --version` printed `Dolphin 2609`. `dolphin-emu-nogui` is not on PATH (command not found) |
| Xvfb | present (usage line only) | `Xvfb -help 2>&1 \| head -1` → `use: X [:<display>] [option]` |
| Mesa | `mesa 1:26.2.4-1` | `pacman -Q mesa vulkan-intel pipewire wireplumber alsa-utils` |
| Vulkan Intel driver | `vulkan-intel 1:26.2.4-1` | same `pacman -Q` command |
| PipeWire | `pipewire 1:1.6.9-1` | same `pacman -Q` command |
| WirePlumber | `wireplumber 0.5.18-1` | same `pacman -Q` command |
| ALSA utils | `alsa-utils 1.2.16-1` | same `pacman -Q` command |
| GPU | `00:02.0 VGA compatible controller: Intel Corporation Kaby Lake-S GT2 [HD Graphics 630] (rev 04)` | `lspci \| grep -iE 'vga\|3d'` |
| Session type | `tty` | `echo $XDG_SESSION_TYPE` |
| Other bootstrap packages | all present: `make 4.4.1-3`, `gcc 16.2.1+r23+gd564253eb6c8-1`, `syft 1.54.1-1`, `cosign 3.1.3-1`, `docker-buildx 0.38.0-1`, `xorg-server-xvfb 21.1.25-2`, `xorg-xwininfo 1.1.7-1`, `xdotool 4.20260303.1-1`, `mesa-utils 9.0.0-7`, `vulkan-tools 1.4.363.0-1`, `jq 1.8.2-1`, `go-yq 4.54.1-1`, `github-cli 2.102.0-1`, `imagemagick 7.1.2.34-1`, `kitty-terminfo 0.49.2-1`, `tesseract-data-eng 2:4.1.0-5` | `pacman -Q <pkg>` |
| Docker service state | `docker.service` active + enabled; `docker:x:966:david` — the user is now a `docker` group member, but the live session still reports `david wheel`, so existing shells must be re-launched for the group to take effect | `systemctl is-active/-enabled docker.service`, `getent group docker`, `groups` |
| Fonts | 679 fonts installed; `fc-match sans-serif` → `NotoSans-Regular.ttf: "Noto Sans" "Regular"` | `fc-list \| wc -l`, `fc-match sans-serif` |
| Chromium system libraries | 5 of 5 sonames resolvable via the system loader (`libatk-1.0.so.0`, `libatk-bridge-2.0.so.0`, `libatspi.so.0`, `libXcomposite.so.1`, `libXdamage.so.1`) — packages `at-spi2-core 2.62.1-1`, `libxcomposite 0.4.7-1`, `libxdamage 1.1.7-1`, `nss 3.131-1`, `libxkbcommon 1.13.2-1`, `libxrandr 1.5.5-1`, `alsa-lib 1.2.16.1-1` | `ldconfig -p \| grep -cE 'libatk-1.0.so.0\|libatk-bridge-2.0.so.0\|libatspi.so.0\|libXcomposite.so.1\|libXdamage.so.1'`, `pacman -Q …` |
| PipeWire user service | `pipewire` and `wireplumber` user units both `active` (exit 0); the daemon answers over the PulseAudio protocol: `pactl list short sinks` → `44\tauto_null\tPipeWire\tfloat32le 2ch 48000Hz\tSUSPENDED`, `pactl list short sources` → `44\tauto_null.monitor\tPipeWire\tfloat32le 2ch 48000Hz\tSUSPENDED`. `wpctl status` shows an **empty Audio Devices list**, one sink (`44. Dummy Output`) and no sources — i.e. PipeWire is running but owns no hardware card. RTKit warnings persist (`org.freedesktop.DBus.Error.ServiceUnknown` → realtime priority not granted); warnings only, the daemon still serves clients | `systemctl --user is-active pipewire wireplumber`, `pactl list short sinks`, `pactl list short sources`, `wpctl status` |
| Sound-card device nodes | `crw-rw---- 1 root audio 116, 10 …  /dev/snd/controlC0` — ACL `user::rw- group::rw- other::---`, and this session is `david wheel` (not `audio`), so the TTY user cannot open the card; that is why PipeWire lists no device | `ls -l /dev/snd/controlC0`, `getfacl /dev/snd/controlC0 2>/dev/null \| head`, `groups` |
| UVC capture devices | none — `/dev/video*` does not exist | `ls /dev/video*` |

## Verified gaps

1. **No graphical session (TTY only).** `XDG_SESSION_TYPE=tty`, `DISPLAY` and
   `WAYLAND_DISPLAY` unset. Consequence: Dolphin rendering must be proven under
   Xvfb (or another virtual display / Xwayland setup) — never assumed. Evidence
   captured above: bare `dolphin-emu --version` aborts with a core dump, while
   `xvfb-run -a dolphin-emu --version` prints `Dolphin 2609`. Actual rendered
   pixel frames and accepted controller input are still **NOT VERIFIED**.
2. **Game image comes from outside the repo.** A user-provided GameCube image
   (CISO) is available on this host and is passed to the application through
   `GAME_NARRATOR_GAME_IMAGE_PATH`; its path is never recorded in Git, in this
   document, or in any evidence bundle — only its hash is (AGENTS.md rule 5).
   `.gitignore` blocks `*.iso`, `*.gcm`, `*.rvz`, `*.ciso`, `*.7z`, `*.gcz`,
   `*.wbfs`, `*.wia` and `*.nkit.iso`. The Wind Waker adapter and real fixture
   authoring can therefore run against a lawful image; the `make test-dolphin`
   gate itself is still unimplemented, so end-to-end GameCube playback:
   **NOT VERIFIED**.
3. **Model endpoints not deployed — BLOCKED.** Clef-Omni decision model, Qwen3-TTS
   VoiceDesign and Qwen3-ASR endpoints are not present on this host and were not
   probed. `make test-models` and live decision/TTS/ASR gates stay blocked until
   the gateway and its endpoints exist.
4. **UNO Q hardware not connected — BLOCKED.** No UNO Q, no UVC capture device
   (`/dev/video*` absent), no USB DAC identified. `make test-hardware` is `NOT RUN`.
5. **LLM and vision roles** will use an OpenAI-compatible gateway selected through
   environment variables per logical role (no URL, hostname, or key stored in the
   repo or in this document). Gateway reachability: **NOT CHECKED** (nothing
   configured yet).
6. **Audio playback path unverified beyond a null sink.** PipeWire and
   WirePlumber user services are now `active` (`systemctl --user is-active
   pipewire wireplumber` → `active`, `active`) and `pactl list short sinks`
   returns exactly one sink: `auto_null` ("Dummy Output", float32le 2ch
   48000Hz), whose monitor `auto_null.monitor` is likewise the only source.
   `wpctl status` lists **no audio devices at all**: this session has no seat
   access to the hardware card — `/dev/snd/controlC0` is `crw-rw---- root:audio`
   with ACL `user::rw- group::rw- other::---`, and `groups` reports `david
   wheel` (no `audio` membership, no active logind seat granting it), so
   PipeWire starts with no card and falls back to the null sink. Consequence
   for tests: playback assertions must target the PipeWire null sink and read
   back its **monitor** source (a deterministic, device-independent sink); they
   prove bytes reached the audio server, not that anything was audible.
   Actual TTS playback through a real DAC or speaker: **NOT VERIFIED**. Adding
   the user to the `audio` group / starting a graphical seat, and then a speaker
   smoke test, is hardware-gate work (`make test-hardware`).
7. **Browser / frontend tooling comes from pnpm, not pacman.** TypeScript, Vite,
   React, oxlint, oxfmt, and Playwright are installed by `pnpm install` in `web/`;
   the Chromium build comes from `pnpm exec playwright install chromium` and its
   system libraries and fonts from `scripts/bootstrap-arch.sh` (see the Fonts and
   Chromium system libraries rows above). `pnpm --dir web test:e2e` launches
   Chromium and passes. Remaining browser work — visual/accessibility depth and
   capture-heavy flows: **NOT CHECKED**.
8. **No application code yet.** Go module, Vite skeleton, SQLite migrations,
   `.env.example`, `make` targets, and ADRs for router/SQLite driver/UI kit/FFmpeg
   and OCI strategy remain Milestone 0 work outside this document: **NOT CHECKED**.
9. **Docker group membership.** `docker.service` is already active and enabled and
    the user has been added to the `docker` group (`docker:x:966:david`), so root
    is no longer required for `docker`. Group membership is only applied at login:
    shells started before the change still lack it (`groups` → `david wheel`) and
    need a re-login / new session. `docker buildx` OCI build test: **NOT CHECKED**.
10. **Host originally had zero fonts.** Before this milestone the Arch host had no
    font packages at all (`fc-list` → 0 fonts), so every UI label rendered as
    empty boxes in browser and Dolphin screenshots — Playwright assertions on
    visible text could never have been trusted. Fixed by installing `ttf-dejavu`,
    `noto-fonts` and `ttf-liberation` (now 679 fonts, `fc-match sans-serif` →
    `Noto Sans`); these are part of the "browser runtime libraries and fonts" group
    in `scripts/bootstrap-arch.sh`.

## Reproduce this inventory

```
uname -srm; go version; node --version; pnpm --version
ffmpeg -version | head -1; v4l2-ctl --version
tesseract --version | head -1; tesseract --list-langs; sqlite3 --version
goreleaser --version; docker --version; docker buildx version
dolphin-emu --version; dolphin-emu-nogui --version; pacman -Q dolphin-emu
xvfb-run -a dolphin-emu --version
Xvfb -help 2>&1 | head -1
pacman -Q mesa vulkan-intel pipewire wireplumber alsa-utils
lspci | grep -iE 'vga|3d'; echo "$XDG_SESSION_TYPE"
fc-list | wc -l; fc-match sans-serif
ldconfig -p | grep -cE 'libatk-1.0.so.0|libatk-bridge-2.0.so.0|libatspi.so.0|libXcomposite.so.1|libXdamage.so.1'
getent group docker; groups
systemctl --user is-active pipewire wireplumber
pactl list short sinks; pactl list short sources
wpctl status 2>&1 | sed -n '/^Audio/,/Streams:/p'   # RTKit warnings aside
ls -l /dev/snd/controlC0; getfacl /dev/snd/controlC0 2>/dev/null | head
scripts/bootstrap-arch.sh --dry-run
```
