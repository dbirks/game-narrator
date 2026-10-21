import AxeBuilder from "@axe-core/playwright";
import { expect, test } from "@playwright/test";
import { readFileSync } from "node:fs";

import { trackConsoleErrors } from "./support/console";
import { ports } from "./support/ports";
import { expectDefaultDataDirUntouched, serverRun } from "./support/servers";

/**
 * The Health screen (docs/design/plum-peach plan.md Phase 1 item 8; ADR-0093
 * §4's /diagnostics → /health rename; ADR-0111) on the packaged binary with
 * the capture seam standing in for the UVC HDMI dongle
 * (GAME_NARRATOR_CAPTURE_FAKE, the capture-dongle walk's labelled-synthetic
 * device fed by a real FFmpeg testsrc2 pipe — AGENTS.md rule 3 class (b)/(d)
 * boundary: the frames are real through the production consumer, every
 * answer flagged synthetic). The walk proves:
 *
 *   1. /diagnostics redirects to /health (the renamed route), and the 3×2
 *      tile grid renders the six canonical components from the ONE status
 *      stream the bar reads (UX-07): the full-wrap labels, tags, one-line
 *      details and the 12-bar spark per tile, the build facts and the
 *      preserved sections full-width under the grid (two columns at the
 *      desktop width); the page's title is the rollup's own summary —
 *      "Health" before the first frame, "N checks not run yet" on this
 *      unprobed idle box, "2 need attention" on the scripted non-ok rollup;
 *   2. the capture tile's UX-11 block reads a real feed: "signal present"
 *      with the JPEG preview (the tracker's bar-framed-window judgment is
 *      honestly "no active picture" on the full-frame test pattern),
 *      LABELLED synthetic in the DOM;
 *   3. the honest states: the anonymous visit's "no frame yet" grid state
 *      (the feed is a viewer read, so the grid never invents tiles) and the
 *      preserved signed-out states on the gated sections;
 *   4. the scripted halves (AGENTS.md rule 3 class (a) synthetic stubs,
 *      labelled as such in each test name): the picture word following the
 *      tracker's judgment (the settled active window's "active picture",
 *      the black card's "no active picture"), and a non-ok rollup's coral
 *      tile whose component the status bar names on another page — the one
 *      stream feeding both — with its tiles' sparklines filled from the
 *      same scripted stream (the level encoding: ok 20 / slow 11 /
 *      fail 5 / unknown the 2-px floor).
 *
 * The sparkline's own contract (the run-3715836 review fix 3): the bars are
 * fed from the one status stream, sampled on the page's fixed one-second
 * cadence into the twelve-bucket window (ADR-0111 §2 as amended) — so an
 * ok tile on the real idle box shows a filled window (the storage tile's
 * bars are asserted at the ok height here) and only a tile nobody probed
 * renders the floor.
 *
 * The three design widths (1440 / 1024 / 390) are screenshotted into
 * test-results/ and axed (WCAG 2.1 A/AA, zero serious/critical); at 390 the
 * document must never scroll horizontally.
 */

const ADMIN = { username: "health-admin", password: "steady as the tide" };
// The seam's fixed device (internal/capture/stack), the same id the
// capture-dongle walk asserts (a stable id, never the raw node path).
const FAKE_ID = "usb-GameNarrator_SyntheticHDMI_0001";
const WIDTHS = [1440, 1024, 390] as const;

/** WCAG 2.1 A/AA: the 2.0 A/AA rules plus the rules 2.1 adds at those levels. */
const WCAG_21_A_AA = ["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"] as const;

interface StatusSnapshot {
  updated_at: string;
  status: string;
  health: {
    components: {
      id: "capture" | "ocr" | "decision" | "tts" | "agent" | "storage";
      level: "ok" | "slow" | "fail" | "unknown";
      detail: string;
    }[];
    ok: number;
    slow: number;
    fail: number;
    unknown: number;
    detail: string;
  };
  device: { version: string; uptime_ms: number };
}

/** The non-ok rollup the scripted-stream case serves (AGENTS.md rule 3a). */
const NON_OK_SNAPSHOT: StatusSnapshot = {
  updated_at: "2026-10-21T00:00:00Z",
  status: "4 ok · 1 slow · 1 fail · decision slow · tts fail",
  health: {
    components: [
      { id: "capture", level: "ok", detail: "29 fps" },
      { id: "ocr", level: "ok", detail: "512 reads" },
      { id: "decision", level: "slow", detail: "p95 3.1 s over budget" },
      { id: "tts", level: "fail", detail: "the provider answered 500" },
      { id: "agent", level: "ok", detail: "answered" },
      { id: "storage", level: "ok", detail: "database ready" },
    ],
    ok: 4,
    slow: 1,
    fail: 1,
    unknown: 0,
    detail: "4 ok · 1 slow · 1 fail",
  },
  device: { version: "v0.9.2-test", uptime_ms: 60_000 },
};

const TILE_IDS = ["capture", "ocr", "decision", "tts", "agent", "storage"] as const;

// The design's full tile names (the run-3715836 review fix 2): the DOM
// text is the whole word — the CSS wraps it, never truncates it.
const TILE_LABEL_TEXT: Record<(typeof TILE_IDS)[number], string> = {
  capture: "capture card",
  ocr: "OCR",
  decision: "decision model",
  tts: "voice (TTS)",
  agent: "agent model",
  storage: "storage",
};

test("health page: redirect, live 3×2 grid, UX-11 signal/preview, screenshots and axe at three widths", async ({
  page,
  request,
}, testInfo) => {
  test.setTimeout(300_000);
  test.skip(
    testInfo.project.name !== "health",
    "runs on the health project's own packaged-binary backend",
  );
  const run = serverRun("health");
  const origin = `http://127.0.0.1:${ports.health}`;
  // 401: the gated reads' expected answer on the anonymous half of the walk;
  // 503: the appliance wires no Bluetooth surface, the Settings-derived
  // section's documented honest degradation (the sibling walks' list).
  // 429 joins the list: the signal and preview reads share the server's one
  // answer per 500 ms one-shot throttle (ADR-0110 §1), and a read that lands
  // inside the other's window gets the 429 the page treats as "retry next
  // tick" — the throttle working, not a fault (the loops are half-period
  // offset, so this is the rare residue).
  const consoleErrors = trackConsoleErrors(page, [401, 429, 503]);

  // ── 1. the renamed route: /diagnostics → /health ──────────────────────────
  await page.goto("/diagnostics");
  await expect(page).toHaveURL(/\/health$/, { timeout: 30_000 });
  // The page's title is the rollup's own summary; before the one stream has
  // delivered a frame (this anonymous visit's 401) it is the page's own
  // name, never a guess (the run-3715836 review fix 4).
  await expect(page.getByRole("heading", { name: "Health", level: 1 })).toBeVisible();

  // The build facts the page preserved from the old diagnostics screen. The
  // health walk's server is the PACKAGED binary (make build's ldflags stamp
  // the version and commit), so — unlike the dev-server walks' "dev"/
  // "unknown" — the stamp must be present: "dev" means the binary is not
  // the packaged one this walk exists to test.
  await expect(page.getByTestId("version")).not.toHaveText("dev");
  await expect(page.getByTestId("commit")).toHaveText(/^[0-9a-f]{7,40}$/);
  await expect(page.getByTestId("platform")).toContainText(/linux\/(amd64|arm64)/);
  await expect(page.getByTestId("readiness")).toHaveText("ok");

  // The anonymous grid state: the status feed answers 401 to an anonymous
  // read (ADR-0100: the rollup is a viewer read, the bar's own rule), so
  // the grid renders its honest "no frame yet" state — never a grid of
  // invented tiles — and the bar carries the claim affordance (the
  // appliance is unclaimed) plus its placeholder string.
  await expect(page.getByTestId("health-tiles-waiting")).toBeVisible();
  await expect(page.getByTestId("nav-setup")).toHaveText("claim this appliance");
  await expect(page.getByTestId("status-live")).toHaveText("· status ·");

  // The preserved sections' honest signed-out states (ADR-0016's gating
  // survived the rename: the data-testids and the sign-in hints are the old
  // page's, unchanged).
  await expect(page.getByTestId("video-input")).toBeVisible();
  await expect(page.getByTestId("Video input-sign-in")).toBeVisible();
  await expect(page.getByTestId("adr0082-window")).toBeVisible();
  await expect(page.getByTestId("model-roles")).toBeVisible();
  await expect(page.getByTestId("voice-registry")).toBeVisible();
  await expect(page.getByTestId("voice-registry-sign-in")).toBeVisible();
  await expect(page.getByTestId("speaker-test")).toBeVisible();
  await expect(page.getByTestId("speaker-test-sign-in")).toBeVisible();

  // The tabs: logs carries the preserved tail (signed out → the hint),
  // setup runs the live onboarding runs (none → the empty state), line
  // traces the session's line activity (none → the honest pointer).
  await page.getByTestId("health-tabs").getByRole("tab", { name: "logs" }).click();
  await expect(page.getByTestId("system-logs")).toBeVisible();
  await expect(page.getByTestId("system-logs-sign-in")).toBeVisible();
  await page.getByTestId("health-tabs").getByRole("tab", { name: "setup runs" }).click();
  await expect(page.getByTestId("health-setup-runs-empty")).toBeVisible();
  await page.getByTestId("health-tabs").getByRole("tab", { name: "line traces" }).click();
  await expect(page.getByTestId("health-line-traces-empty")).toBeVisible();
  await page.getByTestId("health-tabs").getByRole("tab", { name: "health" }).click();
  await expect(page.getByTestId("health-tiles-waiting")).toBeVisible();

  // ── 2. claim the appliance and sign the browser in, through the page ────
  const setup = await request.post(`${origin}/api/v1/auth/setup`, {
    data: { username: ADMIN.username, password: ADMIN.password },
  });
  expect(setup.status(), `auth setup: ${await setup.text()}`).toBe(201);
  // The setup answers the claim, not a session: the login gives THIS API
  // context the cookie the device-selection PUT needs (the page signs in
  // separately, through the form below).
  const apiLogin = await request.post(`${origin}/api/v1/auth/login`, {
    data: { username: ADMIN.username, password: ADMIN.password },
  });
  expect(apiLogin.status(), `auth login: ${await apiLogin.text()}`).toBe(200);

  // The bar's affordance is now the sign-in link (the appliance is claimed);
  // the walk signs in the way an operator does.
  await page.reload();
  await expect(page.getByTestId("nav-login"), "the bar offers sign-in once claimed").toBeVisible({
    timeout: 30_000,
  });
  await page.getByTestId("nav-login").click();
  await expect(page).toHaveURL(/\/login/, { timeout: 15_000 });
  await page.getByTestId("login-username").fill(ADMIN.username);
  await page.getByTestId("login-password").fill(ADMIN.password);
  await page.getByTestId("login-submit").click();
  await expect(page).not.toHaveURL(/\/login/, { timeout: 30_000 });
  await page.goto("/health");

  // ── 2b. the 3×2 grid over the live rollup, now that the feed delivers ──
  // Six tiles, canonical order, each with its tag, its one-line detail and
  // the 12-bar spark; the first frame is due immediately on connect
  // (ADR-0100), so the grid settles within the feed's budget.
  await expect(page.getByTestId("health-grid")).toBeVisible({ timeout: 30_000 });
  for (const id of TILE_IDS) {
    const tile = page.getByTestId(`health-tile-${id}`);
    await expect(tile).toBeVisible();
    await expect(tile.getByTestId(`health-tile-${id}-tag`)).toBeVisible();
    await expect(tile.getByTestId(`health-tile-${id}-detail`)).toBeVisible();
    expect(await tile.getByTestId(`health-tile-${id}-spark`).locator("rect[fill]").count()).toBe(
      12,
    );
  }
  // The server's levels, verbatim, for an idle machine with no session:
  // storage reads its own integrity check (ok); the session-derived tiles
  // are honestly unknown — "no active capture", never a restyled ok
  // (ADR-0111 §2: the client never re-derives a level).
  await expect(page.getByTestId("health-tile-storage-tag")).toHaveText("ok");
  await expect(page.getByTestId("health-tile-capture-tag")).toHaveText("unknown");
  await expect(page.getByTestId("health-tile-capture-detail")).toContainText("no active capture");

  // The page's title is the rollup's own summary: one ok tile (storage) and
  // the five nobody has probed on this fresh appliance — its honest word, in
  // the human phrasing the 2026-10-20 review asked for ("N checks not run
  // yet", never a restyled "all fine") (the run-3715836 review fix 4).
  await expect(page.getByRole("heading", { name: "5 checks not run yet", level: 1 })).toBeVisible();

  // The sparklines are fed from the one stream on the page's fixed
  // one-second cadence (the run-3715836 review fix 3): the storage tile's
  // ok level fills its twelve-bucket window (the ok bar's height is 20 of
  // the spark's 24), while a tile nobody probed keeps the honest 2-px
  // floor — and only that.
  await expect
    .poll(
      async () => {
        const rect = page.getByTestId("health-tile-storage-spark").locator("rect[fill]").first();
        return (await rect.getAttribute("height")) ?? "";
      },
      { timeout: 15_000, message: "the storage sparkline fills from the one stream" },
    )
    .toBe("20");
  await expect(
    page.getByTestId("health-tile-capture-spark").locator("rect[fill]").first(),
  ).toHaveAttribute("height", "2");

  // Persist the seam device's selection so the signal's one-shot path has a
  // device to grab (ADR-0078's persisted selection, the operator's setting).
  const csrf = async () =>
    (await request.cookies(origin)).find((cookie) => cookie.name === "gn_csrf")?.value ?? "";
  const select = await request.put(
    `${origin}/api/v1/capture/devices/${encodeURIComponent(FAKE_ID)}`,
    { headers: { "x-csrf-token": await csrf() } },
  );
  expect(select.status(), `select device: ${await select.text()}`).toBe(200);

  // ── 3. the UX-11 block on a real feed: the words, the JPEG, the label ──
  // The signal read is throttled to one answer per 500 ms server-side and
  // polls at 2.5 s client-side: the timeout is the cadence's, not a fault.
  await expect(page.getByTestId("health-tile-capture-signal")).toHaveText("signal present", {
    timeout: 30_000,
  });
  // The picture half: the tracker's ActiveArea finds the bar-framed
  // (letterboxed) window the game's textbox lives in; the seam's testsrc2
  // pattern fills the whole frame, so the honest judgment on the real pipe
  // is "no active picture" — while the PREVIEW still shows the real JPEG,
  // which is the picture's evidence. The positive word's tracker path is
  // pinned by the Go test on the bordered frame
  // (TestCaptureSignalOneShotBorderedFrame) and its tile rendering by the
  // scripted-stub test below (AGENTS.md rule 3a).
  await expect(page.getByTestId("health-tile-capture-picture")).toHaveText("no active picture", {
    timeout: 30_000,
  });
  // The picture itself: the JPEG through the production encoder (the
  // waiting placeholder and the img share the testid, so wait for the img).
  const preview = page.locator('img[data-testid="health-tile-capture-preview"]');
  await expect(preview).toBeVisible({ timeout: 30_000 });
  expect(await preview.getAttribute("src")).toMatch(/^blob:/);
  // The labelled-honesty: the seam's answers are flagged synthetic in the
  // DOM (AGENTS.md rule 3) — the meta line says so.
  await expect(page.getByTestId("health-tile-capture-signal-block")).toContainText(
    "synthetic seam (labelled, not hardware)",
  );

  // The signed-in sections: the log tail renders (the ring is young — the
  // empty note, not the hint), the host vitals read the machine's numbers.
  await page.getByTestId("health-tabs").getByRole("tab", { name: "logs" }).click();
  await expect(page.getByTestId("system-logs")).toBeVisible();
  await expect(page.getByTestId("system-logs-sign-in")).toHaveCount(0);
  await page.getByTestId("health-tabs").getByRole("tab", { name: "health" }).click();
  await expect(page.getByTestId("host-vitals")).toBeVisible();
  await expect(page.getByTestId("host-ram")).toBeVisible();
  await expect(page.getByTestId("host-storage")).toBeVisible();

  // ── 4. the three design widths: screenshots + axe + the no-scroll rule ──
  for (const width of WIDTHS) {
    await page.setViewportSize({ width, height: 900 });
    await page.goto("/health");
    await expect(page.getByTestId("health-grid")).toBeVisible({ timeout: 30_000 });
    // The bar's live string (the feed's, not the "· status ·" placeholder)
    // so the shot shows the real status line.
    await expect(page.getByTestId("status-live")).not.toContainText("status ·", {
      timeout: 15_000,
    });
    // The shot's sparklines: wait for the storage tile's window to fill from
    // the one stream (the ok bar's height 20) so the picture shows fed
    // sparklines, not the pre-first-tick floor.
    await expect
      .poll(
        async () => {
          const rect = page.getByTestId("health-tile-storage-spark").locator("rect[fill]").first();
          return (await rect.getAttribute("height")) ?? "";
        },
        { timeout: 15_000, message: "the storage sparkline fills before the shot" },
      )
      .toBe("20");
    // Settle the fonts: the self-hosted faces swap in asynchronously
    // (font-display: swap); a shot before the swap reads the fallback layout.
    await page.evaluate(async () => {
      const g = globalThis as unknown as {
        document?: { fonts?: { ready?: Promise<unknown> } };
      };
      await g.document?.fonts?.ready;
    });
    // The grid spans the content width at each design width (the
    // run-3715836 review fix 1): three columns at 1440, two at 1024, one at
    // 390 — never the ~420px column the pre-fix page wrapped it in. Asserted
    // after the font swap so the measured layout is the shot's layout.
    const gridColumns = await page.getByTestId("health-grid").evaluate((element) => {
      const g = globalThis as unknown as {
        getComputedStyle?: (el: unknown) => { gridTemplateColumns?: string };
      };
      return g.getComputedStyle?.(element)?.gridTemplateColumns ?? "";
    });
    const gridBox = await page.getByTestId("health-grid").boundingBox();
    expect(
      gridColumns.split(" ").filter(Boolean).length,
      `the grid's column count @${width}px`,
    ).toBe(width === 1440 ? 3 : width === 1024 ? 2 : 1);
    expect(
      gridBox?.width ?? 0,
      `the grid spans the content width @${width}px`,
    ).toBeGreaterThanOrEqual(width === 1440 ? 1000 : width === 1024 ? 760 : 300);

    // The tile labels are the design's full names, wrapped — never
    // truncated (the run-3715836 review fix 2): the DOM text is the whole
    // word and the CSS wraps it, so no ellipsis can appear.
    for (const id of TILE_IDS) {
      const label = page.getByTestId(`health-tile-${id}`).locator(".health-tile-label");
      expect((await label.textContent())?.trim(), `the ${id} tile's label text @${width}px`).toBe(
        TILE_LABEL_TEXT[id],
      );
      const labelCs = await label.evaluate((element) => {
        const g = globalThis as unknown as {
          getComputedStyle?: (el: unknown) => { whiteSpace?: string };
        };
        return g.getComputedStyle?.(element)?.whiteSpace ?? "";
      });
      expect(labelCs, `the ${id} tile's label wraps @${width}px`).not.toBe("nowrap");
    }

    if (width === 390) {
      // The design's third review width: no horizontal page scroll — the
      // grid reflows to one column and the wide sections scroll inside
      // their own wrappers, so the document is never wider than the
      // viewport (docs/design/plum-peach IA, Phase 0 review fix 3).
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
        `/health @390px scrolls horizontally: documentElement.scrollWidth ${widths.scrollWidth} > innerWidth ${widths.innerWidth}`,
      ).toBeLessThanOrEqual(widths.innerWidth);
    }
    const shotPath = `${testInfo.outputDir}/health-${width}.png`;
    await page.screenshot({ path: shotPath, fullPage: true });
    await testInfo.attach(`health @${width}px`, {
      body: readFileSync(shotPath),
      contentType: "image/png",
    });

    const results = await new AxeBuilder({ page }).withTags([...WCAG_21_A_AA]).analyze();
    const serious = (results.violations ?? []).filter(
      (violation) => violation.impact === "serious" || violation.impact === "critical",
    );
    expect(
      serious,
      `axe @${width}px: ${serious
        .map((violation) => `${violation.id} [${violation.impact}] — ${violation.help}`)
        .join("\n")}`,
    ).toEqual([]);
  }

  expectDefaultDataDirUntouched(run);
  expect(consoleErrors, `browser console: ${consoleErrors.join("\n")}`).toEqual([]);
});

test("health page: the picture word follows the tracker's judgment — active and black halves (scripted signal answers, synthetic stubs — AGENTS.md rule 3a)", async ({
  page,
  request,
}, testInfo) => {
  test.setTimeout(180_000);
  test.skip(
    testInfo.project.name !== "health",
    "runs on the health project's own packaged-binary backend",
  );
  const run = serverRun("health");
  const origin = `http://127.0.0.1:${ports.health}`;
  // 429 joins the list: the signal and preview reads share the server's one
  // answer per 500 ms one-shot throttle (ADR-0110 §1), and a read that lands
  // inside the other's window gets the 429 the page treats as "retry next
  // tick" — the throttle working, not a fault (the loops are half-period
  // offset, so this is the rare residue).
  const consoleErrors = trackConsoleErrors(page, [401, 429, 503]);

  // One appliance per run (the walk's three tests share this server and its
  // data dir): the claim is once per appliance (ADR-0016 §7), so on the
  // first test it answers 201 and on the later ones the 409 that says it was
  // already claimed. Either way the admin exists for the page's sign-in.
  const setup = await request.post(`${origin}/api/v1/auth/setup`, {
    data: { username: ADMIN.username, password: ADMIN.password },
  });
  expect(
    [201, 409].includes(setup.status()),
    `auth setup: ${setup.status()} ${await setup.text()}`,
  ).toBe(true);
  // This test's API context is fresh (Playwright makes one per test): the
  // login POST gives it the session the device-selection PUT needs.
  const login = await request.post(`${origin}/api/v1/auth/login`, {
    data: { username: ADMIN.username, password: ADMIN.password },
  });
  expect(login.status(), `auth login: ${await login.text()}`).toBe(200);
  await page.goto("/login");
  await page.getByTestId("login-username").fill(ADMIN.username);
  await page.getByTestId("login-password").fill(ADMIN.password);
  await page.getByTestId("login-submit").click();
  await expect(page).not.toHaveURL(/\/login/, { timeout: 30_000 });

  const csrf = async () =>
    (await request.cookies(origin)).find((cookie) => cookie.name === "gn_csrf")?.value ?? "";
  const select = await request.put(
    `${origin}/api/v1/capture/devices/${encodeURIComponent(FAKE_ID)}`,
    { headers: { "x-csrf-token": await csrf() } },
  );
  expect(select.status(), `select device: ${await select.text()}`).toBe(200);

  // The scripted halves (AGENTS.md rule 3 class (a)): the signal ANSWER is
  // stubbed in the browser with the ADR-0110 §1 shape; only the tracker's
  // judgment is scripted — the tracker's positive judgment on a real
  // (bordered) frame is pinned by the Go test
  // TestCaptureSignalOneShotBorderedFrame, and the real-seam words by the
  // first walk test. The preview keeps coming from the real pipe.
  const scripted = {
    bordered: {
      device: FAKE_ID,
      mode: "640x480 MJPEG @ 10 fps",
      source: "oneshot",
      signal: true,
      fps_actual: 10,
      fps_target: 10,
      picture: { x: 32, y: 40, w: 576, h: 400, settled: true, active: true },
      age_ms: 20,
      synthetic: true,
    },
    black: {
      device: FAKE_ID,
      mode: "640x480 MJPEG @ 10 fps",
      source: "oneshot",
      signal: true,
      fps_actual: 10,
      fps_target: 10,
      picture: { x: 0, y: 0, w: 640, h: 480, settled: true, active: false },
      age_ms: 20,
      synthetic: true,
    },
  };

  // The positive half: a settled window the tracker found active (the
  // game's letterboxed screen) — the tile says "active picture".
  await page.route("**/api/v1/capture/signal", (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(scripted.bordered),
    }),
  );
  await page.goto("/health");
  await expect(page.getByTestId("health-grid")).toBeVisible({ timeout: 30_000 });
  await expect(page.getByTestId("health-tile-capture-signal")).toHaveText("signal present", {
    timeout: 30_000,
  });
  await expect(page.getByTestId("health-tile-capture-picture")).toHaveText("active picture", {
    timeout: 30_000,
  });
  await page.screenshot({
    path: `${testInfo.outputDir}/health-active-picture.png`,
    fullPage: true,
  });
  await testInfo.attach("health, active picture", {
    body: readFileSync(`${testInfo.outputDir}/health-active-picture.png`),
    contentType: "image/png",
  });

  // The black half: the card is delivering (signal PRESENT — the words UX-11
  // exists to separate from "no signal") but the picture is not active.
  await page.route("**/api/v1/capture/signal", (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(scripted.black),
    }),
  );
  await page.reload();
  await expect(page.getByTestId("health-grid")).toBeVisible({ timeout: 30_000 });
  await expect(page.getByTestId("health-tile-capture-signal")).toHaveText("signal present", {
    timeout: 30_000,
  });
  await expect(page.getByTestId("health-tile-capture-picture")).toHaveText("no active picture", {
    timeout: 30_000,
  });
  await page.screenshot({ path: `${testInfo.outputDir}/health-black-feed.png`, fullPage: true });
  await testInfo.attach("health, black feed", {
    body: readFileSync(`${testInfo.outputDir}/health-black-feed.png`),
    contentType: "image/png",
  });

  expectDefaultDataDirUntouched(run);
  expect(consoleErrors, `browser console: ${consoleErrors.join("\n")}`).toEqual([]);
});

test("health page: a non-ok tile wears the coral tag and its component names the status bar on another page (scripted status stream, synthetic stub — AGENTS.md rule 3a)", async ({
  page,
  request,
}, testInfo) => {
  test.setTimeout(180_000);
  test.skip(
    testInfo.project.name !== "health",
    "runs on the health project's own packaged-binary backend",
  );
  const run = serverRun("health");
  const origin = `http://127.0.0.1:${ports.health}`;
  // 429 joins the list: the signal and preview reads share the server's one
  // answer per 500 ms one-shot throttle (ADR-0110 §1), and a read that lands
  // inside the other's window gets the 429 the page treats as "retry next
  // tick" — the throttle working, not a fault (the loops are half-period
  // offset, so this is the rare residue).
  const consoleErrors = trackConsoleErrors(page, [401, 429, 503]);

  const setup = await request.post(`${origin}/api/v1/auth/setup`, {
    data: { username: ADMIN.username, password: ADMIN.password },
  });
  expect(
    [201, 409].includes(setup.status()),
    `auth setup: ${setup.status()} ${await setup.text()}`,
  ).toBe(true);
  // This test's API context is fresh (Playwright makes one per test): the
  // login POST gives it the session the device-selection PUT needs.
  const login = await request.post(`${origin}/api/v1/auth/login`, {
    data: { username: ADMIN.username, password: ADMIN.password },
  });
  expect(login.status(), `auth login: ${await login.text()}`).toBe(200);
  await page.goto("/login");
  await page.getByTestId("login-username").fill(ADMIN.username);
  await page.getByTestId("login-password").fill(ADMIN.password);
  await page.getByTestId("login-submit").click();
  await expect(page).not.toHaveURL(/\/login/, { timeout: 30_000 });

  // The scripted half (AGENTS.md rule 3 class (a)): the whole status
  // snapshot — GET and the SSE stream — is stubbed in the browser with the
  // server's shape and one non-ok component (tts fail, decision slow). The
  // point is the ONE stream (UX-07): the same bytes drive the tile's coral
  // tag here and the bar's tail on another page, so the two cannot disagree.
  const streamBody = [
    `event: status.changed`,
    `data: ${JSON.stringify(NON_OK_SNAPSHOT)}`,
    ``,
    ``,
  ].join("\n");
  await page.route("**/api/v1/status", (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(NON_OK_SNAPSHOT),
    }),
  );
  await page.route("**/api/v1/status/stream", (route) =>
    route.fulfill({ status: 200, contentType: "text/event-stream", body: streamBody }),
  );

  // The tile's half: the fail tag and the error language (the coral border
  // class), on the tile the rollup names. The page's title is the rollup's
  // own summary of the scripted shape — one slow and one fail: the mock's
  // "2 need attention" (the run-3715836 review fix 4).
  await page.goto("/health");
  await expect(page.getByTestId("health-grid")).toBeVisible({ timeout: 30_000 });
  await expect(page.getByRole("heading", { name: "2 need attention", level: 1 })).toBeVisible();
  await expect(page.getByTestId("health-tile-tts-tag")).toHaveText("fail", { timeout: 30_000 });
  await expect(page.getByTestId("health-tile-tts")).toHaveClass(/health-tile-warn/);
  await expect(page.getByTestId("health-tile-tts-detail")).toContainText(
    "the provider answered 500",
  );
  await expect(page.getByTestId("health-tile-decision-tag")).toHaveText("slow");
  await expect(page.getByTestId("health-tile-decision")).toHaveClass(/health-tile-warn/);
  // An ok tile from the same fixture keeps the neutral language.
  await expect(page.getByTestId("health-tile-capture")).not.toHaveClass(/health-tile-warn/);

  // The sparklines are fed from the SAME scripted stream: the page's fixed
  // one-second sampler fills each tile's twelve-bucket window with the
  // rollup's levels — ok 100 → 20 px, slow 55 → 11 px, fail 25 → 5 px (the
  // fixed-scale encoding, the run-3715836 review fix 3) — so the shot shows
  // the rollup's shape in bars, not an empty floor.
  await expect
    .poll(
      async () => {
        const rect = page.getByTestId("health-tile-tts-spark").locator("rect[fill]").first();
        return (await rect.getAttribute("height")) ?? "";
      },
      { timeout: 15_000, message: "the scripted stream fills the sparkline windows" },
    )
    .toBe("5");
  await expect(
    page.getByTestId("health-tile-decision-spark").locator("rect[fill]").first(),
  ).toHaveAttribute("height", "11");
  await expect(
    page.getByTestId("health-tile-capture-spark").locator("rect[fill]").first(),
  ).toHaveAttribute("height", "20");

  await page.screenshot({ path: `${testInfo.outputDir}/health-non-ok-tile.png`, fullPage: true });
  await testInfo.attach("health, non-ok tile", {
    body: readFileSync(`${testInfo.outputDir}/health-non-ok-tile.png`),
    contentType: "image/png",
  });

  // The bar's half, on ANOTHER page: the status string the same feed
  // carries names the non-ok components (the rollup's tail), which is what
  // the UX-07 one-stream rule exists to guarantee.
  await page.goto("/library");
  const live = page.getByTestId("status-live");
  await expect(live).toContainText("tts fail", { timeout: 30_000 });
  await expect(live).toContainText("decision slow");
  await page.screenshot({
    path: `${testInfo.outputDir}/health-non-ok-bar-library.png`,
    fullPage: true,
  });

  expectDefaultDataDirUntouched(run);
  expect(consoleErrors, `browser console: ${consoleErrors.join("\n")}`).toEqual([]);
});
