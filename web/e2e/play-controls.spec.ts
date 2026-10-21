import AxeBuilder from "@axe-core/playwright";
import { expect, test } from "@playwright/test";
import { readFileSync } from "node:fs";
import { randomUUID } from "node:crypto";
import { resolve } from "node:path";

import { trackConsoleErrors } from "./support/console";
import { buildManifest, type SeedProfile } from "./support/manifest";
import { setPlayView } from "./support/play";
import { apiAsUser, expectDefaultDataDirUntouched, serverRun } from "./support/servers";

/**
 * The player's controls (docs/spec/08 "…/pause, …/resume, …/settings",
 * docs/spec/09 "/play": pause/mute, the narrated classes "Dialogue / +Signs /
 * +Items", the Conservative / Predictive toggle, keyboard access), on the
 * packaged binary with the repo's testdata replay clip and the scripted TTS
 * roles (synthetic stubs at the model boundary only — AGENTS.md rule 3; the
 * server, store, pipeline and stream are the real thing):
 *
 *   1. seed + publish the synthetic profile through the API (the same
 *      dialogue-advance-001 seed the player walk uses) and approve + register
 *      a scripted-TTS voice for Marina, so the committed lines have a voice
 *      to speak and the pause has real audio to fence;
 *   2. /play: start the replay session, then the decision mode and the
 *      narrated classes move through the controls: the checkboxes and the
 *      careful/eager segmented toggle (ADR-0093's 2e control bar: the mode
 *      select became the design's segment) post the partial settings, the
 *      200 snapshot re-syncs the page (the "Narrating: …" line reads back
 *      the applied state), and the server's snapshot agrees;
 *   3. the keyboard shortcuts (s/i/m) drive the same controls from the
 *      window, not the forms;
 *   4. pause at the head of the session — the button follows the server's
 *      snapshot (aria-pressed, label "Resume"), and the server's own view
 *      says paused=true — then resume (the keyboard `p` takes the same
 *      path): at the head the clip's whole lifetime is ahead of the pause
 *      window whatever the host's OCR speed (issue #23);
 *   5. the session settles at exactly the clip's two commits: the paused
 *      window's lines are neither lost nor replayed;
 *   6. the "Current committed line" region is an aria-live polite region,
 *      and axe finds no serious/critical WCAG 2.1 A/AA violation in the
 *      live-session state.
 */

const EDITION = {
  slug: "play-controls-harbour",
  title: "Harbour Light (Controls)",
  region: "us",
  language: "en",
  revision: "rev0",
};
const ADMIN = { username: "playctl-admin", password: "steady as she goes" };
const CONSENT = "original_synthetic_design";

/** WCAG 2.1 A/AA: the 2.0 A/AA rules plus the rules 2.1 adds at those levels. */
const WCAG_21_A_AA = ["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"] as const;

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
  paused?: boolean;
  mode: "conservative" | "predictive";
  narrated_classes?: string[];
  stats: { commits: number; ocr_calls: number };
}

test("play controls: pause/resume, narrated classes, decision mode (synthetic replay)", async ({
  page,
  request,
  baseURL,
}, testInfo) => {
  test.setTimeout(600_000);
  test.skip(
    testInfo.project.name !== "play-controls",
    "runs on the play-controls project's own backend",
  );
  const run = serverRun("play-controls");
  const origin = new URL(baseURL as string).origin;
  const api = apiAsUser(request, origin);
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

  // ── 1. claim, seed + publish the synthetic profile through the API ────────
  const setup = await request.post(`${origin}/api/v1/auth/setup`, {
    data: { username: ADMIN.username, password: ADMIN.password },
  });
  // A standalone run claims the appliance; a repeat against this project's
  // dedicated server (this spec's own earlier run) already claimed it with
  // this same admin — the documented 409 (setup is once per appliance,
  // ADR-0016 §7) still signs the login below.
  expect([201, 409], `auth setup: ${await setup.text()}`).toContain(setup.status());
  await request.post(`${origin}/api/v1/auth/login`, {
    data: { username: ADMIN.username, password: ADMIN.password },
  });

  // A per-run slug: a repeat against this project's dedicated server (this
  // spec's own earlier run) must not hit the UNIQUE slug — the idempotency
  // key is fresh per request, so the same slug is a 409, not a replay.
  const slug = `${EDITION.slug}-${randomUUID().slice(0, 8)}`;
  const game = await api.post("/api/v1/games", {
    data: { slug, title: EDITION.title },
    headers: { "idempotency-key": randomUUID() },
  });
  expect(game.status(), `create game: ${await game.text()}`).toBe(201);
  const gameId = (await game.json()) as { id: string };

  const edition = await api.post(`/api/v1/games/${gameId.id}/editions`, {
    data: { region: EDITION.region, language: EDITION.language, revision: EDITION.revision },
    headers: { "idempotency-key": randomUUID() },
  });
  expect(edition.status(), `create edition: ${await edition.text()}`).toBe(201);
  const editionId = (await edition.json()) as { id: string };

  const { doc } = buildManifest(seedWithRoster(), { edition: EDITION });
  const imported = await api.post(`/api/v1/editions/${editionId.id}/profiles/import-manifest`, {
    data: doc,
  });
  expect(imported.status(), `import-manifest: ${await imported.text()}`).toBe(201);
  const r1 = ((await imported.json()) as { profile: { id: string } }).profile.id;

  const etagOf = async (id: string) => {
    const response = await request.get(`${origin}/api/v1/profiles/${id}`);
    expect(response.status(), "a profile read carries an ETag").toBe(200);
    return response.headers()["etag"] as string;
  };
  await api.post(`/api/v1/profiles/${r1}/status`, {
    data: { to: "review" },
    etag: await etagOf(r1),
  });
  const published = await api.post(`/api/v1/profiles/${r1}/publish`, { etag: await etagOf(r1) });
  expect(published.status(), `publish: ${await published.text()}`).toBe(200);

  // The session resolves each committed line's voice by the speaker's
  // character ROW id (messages.speaker_id), not by the roster name or a
  // friendly seed id — a voice keyed any other way never resolves and the
  // line ends skipped. Fetch the roster and name Marina's row id.
  const roster = (await (
    await request.get(`${origin}/api/v1/profiles/${r1}/characters`)
  ).json()) as { characters: { id: string; name: string; status: string }[] };
  const marina = roster.characters.find((c) => c.name === "Marina");
  expect(marina, "Marina's character row").toBeTruthy();
  const speakerRowId = marina!.id;

  // ── 2. a voice for Marina: the scripted TTS, approved with consent ────────
  // The session's speech layer resolves the speaker's approved voice, so
  // without one the committed lines end skipped and the pause has no audio
  // to fence. The audition is the durable job the response names (docs/spec/08).
  // The voice is keyed by Marina's character row id (the value the session's
  // speaker map holds), not the roster name — see the note above.
  const auditions = await api.post("/api/v1/voices/auditions", {
    data: {
      character: speakerRowId,
      brief: "A calm harbour voice, unhurried, with a slight salt air in it.",
      count: 1,
    },
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
  // The job's own report says how many clips the VoiceDesign checkpoint
  // returned (docs/spec/08: the durable job, not the create call, is the
  // source of truth).
  const auditionResult = (await (
    await request.get(`${origin}/api/v1/jobs/${audition.job_id}`)
  ).json()) as { result: { audition_count: number; audition_ids: string[] } };
  expect(auditionResult.result.audition_count).toBe(1);

  const approved = await api.post(`/api/v1/voices/${audition.voice_id}/approve`, {
    data: {
      audition_id: auditionResult.result.audition_ids[0] ?? "",
      consent: CONSENT,
    },
  });
  expect(approved.status(), `approve: ${await approved.text()}`).toBe(200);
  const registered = await api.post(`/api/v1/voices/${audition.voice_id}/register`);
  expect(registered.status(), `register: ${await registered.text()}`).toBe(200);

  // The stored voice is keyed by the character row id the session resolves —
  // the link that makes the committed lines actually speak.
  const voiceDetail = (await (
    await request.get(`${origin}/api/v1/voices/${audition.voice_id}`)
  ).json()) as { character: string; state: string; fingerprint: string };
  expect(voiceDetail.character, "the voice is keyed by the character row id").toBe(speakerRowId);
  expect(voiceDetail.state).toBe("approved");
  expect(voiceDetail.fingerprint).not.toBe("");

  // ── 3. /play: start the replay session ────────────────────────────────────
  await page.goto("/play");
  await expect(page).toHaveURL(/\/login/);
  await page.getByTestId("login-username").fill(ADMIN.username);
  await page.getByTestId("login-password").fill(ADMIN.password);
  await page.getByTestId("login-submit").click();
  await expect(page).toHaveURL(/\/play$/);

  await page.getByTestId("play-profile").selectOption(r1, { timeout: 20_000 });
  await page.getByTestId("play-start").click();
  await expect(page).toHaveURL(/\/play\?session=/);
  const sessionId = new URL(page.url()).searchParams.get("session") as string;
  expect(sessionId).toBeTruthy();
  const snapshot = async () =>
    (await (await request.get(`${origin}/api/v1/sessions/${sessionId}`)).json()) as SessionSnapshot;

  // The voice pipeline is wired on this server: the audio section reports a
  // sink, not the 503 note. The audio card lives in the operator view (the
  // 2026-10-20 review moved the session's facts out of the caption view),
  // so the walk reads it from "show workings" — the bottom control bar is
  // the same bar in both views, so every control below still reaches it.
  await setPlayView(page, "operator");
  await expect(page.getByTestId("play-audio-sink")).toBeVisible({ timeout: 30_000 });

  await expect(page.getByTestId("play-state")).toHaveText("running", { timeout: 60_000 });

  // The clip's lifetime on a real OCR engine is short (~20s for the 24-frame
  // recording), so the policy controls are driven as soon as the session
  // runs — the controls are live for any running session, commits or not —
  // and the pause/resume and the keyboard shortcuts are driven in the window
  // after the first committed line, while the second line is still pending.
  const pauseButton = page.getByTestId("play-pause");
  await expect(pauseButton).toBeEnabled();

  // ── 4. the decision mode, through the segmented toggle ───────────────────
  // ADR-0093 (Plum & Peach 2e): the pre-redesign mode select is the
  // careful/eager segment (play-mode-segmented) — the server's own
  // conservative/predictive values, the design's friendly labels.
  const modeTablist = page.getByTestId("play-mode-segmented");
  const carefulTab = modeTablist.getByRole("tab", { name: "careful" });
  const eagerTab = modeTablist.getByRole("tab", { name: "eager" });
  await expect(carefulTab).toHaveAttribute("aria-selected", "true");
  await eagerTab.click();
  await expect(eagerTab).toHaveAttribute("aria-selected", "true", { timeout: 15_000 });
  let settingsSnapshot = await snapshot();
  expect(settingsSnapshot.mode, "the server holds the applied mode").toBe("predictive");
  // The partial body left the classes at the session's value.
  expect(settingsSnapshot.narrated_classes).toEqual(["dialogue"]);

  // ── 5. the narrated classes, through the controls ─────────────────────────
  // The dialogue checkbox is locked on: the product's default, always present.
  const dialogue = page.getByTestId("play-class-dialogue");
  await expect(dialogue).toBeChecked();
  await expect(dialogue).toBeDisabled();
  await expect(page.getByTestId("play-narrated")).toHaveText("dialogue");

  const signs = page.getByTestId("play-class-signs");
  const items = page.getByTestId("play-class-items");
  // The checkboxes are React-controlled from the server's snapshot, so a
  // click does not flip them synchronously: React re-renders them from the
  // (still old) state until the PUT's 200 snapshot lands. Click, then wait
  // for the server round-trip — do not use .check(), which asserts a
  // synchronous state change a controlled input cannot honour.
  await signs.click();
  await expect(signs).toBeChecked({ timeout: 15_000 });
  await expect(page.getByTestId("play-narrated")).toHaveText("dialogue, sign", {
    timeout: 15_000,
  });
  settingsSnapshot = await snapshot();
  expect(settingsSnapshot.narrated_classes, "the server holds the applied classes").toEqual([
    "dialogue",
    "sign",
  ]);
  // The mode field was not touched by the class toggle: the partial body
  // left it at the session's value.
  expect(settingsSnapshot.mode).toBe("predictive");

  await items.click();
  await expect(items).toBeChecked({ timeout: 15_000 });
  await expect(page.getByTestId("play-narrated")).toHaveText("dialogue, sign, item", {
    timeout: 15_000,
  });
  settingsSnapshot = await snapshot();
  expect(settingsSnapshot.narrated_classes).toEqual(["dialogue", "sign", "item"]);

  // ── 6. the keyboard shortcuts drive the same controls ─────────────────────
  // The shortcuts ignore keystrokes while a form control has focus, so the
  // page's own rule is to act from the window: blur first, then press. The
  // e2e tsconfig has no DOM lib, so the evaluate callback names the shape it
  // touches (the codebase pattern, as in voice-preview-audio.spec.ts).
  const blur = () =>
    page.evaluate(() => {
      const win = globalThis as unknown as {
        document: { activeElement: { blur?: () => void } | null };
      };
      win.document.activeElement?.blur?.();
    });

  // The non-pause shortcuts are driven before any commit: the clip's lifetime
  // is the host's own OCR throughput (replay drains lossless and
  // back-pressured, never real-time paced — ~20s on a loaded host, well under
  // a second on a fast one), and the controls are live for any running
  // session, commits or not. The shortcuts finish the round-trip by restoring
  // the product's defaults, so the run itself commits under the documented
  // default policy.
  await blur();
  await page.keyboard.press("s"); // signs off
  await expect(signs).not.toBeChecked({ timeout: 15_000 });
  await blur();
  await page.keyboard.press("i"); // items off
  await expect(items).not.toBeChecked({ timeout: 15_000 });
  await blur();
  await page.keyboard.press("m"); // the mode toggles through the same control
  await expect(carefulTab).toHaveAttribute("aria-selected", "true", { timeout: 15_000 });
  await expect(page.getByTestId("play-narrated")).toHaveText("dialogue", {
    timeout: 15_000,
  });

  settingsSnapshot = await snapshot();
  expect(settingsSnapshot.mode, "the shortcuts drove the mode").toBe("conservative");
  expect(settingsSnapshot.narrated_classes, "the shortcuts drove the classes").toEqual([
    "dialogue",
  ]);

  // ── 7. pause and resume at the head of the session: the button follows the
  //     server's snapshot ─────────────────────────────────────────────────────
  // The pause window must sit at the head of the session, not after the first
  // committed line (the first draft's placement). The clip drains at the
  // host's OCR throughput, and a pause does not stop the source — the text
  // machines keep observing through it — so a pause that lands near the
  // clip's tail ends the session (source_end) before the second page's
  // commit margin is reached: on a fast host the 24-frame clip exhausts in
  // well under a second, and by the time the first line has committed only
  // the second page's tail is left. Pausing at the head proves the same
  // product behaviour — the pause stops commitment and speech, the button
  // follows the server's snapshot, the keyboard `p` takes the same path, and
  // the resume continues from the next commit — with the clip's whole
  // lifetime ahead of it, whatever the host's speed (issue #23).
  await pauseButton.click();
  await expect(pauseButton).toHaveAttribute("aria-pressed", "true", { timeout: 15_000 });
  await expect(pauseButton).toHaveText("Resume");
  const pausedSnapshot = await snapshot();
  expect(pausedSnapshot.paused, "the server's own view is paused").toBe(true);
  const commitsAtPause = pausedSnapshot.stats.commits;

  // The `p` shortcut takes the same control path as pause: resume through the
  // button, then pause and resume again from the window (the page's own rule
  // is to act from the window, so blur first).
  await pauseButton.click();
  await expect(pauseButton).toHaveAttribute("aria-pressed", "false", { timeout: 15_000 });
  await expect(pauseButton).toHaveText("Pause");
  await blur();
  await page.keyboard.press("p"); // pause, from the window
  await expect(pauseButton).toHaveAttribute("aria-pressed", "true", { timeout: 15_000 });
  await blur();
  await page.keyboard.press("p"); // and back
  await expect(pauseButton).toHaveAttribute("aria-pressed", "false", { timeout: 15_000 });

  // The server's flag is the evidence, and the read must not race the resume's
  // settle: poll the observed state to the unpaused flag under a generous
  // ceiling — a single read right after the click saw the pre-resume flag on
  // a loaded host (issue #23).
  await expect
    .poll(async () => (await snapshot()).paused, {
      timeout: 60_000,
      message: "the server's own view is unpaused",
    })
    .toBe(false);

  // ── 8. the session settles at exactly the clip's two commits ──────────────
  // The clip drains at the host's own OCR throughput (replay is lossless and
  // back-pressured, never real-time paced), so on a loaded host the settle
  // takes longer than the idle-host wall clock the first draft assumed. The
  // commits counter is the clip's own progress: poll it to two — the settle
  // is observed, not read once after the state flipped (issue #23).
  await expect
    .poll(async () => (await snapshot()).stats.commits, {
      timeout: 300_000,
      message: "the synthetic clip commits its two pages",
    })
    .toBe(2);
  // Then the session itself settles to stopped.
  await expect
    .poll(async () => (await snapshot()).state, {
      timeout: 60_000,
      message: "the session settles",
    })
    .toBe("stopped");
  const final = await snapshot();
  expect(final.state, `the session ended: ${final.reason ?? ""}`).toBe("stopped");
  expect(final.stats.commits, "the pause neither lost nor replayed a commit").toBe(
    Math.max(commitsAtPause, 2),
  );

  // The page, from the other side of the wire: the same numbers. The commit
  // counter is one of the session's facts (the operator view since the
  // 2026-10-20 review), so the read goes through "show workings".
  await expect(page.getByTestId("play-state")).toHaveText("stopped", { timeout: 30_000 });
  await setPlayView(page, "operator");
  await expect(page.getByTestId("play-commits")).toHaveText("2");
  // The committed lines themselves are the caption view's: back across.
  await setPlayView(page, "now");
  // The committed line's speaker and order, from the other side of the wire:
  // Marina's two lines, newest first (the same order the sse walk asserts) —
  // demo-page-2 committed second and tops the list, demo-page-1 is below it.
  const linesAfter = page.locator('[data-testid="recent-lines"] li');
  await expect(linesAfter).toHaveCount(2, { timeout: 30_000 });
  await expect(page.getByTestId("current-line-speaker")).toHaveText("Marina");
  await expect(linesAfter.first()).toContainText(
    "nobody in the village had seen that boat before.",
  );
  await expect(linesAfter.last()).toContainText(
    "the harbour bell rang twice before anyone looked up.",
  );

  // The voice spoke the lines through the scripted TTS: the counters moved,
  // and the utterance machinery ran (requested → started → ended). The
  // `started` counter is the authoritative server-side signal that the voice
  // resolved and audio actually began — a line the pipeline skipped (no voice)
  // ends without ever starting, so `started >= 1` proves the committed lines
  // were spoken by the approved voice, not dropped. The counters are the
  // audio card's (the operator view), so the reads go through "show
  // workings"; they are driven by the session snapshot, so poll to ride out
  // the settle.
  await setPlayView(page, "operator");
  const spoken = async (testId: string) =>
    Number((await page.getByTestId(testId).textContent()) ?? "0");
  await expect
    .poll(() => spoken("play-speech-started"), {
      timeout: 60_000,
      message: "the approved voice speaks a committed line",
    })
    .toBeGreaterThanOrEqual(1);
  expect(await spoken("play-speech-requested")).toBeGreaterThanOrEqual(1);
  expect(await spoken("play-speech-ended")).toBeGreaterThanOrEqual(1);

  // ── 9. the live region and the accessibility pass ─────────────────────────
  // The live region is the caption view's; the axe pass runs over it.
  await setPlayView(page, "now");
  const liveRegion = page.getByLabel("Current committed line");
  await expect(liveRegion).toHaveAttribute("aria-live", "polite");

  const results = await new AxeBuilder({ page }).withTags([...WCAG_21_A_AA]).analyze();
  const serious = results.violations.filter(
    (v) => v.impact === "serious" || v.impact === "critical",
  );
  expect(
    serious.map((v) => `${v.id}: ${v.nodes.length} node(s)`),
    "axe WCAG 2.1 A/AA violations on the live-session page:",
  ).toEqual([]);

  expect(consoleErrors, `browser console: ${consoleErrors.join("\n")}`).toEqual([]);
  expectDefaultDataDirUntouched(run);
});
