# 15 — Publishing, GoReleaser, packages, container images and upgrades

## Targets

- Primary: `linux/arm64` GNU/Linux Go binary for Debian-based UNO Q (4 GB), with frontend embedded.
- Developer: `linux/amd64` Arch Linux standalone binary, identical feature flags except Arch-specific dev harness permissions.
- Containers: multi-platform `linux/amd64` and `linux/arm64` OCI image (GHCR by default; repository TBD) for dev/optional deployment. Container entrypoint doesn't magically include Tesseract, FFmpeg, device permissions or the game.
- OS packages: `.deb` for UNO Q and optional Arch package; nFPM integration through GoReleaser. Tar archives with checksums. Optional `.rpm` if tested; avoid claiming support without install smoke.

## GoReleaser v2 plan

Use `version: 2`, `builds` with `CGO_ENABLED=0` *if and only if Go packages are pure-Go* and native tools are subprocesses; if an SQLite driver needs CGO, either use pure-Go SQLite or cross-compile with a pinned native cross-toolchain, and document choice via ADR. Assemble frontend assets before Go build (`pnpm --frozen-lockfile && pnpm build`, copy into embed dir). `archives`, `checksum`, `nfpms` and **`dockers_v2`** with Docker Buildx for multiarch images (current GoReleaser docs explicitly prefer new Docker v2). Verify exact installed version/config schema via `goreleaser check`; no invented config keys. Provide Dockerfile and pinned base digest once tested.

## Example release workflow (intent, adjust syntax to installed tools)

```text
pnpm --dir web install --frozen-lockfile
pnpm --dir web run lint && pnpm --dir web run test && pnpm --dir web run build
# copy dist to internal/webassets/dist (generated, not user data)
go test ./...
go vet ./...
goreleaser check
goreleaser release --snapshot --clean
# validate archives, OS pkg install, OCI manifest and embedded UI on both arch targets
# tagged publish only after full gates and explicit repo permissions
```

Use `goreleaser/goreleaser-action` pinned to a reviewed version in CI and `permissions: contents: write, packages: write` only on release job; no broad permissions for test jobs. Publish tag/version/commit into `game-narrator version` and `/system/info`. Generate SBOM with Syft (or GoReleaser integration verified against installed version), checksums and optional keyless Sigstore/cosign signatures.

## OCI image design

Multi-stage build: Node/Vite UI, Go build, runtime Debian slim or distroless **only if** compatible with Tesseract/FFmpeg/audio dependencies. If those utilities are external, pick a thin Debian base and install pinned distro packages, run as non-root, expose health port, make `/var/lib/game-narrator` a volume, and document optional `/dev/video*`, `/dev/snd`, group mappings. Container build must not copy ROMs, `.env`, user art/voices or dev fixture dumps. Add OCI labels, healthcheck, manifest-list tests and vuln scan. Use tagged `:vX.Y.Z` and digest pin; `latest` only stable if policy chosen.

## Debian/systemd package

Ship binary, service unit, config example, docs and native dependency declarations. Preserve existing `/etc/game-narrator/config.yaml` and `/var/lib/game-narrator` on upgrade/uninstall unless explicit purge. Run migration with safe backup/rollback instructions, restart health readiness and role-controlled startup. `game-narrator doctor` should verify OS permissions after install. Package scripts must be safe and idempotent.

## Release gate matrix

`release-snapshot` validates build, per-platform binary startup + UI served, schema migrations + sample profile loaded, OCI health, package install smoke and supply-chain artifacts. `release` requires tagged version, security checks, entire available E2E suite with explicit unverified hardware note if applicable, milestone implementation complete, ADR updates and release notes. Rollback plan pins previous binary/container digest and uses backward-compatible DB migration or documented DB restore.

## Publishing restrictions

Never include games/scripts, voice reference samples, debug screenshots, cache, API keys or private gateway topology in Docker layers, archives, apt packages, SBOMs or GitHub Releases. Run artifact scanner for known MIME/file patterns before publishing. Public repo path/registry must remain configurable, no invented organization ownership.
