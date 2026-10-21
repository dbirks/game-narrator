import { expect, test } from "@playwright/test";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { randomUUID } from "node:crypto";

import { trackConsoleErrors } from "./support/console";
import { buildManifest, type SeedProfile } from "./support/manifest";
import { expectDefaultDataDirUntouched, serverRun } from "./support/servers";

/**
 * The route × first-run matrix (docs/spec/09 "Routes and page requirements"),
 * on a fresh appliance of the packaged binary — the routes the rest of the
 * suite reaches only in one lifecycle phase each:
 *
 *   1. `/` on an unclaimed appliance: the guard routes the root to /setup;
 *   2. the claim mints no session (ADR-0016 §7): the wizard lands on /login;
 *   3. sign-in returns to the remembered target: `/` redirects to /library;
 *   4. signed out, /setup on a claimed appliance routes to /login itself:
 *      the page's own redirect settles on the login form. (With a live
 *      session the same hop continues /login → /library, so /login would be
 *      only a transient hop there — the matrix asserts it signed out, where
 *      /login is the stable terminal state);
 *   5. still anonymous: a privileged route answers /login?next=… and the
 *      remembered target survives the round trip;
 *   6. the public routes (/health — /diagnostics redirects to it, ADR-0111 —
 *      and the not-found page) stay reachable with no account at all;
 *   7. signed in again with a seeded revision, /games/:id/graph exposes the
 *      keyboard-accessible table view (issue #34): the toggle switches the
 *      canvas to the table of the same page's messages — each with its text
 *      kind and speaker and its successors — the non-canvas alternative the
 *      spec 09 keyboard rule names.
 */

const ADMIN = { username: "route-admin", password: "steady as the tide" };

test("the route matrix across the first-run, authenticated and anonymous phases", async ({
  page,
  request,
}, testInfo) => {
  test.setTimeout(300_000);
  test.skip(testInfo.project.name !== "routes", "runs on the routes project's fresh appliance");
  const run = serverRun("routes");
  const consoleErrors = trackConsoleErrors(page);

  // ── 1. unclaimed: the root lands on the first-run page ────────────────────
  await page.goto("/");
  await expect(page).toHaveURL(/\/setup$/, { timeout: 20_000 });
  await expect(page.getByTestId("setup-form")).toBeVisible();

  // ── 2. the claim creates the admin and no session ─────────────────────────
  await page.getByTestId("setup-username").fill(ADMIN.username);
  await page.getByTestId("setup-password").fill(ADMIN.password);
  await page.getByTestId("setup-submit").click();
  await expect(page).toHaveURL(/\/login$/, { timeout: 20_000 });

  // ── 3. sign-in returns to the remembered target: /library ─────────────────
  await page.getByTestId("login-username").fill(ADMIN.username);
  await page.getByTestId("login-password").fill(ADMIN.password);
  await page.getByTestId("login-submit").click();
  await expect(page).toHaveURL(/\/library$/, { timeout: 20_000 });
  await expect(page.getByTestId("nav-user")).toContainText(ADMIN.username);
  await expect(page.getByTestId("nav-role")).toHaveText("admin");
  await expect(page.getByTestId("empty-state")).toContainText("No games yet");

  // ── 4. /setup on a claimed appliance routes to /login, by the page ────────
  // The SetupPage's own redirect, not the guard's. It is asserted signed out
  // on purpose: with a live session the page's redirect chain is
  // /setup → /login → /library (the login page hands an authenticated
  // visitor straight to the library), so /login is only a transient hop and
  // a URL wait on it races the two client-side navigations. Signed out, the
  // login form is the terminal state the page settles on, and the wait is
  // on a stable URL plus the visible form.
  await page.getByTestId("nav-logout").click();
  await expect(page).toHaveURL(/\/login/, { timeout: 20_000 });
  await page.goto("/setup");
  await expect(page).toHaveURL(/\/login$/, { timeout: 20_000 });
  await expect(page.getByTestId("login-username")).toBeVisible();

  // ── 5. still anonymous: the guard refuses and remembers the target ───────
  await page.goto("/games/new");
  await expect(page).toHaveURL(/\/login\?next=%2Fgames%2Fnew$/, { timeout: 20_000 });
  await expect(page.getByTestId("login-username")).toBeVisible();

  // …and the remembered target is where the sign-in lands.
  await page.getByTestId("login-username").fill(ADMIN.username);
  await page.getByTestId("login-password").fill(ADMIN.password);
  await page.getByTestId("login-submit").click();
  await expect(page).toHaveURL(/\/games\/new$/, { timeout: 20_000 });

  // ── 6. the public routes answer with no account ───────────────────────────
  await page.getByTestId("nav-logout").click();
  await expect(page).toHaveURL(/\/login/);
  await page.goto("/diagnostics");
  await expect(page).toHaveURL(/\/health$/);
  await expect(page.getByRole("heading", { name: "Health" })).toBeVisible();
  await page.goto("/definitely/not/a/route");
  await expect(page.getByRole("heading", { name: "Not found" })).toBeVisible();

  // ── 7. signed in with a seeded revision: /graph's table view (issue #34) ──
  // The matrix's graph assertion: the route's non-canvas alternative is
  // present, keyboard-operable and carries the canvas's own page — every
  // message with its text kind and speaker, then its successors. The
  // revision is seeded through the API the way the graph-regions walk does
  // (import-manifest with the synthetic seed the m5 walks share, a next edge
  // included); the page signs in again, so the route is asserted from the
  // signed-in side.
  await page.goto("/login");
  await page.getByTestId("login-username").fill(ADMIN.username);
  await page.getByTestId("login-password").fill(ADMIN.password);
  await page.getByTestId("login-submit").click();
  await expect(page.getByTestId("nav-user")).toContainText(ADMIN.username);

  const origin = new URL(await page.url()).origin;
  const apiLogin = await request.post(`${origin}/api/v1/auth/login`, {
    data: { username: ADMIN.username, password: ADMIN.password },
  });
  expect(apiLogin.status(), `route matrix api login: ${await apiLogin.text()}`).toBe(200);
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
    { slug: "route-graph", title: "Route Graph" },
    { "idempotency-key": randomUUID(), "x-csrf-token": await csrf() },
  );
  expect(game.status(), `create graph game: ${await game.text()}`).toBe(201);
  const gameId = (await game.json()) as { id: string };
  const edition = await post(
    `/api/v1/games/${gameId.id}/editions`,
    { region: "us", language: "en", revision: "rev0" },
    { "idempotency-key": randomUUID(), "x-csrf-token": await csrf() },
  );
  expect(edition.status(), `create graph edition: ${await edition.text()}`).toBe(201);
  const editionId = ((await edition.json()) as { id: string }).id;
  // The synthetic seed: the two real lines ride the roster (Marina speaks
  // them); the seed's own next edge (demo-page-1 → demo-page-2) gives the
  // table's successor column something to render beside the seeded rows.
  const seed = JSON.parse(
    readFileSync(
      resolve(import.meta.dirname, "../../testdata/synthetic/profiles/dialogue-advance-001.json"),
      "utf8",
    ),
  ) as SeedProfile;
  for (const message of seed.messages) {
    const isRealLine = message.id === "demo-page-1" || message.id === "demo-page-2";
    message.speaker = isRealLine ? "marina" : "keeper";
    message.attribution = isRealLine
      ? { status: "confirmed", confidence: 0.95 }
      : { status: "unknown", confidence: 0 };
  }
  seed.characters = [
    { id: "marina", name: "Marina", role: "protagonist", status: "confirmed" },
    { id: "keeper", name: "Keeper", role: "supporting", status: "proposed" },
  ];
  const { doc } = buildManifest(seed, {
    edition: {
      slug: "route-graph",
      title: "Route Graph",
      region: "us",
      language: "en",
      revision: "rev0",
    },
  });
  const imported = await post(`/api/v1/editions/${editionId}/profiles/import-manifest`, doc, {
    "x-csrf-token": await csrf(),
  });
  expect(imported.status(), `import graph profile: ${await imported.text()}`).toBe(201);
  const { profile } = (await imported.json()) as { profile: { id: string } };

  await page.goto(`/games/${gameId.id}/graph?profile=${encodeURIComponent(profile.id)}`);
  await expect(page.getByTestId("graph-page")).toBeVisible({ timeout: 30_000 });
  await expect(page.getByText("14 of 14 lines", { exact: false })).toBeVisible({ timeout: 30_000 });

  // The toggle (the design's segmented switch): keyboard-operable — focus
  // the table tab, Enter — and the table view renders the same page's data
  // while the canvas list yields.
  const tableTab = page.getByRole("tab", { name: "table" });
  await tableTab.focus();
  await page.keyboard.press("Enter");
  await expect(page.getByTestId("graph-table"), "the table view renders").toBeVisible({
    timeout: 30_000,
  });
  await expect(page.getByTestId("graph-node-list"), "the canvas list yields").toHaveCount(0);
  // The table's rows: the canvas's own messages with kind, speaker and the
  // seeded successor — demo-page-1's next edge names its successor's text,
  // kind and speaker.
  const rows = page.locator('[data-testid^="graph-table-row-"]');
  await expect(rows).toHaveCount(14);
  // demo-page-1 ("…before anyone looked up.") — not demo-x01, which shares
  // its opening words and would match the shorter fragment.
  const page1Row = rows.filter({ hasText: "before anyone looked up" });
  await expect(page1Row).toHaveCount(1, { timeout: 10_000 });
  await expect(page1Row).toContainText("dialogue");
  await expect(page1Row).toContainText("Marina");
  await expect(page1Row).toContainText("next →");
  await expect(page1Row).toContainText("nobody in the village had seen that boat before");

  expect(consoleErrors, `browser console: ${consoleErrors.join("\n")}`).toEqual([]);
  expectDefaultDataDirUntouched(run);
});
