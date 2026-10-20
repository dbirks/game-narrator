import { expect, test } from "@playwright/test";
import { randomUUID } from "node:crypto";
import { gunzipSync } from "node:zlib";
import { existsSync, readFileSync, rmSync } from "node:fs";
import { resolve } from "node:path";

import { trackConsoleErrors } from "./support/console";
import { buildManifest, type SeedProfile } from "./support/manifest";
import { expectDefaultDataDirUntouched, serverRun } from "./support/servers";

/**
 * The developer inspector's per-event actions (ADR-0083, docs/spec/09):
 * on the /developer page, for the page-1 trace of a real replay session —
 * the six correction labels (persisted through POST /corrections and read
 * back), the "save regression fixture" action (a self-contained directory
 * under the external fixture root, with the hash manifest), the "re-run
 * against current model" action (the stored observation re-ranked by the
 * current build's matcher over the run's own immutable revision, with its
 * diff), the "download evidence" action (the redacted tar.gz), and the
 * utterance's waveform drawn from the served WAV.
 *
 * The session is real (the packaged binary, the synthetic clip, the real
 * OCR pipeline); the utterance's audio is the scripted TTS instance's
 * deterministic WAV — a synthetic stub, labelled as such, exactly the class
 * web/e2e/fake-tts.mjs is for the rest of the suite. The seed's lines carry
 * no speaker, so the narrator voice (the session's fallback for
 * unattributed lines) is registered the way play-controls.spec.ts registers
 * a roster voice: audition → approve → register, keyed by the literal
 * character id "narrator".
 */

const SEED_PATH = resolve(
  import.meta.dirname,
  "..",
  "..",
  "testdata",
  "synthetic",
  "profiles",
  "dialogue-advance-001.json",
);
const EDITION = {
  slug: "synthetic-actions",
  title: "Synthetic Actions",
  region: "us",
  language: "en",
  revision: "rev0",
};
const ADMIN = { username: "actions-admin", password: "correct horse battery" };
const CONSENT = "original_synthetic_design";
const sessionTimeout = 180_000;

interface DevEvent {
  event_id: string;
  seq: number;
  name: string;
  trace_id?: string;
  occurred_at: string;
  data: Record<string, unknown>;
}

/** The seed's lines are committed by the local matcher (MATCH_EXACT) and
 *  carry no speaker, so the session speaks them with the narrator voice —
 *  the voice profile keyed by the literal character id "narrator" (the
 *  session's fallback for unattributed lines, internal/runtime/speech.go).
 *  Registering it the way play-controls.spec.ts registers a roster voice
 *  (audition → approve → register) is what makes the utterance real. */
const NARRATOR_CHARACTER = "narrator";

test("the inspector's per-event actions over a real replay session", async ({
  page,
  request,
  baseURL,
}, testInfo) => {
  test.skip(
    testInfo.project.name !== "developer-actions",
    "runs on the developer-actions project's own embedded-binary backend",
  );
  test.setTimeout(420_000);
  const origin = new URL(baseURL as string).origin;
  const run = serverRun("developer-actions");
  const consoleErrors = trackConsoleErrors(page); // 401 (the unauth /auth/me probe) is the only expected one
  const shot = (step: string) =>
    page.screenshot({ path: `test-results/developer-actions-${step}.png`, fullPage: true });

  let fixtureDir: string | null = null;
  try {
    // ── 1. the appliance: admin, the seeded profile, Marina's voice ───────
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

    const seed = JSON.parse(readFileSync(SEED_PATH, "utf8")) as SeedProfile;
    const { doc: manifest } = buildManifest(seed, { edition: EDITION });
    const imported = await post(
      `/api/v1/editions/${editionId.id}/profiles/import-manifest`,
      manifest,
    );
    expect(imported.status(), `import-manifest: ${await imported.text()}`).toBe(201);
    const { profile } = (await imported.json()) as { profile: { id: string } };

    let etagResponse = await request.get(`${origin}/api/v1/profiles/${profile.id}`);
    let etag = etagResponse.headers()["etag"];
    const toReview = await post(
      `/api/v1/profiles/${profile.id}/status`,
      { to: "review" },
      { "if-match": etag },
    );
    expect(toReview.status(), `to review: ${await toReview.text()}`).toBe(200);
    etagResponse = await request.get(`${origin}/api/v1/profiles/${profile.id}`);
    etag = etagResponse.headers()["etag"] as string;
    const published = await post(`/api/v1/profiles/${profile.id}/publish`, undefined, {
      "if-match": etag,
    });
    expect(published.status(), `publish: ${await published.text()}`).toBe(200);

    const messagesPage = (await (
      await request.get(`${origin}/api/v1/profiles/${profile.id}/messages?limit=200`)
    ).json()) as { messages: { id: string; source_message_id?: string }[] };
    const storeIds: Record<string, string> = {};
    for (const row of messagesPage.messages ?? []) {
      if (row.source_message_id) storeIds[row.source_message_id] = row.id;
    }
    expect(storeIds["demo-page-1"], "demo-page-1 is a stored row").toBeTruthy();

    // The narrator voice: audition → approve (consent) → register, keyed by
    // the literal character id the session's speaker fallback resolves to.
    const auditions = await post("/api/v1/voices/auditions", {
      character: NARRATOR_CHARACTER,
      brief: "A calm harbour voice, unhurried, with a slight salt air in it.",
      count: 1,
    });
    expect(auditions.status(), `auditions: ${await auditions.text()}`).toBe(202);
    const audition = (await auditions.json()) as { job_id: string; voice_id: string };
    await expect
      .poll(
        async () => {
          const body = (await (
            await request.get(`${origin}/api/v1/jobs/${audition.job_id}`)
          ).json()) as { state: string };
          if (body.state === "failed")
            throw new Error(`the audition job failed: ${JSON.stringify(body)}`);
          return body.state;
        },
        { timeout: 90_000, message: "the audition job reaches a terminal state" },
      )
      .toBe("succeeded");
    const auditionResult = (await (
      await request.get(`${origin}/api/v1/jobs/${audition.job_id}`)
    ).json()) as { result: { audition_count: number; audition_ids: string[] } };
    const approved = await post(`/api/v1/voices/${audition.voice_id}/approve`, {
      audition_id: auditionResult.result.audition_ids[0] ?? "",
      consent: CONSENT,
    });
    expect(approved.status(), `approve: ${await approved.text()}`).toBe(200);
    const registered = await post(`/api/v1/voices/${audition.voice_id}/register`);
    expect(registered.status(), `register: ${await registered.text()}`).toBe(200);

    // ── 2. the session: the replay with dev trace on, through the page ─────
    await page.goto("/developer");
    await expect(page).toHaveURL(/\/login/);
    await page.getByTestId("login-username").fill(ADMIN.username);
    await page.getByTestId("login-password").fill(ADMIN.password);
    await page.getByTestId("login-submit").click();
    await expect(page).toHaveURL(/\/developer$/);

    await page.getByTestId("session-profile").selectOption(profile.id, { timeout: 15_000 });
    await expect(page.getByTestId("session-dev-trace")).toBeChecked();
    await page.getByTestId("session-start").click();
    await shot("01-session-started");
    await expect(page.getByTestId("session-state")).toHaveText("stopped", {
      timeout: sessionTimeout,
    });
    const sessionId = (await page.getByTestId("session-id").textContent()) as string;
    const session = (await (
      await request.get(`${origin}/api/v1/sessions/${sessionId}`)
    ).json()) as { state: string; stats: { commits: number } };
    expect(session.state).toBe("stopped");
    expect(session.stats.commits, "the synthetic clip commits its two pages").toBe(2);
    await shot("02-session-stopped");

    const events = (
      (await (
        await request.get(
          `${origin}/api/v1/dev/events?session_id=${encodeURIComponent(sessionId)}&limit=1000`,
        )
      ).json()) as { events: DevEvent[] }
    ).events;
    const commits = events.filter((event) => event.name === "message.committed");
    // The seed's lines commit locally (the clip's OCR reading is an exact
    // corpus match), so the commit resolves to a stored row and speaks — the
    // actions are exercised on a fully committed, fully spoken trace. The
    // decision-call chain (the ambiguous seed's walk) is developer-decision
    // spec's territory; the actions do not need a provider call to exist.
    expect(commits.length, "the clip commits its two pages").toBe(2);
    const page1 = commits[0];
    const page1MessageId = page1.data.message_id as string;
    expect(
      Object.values(storeIds),
      `the page-1 commit resolves to a stored row, not the abstain row: ${page1MessageId}`,
    ).toContain(page1MessageId);
    const traceId = page1.trace_id as string;
    expect(traceId).toBeTruthy();
    const speechReady = events.find(
      (event) => event.name === "speech.ready" && event.trace_id === traceId,
    );
    expect(speechReady, "the utterance speaks on the page-1 trace").toBeTruthy();
    const cacheKey = speechReady!.data.cache_key as string;
    expect(cacheKey).toMatch(/^[0-9a-f]{64}$/);

    // ── 3. pin the commit: the trace's actions appear ──────────────────────
    await page.getByTestId(`event-row-${page1!.seq}`).click();
    await expect(page.getByTestId("commit-message")).toHaveText("demo-page-1");
    await expect(page.getByTestId("trace-actions")).toBeVisible();
    await shot("03-trace-actions");

    // The six labels, in docs/spec/09's order.
    const labelButtons = [
      "speaker-incorrect",
      "text-incorrect",
      "wrong-region",
      "not-narratable",
      "duplicate",
      "prediction-misfire",
    ];
    for (const key of labelButtons) {
      await expect(page.getByTestId(`action-label-${key}`)).toBeVisible();
    }

    // The utterance's audio: the waveform canvas and the player, addressed by
    // the speech cache key the trace's speech.ready carried.
    await expect(page.getByTestId("action-audio-waveform")).toBeVisible({ timeout: 15_000 });
    const player = page.getByTestId("action-audio-player");
    await expect(player).toBeVisible();
    expect(await player.getAttribute("src")).toBe(
      `/api/v1/dev/speech/audio?cache_key=${encodeURIComponent(cacheKey)}`,
    );
    // The served bytes are the utterance's WAV (ETag'd, no-store).
    const audioResponse = await request.get(
      `${origin}/api/v1/dev/speech/audio?cache_key=${encodeURIComponent(cacheKey)}`,
    );
    expect(audioResponse.status()).toBe(200);
    expect(audioResponse.headers()["content-type"]).toBe("audio/wav");
    const wavBytes = await audioResponse.body();
    expect(wavBytes.subarray(0, 4).toString("hex")).toBe("52494646"); // RIFF
    await shot("04-waveform");

    // ── 4. the labels: two clicks persist two rows the bench can read ──────
    await expect(page.getByTestId("action-labels-empty")).toBeVisible();
    await page.getByTestId("action-label-text-incorrect").click();
    await expect(page.getByTestId("action-label-saved")).toContainText("Text incorrect");
    await expect(page.getByTestId("action-label-recorded")).toHaveCount(1);
    await page.getByTestId("action-label-speaker-incorrect").click();
    await expect(page.getByTestId("action-label-recorded")).toHaveCount(2);
    await shot("05-labels-recorded");

    // The server's own rows: the labels landed through POST /corrections with
    // the trace's correlation ids in the evidence member. The list member is
    // `items` (openapi.json CorrectionList), the same shape the UI and the
    // corrections-fixtures spec read.
    const correctionsBody = (await (
      await request.get(`${origin}/api/v1/corrections?profile_id=${profile.id}`)
    ).json()) as { items: { kind: string; reason: string; evidence: string[] }[] };
    const linked = correctionsBody.items.filter((row) =>
      row.evidence.some((entry) => entry.includes(`trace:${traceId}`)),
    );
    expect(linked, "the labels carry the trace's correlation").toHaveLength(2);
    const kinds = linked.map((row) => row.kind).sort();
    expect(kinds).toEqual(["speaker", "text"]);
    expect(linked.every((row) => row.reason.startsWith("inspector:"))).toBe(true);

    // ── 5. save the fixture: one directory under the external fixture root ─
    await page.getByTestId("action-fixture").click();
    await expect(page.getByTestId("action-fixture-result")).toBeVisible({ timeout: 30_000 });
    const dirOnPage = (await page.getByTestId("action-fixture-dir").textContent()) as string;
    expect(dirOnPage).toBeTruthy();
    const repoRoot = resolve(import.meta.dirname, "..", "..");
    fixtureDir = resolve(repoRoot, dirOnPage);
    expect(existsSync(fixtureDir), `the fixture directory exists at ${fixtureDir}`).toBe(true);
    const fixtureManifest = JSON.parse(
      readFileSync(resolve(fixtureDir, "manifest.json"), "utf8"),
    ) as {
      kind: string;
      source: { trace_id: string };
      entries: { path: string; sha256: string; size: number }[];
    };
    expect(fixtureManifest.kind).toBe("trace-fixture");
    expect(fixtureManifest.source.trace_id).toBe(traceId);
    const paths = fixtureManifest.entries.map((entry) => entry.path).sort();
    // The observation, the expectation, the samples and the utterance audio:
    // the run stored them (dev trace on, the voice speaks), so the fixture
    // carries them, and the manifest hashes every byte.
    for (const required of [
      "corrections.json",
      "expected.json",
      "frames/crop.png",
      "frames/frame.jpg",
      "observation.json",
      "speech.json",
      "speech/utterance.wav",
    ]) {
      expect(paths, `the manifest names ${required}`).toContain(required);
    }
    expect(paths).not.toContain("manifest.json"); // the manifest cannot carry its own hash
    for (const entry of fixtureManifest.entries) {
      const bytes = readFileSync(resolve(fixtureDir, entry.path));
      expect(bytes.length).toBe(entry.size);
    }
    await shot("06-fixture-saved");

    // ── 6. the re-run: the stored observation re-ranked, with its diff ─────
    await page.getByTestId("action-rerun").click();
    await expect(page.getByTestId("action-rerun-result")).toBeVisible({ timeout: 30_000 });
    // The truth OCR is deterministic and the revision is immutable, so the
    // re-run of the same observation over the same corpus is the same
    // ranking: the page-1 line holds the top and nothing moved.
    await expect(page.getByTestId("action-rerun-changed")).toHaveText("no");
    // The current build's matcher re-ranks the same observation over the same
    // immutable revision: the winner is the commit's own row (the exact
    // match's top candidate) and nothing moved.
    const rerunWinner = (await page.getByTestId("action-rerun-winner").textContent()) as string;
    expect(rerunWinner, "the re-run's winner is the commit's row").toBe(page1MessageId);
    await expect(page.getByTestId("action-rerun-decision")).toContainText(
      "the local decision path",
    );
    await expect(page.getByTestId("action-rerun-row-" + page1MessageId)).toBeVisible();
    await shot("07-rerun");

    // ── 7. the evidence: the redacted bundle downloads as a tar.gz ─────────
    const [download] = await Promise.all([
      page.waitForEvent("download", { timeout: 30_000 }),
      page.getByTestId("action-evidence").click(),
    ]);
    expect(download.suggestedFilename()).toBe(`trace-evidence-${traceId}.tar.gz`);
    const downloadPath = await download.path();
    const bundle = readFileSync(downloadPath as string);
    expect(bundle.subarray(0, 2).toString("hex")).toBe("1f8b"); // gzip
    const untarred = gunzipSync(bundle);
    // The bundle's entries name themselves in the tar headers (plain text):
    // the redacted event log and the hash manifest are both there.
    const names = untarred.toString("latin1");
    expect(names).toContain("events.json");
    expect(names).toContain("manifest.json");
    expect(names).toContain("utterance.wav");
    // The redaction is the support-bundle's rule set: no credentials or home
    // paths in what leaves the machine. (The event payloads carry no IPs or
    // gateway URLs to begin with; the unit test covers the walker's full rule
    // set, TestRedactJSONValue.)
    for (const leak of ["sk_live", "Bearer ", "/home/"]) {
      expect(names, `the bundle leaks ${leak}`).not.toContain(leak);
    }
    await shot("08-evidence-downloaded");

    // ── 8. the browser stayed clean and the default appliance untouched ────
    expect(consoleErrors, `browser console: ${consoleErrors.join("\n")}`).toEqual([]);
    expectDefaultDataDirUntouched(run);
  } finally {
    // The fixture the test recorded is test state, not a regression asset:
    // remove the directory it created (AGENTS.md rule 5: nothing of the
    // test's into the repository).
    if (fixtureDir && existsSync(fixtureDir)) {
      rmSync(fixtureDir, { recursive: true, force: true });
    }
  }
});
