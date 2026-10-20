# ADR-0093: Adopt the "Plum & Peach" design system and new IA for the web UI

Date: 2026-10-20
Status: accepted (design direction); open questions (a)–(e) carry the
orchestrator's provisional recommendations, pending owner review
Owners: project owner (design decision); orchestrator (recorded)
Related: `docs/design/plum-peach/README.md` (design system and screen spec),
`docs/design/plum-peach/product-notes.md` (UX-01…UX-13),
`docs/design/plum-peach/plan.md` (implementation phases),
docs/spec/09 ("Redesign (Plum & Peach)"); ADR 0011 (CSP `style-src 'self'`),
ADR 0086 (Apache-2.0)

## Context

A design review of the current web UI (unstyled system-font HTML in a 60rem
left-hugging column) produced the "Plum & Peach" redesign: a complete design
language (plum ink on peach paper, 1.5px lines, Fredoka + Space Mono, CSS
dither textures, a 28px live status bar, a global ⌘J agent dock and a ⌘K
general search) plus a reorganised information architecture:

- **Cast** merges the Characters page and the Voices page into one
  screen (one cast, one place to review voices).
- **Developer becomes Traces** (`/developer` → `/traces`), named for what it
  shows: per-line traces of the live run.
- **Diagnostics becomes Health** (`/diagnostics` → `/health`), presented as a
  tile grid of subsystem statuses.
- **Play** merges the "now speaking" view (2e) and the operator view (2f)
  behind a **"show workings"** toggle.

The handoff bundle ("App design system review.zip", sha256
`8756c65047e6e652feea9eea647fdadf04051a25694f2d50317c0364f56cdd53`, from
Claude Design) is untrusted input, kept outside the repo, and its prototypes
quote real game dialogue that must not be copied (AGENTS.md rule 5). The
adopted spec is written in our words with placeholder copy.

Constraints:

- ADR 0011: the served bundle carries `style-src 'self'`; no runtime-injected
  styles. The theme must live in CSS files.
- ADR 0086: the project is Apache-2.0; self-hosted fonts must be license-
  compatible.
- Existing Playwright e2e suite and axe suite must stay green; e2e selectors
  are by `data-testid` and role, not by styles.
- The appliance has no screen of its own; Play is viewed on a laptop, tablet
  or phone beside the TV, so tablet/phone widths are in scope.

## Options

- **Adopt the redesign fully (tokens + chrome + new IA):** coherent, and the
  review validated it end to end; cost is a large parallel frontend effort.
- **Adopt the visual language only, keep the old IA:** cheaper, but leaves the
  characters/voices split and the developer/diagnostics naming, which the
  review specifically flagged as confusing.
- **Keep the current unstyled UI:** rejected; it cannot express the
  status-board, board, and theming behaviour the product needs.

## Decision

1. **Adopt the Plum & Peach design** as the web UI direction, per
   `docs/design/plum-peach/README.md` (Batch 2 screens 2a–2i). Batch 1
   explorations and the swimlane (1h) are explicitly **not built**.
2. **CSS custom-property tokens only.** Every colour, line weight, radius and
   spacing comes from the token set in `web/src/index.css` (one theme file, no
   build step). No runtime-injected styles or inline theme styles (ADR 0011).
   Themes (Ink Blue & Apricot, Sherbet, Lilac Night dark) are alternate token
   sets; the theme page (UX-09) selects among them.
3. **Self-hosted fonts.** Fredoka and Space Mono (both SIL OFL 1.1) are
   vendored into `web/public` and declared via `@font-face` in the stylesheet.
   OFL 1.1 is compatible with Apache-2.0 (ADR 0086): it is a permissive font
   license with no copyleft on code. The **OFL licence text ships with the
   font files** (OFL 1.1 requirement), and the fonts keep their copyright
   notices and original names.
4. **IA changes:** Cast merges `/games/:id/characters` and
   `/games/:id/voices` into `/games/:id/cast` (old paths redirect);
   `/developer` is renamed **Traces** at `/traces` (old path redirects);
   `/diagnostics` is renamed **Health** at `/health` (old path redirects);
   `/play` keeps the session and adds a "show workings" toggle merging 2e
   (now speaking, default) and 2f (operator).
5. **Provisional answers to the handoff's open questions** (orchestrator's
   recommendations, pending owner review):
   - **(a)** After setup finishes, **land on Cast** with a prominent
     **"Start narrating"** CTA — review the voices, then start.
   - **(b)** **Yes**, merge 2e and 2f into one page behind a
     **"show workings"** toggle (default off; the toggle is the only path to
     the operator view).
   - **(c)** Play is viewed on a **laptop, tablet or phone beside the TV**
     (the appliance has no screen of its own). Design and test at tablet and
     phone widths, not just desktop.
   - **(d)** **Regenerate keeps the previous take** as a one-step undo
     (UX-03d); the old version stays playable until replaced again.
   - **(e)** A game still setting up takes the Library **hero only when no
     ready game exists**; otherwise its progress shows in the status bar and
     on its tile.
6. **Live status:** one SSE status stream feeds the global status bar
   (session, capture fps, frame→voice latency, setup progress, any non-ok
   health tile) — see UX-06/UX-07.

## Tradeoffs

- Tokens-only CSS forgoes per-element runtime theming flexibility, but is
  what the CSP allows and keeps theming auditable (one file per theme).
- Merging Characters and Voices loses a dedicated voice-registry page;
  registry health moves to the Cast screen (list of takes, versions) and to
  Health. If the voice registry outgrows that, the route can be re-split —
  the data model is unchanged.
- Renaming `/developer` and `/diagnostics` breaks muscle memory and any
  external links; redirects keep old URLs working.
- A single Play page with a toggle hides pipeline detail from the default
  view (couch-friendly) but forces debugging through the toggle; accepted
  because the default audience is the family, not the operator.

## Validation

- **Playwright visual review:** screenshots of every screen at **1440px,
  1024px and 390px** widths, committed to the evidence bundle; review pass
  before merge of each phase.
- **Axe suite at zero serious/critical** on all new and restyled pages
  (existing suite extended with the new routes).
- **Every existing e2e spec stays green.** Selectors are by `data-testid` and
  role, never by styles, so the re-skin must not change test contract;
  `data-testid` attributes are preserved or extended, and new components
  get test ids from day one.
- Token audit: no raw hex colour outside the theme files (scripted check over
  `web/src/**`).
- Font audit: `OFL.txt` present in `web/public` and in the release archive;
  `@font-face` points at self-hosted files; CSP header unchanged.

## Consequences

- docs/spec/09 gains a short "Redesign (Plum & Peach)" section pointing at
  this ADR and the design docs; the historical spec text is left as-is.
- The shared component kit (Card, Pill button, Chip, NameTab, DitherCover,
  Segmented, StatTile, ProgressBar, Sparkline, Waveform) and the live-status
  API/SSE become first-class shared infrastructure (plan.md Phase 0).
- Themes are now a first-class product surface (theme page, per-user
  persistence).
- e2e and axe baselines extend with the new routes and the 390px width.

## Reversal

The IA renames are redirects, so they reverse cleanly by re-pointing them.
Theme tokens are per-theme files; dropping a theme or reverting to the old
chrome is a stylesheet/shell change. If the merged Cast page outgrows its
purpose, the characters/voices routes can be restored from the same data
model without data migration. The design decision itself (Plum & Peach as
the language) is reversible only by re-running a design review and a new ADR,
since screens, tests and docs will have been built on the tokens.
