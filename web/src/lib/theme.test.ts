import { afterEach, describe, expect, it, vi } from "vitest";

import { applyTheme, readStoredTheme, setTheme, THEME_STORAGE_KEY } from "./theme";
import { THEME_LABELS, THEMES } from "./themes";

/**
 * The theme seam (plan.md Phase 0 item 3, Phase 1's picker): the theme is
 * one `data-theme` attribute on <html> (the default theme clears it so the
 * :root set applies), the choice survives reloads via localStorage, and a
 * private-mode storage denial degrades to the default rather than breaking
 * the shell.
 */

afterEach(() => {
  document.documentElement.removeAttribute("data-theme");
  localStorage.clear();
  vi.restoreAllMocks();
});

describe("applyTheme", () => {
  it("sets the data-theme attribute for an alternate theme", () => {
    applyTheme("lilac-night");
    expect(document.documentElement.dataset.theme).toBe("lilac-night");
  });

  it("clears the attribute for the default theme (the :root set applies)", () => {
    applyTheme("lilac-night");
    applyTheme("plum");
    expect(document.documentElement.hasAttribute("data-theme")).toBe(false);
  });

  it("falls back to the default on an unknown or empty name", () => {
    applyTheme("no-such-theme");
    expect(document.documentElement.hasAttribute("data-theme")).toBe(false);
    applyTheme("");
    expect(document.documentElement.hasAttribute("data-theme")).toBe(false);
  });
});

describe("readStoredTheme", () => {
  it("reads the persisted choice (null when absent)", () => {
    expect(readStoredTheme()).toBeNull();
    localStorage.setItem(THEME_STORAGE_KEY, "sherbet");
    expect(readStoredTheme()).toBe("sherbet");
  });

  it("swallows storage denials (private mode)", () => {
    vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
      throw new DOMException("denied", "SecurityError");
    });
    expect(readStoredTheme()).toBeNull();
  });
});

describe("setTheme", () => {
  it("applies and stores the choice", () => {
    expect(setTheme("ink-apricot")).toBe("ink-apricot");
    expect(document.documentElement.dataset.theme).toBe("ink-apricot");
    expect(localStorage.getItem(THEME_STORAGE_KEY)).toBe("ink-apricot");
  });

  it("still applies when the storage write is denied", () => {
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new DOMException("denied", "QuotaExceededError");
    });
    expect(setTheme("lilac-night")).toBe("lilac-night");
    expect(document.documentElement.dataset.theme).toBe("lilac-night");
  });
});

describe("THEMES", () => {
  it("keeps exactly the four token sets the design defines, each labelled", () => {
    expect([...THEMES].sort()).toEqual(["ink-apricot", "lilac-night", "plum", "sherbet"]);
    for (const theme of THEMES) expect(THEME_LABELS[theme]?.length).toBeGreaterThan(0);
  });
});
