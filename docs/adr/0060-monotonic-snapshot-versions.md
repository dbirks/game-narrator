# ADR-0060: Monotonic snapshot package versions (commit timestamp, not bare SHA)

Date: 2026-10-18
Status: accepted
Owners: agent worker
Related requirements/tests: docs/spec/15 ("OS packages", "Release gate matrix"), docs/spec/16 (M8 lifecycle: clean install/upgrade/rollback), docs/release.md sections 1, 3, 4, 6, 8, 9, ADR 0033 (packaging), ADR 0048 (publishing gates; snapshot runs never publish), ADR 0053 (Arch package and its `pkgver 0.0.0-1` wart), `scripts/release/lifecycle-on-device.sh` / `lifecycle-remote.sh` (device lifecycle driver)

## Context and constraints

The GoReleaser snapshot version template was `{{ .Version }}-snapshot-{{ .ShortCommit }}`, producing `0.0.0-snapshot-<sha7>` in artifact names. The `.deb`'s control `Version` is re-derived by nfpm (which splits the version into upstream `0.0.0` + prerelease `snapshot-<sha7>` and re-joins it with a tilde) as `0.0.0~snapshot-<sha7>`.

dpkg compares version parts with its numeric/alphabetic algorithm: equal upstream parts (`0.0.0`) fall through to the prerelease, which is compared character classes — digits numerically, non-digits lexically. In `0.0.0~snapshot-<sha>` the SHA is a pure non-digit run, so two snapshots compared their **short SHAs lexicographically**: `0cbebce` sorted *below* `f9a572e`. On the UNO Q (Debian 13) this made the refresh `f9a572e → 0cbebce` a version *downgrade* in apt's eyes: `apt-get install ./new.deb` refused the upgrade, and the device lifecycle (`scripts/release/lifecycle-on-device.sh`) needed `--allow-downgrades` on its upgrade steps to proceed. A package manager that treats a newer build as a downgrade defeats the upgrade path, breaks one-shot `apt-get install` for operators, and made rollback the only transition the flag was "supposed" to cover — while in practice the clean-install and upgrade steps needed it too.

Constraints:

- The version string doubles as the **docker tag**: `dockers_v2.tags` is `{{ .Version }}` (ADR 0048 builds and, in CI, pushes per-arch images under it), and docker tags must match `[A-Za-z0-9_][A-Za-z0-9._-]{0,127}` — a `~` (the naive "sort before the tag" separator) is an invalid tag character.
- Every real tag (`v0.0.1` and above; `0.0.0` is the reserved dev base, never tagged) must compare greater than every snapshot, in both dpkg and vercmp.
- The version must stay **traceable to its commit** (artifact names, checksums, `artifacts.json`, and the developer UI all correlate by version/SHA).
- Tagged-release versions must not change (a static `epoch`/`release`/`prerelease` on the shared `nfpms` entry would change the .deb of a tagged release — goreleaser 2.14 reads those fields only from the top-level entry, per-format overrides are ignored, and none of the three is template-rendered).
- The `.deb`'s behaviour and the Arch `.pkg`'s file layout stay unchanged (ADR 0053).

## Decision

Snapshot versions become `0.0.0-snapshot.<commit-ts>.g<shortsha>`:

```yaml
snapshot:
  version_template: "{{ .Version }}-snapshot.{{ .CommitTimestamp }}.g{{ .ShortCommit }}"
```

- `<commit-ts>` is the commit's Unix timestamp in UTC (GoReleaser's `.CommitTimestamp` template field — the commit date, not the build time, so a given commit always produces the same version, on any machine, on any worktree).
- The digit run `<commit-ts>` is the first differing part between two snapshots and is compared **numerically** by dpkg and by Arch's `vercmp`, so a later commit always sorts greater than an earlier one. The `.g<shortsha>` suffix keeps the version traceable to its commit and breaks ties between commits that land in the same second (their relative order is then arbitrary but stable).
- The `.deb`'s control `Version` becomes `0.0.0~snapshot.<commit-ts>.g<sha>` (nfpm's re-derivation inserts the `~`), so snapshots keep sorting below the same upstream version; and any real tag's upstream part (`0.0.1+`, `0.1.0`, …) compares greater on the numeric upstream components regardless of the prerelease. Both were verified with `dpkg --compare-versions` (see Verification).
- The lifecycle script's upgrade transitions use a plain `apt-get install` (no `--allow-downgrades`); **only the deliberate rollback step** (newer snapshot → older snapshot, now a genuine version decrease) keeps the flag. If a device happens to run a build newer than the chosen "previous" package, the plain install's refusal is the correct outcome — the prev/cur pair is invalid for that run.
- The Arch package keeps `pkgver 0.0.0-1` (ADR 0053's wart, now understood at the nfpm level: the core splits the version, the arch packager's `pkgver` is the upstream part only, and the prerelease is appended only when an epoch is set — and an epoch/release/prerelease set here is static, applies to all formats, and would change tagged-release versions, which is off-limits). The version *string* itself is vercmp-monotonic (verified below), so the wart is cosmetic: `pacman -U` of an explicit local file reinstalls at the same version (verified 2026-10-18), and provenance stays the artifact name + `checksums.txt`.

## Options considered

- **Build timestamp instead of commit timestamp (`.Timestamp`).** Monotonic per build, but non-deterministic per commit: rebuilding the same commit (a routine re-run of the evidence pipeline) produces a new version, and two builds of the same commit would then differ only in the SHA — reintroducing a lexical tiebreak for the exact same-second case. Rejected.
- **Commit count (`git rev-list --count HEAD`).** Monotonic and short, but history-dependent (a rebased/forked commit gets a different count), not traceable to time, and requires exporting the count into the build environment — the commit timestamp is a first-class GoReleaser template field. Rejected.
- **`~` separator on the artifact side (`0.0.0~snapshot.<ts>.g<sha>`).** The most "Debian-correct" prerelease form, but `~` is invalid in a docker tag and the version doubles as the tag — it would break local `make release-snapshot` image builds (docker group) and the CI edge job. Unnecessary anyway: nfpm already inserts the `~` into the .deb's control `Version`, and real tags sort above the snapshots regardless. Rejected.
- **Set a top-level `nfpms.epoch`/`prerelease` to make the Arch `pkgver` monotonic.** Would embed the prerelease in `pkgver` on Arch — but the same fields flow into the .deb of **tagged** releases (`1:0.1.0~…`, `0.1.0-…`), changing every tagged version, and goreleaser 2.14 neither templates these fields nor honours per-format overrides of them. Rejected (documented as the remaining Arch wart instead).
- **Keep the SHA-only format and leave `--allow-downgrades` on all lifecycle steps.** Status quo: it works (the 2026-10-18 UNO Q run passed with the flag), but it bakes a known-wrong version ordering into the product, forces the flag on operators' one-shot `apt-get install ./new.deb`, and makes "newer build" indistinguishable from "older build" to every dpkg-based tool (apt lists, policy, backports). The failure was observed on the appliance, so this is a real repair, not a hypothetical hardening. Rejected.

## Tradeoffs

- **Same-second commits** (a commit authored in the same UTC second as its predecessor — or a backdated commit with an older timestamp than its parent) do not order by authorship time; the SHA suffix then decides, arbitrarily. Accepted: at this project's commit rate a backdated/zero-gap timestamp is pathological, and the failure mode is exactly the pre-fix one (lexical SHA order) rather than a crash or refusal to build.
- The version string is longer (`0.0.0-snapshot.1760745600.gf9a572e` vs `0.0.0-snapshot-f9a572e`); artifact names grow by ~10 characters. No tool in the pipeline assumes the old shape: `smoke.sh` recovers the version from the artifact name by pattern, the lifecycle doc's glob was updated accordingly, and the CI edge job (`.github/workflows/publish.yml`) reads the per-arch image tag from GoReleaser's own `dist/metadata.json` (`.version`, exported to `$GITHUB_ENV` as `GN_SNAPSHOT_VERSION` and verified against the locally built images) instead of recomputing `0.0.0-snapshot-<sha>` — so any future template change cannot silently desync the scan/push/manifest steps.
- The Arch `pkgver` wart persists (ADR 0053), now with a precise mechanism and a vercmp check proving the underlying version ordering is sound.

## How to verify (commands and evidence)

- `dpkg --compare-versions 0.0.0~snapshot.1760745600.gf9a572e lt 0.0.0~snapshot.1761106800.g0cbebce` → 0 (later commit greater); `dpkg --compare-versions 0.0.0~snapshot.9999999999.gfffff99 lt 0.1.0` → 0 (a tagged release beats every snapshot); the old form fails both in its failure mode (`0.0.0~snapshot-f9a572e` does **not** compare less than `0.0.0~snapshot-0cbebce`).
- `vercmp 0.0.0~snapshot.1760745600.gf9a572e 0.0.0~snapshot.1761106800.g0cbebce` → `-1` (first is older) and `vercmp 0.0.0~snapshot.9999999999.gfffff99 0.1.0` → `-1` (pacman 7.1.0 on this host).
- Two real snapshot builds from clean detached worktrees of the exact UNO Q pair (the refresh that was refused on the appliance): `git worktree add --detach /tmp/gn-v-<sha> <sha>` for `f9a572e` (older) and `0cbebce` (newer), `goreleaser release --snapshot --clean` in each, then `dpkg-deb -f <deb> Version` on both and `dpkg --compare-versions <older> lt <newer>` → 0. Worktrees removed afterwards.
- `goreleaser check` passes with the new template; `shellcheck` passes on both lifecycle scripts; `actionlint` passes on `.github/workflows/*.yml` including the edge job's `GN_SNAPSHOT_VERSION` derivation step.
- On-device (next UNO Q lifecycle run, docs/release.md section 6): the upgrade steps pass with plain `apt-get install`; only the rollback step needs `--allow-downgrades`. Not re-run on the appliance for this ADR — the appliance run remains the pending gate (AGENTS.md rule 12).

## Consequences

- Upgrades between snapshot builds are ordinary version increases on the UNO Q: one-shot `apt-get install ./new.deb` works without force flags; apt history/policy see a sensible ordering.
- The device lifecycle's flag surface shrinks to the single place a downgrade is intended (rollback), matching docs/release.md section 4.
- Snapshot versions are reproducible per commit (same commit ⇒ same version ⇒ same package filename), which makes `checksums.txt` and `artifacts.json` correlation across re-runs exact.
- Docker tags remain valid for any snapshot version (artifact-side characters are `[a-z0-9.-]` only).
- The Arch package keeps its same-version reinstall semantics; a future GoReleaser that templates `nfpms.prerelease` (or per-format `epoch`) can lift the `pkgver` wart without reordering anything, since the version string is already vercmp-monotonic.

## Rollback / migration path

Restore `snapshot: version_template: "{{ .Version }}-snapshot-{{ .ShortCommit }}"` in `.goreleaser.yaml`, restore `--allow-downgrades` on the lifecycle's install-previous step (`lifecycle-remote.sh` step 2), and revert the docs/release.md section 9 text. No data migration: the ordering lives in the version strings of newly built packages only; an appliance already running a `0.0.0-snapshot-<sha>` package upgrades cleanly to the new format (`0.0.0~snapshot.<ts>.g<sha>` compares greater than any `0.0.0~snapshot-<sha>` — the prerelease's digit run decides before any lexical SHA comparison), so mixed old/new vintages never deadlock.

## Links / references

- `.goreleaser.yaml` — `snapshot.version_template` (the change), `dockers_v2.tags` (why the format is docker-tag-safe), `nfpms` archlinux note (the `pkgver` wart, ADR 0053 mechanism).
- `.github/workflows/publish.yml` — edge job: "Derive the snapshot image tag from GoReleaser's own output" step (`jq -r '.version' dist/metadata.json` → pattern check → `docker image inspect` of both per-arch images → `echo GN_SNAPSHOT_VERSION=... >> "$GITHUB_ENV"`); scan/push/manifest steps consume it. `edge` and `sha-<short>` tags unchanged.
- `scripts/release/lifecycle-remote.sh` — step 2 (plain install), step 6 (rollback keeps `--allow-downgrades`).
- `docs/release.md` — section 9 (ordering rules and verified comparisons), sections 3/4/6 (upgrade, rollback, lifecycle).
- `nfpm` v2.45.0 (pinned by goreleaser 2.14): `parseSemver` in `nfpm.go` (version split), `arch/arch.go` `createPkginfo` (pkgver = upstream part, prerelease only with epoch), `deb` control template (`~` insertion).
- GoReleaser 2.14: `internal/pipe/snapshot/snapshot.go` (template rendering), `internal/tmpl/tmpl.go` (`.CommitTimestamp` = commit date, UTC, Unix seconds), `internal/pipe/nfpm/nfpm.go` (epoch/release/prerelease read only from the top-level entry, not template-rendered).
- ADR 0048 (snapshot runs never publish; image tags), ADR 0053 (Arch package, `pkgver` wart, same-version reinstall verified 2026-10-18).
