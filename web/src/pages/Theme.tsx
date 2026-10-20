import { useEffect, useRef, useState } from "react";
import { Link } from "react-router";

import { Card, Chip, NameTab, PillButton } from "../components/kit";
import { readStoredTheme, setTheme } from "../lib/theme";
import { THEMES, THEME_LABELS, type Theme } from "../lib/themes";

/**
 * `/settings/theme` — the theme picker (docs/design/plum-peach product-notes
 * UX-09; ADR-0093 §2; ADR-0109). The four themes of docs/design/plum-peach/README.md
 * "Themes" as cards: a static swatch preview (a nested `data-theme` element
 * the token blocks style — the same attribute the whole app carries, never
 * injected CSS), the theme's character note, and the selection action.
 *
 * Selecting a theme calls `setTheme` (the Phase 0 seam): the choice applies
 * to `<html>` **without a reload** — the live preview, the whole app
 * re-theming at once — and persists to localStorage so a reload keeps it
 * (the reload proof UX-09 owes).
 *
 * The token slot table renders every theme's eight slots with the handoff's
 * *derived* slots flagged (README.md "Themes": the coral/sand/hair of Ink
 * Blue & Apricot and the muted/sand/hair of Sherbet were not in the mocks).
 * The values are read from the computed style of each theme's preview
 * element — the CSS token set stays the only owner of colour (ADR-0011,
 * ADR-0093 §2); this page only reads what the stylesheet already declares.
 */

/** The eight slots of the token set, in the README's order. */
const SLOTS = ["ink", "paper", "card", "peach", "coral", "muted", "sand", "hair"] as const;

/** The handoff's *derived* slots per theme (docs/design/plum-peach README
 *  "Themes", marked there as "not in mocks"): they are reviewable here. */
const DERIVED_SLOTS: Record<Theme, readonly (typeof SLOTS)[number][]> = {
  plum: [],
  "ink-apricot": ["coral", "sand", "hair"],
  sherbet: ["muted", "sand", "hair"],
  "lilac-night": [],
};

/** The README's one-line character of each theme. */
const THEME_CHARACTER: Record<Theme, string> = {
  plum: "The default: plum ink on peach paper.",
  "ink-apricot": "Apricot on ink blue; the dithered offset shadow is the elevation.",
  sherbet: "Mint and apricot on cream; the dithered blobs behind content.",
  "lilac-night": "The dark theme — the dither reads as glow; intended for the live session.",
};

function isTheme(value: string | null): value is Theme {
  return value !== null && (THEMES as readonly string[]).includes(value);
}

/** One theme's static swatch preview: the `data-theme` attribute scopes the
 *  token set to this element (the token blocks are bare [data-theme]
 *  selectors, so they apply at any depth), and each tile is a pure CSS
 *  background of its token — no inline styles, no injection. The slot name
 *  sits BELOW the colour block, on the card background (ink on card, the
 *  design's primary readable pair): a label printed on the token itself
 *  fails the contrast gate on the light slots (the a11y walk's axe check),
 *  so the name is a caption, not a watermark. */
function ThemePreview({ theme, testid }: { theme: Theme; testid: string }) {
  return (
    <div className="theme-preview" data-theme={theme} data-testid={testid}>
      {SLOTS.map((slot) => (
        <span key={slot} className="theme-preview-item">
          <span className={`theme-preview-swatch theme-preview-${slot}`} aria-hidden="true" />
          <span className="theme-preview-name">{slot}</span>
        </span>
      ))}
    </div>
  );
}

/** The slot table's one theme row: each slot's swatch (background from the
 *  scoped tokens) and its computed value, the derived slots flagged. The
 *  values are filled from `getComputedStyle` after mount (the effect below),
 *  so the CSS — not a copy in TypeScript — is the source of truth. */
function SlotRow({ theme }: { theme: Theme }) {
  const derived = DERIVED_SLOTS[theme];
  return (
    <tr data-theme={theme} data-testid={`theme-slot-row-${theme}`} className="theme-slots">
      {SLOTS.map((slot) => (
        <td key={slot} className="theme-slot-cell">
          <span className={`theme-slot-swatch theme-slot-${slot}`} aria-hidden="true" />
          <code data-testid={`theme-slot-${theme}-${slot}`} data-slot={slot}>
            …
          </code>
          {derived.includes(slot) ? (
            <span className="theme-slot-derived">
              <Chip tone="neutral" testid={`theme-slot-derived-${theme}-${slot}`}>
                derived
              </Chip>
            </span>
          ) : null}
        </td>
      ))}
    </tr>
  );
}

export function ThemePage() {
  const [current, setCurrent] = useState<Theme>(() => {
    const stored = readStoredTheme();
    return isTheme(stored) ? stored : THEMES[0];
  });
  const rowsRef = useRef<HTMLTableSectionElement | null>(null);

  // Read each theme's slot values from the scoped computed style (the
  // [data-theme] blocks set the custom properties on the row element). The
  // CSS is the only owner of colour; this page just reports it.
  useEffect(() => {
    const rows = rowsRef.current;
    if (!rows) return;
    for (const theme of THEMES) {
      const row = rows.querySelector(`[data-theme="${theme}"]`);
      if (row === null) continue;
      const computed = window.getComputedStyle(row);
      for (const slot of SLOTS) {
        const cell = row.querySelector(`[data-slot="${slot}"]`);
        if (cell === null) continue;
        const value = computed.getPropertyValue(`--${slot}`).trim();
        if (value !== "") cell.textContent = value;
      }
    }
  }, []);

  const pick = (theme: Theme) => {
    setTheme(theme);
    setCurrent(theme);
  };

  return (
    <main className="page">
      <header className="page-header">
        <h1>Theme</h1>
        <Link to="/settings" data-testid="theme-back">
          ← settings
        </Link>
      </header>

      <p className="muted" data-testid="theme-intro">
        The whole app is one token set: picking a theme re-themes every screen at once — no reload —
        and the choice is kept across reloads.
      </p>

      <div className="theme-grid" data-testid="theme-grid">
        {THEMES.map((theme) => (
          <Card key={theme} testid={`theme-card-${theme}`}>
            <ThemePreview theme={theme} testid={`theme-preview-${theme}`} />
            <div className="row">
              <NameTab>{THEME_LABELS[theme]}</NameTab>
              {theme === THEMES[0] ? (
                <Chip tone="neutral" testid={`theme-badge-${theme}`}>
                  default
                </Chip>
              ) : null}
              {theme === "lilac-night" ? (
                <Chip tone="neutral" testid={`theme-badge-${theme}`}>
                  dark
                </Chip>
              ) : null}
            </div>
            <p className="muted" data-testid={`theme-character-${theme}`}>
              {THEME_CHARACTER[theme]}
            </p>
            {current === theme ? <Chip testid={`theme-selected-${theme}`}>selected</Chip> : null}
            <PillButton
              variant={current === theme ? "ink" : "outline"}
              testid={`theme-pick-${theme}`}
              aria-pressed={current === theme}
              onClick={() => pick(theme)}
              disabled={current === theme}
            >
              {current === theme ? "Current theme" : "Use this theme"}
            </PillButton>
          </Card>
        ))}
      </div>

      <Card title="Token slots" testid="theme-slots">
        <p className="muted">
          Every colour in the app is one of these eight slots (ADR-0093 §2: the token set is the
          only colour source). The <em>derived</em> slots are the ones the design handoff did not
          show in the runner-up mocks (docs/design/plum-peach README, "Themes"); their values are
          reviewed here before shipping.
        </p>
        <div className="table-overflow" tabIndex={0} aria-label="Token slots per theme">
          <table className="kit-table" data-testid="theme-slot-table">
            <thead>
              <tr>
                {SLOTS.map((slot) => (
                  <th key={slot} scope="col">
                    {slot}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody ref={rowsRef}>
              {THEMES.map((theme) => (
                <SlotRow key={theme} theme={theme} />
              ))}
            </tbody>
          </table>
        </div>
      </Card>
    </main>
  );
}
