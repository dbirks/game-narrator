import { expect, test } from "@playwright/test";

import { trackConsoleErrors } from "./support/console";
import { claimAppliance, expectDefaultDataDirUntouched, serverRun } from "./support/servers";

/**
 * The curator's speaker-link walk (ADR-0070, the naming flow of ADR-0135),
 * on the packaged binary whose disc-import allowlist is the repo's synthetic
 * disc (testdata/disc — a synthetic BMG of four invented lines, each
 * carrying a different invented speaker attribute byte, so the import
 * creates four speaker placeholders named by their opaque keys).
 *
 *   sign in (claiming the appliance only on a standalone run) → game →
 *   edition → draft → import the disc image
 *   → the cast page of the draft lists the four placeholders as the roster's
 *   "Unnamed speaker" rows (the redesign's Unnamed chip counts them)
 *   → open the first placeholder's voice picker and name it after the
 *   second (the speaker-link by name: an existing non-retired row of that
 *   exact name when one exists) → the confirmation lands, the selection
 *   follows the name the row now carries, the placeholder retires behind
 *   its chip, and the roster row of the target shows the moved lines.
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

test("name the import's speaker placeholders on the cast page", async ({ page }, testInfo) => {
  test.setTimeout(300_000);
  test.skip(
    testInfo.project.name !== "disc",
    "runs on the disc project's own embedded-binary backend",
  );
  const run = serverRun("disc");
  // 401 is the unauthenticated probe the guard makes on mount. 503 joins it:
  // the disc server's voice workflow is unwired (no speech roles, docs/spec/13
  // degraded mode), and the merged Cast screen (ADR-0093) reads the voice
  // roster on load — the page degrades (the roster stays, the voice work
  // shows its unavailable note), so the read is the machine's answer, not a
  // fault of the walk.
  const consoleErrors = trackConsoleErrors(page, [401, 409, 503]);
  const shot = (step: string) =>
    page.screenshot({ path: `test-results/speaker-link-${step}.png`, fullPage: true });

  // ── 1. sign in: claim a standalone run, sign in a shared one ─────────
  // The claim is once per appliance (ADR-0016 §7): a full-suite run finds
  // it claimed by the disc-import walk (same server, the earlier spec) or by
  // a parallel worker, and a standalone run claims it here; either way the
  // helper's losing submit just walks to the sign-in.
  await page.goto("/library");
  await claimAppliance(page, ADMIN);
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

  // ── 4. the cast page of the draft: the placeholders are roster rows ────
  // Cast merged the characters page (ADR-0093), and the redesign (ADR-0135)
  // folded the speaker-placeholder section into the roster: the four import
  // placeholders are the roster's "Unnamed speaker" rows — proposed rows
  // whose lines carry the source's own source_field linkage — and the
  // Unnamed chip counts them.
  await revision1.getByTestId("cast-1").click();
  await expect(page.getByTestId("cast-title")).toBeVisible({ timeout: 30_000 });
  const roster = page.getByTestId("character-table");
  await expect(roster).toBeVisible();
  await expect(page.getByTestId("cast-filter-unnamed")).toContainText("Unnamed 4");
  for (const name of [SOURCE, TARGET, "ww:spk:3", "ww:spk:4"]) {
    const row = roster.getByTestId(`character-row-${slug(name)}`);
    await expect(row).toBeVisible();
    await expect(row.getByTestId(`cast-row-name-${slug(name)}`)).toContainText("Unnamed speaker");
    // The roster key the source named the speaker by is the row's tooltip
    // target and its search string; the line count is the row's figure.
    await expect(row).toHaveAttribute("title", new RegExp(escapeRegExp(name)));
    await expect(row.getByTestId(`character-lines-${slug(name)}`)).toHaveText("1");
  }
  await shot("02-placeholders-listed");

  // ── 5. name the first placeholder as the second, through the page ──────
  // The row's one action is Name; the picker's Name block takes the name and
  // runs the speaker-link with it: an existing non-retired row of that exact
  // name when one exists (ww:spk:2 does), a new confirmed character when it
  // does not. The selection follows the name the row then carries.
  const sourceRow = roster.getByTestId(`character-row-${slug(SOURCE)}`);
  await sourceRow.getByTestId(`cast-row-action-${slug(SOURCE)}`).click();
  await expect(page).toHaveURL(new RegExp(`character=${encodeURIComponent(SOURCE)}$`));
  await expect(page.getByTestId("cast-detail-name")).toHaveText(SOURCE);
  await expect(page.getByTestId("speaker-name")).toBeVisible();
  await expect(page.getByTestId("speaker-name-input")).toBeVisible();
  await page.getByTestId("speaker-name-input").fill(TARGET);
  await page.getByTestId("speaker-name-save").click();
  // The link lands: the selection follows the name the row now carries (the
  // panel moves onto the target row; the roster's change is the
  // confirmation itself).
  await expect(page).toHaveURL(new RegExp(`character=${encodeURIComponent(TARGET)}$`));
  await expect(page.getByTestId("cast-detail-name")).toHaveText(TARGET, { timeout: 30_000 });
  await shot("03-linked");

  // ── 6. the roster agrees: the target holds the moved line ────────────
  // The moved line is confirmed by the link; the target's own line belongs
  // to a different speaker (its own placeholder), so the row reads inferred
  // with the two statuses listed — verified needs the target's lines settled
  // too, which only the curator's own link of the target provides.
  // The cell lists the distinct statuses in *line* order, and line order is
  // the ULID order: both lines were minted in the same millisecond at import,
  // so the random suffix decides which sorts first (internal/games/ids.go —
  // ids within one millisecond are not monotonic). The pair may read in
  // either order on any run, so the assertion pins the set — both statuses
  // present, no third, the row still inferred — not their order.
  const targetRow = roster.getByTestId(`character-row-${slug(TARGET)}`);
  await expect(targetRow.getByTestId(`character-lines-${slug(TARGET)}`)).toHaveText("2");
  await expect(targetRow.getByTestId(`character-attribution-${slug(TARGET)}`)).toHaveText(
    /^inferred \(speaker attribution is (?:confirmed\/proposed|proposed\/confirmed), not confirmed\)$/,
  );
  // The placeholder retires: out of the live chips, into the Retired one.
  await expect(roster.getByTestId(`character-row-${slug(SOURCE)}`)).toHaveCount(0);
  await page.getByTestId("cast-filter-retired").click();
  const sourceRosterRow = roster.getByTestId(`character-row-${slug(SOURCE)}`);
  await expect(sourceRosterRow).toBeVisible();
  await expect(sourceRosterRow).toContainText("retired");
  await expect(sourceRosterRow.getByTestId(`character-lines-${slug(SOURCE)}`)).toHaveText("0");
  await shot("04-roster-agrees");

  expect(consoleErrors).toEqual([]);
  expectDefaultDataDirUntouched(run);
});

function escapeRegExp(text: string): string {
  return text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}
