import type { Page } from "@playwright/test";

/**
 * Collects the page's real faults — unexplained console errors and page errors
 * — for a spec that must end with "the browser saw nothing it did not cause".
 *
 * The browser logs every HTTP response with a failed status as a console error
 * ("Failed to load resource: the server responded with a status of 401 (…)"),
 * so a walk-through that *deliberately* makes a rejected request would fail its
 * own cleanliness check. `ignoredStatuses` names the statuses the spec triggers
 * by design; an error whose status is not on the list still counts.
 *
 * The default — 401 — is the unauthenticated `GET /api/v1/auth/me` probe the
 * guard fires on every mount: the guard asks, the server says no, the app
 * renders the login form (ADR-0016). That 401 is the app working, not a fault.
 */
export function trackConsoleErrors(
  page: Page,
  ignoredStatuses: readonly number[] = [401],
): string[] {
  const errors: string[] = [];
  const ignored = new Set(ignoredStatuses);
  const statusOf = (text: string): number => {
    const match = /status of (\d{3})/.exec(text);
    return match ? Number(match[1]) : -1;
  };
  page.on("console", (message) => {
    if (message.type() !== "error") return;
    if (ignored.has(statusOf(message.text()))) return;
    errors.push(message.text());
  });
  page.on("pageerror", (error) => errors.push(String(error)));
  return errors;
}
