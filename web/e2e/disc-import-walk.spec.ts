import { expect, test } from "@playwright/test";

import { trackConsoleErrors } from "./support/console";
import { expectDefaultDataDirUntouched, serverRun } from "./support/servers";

/**
 * The disc-import walk (ADR-0066), on the packaged binary whose
 * disc-import allowlist is the repo's synthetic disc (testdata/disc —
 * a synthetic fixture with guarded provenance, AGENTS.md rules 3 and 5):
 *
 *   claim the appliance → game → edition → draft → import the disc image
 *   through the overview's disc panel (the dir shape: the select lists the
 *   image from the allowlisted directory) → the quality report lands with
 *   the disc's provenance (game id, region, the image's own SHA-256, the
 *   adapter) → publish → the /play setup panel lists the published
 *   revision's messages.
 *
 * Every mutation happens through the page, exactly as the curation walk does
 * it: "the image is listed, the import lands with a report, the play page
 * shows the messages" is visible state, not an HTTP 200 (docs/spec/09,
 * AGENTS.md rule 9).
 */

const ADMIN = { username: "disc-walk-admin", password: "steady as the tide" };
const GAME = { slug: "disc-harbour", title: "Disc Harbour" };
const EDITION = { region: "us", language: "en", revision: "disc-1" };
// The synthetic disc committed under testdata/disc (TestSyntheticDiscFixture
// guards its bytes): one image, four lines, game id GZLE01.
const DISC_IMAGE = "synth-windwaker.gcm";
// The fixture's pinned SHA-256 (testdata/disc/README.md): the report shows the
// image's own hash as raw hex, so the walk asserts the pinned prefix — proof
// the run hashed the committed fixture, not something else.
const DISC_IMAGE_SHA_PREFIX = "86bca76f055db1b9";
const DISC_LINE_1 = "Wind Waker disc line 1";
// Both the dialogue page and the /play setup panel list original_text — the
// human-readable line, in the disc's own case. normalized_text (textnorm
// step 3, Unicode simple lowercase) is the store's matching key, not a
// display field (the Message schema), so the walk's last assertion expects
// the same case the import stored.
const DISC_GAME_ID = "GZLE01";

test("import a disc image through the UI, publish, and see the messages on /play", async ({
  page,
}, testInfo) => {
  test.setTimeout(300_000);
  test.skip(
    testInfo.project.name !== "disc",
    "runs on the disc project's own embedded-binary backend",
  );
  const run = serverRun("disc");
  // 404 joins 401: the /play stop's start form probes every edition's
  // published revision, and this appliance's library also holds the
  // speaker-link walk's game (the two disc specs share the appliance —
  // their games are disjoint, its edition unpublished): its probe answers
  // 404, the page's documented "nothing published" state, and the browser
  // logs the probe as a failed resource — the machine's answer, not a
  // fault of this walk.
  const consoleErrors = trackConsoleErrors(page, [401, 404]);
  const shot = (step: string) =>
    page.screenshot({ path: `test-results/disc-import-walk-${step}.png`, fullPage: true });

  // ── 1. claim the appliance and sign in, through the page ─────────────
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

  // ── 2. the game, the edition and the first draft ─────────────────────
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
  const revision1 = page.getByTestId("revision-1");
  await expect(revision1).toBeVisible({ timeout: 20_000 });
  await expect(revision1.getByTestId("badge-draft")).toBeVisible();

  // ── 3. the disc panel lists the image from the import directory ─────
  // The dir shape is the full-size path: the image is read in place from the
  // allowlisted directory (never copied), and the panel offers the upload
  // shape only as the small-image fallback.
  const discImport = revision1.getByTestId("disc-import-panel");
  await expect(discImport.getByTestId("disc-import-image")).toBeVisible({
    timeout: 30_000,
  });
  const imageOptions = await discImport
    .getByTestId("disc-import-image")
    .locator("option")
    .allTextContents();
  // The option's text is the basename with its size (the value stays the plain
  // basename, which is what the dir shape posts).
  expect(
    imageOptions.some((text) => text.startsWith(`${DISC_IMAGE} `)),
    `the synthetic disc is listed in the import directory (got: ${imageOptions.join(" | ")})`,
  ).toBeTruthy();

  // ── 4. import it: the 202's job runs, the report lands ───────────────
  await discImport.getByTestId("disc-import-image").selectOption(DISC_IMAGE);
  await discImport.getByTestId("disc-import-submit").click();
  await expect(page.getByTestId("job-progress")).toContainText("succeeded", {
    timeout: 120_000,
  });
  await expect(revision1.getByTestId("disc-import-inserted")).toHaveText("4");
  await expect(revision1.getByTestId("disc-import-game")).toContainText(DISC_GAME_ID);
  await expect(revision1.getByTestId("disc-import-image-sha")).toContainText(DISC_IMAGE_SHA_PREFIX);
  await shot("01-disc-imported");

  // The imported lines are on the dialogue page — the disc's script, in the
  // adapter's vocabulary (GZLE01 is the US region of the game id).
  await page.getByTestId("dialogue-1").click();
  await expect(page.getByTestId("message-table")).toBeVisible({ timeout: 30_000 });
  await expect(page.getByTestId("message-table")).toContainText(DISC_LINE_1);
  await shot("02-disc-script");

  // ── 5. publish the revision the disc filled ──────────────────────────
  await page.getByTestId("overview-link").click();
  await expect(page.getByTestId("game-title")).toHaveText(GAME.title, {
    timeout: 20_000,
  });
  await revision1.getByTestId("status-review").click();
  await expect(revision1.getByTestId("badge-review")).toBeVisible();
  await revision1.getByTestId("publish").click();
  await expect(
    revision1.getByTestId("badge-published"),
    "the disc-filled revision is published",
  ).toBeVisible({ timeout: 30_000 });
  await shot("03-published");

  // ── 6. /play lists the published revision's messages ────────────────
  // The setup panel selects the published revision and lists its script:
  // after a publish, the play page shows the messages, not just an HTTP 200.
  await page.goto("/play");
  await expect(page.getByTestId("play-start-form")).toBeVisible({ timeout: 30_000 });
  const profileSelect = page.getByTestId("play-profile");
  await expect(profileSelect).toBeEnabled();
  // The placeholder plus exactly this appliance's one published revision.
  await profileSelect.selectOption({ index: 1 });
  await expect(page.getByTestId("play-message-count")).toHaveText("4");
  await expect(page.getByTestId("play-message-list")).toContainText(DISC_LINE_1);
  await shot("04-play-lists-messages");

  expect(consoleErrors).toEqual([]);
  expectDefaultDataDirUntouched(run);
});
