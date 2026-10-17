# ADR-0033: GoReleaser snapshot pipeline, multi-arch artifacts, .deb/systemd packaging and artifact scanner

Date: 2026-10-17
Status: accepted
Owners: project
Related requirements/tests: docs/spec/15 (release policy, supply chain, release gate matrix); AGENTS.md rules 5 (provenance, never commit secrets/private topology), 10 (version/migration discipline), 12 (release gates, hardware NOT VERIFIED); builds on ADR 0006 (multi-arch strategy), ADR 0012 (native media tools + pinned OCI base), ADR 0008 (SQLite, no GoReleaser DB migrations)

## Context and constraints

- m8-packaging requires a reproducible artifact pipeline: `linux/amd64` + `linux/arm64` tar.gz with checksums, `.deb` packages, a multi-arch OCI image, SBOMs, and an artifact scanner proving no game/voice/private data leaks into release artifacts.
- GoReleaser v2 is the chosen tool (ADR 0006). The installed version is **2.14.0**; its schema was verified empirically with `goreleaser check`/`goreleaser jsonschema` rather than assumed.
- The product binary is pure Go (CGO_ENABLED=0) with the React/TypeScript frontend embedded at build time (`web/dist`, embedded via `make web-build` before goreleaser runs).
- Hard constraints: no publishing without explicit user approval; no `.rpm` unless install-smoked; no secrets or private topology (gateway hostname) in artifacts or committed files; `arm64`/UNO Q runtime cannot be executed on the amd64 dev host, so those verifications must be reported NOT VERIFIED, not faked (AGENTS.md 12, No simulation theater).

## Options considered

1. **GoReleaser v2 with `dockers`** — the classic pipe. Rejected: in 2.14 `dockers` is deprecated in favour of `dockers_v2` (per-platform builds with `--load`/`--push`, SBOM attestation support).
2. **`dockers_v2`** — chosen. Snapshot mode builds and `docker load`s per-arch images tagged `<image>-<arch>` without pushing; tagged releases push a multi-arch manifest list with an optional SBOM attestation. Verified from the 2.14 source (`internal/pipe/docker/v2`), not just docs.
3. **`nfpms` formats**: `.deb` only for the snapshot pipeline. `.rpm` is a cheap add but cannot be install-smoked on this host (no rpm toolchain/container policy agreed), so per the m8 constraint it is omitted until it can be verified.
4. **SBOM tooling**: syft via GoReleaser's `sboms` pipe (SPDX JSON, one file per artifact). `--enrich` (Vulnerability/OS enrichment) requires network enrichment of package indices and was left off for offline reproducible snapshots; enrichment can be re-enabled on tagged releases.
5. **Scanner scope for images**: scanning *all* image layer contents for `/home/` etc. false-positives on the distribution's own files (e.g. Debian's `/usr/sbin/update-passwd`, `/etc/openal/alsoft.conf` legitimately contain `/home/`). Decision: the scanner checks the **owned** file in the image (the `game-narrator` binary — the only thing the Dockerfile `COPY`s) plus the full contents of our own archives/.debs. Distribution layers are covered by the pinned base digest (ADR 0012) + SBOM.
6. **Docker availability gating**: the Makefile runs `goreleaser` inside `newgrp docker` so buildx can talk to the daemon without sudo; the `dockers_v2` section self-disables when `GN_DOCKER_REPO` is unset (via `isEnvSet`), so the snapshot pipeline degrades to archives/.debs/SBOMs on hosts without a docker daemon.

## Decision

- Ship `.goreleaser.yaml` (v2) with: `builds` (static, `-trimpath`, version+commit ldflags), `before` (`make web-build`), `archives` (tar.gz with README, config examples, systemd unit; globs, no `format_overrides`), `checksum` (SHA256SUMS), `sboms` (syft SPDX JSON per artifact), `nfpms` (`.deb` only, `config|noreplace` for the config example, maintainer scripts under `/usr/lib/game-narrator/`, `Recommends` for `ffmpeg|tesseract-ocr`), and `dockers_v2` (self-disabling when `GN_DOCKER_REPO` unset; tags from `{{ .Version }}`).
- `packaging/` holds the systemd unit (dedicated `game-narrator` user, `SupplementaryGroups=video audio`, hardening set, no `SystemCallFilter` yet — to be added after UNO Q validation), the deb maintainer scripts (idempotent, never delete `/var/lib/game-narrator`), and the `Dockerfile` (pinned base digest per ADR 0012, non-root, pinned bookworm package versions, `/var/lib/game-narrator` volume, `/api/v1/health/live` healthcheck).
- `scripts/release/scan-artifacts.sh` fails on: forbidden file names (game/voice/private data globs), `/home/` path leakage, the private gateway hostname (when supplied via `GN_GATEWAY_HOSTNAME`) and gateway token values in any owned artifact (archives, .deb data+control, image-owned binary).
- `scripts/release/smoke.sh` runs the scanner plus real artifact verification: amd64 binary `version` + `serve` + health/`/diagnostics` HTTP 200s; arm64 binary `file` check (aarch64, static); `.deb` `dpkg-deb --info/--contents`; when docker is available, install the `.deb` in a `debian:bookworm-slim` container with the real apt dependencies and run the amd64 image (`/api/v1/health/live` 200, non-root uid, `HEALTHCHECK` → healthy). Unrunnable steps report `NOT RUN`, never green.
- `make release-snapshot` = `goreleaser check` → `goreleaser release --snapshot --clean` (in `newgrp docker`, `GN_DOCKER_REPO=game-narrator`) → `smoke.sh`. It is a real gate, not a pending placeholder; it never publishes (snapshot mode + `release.disable: true`).
- Tagged releases keep `release.disable: true` committed; enabling it, choosing the registry, and running `goreleaser release` on a tag is a user-approved action (docs/release.md §2). cosign keyless signing is documented but not enabled without approval.

## Tradeoffs

- **No `.rpm` in the snapshot pipeline**: smaller surface, everything shipped is install-smoked. Cost: RHEL-family users wait for the rpm install-smoke path.
- **Image scanner scoped to the owned binary**: avoids false positives on distro files at the cost of not scanning third-party layers for our leak patterns — acceptable because those layers are distribution packages pinned by digest and listed in the SBOM, and our Dockerfile cannot leak repo files (goreleaser context only contains the binary + .deb).
- **`GN_DOCKER_REPO` gating via `isEnvSet`**: snapshots build local-only images without a configured registry; the same config serves tagged releases pointing at the user's registry. Cost: the image name for snapshots is the literal `game-narrator` (library namespace) — fine for local `docker load`, never pushed.
- **syft without `--enrich`**: reproducible offline SBOMs; vulnerability enrichment deferred to the tagged-release stage.

## Validation

- `goreleaser check` passes on the committed config (2.14.0).
- Full `make release-snapshot` run produced: `game-narrator_0.0.0-SNAPSHOT-<sha>_linux_{amd64,arm64}.tar.gz`, `..._linux_{amd64,arm64}.deb`, per-arch `sbom.spdx.json`, `checksums.txt`, and local images `game-narrator:<version>-{amd64,arm64}` (built under QEMU binfmt for arm64; not pushed).
- `scripts/release/smoke.sh`: 16 steps passed, 0 NOT RUN on this host (scanner clean; amd64 binary serves health+diagnostics; arm64 binary is aarch64 static; both `.deb`s valid; `.deb` installed and ran in bookworm; amd64 image healthy and non-root; arm64 image present).
- arm64 *runtime* and UNO Q *hardware* install are **NOT VERIFIED** (no arm64/UNO Q hardware on this host) — reported as such, per AGENTS.md 12.

## Reversal plan

- Roll back by removing the `dockers_v2`/`sboms`/`nfpms` blocks (or the whole file) and the `release-snapshot` Makefile target; artifacts revert to a plain `go build`. Packaging files under `packaging/` and `scripts/release/` are self-contained and can be deleted without touching Go sources.
- To adopt `dockers` (v1) instead of `dockers_v2`, or `.rpm`, requires a new ADR and install-smoke evidence.
