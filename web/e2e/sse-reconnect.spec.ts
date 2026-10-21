import { expect, test } from "@playwright/test";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { randomUUID } from "node:crypto";

import { trackConsoleErrors } from "./support/console";
import { buildManifest, type SeedProfile } from "./support/manifest";
import { setPlayView } from "./support/play";
import { expectDefaultDataDirUntouched, serverRun } from "./support/servers";

/**
 * SSE reconnect (docs/spec/16 M5: "SSE reconnect reflects accurate state";
 * docs/spec/09 "Session survives browser refresh; SSE reconnect, event
 * replay and state reconciliation"), on the packaged binary with the repo's
 * testdata replay clip and the scripted TTS roles (synthetic stubs at the
 * model boundary only — AGENTS.md rule 3; the server, store, hub and stream
 * are the real thing):
 *
 *   1. seed + publish the synthetic profile through the API (the same
 *      dialogue-advance-001 seed the player walk uses);
 *   2. /play: start the replay session and watch the first committed line
 *      arrive over the live event stream;
 *   3. tear the stream down the way a browser does (a reload: the in-flight
 *      SSE fetch is aborted and the server-side subscription drops with it);
 *   4. the page reopens the session from its own snapshot and the new
 *      /api/v1/events request is observed carrying the session's own event
 *      cursor as Last-Event-ID — the resume contract: the hub replays from
 *      the cursor, the page never asks for the stream from zero;
 *   5. once the session settles, the page's state is diffed against the
 *      server's final snapshot: state, commits, OCR calls, and the committed
 *      lines themselves (text and message ids) — "reflects accurate state"
 *      as a diff, not an impression.
 */

const EDITION = {
  slug: "sse-harbour",
  title: "Harbour Light",
  region: "us",
  language: "en",
  revision: "rev0",
};
const ADMIN = { username: "sse-admin", password: "steady as the tide" };

/** The committed lines of the clip belong to Marina (confirmed); the
 *  distractors ride on the Keeper (unknown) — the same roster the player
 *  walk seeds, so the replay commits demo-page-1 then demo-page-2. */
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
      aliases: [],
      role: "protagonist",
      summary: "Keeps the harbour log.",
      status: "confirmed",
      verified_source_refs: ["synthetic:harbour-log"],
    },
    { id: "keeper", name: "Keeper", aliases: [], role: "supporting", status: "proposed" },
  ];
  return seed;
}

interface SessionSnapshot {
  state: string;
  reason?: string;
  event_cursor: number;
  stats: { commits: number; ocr_calls: number };
}

test("SSE reconnect resumes from the cursor and the state matches the server", async ({
  page,
  request,
  baseURL,
}, testInfo) => {
  test.setTimeout(600_000);
  test.skip(testInfo.project.name !== "sse", "runs on the sse project's own backend");
  const run = serverRun("sse");
  const origin = new URL(baseURL as string).origin;
  const consoleErrors = trackConsoleErrors(
    page,
    [
      // 401: the unauthenticated probe before sign-in. 409/429: the
      // operator view's frame poller on a replay/one-shot server —
      // "capture signal unavailable" and the preview throttle are the
      // designed transient answers the frame card renders as "unknown".
      // 503: unwired surfaces (Bluetooth, unconfigured voices).
      401, 409, 429, 503,
    ],
  );

  // ── 1. seed + publish the synthetic profile through the API ───────────────
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

  const game = await post(
    "/api/v1/games",
    { slug: EDITION.slug, title: EDITION.title },
    { "idempotency-key": randomUUID(), "x-csrf-token": await csrf() },
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
  const r1 = ((await imported.json()) as { profile: { id: string } }).profile.id;

  const etagOf = async (id: string) => {
    const response = await request.get(`${origin}/api/v1/profiles/${id}`);
    expect(response.status(), "a profile read carries an ETag").toBe(200);
    return response.headers()["etag"] as string;
  };
  await post(
    `/api/v1/profiles/${r1}/status`,
    { to: "review" },
    {
      "if-match": await etagOf(r1),
      "x-csrf-token": await csrf(),
    },
  );
  const published = await post(`/api/v1/profiles/${r1}/publish`, undefined, {
    "if-match": await etagOf(r1),
    "x-csrf-token": await csrf(),
  });
  expect(published.status(), `publish: ${await published.text()}`).toBe(200);

  const messagesPage = (await (
    await request.get(`${origin}/api/v1/profiles/${r1}/messages?limit=200`)
  ).json()) as { messages: { id: string; source_message_id?: string }[] };
  const storeIds: Record<string, string> = {};
  for (const row of messagesPage.messages ?? []) {
    if (row.source_message_id) storeIds[row.source_message_id] = row.id;
  }
  expect(storeIds["demo-page-1"], "demo-page-1 is a stored message").toBeTruthy();
  expect(storeIds["demo-page-2"], "demo-page-2 is a stored message").toBeTruthy();

  // ── 2. /play: start the session, the first line arrives live ──────────────
  await page.goto("/play");
  await expect(page).toHaveURL(/\/login/);
  await page.getByTestId("login-username").fill(ADMIN.username);
  await page.getByTestId("login-password").fill(ADMIN.password);
  await page.getByTestId("login-submit").click();
  await expect(page).toHaveURL(/\/play$/);

  // Every /events request the browser makes, with its resume cursor if it has
  // one. The initial connection carries none (the page starts at "0" and the
  // client omits the header); a reconnect after history must carry a cursor.
  const withCursor: { before: string[]; after: string[] } = { before: [], after: [] };
  let phase: "before" | "after" = "before";
  page.on("request", (req) => {
    if (!req.url().includes("/api/v1/events")) return;
    const cursor = req.headers()["last-event-id"];
    if (cursor === undefined) return;
    (phase === "before" ? withCursor.before : withCursor.after).push(cursor);
  });

  await page.getByTestId("play-profile").selectOption(r1, { timeout: 20_000 });
  await page.getByTestId("play-start").click();
  await expect(page).toHaveURL(/\/play\?session=/);
  const sessionId = new URL(page.url()).searchParams.get("session") as string;
  expect(sessionId).toBeTruthy();
  await expect(page.getByTestId("play-state")).toHaveText("running", { timeout: 60_000 });
  const lines = page.locator('[data-testid="recent-lines"] li');
  await expect
    .poll(() => lines.count(), { timeout: 120_000, intervals: [50] })
    .toBeGreaterThanOrEqual(1);
  const liveLine = (await lines.first().textContent()) as string;
  expect(liveLine, "the first line arrived over the live stream").toContain(
    storeIds["demo-page-1"],
  );

  // The server's own view at the moment the wire is cut: the cursor the page
  // must resume from is fixed when the session starts (internal/runtime).
  const snapshotA = (await (
    await request.get(`${origin}/api/v1/sessions/${sessionId}`)
  ).json()) as SessionSnapshot;
  const cursorA = snapshotA.event_cursor;
  expect(cursorA, "the session has an event cursor to resume from").toBeGreaterThan(0);

  // ── 3. the reload tears the in-flight stream down ─────────────────────────
  phase = "after";
  await page.reload();
  await expect(page).toHaveURL(/\/play\?session=/);
  await expect(page.getByTestId("play-session-id")).toHaveText(sessionId);

  // ── 4. the reconnect carries the session's own cursor ─────────────────────
  await expect
    .poll(async () => withCursor.after.length, { timeout: 15_000, message: "the page reconnected" })
    .toBeGreaterThanOrEqual(1);
  expect(
    withCursor.after[0],
    "the first reconnect resumes from the session's own event cursor",
  ).toBe(String(cursorA));
  for (const value of withCursor.after) {
    expect(
      Number(value),
      "every reconnect carries the cursor the stream had reached",
    ).toBeGreaterThanOrEqual(cursorA);
  }

  // ── 5. the page settles to exactly the server's final state ───────────────
  await expect
    .poll(
      async () => {
        const s = (await (
          await request.get(`${origin}/api/v1/sessions/${sessionId}`)
        ).json()) as SessionSnapshot;
        return s.state;
      },
      { timeout: 180_000, message: "the session settles" },
    )
    .toMatch(/stopped|failed/);
  const final = (await (
    await request.get(`${origin}/api/v1/sessions/${sessionId}`)
  ).json()) as SessionSnapshot;
  expect(final.state, `the session ended: ${final.reason ?? ""}`).toBe("stopped");
  expect(final.stats.commits, "the synthetic clip commits its two pages").toBe(2);
  expect(final.stats.ocr_calls).toBeGreaterThan(0);

  // The page, from the other side of the broken wire: the same numbers. The
  // commit counter is a session fact (the operator view since the 2026-10-20
  // review), so the read goes through "show workings"; the caption view's
  // rows read straight after, so the walk toggles back.
  await expect(page.getByTestId("play-state")).toHaveText("stopped", { timeout: 30_000 });
  await setPlayView(page, "operator");
  await expect(page.getByTestId("play-commits")).toHaveText(String(final.stats.commits));
  await setPlayView(page, "now"); // back to now-speaking
  const linesAfter = page.locator('[data-testid="recent-lines"] li');
  await expect(linesAfter).toHaveCount(2, { timeout: 30_000 });
  await expect(linesAfter.first()).toContainText(
    "nobody in the village had seen that boat before.",
  );
  await expect(linesAfter.first()).toContainText(storeIds["demo-page-2"]);
  await expect(linesAfter.last()).toContainText(
    "the harbour bell rang twice before anyone looked up.",
  );
  await expect(linesAfter.last()).toContainText(storeIds["demo-page-1"]);
  await expect(page.getByTestId("current-line-message-id")).toHaveText(storeIds["demo-page-2"]);
  await expect(page.getByTestId("current-line-speaker")).toHaveText("Marina");
  // The stream recovered on its own: no fallback banner the page would show
  // if the fallback had taken over (the counters kept polling, but the live
  // path is what delivered the replay).
  await expect(page.getByTestId("play-stream-failed")).toHaveCount(0);

  expect(consoleErrors, `browser console: ${consoleErrors.join("\n")}`).toEqual([]);
  expectDefaultDataDirUntouched(run);
});
