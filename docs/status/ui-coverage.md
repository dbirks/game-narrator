# UI route × spec coverage (docs/spec/16 M5)

Inventory of every route in `web/src/App.tsx` against the `web/e2e/*.spec.ts`
that asserts its **visible state** (not just an HTTP 200 — AGENTS.md "browser
tests assert visible state"), plus the M5 acceptance items. Generated
2026-10-18 after the M5 route-coverage milestone.

Servers: `chromium` = Vite dev server + `go run` API (port 18090); every other
project runs the **packaged binary** (`make build`) on its own throwaway data
directory (`web/e2e/serve.mjs`), with the scripted TTS/model instances
(`fake-tts.mjs`, `fake-model.mjs`) where a spec's comment says so — synthetic
stubs at the model boundary only (AGENTS.md rule 3). Every port is derived
from one per-run base (`GN_E2E_PORT_BASE`; default 18090, so the numbers
quoted in this doc hold — `auto` probes for a free 30-port band at config
load, and the run's manifests live under its per-run root in `os.tmpdir()`,
ADR-0094), so several `make test-ui`-style Playwright runs can share one
checkout at once. `a11y` (port 18108) is
covered by the Accessibility section below: axe over every route of this
matrix, in the states those specs establish.

## Route matrix

| Route | Covered by | What is asserted (visible state) |
|---|---|---|
| `/` (redirect) | `m5-settings-tests`, `route-coverage` | unclaimed: `/` lands on `/setup`; after sign-in the remembered target lands on `/library` with the nav bar and the "No games yet" empty state |
| `/setup` | `route-coverage`, `m5-settings-tests`, `catalog` | claim form visible on an unclaimed appliance; claim → `/login` (no session minted); on a claimed appliance the page itself routes to `/login` |
| `/login` | `route-coverage`, `catalog` (+ every walk) | login form visible; wrong credentials show the server's error text; `?next=` target survives sign-out and sign-in |
| `/diagnostics` | `diagnostics` (dev server), `embedded` (packaged), `m5-part1` | readiness `ok` with per-check detail (transient probe 503s retried), model-health panel, version/commit; reachable anonymously (`route-coverage`) |
| `/settings` | `m5-settings-tests` | all role rows render NOT CONFIGURED/CONFIGURED/NOT AVAILABLE with no endpoint or token material in the DOM; storage/audio/security/imports/version sections; backup download is a real SQLite file (16-byte header asserted on the bytes) |
| `/tests` | `m5-settings-tests` | scenario catalog lists a blocked row with its reason (the dolphin fixture, whose frames are unreachable today) and the unblocked GN-T047 as runnable; running the synthetic scenario reports PASS with a succeeded state; the report opens in a `sandbox=""` iframe and shows PASS |
| `/replay-clips` | `replay-clips-walk` | the managed clip store (ADR-0073): the committed synthetic clip uploaded through the page lands as a row with its frame count and the uploaded archive's own SHA-256 (provenance, checked against the bytes the browser sent); the Play page's clip picker lists it and the session runs it by id on a server with NO replay allowlist; the delete empties the store back to its empty state |
| `/traces` | `traces`, `developer` | the run's committed lines as a filterable list (spoken/skipped/slow), each line's 5-step strip (Saw → Read → Matched → Who → Spoke, +ms) matching the API's per-trace record, what it read (crop + raw/normalised OCR), why this line (candidate bars + reason), the actions (replay audio, wrong speaker → the agent's confirmation gate, save as test, download evidence); the session inspector is the preserved surface at `?view=inspector` (ADR-0141): session state `stopped`, the commit events (message ids, speech-eligible flags), frame/ROI/crop samples, OCR normalized text + confidence, ranked candidates table |
| `/library` | `catalog`, `voices`, `onboarding`, `route-coverage` | "No games yet" empty state; created game card renders with its title; sign-out makes the route redirect to `/login?next=/library` |
| `/games/new` | `onboarding` | the setup agent's form and streamed tool timeline: import + quality tools succeed with sources shown, the draft goes draft → published through the approve action, the game then appears in `/library`; malicious-page variant never calls the non-existent tool and says so |
| `/games/:id/overview` | `catalog`, `curation-walk` | game title; edition tab after creation; revision rows with status badges; publish refused for an empty revision ("no messages") and succeeding for the real one; export download; manifest re-import report (ADR-0056) |
| `/games/:id/dialogue` | `catalog`, `curation-walk` | paginated table (exactly 50 rows per page), search results scoped, speech edit saved via the page; stale-token conflict refused with the server's words and the typed text preserved; frozen revision refuses with "frozen" and offers derive; re-imported draft carries the edited line |
| `/games/:id/voices` | `voices`, `voice-preview-audio`, `curation-walk` | roster radios; 3 audition cards with players + transcripts; consent gate (button disabled until consent); approval badge + fingerprint + consent record; registration pending → ready with the instance name; preview panel with the cache decision |
| `/games/:id/characters` | `m5-part1`, `curation-walk` | roster table with per-character line counts and verified/inferred attribution badges; links to the voices page |
| `/games/:id/graph` | `m5-graph-regions` | seeded lines render in the node list with attribution labels; legend carries the five edge kinds; the speaker filter is server-side (the count drops to the speaker's lines) |
| `/games/:id/regions` | `m5-graph-regions`, `curation-walk` | stored box loads; a second box drawn on the canvas; "Preview OCR" runs the real tesseract over the fixture frame and reads the recorded line (four variants per box); save with If-Match; a reload shows the saved set |
| `/games/:id/publish` | `m5-part1`, `corrections-fixtures` | diff panel ("1 changed / 0 added / 0 removed" with the changed row's text); draft → review → publish transaction lands with the history row "published revision 2" and a success badge; the diff then empties and the rollback button names the superseded revision |
| `/play` | `m5-part1`, `sse-reconnect` | session start on the replay clip; committed lines with message ids and roster speakers; speech counters honest (requested / ended skipped when no approved voice); audio controls applied live and surviving a reload; session stops with `stats.commits == 2` |
| `*` (not found) | `embedded`, `route-coverage` | deep link `/definitely/not/a/route` renders the "Not found" heading with the back-to-Diagnostics link; reachable anonymously |

## M5 acceptance items (docs/spec/16)

| Acceptance item | Spec | Proof |
|---|---|---|
| Playwright coverage of each route | matrix above | every route has at least one visible-state assertion; the first-run/anonymous phases that no other spec reaches are covered by `route-coverage` on a fresh appliance |
| Correction produces a regression fixture | `corrections-fixtures` (ADR-0052) | the editor's save records the `corrections` row (before/after, actor, `accepted`); `GET /api/v1/corrections` and the developer's `GET /api/v1/dev/fixtures` serve the same rows; a viewer gets 403 |
| SSE reconnect reflects accurate state | `sse-reconnect` | the reload tears the in-flight stream down; the new `/api/v1/events` request is observed carrying the session's own `event_cursor` as `Last-Event-ID` (every subsequent reconnect ≥ that cursor); after the session settles, the page's state, commits, OCR count and committed lines are diffed against the server's final snapshot |
| Voice preview works | `voice-preview-audio` | the `<audio>` elements point at `blob:` URLs whose metadata loads (`duration > 0` — the browser decoded the payload); the server's bytes for the same audition/preview are non-empty RIFF/WAV; the preview answers MISS then HIT on the identical line |
| Curate a full synthetic game with no CLI | `curation-walk` | one spec, UI only (no API writes, no CLI): create → edition → draft → import synthetic script (40 rows) → edit a line → characters → voice (audition/approve/register/preview) → regions (draw/OCR/save) → publish → export `.gnprofile.json` → **re-import the export** through the overview's manifest control (ADR-0056), asserting the new draft carries the edited line's text and the export's own digest |

## Accessibility (axe)

`@axe-core/playwright@4.13.0` (exact; published 2026-08-11, well over two weeks
old — 4.14.0 was too new when this was added). License: MPL-2.0 (OSI-approved,
file-scoped copyleft; devDependency only — it is not bundled into the shipped
binary, and the product's users never load it). `web/e2e/a11y.spec.ts` runs on
its own Playwright project (the packaged binary, a fresh appliance that the
spec claims and seeds through the UI, port 18108), visits every route of the
matrix in the state the other specs establish (first-run/anonymous phases,
then the seeded synthetic game as the signed-in admin, then signed-out), scans
each with axe's WCAG 2.1 A/AA tags (`wcag2a`, `wcag2aa`, `wcag21a`, `wcag21aa`),
attaches the per-route JSON to the report (`a11y-results`), and fails on any
`serious`/`critical` violation. Run: `pnpm exec playwright test a11y` (needs
`make build` first).

Per-route violating-node counts (the only rule in play; moderate/minor were
absent in both runs):

| Route | color-contrast before | color-contrast after |
|---|---|---|
| `/setup` (first run) | 0 | 0 |
| `/diagnostics` (anonymous) | 0 | 0 |
| `*` not-found (anonymous) | 0 | 0 |
| `/` → `/library` | 1 | 0 |
| `/games/new` | 0 | 0 |
| `/games/:id/overview` | 1 | 0 |
| `/games/:id/dialogue` | 2 | 0 |
| `/games/:id/voices` | 6 | 0 |
| `/games/:id/characters` | 3 | 0 |
| `/games/:id/graph` | 4–7 | 0 |
| `/games/:id/regions` | 0 | 0 |
| `/games/:id/publish` | 0 | 0 |
| `/play` | 0 | 0 |
| `/settings` | 0 | 0 |
| `/tests` | 0 | 0 |
| `/replay-clips` | 0 | 0 |
| `/traces?view=inspector` | 0 | 0 |
| `/diagnostics` (signed in) | 0 | 0 |
| `/login` (signed out) | 0 | 0 |
| `/setup` (claimed → `/login`) | 0 | 0 |
| **total serious/critical** | **18** | **0** |

(The graph count varies 4–7 between runs: the node list is paginated and the
speaker filter changes which `verified` badges page one carries.)

What the before-run's 18 nodes were, and the fixes in `web/src`:

- 11× links/badges rendering at `--accent`×0.7 (`#6d9af1`, 2.78:1) or `--ok`×0.7
  (`#60a37e`, 2.98:1): `.muted { opacity: 0.7 }` faded the accent links and the
  `badge-published` badges that sit inside `.muted` paragraphs (library card
  slug, the overview/dialogue/characters page headers, the voices back link).
  Fix: `.muted` now uses a real color token `--muted: light-dark(#4b5058,
  #aab1bc)` (≥8:1 on `--bg` in both themes) instead of opacity — the links and
  badges inside keep their own colors.
- 5× `.badge-draft` at `--warn` `#a2740b` (4.16:1 on white): the voices page's
  per-character status badges. Fix: `--warn: light-dark(#806000, #d9a514)`
  (5.8:1 light / 8.1:1 dark). `--ok` and `--danger` got the same treatment
  (`light-dark(#1c7c46, #4ade80)`, `light-dark(#c0392b, #f87171)`) because they
  double as badge text and fail 4.5:1 on the dark `--bg`.
- 4–7× graph attribution badges at hard-coded `#059669` (3.36–3.76:1): now
  `var(--ok)`; `.attr-badge.inferred` and the graph speaker/selected-row colors
  were de-hardcoded the same way (`light-dark(#5b6470, #9ca3af)` — the light
  value must also hold 4.5:1 on the selected row's `#eef2ff`, `var(--muted)`,
  selected row `light-dark(#eef2ff, #1e2430)`).
- Latent (not in the walk's states, fixed the same pass): the regions canvas
  placeholder `#9ca3af` (2.54:1 on its hard-coded `#111` canvas) → `#aab1bc`
  (8.7:1, both themes — the canvas is dark in both).

The two pages whose back links had no testid (noted here, added this pass):
`/games/:id/characters` carries `characters-back-link` on its Overview link,
and `/games/:id/regions` gained a back link with `regions-back-link` (the page
previously had none — the curation walk used the browser history).
