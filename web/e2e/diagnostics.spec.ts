import { expect, test } from "@playwright/test";

import { trackConsoleErrors } from "./support/console";

// Real E2E against the Go server started by playwright.config.ts (docs/spec/12
// "UI" mode). `go run` builds without release ldflags, so the stamped version
// and commit fall back to "dev"/"unknown" (internal/bootstrap/version.go).
// `/diagnostics` is a public route (ADR-0016 §3, ADR-0087), so this spec never
// signs in: the sections the scope table gates (video input, the model-role
// probes, the voice registry, the speaker test, the log tail) must render
// their honest signed-out state, not fail and not fabricate data.
//
// Console note: 401 is the expected answer to the gated reads on an anonymous
// visit (the page renders "sign in" hints from it), and 503 is the dev
// server's answer to the capture-device read when no capture layer is wired —
// the state the page renders as its own "unavailable" note. Both are the page
// working, not faults, so both join the ignore list.
test("diagnostics page shows live data and its honest signed-out states", async ({ page }) => {
  const consoleErrors = trackConsoleErrors(page, [401, 503]);

  await page.goto("/diagnostics");

  await expect(page.getByRole("heading", { name: "Diagnostics" })).toBeVisible();

  // The build facts (the pre-existing half of the page).
  const version = page.getByTestId("version");
  await expect(version).toBeVisible();
  await expect(version).toHaveText("dev");
  await expect(page.getByTestId("readiness")).toHaveText("ok");
  await expect(page.getByTestId("commit")).toHaveText("unknown");
  await expect(page.getByTestId("platform")).toContainText(/linux\/(amd64|arm64)/);

  // The host vitals (G10): the public section answers with the machine's own
  // numbers — a read the page could not answer would render its own error
  // state, which the assertions below would not match.
  await expect(page.getByTestId("host-vitals")).toBeVisible();
  await expect(page.getByTestId("host-ram")).toBeVisible();
  await expect(page.getByTestId("host-storage")).toBeVisible();
  await expect(page.getByTestId("host-thermal")).toBeVisible();
  await expect(page.getByTestId("host-throttle")).toBeVisible();

  // Video input (G10): signed out, the honest state is the sign-in hint —
  // never an empty device table that reads as "no devices". The hint's
  // testid is the GatedHint action string, verbatim.
  await expect(page.getByTestId("video-input")).toBeVisible();
  await expect(page.getByTestId("Video input-sign-in")).toBeVisible();
  // The ADR-0082 active-picture window: where the capture layer exposes it the
  // section renders it; where it cannot, the section says so. Either way the
  // note is the page's, not an absence.
  await expect(page.getByTestId("adr0082-window")).toBeVisible();

  // Model roles (G10): signed out, starting a probe gets the honest answer —
  // the 401 rendered as the sign-in hint, no run started, no fabricated rows.
  await expect(page.getByTestId("model-roles")).toBeVisible();
  await page.getByTestId("model-roles-probe-all").click();
  await expect(page.getByTestId("model-roles-sign-in")).toBeVisible();

  // Voice registry (G10): signed out, the sign-in hint stands in for a table.
  await expect(page.getByTestId("voice-registry")).toBeVisible();
  await expect(page.getByTestId("voice-registry-sign-in")).toBeVisible();

  // Speaker test (G10): signed out, the hint — the test tone needs a session.
  await expect(page.getByTestId("speaker-test")).toBeVisible();
  await expect(page.getByTestId("speaker-test-sign-in")).toBeVisible();

  // System logs (G10): the developer-tier read answers 401 to an anonymous
  // visit; the page renders the hint, and the redaction happens server-side
  // before this point matters.
  await expect(page.getByTestId("system-logs")).toBeVisible();
  await expect(page.getByTestId("system-logs-sign-in")).toBeVisible();

  await page.screenshot({ path: "test-results/diagnostics.png", fullPage: true });

  expect(consoleErrors, `browser console: ${consoleErrors.join("\n")}`).toEqual([]);
});
