import { expect, test } from "@playwright/test";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { randomUUID } from "node:crypto";

import { trackConsoleErrors } from "./support/console";
import { buildManifest, type SeedProfile } from "./support/manifest";
import { serverRun } from "./support/servers";

/**
 * The milestone 5 graph/regions walk (docs/spec/09 "/games/:id/graph" and
 * "/games/:id/regions"), on the packaged binary with the repo's testdata
 * admitted to the replay allowlist (the `graph-regions` webServer,
 * ADR-0024). One test, one data directory, real APIs only:
 *
 *   1. seed the synthetic profile through the API (manifest import, the
 *      profile stays a draft so the region write has somewhere to land);
 *   2. /games/:id/graph: the virtualized node list renders the seeded lines
 *      with their attribution labels, the legend carries the five edge kinds,
 *      and the speaker filter is server-side (the count drops to the
 *      speaker's lines);
 *   3. /games/:id/regions: the stored region box loads, a second box is
 *      drawn on the canvas, "Preview OCR" runs the real tesseract over the
 *      synthetic frame and reads the line its truth file records, the save
 *      carries the profile's If-Match token, and a reload shows the saved
 *      set again.
 */

const EDITION = {
  slug: "m5-graph-regions",
  title: "Graph Regions Walk",
  region: "us",
  language: "en",
  revision: "rev0",
};
const ADMIN = { username: "m5gr-admin", password: "steady as the tide" };
const FIXTURE = "synthetic/textbox/plain-dejavu-01.png";

function seedWithRoster(): SeedProfile {
  const seed = JSON.parse(
    readFileSync(
      resolve(import.meta.dirname, "../../testdata/synthetic/profiles/dialogue-advance-001.json"),
      "utf8",
    ),
  ) as SeedProfile;
  // The page needs a roster to filter by and edges to colour in the legend.
  seed.edges = [
    ...seed.edges,
    { from: "demo-page-2", to: "demo-page-1", type: "repeat", confidence: 0.4 },
    { from: "demo-page-1", to: "demo-x01", type: "branch", confidence: 0.6 },
  ];
  for (const message of seed.messages) {
    const isRealLine = message.id === "demo-page-1" || message.id === "demo-page-2";
    message.speaker = isRealLine ? "marina" : "keeper";
    message.attribution = isRealLine
      ? { status: "confirmed", confidence: 0.95 }
      : { status: "unknown", confidence: 0 };
  }
  seed.characters = [
    {
      id: "marina",
      name: "Marina",
      aliases: [],
      role: "protagonist",
      summary: "Keeps the harbour log.",
      status: "confirmed",
      verified_source_refs: ["synthetic:harbour-log"],
    },
    { id: "keeper", name: "Keeper", aliases: [], role: "supporting", status: "proposed" },
  ];
  return seed;
}

test("m5: the dialogue graph and the region editor", async ({
  page,
  request,
  baseURL,
}, testInfo) => {
  test.setTimeout(600_000);
  test.skip(
    testInfo.project.name !== "graph-regions",
    "runs on the graph-regions project's own backend",
  );
  serverRun("graph-regions");
  const origin = new URL(baseURL as string).origin;
  const consoleErrors = trackConsoleErrors(page, [
    401, // the unauthenticated probe the auth context makes before signing in
    503, // unwired subsystems answer 503; the pages render honest panels
  ]);
  const shot = (step: string) =>
    page.screenshot({ path: `test-results/m5-graph-regions-${step}.png`, fullPage: true });

  // ── 1. seed the profile through the API (it stays a draft) ───────────────
  const setup = await request.post(`${origin}/api/v1/auth/setup`, {
    data: { username: ADMIN.username, password: ADMIN.password },
  });
  expect(setup.status(), `auth setup: ${await setup.text()}`).toBe(201);
  await request.post(`${origin}/api/v1/auth/login`, {
    data: { username: ADMIN.username, password: ADMIN.password },
  });

  const csrf = async () =>
    (await request.cookies(origin)).find((cookie) => cookie.name === "gn_csrf")?.value ?? "";
  const post = (path: string, data?: unknown, headers: Record<string, string> = {}) =>
    request.post(`${origin}${path}`, {
      data,
      headers: {
        ...(data === undefined ? {} : { "content-type": "application/json" }),
        ...headers,
      },
    });

  const game = await post(
    "/api/v1/games",
    { slug: EDITION.slug, title: EDITION.title },
    {
      "idempotency-key": randomUUID(),
      "x-csrf-token": await csrf(),
    },
  );
  expect(game.status(), `create game: ${await game.text()}`).toBe(201);
  const gameId = (await game.json()) as { id: string };

  const edition = await post(
    `/api/v1/games/${gameId.id}/editions`,
    { region: EDITION.region, language: EDITION.language, revision: EDITION.revision },
    { "idempotency-key": randomUUID(), "x-csrf-token": await csrf() },
  );
  expect(edition.status(), `create edition: ${await edition.text()}`).toBe(201);
  const editionId = (await edition.json()) as { id: string };

  const { doc } = buildManifest(seedWithRoster(), { edition: EDITION });
  const imported = await post(`/api/v1/editions/${editionId.id}/profiles/import-manifest`, doc, {
    "x-csrf-token": await csrf(),
  });
  expect(imported.status(), `import-manifest: ${await imported.text()}`).toBe(201);
  const { profile } = (await imported.json()) as { profile: { id: string } };

  // ── 2. the dialogue graph page ───────────────────────────────────────────
  // The API seed above logged the shared context in; the walk signs in through
  // the UI, so the page's session is the one the form makes, not the API's.
  await page.context().clearCookies();
  await page.goto(`/games/${gameId.id}/graph?profile=${encodeURIComponent(profile.id)}`);
  await expect(page).toHaveURL(/\/login/);
  await page.getByTestId("login-username").fill(ADMIN.username);
  await page.getByTestId("login-password").fill(ADMIN.password);
  await page.getByTestId("login-submit").click();
  await expect(page.getByTestId("graph-page")).toBeVisible({ timeout: 30_000 });

  // All fourteen seeded lines, and the count the page renders for them.
  await expect(page.getByText("14 of 14 lines", { exact: false })).toBeVisible({ timeout: 30_000 });
  const legendItems = page.getByTestId("graph-legend").locator("li");
  await expect(legendItems).toHaveCount(5);
  // The attribution labels render: the unconfirmed lines carry the inferred
  // badge, and the confirmed ones the verified one (both on the visible page).
  await expect(
    page.locator('[data-testid^="graph-node-attr-"]').filter({ hasText: "inferred" }),
  ).not.toHaveCount(0);
  // Awaited on purpose: a full-page screenshot that overlaps the next
  // action (or the goto below) can never return — concurrent captures on
  // one page deadlocked this walk (the intermittent 10-minute timeout).
  await shot("graph");

  // The speaker filter is server-side: Marina's two lines only, both verified.
  const speakerSelect = page.getByTestId("graph-filters").locator("select").first();
  await speakerSelect.selectOption({ label: "Marina" });
  await expect(page.getByText("2 of 2 lines", { exact: false })).toBeVisible({ timeout: 30_000 });
  await expect(
    page.locator('[data-testid^="graph-node-attr-"]').filter({ hasText: "verified" }),
  ).toHaveCount(2);
  await shot("graph-filtered");
  await speakerSelect.selectOption({ label: "All" });
  await expect(page.getByText("14 of 14 lines", { exact: false })).toBeVisible({ timeout: 30_000 });

  // ── 3. the region editor ─────────────────────────────────────────────────
  await page.goto(`/games/${gameId.id}/regions?profile=${encodeURIComponent(profile.id)}`);
  await expect(page.getByTestId("regions-page")).toBeVisible({ timeout: 30_000 });

  // The seeded region (manifest "bottom") loaded from the API.
  const boxes = page.locator('[data-testid^="region-box-"]');
  await expect(boxes).toHaveCount(1, { timeout: 30_000 });

  // The fixture frame is the default; draw a second box over the textbox area.
  await expect(page.getByTestId("regions-fixture-input")).toHaveValue(FIXTURE);
  const canvas = page.getByTestId("regions-canvas");
  const frame = await canvas.boundingBox();
  if (!frame) throw new Error("the canvas has no bounding box");
  const from = { x: frame.x + frame.width * 0.05, y: frame.y + frame.height * 0.65 };
  const to = { x: frame.x + frame.width * 0.95, y: frame.y + frame.height * 0.97 };
  await page.mouse.move(from.x, from.y);
  await page.mouse.down();
  await page.mouse.move(to.x, to.y, { steps: 12 });
  await page.mouse.up();
  await expect(boxes).toHaveCount(2);
  await shot("regions-drawn");

  // Preview: the real engine over the synthetic frame, four variants per box.
  await page.getByTestId("regions-preview").click();
  const previewResult = page.getByTestId("regions-preview-result");
  await expect(previewResult).toBeVisible({ timeout: 120_000 });
  const variantTexts = previewResult.locator('[data-testid^="regions-variant-text-"]');
  const readCount = await variantTexts.count();
  expect(readCount, "every box ran the four-variant matrix").toBeGreaterThanOrEqual(4);
  let readTheLine = false;
  for (let i = 0; i < readCount; i += 1) {
    const text = (await variantTexts.nth(i).textContent()) ?? "";
    if (text.toLowerCase().includes("lighthouse")) readTheLine = true;
  }
  expect(readTheLine, "a variant read the synthetic line the fixture records").toBeTruthy();
  await expect(page.getByTestId("regions-verdict")).not.toBeEmpty();
  await shot("regions-preview");

  // Save: the whole set with the profile's If-Match token.
  await page.getByTestId("regions-save").click();
  await expect(page.getByTestId("regions-conflict-stale"), "no stale-token conflict").toHaveCount(
    0,
  );
  await expect(
    page.getByTestId("regions-conflict-frozen"),
    "no frozen-revision conflict",
  ).toHaveCount(0);
  // The save minted ids for the boxes; the reload must show both again.
  await page.reload();
  await expect(page.getByTestId("regions-page")).toBeVisible({ timeout: 30_000 });
  await expect(boxes).toHaveCount(2, { timeout: 30_000 });
  await shot("regions-saved");

  expect(consoleErrors, "no unexpected console errors").toEqual([]);
});
