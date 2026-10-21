import { expect, test, type Page } from "@playwright/test";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { randomUUID } from "node:crypto";

import { trackConsoleErrors } from "./support/console";
import { buildManifest, type SeedProfile } from "./support/manifest";
import { expectDefaultDataDirUntouched, serverRun } from "./support/servers";

/**
 * The voice preview's audio (docs/spec/16 M5: "voice preview works";
 * docs/spec/09 "voice test … plays approved audio"), on the curation
 * server — the packaged binary with the speech roles and the registry
 * pointed at the scripted TTS instance (fake-tts.mjs): synthetic stub at
 * the model boundary only (AGENTS.md rule 3), the real API process, job
 * queue, speech cache and asset store behind it.
 *
 * The curation walk (curation-walk.spec.ts) runs first on the same
 * appliance and claimed it, so this spec signs in with that walk's admin
 * (a second POST /auth/setup would be the documented 409) and curates a
 * different game. What the existing voices spec does not prove, and this
 * exists to prove: the <audio> element the page renders holds the server's
 * bytes — a non-empty blob that is a real WAV, with a playable duration —
 * for the audition clip the approval froze and for the preview line, and
 * the second preview of the same line comes back from the speech cache.
 */

const EDITION = {
  slug: "preview-harbour",
  title: "Preview Harbour",
  region: "us",
  language: "en",
  revision: "rev0",
};
// The curation walk's admin: this server is the curation server (ports.ts).
// A full-suite run finds it claimed by that walk (setup is once per
// appliance, ADR-0016 §7); a standalone run claims it under the same name.
const ADMIN = { username: "curation-admin", password: "steady as the tide" };
const CHARACTER = "Marina";
const CONSENT = "original_synthetic_design";
const PREVIEW_LINE = "The road ahead was long, but the light held.";

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

/**
 * The <audio> element's own view from inside the page: the blob URL it points
 * at, its readyState and its duration once the metadata has loaded. A
 * placeholder player (no bytes, or bytes the browser cannot decode) never
 * reaches duration > 0 — the element proves its payload exists.
 *
 * (The page's own fetch cannot re-read a blob: URL — the server's CSP sets
 * connect-src 'self' (ADR-0016) — so the byte-level identity of the same
 * content is checked from outside the page, against the server's own bytes.)
 */
async function elementAudioMeta(
  page: Page,
  testid: string,
): Promise<{ src: string; readyState: number; duration: number }> {
  return page.evaluate((tid) => {
    const win = globalThis as unknown as {
      document: {
        querySelector(selector: string): {
          src: string;
          readyState: number;
          duration: number;
        } | null;
      };
    };
    const element = win.document.querySelector(`audio[data-testid="${tid}"]`);
    if (!element || !element.src) throw new Error(`the ${tid} audio element has no source`);
    return { src: element.src, readyState: element.readyState, duration: element.duration };
  }, testid);
}

/** The server's bytes for the same content, from outside the page. */
async function serverWavBytes(
  request: import("@playwright/test").APIRequestContext,
  url: string,
  init?: { method?: "GET" | "POST"; headers?: Record<string, string>; data?: unknown },
): Promise<{ size: number; riff: string; wave: string; cache: string | null }> {
  const response = await request.fetch(url, {
    method: init?.method ?? "GET",
    headers: init?.headers,
    data: init?.data,
  });
  expect(response.status(), `GET/POST ${url}: ${await response.text()}`).toBe(200);
  const body = await response.body();
  const fourcc = (offset: number) =>
    String.fromCharCode(body[offset], body[offset + 1], body[offset + 2], body[offset + 3]);
  return {
    size: body.length,
    riff: fourcc(0),
    wave: fourcc(8),
    cache: response.headers()["x-cache"] ?? null,
  };
}

test("the audition clip and the preview line are real, playable WAV blobs", async ({
  page,
  request,
  baseURL,
}, testInfo) => {
  test.setTimeout(300_000);
  test.skip(
    testInfo.project.name !== "voice-preview",
    "runs on the curation server the voice-preview project points at",
  );
  const run = serverRun("curation");
  const origin = new URL(baseURL as string).origin;
  const consoleErrors = trackConsoleErrors(page, [401]);

  // ── 1. sign in: claim a standalone run, sign in a shared one ─────────────
  // A full-suite run finds the appliance already claimed by the curation
  // walk (same server, earlier project); a standalone run claims it here.
  // Either way the session that follows is this walk's own.
  await page.goto("/library");
  const setupForm = page.getByTestId("setup-form");
  const loginUsername = page.getByTestId("login-username");
  await expect(setupForm.or(loginUsername)).toBeVisible({ timeout: 30_000 });
  if (await setupForm.isVisible()) {
    await page.getByTestId("setup-username").fill(ADMIN.username);
    await page.getByTestId("setup-password").fill(ADMIN.password);
    await page.getByTestId("setup-submit").click();
    await expect(page).toHaveURL(/\/login$/);
  }
  await loginUsername.fill(ADMIN.username);
  await page.getByTestId("login-password").fill(ADMIN.password);
  await page.getByTestId("login-submit").click();
  await expect(page.getByTestId("nav-user")).toContainText(ADMIN.username);
  await request.addCookies(await page.context().cookies());

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

  // ── 2. a game, an edition, a draft revision with a roster (API seed) ──────
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
  const profileId = ((await imported.json()) as { profile: { id: string } }).profile.id;

  // ── 3. the cast page: audition, approve, register (UI only from here) ─────
  // Cast merged the voices page (ADR-0093); the dial is switched off so the
  // manual approval path (three takes, one of which is frozen) is exercised.
  await page.goto(`/games/${gameId.id}/cast?profile=${encodeURIComponent(profileId)}`);
  await expect(page.getByTestId("voice-characters")).toBeVisible({ timeout: 20_000 });
  await page.getByTestId("auto-cast-switch").click();
  await expect(page.getByTestId("auto-cast-switch")).not.toBeChecked();
  await page.getByTestId(`cast-row-name-${CHARACTER}`).click();
  await expect(page).toHaveURL(new RegExp(`character=${encodeURIComponent(CHARACTER)}$`));
  await expect(page.getByTestId("no-voice")).toContainText(`No voice for ${CHARACTER} yet`);

  const brief = "A calm harbour voice, unhurried, with a slight salt air in it.";
  await page.getByTestId("voice-brief").fill(brief);
  const accepted = page.waitForResponse(
    (r) => r.request().method() === "POST" && r.url().includes("/api/v1/voices/auditions"),
  );
  await page.getByTestId("generate-auditions").click();
  const createResponse = (await (await accepted).json()) as {
    job_id: string;
    voice_id: string;
    state: string;
  };
  expect(createResponse.state).toBe("draft");

  await expect
    .poll(
      async () => {
        const body = (await (
          await request.get(`/api/v1/jobs/${createResponse.job_id}`)
        ).json()) as {
          state: string;
        };
        if (body.state === "failed") throw new Error(`the audition job failed`);
        return body.state;
      },
      { message: "the audition job reached a terminal state", timeout: 90_000 },
    )
    .toBe("succeeded");

  await expect(page.getByTestId("voice-state-badge")).toHaveText("auditioning", {
    timeout: 30_000,
  });
  const cards = page.getByTestId(/^audition-card-/);
  await expect(cards).toHaveCount(3);
  for (let i = 0; i < 3; i += 1) {
    await expect(page.getByTestId(`audition-player-${i}`)).toBeVisible();
    await expect(page.getByTestId(`audition-transcript-${i}`)).not.toBeEmpty();
  }

  // The audition clip the approval will freeze is still mounted while the
  // voice is auditioning (the cards unmount at approval), so the proof
  // happens here: the element points at a blob, its metadata loads (the
  // browser decoded the payload), and the server's bytes for the same clip
  // are a non-empty RIFF/WAV.
  const auditionMeta = await elementAudioMeta(page, "audition-player-0");
  expect(auditionMeta.src, "the audition clip is served to the element as a blob URL").toMatch(
    /^blob:/,
  );
  await expect
    .poll(async () => (await elementAudioMeta(page, "audition-player-0")).duration, {
      timeout: 15_000,
      message: "the audition clip's metadata loads",
    })
    .toBeGreaterThan(0);
  const voiceDetail = (await (
    await request.get(`${origin}/api/v1/voices/${createResponse.voice_id}`)
  ).json()) as { auditions: { id: string }[] };
  expect(voiceDetail.auditions, "the voice detail lists the auditions").toHaveLength(3);
  const auditionBytes = await serverWavBytes(
    request,
    `${origin}/api/v1/voices/${createResponse.voice_id}/auditions/${voiceDetail.auditions[0].id}`,
  );
  expect(auditionBytes.size, "the audition clip is not empty").toBeGreaterThan(0);
  expect(auditionBytes.riff, "the audition clip is a RIFF container").toBe("RIFF");
  expect(auditionBytes.wave, "the audition clip is a WAV").toBe("WAVE");

  // Approve candidate 0 behind the consent gate, then register it.
  await expect(page.getByTestId("consent-required-note")).toBeVisible();
  await expect(page.getByTestId("approve-voice")).toBeDisabled();
  await page.getByTestId("audition-approve-0").click();
  await expect(page.getByTestId("audition-approve-0")).toHaveText("Selected");
  await page.getByTestId("consent-input").fill(CONSENT);
  await expect(page.getByTestId("approve-voice")).toBeEnabled();
  await page.getByTestId("approve-voice").click();
  await expect(page.getByTestId("voice-state-badge")).toHaveText("approved", { timeout: 20_000 });
  await expect(page.getByTestId("voice-fingerprint")).toBeVisible();
  await expect(page.getByTestId("voice-consent")).toContainText(CONSENT);

  await expect(page.getByTestId(/^registration-state-/)).toHaveText("pending");
  const registerResponse = page.waitForResponse(
    (r) => r.request().method() === "POST" && r.url().includes("/api/v1/voices/"),
  );
  await page.getByTestId("register-voice").click();
  const registration = (await (await registerResponse).json()) as {
    registered_name: string;
    state: string;
  };
  expect(registration.state).toBe("ready");
  expect(registration.registered_name).toMatch(/^gn_/);
  await expect(page.getByTestId(/^registration-state-/)).toHaveText("ready");

  // ── 4. the preview line: MISS then HIT, and the element holds the bytes ──
  await expect(page.getByTestId("preview-panel")).toBeVisible();
  await expect(page.getByTestId("preview-line")).toHaveValue(PREVIEW_LINE);

  const firstPreview = page.waitForResponse(
    (r) => r.request().method() === "POST" && r.url().includes("/preview"),
  );
  await page.getByTestId("preview-button").click();
  expect((await firstPreview).headers()["x-cache"]).toBe("MISS");
  await expect(page.getByTestId("preview-cache")).toHaveText("MISS");
  await expect(page.getByTestId("preview-player")).toBeVisible();

  const previewMeta = await elementAudioMeta(page, "preview-player");
  expect(previewMeta.src, "the preview is served to the element as a blob URL").toMatch(/^blob:/);
  await expect
    .poll(async () => (await elementAudioMeta(page, "preview-player")).duration, {
      timeout: 15_000,
      message: "the preview audio's metadata loads",
    })
    .toBeGreaterThan(0);
  // The same line's bytes, asked of the server from outside the page: a
  // non-empty RIFF/WAV (and the second ask for the line is the cache's).
  const csrfToken =
    (await request.cookies(origin)).find((cookie) => cookie.name === "gn_csrf")?.value ?? "";
  const previewBytes = await serverWavBytes(
    request,
    `${origin}/api/v1/voices/${createResponse.voice_id}/preview`,
    {
      method: "POST",
      headers: { "content-type": "application/json", "x-csrf-token": csrfToken },
      data: { text: PREVIEW_LINE },
    },
  );
  expect(previewBytes.size, "the preview audio is not empty").toBeGreaterThan(0);
  expect(previewBytes.riff, "the preview audio is a RIFF container").toBe("RIFF");
  expect(previewBytes.wave, "the preview audio is a WAV").toBe("WAVE");

  // The identical line is the same cache key: the server serves it from the
  // speech cache and says so, and the element keeps the playable bytes.
  const secondPreview = page.waitForResponse(
    (r) => r.request().method() === "POST" && r.url().includes("/preview"),
  );
  await page.getByTestId("preview-button").click();
  expect((await secondPreview).headers()["x-cache"]).toBe("HIT");
  await expect(page.getByTestId("preview-cache")).toHaveText("HIT");
  // The element keeps a live, decodable payload after the HIT re-render.
  const previewAgain = await elementAudioMeta(page, "preview-player");
  expect(previewAgain.src, "the HIT preview still feeds the element").toMatch(/^blob:/);
  await expect
    .poll(async () => (await elementAudioMeta(page, "preview-player")).duration, {
      timeout: 15_000,
    })
    .toBeGreaterThan(0);

  expect(consoleErrors, `browser console: ${consoleErrors.join("\n")}`).toEqual([]);
  expectDefaultDataDirUntouched(run);
});
