# ADR-0032: M5-part1 player walk e2e project, roster manifest builder, and revision-stable speaker diff

Date: 2026-10-17
Status: accepted
Owners: project
Related requirements/tests: docs/spec/09 "/play", "/games/:id/characters", "/games/:id/publish"; docs/spec/16 M5 ("user can curate a full synthetic game with no CLI"); docs/spec/12 (a) synthetic-stub and (b) captured-frame replay tiers; builds on ADR-0004 (replay evidence), ADR-0024 (session runtime and replay allowlist), ADR-0015 (revision publication)

## Context and constraints

- M5-part1 adds the `/play` operator session view, the `/games/:id/characters`
  roster, the `/games/:id/publish` review (diff, rights, voice gate, history),
  and the extended `/diagnostics`. Its acceptance path is a single walk in
  `web/e2e/m5-part1.spec.ts`: seed a synthetic profile **with a roster**
  (characters, per-line speakers, attribution), publish it, run a real replay
  session on the packaged binary, reload the page mid-session, inspect the
  roster, derive a revision, edit one line, review and publish the diff, and
  read the diagnostics page.
- The existing e2e infrastructure (ADR-0004, ADR-0024) already provides: a
  per-project `webServer` entry started by `web/e2e/serve.mjs` into an
  isolated `os.tmpdir()` data directory (proven by assertion, not config), the
  embedded binary serving the SPA, and an explicitly allowlisted replay root
  (`GAME_NARRATOR_REPLAY_ROOTS`).
- The shared manifest builder (`web/e2e/support/manifest.ts`) previously
  emitted only the sections the developer walk needed. The player walk needs
  `characters` (id, name, aliases, role, status, verified_source_refs) and
  per-message `speaker_id`/`attribution` so the roster page has a verified row
  and an inferred row to show.
- A derived revision mints fresh row ids for every message **and** character
  (internal/store `NewRevision`), so a client-side diff that compares store
  ids — including `message.speaker_id`, which references a character row id —
  reports every line as changed on any pristine derive.

## Decision

- Add a `play` Playwright project: its own embedded-binary webServer on port
  18102 with the repo's synthetic replay clip allowlisted, mirroring the
  `developer` project. One spec, one data directory, real APIs only; the spec
  signs in through the UI and uses `page.request` (with the page's own
  session cookie and CSRF token) for the seed/publish half.
- Extend `buildManifest` to emit the roster: `characters` rows and
  `message.speaker_id`/`attribution`, signing the digest exactly as
  `internal/profileio/canonical.go` does. The existing developer seed file
  (`testdata/synthetic/profiles/dialogue-advance-001.json`) is reused as-is
  and enriched in memory, keeping the engine-tuned distractor set that makes
  the replay commit `demo-page-1` then `demo-page-2`.
- `src/lib/profileDiff.ts` (client-side, unit-tested, pure) keys lines on
  `source_message_id` (falling back to text) and compares `speaker_id`
  **revision-stably**: the character *name* the id resolves to within each
  revision's own roster (a dangling reference is compared by raw id so it
  still shows as a change). Character rows diff by name.
- The `/play` page resumes its SSE stream from the session's own
  `event_cursor` even for a settled session: a terminal state must not gate
  the open, or a refresh after the run ended would show no committed lines.

## Alternatives considered

- *Server-side diff endpoint.* Rejected for M5-part1: the REST contract has
  no diff surface, the two revisions are small (messages + roster), and the
  honest, testable unit is a pure client function pinned by vitest. A future
  milestone may add an endpoint if revisions outgrow the payload (ADR-0017
  keeps the contract the source of truth).
- *Key the diff on store row ids.* Rejected: every line would render as
  removed+added on any derive — the exact failure the publish review exists
  to prevent.
- *A hand-rolled 4-distractor seed for the spec.* Rejected: the engine's
  conservative candidate ranking is calibrated against the existing 14-line
  seed; a bespoke distractor set changes which lines commit and makes the
  spec assert engine behaviour it was not designed to prove.

## Consequences and risks

- `play` is a 12th webServer; every Playwright run starts all servers
  (existing cost, one more). The spec's console-error whitelist now tolerates
  503 (the documented `/api/v1/voices` answer when voice models are not
  configured) alongside the 401 auth probe — the pages render their honest
  "not available in this build" panels for it.
- `buildManifest` output for existing callers is unchanged unless the seed
  carries `characters`/`speaker` fields; the developer spec's seed does not.
- The diff's speaker comparison hides a change only when both revisions'
  rosters resolve the ids to the same character name — a reviewer of the
  changed-fields list still sees the field name (`speaker_id`), never the
  opaque ULIDs.
- Mid-session reload timing is machine-speed dependent; the spec reloads the
  moment the first line renders and the assertions hold whether the session
  is still running (live resume) or already settled (replay-from-cursor), so
  both paths are proven without pinning wall-clock.

## Evidence and test plan

- `web/src/lib/profileDiff.test.ts` (synthetic stubs): pristine derive is zero
  changes with re-minted row **and** character ids; one edited `speech_text`
  reports exactly one changed message; speaker id re-mint to the same name is
  not a change; re-mint to a different name and to a dangling reference are
  changes named `speaker_id`; add/remove direction; text-keyed fallback.
- `web/src/lib/playReducer.test.ts` (synthetic stubs): commit folding,
  occurrence dedupe, order and speaker fallback semantics.
- `web/e2e/m5-part1.spec.ts` (captured-frame replay tier, embedded binary,
  isolated data dir): seed with roster → import-manifest (digest re-verified
  server-side) → review → publish; `/play` starts a replay session, both
  committed lines appear with their store message ids and roster speaker,
  reload brings the state back via `event_cursor`; `/characters` shows
  Marina (2 lines, verified) and Keeper (12 lines, inferred); derive + edit
  one line; `/publish` shows "1 changed" with the edited text, the
  draft→review→publish transaction lands, history records it with a success
  badge, and the diff empties once the pointer swaps; `/diagnostics` shows
  readiness `ok` and its detail. No unexplained console errors; the
  developer's own data directory is asserted untouched.
- Commands: `cd web && pnpm check:api && pnpm typecheck && pnpm lint &&
  pnpm fmt:check && pnpm test` and
  `systemd-run --user --scope -q -p MemoryMax=4G -p MemorySwapMax=0 make
  test-ui` (all e2e projects).

## Migration / rollback

- No schema, protocol, or persistence change: rollback is reverting the web
  tree; the `play` webServer entry, the spec, and the two pure libs leave
  together. The `buildManifest` extension is additive (fields appear only
  when the seed provides them).

## Follow-ups

- A server-side diff endpoint if revision payloads outgrow the client diff.
- Audio playback controls become live (they render disabled today: no
  `/audio` route exists in this build).
- Model-role health detail on `/diagnostics` when readiness exposes it beyond
  the `database` check.
