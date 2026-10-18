# UI route × spec coverage (docs/spec/16 M5)

Inventory of every route in `web/src/App.tsx` against the `web/e2e/*.spec.ts`
that asserts its **visible state** (not just an HTTP 200 — AGENTS.md "browser
tests assert visible state"), plus the M5 acceptance items. Generated
2026-10-18 after the M5 route-coverage milestone.

Servers: `chromium` = Vite dev server + `go run` API (port 18090); every other
project runs the **packaged binary** (`make build`) on its own throwaway data
directory (`web/e2e/serve.mjs`), with the scripted TTS/model instances
(`fake-tts.mjs`, `fake-model.mjs`) where a spec's comment says so — synthetic
stubs at the model boundary only (AGENTS.md rule 3).

## Route matrix

| Route | Covered by | What is asserted (visible state) |
|---|---|---|
| `/` (redirect) | `m5-settings-tests`, `route-coverage` | unclaimed: `/` lands on `/setup`; after sign-in the remembered target lands on `/library` with the nav bar and the "No games yet" empty state |
| `/setup` | `route-coverage`, `m5-settings-tests`, `catalog` | claim form visible on an unclaimed appliance; claim → `/login` (no session minted); on a claimed appliance the page itself routes to `/login` |
| `/login` | `route-coverage`, `catalog` (+ every walk) | login form visible; wrong credentials show the server's error text; `?next=` target survives sign-out and sign-in |
| `/diagnostics` | `diagnostics` (dev server), `embedded` (packaged), `m5-part1` | readiness `ok` with per-check detail (transient probe 503s retried), model-health panel, version/commit; reachable anonymously (`route-coverage`) |
| `/settings` | `m5-settings-tests` | all role rows render NOT CONFIGURED/CONFIGURED/NOT AVAILABLE with no endpoint or token material in the DOM; storage/audio/security/imports/version sections; backup download is a real SQLite file (16-byte header asserted on the bytes) |
| `/tests` | `m5-settings-tests` | scenario catalog lists the blocked GN-T047 row with its reason; running the synthetic scenario reports PASS with a succeeded state; the report opens in a `sandbox=""` iframe and shows PASS |
| `/developer` | `developer` | inspector over a real replay session: session state `stopped`, the commit events (message ids, speech-eligible flags), frame/ROI/crop samples, OCR normalized text + confidence, ranked candidates table |
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

`@axe-core/playwright` is **not** a dev dependency of `web/package.json`, so per
the milestone's rule axe was skipped and is recorded here as **not run** — no
violation counts exist yet. Adding it is a one-line dev dependency plus a
per-route loop over the matrix's first column; the pages already render text
equivalents beside the audio controls (docs/spec/09 "UX robustness"), which
the matrix's assertions exercise.
