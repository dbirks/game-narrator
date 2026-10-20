import AxeBuilder from "@axe-core/playwright";
import { expect, test, type APIRequestContext, type Page } from "@playwright/test";
import { readFileSync, mkdirSync, writeFileSync } from "node:fs";
import { get as httpGet } from "node:http";
import { dirname, join, resolve } from "node:path";
import { tmpdir } from "node:os";

import {
  expectDefaultDataDirUntouched,
  expectIsolatedDataDir,
  serverNameForProject,
  serverRun,
} from "./support/servers";
import { trackConsoleErrors } from "./support/console";

/**
 * The setup board end-to-end (docs/design/plum-peach plan.md Phase 1 item 4;
 * ADR-0093 open question (a) + UX-06; the board's backend is ADR-0108),
 * against the packaged binary with the `agent_reasoning` role pointed at the
 * scripted setup gateway (web/e2e/fake-setup-model.mjs) and the speech roles
 * at the scripted TTS instance. No mocked workflow: the real API, the real
 * job queue, the real agent loop and tools, the real `setup.*` SSE events,
 * the real audition job and the real approval boundary — only the *model* is
 * scripted (AGENTS.md rule 3, the same labelled-synthetic boundary as
 * onboarding.spec.ts).
 *
 * (a) the walk: sign in, start a run with the staged script, watch the six
 *     rail cells, the three tiles and the cards on the four-column board;
 *     the cards MOVE on the SSE stream (the spec asserts the stream carried
 *     Marla's finding → brief → voicing → done in order, and that the board
 *     snapshot endpoint was NOT polled while they moved — it is fetched at
 *     load and on roster tool completions only); Marla's voicing card carries
 *     the coral border; the publish review approves through the token; the
 *     completed run lands on the Cast seam with the prominent
 *     "Start narrating".
 * (b) prefers-reduced-motion: the glide settles instantly (the design's
 *     accessibility rule) while the board still shows the run's state.
 * (c) the three design widths (1440 / 1024 / 390): screenshots into
 *     test-results and axe (WCAG 2.1 A/AA) — zero serious or critical, and
 *     no horizontal page scroll at 390.
 *
 * The three tests share one run: (a) starts it and records its id, (b) and
 * (c) deep-link to it (?run=<id>) — the deep link is the snapshot-rebuild
 * path ADR-0108 names (a fresh load cannot trust a stream it never saw).
 * The suite runs serially (workers: 1), so the shared run id is a module
 * variable, the same convention the onboarding spec's scenario setter uses.
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

const ADMIN = { username: "setup-board-admin", password: "steady as the tide" };
const WAIT_MS = 120_000; // job queue + scripted model + the audition job
const MARLA_BRIEF =
  "A calm harbor voice, unhurried, with a slight salt air in it. Late thirties, " +
  "low and steady, slightly hoarse; measured cadence, pauses before the important word.";

/** The run (a) starts, (b) and (c) deep-link to. */
let sharedRunId = "";

/** The board card's run id, shared by the three tests. The module variable is
 *  the fast path; the file under the run root is the fallback for the case
 *  where the worker re-imports the test file between tests (module state is
 *  per-worker, and a re-import starts at ""). Both live and die with the
 *  run's e2e root, so no stale run leaks into a later one. */
function sharedRunIdPath(): string {
  return join(
    tmpdir(),
    process.env.GN_E2E_RUN_ROOT ?? "game-narrator-e2e-shared",
    "setup-board.run-id",
  );
}

function rememberRunId(runId: string): void {
  mkdirSync(dirname(sharedRunIdPath()), { recursive: true });
  writeFileSync(sharedRunIdPath(), runId);
}

function loadSharedRunId(): string {
  if (sharedRunId !== "") return sharedRunId;
  try {
    sharedRunId = readFileSync(sharedRunIdPath(), "utf8").trim();
  } catch {
    sharedRunId = "";
  }
  return sharedRunId;
}

// ── helpers ─────────────────────────────────────────────────────────────────

/** Claim the appliance if it is unclaimed, then sign in (the onboarding
 *  spec's shape: setup mints no session, so login is always the step that
 *  authenticates). */
async function signIn(page: Page) {
  await page.goto("/games/new");
  const setupForm = page.getByTestId("setup-form");
  const loginUsername = page.getByTestId("login-username");
  await expect(setupForm.or(loginUsername)).toBeVisible({ timeout: 20_000 });
  if (await setupForm.isVisible()) {
    await page.getByTestId("setup-username").fill(ADMIN.username);
    await page.getByTestId("setup-password").fill(ADMIN.password);
    await page.getByTestId("setup-submit").click();
    await expect(page).toHaveURL(/\/login$/);
  }
  await loginUsername.fill(ADMIN.username);
  await page.getByTestId("login-password").fill(ADMIN.password);
  await page.getByTestId("login-submit").click();
  await expect(page.getByTestId("nav-user")).toBeVisible();
  await expect(page.getByTestId("nav-role")).toHaveText("admin");
}

/** The one board card a name owns (the cards are id-keyed; the name is the
 *  stable handle). Only the card element itself matches — the card's inner
 *  name/lines spans carry their own testids, and the article is the element
 *  that carries data-stage. */
function boardCard(page: Page, name: string) {
  return page.locator("article.setup-card", { hasText: name });
}

/** The session cookie the spec-process SSE read authenticates with (the
 *  stream endpoint takes the same session the browser's EventSource uses).
 *  The server mints `gn_session` (internal/auth's CookieSession). */
async function sessionCookie(page: Page): Promise<string> {
  const state = await page.context().storageState();
  return state.cookies
    .filter((cookie) => cookie.name === "gn_session")
    .map((cookie) => `${cookie.name}=${cookie.value}`)
    .join("; ");
}

/** The spec-process's own authenticated API session (the `request` fixture
 *  keeps its own cookie jar — the capture-device spec's pattern): a login of
 *  the same admin the page signed in as, plus the x-csrf-token header every
 *  authenticated POST needs (the double-submit the browser's client sends). */
async function apiHeaders(
  request: APIRequestContext,
  origin: string,
): Promise<Record<string, string>> {
  const login = await request.post(`${origin}/api/v1/auth/login`, {
    data: { username: ADMIN.username, password: ADMIN.password },
  });
  expect(login.status(), "the spec's own API session signed in").toBe(200);
  const cookies = await request.cookies(origin);
  const csrf = cookies.find((cookie) => cookie.name === "gn_csrf")?.value ?? "";
  expect(csrf, "the spec's session carries the CSRF cookie").not.toBe("");
  return { "x-csrf-token": csrf };
}

interface WireFrame {
  id: string;
  event: string;
  data: Record<string, unknown>;
}

/** The post-hoc read of the durable event log (AGENTS.md: the evidence is
 *  the wire, not the UI alone). Replays the setup.* frames from the start
 *  (last_event_id=0) and cuts the stream after `ms` — the endpoint replays
 *  history before going live, so a bounded read is a complete read of what
 *  the run emitted. The request is destroyed on the timer: the partial body
 *  received by then is exactly the replay (the stream would otherwise run
 *  forever, and a plain buffered read would never settle). */
async function readSetupFrames(page: Page, origin: string, ms: number): Promise<WireFrame[]> {
  const cookie = await sessionCookie(page);
  const target = new URL(origin);
  const collected: Buffer[] = [];
  const done = new Promise<string>((resolveText, reject) => {
    const request = httpGet(
      {
        // node:http wants the port separate: a host with the port in it is
        // resolved as one hostname and dies in getaddrinfo.
        hostname: target.hostname,
        port: target.port,
        path: "/api/v1/events?names=setup.character_stage,setup.progress&last_event_id=0",
        headers: { cookie, accept: "text/event-stream" },
      },
      (response) => {
        if (response.statusCode !== 200) {
          response.resume();
          reject(new Error(`the event stream answered ${response.statusCode}`));
          return;
        }
        response.on("data", (chunk: Buffer) => collected.push(chunk));
      },
    );
    request.on("error", (error) => {
      // The timer's destroy surfaces as ECONNRESET: the expected end.
      if (String(error).includes("ECONNRESET")) return;
      reject(error);
    });
    setTimeout(() => {
      request.destroy();
      resolveText(Buffer.concat(collected).toString("utf8"));
    }, ms);
  });
  const text = await done;
  const frames: WireFrame[] = [];
  for (const block of text.split("\n\n")) {
    let id = "";
    let event = "";
    let data = "";
    for (const line of block.split("\n")) {
      if (line.startsWith("id:")) id = line.slice(3).trim();
      else if (line.startsWith("event:")) event = line.slice(6).trim();
      else if (line.startsWith("data:")) data += line.slice(5).trimStart();
    }
    if (data !== "") {
      // The data line carries the event ENVELOPE (schema_version · event_id ·
      // seq · name · occurred_at · data); the payload the spec asserts on is
      // the envelope's data field — unwrapped here so the frames read as
      // payloads.
      const envelope = JSON.parse(data) as { data?: unknown };
      frames.push({ id, event, data: (envelope.data ?? {}) as Record<string, unknown> });
    }
  }
  return frames;
}

// ── (a) the walk ────────────────────────────────────────────────────────────

test("setup board: the cards glide on the stream, no polling, coral while voicing, Cast on completion", async ({
  page,
  request,
}, testInfo) => {
  test.setTimeout(420_000);
  test.skip(
    testInfo.project.name !== "setup-board",
    "runs on the setup-board project's fresh appliance",
  );
  await expectIsolatedDataDir(serverNameForProject("setup-board"));
  const consoleErrors = trackConsoleErrors(page, [401]);

  const boardGets: number[] = [];
  page.on("request", (req) => {
    if (req.method() === "GET" && /\/api\/v1\/onboarding\/runs\/[^/]+\/board$/.test(req.url())) {
      boardGets.push(Date.now());
    }
  });

  await signIn(page);
  await page.goto("/games/new");

  // ── the setup form: title, platform, web off, staged script ─────────────
  await expect(page.getByTestId("onb-setup-form")).toBeVisible();
  await page.getByTestId("onb-title").fill("Harbour Lights");
  await page.getByTestId("onb-platform").fill("GameCube");
  await expect(page.getByTestId("onb-web-toggle")).toBeChecked();
  await page.getByTestId("onb-web-toggle").uncheck();
  await page.getByTestId("onb-file").setInputFiles(ADVENTURE_CSV);
  await page.getByTestId("onb-start").click();

  // The board owns the run's identity in the URL (the deep link (b) and (c)
  // reuse).
  await expect(page).toHaveURL(/\/games\/new\?run=[a-z0-9_-]+/, { timeout: WAIT_MS });
  sharedRunId = new URL(page.url()).searchParams.get("run") ?? "";
  expect(sharedRunId, "the board's URL carries the run id").not.toBe("");
  rememberRunId(sharedRunId);

  // ── the board's structure: rail, tiles, four columns ─────────────────────
  await expect(page.getByTestId("setup-board"), "the live board is the page").toBeVisible();
  for (const cell of ["find_game", "dialogue", "characters", "briefs", "voices", "publish"]) {
    await expect(page.getByTestId(`setup-rail-cell-${cell}`)).toBeVisible();
  }
  for (const tile of ["onb-tile-lines", "onb-tile-characters", "onb-tile-voiced"]) {
    await expect(page.getByTestId(tile)).toBeVisible();
  }
  for (const stage of ["finding", "brief", "voicing", "done"]) {
    await expect(page.getByTestId(`setup-column-${stage}`)).toBeVisible();
  }

  // ── the roster lands as the agent stages it (snapshot + stream) ─────────
  const marla = boardCard(page, "Marla");
  const corvin = boardCard(page, "Corvin");
  await expect(marla, "Marla's card is on the board").toBeVisible({ timeout: WAIT_MS });
  await expect(corvin, "Corvin's card is on the board").toBeVisible({ timeout: WAIT_MS });
  // Both stage_character calls landed in the tool timeline (the run's own
  // record of what staged the roster).
  await expect(page.getByTestId("onb-tool-stage_character")).toHaveCount(2, {
    timeout: WAIT_MS,
  });

  // Marla moves FINDING → BRIEF on analyze_character's success — the card's
  // data-stage is the assertion the stream made it.
  await expect(marla).toHaveAttribute("data-stage", "brief", { timeout: WAIT_MS });
  const tBrief = Date.now();

  // ── the voicing half: the real audition job drives the real observer ────
  // The design job is queue-backed (the response carries the job id), so the
  // spec reads the voice back until the durable row has the take.
  const origin = new URL(page.url()).origin;
  const headers = await apiHeaders(request, origin);
  const createResponse = await request.post("/api/v1/voices/auditions", {
    data: { character: "Marla", brief: MARLA_BRIEF },
    headers,
  });
  expect(
    createResponse.status(),
    "the audition was accepted (queue-backed: the job does the design)",
  ).toBe(202);
  const created = (await createResponse.json()) as { voice_id: string };
  expect(created.voice_id, "the audition names its voice").not.toBe("");

  await expect
    .poll(
      async () => {
        const detail = (await (await request.get(`/api/v1/voices/${created.voice_id}`)).json()) as {
          auditions?: unknown[];
        };
        return Array.isArray(detail.auditions) ? detail.auditions.length : -1;
      },
      { timeout: WAIT_MS },
    )
    .toBeGreaterThan(0);

  const detail = (await (await request.get(`/api/v1/voices/${created.voice_id}`)).json()) as {
    auditions: { id: string }[];
  };
  expect(detail.auditions, "the take is durably recorded").toHaveLength(detail.auditions.length);
  expect(detail.auditions.length).toBeGreaterThan(0);

  // The card moves into VOICING on the SSE event the StageObserver emitted
  // (AuditionsReady), and the VOICING card carries the coral border — the
  // one hot column of the design (ADR-0093 2c).
  await expect(marla, "the voicing card is the stream's move").toHaveAttribute(
    "data-stage",
    "voicing",
    { timeout: WAIT_MS },
  );
  const coralProbe = await page.evaluate(() => {
    // The e2e tsconfig has no DOM lib, so the page's style API is reached
    // through a typed globalThis cast (the redesign spec's pattern). A probe
    // element styled var(--coral) resolves the design's token exactly the way
    // the stylesheet resolves it for the card — theme included — so the
    // assertion reads the sheet's value instead of hard-coding a hex.
    const g = globalThis as unknown as {
      document: {
        createElement(tag: string): { style: { borderTopColor: string } };
        body: { appendChild(el: unknown): void; removeChild(el: unknown): void };
      };
      getComputedStyle: (el: unknown) => { borderTopColor: string };
    };
    const probe = g.document.createElement("i");
    probe.style.borderTopColor = "var(--coral)";
    g.document.body.appendChild(probe);
    const color = g.getComputedStyle(probe).borderTopColor;
    g.document.body.removeChild(probe);
    return color;
  });
  expect(coralProbe, "the design's coral token resolves to a color").not.toBe("");
  const borders = await marla.evaluate((el) => {
    const g = globalThis as unknown as {
      getComputedStyle: (el: unknown) => { borderTopColor: string; borderLeftColor: string };
    };
    const style = g.getComputedStyle(el);
    return { top: style.borderTopColor, left: style.borderLeftColor };
  });
  expect(borders.top, "the voicing card's border is the design's coral").toBe(coralProbe);
  expect(borders.left).toBe(coralProbe);

  // Approve the take (the consent-gated freeze, docs/spec/14): the card then
  // moves VOICING → DONE on VoiceApproved's event.
  const approveResponse = await request.post(`/api/v1/voices/${created.voice_id}/approve`, {
    data: {
      audition_id: detail.auditions[0].id,
      consent:
        "e2e consent: the recorded takes are synthetic, generated by the scripted TTS instance",
    },
    headers,
  });
  expect(approveResponse.status(), "the take was approved").toBe(200);
  await expect(marla, "the done card is the approval's move").toHaveAttribute(
    "data-stage",
    "done",
    { timeout: WAIT_MS },
  );

  // And the board the page shows equals what the log will say (ADR-0108's
  // snapshot ≡ replay, at the end of the board's walk): Marla done, Corvin
  // still finding. The page still mounts the board here — after the Cast
  // navigation below it is gone, so this is where the DOM half of the check
  // can live.
  await expect(marla).toHaveAttribute("data-stage", "done");
  await expect(corvin, "the second character stayed on finding").toHaveAttribute(
    "data-stage",
    "finding",
  );

  // ── the no-polling assertion (UX-06) ─────────────────────────────────────
  // The board snapshot was fetched at load and at most once per roster tool
  // completion (event-triggered sync); it was NOT polled: from the moment
  // Marla reached BRIEF — while VOICING and DONE still moved her — a single
  // extra GET /board would be the poll. The cards moved on events.
  const getsAfterBrief = boardGets.filter((t) => t >= tBrief);
  expect(
    getsAfterBrief,
    `the board endpoint was polled ${getsAfterBrief.length} time(s) after Marla reached brief (the moves are the stream's)`,
  ).toEqual([]);
  expect(
    boardGets.length,
    "the snapshot fetches stay bounded by the roster tool completions",
  ).toBeLessThanOrEqual(4);

  // ── the publish boundary: the token is the only path to publication ─────
  await expect(page.getByTestId("onb-review-item-publish")).toBeVisible({ timeout: WAIT_MS });
  await expect(page.getByTestId("onb-draft-status")).toHaveText("draft");
  await page.getByTestId("onb-review-approve-publish").click();
  await expect(page.getByTestId("onb-draft-status")).toHaveText("published", {
    timeout: WAIT_MS,
  });
  const publishCall = page.getByTestId("onb-tool-publish_profile");
  await expect(publishCall, "the publish went through the tool").toBeVisible({
    timeout: WAIT_MS,
  });
  await expect(page.getByTestId("onb-tool-status-publish_profile")).toHaveText("succeeded");

  // ── the completion handoff (ADR-0093 (a)) ────────────────────────────────
  // The run's work is done; the prominent Start narrating is the way to the
  // Cast seam, where the voices are reviewed and the narration starts.
  await expect(page.getByTestId("setup-complete"), "the completion panel").toBeVisible({
    timeout: WAIT_MS,
  });
  await page.getByTestId("onb-start-narrating").click();
  await expect(page).toHaveURL(/\/cast$/, { timeout: WAIT_MS });
  await expect(page.getByTestId("cast-start-narrating")).toBeVisible();
  await expect(page.getByTestId("cast-start-narrating")).toHaveText("Start narrating");

  // ── the wire read: the stream the UI claims to be driven by ─────────────
  // The durable log replayed from the start (AGENTS.md evidence): Marla's
  // moves are finding → brief → voicing → done, in order, and the progress
  // frames fed the tiles and the rail.
  const frames = await readSetupFrames(page, origin, 10_000);
  const stagesByCharacter: Record<string, string[]> = {};
  let progressCount = 0;
  let maxVoiced = 0;
  for (const frame of frames) {
    if (frame.event === "setup.character_stage") {
      const data = frame.data as { name?: string; stage?: string };
      if (typeof data.name === "string" && typeof data.stage === "string") {
        (stagesByCharacter[data.name] ??= []).push(data.stage);
      }
    } else if (frame.event === "setup.progress") {
      progressCount += 1;
      const data = frame.data as { voiced?: number };
      if (typeof data.voiced === "number" && data.voiced > maxVoiced) {
        maxVoiced = data.voiced;
      }
    }
  }
  const marlaStages = stagesByCharacter["Marla"] ?? [];
  const marlaOrder = ["finding", "brief", "voicing", "done"];
  let cursor = 0;
  for (const stage of marlaStages) {
    if (stage === marlaOrder[cursor]) cursor += 1;
    else if (marlaOrder.indexOf(stage) < cursor) {
      throw new Error(`Marla's card moved backwards on the wire (${stage})`);
    }
  }
  expect(
    cursor,
    `the wire carried Marla's full walk (finding → brief → voicing → done); saw: ${marlaStages.join(
      " → ",
    )}`,
  ).toBe(marlaOrder.length);
  expect(stagesByCharacter["Corvin"] ?? [], "Corvin was staged too").toContain("finding");
  expect(progressCount, "the tiles' progress frames were streamed").toBeGreaterThan(0);
  expect(maxVoiced, "the progress counted the voiced character").toBeGreaterThanOrEqual(1);

  expect(
    consoleErrors,
    `the browser saw faults it did not cause: ${consoleErrors.join(" | ")}`,
  ).toEqual([]);
  expectDefaultDataDirUntouched(serverRun("setup-board"));
});

// ── (b) prefers-reduced-motion ──────────────────────────────────────────────

test("setup board: prefers-reduced-motion settles the glide instantly", async ({
  browser,
}, testInfo) => {
  test.setTimeout(240_000);
  test.skip(
    testInfo.project.name !== "setup-board",
    "runs on the setup-board project's fresh appliance",
  );
  expect(loadSharedRunId(), "the reduced-motion test deep-links the walk's run").not.toBe("");

  const context = await browser.newContext({ reducedMotion: "reduce" });
  const page = await context.newPage();
  try {
    await signIn(page);
    await page.goto(`/games/new?run=${sharedRunId}`);

    // The deep link rebuilds the board from the SNAPSHOT (the stream is the
    // live half; a fresh load starts from the durable rows) — Marla is where
    // the walk left her.
    const marla = boardCard(page, "Marla");
    await expect(marla, "the snapshot rebuilt the board").toBeVisible({ timeout: WAIT_MS });
    await expect(marla).toHaveAttribute("data-stage", "done");

    // The design's accessibility rule: under reduced motion the card's glide
    // settles instantly — the stylesheet's universal override (tokens.css)
    // wins over the 1.1s transition. Chromium normalises the computed value
    // (0.01ms answers as 1e-05s), so the assertion compares durations, not
    // spellings.
    const duration = await marla.evaluate((el) => {
      const g = globalThis as unknown as {
        getComputedStyle: (el: unknown) => { transitionDuration: string };
      };
      const value = g.getComputedStyle(el).transitionDuration;
      // Computed durations are comma lists; the card's single transition is
      // the first entry. "0s" / "1e-05s" / "0.01ms" are all instant.
      const first = value.split(",")[0].trim();
      const match = /^([\d.eE+-]+)(ms|s)$/.exec(first);
      if (match === null) return Number.NaN;
      const amount = Number.parseFloat(match[1]);
      return match[2] === "s" ? amount * 1000 : amount;
    });
    expect(
      Number.isFinite(duration) && duration <= 0.01,
      `the card's transition settles instantly under prefers-reduced-motion (computed: ${duration}ms)`,
    ).toBe(true);
  } finally {
    await context.close();
  }
});

// ── (c) the three design widths ─────────────────────────────────────────────

/** WCAG 2.1 A/AA: the 2.0 A/AA rules plus the rules 2.1 adds at those levels. */
const WCAG_21_A_AA = ["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"] as const;

test("setup board: screenshots and axe at the three design widths", async ({ page }, testInfo) => {
  test.setTimeout(300_000);
  test.skip(
    testInfo.project.name !== "setup-board",
    "runs on the setup-board project's fresh appliance",
  );
  expect(loadSharedRunId(), "the width test deep-links the walk's run").not.toBe("");
  const consoleErrors = trackConsoleErrors(page, [401]);

  await signIn(page);

  const reports: { width: number; serious: string[] }[] = [];
  for (const width of [1440, 1024, 390]) {
    await page.setViewportSize({ width, height: 900 });
    await page.goto(`/games/new?run=${sharedRunId}`);
    // The board's content settled, not just the chrome: the run's header,
    // the board and Marla's settled card.
    await expect(page.getByTestId("setup-title"), "the board's header").toBeVisible();
    await expect(page.getByTestId("setup-board"), "the live board").toBeVisible({
      timeout: WAIT_MS,
    });
    await expect(boardCard(page, "Marla")).toHaveAttribute("data-stage", "done");
    // Settle the fonts: the self-hosted faces swap in asynchronously
    // (font-display: swap), and a shot taken before the swap reads the
    // fallback layout, not the design's.
    await page.evaluate(async () => {
      const g = globalThis as unknown as {
        document?: { fonts?: { ready?: Promise<unknown> } };
      };
      await g.document?.fonts?.ready;
    });
    // Phone width: no horizontal page scroll — the rail wraps, the agent
    // panel stacks under the board and the board's cards shrink, so the
    // document must never be wider than the viewport.
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
        `the setup board @390px scrolls horizontally: documentElement.scrollWidth ${widths.scrollWidth} > innerWidth ${widths.innerWidth}`,
      ).toBeLessThanOrEqual(widths.innerWidth);
    }
    const shot = `setup-board-${width}.png`;
    const shotPath = `${testInfo.outputDir}/${shot}`;
    await page.screenshot({ path: shotPath, fullPage: true });
    await testInfo.attach(shot, { body: readFileSync(shotPath), contentType: "image/png" });

    const results = await new AxeBuilder({ page }).withTags([...WCAG_21_A_AA]).analyze();
    const violations = results.violations ?? [];
    reports.push({
      width,
      serious: violations
        .filter((violation) => violation.impact === "serious" || violation.impact === "critical")
        .map(
          (violation) =>
            `${violation.id} [${violation.impact}] ×${violation.nodes.length} — ${violation.help}`,
        ),
    });
  }

  const failing = reports.filter((report) => report.serious.length > 0);
  expect(
    failing,
    `serious/critical axe violations on the setup board:\n${failing
      .map((report) => `— @${report.width}px:\n${report.serious.join("\n")}`)
      .join("\n\n")}`,
  ).toEqual([]);

  expect(
    consoleErrors,
    `the browser saw faults it did not cause: ${consoleErrors.join(" | ")}`,
  ).toEqual([]);
  expectDefaultDataDirUntouched(serverRun("setup-board"));
});
