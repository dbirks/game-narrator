/**
 * /play view helpers for the e2e walks.
 */
import { expect, type Page } from "@playwright/test";

export type PlayView = "now" | "operator";

const VIEW_TARGET: Record<PlayView, string> = {
  now: "play-now-speaking",
  operator: "play-operator",
};

/**
 * Switch the /play view (the now-speaking caption surface and the operator's
 * "show workings").
 *
 * A bare CDP gesture on the toggle is not a reliable state change on this
 * headless build: under load the gesture has been observed to double-dispatch
 * (the view flips and flips back, or an extra click lands moments later), so
 * specs that read the operator-only facts from the wrong view would stall.
 * This helper drives exactly one native DOM click — `HTMLElement.click()`
 * fires a single event, which cannot double-fire the way a transport-level
 * gesture can — and then asserts the requested view is actually up, retrying
 * a bounded number of times. The product's own pointer and keyboard contracts
 * are exercised by play-pc's direct clicks.
 *
 * (The e2e project type-checks without the DOM lib, so the click goes
 * through a structural cast, the pattern the other e2e files use for page-
 * context DOM.)
 */
export async function setPlayView(page: Page, view: PlayView): Promise<void> {
  const target = page.getByTestId(VIEW_TARGET[view]);
  const toggle = page.locator('[data-testid="play-show-workings"]');
  for (let attempt = 0; attempt < 3; attempt += 1) {
    if (await target.isVisible().catch(() => false)) return;
    await toggle.evaluate((el) => {
      const node = el as unknown as { click: () => void };
      node.click();
    });
    // Let React commit the re-render before checking.
    await page.waitForTimeout(100);
  }
  await expect(target).toBeVisible();
}
