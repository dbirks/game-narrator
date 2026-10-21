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
   control), `dist/*.pkg.tar.zst` (payload + `.PKGINFO`/`.MTREE`/`.INSTALL`
   metadata) and, when docker is available, the image's **owned** binary file
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
unit), `*.deb`, `*.pkg.tar.zst` (Arch, ADR 0053), per-arch `sbom.spdx.json`
(syft, no network enrichment),
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
   — per-arch images built locally
   (`0.0.0-snapshot.<commit-ts>.g<sha7>-amd64|arm64`, section 9);
   snapshot mode never pushes.
2. `scripts/release/scan-artifacts.sh` over the dist artifacts **and the
   image layers of both per-arch images** (rule split in §2.5) — a failure
   pushes nothing, and every finding is annotated on the run.
3. Push: the per-arch images, then the multi-arch manifests tagged `edge`
   and `sha-<short>` (immutable per-commit handle).
4. `cosign sign` the manifest digest and attach the SBOM attestation
   (`cosign attest --type spdxjson`; predicate = the goreleaser/syft SBOM
   of the amd64 archive). The job finishes with a `cosign verify`
   self-check against the publish.yml certificate identity.

Pull `ghcr.io/dbirks/game-narrator:edge` (or `:sha-<short>`) to consume.

### 2.2 Cutting a release (tagged `v*`)

Preconditions (docs/spec/15 "Release gate matrix"): `make test-full` passes
on the dev host (gates that cannot run here are reported NOT RUN with setup
instructions; the UNO Q hardware gate is NOT RUN until UNO Q is available),
milestone complete, ADRs updated.

Optional but recommended: dry-run the tagged config path locally before
pushing the tag (in a throwaway `git worktree` of the commit you will tag —
never push the tag; goreleaser refuses a dirty worktree, so keep build logs
outside it):

```sh
git worktree add --detach /tmp/gn-rel-dryrun HEAD
cd /tmp/gn-rel-dryrun
git tag -a v0.1.0-rc.0 -m "dry-run"        # local-only
goreleaser release --clean --skip=announce,publish,sign,docker
# inspect dist/ (names, CHANGELOG.md, dpkg-deb -f ..._linux_arm64.deb Version)
git tag -d v0.1.0-rc.0 && git worktree remove --force /tmp/gn-rel-dryrun
```

`--skip` values are valid goreleaser 2.14 pipe names; the skip set means
nothing is announced, published (no GitHub Release, no image push) or
signed. The run still exercises the tagged path end to end: versions
without the snapshot suffix, archive/nfpm names, `dist/CHANGELOG.md`,
`checksums.txt`, SBOMs and the embedded `game-narrator version` stamp
(dry-run evidence: tag `v0.1.0-rc.0` -> `game-narrator_0.1.0-rc.0_linux_
{arm64,amd64}.{tar.gz,deb,pkg.tar.zst}`, deb control `Version: 0.1.0~rc.0`,
which `dpkg --compare-versions … gt 0.0.0~snapshot.9999999999.gffffff`).

Then cut and push the tag:

```sh
git tag -a v0.4.2 -m "Release v0.4.2" && git push origin v0.4.2
```

The push triggers the `release` job in `.github/workflows/publish.yml`
(`if: push && startsWith(github.ref, 'refs/tags/v')`), which runs
`goreleaser release --clean` with `GN_DOCKER_REPO=ghcr.io/dbirks/game-narrator`
and the repo `GITHUB_TOKEN`:

- **Version**: goreleaser `.Version` is the tag WITHOUT the leading `v`
  (`v0.4.2` -> `0.4.2`); everything below is named from it. The .deb's
  control `Version` is `0.4.2` (a prerelease tag like `v0.1.0-rc.0` becomes
  `0.1.0~rc.0`, which `dpkg --compare-versions` orders above every
  `0.0.0~snapshot.*` — section 9); the Arch package keeps `pkgver 0.4.2`.
- **Artifacts**: `game-narrator_0.4.2_linux_{amd64,arm64}.tar.gz`, the
  `.deb` and `*.pkg.tar.zst` packages, one Syft SPDX SBOM per archive
  (`game-narrator_0.4.2_<arch>.sbom.spdx.json`), `checksums.txt` and
  `CHANGELOG.md`.
- **Release notes**: the `changelog` pipe (default `use: git`) generates
  the notes from `git log <previous-tag>..v0.4.2` (commit subjects, full
  history if this is the first tag) and writes `dist/CHANGELOG.md`; the
  `release` pipe then uses that text as the GitHub Release body
  (v2.14.0 `internal/pipe/release/body.go`: header + notes + footer).
- **GitHub Release**: created as a **prerelease** for `v0.*` tags
  (`release.prerelease` template: `enable` iff the tag has the `v0.`
  prefix, else `disable`), normal otherwise. Local runs can never create
  it (`release.disable` is true when `GITHUB_ACTIONS` is unset — §2.3).
- **Image**: the multi-arch manifest is pushed as
  `ghcr.io/dbirks/game-narrator:0.4.2` (single tag; per-arch digests
  inside) with the goreleaser SBOM attestation, then keyless cosign
  signatures on the per-arch images **and** the manifest
  (`docker_signs`, `artifacts: all`). Identity: Sigstore via GitHub OIDC,
  certificate identity
  `https://github.com/dbirks/game-narrator/.github/workflows/publish.yml@refs/tags/v0.4.2`
  (no keys in the repo, ADR 0048). `cosign sign-blob` is not used anywhere:
  archives/checksums are **not** individually signed — their integrity is
  `checksums.txt` + the scanner verdict + the release's artifact listing.
- **Scan + self-check**: `scripts/release/scan-artifacts.sh` over `dist/`,
  then `cosign verify` of the pushed image tag against the publish.yml
  identity (the same command as §2.4 with `:0.4.2`).

**Verify before consuming** (docs/manuals/17): the image signature and
SBOM attestation (§2.4, tag `:0.4.2` — no `v`), `sha256sum -c
checksums.txt` over the downloaded artifacts, and the run's recorded
scanner verdicts (§2.5, §7).

**Rolling back a bad release**: a pushed release is treated as immutable
— never move a published image tag to a different digest or force-update
a tag (digests and scanner evidence are recorded per tag in
`artifacts/<run-id>/manifest`). Instead: fix and cut the next tag
(`v0.4.3`, or `v0.1.0-rc.1` within a prerelease line) and point consumers
at the new tag/digest; optionally mark the bad GitHub Release as
`yanked` via the GitHub UI (artifacts remain, for audit). On devices that
already installed it, roll back per section 4 (previous `.deb`/
`pacman` + data-directory backup; images: `docker pull
<repo>@<previous-digest>`).

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

For a tagged release replace `:edge` with the image tag — the release
version WITHOUT the leading `v` (e.g. `:1.2.3` for tag `v1.2.3`);
`cosign verify-attestation --type spdxjson` shows the SBOM attestation.

**UNO Q remains NOT VERIFIED** until the hardware smoke
(`make test-hardware`) passes (AGENTS.md rule 12): publishing and signing do
not change that — arm64 artifacts are cross-compiled and file-verified only,
and the arm64 image layer builds under QEMU but was not executed on real ARM.

### 2.5 Artifact scan: image phase and annotations (ADR 0033 amendments)

The release image mixes product files with distribution content: the pinned
`debian@sha256:…` base **plus** the apt layer that `packaging/Dockerfile`
installs. The scanner first rebuilds the image's **merged filesystem**
(`scripts/release/scan-artifacts.sh`): `docker save`, then every layer blob
(gzip, zstd, or plain tar, detected by magic bytes) extracted in manifest
order, whiteouts (`.wh.<name>` deletes, `.wh..wh..opq` opaque dirs) applied,
so the rules judge the final filesystem. Both `docker save` layout kinds are
handled — the OCI layout of the Docker 2x containerd image store
(index.json → manifest → blobs/sha256/\<digest\>) and the classic layout
(manifest.json + \<id\>/layer.tar). **No digest is ever matched**: the OCI
layout keys layer blobs by the manifest's *compressed* layer digest while
the image config's `rootfs.diff_ids` hold *uncompressed* digests, and
digest-matching across kinds misclassified layers on real runners — only
ordered local blob paths are consumed.

Two rule classes have different scope over the merged filesystem:

- **Whole-image rules** — `forbidden-name` (game/voice/private file names;
  a `*.pem` without a PEM private-key marker is public-cert material, not a
  credential — the ca-certificates package installs ~140
  `/etc/ssl/certs/*.pem` files at build time), `game-data` (game-image
  extensions + CISO/RARC/Yaz0/CD001 magic, catches dumps hidden under
  innocent names), `big-binary` (non-text ≥ 256 MiB; the largest
  legitimate file in the pinned base + pinned apt closure is
  libLLVM-15.so.1 at ~112 MiB) — run over **every file** of the merged
  filesystem. Verified clean on the crane-exported pinned base (3265
  files), the six pinned apt packages' file lists, and a crane-built
  layout of the pinned base with the app layer appended (2026-10-18).
- **Content rules** (home paths, gateway hostname/token) — run **only over
  the files our Dockerfile adds**: the paths the `COPY` places files at
  (parsed from `packaging/Dockerfile`, WORKDIR-aware) in the merged
  filesystem, plus the image config blob (labels/env/entrypoint). The base
  distribution never writes those paths, so a base or apt file can never
  be content-scanned — this restriction (not base-digest exclusion, not a
  hand-written Debian allowlist) is what stops the real false-positives:
  the pinned base is 100% clean, but the apt layer ships four Debian files
  with literal `/home/<name>/` strings (OpenAL/ALSA sample configs, xkb
  symbols), and those live in layers the product builds. An image with no
  file at a `COPY` destination in the merged filesystem **fails**
  (`image-no-owned-files`) instead of silently passing — the app binary is
  required, not optional.

**Annotations.** Under `GITHUB_ACTIONS=true` both scanner gates emit
workflow commands (escaping mirrors `scripts/gates/annotate-go.sh`):
`scan-artifacts.sh` — one `::error file=<path>,title=artifact-scan::<rule>
<artifact>:<path> [masked]` per finding plus a final summary (error on
fail, `::notice` on pass); `secret-scan.sh` — `::error` for real findings
(`title=secret-scan::<rule>`), `::notice` for allowlisted items, and a
final `::notice`/`::error` summary. The matched value is never printed;
job logs require auth, so the check-run annotations are the public
surface.

**NOT RUN semantics.** Without docker (or for a ref not built locally)
the image scan prints `NOT RUN: image scan …` and never fails the run —
the archive scan still runs; on the tagged-release job the image layers
are covered by the pinned base digest, the image SBOM attestation and
cosign, while the `edge` job scans the images directly (step 2).

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
2. **Previous binary/package:** reinstall the previous `.deb`. Snapshot
   versions are monotonic in the commit (section 9), so rolling back from a
   newer snapshot to an older one is a genuine version *downgrade* in dpkg's
   eyes; use `apt-get install --allow-downgrades ./game-narrator_<old>...deb`
   (apt keeps history for one prior version) or the previously recorded
   binary from the release evidence bundle. Upgrades between snapshot builds
   are ordinary version increases and need no special flags (section 3).
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

## 6. Device lifecycle (clean install / upgrade / rollback / purge)

`scripts/release/lifecycle-on-device.sh` drives the full M8 cycle on a
running UNO Q (docs/spec/16 "Test clean install/upgrade/rollback,
signed/checksummed artifacts") and is idempotent: re-running it from any
intermediate state — including a purged device — repeats the cycle from a
fresh pre-flight backup. It takes the ssh target and the two arm64 `.deb`
packages built from clean committed trees (section 1, different package
versions):

```sh
git worktree add --detach /tmp/gn-deb-<sha-prev> <prev-commit>
git worktree add --detach /tmp/gn-deb-<sha-cur>  <cur-commit>
(cd /tmp/gn-deb-<sha-prev> && goreleaser release --snapshot --clean)
(cd /tmp/gn-deb-<sha-cur>  && goreleaser release --snapshot --clean)
(cd /tmp/gn-deb-<sha-prev>/dist && sha256sum -c checksums.txt)
(cd /tmp/gn-deb-<sha-cur>/dist  && sha256sum -c checksums.txt)

scripts/release/lifecycle-on-device.sh <user>@<uno-q-host> \
  /tmp/gn-deb-<sha-prev>/dist/game-narrator_*-snapshot.*_linux_arm64.deb \
  /tmp/gn-deb-<sha-cur>/dist/game-narrator_*-snapshot.*_linux_arm64.deb
```

(With the monotonic snapshot versioning of section 9, `<sha-prev>` must be
the older commit: the lifecycle upgrades prev → cur, and the plain installs
assume prev ≤ cur by version.)

The script (a) backs up the data directory to
`/home/arduino/gn-data.bak-<ts>` and `/etc/game-narrator` to `/tmp/gn-lc`
(the env file is verified by mode, owner and line count only — never
printed or copied into logs), (b) installs the "previous" package with a
plain `apt-get install`, replacing whatever dirty or old build is installed
(with monotonic versions, an older build is an ordinary upgrade — only the
deliberate rollback in step (e) uses `--allow-downgrades`), (c) starts the
unit and verifies the
dpkg version, `/api/v1/health/ready`, `game-narrator doctor` and a test
game that must survive a service restart, (d) upgrades to "current" (data
and env preserved; the unit is stopped by `prerm` and started again per
section 3), (e) rolls back to "previous" per section 4 — recording which
policy applied (the previous binary starts on the newer data, or the DB
backup is restored first), (f) re-upgrades to "current", and (g) purges and
clean-installs "current", verifying that the data directory, the system
user and the env file survive the purge per section 5. Every step prints
PASS/FAIL/SKIP; the exit code is 0 only when every step passed and the
device is left running the "current" package.

The doctor gate is measured against a pre-lifecycle baseline: a check that
already FAILED before the lifecycle started (device/gateway state, e.g. a
TTS voice the instance has not been provisioned with) is not a packaging
regression, while any FAIL that appears after a transition is. When the
baseline is empty — including the fresh/purged state — every step requires
doctor 0 FAIL. The first run on the appliance (2026-10-18) left a
pre-existing `providers.voice_clone` FAIL in the then-installed dirty build
(`voice_missing` on a fresh instance); the clean build reports 0 FAIL.

2026-10-18 run (HEAD `f9a572e` over `398cad0`): 9/9 steps PASS twice
(including an idempotent re-run); no maintainer-script bug was found —
postinst/prerm/postrm behaved per section 5 on every transition, including
remove/purge.

## 7. Verification evidence

Each snapshot/release run should retain: `goreleaser` log, `dist/` listing +
`checksums.txt`, scanner output, smoke log (all steps + NOT RUN reasons),
SBOMs, image digests (`docker image inspect`), the device-lifecycle log
(section 6, PASS/FAIL per step + the backup paths), and for published
images the manifest digest plus the `cosign verify` output (sections
2.1/2.2/2.4).
`make test-full` output lists every gate and its NOT RUN
reason (hardware, replay, Dolphin, models — unverified on this host).

## 8. Arch Linux package (optional — ADR 0053)

The same pipeline also builds
`game-narrator_<version>_linux_{amd64,arm64}.pkg.tar.zst` (nfpm `archlinux`
format; one nfpms entry builds the .deb and the .pkg side by side). It reuses
the deb's unit and files, with these format-specific choices (ADR 0053):

- The shared nfpms `dependencies` list keeps the Debian package names for
  the .deb; the Arch format overrides it with the Arch names (`ffmpeg`,
  `tesseract`, `tesseract-data-eng`, `v4l-utils`, `ca-certificates` — all in
  core/extra), so `pacman` resolves them normally.
- The unit ships in `/usr/lib/systemd/system/` — on Arch `/lib` is a symlink
  owned by the `filesystem` package, so the deb's `/lib/...` entry cannot be
  packaged for Arch (pacman file conflict).
- The sysuser and the directory definitions are the declarative
  `/usr/lib/sysusers.d/game-narrator.conf` + `/usr/lib/tmpfiles.d/game-narrator.conf`
  (both formats ship these files; on the .deb they are inert without systemd
  and idempotent under systemd — deb behaviour is unchanged).
- Every snapshot build has `pkgver 0.0.0-1` in `.PKGINFO`: the nfpm core
  splits the version into upstream `0.0.0` + prerelease
  `snapshot.<commit-ts>.g<sha>`, and the arch packager keeps only the
  upstream part in `pkgver` (the prerelease is appended only when an epoch is
  set). goreleaser 2.14 reads `epoch`/`release`/`prerelease` only from the
  top-level nfpms entry and does **not** template them, so setting them would
  also change the tagged releases' versions (ADR 0060) — hence the wart is
  kept. The version *string* itself is vercmp-monotonic (section 9, verified
  with pacman's `vercmp` 7.1.0), so a future goreleaser with templated
  `prerelease` could fix `pacman -Qi` without reordering anything. Pacman
  re-installs an explicit local file at the same version ("up to date --
  reinstalling", verified 2026-10-18), so `pacman -U` upgrades work between
  any two snapshot builds; build provenance is the artifact name +
  `checksums.txt`, not `pacman -Qi`.

**Install:** `sudo pacman -U --noconfirm <pkg>` — pacman resolves the
declared Arch dependencies (`ffmpeg`, `tesseract`, `tesseract-data-eng`,
`v4l-utils`, `ca-certificates`) from core/extra, and the `post_install` hook
creates the `game-narrator` sysuser (from the shipped sysusers.d file via
`systemd-sysusers`, `useradd` fallback), the data/config directories with the
.deb's ownership/modes (`/var/lib/game-narrator` 0700 game-narrator,
`/etc/game-narrator` 0750 root:game-narrator, config.yaml 0640) and reloads
systemd.

**Upgrade:** `sudo pacman -U --noconfirm <new-pkg>` — `post_upgrade` stops
the unit (graceful drain) and re-runs the setup; the unit is left stopped
across the upgrade, so start it to pick up the new binary (section 3
contract).

**Remove:** `sudo pacman -Rns --noconfirm game-narrator` — `pre_remove`
stops the unit, `post_remove` disables it and reloads systemd. Pacman has no
purge verb: package files (including the config example) are removed on every
remove, and the admin's `/etc/game-narrator/env` survives only because a
non-empty directory cannot be deleted — keep it or `rm` it yourself.
`/var/lib/game-narrator` is NOT a package file and is never touched
(section 5); the sysuser is intentionally not removed (section 5).

**Verification:** real `pacman -U`/`-Rns` install smoke on the Arch amd64
dev host (2026-10-18, final build `bc9e690`, re-run **without** `--nodeps`):
dependency resolution (`resolving dependencies…` over the five Arch dep
names), unit file, sysuser, directory modes, `game-narrator version`,
same-version reinstall (build `2ef5bc5`) and remove cleanup all verified; the
service was never enabled or started on the dev host. arm64 is
cross-compiled only — NOT VERIFIED (same status as the .deb arm64).
A host-level defect was noted during testing: on that host
`systemd-tmpfiles` could not resolve any non-root user name ("Unknown user";
`root:root` resolves), so the hooks set directory ownership with
`install`/`chown` (libc NSS) instead of `systemd-tmpfiles` — see ADR 0053.

## 9. Snapshot version ordering (ADR 0060)

Snapshot versions are `0.0.0-snapshot.<commit-ts>.g<shortsha>`, where
`<commit-ts>` is the commit's Unix timestamp in UTC (GoReleaser's
`snapshot.version_template`, rendered with `.CommitTimestamp`) and
`<shortsha>` is the short commit hash. The format has three properties:

- **Monotonic.** dpkg (and Arch's `vercmp`) compare the digit run
  `<commit-ts>` numerically, so a later commit always sorts greater than an
  earlier one. The previous format `0.0.0-snapshot-<sha>` compared the bare
  short SHA *lexically* — `0cbebce` (newer) sorted below `f9a572e` (older),
  so on the UNO Q (Debian 13) `apt-get install ./new.deb` refused the
  upgrade as a downgrade, and the device lifecycle needed
  `--allow-downgrades` on its upgrade steps. That wart is gone: upgrades
  between snapshot builds are ordinary version increases.
- **Below every real tag.** The `.deb`'s control `Version` is re-derived by
  nfpm as `0.0.0~snapshot.<commit-ts>.g<shortsha>` (the `~` puts the
  prerelease below the empty prerelease of the same upstream version), and
  every real tag (`v0.0.1` and above) compares greater on the upstream part
  anyway. Verified:
  `dpkg --compare-versions 0.0.0~snapshot.1760745600.gf9a572e lt
  0.0.0~snapshot.1761106800.g0cbebce` (later commit greater) and
  `dpkg --compare-versions 0.0.0~snapshot.9999999999.gfffff99 lt 0.1.0`
  (a tagged release beats every snapshot).
- **Docker-tag safe.** The version string doubles as the `dockers_v2` image
  tag, and docker tags must match `[a-z0-9._-]` — a `~` separator would be
  an invalid tag, which is why the artifact-side format uses `-` and the
  below-tag ordering comes from nfpm's `~` instead. The `.g<shortsha>`
  suffix keeps the version traceable to its commit and breaks ties between
  commits in the same second.

The rollback step (section 4.2) is the *only* package transition that moves
from a higher to a lower version, so it — and only it — uses
`apt-get install --allow-downgrades`. The same strings compare correctly
under Arch's `vercmp` (pacman 7.1.0, `-1` = first is older), but the Arch
package's `pkgver` stays `0.0.0-1` (section 8): making it monotonic would
require `epoch`/`prerelease`, which goreleaser 2.14 applies to *all* formats
including the .deb of tagged releases.

## 10. The image's apt pins and their drift guard (ADR 0131)

The OCI image (section 1, `dockers_v2`) installs six exact Debian package
versions — `ca-certificates`, `ffmpeg`, `tesseract-ocr`, `tesseract-ocr-eng`,
`v4l-utils`, `wget` — on the digest-pinned `debian:bookworm-slim` base
(ADR 0012). The live Debian archive keeps only the *newest* version of each
package, so an upstream upload can silently remove a pinned version: on
2026-10-21 the security upload `ca-certificates 20250419~deb12u1 →
20250419~deb12u2` removed the pinned version from `deb.debian.org` and the
edge image build failed inside the apt stage (run 39530207238).

The durable fix (ADR 0131): the Dockerfile's apt stage resolves the pins
from **immutable snapshot.debian.org timestamps** matching the base
image's build date (2026-10-05), not from the live archive:

- `DEBIAN_SNAPSHOT_TS=20261005T023518Z` → suites `bookworm`,
  `bookworm-updates` (component `main`)
- `DEBIAN_SECURITY_SNAPSHOT_TS=20261005T022253Z` → suite
  `bookworm-security` (component `main`)

(The `debian` and `debian-security` mirrors are snapshotted on independent
schedules, so each has its own timestamp. The base image's own
`/etc/apt/sources.list.d/debian.sources` records the reference
`20261005T000000Z`; the two ARGs are the nearest *real* snapshots, from the
`https://snapshot.debian.org/archive/debian/?year=2026&month=10` index.)

Mechanics, as implemented in `packaging/Dockerfile`:

- The stock `debian.sources` is swapped, for one `RUN` layer only (stage
  `apt-pins`), for a file of the same deb822 shape pointing at the frozen
  snapshots, then restored at the end of that layer. The runtime image
  therefore keeps the stock apt configuration and carries no frozen
  pointer (docs/spec/15: the container is not an apt appliance).
- Integrity is the same model the stock image uses: the frozen
  `InRelease` is GPG-verified against the base image's own keyring
  (`Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg`), with
  `Check-Valid-Until: no` because a frozen release is *by design* past its
  short valid-until window. The sources use plain `http://` because the
  slim base ships no CA store and the first package installed *is*
  `ca-certificates` — the same chicken-and-egg the official image has with
  `deb.debian.org`.
- The six pinned versions are **unchanged**: each was verified present in
  exactly these snapshots for both `linux/amd64` and `linux/arm64`, so the
  image content is byte-identical to what the pins were validated against.
- The final stage continues with `FROM apt-pins` plus the binary `COPY` /
  user / healthcheck (unchanged); `docker buildx build --target apt-pins`
  builds only the apt layer (the stage copies nothing from the build
  context), which makes it a cheap, self-contained verification target.

**Drift guard.** `make verify-apt-pins` (`scripts/release/verify-apt-pins.sh`)
uses `packaging/Dockerfile` as the single source of truth (it parses the
`BASE_IMAGE` and the two snapshot ARGs plus the `pkg=version` pins) and
verifies in two layers:

1. **Index check** (always; network, no docker needed): each pin must exist
   in the frozen snapshot index for **both** `linux/amd64` and
   `linux/arm64`, over the union of the enabled suites. On drift it prints
   the version(s) that *are* available and the fix recipe, then fails.
2. **Stage build** (when docker is usable): `docker buildx build
   --target apt-pins` — the real `apt-get update` + exact-version install,
   GPG-verified — for `linux/amd64`, and the same for `linux/arm64` under
   binfmt emulation (otherwise reported NOT RUN, never pass). It builds
   against an **empty context dir** (the apt stage references no context
   files), and a grep over the plain-progress log fails the check on apt
   `W:`/`E:` lines (GPG/index trouble) that BuildKit would otherwise
   swallow as success.

The guard is wired into `make release-snapshot` (first step) and both jobs
of `.github/workflows/publish.yml` (after the QEMU/BuildKit setup, before
any GoReleaser work), so a Debian version update fails *there* with an
actionable message instead of mid-image-build.

**When the guard fails (fix recipe):**

- **A pin does not resolve in the frozen snapshots** (e.g. you updated a
  pin to a newer version that the frozen timestamp predates): update the
  pin in `packaging/Dockerfile` to a version the guard lists as available,
  verifying it for **both** architectures, then re-run the guard. The six
  version pins change only when tool feature/doctor requirements demand it
  (ADR 0012's CLI-drift note).
- **A snapshot index cannot be fetched** (the pinned timestamp no longer
  serves a suite/arch, or it was never a real snapshot): pick a real
  snapshot of the base image's build date from
  `https://snapshot.debian.org/archive/debian/?year=<yyyy>&month=<mm>` (and
  `…/archive/debian-security/?year=<yyyy>&month=<mm>`), update the ARG, and
  re-run the guard.
- **The base image digest is updated** (rebuild cadence, ADR 0012): update
  *both* snapshot ARGs to the nearest real snapshots of the new build date,
  re-run the guard, and keep the version pins unless the guard says
  otherwise. This is the whole cadence: two ARGs plus a guard run.

The known trade-offs (snapshot-host dependency, plain-`http` transport, the
frozen sources lagging upstream security fixes by design) and their
mitigations are recorded in ADR 0131; they are accepted, not deferred.
