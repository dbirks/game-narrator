import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { randomUUID } from "node:crypto";

import AxeBuilder from "@axe-core/playwright";
import { expect, test } from "@playwright/test";

import { trackConsoleErrors } from "./support/console";
import { buildManifest, type SeedProfile } from "./support/manifest";
import { expectDefaultDataDirUntouched, serverRun } from "./support/servers";

/**
 * The Plum & Peach Play page (docs/design/plum-peach 2e/2f/2g, ADR-0093):
 * one replay session with three views over it — now-speaking (default),
 * the operator "show workings" (frame + event stream + candidates +
 * corrections) and the kid mode (`?mode=kid`) — driven on the packaged
 * binary with the repo's testdata on the replay allowlist, the scripted
 * TTS roles (the audio pipeline is wired) and the labelled-synthetic
 * capture seam (GAME_NARRATOR_CAPTURE_FAKE) standing in for the HDMI
 * dongle, so the operator frame's signal/preview half runs the
 * production consumer without hardware (ADR-0078's seam; ADR-0110's
 * one-shot path answers a replay session).
 *
 * Acceptances proven here (plan.md Phase 1 item 6):
 *   - the operator's corrections (Wrong speaker · Wrong text) record the
 *     ADR-0052 label: the 201 is a proposed correction keyed to the line's
 *     message row, never a fix;
 *   - toggling the views neither restarts nor duplicates speech: the
 *     speech counters and the speech.started rows are stable across the
 *     toggle and no line speaks twice;
 *   - the operator frame asserts its signal state by data-testid (UX-11)
 *     and holds the layout at 1024/390 with no horizontal scroll;
 *   - the caption view is the clean 2e surface (the 2026-10-20 review,
 *     ADR-0093): portrait, name tab, line, waveform, last three lines and
 *     the bottom control bar — the session stats block and the audio
 *     output card live in the operator view only; the candidate rows name
 *     the line (snippet + bar + value), the store id stays a tooltip;
 *   - the kid mode renders −/pause/+ and nothing else (UX-10);
 *   - axe stays at zero serious/critical at all three widths.
 *
 * The clip is short (back-to-back commits, source_end seconds later under a
 * fast engine), so the spec waits the session out and then drives the
 * corrections against the settled run: the label is a curation act the API
 * owns — POST /corrections is session-agnostic (the label targets the
 * line's message row, and UX-05 reaches labels "from Play or a trace",
 * settled runs included) — so recording it after the run settles proves the
 * same contract without racing source_end. Everything durable (the no-dup
 * check, the widths and axe) runs on the settled session. The speech half
 * is a synthetic-stub pipeline (scripted TTS, no approved narrator: the
 * utterances end skipped, exactly as m5-part1 walks) — labelled as such,
 * never presented as a real voice (AGENTS.md rule 3).
 */

const EDITION = {
  slug: "play-pc-harbour",
  title: "Harbour Light",
  region: "us",
  language: "en",
  revision: "rev0",
};
const ADMIN = { username: "play-pc-admin", password: "steady as the tide" };
const REPLAY_DIR = "testdata/synthetic/replay/dialogue-advance-001";
const WIDTHS = [1440, 1024, 390] as const;
const WCAG_21_A_AA = ["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"];

function seedWithRoster(): SeedProfile {
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
    {
      id: "marina",
      name: "Marina",
      aliases: ["the keeper's daughter"],
      role: "protagonist",
      summary: "Quiet, watchful; keeps her father's log.",
      status: "confirmed",
      verified_source_refs: ["synthetic:harbour-log"],
    },
    {
      id: "keeper",
      name: "Keeper",
      aliases: ["the lighthouse keeper"],
      role: "supporting",
      status: "proposed",
    },
  ];
  return seed;
}

test("play (ADR-0093): now-speaking + operator workings + kid mode over one session", async ({
  page,
  request,
  baseURL,
}, testInfo) => {
  test.setTimeout(900_000);
  test.skip(testInfo.project.name !== "play-pc", "runs on the play-pc project's own server");
  const run = serverRun("play-pc");
  const origin = new URL(baseURL as string).origin;
  // 401: the guard's first read before the login lands; 429: the one-shot
  // capture preview's rate limit (the hardware-free seam's honest
  // backpressure — the frame view keeps its last state on a 429);
  // 503: the model-role health probe's not-configured answer.
  const consoleErrors = trackConsoleErrors(page, [401, 409, 429, 503]);
  const shot = (step: string) =>
    page.screenshot({ path: `${testInfo.outputDir}/play-pc-${step}.png`, fullPage: true });

  // ── 1. claim the appliance (the API's own context) and sign in (the
  //      browser's) — two contexts, both authenticated from their own side ─
  const setup = await request.post(`${origin}/api/v1/auth/setup`, {
    data: { username: ADMIN.username, password: ADMIN.password },
  });
  expect(setup.status(), `auth setup: ${await setup.text()}`).toBe(201);
  await request.post(`${origin}/api/v1/auth/login`, {
    data: { username: ADMIN.username, password: ADMIN.password },
  });

  const post = (path: string, data?: unknown, headers: Record<string, string> = {}) =>
    request.post(`${origin}${path}`, {
      data,
      headers: {
        ...(data === undefined ? {} : { "content-type": "application/json" }),
        ...headers,
      },
    });
  const csrf = async () =>
    (await request.cookies(origin)).find((cookie) => cookie.name === "gn_csrf")?.value ?? "";
  const etagOf = async (id: string) => {
    const response = await request.get(`${origin}/api/v1/profiles/${id}`);
    expect(response.status(), "a profile read carries an ETag").toBe(200);
    return response.headers()["etag"] as string;
  };

  // ── 2. seed + publish the synthetic profile through the API ────────────
  const game = await post(
    "/api/v1/games",
    { slug: EDITION.slug, title: EDITION.title },
    {
      "idempotency-key": randomUUID(),
      "x-csrf-token": await csrf(),
    },
  );
  expect(game.status(), `create game: ${await game.text()}`).toBe(201);
  const gameId = (await game.json()) as { id: string };
  const edition = await post(
    `/api/v1/games/${gameId.id}/editions`,
    { region: EDITION.region, language: EDITION.language, revision: EDITION.revision },
    { "idempotency-key": randomUUID(), "x-csrf-token": await csrf() },
  );
  expect(edition.status(), `create edition: ${await edition.text()}`).toBe(201);
  const editionId = (await edition.json()) as { id: string };
  const { doc } = buildManifest(seedWithRoster(), { edition: EDITION });
  const imported = await post(`/api/v1/editions/${editionId.id}/profiles/import-manifest`, doc, {
    "x-csrf-token": await csrf(),
  });
  expect(imported.status(), `import-manifest: ${await imported.text()}`).toBe(201);
  const { profile } = (await imported.json()) as { profile: { id: string } };
  await post(
    `/api/v1/profiles/${profile.id}/status`,
    { to: "review" },
    { "if-match": await etagOf(profile.id), "x-csrf-token": await csrf() },
  );
  const published = await post(`/api/v1/profiles/${profile.id}/publish`, undefined, {
    "if-match": await etagOf(profile.id),
    "x-csrf-token": await csrf(),
  });
  expect(published.status(), `publish: ${await published.text()}`).toBe(200);
  const r1 = profile.id;

  const messagesPage = (await (
    await request.get(`${origin}/api/v1/profiles/${r1}/messages?limit=200`)
  ).json()) as { messages: { id: string; source_message_id?: string }[] };
  const storeIds: Record<string, string> = {};
  for (const row of messagesPage.messages ?? []) {
    if (row.source_message_id) storeIds[row.source_message_id] = row.id;
  }
  expect(storeIds["demo-page-1"], "demo-page-1 is a stored message").toBeTruthy();
  expect(storeIds["demo-page-2"], "demo-page-2 is a stored message").toBeTruthy();

  // ── 3. start the replay session (the browser signs in through the page) ─
  await page.goto("/play");
  await expect(
    page.getByTestId("login-username"),
    "the unauthenticated guard redirects",
  ).toBeVisible({ timeout: 20_000 });
  await page.getByTestId("login-username").fill(ADMIN.username);
  await page.getByTestId("login-password").fill(ADMIN.password);
  await page.getByTestId("login-submit").click();
  const profileSelect = page.getByTestId("play-profile");
  await expect(profileSelect, "the session-start form").toBeVisible({ timeout: 20_000 });
  await profileSelect.selectOption(r1, { timeout: 20_000 });
  await expect(profileSelect).toHaveValue(r1);
  await expect(page.getByTestId("play-replay-dir")).toHaveValue(REPLAY_DIR);
  await page.getByTestId("play-start").click();
  await expect(page).toHaveURL(/\/play\?session=/);
  const sessionId = new URL(page.url()).searchParams.get("session") as string;
  expect(sessionId).toBeTruthy();

  // The watch is attached and the badge shows the session's state. The clip
  // is short — under a fast engine source_end lands seconds after the start
  // — so the spec waits the run out and reads the settled truth; the run's
  // own lifecycle (it started, it committed, it stopped) is then asserted
  // from the operator table's event rows, where the replay of the whole run
  // makes it durable.
  await expect(page.getByTestId("play-state")).toHaveText(/^(starting|running|stopped)$/, {
    timeout: 30_000,
  });
  await expect(page.getByTestId("play-state")).toHaveText("stopped", { timeout: 180_000 });

  // The default view is the clean caption surface (ADR-0093, the 2026-10-20
  // review against mock 2e): portrait, name tab, line, waveform, last
  // three lines and the bottom control bar — nothing else. The session's
  // stats block and the audio output card belong to the operator view.
  await expect(page.getByTestId("play-now-speaking")).toBeVisible();
  await expect(page.getByTestId("play-status")).toHaveCount(0);
  await expect(page.getByTestId("play-audio-card")).toHaveCount(0);

  // The session's facts read from the operator view ("show workings").
  await page.getByTestId("play-show-workings").click();
  await expect(page.getByTestId("play-operator")).toBeVisible();
  await expect(page.getByTestId("play-source")).toContainText("replay · stopped");
  await expect(page.getByTestId("play-source-error")).toHaveCount(0);
  await expect(page.getByTestId("play-commits")).toHaveText("2");
  await expect(page.getByTestId("play-terminal")).toBeVisible();
  await expect(page.getByTestId("play-audio-card")).toBeVisible();

  // The audio pipeline is wired (scripted TTS) but the roster has no
  // approved narrator: the narratable lines requested speech and the
  // utterances ended skipped with the reason — the counters say so
  // (docs/spec/13), exactly as m5-part1 walks. Read at rest, so no race.
  await expect(page.getByTestId("play-speech-requested")).toHaveText(/^[1-9]/);
  await expect(page.getByTestId("play-utterance-state")).toContainText("skipped");

  // ── 4. back to the caption view: the committed lines, with the roster's
  //      speaker — the run settled before the view swap, so the lines are
  //      the durable replay, not a live race ──────────────────────────────
  await page.getByTestId("play-show-workings").click(); // back to now-speaking
  await expect(page.getByTestId("play-now-speaking")).toBeVisible();
  const committedLines = page.locator('[data-testid="recent-lines"] li');
  const firstLine = committedLines.filter({ hasText: storeIds["demo-page-1"] });
  await expect
    .poll(() => firstLine.count(), { timeout: 120_000, intervals: [250] })
    .toBeGreaterThanOrEqual(1);
  await expect(page.getByTestId("current-line-speaker")).toHaveText("Marina");

  // Both commits, the newest on top.
  await expect
    .poll(() => committedLines.count(), { timeout: 120_000, intervals: [500] })
    .toBeGreaterThanOrEqual(2);
  await expect(page.getByTestId("current-line-message-id")).toHaveText(storeIds["demo-page-2"]);

  // ── 5. the corrections record the label (ADR-0052) — UX-05 reaches
  //      labels "from Play or a trace", so the settled run is a legitimate
  //      target: Wrong speaker? from the now-speaking bar, then Wrong text
  //      in the operator view; both fire POST /api/v1/corrections and the
  //      201 is a proposed correction keyed to the line's message row ──
  const speakerCorrection = page.waitForResponse(
    (r) => r.request().method() === "POST" && new URL(r.url()).pathname.endsWith("/corrections"),
    { timeout: 20_000 },
  );
  await page.getByTestId("play-correct-speaker").click();
  const speakerRes = await speakerCorrection;
  expect(speakerRes.status(), `wrong-speaker: ${await speakerRes.text()}`).toBe(201);

  const textCorrection = page.waitForResponse(
    (r) => r.request().method() === "POST" && new URL(r.url()).pathname.endsWith("/corrections"),
    { timeout: 20_000 },
  );
  await page.getByTestId("play-show-workings").click();
  await page.getByTestId("play-correct-text").click();
  const textRes = await textCorrection;
  expect(textRes.status(), `wrong-text: ${await textRes.text()}`).toBe(201);
  await expect(page.getByTestId("play-correction-result")).toBeVisible({ timeout: 10_000 });

  // The label is a proposal in the store, keyed to the line's message row
  // (the session profile's stored message id, not the clip's). The current
  // line at the click is one of the clip's two demo lines (the label names
  // whatever was on screen, which is the contract).
  const correctionBody = (await speakerRes.json()) as {
    correction: {
      kind: string;
      target_type: string;
      target_id: string;
      status: string;
      profile_id: string;
    };
  };
  expect(correctionBody.correction.kind).toBe("speaker");
  expect(
    [storeIds["demo-page-1"], storeIds["demo-page-2"]],
    "the label names the line's stored message row",
  ).toContain(correctionBody.correction.target_id);
  expect(correctionBody.correction.status).toBe("proposed");
  expect(correctionBody.correction.profile_id).toBe(r1);

  // ── 6. the operator view: the committed line, the frame, the signal
  //      state, the events and the candidates ────────────────────────────
  await expect(page.getByTestId("play-operator")).toBeVisible();
  await expect(page.getByTestId("play-show-workings")).toHaveAttribute("aria-pressed", "true");

  // The committed line beneath the frame (name tab + line + message id):
  // whichever of the clip's two demo lines is current names its stored row.
  const opLineText = await page.getByTestId("play-operator-line").innerText();
  expect(
    [storeIds["demo-page-1"], storeIds["demo-page-2"]].some((id) => opLineText.includes(id)),
    `the committed line names its stored message row (got: ${opLineText})`,
  ).toBe(true);
  await expect(page.getByTestId("play-operator-line-speaker")).toHaveText("Marina");

  // The one-shot preview answers for the replay session (ADR-0110 §2): the
  // labelled-synthetic seam's testsrc2 frame, with the name-plate ROI.
  await expect(page.getByTestId("play-frame-image")).toBeVisible({ timeout: 60_000 });
  await expect(page.getByTestId("play-roi-nameplate")).toBeVisible();

  // UX-11: the signal state, asserted by its testid — the seam's probe finds
  // the device, so the signal is present; the active-picture judgment is the
  // machine's own (the ActiveArea scan looks for a bar-framed picture, and
  // the seam's testsrc2 pattern is not one — the text is the machine's, not
  // a decoration, so the spec accepts the judgment it answers with).
  await expect(page.getByTestId("play-signal-state")).toHaveText("signal present", {
    timeout: 60_000,
  });
  await expect(page.getByTestId("play-picture-state")).toHaveText(
    /^(active picture|no active picture)$/,
  );
  await expect(page.getByTestId("play-fps")).toBeVisible();
  // The seam's honesty labels (AGENTS.md rule 3): the frame is synthetic,
  // and the answer came from the one-shot path (no v4l2 session holds the
  // seam's device — the replay session's source is the clip).
  await expect(page.getByTestId("play-frame-synthetic")).toBeVisible();
  await expect(page.getByTestId("play-frame-source")).toHaveText("oneshot");

  // The event stream table: the run's own events, newest first — and the
  // run's lifecycle is in it: the hub's stop row and the committed lines'
  // rows (the replay of the whole run is what a fresh load of a settled
  // session gets; the start row is the run's head, replayed the same way).
  await expect(page.getByTestId("play-events")).toBeVisible({ timeout: 30_000 });
  await expect(page.locator('[data-testid^="play-event-row-"]').first()).toBeVisible();
  await expect(
    page.locator('[data-testid="play-events"] .pp-events-name', { hasText: "session.stopped" }),
  ).toBeVisible();
  await expect
    .poll(() =>
      page
        .locator('[data-testid="play-events"] .pp-events-name', { hasText: "message.committed" })
        .count(),
    )
    .toBeGreaterThanOrEqual(2);
  // The candidates card answers with the matcher's verdict (the corpus
  // match's ranking or the empty state — both are honest; assert it shows).
  await expect(page.getByTestId("play-candidates")).toBeVisible();
  // The candidate rows name the line, not its store ULID (the 2026-10-20
  // review): each row carries the text snippet, the probability bar and the
  // value; the message id lives in the row's title/tooltip. A ranked row is
  // one whose testid is the bare message id (the bar's svg and the snippet's
  // span carry suffixed ids).
  const barIds = await page
    .locator('[data-testid^="play-candidate-bar-"]')
    .evaluateAll((bars) =>
      bars.map((bar) =>
        (bar.getAttribute("data-testid") ?? "").slice("play-candidate-bar-".length),
      ),
    );
  if (barIds.length > 0) {
    const row = page.locator(`[data-testid="play-candidate-${barIds[0]}"]`);
    await expect(row).toBeVisible();
    // The message id lives in the row's title/tooltip, not its visible text.
    expect(await row.getAttribute("title"), "the row's tooltip carries the id").toBe(barIds[0]);
    // The visible row text is the snippet + the value, never the bare id:
    // it carries the probability's percent and not the 26-character id.
    const rowText = ((await row.innerText()) ?? "").replace(/\n/g, " ");
    expect(rowText, `candidate row names the line: ${rowText}`).toContain("%");
    expect(rowText, "the raw ULID is not the row's visible lead").not.toContain(barIds[0]);
  }
  await shot("02-operator");

  // ── 7. the view toggle: the same session, the same lines — the swap is a
  //      view change, not a stream restart ────────────────────────────────
  await page.getByTestId("play-show-workings").click(); // back to now-speaking
  await expect(page.getByTestId("play-now-speaking")).toBeVisible();
  await expect(
    page.getByTestId("current-line-speaker"),
    "the current line is untouched by the view swap",
  ).toHaveText("Marina");
  await shot("03-now-speaking");

  // ── 8. the kid mode: −/pause/+ and nothing else (UX-10) — the minimal
  //      view over the session, whatever its state (the view renders the
  //      session's truth: a live pause, or the settled lines with the
  //      control honestly inert) ─────────────────────────────────────────
  await page.goto(`/play?session=${encodeURIComponent(sessionId)}&mode=kid`);
  await expect(page.getByTestId("play-kid-mode")).toBeVisible({ timeout: 20_000 });
  await expect(page.getByTestId("play-kid-name")).toHaveText("Marina");
  await expect(page.getByTestId("play-kid-line")).toBeVisible();
  const kidControls = page.getByTestId("play-kid-controls");
  await expect(kidControls.getByRole("button")).toHaveCount(3, { timeout: 10_000 });
  // No other interactive control lives inside the mode (the shell chrome —
  // the status bar and the ⌘J pill — is the shell's on every route, not the
  // mode's; the acceptance's "no other interactive controls" scopes the
  // mode's own view).
  expect(
    await kidControls.locator("input, select, a, [role='switch'], [role='tab']").count(),
    "the kid mode renders −/pause/+ and nothing else",
  ).toBe(0);
  await shot("04-kid");

  // Kid holds the review's widths too (1440 · 1024 · 390): one full-page shot
  // and an axe pass per width over the same minimal view, and the document
  // never widens past the viewport.
  for (const width of WIDTHS) {
    await page.setViewportSize({ width, height: 900 });
    const kidDims = await page.evaluate(() => {
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
      kidDims.scrollWidth,
      `/play (kid) @${width}px scrolls horizontally: scrollWidth ${kidDims.scrollWidth} > innerWidth ${kidDims.innerWidth}`,
    ).toBeLessThanOrEqual(kidDims.innerWidth);
    const kidShot = `${testInfo.outputDir}/play-pc-kid-${width}.png`;
    await page.screenshot({ path: kidShot, fullPage: true });
    await testInfo.attach(`kid@${width}`, {
      body: readFileSync(kidShot),
      contentType: "image/png",
    });
    const kidAxe = await new AxeBuilder({ page }).withTags([...WCAG_21_A_AA]).analyze();
    const kidBad = (kidAxe.violations ?? []).filter(
      (v) => v.impact === "serious" || v.impact === "critical",
    );
    expect(
      kidBad,
      `axe serious/critical on /play kid @${width}: ${kidBad
        .map((v) => `${v.id} ×${v.nodes.length}`)
        .join(", ")}`,
    ).toEqual([]);
  }

  // ── 9. the no-duplicate-speech acceptance, settled: the counters are the
  //      session's own snapshot (the one pipeline the page renders), and
  //      the speech.started rows are the operator table's count of the run
  //      — toggling the views on the settled run restarts nothing, so both
  //      are stable across the toggle and no line speaks twice ───────────
  await page.goto(`/play?session=${encodeURIComponent(sessionId)}`); // back to the default view
  const speechOf = async () =>
    (
      (await (await request.get(`${origin}/api/v1/sessions/${sessionId}`)).json()) as {
        audio?: { speech: { requested: number; started: number } };
      }
    ).audio?.speech ?? { requested: 0, started: 0 };
  const speechStartedRows = () =>
    page.locator('[data-testid="play-events"] tbody td.pp-events-name', {
      hasText: "speech.started",
    });
  const settled = await speechOf();
  const startedBefore = await (async () => {
    await page.getByTestId("play-show-workings").click(); // operator view (the run's full event replay)
    await expect(page.getByTestId("play-operator")).toBeVisible();
    await page.waitForTimeout(2_000);
    const rows = await speechStartedRows().count();
    await page.getByTestId("play-show-workings").click(); // now-speaking
    await expect(page.getByTestId("play-now-speaking")).toBeVisible();
    return rows;
  })();
  expect(startedBefore, "no line speaks more than once").toBeLessThanOrEqual(2);

  const settledAfter = await speechOf();
  const startedAfter = await (async () => {
    await page.getByTestId("play-show-workings").click();
    await expect(page.getByTestId("play-operator")).toBeVisible();
    await page.waitForTimeout(2_000);
    const rows = await speechStartedRows().count();
    await page.getByTestId("play-show-workings").click();
    await expect(page.getByTestId("play-now-speaking")).toBeVisible();
    return rows;
  })();
  expect(settledAfter.requested, "toggling the settled run requests no extra speech").toBe(
    settled.requested,
  );
  expect(settledAfter.started, "toggling the settled run starts no extra speech").toBe(
    settled.started,
  );
  expect(startedAfter, "toggling the settled run's views emits no duplicate speech.started").toBe(
    startedBefore,
  );
  await shot("05-settled");

  // ── 10. the widths and axe hold (the run is settled; Stop is m5's walk) ─
  for (const width of WIDTHS) {
    await page.setViewportSize({ width, height: 900 });
    await page.evaluate(async () => {
      const g = globalThis as unknown as {
        document?: { fonts?: { ready?: Promise<unknown> } };
      };
      await g.document?.fonts?.ready;
    });
    // No horizontal page scroll at any width (the frame and the tables
    // scroll inside their own wrappers; the document never widens).
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
      `/play (now-speaking) @${width}px scrolls horizontally: scrollWidth ${dims.scrollWidth} > innerWidth ${dims.innerWidth}`,
    ).toBeLessThanOrEqual(dims.innerWidth);
    // The control bar is the page's bottom strip (the 2026-10-20 review):
    // a horizontal bar along the bottom, not a vertical card under the
    // portrait — its box spans the panel's content width and its top edge
    // sits below the portrait's bottom at every width (at 390 it stacks
    // its slices, but still under the whole caption surface).
    const barBox = await page.getByTestId("play-session-controls").boundingBox();
    const panelBox = await page.getByTestId("play-live").boundingBox();
    const portraitBox = await page.getByTestId("play-portrait").boundingBox();
    if (barBox === null || panelBox === null || portraitBox === null) {
      throw new Error(`control bar / panel / portrait missing @${width}`);
    }
    expect(
      barBox.width,
      `the bar is a horizontal strip @${width}: width ${barBox.width} < panel ${panelBox.width}`,
    ).toBeGreaterThanOrEqual(panelBox.width - 40);
    expect(
      barBox.y,
      `the bar sits below the portrait @${width}: bar top ${barBox.y} <= portrait bottom ${portraitBox.y + portraitBox.height}`,
    ).toBeGreaterThanOrEqual(portraitBox.y + portraitBox.height);
    const nowShot = `${testInfo.outputDir}/play-pc-now-${width}.png`;
    await page.screenshot({ path: nowShot, fullPage: true });
    await testInfo.attach(`now-speaking@${width}`, {
      body: readFileSync(nowShot),
      contentType: "image/png",
    });
    const nowAxe = await new AxeBuilder({ page }).withTags([...WCAG_21_A_AA]).analyze();
    const nowBad = (nowAxe.violations ?? []).filter(
      (v) => v.impact === "serious" || v.impact === "critical",
    );
    expect(
      nowBad,
      `axe serious/critical on /play now-speaking @${width}: ${nowBad
        .map((v) => `${v.id} ×${v.nodes.length}`)
        .join(", ")}`,
    ).toEqual([]);
  }

  // The operator view holds at all three review widths (1440 · 1024 · 390):
  // the acceptance's widths plus the desktop capture.
  await page.getByTestId("play-show-workings").click();
  for (const width of WIDTHS) {
    await page.setViewportSize({ width, height: 900 });
    await expect(page.getByTestId("play-operator")).toBeVisible();
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
      `/play (operator) @${width}px scrolls horizontally: scrollWidth ${dims.scrollWidth} > innerWidth ${dims.innerWidth}`,
    ).toBeLessThanOrEqual(dims.innerWidth);
    // The bar follows the operator view too (one bar, both views): still a
    // horizontal strip along the bottom, under the frame.
    const opBarBox = await page.getByTestId("play-session-controls").boundingBox();
    const frameBox = await page.getByTestId("play-operator-frame").boundingBox();
    if (opBarBox === null || frameBox === null) {
      throw new Error(`control bar / frame missing in the operator view @${width}`);
    }
    expect(
      opBarBox.y,
      `the bar sits below the frame @${width}: bar top ${opBarBox.y} < frame bottom ${frameBox.y + frameBox.height}`,
    ).toBeGreaterThanOrEqual(frameBox.y + frameBox.height);
    const opShot = `${testInfo.outputDir}/play-pc-operator-${width}.png`;
    await page.screenshot({ path: opShot, fullPage: true });
    await testInfo.attach(`operator@${width}`, {
      body: readFileSync(opShot),
      contentType: "image/png",
    });
    const opAxe = await new AxeBuilder({ page }).withTags([...WCAG_21_A_AA]).analyze();
    const opBad = (opAxe.violations ?? []).filter(
      (v) => v.impact === "serious" || v.impact === "critical",
    );
    expect(
      opBad,
      `axe serious/critical on /play operator @${width}: ${opBad
        .map((v) => `${v.id} ×${v.nodes.length}`)
        .join(", ")}`,
    ).toEqual([]);
  }

  // The console stayed clean (the ignore list above is the documented noise).
  expect(consoleErrors).toEqual([]);
  expectDefaultDataDirUntouched(run);
});
