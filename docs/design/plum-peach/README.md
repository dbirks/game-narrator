# Plum & Peach — design system and screen spec

Status: **adopted direction** (ADR 0093). This document is the spec for the
web UI redesign; it is written in our own words from a design-review handoff
bundle (see Provenance) and is the source of truth for tokens, chrome, screens
and accessibility rules. Functional requirements derived from it live in
`product-notes.md`; the implementation plan lives in `plan.md`.

## Provenance

- Bundle: **"App design system review.zip"**,
  sha256 `8756c65047e6e652feea9eea647fdadf04051a25694f2d50317c0364f56cdd53`,
  produced by Claude Design. The archive and its extracted contents are kept
  **outside the repository** (extracted read-only at
  `game-narrator-design-src/design_handoff_game_narrator_redesign/`) and must
  never be committed.
- The bundle's HTML prototypes are **untrusted input**: treat them as design
  reference only. Do not execute or import their scripts, and do not copy
  their markup into the app.
- The mocks quote real game dialogue as placeholder copy. Per AGENTS.md rule 5
  that dialogue is **not** reproduced anywhere in this repo. All quoted lines
  in this spec are invented placeholders ("Placeholder line for the speaking
  character."). Character and place names (e.g. Aryll, Grandma, Skies of
  Arcadia) are used as neutral examples only.
- Fidelity contract: **high** for the visual language (colour, type, lines,
  dither textures, status bar, card/tab treatment); **medium** for exact
  layouts — structure, hierarchy and components are binding, pixel positions
  may adapt to real data and responsive needs.

## Scope and what is not built

- **Chosen direction:** the combined screen set (2a–2i, "Batch 2" in the
  handoff): Library, ⌘K search, setup run, Cast, Play (now-speaking and
  operator), kid mode, line trace, health.
- **Not built:** the Batch 1 explorations (1a–1l in the handoff — the
  four-way variants of Library, add-a-game, Cast, the swimlane, etc.) fed the
  chosen design and are explicitly not to be built. The swimlane timeline
  (1h) was rejected and must not be built for now.

## Design tokens

### Colour — Plum & Peach (light, default theme)

| Token | Hex | Use |
|---|---|---|
| `--ink` | `#5a2a4a` | all text, 1.5px lines, primary (ink-fill) buttons, status bar background, name tabs |
| `--paper` | `#f8eedd` | page background; text on ink surfaces |
| `--card` | `#fff7ea` | cards, panels, tables, inputs |
| `--peach` | `#f3b49a` | selection/highlight fill, chips, active nav text on ink, portrait base |
| `--coral` | `#e2725b` | the one hot accent: primary CTAs (Regenerate, add +), "needs you", ROI boxes, active underline, Stop |
| `--muted` | `#7a5468` | secondary text, column headers, metadata — one step darker than the mock's `#8a6178` (4.49:1 on paper) so the token holds WCAG 1.4.3 at small sizes (5.5:1 on `--paper`/`--card`); the axe gate (zero serious) drove the shade |
| `--sand` | `#efdfc8` | empty track of progress bars, neutral board columns |
| `--hair` | `#d9bfa8` | dashed row separators |

Rules: `--ink` + `--paper` + `--card` carry ~90% of the UI. `--peach` marks
"selected / current". `--coral` is used sparingly, only for "act now". Because
theming is a product requirement (see Themes and UX-09), **every colour in the
UI must come from a token** — no raw hex values in component styles.

### Typography

- **Fredoka** (SIL OFL 1.1, self-hosted), weights 400/500/600/700: titles,
  names, buttons, body copy, quoted lines.
  - Page title 36px/600, line-height 1. Section heading 26px/600.
  - Card name 16–17px/600. Body 15–18px/400, line-height 1.35–1.45.
  - Now-speaking line 52px/500, line-height 1.1.
  - Fredoka for setup-style headlines ("Setting up …") is a deliberate keeper.
- **Space Mono** (SIL OFL 1.1, self-hosted), weights 400/700: all data and
  metadata — counts, ids, timings, column headers (11px uppercase), status bar
  (12px), chips (11–12px). Name tabs: 11–13px/700, letter-spacing 0.1em,
  uppercase.
- Nothing else. No third typeface.

### Lines, radius, elevation

- Borders: **1.5px solid `--ink`** on cards and panels. Rows: 1px dashed
  `--hair`. Dashed 1.5px `--ink` means "drop zone / in progress / add".
- Radius: cards 10–16px; chips and buttons 999px (pill); small tags 3–4px;
  portraits 8–12px; the hero portrait may be an arch
  (`999px 999px 24px 24px`).
- No blur shadows. The only elevation is a flat offset,
  `0 4px 0 rgba(90, 42, 74, 0.25)`, on the floating agent pill. Dithered
  offset shadows are an optional flourish, not a default.

### Dither textures (CSS only, no image assets)

The signature texture system. All recipes are 2–4px repeating patterns of two
tokens (INK and BG substituted per use):

| Name | Recipe |
|---|---|
| 12% | `radial-gradient(INK 0.6px, transparent 1px) 0 0 / 3px 3px, BG` |
| 25% | `radial-gradient(INK 0.9px, transparent 1.3px) 0 0 / 4px 4px, BG` |
| 50% checker | `repeating-conic-gradient(INK 0 25%, BG 0 50%) 0 0 / 2px 2px` |
| 75% | the 25% pattern with INK and BG swapped |

Standard uses: the status bar's 3-row fade (12% → 50% → 75%, each row 4px
tall, ink toward paper), progress-bar fills (ink + coral checker),
portrait/cover placeholders (stacked bands 0% → 12% → 50% → 75% → 100%),
health sparklines, the "voicing" indicator dot. Use
`image-rendering: pixelated` where it matters and keep pattern sizes on whole
pixels.

### Spacing

Scale: 4 / 6 / 8 / 10 / 12 / 14 / 16 / 18 / 22 / 24 / 28 / 32px.
Page padding 22–36px; card padding 10–16px; grid gaps 12–22px.

## Themes

Every theme maps onto the same token set, so components are theme-agnostic.
Values below were extracted from the handoff's style exploration; slots not
shown in the runner-up mocks are marked *derived* and must be confirmed in a
theme-page review pass before shipping.

### Plum & Peach (default, light) — see colour table above.

### Ink Blue & Apricot (light)

| Token | Hex | Source |
|---|---|---|
| `--ink` | `#24357a` | mock (4c/5c) |
| `--paper` | `#fdf0e2` | mock (4c/5c) |
| `--card` | `#fff9f1` | mock (5c) |
| `--peach` | `#ffc59a` (apricot) | mock (4c/5c) |
| `--muted` | `#4b5058` | mock (5c) |
| `--coral`, `--sand`, `--hair` | *derived* | not in mocks |

Character: dithered drop shadows instead of blur (`4 4px` checker offset
panels).

### Sherbet (light)

| Token | Hex | Source |
|---|---|---|
| `--ink` | `#43284e` | mock (4f) |
| `--paper` | `#fff6e6` | mock (4f) |
| `--card` | `#fff6e6` | mock (4f; card same as paper) |
| `--peach` | `#a3d9b1` (mint) | mock (4f) |
| `--coral` | `#ff9c7a` (apricot) | mock (4f) |
| `--muted`, `--sand`, `--hair` | *derived* | not in mocks |

Character: three-ink dithered blobs behind content; two accent chips
(mint + apricot).

### Lilac Night (dark)

| Token | Hex | Source |
|---|---|---|
| `--ink` (background) | `#221a33` | mock (4e/5d) |
| `--paper` (text on dark) | `#f1e6ff` | mock (4e/5d) |
| `--card` | `#2e2444` | mock (4e/5d) |
| `--peach` (selection) | `#c7a9ff` (lilac) | mock (4e/5d) |
| `--coral` (hot accent) | `#ffb3c7` (pink) | mock (4e/5d) |
| `--muted` | `#e2d4fa` | mock (4e/5d) |
| `--sand` (tracks) | `#3a2f55` | mock (5d) |
| `--hair` | `#4a3d66` | mock (5d) |

Character: dark mode where dither reads as glow; intended for the live
session. A dark theme inverts which token is "background", so the token
contract (not the light defaults) is what components must respect.

### Fonts and licensing

- Fredoka and Space Mono are both **SIL Open Font License 1.1**. We
  **self-host** the font files in `web/public/fonts` (or equivalent static
  path) and reference them from `@font-face` rules in the stylesheet. No
  runtime font/style injection — this keeps the strict CSP
  (`style-src 'self'`, ADR 0011).
- OFL 1.1 is compatible with our **Apache-2.0** project license (ADR 0086):
  OFL is a permissive font license that permits redistribution and derivative
  works with attribution; it does not impose copyleft on the code.
- **The OFL licence text must ship with the font files** (OFL 1.1 requires the
  license to accompany distributed fonts): ship `OFL.txt` next to the font
  files in `web/public`, include it in the release archives, and keep the
  fonts' copyright notices intact. Do not rename the fonts.

**Source and provenance** (AGENTS.md rule 5: imports are provenance-aware).
The woff2 files in `web/public/fonts/` were downloaded from the official
Google Fonts GitHub repository (`google/fonts`, SIL OFL 1.1), verified
byte-identical (sha256) against `main` on 2026-10-20, and the Fredoka statics
instantiated from the family's variable font (wght 400/500/600/700 at the
default width) before woff2 conversion:

| Shipped file | Upstream source (raw URL) | sha256 (upstream source) |
| --- | --- | --- |
| `Fredoka-400.woff2` … `Fredoka-700.woff2` | `https://github.com/google/fonts/raw/main/ofl/fredoka/Fredoka%5Bwdth%2Cwght%5D.ttf` (variable, wdth 75–100 / wght 300–700) | `2ba02e68b152868aef9ba28e24b3648c7d457fe6f25c761f2c2c53fb61a73fc8` |
| `SpaceMono-Regular.woff2` | `https://github.com/google/fonts/raw/main/ofl/spacemono/SpaceMono-Regular.ttf` | `95837e182baeeada83368f7748db28357f0a1b75c6b84ff7065b5edf933c8e18` |
| `SpaceMono-Bold.woff2` | `https://github.com/google/fonts/raw/main/ofl/spacemono/SpaceMono-Bold.ttf` | `405e73d41afb7e5906efce206a326af5c956f38e255f35421c260e861e599c59` |

sha256 of the shipped artifacts themselves (`web/public/fonts/`), what the
`@font-face` rules load:

| File | sha256 |
| --- | --- |
| `Fredoka-400.woff2` | `18ae0d79e4f0173951bd959ea7bf4feb0136ca1fadd219bb3b9466ae3e8e63d5` |
| `Fredoka-500.woff2` | `5fdf23b5a4a1b8026163c826c15aa2d9916560f8a42fb327eeb4e73454db76ee` |
| `Fredoka-600.woff2` | `5744cd6443b6f6ad318219b318019ed8f11fdd28909e5bcf5a32af3eafbf47a3` |
| `Fredoka-700.woff2` | `30cc69971c3f9bcaf0efc805087ad7f66924ee83b8ec8b3a38b3f9520317dab7` |
| `SpaceMono-Regular.woff2` | `e3f87790f0d51da394d93cfed60d03f05bd17c93398e6872f6a76edf22d3b85b` |
| `SpaceMono-Bold.woff2` | `a3d3b345e5895aba197c2b4a4ab8b93f0cea07417fa2338efd9c8795921890bd` |

The shipped `OFL.txt` (sha256
`1492a1f28d87de59156f1e56b0972773540572dcfa6ca0c85c9f214bfcbb234d`) carries
both families' copyright notices (Fredoka Project Authors; Space Mono Project
Authors) from the upstream per-family OFL copies.

## Global chrome (every screen)

### Status bar

- A 28px `--ink` bar, Space Mono 12px `--paper` text. Left to right:
  1. Wordmark `GAME·NARRATOR` (700, letter-spacing 0.12em).
  2. Primary nav: `library · play · traces · settings`; the active item is
     `--peach` 700. In-game pages replace the middle nav item with the game
     name.
  3. Right-aligned **live status string** for whatever is happening, e.g.
     `● setting up Skies of Arcadia · 62%`,
     `● agent running · 9 of 12 voiced · 18m 04s`,
     `● live · capture 30fps · 412ms frame→voice`, or
     `5 ok · 1 slow · v0.9.2 · uno-q`.
- Below the bar: three 4px dither rows fading ink → peach (12% → 50% → 75%).
- The string is fed by the single live status stream (UX-07); it is not
  assembled per page.

### Ask the agent (⌘J)

- A floating `--ink` pill, bottom-right, carrying a `--peach` **context chip**
  for the current screen and selection (e.g. `library`, `cast › Grandma`,
  `Skies of Arcadia › setup`), plus a `⌘J` hint. Elevation: flat offset shadow
  only.
- It expands to the docked right panel (360px): header with context chip, a
  **NEEDS YOU** block (coral, non-blocking questions with answer pills and an
  "or keep going" escape), narrative/summary text, condensed tool cards
  (✓ name · result · duration; in-progress cards are dashed with a dither
  dot), user/agent chat bubbles, and the input.
- Every agent message carries the context payload
  `{route, gameId, profileId, selection: {type, id}, visibleIds[]}` so that an
  instruction like "make her older" resolves to the selected character.
  (UX-01.)

### ⌘K search

- A general-purpose search overlay reachable from any screen, across **games,
  characters, dialogue lines, traces, settings and actions** (UX-02).
- Results are grouped with mono group headers; the active result row is
  `--peach`. Keyboard: ↑↓ move, ↵ open, **Tab hands the query to the agent**
  (pre-filled in the dock). It is *not* the add-a-game flow — adding a game
  lives in Library.

## Screens

| Mock | Screen | Route | Built from |
|---|---|---|---|
| 2a | Library | `/library` | `Library.tsx` |
| 2b | ⌘K search | global overlay | new |
| 2c | Setting up a game | `/games/new` | `NewGame.tsx`, onboarding API |
| 2d | Cast | `/games/:id/cast` (merges `/games/:id/characters` and `/games/:id/voices`; old paths redirect) | `Characters.tsx`, `Voices.tsx` |
| 2e/2f | Play | `/play` (operator view behind "show workings") | `Play.tsx` |
| 2g | Kid/tablet mode | `/play?mode=kid` | new, low priority |
| 2h | Line trace (Traces) | `/traces` (renamed from `/developer`) | `developer/*` |
| 2i | Health | `/health` (renamed from `/diagnostics`) | `Diagnostics.tsx` |

### 2a — Library (`/library`)

- **Hero card** (`--card` bg, 1.5px `--ink`, radius 16, padding 16): 300px
  cover on the left; on the right the mono-12 `--muted` eyebrow "PICK UP WHERE
  YOU LEFT OFF", the title (Fredoka 44/600), a one-line summary (18px, e.g.
  "Last played 2 days ago on Outset Island. 12 characters, all voiced."), and
  pill buttons: **▶ Start narrating** (ink fill), **Cast**, **Dialogue**
  (outline).
- An **"All games"** row (section 26/600) with a `⌘K search anything` pill and
  a **shelf / list segmented toggle** (active segment ink-filled).
- **Shelf view:** 5-column grid, first tile a dashed **Add a game** tile
  (coral circle +). Game tiles hold a 150px dithered cover, title 17/600, a
  state chip (`--peach`) and a ▶ circle.
- **List view:** columns cover 44px · game (title + state) · platform · lines ·
  cast · voiced (count + dithered progress bar) · last played · ▶.
- ▶ on any game starts a session for that game's published profile.
- Hero slot policy: a game still setting up takes the hero **only when no
  ready game exists**; otherwise its progress shows in the status bar and on
  its tile (UX-14; ADR 0093, open question (e)).

### 2b — ⌘K search (global)

See Global chrome. Result groups: characters, actions, lines (plus games,
traces, settings). Action rows offer app actions ("Regenerate … voice", "Play
all of …'s lines") and an "Ask the agent: …" row. Line rows show the line id
(mono). Footnote bar: `↑↓ move · ↵ open · tab ask agent`.

### 2c — Setting up a game (`/games/new`)

- Header: "Setting up {Game}" (36/600) with **Pause** and **Trace** pills on
  the right.
- **Stage rail:** six equal cells in one bordered strip — 01 Find game · 02
  Dialogue · 03 Characters · 04 Briefs · 05 Voices · 06 Publish. Each cell:
  number (`--muted`), name (Fredoka 15/600), a value (e.g. "4,212 lines").
- **Three stat tiles:** lines read · characters · voiced, the last with a
  dithered progress bar (ink + coral checker over `--sand`, animated
  `clip-path`).
- **Live board:** four columns (FINDING · BRIEF · VOICING · DONE), each with a
  mono count. Character cards are absolutely positioned and **glide between
  columns** on state change (`transition: left/top 1.1s
  cubic-bezier(.65,0,.35,1)`); a card in VOICING gets a coral border.
  Positions are driven by server SSE stage events, never by polling
  (UX-06). Motion respects `prefers-reduced-motion` (cards reposition
  instantly, no glide).
- **Right agent panel (360px):** context chip, NEEDS YOU block (non-blocking
  question + answer pills + "or keep going"), narrative summary, compact tool
  cards, chat bubbles, input. The panel is the docked form of the ⌘J pill.

### 2d — Cast (`/games/:id/cast`)

- Header: game title (36/600) with game tabs
  (overview · dialogue · **cast** · regions · publish; active = coral 2px
  underline), an **auto-cast** switch (default ON) and a list / cards toggle.
- **List view:** columns avatar · name (Fredoka 16/600) · lines · state pill ·
  voice brief (ellipsis). Clicking a row selects it (`--peach` row bg) and
  puts `?character=` in the URL.
- **Cards view:** 3-column grid of cards with an ink **name tab** overlapping
  the top border (mono 700 uppercase), dithered portrait strip, line count and
  state.
- **Detail panel (380px):** ink name tab overlapping the top border, portrait
  (dither bands as placeholder; real art drops into the same frame), facts
  (mono: lines · verified, voice version · approval state, first line id), an
  **editable voice brief** textarea, the current take player (`--peach` pill
  with waveform + ink play button), **↻ Regenerate voice** (coral fill),
  "3 more takes" and "Hear lines" (outline), and sample lines (invented
  placeholder text in the spec and tests).
- Auto-cast semantics (UX-03): with the switch on, every character gets one
  take that is **auto-approved**; the existing consent field is satisfied by a
  **per-profile consent default** (e.g. `original_synthetic_design`), not a
  per-click prompt. Multiple auditions are opt-in. Regenerating a voice keeps
  the previous take as a one-step undo (UX-03d; ADR 0093).

### 2e/2f — Play (`/play`)

One page, two views. The default is **Now speaking**; a "show workings"
toggle swaps to **Operator** (ADR 0093, open question (b)).

- **2e Now speaking (default):** portrait arch (260×300, radius
  `999px 999px 24px 24px`, dither bands) + ink name tab + the current line at
  52px (doubles as captions) + a big waveform + the last 3 lines at 75%
  opacity (speaker mono tab, line text, timestamp). Bottom control bar:
  pause · Mute · volume (dithered bar, percentage) · delay (ms) · **careful /
  eager** segmented toggle (the friendly names for conservative / predictive) ·
  "Wrong speaker?" · **Stop** (coral outline).
- **2f Operator:** a 4:3 live frame (600px) with ROI overlays (coral textbox
  box labelled "textbox · OCR 0.93", dashed name-plate box) and the committed
  line beneath (name tab + line). Right side: an event stream table (time ·
  kind · detail; current row `--peach`), match candidates with probability
  bars (ink + coral checker), and correction buttons: Wrong speaker · Wrong
  text · Save as test · Stop.
- Both views keep the same session state; switching views does not restart or
  duplicate speech (existing UX-robustness rule: no duplicate speech due to
  UI remount).
- Target widths: Play runs on a laptop, tablet or phone **beside the TV** —
  the appliance has no screen of its own — so layout must hold at tablet and
  phone widths as well as 1440px (ADR 0093, open question (c)).

### 2g — Kid/tablet mode (`/play?mode=kid`, low priority)

- Three huge buttons and nothing else: − / pause / + (volume and pause), a
  260px circular portrait, the character name at 48px and the current line at
  24px. No controls beyond pause and volume. Entry/exit policy (e.g. a PIN to
  leave) is an open detail, not a requirement yet.

### 2h — Line trace (`/traces`, renamed from `/developer`)

- Left: a filterable list of spoken lines with filter pills (spoken / skipped
  / slow); each item shows speaker + time + line; the current one is
  `--peach`.
- Main: the line as a 30px title, then a **5-step strip** — Saw frame → Read
  OCR → Matched (probability) → Who → Spoke (cache hit/miss), each with a +ms
  offset; the last step is `--peach`.
- "What it read": the textbox crop + raw vs normalised OCR text (mono).
  "Why this line": candidate probability bars + a one-sentence reason.
- Actions: ▶ Replay audio · Wrong speaker · Save as test · Download evidence.
- Data maps onto the existing `trace_id` / `dialogue_occurrence_id` records;
  the existing developer-dashboard inspection (candidates, Clef payloads,
  model inspector) remains reachable from here.

### 2i — Health (`/health`, renamed from `/diagnostics`)

- Page title (36/600) plus tabs: line traces · setup runs · **health** · logs.
- A 3×2 tile grid: capture, OCR, decision model, voice (TTS), agent model,
  storage. Each tile: mono label, status tag (`--peach` = ok, `--coral` =
  slow/fail), a one-line detail, and a 12-bar dithered sparkline. Each tile
  opens its own detail (the existing diagnostics content: readiness checks,
  logs, etc.).
- A non-ok tile also surfaces in the global status bar on every page (UX-07).

## Interactions and state

- View toggles are **persisted per user** (library `view: shelf|list`, cast
  `view: list|cards`); localStorage is acceptable (UX-08).
- Cast selection lives in the URL (`?character=`), as the current Voices page
  does.
- Setup board: each character has `stage ∈ {finding, brief, voicing, done}`
  from onboarding job events; cards animate **on change only**.
- Agent context: `{route, gameId, profileId, selection: {type, id},
  visibleIds[]}` is sent with every agent message (UX-01).
- All existing empty/loading/error states and `request_id` error metadata stay
  in place, restyled: **empty** = dashed 1.5px `--ink` card; **error** =
  `--coral` border card with the problem+json `request_id` shown.

## Accessibility

- Hold **4.5:1 contrast for text**. Ink on paper/card/peach passes. Never put
  `--muted` text on `--peach`.
- **Focus rings:** 2px `--coral`, offset 1px, on every interactive element.
- The existing **axe suite stays at zero serious/critical** findings
  (AGENTS.md rule 9); new screens are added to it.
- Keyboard: ⌘K and ⌘J work from anywhere; ⌘K results are navigable by
  keyboard; the setup board's moving cards must remain operable (and must
  honour `prefers-reduced-motion`, UX-06).
- Captions/text equivalents: the now-speaking line is the caption for the
  audio; every voice clip keeps its text twin (existing UX-robustness rule).

## Placeholder copy

Any line of quoted text in this spec or in tests is an invented placeholder
("Placeholder line for the speaking character."). The handoff's mocks quote
real game dialogue; that text must not be copied into the repo (AGENTS.md rule
5). Character and place names used here are examples only.
