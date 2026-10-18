# Release runbook — Game Narrator

Covers snapshots, verification, tagged releases, upgrade, rollback and purge
semantics. Packaging decisions and their tradeoffs: ADR 0033; strategy:
ADR 0006 / ADR 0012; policy: docs/spec/15, docs/spec/14 "Supply chain",
AGENTS.md rules 5 and 12.

## Hard rules

- **Publishing is approved and limited to the GitHub Actions pipeline**
  (user approval 2026-10-17; ADR 0048). `.github/workflows/publish.yml`
  publishes `edge` images from `main` (after CI) and `v*` tags (prerelease
  for `v0.*`), using the repo's `GITHUB_TOKEN` as the only credential and
  cosign **keyless** (Sigstore identity bound to publish.yml — no keys in
  the repo). Local `goreleaser` runs never publish: snapshot runs skip
  publishing by design, and `release.disable` is a template that evaluates
  to `true` whenever `GITHUB_ACTIONS` is unset (section 2.3).
- **Hardware is NOT VERIFIED until proven.** Nothing in this repo proves the
  UNO Q install works. `arm64` artifacts are cross-compiled and file-verified
  only; the arm64 image layer builds under QEMU but was not executed on real
  ARM. Any release report must state which of (UNO Q install, real-hardware
  capture/OCR/audio) remain unverified — AGENTS.md rule 12. (cosign
  signatures are produced and self-verified by the publish pipeline,
  section 2.)
- **Secrets and private topology never enter artifacts.** The scanner
  (`scripts/release/scan-artifacts.sh`) fails the build on game/voice/private
  file names, `/home/` paths, the private gateway hostname (when provided via
  `GN_GATEWAY_HOSTNAME`) and gateway token values: any non-empty
  `GAME_NARRATOR_GATEWAY_TOKEN=` value other than the exact placeholder
  `replace-with-real-secret-outside-git` (see examples/config/env.example)
  fails the build. The gateway hostname is read from the environment at scan
  time and is never written into the repo.

## 1. Snapshot (local, no publishing)

```sh
make release-snapshot
```

Runs `goreleaser check`, then `goreleaser release --snapshot --clean`
(`GN_DOCKER_REPO=game-narrator`, local-only image names; with the env unset
the `dockers_v2` section disables itself and no image is built), then
`scripts/release/smoke.sh`:

1. **Artifact scanner** over every `dist/*.tar.gz`, `dist/*.deb` (data +
   control) and, when docker is available, the image's **owned** binary file
   (distribution layers are the distribution's supply chain — pinned base
   digest + SBOM — not this scanner's scope; Debian files legitimately
   contain `/home/` strings and would false-positive).
2. **amd64 binary** (from the archive): `version`; `serve` on a temp port
   with a temp data dir; `GET /api/v1/health/ready` → 200,
   `GET /api/v1/health/live` → 200, `GET /diagnostics` serves the embedded
   React UI HTML.
3. **arm64 binary**: `file` shows ARM aarch64, statically linked
   (not executed — no arm64 hardware/VM on the dev host).
4. **.deb**: `dpkg-deb --info` / `--contents` (binary,
   `/lib/systemd/system/game-narrator.service`,
   `/etc/game-narrator/config.yaml` 0640) for both architectures.
5. **Docker** (when available): the .deb installed in a `debian:bookworm-slim`
   container with the real apt dependencies (`dpkg -i`, postinst exercised),
   `game-narrator version` + `migrate` inside it; the amd64 image `docker run`
   — `/api/v1/health/live` 200, non-root `id -u`, `HEALTHCHECK` → `healthy`;
   both per-arch local images present.

Artifacts land in `dist/`: `*.tar.gz` (+ README, config examples, the systemd
unit), `*.deb`, per-arch `sbom.spdx.json` (syft, no network enrichment),
`checksums.txt`, `artifacts.json`, and the local images
`game-narrator:<version>-amd64|arm64` (snapshot mode loads per-arch images;
it does **not** create a manifest list and does **not** push — verified
against the goreleaser 2.14 `internal/pipe/docker/v2` source).

Unimplemented steps report `NOT RUN: …`, never green.

## 2. Publishing (approved — GitHub Actions only, ADR 0048)

Publishing is limited to `.github/workflows/publish.yml`. It uses the repo's
`GITHUB_TOKEN` only (`permissions: packages: write, id-token: write`): no
personal registry credentials and no cosign keys — signatures are keyless
Sigstore (Fulcio + CT) via GitHub OIDC, bound to the publish workflow.

### 2.1 `edge` on every `main` push (after CI succeeds)

Triggered by `workflow_run` when the `ci` workflow completes `success` for a
push to `main`:

1. GoReleaser **snapshot** with `GN_DOCKER_REPO=ghcr.io/dbirks/game-narrator`
   — per-arch images built locally (`0.0.0-snapshot-<sha7>-amd64|arm64`);
   snapshot mode never pushes.
2. `scripts/release/scan-artifacts.sh` over the dist artifacts **and the
   image layers of both per-arch images** — a failure pushes nothing.
3. Push: the per-arch images, then the multi-arch manifests tagged `edge`
   and `sha-<short>` (immutable per-commit handle).
4. `cosign sign` the manifest digest and attach the SBOM attestation
   (`cosign attest --type spdxjson`; predicate = the goreleaser/syft SBOM
   of the amd64 archive). The job finishes with a `cosign verify`
   self-check against the publish.yml certificate identity.

Pull `ghcr.io/dbirks/game-narrator:edge` (or `:sha-<short>`) to consume.

### 2.2 Tagged release (`v*` tag)

Preconditions (docs/spec/15 "Release gate matrix"): `make test-full` passes
on the dev host (gates that cannot run here are reported NOT RUN with setup
instructions; the UNO Q hardware gate is NOT RUN until UNO Q is available),
milestone complete, ADRs updated, release notes ready.

```sh
git tag v0.4.2 && git push origin v0.4.2
```

This runs the `release` job in `.github/workflows/publish.yml`:
`goreleaser release --clean` with `GN_DOCKER_REPO=ghcr.io/dbirks/game-narrator`
— archives/.debs/SBOMs/checksums, a GitHub Release (**prerelease** for
`v0.*` tags, normal release otherwise), the multi-arch image pushed under
the tag (`v0.4.2`) with the goreleaser SBOM attestation, and keyless cosign
signatures on the per-arch images **and** the multi-arch manifest
(`docker_signs` in `.goreleaser.yaml`). The artifact scanner then runs over
`dist/` (archives/.debs; image layers are covered by the pinned base digest
and the image SBOM attestation), and the job verifies the image signature
(self-check).

### 2.3 Why local `goreleaser` cannot publish

- `--snapshot` implies `--skip=announce,publish,validate` (goreleaser 2.14):
  `make release-snapshot` never pushes and never signs.
- `release.disable` is the template
  `{{ if not (isEnvSet "GITHUB_ACTIONS") }}true{{ end }}`: a local
  `goreleaser release` (no CI env) creates no GitHub Release.
- `dockers_v2` pushes only when `GN_DOCKER_REPO` is set, and `docker_signs`
  runs only in the publish phase. A local non-snapshot `goreleaser release`
  with `GN_DOCKER_REPO` set is an explicit operator opt-in that will push
  images but fail at signing without CI OIDC — the safe failure is loud.

### 2.4 Verifying a signature

```sh
cosign verify \
  ghcr.io/dbirks/game-narrator:edge \
  --certificate-identity-regexp 'https://github.com/dbirks/game-narrator/.github/workflows/publish.yml@.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

For a tagged release replace `:edge` with the tag (e.g. `:v1.2.3`);
`cosign verify-attestation --type spdxjson` shows the SBOM attestation.

**UNO Q remains NOT VERIFIED** until the hardware smoke
(`make test-hardware`) passes (AGENTS.md rule 12): publishing and signing do
not change that — arm64 artifacts are cross-compiled and file-verified only,
and the arm64 image layer builds under QEMU but was not executed on real ARM.

## 3. Upgrade

- **.deb + systemd (primary UNO Q path):**
  `apt-get install ./game-narrator_<new>_linux_arm64.deb` (or the apt repo
  once one exists). `prerm` stops the unit (graceful drain: provider calls,
  speech, SQLite flush, camera/DAC close); the new binary is installed;
  **start the unit again** (`systemctl start game-narrator`) — the package
  does not auto-restart a running service across an upgrade, so an operator
  who was running it must notice it stopped and start it back.
  Migrations run automatically when `serve` opens the
  database (or pre-emptively with `game-narrator migrate`); schema migrations
  are the only mutators (ADR 0008) and are backward-compatible readers for
  one prior minor revision (docs/spec/02).
- **Binary:** stop the unit, replace the binary, `systemctl start`.
- **Container:** pull the new tag/digest, recreate the container with the
  same data volume.
- `/etc/game-narrator/config.yaml` and `env` are `config|noreplace` /
  admin-created: upgrades preserve them. Take a backup first (below).

## 4. Rollback

1. **Backup first (always):** stop the service, then copy the durable data
   directory (stopped process ⇒ consistent WAL) — or use the programmatic
   backup API (`store.Backup`/`store.Restore`, SQLite online backup API,
   docs/spec/02 "Migration and backup acceptance") once exposed at the CLI.
   The data directory (catalogue + content-addressed assets) is one unit:
   restore both or neither.
2. **Previous binary/package:** reinstall the previous `.deb` (apt keeps
   history for one prior version) or the previously recorded binary from the
   release evidence bundle.
3. **Previous image:** `docker pull <repo>@<previous-digest>` (digests are
   recorded in `artifacts/<run-id>/manifest`) and recreate the container.
4. **Schema:** if the new version applied a migration and the old binary
   cannot read the new schema, restore the DB backup (step 1) before
   starting the old binary. Migrations do not auto-downgrade (docs/spec/02).
5. Verify: `game-narrator doctor` + `/api/v1/health/ready`.

## 5. Purge semantics (Debian)

- `dpkg -r game-narrator` (remove): package files removed, **config files
  kept** (`/etc/game-narrator/*`).
- `dpkg --purge game-narrator`: package files **and** config files removed.
- **`/var/lib/game-narrator` is never deleted by the package** — on remove or
  purge. It is user data (catalogue + assets + voice references, including
  approved voice references that AGENTS.md forbids deleting). Deletion is an
  explicit admin act: back it up (section 4), then remove it.
- The `game-narrator` system user is intentionally **not** removed by the
  maintainer scripts either (removing it mid-purge would orphan ownership of
  a not-yet-deleted data directory). `userdel game-narrator` after the data
  directory is gone is the admin's call.

## 6. Verification evidence

Each snapshot/release run should retain: `goreleaser` log, `dist/` listing +
`checksums.txt`, scanner output, smoke log (all steps + NOT RUN reasons),
SBOMs, image digests (`docker image inspect`), and for published images the
manifest digest plus the `cosign verify` output (sections 2.1/2.2/2.4).
`make test-full` output lists every gate and its NOT RUN
reason (hardware, replay, Dolphin, models — unverified on this host).
