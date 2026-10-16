# ADR-0012: Native media tools as supervised subprocesses; thin Debian OCI base
Date: 2026-10-16
Status: accepted (subprocess boundary) / proposed (OCI base — unverified image)
Owners: project
Related requirements/tests: FR-01, FR-06, FR-11, FR-13, FR-15; docs/spec/05 "Capture" ("Local UNO Q capture runs FFmpeg/V4L2 with supervised subprocess lifecycle"), docs/spec/05 "OCR" (Tesseract PSM 6/7, configurable language pack), docs/spec/10 "Hardware discovery and preflight" ("Require `v4l-utils`, FFmpeg, Tesseract+language packs … Prefer video4linux direct library only after compatibility proven") and "UNO Q software deployment" ("migrate app from Arch developer host without building native OCR into Go"; systemd unit is the primary deployment), docs/spec/15 "OCI image design" + "GoReleaser v2 plan" (`CGO_ENABLED=0` "if and only if Go packages are pure-Go **and native tools are subprocesses**"), docs/spec/16 Milestone 0 ("Write ADRs for … FFmpeg/OCI strategy") and Milestone 8 (GHCR multiarch images, security scan); ADR-0006 (GoReleaser), ADR-0008 (`CGO_ENABLED=0` + pure-Go SQLite)

## Context and constraints

- Three native tools do work the Go process must not do in-process: **FFmpeg**
  (V4L2 frame acquisition, frame scaling/deinterlace, container/codec probing),
  **`v4l2-ctl`** from v4l-utils (device enumeration, format/fps negotiation —
  docs/spec/10 forbids assuming `/dev/video0`), **Tesseract** (region OCR with
  PSM/language-pack choices), plus an audio sink (ALSA/PipeWire, docs/spec/07).
- ADR-0008 fixed the persistence layer as pure Go specifically so releases stay
  `CGO_ENABLED=0` and cross-compile from the Arch amd64 dev host to Debian
  arm64. Linking FFmpeg/Tesseract through cgo would void that: a cross-toolchain
  for libavcodec/leptonica on arm64, non-static binaries, and a release matrix
  that docs/spec/15 explicitly conditions on native tools being subprocesses.
- The target is a 4 GB, 4-core appliance where an OCR or capture crash must not
  take the process that owns the SQLite handle, the SSE stream to the browser and
  the playback token (docs/spec/07 "Only event reducer can approve playback
  token"; AGENTS.md rule 7 "safe audio output, graceful degraded modes").
- Both tool families are already an *explicit OS dependency* of the dev
  environment: `scripts/bootstrap-arch.sh:62` installs `ffmpeg v4l-utils
  tesseract tesseract-data-eng` (Arch names), while Debian names differ
  (`tesseract-ocr`, `tesseract-ocr-eng`) — so "which version is installed" is a
  per-host fact, not a build-time constant.
- No Go code invokes a subprocess yet (`grep -rn "exec.Command\|os/exec" --include=*.go`
  is empty), and there is no `Dockerfile`, `.dockerignore` or `.goreleaser.yaml`
  in the repository. Both halves of this ADR are therefore forward decisions
  with recorded probe evidence, not descriptions of shipped behaviour — which is
  why the OCI half stays `proposed`.

## Decision

### Part 1 — native tools are supervised subprocesses (accepted)

1. FFmpeg, `v4l2-ctl` and Tesseract are invoked as **child processes with
   argv-only interfaces**. No cgo binding, no `LD_LIBRARY_PATH` games, no
   distribution-specific build. Each tool family gets one wrapper in the package
   that owns it (`internal/capture` for V4L2/FFmpeg, `internal/vision` for OCR,
   per the package layout in docs/spec/01)
   exposing typed Go calls, never a shell string: `exec.CommandContext` with an
   explicit context deadline and cancellation propagated, stdout/stderr read on
   bounded pipes, no `sh -c` anywhere (AGENTS.md rule 6: no arbitrary shell
   execution).
2. **Supervision is the wrapper's contract**, per docs/spec/05: fixed max
   concurrent invocations, hard deadline, kill-on-context-cancel including the
   process group (an OCR child that ignores SIGTERM must not survive its scene
   observation), non-zero exit and stderr tail classified into the provider-style
   failure taxonomy of ADR-0009 so the UI can say "OCR unavailable" rather than
   hang, and restart/backoff for long-lived capture processes.
3. **Binaries are probed at runtime, never assumed.** `game-narrator doctor`
   (diagnostics, docs/spec/10 "Hardware discovery and preflight") resolves each
   tool on `PATH`, records name/version/build-config, checks Tesseract language
   packs, lists `/dev/video*` by stable identity, and reports the capability
   matrix; `serve` degrades instead of failing to boot when a tool is missing.
   Version strings become part of the evidence bundle manifest (AGENTS.md
   "manifest").
4. Zero-copy optimisations stay available: FFmpeg writes raw frames to a pipe or
   a bounded ring of files; the Go side never re-implements a decoder.
5. **`v4l2` direct-syscall capture is deferred, not rejected.** docs/spec/10 says
   "prefer video4linux direct library only after compatibility proven"; the
   proven path is the subprocess, so a Go V4L2 implementation needs its own ADR
   with measured dropped-frame/CPU numbers on UNO Q hardware.

### Part 2 — OCI base: thin Debian slim, non-root, GoReleaser-built (proposed)

Per docs/spec/15 "OCI image design", choose a **pinned Debian slim runtime
stage** with distro packages, *not* distroless:

- Multi-stage: `web-build` (pnpm + Vite, mirroring the `web-build` Make target)
  → Go build with `CGO_ENABLED=0` (ADR-0008) → runtime stage.
- Runtime installs **`ffmpeg`, `tesseract-ocr`, `tesseract-ocr-eng`,
  `v4l-utils`** (+ `ca-certificates`, and the ALSA/PipeWire client library the
  audio sink needs), all pinned to digest/epoch versions recorded in the
  Dockerfile; image digest recorded in the release manifest.
- Non-root user, `EXPOSE` the API port, `HEALTHCHECK CMD` against
  **`/api/v1/health/live`** (the liveness route that exists today and does no
  dependency probing; readiness stays `/api/v1/health/ready` for orchestrators
  and the systemd/CI smoke), `VOLUME /var/lib/game-narrator`, documented optional
  mounts for `/dev/video*` and the sound device with group mapping.
- Build must copy neither ROMs, `.env`, user art/voices nor dev fixture dumps
  (AGENTS.md rule 5), and gets OCI labels + a vulnerability scan (docs/spec/15).
- Image built and published by **GoReleaser `dockers_v2`** with Buildx for
  linux/arm64 + linux/amd64 (ADR-0006); `goreleaser check` validates the syntax
  against the pinned installed GoReleaser, no invented keys.
- **The container is the distributable, not the recommended UNO Q install.**
  docs/spec/10 makes the dedicated-user `systemd` unit and the `.deb` (nfpms,
  ADR-0006) the primary path: a container adds no isolation benefit for a
  single-user appliance that needs `/dev/video*` and `/dev/snd` by host device,
  and privileged-ish device access through a container is a worse security
  boundary than a sandboxed unit with Linux device ACL/group membership.

## Alternatives considered

- **cgo bindings (FFmpeg/libav via cgo, gobwas/tesseract-style OCR, Go V4L2
  libraries).** Pros: no process churn per OCR call, no `PATH` dependency, frames
  never leave the process, lower CPU on very high call rates. Rejected now
  because it breaks the `CGO_ENABLED=0` release rule that ADR-0008 and
  docs/spec/15 depend on, multiplies the cross-compile matrix, and turns a
  segfault inside libavcodec into the loss of the database handle, the event bus
  and audio playback. The subprocess cost is one process spawn per OCR call —
  measurable, and the measurable version goes in the benchmark below before any
  reconsideration.
- **Distroless or `scratch` runtime.** Rejected: distroless has no FFmpeg,
  Tesseract, `v4l2-ctl` or ALSA client, and adding them means either shipping
  hand-built binaries (unpinnable, unauditable, no security updates) or
  re-introducing a distro. docs/spec/15 pre-conditions distroless on
  "only if compatible with Tesseract/FFmpeg/audio dependencies", and it is not.
- **`ubuntu`/`debian` full as base.** Rejected on image size and attack surface
  for no capability gain; the packages we need exist in slim + apt.
- **Static musl build with bundled ffmpeg (`ffbinaries`-style vendored binaries).**
  Rejected: vendored media binaries are outside Debian's security update path,
  break the "check actual Debian packages available on UNO Q image" requirement
  in docs/spec/10, and would still not provide `v4l-utils`.
- **Two images (runtime + sidecar per tool).** Rejected for a single-appliance
  product where the systemd path is primary; sidecars complicate the one thing
  the deployment must be good at (device access on boot).
- **`systemd` unit inside the container.** Rejected — no init in a container; the
  image's entrypoint is `game-narrator serve` and the unit file lives in the
  package.

## Evidence and test plan

Dev-host tool presence (Arch, `uname -s` Linux, `x86_64`) — the versions the
subprocess wrappers must tolerate at runtime:

```
$ ffmpeg -version | head -1
ffmpeg version n9.0.2 Copyright (c) 2000-2026 the FFmpeg developers

$ tesseract --version | head -1
tesseract 5.5.3

$ v4l2-ctl --version
v4l2-ctl 1.32.0

$ tesseract --list-langs
List of available languages in "/usr/share/tessdata/" (2):
eng
osd
```

Capability evidence that the distro FFmpeg is actually sufficient for the capture
path (docs/spec/05 prefers UVC MJPEG/YUYV over a decoded container): the local
build is configured `--enable-libv4l2` and `ffmpeg -hide_banner -devices` lists
`DE video4linux2,v4l2` plus `DE alsa` — demuxing *and* muxing marked. Tesseract
`--help-extra` exposes `--psm PSM|NUM`, which is what docs/spec/05's PSM 6/7
requirement rides on. **Debian arm64 equivalents are unverified here**: bookworm
carries `ffmpeg 7:5.1.8-0+deb12u1`, `v4l-utils 1.22.1-5`, `tesseract-ocr 5.3.0-2`
and `tesseract-ocr-eng` per packages.debian.org, i.e. an FFmpeg **major version
older** than the dev host's 9.x and a Tesseract 5.3 vs 5.5 — so the wrappers must
parse versions and `doctor` must be the gate, and CLI differences between them are
M8 validation work, not an accepted claim.

The `CGO_ENABLED=0` premise that makes Part 1 viable, cross-compiled from this
host to the UNO Q architecture:

```
$ CGO_ENABLED=0 GOARCH=arm64 go build -o /tmp/gn-arm64 ./cmd/game-narrator && file /tmp/gn-arm64
/tmp/gn-arm64: ELF 64-bit LSB executable, ARM aarch64, version 1 (SYSV), statically linked, Go BuildID=aVkpESNvSYMsugo1U3WK/d833ZB5ITzxdbnZRilz9/wCQ5ZD76UilKjadeD_lF/XZIgIbGSJxtPH1pZOrIc, BuildID[sha1]=94881481a89b3ebb8739372260ac4858da54c650, with debug_info, not stripped
# and, on the same file:
$ ldd /tmp/gn-arm64
	not a dynamic executable
```

(16,976,733 bytes unstripped, `-trimpath` in the release build; the temporary
binary was removed after the check.)

Subprocess behaviour is specified, not yet implemented, so the tests below are
the **gate for accepting Part 1 in code** — written as failing tests first per
AGENTS.md "first reproduce, then repair":

- `internal/capture`: context cancel within deadline kills the FFmpeg process
  group (assert `wait` returns and no `ffmpeg` pid survives), device-lost exit
  code maps to a typed error, `-list_formats` output parses to the docs/spec/10
  format/fps record, dropped-frame rate computed from timestamps.
- `internal/vision`: Tesseract timeout produces `timeout` (retryable) rather than a
  wedged goroutine; missing `eng` language pack reports a named, actionable
  error; stdout/stderr overrun is bounded (feed 10 MB of stderr, assert the
  buffer cap and that memory stays flat); PSM 6/7 argv are the documented values.
- `game-narrator doctor`: with `PATH` emptied, exits non-zero and lists each tool
  as `MISSING`; with a stubbed `ffmpeg` on `PATH`, reports the stub's version
  string — proving it probes rather than assumes.
- No shell: a test asserts argv contains no `sh`/`bash` and that a filename with
  spaces and `;` is passed as one argument.
- Release gate: `goreleaser check`, image build for both architectures, manifest
  list assertion, `docker run` + `HEALTHCHECK` transition to `healthy`, non-root
  `id -u` assertion, and the vuln scan clean-or-tracked (docs/spec/15, M8).

## Consequences and risks

Positive: the release stays a single static Go binary plus documented OS
packages, cross-compiling unchanged (verified above); a crashing FFmpeg or
Tesseract degrades one capability instead of the whole service; tool versions are
observable per host and land in the evidence bundle; the container stays thin and
apt-updatable; the systemd `.deb` remains the primary UNO Q path with no container
runtime dependency.

Costs and risks:

- Process spawn per OCR call (tens of ms on 4 ARM cores) and pipe copies of raw
  frames — must be benchmarked on UNO Q against docs/spec/12 thresholds; the
  mitigation is batching regions per invocation and keeping a long-lived capture
  process, not a rewrite in cgo.
- **CLI/interface drift** between dev host (FFmpeg 9, Tesseract 5.5) and the
  Debian runtime (FFmpeg 5.1, Tesseract 5.3). Parsing is versioned at runtime;
  `doctor` is the acceptance gate; fixture replay (docs/spec/12) must record the
  producing tool versions.
- Subprocess supervision is real code with real failure modes (zombies, orphaned
  process groups, pipe deadlock when stdout is unread while the child fills
  stderr, SIGPIPE on early exit). The tests above exist because of this list.
- Device permissions (`/dev/video*`, `/dev/snd`) + group membership are the top
  deployment blocker; docs/spec/10 requires them validated before the sandbox
  options are chosen, and a container changes but does not remove that problem.
- Debian package pinning means the image is only as fresh as its base digest;
  requires a rebuild cadence plus scan gate (docs/spec/15).
- Tesseract accuracy is language-pack and PSM dependent — quality risk, tracked
  by docs/spec/05's temporal voting, not by this ADR.

## Migration / rollback

Part 1 is additive: introducing a wrapper package plus `doctor` touches no
existing behaviour, and rollback is deleting the wrapper (nothing else can call
the tools because the wrapper is the only exec site). If a tool turns out to be
unsuitable, replacing FFmpeg with direct V4L2 syscalls is a change inside
`internal/capture`'s `FrameSource` boundary (docs/spec/01), with no schema or API
change, and gets a superseding ADR carrying the frame-rate/CPU measurements.

Part 2 rollback is at the deployment layer, not the data layer: the OCI image is
a second delivery channel for the *same* binary and *same* `/var/lib/game-narrator`
data directory, so switching between container and `.deb` + systemd install keeps
data intact via the ADR-0008 `Backup`/`Restore` path (`game-narrator migrate`
stays the only schema mutator). If the Debian slim base becomes unusable,
rebuild from a pinned newer slim or an explicit base digest — never a moving tag;
if the whole OCI channel is dropped, `dockers_v2` is removed from the GoReleaser
config and the archive/`.deb` channels stand alone (ADR-0006).

## Follow-ups

- Implement `game-narrator doctor` (in flight alongside this ADR) and paste its
  real output here; it is the evidence that Part 1's "probe, never assume" rule is
  enforced.
- Build the first real image in M8 and replace Part 2's status with `accepted`,
  attaching: image digest, base digest, pinned package versions, `docker history`
  size, non-root check, manifest-list output and the vulnerability-scan report.
- Measure per-invocation OCR latency and CPU on UNO Q (arm64, 4 GB) versus the
  dev host; record the numbers here before any cgo reconsideration.
- Decide frame transport detail (pipe vs `/dev/shm` ring) once the 1080p32 → ROI
  crop budget in docs/spec/05 is measured.
- Choose the ALSA/PipeWire client dependency for the audio sink and pin it in the
  same Dockerfile block (docs/spec/07 "USB ALSA/PipeWire sink by stable device
  identity").
- Document device mapping for containers (`--device /dev/video*`, sound group) and
  confirm the `systemd` unit remains the recommended path in the release notes.
- Review when Milestone 2 (capture + OCR) starts and again at M8 packaging.
