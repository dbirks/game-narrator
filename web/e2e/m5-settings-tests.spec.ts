import { expect, test } from "@playwright/test";
import { mkdtemp, readFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { trackConsoleErrors } from "./support/console";
import { ports } from "./support/ports";
import { assertHermeticProviderEnv } from "./support/servers";

/**
 * The /settings and /tests pages (docs/spec/09), on the shared `api` webServer
 * (playwright.config.ts): the real Go binary (`go run ./cmd/game-narrator
 * serve`) started from the repository checkout, which is why the developer
 * harness can resolve its root from the working directory and list the
 * repository's own scenario catalog (ADR-0047).
 *
 *   1. /settings (admin): the effective configuration renders — the model
 *      roles with their state (the api server's provider environment is
 *      scrubbed, so NOT CONFIGURED naming each role's model variable in any
 *      developer shell), the storage/audio/security/import sections, the
 *      no-auto-update policy — and no secret material reaches the DOM (the
 *      token is a presence boolean, the endpoint is the "<configured>"
 *      marker, and the operator's real env values are asserted absent); the
 *      backup download is a real SQLite file (its 16-byte header is asserted
 *      on the bytes);
 *   2. /tests (developer via the admin role): the catalog lists the unblocked
 *      Dolphin scenario (the savestate work) and the unblocked GN-T047 as
 *      runnable, running synthetic/dialogue-advance-001 enqueues the durable
 *      job and the in-process worker reports PASS, and the report opens in
 *      the sandboxed iframe (sandbox="" — no scripts, no same-origin access).
 */

const ADMIN = { username: "m5st-admin", password: "steady as the tide" };
// The savestate work unblocked the last two Dolphin scenarios: the catalogue
// must now list the row as runnable (the blocked-row contract — the row shows
// its reason and its Run button is disabled — is covered by the page's unit
// test web/src/pages/Tests.test.tsx against a synthetic blocked fixture).
const UNBLOCKED_DOLPHIN_ID = "dolphin/first-gameplay-dialogue";
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
  // 503 joins the default 401 on the ignore list: this dev server has no
  // capture layer wired (no GAME_NARRATOR_CAPTURE_FAKE), so the settings
  // page's capture-device read answers 503 by design and the page renders
  // its honest "unavailable" state — the same 503 the setup-wizard and a11y
  // walks ignore for the same reason (ADR-0078's seam is opt-in per server).
  const consoleErrors = trackConsoleErrors(page, [401, 503]);
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

  // Roles: the `api` server's provider environment is scrubbed by
  // e2e/serve.mjs (the manifest records which inherited GAME_NARRATOR_*
  // variables it dropped), so the rows are deterministic in any developer
  // shell: every role reports NOT CONFIGURED naming its own model variable.
  // Without the scrub this assertion would depend on the host's live
  // configuration — the developer's own Clef/ASR/TTS wiring would render as
  // CONFIGURED rows here. The scrub itself is asserted against the manifest
  // below: every GAME_NARRATOR_* variable this spec's process has is either
  // declared by the project (kept, with the project's value) or recorded as
  // dropped.
  const providerEnv = assertHermeticProviderEnv("api");
  const roleModelEnv: Record<string, string> = {
    agent_reasoning: "GAME_NARRATOR_ROLE_AGENT_REASONING_MODEL",
    fast_reasoning: "GAME_NARRATOR_ROLE_FAST_REASONING_MODEL",
    vision_ocr_fallback: "GAME_NARRATOR_ROLE_VISION_OCR_FALLBACK_MODEL",
    decision: "GAME_NARRATOR_ROLE_DECISION_MODEL",
    voice_design: "GAME_NARRATOR_ROLE_VOICE_DESIGN_MODEL",
    voice_clone: "GAME_NARRATOR_ROLE_VOICE_CLONE_MODEL",
    speech_asr: "GAME_NARRATOR_ROLE_SPEECH_ASR_MODEL",
  };
  for (const [role, modelEnv] of Object.entries(roleModelEnv)) {
    await expect(page.getByTestId(`role-${role}`)).toHaveText(
      `NOT CONFIGURED: no model configured (set ${modelEnv})`,
    );
  }
  // The scrub record must name every GAME_NARRATOR_* variable this spec's
  // process sees — kept (declared) or dropped, never silently inherited. The
  // api project declares none, so all of them were dropped.
  for (const key of Object.keys(process.env).filter((k) => k.startsWith("GAME_NARRATOR_"))) {
    expect(
      providerEnv.scrubbed,
      `${key} was inherited by the spec but the api manifest does not record it as scrubbed`,
    ).toContain(key);
  }
  // The gateway row is presence-only and shows variable NAMES, never values
  // (docs/spec/14): the endpoint's variable is a constant; the token's
  // variable name depends on the environment (the default
  // GAME_NARRATOR_GATEWAY_TOKEN or whatever GAME_NARRATOR_GATEWAY_TOKEN_ENV
  // points at), so only the constant name and the redaction note are
  // asserted here — the values themselves are asserted absent below.
  await expect(page.getByTestId("settings-gateway")).toContainText("GAME_NARRATOR_GATEWAY_URL");
  await expect(page.getByTestId("settings-gateway")).toContainText("presence only");
  // ADR-0074 made the audio output the one runtime-mutable setting: the note
  // counts it, and the section the count points at is on the page.
  await expect(page.getByTestId("settings-mutable-note")).toContainText(
    "1 setting(s) are mutable at runtime",
  );
  await expect(page.getByTestId("settings-audio-output")).toBeVisible();
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

  // A scenario the fixture data unblocked is a row that says runnable — the
  // savestate work lifted the last Dolphin block, and the unblock is asserted
  // the way a fixture `status` change is visible: the row is listed (not
  // hidden) with its Run button enabled. GN-T047 was unblocked by the
  // streamed/cancellable TTS contract; both unblocks are asserted, so a
  // fixture edit that re-blocks either is visible. (The blocked-row contract
  // — reason shown, Run disabled — is pinned by the page's unit test.)
  const dolphinRow = page.getByTestId(`scenario-${UNBLOCKED_DOLPHIN_ID}`);
  await expect(dolphinRow).toContainText("runnable");
  await expect(dolphinRow.getByRole("button", { name: "Run" })).toBeEnabled();
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
