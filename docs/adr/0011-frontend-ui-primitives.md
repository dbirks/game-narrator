# ADR-0011: Frontend UI primitives — Radix Primitives (per-component), plain CSS, virtualized tables
Date: 2026-10-16
Status: proposed
Owners: project
Related requirements/tests: FR-01, FR-11, FR-12, FR-14; docs/spec/09 "Technology" ("lightweight UI component system (Radix-style accessible primitives or similar)"), docs/spec/09 "Routes and page requirements", docs/spec/09 "UX robustness" (keyboard-accessible tabs and dialogs, correct empty/loading/error states, "don't load thousands of messages into React memory"), docs/spec/09 "Embedded binary test", docs/spec/12 "Unit"/"UI" modes, docs/spec/16 Milestone 0 ("Write ADRs for … frontend UI kit"); `pnpm --dir web test`, `make test-ui`

## Context and constraints

- docs/spec/09 asks for *accessible primitives*, not a styled component kit.
  The named requirements are explicit: "Keyboard accessible tabs, dialogs" plus
  an "interactive graph alternative in table view" (docs/spec/09 "UX robustness"),
  and `/play` requires "volume/speech delay, pause/mute ... conservative/predictive
  toggle" — i.e. a slider and a toggle. Menus, selects and toasts are the
  inferred remainder of the same page list (`/games/:id/regions` aspect/resolution
  presets, `/library` row actions, the dashboard's queue/blocked-reason feedback
  on long jobs), so they are priced here as requirements the kit must cover.
- Styling is our own. The bundle is served under a strict CSP
  (`staticCSP` in `internal/api/static.go`: `style-src 'self'`, no inline or
  remote styles), so any approach that needs an injected or remote stylesheet is
  disqualified before it is evaluated.
- React 19.3.1 / react-dom 19.3.1 (`web/package.json`), Vite 8, TypeScript 7,
  `@tanstack/react-query` 5 and `react-router` 8 are already chosen by
  docs/spec/09; no UI kit is installed today — the dependency block carries only
  those four plus React itself.
- The 4 GB UNO Q target and the "don't load thousands of messages into React
  memory" rule (docs/spec/09 "UX robustness") make per-component, tree-shakeable
  imports and virtualization functional requirements, not preferences.
- Test surface already fixed: `vitest` + `@testing-library/react` + `jsdom` for
  unit, Playwright for real keyboard interaction in a browser.
- No page exists yet (M5 builds them), so this ADR *proposes* the kit; it becomes
  `accepted` when the first M5 route imports it and Playwright proves the
  keyboard paths.

## Decision

**Recommend Radix Primitives, imported per component from
`@radix-ui/react-*`** (dialog, tabs, menu, select, slider, tooltip, toggle, and
where needed `popover`/`switch`/`separator`/`visually-hidden`), with these
attached rules:

1. **Do not install the `radix-ui` umbrella barrel.** `radix-ui@1.8.1` declares
   **55** `@radix-ui/*` packages as hard `dependencies` and nothing else, so one
   import of the barrel pulls the whole primitive set into `node_modules` and
   makes the install footprint independent of what the app uses. Per-component
   packages give the same code (`import * as Dialog from "@radix-ui/react-dialog"`)
   with a closure that matches the components actually used.
2. **Plain CSS with CSS custom properties. No CSS framework.** Tokens (colour,
   spacing, radii, focus-ring, z-index, duration) live in one `:root` block;
   components style themselves from `[data-state]`, `[data-open]`,
   `[data-disabled]`, `[aria-*]` and `role` attribute selectors, which Radix
   emits — the reason unstyled primitives are a good fit for hand-written CSS.
   No utility-class build step, no preprocessor, no runtime style injection
   (CSP-safe).
3. **Toasts are an explicit gap, decided at M5.** Radix ships `Toast`
   (`@radix-ui/react-toast@1.3.1`, still exported by `radix-ui@1.8.1`) but the
   ecosystem has moved: shadcn/ui deprecated its Radix-based toast in favour of
   Sonner. Whichever way M5 goes, the requirement is the *queue* semantics in
   docs/spec/09 (SSE-driven job progress, no duplicate speech/UI remount noise),
   so the toast store will be a small app-level reducer regardless of renderer.
4. **Data tables and graph views are virtualized, library choice deferred.**
   Neither Radix nor Headless UI provides them; React Aria Components does
   (`Table`, `Virtualizer`). The candidate is TanStack Virtual, which shares the
   already-adopted TanStack Query's mental model and pairs with server pagination
   from docs/spec/08. Recorded here as a requirement with an open implementation
   choice; the graph-view alternative stays a real `<table>` with keyboard focus
   (docs/spec/09 "interactive graph alternative in table view").
5. No shadcn/ui: it is Radix *plus Tailwind plus their design opinions*, and its
   generated source is copied into the app — a styling framework and a code
   lineage we did not ask for.

## Alternatives considered

- **React Aria Components `1.22.1`** — the strongest coverage: its published
  file list exports 75 components including `Dialog`/`Modal`, `Tabs`, `Menu`,
  `Select`, `Slider`, `Tooltip`, `ToggleButton`, **plus `Toast`, `Table`,
  `Virtualizer`, `Meter`, `NumberField`, `Tree`, `NavigationTree`**, so gaps 3
  and 4 disappear. It also ships `i18n/` (~544 KB of locale strings in the
  package) which is what makes screen-reader announcements translatable.
  Rejected as the default on footprint and coupling: **6,935,582 bytes unpacked
  across 1,446 files** in one package (vs. ~1.3 MB for the eight Radix component
  packages we need), a transitive root of `react-aria` + `react-stately` +
  `@react-types/shared` + `@internationalized/*`, a render-prop
  `composeRenderProps` styling model that we would mostly bypass with our own CSS,
  and a peer range written as `^19.0.0-rc.1` (admits 19.3.1 under semver, but no
  stable-19 peer string). If M5 finds ourselves rebuilding tables and toasts on
  top of Radix, RAC is the superseding candidate and this ADR gets replaced with
  the bundle measurements that forced it.
- **Headless UI `@headlessui/react@2.2.10`** — smallest of the three styled-adjacent
  options (1,017,854 bytes unpacked, 329 files), good Dialog/Combobox/Listbox/Menu/
  Tabs/Switch quality. **Rejected on capability**: the published ESM entry re-exports
  exactly `button, checkbox, close-button, combobox, data-interactive, description,
  dialog, disclosure, field, fieldset, focus-trap, input, label, legend, listbox,
  menu, popover, portal, radio-group, select, switch, tabs, textarea, transition` —
  there is **no Slider, no Toast, no Tooltip and no Toggle**, i.e. four of the
  eight primitives priced above are missing, and there is no table or
  virtualization. Its last stable release is also 2026-04-07, six months stale
  against the other two (which released within the last week).
- **A styled kit (MUI, Mantine, Ant Design)** — rejected: bundles a design
  language, CSS-in-JS runtime style injection (CSP `style-src 'self'`), and
  React-19 migration churn, against a spec that explicitly asks for primitives.
- **Hand-rolled primitives** — the app-level toast store in Decision 3 shows we
  are willing to own small state machines, but focus trapping, typeahead,
  roving tabindex, `aria-activedescendant` and disclosure semantics are exactly
  the parts with the worst failure modes and the least product differentiation.
  Rejected except where no library covers our need (toast queue, virtualization).

## Evidence and test plan

Registry facts, read with `pnpm view` on 2026-10-16 (no package was installed;
`web/package.json` and `web/pnpm-lock.yaml` are unchanged):

```
$ pnpm --dir web view radix-ui version peerDependencies
version = '1.8.1'
peerDependencies = {"react":"^16.8 || ^17.0 || ^18.0 || ^19.0 || ^19.0.0-rc","react-dom":"^16.8 || ^17.0 || ^18.0 || ^19.0.0-rc","@types/react":"*","@types/react-dom":"*"}

$ pnpm --dir web view react-aria-components version peerDependencies
version = '1.22.1'
peerDependencies = {"react":"^16.8.0 || ^17.0.0-rc.1 || ^18.0.0 || ^19.0.0-rc.1","react-dom":"^16.8.0 || ^17.0.0-rc.1 || ^18.0.0 || ^19.0.0-rc.1"}

$ pnpm --dir web view @headlessui/react version peerDependencies
version = '2.2.10'
peerDependencies = {"react":"^18 || ^19 || ^19.0.0-rc","react-dom":"^18 || ^19 || ^19.0.0-rc"}
```

Footprint and maintenance, same source:

| Package | version | unpackedSize | files | latest stable published |
| --- | --- | --- | --- | --- |
| `radix-ui` (umbrella, **not adopted**) | 1.8.1 | 105,530 B (re-export shim; 55 `@radix-ui/*` deps) | 262 | 2026-10-12 |
| `react-aria-components` | 1.22.1 | 6,935,582 B | 1,446 | 2026-10-09 |
| `@headlessui/react` | 2.2.10 | 1,017,854 B | 329 | 2026-04-07 |

Adopted shape — the primitives priced individually
(`pnpm --dir web view @radix-ui/react-<name>@<ver> dist.unpackedSize`):
`react-dialog@1.3.1` 107,250 · `react-tabs@1.2.1` 55,797 · `react-menu@2.2.1`
261,485 · `react-select@2.4.1` 371,812 · `react-slider@1.6.1` 187,895 ·
`react-toast@1.3.1` 193,487 · `react-tooltip@1.4.1` 143,302 ·
`react-toggle@1.2.1` 15,166 = **1,336,194 B on disk total** (CJS + ESM + types;
not a browser bundle figure). `radix-ui@1.8.1` re-exports all of these, and its
dependency list was the source of the per-component versions.

Coverage of the docs/spec/09 requirement set (explicit + inferred, per Context): Radix = dialog ✓ tabs ✓ menu ✓
select ✓ slider ✓ tooltip ✓ toggle ✓ toast △ (exists; queue is ours) —
Headless UI = dialog ✓ tabs ✓ menu ✓ select/listbox ✓ slider ✗ tooltip ✗
toggle ✗ toast ✗ — React Aria = all ✓.

External corroboration for Decision 3: shadcn/ui's Radix toast page states
"The toast component has been deprecated. Use the sonner component instead"
(https://ui.shadcn.com/docs/components/radix/toast). Radix's own Toast docs
(https://www.radix-ui.com/primitives/docs/components/toast) are still live.

Plan to convert this ADR to `accepted` (M5, first page that needs it):

1. `pnpm --dir web add @radix-ui/react-dialog @radix-ui/react-tabs …` (per
   component only), commit the lockfile, and record the real `vite build` output
   sizes before/after — unpackedSize above is an install footprint, **not** a
   bundle measurement, and the bundle delta is the number that can reopen this.
2. Playwright keyboard suite per docs/spec/09 "UX robustness": Tab/Shift-Tab
   reaches every control, Escape closes dialogs and menus without trapping focus,
   arrow keys move within tabs/menus, the slider is adjustable by keyboard and
   announces its value, focus returns to the trigger on close.
3. Accessibility assertions in `vitest` (`@testing-library/react` + jsdom): roles
   and `aria-*` on the styled wrappers, plus the required empty/loading/error
   states for each primitive.
4. Verify under the CSP the binary actually sends (`staticCSP`, `make test-ui` /
   `web/e2e/embedded.spec.ts`) — no style injection from any library.
5. `pnpm --dir web typecheck && pnpm --dir web lint && pnpm --dir web test`.

## Consequences and risks

Positive: a11y semantics for the hard parts come from a maintained library whose
data attributes are the styling contract; per-component installs keep the
footprint proportional to what pages use; plain CSS + custom properties keeps the
theme in one readable file, survives the CSP, and adds no build step; the
documented gaps (toast queue, virtualization) are named now instead of being
discovered mid-M5.

Costs and risks:

- **Many packages, many versions.** Radix releases are per-package (dialog 1.3.1
  and tooltip 1.4.1 do not share a version line), so upgrades are a set, not a
  number. Mitigation: pin in `pnpm-lock.yaml`, review with `pnpm --dir web
  outdated`, prefer the umbrella *only* as a version oracle.
- React 19 is inside every candidate's peer range as published above, but
  adoption is unverified until the real install happens — a peer warning during
  step 1 is evidence for this ADR, not a surprise to absorb silently.
- Toast: choosing Sonner later means a non-Radix dependency and a second styling
  contract; choosing Radix Toast means owning a queue on top of it. Either way
  the store is ours, so the risk is bounded to presentation.
- Virtualization is a correctness requirement for the 10k-message benchmark
  (docs/spec/12, docs/spec/16 M1 acceptance "search 10k synthetic messages
  without memory blowup"); picking the library late is the main schedule risk in
  this ADR.
- Unstyled primitives shift contrast/focus-visible correctness to our CSS; the
  Playwright/accessibility suite in step 2-3 is the mitigation, not a review.

## Migration / rollback

Purely additive frontend dependency change with no persistence, API or protocol
surface. Rollback before adoption: delete the entries from `web/package.json`
and the lockfile (no code exists yet that imports them). Rollback after adoption:
components are thin wrappers in `web/src/components/` around the primitives, so
swapping Radix for React Aria Components touches those wrappers plus the
`[data-state]` → `is*` render-prop styling convention; CSS custom properties and
the token file survive the swap unchanged. If we end up hand-rolling a primitive
because a library fights the CSP or a design constraint, that decision gets its
own ADR entry with the failing Playwright test as evidence.

## Follow-ups

- Accept (or supersede) this ADR at the first M5 page build with measured
  `vite build` gzip deltas per added primitive.
- Decide the toast renderer (Radix Toast vs Sonner vs hand-rolled `aria-live`
  region) together with the event-bus queue design in docs/spec/08.
- Choose the virtualization library for message dumps and graph tables with a
  memory measurement against 10k rows, not a feature table.
- Write the token layer (`:root` custom properties, `prefers-reduced-motion`,
  forced-colours/`prefers-contrast` behaviour) — docs/spec/09 accessibility claims
  are currently library claims, not project claims.
- Confirm accessibility-tree assertions in CI (Playwright `aria` snapshot or an
  axe run) and record which is used.
- Re-check React Aria's stable-19 peer declaration at adoption time.
- Review when Milestone 5 starts.
