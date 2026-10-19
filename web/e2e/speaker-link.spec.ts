import { expect, test } from "@playwright/test";

import { trackConsoleErrors } from "./support/console";
import { expectDefaultDataDirUntouched, serverRun } from "./support/servers";

/**
 * The curator's speaker-link walk (ADR-0070), on the packaged binary whose
 * disc-import allowlist is the repo's synthetic disc (testdata/disc — a
 * synthetic BMG of four invented lines, each carrying a different invented
 * speaker attribute byte, so the import creates four speaker placeholders
 * named by their opaque keys).
 *
 *   sign in (claiming the appliance only on a standalone run) → game →
   edition → draft → import the disc image
 *   → the characters page of the draft lists the four placeholders with
 *   their line counts → link the first placeholder to the second through
 *   the target picker → the confirmation lands, the placeholder row leaves
 *   the section (it is retired), and the roster row of the target shows the
 *   moved lines as verified.
 *
 * Every step asserts visible page state, not an HTTP status (docs/spec/09,
 * AGENTS.md rule 9).
 */

// The disc-import walk's admin (disc-import-walk.spec.ts): this server is the
// disc server (ports.ts). A full-suite run finds it claimed by that walk
// (setup is once per appliance, ADR-0016 §7); a standalone run claims it
// under the same name. The games the two walks create are disjoint.
const ADMIN = { username: "disc-walk-admin", password: "steady as the tide" };
const GAME = { slug: "spk-harbour", title: "Speaker Harbour" };
const EDITION = { region: "us", language: "en", revision: "spk-1" };
const DISC_IMAGE = "synth-windwaker.gcm";
// The fixture's four lines carry the invented attribute bytes 1..4
// (synthBMG in internal/games/adapter/windwaker), so the import names four
// placeholders by their opaque keys.
const SOURCE = "ww:spk:1";
const TARGET = "ww:spk:2";
const slug = (name: string) => encodeURIComponent(name);

test("link the import's speaker placeholders on the characters page", async ({
  page,
}, testInfo) => {
  test.setTimeout(300_000);
  test.skip(
    testInfo.project.name !== "disc",
    "runs on the disc project's own embedded-binary backend",
  );
  const run = serverRun("disc");
  const consoleErrors = trackConsoleErrors(page, [401]);
  const shot = (step: string) =>
    page.screenshot({ path: `test-results/speaker-link-${step}.png`, fullPage: true });

  // ── 1. sign in: claim a standalone run, sign in a shared one ─────────
  // A full-suite run finds the appliance already claimed by the
  // disc-import walk (same server, the earlier spec); a standalone run
  // claims it here. Either way the session that follows is this walk's own.
  await page.goto("/library");
  const setupForm = page.getByTestId("setup-form");
  const loginUsername = page.getByTestId("login-username");
  await expect(setupForm.or(loginUsername)).toBeVisible({ timeout: 30_000 });
  if (await setupForm.isVisible()) {
    await page.getByTestId("setup-username").fill(ADMIN.username);
    await page.getByTestId("setup-password").fill(ADMIN.password);
    await page.getByTestId("setup-submit").click();
    await expect(page).toHaveURL(/\/login$/);
  }
  await loginUsername.fill(ADMIN.username);
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
  const editionTab = page.getByRole("tab", { name: "us / en / spk-1" });
  await expect(editionTab).toBeVisible({ timeout: 20_000 });
  await page
    .getByTestId(/edition-panel-/)
    .getByTestId("new-draft")
    .click();
  const revision1 = page.getByTestId("revision-1");
  await expect(revision1).toBeVisible({ timeout: 20_000 });
  await expect(revision1.getByTestId("badge-draft")).toBeVisible();

  // ── 3. import the synthetic disc: its four lines each carry a different
  // invented speaker byte, so the import creates four placeholders ───────
  const discImport = revision1.getByTestId("disc-import-panel");
  await expect(discImport.getByTestId("disc-import-image")).toBeVisible({ timeout: 30_000 });
  await discImport.getByTestId("disc-import-image").selectOption(DISC_IMAGE);
  await discImport.getByTestId("disc-import-submit").click();
  await expect(page.getByTestId("job-progress")).toContainText("succeeded", {
    timeout: 120_000,
  });
  await expect(revision1.getByTestId("disc-import-inserted")).toHaveText("4");
  await shot("01-disc-imported");

  // ── 4. the characters page of the draft lists the placeholders ───────
  await revision1.getByTestId("characters-1").click();
  await expect(page.getByTestId("characters-title")).toBeVisible({ timeout: 30_000 });

  const section = page.getByTestId("speaker-placeholders");
  await expect(section).toBeVisible();
  await expect(section.locator("li")).toHaveCount(4);
  for (const name of [SOURCE, TARGET, "ww:spk:3", "ww:spk:4"]) {
    const row = section.getByTestId(`speaker-placeholder-${slug(name)}`);
    await expect(row).toBeVisible();
    await expect(row).toContainText(name);
    await expect(row).toContainText("1 line");
  }
  await shot("02-placeholders-listed");

  // ── 5. link the first placeholder to the second, through the page ─────
  const sourceRow = section.getByTestId(`speaker-placeholder-${slug(SOURCE)}`);
  await sourceRow
    .getByTestId(`speaker-placeholder-target-${slug(SOURCE)}`)
    .selectOption({ label: TARGET });
  await sourceRow.getByTestId(`speaker-placeholder-link-${slug(SOURCE)}`).click();

  // The confirmation lands at the section's level: the linked row retires and
  // leaves the list on the next read, but the page says what just happened.
  await expect(page.getByTestId("speaker-link-last-result")).toContainText(
    `${SOURCE} is ${TARGET}`,
    { timeout: 30_000 },
  );
  await expect(page.getByTestId("speaker-link-last-result")).toContainText("1 line");
  await expect(section.locator("li")).toHaveCount(3);
  await shot("03-linked");

  // ── 6. the roster agrees: the target holds the moved line ────────────
  // The moved line is confirmed by the link; the target's own line belongs
  // to a different speaker (its own placeholder), so the row reads inferred
  // with the two statuses listed — verified needs the target's lines settled
  // too, which only the curator's own link of the target provides.
  const roster = page.getByTestId("character-table");
  const targetRow = roster.getByTestId(`character-row-${slug(TARGET)}`);
  await expect(targetRow.getByTestId(`character-lines-${slug(TARGET)}`)).toHaveText("2");
  await expect(targetRow.getByTestId(`character-attribution-${slug(TARGET)}`)).toContainText(
    "inferred",
  );
  await expect(targetRow.getByTestId(`character-attribution-${slug(TARGET)}`)).toContainText(
    "confirmed/proposed",
  );
  const sourceRosterRow = roster.getByTestId(`character-row-${slug(SOURCE)}`);
  await expect(sourceRosterRow).toContainText("retired");
  await expect(sourceRosterRow.getByTestId(`character-lines-${slug(SOURCE)}`)).toHaveText("0");
  await shot("04-roster-agrees");

  expect(consoleErrors).toEqual([]);
  expectDefaultDataDirUntouched(run);
});
