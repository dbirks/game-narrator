/**
 * `src/lib/theme.ts` — the persisted theme selection (UX-09 seam).
 *
 * Theming is a product requirement and ADR-0011's `style-src 'self'` rules
 * out runtime-injected styles, so a theme is an attribute, not a <style>:
 * `data-theme` on <html> selects one of the token sets in
 * src/styles/tokens.css. The picker page (Phase 1, `/settings/theme`)
 * writes through `setTheme`; this module owns the storage key and the
 * apply-to-<html> step, which runs once at startup (src/main.tsx) so a
 * reload keeps the choice without a flash.
 */

import { THEMES } from "./themes";

export const THEME_STORAGE_KEY = "gn-theme";

/** Read the persisted theme; a missing/corrupt value is the default, never
 *  a fault (storage can throw in private mode — the app must not). */
export function readStoredTheme(storage: Storage = globalThis.localStorage): string | null {
  try {
    const value = storage.getItem(THEME_STORAGE_KEY);
    return value === null ? null : value;
  } catch {
    return null;
  }
}

/** Apply a theme to <html>: the default (plum) clears the attribute so the
 *  :root token set applies; any other name selects its [data-theme] block. */
export function applyTheme(theme: string): void {
  const root = globalThis.document?.documentElement;
  if (root === undefined) return; // no document (SSR/tests without DOM)
  if (theme === THEMES[0] || !THEMES.includes(theme as (typeof THEMES)[number])) {
    root.removeAttribute("data-theme");
  } else {
    root.setAttribute("data-theme", theme);
  }
}

/** Persist and apply. Returns the theme that was applied (the caller can
 *  surface it in a picker). Safe on storage exceptions: the choice applies
 *  for the session even when it cannot persist. */
export function setTheme(theme: string): string {
  applyTheme(theme);
  try {
    globalThis.localStorage.setItem(THEME_STORAGE_KEY, theme);
  } catch {
    // A storage exception (private mode, quota) degrades to per-session;
    // the theme itself is applied either way.
  }
  return theme;
}
