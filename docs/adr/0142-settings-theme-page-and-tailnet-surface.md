# ADR-0142: Settings rework, the theme page, and the tailnet URL surface

Date: 2026-10-21
Status: accepted
Related: ADR 0093 (Plum & Peach design system and IA), ADR 0011 (CSP
`style-src 'self'`), ADR 0074 (audio output surface), ADR 0091 (Bluetooth
output), docs/design/plum-peach/plan.md item 9, product-notes.md UX-09,
UX-12, UX-13

## Context

Phase 1 item 9 of the Plum & Peach plan reworks `/settings` and adds the
theme page (`/settings/theme`). Three binding questions:

1. **Theme picking (UX-09).** ADR-0093 §2 forbids runtime-injected styles
   (ADR-0011), so a theme is a `data-theme` token set in
   `web/src/styles/tokens.css`, selected on `<html>`. The Phase 0 seam
   (`web/src/lib/theme.ts`, `localStorage["gn-theme"]`, applied at startup
   in `src/main.tsx`) owns the storage key and the apply step. The page must
   pick among the four themes with a live preview and survive reload, and the
   handoff's *derived* token slots (the coral/sand/hair of Ink Blue & Apricot
   and Sherbet, marked in the design README) must be reviewable on the page.

2. **The tailnet URL (UX-13).** The appliance has no screen; the UI is
   reached over the tailnet. The settings page must show the URL *copied from
   the running system* so it always matches the appliance, or an explicit
   "not configured" state — never a blank field. No such endpoint existed;
   the closest existing fact is the status stream's `device.listen_addr`
   (the bound address when the host wired it).

3. **The audio selector (UX-12).** The existing audio surface (ADR-0074)
   lists enumerated devices with their kind and persists the selection; the
   design requires the selector to present the appliance's real sink
   categories (the hub's HDMI port, USB DAC, Bluetooth) and to surface the
   pairing flow for an unpaired Bluetooth device instead of failing silently.

## Decision

1. **`/settings` is restyled over the component kit; its e2e contract is
   preserved.** The rework keeps every existing `data-testid` and role
   (ADR-0093's validation rule: selectors are by testid and role, and the
   existing specs stay green). Sections render as kit `Card`s with the kit
   `Table` for the model-role rows; the audio and Bluetooth sections gain
   the category grouping below. New sections: **Theme** (link to the theme
   page, current theme named) and **Network** (the tailnet URL). The page
   stays admin-gated by the API as today (ADR-0047).

2. **The theme page lives at `/settings/theme`** (under the settings nav
   item, per the plan's route seam) and renders the four themes as cards:
   each card carries a static swatch preview (a nested `data-theme` element,
   which the token blocks style — no injected CSS), the theme's character
   note, and a "Use this theme" action. Selecting a theme calls
   `setTheme` (the Phase 0 seam): the choice applies to `<html>` **without a
   reload** (the live preview — the whole app re-themes at once) and persists
   to `localStorage` (the reload proof). The current theme is marked
   "selected" on its card. A **token slot table** renders every theme's
   eight slots (ink, paper, card, peach, coral, muted, sand, hair) with the
   README's *derived* slots flagged, so the derived values the handoff left
   to the review pass are visible and reviewable on the page (UX-09's last
   clause).

3. **The tailnet URL is a `network` section of `GET /api/v1/settings`**
   (admin role, the same gate as the page that renders it). Resolution is
   the running system, in order:

   1. `GAME_NARRATOR_TAILNET_URL` — an operator pin (source `"env"`). The
      appliance's env file can carry the stable `*.ts.net` name; the pin
      wins so the display never depends on CLI availability.
   2. the live `tailscale status --json` self record (source
      `"tailscale"`): the machine's MagicDNS name from `Self.DNSName` with
      the bound listen port, e.g. `http://uno-q.<tailnet>.ts.net:8080`. The
      probe is the audio package's subprocess posture (ADR-0023): the binary
      is looked up on PATH, runs with a bounded context, and any failure is
      an honest "not configured", never an invented URL.
   3. otherwise `tailnet_url: null` with a note naming the gap ("Tailscale
      is not configured"). The UI renders the note verbatim — an explicit
      not-configured state, not a blank field.

   The URL is an address, not a credential; the admin-gated settings route
   already renders the bound listen address, and the value crosses the
   boundary only from the machine's own tailscale state or the operator's
   own env. It never appears in logs, the public status stream, or
   system-info.

4. **The audio selector presents sink categories and the pairing prompt.**
   The device selector's options are grouped by the enumerated kind — HDMI,
   USB, Bluetooth, other (one `optgroup` each, each carrying a
   `data-testid`) — so the appliance's real sink categories (the hub's HDMI
   port, USB DAC, Bluetooth A2DP sinks) are the selector's shape, as the
   design names them. An unpaired Bluetooth device from the Bluetooth
   inventory (the ADR-0091 surface) appears as a "pair first" option;
   selecting it renders the **pairing prompt** (the device's name, why it
   cannot be the output yet, and a Pair button driving the existing pair
   action) instead of persisting an id that has no sink — the silent-failure
   the acceptance rule rules out. The device table, the persisted selection
   (`PUT /api/v1/settings/audio`) and the consent-gated test tone keep their
   existing contract untouched.

## Tradeoffs

- Restyling in place (keeping testids) costs more CSS discipline than a
  rewrite, but it is the only path that keeps the whole existing e2e suite
  (settings, audio, Bluetooth, capture, restore, a11y) a regression net for
  the rework — ADR-0093's validation demands exactly that.
- Reading `tailscale status` per settings request costs a subprocess when no
  pin is set. The appliance is a single-user appliance, the probe is bounded
  (5 s) and only runs on the admin-gated route; the env pin is the
  documented escape hatch.
- The theme page under `/settings/theme` puts a user-facing theming surface
  under the admin-gated route. Consistent with ADR-0093's nav (the design's
  status bar has one "settings" entry) and with the plan's "route under
  settings" seam; if theming becomes family-facing, the gate moves in a
  later ADR — the page and storage seam need no change.

## Validation

- **New Playwright spec** `web/e2e/settings-theme.spec.ts` (its own
  packaged-binary server with the labelled-synthetic audio seam, the
  labelled-synthetic Bluetooth controller, the filesink tone recorder and
  the tailnet pin): UX-09 (selecting Lilac Night switches `--ink`/`--paper`
  on `:root` and the choice survives reload; the token slot table marks the
  derived slots), UX-12 (the selector lists the HDMI/USB/Bluetooth
  categories; selecting the HDMI sink persists it and the API reads it
  back; the test tone request fires and the recorded WAV is the evidence;
  selecting the unpaired Bluetooth device shows the pairing prompt and the
  Pair action pairs it), UX-13 (the displayed tailnet URL equals the
  `GET /api/v1/settings` value; a second server without the pin renders the
  explicit not-configured note). Screenshots at 1440/1024/390, axe at zero
  serious/critical, no horizontal scroll at 390.
- **Go unit tests** for the network section (pin wins; the live derivation;
  the not-configured states) and the existing settings handler tests
  extended with the new field.
- **OpenAPI contract**: `SettingsNetwork` added, `schema.d.ts` regenerated,
  the contract check green.

## Reversal

The tailnet section is one struct field of the settings response: drop it
from the handler, the schema and the page and the route reverts. The theme
page is one route and one page file over the Phase 0 storage seam; removing
it reverts the page, the seam in `theme.ts` stays (Phase 0). The category
grouping and the pairing prompt are selector rendering only — the persisted
selection contract (ADR-0074) is unchanged and reverts by removing the
optgroup/prompt markup.

## Addendum (2026-10-20): visual-review round

The first pass's two defects and their fixes, both CSS-only (no contract,
testid or backend change):

1. **The token slot table collapsed to one column.** `display: grid` on the
   `td` removed the cell from the table layout — a `tr` with no `table-cell`
   children degenerates to a block, so the row's eight cells stacked into a
   single ~32-row column (the header row, unstyled, still read correctly).
   Fix: the cell keeps `table-cell` display (swatch + hex on one line, the
   *derived* chip on the next), and the table carries the kit's table style
   (`.kit-table`): 1px dashed `--hair` separators, mono 11px uppercase
   header. At 390px the grid scrolls inside its existing `.table-overflow`
   wrapper — no page overflow (asserted by the screenshot test's
   scrollWidth check). The spec now asserts the grid shape itself (4 rows ×
   8 side-by-side cells, single-row heights) so the regression is caught,
   not just screenshotted.
2. **The form controls were unstyled native.** Two kit surfaces added in
   `tokens.css` (and to the `/dev/kit` gallery): `.kit-select` (card
   background, 1.5px solid `--ink` border, `--radius-card`, Space Mono) for
   the audio device selector (and the capture device selector, same screen),
   and `.kit-range` for the master volume: `accent-color: var(--coral)`
   keeps the thumb on the token, and the fill — the coral-over-ink two-stop
   gradient on the track — is carried by the input's `data-volume`
   attribute (one rule per step-0.05 value, `--kit-range-fill` 5%…100%).
   ADR-0011 admits no inline styles, so the value crosses into CSS only as
   an attribute the stylesheet reads; `accent-color` remains the fill on
   engines without track pseudo-elements (the `::-moz-range-progress` rule
   is the Firefox half of the same picture).

Validation for the round: the spec's grid assertion, the same
screenshots/axe walk re-run at 1440/1024/390, and the full suite.
