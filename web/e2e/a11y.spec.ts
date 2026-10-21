import AxeBuilder from "@axe-core/playwright";
import { expect, test, type Page } from "@playwright/test";
import { resolve } from "node:path";

import { trackConsoleErrors } from "./support/console";
import { expectDefaultDataDirUntouched, serverRun } from "./support/servers";

/**
 * Accessibility (axe-core) for every route in docs/status/ui-coverage.md —
 * AGENTS.md rule 9: "Exercise real pages through Playwright; … capture
 * traces/screenshots and accessibility results."
 *
 * One walk on one fresh appliance (the packaged binary): first the
 * anonymous/first-run states the route-coverage spec walks (claim form, public
 * health — /diagnostics redirects to it, ADR-0111 — not-found), then the
 * appliance is claimed and the walk signs in
 * as admin and seeds the synthetic game the way the curation walk does
 * (create → edition → draft → script import), so every `/games/:id/…` route is
 * visited in the state the other specs establish. Each route is scanned with
 * the WCAG 2.1 A/AA tags, the per-route results are attached to the report as
 * JSON, and any serious or critical violation fails the run (moderate and
 * minor are recorded without failing — the fix target is A/AA).
 *
 * Model boundary: none. No session, audition or preview runs in this walk, so
 * no scripted model instance is needed (AGENTS.md rule 3).
 */

const ADMIN = { username: "a11y-admin", password: "steady as the tide" };
const GAME = { slug: "a11y-harbour", title: "A11y Harbour" };
const EDITION = { region: "us", language: "en", revision: "disc-1" };
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

/** WCAG 2.1 A/AA: the 2.0 A/AA rules plus the rules 2.1 adds at those levels. */
const WCAG_21_A_AA = ["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"] as const;

interface RouteReport {
  url: string;
  /** rule id → node count, every WCAG 2.1 A/AA violation impact. */
  violations: Record<string, number>;
  /** the failing ones, formatted for the assertion message. */
  serious: string[];
}

async function settled(page: Page, testid: string | RegExp, timeout = 20_000) {
  await expect(page.getByTestId(testid)).toBeVisible({ timeout });
}

/** Scan the current page and record it; the failure check happens once, over all routes. */
async function scan(page: Page, name: string, report: Record<string, RouteReport>): Promise<void> {
  const results = await new AxeBuilder({ page }).withTags([...WCAG_21_A_AA]).analyze();
  const violations = results.violations ?? [];
  const byRule: Record<string, number> = {};
  for (const violation of violations) {
    byRule[violation.id] = (byRule[violation.id] ?? 0) + violation.nodes.length;
  }
  report[name] = {
    url: page.url(),
    violations: Object.fromEntries(Object.entries(byRule).sort(([a], [b]) => a.localeCompare(b))),
    serious: violations
      .filter((violation) => violation.impact === "serious" || violation.impact === "critical")
      .map(
        (violation) =>
          `${violation.id} [${violation.impact}] ×${violation.nodes.length} — ${violation.help}`,
      ),
  };
  // A11Y_DEBUG=1 prints the violating nodes (targets + impacts) for triage.
  if (process.env.A11Y_DEBUG) {
    for (const violation of violations) {
      for (const node of violation.nodes) {
        console.log(
          `A11Y-DEBUG ${name} ${violation.id}[${violation.impact}] ${node.target
            .map((part) => String(part))
            .join(" >> ")}`,
        );
      }
    }
  }
}

test("axe: every route of the ui-coverage matrix, WCAG 2.1 A/AA", async ({ page }, testInfo) => {
  test.setTimeout(600_000);
  test.skip(testInfo.project.name !== "a11y", "runs on the a11y project's fresh appliance");
  const run = serverRun("a11y");
  // 503 joins 401 on the ignore list: on this appliance the capture layer is
  // unwired (no UVC dongle, no test seam), and the setup wizard's devices step
  // renders that 503 as its own "no capture layer" note — the machine's
  // answer, not a fault of the walk. 409 joins them for the same layer on
  // the health page's signal read: ADR-0110 §1's 409 carries the layer's own
  // reason and the tile renders it as its own state.
  const consoleErrors = trackConsoleErrors(page, [401, 409, 503]);
  const report: Record<string, RouteReport> = {};

  const visit = async (name: string, url: string, settle: (page: Page) => Promise<void>) => {
    await page.goto(url);
    await settle(page);
    await scan(page, name, report);
  };

  // ── 1. anonymous first-run states (unclaimed appliance) ─────────────────
  await visit("setup (first run)", "/", async (page) => {
    await expect(page).toHaveURL(/\/setup$/, { timeout: 30_000 });
    await settled(page, "setup-form");
  });
  await visit("health (anonymous)", "/diagnostics", async (page) => {
    // The ADR-0093 §4 rename: /diagnostics redirects to /health (ADR-0111).
    await expect(page).toHaveURL(/\/health$/);
    await expect(page.getByRole("heading", { name: "Health" })).toBeVisible();
    await settled(page, "readiness");
  });
  await visit("not-found (anonymous)", "/definitely/not/a/route", async (page) => {
    await expect(page.getByRole("heading", { name: "Not found" })).toBeVisible();
  });

  // ── 2. claim the appliance and sign in, through the page ────────────────
  await page.goto("/setup");
  await settled(page, "setup-form");
  await page.getByTestId("setup-username").fill(ADMIN.username);
  await page.getByTestId("setup-password").fill(ADMIN.password);
  await page.getByTestId("setup-submit").click();
  await expect(page).toHaveURL(/\/login$/, { timeout: 20_000 });
  await page.getByTestId("login-username").fill(ADMIN.username);
  await page.getByTestId("login-password").fill(ADMIN.password);
  await page.getByTestId("login-submit").click();
  await expect(page.getByTestId("nav-user")).toContainText(ADMIN.username);
  await expect(page.getByTestId("nav-role")).toHaveText("admin");

  // ── 3. seed the synthetic game: create, edition, draft, import ──────────
  await page.getByTestId("new-game").click();
  await page.getByTestId("new-game-slug").fill(GAME.slug);
  await page.getByTestId("new-game-title").fill(GAME.title);
  await page.getByTestId("new-game-platform").fill("GameCube");
  await page.getByTestId("new-game-submit").click();
  const gameCard = page.getByTestId(`game-${GAME.slug}`);
  await expect(gameCard).toContainText(GAME.title, { timeout: 30_000 });

  await gameCard.click();
  await expect(page.getByTestId("game-title")).toHaveText(GAME.title);
  const gameId = /\/games\/([^/]+)\/overview/.exec(page.url())?.[1] ?? "";
  expect(gameId, "the overview URL carries the game id").toBeTruthy();

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

  const revision1 = page.getByTestId("revision-1");
  await revision1.getByTestId("import-file").setInputFiles(SCRIPT_CSV);
  await revision1.getByTestId("import-submit").click();
  await expect(page.getByTestId("job-progress")).toContainText("succeeded", { timeout: 60_000 });
  await expect(page.getByTestId("import-inserted")).toHaveText("40");

  // Publish the seeded revision through the overview's own row actions, the
  // way the curation walk does: the characters page resolves the published
  // revision, the publish page and the play/developer selectors become live.
  await revision1.getByTestId("status-review").click();
  await expect(revision1.getByTestId("badge-review")).toBeVisible();
  await revision1.getByTestId("publish").click();
  await expect(
    revision1.getByTestId("badge-published"),
    "the seeded revision is published",
  ).toBeVisible({ timeout: 60_000 });

  // ── 4. signed-in curator: every /games/:id/… and global route ───────────
  const stops: { name: string; url: string; settle: (page: Page) => Promise<void> }[] = [
    {
      name: "library (/)",
      url: "/",
      settle: async (page) => {
        await expect(page).toHaveURL(/\/library$/, { timeout: 20_000 });
        await settled(page, `game-${GAME.slug}`);
      },
    },
    { name: "games-new", url: "/games/new", settle: (page) => settled(page, "onb-file") },
    {
      name: "overview",
      url: `/games/${gameId}/overview`,
      settle: async (page) => {
        await settled(page, "game-title");
        await expect(page.getByRole("tab", { name: "us / en / disc-1" })).toBeVisible();
      },
    },
    {
      name: "dialogue",
      url: `/games/${gameId}/dialogue`,
      settle: (page) => settled(page, "message-table"),
    },
    {
      name: "voices",
      url: `/games/${gameId}/voices`,
      settle: (page) => settled(page, "voice-characters"),
    },
    {
      name: "characters",
      url: `/games/${gameId}/characters`,
      settle: (page) => settled(page, "character-table"),
    },
    {
      // The merged screen (ADR-0093); the two entries above reach it
      // through the redirects and audit the same page.
      name: "cast",
      url: `/games/${gameId}/cast`,
      settle: (page) => settled(page, "character-table"),
    },
    {
      name: "graph",
      url: `/games/${gameId}/graph`,
      settle: (page) => settled(page, "graph-node-list"),
    },
    {
      name: "regions",
      url: `/games/${gameId}/regions`,
      settle: async (page) => {
        await settled(page, "regions-page");
        // The fixture is the canvas's normal state (the frame streams only for
        // an uploaded file), so the placeholder must be up before the scan.
        await expect(page.getByTestId("regions-frame-placeholder")).toBeVisible();
      },
    },
    {
      name: "publish",
      url: `/games/${gameId}/publish`,
      settle: async (page) => {
        // The seeded revision is published and no new draft exists: the page
        // says so instead of diffing against nothing.
        await expect(page.getByRole("heading", { name: "Publication review" })).toBeVisible();
        await expect(page.getByText("Nothing to review")).toBeVisible();
      },
    },
    { name: "play", url: "/play", settle: (page) => settled(page, "play-start-form") },
    { name: "settings", url: "/settings", settle: (page) => settled(page, "settings-roles") },
    { name: "tests", url: "/tests", settle: (page) => settled(page, "scenario-catalog") },
    {
      name: "replay-clips",
      url: "/replay-clips",
      settle: async (page) => {
        // The store is empty on this appliance: the upload panel and the
        // empty state are the settled shape (ADR-0073).
        await settled(page, "clips-upload-form");
        await expect(page.getByTestId("empty-state")).toBeVisible();
      },
    },
    // The ADR-0109 rename: the a11y walk visits the renamed route's
    // inspector view (the session inspector the /developer surface was),
    // settled on the same control.
    {
      name: "traces",
      url: "/traces?view=inspector",
      settle: (page) => settled(page, "session-start"),
    },
    {
      name: "health (signed in)",
      url: "/diagnostics",
      settle: (page) => settled(page, "readiness"),
    },
    {
      // The first-run wizard as its operator uses it (G9, ADR-0087): the
      // wizard is admin-only and every step re-reads its own live state, so
      // the scan sees the same honest states the setup-wizard spec asserts.
      name: "setup (wizard)",
      url: "/setup?step=devices",
      settle: async (page) => {
        await settled(page, "setup-wizard");
        await settled(page, "setup-step-devices");
      },
    },
  ];
  for (const stop of stops) await visit(stop.name, stop.url, stop.settle);

  // ── 5. anonymous again: the sign-in page and the claimed /setup redirect ─
  await page.getByTestId("nav-logout").click();
  await expect(page).toHaveURL(/\/login$/, { timeout: 20_000 });
  await settled(page, "login-form");
  await scan(page, "login (signed out)", report);
  await visit("setup (claimed)", "/setup", async (page) => {
    await expect(page).toHaveURL(/\/login$/);
    await settled(page, "login-form");
  });

  // The results go to the report either way; the assertion names the failing
  // routes, so one run reports the whole matrix instead of stopping at page one.
  const perRoute: Record<string, number> = {};
  for (const [route, result] of Object.entries(report)) {
    perRoute[route] = Object.values(result.violations).reduce((sum, n) => sum + n, 0);
  }
  console.log(
    "axe WCAG 2.1 A/AA per route (violating nodes): " +
      Object.entries(perRoute)
        .map(([route, total]) => `${route}=${total === 0 ? 0 : total}`)
        .join(" "),
  );
  testInfo.attach("a11y-results", {
    body: JSON.stringify(report, null, 2),
    contentType: "application/json",
  });

  const failures = Object.entries(report).flatMap(([route, result]) =>
    result.serious.map((violation) => `${route} (${result.url}): ${violation}`),
  );
  expect(
    failures,
    "axe: serious/critical WCAG 2.1 A/AA violations:\n" + failures.join("\n"),
  ).toEqual([]);
  expect(consoleErrors, `browser console: ${consoleErrors.join("\n")}`).toEqual([]);
  expectDefaultDataDirUntouched(run);
});
