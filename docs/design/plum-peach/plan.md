# Plum & Peach — phased implementation plan

For parallel workers. Phase 0 is sequential (one worker) and must land first;
Phase 1 items run in parallel, each owning its files; Phase 2 is the
cross-screen closeout. Requirements referenced as UX-NN are from
`product-notes.md`; tokens/chrome/screens from `README.md`; binding decisions
from ADR 0093.

Ground rules for every worker:

- CSS custom-property tokens only; no raw hex outside the theme files; no
  runtime-injected styles (ADR 0011).
- Every new interactive element gets a stable `data-testid` and an accessible
  role; e2e selectors never use styles.
- All quoted line copy in UI and tests is invented placeholder text
  ("Placeholder line for the speaking character.") — never game dialogue
  (AGENTS.md rule 5).
- Each item below ends with acceptance criteria a Playwright spec can assert.

## Phase 0 — Foundation (one worker, first)

**Owners:** `web/src/index.css` (tokens, themes, chrome, dither utilities, focus
rings), `web/public/fonts/**` + `OFL.txt`, `web/src/components/kit/**`
(shared kit), `web/src/lib/liveStatus.ts` (and the Go status SSE endpoint in
`internal/api`), `web/src/App.tsx` (shell layout only).

1. **Tokens and themes.** Implement the Plum & Peach token set and the three
   runner-up themes (Ink Blue & Apricot, Sherbet, Lilac Night dark) as
   alternate token sets on `:root`/`[data-theme]`; all existing component
   styles re-expressed over the tokens.
   *Acceptance:* a Playwright spec asserts `getComputedStyle(document.body)`
   token values for the default theme; setting `data-theme` switches them; a
   scripted audit finds no raw hex in `web/src/**` outside `index.css`
   theme blocks.
2. **Fonts.** Vendor Fredoka (400/500/600/700) and Space Mono (400/700) into
   `web/public/fonts`, add `@font-face` rules and `OFL.txt`; verify CSP is
   unchanged and fonts load from `style-src`-compliant static files.
   *Acceptance:* spec asserts `document.fonts` check passes for both families
   and that `web/public/OFL.txt` is served and included in the release
   archive (embedded asset test).
3. **Global shell.** Status bar (wordmark, nav with active state, live status
   string slot, 3-row dither fade), page layout, 2px-coral focus rings,
   dither utility classes (12/25/50/75% recipes, stacked cover bands).
   *Acceptance:* spec asserts the status bar is present on `/library`,
   `/play`, `/traces`, `/health`, `/settings`; the nav active item tracks the
   route; Tab focus shows the 2px coral ring (computed outline/border);
   `image-rendering: pixelated` applied on dither surfaces.
4. **Shared component kit.** `Card`, `Pill` (button), `Chip`, `NameTab`,
   `DitherCover`, `Segmented`, `StatTile`, `ProgressBar`, `Sparkline`,
   `Waveform` — each with `data-testid`, accessible semantics (buttons are
   buttons, segmented control is radiogroup/switch per Radix), and
   restyled empty (dashed) / error (coral + `request_id`) variants.
   *Acceptance:* per-component axe run zero serious/critical; a Playwright
   spec renders each with fixture data and asserts role + testid +
   `aria-*` (e.g. `ProgressBar` exposes `role=progressbar` with the right
   `aria-valuenow`).
5. **Live-status API + SSE.** One server-side status aggregate (session
   state, capture fps, frame→voice latency, setup progress n/N and %,
   non-ok health tiles) exposed as a single SSE stream
   (`GET /api/v1/status` or extension of the existing events SSE) with
   Last-Event-ID replay; `lib/liveStatus.ts` reducer feeding a React context
   that renders the status-bar string.
   *Acceptance:* spec injects fixture events (fake SSE) and asserts the
   status-bar text updates for each of: setup progress tick, live session
   start (fps + latency present), a health tile going non-ok (tile name
   appears in the string); only one status subscription is open per page.

## Phase 1 — Screens and features (parallel; one worker per item)

File-ownership is exclusive within an item; shared kit and live-status API
are read-only dependencies from Phase 0.

1. **Library** — `web/src/pages/Library.tsx`, `web/e2e/library*.spec.ts`.
   Hero card with Start narrating / Cast / Dialogue pills; All-games row with
   ⌘K pill + shelf/list Segmented; shelf grid (Add-a-game dashed tile first)
   and list view; ▶ starts a session for the game's published profile;
   hero-slot policy UX (setting-up game is hero only when no ready game).
   *Acceptance:* spec seeds 1 ready + 1 in-setup game, asserts the ready
   game is the hero and the in-setup game's progress chip is on its tile and
   in the status bar; toggling shelf/list persists across reload (UX-08);
   clicking ▶ lands on `/play` with an active session.
2. **⌘K search (API and UI)** — `web/src/pages/KSearch.tsx` (overlay),
   `web/src/lib/search.ts`, a search endpoint in `internal/api` (or reuse of
   existing search APIs), `web/e2e/ksearch.spec.ts`.
   Groups: games, characters, lines, traces, settings, actions; ↑↓/↵/Tab
   keys; Tab hands the query to the agent dock.
   *Acceptance:* UX-02 assertions: opens from any page without navigation;
   grouped multi-category results; ↵ navigates/executes per row type; Tab
   pre-fills the dock input and the outgoing agent message carries the query.
3. **Agent dock (API and UI)** — `web/src/components/agent/**`, agent API
   changes (context payload), `web/e2e/agent-dock.spec.ts`.
   Floating pill with peach context chip; docked 360px panel (NEEDS YOU,
   tool cards, chat, input); context payload `{route, gameId, profileId,
   selection, visibleIds}` on every message; works on every screen.
   *Acceptance:* UX-01 assertions: chip text matches screen/selection and
   follows selection changes; agent request body contains the full context
   payload (asserted redacted via API capture); dock reachable via ⌘J from
   each of the six primary routes.
4. **Setup board** — `web/src/pages/NewGame.tsx` (rework), onboarding SSE
   stage events, `web/e2e/setup-board.spec.ts`.
   Stage rail (6 cells), 3 stat tiles, 4-column live board with gliding
   cards driven by SSE stage events, coral border while voicing, right agent
   panel (docked ⌘J form); Pause and Trace pills.
   *Acceptance:* UX-06 assertions: cards move only on injected SSE stage
   events (no polling); voicing cards show the coral border; under
   `prefers-reduced-motion: reduce` transitions are disabled; on run
   completion the user lands on Cast with a prominent "Start narrating"
   (open question (a) provisional answer).
5. **Cast with auto-cast (backend and UI)** — `web/src/pages/Cast.tsx`
   (merging `Characters.tsx` + `Voices.tsx`), voice/onboarding backend
   changes for auto-cast + per-profile consent default + regenerate-undo,
   redirects for `/games/:id/characters` and `/games/:id/voices`,
   `web/e2e/cast.spec.ts`.
   *Acceptance:* UX-03/UX-03d assertions: one approved take per character
   with auto-cast on and no per-character prompt (consent attributed to the
   profile default); brief edit + regenerate produces a new current version
   with the previous one still playable and one-step undo available;
   `?character=` selection works and old routes redirect; list/cards toggle
   persists (UX-08).
6. **Play now-speaking and operator** — `web/src/pages/Play.tsx`
   (rework), `web/e2e/play-pc.spec.ts`.
   Default now-speaking view (portrait arch, 52px line, waveform, last-3
   lines, control bar with careful/eager); "show workings" toggle to the
   operator view (4:3 frame with ROI overlays, event stream, candidates,
   correction buttons); one session state across the toggle; tablet/phone
   widths; kid-mode route stub `/play?mode=kid`.
   *Acceptance:* toggling views does not restart or duplicate speech (no
   duplicate `speech.started` for one line); correction buttons (Wrong
   speaker / Wrong text) fire the correction API; layout holds at 1024px and
   390px (screenshot review + no horizontal scroll); kid mode shows only
   −/pause/+ (UX-10).
7. **Traces** — `web/src/pages/developer/**` → `web/src/pages/Traces.tsx`,
   route rename `/developer` → `/traces` (redirect), `web/e2e/traces.spec.ts`.
   Left filterable line list (spoken/skipped/slow), 5-step strip (Saw → Read
   → Matched → Who → Spoke with +ms), "What it read" (crop + raw/normalized),
   "Why this line" (candidate bars + reason), actions (Replay audio, Wrong
   speaker, Save as test, Download evidence); existing inspection kept
   reachable.
   *Acceptance:* for a seeded `trace_id`, the 5-step strip values match the
   API record; "Save as test" creates a fixture (assert via API); old
   `/developer` URL redirects to `/traces`; corrections from a trace trigger
   the UX-05 agent-feedback flow.
8. **Health** — `web/src/pages/Diagnostics.tsx` → `web/src/pages/Health.tsx`,
   route rename `/diagnostics` → `/health` (redirect), `web/e2e/health.spec.ts`.
   3×2 tile grid (capture, OCR, decision model, TTS, agent model, storage)
   with status tags, one-line detail, 12-bar dither sparkline; tabs
   (line traces · setup runs · health · logs); existing readiness checks and
   logs preserved under tiles; non-ok tiles surface in the status bar.
   *Acceptance:* with a fixture health report containing one non-ok tile,
   the tile shows the coral tag and the status bar on any page includes that
   tile (UX-07/UX-11); old `/diagnostics` redirects.
9. **Settings and theme page** — `web/src/pages/Settings.tsx` (rework),
   new theme page (route under settings, e.g. `/settings/theme`),
   `web/e2e/settings-theme.spec.ts`.
   Theme picker (four themes) with live preview + persistence; audio output
   selector with test tone (HDMI-hub, USB, Bluetooth sinks; pairing flow for
   unpaired Bluetooth); tailnet URL display.
   *Acceptance:* UX-09 (theme persists across reload, tokens switch), UX-12
   (selector lists sink categories; selection changes the API sink; test
   tone request fires; unpaired BT shows the pairing prompt), UX-13
   (tailnet URL equals the API value, or explicit "not configured").
10. **Kid mode** — `web/src/pages/KidMode.tsx` (or `Play.tsx?mode=kid`),
    `web/e2e/kid-mode.spec.ts`.
    *Acceptance:* UX-10 assertions (three controls only; pause/volume work
    via the session API).

## Phase 2 — Cross-screen polish, a11y, visual regression, docs

1. **Cross-screen polish:** consistent restyle of empty/loading/error states
   (dashed empty card, coral error card with `request_id`), session-survives-
   refresh behaviour on all new routes (SSE reconnect + replay), no duplicate
   speech on remount; keyboard walkthrough of the whole flow
   (⌘K → character → Cast → ⌘J agent → regenerate → Play).
   *Acceptance:* full Playwright walkthrough spec passes; refresh mid-session
   restores the session with no duplicate speech.
2. **A11y:** axe suite extended to all new routes and the theme page;
   contrast holds 4.5:1 (no `--muted` on `--peach`); `prefers-reduced-motion`
   respected in every animated surface.
   *Acceptance:* axe report zero serious/critical across the suite; computed
   style checks for focus ring and contrast pairs.
3. **Visual regression:** Playwright screenshot baselines of every screen at
   **1440px, 1024px and 390px** (default theme, plus Lilac Night at 1440px);
   baselines reviewed and committed; CI fails on visual drift beyond
   threshold.
   *Acceptance:* `make test-ui` includes the screenshot pass and the three-
   width matrix; new screens appear in the baseline set.
4. **Docs and screenshots:** add/refresh `docs/manuals/` pages (Play setup,
   Health/audio on the UNO Q, theming) with screenshots from the evidence bundle;
   record the working appliance recipes (audio sink selection, HDMI signal)
   per AGENTS.md "Codify one-offs".
   *Acceptance:* manual pages link to the new routes and show current
   screenshots; the audio section covers HDMI-hub/USB/Bluetooth selection
   with the test tone.
