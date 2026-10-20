import { expect, test } from "@playwright/test";
import { readFile } from "node:fs/promises";
import { resolve } from "node:path";

import { trackConsoleErrors } from "./support/console";
import { expectDefaultDataDirUntouched, serverRun } from "./support/servers";

/**
 * The full curation walk (docs/spec/16 M5: "user can curate a full synthetic
 * game with no CLI"), on the packaged binary with the scripted TTS roles and
 * the repo's testdata on the replay allowlist (synthetic stubs at the model
 * boundary only — AGENTS.md rule 3). Everything that mutates the catalog
 * happens through the page: no API writes, no CLI. The API client is used
 * only to read (the export comparison) and to observe the fake registry.
 *
 *   create game → edition → draft → import a synthetic script →
 *   edit one line → the characters page shows the roster the script's
 *   speaker column built → voice: audition, approve behind consent,
 *   register → preview → regions: draw, OCR preview on the fixture frame,
 *   save → publish → export the `.gnprofile.json` → re-import the export
 *   through the overview's manifest control (the new draft carries the
 *   edited line's speech text).
 */

const ADMIN = { username: "curation-admin", password: "steady as the tide" };
const GAME = { slug: "curation-harbour", title: "Curation Harbour" };
const EDITION = { region: "us", language: "en", revision: "disc-1" };
const CHARACTER = "Marla";
const CONSENT = "original_synthetic_design";
const SCRIPT_CSV = resolve(
  import.meta.dirname,
  "../..",
  "internal",
  "games",
  "adapter",
  "generic",
  "testdata",
  "synthetic-adventure.csv",
);
const FIXTURE = "synthetic/textbox/plain-dejavu-01.png";
const ORIGINAL_L001 = "Welcome to Saltmarsh, traveller. The tide is out and the boats are waiting.";
const CORRECTED_L001 = "Welcome to Saltmarsh. The tide is out, and the boats are waiting.";

test("curate a full synthetic game through the UI only, then re-import the export", async ({
  page,
  request,
}, testInfo) => {
  test.setTimeout(900_000);
  test.skip(
    testInfo.project.name !== "curation",
    "runs on the curation project's own embedded-binary backend",
  );
  const run = serverRun("curation");
  const consoleErrors = trackConsoleErrors(page, [401]);
  const shot = (step: string) =>
    page.screenshot({ path: `test-results/curation-walk-${step}.png`, fullPage: true });

  // ── 1. first run: claim the appliance and sign in, through the page ───────
  await page.goto("/library");
  await expect(page.getByTestId("setup-form")).toBeVisible({ timeout: 30_000 });
  await page.getByTestId("setup-username").fill(ADMIN.username);
  await page.getByTestId("setup-password").fill(ADMIN.password);
  await page.getByTestId("setup-submit").click();
  await expect(page).toHaveURL(/\/login$/);
  await page.getByTestId("login-username").fill(ADMIN.username);
  await page.getByTestId("login-password").fill(ADMIN.password);
  await page.getByTestId("login-submit").click();
  await expect(page.getByTestId("nav-user")).toContainText(ADMIN.username);
  await expect(page.getByTestId("nav-role")).toHaveText("admin");
  await expect(page.getByTestId("empty-state")).toContainText("No games yet");
  // The `request` fixture is its own context with its own cookie jar (the
  // walk signs in through the page, so the jar this spec polls with is empty
  // until it copies the browser's):
  await request.addCookies(await page.context().cookies());

  // ── 2. create the game, the edition and the first draft ───────────────────
  await page.getByTestId("new-game").click();
  await page.getByTestId("new-game-slug").fill(GAME.slug);
  await page.getByTestId("new-game-title").fill(GAME.title);
  await page.getByTestId("new-game-platform").fill("GameCube");
  await page.getByTestId("new-game-submit").click();
  const gameCard = page.getByTestId(`game-${GAME.slug}`);
  await expect(gameCard).toContainText(GAME.title, { timeout: 30_000 });

  await gameCard.click();
  await expect(page.getByTestId("game-title")).toHaveText(GAME.title);

  await page.getByTestId("new-edition").click();
  await page.getByTestId("edition-region").fill(EDITION.region);
  await page.getByTestId("edition-language").fill(EDITION.language);
  await page.getByTestId("edition-revision").fill(EDITION.revision);
  await page.getByTestId("edition-submit").click();
  const editionTab = page.getByRole("tab", { name: "us / en / disc-1" });
  await expect(editionTab).toBeVisible({ timeout: 20_000 });
  await page
    .getByTestId(/edition-panel-/)
    .getByTestId("new-draft")
    .click();
  await expect(page.getByTestId("revision-1")).toBeVisible({ timeout: 20_000 });
  await expect(page.getByTestId("revision-1").getByTestId("badge-draft")).toBeVisible();

  // ── 3. import the synthetic script into the draft ─────────────────────────
  const revision1 = page.getByTestId("revision-1");
  await revision1.getByTestId("import-file").setInputFiles(SCRIPT_CSV);
  await revision1.getByTestId("import-submit").click();
  await expect(page.getByTestId("job-progress")).toContainText("succeeded", { timeout: 60_000 });
  await expect(page.getByTestId("import-inserted")).toHaveText("40");
  await shot("01-imported");

  // ── 4. edit one line through the dialogue page ────────────────────────────
  await page.getByTestId("dialogue-1").click();
  await expect(page.getByTestId("message-table")).toBeVisible({ timeout: 30_000 });
  const row1 = page.getByTestId("message-row-l-001");
  await expect(row1).toBeVisible();
  await row1.getByTestId(/select-/).click();
  await expect(page.getByTestId("message-detail")).toBeVisible();
  await expect(page.getByTestId("original-text")).toHaveText(ORIGINAL_L001);
  const speech = page.getByTestId("speech-text");
  await speech.fill(CORRECTED_L001);
  await page.getByTestId("speech-save").click();
  await expect(page.getByTestId("speech-save")).toBeDisabled();
  await shot("02-line-edited");

  // ── 5. the characters page: the roster the import built ───────────────────
  await page.getByTestId("overview-link").click();
  await expect(page.getByTestId("game-title")).toHaveText(GAME.title, { timeout: 20_000 });
  await page.getByTestId("characters-1").click();
  await expect(page.getByTestId("character-table")).toBeVisible({ timeout: 20_000 });
  const marinaRow = page.getByTestId("character-row-Marla");
  await expect(marinaRow).toBeVisible();
  const marlaLines = (await page.getByTestId("character-lines-Marla").textContent()) ?? "0";
  expect(Number(marlaLines), "Marla has lines in the script").toBeGreaterThan(0);
  await shot("03-characters");

  // ── 6. the voice page: audition, approve behind consent, register, preview ─
  // The characters page's back link carries no testid of its own (it is the
  // page header's "Overview" link), so the role name is the selector.
  await page.getByRole("link", { name: "Overview" }).click();
  await expect(page.getByTestId("game-title")).toHaveText(GAME.title, { timeout: 20_000 });
  await page.getByTestId("voices-1").click();
  await expect(page.getByTestId("voice-characters")).toBeVisible({ timeout: 20_000 });
  const marla = page.getByTestId("voice-characters").locator("label", { hasText: CHARACTER });
  await marla.getByRole("radio").click();
  await expect(page).toHaveURL(new RegExp(`character=${encodeURIComponent(CHARACTER)}$`));
  await expect(page.getByTestId("no-voice")).toContainText(`No voice for ${CHARACTER} yet`);

  const brief = "A steady, unhurried voice for a coastal town at dusk.";
  await page.getByTestId("voice-brief").fill(brief);
  const accepted = page.waitForResponse(
    (r) => r.request().method() === "POST" && r.url().includes("/api/v1/voices/auditions"),
  );
  await page.getByTestId("generate-auditions").click();
  const auditionJob = (await (await accepted).json()) as { job_id: string; state: string };
  expect(auditionJob.state).toBe("draft");
  await expect
    .poll(
      async () => {
        const body = (await (await request.get(`/api/v1/jobs/${auditionJob.job_id}`)).json()) as {
          state: string;
        };
        if (body.state === "failed") throw new Error("the audition job failed");
        return body.state;
      },
      { message: "the audition job reached a terminal state", timeout: 90_000 },
    )
    .toBe("succeeded");
  await expect(page.getByTestId("voice-state-badge")).toHaveText("auditioning", {
    timeout: 30_000,
  });
  await expect(page.getByTestId(/^audition-card-/)).toHaveCount(3);
  for (let i = 0; i < 3; i += 1) {
    await expect(page.getByTestId(`audition-player-${i}`)).toBeVisible();
    await expect(page.getByTestId(`audition-transcript-${i}`)).not.toBeEmpty();
  }

  // The approval is consent-gated on the page the way it is on the server.
  await expect(page.getByTestId("consent-required-note")).toBeVisible();
  await expect(page.getByTestId("approve-voice")).toBeDisabled();
  await page.getByTestId("audition-approve-0").click();
  await expect(page.getByTestId("audition-approve-0")).toHaveText("Selected");
  await page.getByTestId("consent-input").fill(CONSENT);
  await expect(page.getByTestId("approve-voice")).toBeEnabled();
  await page.getByTestId("approve-voice").click();
  await expect(page.getByTestId("voice-state-badge")).toHaveText("approved", {
    timeout: 20_000,
  });
  await expect(page.getByTestId("voice-consent")).toContainText(CONSENT);

  await expect(page.getByTestId(/^registration-state-/)).toHaveText("pending");
  await page.getByTestId("register-voice").click();
  await expect(page.getByTestId(/^registration-state-/)).toHaveText("ready", {
    timeout: 30_000,
  });
  await expect(page.getByTestId("registration-name")).toContainText(/gn_/);

  // The preview: the page's player exists and the server's cache decision is
  // shown. The blob/duration proof of the audio bytes lives in the
  // voice-preview-audio spec, which keeps this walk to the curation surface.
  await expect(page.getByTestId("preview-panel")).toBeVisible();
  await page.getByTestId("preview-button").click();
  await expect(page.getByTestId("preview-player")).toBeVisible({ timeout: 30_000 });
  await expect(page.getByTestId("preview-cache")).toHaveText(/HIT|MISS/);
  await shot("04-voice");

  // ── 7. the regions page: draw a box, preview the OCR, save, reload ────────
  await page.getByTestId("voices-back").click();
  await expect(page.getByTestId("game-title")).toHaveText(GAME.title, { timeout: 20_000 });
  await page.getByTestId("regions-1").click();
  await expect(page.getByTestId("regions-page")).toBeVisible({ timeout: 30_000 });
  await expect(page.getByTestId("regions-fixture-input")).toHaveValue(FIXTURE);
  const boxes = page.locator('[data-testid^="region-box-"]');
  await expect(boxes).toHaveCount(0);
  const canvas = page.getByTestId("regions-canvas");
  const frame = await canvas.boundingBox();
  if (!frame) throw new Error("the canvas has no bounding box");
  await page.mouse.move(frame.x + frame.width * 0.05, frame.y + frame.height * 0.65);
  await page.mouse.down();
  await page.mouse.move(frame.x + frame.width * 0.95, frame.y + frame.height * 0.97, {
    steps: 12,
  });
  await page.mouse.up();
  await expect(boxes).toHaveCount(1);

  // The real engine over the fixture frame: the read-back is the point.
  await page.getByTestId("regions-preview").click();
  const previewResult = page.getByTestId("regions-preview-result");
  await expect(previewResult).toBeVisible({ timeout: 120_000 });
  await expect(page.getByTestId("regions-verdict")).not.toBeEmpty();
  const variantTexts = previewResult.locator('[data-testid^="regions-variant-text-"]');
  expect(
    await variantTexts.count(),
    "every box ran the four-variant matrix",
  ).toBeGreaterThanOrEqual(4);

  await page.getByTestId("regions-save").click();
  await expect(page.getByTestId("regions-conflict-stale")).toHaveCount(0);
  await expect(page.getByTestId("regions-conflict-frozen")).toHaveCount(0);
  await page.reload();
  await expect(page.getByTestId("regions-page")).toBeVisible({ timeout: 30_000 });
  await expect(boxes).toHaveCount(1, { timeout: 30_000 });
  await shot("05-regions-saved");

  // ── 8. publish the revision the walk just curated ────────────────────────
  // The regions page has no back link of its own: the browser's own history
  // is the path the page's user takes.
  await page.goBack();
  await expect(page.getByTestId("game-title")).toHaveText(GAME.title, { timeout: 20_000 });
  const publishRow = page.getByTestId("revision-1");
  await publishRow.getByTestId("status-review").click();
  await expect(publishRow.getByTestId("badge-review")).toBeVisible();
  await publishRow.getByTestId("publish").click();
  await expect(
    publishRow.getByTestId("badge-published"),
    "the walk's revision is published",
  ).toBeVisible({
    timeout: 60_000,
  });
  await shot("06-published");

  // ── 9. export, through the browser's own download path ─────────────────────
  const [download] = await Promise.all([
    page.waitForEvent("download", { timeout: 30_000 }),
    page.getByTestId("export-1").click(),
  ]);
  expect(download.suggestedFilename()).toMatch(/\.gnprofile\.json$/);
  const exportPath = resolve("test-results", `curation-walk-${download.suggestedFilename()}`);
  await download.saveAs(exportPath);
  const exported = JSON.parse(await readFile(exportPath, "utf8")) as {
    schema_version: string;
    manifest_digest: string;
    publication: { status: string };
    messages: unknown[];
  };
  // 1.1 since ADR-0101 (portable voice mappings: the manifest gained the
  // voices section and the narrator fallback; the bump is the ADR's contract).
  expect(exported.schema_version).toBe("1.1");
  expect(exported.publication.status).toBe("published");
  expect(exported.messages, "the export carries the full script").toHaveLength(40);
  await shot("07-exported");

  // ── 10. re-import the export: a new draft, the edited line intact ─────────
  const editionPanel = page.getByTestId(/edition-panel-/);
  await editionPanel.getByTestId("manifest-file").setInputFiles(exportPath);
  await editionPanel.getByTestId("manifest-submit").click();
  await expect(page.getByTestId("manifest-report"), "the re-import is reported").toBeVisible({
    timeout: 30_000,
  });
  await expect(page.getByTestId("manifest-messages")).toHaveText("40");
  await expect(page.getByTestId("manifest-digest")).toContainText(exported.manifest_digest);

  // The import minted a brand-new draft (ADR-0015: never an overwrite):
  // revision 2, still a draft, beside the published revision 1.
  const revision2 = page.getByTestId("revision-2");
  await expect(revision2, "the re-import created a new draft").toBeVisible();
  await expect(revision2.getByTestId("badge-draft")).toBeVisible();

  // And the new draft carries the content the export held — including the
  // line this walk edited: the re-import preserved its speech text.
  await page.getByTestId("dialogue-2").click();
  await expect(page.getByTestId("message-table")).toBeVisible({ timeout: 30_000 });
  const reimportedRow = page.getByTestId("message-row-l-001");
  await expect(reimportedRow).toBeVisible();
  await reimportedRow.getByTestId(/select-/).click();
  await expect(page.getByTestId("message-detail")).toBeVisible();
  await expect(page.getByTestId("original-text")).toHaveText(ORIGINAL_L001);
  await expect(page.getByTestId("speech-text")).toHaveValue(CORRECTED_L001);
  await shot("08-reimported");

  expect(consoleErrors, `browser console: ${consoleErrors.join("\n")}`).toEqual([]);
  expectDefaultDataDirUntouched(run);
});
