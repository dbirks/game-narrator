import { expect, test } from "@playwright/test";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { randomUUID } from "node:crypto";

import { trackConsoleErrors } from "./support/console";
import { buildManifest, type SeedProfile } from "./support/manifest";
import { expectDefaultDataDirUntouched, serverRun } from "./support/servers";

/**
 * The correction labels and the regression fixtures they are (ADR-0052,
 * docs/spec/08 "user fixes become labels", docs/spec/16 M5 "correction
 * produces regression fixture"), on the packaged binary with the same
 * synthetic seed the player walk uses:
 *
 *   1. seed + import the profile, derive revision 2, and correct one speech
 *      text through the API exactly as the dialogue editor's own save does —
 *      the save is the correction, so the store must record the label;
 *   2. /games/:id/publish shows the edit as "1 changed" (the existing page,
 *      not a new one — this walk covers the label surface through the
 *      request helpers and the pages that already exist);
 *   3. GET /api/v1/corrections serves the label a curator can read: the
 *      before/after pair, the row it names, the acting curator as actor,
 *      status accepted (the fix is applied by the same save);
 *   4. POST /api/v1/corrections records an explicit label the inspector's
 *      "speaker incorrect" action would send: proposed, actor the caller,
 *      applied to nothing;
 *   5. GET /api/v1/dev/fixtures — the developer's fixture view — serves the
 *      same rows; the reading is the difference, not the data;
 *   6. a viewer account gets a 403: recording and reading labels is
 *      curator-and-up (docs/spec/08's scope table).
 */

const EDITION = {
  slug: "corr-harbour",
  title: "Harbour Light",
  region: "us",
  language: "en",
  revision: "rev0",
};
const ADMIN = { username: "corr-admin", password: "steady as the tide" };
const VIEWER = { username: "corr-viewer", password: "steady as the tide" };
const CORRECTED_TEXT = "The harbour bell rang twice, and no one looked up.";

const seed = JSON.parse(
  readFileSync(
    resolve(import.meta.dirname, "../../testdata/synthetic/profiles/dialogue-advance-001.json"),
    "utf8",
  ),
) as SeedProfile;

interface CorrectionRow {
  id: string;
  kind: string;
  target_type: string;
  target_id: string;
  message_id: string;
  before_value: string;
  after_value: string;
  actor: string;
  status: string;
  reviewer: string;
}

test("the editor's save records the regression fixture; the fixture view serves it", async ({
  page,
  request,
  baseURL,
  playwright,
}, testInfo) => {
  test.setTimeout(300_000);
  test.skip(
    testInfo.project.name !== "corrections",
    "runs on the corrections project's own embedded-binary backend",
  );
  const run = serverRun("corrections");
  const origin = new URL(baseURL as string).origin;
  const consoleErrors = trackConsoleErrors(
    page,
    [
      // 401: the unauthenticated probe the auth context makes before signing in.
      // 403: the viewer's refused read, asserted explicitly below.
      401, 403,
    ],
  );

  // ── 1. the admin setup and the seeded revision ───────────────────────────
  const setup = await request.post(`${origin}/api/v1/auth/setup`, {
    data: { username: ADMIN.username, password: ADMIN.password },
  });
  expect(setup.status(), `auth setup: ${await setup.text()}`).toBe(201);
  await request.post(`${origin}/api/v1/auth/login`, {
    data: { username: ADMIN.username, password: ADMIN.password },
  });

  const post = (path: string, data?: unknown, headers: Record<string, string> = {}) =>
    request.post(`${origin}${path}`, {
      data,
      headers: {
        ...(data === undefined ? {} : { "content-type": "application/json" }),
        ...headers,
      },
    });
  const csrf = async () =>
    (await request.cookies(origin)).find((cookie) => cookie.name === "gn_csrf")?.value ?? "";

  const game = await post(
    "/api/v1/games",
    { slug: EDITION.slug, title: EDITION.title },
    { "idempotency-key": randomUUID(), "x-csrf-token": await csrf() },
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

  const { doc } = buildManifest(seed, { edition: EDITION });
  const imported = await post(`/api/v1/editions/${editionId.id}/profiles/import-manifest`, doc, {
    "x-csrf-token": await csrf(),
  });
  expect(imported.status(), `import-manifest: ${await imported.text()}`).toBe(201);
  const r1 = ((await imported.json()) as { profile: { id: string } }).profile.id;

  // Publish revision 1: the publish page's diff needs a published side to
  // diff against (the m5 walk does the same draft → review → publish first).
  const etagOf = async (id: string) => {
    const response = await request.get(`${origin}/api/v1/profiles/${id}`);
    expect(response.status(), "a profile read carries an ETag").toBe(200);
    return response.headers()["etag"] as string;
  };
  await post(
    `/api/v1/profiles/${r1}/status`,
    { to: "review" },
    {
      "if-match": await etagOf(r1),
      "x-csrf-token": await csrf(),
    },
  );
  const published = await post(`/api/v1/profiles/${r1}/publish`, undefined, {
    "if-match": await etagOf(r1),
    "x-csrf-token": await csrf(),
  });
  expect(published.status(), `publish r1: ${await published.text()}`).toBe(200);

  // ── 2. derive revision 2 and correct one line the way the editor saves ───
  const derived = await post(`/api/v1/profiles/${r1}/revisions`, undefined, {
    "x-csrf-token": await csrf(),
  });
  expect(derived.status(), `derive: ${await derived.text()}`).toBe(201);
  const r2 = ((await derived.json()) as { id: string }).id;

  const messagesPage = (await (
    await request.get(`${origin}/api/v1/profiles/${r2}/messages?limit=200`)
  ).json()) as { messages: { id: string; source_message_id?: string; speech_text?: string }[] };
  const page1 = messagesPage.messages.find((row) => row.source_message_id === "demo-page-1") as {
    id: string;
    speech_text: string;
  };
  expect(page1.id, "r2 re-minted the demo-page-1 row").toBeTruthy();
  const before = page1.speech_text;
  expect(before).toBeTruthy();
  expect(before).not.toBe(CORRECTED_TEXT);

  const etag = await etagOf(r2);
  const edited = await request.patch(`${origin}/api/v1/profiles/${r2}/messages/${page1.id}`, {
    data: { speech_text: CORRECTED_TEXT },
    headers: { "content-type": "application/json", "x-csrf-token": await csrf(), "if-match": etag },
  });
  expect(edited.status(), `edit speech: ${await edited.text()}`).toBe(200);

  // ── 3. the existing publish page shows the same edit ─────────────────────
  // The API request context and the page's browser context keep separate cookie
  // jars (the m5 walk signs in twice for the same reason): the page needs its
  // own sign-in before it will render the curation surface.
  await page.goto(`/games/${gameId.id}/publish?profile=${encodeURIComponent(r2)}`);
  await expect(page).toHaveURL(/\/login/);
  await page.getByTestId("login-username").fill(ADMIN.username);
  await page.getByTestId("login-password").fill(ADMIN.password);
  await page.getByTestId("login-submit").click();
  await expect(page).toHaveURL(new RegExp(`/games/${gameId.id}/publish`));
  await expect(page.getByTestId("diff-panel")).toBeVisible({ timeout: 20_000 });
  await expect(page.getByTestId("diff-changed")).toHaveText("1 changed");
  await expect(page.getByTestId("diff-added")).toHaveText("0 added");
  await expect(page.getByTestId("diff-removed")).toHaveText("0 removed");
  await expect(page.getByTestId("diff-changed-rows")).toContainText("no one looked up");
  await page.screenshot({ path: "test-results/corrections-01-diff.png", fullPage: true });

  // ── 4. the save IS the correction: the label is stored and served ───────
  const listRes = await request.get(
    `${origin}/api/v1/corrections?profile_id=${encodeURIComponent(r2)}`,
  );
  expect(listRes.status(), `corrections list: ${await listRes.text()}`).toBe(200);
  const list = (await listRes.json()) as { items: CorrectionRow[] };
  expect(list.items, "the save recorded exactly one label").toHaveLength(1);
  const label = list.items[0];
  expect(label.kind).toBe("speech_text");
  expect(label.target_type).toBe("message");
  expect(label.target_id).toBe(page1.id);
  expect(label.message_id).toBe(page1.id);
  expect(label.before_value).toBe(before);
  expect(label.after_value).toBe(CORRECTED_TEXT);
  expect(label.status).toBe("accepted");
  expect(label.actor).toBe(ADMIN.username);
  expect(label.reviewer).toBe(ADMIN.username);
  await page.screenshot({ path: "test-results/corrections-02-label.png", fullPage: true });

  // ── 5. the explicit recording: a label only, applied to nothing ─────────
  const created = await post(
    "/api/v1/corrections",
    {
      profile_id: r2,
      kind: "speaker",
      target_type: "message",
      target_id: page1.id,
      before_value: "Keeper",
      after_value: "Marina",
      reason: "the on-screen label disagrees with the script order",
      evidence: ["frame:demo-page-1"],
    },
    { "x-csrf-token": await csrf() },
  );
  expect(created.status(), `create correction: ${await created.text()}`).toBe(201);
  const proposed = ((await created.json()) as { correction: CorrectionRow }).correction;
  expect(proposed.status).toBe("proposed");
  expect(proposed.actor).toBe(ADMIN.username);
  // A proposed row carries no reviewer (the DTO omits the empty member).
  expect(proposed.reviewer ?? "").toBe("");
  // The label was applied to nothing: the row still carries the editor's text.
  const r2Again = (await (
    await request.get(`${origin}/api/v1/profiles/${r2}/messages?limit=200`)
  ).json()) as { messages: { id: string; speech_text?: string }[] };
  const page1Again = r2Again.messages.find((row) => row.id === page1.id) as { speech_text: string };
  expect(page1Again.speech_text).toBe(CORRECTED_TEXT);

  // ── 6. the developer's fixture view serves the same rows ─────────────────
  const fixturesRes = await request.get(
    `${origin}/api/v1/dev/fixtures?profile_id=${encodeURIComponent(r2)}`,
  );
  expect(fixturesRes.status(), `fixture list: ${await fixturesRes.text()}`).toBe(200);
  const fixtures = (await fixturesRes.json()) as { items: CorrectionRow[] };
  expect(fixtures.items).toHaveLength(2);
  const fixtureIds = fixtures.items.map((row) => row.id).sort();
  expect(fixtureIds).toEqual([label.id, proposed.id].sort());
  const asFixture = fixtures.items.find((row) => row.id === label.id) as CorrectionRow;
  expect(asFixture.before_value).toBe(before);
  expect(asFixture.after_value).toBe(CORRECTED_TEXT);
  await page.screenshot({ path: "test-results/corrections-03-fixtures.png", fullPage: true });

  // ── 7. the scope table: a viewer cannot read the labels ─────────────────
  const viewerUser = await post(
    "/api/v1/users",
    { username: VIEWER.username, password: VIEWER.password, role: "viewer" },
    { "x-csrf-token": await csrf() },
  );
  expect(viewerUser.status(), `create viewer: ${await viewerUser.text()}`).toBe(201);
  const viewer = await playwright.request.newContext();
  try {
    const viewerLogin = await viewer.post(`${origin}/api/v1/auth/login`, {
      data: { username: VIEWER.username, password: VIEWER.password },
    });
    expect(viewerLogin.status(), `viewer login: ${await viewerLogin.text()}`).toBe(200);
    const denied = await viewer.get(`${origin}/api/v1/corrections`);
    expect(denied.status(), `a viewer read the labels: ${await denied.text()}`).toBe(403);
    const deniedFixtures = await viewer.get(`${origin}/api/v1/dev/fixtures`);
    expect(
      deniedFixtures.status(),
      `a viewer read the fixtures: ${await deniedFixtures.text()}`,
    ).toBe(403);
  } finally {
    await viewer.dispose();
  }

  expect(consoleErrors, `browser console: ${consoleErrors.join("\n")}`).toEqual([]);
  expectDefaultDataDirUntouched(run);
});
