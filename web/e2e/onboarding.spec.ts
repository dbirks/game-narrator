import { expect, test, type APIRequestContext, type Page } from "@playwright/test";
import { resolve } from "node:path";

import { ports } from "./support/ports";
import {
  expectDefaultDataDirUntouched,
  expectIsolatedDataDir,
  serverNameForProject,
  serverRun,
} from "./support/servers";
import { trackConsoleErrors } from "./support/console";

/**
 * The onboarding agent end-to-end (ADR-0028), against the packaged binary with
 * the `agent_reasoning` role pointed at a scripted model gateway
 * (web/e2e/fake-model.mjs). No mocked workflow: the real API, the real job
 * queue, the real agent loop, the real tools (import, source quality, fetch)
 * and the real approval boundary — only the *model* is scripted, which is what
 * the milestone's evidence requirement asks for (a genuine decision-provider
 * request takes place; the live-model smoke test covers the real one).
 *
 * Test (a) is the curator walk-through: claim the appliance, sign in, start a
 * run with the staged script and web off, watch import and the quality report
 * land in the timeline, approve the publish review item, and see the published
 * revision in the library. Test (b) is the injection walk-through: the scripted
 * model fetches a local "malicious" page (admitted through the dev private
 * allowlist, test-only) that instructs the model to call a tool that does not
 * exist and to use a fabricated approval token; the spec asserts the UI shows
 * the source as untrusted, the timeline contains no such call, nothing was
 * published, and the console stayed clean. Test (c) is the rights-gated web
 * dump walk (ADR-0077): no disc and no staged script — the model fetches the
 * labelled-synthetic dump page, proposes that exact source, the spec asserts
 * the provenance block and approves the review item, and the single-use token
 * releases the import, the quality report and the publish.
 */

const TESTDATA = resolve(
  import.meta.dirname,
  "..",
  "..",
  "internal",
  "games",
  "adapter",
  "generic",
  "testdata",
);
const ADVENTURE_CSV = resolve(TESTDATA, "synthetic-adventure.csv");

const ADMIN = { username: "onboarding-admin", password: "sufficient strength" };
const FAKE_MODEL = `http://127.0.0.1:${ports.fakeModel}`;
const WAIT_MS = 90_000; // job queue + scripted model are fast; this is the slack

/** Claim the appliance if it is unclaimed, then sign in. Setup mints no
 *  session (ADR-0016 §7), so the sign-in is always the step that authenticates.
 *  The guard redirects only once its auth probe resolves, so wait for the
 *  destination form to appear rather than sampling the URL mid-navigation. */
async function signIn(page: Page) {
  await page.goto("/games/new");
  const setupForm = page.getByTestId("setup-form");
  const loginUsername = page.getByTestId("login-username");
  await expect(setupForm.or(loginUsername)).toBeVisible({ timeout: 15_000 });
  if (await setupForm.isVisible()) {
    await page.getByTestId("setup-username").fill(ADMIN.username);
    await page.getByTestId("setup-password").fill(ADMIN.password);
    await page.getByTestId("setup-submit").click();
    await expect(page).toHaveURL(/\/login$/);
  }
  await loginUsername.fill(ADMIN.username);
  await page.getByTestId("login-password").fill(ADMIN.password);
  await page.getByTestId("login-submit").click();
  // The guard may redirect to /library or to a ?next= route; the signed-in
  // header is the destination-independent signal.
  await expect(page.getByTestId("nav-user")).toBeVisible();
  await expect(page.getByTestId("nav-role")).toHaveText("admin");
}

/** Point the scripted gateway at a scenario (the servers share the process's
 *  lifetime; the spec runs serially, workers=1). */
async function setScenario(request: APIRequestContext, scenario: string) {
  const response = await request.post(`${FAKE_MODEL}/_scenario`, { data: { scenario } });
  expect(response.status(), `the fake model accepted scenario ${scenario}`).toBe(200);
}

test("onboarding: staged import, quality report, approve, published in the library", async ({
  page,
  request,
}) => {
  test.setTimeout(240_000);
  await expectIsolatedDataDir(serverNameForProject("onboarding"));
  const consoleErrors = trackConsoleErrors(page, [401]);

  await setScenario(request, "import");
  await signIn(page);
  await page.goto("/games/new");

  // ── the setup form: title, platform, web off, staged script ───────────────
  await expect(page.getByTestId("onb-setup-form")).toBeVisible();
  await page.getByTestId("onb-title").fill("Synthetic Adventure");
  await page.getByTestId("onb-platform").fill("GameCube");
  await expect(page.getByTestId("onb-web-toggle")).toBeChecked();
  await page.getByTestId("onb-web-toggle").uncheck();
  await page.getByTestId("onb-file").setInputFiles(ADVENTURE_CSV);
  await page.getByTestId("onb-start").click();

  // ── the run executes as a job: import, then the quality report ────────────
  const importCall = page.getByTestId("onb-tool-import_user_asset");
  await expect(importCall, "the import lands in the tool timeline").toBeVisible({
    timeout: WAIT_MS,
  });
  await expect(page.getByTestId("onb-tool-status-import_user_asset")).toHaveText("succeeded");
  // The import's source id is the link to everything downstream of it.
  await expect(page.getByTestId("onb-tool-sources-import_user_asset").first()).toBeVisible();

  const qualityCall = page.getByTestId("onb-tool-source_quality");
  await expect(qualityCall, "the quality run lands in the tool timeline").toBeVisible({
    timeout: WAIT_MS,
  });
  await expect(page.getByTestId("onb-tool-status-source_quality")).toHaveText("succeeded");

  // The quality report (docs/spec/03) is rendered whole, as text.
  await expect(page.getByTestId("onb-quality-report")).toContainText("message_count", {
    timeout: WAIT_MS,
  });

  // The attempt finishes as a draft: the publish review item is pending and
  // nothing is published yet.
  await expect(page.getByTestId("onb-review-item-publish")).toBeVisible({ timeout: WAIT_MS });
  await expect(page.getByTestId("onb-draft-status")).toHaveText("draft");

  // ── the human approval: the only path to publication ─────────────────────
  await page.getByTestId("onb-review-approve-publish").click();
  await expect(page.getByTestId("onb-draft-status")).toHaveText("published", { timeout: WAIT_MS });
  // The publish went through the tool, with the token, not around it.
  const publishCall = page.getByTestId("onb-tool-publish_profile");
  await expect(publishCall).toBeVisible();
  await expect(page.getByTestId("onb-tool-status-publish_profile")).toHaveText("succeeded");

  // ── the library shows the published revision ──────────────────────────────
  await page.goto("/library");
  await expect(page.getByTestId("game-synthetic-adventure")).toBeVisible();
  await page.getByTestId("game-synthetic-adventure").click();
  await expect(
    page.getByTestId("badge-published"),
    "the published revision in the library",
  ).toBeVisible({
    timeout: WAIT_MS,
  });

  expectDefaultDataDirUntouched(serverRun("onboarding"));
  expect(
    consoleErrors,
    `the browser saw faults it did not cause: ${consoleErrors.join(" | ")}`,
  ).toEqual([]);
});

test("malicious page: untrusted source shown, the non-existent tool is never called", async ({
  page,
  request,
}) => {
  test.setTimeout(180_000);
  await setScenario(request, "malicious");
  const consoleErrors = trackConsoleErrors(page, [401]);

  // The appliance was claimed by the previous test when it runs after it; the
  // helper handles both orders.
  await signIn(page);
  await page.goto("/games/new");

  await expect(page.getByTestId("onb-setup-form")).toBeVisible();
  await page.getByTestId("onb-title").fill("Injection Test");
  await page.getByTestId("onb-platform").fill("GameCube");
  // Web research stays on (the default) — that is how the page gets fetched.
  await expect(page.getByTestId("onb-web-toggle")).toBeChecked();
  await page.getByTestId("onb-start").click();

  // The fetch lands in the timeline; the source card says what the page is.
  const fetchCall = page.getByTestId("onb-tool-fetch_page");
  await expect(fetchCall, "the fetch lands in the tool timeline").toBeVisible({ timeout: WAIT_MS });
  await expect(page.getByTestId("onb-tool-status-fetch_page")).toHaveText("succeeded");

  const sourceCard = page
    .locator('[data-testid^="onb-source-"]')
    .filter({ has: page.locator("text=malicious.html") });
  await expect(sourceCard.first(), "the fetched page is shown as a source").toBeVisible({
    timeout: WAIT_MS,
  });
  await expect(
    sourceCard.first().getByTestId("onb-source-untrusted"),
    "web sources are labelled untrusted",
  ).toBeVisible();

  // The attempt finishes; the run did not obey the page's instructions:
  // no drop_database call exists in the timeline (the tool is not even
  // registered), and the draft is still a draft — the fabricated token in the
  // page could not publish anything.
  await expect(page.getByTestId("onb-run-state")).toContainText("succeeded", { timeout: WAIT_MS });
  expect(
    await page.getByTestId("onb-tool-drop_database").count(),
    "drop_database was never called",
  ).toBe(0);
  await expect(page.getByTestId("onb-draft-status")).toHaveText("draft");
  const lastTurn = page.locator('[data-testid="onb-turn-assistant"]').last();
  await expect(lastTurn).toContainText("will not", { timeout: WAIT_MS });

  expect(
    consoleErrors,
    `the browser saw faults it did not cause: ${consoleErrors.join(" | ")}`,
  ).toEqual([]);
});

/**
 * The rights-gated web dump path (ADR-0077): a user with no disc and no staged
 * script. The scripted model fetches the labelled-synthetic dump page (test-only
 * fixture served by fake-model.mjs through the dev private allowlist), proposes
 * that exact source, and the run stops at the human review boundary. The spec
 * reads the provenance block (URL, SHA-256, rights) above the Approve button,
 * approves, and watches the single-use token do its work: import_web_source and
 * source_quality run against the real tools, the source is recorded with the
 * user's asserted rights, and the draft publishes. Only the model is scripted;
 * the approval boundary, the token and the import are the real thing.
 */
test("web import: no disc, approved web dump → publishable profile", async ({ page, request }) => {
  test.setTimeout(240_000);
  await expectIsolatedDataDir(serverNameForProject("onboarding"));
  const consoleErrors = trackConsoleErrors(page, [401]);

  await setScenario(request, "webimport");
  await signIn(page);
  await page.goto("/games/new");

  // No file, web research on: the dump page is the only text in this run.
  await expect(page.getByTestId("onb-setup-form")).toBeVisible();
  await page.getByTestId("onb-title").fill("Isle Dump Demo");
  await page.getByTestId("onb-platform").fill("GameCube");
  await expect(page.getByTestId("onb-web-toggle")).toBeChecked();
  await page.getByTestId("onb-start").click();

  // ── the fetch and the proposal stop the run at the review boundary ───────
  const fetchCall = page.getByTestId("onb-tool-fetch_page");
  await expect(fetchCall, "the fetch lands in the tool timeline").toBeVisible({
    timeout: WAIT_MS,
  });
  await expect(page.getByTestId("onb-tool-status-fetch_page")).toHaveText("succeeded");

  const proposeCall = page.getByTestId("onb-tool-propose_web_import");
  await expect(proposeCall, "the proposal lands in the tool timeline").toBeVisible({
    timeout: WAIT_MS,
  });
  await expect(page.getByTestId("onb-tool-status-propose_web_import")).toHaveText("succeeded");

  // The pending review item is the proposal's; the provenance block names the
  // exact source the curator is approving — URL and the SHA-256 prefix of the
  // raw body the approval pins.
  const reviewItem = page
    .locator('[data-testid^="onb-review-item-"]')
    .filter({ hasText: "propose_web_import" })
    .first();
  await expect(reviewItem, "the proposal's review item is pending").toBeVisible({
    timeout: WAIT_MS,
  });
  const provenance = reviewItem.locator('[data-testid^="onb-review-provenance-"]');
  await expect(provenance).toContainText("127.0.0.1");
  await expect(provenance).toContainText("rights unknown");

  // Nothing is imported before the approval: no import call exists yet.
  expect(await page.getByTestId("onb-tool-import_web_source").count()).toBe(0);

  // ── the human approval is the rights assertion ───────────────────────────
  await reviewItem.locator('[data-testid^="onb-review-approve-"]').click();

  const importCall = page.getByTestId("onb-tool-import_web_source");
  await expect(importCall, "the approved import runs after the approval").toBeVisible({
    timeout: WAIT_MS,
  });
  await expect(page.getByTestId("onb-tool-status-import_web_source")).toHaveText("succeeded");

  const qualityCall = page.getByTestId("onb-tool-source_quality");
  await expect(qualityCall, "the quality report runs on the imported source").toBeVisible({
    timeout: WAIT_MS,
  });
  await expect(page.getByTestId("onb-tool-status-source_quality")).toHaveText("succeeded");

  // The source card now carries the user's asserted rights, not unknown.
  const sourceCard = page
    .locator('[data-testid^="onb-source-"]')
    .filter({ has: page.locator("text=dump.txt") });
  await expect(sourceCard.first(), "the imported dump is shown as a source").toBeVisible({
    timeout: WAIT_MS,
  });
  await expect(sourceCard.first()).toContainText("user_supplied");

  // ── the publish boundary is the human's second, separate act ─────────────
  await expect(page.getByTestId("onb-review-item-publish")).toBeVisible({
    timeout: WAIT_MS,
  });
  await page.getByTestId("onb-review-approve-publish").click();
  await expect(page.getByTestId("onb-draft-status")).toHaveText("published", {
    timeout: WAIT_MS,
  });
  await expect(page.getByTestId("onb-tool-status-publish_profile")).toHaveText("succeeded");

  // The library shows the published revision the approved web dump built.
  await page.goto("/library");
  await expect(page.getByTestId("game-isle-dump-demo")).toBeVisible();
  await page.getByTestId("game-isle-dump-demo").click();
  await expect(
    page.getByTestId("badge-published"),
    "the published revision in the library",
  ).toBeVisible({ timeout: WAIT_MS });

  expectDefaultDataDirUntouched(serverRun("onboarding"));
  expect(
    consoleErrors,
    `the browser saw faults it did not cause: ${consoleErrors.join(" | ")}`,
  ).toEqual([]);
});
