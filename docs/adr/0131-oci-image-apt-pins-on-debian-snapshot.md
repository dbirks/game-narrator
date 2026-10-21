# ADR-0131: The OCI image's apt resolves exact pins from snapshot.debian.org timestamps matching the base image

Date: 2026-10-21
Status: accepted
Owners: agent (release-pins task)
Related requirements/tests: AGENTS.md (rule 12 gate discipline; "CLI targets" — `make release-snapshot`), docs/spec/15 ("OCI image design": pinned distro packages; "Release gate matrix": `release-snapshot` validates the OCI image), ADR-0006 (GoReleaser), ADR-0012 Part 2 (thin Debian OCI base — "pinned to digest/epoch versions recorded in the Dockerfile"), ADR-0033 (snapshot pipeline; scanner scope: distribution layers covered by pinned base + SBOM), ADR-0048 (publish pipeline).
Tests: `scripts/release/verify-apt-pins.sh` (new guard), `make verify-apt-pins`, `make release-snapshot`, `.github/workflows/publish.yml` (guard step in the edge and release jobs), build evidence below.

## Context

`packaging/Dockerfile` pins six exact Debian package versions —
`ca-certificates=20250419~deb12u1`, `ffmpeg=7:5.1.9-0+deb12u1`,
`tesseract-ocr=5.3.0-2`, `tesseract-ocr-eng=1:4.1.0-2`,
`v4l-utils=1.22.1-5+b2`, `wget=1.21.3-1+deb12u1` — on top of a
digest-pinned base (`debian@sha256:7c7b2c96…`, the `debian:bookworm-slim`
index rebuilt 2026-10-05), per ADR-0012.

The edge publish (GitHub run 39530207238, 2026-10-21) failed inside the apt
stage of the image build:

```
E: Version '20250419~deb12u1' for 'ca-certificates' was not found
ERROR: failed to build: ... apt-get install ... exit code: 100
```

Root cause: the live Debian archive keeps only the newest version of each
package. The `bookworm-security` upload
`ca-certificates 20250419~deb12u1 → 20250419~deb12u2` (verified: the live
`bookworm-security` index now lists only `~deb12u2`) silently removed the
pinned version from `deb.debian.org`. Exact-version pins against a moving
source are therefore only reproducible *until the next upstream update* —
ADR-0012's "versions verified against bookworm" held at verification time
(2026-07-20) and silently stopped holding.

Two facts make a durable fix straightforward:

1. **The base image records its own apt state.** The base's
   `/etc/apt/sources.list.d/debian.sources` (deb822, two stanzas) carries the
   debuerreotype build references
   `# http://snapshot.debian.org/archive/debian/20261005T000000Z` and
   `# http://snapshot.debian.org/archive/debian-security/20261005T000000Z` —
   i.e. the exact archive state the image was built from, as a snapshot
   date.
2. **snapshot.debian.org** (Debian's official immutable mirror archive)
   serves every historical state of the `debian` and `debian-security`
   mirrors by timestamp, forever. The two mirrors are snapshotted on
   independent schedules, so each needs its own timestamp.

## Decision

1. **The apt stage of `packaging/Dockerfile` resolves the exact pins from
   frozen snapshot sources instead of the live archive.** The stock
   `/etc/apt/sources.list.d/debian.sources` is swapped, for one RUN layer
   only, for a file of the same deb822 shape pointing at:
   - `http://snapshot.debian.org/archive/debian/20261005T023518Z`
     (suites `bookworm bookworm-updates`, component `main`)
   - `http://snapshot.debian.org/archive/debian-security/20261005T022253Z`
     (suite `bookworm-security`, component `main`)

   The timestamps are the real snapshots closest to the base image's build
   date 2026-10-05 (the base's `T000000Z` references are rounded and are not
   real snapshots; 02:35:18Z / 02:22:53Z are the nearest real ones, from the
   `https://snapshot.debian.org/archive/debian/?year=2026&month=10` index).
   The six pinned versions are **unchanged**: each was verified present in
   exactly these snapshots for **both** `linux/amd64` and `linux/arm64`
   (per-arch `Packages` index check below), so the image content is
   byte-identical to what the pins were validated against.

2. **Integrity model.** The frozen `InRelease`/`Release` metadata is still
   GPG-verified: the stanzas keep `Signed-By:
   /usr/share/keyrings/debian-archive-keyring.gpg` (the base image's own
   archive keyring) and add `Check-Valid-Until: no` — a frozen InRelease is
   by design past its short valid-until window, and that (and only that)
   check is waived. The sources use plain `http://` (not https): the slim
   base image ships **no CA store**, and the first package we install *is*
   `ca-certificates` (chicken-and-egg); the stock debian image fetches
   `deb.debian.org` over plain http for the same reason, so this is the
   same trust model the official image already uses (transport + GPG-signed
   metadata), not a new one. `snapshot.debian.org` is operated by the
   Debian infrastructure team — the same organization whose keys sign the
   metadata.

3. **The swap is contained in one RUN layer and the stock sources file is
   restored at its end**; `apt-get clean` + `rm -rf /var/lib/apt/lists/*`
   as before. The runtime image therefore keeps the stock apt
   configuration and carries no frozen pointer (docs/spec/15: the container
   is not an apt appliance).

4. **The apt stage is named `apt-pins`; the final stage continues with
   `FROM apt-pins`** (plus the binary COPY / user / healthcheck, unchanged).
   A full build produces the same image content as the previous single-stage
   layout; `docker buildx build --target apt-pins` builds only the apt stage
   and copies nothing from the build context, which makes it a cheap,
   self-contained verification target.

5. **A CI-checkable drift guard, `scripts/release/verify-apt-pins.sh`, wired
   into `make release-snapshot` (first step) and both jobs of
   `.github/workflows/publish.yml` (after the QEMU/BuildKit setup, before
   any GoReleaser work).** It parses `packaging/Dockerfile` (the single
   source of truth: `BASE_IMAGE`, the two snapshot ARGs, the `pkg=version`
   pins) and verifies in two layers:
   - **index check** (always; network, no docker): for every pin and for
     **both** `linux/amd64` and `linux/arm64`, the exact version exists in
     the frozen snapshot index, union of the enabled suites
     (`bookworm`, `bookworm-updates`, `bookworm-security`, component
     `main`). On drift it prints the version(s) that *are* available and
     the fix recipe, then fails.
   - **stage build** (when docker is usable): `docker buildx build
     --target apt-pins` for `linux/amd64` — the real `apt-get update` +
     exact-version install, GPG-verified — and the same for `linux/arm64`
     when binfmt emulation is present (otherwise reported NOT RUN, never as
     pass). It builds against an **empty build context** (the apt stage
     references no context files), so it is fast and immune to local
     `dist/`/`artifacts/` noise. A grep over the plain-progress log fails
     the check on apt `W:`/`E:` lines (GPG/index trouble) that BuildKit
     would otherwise swallow as success.

6. **Repin cadence (what "durable" means).** When the base image digest is
   updated, the two snapshot ARGs are updated to the nearest real snapshots
   of the new build date and the guard is re-run; the six version pins
   change only when tool feature/doctor requirements demand it (ADR-0012's
   CLI-drift note), and then are verified per-arch against the frozen
   snapshots the same way. The guard makes the previously silent failure
   mode — an upstream upload removing a pinned version — loud and
   actionable on every release path instead of mid-image-build.

## Options considered

1. **Bump the pins to the current live versions, keep live sources
   (fastest unblock).** Rejected: the identical failure recurs at the next
   upstream update (already proven within one week of the last re-pin); the
   image content then becomes a function of build time, defeating ADR-0012's
   "pinned to digest/epoch versions" contract.
2. **Keep exact pins, keep live sources, but retry with the newest
   candidate on failure.** Rejected: same moving-target problem, plus
   non-deterministic "what did we actually install" between runs.
3. **Drop exact versions; pin sources to the snapshot and install
   snapshot-latest.** Rejected: strictly weaker than the status quo (the
   current pins resolve from the snapshots unchanged — no reason to give up
   exact content).
4. **Vendor the snapshot `InRelease`/`Release` + `.deb`s into the repo as an
   offline apt mirror.** Rejected: the `ffmpeg` dependency tree is tens of
   MB per arch (copyrighted redistribution of distribution packages in the
   repo), a second source of truth, and a build-time verification step —
   the snapshot URL is already the pin.
5. **Build a second pinned "base+tools" image on a cadence and digest-pin
   that instead.** Deferred: adds a second registry repository plus its own
   publish/sign flow; revisit if the package set grows or the cadence
   burden grows. Recorded here so the choice is visible.

## Evidence (validation)

- **Failure reproduced from the run log** (run 39530207238, edge job,
  GoReleaser step): `E: Version '20250419~deb12u1' for 'ca-certificates'
  was not found` … `exit code: 100`, image build failed before any push.
- **Root cause confirmed live:** `bookworm-security` index at snapshot
  `20261020T213255Z` (nearest to the failure) lists
  `ca-certificates 20250419~deb12u2` only — the pinned `~deb12u1` is gone
  from the live archive.
- **Timestamp choice:** the base manifest
  (`sha256:7c7b2c96…`, Docker Hub `library/debian`) annotates
  `org.opencontainers.image.version: bookworm-slim`,
  `org.opencontainers.image.created: 2026-10-05`; the base image's own
  sources file references `20261005T000000Z` for both mirrors; the nearest
  real snapshots are `20261005T023518Z` (debian) and `20261005T022253Z`
  (debian-security).
- **Pins resolve from the frozen snapshots, both arches:** per-arch
  `Packages` index check (bookworm + bookworm-updates @ `20261005T023518Z`,
  bookworm-security @ `20261005T022253Z`, `binary-amd64` + `binary-arm64`)
  shows every pinned version present:
  `ca-certificates 20250419~deb12u1` (bookworm-security), `ffmpeg
  7:5.1.9-0+deb12u1`, `tesseract-ocr 5.3.0-2`, `tesseract-ocr-eng
  1:4.1.0-2`, `v4l-utils 1.22.1-5+b2`, `wget 1.21.3-1+deb12u1` (bookworm) —
  identical versions on amd64 and arm64.
- **Real apt run inside the pinned base image** (`docker run
  --platform linux/amd64 debian@sha256:7c7b2c96…` with the frozen sources):
  `apt-get update` exit 0 with **zero** `W:`/`E:` lines (InRelease
  GPG-verified via the base keyring; `Check-Valid-Until: no` as designed),
  exact-version install exit 0; `dpkg-query` reports all six versions
  verbatim. Same run under QEMU for `linux/arm64`: all six install,
  `APT-STAGE-OK platform=linux/arm64`.
- **Guard** (`scripts/release/verify-apt-pins.sh`): full pass — 12/12
  per-arch pin checks OK, `apt-pins` stage build OK for `linux/amd64` and
  (binfmt present) `linux/arm64`: "verify-apt-pins: all checks passed".
- **Full image builds** (this change's Dockerfile, buildx): recorded in the
  task report / `artifacts/` — `apt-pins` stage + final stage for
  `linux/amd64` and `linux/arm64`.
- **Second latent gate defect, found and fixed during local verification
  (same change):** `make release-snapshot`'s artifact scanner
  (`scripts/release/scan-artifacts.sh`) could not parse docker 29.9.1's
  `docker save` layout — that docker writes a *nested* OCI index
  (`index.json` → nested index → [image manifest, attestation manifest
  annotated `vnd.docker.reference.type: attestation-manifest`]), while
  `_save_layout_parse` understood only single-level indexes. Every image
  scan on this host failed with "layout not recognized (neither index.json
  nor manifest.json)" (reproduced on both per-arch snapshot images, and on
  any image — content-independent). Fix: the parser now walks nested
  indexes (bounded BFS with a seen-set) to the first non-attestation image
  manifest; attestation manifests are skipped and *not scanned* — they are
  build provenance, neither distribution layers nor app content (the
  scanner's documented scope, docs/spec/15). Self-test extended with a
  nested-layout fixture: a clean image must pass, and the fixture's
  attestation layer carries a planted home path that must NOT be flagged
  (pins the exclusion semantics). Evidence: parser resolves the real
  docker-29 save (6 image layers, attestation excluded); `--self-test`
  5/5 PASS including the new `clean-nested:nested-oci` case; full
  `make release-snapshot` re-run recorded in
  `artifacts/release-snapshot-2026-10-21-apt-pins.log`.

## Consequences and risks

Positive:

- The image build is reproducible across upstream Debian updates: digest-
  pinned base + frozen (immutable) sources + exact versions. The failure
  mode of this incident — an upstream upload silently removing a pinned
  version — cannot recur silently: the guard detects it on `make
  release-snapshot` and on both publish jobs, before any release work, with
  the available version(s) and the fix recipe printed.
- The ADR-0012 "rebuild cadence" is now concrete and cheap: two ARGs plus a
  guard run.
- `--target apt-pins` makes the distribution layer cheaply verifiable in
  isolation (no binary staging, empty context).

Negative / risks (accepted, mitigations noted):

- **Build now depends on snapshot.debian.org availability** (Debian
  infrastructure). If it is down or a snapshot is pruned, release image
  builds fail. Same failure class as depending on Docker Hub for the base
  image (already true); the guard fails early and says so.
- **http transport to the snapshot host:** integrity rests on (a) the
  official snapshot host, (b) GPG-signed `InRelease`/`Release` verified
  against the base image's keyring, (c) the digest-pinned base image, (d)
  the release pipeline's image scan + keyless cosign signature + SBOM
  attestation (ADR-0048). A MITM that can rewrite the indices would defeat
  (a); that threat is out of scope for an internal release pipeline and is
  the same exposure the stock debian image has with `deb.debian.org`.
  Recorded so a future https upgrade (e.g. vendoring a CA bundle into the
  apt stage) is a conscious ADR change, not an accident.
- **Frozen sources lag upstream security fixes by definition.** That is the
  stated trade for reproducibility; the mitigation is the repin cadence
  (decision 6) and the image SBOM/vuln-scan gate (docs/spec/15). The
  alternative — live sources — is exactly what broke the build.
- **Snapshot timestamps must be real snapshots** (the archive has no
  arbitrary-hour resolution). The guard's index check fails with the fix
  recipe if a wrong timestamp is pinned.

## Rollback / migration

- **Rollback:** restore the previous `packaging/Dockerfile` (single-stage,
  live sources) — one file. No data, API, or artifact-format change; the
  guard script and workflow step can stay in place (they parse the ARGs;
  with live sources the guard would need the ARGs removed or the Dockerfile
  restored — the ADR link makes the coupling explicit).
- **Migration:** none — the image content is identical (same base digest,
  same six package versions, same file layout); only the build-time
  resolution path and the stage topology changed.

## Links / references

- Failure run: `gh run view 39530207238 --log-failed` (edge job, 2026-10-21).
- `packaging/Dockerfile` (frozen sources + `apt-pins` stage),
  `scripts/release/verify-apt-pins.sh` (guard), `Makefile`
  (`verify-apt-pins`, wired into `release-snapshot`),
  `.github/workflows/publish.yml` (guard step in edge + release jobs),
  docs/release.md (§ "The image's apt pins and their drift guard").
- snapshot.debian.org: `https://snapshot.debian.org/`, index
  `https://snapshot.debian.org/archive/debian/?year=2026&month=10`.
- ADR-0012 (OCI base, pinned versions), ADR-0048 (publish gates, cosign),
  docs/spec/15 (OCI image design, release gate matrix).

Supersedes / superseded by: — (extends ADR-0012 Part 2; does not supersede
it).
