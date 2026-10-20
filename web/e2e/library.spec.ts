import AxeBuilder from "@axe-core/playwright";
import { expect, test, type APIRequestContext, type Page } from "@playwright/test";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

import { ports } from "./support/ports";
import {
  expectDefaultDataDirUntouched,
  expectIsolatedDataDir,
  serverNameForProject,
  serverRun,
} from "./support/servers";
import { trackConsoleErrors } from "./support/console";
import { acquireScenarioLock } from "./support/scenario-lock";

/**
 * The /library screen (docs/design/plum-peach 2a; ADR-0109) on a fresh
 * packaged-binary appliance with the `agent_reasoning` role pointed at the
 * scripted model gateway (web/e2e/fake-model.mjs) — the same labelled-synthetic
 * boundary the onboarding walk uses (AGENTS.md rule 3: the model is scripted,
 * everything around it is real: the real API, the real job queue, the real
 * agent loop, the real status stream).
 *
 * The seed the acceptance asks for:
 *
 * - one READY game ("Ready Voyage"): created, editioned, drafted, the
 *   synthetic script imported and the revision published, through the UI the
 *   way the a11y walk seeds (create → edition → draft → import → review →
 *   publish).
 * - one IN-SETUP game ("Setting Voyage"): a live onboarding run on the
 *   scripted gateway (scenario "stall") left in an active state with its
 *   content in place: the scenario imports the staged script, runs the
 *   quality report, then deliberately stalls by re-issuing the same read-only
 *   preview_profile call; the loop's repetition guard ends the run BLOCKED
 *   (labelled-synthetic: the model is scripted, AGENTS.md rule 3) — a
 *   durable active state whose counts (0 of 5 voiced) the status bar
 *   renders and the shelf chip must show on its tile.
 *
 * Asserted, then: the ready game is the hero (the hero-slot policy of ADR
 * 0093 open question (e)), the ⌘K pill and the shelf/list toggle on the
 * "All games" row, the dashed Add-a-game tile first on the shelf, the
 * setting-up game's progress chip on its tile AND in the status bar, the
 * shelf/list choice persisting across reload (UX-08), and ▶ landing on
 * /play with an active session. Screenshots at the design's three widths
 * (1440 / 1024 / 390) with axe at zero serious/critical and no horizontal
 * scroll at 390 (AGENTS.md rule 9).
 */

const TESTDATA = resolve(
  import.meta.dirname,
  "..",
  "..",
  "internal",
  "games",
  "adapter",
  "generic",
  "testdata",
);
const ADVENTURE_CSV = resolve(TESTDATA, "synthetic-adventure.csv");

const ADMIN = { username: "library-admin", password: "steady as the tide" };
const FAKE_MODEL = `http://127.0.0.1:${ports.fakeModel}`;
const WAIT_MS = 90_000;

const READY = { slug: "ready-voyage", title: "Ready Voyage" };
const SETTING = { slug: "setting-voyage", title: "Setting Voyage" };
/** The design's three review widths (docs/design/plum-peach README, IA). */
const WIDTHS = [1440, 1024, 390] as const;

/** WCAG 2.1 A/AA: the 2.0 A/AA rules plus the rules 2.1 adds at those levels. */
const WCAG_21_A_AA = ["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"] as const;

interface ScanReport {
  url: string;
  width: number;
  view: string;
  serious: string[];
  violations: Record<string, number>;
}

/** Point the scripted gateway at a scenario (the servers share the process's
 *  lifetime; the spec runs serially, workers=1). */
async function setScenario(request: APIRequestContext, scenario: string) {
  const response = await request.post(`${FAKE_MODEL}/_scenario`, { data: { scenario } });
  expect(response.status(), `the fake model accepted scenario ${scenario}`).toBe(200);
}

/** Poll the status read (the bar's own source) until a setup run is active
 *  and blocked: the stall's repetition guard has ended the seed's run in a
 *  durable active state, its content (the import) in place. Returns the run's
 *  id; fails on any other terminal answer. */
async function expectRunBlocked(request: APIRequestContext): Promise<string> {
  const apiOrigin = `http://127.0.0.1:${ports.library}`;
  const deadline = Date.now() + WAIT_MS;
  for (;;) {
    const response = await request.get(`${apiOrigin}/api/v1/status`);
    expect(response.status(), "the status read answers 200").toBe(200);
    const snap = (await response.json()) as {
      setup_runs?: Array<{ run_id: string; state: string; voiced: number; characters: number }>;
    };
    const run = snap.setup_runs?.[0];
    if (run !== undefined && run.state === "blocked") {
      expect(run.characters, "the imported content is counted (5 speakers)").toBe(5);
      expect(run.voiced, "nothing is voiced yet").toBe(0);
      return run.run_id;
    }
    if (run === undefined) {
      throw new Error(
        `the setup run ended (terminal) before the stall's repetition guard blocked it: the stall lost the race`,
      );
    }
    // transient (queued/running): the stall's repetitions are in flight.
    if (Date.now() > deadline) {
      throw new Error(`the run stayed ${run.state} past ${WAIT_MS}ms: the stall never blocked it`);
    }
    await new Promise((resolve) => setTimeout(resolve, 500));
  }
}

/** The bar's live string is the server-rendered one (not the "· status ·"
 *  placeholder the feed-less fallback shows). */
async function liveTextSettled(page: Page): Promise<void> {
  await page.waitForFunction(
    (selector: string) => {
      const g = globalThis as {
        document?: { querySelector(sel: string): { textContent: string | null } | null };
      };
      const el = g.document?.querySelector(selector) ?? null;
      const text = (el?.textContent ?? "").trim();
      return text.length > 0 && !text.includes("status ·");
    },
    "[data-testid='status-live']",
    { timeout: 15_000 },
  );
}

/** Settle the self-hosted fonts (font-display: swap) before measuring or
 *  shooting, so the layout read is the design's, not the fallback's. */
async function fontsReady(page: Page): Promise<void> {
  await page.evaluate(async () => {
    const g = globalThis as unknown as {
      document?: { fonts?: { ready?: Promise<unknown> } };
    };
    await g.document?.fonts?.ready;
  });
}

/** No horizontal page scroll at the width (the document must never be wider
 *  than the viewport; inner tables may scroll within their own wrappers). */
async function expectNoHorizontalScroll(page: Page, width: number): Promise<void> {
  const dims = await page.evaluate(() => {
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
    dims.scrollWidth,
    `/library @${width}px scrolls horizontally: documentElement.scrollWidth ${dims.scrollWidth} > innerWidth ${dims.innerWidth}`,
  ).toBeLessThanOrEqual(dims.innerWidth);
}

test("library: hero, status chip, view persistence and a real session start", async ({
  page,
  request,
}, testInfo) => {
  test.setTimeout(600_000);
  test.skip(
    testInfo.project.name !== "library",
    "runs on the library project's fresh appliance (scripted model)",
  );
  await expectIsolatedDataDir(serverNameForProject("library"));
  // 503 joins 401 on the ignore list: on this appliance the capture layer is
  // unwired (no UVC dongle, no test seam), and the Settings capture section
  // renders that 503 as its own "no capture layer" note.
  const consoleErrors = trackConsoleErrors(page, [401, 503]);

  // The scenario lock (ADR-0120): the in-setup seed drives the shared
  // fake-model gateway's one global scenario (stall), so this walk is serial
  // with the other scenario walks across the run's workers.
  const releaseScenario = await acquireScenarioLock(testInfo, "stall");
  try {
    // ── claim the appliance and sign in ──────────────────────────────────────
    await setScenario(request, "stall");
    await page.goto("/");
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

    // ── seed 1: the READY game, through the UI (the a11y walk's seed) ────────
    await page.goto("/library");
    await page.getByTestId("new-game").click();
    await page.getByTestId("new-game-slug").fill(READY.slug);
    await page.getByTestId("new-game-title").fill(READY.title);
    await page.getByTestId("new-game-platform").fill("GameCube");
    await page.getByTestId("new-game-submit").click();
    const readyCard = page.getByTestId(`game-${READY.slug}`);
    await expect(readyCard, "the ready game lands on the shelf").toContainText(READY.title, {
      timeout: 30_000,
    });

    await readyCard.click();
    await expect(page.getByTestId("game-title")).toHaveText(READY.title);
    const readyGameId = /\/games\/([^/]+)\/overview/.exec(page.url())?.[1] ?? "";
    expect(readyGameId, "the overview URL carries the game id").toBeTruthy();

    await page.getByTestId("new-edition").click();
    await page.getByTestId("edition-region").fill("us");
    await page.getByTestId("edition-language").fill("en");
    await page.getByTestId("edition-revision").fill("rev0");
    await page.getByTestId("edition-submit").click();
    const editionTab = page.getByRole("tab", { name: "us / en / rev0" });
    await expect(editionTab).toBeVisible({ timeout: 20_000 });
    await page
      .getByTestId(/edition-panel-/)
      .getByTestId("new-draft")
      .click();
    await expect(page.getByTestId("revision-1")).toBeVisible({ timeout: 20_000 });

    const revision1 = page.getByTestId("revision-1");
    await revision1.getByTestId("import-file").setInputFiles(ADVENTURE_CSV);
    await revision1.getByTestId("import-submit").click();
    await expect(page.getByTestId("job-progress")).toContainText("succeeded", { timeout: 60_000 });
    await expect(page.getByTestId("import-inserted")).toHaveText("40");

    // Calibrate: a published profile is runnable only with observable regions
    // (sessions.go answers profile-not-runnable without one), so the draft gets
    // the region editor's minimal calibration — one set-wide textbox box —
    // through the same API the editor calls, before the publish freezes it.
    const editionsResp = await page.request.get(`/api/v1/games/${readyGameId}/editions`);
    expect(editionsResp.status(), "the editions list answers").toBe(200);
    const editions = (await editionsResp.json()) as { editions: Array<{ id: string }> };
    expect(editions.editions, "the seed created exactly one edition").toHaveLength(1);
    const profilesResp = await page.request.get(
      `/api/v1/editions/${editions.editions[0].id}/profiles`,
    );
    expect(profilesResp.status(), "the profiles list answers").toBe(200);
    const profiles = (await profilesResp.json()) as {
      profiles: Array<{ id: string; status: string }>;
    };
    const readyDraft = profiles.profiles.find((p) => p.status === "draft");
    if (readyDraft === undefined) {
      throw new Error(
        `the imported draft is not listed: ${JSON.stringify(profiles.profiles.map((p) => p.status))}`,
      );
    }
    const profileResp = await page.request.get(`/api/v1/profiles/${readyDraft.id}`);
    expect(profileResp.status(), "the profile read answers").toBe(200);
    const ifMatch = profileResp.headers()["etag"];
    // Cookie auth + an unsafe method needs the session's CSRF token (the
    // non-HttpOnly gn_csrf cookie, the SPA's own source) and a same-origin
    // Origin: page.request is not a browser fetch, so the spec supplies both.
    const csrfCookie = (await page.context().cookies()).find((c) => c.name === "gn_csrf");
    const putRegions = await page.request.put(`/api/v1/profiles/${readyDraft.id}/regions`, {
      headers: {
        "if-match": ifMatch,
        "x-csrf-token": csrfCookie?.value ?? "",
        origin: new URL(page.url()).origin,
      },
      data: {
        regions: [{ label: "textbox", region_type: "textbox", x: 0.1, y: 0.75, w: 0.8, h: 0.15 }],
      },
    });
    expect(putRegions.status(), "the set-wide textbox box is accepted").toBe(200);

    await revision1.getByTestId("status-review").click();
    await expect(revision1.getByTestId("badge-review")).toBeVisible();
    await revision1.getByTestId("publish").click();
    await expect(
      revision1.getByTestId("badge-published"),
      "the ready game's revision is published",
    ).toBeVisible({ timeout: 60_000 });

    // ── seed 2: the IN-SETUP game, a live onboarding run left active ───────────
    await page.goto("/games/new");
    await expect(page.getByTestId("onb-setup-form")).toBeVisible({ timeout: 20_000 });
    await page.getByTestId("onb-title").fill(SETTING.title);
    await page.getByTestId("onb-platform").fill("GameCube");
    await page.getByTestId("onb-web-toggle").uncheck();
    await page.getByTestId("onb-file").setInputFiles(ADVENTURE_CSV);
    await page.getByTestId("onb-start").click();

    const importCall = page.getByTestId("onb-tool-import_user_asset");
    await expect(importCall, "the import lands in the tool timeline").toBeVisible({
      timeout: WAIT_MS,
    });
    await expect(page.getByTestId("onb-tool-status-import_user_asset")).toHaveText("succeeded");
    const qualityCall = page.getByTestId("onb-tool-source_quality");
    await expect(qualityCall, "the quality run lands in the tool timeline").toBeVisible({
      timeout: WAIT_MS,
    });
    await expect(page.getByTestId("onb-tool-status-source_quality")).toHaveText("succeeded");

    // The stall: the scripted model keeps re-issuing the same read-only
    // preview_profile call, so the loop's repetition guard ends the run
    // blocked. Poll the status read (the bar's own source, through the
    // signed-in page's request context) until the seed's run is the active
    // one — deterministic, not a wall-clock guess.
    await expectRunBlocked(page.request);

    // ── the library screen ────────────────────────────────────────────────────
    await page.setViewportSize({ width: 1440, height: 900 });
    await page.goto("/library");
    await expect(page.getByTestId("lib-hero")).toBeVisible({ timeout: WAIT_MS });
    await liveTextSettled(page);

    // 1. The ready game is the hero (the hero-slot policy): its title, its
    //    summary line and the design's three pills.
    await expect(page.getByTestId("lib-hero-title")).toHaveText(READY.title);
    await expect(page.getByTestId("lib-hero-summary")).toContainText("40 lines");
    await expect(page.getByTestId("lib-hero-summary")).toContainText("5 characters");
    await expect(page.getByTestId("lib-hero-start")).toBeVisible();
    await expect(page.getByTestId("lib-hero-cast")).toBeVisible();
    await expect(page.getByTestId("lib-hero-dialogue")).toBeVisible();

    // 2. The setting-up game's progress: on its tile chip AND in the status
    //    bar (the server-rendered string — the bar and the chip read the same
    //    store counts, so they cannot disagree).
    const settingChip = page.getByTestId(`lib-chip-${SETTING.slug}`);
    await expect(settingChip).toBeVisible({ timeout: WAIT_MS });
    await expect(settingChip).toContainText("setting up");
    await expect(settingChip).toContainText("0 of 5 voiced");
    await expect(page.getByTestId("status-live")).toContainText("agent blocked");
    await expect(page.getByTestId("status-live")).toContainText("0 of 5 voiced");

    // 3. The "All games" row: the ⌘K pill opens the palette; the shelf/list
    //    toggle is the design's segmented control.
    const allGames = page.getByTestId("lib-all-games-row");
    await expect(allGames.getByRole("heading", { name: "All games" })).toBeVisible();
    await expect(page.getByTestId("lib-kbd-pill")).toBeVisible();
    await page.getByTestId("lib-kbd-pill").click();
    await expect(page.getByTestId("palette")).toBeVisible();
    await expect(page.getByTestId("palette-input")).toBeFocused();
    await page.keyboard.press("Escape");
    await expect(page.getByTestId("palette")).toHaveCount(0);

    // 4. The shelf: the dashed Add-a-game tile first, then the game tiles
    //    (the ready tile carries its published chip and its ▶).
    const shelf = page.getByTestId("lib-shelf");
    await expect(shelf).toBeVisible();
    const addTile = page.getByTestId("lib-add-game-tile");
    await expect(addTile, "the dashed Add-a-game tile is the shelf's first tile").toBeVisible();
    await expect(addTile).toContainText("Add a game");
    const readyTile = page.getByTestId(`lib-tile-${READY.slug}`);
    await expect(readyTile).toBeVisible();
    await expect(page.getByTestId(`lib-chip-${READY.slug}`)).toContainText("published");
    await expect(page.getByTestId(`lib-play-${READY.slug}`)).toBeVisible();
    await expect(page.getByTestId(`lib-tile-${SETTING.slug}`)).toBeVisible();

    // 5. The list view, and the toggle's persistence across reload (UX-08).
    await page.getByTestId("lib-view-toggle").getByRole("tab", { name: "List" }).click();
    const list = page.getByTestId("lib-list");
    await expect(list, "the list view renders").toBeVisible();
    await expect(list.getByRole("row", { name: /Ready Voyage/ })).toContainText("40");
    await expect(list.getByRole("row", { name: /Ready Voyage/ })).toContainText("0/5");
    await expect(list.getByRole("row", { name: /Setting Voyage/ })).toContainText("0/5");
    await fontsReady(page);
    const listShotPath = `${testInfo.outputDir}/library-list-1440.png`;
    await page.screenshot({ path: listShotPath, fullPage: true });
    await testInfo.attach("library-list-1440.png", {
      body: readFileSync(listShotPath),
      contentType: "image/png",
    });
    await page.reload();
    await expect(
      page.getByTestId("lib-list"),
      "the list choice persists across reload (UX-08)",
    ).toBeVisible({
      timeout: WAIT_MS,
    });
    // and back the other way, for the symmetric proof.
    await page.getByTestId("lib-view-toggle").getByRole("tab", { name: "Shelf" }).click();
    await expect(page.getByTestId("lib-shelf")).toBeVisible();
    await page.reload();
    await expect(
      page.getByTestId("lib-shelf"),
      "the shelf choice persists across reload (UX-08)",
    ).toBeVisible({
      timeout: WAIT_MS,
    });

    // 6. Screenshots + axe at the design's three widths (AGENTS.md rule 9).
    const reports: ScanReport[] = [];
    for (const width of WIDTHS) {
      await page.setViewportSize({ width, height: 900 });
      await page.goto("/library");
      await expect(page.getByTestId("lib-hero")).toBeVisible({ timeout: WAIT_MS });
      await liveTextSettled(page);
      await fontsReady(page);
      await expectNoHorizontalScroll(page, width);
      const shot = `library-${width}.png`;
      const shotPath = `${testInfo.outputDir}/${shot}`;
      await page.screenshot({ path: shotPath, fullPage: true });
      await testInfo.attach(shot, { body: readFileSync(shotPath), contentType: "image/png" });

      const results = await new AxeBuilder({ page }).withTags([...WCAG_21_A_AA]).analyze();
      const violations = results.violations ?? [];
      const byRule: Record<string, number> = {};
      for (const violation of violations) {
        byRule[violation.id] = (byRule[violation.id] ?? 0) + violation.nodes.length;
      }
      reports.push({
        url: page.url(),
        width,
        view: "shelf",
        violations: Object.fromEntries(
          Object.entries(byRule).sort(([a], [b]) => a.localeCompare(b)),
        ),
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
    const failing = reports.filter((report) => report.serious.length > 0);
    expect(
      failing,
      `serious/critical axe violations on /library:\n${failing
        .map((report) => `— ${report.url} @${report.width}px:\n${report.serious.join("\n")}`)
        .join("\n\n")}`,
    ).toEqual([]);

    // 7. ▶ starts a session for the published profile and lands on /play with
    //    it active (the session id in the URL, the live panel settled).
    await page.setViewportSize({ width: 1440, height: 900 });
    await page.goto("/library");
    await expect(page.getByTestId("lib-hero-start")).toBeVisible({ timeout: WAIT_MS });
    await page.getByTestId("lib-hero-start").click();
    await expect(page, "the start lands on /play with the session in the URL").toHaveURL(
      /\/play\?session=.[^&]+/,
      { timeout: WAIT_MS },
    );
    await expect(page.getByTestId("play-live"), "the session is live on /play").toBeVisible({
      timeout: WAIT_MS,
    });
    await expect(page.getByTestId("play-session-id")).not.toHaveText("");
    // The session runs the ready game's published profile with the library's
    // synthetic frame source: the snapshot names test_pattern.
    await expect(page.getByTestId("play-source")).toContainText("test_pattern", {
      timeout: WAIT_MS,
    });

    // the walk must not have left a console error behind, and must not have
    // touched the developer's real data directory.
    expect(consoleErrors).toEqual([]);
    expectDefaultDataDirUntouched(serverRun("library"));
  } finally {
    releaseScenario();
  }
});
