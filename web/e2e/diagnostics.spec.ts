import { expect, test } from "@playwright/test";

// Real E2E against the Go server started by playwright.config.ts (docs/spec/12
// "UI" mode). `go run` builds without release ldflags, so the stamped version
// and commit fall back to "dev"/"unknown" (internal/bootstrap/version.go).
test("diagnostics page shows live data from the Go API", async ({ page }) => {
  const consoleErrors: string[] = [];
  page.on("console", (message) => {
    if (message.type() === "error") {
      consoleErrors.push(message.text());
    }
  });
  page.on("pageerror", (error) => {
    consoleErrors.push(String(error));
  });

  await page.goto("/");
  await expect.poll(() => new URL(page.url()).pathname).toBe("/diagnostics");

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
