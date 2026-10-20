import { expect, test, type Page } from "@playwright/test";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { randomUUID } from "node:crypto";

import { trackConsoleErrors } from "./support/console";
import { buildManifest, type SeedProfile } from "./support/manifest";
import { expectDefaultDataDirUntouched, serverRun } from "./support/servers";

/**
 * The milestone 5 player walk (docs/spec/16 M5: "user can curate a full
 * synthetic game with no CLI"), on the packaged binary with the repo's testdata
 * admitted to the replay allowlist (the `play` webServer, ADR-0024). One test,
 * one data directory, real APIs only:
 *
 *   1. seed + publish the synthetic profile through the API (characters,
 *      speakers and attribution included — the manifest builder signs it the
 *      way internal/profileio/canonical.go does);
 *   2. /play: start a replay session on the published revision, watch the two
 *      committed lines arrive with their message ids and roster speakers, and
 *      the speech counters show them requested and ended skipped (no approved
 *      voice in this walk); the live audio controls — mute, volume, speech
 *      delay — are applied through POST /audio (ADR-0039) and survive the
 *      mid-session page reload (SSE resumes from the session's event cursor),
 *      then stop;
 *   3. /games/:id/characters: the roster with line counts and the verified /
 *      inferred attribution badges;
 *   4. derive a revision, edit one speech text, /games/:id/publish shows
 *      "1 changed", the review-and-publish transaction lands, and the history
 *      records it;
 *   5. /health (the renamed /diagnostics, ADR-0111) shows the readiness
 *      detail.
 */

const EDITION = {
  slug: "m5-harbour",
  title: "Harbour Light",
  region: "us",
  language: "en",
  revision: "rev0",
};
const ADMIN = { username: "m5-admin", password: "steady as the tide" };
const REPLAY_DIR = "testdata/synthetic/replay/dialogue-advance-001";

/** The seed: the same engine-tuned profile the developer walk seeds from
 *  (testdata/synthetic/profiles/dialogue-advance-001.json — its distractors
 *  are calibrated so the replay commits demo-page-1 then demo-page-2), with a
 *  roster and attribution added in memory: the two committed lines belong to
 *  Marina (confirmed), the distractors ride on the Keeper (unknown). The
 *  characters page then has a verified row and an inferred row to show. */
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

test("m5 part 1: play, characters, publish review, diagnostics", async ({
  page,
  request,
  baseURL,
}, testInfo) => {
  test.setTimeout(600_000);
  test.skip(
    testInfo.project.name !== "play",
    "runs on the play project's own embedded-binary backend",
  );
  const run = serverRun("play");
  const origin = new URL(baseURL as string).origin;
  const consoleErrors = trackConsoleErrors(
    page,
    [
      // 401: the unauthenticated probe the auth context makes before signing in.
      // 503: /api/v1/voices on a build whose voice models are not configured —
      // the pages answer it with their honest "not available" panels, which is
      // the documented behaviour this walk is checking, not a fault.
      // 409: the health page's capture signal read (this walk's last route is
      // the /diagnostics → /health redirect) on an appliance with no capture
      // layer — ADR-0110 §1's 409 carries the layer's own reason, and the
      // tile renders it as its own state, so it is the machine's answer,
      // not a fault.
      401, 409, 503,
    ],
  );
  const shot = (step: string) =>
    page.screenshot({ path: `test-results/m5-part1-${step}.png`, fullPage: true });

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
    {
      "idempotency-key": randomUUID(),
      "x-csrf-token": await csrf(),
    },
  );
  expect(game.status(), `create game: ${await game.text()}`).toBe(201);
  const gameId = (await game.json()) as { id: string };

  const edition = await post(
    `/api/v1/games/${gameId.id}/editions`,
    {
      region: EDITION.region,
      language: EDITION.language,
      revision: EDITION.revision,
    },
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

  const etagOf = async (id: string) => {
    const response = await request.get(`${origin}/api/v1/profiles/${id}`);
    expect(response.status(), "a profile read carries an ETag").toBe(200);
    return response.headers()["etag"] as string;
  };
  await post(
    `/api/v1/profiles/${profile.id}/status`,
    { to: "review" },
    {
      "if-match": await etagOf(profile.id),
      "x-csrf-token": await csrf(),
    },
  );
  const published = await post(`/api/v1/profiles/${profile.id}/publish`, undefined, {
    "if-match": await etagOf(profile.id),
    "x-csrf-token": await csrf(),
  });
  expect(published.status(), `publish: ${await published.text()}`).toBe(200);
  const r1 = profile.id;

  // The store minted row ULIDs; the committed events carry those, so the
  // friendly names are read back the way the pages read them.
  const messagesPage = (await (
    await request.get(`${origin}/api/v1/profiles/${r1}/messages?limit=200`)
  ).json()) as { messages: { id: string; source_message_id?: string; speech_text?: string }[] };
  const storeIds: Record<string, string> = {};
  for (const row of messagesPage.messages ?? []) {
    if (row.source_message_id) storeIds[row.source_message_id] = row.id;
  }
  expect(storeIds["demo-page-1"], "demo-page-1 is a stored message").toBeTruthy();
  expect(storeIds["demo-page-2"], "demo-page-2 is a stored message").toBeTruthy();

  // ── 2. /play: sign in, start the replay, watch the commits ────────────────
  await page.goto("/play");
  await expect(page).toHaveURL(/\/login/);
  await page.getByTestId("login-username").fill(ADMIN.username);
  await page.getByTestId("login-password").fill(ADMIN.password);
  await page.getByTestId("login-submit").click();
  await expect(page).toHaveURL(/\/play$/);

  const profileSelect = page.getByTestId("play-profile");
  await profileSelect.selectOption(r1, { timeout: 20_000 });
  await expect(profileSelect).toHaveValue(r1);
  await expect(page.getByTestId("play-replay-dir")).toHaveValue(REPLAY_DIR);
  await page.getByTestId("play-start").click();
  await expect(page).toHaveURL(/\/play\?session=/);
  const sessionId = new URL(page.url()).searchParams.get("session") as string;
  expect(sessionId).toBeTruthy();

  await expect(page.getByTestId("play-state")).toHaveText("running", { timeout: 60_000 });
  // The session's capture source self-report (the API view of capture.Stats):
  // the replay source names its kind and is running with no failure text —
  // the field that makes a silently dead capture child visible (the 2026-10-19
  // live-play run: a dolphin source delivered zero frames for 25 minutes and
  // nothing in the snapshot said why).
  await expect(page.getByTestId("play-source")).toContainText("replay · running", {
    timeout: 30_000,
  });
  await expect(page.getByTestId("play-source-error")).toHaveCount(0);
  const committedLines = page.locator('[data-testid="recent-lines"] li');
  // Anchor on the narratable demo line specifically, not on any committed
  // line: an abstained box (unclear whether the text should be spoken)
  // commits too — with message_id unknown and speech_eligible false
  // (docs/spec/05) — and such a commit emits no speech events at all
  // (ADR-0092). Counting any line and then demanding a speech request
  // within a fixed window raced the clip's own timing under load (the
  // 2026-10-20 full-suite run: the first commit was in, the narratable
  // line still queued, the counter sat at zero for the whole window).
  // The id in the row is the store's ULID, so the anchor holds regardless
  // of how the OCR engine rendered the case of the line's text.
  const narratableLine = committedLines.filter({ hasText: storeIds["demo-page-1"] });
  await expect
    .poll(() => narratableLine.count(), {
      timeout: 120_000,
      intervals: [50],
    })
    .toBeGreaterThanOrEqual(1);

  // ── 2b. the audio controls: real, applied live, counters honest ───────────
  // The play server runs with the speech roles pointed at the scripted TTS
  // instance, so this session has an audio pipeline. The walk's roster has
  // no approved voice (no narrator is configured), so the committed lines
  // end skipped with the reason — the counters have to say so, not fake a
  // playback (docs/spec/13 "did it speak, and why not").
  await expect(page.getByTestId("play-audio-sink")).toHaveText("filesink");
  await expect(page.getByTestId("play-audio-counters")).toBeVisible();
  await expect(page.getByTestId("play-speech-requested")).toHaveText(/^[1-9]/, { timeout: 30_000 });
  await expect(page.getByTestId("play-utterance-state")).toContainText("skipped", {
    timeout: 30_000,
  });

  const audioOf = async () => {
    const s = (await (await request.get(`${origin}/api/v1/sessions/${sessionId}`)).json()) as {
      audio?: { muted: boolean; volume: number; speech_delay_ms: number };
    };
    return s.audio;
  };

  // One audio apply, awaited the way the page owns it: the browser's own POST
  // response is the acknowledgement (the server applies the settings
  // synchronously and answers with the fresh snapshot). A separate client's
  // read confirms the server's state without telling us the page has seen it,
  // so a gesture or a reload must wait on the page's own acknowledgement.
  const audioAck = (pg: Page) =>
    pg.waitForResponse(
      (r) => r.request().method() === "POST" && new URL(r.url()).pathname.endsWith("/audio"),
      { timeout: 10_000 },
    );

  // Mute: the 200 body is the fresh snapshot with the applied state, and the
  // API's own snapshot agrees — the control was applied to a pipeline, not to
  // a placeholder. The acknowledgement is awaited before the next gesture, so
  // it cannot land while the mute apply is still in flight.
  const muteApply = audioAck(page);
  await page.getByTestId("play-mute").click();
  const muteAckRes = await muteApply;
  expect(muteAckRes.status(), "the mute apply is acknowledged").toBe(200);
  await expect(page.getByTestId("play-mute")).toHaveText("Unmute");
  await expect.poll(() => audioOf().then((a) => a?.muted === true)).toBe(true);

  // Ten detents down from the default of 100 lands on 90: one debounce
  // window posts one apply, its response carries the fresh snapshot, and the
  // resting value the control settles on is asserted against that
  // acknowledgement (then the server's own snapshot is checked to agree).
  const volume = page.getByTestId("play-volume");
  await volume.focus();
  // The debounce posts one apply per rest; await its response so the resting
  // value read below is the acknowledged one, not a value the page is still
  // settling (the 1s snapshot poll can re-sync local state while presses land).
  const volumeApply = audioAck(page);
  for (let i = 0; i < 10; i++) {
    await page.keyboard.press("ArrowLeft");
  }
  const volumeAckRes = await volumeApply;
  expect(volumeAckRes.status(), "the volume apply is acknowledged").toBe(200);
  await expect(volume).toHaveValue("90", { timeout: 10_000 });
  const volumeAtRest = await volume.inputValue();
  expect(volumeAtRest).toBe("90");
  const delayApply = audioAck(page);
  await page.getByTestId("play-speech-delay").fill("750");
  const delayAckRes = await delayApply;
  expect(delayAckRes.status(), "the delay apply is acknowledged").toBe(200);
  await expect
    .poll(
      async () => {
        const a = await audioOf();
        return a?.volume === Number(volumeAtRest) / 100 && a?.speech_delay_ms === 750;
      },
      { timeout: 30_000 },
    )
    .toBe(true);

  // The refresh: the URL keeps the session, the snapshot refetches, and the
  // stream resumes from the session's own event cursor.
  await page.reload();
  await expect(page).toHaveURL(/\/play\?session=/);
  await expect(page.getByTestId("play-session-id")).toHaveText(sessionId);
  // The applied audio state survives the reload from the snapshot — the
  // controls are the server's state, not the page's scratch.
  await expect(page.getByTestId("play-mute")).toHaveText("Unmute");
  await expect(page.getByTestId("play-volume")).toHaveValue(volumeAtRest);
  await expect(page.getByTestId("play-speech-delay")).toHaveValue("750");
  // Commit 1 is replayed from the hub's window; commit 2 arrives live or from
  // the same replay. The order is the story's order: newest line on top.
  const linesAfterReload = page.locator('[data-testid="recent-lines"] li');
  await expect(linesAfterReload).toHaveCount(2, { timeout: 120_000 });
  // The committed text is the OCR normalisation of the page (the engine
  // lowercases), so compare case-insensitively; the ids are exact.
  await expect(linesAfterReload.first()).toContainText(
    "nobody in the village had seen that boat before.",
  );
  await expect(linesAfterReload.first()).toContainText(storeIds["demo-page-2"]);
  await expect(linesAfterReload.last()).toContainText(
    "the harbour bell rang twice before anyone looked up.",
  );
  await expect(linesAfterReload.last()).toContainText(storeIds["demo-page-1"]);
  await expect(page.getByTestId("current-line-message-id")).toHaveText(storeIds["demo-page-2"]);
  await expect(page.getByTestId("current-line-speaker")).toHaveText("Marina");
  await expect(page.getByTestId("play-commits")).toHaveText("2");
  await shot("02-after-reload");

  // Stop the session (it may also end on its own on source exhaustion; either
  // terminal state is the documented outcome of this clip).
  const stop = page.getByTestId("play-stop");
  if (await stop.isVisible()) {
    await stop.click();
  }
  await expect(page.getByTestId("play-state")).toHaveText("stopped", { timeout: 180_000 });
  const finalSession = (await (
    await request.get(`${origin}/api/v1/sessions/${sessionId}`)
  ).json()) as { state: string; reason?: string; stats: { commits: number; ocr_calls: number } };
  expect(finalSession.state).toBe("stopped");
  expect(finalSession.reason).toBeTruthy();
  expect(finalSession.stats.commits, "the synthetic clip commits its two pages").toBe(2);
  expect(finalSession.stats.ocr_calls).toBeGreaterThan(0);
  await shot("03-session-stopped");

  // ── 3. /characters: the roster with counts and attribution badges ────────
  await page.goto(`/games/${gameId.id}/characters?profile=${encodeURIComponent(r1)}`);
  await expect(page.getByTestId("character-table")).toBeVisible({ timeout: 20_000 });
  const marina = page.getByTestId("character-row-Marina");
  await expect(marina).toBeVisible();
  await expect(page.getByTestId("character-lines-Marina")).toHaveText("2");
  await expect(page.getByTestId("character-attribution-Marina")).toContainText("verified");
  const keeper = page.getByTestId("character-row-Keeper");
  await expect(keeper).toBeVisible();
  await expect(page.getByTestId("character-lines-Keeper")).toHaveText("12");
  await expect(page.getByTestId("character-attribution-Keeper")).toContainText("inferred");
  await expect(page.getByTestId("characters-voices-link")).toBeVisible();
  await shot("04-characters");

  // ── 4. publish review: derive, edit one line, diff, publish, history ─────
  const derived = await post(`/api/v1/profiles/${r1}/revisions`, undefined, {
    "x-csrf-token": await csrf(),
  });
  expect(derived.status(), `derive: ${await derived.text()}`).toBe(201);
  const r2 = ((await derived.json()) as { id: string }).id;

  // A derived revision mints fresh row ids; the friendly id survives as
  // source_message_id, which is the only stable handle across revisions.
  const r2Messages = (await (
    await request.get(`${origin}/api/v1/profiles/${r2}/messages?limit=200`)
  ).json()) as { messages: { id: string; source_message_id?: string }[] };
  const page1Message = r2Messages.messages.find(
    (row) => row.source_message_id === "demo-page-1",
  ) as { id: string };
  expect(page1Message.id, "r2 re-minted the demo-page-1 row").toBeTruthy();
  const edited = await request.patch(
    `${origin}/api/v1/profiles/${r2}/messages/${page1Message.id}`,
    {
      data: { speech_text: "The harbour bell rang twice, and no one looked up." },
      headers: {
        "content-type": "application/json",
        "x-csrf-token": await csrf(),
        "if-match": await etagOf(r2),
      },
    },
  );
  expect(edited.status(), `edit speech: ${await edited.text()}`).toBe(200);

  await page.goto(`/games/${gameId.id}/publish?profile=${encodeURIComponent(r2)}`);
  await expect(page.getByTestId("diff-panel")).toBeVisible({ timeout: 20_000 });
  await expect(page.getByTestId("diff-changed")).toHaveText("1 changed");
  await expect(page.getByTestId("diff-added")).toHaveText("0 added");
  await expect(page.getByTestId("diff-removed")).toHaveText("0 removed");
  await expect(page.getByTestId("diff-changed-rows")).toContainText("no one looked up");
  // Nothing is published-side that the draft lost, and the gate's inputs are
  // visible: the roster has no voice yet, and no source rows block publication.
  await expect(page.getByTestId("voice-gate-table")).toBeVisible();
  await expect(page.getByTestId("publish-history")).toBeVisible();
  await shot("05-diff-one-changed");

  // draft → review → publish: the page's own transaction, every refusal shown.
  await page.getByTestId("publish-to-review").click();
  await expect(page.getByTestId("publish-button")).toBeVisible({ timeout: 15_000 });
  await page.getByTestId("publish-button").click();
  const historyRow = page.getByTestId("publish-action-row").first();
  // The newest entry is this publish, named by the revision it shipped (the
  // row's result_row_version is only a secondary detail — the audit row has
  // no revision number of its own), and its result badge is the success one.
  await expect(historyRow).toContainText("published revision 2", { timeout: 30_000 });
  await expect(historyRow).toContainText("row version");
  await expect(historyRow.locator(".badge-succeeded")).toBeVisible();
  // The pointer swapped: the draft now equals the published revision, so the
  // diff against "what is live" empties out.
  await expect(page.getByTestId("diff-changed")).toHaveText("0 changed");
  // The rollback button names the revision this publish superseded — the
  // previously published revision 1 — not the revision that is live now.
  await expect(page.getByTestId("rollback-derive")).toContainText("Roll back to revision 1");
  await shot("06-published");

  // ── 5. /health (the renamed /diagnostics, ADR-0111): readiness and its
  // detail ─────────────────────────────────────────────────────────────
  // The readiness probe runs a live integrity check. While the replay session
  // has been writing, that probe can transiently report a failure and answer
  // 503 — the page's documented degradation is the honest error panel with a
  // retry, so the walk exercises that path until the probe agrees again. A
  // readiness that never recovers is a real failure: the retries exhaust and
  // the assertion below fails.
  await page.goto("/diagnostics");
  let readinessOk = false;
  for (let attempt = 0; attempt < 6 && !readinessOk; attempt++) {
    if (attempt > 0) {
      await page.getByRole("button", { name: /try again/i }).click();
    }
    readinessOk = await expect(page.getByTestId("readiness"))
      .toHaveText("ok", { timeout: 5_000 })
      .then(
        () => true,
        () => false,
      );
  }
  expect(readinessOk, "readiness reports ok (the transient probe 503s are retried)").toBe(true);
  await expect(page.getByTestId("readiness-check-database")).toContainText("ok");
  // ADR-0087 (G10) replaced the old "model health" slice with the model-roles
  // section: the diagnostics run's per-role probe rows (or the sign-in state
  // for the anonymous visitor). The signed-in admin sees the probe control.
  await expect(page.getByTestId("model-roles")).toBeVisible();
  await expect(page.getByTestId("model-roles-probe-all")).toBeVisible();
  await shot("07-diagnostics");

  expect(consoleErrors, `browser console: ${consoleErrors.join("\n")}`).toEqual([]);
  expectDefaultDataDirUntouched(run);
});
