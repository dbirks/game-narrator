import { expect, test } from "@playwright/test";
import { mkdtemp, readFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { trackConsoleErrors } from "./support/console";
import { ports } from "./support/ports";

/**
 * The /settings and /tests pages (docs/spec/09), on the shared `api` webServer
 * (playwright.config.ts): the real Go binary (`go run ./cmd/game-narrator
 * serve`) started from the repository checkout, which is why the developer
 * harness can resolve its root from the working directory and list the
 * repository's own scenario catalog (ADR-0047).
 *
 *   1. /settings (admin): the effective configuration renders — the model
 *      roles with their environment-dependent state (NOT CONFIGURED on a
 *      fresh checkout, CONFIGURED where the operator's env sets them), the
 *      storage/audio/security/import sections, the no-auto-update policy —
 *      and no secret material reaches the DOM (the token is a presence
 *      boolean, the endpoint is the "<configured>" marker, and the
 *      operator's real env values are asserted absent); the backup download
 *      is a real SQLite file (its 16-byte header is asserted on the bytes);
 *   2. /tests (developer via the admin role): the catalog lists a blocked
 *      scenario with its reason and the unblocked GN-T047 as runnable, running
 *      synthetic/dialogue-advance-001 enqueues the durable job and the
 *      in-process worker reports PASS, and the report opens in the
 *      sandboxed iframe (sandbox="" — no scripts, no same-origin access).
 */

const ADMIN = { username: "m5st-admin", password: "steady as the tide" };
// Blocked by its own fixture data (`status: blocked` + reason): the catalog
// must show the row and its reason, not hide it (ADR-0037).
const STILL_BLOCKED_ID = "dolphin/first-gameplay-dialogue";
// GN-T047 was blocked while the TTS contract was atomic; the streamed /
// cancellable endpoint unblocked it (see the fixture's header), so the
// catalog now lists it as runnable — the unblock is asserted too.
const UNBLOCKED_ID = "synthetic/gn-t047-tts-partial-cancel";
const RUN_ID = "synthetic/dialogue-advance-001";

test("m5: the settings page and the test harness page", async ({ page }, testInfo) => {
  test.setTimeout(600_000);
  test.skip(testInfo.project.name !== "chromium", "runs on the shared api webServer");
  // The chromium project's baseURL is the Vite dev server, but this spec
  // deliberately drives the API server's own origin: the dev proxy rewrites
  // the Host header (changeOrigin), so the CSRF origin check — the browser's
  // Origin must name the host the API process actually sees — would reject
  // every cookie-authenticated state-changing call routed through it (the
  // backup download 403s with origin-mismatch). Same-origin end to end, the
  // page exercises exactly what a production LAN deployment sees.
  const origin = `http://127.0.0.1:${ports.api}`;
  const consoleErrors = trackConsoleErrors(page);
  const shot = (step: string) =>
    page.screenshot({
      path: `test-results/m5-settings-tests-${step}.png`,
      fullPage: true,
    });

  // ── 0. claim the throwaway appliance and sign the page in ──────────────────
  await page.goto(`${origin}/`);
  await expect(page).toHaveURL(/\/setup$/);
  await page.getByTestId("setup-form").waitFor();
  await page.getByTestId("setup-username").fill(ADMIN.username);
  await page.getByTestId("setup-password").fill(ADMIN.password);
  await page.getByTestId("setup-submit").click();
  await expect(page).toHaveURL(/\/login$/);
  await page.getByTestId("login-username").fill(ADMIN.username);
  await page.getByTestId("login-password").fill(ADMIN.password);
  await page.getByTestId("login-submit").click();
  await expect(page).toHaveURL(/\/library$/);

  // ── 1. /settings: the effective configuration, redacted ───────────────────
  // Absolute: page.goto resolves relative URLs against the project's baseURL
  // (the Vite dev server), not the current page — a relative goto would load
  // the document from Vite and every /api call would ride its proxy.
  await page.goto(`${origin}/settings`);
  await expect(page.getByRole("heading", { name: "Settings" })).toBeVisible();
  await expect(page.getByTestId("settings-roles")).toBeVisible();

  // Roles: the per-role state depends on the operator's environment — a
  // fresh checkout renders NOT CONFIGURED with the variable to set, an
  // operator with role models set sees CONFIGURED (or NOT AVAILABLE where the
  // protocol has no adapter yet). The invariant the page must keep in every
  // state: no endpoint value and no token in the DOM.
  for (const role of [
    "agent_reasoning",
    "fast_reasoning",
    "vision_ocr_fallback",
    "decision",
    "voice_design",
    "voice_clone",
    "speech_asr",
  ]) {
    await expect(page.getByTestId(`role-${role}`)).toHaveText(
      /NOT CONFIGURED|NOT AVAILABLE|CONFIGURED/,
    );
  }
  // The gateway row is presence-only and shows variable NAMES, never values
  // (docs/spec/14): the endpoint's variable is a constant; the token's
  // variable name depends on the environment (the default
  // GAME_NARRATOR_GATEWAY_TOKEN or whatever GAME_NARRATOR_GATEWAY_TOKEN_ENV
  // points at), so only the constant name and the redaction note are
  // asserted here — the values themselves are asserted absent below.
  await expect(page.getByTestId("settings-gateway")).toContainText("GAME_NARRATOR_GATEWAY_URL");
  await expect(page.getByTestId("settings-gateway")).toContainText("presence only");
  await expect(page.getByTestId("settings-mutable-note")).toContainText(
    "Nothing is mutable at runtime",
  );
  await expect(page.getByTestId("settings-storage")).toBeVisible();
  await expect(page.getByTestId("settings-audio")).toBeVisible();
  await expect(page.getByTestId("settings-security")).toContainText("127.0.0.1");
  await expect(page.getByTestId("settings-imports")).toBeVisible();
  await expect(page.getByTestId("settings-version")).toContainText("no auto-update");
  await shot("settings");

  // No secret material in the DOM: every URL-shaped string the page holds must
  // be same-origin. The page renders variable names and the "<configured>"
  // marker, never a host, path or token. `page.content()` serialises the live
  // document (head and body), which is the surface a browser user could copy.
  const domText = await page.content();
  const urlish: string[] = domText.match(/https?:\/\/[^\s"'<>]+/g) ?? [];
  const foreign = urlish.filter((u) => !u.startsWith(origin) && u !== "http://localhost");
  expect(
    foreign,
    `foreign URL material in the settings DOM: ${foreign.slice(0, 5).join(", ")}`,
  ).toEqual([]);
  expect(domText).not.toMatch(/sk-[A-Za-z0-9]{16,}/); // the gateway's token shape
  // The stronger form of the same rule: the operator's real values must not
  // be in the DOM. This process and the `go run` server inherit the same
  // environment, so a value that is set here is exactly what the server
  // could be tempted to render.
  const tokenEnvName =
    (process.env.GAME_NARRATOR_GATEWAY_TOKEN_ENV ?? "").trim() || "GAME_NARRATOR_GATEWAY_TOKEN";
  for (const [key, value] of Object.entries(process.env)) {
    if (!value || value.trim() === "") continue;
    const isSecret =
      key === tokenEnvName ||
      key === "GAME_NARRATOR_GATEWAY_URL" ||
      /^GAME_NARRATOR_ROLE_[A-Z0-9]+_URL$/.test(key);
    if (!isSecret) continue;
    expect(domText, `the value of ${key} is present in the settings DOM`).not.toContain(value);
  }
  await shot("settings-dom");

  // The backup download: a real SQLite file, asserted on its 16-byte header.
  const downloadDir = await mkdtemp(join(tmpdir(), "gn-e2e-backup-"));
  const [download] = await Promise.all([
    page.waitForEvent("download"),
    page.getByTestId("backup-download").click(),
  ]);
  const backupPath = join(downloadDir, download.suggestedFilename());
  await download.saveAs(backupPath);
  const header = (await readFile(backupPath)).subarray(0, 16).toString("latin1");
  expect(header, `the backup's 16-byte header: ${JSON.stringify(header)}`).toBe(
    "SQLite format 3\0",
  );
  await shot("settings-backup");

  // ── 2. /tests: the catalog, the blocked row, the run, the report ───────────
  await page.goto(`${origin}/tests`);
  await expect(page.getByRole("heading", { name: "Tests" })).toBeVisible();
  await expect(page.getByTestId("scenario-catalog")).toBeVisible();

  // A blocked scenario is a row that says why, not a row that is hidden
  // (ADR-0037): the fixture's `status: blocked` renders the pill and the
  // reason, and its Run button is disabled. The synthetic GN-T047 scenario
  // was blocked while the TTS contract was atomic; the streamed / cancellable
  // endpoint unblocked it, and the catalog now lists it as runnable — both
  // states are asserted, so a fixture edit that moves either is visible.
  const blockedRow = page.getByTestId(`scenario-${STILL_BLOCKED_ID}`);
  await expect(blockedRow).toContainText("blocked");
  await expect(blockedRow).toContainText("unreachable today");
  await expect(blockedRow.getByRole("button", { name: "Run" })).toBeDisabled();
  const unblockedRow = page.getByTestId(`scenario-${UNBLOCKED_ID}`);
  await expect(unblockedRow).toContainText("GN-T047");
  await expect(unblockedRow).toContainText("runnable");
  await expect(page.getByTestId("gate-results")).toBeVisible();
  await expect(page.getByTestId("dolphin-readiness")).toBeVisible();
  await expect(page.getByTestId("provider-readiness")).toBeVisible();
  await shot("tests-catalog");

  // Run the synthetic scenario: enqueue, poll, PASS.
  const runRow = page.getByTestId(`scenario-${RUN_ID}`);
  await runRow.getByRole("button", { name: "Run" }).click();
  await expect(page.getByTestId("run-state")).toBeVisible();
  await expect(page.getByTestId("run-verdict"), "the run reports PASS").toHaveText("PASS", {
    timeout: 300_000,
  });
  await expect(page.getByTestId("run-state")).toContainText("succeeded");
  await shot("tests-run-pass");

  // The report opens in the sandboxed iframe: sandbox="" strips scripts and
  // same-origin access, and the report is a static verdict page by
  // construction — its contents are still readable through the frame tree.
  await page.getByRole("button", { name: "Open the report" }).click();
  const iframe = page.getByTestId("report-iframe");
  await expect(iframe).toBeVisible();
  await expect(iframe).toHaveAttribute("sandbox", "");
  const src = await iframe.getAttribute("src");
  expect(src, `the report src: ${src}`).toContain(`${origin}/api/v1/dev/tests/reports/`);
  await expect(page.frameLocator('[data-testid="report-iframe"]').getByText(/PASS/)).toBeVisible();
  await expect(page.getByTestId("report-download")).toBeVisible();
  await shot("tests-report");

  expect(consoleErrors).toEqual([]);
});
