import AxeBuilder from "@axe-core/playwright";
import { expect, test, type Page } from "@playwright/test";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

import { ports } from "./support/ports";
import { trackConsoleErrors } from "./support/console";
import { expectDefaultDataDirUntouched, serverRun } from "./support/servers";
import { acquireScenarioLock } from "./support/scenario-lock";

/**
 * The agent dock (ADR-0099; docs/design/plum-peach plan.md Phase 1 item 3,
 * UX-01): the ⌘J pill and the docked panel on a fresh packaged-binary
 * appliance whose agent_reasoning role is the scripted model gateway
 * (fake-model.mjs, scenario "assistant") — so the dock's send makes a
 * genuine provider request the scripted instance answers, and the walk
 * proves the provider boundary without a real model (AGENTS.md rule 3).
 *
 * The walk claims and seeds the appliance the way a11y.spec.ts does
 * (synthetic game, no game dialogue anywhere — AGENTS.md rule 5), then:
 *
 * - on a selection screen (`/games/:id/voices?profile=&character=`) the
 *   pill's chip reads `cast › <character>` and the outgoing message body
 *   carries the full context payload — asserted from a redacted capture of
 *   the request the page itself made (no fixture data leaves the appliance;
 *   the captured body contains only the placeholder message and ids);
 * - the chip follows the selection when it changes;
 * - on a bare screen the payload carries no selection;
 * - ⌘J reaches the dock from each of the six primary routes.
 *
 * Evidence: screenshots at the design's three widths (1440 / 1024 / 390) of
 * the docked panel with a live conversation, axe (WCAG 2.1 A/AA), and no
 * horizontal document scroll at 390 (AGENTS.md rule 9).
 */

const ADMIN = { username: "dock-admin", password: "steady as the tide" };
const GAME = { slug: "dock-harbour", title: "Dock Harbour" };
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
/** Placeholder copy only (AGENTS.md rule 5): the message the walk sends. */
const PLACEHOLDER_MESSAGE = "Placeholder message to the dock agent from the e2e walk.";
/** The scripted assistant's placeholder reply (fake-model.mjs). */
const SCRIPTED_REPLY = "Placeholder reply from the scripted assistant";
/** The design's three review widths (docs/design/plum-peach README, IA). */
const WIDTHS = [1440, 1024, 390] as const;
/** WCAG 2.1 A/AA: the 2.0 A/AA rules plus the rules 2.1 adds at those levels. */
const WCAG_21_A_AA = ["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"] as const;

/** The six primary routes the ⌘J pill must reach (plan.md item 3): the
 *  status-bar nav plus the two top-level destinations they redirect to. */
const PRIMARY_ROUTES: { path: string; chip: string; settle: (page: Page) => Promise<void> }[] = [
  {
    path: "/library",
    chip: "library",
    settle: (page) =>
      expect(
        page.locator('[data-testid="game-list"], [data-testid="new-game"]').first(),
      ).toBeVisible({ timeout: 20_000 }),
  },
  {
    path: "/play",
    chip: "play",
    settle: (page) => expect(page.getByTestId("play-start-form")).toBeVisible({ timeout: 20_000 }),
  },
  {
    // /traces is the Traces screen now (the ADR-0109 rename landed): the
    // chip names the destination and the line-trace list settles on its
    // panel.
    path: "/traces",
    chip: "traces",
    settle: (page) => expect(page.getByTestId("traces-list")).toBeVisible({ timeout: 20_000 }),
  },
  {
    path: "/settings",
    chip: "settings",
    settle: (page) => expect(page.getByTestId("settings-roles")).toBeVisible({ timeout: 20_000 }),
  },
  {
    // /health is the Health screen (the ADR-0093 §4 seam renamed in
    // ADR-0111): the readiness detail settles on the page.
    path: "/health",
    chip: "health",
    settle: (page) => expect(page.getByTestId("readiness")).toBeVisible({ timeout: 20_000 }),
  },
  {
    path: "/games/new",
    chip: "setup",
    settle: (page) => expect(page.getByTestId("onb-file")).toBeVisible({ timeout: 20_000 }),
  },
];

/** Settle the fonts before a shot: the self-hosted faces swap in
 *  asynchronously and the fallback metrics are wider (the redesign walk's
 *  same guard). */
async function fontsSettled(page: Page): Promise<void> {
  await page.evaluate(async () => {
    const g = globalThis as unknown as {
      document?: { fonts?: { ready?: Promise<unknown> } };
    };
    await g.document?.fonts?.ready;
  });
}

test("agent dock: context payload, the selection chip and ⌘J reachability (UX-01)", async ({
  page,
}, testInfo) => {
  test.setTimeout(600_000);
  test.skip(
    testInfo.project.name !== "agent-dock",
    "runs on the agent-dock project's fresh appliance",
  );
  const run = serverRun("agent-dock");
  // 503 joins 401 on the ignore list: on this appliance the capture layer is
  // unwired (no UVC dongle, no test seam) and the settings/health
  // surfaces render that 503 as their own "unavailable" note — the machine's
  // answer, not a fault of the walk. 409 joins them for the same layer on
  // the health page's signal read: ADR-0110 §1's 409 carries the layer's own
  // reason and the tile renders it as its own state.
  const consoleErrors = trackConsoleErrors(page, [401, 409, 503]);

  // The scripted model: the assistant scenario — one plain final reply, no
  // tool calls. The scenario is global to the shared gateway, so the spec
  // sets its own before the first chat request (the onboarding spec does
  // the same for its scenarios; the scenario lock — ADR-0112 — keeps these
  // walks serial across the run's workers).
  const releaseScenario = await acquireScenarioLock(testInfo, "assistant");
  try {
    const scenario = await fetch(`http://127.0.0.1:${ports.fakeModel}/_scenario`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ scenario: "assistant" }),
    });
    expect(scenario.status, "the scripted gateway accepts the assistant scenario").toBe(200);

    // ── 1. claim the appliance and sign in, through the page ───────────────
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

    // ── 2. seed the synthetic game: create, edition, draft, import, publish ─
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
    await revision1.getByTestId("status-review").click();
    await expect(revision1.getByTestId("badge-review")).toBeVisible();
    await revision1.getByTestId("publish").click();
    await expect(revision1.getByTestId("badge-published")).toBeVisible({ timeout: 60_000 });

    // ── 3. the ids the payload must carry: edition → profile → roster ─────
    const editions = (await (
      await page.request.get(`/api/v1/games/${gameId}/editions`)
    ).json()) as {
      editions: { id: string }[];
    };
    expect(editions.editions, "the seeded edition exists").toHaveLength(1);
    const editionId = editions.editions[0].id;
    const profiles = (await (
      await page.request.get(`/api/v1/editions/${editionId}/profiles`)
    ).json()) as { profiles: { id: string; state: string }[] };
    expect(profiles.profiles, "the seeded profile exists").toHaveLength(1);
    const profileId = profiles.profiles[0].id;
    const roster = (await (
      await page.request.get(`/api/v1/profiles/${profileId}/characters`)
    ).json()) as { characters: { id: string; name: string }[] };
    expect(roster.characters.length, "the synthetic script has characters").toBeGreaterThanOrEqual(
      2,
    );
    const first = roster.characters[0];
    const second = roster.characters[1];
    const voicesUrl = (characterId: string) =>
      `/games/${gameId}/voices?profile=${profileId}&character=${characterId}`;

    // ── 4. the selection screen: the chip, the dock and the context payload ─
    const captured: string[] = [];
    page.on("request", (request) => {
      if (request.method() === "POST" && request.url().includes("/api/v1/assistant/messages")) {
        captured.push(request.postData() ?? "");
      }
    });

    await page.setViewportSize({ width: 1440, height: 900 });
    await page.goto(voicesUrl(first.id));
    await expect(page.getByTestId("voice-characters")).toBeVisible({ timeout: 20_000 });
    // The pill's chip reads the screen and the resolved selection (UX-01).
    await expect(page.getByTestId("agent-pill-context")).toHaveText(`cast › ${first.name}`, {
      timeout: 20_000,
    });

    // ⌘J opens the docked panel; its header chip reads the same seam.
    await page.keyboard.press("Meta+j");
    const dock = page.getByTestId("agent-dock");
    await expect(dock).toBeVisible();
    await expect(page.getByTestId("agent-dock-context")).toHaveText(`cast › ${first.name}`);

    // One message: the body the page sends is the assertion (asserted
    // redacted — the capture holds only the placeholder text and the ids).
    await page.getByTestId("agent-dock-input").fill(PLACEHOLDER_MESSAGE);
    await page.getByTestId("agent-dock-send").click();
    await expect(page.getByTestId(/dock-turn-user-/)).toContainText(PLACEHOLDER_MESSAGE);
    // The body the page sent is the assertion (asserted redacted — the
    // capture holds only the placeholder text and the ids).
    await expect
      .poll(() => captured.length, {
        timeout: 15_000,
        message: "the message has not left the page yet",
      })
      .toBeGreaterThanOrEqual(1);
    const body = JSON.parse(captured[0]) as {
      text: string;
      context: {
        route?: string;
        game_id?: string;
        profile_id?: string;
        selection?: { type: string; id: string };
      };
    };
    // The full context payload (UX-01): the route (path + the selection's
    // query), the game, the profile and the selection.
    expect(body.text).toBe(PLACEHOLDER_MESSAGE);
    expect(body.context?.route).toBe(
      `/games/${gameId}/voices?profile=${profileId}&character=${first.id}`,
    );
    expect(body.context?.game_id).toBe(gameId);
    expect(body.context?.profile_id).toBe(profileId);
    expect(body.context?.selection).toEqual({ type: "character", id: first.id });

    // The reply: the genuine provider call the scripted gateway answers,
    // streamed back to the panel (the dock's chat bubble, not a fixture).
    await expect(page.getByTestId(/dock-turn-agent-/)).toContainText(SCRIPTED_REPLY, {
      timeout: 30_000,
    });

    // ── 5. the chip follows the selection when it changes ───────────────────
    await page.goto(voicesUrl(second.id));
    await expect(page.getByTestId("voice-characters")).toBeVisible({ timeout: 20_000 });
    await expect(page.getByTestId("agent-pill-context")).toHaveText(`cast › ${second.name}`, {
      timeout: 20_000,
    });

    // ── 6. a bare screen: the payload carries no selection ─────────────────
    await page.goto("/library");
    await PRIMARY_ROUTES[0].settle(page);
    await expect(page.getByTestId("agent-pill-context")).toHaveText("library");
    await page.keyboard.press("Meta+j");
    await expect(page.getByTestId("agent-dock-context")).toHaveText("library");
    await page.getByTestId("agent-dock-input").fill(PLACEHOLDER_MESSAGE);
    await page.getByTestId("agent-dock-send").click();
    await expect
      .poll(() => captured.length, {
        timeout: 15_000,
        message: "the bare-screen message has not left the page yet",
      })
      .toBeGreaterThanOrEqual(2);
    const bareBody = JSON.parse(captured[1]) as {
      context?: { route?: string; game_id?: string; selection?: unknown };
    };
    // The bare screen's context is the route and nothing more: no game, no
    // selection (the server refuses a selection it cannot verify anyway).
    expect(bareBody.context).toEqual({ route: "/library" });

    // ── 7. ⌘J reaches the dock from each of the six primary routes ─────────
    for (const route of PRIMARY_ROUTES) {
      await page.goto(route.path);
      await route.settle(page);
      await page.keyboard.press("Meta+j");
      await expect(
        page.getByTestId("agent-dock"),
        `⌘J does not open the dock on ${route.path}`,
      ).toBeVisible();
      await expect(page.getByTestId("agent-dock-context")).toHaveText(route.chip);
      await page.getByTestId("agent-dock-close").click();
      await expect(page.getByTestId("agent-dock")).toHaveCount(0);
    }

    // ── 8. evidence: the docked panel at the design's three widths ──────────
    await page.goto(voicesUrl(first.id));
    await expect(page.getByTestId("voice-characters")).toBeVisible({ timeout: 20_000 });
    await page.keyboard.press("Meta+j");
    await expect(page.getByTestId("agent-dock")).toBeVisible();

    interface ScanReport {
      url: string;
      width: number;
      serious: string[];
      violations: Record<string, number>;
    }
    const reports: ScanReport[] = [];
    for (const width of WIDTHS) {
      await page.setViewportSize({ width, height: 900 });
      await fontsSettled(page);
      // Phone width: no horizontal document scroll — the dock is a 360px
      // panel and the page must never be wider than the viewport (the
      // redesign walk's Phase 0 review fix 3, held here for the dock).
      if (width === 390) {
        const widths = await page.evaluate(() => {
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
          widths.scrollWidth,
          `the docked dock @390px scrolls horizontally: documentElement.scrollWidth ${widths.scrollWidth} > innerWidth ${widths.innerWidth}`,
        ).toBeLessThanOrEqual(widths.innerWidth);
      }
      const shot = `agent-dock-${width}.png`;
      const shotPath = `${testInfo.outputDir}/${shot}`;
      await page.screenshot({ path: shotPath, fullPage: true });
      await testInfo.attach(shot, { body: readFileSync(shotPath), contentType: "image/png" });

      const results = await new AxeBuilder({ page }).withTags([...WCAG_21_A_AA]).analyze();
      const violations = results.violations ?? [];
      reports.push({
        url: page.url(),
        width,
        violations: Object.fromEntries(
          Object.entries(
            violations.reduce<Record<string, number>>((byRule, violation) => {
              byRule[violation.id] = (byRule[violation.id] ?? 0) + violation.nodes.length;
              return byRule;
            }, {}),
          ).sort(([a], [b]) => a.localeCompare(b)),
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
      `serious/critical axe violations on the agent dock:\n${failing
        .map((report) => `— ${report.url} @${report.width}px:\n${report.serious.join("\n")}`)
        .join("\n\n")}`,
    ).toEqual([]);

    // The redacted capture: the two outgoing bodies, exactly as the page sent
    // them (placeholder text and ids only — no game dialogue, no secrets).
    await testInfo.attach("assistant-messages-redacted.json", {
      body: JSON.stringify(
        captured.map((raw) => JSON.parse(raw)),
        null,
        2,
      ),
      contentType: "application/json",
    });

    expect(consoleErrors, `browser console: ${consoleErrors.join("\n")}`).toEqual([]);
    expectDefaultDataDirUntouched(run);
  } finally {
    releaseScenario();
  }
});
