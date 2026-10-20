# ADR-0109: Library screen (Plum & Peach 2a) and the per-game library view endpoint

Date: 2026-10-20
Status: accepted
Related: ADR 0093 (Plum & Peach design system and IA; the screen spec and the
hero-slot policy open question (e)), `docs/design/plum-peach/README.md`
screen 2a, `docs/design/plum-peach/product-notes.md` UX-07/UX-08, ADR 0100
(status stream), ADR 0017 (OpenAPI contract discipline), ADR 0107 (library
lifecycle), docs/spec/09 ("`/library`")

## Context

Plum & Peach Phase 1, plan item 1: the `/library` screen is rebuilt from the
legacy games grid into the design's 2a layout — a hero card ("PICK UP WHERE
YOU LEFT OFF", 300px dither cover, one-line summary, **▶ Start narrating** /
**Cast** / **Dialogue** pills), an "All games" row with the ⌘K pill and a
**shelf / list** segmented toggle, a 5-column shelf grid whose first tile is
the dashed **Add a game** affordance, and a list view (cover · game + state ·
platform · lines · cast · voiced with a dithered progress bar · ▶). ▶ on any
game starts a session for that game's **published profile**; a game still
setting up takes the hero only when no ready game exists (ADR 0093 open
question (e), provisional answer adopted).

Two data questions the screen cannot answer with the routes that exist:

1. **"Which revision would ▶ run?"** A game can hold several editions, each
   with several revisions; a session may pin only the edition's *published*
   revision (the existing `GET /api/v1/editions/{id}/published` answers one
   edition at a time, and the Play page already pays the fan-out cost of
   games × editions × published to build its start form). The Library needs
   the game-level answer — one published revision to act on — per game.
2. **"How big is it, and how much of it has a voice?"** The list columns
   (lines, cast, voiced) and the hero summary ("12 characters, all voiced")
   are counts of one revision's content. The message list is cursor-paginated
   with no total, so the dialogue length is *not* cheaply derivable by any
   client; characters and voiced have no read at all outside the status
   bar's setup-run progress.

The progress chip on a setting-up tile is a different matter, and it stays
where Phase 0 put it: the single live status stream (UX-07, ADR 0100) already
carries each active setup run's game title and voiced/total counts
(`GET /api/v1/status` `setup_runs[]`). The tile chip reads that same
snapshot, matched by game title, so the bar and the tile cannot disagree
about a run's progress.

## Options

1. **A per-game read `GET /api/v1/games/{id}/library`** returning the
   actionable revision (published preferred, newest publication wins, else
   the newest draft) plus `lines` / `characters` / `voiced` counts.
   Client-side per-game react-query cache; the list endpoint is untouched.
2. **Extend `GET /api/v1/games` (the list response)** with per-game stats.
   One request for the page, but the shared catalogue list — used by the
   Play start form and the ⌘K search — pays an N×indexed-COUNT fan-out for
   callers that render no counts, and the `GameList`/`Game` contract shape
   grows for every client.
3. **Compose client-side** (games × editions × published × characters ×
   message page-walks). No backend change, but the dialogue length needs a
   full page-walk per game (10k-line profile ⇒ 10k rows over the wire per
   tile), which is exactly the cost docs/spec/08's pagination exists to
   keep off the common path.

## Decision

Adopt option 1:

- `GET /api/v1/games/{id}/library` (viewer, like the rest of the library
  browser; the scope table grants it explicitly before the curator wildcard
  that owns the game subtree). The answer: the game's **actionable
  revision** — the newest published revision of any edition (the one a
  session pins), else the newest revision of the first edition that has
  one (a draft a setup run is still filling), else absent — with
  `lines` / `characters` / `voiced` indexed counts of that revision
  (`ProfileCharacterVoiceCounts` for the pair, a new
  `ProfileMessageCount` COUNT for the dialogue length). A game with no
  profile answers 200 with the empty view: the honest zero the shelf
  renders, not a 404.
- The screen (item 1's owned files: `web/src/pages/Library.tsx`,
  `web/e2e/library*.spec.ts`):
  - **Hero policy** per ADR 0093 (e): a game with a published profile is
    the hero (the newest publication, deterministically); a setting-up game
    (an active setup run matched to it by title in the status snapshot) is
    the hero only when no ready game exists.
  - **Shelf / list toggle** persists per user in localStorage (UX-08) via
    the kit `Segmented`'s `storageKey` seam; the choice is restored on
    reload.
  - **▶** starts a session for the game's published profile. The frame
    source is `test_pattern` (the built-in synthetic source, real frames
    through the production consumer) because the Library's start action is
    "run this game now" and must work on an appliance whose capture dongle
    is not the session's operator choice; the operator who wants a replay,
    a capture or an emulator session uses the Play setup form, which keeps
    its source picker. The session id lands in the URL
    (`/play?session=…`), the Play page's existing session-survives-refresh
    seam.
  - **⌘K pill** opens the global palette through the shell's existing
    window-level ⌘K handler (a button that dispatches the same keyboard
    contract; the palette itself is item 2's).
  - Restyled per the design on tokens only (no raw hex, no inline styles;
    ADR 0011). The legacy testids the existing suite addresses (`game-list`,
    `new-game`) are preserved on their new markup; new surfaces get new
    testids from day one.

## Tradeoffs

- Option 2's single request is cheaper for a 50-row page, but the list
  endpoint is the catalogue's front door; keeping it a bare row read and
  paying one extra indexed read per *visible* game keeps every other caller
  (Play, ⌘K) at today's cost.
- The title match between a status setup run and a shelf tile is a display
  join (the run names the game the way the bar already reads it); two games
  with identical titles would share a chip, the same way the bar's string
  already names one run at a time. The run's `run_id` and progress remain
  authoritative in the bar.
- `test_pattern` as the Library start source means the appliance's
  "Start narrating" does not imply the HDMI capture path; that is the
  documented operator choice (Play setup form), and a capture-less appliance
  still gets a live, honestly-labelled session instead of a dead button.

## Validation

- Go: `gofmt` / `go vet` clean; `internal/api/game_library_test.go`
  (zero view, draft stand-in, import + publish counts, the pick rule across
  two editions, 404 and 401); `internal/store/repo_content_test.go`
  (per-revision count scoping). The OpenAPI contract test (ADR 0017) covers
  the new route bidirectionally: registered ⇔ documented, `x-required-role`
  ⇔ scope table, and the handler's 200 body validates against `GameLibrary`
  on the way through.
- `web/src/api/schema.d.ts` regenerated from the contract (`pnpm gen:api`);
  `pnpm check:api` passes.
- Playwright: `web/e2e/library.spec.ts` on its own packaged-binary server
  (dedicated project, port base+31) — seeds one ready (published) and one
  setting-up (active onboarding run, scripted-model-labelled) game; asserts
  the ready game is the hero, the setting-up game's progress chip on its
  tile and in the status bar, the shelf/list persistence across reload
  (UX-08), and ▶ landing on `/play` with an active session. Screenshots at
  1440 / 1024 / 390 with axe at zero serious/critical and no horizontal
  scroll at 390 (AGENTS.md rule 9). The two seams the walk needed:
  - the ready game's draft gets one set-wide textbox region through the
    region API before the publish (a published profile without observable
    regions is not startable: `profile-not-runnable`, sessions.go);
  - the in-setup seed uses the scripted gateway's new `stall` scenario
    (`web/e2e/fake-model.mjs`): import + quality, then a deliberate repeat
    of one read-only `preview_profile` call, which the loop's repetition
    guard ends as **blocked** — a durable active state with the content in
    place (terminal runs are not status data, so a finished run would leave
    the bar and the chip with nothing to read).

## Consequences

- The OpenAPI contract, the generated client and the scope table each gain
  one entry; the catalogue list's shape is unchanged.
- The `/library` screen is now the design's 2a; the legacy grid markup is
  gone (its testids moved, its behaviour — list, create a game, per-game
  links — preserved or restyled).
- "Last played" from the design's list columns has no data source in the
  catalogue (sessions keep no history read yet); the column is omitted
  rather than fabricated, and the remaining columns render real counts.

## Reversal

The route is additive: deregister it, drop the grant and the OpenAPI
operation, and regenerate the client. The screen change is a restyle of one
page: the legacy grid component can be restored without touching the new
endpoint. The `test_pattern` default for the Library's ▶ is a one-constant
choice in the page's start call.
