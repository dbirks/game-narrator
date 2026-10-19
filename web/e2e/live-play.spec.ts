import { expect, test } from "@playwright/test";
import { mkdirSync } from "node:fs";
import { join } from "node:path";

import { trackConsoleErrors } from "./support/console";

/**
 * The live play path through the real UI (task browser-live-play, AGENTS.md
 * rule 3 class (d)+(e)): a real Dolphin emulator on a real disc image, the
 * real gateway for OCR/Clef/TTS, driven through the packaged pages exactly
 * as a developer would — start the session from /play with the Dolphin
 * frame source, watch it speak, then open the developer inspector and read
 * the correlated trace.
 *
 * The server is the Go orchestrator's in-process API + embedded SPA
 * (internal/e2e TestLivePlayUI) — the only process that can wire the dev
 * Dolphin bridge into the runtime (the production binary answers 501/503
 * for it, internal/api/dolphin.go). The Go test passes the origin, the
 * login credentials and the evidence bundle dir as GN_LIVE_UI_*; without
 * them this spec is NOT RUN, which is how `make test-ui` and CI skip it.
 * The Go side owns the emulator drive to the opening legend; this spec
 * owns the browser half and stops the session from the Play page.
 *
 * Log-safe per AGENTS.md rule 5: the spec asserts on structure, ids and
 * counters — never on dialogue text. Screenshots land in the gitignored
 * bundle dir, and the Playwright trace is captured by the runner config.
 */

const url = process.env.GN_LIVE_UI_URL ?? "";
const admin = process.env.GN_LIVE_UI_ADMIN ?? "";
const password = process.env.GN_LIVE_UI_PASS ?? "";
const bundle = process.env.GN_LIVE_UI_BUNDLE ?? "";

// The UI stage labels of the directed chain (src/pages/developer/decision.ts
// CHAIN_STAGES): frame → OCR → candidates → Clef decision → commit → speech.
// frame.observed is not an emitted event name in the live pipeline — the
// frame half of the chain is the pinned observation's stored frame and ROI
// crop, asserted as the frame-image/crop-image pair, while the trace rows
// carry the OCR → candidates → Clef → commit → speech half.
// The stable ordered milestones of one commit's trace. The Clef stages are NOT
// in this ordered chain: the commit may run on the local evidence before the
// decision resolves (the degraded path, decision.ts), so "Clef decision" can
// land AFTER "commit", and "Clef request" (decision.requested) is not always a
// rendered trace row. The decision provider's correlation is asserted
// separately (the decision row's request id, below); here we only require the
// milestones that are always in this order and that a truncated chain would
// drop: OCR -> candidates -> commit -> speech end (the terminal speech stage,
// so a chain that stops early cannot pass). A live legend commits a line per
// page and a newer commit fences the older line's utterance (docs/spec/07),
// so the walk selects the commit whose trace actually carries this full
// chain — the oldest commit's may legitimately end at "speech cancelled".
const TRACE_CHAIN = ["OCR", "candidates", "commit", "speech end"] as const;

test("live play: dolphin frames through the real UI to speech and the inspector chain", async ({
  page,
}, testInfo) => {
  test.skip(
    testInfo.project.name !== "live-play" || url === "" || admin === "" || password === "",
    "NOT RUN: the live play path needs the Go orchestrator's env (GN_LIVE_UI_URL/ADMIN/PASS) — " +
      "run it via make test-dolphin (internal/e2e TestLivePlayUI), not the plain UI suite",
  );
  test.setTimeout(45 * 60 * 1000);

  mkdirSync(bundle, { recursive: true });
  const consoleErrors = trackConsoleErrors(
    page,
    [
      // 401: the unauthenticated probe the auth context makes before signing in.
      401, 503,
    ],
  );
  const shot = (step: string) =>
    page.screenshot({ path: join(bundle, `live-play-${step}.png`), fullPage: true });
  const counter = async (testId: string) =>
    Number(
      await page
        .getByTestId(testId)
        .innerText()
        .then((t) => t.trim().replace(/[^0-9]/g, "")),
    );

  // ── 2. /play: sign in (the orchestrator claimed the appliance + profile) ─
  // The session cookie is HttpOnly and minted by the login the SPA itself
  // performs, so the browser signs in through the login form — the same
  // pattern the other UI walks use — rather than through a separate
  // request context whose cookie jar Chromium does not share.
  await page.goto("/play");
  await expect(page).toHaveURL(/\/login/);
  await page.getByTestId("login-username").fill(admin);
  await page.getByTestId("login-password").fill(password);
  await page.getByTestId("login-submit").click();
  await expect(page).toHaveURL(/\/play$/);
  await expect(page.getByTestId("play-setup")).toBeVisible();
  // The orchestrator published exactly one revision; the select's first real
  // option is it (index 0 is the placeholder).
  await page.getByTestId("play-profile").selectOption({ index: 1 });
  await page.getByTestId("play-source-kind").selectOption("dolphin");
  await page.getByTestId("play-mode").selectOption("conservative");
  // Developer trace on: the inspector's frame/crop evidence needs it, and
  // the signed-in admin holds the developer role the flag requires.
  await page.getByTestId("play-dev-trace").check();
  await shot("01-play-setup");
  await page.getByTestId("play-start").click();

  // ── 3. the session goes live in the player view ────────────────────────
  await expect(page.getByTestId("play-state")).toHaveText("running", {
    timeout: 5 * 60 * 1000,
  });
  const sessionId = (await page.getByTestId("play-session-id").innerText()).trim();
  expect(sessionId.length, "session id on the player view").toBeGreaterThan(0);
  await shot("02-play-live");

  // ── 4. a committed line appears in the player view ─────────────────────
  // The Go side drives the emulator to the opening legend in parallel; the
  // wait here is the drive's wall clock (minutes), not a flake budget.
  await expect(page.getByTestId("current-line-text")).not.toBeEmpty({
    timeout: 25 * 60 * 1000,
  });
  await shot("03-play-committed");

  // ── 5. the audio indicator: speech started, then ended ─────────────────
  await expect
    .poll(async () => await counter("play-speech-started"), {
      timeout: 15 * 60 * 1000,
      message: "the audio counter shows the utterance started",
    })
    .toBeGreaterThanOrEqual(1);
  await expect
    .poll(async () => await counter("play-speech-ended"), {
      timeout: 15 * 60 * 1000,
      message: "the audio counter shows the utterance ended",
    })
    .toBeGreaterThanOrEqual(1);
  await shot("04-play-speech-ended");

  // ── 6. the developer inspector for that session: the trace row chain ───
  await page.goto(`/developer?session=${encodeURIComponent(sessionId)}`);
  await expect(page.getByTestId("event-timeline")).toBeVisible();

  // The session started before this page opened, so the commit sits in the
  // durable history: page it in with "Load older" (and let the live stream
  // top up the window) until the commit row is visible.
  const rows = page.locator("tr[data-testid^='event-row-']");
  const commitRow = page
    .locator("tr[data-testid^='event-row-']")
    .filter({ has: page.locator("td", { hasText: "message.committed" }) });
  // Poll until the commit row is in the timeline. The session started before
  // this page opened, so the row may sit in the durable history: while the
  // pager is enabled (it disables at the log's start) click it to page the
  // older history into the window between checks.
  await expect
    .poll(
      async () => {
        const count = await rows.count();
        for (let i = 0; i < count; i++) {
          const name = await rows.nth(i).locator("td[data-testid^='event-name-']").innerText();
          if (name === "message.committed") return i;
        }
        const older = page.getByTestId("load-older");
        if (await older.isEnabled()) await older.click();
        return -1;
      },
      { timeout: 10 * 60 * 1000, message: "the commit row reached the timeline" },
    )
    .toBeGreaterThanOrEqual(0);
  await expect(commitRow.first()).toBeVisible();
  // The window may hold more than one commit: the game's opening legend
  // commits a line per page, and a newer commit fences the older line's
  // utterance (docs/spec/07 "stale speech after advance"), so the OLDEST
  // commit's trace can legitimately end at "speech cancelled" while a later
  // commit's ends at "speech end". Select the commit whose trace carries the
  // whole directed chain through its terminal speech stage and whose Clef
  // decision row names its provider request id — newest first, the newest
  // line is the least fenced. A truncated chain (no terminal stage) can
  // never satisfy the selection, so the proof is not weakened: the player
  // view's speech counters already proved an utterance ran to completion.
  const stagesOf = async (): Promise<string[]> => {
    const traceRows = page.locator("[data-testid^='trace-event-']");
    const n = await traceRows.count();
    const stages: string[] = [];
    for (let i = 0; i < n; i++) {
      const t = await traceRows.nth(i).innerText();
      const m = t.match(/\[([^\]]+)\]/);
      stages.push(m ? m[1] : "");
    }
    return stages;
  };
  const hasChain = (stages: string[]) => {
    let cursor = 0;
    for (const stage of stages) {
      if (stage === TRACE_CHAIN[cursor]) cursor++;
      if (cursor === TRACE_CHAIN.length) break;
    }
    return cursor === TRACE_CHAIN.length;
  };
  const commitCount = await commitRow.count();
  const tried: string[] = [];
  let chosen = false;
  for (let c = commitCount - 1; c >= 0 && !chosen; c--) {
    const row = commitRow.nth(c);
    const trace = ((await row.locator("td").nth(4).getAttribute("title")) ?? "").trim();
    if (trace.length === 0) {
      tried.push(`commit #${c}: no trace id`);
      continue;
    }
    // Pin the observation of that trace: the pinned detail shows the stored
    // frame + ROI crop (the frame half of the chain) and the trace's rows.
    const match = page
      .locator("tr[data-testid^='event-row-']")
      .filter({ has: page.locator("td", { hasText: "text.observed" }) })
      .filter({ has: page.locator("td[title=" + JSON.stringify(trace) + "]") });
    if ((await match.count()) === 0) {
      tried.push(`${trace}: no text.observed row`);
      continue;
    }
    await match.first().click();
    const heading = page.getByTestId("trace-view").locator("h3 code").first();
    try {
      await expect(heading).toHaveText(trace, { timeout: 15_000 });
    } catch {
      tried.push(`${trace}: the trace view never showed it`);
      continue;
    }
    const stages = await stagesOf();
    let requestId = "";
    const decisionReq = page.getByTestId("trace-decision-request");
    if ((await decisionReq.count()) > 0) requestId = (await decisionReq.innerText()).trim();
    if (!hasChain(stages) || requestId.length === 0) {
      tried.push(`${trace}: saw ${stages.join(" → ")}, request id ${requestId || "(none)"}`);
      continue;
    }
    // The Clef decision row shows the provider's request id (the correlation
    // proof docs/spec/12 demands of a live run).
    expect(
      requestId.length,
      `the Clef decision row names its request id (trace ${trace})`,
    ).toBeGreaterThan(0);
    chosen = true;
    // The frame half: the captured frame and the ROI crop stored for the
    // observation (dev trace on at session start).
    await expect(page.getByTestId("detail-frame")).toBeVisible();
    await expect(page.getByTestId("frame-image")).toBeVisible();
    await expect(page.getByTestId("crop-image")).toBeVisible();
    await expect(page.getByTestId("detail-ocr")).toBeVisible();
    // The trace row chain, same trace id end to end.
    await expect(page.getByTestId("trace-view")).toBeVisible();
    const headingTrace = (await heading.innerText()).trim();
    expect(headingTrace, "the trace view shows the commit's trace id").toBe(trace);
    expect(
      hasChain(stages),
      `the trace rows carry the directed chain in order (saw: ${stages.join(" → ")})`,
    ).toBe(true);
    await shot("05-inspector-trace");
  }
  expect(
    chosen,
    "no commit's trace carried the full chain (tried: " + tried.join(" | ") + ")",
  ).toBe(true);

  // ── 7. stop the session from the Play page and let it settle ───────────
  await page.goto(`/play?session=${encodeURIComponent(sessionId)}`);
  await expect(page.getByTestId("play-state")).toBeVisible();
  await page.getByTestId("play-stop").click();
  await expect(page.getByTestId("play-state")).toHaveText(/stopped|failed/, {
    timeout: 5 * 60 * 1000,
  });
  await shot("06-play-stopped");

  expect(consoleErrors, "no unexpected console errors during the live walk").toEqual([]);
});
