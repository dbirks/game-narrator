import { Link } from "react-router";

import { readStoredTheme } from "../../lib/theme";
import { THEMES, THEME_LABELS, type Theme } from "../../lib/themes";
import { Card } from "../../components/kit";

/**
 * `/settings` "Theme" (ADR-0109): the link to the theme page
 * (`/settings/theme`) with the current theme named. The page carries the
 * live value off the `<html>` attribute the Phase 0 seam (lib/theme.ts)
 * owns — the applied attribute wins, and a missing attribute (the default
 * theme clears it) falls back to the stored value, then to the default.
 */
function currentTheme(): Theme {
  const live = globalThis.document?.documentElement?.getAttribute("data-theme");
  if (live !== null && live !== "" && (THEMES as readonly string[]).includes(live)) {
    return live as Theme;
  }
  const stored = readStoredTheme();
  if (stored !== null && (THEMES as readonly string[]).includes(stored)) {
    return stored as Theme;
  }
  return THEMES[0];
}

export function ThemeSection() {
  const theme = currentTheme();
  return (
    <Card title="Theme" testid="settings-card-theme">
      <div className="row" data-testid="settings-theme">
        <span>
          The app is running in{" "}
          <strong data-testid="settings-theme-current">{THEME_LABELS[theme]}</strong>
          {theme === THEMES[0] ? " (the default)" : ""} — every colour is one of the eight token
          slots of that set.
        </span>
        <Link to="/settings/theme" data-testid="theme-page-link">
          Open the theme page →
        </Link>
      </div>
    </Card>
  );
}
