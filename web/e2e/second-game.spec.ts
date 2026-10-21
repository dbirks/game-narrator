import { expect, test } from "@playwright/test";
import { randomUUID } from "node:crypto";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

import { trackConsoleErrors } from "./support/console";
import { buildManifest, type SeedProfile } from "./support/manifest";
import { expectDefaultDataDirUntouched, serverRun } from "./support/servers";

/**
 * G12 second-game portability — the dev-UI leg (issue #33, ADR-0079): the
 * structurally different second game ("Black Water", the committed
 * bench-synthetic-002 set) through the real pages on the packaged binary.
 *
 * The leg the gap audit found untested: no spec ever imported the second
 * game's profile, opened it in the Library or the developer inspector, or
 * asserted its upper-left region renders where the committed data says.
 * Everything here is committed synthetic fixture (AGENTS.md rules 3 and 5):
 *
 *   - the profile is the committed bench set's profile.json
 *     (testdata/bench/bench-synthetic-002) — its six invented lines and its
 *     single upper-left textbox region, imported through the API's own
 *     manifest path (POST /editions/{id}/profiles/import-manifest, the only
 *     API that creates the region + messages in one transaction; the digest
 *     is re-verified server-side, so e2e/support/manifest.ts signs the
 *     document the way internal/profileio/canonical.go does);
 *   - the replay frames are the committed black-water-001 clip
 *     (testdata/synthetic/replay/black-water-001, ADR-0078: upper-left
 *     square-cornered box, instant page reveals, one non-Latin page the eng
 *     OCR model cannot read).
 *
 * The assertions, per the gap audit's G12 unit of work ("same assertions as
 * the first game's holdout"), run in the order the pages admit — the session
 * must exist before the Regions page, which draws its boxes relative to the
 * game's last observed frame:
 *
 *   1. Library — the imported game renders on the shelf with its published
 *      state and start action (the second game is a first-class library
 *      citizen, not a test fixture);
 *   2. Developer inspector — a replay session over the committed clip with
 *      the imported profile commits the clip's two Latin lines (bw-02 and
 *      bw-06 — the fuzzy floor's real decision for page A, the exact match
 *      for page B) in capture order, and never commits a known line for the
 *      non-Latin page (the eng model cannot read it; the narrator stays
 *      silent rather than fabricating, ADR-0079), and the committed line
 *      shows through the dev UI's timeline and detail panel, with the
 *      region's ROI box drawn on the observed frame;
 *   3. Regions — the imported region row stores the committed rect and the
 *      page renders it at the committed upper-left position (the first
 *      game's box sits at the bottom, y≈0.756: a page that drew the first
 *      layout could not pass this);
 *
 * Only the stable testids on main — the spec is independent of the
 * redesign branches' in-flight surfaces.
 */

// The committed bench profile, in the bench set's ScriptProfile shape
// (internal/bench/profile.go): the messages the matcher ranks against, the
// region the pipeline watches, and the textbox layout the set renders with.
interface BenchProfile {
  schema_version: number;
  id: string;
  origin: string;
  description: string;
  scene: string;
  regions: { id: string; type: string; x: number; y: number; w: number; h: number }[];
  messages: { id: string; text: string; speaker: string }[];
  edges: { from: string; to: string; type: string; confidence: number }[] | null;
  layout?: { box_x: number; box_y: number; box_w: number; box_h: number; rounded?: boolean };
}

const BENCH_PROFILE_PATH = resolve(
  import.meta.dirname,
  "..",
  "..",
  "testdata",
  "bench",
  "bench-synthetic-002",
  "profile.json",
);
const CLIP_DIR = "testdata/synthetic/replay/black-water-001";
const EDITION = {
  slug: "black-water",
  title: "Black Water",
  region: "us",
  language: "en",
  revision: "rev0",
};
const ADMIN = { username: "second-game-admin", password: "black water is a synthetic game" };

/** One durable event envelope as the dev inspector renders it. */
interface DevEvent {
  event_id: string;
  seq: number;
  name: string;
  trace_id?: string;
  causation_id?: string;
  occurred_at: string;
  data: Record<string, unknown>;
}

test("the second game (bench-synthetic-002) through the library, regions and dev inspector", async ({
  page,
  request,
  baseURL,
}, testInfo) => {
  test.setTimeout(300_000);
  // This spec drives the `second-game` webServer's backend — its own data dir
  // and the explicitly-named replay allowlist (playwright.config.ts). On the
  // chromium project it would share the 18090 backend with the catalogue spec
  // and the two auth/setups would collide, so it runs only on its own project.
  test.skip(
    testInfo.project.name !== "second-game",
    "runs on the second-game project's own embedded-binary backend",
  );
  const origin = new URL(baseURL as string).origin;
  const run = serverRun("second-game");

  const consoleErrors = trackConsoleErrors(page); // 401 (the unauth /auth/me probe) is the only expected one
  const shot = (step: string) =>
    page.screenshot({ path: `test-results/second-game-${step}.png`, fullPage: true });

  // ── 0. the committed fixture: read the bench set's profile and check the
  // ── second-game shape before anything imports it (a wrong file would fail
  // ── here, not mid-walk).
  const bench = JSON.parse(readFileSync(BENCH_PROFILE_PATH, "utf8")) as BenchProfile;
  expect(bench.id).toBe("bench-synthetic-002");
  expect(bench.origin).toBe("generated_synthetic");
  expect(bench.messages).toHaveLength(6);
  expect(bench.regions).toHaveLength(1);
  const region = bench.regions[0];
  expect(region.id).toBe("upper-left");
  // The structural difference the set exists to carry (ADR-0079): the box is
  // UPPER-LEFT (the first game's bottom box sits at y≈0.756).
  expect(
    region.y,
    "the second game's region is the upper-left box, not the first game's bottom",
  ).toBeLessThan(0.5);
  expect(region.y + region.h).toBeLessThan(0.5);
  if (bench.layout) {
    expect(bench.layout.box_y, "the set's layout is the upper-left box").toBeLessThan(0.5);
  }

  // ── 1. prepare the appliance through the API: admin, game, edition ────────
  const setup = await request.post(`${origin}/api/v1/auth/setup`, {
    data: { username: ADMIN.username, password: ADMIN.password },
  });
  expect(setup.status(), `auth setup: ${await setup.text()}`).toBe(201);
  const login = await request.post(`${origin}/api/v1/auth/login`, {
    data: { username: ADMIN.username, password: ADMIN.password },
  });
  expect(login.status(), `admin login: ${await login.text()}`).toBe(200);

  const csrf = async () =>
    (await request.cookies(origin)).find((cookie) => cookie.name === "gn_csrf")?.value ?? "";
  const post = (path: string, data?: unknown, headers: Record<string, string> = {}) =>
    csrf().then((token) =>
      request.post(`${origin}${path}`, {
        data,
        headers: {
          ...(data === undefined ? {} : { "content-type": "application/json" }),
          "x-csrf-token": token,
          ...headers,
        },
      }),
    );

  const game = await post(
    "/api/v1/games",
    { slug: EDITION.slug, title: EDITION.title, platform: "GameCube" },
    { "idempotency-key": randomUUID() },
  );
  expect(game.status(), `create game: ${await game.text()}`).toBe(201);
  const gameId = (await game.json()) as { id: string };

  const edition = await post(
    `/api/v1/games/${gameId.id}/editions`,
    { region: EDITION.region, language: EDITION.language, revision: EDITION.revision },
    { "idempotency-key": randomUUID() },
  );
  expect(edition.status(), `create edition: ${await edition.text()}`).toBe(201);
  const editionId = (await edition.json()) as { id: string };

  // ── 2. import the committed bench profile through the API and publish it ──
  // The seed shape is the games tables' columns; the bench profile's messages
  // travel as (id, original_text) and its region is the seed's region as-is
  // (both shapes are the same normalized rect). The bench set carries no
  // edges (its script is a flat holdout corpus), so the manifest's edge
  // section is empty — the import creates the region and the six messages in
  // one transaction and re-verifies the digest.
  const seed: SeedProfile = {
    id: bench.id,
    origin: bench.origin,
    description: bench.description,
    text_kind: "dialogue",
    messages: bench.messages.map((message) => ({ id: message.id, original_text: message.text })),
    edges: (bench.edges ?? []).map((edge) => ({
      from: edge.from,
      to: edge.to,
      type: edge.type,
      confidence: edge.confidence,
    })),
    regions: bench.regions,
  };
  const { doc: manifest } = buildManifest(seed, { edition: EDITION });
  const imported = await post(
    `/api/v1/editions/${editionId.id}/profiles/import-manifest`,
    manifest,
  );
  expect(imported.status(), `import-manifest: ${await imported.text()}`).toBe(201);
  const { profile } = (await imported.json()) as { profile: { id: string } };
  expect(profile.id).toBeTruthy();

  // The lifecycle moves the row version on every write, so the ETag is
  // re-read between the status move and the publish (ADR-0015).
  let etagResponse = await request.get(`${origin}/api/v1/profiles/${profile.id}`);
  expect(etagResponse.status()).toBe(200);
  let etag = etagResponse.headers()["etag"];
  expect(etag, "a profile read carries an ETag").toBeTruthy();
  const toReview = await post(
    `/api/v1/profiles/${profile.id}/status`,
    { to: "review" },
    {
      "if-match": etag,
    },
  );
  expect(toReview.status(), `to review: ${await toReview.text()}`).toBe(200);
  etagResponse = await request.get(`${origin}/api/v1/profiles/${profile.id}`);
  etag = etagResponse.headers()["etag"] as string;
  const published = await post(`/api/v1/profiles/${profile.id}/publish`, undefined, {
    "if-match": etag,
  });
  expect(published.status(), `publish: ${await published.text()}`).toBe(200);

  // The import minted the store's row ULIDs; map the friendly ids to them so
  // the event assertions can name the commits the way the pipeline does.
  const messagesPage = (await (
    await request.get(`${origin}/api/v1/profiles/${profile.id}/messages?limit=200`)
  ).json()) as { messages: { id: string; source_message_id?: string }[] };
  const storeIds: Record<string, string> = {};
  for (const row of messagesPage.messages ?? []) {
    if (row.source_message_id) storeIds[row.source_message_id] = row.id;
  }
  for (const friendly of bench.messages.map((message) => message.id)) {
    expect(storeIds[friendly], `the imported profile stores message ${friendly}`).toBeTruthy();
  }

  // ── 3. the Library: the second game renders as a first-class citizen ──────
  await page.goto("/library");
  await expect(page).toHaveURL(/\/login/);
  await page.getByTestId("login-username").fill(ADMIN.username);
  await page.getByTestId("login-password").fill(ADMIN.password);
  await page.getByTestId("login-submit").click();
  await expect(page).toHaveURL(/\/library$/);

  // A published game takes the hero slot, and the shelf carries the tile.
  await expect(page.getByTestId("lib-hero-title")).toHaveText(EDITION.title, { timeout: 15_000 });
  await expect(page.getByTestId("lib-shelf")).toBeVisible();
  await expect(page.getByTestId(`lib-tile-${EDITION.slug}`)).toBeVisible();
  await expect(page.getByTestId(`game-${EDITION.slug}`)).toHaveText(EDITION.title);
  await expect(page.getByTestId(`lib-chip-${EDITION.slug}`)).toHaveText("published");
  // A published game's start action is on the tile.
  await expect(page.getByTestId(`lib-play-${EDITION.slug}`)).toBeVisible();
  await shot("01-library");

  // ── 4. the developer inspector: a replay session over the committed clip ──
  // (runs before the Regions page: that page draws the region boxes relative
  // to the game's last observed frame, so the session must exist first). The
  // inspector view on the renamed route (ADR-0109): /developer redirects here,
  // preserving the query.
  await page.goto("/traces?view=inspector");
  await expect(page).toHaveURL(/\/traces\?view=inspector$/);
  await expect(page.getByTestId("nav-role")).toHaveText("admin");

  // The selector lists exactly the published revisions; the imported
  // revision is the only one, and the page must have picked it up.
  await expect(page.getByTestId("session-profile")).toHaveValue(profile.id, { timeout: 15_000 });
  await page.getByTestId("session-replay-dir").fill(CLIP_DIR);
  await expect(page.getByTestId("session-dev-trace")).toBeChecked();

  // The session runs the real pipeline on Tesseract and ends on its own when
  // the 24-frame clip is exhausted (docs/spec/12: no machine-speed
  // dependence). A timed-out or errored read is retried (ModeReplay), so a
  // run that ends without the clip's commits is a real failure and the
  // assertion below says so.
  await page.getByTestId("session-start").click();
  await shot("03-session-started");
  await expect(page.getByTestId("session-state")).toHaveText("stopped", { timeout: 180_000 });
  const sessionId = (await page.getByTestId("session-id").textContent()) as string;
  const session = (await (await request.get(`${origin}/api/v1/sessions/${sessionId}`)).json()) as {
    state: string;
    reason?: string;
    stats: { commits: number; ocr_calls: number; rankings_abstained: number };
  };
  await shot("04-session-stopped");
  expect(sessionId).toBeTruthy();
  expect(session.state).toBe("stopped");
  expect(session.reason).toBe("source_end");
  // The two Latin pages of the clip commit their committed lines (ADR-0079):
  // page A commits the closest corpus line (bw-02, the fuzzy floor's real
  // decision) and page B commits its exact match (bw-06). The non-Latin page
  // commits no known line — the eng model cannot read it and the narrator
  // stays silent rather than fabricating: the session's crop either abstains
  // outright or commits the unread text as-is (message_id "unknown",
  // speech_eligible false), so the total commit count is not pinned, but the
  // named commits are exactly the two Latin lines, in capture order.
  expect(
    session.stats.commits,
    "the session commits the clip's Latin pages",
  ).toBeGreaterThanOrEqual(2);
  expect(session.stats.ocr_calls).toBeGreaterThan(0);

  // ── 5. the committed lines show through the dev UI ─────────────────────────
  const events = (
    (await (
      await request.get(
        `${origin}/api/v1/dev/events?session_id=${encodeURIComponent(sessionId)}&limit=1000`,
      )
    ).json()) as { events: DevEvent[] }
  ).events;
  const commits = events.filter((event) => event.name === "message.committed");
  // The named commits are exactly the two Latin lines, in capture order — and
  // the non-Latin page never commits a known line (the load-bearing negative
  // assertion, ADR-0079): no commit names bw-05, whatever the engine's
  // unreadable-page output is.
  const named = commits.filter((event) => event.data.message_id !== "unknown");
  expect(named.map((event) => event.data.message_id)).toEqual([
    storeIds["bw-02"],
    storeIds["bw-06"],
  ]);
  for (const commit of commits) {
    expect(commit.data.message_id, "the non-Latin page never commits a known line").not.toBe(
      storeIds["bw-05"],
    );
  }

  // The committed lines are visible rows in the timeline the page rendered.
  const timeline = page.getByTestId("event-timeline");
  const committedRows = timeline.locator("tr").filter({ hasText: "message.committed" });
  expect(
    await committedRows.count(),
    "the timeline renders the committed lines",
  ).toBeGreaterThanOrEqual(2);

  // The committed line's detail: the dev UI names the line (its source id),
  // its content class, and whether speech may announce it.
  const pageB = commits.find((event) => event.data.message_id === storeIds["bw-06"]);
  if (!pageB) throw new Error("the bw-06 commit is present in the dev log");
  await page.getByTestId(`event-row-${pageB.seq}`).click();
  await expect(page.getByTestId("commit-message")).toHaveText("bw-06");
  await expect(page.getByTestId("commit-text-kind")).toHaveText("dialogue");
  await expect(page.getByTestId("commit-speech-eligible")).toHaveText(/^(yes|no)$/);
  await shot("05-committed-line");

  // An observation from the same run: the frame the pipeline watched, with
  // the second game's region drawn as the ROI box — its position is the
  // region rect (the borderless 4:3 clip never settles a tracker window, so
  // the box is drawn through the whole-frame basis), i.e. upper-left.
  const observed = events.find(
    (event) => event.name === "text.observed" && event.data.region_present === true,
  );
  expect(observed, "the run observed the second game's region").toBeTruthy();
  if (!observed) throw new Error("the run's dev log carries no region observation");
  await page.getByTestId(`event-row-${observed.seq}`).click();
  const frame = page.getByTestId("frame-image");
  await expect(frame).toBeVisible();
  expect(
    await frame.evaluate((el) => (el as { naturalWidth: number }).naturalWidth),
  ).toBeGreaterThan(0);
  const roiBox = page.getByTestId("roi-box");
  await expect(roiBox).toBeVisible();
  const roiStyle = (await roiBox.getAttribute("style")) ?? "";
  const roiPercent = (name: string): number => {
    const match = new RegExp(`${name}:\\s*([\\d.]+)%`).exec(roiStyle);
    expect(match, `the ROI box carries a ${name} percentage: ${roiStyle}`).toBeTruthy();
    return Number(match?.[1]);
  };
  expect(roiPercent("top"), "the ROI box renders the second game's upper-left region").toBeLessThan(
    30,
  );
  expect(roiPercent("left")).toBeLessThan(20);
  expect(roiPercent("width")).toBeCloseTo(region.w * 100, 1);
  await shot("05-roi-upper-left");

  // ── 6. the Regions page: the second game's region renders where the ───────
  // ── committed data says (upper-left, not the first game's bottom box) ─────
  // The import minted the region's store row id (the seed's "upper-left" id
  // travels as the seed, not the row), so read the row back through the API:
  // the stored rect must be the committed one, and the page's rendered box
  // must be that row's box at that rect.
  const regionsData = (await (
    await request.get(`${origin}/api/v1/profiles/${profile.id}/regions`)
  ).json()) as {
    regions: {
      id: string;
      region_type: string;
      rect_x: number;
      rect_y: number;
      rect_w: number;
      rect_h: number;
    }[];
  };
  expect(
    regionsData.regions,
    "the imported profile stores exactly the second game's region",
  ).toHaveLength(1);
  const regionRow = regionsData.regions[0];
  expect(regionRow.region_type).toBe("textbox");
  expect(regionRow.rect_x, "the stored region is the committed rect").toBeCloseTo(region.x, 6);
  expect(regionRow.rect_y).toBeCloseTo(region.y, 6);
  expect(regionRow.rect_w).toBeCloseTo(region.w, 6);
  expect(regionRow.rect_h).toBeCloseTo(region.h, 6);
  expect(regionRow.rect_y, "the second game's region is the upper-left box").toBeLessThan(0.5);

  await page.goto(`/games/${gameId.id}/regions?profile=${profile.id}`);
  await expect(page.getByTestId("regions-page")).toBeVisible({ timeout: 15_000 });
  const regionBox = page.getByTestId(`region-box-${regionRow.id}`);
  await expect(regionBox).toBeVisible({ timeout: 15_000 });
  const regionStyle = (await regionBox.getAttribute("style")) ?? "";
  const percent = (style: string, name: string): number => {
    const match = new RegExp(`${name}:\\s*([\\d.]+)%`).exec(style);
    expect(match, `the box carries a ${name} percentage: ${style}`).toBeTruthy();
    return Number(match?.[1]);
  };
  // The rendered box is the committed region rect (fractions × 100), within
  // the page's own rounding — and, structurally, in the upper-left quadrant.
  // The first game's bottom box sits at y≈0.756: a page that drew the first
  // layout could not pass this.
  expect(percent(regionStyle, "left")).toBeCloseTo(region.x * 100, 1);
  expect(percent(regionStyle, "top")).toBeCloseTo(region.y * 100, 1);
  expect(percent(regionStyle, "width")).toBeCloseTo(region.w * 100, 1);
  expect(percent(regionStyle, "height")).toBeCloseTo(region.h * 100, 1);
  expect(percent(regionStyle, "top"), "the second game's region renders upper-left").toBeLessThan(
    30,
  );
  expect(percent(regionStyle, "left")).toBeLessThan(20);
  await shot("06-regions");

  // ── 7. the browser stayed clean and the default appliance untouched ───────
  expect(consoleErrors, `browser console: ${consoleErrors.join("\n")}`).toEqual([]);
  expectDefaultDataDirUntouched(run);
});
