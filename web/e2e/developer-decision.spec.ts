import { expect, test } from "@playwright/test";
import { randomUUID } from "node:crypto";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

import { trackConsoleErrors } from "./support/console";
import { buildManifest, type SeedProfile } from "./support/manifest";
import { expectDefaultDataDirUntouched, serverRun } from "./support/servers";
import { ports } from "./support/ports";

/**
 * The milestone 2 acceptance path for the inspector's decision half
 * (docs/spec/16: "did it speak, and why not"; ADR-0055): a real replay session
 * on the packaged binary whose ambiguous ranking is sent to a *scripted*
 * System One / Clef-Omni instance configured as the decision role — a
 * synthetic stub, labelled as such, exactly like web/e2e/fake-model.mjs is a
 * scripted gateway, not a model. The inspector must show frame → OCR →
 * candidates → Clef decision → commit → speech as one correlated trace: the
 * decision row carries the request id the scripted instance logged (the
 * client-minted X-Request-Id — the instance answers no request-id header, the
 * collective gateway's documented behaviour), the commit and the speech rows
 * share the trace, and the endpoint-down case shows the degraded local commit
 * with the reason the wiring recorded.
 *
 * Both cases run on one appliance: the first test drives the session with the
 * scripted instance answering, and the second toggles it into down mode (every
 * /systemone connection is torn down — the endpoint-unreachable class) and
 * drives a fresh session against the same appliance.
 */

const SEED_PATH = resolve(
  import.meta.dirname,
  "..",
  "..",
  "testdata",
  "synthetic",
  "profiles",
  "decision-ambiguous-001.json",
);
const EDITION = {
  slug: "synthetic-decision",
  title: "Synthetic Decision",
  region: "us",
  language: "en",
  revision: "rev0",
};
const ADMIN = { username: "decision-admin", password: "correct horse battery" };
const FAKE_CLEF = `http://127.0.0.1:${ports.fakeClef}`;

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

interface ClefLogEntry {
  request_id: string;
  model: string;
  candidate_choice: string | null;
}

const sessionTimeout = 180_000;

/** The appliance preparation the two cases share: admin, the synthetic
 *  profile imported and published, the message name → store id mapping. */
async function prepareAppliance(
  origin: string,
  request: import("@playwright/test").APIRequestContext,
) {
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
  expect(profile.id).toBeTruthy();

  let etagResponse = await request.get(`${origin}/api/v1/profiles/${profile.id}`);
  expect(etagResponse.status()).toBe(200);
  let etag = etagResponse.headers()["etag"];
  expect(etag, "a profile read carries an ETag").toBeTruthy();
  const toReview = await post(
    `/api/v1/profiles/${profile.id}/status`,
    { to: "review" },
    {
      "if-match": etag,
    },
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
  expect(storeIds["demo-page-1"], "demo-page-1 is a stored message").toBeTruthy();
  return { profileId: profile.id, storeIds, post };
}

/** Start the replay from the page and wait for the terminal snapshot. */
async function runReplay(
  page: import("@playwright/test").Page,
  origin: string,
  request: import("@playwright/test").APIRequestContext,
) {
  await page.getByTestId("session-start").click();
  await expect(page.getByTestId("session-state")).toHaveText("stopped", {
    timeout: sessionTimeout,
  });
  const sessionId = (await page.getByTestId("session-id").textContent()) as string;
  expect(sessionId).toBeTruthy();
  const session = (await (await request.get(`${origin}/api/v1/sessions/${sessionId}`)).json()) as {
    state: string;
    reason?: string;
    stats: { commits: number; ocr_calls: number };
  };
  expect(session.state).toBe("stopped");
  expect(session.reason).toBe("source_end");
  // The clip's two pages commit (ADR-0024) — with or without the model's
  // answer, the local evidence carries the run.
  expect(session.stats.commits, "the synthetic clip commits its two pages").toBe(2);
  expect(session.stats.ocr_calls).toBeGreaterThan(0);
  return sessionId;
}

/** The session's durable events (all of them, in seq order). */
async function sessionEvents(
  origin: string,
  request: import("@playwright/test").APIRequestContext,
  sessionId: string,
): Promise<DevEvent[]> {
  const body = (await (
    await request.get(
      `${origin}/api/v1/dev/events?session_id=${encodeURIComponent(sessionId)}&limit=1000`,
    )
  ).json()) as { events: DevEvent[] };
  return body.events;
}

test.describe("the inspector's Clef decision chain", () => {
  test("shows the decision with the scripted instance's request id, on the same trace as the commit and speech", async ({
    page,
    request,
    baseURL,
  }, testInfo) => {
    test.skip(
      testInfo.project.name !== "developer-decision",
      "runs on the developer-decision project's embedded-binary backend",
    );
    test.setTimeout(300_000);
    const origin = new URL(baseURL as string).origin;
    const run = serverRun("developer-decision");
    const consoleErrors = trackConsoleErrors(page);
    const shot = (step: string) =>
      page.screenshot({ path: `test-results/developer-decision-${step}.png`, fullPage: true });

    const { profileId } = await prepareAppliance(origin, request);

    // The scripted instance answers (test 2 of this project flips it to down).
    const modeUp = await request.post(`${FAKE_CLEF}/_mode`, { data: { mode: "answer" } });
    expect(modeUp.status(), `fake-clef answer mode: ${await modeUp.text()}`).toBe(200);
    const logBefore = (
      (await (await request.get(`${FAKE_CLEF}/_requests`)).json()) as {
        requests: ClefLogEntry[];
      }
    ).requests.length;

    await page.goto("/developer");
    await expect(page).toHaveURL(/\/login/);
    await page.getByTestId("login-username").fill(ADMIN.username);
    await page.getByTestId("login-password").fill(ADMIN.password);
    await page.getByTestId("login-submit").click();
    await expect(page).toHaveURL(/\/developer$/);
    await expect(page.getByTestId("session-profile")).toHaveValue(profileId, { timeout: 15_000 });
    await expect(page.getByTestId("session-dev-trace")).toBeChecked();

    const sessionId = await runReplay(page, origin, request);
    const events = await sessionEvents(origin, request, sessionId);

    // The ambiguous rankings of the clip's reveals were sent to the scripted
    // instance: decision.requested and decision.resolved ride the session's
    // events, and the instance logged at least one of them.
    const decisions = events.filter((event) => event.name === "decision.resolved");
    expect(decisions.length, "the session asked the decision role").toBeGreaterThan(0);
    const log = (
      (await (await request.get(`${FAKE_CLEF}/_requests`)).json()) as {
        requests: ClefLogEntry[];
      }
    ).requests;
    expect(log.length, "the scripted instance answered the session's calls").toBeGreaterThan(
      logBefore,
    );
    const loggedIds = new Set(log.map((entry) => entry.request_id));
    const applied = decisions.find((event) => event.data.accepted === true);
    expect(applied, "one adjudication carried a valid answer").toBeTruthy();
    const appliedData = applied!.data;
    const appliedReqId = appliedData.request_id as string;
    expect(appliedReqId, "the resolved event carries the correlation id").toBeTruthy();
    expect(loggedIds.has(appliedReqId), "the id is the one the scripted instance logged").toBe(
      true,
    );
    // The instance answers no request-id header, so the correlation id is the
    // client-minted X-Request-Id — the event names that, and the UI must.
    expect(appliedData.request_id_source).toBe("client");

    // The applied answer maps back to a candidate of the local ranking, under
    // the generation the policy asked (the fake picks c1, the top candidate).
    expect(appliedData.provider).toBe("systemone");
    expect(appliedData.model).toBe("fake-clef");
    expect(typeof appliedData.generation).toBe("number");
    expect(typeof appliedData.observation_seq).toBe("number");
    expect(appliedData.latency_ms).toBeGreaterThanOrEqual(0);
    expect(appliedData.message_id, "the answer narrows to a local candidate").toBeTruthy();

    // The page-1 commit is the first occurrence's; the scripted answer may
    // have narrowed it to a sibling candidate (the model narrows, never
    // supplies — its c1 is always a local candidate), so the assertion is on
    // the chain, not on a fixed message: the commit carries its trace, and
    // the trace carries the decisions the occurrence asked.
    const commits = events.filter((event) => event.name === "message.committed");
    expect(commits.length, "the clip commits its two pages").toBe(2);
    const page1 = commits[0];
    const decisionEventsInPage1Trace = events.filter(
      (event) => event.name === "decision.resolved" && event.trace_id === page1.trace_id,
    );
    expect(
      decisionEventsInPage1Trace.length,
      "the page-1 trace carries the decisions it asked",
    ).toBeGreaterThan(0);
    const traceDecision = decisionEventsInPage1Trace[decisionEventsInPage1Trace.length - 1];

    // The inspector: pin the page-1 commit. The detail panel shows the
    // candidates, the Clef decision with the scripted instance's request id,
    // and the commit — and the open trace's chain lists them all: OCR,
    // candidates, the decision row with the same request id, the commit and
    // the speech rows on the same trace.
    await page.getByTestId(`event-row-${page1.seq}`).click();
    await expect(page.getByTestId("commit-message")).not.toHaveText("");
    const decisionBlock = page.getByTestId("decision-block");
    await expect(decisionBlock).toBeVisible();
    await expect(page.getByTestId("decision-outcome")).toContainText("accepted");
    await expect(page.getByTestId("decision-provider-model")).toContainText("fake-clef");
    await expect(page.getByTestId("decision-request-id")).toContainText(
      String(traceDecision.data.request_id),
    );
    await expect(page.getByTestId("decision-request-source")).toHaveText("(client-issued)");
    await expect(page.getByTestId("decision-latency")).toHaveText(/\d+ ms/);
    await expect(page.getByTestId("decision-generation")).toContainText(
      String(traceDecision.data.generation),
    );
    await shot("01-decision-block");

    // The decision the panel shows is the trace's own (its request id is the
    // one the scripted instance logged — the correlation the spec demands).
    const shownReqId = (
      (await page.getByTestId("decision-request-id").locator("code").textContent()) ?? ""
    ).trim();
    expect(loggedIds.has(shownReqId), "the panel's request id is the instance's").toBe(true);

    const chain = await page.locator("li[data-testid^='trace-event-']").allTextContents();
    const chainText = chain.join("\n");
    expect(chainText).toContain("text.observed");
    expect(chainText).toContain("message.candidates");
    expect(chainText).toContain("decision.resolved");
    expect(chainText).toContain("message.committed");
    expect(chainText).toContain("speech"); // the committed line's speech rows share the trace
    expect(chainText).toContain(String(traceDecision.data.request_id));
    const traceDecisionRow = page.locator("li[data-testid^='trace-event-']").filter({
      has: page.getByTestId("trace-decision-request").filter({
        hasText: String(traceDecision.data.request_id),
      }),
    });
    await expect(traceDecisionRow).toBeVisible();
    await shot("02-trace-chain");

    expect(consoleErrors, `browser console: ${consoleErrors.join("\n")}`).toEqual([]);
    expectDefaultDataDirUntouched(run);
  });

  test("shows the degraded local commit when the decision endpoint is down", async ({
    page,
    request,
    baseURL,
  }, testInfo) => {
    test.skip(
      testInfo.project.name !== "developer-decision",
      "runs on the developer-decision project's embedded-binary backend",
    );
    test.setTimeout(300_000);
    const origin = new URL(baseURL as string).origin;
    const run = serverRun("developer-decision");
    const consoleErrors = trackConsoleErrors(page);
    const shot = (step: string) =>
      page.screenshot({ path: `test-results/developer-decision-${step}.png`, fullPage: true });

    // The appliance is already claimed and seeded by the previous test; the
    // page signs in and drives a fresh session. The scripted instance goes
    // down first: every /systemone connection is torn down, the
    // endpoint-unreachable class the wiring degrades on. The API context is a
    // fresh cookie jar for this test, so it signs in on its own before it may
    // read the session's durable events.
    const modeAck = await request.post(`${FAKE_CLEF}/_mode`, { data: { mode: "down" } });
    expect(modeAck.status(), `fake-clef down: ${await modeAck.text()}`).toBe(200);
    const login = await request.post(`${origin}/api/v1/auth/login`, {
      data: { username: ADMIN.username, password: ADMIN.password },
    });
    expect(login.status(), `admin login: ${await login.text()}`).toBe(200);
    const logBefore = (
      (await (await request.get(`${FAKE_CLEF}/_requests`)).json()) as {
        requests: ClefLogEntry[];
      }
    ).requests.length;

    await page.goto("/developer");
    await page.getByTestId("login-username").fill(ADMIN.username);
    await page.getByTestId("login-password").fill(ADMIN.password);
    await page.getByTestId("login-submit").click();
    await expect(page).toHaveURL(/\/developer$/);
    // The previous test's profile is the appliance's only published revision;
    // the selector lists exactly those and picks the first.
    await expect(page.getByTestId("session-profile")).toHaveValue(/./, { timeout: 15_000 });

    await page.getByTestId("session-start").click();
    await expect(page.getByTestId("session-state")).toHaveText("stopped", {
      timeout: sessionTimeout,
    });
    const sessionId = (await page.getByTestId("session-id").textContent()) as string;

    const events = await sessionEvents(origin, request, sessionId);
    // The session ran to its end on the local evidence: both pages committed.
    const commits = events.filter((event) => event.name === "message.committed");
    expect(commits.length).toBe(2);
    // The ambiguous rankings still asked for a decision — the call was just
    // torn down — so the resolved events are the record of the degradation.
    const decisions = events.filter(
      (event) => event.name === "decision.resolved" && event.data.accepted === false,
    );
    expect(decisions.length, "the failures went on the session's events").toBeGreaterThan(0);
    for (const event of decisions) {
      expect(String(event.data.reason)).toMatch(/^provider_error/);
    }
    // The scripted instance answered nothing: no new entries in its log.
    const logAfter = (
      (await (await request.get(`${FAKE_CLEF}/_requests`)).json()) as {
        requests: ClefLogEntry[];
      }
    ).requests.length;
    expect(logAfter).toBe(logBefore);

    // The inspector's decision row for the page-1 commit says the commit ran
    // locally and why: the wiring's reason, on the degraded row.
    const page1 = commits[0];
    await page.getByTestId(`event-row-${page1.seq}`).click();
    const decisionBlock = page.getByTestId("decision-block");
    await expect(decisionBlock).toBeVisible();
    await expect(page.getByTestId("decision-outcome")).toContainText(
      "local commit (decision unavailable:",
    );
    expect(await decisionBlock.evaluate((el) => el.className)).toContain("degraded");
    expect(
      await page.getByTestId("decision-request-id").locator("code").textContent(),
      "the failed call still carries the client-minted correlation id",
    ).toMatch(/^[0-9A-HJKMNP-TV-Z]{26}$/);
    expect(
      await page.getByTestId("decision-request-source").textContent(),
      "a failed call's error carries the effective id but not its origin",
    ).toContain("origin unknown");
    // The commit row still shows the local evidence's outcome.
    await expect(page.getByTestId("commit-message")).not.toHaveText("");
    await shot("03-degraded-decision");
    expect(consoleErrors, `browser console: ${consoleErrors.join("\n")}`).toEqual([]);
    expectDefaultDataDirUntouched(run);
  });
});
