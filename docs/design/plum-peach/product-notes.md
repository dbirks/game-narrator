# Plum & Peach — product notes (functional requirements)

Functional requirements implied by the design review, written as testable
requirements. Each has an ID, a statement, and acceptance criteria a
Playwright spec (or API test) can assert. Visual detail lives in `README.md`;
this file is about behaviour. IDs are stable — reference them in tests and
the plan.

## Agent presence and context

### UX-01 — Agent on every screen, context-aware

The agent (⌘J dock) is present and functional on every screen. Every agent
message carries a context payload
`{route, gameId, profileId, selection: {type, id}, visibleIds[]}` describing
the current screen and selection, so instructions resolve against it ("make
her older" means the selected character; "add this to the game" means the
current game).

Acceptance:
- With the dock open on `/games/:id/cast?character=:cid`, sending a message
  results in an agent request whose body contains `route`, `gameId` = `:id`,
  `selection = {type: "character", id: ":cid"}` (assert via API capture /
  server logs, redacted).
- The dock's context chip text matches the current screen/selection and
  updates when the selection changes (assert visible text by
  `data-testid`/role, not styles).
- On screens with no selection, `selection` is null/absent and the chip shows
  the bare screen name.

### UX-02 — ⌘K general search

⌘K opens a general search overlay from any screen, searching **games,
characters, dialogue lines, traces, settings and actions** (not just a game
picker). Results are grouped; ↑↓ move, ↵ opens the result, **Tab hands the
query to the agent** (pre-filled in the dock input). Adding a game is not
reachable from ⌘K — it lives in Library.

Acceptance:
- Pressing ⌘K on any of library/play/traces/settings/health opens the overlay
  (role=dialog) without navigating.
- A query returns grouped results spanning at least two of the six
  categories (seed data guarantees e.g. one character and one line match).
- ↵ on a character row navigates to its Cast detail; ↵ on an action row
  executes the action; ↵ on a line row opens its trace.
- Pressing Tab with a query sends that query to the agent dock (assert the
  dock input value and the outgoing agent message).

## Casting

### UX-03 — Auto-cast by default

Auto-cast is ON by default for new setups: every character receives **one
take, auto-approved** without a per-character prompt. The existing voice
consent requirement is satisfied by a **per-profile consent default** (e.g.
`original_synthetic_design`), not a per-click prompt. Extra takes are opt-in
("3 more takes"). Editing the brief and regenerating produces a new take.

Acceptance:
- Starting a setup run with auto-cast on produces, at completion, exactly one
  approved voice version per character with zero pending approvals (assert via
  API: voice versions, approval state).
- The consent response recorded for the profile shows the default consent
  source (assert the consent field is non-null and attributed to the
  profile-level default, not a user click).
- Toggling auto-cast off in Cast before/after the run leaves characters
  unvoiced or pending approval respectively.
- "3 more takes" queues additional takes; the UI shows their state (queued /
  ready) by `data-testid`.

### UX-03d — Regenerate keeps the previous take as undo

Regenerating a voice keeps the previous take available as a **one-step undo**
(the previous version is not deleted; the new take becomes current).

Acceptance:
- After regenerating a character's voice, the API lists two versions: the new
  current one and the previous one, retrievable and playable (assert both
  audio responses are 200 with bytes).
- The UI offers a one-step "keep old take / undo" affordance after
  regeneration that restores the previous version as current (assert version
  state after the click).

## Agent collaboration

### UX-04 — Non-blocking agent questions

Agent questions raised during a run (NEEDS YOU) do **not** pause the run: the
run keeps advancing, and the answer is applied when (and if) given. A
question can be deferred with "or keep going".

Acceptance:
- With a seeded ambiguous case (two name variants), the setup run reaches
  later stages while the question is unanswered (assert board stages advanced
  and the question still open).
- Answering after deferral changes the recorded entity (e.g. merged
  characters) and the run continues from where it left off without restart.

### UX-05 — Corrections feed back to the agent

When the user marks "wrong speaker" or "wrong text" (from Play or a trace),
the in-app agent checks whether **similar lines were also wrong** (same
speaker plate, same scene, same OCR pattern) and then either fixes them in
bulk or asks the user before doing so. Corrections are user-confirmed before
any bulk change lands.

Acceptance:
- Marking "wrong speaker" on a line triggers an agent suggestion naming the
  affected similar lines (assert the suggestion message lists ≥1 other line
  id when seed data has a matching pattern).
- Bulk application happens only after explicit user confirmation (assert no
  speaker changes in the DB until the confirm action; the corrected line is
  changed immediately).
- The same flow exists from a trace page action and from the Play operator
  view.

## Setup run

### UX-06 — Live setup board driven by SSE

The setup board shows each character's stage
`stage ∈ {finding, brief, voicing, done}` and updates from **server SSE
stage events** (not polling). Cards glide between columns on change only,
with a coral border while voicing, and the animation is disabled under
`prefers-reduced-motion` (cards reposition instantly).

Acceptance:
- Injecting stage events via the (fake) SSE feed moves the corresponding card
  to the matching column; no card moves without an event (assert DOM
  position/column membership by `data-testid`).
- The browser network panel / test shows no polling requests to the board
  state while the board updates.
- With `prefers-reduced-motion: reduce` emulated, the same events reposition
  cards with no transition (assert the computed transition is
  `none`/instant, or assert the test flag disables transitions).

### UX-07 — One live status stream feeds the status bar

A single live status feed (SSE) drives the status bar string across all
pages, composed from: session state (idle / setting up / live), capture fps,
frame→voice latency, setup progress (n of N, %), and any non-ok health tile.
Non-ok health surfaces on every page, not just Health.

Acceptance:
- The status bar text updates reactively to: a setup job reaching a new
  voiced count, a live session starting (fps + frame→voice latency appear),
  and a health tile going non-ok (its tile appears in the string).
- Exactly one SSE stream subscription per page powers the status bar (assert
  in the UI state/log, not by counting raw connections).

## Views and theming

### UX-08 — Per-user view toggles

Library `shelf|list` and Cast `list|cards` toggles are persisted **per user**
and restored on reload and across browsers for the same user.

Acceptance:
- Switching library to list, reloading, and visiting Cast→list again each
  restore the last choice (assert visible view state by `data-testid`, not
  styles).

### UX-09 — Themes plus a theme page

All colours come from tokens; a **theme page** lets the user pick among Plum
& Peach (default, light), Ink Blue & Apricot, Sherbet, and Lilac Night (dark).
Switching themes re-themes the whole app without reload and persists the
choice. Derived theme slots (marked in `README.md`) are reviewable on the
theme page.

Acceptance:
- Selecting Lilac Night changes `--ink`/`--paper`/etc. on `:root` and the
  choice survives reload (assert CSS custom property values + visible theme
  page state).
- No component style contains a raw hex colour (lint/audit over
  `web/src/**` — the token set is the only colour source).

### UX-10 — Kid/tablet mode (low priority)

`/play?mode=kid` renders the minimal play view: large −/pause/+ buttons,
portrait, name, and current line; no other controls. Exit policy (e.g. PIN)
is an open detail.

Acceptance:
- Navigating to `/play?mode=kid` during a session shows only the three
  controls plus line/portrait (assert no other interactive controls by role).
- Pause and volume work in kid mode (session state changes; assert via API).

## Appliance-specific (learned on real hardware)

### UX-11 — HDMI signal and active-picture preview (Health and Play)

The UNO Q captures through an external UVC HDMI path (GameCube →
RCA-to-HDMI → splitter → UVC dongle), so "card connected" is not enough:
Health and Play must show **HDMI signal present** and an **active picture**
preview (non-black / changing frame), so a dead cable or sleeping converter
is visible immediately.

Acceptance:
- With a black/static feed, the Health capture tile and the Play frame
  indicate "no active picture" (assert status text by `data-testid`).
- With a changing fixture feed, both show an active picture; the Health tile
  reports signal present.
- The signal/active state is part of the live status stream (UX-07).

### UX-12 — Audio output selector with test tone (HDMI hub, USB, Bluetooth)

Settings offers an **audio output selector** covering the appliance's real
sinks — the hub's HDMI port (AV receiver), USB DAC, and Bluetooth
headphones/speakers (issues #2, #3, #5) — with a **test tone** so the user
can verify which output they actually hear, plus pairing/selection support
for Bluetooth.

Acceptance:
- The selector lists the discovered sinks (HDMI, USB, Bluetooth categories)
  and selecting one changes the configured audio sink (assert via API).
- Pressing "play test tone" produces an audible/streaming tone on the
  selected sink (assert the API call and, where available, the audio output
  in the test environment; in CI assert the request and a non-empty response).
- Selecting a Bluetooth sink not yet paired triggers the pairing flow rather
  than a silent failure (assert an error/prompt, per existing UX-robustness
  rules).

### UX-13 — Tailnet URL shown in settings

Settings shows the device's **tailnet URL** (the Tailscale address for
reaching the web UI over the tailnet), copied from the running system so it
always matches the appliance.

Acceptance:
- The settings page displays a tailnet URL matching the system's Tailscale
  hostname/IP (assert text equality against the API value; when Tailscale is
  absent, an explicit "not configured" state, not a blank field).
