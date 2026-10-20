# ADR-0088: Update notification (release check, offline cache, no auto-update)

Date: 2026-10-20
Status: accepted
Owners: g6-g11 worker
Related requirements/tests: docs/spec/09 (/settings "software updates" — current vs latest version, a release-notes link, configurable and disable-able, no auto-update), docs/spec/13 (the doctor's state vocabulary for a check that could not run), docs/status/gap-audit-2026-10-19.md (G11: "NOT IMPLEMENTED (documented as a static note)"), ADR-0047 (the no-auto-update posture: replacing the binary is a documented manual step), ADR-0090 (the sibling ADR for the restore pair, same /settings section); tests `TestCompareVersions*`/`TestChecker*` (internal/updates), `TestSettingsUpdate*` (internal/api), `TestUpdatesRow*` (internal/doctor)

## Context and constraints

The /settings page currently renders a static note ("no auto-update in this build") and nothing more: the operator has no way to learn that a newer release exists, which version is current, or where the release notes live. Gap G11 asks for the minimal update surface: a "new version available" notification with the manual upgrade steps — **not** an updater.

Constraints that bind the shape:

- **The check informs; it never installs** (ADR-0047's posture, and the spec line "no auto-update"). The notification carries a release-notes link and points at the documented manual replacement; there is no download, no binary replacement, no self-restart-for-upgrades in this surface.
- **Offline-safe**: the appliance runs on the user's LAN; the check must never make the settings page wait for a network, and a cached answer must survive the network leaving. The answer must stay honest: "fresh", "from cache", "could not check (reason)", "disabled", "no releases yet" are different states.
- **Configurable and disable-able** (spec): the repository, the endpoint (self-hosted GitHub / mirror / the test's fake server) and the whole check are environment-controlled, and the settings page names the variables (the page's standing rule: a change has somewhere to go).
- **No credentials, no hostnames beyond the configured endpoint** (AGENTS.md rule 5): the check is a public read of a public releases API with a plain User-Agent.

## Options

- **No check (the static note, status quo).** Rejected: it is the gap.
- **An external updater (apt/cosign automation, self-replacing binary).** Rejected: it violates the no-auto-update posture and moves the failure surface onto the program's own files — the one thing a LAN appliance must not do to itself.
- **A bounded in-process check with an on-disk cache (chosen).** `internal/updates` performs one GitHub-compatible `GET /repos/{owner}/{name}/releases/latest` under a 5 s budget, caches the last good answer in the data directory (0600; a tag, a URL and a time — no secret), and reports a `Report` that the UI and the doctor render verbatim. A background loop refreshes hourly; a manual re-check route bypasses the interval.

## Decision

- `internal/updates`: `Checker.Check` answers fresh when the endpoint is reachable; on failure it falls back to the cache (`FromCache`) or reports the error (one line, no credentials); a disabled check does no I/O and answers `Disabled`. `CompareVersions`/`Newer` compare `<v?>MAJOR.MINOR.PATCH` (trailing pre-release/build metadata ignored); a `dev` build does not parse, and the answer for it is the "dev build" fact, never "no update".
- Configuration (documented on the settings page's section): `GAME_NARRATOR_UPDATE_CHECK` (default on; `0/false/off/no` disables), `GAME_NARRATOR_UPDATE_REPO` (default `dbirks/game-narrator`), `GAME_NARRATOR_UPDATE_BASE_URL` (default the public GitHub API; a mirror or the test's fake server).
- The **/settings section** (`update` view of `GET /api/v1/settings`): `auto_update` is always `false`; `current` is this build's stamp; `latest` is the release as the last check determined it (version, notes URL — the spec's release-notes link — publication time); `available` is true only when latest is strictly newer than current and both parse; `checked_at` / `from_cache` / `checking` / `no_releases` / `disabled` / `error` carry the honest state; `env` names the three variables.
- `POST /api/v1/settings/updates/refresh` (admin): one bounded fresh check now, rendered in the section's own shape so the page and the refresh answer cannot disagree.
- `serve` starts the checker in the background (first check at boot, then hourly); the cache lives in the data directory so the CLI and the appliance share the offline answer.
- The **doctor row** `updates.check` renders the same facts in the report's states: NOT CONFIGURED (disabled or unwired), OK (up to date, no releases yet, or the dev-build fact), DEGRADED (a new version is available — with the manual-replacement remedy; the answer came from the cache; or the check could not run) — and it **never FAILs**: an offline or outdated appliance is not a broken component.

## Tradeoffs

- The check is GitHub-REST-shaped (`releases/latest`); a non-GitHub endpoint that does not speak that shape answers an error, and the cached answer (if any) is what the page shows. That is the documented contract, not a silent failure.
- Version comparison is numeric triplets: a `dev` or untagged build cannot be compared and is rendered as such. The release process (tagging `vX.Y.Z`) is what makes the notification meaningful — tagging is the release owner's job (the gap audit's carried G20), not this ADR's.
- Hourly polling of a public endpoint from a LAN appliance is more than a release cadence needs; it is bounded (5 s each) and disable-able, and the cache makes the cost of being offline zero.

## Validation

- Unit (hermetic): `internal/updates` against a fake release server (fresh, 404-no-releases, unreachable-with-cache, unreachable-without-cache, disabled) plus the `CompareVersions`/`Newer` table; `TestSettingsUpdate*` in internal/api (the section from a live fake endpoint, disabled, no-checker static facts, the refresh gate and answer, the offline cache fallback); `TestUpdatesRow*` in internal/doctor (every state, the never-FAIL guarantee).
- OpenAPI contract + enum tests carry the section's new fields and the refresh route.
- Live evidence owed (Milestone gate): point a dev host at the real repository with a tagged release and record the section, the refresh and the doctor row (redacted) in the evidence bundle.

## Consequences

- The operator learns, from the page or `doctor`, that a newer release exists and where its notes live; the upgrade itself remains the documented manual step (make release-snapshot on the release host, copy, restart — or the replacement procedure in docs/release.md).
- The data directory gains `updates/latest.json` (0600, no secret) — part of the support bundle's data-directory snapshot.
- A repository without any releases yet renders `no_releases`, not an error: early in the project's release life the section is a fact, not noise.

## Reversal plan

Set `GAME_NARRATOR_UPDATE_CHECK=0` (no I/O, the section says disabled). To remove the feature, drop the checker wiring in `serve`, the section's dynamic fields fall back to the static note, and the cache file is inert (deletable). No data-format change is introduced.
