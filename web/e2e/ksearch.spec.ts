import AxeBuilder from "@axe-core/playwright";
import { expect, test, type Page } from "@playwright/test";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

import { trackConsoleErrors } from "./support/console";
import { ports } from "./support/ports";
import { expectDefaultDataDirUntouched, serverRun } from "./support/servers";

/**
 * The ⌘K command palette (docs/design/plum-peach plan.md Phase 1 item 2;
 * ADR-0098; UX-02) on a fresh packaged-binary appliance the spec claims and
 * seeds the way the a11y walk does (create → edition → draft → import), so
 * every palette behaviour runs against a real catalogue:
 *
 *   ⌘K opens the overlay (role=dialog) from any page without navigating;
 *   a query returns grouped results spanning ≥2 of the six categories
 *   (games / characters / lines / traces / settings / actions);
 *   ↵ on a character row lands on the Cast page; ↵ on a line row opens the
 *   line's trace; ↵ on an action row runs the action (the registry's one API
 *   target, POST /api/v1/voices/auditions, 202s); Tab hands the query to the
 *   agent dock and the message sent to the agent includes the query.
 *
 * The row routes assert against the ADR-0093 IA seams: /games/:id/cast lands
 * on the merged characters page until plan item 5's Cast page replaces it,
 * and /traces?line= lands on /developer until item 7's Traces page does —
 * the seams are the current contract, and this spec pins them.
 *
 * Model boundary: the voice roles are the project's fake gateway (a
 * synthetic stub, named for what it is) — the regenerate action's assertion
 * is the 202 that queues the audition job (AGENTS.md rule 3); no narration,
 * session or preview runs in this walk.
 */

const ORIGIN = `http://127.0.0.1:${ports.ksearch}`;
const ADMIN = { username: "ksearch-admin", password: "steady as the tide" };
const GAME = { slug: "ksearch-harbour", title: "Ksearch Harbour" };
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

/** The design's three review widths (docs/design/plum-peach README, IA). */
const WIDTHS = [1440, 1024, 390] as const;

/** WCAG 2.1 A/AA: the 2.0 A/AA rules plus the rules 2.1 adds at those levels. */
const WCAG_21_A_AA = ["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"] as const;

/** The six palette categories ADR-0098 groups by, in display order. */
const KINDS = ["game", "character", "line", "trace", "setting", "action"] as const;

async function signIn(page: Page): Promise<void> {
  await page.goto("/login");
  await expect(page.getByTestId("login-form")).toBeVisible({ timeout: 30_000 });
  await page.getByTestId("login-username").fill(ADMIN.username);
  await page.getByTestId("login-password").fill(ADMIN.password);
  await page.getByTestId("login-submit").click();
  await expect(page.getByTestId("nav-user")).toContainText(ADMIN.username, { timeout: 20_000 });
  await expect(page.getByTestId("nav-role")).toHaveText("admin");
}

/** The game id the seeded library card links to. */
async function seededGameId(page: Page): Promise<string> {
  await page.goto("/library");
  const card = page.getByTestId(`game-${GAME.slug}`);
  await expect(card).toBeVisible({ timeout: 20_000 });
  const href = (await card.getAttribute("href")) ?? "";
  const id = /\/games\/([^/]+)\/overview/.exec(href)?.[1] ?? "";
  expect(id, `the card link ${href} carries the game id`).toBeTruthy();
  return id;
}

/** Press ⌘K and wait for the palette's dialog (and its focused input). */
async function openPalette(page: Page): Promise<void> {
  await page.keyboard.press("Meta+k");
  await expect(page.getByTestId("palette")).toBeVisible({ timeout: 10_000 });
  await expect(page.getByRole("dialog", { name: "Search" })).toBeVisible();
  await expect(page.getByTestId("palette-input")).toBeFocused();
}

/** Type a query and wait until its read has settled (a group or the
 *  nothing-matched state is on screen, not the loading line). */
async function query(page: Page, text: string): Promise<void> {
  const input = page.getByTestId("palette-input");
  await input.fill(text);
  await expect(page.getByTestId("palette-loading")).toHaveCount(0, { timeout: 15_000 });
}

/** The ids of the categories whose group header is on screen. */
async function visibleKinds(page: Page): Promise<string[]> {
  const present: string[] = [];
  for (const kind of KINDS) {
    if ((await page.getByTestId(`palette-group-${kind}`).count()) > 0) present.push(kind);
  }
  return present;
}

test("⌘K: claim and seed the synthetic game", async ({ page }) => {
  test.setTimeout(600_000);
  const run = serverRun("ksearch");
  const consoleErrors = trackConsoleErrors(page, [401, 503]);

  // Claim the appliance and sign in as admin, through the page.
  await page.goto("/setup");
  await expect(page.getByTestId("setup-form")).toBeVisible({ timeout: 30_000 });
  await page.getByTestId("setup-username").fill(ADMIN.username);
  await page.getByTestId("setup-password").fill(ADMIN.password);
  await page.getByTestId("setup-submit").click();
  await expect(page).toHaveURL(/\/login$/, { timeout: 20_000 });
  await page.getByTestId("login-username").fill(ADMIN.username);
  await page.getByTestId("login-password").fill(ADMIN.password);
  await page.getByTestId("login-submit").click();
  await expect(page.getByTestId("nav-user")).toContainText(ADMIN.username);
  await expect(page.getByTestId("nav-role")).toHaveText("admin");

  // Seed the synthetic game: create, edition, draft, import, publish.
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
  await expect(page.getByRole("tab", { name: "us / en / disc-1" })).toBeVisible({
    timeout: 20_000,
  });
  await page
    .getByTestId(/edition-panel-/)
    .getByTestId("new-draft")
    .click();
  await expect(page.getByTestId("revision-1")).toBeVisible({ timeout: 20_000 });

  const revision1 = page.getByTestId("revision-1");
  await revision1.getByTestId("import-file").setInputFiles(SCRIPT_CSV);
  await revision1.getByTestId("import-submit").click();
  await expect(page.getByTestId("job-progress")).toContainText("succeeded", {
    timeout: 60_000,
  });
  await expect(page.getByTestId("import-inserted")).toHaveText("40");

  await revision1.getByTestId("status-review").click();
  await expect(revision1.getByTestId("badge-review")).toBeVisible();
  await revision1.getByTestId("publish").click();
  await expect(
    revision1.getByTestId("badge-published"),
    "the seeded revision is published",
  ).toBeVisible({ timeout: 60_000 });

  // The seeded game's search surface is live: the empty-query palette read
  // (recents) already names the game on the library page.
  await page.goto("/library");
  await expect(page.getByTestId(`game-${GAME.slug}`)).toBeVisible();
  await openPalette(page);
  await expect(page.getByTestId("palette-group-game")).toBeVisible({ timeout: 15_000 });
  await expect(page.getByTestId("palette-row").first()).toContainText(GAME.title);
  await page.keyboard.press("Escape");
  await expect(page.getByTestId("palette")).toHaveCount(0);

  expect(consoleErrors).toEqual([]);
  expectDefaultDataDirUntouched(run);
});

test("⌘K opens the overlay (role=dialog) from any page without navigation", async ({ page }) => {
  test.setTimeout(300_000);
  // 409 joins 503 for the /health stop's capture-signal read: on this
  // appliance no session runs and the one-shot grab cannot run, so the
  // route answers 409 `capture-signal-unavailable` with the operator
  // action (ADR-0110 §1) and the tile renders it as its own state — the
  // machine's answer, not a fault of the walk.
  const consoleErrors = trackConsoleErrors(page, [401, 409, 503]);
  await signIn(page);

  const stops: Array<{ path: string; settled: (page: Page) => Promise<void> }> = [
    {
      path: "/library",
      settled: (page) => expect(page.getByTestId("game-list")).toBeVisible({ timeout: 20_000 }),
    },
    {
      path: "/play",
      settled: (page) =>
        expect(page.getByTestId("play-start-form")).toBeVisible({ timeout: 20_000 }),
    },
    {
      // /traces is the Traces screen itself now (the ADR-0109 rename landed):
      // the line-trace list settles on its panel.
      path: "/traces",
      settled: (page) => expect(page.getByTestId("traces-list")).toBeVisible({ timeout: 20_000 }),
    },
    {
      path: "/settings",
      settled: (page) =>
        expect(page.getByTestId("settings-roles")).toBeVisible({ timeout: 20_000 }),
    },
    {
      // /health is the Health screen (the ADR-0093 §4 seam renamed in
      // ADR-0111): the status rollup settles on the page's main testid.
      path: "/health",
      settled: (page) => expect(page.getByTestId("health-page")).toBeVisible({ timeout: 20_000 }),
    },
  ];

  for (const stop of stops) {
    await page.goto(stop.path);
    await stop.settled(page);
    const before = page.url();

    await openPalette(page);
    // The overlay is up on THIS page: the dialog role and its input, and the
    // URL is exactly where it was — no navigation to a search page.
    await expect(page.getByRole("dialog", { name: "Search" })).toBeVisible();
    await expect(page).toHaveURL(before);

    // Esc closes it and the page stays put.
    await page.keyboard.press("Escape");
    await expect(page.getByTestId("palette")).toHaveCount(0);
    await expect(page).toHaveURL(before);
  }

  expect(consoleErrors).toEqual([]);
});

test("⌘K: grouped results across categories; ↵ per row kind", async ({ page }) => {
  test.setTimeout(600_000);
  const consoleErrors = trackConsoleErrors(page, [401, 503]);
  await signIn(page);
  const gameId = await seededGameId(page);

  await page.goto("/library");
  await expect(page.getByTestId("game-list")).toBeVisible();

  // ── a query returns grouped results spanning ≥2 of the six categories ──
  // "harbour": the game title, the harbourmaster's lines, and the verb the
  // query's game hit expands ("start narrating …").
  await openPalette(page);
  await query(page, "harbour");
  const kinds = await visibleKinds(page);
  expect(kinds.length, `the query "harbour" spanned ${kinds.join(", ")}`).toBeGreaterThanOrEqual(2);
  expect(kinds).toContain("game");
  expect(kinds).toContain("line");
  // Group headers are the mono category names (the design's mock 2b).
  await expect(page.getByTestId("palette-group-game")).toHaveText("Games");
  await expect(page.getByTestId("palette-group-line")).toHaveText("Lines");
  // The group order is the server's canonical one (game before line).
  const groupOrder = kinds.filter((kind) => kind === "game" || kind === "line");
  expect(groupOrder).toEqual(["game", "line"]);

  // ── ↵ on a line row opens the line's trace ──────────────────────────────
  // The line rows carry the line id in mono and the /traces?line= route; the
  // ADR-0109 rename landed the trace query on the Traces screen itself.
  const lineRow = page.locator('[data-testid="palette-row"][data-kind="line"]').first();
  const lineRoute = (await lineRow.getAttribute("data-route")) ?? "";
  expect(lineRoute, "the line row targets the line's trace").toMatch(/^\/traces\?line=/);
  // The line id shown in mono is the one the route names.
  await expect(lineRow.getByTestId("palette-row-id")).toHaveText(lineRoute.split("=")[1] ?? "");

  // Navigate to the first line row (the single game row leads) and ↵.
  await page.keyboard.press("ArrowDown");
  await page.keyboard.press("ArrowDown");
  await expect(lineRow).toBeFocused();
  await page.keyboard.press("Enter");
  await expect(page).toHaveURL(/traces\?line=/, { timeout: 20_000 });
  await expect(page.getByTestId("palette")).toHaveCount(0);

  // ── ↵ on a character row lands on the Cast page ─────────────────────────
  // "marla" ranks the character, and the character row targets the game's
  // Cast page (ADR-0098's IA: /games/:id/cast) — the Cast seam page the
  // setup board's completion lands on, so the CTA is live there.
  await openPalette(page);
  await query(page, "marla");
  const characterRow = page.locator('[data-testid="palette-row"][data-kind="character"]').first();
  await expect(characterRow).toContainText("Marla");
  await expect(characterRow).toHaveAttribute("data-route", `/games/${gameId}/cast`);
  await page.keyboard.press("ArrowDown"); // the character row leads this answer
  await expect(characterRow).toBeFocused();
  await page.keyboard.press("Enter");
  await expect(page).toHaveURL(`/games/${gameId}/cast`, { timeout: 20_000 });
  await expect(page.getByTestId("cast-start-narrating")).toBeVisible();
  await expect(page.getByTestId("palette")).toHaveCount(0);

  // ── ↵ on an action row runs the action ──────────────────────────────────
  // The same "marla" answer offers the registry's only API target: the
  // voice-regenerate audition. ↵ queues it — the assertion is the 202, not
  // the job's run (no model in this walk).
  await openPalette(page);
  await query(page, "marla");
  const actionRow = page
    .locator(
      '[data-testid="palette-row"][data-kind="action"][data-action="POST /api/v1/voices/auditions"]',
    )
    .first();
  await expect(actionRow).toBeVisible({ timeout: 15_000 });
  await expect(actionRow).toContainText("regenerate Marla");
  // The "Ask the agent: …" row leads the action group (the mouse path to Tab).
  const askRow = page.locator('[data-testid="palette-row"][data-id="ask-agent"]');
  await expect(askRow).toBeVisible();

  const [audition] = await Promise.all([
    page.waitForResponse(
      (response) =>
        response.url().includes("/api/v1/voices/auditions") &&
        response.request().method() === "POST",
      { timeout: 20_000 },
    ),
    (async () => {
      // Focus the action row and ↵ (the row list's order is the server's,
      // and a query's trace rows make a keystroke count position-dependent).
      await actionRow.focus();
      await expect(actionRow).toBeFocused();
      await page.keyboard.press("Enter");
    })(),
  ]);
  expect(audition.status(), "the audition is queued").toBe(202);
  const queued = await audition.json();
  expect(queued.job_id).toBeTruthy();
  const sent = await audition.request().postDataJSON();
  expect(sent.character).toBe("Marla");
  // The palette reports the run's honest outcome (the queued job id), not a
  // success it cannot know yet.
  await expect(page.getByTestId("palette-result")).toContainText("job");
  await page.keyboard.press("Escape");
  await expect(page.getByTestId("palette")).toHaveCount(0);

  // ── ↵ on an action row with a route navigates ───────────────────────────
  // "ksearch" matches the game; the verb row targets /play?game=<id>.
  await openPalette(page);
  await query(page, "ksearch");
  const navigateRow = page
    .locator(`[data-testid="palette-row"][data-kind="action"][data-route="/play?game=${gameId}"]`)
    .first();
  await expect(navigateRow).toBeVisible({ timeout: 15_000 });
  await expect(navigateRow).toContainText("start narrating");
  // ↵ on the focused action row (same reason as above: no keystroke count).
  await navigateRow.focus();
  await expect(navigateRow).toBeFocused();
  await page.keyboard.press("Enter");
  await expect(page).toHaveURL(`/play?game=${gameId}`, { timeout: 20_000 });
  await expect(page.getByTestId("palette")).toHaveCount(0);

  expect(consoleErrors).toEqual([]);
});

test("⌘K: Tab hands the query to the agent dock; the message includes it", async ({ page }) => {
  test.setTimeout(300_000);
  const consoleErrors = trackConsoleErrors(page, [401, 503]);
  await signIn(page);
  await page.goto("/library");
  await expect(page.getByTestId("game-list")).toBeVisible();

  const QUERY = "marla";
  await openPalette(page);
  await query(page, QUERY);
  // Tab: the overlay closes and the dock opens with the query pre-filled —
  // the normalised spelling the server echoes as agent_prompt.
  await page.keyboard.press("Tab");
  await expect(page.getByTestId("palette")).toHaveCount(0);
  await expect(page.getByTestId("agent-dock")).toBeVisible();
  await expect(page.getByTestId("agent-dock-input")).toHaveValue(QUERY);

  // The sent agent message includes the query: the message the dock sends is
  // the pre-filled text, and the durable job payload the attempt is queued
  // with carries it as the opening user request. (The dock's send button is
  // plan item 3's; this asserts the seam's contract at the boundary the item
  // keeps. The attempt's user turn lands in the durable thread when the
  // attempt runs — a model's step — so the payload is the assertion, not the
  // conversation tail.)
  const cookies = await page.context().cookies();
  const csrfToken = cookies.find((cookie) => cookie.name === "gn_csrf")?.value ?? "";
  expect(csrfToken, "the session's CSRF twin is readable by the client").toBeTruthy();

  const response = await page.request.post(`${ORIGIN}/api/v1/assistant/messages`, {
    data: { text: QUERY },
    headers: { "X-CSRF-Token": csrfToken },
  });
  expect(response.status(), "the message is accepted and the attempt queued").toBe(202);
  const accepted = await response.json();
  expect(accepted.thread_id).toBeTruthy();
  expect(accepted.job_id).toBeTruthy();

  const job = await (await page.request.get(`${ORIGIN}/api/v1/jobs/${accepted.job_id}`)).json();
  expect(job.kind).toBe("assistant.run");
  const userRequest = job.payload?.user_request ?? "";
  expect(
    userRequest.includes(QUERY),
    `the sent agent message (the attempt's opening user request) includes the query; got: ${userRequest.slice(0, 200)}`,
  ).toBe(true);

  // The "Ask the agent: …" row is the mouse path to the same handoff: it
  // closes the palette, opens the dock, and pre-fills the same query.
  await openPalette(page);
  await query(page, QUERY);
  await page.locator('[data-testid="palette-row"][data-id="ask-agent"]').click();
  await expect(page.getByTestId("palette")).toHaveCount(0);
  await expect(page.getByTestId("agent-dock")).toBeVisible();
  await expect(page.getByTestId("agent-dock-input")).toHaveValue(QUERY);

  expect(consoleErrors).toEqual([]);
});

test("⌘K: screenshots, axe and no horizontal scroll at 390", async ({ page }, testInfo) => {
  test.setTimeout(600_000);
  const run = serverRun("ksearch");
  const consoleErrors = trackConsoleErrors(page, [401, 503]);
  await signIn(page);
  await page.goto("/library");
  await expect(page.getByTestId("game-list")).toBeVisible();

  // The open palette with a settled, grouped answer: the state the design's
  // mock 2b shows (input, mono group headers, the row list, the footnote).
  await openPalette(page);
  await query(page, "harbour");
  await expect(page.getByTestId("palette-group-line")).toBeVisible();

  interface ScanReport {
    width: number;
    serious: string[];
    violations: Record<string, number>;
  }
  const reports: ScanReport[] = [];
  for (const width of WIDTHS) {
    await page.setViewportSize({ width, height: 900 });
    // Settle the fonts: the self-hosted faces swap in asynchronously and the
    // fallback metrics are wider — shooting before the swap reads the
    // fallback layout, not the design's.
    await page.evaluate(async () => {
      const g = globalThis as unknown as {
        document?: { fonts?: { ready?: Promise<unknown> } };
      };
      await g.document?.fonts?.ready;
    });
    // Phone width: the dialog never widens the document (rows truncate).
    if (width === 390) {
      const measures = await page.evaluate(() => {
        const g = globalThis as {
          document?: { documentElement?: { scrollWidth?: number } };
          innerWidth?: number;
        };
        return {
          scrollWidth: g.document?.documentElement?.scrollWidth ?? 0,
          innerWidth: g.innerWidth ?? 0,
        };
      });
      expect(
        measures.scrollWidth,
        `the palette @390px scrolls horizontally: documentElement.scrollWidth ${measures.scrollWidth} > innerWidth ${measures.innerWidth}`,
      ).toBeLessThanOrEqual(measures.innerWidth);
    }

    const shot = `ksearch-palette-${width}.png`;
    const shotPath = `${testInfo.outputDir}/${shot}`;
    await page.screenshot({ path: shotPath });
    await testInfo.attach(shot, {
      body: readFileSync(shotPath),
      contentType: "image/png",
    });

    const results = await new AxeBuilder({ page }).withTags([...WCAG_21_A_AA]).analyze();
    const violations = results.violations ?? [];
    const byRule: Record<string, number> = {};
    for (const violation of violations) {
      byRule[violation.id] = (byRule[violation.id] ?? 0) + violation.nodes.length;
    }
    reports.push({
      width,
      violations: byRule,
      serious: violations
        .filter((violation) => violation.impact === "serious" || violation.impact === "critical")
        .map(
          (violation) =>
            `${violation.id} [${violation.impact}] ×${violation.nodes.length} — ${violation.help}`,
        ),
    });
  }
  await testInfo.attach("axe-reports.json", {
    body: JSON.stringify(reports, null, 2),
    contentType: "application/json",
  });

  // any serious or critical violation on any width fails the walk.
  const failing = reports.filter((report) => report.serious.length > 0);
  expect(
    failing,
    `serious/critical axe violations on the ⌘K palette:\n${failing
      .map((report) => `— @${report.width}px:\n${report.serious.join("\n")}`)
      .join("\n\n")}`,
  ).toEqual([]);

  expect(consoleErrors).toEqual([]);
  expectDefaultDataDirUntouched(run);
});
