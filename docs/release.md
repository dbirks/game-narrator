# Release runbook — Game Narrator

Covers snapshots, verification, tagged releases, upgrade, rollback and purge
semantics. Packaging decisions and their tradeoffs: ADR 0033; strategy:
ADR 0006 / ADR 0012; policy: docs/spec/15, docs/spec/14 "Supply chain",
AGENTS.md rules 5 and 12.

## Hard rules

- **Never publish without explicit user approval.** `release.disable: true`
  is committed in `.goreleaser.yaml`; flipping it is a reviewed change plus
  user sign-off. Snapshot runs never publish regardless.
- **Hardware is NOT VERIFIED until proven.** Nothing in this repo proves the
  UNO Q install works. `arm64` artifacts are cross-compiled and file-verified
  only; the arm64 image layer builds under QEMU but was not executed on real
  ARM. Any release report must state which of (UNO Q install, real-hardware
  capture/OCR/audio, cosign signatures) remain unverified — AGENTS.md rule 12.
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

## 2. Tagged release (only after gates + approval)

Preconditions (docs/spec/15 "Release gate matrix"):

1. `make test-full` passes on the dev host (gates that cannot run here are
   reported NOT RUN with setup instructions; the UNO Q hardware gate is
   NOT RUN until UNO Q is available).
2. Milestone complete, ADRs updated, release notes ready.
3. **User approval** to publish: enable `release` in `.goreleaser.yaml` in a
   reviewed change (draft: true recommended), provide the registry/repository
   and credentials out-of-band.

Then:

```sh
git tag v1.2.3
GN_DOCKER_REPO=ghcr.io/<owner>/game-narrator goreleaser release --clean
```

- Archives/.debs/SBOMs/checksums are produced as in the snapshot; the
  `release` pipe uploads to the (configured, non-GitHub-by-default) release
  target; `dockers_v2` pushes a multi-arch manifest list with an SBOM
  attestation (`--push --attest=type=sbom`) to `$GN_DOCKER_REPO` — requires a
  `docker login` to a registry you own. Without `GN_DOCKER_REPO` the docker
  section is disabled: no push path exists.
- Record the pushed manifest digest in the release evidence
  (`artifacts/<run-id>/manifest`, AGENTS.md).
- **cosign keyless signing is documented, not enabled.** When approved, the
  config addition is:

  ```yaml
  signs:
    - cmd: cosign
      args: [sign-blob, "--yes", "--output-sig=${signature}", "${artifact}"]
    - cmd: cosign
      artifacts: checksum
      args: [sign-blob, "--yes", "--output-sig=${signature}", "${artifact}"]
  ```

  (cosign keyless, Sigstore identity; fulcio at the default endpoint unless
  `COSIGN_FULCIO_URL` is set in the signing environment. The `signs` schema
  was verified against the installed 2.14 jsonschema; run `goreleaser check`
  again after any edit. Not run here without approval.)

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
SBOMs, image digests (`docker image inspect`), and for tagged releases the
manifest digest. `make test-full` output lists every gate and its NOT RUN
reason (hardware, replay, Dolphin, models — unverified on this host).
