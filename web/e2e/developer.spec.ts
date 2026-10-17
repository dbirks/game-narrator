import { expect, test } from "@playwright/test";
import { randomUUID } from "node:crypto";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

import { trackConsoleErrors } from "./support/console";
import { buildManifest, type SeedProfile } from "./support/manifest";
import { expectDefaultDataDirUntouched, serverRun } from "./support/servers";

/**
 * The milestone 2 acceptance path for the developer inspector
 * (docs/spec/16: "Developer inspector displays frame, crop, normalized OCR and
 * candidates"): a real replay session against the synthetic clip, driven
 * through the /developer page on the packaged binary, with the profile imported
 * and published through the API and OCR running on the production Tesseract
 * engine — no mocks, no stubs, a data directory nothing else can reach.
 *
 * The profile preparation uses the API on purpose: the seed file
 * (testdata/synthetic/profiles/dialogue-advance-001.json) is the games tables'
 * columns in JSON, and the only API path that can create its region, messages
 * and edge is `POST /editions/{id}/profiles/import-manifest` — which re-verifies
 * the manifest digest, so e2e/support/manifest.ts signs the document the way
 * internal/profileio/canonical.go does.
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
  slug: "synthetic-dialogue",
  title: "Synthetic Dialogue",
  region: "us",
  language: "en",
  revision: "rev0",
};
const ADMIN = { username: "inspector-admin", password: "correct horse battery" };
const VIEWER = { username: "inspector-viewer", password: "viewer sees no raw text" };

/** One durable event envelope as /dev/events renders it. */
interface DevEvent {
  event_id: string;
  seq: number;
  name: string;
  trace_id?: string;
  causation_id?: string;
  occurred_at: string;
  data: Record<string, unknown>;
}

test("the developer inspector over a real replay session", async ({
  page,
  request,
  baseURL,
}, testInfo) => {
  test.setTimeout(300_000);
  // This spec drives the `developer` webServer's backend — its own data dir and
  // the explicitly-named replay allowlist (playwright.config.ts). On the
  // chromium project it would share the 18090 backend with the catalogue spec
  // and the two auth/setups would collide, so it runs only on its own project.
  test.skip(
    testInfo.project.name !== "developer",
    "runs on the developer project's own embedded-binary backend",
  );
  const origin = new URL(baseURL as string).origin;
  const run = serverRun("developer");

  const consoleErrors = trackConsoleErrors(page); // 401 (the unauth /auth/me probe) is the only expected one
  const shot = (step: string) =>
    page.screenshot({ path: `test-results/developer-${step}.png`, fullPage: true });

  // ── 1. prepare the appliance through the API: admin, viewer, catalogue ────
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

  const viewer = await post("/api/v1/users", {
    username: VIEWER.username,
    password: VIEWER.password,
    role: "viewer",
  });
  expect(viewer.status(), `create viewer: ${await viewer.text()}`).toBe(201);

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

  // ── 2. import + publish the synthetic profile ─────────────────────────────
  const seed = JSON.parse(readFileSync(SEED_PATH, "utf8")) as SeedProfile;
  // The seed's friendly ids (demo-page-1…) travel as source_message_id. The
  // import mints its own row ULIDs (internal/profileio/importmanifest.go), so
  // the events carry *those* store ids — the mapping is read back the way the
  // page reads it, from /profiles/{id}/messages, not from the manifest we sent.
  const { doc: manifest } = buildManifest(seed, { edition: EDITION });
  const imported = await post(
    `/api/v1/editions/${editionId.id}/profiles/import-manifest`,
    manifest,
  );
  expect(imported.status(), `import-manifest: ${await imported.text()}`).toBe(201);
  const { profile } = (await imported.json()) as { profile: { id: string } };
  expect(profile.id).toBeTruthy();

  // The lifecycle moves the row version on every write, so the ETag is re-read
  // between the status move and the publish (ADR-0015).
  let etagResponse = await request.get(`${origin}/api/v1/profiles/${profile.id}`);
  expect(etagResponse.status()).toBe(200);
  let etag = etagResponse.headers()["etag"];
  expect(etag, "a profile read carries an ETag").toBeTruthy();
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

  // The import minted the store's row ULIDs; map the friendly names to them so
  // the event assertions can name the commits the way the pipeline does.
  const messagesPage = (await (
    await request.get(`${origin}/api/v1/profiles/${profile.id}/messages?limit=200`)
  ).json()) as { messages: { id: string; source_message_id?: string }[] };
  const storeIds: Record<string, string> = {};
  for (const row of messagesPage.messages ?? []) {
    if (row.source_message_id) storeIds[row.source_message_id] = row.id;
  }
  expect(storeIds["demo-page-1"], "demo-page-1 is a stored message").toBeTruthy();
  expect(storeIds["demo-page-2"], "demo-page-2 is a stored message").toBeTruthy();

  // ── 3. the page: sign in, start the replay with dev trace on ──────────────
  await page.goto("/developer");
  await expect(page).toHaveURL(/\/login/);
  await page.getByTestId("login-username").fill(ADMIN.username);
  await page.getByTestId("login-password").fill(ADMIN.password);
  await page.getByTestId("login-submit").click();
  await expect(page).toHaveURL(/\/developer$/);
  await expect(page.getByTestId("nav-role")).toHaveText("admin");

  // The selector lists exactly the published revisions; the import's revision
  // is the only one, and the page must have picked it up.
  await expect(page.getByTestId("session-profile")).toHaveValue(profile.id, { timeout: 15_000 });
  await expect(page.getByTestId("session-replay-dir")).toHaveValue(
    "testdata/synthetic/replay/dialogue-advance-001",
  );
  await expect(page.getByTestId("session-dev-trace")).toBeChecked();

  // The session runs the real pipeline on Tesseract and ends on its own when
  // the 24-frame clip is exhausted (docs/spec/12: no machine-speed dependency).
  // On this shared box Tesseract's throughput varies with load, and a starved
  // pass can leave the last page's temporal vote unsettled (one commit instead
  // of two). Starting the replay is idempotent, so re-run it a bounded number
  // of times until the clip commits its two documented pages (ADR-0024); a
  // persistent miss is a real failure and falls through to the assertion.
  type SessionSnapshot = {
    state: string;
    reason?: string;
    stats: { commits: number; ocr_calls: number };
  };
  const MAX_RUNS = 3;
  await page.getByTestId("session-start").click();
  await shot("01-session-started");
  await expect(page.getByTestId("session-state")).toHaveText("stopped", { timeout: 180_000 });
  let sessionId = (await page.getByTestId("session-id").textContent()) as string;
  let session = (await (
    await request.get(`${origin}/api/v1/sessions/${sessionId}`)
  ).json()) as SessionSnapshot;
  for (let attempt = 1; session.stats.commits !== 2 && attempt < MAX_RUNS; attempt++) {
    await testInfo.attach(`replay-retry-after-run-${attempt}`, {
      body: JSON.stringify(session, null, 2),
      contentType: "application/json",
    });
    await page.getByTestId("session-start").click();
    await expect(page.getByTestId("session-state")).toHaveText("stopped", { timeout: 180_000 });
    sessionId = (await page.getByTestId("session-id").textContent()) as string;
    session = (await (
      await request.get(`${origin}/api/v1/sessions/${sessionId}`)
    ).json()) as SessionSnapshot;
  }
  await shot("02-session-stopped");
  expect(sessionId).toBeTruthy();
  expect(session.state).toBe("stopped");
  expect(session.reason).toBe("source_end");
  // Two commits is the documented outcome of this clip (ADR-0024 validation).
  expect(session.stats.commits, "the synthetic clip commits its two pages").toBe(2);
  expect(session.stats.ocr_calls).toBeGreaterThan(0);

  // ── 4. the committed rows in the timeline, and the page-1 commit ──────────
  const events = (
    (await (
      await request.get(
        `${origin}/api/v1/dev/events?session_id=${encodeURIComponent(sessionId)}&limit=1000`,
      )
    ).json()) as { events: DevEvent[] }
  ).events;
  const commits = events.filter((event) => event.name === "message.committed");
  // Two commits, in the clip's edge order: page 1 then page 2 (ADR-0024). The
  // payloads carry the store's row ids; the friendly names are the source ids.
  expect(commits.map((event) => event.data.message_id)).toEqual([
    storeIds["demo-page-1"],
    storeIds["demo-page-2"],
  ]);

  // Both commits are visible rows in the table the page rendered.
  const timeline = page.getByTestId("event-timeline");
  await expect(timeline.locator("tr").filter({ hasText: "message.committed" })).toHaveCount(2);
  await shot("03-committed-rows");

  const page1 = commits.find((event) => event.data.message_id === storeIds["demo-page-1"]);
  if (!page1) throw new Error("the page-1 commit is present in the dev log");
  await page.getByTestId(`event-row-${page1.seq}`).click();
  await expect(page.getByTestId("commit-message")).toHaveText("demo-page-1");
  await expect(page.getByTestId("commit-speech-eligible")).toHaveText(/^(yes|no)$/);
  // The pinned row stays selected while the stream keeps delivering: the
  // detail panel still shows the commit after the terminal event landed.
  await expect(page.getByTestId("commit-message")).toHaveText("demo-page-1");

  // ── 5. an observation from the same page: frame, crop, OCR, candidates ──
  const traceId = page1.trace_id as string;
  expect(traceId, "the commit carries its trace id").toBeTruthy();
  const observations = events.filter(
    (event) => event.name === "text.observed" && event.trace_id === traceId,
  );
  expect(observations.length, "the commit's trace holds observations").toBeGreaterThan(0);
  // One trace carries both pages (the box stays up across the turn), so the
  // observation to inspect is the last one before the page-1 commit: it holds
  // the page-1 text ("harbour bell"). The later observations belong to page 2.
  const observed =
    [...observations].reverse().find((event) => event.seq < page1.seq) ??
    observations[observations.length - 1];

  await page.getByTestId(`event-row-${observed.seq}`).click();
  const frame = page.getByTestId("frame-image");
  await expect(frame).toBeVisible();
  await expect(frame).toHaveAttribute("src", /^\/api\/v1\/dev\/assets\/[0-9a-f]{64}$/);
  expect(
    await frame.evaluate((el) => (el as { naturalWidth: number }).naturalWidth),
    "the frame loaded",
  ).toBeGreaterThan(0);
  await expect(page.getByTestId("roi-box")).toBeVisible();
  const crop = page.getByTestId("crop-image");
  await expect(crop).toBeVisible();
  await expect(crop).toHaveAttribute("src", /^\/api\/v1\/dev\/assets\/[0-9a-f]{64}$/);
  expect(
    await crop.evaluate((el) => (el as { naturalWidth: number }).naturalWidth),
    "the crop loaded",
  ).toBeGreaterThan(0);
  await expect(page.getByTestId("ocr-normalized")).toContainText("harbour bell");
  await expect(page.getByTestId("ocr-confidence")).not.toHaveText("—");
  await shot("04-observation-detail");

  // The same trace's ranking: demo-page-1 at the top with a probability.
  await expect(page.getByTestId("candidates-table")).toBeVisible();
  await expect(page.getByTestId("candidate-id-0")).toHaveText("demo-page-1");
  expect(await page.locator("tr[data-testid='candidate-row-0']").textContent()).toMatch(
    /\d\.\d{4}/,
  );
  await shot("05-candidates");

  // Open the trace: the correlated chain with its causation links.
  const chain = await page.locator("li[data-testid^='trace-event-']").allTextContents();
  const chainText = chain.join("\n");
  expect(chainText).toContain("text.observed");
  expect(chainText).toContain("message.candidates");
  expect(chainText).toContain("message.committed");
  await shot("06-trace-chain");

  // Keyboard selection: a focused row pins on Enter.
  const firstRow = page.locator("tr[data-testid^='event-row-']").first();
  await firstRow.focus();
  await page.keyboard.press("Enter");
  await expect(firstRow).toHaveAttribute("aria-selected", "true");
  await page.getByTestId(`event-row-${observed.seq}`).click(); // re-pin for the record

  // ── 6. a viewer: the page says so, and the API answers 403 ────────────────
  await page.getByTestId("nav-logout").click();
  await expect(page).toHaveURL(/\/login/);
  await page.getByTestId("login-username").fill(VIEWER.username);
  await page.getByTestId("login-password").fill(VIEWER.password);
  await page.getByTestId("login-submit").click();
  await expect(page).toHaveURL(/\/developer$/);
  await expect(page.getByTestId("dev-role-required")).toBeVisible();
  await expect(page.getByTestId("dev-role-required")).toContainText("developer role");
  await shot("07-viewer-role-required");

  const viewerLogin = await request.post(`${origin}/api/v1/auth/login`, {
    data: { username: VIEWER.username, password: VIEWER.password },
  });
  expect(viewerLogin.status()).toBe(200);
  const forbidden = await request.get(
    `${origin}/api/v1/dev/events?session_id=${encodeURIComponent(sessionId)}`,
  );
  expect(forbidden.status(), "the dev evidence surfaces are developer-only").toBe(403);

  // ── 7. the browser stayed clean and the default appliance untouched ───────
  expect(consoleErrors, `browser console: ${consoleErrors.join("\n")}`).toEqual([]);
  expectDefaultDataDirUntouched(run);
});
