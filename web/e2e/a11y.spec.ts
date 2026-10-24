import AxeBuilder from "@axe-core/playwright";
import { expect, test, type Page } from "@playwright/test";
import { readFileSync } from "node:fs";
import { randomUUID } from "node:crypto";
import { resolve } from "node:path";

import { trackConsoleErrors } from "./support/console";
import { buildManifest, type SeedProfile } from "./support/manifest";
import { setPlayView } from "./support/play";
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
 * Plus the live-session stop (issue #34, gap row G16): the states a user
 * reaches DURING play — streaming committed lines, the in-play audio
 * controls, the now-speaking caption — are the a11y surface the idle walk
 * above cannot see. Two replay sessions of the committed synthetic clip
 * cover it: one carries the in-play keyboard contract at the head of the
 * session, where the clip's whole lifetime is ahead of the walk whatever the
 * host's OCR throughput (issue #23's placement — the replay drains at the
 * host's own throughput and a pause does not stop the source), and the other
 * streams its committed lines through the real pipeline and is read settled
 * (the play-pc walks' wait-out-the-run pattern) for the now-speaking and
 * operator scans.
 *
 * Model boundary: AGENTS.md rule 3 class (b) — the live sessions are a
 * captured-frame replay of the repo's synthetic clip: the real Tesseract OCR
 * and the local matcher/commit chain, no model providers (the appliance
 * carries no speech roles, so the utterances end skipped and the speech
 * counters say so; the walk is never presented as a real voice).
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

/**
 * Theme sweeps for the walk (task: the axe walks ran mostly on the default
 * theme, which is how the cast badge's 2.68:1 coral-on-cream slipped
 * through): one dark theme (Lilac Night) and one other light theme
 * (Sherbet — the lightest paper/card pair, the strongest regression
 * signal). The theme is applied by setting `data-theme` on <html> (the
 * same seam src/lib/theme.ts uses) directly before analyze and removed
 * right after, so each route is scanned on all three themes without
 * re-visiting it.
 */
const THEME_SWEEPS = ["lilac-night", "sherbet"] as const;

async function setTheme(page: Page, theme: string | null): Promise<void> {
  await page.evaluate((t) => {
    // The e2e tsconfig has no DOM lib: the element is reached through a
    // typed globalThis cast (the suite's pattern, settings-theme.spec.ts).
    const html = (
      globalThis as {
        document?: {
          documentElement?: {
            setAttribute(name: string, value: string): void;
            removeAttribute(name: string): void;
          };
        };
      }
    ).document?.documentElement;
    if (html === undefined) return;
    if (t === null) html.removeAttribute("data-theme");
    else html.setAttribute("data-theme", t);
  }, theme);
}

async function scanTheme(
  page: Page,
  name: string,
  report: Record<string, RouteReport>,
  theme: string | null,
): Promise<void> {
  if (theme !== null) await setTheme(page, theme);
  const results = await new AxeBuilder({ page }).withTags([...WCAG_21_A_AA]).analyze();
  if (theme !== null) await setTheme(page, null);
  const violations = results.violations ?? [];
  const byRule: Record<string, number> = {};
  for (const violation of violations) {
    byRule[violation.id] = (byRule[violation.id] ?? 0) + violation.nodes.length;
  }
  report[name] = {
    url: page.url(),
    violations: Object.fromEntries(Object.entries(byRule).sort(([a], [b]) => a.localeCompare(b))),
    // Every failing node is named (target + axe's failureSummary) so the
    // failure message points at the element, not just the rule.
    serious: violations
      .filter((violation) => violation.impact === "serious" || violation.impact === "critical")
      .map(
        (violation) =>
          `${violation.id} [${violation.impact}] ×${violation.nodes.length} — ${violation.help} — at ${violation.nodes
            .map(
              (node) =>
                `${node.target.join(" ")}: ${(node.failureSummary ?? "").replace(/\s+/g, " ").trim()}`,
            )
            .join(" | ")}`,
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

/** Scan the current page and record it; the failure check happens once, over all routes. */
async function scan(page: Page, name: string, report: Record<string, RouteReport>): Promise<void> {
  await scanTheme(page, name, report, null);
  for (const theme of THEME_SWEEPS) {
    await scanTheme(page, `${name} [theme: ${theme}]`, report, theme);
  }
}

test("axe: every route of the ui-coverage matrix, WCAG 2.1 A/AA", async ({
  page,
  request,
  baseURL,
}, testInfo) => {
  test.setTimeout(900_000);
  test.skip(testInfo.project.name !== "a11y", "runs on the a11y project's fresh appliance");
  const run = serverRun("a11y");
  // 503 joins 401 on the ignore list: on this appliance the capture layer is
  // unwired (no UVC dongle, no test seam), and the setup wizard's devices step
  // renders that 503 as its own "no capture layer" note — the machine's
  // answer, not a fault of the walk. 409 joins them for the same layer on
  // the health page's signal read: ADR-0110 §1's 409 carries the layer's own
  // reason and the tile renders it as its own state. 429 joins for the
  // live-session stop: the operator view's frame poller on the replay server
  // gets the one-shot preview's designed throttle answer — the frame card
  // renders it as its own state (the play/m5-part1 walks' documented noise).
  const consoleErrors = trackConsoleErrors(page, [401, 409, 429, 503]);
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
    // The ADR-0141 rename: the a11y walk visits the renamed route's
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

  // ── 4a. the graph's table view (issue #34): the non-canvas alternative ──
  // The canvas's own page as a real table (the spec 09 keyboard-accessibility
  // rule's text equivalent), reached through the toggle's own keyboard
  // contract — focus the "table" tab, Enter — and scanned on its own. The
  // canvas default is restored afterwards (the choice persists per user,
  // UX-08, and the Lilac matrix below re-walks the route in its default
  // state); the kit table's tokens are the ones the dialogue route already
  // carries into both themes.
  await page.goto(`/games/${gameId}/graph`);
  await settled(page, "graph-node-list");
  const graphTableTab = page.getByRole("tab", { name: "table" });
  await graphTableTab.focus();
  await page.keyboard.press("Enter");
  await settled(page, "graph-table");
  await scan(page, "graph (table view)", report);
  const graphCanvasTab = page.getByRole("tab", { name: "canvas" });
  await graphCanvasTab.focus();
  await page.keyboard.press("Enter");
  await settled(page, "graph-node-list");

  // ── 4b. the theme page and the same signed-in matrix under Lilac Night
  //     (issue #30) ─────────────────────────────────────────────────────
  // The dark theme inverts which token is the background, so a component
  // style that names --ink for text is correct on every light theme and
  // unreadable here. The picker's own walk switches the theme (the UX-09
  // seam: the choice applies to <html> without a reload and persists), the
  // signed-in matrix — plus the theme page itself, which the default-theme
  // matrix above does not carry — is repeated under the dark token set, and
  // the theme is restored so the anonymous states below keep their
  // default-theme coverage.
  await page.goto("/settings/theme");
  await settled(page, "theme-grid");
  await scan(page, "theme (picker)", report);
  await page.getByTestId("theme-pick-lilac-night").click();
  await expect(page.getByTestId("theme-selected-lilac-night")).toBeVisible();
  await expect(page.getByTestId("theme-pick-lilac-night")).toBeDisabled();
  await page.reload(); // the persisted choice survives the reload (UX-09)
  await expect(page.getByTestId("theme-selected-lilac-night")).toBeVisible();
  await scan(page, "theme (picker, lilac night)", report);
  for (const stop of stops) {
    await visit(`${stop.name} (lilac night)`, stop.url, stop.settle);
  }
  await page.goto("/settings/theme");
  await settled(page, "theme-pick-plum");
  await page.getByTestId("theme-pick-plum").click();
  await expect(page.getByTestId("theme-selected-plum")).toBeVisible();

  // ── 4c. the live-session stop (issue #34, gap row G16): /play during play ─
  // The idle walk above settles on the start form; the states a user reaches
  // while a session runs — the streaming committed lines, the in-play audio
  // controls, the now-speaking caption — are covered here by two replay
  // sessions of the committed synthetic clip (AGENTS.md rule 3 class (b):
  // real Tesseract OCR and the local matcher/commit chain, no model — this
  // appliance carries no speech roles, so the utterances end skipped and the
  // speech counters say so). The replay drains at the host's own OCR
  // throughput (lossless and back-pressured, never real-time paced — the
  // play-controls walk's issue #23 note), and a pause does not stop the
  // source — the text machines keep observing through it — so the keyboard
  // contract runs at the head of the first session, where the clip's whole
  // lifetime is ahead of the walk whatever the host's speed (issue #23's
  // placement); the axe scans read the second session's settled run, the way
  // the play-pc walks wait the run out instead of racing source_end.
  const liveOrigin = new URL((baseURL as string) ?? "").origin;
  // The walk already claimed the appliance through the page; the API context
  // (its own cookie jar) only signs in and seeds the sessions' profile the
  // way the play-pc / m5-part1 walks do.
  const apiLogin = await request.post(`${liveOrigin}/api/v1/auth/login`, {
    data: { username: ADMIN.username, password: ADMIN.password },
  });
  expect(apiLogin.status(), `api login: ${await apiLogin.text()}`).toBe(200);
  const csrf = async () =>
    (await request.cookies(liveOrigin)).find((cookie) => cookie.name === "gn_csrf")?.value ?? "";
  const livePost = (path: string, data?: unknown, headers: Record<string, string> = {}) =>
    request.post(`${liveOrigin}${path}`, {
      data,
      headers: {
        ...(data === undefined ? {} : { "content-type": "application/json" }),
        ...headers,
      },
    });
  const LIVE = { slug: "a11y-live-harbour", title: "A11y Live Harbour" };
  const liveGame = await livePost(
    "/api/v1/games",
    { slug: LIVE.slug, title: LIVE.title },
    { "idempotency-key": randomUUID(), "x-csrf-token": await csrf() },
  );
  expect(liveGame.status(), `create live game: ${await liveGame.text()}`).toBe(201);
  const liveGameId = (await liveGame.json()) as { id: string };
  const liveEdition = await livePost(
    `/api/v1/games/${liveGameId.id}/editions`,
    { region: EDITION.region, language: EDITION.language, revision: EDITION.revision },
    { "idempotency-key": randomUUID(), "x-csrf-token": await csrf() },
  );
  expect(liveEdition.status(), `create live edition: ${await liveEdition.text()}`).toBe(201);
  const liveEditionId = ((await liveEdition.json()) as { id: string }).id;
  // The clip's own engine-tuned profile (the seed the replay commits its two
  // pages from), with the roster the committed lines name (Marina speaks the
  // clip's two real lines; the distractors ride the Keeper, unattributed).
  const liveSeed = JSON.parse(
    readFileSync(
      resolve(import.meta.dirname, "../../testdata/synthetic/profiles/dialogue-advance-001.json"),
      "utf8",
    ),
  ) as SeedProfile;
  for (const message of liveSeed.messages) {
    const isRealLine = message.id === "demo-page-1" || message.id === "demo-page-2";
    message.speaker = isRealLine ? "marina" : "keeper";
    message.attribution = isRealLine
      ? { status: "confirmed", confidence: 0.95 }
      : { status: "unknown", confidence: 0 };
  }
  liveSeed.characters = [
    { id: "marina", name: "Marina", role: "protagonist", status: "confirmed" },
    { id: "keeper", name: "Keeper", role: "supporting", status: "proposed" },
  ];
  const { doc: liveManifest } = buildManifest(liveSeed, {
    edition: {
      ...LIVE,
      region: EDITION.region,
      language: EDITION.language,
      revision: EDITION.revision,
    },
  });
  const liveImported = await livePost(
    `/api/v1/editions/${liveEditionId}/profiles/import-manifest`,
    liveManifest,
    { "x-csrf-token": await csrf() },
  );
  expect(liveImported.status(), `import live profile: ${await liveImported.text()}`).toBe(201);
  const { profile: liveProfile } = (await liveImported.json()) as { profile: { id: string } };
  const liveEtag = async (id: string) => {
    const res = await request.get(`${liveOrigin}/api/v1/profiles/${id}`);
    expect(res.status(), "the live profile read carries an ETag").toBe(200);
    return res.headers()["etag"] as string;
  };
  await livePost(
    `/api/v1/profiles/${liveProfile.id}/status`,
    { to: "review" },
    { "if-match": await liveEtag(liveProfile.id), "x-csrf-token": await csrf() },
  );
  const livePublished = await livePost(`/api/v1/profiles/${liveProfile.id}/publish`, undefined, {
    "if-match": await liveEtag(liveProfile.id),
    "x-csrf-token": await csrf(),
  });
  expect(livePublished.status(), `publish live profile: ${await livePublished.text()}`).toBe(200);

  // Both sessions start through the page (the walk is signed in): the form's
  // default replay directory is the synthetic clip's own.
  const startLiveSession = async () => {
    await page.goto("/play");
    await expect(page.getByTestId("play-profile")).toBeVisible({ timeout: 30_000 });
    await page.getByTestId("play-profile").selectOption(liveProfile.id, { timeout: 30_000 });
    await page.getByTestId("play-start").click();
    await expect(page).toHaveURL(/\/play\?session=/, { timeout: 30_000 });
    await expect(page.getByTestId("play-state")).toHaveText(/^(starting|running)$/, {
      timeout: 60_000,
    });
  };

  // ── session one: the in-play keyboard contract, at the head ──────────────
  // The bar's own Pause is the head freeze — the keyboard activation the
  // contract names (Space on the live control), and the placement issue #23
  // proved: at the head the clip's whole lifetime is ahead of the pause
  // window, whatever the host's OCR throughput. Live-but-paused, every
  // in-play control the bar renders for a running session stays enabled, and
  // the commit gate holds — no committed line (and no correction button it
  // would enable in the bar) renders under the walk, so the Tab order
  // recorded below is stable ground truth for the re-walk.
  await startLiveSession();
  await page.getByTestId("play-pause").focus();
  await page.keyboard.press("Space");
  await expect(page.getByTestId("play-pause")).toHaveAttribute("aria-pressed", "true", {
    timeout: 15_000,
  });
  await expect(page.getByTestId("play-pause")).toHaveText("Resume", { timeout: 15_000 });

  const activeTestId = () =>
    page.evaluate(() => {
      const g = globalThis as unknown as {
        document?: { activeElement?: { getAttribute?: (name: string) => string | null } };
      };
      return g.document?.activeElement?.getAttribute?.("data-testid") ?? "";
    });

  // Tab order from the session header's "show workings" toggle (a known
  // focusable in the page's top action group): the watch's close link, then
  // the bar's Pause, Mute, (volume · delay · careful/eager), Stop and the
  // read-through toggles — each reached by Tab.
  await page.getByTestId("play-show-workings").focus();
  const tabOrder: string[] = [];
  const tabTargets = [
    "play-pause",
    "play-mute",
    "play-stop",
    "play-class-signs",
    "play-class-items",
  ];
  for (let i = 0; i < 30; i += 1) {
    await page.keyboard.press("Tab");
    tabOrder.push(await activeTestId());
    if (tabTargets.every((target) => tabOrder.includes(target))) break;
  }
  const orderOf = (testid: string) => tabOrder.indexOf(testid);
  expect(
    orderOf("play-pause"),
    `Tab reached Pause (order: ${tabOrder.join(" → ")})`,
  ).toBeGreaterThanOrEqual(0);
  expect(orderOf("play-mute"), `Tab reached Mute (order: ${tabOrder.join(" → ")})`).toBeGreaterThan(
    orderOf("play-pause"),
  );
  expect(orderOf("play-stop"), `Tab reached Stop (order: ${tabOrder.join(" → ")})`).toBeGreaterThan(
    orderOf("play-mute"),
  );
  expect(
    orderOf("play-class-signs"),
    `Tab reached the read-through toggles (order: ${tabOrder.join(" → ")})`,
  ).toBeGreaterThan(orderOf("play-stop"));
  expect(
    orderOf("play-class-items"),
    `Tab reached the second read-through toggle (order: ${tabOrder.join(" → ")})`,
  ).toBeGreaterThan(orderOf("play-class-signs"));

  // The activations, re-driven from the anchor along the recorded path: the
  // walk Tabs step for step and acts on each target as focus lands on it —
  // Space on Mute and on the read-through toggle, then Enter on Stop ends the
  // session. The session sits paused (its own head freeze above), so no
  // commit lands under the walk and the recorded path is the path.
  await page.getByTestId("play-show-workings").focus();
  for (let i = 0; i < tabOrder.length; i += 1) {
    await page.keyboard.press("Tab");
    const focused = await activeTestId();
    expect(focused, `the re-walk follows the recorded path (step ${i + 1})`).toBe(tabOrder[i]);
    if (focused === "play-mute") {
      await page.keyboard.press("Space");
      await expect(page.getByTestId("play-mute")).toHaveAttribute("aria-checked", "true", {
        timeout: 15_000,
      });
    } else if (focused === "play-class-signs") {
      await page.keyboard.press("Space");
      await expect(page.getByTestId("play-class-signs")).toBeChecked({ timeout: 15_000 });
      await expect(page.getByTestId("play-narrated")).toContainText("sign", { timeout: 15_000 });
      // The Applying… window disables the toggle (focus would drop to body);
      // re-anchor on the header's own toggle and walk forward to the bar's
      // Stop along the recorded path — (stopIndex + 1) Tabs from the anchor
      // land on tabOrder[stopIndex], the Stop itself. Enter ends the session.
      await page.getByTestId("play-show-workings").focus();
      const stopIndex = tabOrder.indexOf("play-stop");
      for (let step = 0; step <= stopIndex; step += 1) {
        await page.keyboard.press("Tab");
        expect(await activeTestId(), `back to Stop (step ${step + 1})`).toBe(tabOrder[step]);
      }
      await page.keyboard.press("Enter");
      break;
    }
  }
  await expect(page.getByTestId("play-state")).toHaveText("stopped", { timeout: 180_000 });

  // ── session two: the live-session surface, read settled ──────────────────
  // The same clip streams its committed lines through the real pipeline (the
  // walk above ended its own session before the first commit — this session
  // is the one that lets the run play out: the play-pc walks' pattern of
  // waiting the run out and reading the settled truth, never racing
  // source_end). The now-speaking and operator views the scans read are the
  // in-play surface itself — the same regions a live session renders — in
  // their settled state, where the walk is deterministic.
  await startLiveSession();
  const committedLines = page.locator('[data-testid="recent-lines"] li');
  // The first committed line — the real OCR over the synthetic clip's frames,
  // the commit chain's local verdict.
  await expect
    .poll(() => committedLines.count(), { timeout: 180_000, intervals: [250] })
    .toBeGreaterThanOrEqual(1);
  // Then the run plays out to its own end (the clip's source_end).
  await expect(page.getByTestId("play-state")).toHaveText("stopped", { timeout: 180_000 });

  // Now-speaking: the committed line settled on screen (the caption — the
  // text equivalent of the audio, docs/design/plum-peach "Accessibility"),
  // and the aria-live contract on the caption region and the committed-lines
  // list, pinned here over a run that actually streamed them.
  await expect(page.getByTestId("play-now-speaking")).toBeVisible();
  await expect(page.getByTestId("current-line-text")).not.toBeEmpty();
  await expect(page.getByTestId("current-line-speaker")).toHaveText("Marina");
  await expect(committedLines).not.toHaveCount(0);
  await expect(page.getByRole("region", { name: "Current committed line" })).toHaveAttribute(
    "aria-live",
    "polite",
  );
  await expect(page.getByTestId("recent-lines")).toHaveAttribute("aria-live", "polite");
  await scan(page, "play (live session, now-speaking)", report);

  // Operator: the same settled run's committed line beneath the frame —
  // the operator's twin of the caption, aria-live the same way — and the
  // run's own event stream behind it.
  await setPlayView(page, "operator");
  await expect(page.getByTestId("play-operator-line")).toBeVisible();
  await expect(page.getByTestId("play-operator-line")).not.toBeEmpty();
  await expect(page.getByTestId("play-operator-line")).toHaveAttribute("aria-live", "polite");
  await expect(page.getByTestId("play-events")).toBeVisible({ timeout: 30_000 });
  await scan(page, "play (live session, operator)", report);

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
