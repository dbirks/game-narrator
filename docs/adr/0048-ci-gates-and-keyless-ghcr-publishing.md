# ADR-0048: CI gates on GitHub Actions and keyless GHCR publishing

Date: 2026-10-17
Status: accepted
Owners: agent (ci-publish task)
Related requirements/tests: AGENTS.md (rules 3, 5, 8, 12; "CLI targets";
"Minimum quality policy"), docs/spec/15 (release gate matrix, SBOM, supply
chain), docs/spec/16 (implementation and acceptance gates), ADR-0033
(packaging and the local snapshot pipeline), docs/release.md (runbook,
rewritten by this ADR).
Tests: `.github/workflows/ci.yml` (lint, test-unit -race, web checks,
test-replay), `.github/workflows/publish.yml` (edge + tag release, cosign
self-verify), `goreleaser check`, `actionlint`, `make release-snapshot`
(unchanged local gate).

## Context and constraints

- The user approved (2026-10-17) publishing images to GHCR
  (`ghcr.io/dbirks/game-narrator`) and signing with cosign. The dev host has
  no GHCR credentials, so publishing must run in GitHub Actions with the
  repo's `GITHUB_TOKEN` (`packages: write`) and cosign **keyless** via GitHub
  OIDC (`id-token: write`) — no private keys and no external credentials may
  enter the repo (AGENTS.md rules 5, 6).
- Before this ADR the repo had no `.github/` at all; `release.disable: true`
  was committed so nothing could publish, and docs/release.md required a
  manual, approval-gated tagged release.
- GoReleaser 2.14 verified facts (source inspection, `v2.14.0` tag):
  `--snapshot` implies `--skip=announce,publish,validate` (snapshot runs can
  never push or sign); the default snapshot version template
  `{{ .Version }}-SNAPSHOT-{{ .ShortCommit }}` is **invalid as a docker tag**
  (uppercase `SNAPSHOT`; docker tags match `[a-z0-9._-]`) — a latent break in
  the first image build with docker available; `release.disable` and
  `release.prerelease` are template strings; `docker_signs` runs only in the
  publish phase, signs `DockerImage`/`DockerManifest` artifacts, and its
  default cosign args carry `--key=cosign.key` (must be overridden for
  keyless).
- `web/package.json` has no `packageManager` field, so the CI pnpm version
  is pinned in the workflow to the dev-host version (12.14.0); the lockfile
  is `web/pnpm-lock.yaml` (lockfileVersion 9.0).

## Decision

1. **CI (`.github/workflows/ci.yml`)**: on push/PR to `main` — Go 1.27
   (`go-version-file: go.mod`), Node 26, pnpm 12.14.0 (Go and pnpm caches
   enabled), apt `tesseract-ocr tesseract-ocr-eng ffmpeg imagemagick`, then
   `make lint`, `make test-unit` (race), the web checks
   (`pnpm --dir web check:api typecheck lint fmt:check test`) and
   `make test-replay`. Live-model, Dolphin, hardware and soak gates are
   deliberately NOT in CI (private resources); they report NOT RUN on the
   dev host (AGENTS.md rule 3: no simulation theater).
2. **Publishing (`.github/workflows/publish.yml`)**, repo `GITHUB_TOKEN`
   only, all actions pinned to full commit SHAs with a version comment
   (resolved 2026-07-20 via the GitHub API `GET /repos/<owner>/<repo>/tags`):
   - **edge**: `workflow_run` on `ci` completion `success` for pushes to
     `main` → GoReleaser **snapshot** with
     `GN_DOCKER_REPO=ghcr.io/dbirks/game-narrator` (per-arch images built
     locally; snapshot never pushes) → `scripts/release/scan-artifacts.sh`
     over dist **and both per-arch image layers** (a failure pushes nothing)
     → push the per-arch images → create the multi-arch manifest tags
     `edge` and `sha-<short>` registry-side with
     `docker buildx imagetools create` (a manifest list can only reference
     images already present in a registry — `docker manifest create`/`push`
     on local-only per-arch images fails, which is why the pre-push local
     manifest step was removed; run 39087325043, commit f3da74f) →
     `cosign sign --yes ghcr.io/...@<digest>` (digest resolved via
     `docker buildx imagetools inspect --format '{{.Manifest.Digest}}'`;
     see amendment 2026-10-18) +
     `cosign attest --type spdxjson` (predicate: the goreleaser/syft SBOM of
     the amd64 archive) → `cosign verify` self-check against the publish.yml
     certificate identity.
   - **tag `v*`**: GoReleaser `release --clean` with `GN_DOCKER_REPO` set —
     archives/.debs/SBOMs, GitHub Release (**prerelease** for `v0.*` via
     `release.prerelease: "{{ if hasPrefix .Tag \"v0.\" }}enable{{ end }}"`),
     multi-arch image push with the goreleaser SBOM attestation, keyless
     cosign signatures on images **and** manifest via new `docker_signs`
     (args override drops `--key` so cosign runs keyless with the Actions
     OIDC identity), post-build archive scan, signature self-check.
   - **Local non-publishability is structural**: snapshot ⇒ publish skipped;
     `release.disable: "{{ if not (isEnvSet \"GITHUB_ACTIONS\") }}true{{ end }}"`
     blocks a local tagged release from creating a GitHub Release;
     `dockers_v2` pushes only when `GN_DOCKER_REPO` is set; `docker_signs`
     runs only in the publish phase. `make release-snapshot` is unchanged and
     still pushes/signs nothing.
3. **`snapshot.version_template: "{{ .Version }}-snapshot-{{ .ShortCommit }}"`**
   in `.goreleaser.yaml`: lowercase so snapshot image tags are valid docker
   tags; `scripts/release/smoke.sh` recovers the version from artifact names,
   so the local pipeline stays consistent.
4. **Docs/verification**: docs/release.md rewritten around the approved
   flow; verification command documented
   (`cosign verify ghcr.io/dbirks/game-narrator:edge
   --certificate-identity-regexp 'https://github.com/dbirks/game-narrator/.github/workflows/publish.yml@.*'
   --certificate-oidc-issuer https://token.actions.githubusercontent.com`).
   UNO Q remains **NOT VERIFIED** until `make test-hardware` passes
   (AGENTS.md rule 12) — publishing does not change hardware evidence.

## Tradeoffs

- **Keyless over a committed signing key**: signatures verify against a
  workflow identity + OIDC issuer rather than a long-lived key. A compromise
  of the repo/Actions can produce a valid-looking signature until the
  workflow is changed; a committed key would survive workflow tampering but
  would need secret management on the keyless-free dev host — rejected:
  the user approved keyless and no host credentials exist.
- **`edge` via snapshot, not a real release**: edge gets no GitHub Release
  entry and its SBOM attestation predicate is the archive SBOM (Go binary),
  while tag releases get goreleaser's image SBOM attestation. Accepted:
  edge is a moving head for testing; immutable, attested releases are the
  tags.
- **Single CI job**: sequential gates (no parallel fan-out) keep the
  workflow trivially auditable at the cost of a few minutes of runtime.
- **Tagged images are pushed by goreleaser directly** (buildx `--push`), so
  the scanner cannot inspect tag-release image layers pre-push; mitigation:
  pinned base digest, goreleaser image SBOM attestation, post-build archive
  scan, and signature self-check. Edge is fully scanned pre-push.

## Validation

- `goreleaser check` passes with the new `snapshot`/`release`/`docker_signs`
  blocks (goreleaser 2.14.0).
- `actionlint` (run via `go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12`)
  passes on both workflows.
- `make release-snapshot` runs the unchanged local pipeline and still pushes
  and signs nothing (captured branch message: "docker not usable —
  dockers_v2 stays disabled"; snapshot ⇒ publish skipped). It currently exits
  at the artifact scanner because of a **pre-existing false positive**, not
  this change: commit 99537eb (support bundle, ADR-0042) added
  `internal/support/redact.go`, whose compiled regex literal contains the
  substring `/home/`, which the scanner's fixed-string content check
  (`CONTENT_STRINGS=('/home/')`) flags in every archive/.deb. Fixing it
  requires touching `scripts/release/scan-artifacts.sh` or
  `internal/support/redact.go` — outside this task's file ownership.
  (The shared worktree also transiently broke Go compilation mid-run from a
  concurrent task's in-flight edits; `go build ./...` passes once it
  settles.)
- Local `goreleaser release --snapshot` cannot sign/push (publish phase
  skipped); local `goreleaser release` cannot create a Release
  (`release.disable` template ⇒ true without `GITHUB_ACTIONS`).
- Not run on this host (no GHCR credentials, no Actions runner): the actual
  edge/tag publish, cosign signature creation and registry round-trip —
  first real evidence comes from the first CI publish run.

## Consequences and reversal

- Consequences: every `main` push produces a signed, scanned
  `ghcr.io/dbirks/game-narrator:edge` + `sha-<short>`; every `v*` tag
  produces a signed image + GitHub Release (prerelease for `v0.*`); release
  reports keep the NOT VERIFIED hardware statement; `docs/release.md`
  section 2 is now the authoritative publishing runbook.
- Reversal: delete `docker_signs` and restore `release.disable: true`
  (reviewed change) to stop all publishing; delete `publish.yml` to stop
  edge; delete `ci.yml` to stop CI. No data or schema is involved; the
  `snapshot.version_template` rename is cosmetic (artifacts, not state).

## Amendment 2026-10-18: digest extraction for the edge sign step

The first publish run (4b2c074) pushed the per-arch images and the
`edge`/`sha-<short>` index, then aborted in the sign step: the digest was
resolved with `docker buildx imagetools inspect --format
'{{json .Manifest}}' | python3 …["Digest"]`, but for a manifest list
`{{json .Manifest}}` emits the index's full JSON with **lowercase** keys
(`digest`, `mediaType`, `size`) — verified live against
`ghcr.io/dbirks/game-narrator:edge` with buildx 0.38.0 (2026-10-18) — so
the python lookup raised `KeyError` and `set -euo pipefail` exited before
`cosign sign` ran (no `.sig` tag in the registry).

Fix (smallest, in `publish.yml` only):
`DIGEST="$(docker buildx imagetools inspect --format
'{{.Manifest.Digest}}' "${REPO}:edge")"` — the template field prints exactly
`sha256:ba530a63…` (the index digest), no python needed. The existing
`sha256:` prefix check is kept; it also fails loudly if a buildx version
ever ignores non-`json` templates (docker/buildx#1175, pre-2023 behaviour).

Audit of the rest of the never-exercised sign/verify steps (same run
scope): the SBOM glob `dist/game-narrator_*_amd64.sbom.spdx.json` matches
`.goreleaser.yaml` `sboms.documents` for the snapshot version
(`game-narrator_0.0.0-snapshot-<sha7>_amd64.sbom.spdx.json`, ~16 KiB — no
predicate size concern); `cosign attest --type spdxjson` is a **built-in**
predicate type in the pinned cosign v3.0.6 (source, v3.0.6 tag: the file is
JSON-decoded and embedded as an in-toto `Statement` predicate with
`predicateType` from the spdxjson→SPDX mapping; invalid JSON errors
loudly), and `cosign verify-attestation --type spdxjson` resolves the same
mapping, so attest and verify agree. The verify step's
`--certificate-identity-regexp 'https://github.com/dbirks/game-narrator/.github/workflows/publish.yml@.*'`
matches the GitHub Actions OIDC identity documented by Sigstore
(`https://github.com/OWNER/REPO/.github/workflows/FILE@<ref>` with issuer
`https://token.actions.githubusercontent.com`): the `workflow_run` edge flow
issues `@refs/heads/main`, tag releases `@refs/tags/v*` — both match `@.*`.
No change to action/version pins, `docs/release.md` (it documents the
command-free flow and stays accurate), or the tag-release job.
