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

## Amendment (2026-10-22): image-scan rule split, owned-file derivation, GHA annotations

Supersedes the Option 5 / "image scanner scoped to the owned binary" scope
only; everything else in this ADR stands.

**Context.** The CI publish run's "Scan release artifacts" step failed on the
image phase: the content (secret/topology) rule fired inside distribution
files. Crane-based verification showed the pinned base
(`debian@sha256:7c7b2c96…`, 3265 files) is 100% clean — the hits come from
the **apt layer** the Dockerfile installs: four Debian files ship literal
`/home/<name>/` strings (OpenAL/ALSA sample configs, xkb symbols). Those
layers are not base-digest-excludable, so scoping by "owned binary only"
(coarse) or by a Debian path allowlist (fragile) were both rejected.

**Decision.** `scripts/release/scan-artifacts.sh` image phase now splits the
rules:

- **Whole-image rules** — `forbidden-name`, `game-data` (game-image
  extensions + CISO/RARC/Yaz0/CD001 magic, so dumps hidden under innocent
  names are caught), `big-binary` (non-text ≥ 256 MiB; largest legitimate
  file in base + pinned apt closure is ~112 MiB libLLVM-15.so.1) — run over
  every file in every layer. Verified 0 hits on the crane-exported base and
  the 223-package apt closure.
- **Content rules** (home paths, gateway hostname/token) — run only over the
  files our Dockerfile adds: the `COPY` destination(s) (parsed from
  `packaging/Dockerfile`, WORKDIR-aware) and the image config blob. Base
  layers are additionally excluded by digest (pinned base ref parsed from
  the Dockerfile; layer digests via `docker manifest inspect`, fallbacks:
  local base layer count, then unknown). The owned set is derived from the
  Dockerfile — **never** a hand-written Debian allowlist.
- An image with no file at a `COPY` destination in a non-base layer **fails**
  (`image-no-owned-files`) — a missing/broken binary must not silently pass.
- Under `GITHUB_ACTIONS=true`, `scan-artifacts.sh` emits one
  `::error file=…,title=artifact-scan::<rule> <artifact>:<path> [masked]`
  per finding plus a final `::notice`/`::error` summary (escaping mirrors
  `scripts/gates/annotate-go.sh`; the matched value is never printed). In
  `scripts/gates/secret-scan.sh`, allowlisted items are `::notice` (or
  omitted) and only real findings are `::error`, with a final summary
  annotation — the green run previously showed ~20 "allowlisted" failure
  annotations.

**Validation.** Scanner self-test (4 cases: planted home path fails; redactor
regex source passes; synthetic image split; apt-layer non-COPY Debian file
must not flag while a planted app secret at the COPY destination fails);
crane export of the pinned base (0 hits on all whole-image rules); fake-docker
pipeline runs exercising `docker save` layout parsing, per-layer extraction,
digest-based base classification and the rule split, including the four real
apt files (clean image passes; planted app secret fails with exactly one
finding). `GITHUB_ACTIONS=true scripts/gates/secret-scan.sh` emits 0
`::error` lines on a green tree.

**Reversal.** Revert `scripts/release/scan-artifacts.sh` (image phase) and
the `scripts/gates/secret-scan.sh` annotation changes; no schema, protocol
or packaging impact.
