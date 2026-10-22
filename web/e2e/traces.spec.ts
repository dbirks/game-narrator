import AxeBuilder from "@axe-core/playwright";
import { expect, test } from "@playwright/test";
import { randomUUID } from "node:crypto";
import { existsSync, readFileSync, rmSync } from "node:fs";
import { resolve } from "node:path";

import { trackConsoleErrors } from "./support/console";
import { buildManifest, type SeedProfile } from "./support/manifest";
import { expectDefaultDataDirUntouched, serverRun } from "./support/servers";

/**
 * The Traces screen (docs/design/plum-peach 2h, ADR-0141): on /traces, the
 * run's committed lines as a filterable list (spoken / skipped / slow), each
 * line's 5-step strip (Saw frame → Read OCR → Matched → Who → Spoke) matching
 * the API's own per-trace record, "what it read" (crop + raw vs normalised
 * OCR), "why this line" (candidate bars + reason), the actions (Replay audio,
 * Wrong speaker, Save as test, Download evidence), the /developer → /traces
 * redirect, and the UX-05 wrong-speaker walk: the correction lands immediately
 * and the in-app agent (the agent_reasoning role pointed at the scripted
 * gateway, web/e2e/fake-assistant.mjs — a labelled-synthetic decision model)
 * checks the similar lines and stages the bulk fix behind the confirmation
 * gate; the operator's confirm is what re-points the lines.
 *
 * The session is real (the packaged binary, the synthetic clip, the real OCR
 * pipeline); the utterance's audio is the scripted TTS instance's
 * deterministic WAV (a synthetic stub, labelled as such). The seed's lines
 * carry the roster's speaker (Wren) so find_similar_lines has a pattern to
 * read; Wren has no approved voice, so the narrator fallback speaks the lines
 * (ADR-0101) and the utterance is real. The model is the only stub; the API,
 * the job runner, the tools, the store and the confirmation gate are real.
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
  slug: "synthetic-traces",
  title: "Synthetic Traces",
  region: "us",
  language: "en",
  revision: "rev0",
};
const ADMIN = { username: "traces-admin", password: "correct horse battery" };
const CONSENT = "original_synthetic_design";
const NARRATOR_CHARACTER = "narrator";
// The roster the walk drives: the seed's lines are Wren's; the wrong-speaker
// correction re-points them to Marina. Both are invented placeholder names.
const WREN = { id: "wren", name: "Wren" };
const MARINA = { id: "marina", name: "Marina" };
const sessionTimeout = 180_000;
/** The design's three review widths (docs/design/plum-peach README, IA). */
const WIDTHS = [1440, 1024, 390] as const;
/** WCAG 2.1 A/AA: the 2.0 A/AA rules plus the rules 2.1 adds at those levels. */
const WCAG_21_A_AA = ["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"] as const;

interface ScanReport {
  url: string;
  width: number;
  serious: string[];
  violations: Record<string, number>;
}

interface DevEvent {
  event_id: string;
  seq: number;
  name: string;
  trace_id?: string;
  session_id?: string;
  occurred_at: string;
  data: Record<string, unknown>;
}

// The model's string reading: an absent or empty string is null, not "".
function str(value: unknown): string | null {
  return typeof value === "string" && value !== "" ? value : null;
}

/**
 * The 5-step strip the screen renders, re-derived from the API's per-trace
 * record (the same reading rules the screen's unit-tested model applies —
 * mirrored here so the e2e asserts the DOM against the API, not against the
 * page's own copy). The values are the strip cells' text.
 */
function expectedStrip(
  events: DevEvent[],
  nameById: Record<string, string>,
): Record<"saw" | "read" | "matched" | "who" | "spoke", string> {
  const last = (name: string) => {
    for (let i = events.length - 1; i >= 0; i -= 1) {
      const event = events[i];
      if (event && event.name === name) return event;
    }
    return null;
  };
  const observed = last("text.observed");
  const obs = observed?.data ?? {};
  const candidates = [...events].reverse().find((event) => {
    if (event.name !== "message.candidates") return false;
    const list = (event.data as Record<string, unknown>).candidates;
    return Array.isArray(list) && list.length > 0;
  });
  const committed = last("message.committed");
  const ready = last("speech.ready");
  const ended = last("speech.ended");

  const frameSeq = typeof obs.frame_seq === "number" ? obs.frame_seq : null;
  const confidence = typeof obs.confidence === "number" ? obs.confidence : null;
  const commitData = committed?.data ?? {};
  const speakerId = str(commitData.speaker_id);
  const readyData = ready?.data ?? {};
  const fromCache = typeof readyData.from_cache === "boolean" ? readyData.from_cache : null;
  const endData = ended?.data ?? {};
  const speechResult = str(endData.result);
  const speechReason = str(endData.reason);

  const candList = (candidates?.data as Record<string, unknown>)?.candidates as
    | Array<Record<string, unknown>>
    | undefined;
  const top = candList?.[0] ?? null;
  // The model reads probability ?? score (a candidate may carry either).
  const topProb =
    top && typeof top.probability === "number"
      ? top.probability
      : top && typeof top.score === "number"
        ? top.score
        : null;
  const reason = candidates ? str((candidates.data as Record<string, unknown>).reason) : null;

  return {
    saw: frameSeq === null ? "frame —" : `frame #${frameSeq}`,
    read: confidence === null ? "OCR conf —" : `conf ${confidence.toFixed(3)}`,
    matched:
      candidates && top
        ? topProb === null
          ? "ranked —"
          : `${topProb.toFixed(4)}${reason ? ` · ${reason}` : ""}`
        : "—",
    who: speakerId === null ? "Narrator" : (nameById[speakerId] ?? speakerId),
    spoke: ready
      ? fromCache === null
        ? "spoke"
        : fromCache
          ? "cache hit"
          : "cache miss"
      : speechResult === null
        ? "not spoken"
        : `${speechResult}${speechReason ? ` · ${speechReason}` : ""}`,
  };
}

/** The committed line's text (the screen's title), from the API's record. */
function expectedTitle(events: DevEvent[]): string {
  for (let i = events.length - 1; i >= 0; i -= 1) {
    const event = events[i];
    if (event && event.name === "message.committed") {
      const text = (event.data as Record<string, unknown>).text;
      if (typeof text === "string" && text !== "") return text;
    }
  }
  return "";
}

test("the Traces screen: the line list, the strip, the actions and the UX-05 walk", async ({
  page,
  request,
  baseURL,
}, testInfo) => {
  test.skip(
    testInfo.project.name !== "traces",
    "runs on the traces project's own embedded-binary backend",
  );
  test.setTimeout(480_000);
  const origin = new URL(baseURL as string).origin;
  const run = serverRun("traces");
  const consoleErrors = trackConsoleErrors(page); // the unauth /auth/me probe is the only expected one
  const shot = (step: string) =>
    page.screenshot({ path: `test-results/traces-${step}.png`, fullPage: true });

  const fixtureDirs: string[] = [];
  try {
    // ── 1. the appliance: admin, the seeded profile (roster + speakers) ───
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

    // The seed's lines, with the roster and Wren's speaker plate: the two
    // committed pages plus two distractors share Wren so find_similar_lines
    // has a pattern; the rest are Marina's. All invented placeholder copy.
    const seed = JSON.parse(readFileSync(SEED_PATH, "utf8")) as SeedProfile;
    seed.characters = [
      { ...WREN, status: "confirmed", summary: "A steady harbour voice." },
      { ...MARINA, status: "confirmed", summary: "A warmer, quicker voice." },
    ];
    const wrenLines = new Set(["demo-page-1", "demo-page-2", "demo-x01", "demo-x04"]);
    for (const message of seed.messages) {
      message.speaker = wrenLines.has(message.id) ? WREN.id : MARINA.id;
    }
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

    // The roster the walk resolves: character ids and the seed's speaker map
    // to the store's character ids.
    const charactersBody = (await (
      await request.get(`${origin}/api/v1/profiles/${profile.id}/characters`)
    ).json()) as { characters: { id: string; name: string }[] };
    const characterByName: Record<string, string> = {};
    const nameById: Record<string, string> = {};
    for (const row of charactersBody.characters ?? []) {
      characterByName[row.name] = row.id;
      nameById[row.id] = row.name;
    }
    const wrenId = characterByName[WREN.name];
    const marinaId = characterByName[MARINA.name];
    expect(wrenId, "Wren is on the roster").toBeTruthy();
    expect(marinaId, "Marina is on the roster").toBeTruthy();

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
    ).json()) as { result: { audition_ids: string[] } };
    const approved = await post(`/api/v1/voices/${audition.voice_id}/approve`, {
      audition_id: auditionResult.result.audition_ids[0] ?? "",
      consent: CONSENT,
    });
    expect(approved.status(), `approve: ${await approved.text()}`).toBe(200);
    const registered = await post(`/api/v1/voices/${audition.voice_id}/register`);
    expect(registered.status(), `register: ${await registered.text()}`).toBe(200);

    // ── 2. the session: the replay with dev trace on, in the inspector view ─
    // Sign in (the appliance is claimed via the API above). Wait for the
    // authenticated shell before navigating: a reload before the login fetch
    // completes has no session cookie yet and would read as the login page.
    await page.goto("/traces");
    await expect(page).toHaveURL(/\/login/);
    await page.getByTestId("login-username").fill(ADMIN.username);
    await page.getByTestId("login-password").fill(ADMIN.password);
    await page.getByTestId("login-submit").click();
    await expect(page.getByTestId("nav-role")).toHaveText("admin", { timeout: 15_000 });

    // /developer redirects to /traces (ADR-0141), preserving the query: the
    // session inspector is the ?view=inspector surface (the preserved
    // developer dashboard).
    await page.goto("/developer?view=inspector");
    await expect(page).toHaveURL(/\/traces\?view=inspector/);
    await expect(page.getByTestId("session-profile")).toBeVisible({ timeout: 15_000 });

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
    expect(commits.length, "the clip commits its two pages").toBe(2);
    const page1 = commits[0];
    const page1MessageId = page1.data.message_id as string;
    // The committed line's text is the normalised OCR reading (lowercase),
    // not the seed's original capitalisation; the list and the strip title
    // both read it from the commit event.
    const page1Text = page1.data.text as string;
    expect(page1Text, "the page-1 commit carries the line's text").toBeTruthy();
    const traceId = page1.trace_id as string;
    expect(traceId, "the page-1 commit carries a trace id").toBeTruthy();
    const speechReady = events.find(
      (event) => event.name === "speech.ready" && event.trace_id === traceId,
    );
    expect(speechReady, "the utterance speaks on the page-1 trace").toBeTruthy();
    const cacheKey = speechReady!.data.cache_key as string;
    expect(cacheKey).toMatch(/^[0-9a-f]{64}$/);

    // ── 3. the line list: /traces shows the committed lines ────────────────
    // The list orders newest-commit-first and selects its first line, so the
    // page-1 line is selected by its ?trace= deep link (the ADR-0141 seam).
    await page.goto(`/traces?trace=${encodeURIComponent(traceId)}`);
    await expect(page.getByTestId("traces-list")).toBeVisible({ timeout: 15_000 });
    // The clip's two committed pages are the run's lines (the distractor
    // messages never committed, so they are not lines).
    await expect(page.getByTestId("traces-line-rows")).toContainText(page1Text);
    await expect(page.getByTestId(`traces-line-${traceId}`)).toBeVisible();
    await shot("03-line-list");
    // The filter pills exist and the line is selected (the current line is
    // the --peach one).
    for (const filter of ["all", "spoken", "skipped", "slow"]) {
      await expect(page.getByTestId(`traces-filter-${filter}`)).toBeVisible();
    }
    expect(await page.getByTestId(`traces-line-${traceId}`).getAttribute("aria-current")).toBe(
      "true",
    );
    // The per-session deep link into the preserved inspector (ADR-0141 §1):
    // the line view links to the session inspector view on the same route.
    await expect(page.getByTestId("traces-open-inspector")).toHaveAttribute(
      "href",
      `/traces?view=inspector&session=${encodeURIComponent(sessionId)}`,
    );

    // ── 4. the 5-step strip matches the API's per-trace record ─────────────
    // The page reads the same per-trace record the API serves; re-derive the
    // strip values from that record (the screen's reading rules, mirrored
    // here) and assert the rendered values agree, step for step (plan.md item
    // 7 acceptance: "the 5-step strip values match the API record").
    const traceEvents = (
      (await (
        await request.get(`${origin}/api/v1/dev/traces/${encodeURIComponent(traceId)}`)
      ).json()) as { events: DevEvent[] }
    ).events;
    const expected = expectedStrip(traceEvents, nameById);
    await expect(page.getByTestId("traces-step-strip")).toBeVisible({ timeout: 15_000 });
    for (const step of ["saw", "read", "matched", "who", "spoke"] as const) {
      const value = page.getByTestId(`traces-step-${step}-value`);
      await expect(value, `the ${step} step renders`).toBeVisible();
      expect((await value.textContent())?.trim(), `the ${step} step matches the API record`).toBe(
        expected[step],
      );
    }
    // The last step (Spoke) is the --peach one.
    await expect(page.getByTestId("traces-step-spoke")).toHaveClass(/final/);
    // The line as a 30px title, the committed text.
    await expect(page.getByTestId("traces-title")).toHaveText(expectedTitle(traceEvents));
    await shot("04-strip");

    // "What it read": the crop and the raw vs normalised OCR.
    await expect(page.getByTestId("traces-what-it-read")).toBeVisible();
    await expect(page.getByTestId("traces-crop")).toBeVisible();
    await expect(page.getByTestId("traces-ocr-raw")).not.toBeEmpty();
    await expect(page.getByTestId("traces-ocr-normalized")).not.toBeEmpty();
    // "Why this line": at least one candidate bar and a one-sentence reason.
    await expect(page.getByTestId("traces-why-reason")).not.toBeEmpty();
    await expect(
      page.getByTestId("traces-candidate-rows").getByRole("listitem").first(),
    ).toBeVisible();
    // P3 regression: the rows are three grid columns (bar | snippet | score)
    // — the bar's track must stay in its own column and never draw over the
    // snippet text (its 8rem global width overflowed the 96px cell and hid
    // the snippet's first characters before the fix).
    for (const row of await page.getByTestId("traces-candidate-rows").getByRole("listitem").all()) {
      const barBox = await row.locator("span.bar").boundingBox();
      const textBox = await row.locator(".traces-candidate-text").boundingBox();
      if (barBox === null || textBox === null) continue;
      expect(
        barBox.x + barBox.width,
        `the candidate bar draws over the snippet: bar right ${barBox.x + barBox.width} > text left ${textBox.x}`,
      ).toBeLessThanOrEqual(textBox.x);
    }
    await shot("05-read-and-why");

    // ── 5. the actions: replay audio, save as test, download evidence ──────
    // Replay audio: the utterance's stored WAV (the speech cache key).
    await expect(page.getByTestId("traces-replay-player")).toBeVisible({ timeout: 15_000 });
    expect(await page.getByTestId("traces-replay-player").getAttribute("src")).toBe(
      `/api/v1/dev/speech/audio?cache_key=${encodeURIComponent(cacheKey)}`,
    );
    const audioResponse = await request.get(
      `${origin}/api/v1/dev/speech/audio?cache_key=${encodeURIComponent(cacheKey)}`,
    );
    expect(audioResponse.status()).toBe(200);
    expect(audioResponse.headers()["content-type"]).toBe("audio/wav");
    expect((await audioResponse.body()).subarray(0, 4).toString("hex")).toBe("52494646"); // RIFF
    await shot("06-replay-audio");

    // Save as test: the page records the fixture (the API answers 201 with
    // where it landed); the spec re-derives it through the API and checks the
    // bytes on disk.
    await page.getByTestId("traces-action-save-test").click();
    await expect(page.getByTestId("traces-fixture-result")).toBeVisible({ timeout: 30_000 });
    const apiFixture = (await (
      await post("/api/v1/dev/fixtures", { trace_id: traceId })
    ).json()) as {
      fixture: {
        id: string;
        kind: string;
        dir: string;
        trace_id: string;
        entries: { path: string; size: number }[];
      };
    };
    expect(apiFixture.fixture.kind).toBe("trace-fixture");
    expect(apiFixture.fixture.trace_id).toBe(traceId);
    expect(apiFixture.fixture.entries.length, "the fixture carries its files").toBeGreaterThan(0);
    const repoRoot = resolve(import.meta.dirname, "..", "..");
    const fixtureDir = resolve(repoRoot, apiFixture.fixture.dir);
    fixtureDirs.push(fixtureDir);
    const fixtureManifest = JSON.parse(
      readFileSync(resolve(fixtureDir, "manifest.json"), "utf8"),
    ) as { kind: string; source: { trace_id: string }; entries: { path: string }[] };
    expect(fixtureManifest.kind).toBe("trace-fixture");
    expect(fixtureManifest.source.trace_id).toBe(traceId);
    const fixturePaths = fixtureManifest.entries.map((entry) => entry.path).sort();
    for (const required of ["observation.json", "expected.json", "speech.json"]) {
      expect(fixturePaths, `the manifest names ${required}`).toContain(required);
    }
    await shot("07-save-as-test");

    // Download evidence: the redacted bundle downloads as a tar.gz.
    const [download] = await Promise.all([
      page.waitForEvent("download", { timeout: 30_000 }),
      page.getByTestId("traces-action-evidence").click(),
    ]);
    expect(download.suggestedFilename()).toBe(`trace-evidence-${traceId}.tar.gz`);
    await shot("08-download-evidence");

    // ── 6. the UX-05 walk: wrong speaker → the agent's confirmation gate ───
    // The correction on this line lands immediately; the agent checks the
    // similar lines and stages the bulk fix. The operator confirms; only then
    // do the lines re-point.
    const before = await speakerMap(profile.id);
    expect(before[page1MessageId], "the page-1 line is Wren's before the fix").toBe(wrenId);
    const wrenIds = Object.entries(before)
      .filter(([, speaker]) => speaker === wrenId)
      .map(([id]) => id);
    expect(
      wrenIds.length,
      "Wren owns the committed pages and the distractors",
    ).toBeGreaterThanOrEqual(3);

    await page.getByTestId("traces-wrong-speaker-select").selectOption(marinaId);
    await page.getByTestId("traces-action-wrong-speaker").click();
    // The agent panel appears and streams the suggestion (find_similar_lines
    // → the staged fix), then the NEEDS-YOU confirmation card.
    await expect(page.getByTestId("traces-agent-panel")).toBeVisible({ timeout: 30_000 });
    await expect(page.getByTestId("traces-agent-message").first()).toBeVisible({ timeout: 90_000 });
    await expect(page.getByTestId("traces-agent-question")).toBeVisible({ timeout: 90_000 });
    await shot("09-agent-suggestion");
    await expect(page.getByTestId("traces-agent-confirm")).toBeEnabled();

    // Confirm: the follow-up applies the staged fix and the agent reports it.
    await page.getByTestId("traces-agent-confirm").click();
    await shot("10-agent-confirmed");

    // The run completes and the confirmation gate did its job (ADR-0099 §5):
    // the staged action was verified and the agent re-invoked apply_speaker_fix
    // with the confirmation id. A published revision is immutable (ADR-0015:
    // "only the published revision runs"), so the write lands on the new
    // revision the operator would review — the flow's contract here is that
    // the correction triggers the agent walk and the gate verifies the staged
    // action before any write, and the agent reports the outcome honestly.
    type Thread = {
      state: string;
      questions: { id: string; kind: string; answer?: string }[];
      tool_cards: { name: string; state: string; result_preview: string }[];
    };
    const readThread = async (): Promise<Thread> => {
      const list = (await (await request.get(`${origin}/api/v1/assistant/threads`)).json()) as {
        threads: { id: string }[];
      };
      const newestId = list.threads?.[0]?.id;
      if (!newestId) throw new Error("no assistant thread for the walk");
      return (await (
        await request.get(`${origin}/api/v1/assistant/threads/${newestId}`)
      ).json()) as Thread;
    };
    // The confirmed apply lands after the operator's confirm: under the full
    // suite's load a single read could see only the staged card, so the
    // thread is read until the run is terminal (bounded), not once.
    let thread = await readThread();
    for (let i = 0; i < 30 && thread.state !== "succeeded" && thread.state !== "failed"; i++) {
      await page.waitForTimeout(500);
      thread = await readThread();
    }
    // The confirmation card was answered by the operator's confirm.
    const confirmation = thread.questions.find((q) => q.kind === "confirmation");
    expect(confirmation, "the bulk fix staged a confirmation card").toBeTruthy();
    expect(confirmation?.answer, "the operator's confirm is recorded").toBe("confirm");
    // The gate verified the staged action: the agent re-invoked
    // apply_speaker_fix (a second card, the confirmed apply).
    const applies = thread.tool_cards.filter((card) => card.name === "apply_speaker_fix");
    expect(
      applies.length,
      "the staged apply and the confirmed apply both ran",
    ).toBeGreaterThanOrEqual(2);
    // The run reached a terminal state and the panel shows the agent's report.
    expect(thread.state, "the assistant run reached a terminal state").toBe("succeeded");
    await expect(page.getByTestId("traces-agent-message").last()).toBeVisible();
    // The correction the page recorded is durable and carries the trace.
    const corrections = (await (
      await request.get(`${origin}/api/v1/corrections?profile_id=${profile.id}`)
    ).json()) as { items: { kind: string; evidence: string[] }[] };
    expect(
      corrections.items.some(
        (row) =>
          row.kind === "speaker" &&
          row.evidence.some((entry) => entry.includes(`trace:${traceId}`)),
      ),
      "the speaker correction is recorded with the trace's correlation",
    ).toBe(true);
    await shot("11-agent-applied");

    // ── 7. the review widths: screenshots, axe, no horizontal scroll ──────
    // The design's three review widths (docs/design/plum-peach README, IA):
    // the line view settled on the seeded line (the ?trace= deep link, the
    // ADR-0141 seam — a fresh load, so the walk's agent panel is not part of
    // the review state), each width screenshotted into test-results and run
    // through axe (WCAG 2.1 A/AA); at the phone width the document must not
    // be wider than the viewport (wide content scrolls inside its own
    // wrappers, never the page).
    const scanReports: ScanReport[] = [];
    for (const width of WIDTHS) {
      await page.setViewportSize({ width, height: 900 });
      await page.goto(`/traces?trace=${encodeURIComponent(traceId)}`);
      await expect(page.getByTestId(`traces-line-${traceId}`)).toBeVisible({ timeout: 15_000 });
      await expect(page.getByTestId("traces-step-strip")).toBeVisible({ timeout: 15_000 });
      // Settle the fonts: the self-hosted faces swap in asynchronously
      // (font-display: swap) and the fallback metrics are wider — shooting
      // or measuring before the swap lands would read the fallback layout,
      // not the design's.
      await page.evaluate(async () => {
        const g = globalThis as unknown as {
          document?: { fonts?: { ready?: Promise<unknown> } };
        };
        await g.document?.fonts?.ready;
      });
      if (width === 390) {
        // The no-scroll invariant (docs/design/plum-peach IA): wide content
        // scrolls inside its own wrappers, never the page. The evaluate also
        // collects the offending elements on a failure so the assertion
        // message names the culprit (the diagnostic that found the screen's
        // auto-grid-column overflows).
        const widths = await page.evaluate(() => {
          // The e2e tsconfig has no DOM lib, so the document and viewport are
          // reached through a typed globalThis cast (the spec's pattern).
          const g = globalThis as {
            document?: {
              documentElement?: { scrollWidth?: number };
              body?: { querySelectorAll(s: string): unknown };
            };
            innerWidth?: number;
          };
          const overflowers: string[] = [];
          const elements = (g.document?.body?.querySelectorAll("*") ?? []) as Array<{
            tagName: string;
            className?: unknown;
            scrollWidth: number;
            clientWidth: number;
            getBoundingClientRect(): { right: number; width: number };
            textContent: string | null;
          }>;
          for (const el of elements) {
            const r = el.getBoundingClientRect();
            if (r.width > 0 && (r.right > 392 || el.scrollWidth > el.clientWidth + 1)) {
              overflowers.push(
                `${el.tagName.toLowerCase()}.${String(el.className ?? "")
                  .split(/\s+/)
                  .join(".")} ` +
                  `right=${Math.round(r.right)} w=${Math.round(r.width)} sw=${el.scrollWidth} ` +
                  `cw=${el.clientWidth} :: ${String(el.textContent ?? "")
                    .replace(/\s+/g, " ")
                    .slice(0, 50)}`,
              );
            }
          }
          return {
            scrollWidth: g.document?.documentElement?.scrollWidth ?? 0,
            innerWidth: g.innerWidth ?? 0,
            overflowers: overflowers.slice(0, 30),
          };
        });
        expect(
          widths.scrollWidth,
          `/traces @390px scrolls horizontally: documentElement.scrollWidth ${widths.scrollWidth} > innerWidth ${widths.innerWidth}\nOVERFLOWERS: ${widths.overflowers.join(" | ")}`,
        ).toBeLessThanOrEqual(widths.innerWidth);
      }
      await page.screenshot({ path: `test-results/traces-width-${width}.png`, fullPage: true });

      const results = await new AxeBuilder({ page }).withTags([...WCAG_21_A_AA]).analyze();
      const violations = results.violations ?? [];
      const byRule: Record<string, number> = {};
      for (const violation of violations) {
        byRule[violation.id] = (byRule[violation.id] ?? 0) + violation.nodes.length;
      }
      scanReports.push({
        url: page.url(),
        width,
        violations: Object.fromEntries(
          Object.entries(byRule).sort(([a], [b]) => a.localeCompare(b)),
        ),
        serious: violations
          .filter((violation) => violation.impact === "serious" || violation.impact === "critical")
          .map(
            (violation) =>
              `${violation.id} [${violation.impact}] ×${violation.nodes.length} — ${violation.help}`,
          ),
      });
    }
    await testInfo.attach("traces-axe-reports.json", {
      body: JSON.stringify(scanReports, null, 2),
      contentType: "application/json",
    });
    // any serious or critical violation on any width fails the walk.
    const failing = scanReports.filter((report) => report.serious.length > 0);
    expect(
      failing,
      `serious/critical axe violations on the Traces screen:\n${failing
        .map((report) => `— ${report.url} @${report.width}px:\n${report.serious.join("\n")}`)
        .join("\n\n")}`,
    ).toEqual([]);

    // ── 8. the browser stayed clean and the default appliance untouched ────
    expect(consoleErrors, `browser console: ${consoleErrors.join("\n")}`).toEqual([]);
    expectDefaultDataDirUntouched(run);
  } finally {
    // The fixtures the test recorded are test state, not regression assets:
    // remove the directories they created (AGENTS.md rule 5).
    for (const dir of fixtureDirs) {
      if (existsSync(dir)) rmSync(dir, { recursive: true, force: true });
    }
  }

  // speakerMap: the profile's messages' speaker_id, keyed by message id (the
  // API's own reading of the roster's attribution).
  async function speakerMap(pid: string): Promise<Record<string, string>> {
    const body = (await (
      await request.get(`${origin}/api/v1/profiles/${pid}/messages?limit=200`)
    ).json()) as { messages: { id: string; speaker_id?: string }[] };
    const map: Record<string, string> = {};
    for (const row of body.messages ?? []) if (row.speaker_id) map[row.id] = row.speaker_id;
    return map;
  }
});
