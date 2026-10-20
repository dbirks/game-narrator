import { expect, test } from "@playwright/test";

import { trackConsoleErrors } from "./support/console";
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
 *      and the not-found page) stay reachable with no account at all.
 */

const ADMIN = { username: "route-admin", password: "steady as the tide" };

test("the route matrix across the first-run, authenticated and anonymous phases", async ({
  page,
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

  expect(consoleErrors, `browser console: ${consoleErrors.join("\n")}`).toEqual([]);
  expectDefaultDataDirUntouched(run);
});
