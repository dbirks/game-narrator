# 10 — UNO Q, GameCube hardware, development deployment and operational setup

## Physical connection

`GameCube analog out → existing RCA-to-HDMI converter → powered HDMI 1×2 splitter`. `out 1 → TV` and `out 2 → UVC HDMI capture dongle → USB-A/USB-C hub → UNO Q USB-C data`. UNO Q 5 V / 3 A compliant power distribution through suitable USB-C PD hub/supply; USB audio DAC/speakers and optionally Ethernet attach through hub. The UNO Q's own USB-C video output is not an HDMI input. For DOL-001 an optional Digital AV-to-HDMI path can improve picture quality, but do not require a new adapter for the user's working converter.

## Hardware discovery and preflight

- `lsusb`, `udevadm info`, `v4l2-ctl --list-devices`, format/frame-size enumeration, matching via USB VID/PID and stable path; never assume `/dev/video0`.
- Negotiate actual RCA converter's HDMI output as provided, e.g. 720p/1080p; card may not accept raw 480p or 480i. Record fps, progressive/interlaced status, pixel format and frame timestamps; check TV/splitter EDID and HDCP compatibility (GameCube is not normally HDCP, but capture negotiation can still fail).
- Capture one live screenshot; preview in browser, run test OCR on user-selected region; verify last frame freshness and dropped-frame rate.
- List ALSA/PipeWire audio devices; play deterministic test tone; measure output sample rate and physical latency if possible; reselect by stable ID on reboot.
- Check UNO Q load, temperature/RAM, available persistent storage, Linux/kernel version, hub power warnings, process permissions, remote gateway connectivity and data dir writability.
- Require `v4l-utils`, FFmpeg, Tesseract+language packs, audio backend, device-group access; check actual Debian packages available on UNO Q image. Prefer video4linux direct library only after compatibility proven.

## UNO Q software deployment

Arduino UNO Q has a Debian Linux MPU and a separate real-time microcontroller; first release **only** deploys to Debian MPU, leaving MCU unused. Go binary uses Linux arm64; migrate app from Arch developer host without building native OCR into Go. Service managed by `systemd`, unit `game-narrator.service` with dedicated user, restart policy, Linux device ACL/group membership and sandbox options chosen after validating `/dev/video*` and sound permissions. Configure env file protected at 0600, base config in `/etc/game-narrator/config.yaml`, state `/var/lib/game-narrator`, local log/journal, optional remote asset backend.

Do not automatically execute arbitrary shell scripts received via web UI. Bootstrap installs OS dependencies only via explicitly documented admin steps. Graceful stop cancels provider calls and speech, flushes SQLite and staging asset files, closes USB camera and DAC.

## Arch development host

`pacman` package capability discovery, not assumptions; Go, Node/pnpm, FFmpeg, v4l-utils, Tesseract, Dolphin emulator (user-provided installation), Mesa/Vulkan drivers, Playwright/browser dependencies, a display/Xwayland/Xvfb/virtual GPU setup, `libasound`/PipeWire for audio capture, virtual input tools where authorized, GoReleaser, Docker/Podman/buildx for OCI release tests. Coding agent has passwordless sudo on dedicated machine but must log installs and must not damage unrelated user configuration. Run emulator under isolated Dolphin user directory and avoid modifying default saved games.

## Deployment modes

- `developer`: Go + Vite processes; Dolphin/synthetic frame input; local session and fixture storage; masked logs.
- `single-node Arch`: released Go binary + SQLite + tools, for full E2E without UNO Q.
- `production UNO Q`: ARM64 binary or Debian package + native dependencies, local UVC and USB DAC, remote model gateway, optional remote asset store.
- `container`: OCI image for development/servers; `/dev/video*` and `/dev/snd` mapped only when explicitly requested. On UNO Q, prefer native service for lowest dependency/device complexity. A container image does not automatically grant device access.
- `offline/cache-only`: usable cached voice clips and local OCR; agent research disabled and clear degraded status.

## Resource budgets and failure recovery

UNO Q 4 GB must remain stable when handling captured frame previews, OCR and HTTP app simultaneously; image buffers capped, previews throttle, TTS audio streamed/chunked, session event log buffered, SQLite compacted. Build load tests to determine safe fps, worker count and jpeg preview limits; detect thermal throttling and power brownouts. Storage quotas with eviction priorities (ephemeral frames before generated speech; never delete approved voice references). For hub disconnect, capture actor moves to reconnecting with backoff; UI shows disconnected input; audio ongoing from committed text is policy-controlled.

## Security defaults for LAN deployment

Bind localhost on initial boot and expose LAN via explicit configuration; strongly recommend HTTPS/Tailscale/reverse proxy with authentication. Prevent unauthenticated capture preview or TTS abuse. Restrict test harness endpoints and emulator controls to Arch `dev` mode; never ship them active on UNO Q without auth and flags.
