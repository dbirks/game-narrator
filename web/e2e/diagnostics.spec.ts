import { expect, test } from "@playwright/test";

import { trackConsoleErrors } from "./support/console";

// Real E2E against the Go server started by playwright.config.ts (docs/spec/12
// "UI" mode). `go run` builds without release ldflags, so the stamped version
// and commit fall back to "dev"/"unknown" (internal/bootstrap/version.go).
// `/diagnostics` is a public route (ADR-0016 §3), so this spec never signs in.
// It goes straight to the page rather than through `/`: the root now routes by
// auth state (an unclaimed appliance bounces to /setup), and the point here is
// the public page, not the guard.
test("diagnostics page shows live data from the Go API", async ({ page }) => {
  // 401 is ignored by design: the unauthenticated /auth/me probe the guard
  // fires on mount (ADR-0016) is a console error the browser logs, not a fault.
  const consoleErrors = trackConsoleErrors(page);

  await page.goto("/diagnostics");

  await expect(page.getByRole("heading", { name: "Diagnostics" })).toBeVisible();

  const version = page.getByTestId("version");
  await expect(version).toBeVisible();
  await expect(version).toHaveText("dev");
  await expect(page.getByTestId("readiness")).toHaveText("ok");
  await expect(page.getByTestId("commit")).toHaveText("unknown");
  await expect(page.getByTestId("platform")).toContainText(/linux\/(amd64|arm64)/);

  await page.screenshot({ path: "test-results/diagnostics.png", fullPage: true });

  expect(consoleErrors).toEqual([]);
});
