import { execFileSync } from "node:child_process";
import { createHash, randomUUID } from "node:crypto";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";

import { expect, test } from "@playwright/test";

import { trackConsoleErrors } from "./support/console";
import { buildManifest, type SeedProfile } from "./support/manifest";
import { expectDefaultDataDirUntouched, serverRun } from "./support/servers";

/**
 * The managed replay clip store walk (ADR-0073), on the packaged binary
 * with NO replay allowlist: the server carries no GAME_NARRATOR_REPLAY_ROOTS,
 * so the only replay root the runtime can admit is the managed clip store
 * under the data directory. That is the product path the 30-minute device
 * run was missing — a clip arrives through the UI, the service env file is
 * never opened (it holds the gateway token):
 *
 *   sign in → /replay-clips: upload the committed synthetic clip
 *   (testdata/synthetic/replay/dialogue-advance-001, tarred, under a display
 *   id) → the row carries the frame count and the archive's own SHA-256
 *   (provenance, checked against the bytes the browser actually uploaded) →
 *   /play's source picker lists the clip → select it (the directory field is
 *   ignored) → the session runs the clip by id and commits its two pages →
 *   stop → delete the clip and the store is empty again.
 *
 * Every mutation on the clip surface happens through the page (docs/spec/09,
 * AGENTS.md rule 9); the profile seed goes through the API, the m5 walk's
 * shape, because this walk is the clip surface, not curation.
 */

const ADMIN = { username: "clips-walk-admin", password: "steady as the tide" };
const EDITION = {
  slug: "clip-harbour",
  title: "Clip Harbour",
  region: "us",
  language: "en",
  revision: "clip-1",
};
const CLIP_ID = "synthetic-walk-clip";
const CLIP_BASE = "dialogue-advance-001";
const CLIP_ROOT = resolve(import.meta.dirname, "../../testdata/synthetic/replay");
const PROFILE_SEED = resolve(
  import.meta.dirname,
  "../../testdata/synthetic/profiles/dialogue-advance-001.json",
);
const FRAMES = 24;

/** The same engine-tuned seed the m5 walk publishes: its two real lines
 *  (demo-page-1/2) are the ones the synthetic clip commits; the attribution
 *  and roster make the publish gate's inputs complete. */
function seedWithRoster(): SeedProfile {
  const seed = JSON.parse(readFileSync(PROFILE_SEED, "utf8")) as SeedProfile;
  for (const message of seed.messages) {
    message.attribution =
      message.id === "demo-page-1" || message.id === "demo-page-2"
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
  ];
  return seed;
}

test("upload the synthetic clip, play it by id with no replay allowlist, delete it", async ({
  page,
  request,
  baseURL,
}, testInfo) => {
  test.setTimeout(300_000);
  test.skip(
    testInfo.project.name !== "clips",
    "runs on the clips project's own embedded-binary backend",
  );
  const run = serverRun("clips");
  const origin = new URL(baseURL as string).origin;
  const consoleErrors = trackConsoleErrors(page, [401, 503]);
  const shot = (step: string) =>
    page.screenshot({ path: `test-results/replay-clips-walk-${step}.png`, fullPage: true });

  // ── 1. claim the appliance and seed + publish the profile (API, m5 shape) ──
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
  const { profile } = (await imported.json()) as { profile: { id: string } };
  const etagOf = async (id: string) =>
    (await request.get(`${origin}/api/v1/profiles/${id}`)).headers()["etag"] as string;
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
  // ── 2. /replay-clips: upload the committed synthetic clip, through the page ──
  // The walk tars the committed layout on the fly (its provenance is the
  // testdata hash pin, TestSyntheticReplayFixture's business); the store's
  // own provenance is the uploaded archive's SHA-256, asserted below against
  // these exact bytes.
  const staging = mkdtempSync(join(tmpdir(), "gn-clips-e2e-"));
  const tarPath = join(staging, `${CLIP_BASE}.tar`);
  execFileSync("tar", ["-cf", tarPath, "-C", CLIP_ROOT, CLIP_BASE]);
  const tarBytes = readFileSync(tarPath);
  const tarSha = createHash("sha256").update(tarBytes).digest("hex");
  rmSync(staging, { recursive: true, force: true });

  await page.goto("/replay-clips");
  await expect(page).toHaveURL(/\/login/);
  await page.getByTestId("login-username").fill(ADMIN.username);
  await page.getByTestId("login-password").fill(ADMIN.password);
  await page.getByTestId("login-submit").click();
  await expect(page).toHaveURL(/\/replay-clips$/);
  // The nav link exists at the developer tier and above (admin passes it);
  // the page itself is the developer-scoped surface (ADR-0073).
  await expect(page.getByTestId("nav-replay-clips")).toBeVisible();

  await page.getByTestId("clips-name").fill(CLIP_ID);
  await page
    .getByTestId("clips-file")
    .setInputFiles({ name: `${CLIP_BASE}.tar`, mimeType: "application/x-tar", buffer: tarBytes });
  await page.getByTestId("clips-upload").click();
  const row = page.getByTestId(`clip-row-${CLIP_ID}`);
  await expect(row, "the clip row lands under its chosen id").toBeVisible({ timeout: 60_000 });
  await expect(page.getByTestId(`clip-frames-${CLIP_ID}`)).toHaveText(String(FRAMES));
  // Provenance: the store's row carries the uploaded archive's content hash,
  // the first 12 hex of which the page shows. It must be the bytes the
  // browser just uploaded, not somebody else's clip.
  await expect(page.getByTestId(`clip-hash-${CLIP_ID}`)).toContainText(tarSha.slice(0, 12));
  await expect(page.getByTestId("clips-upload-result")).toContainText(CLIP_ID);
  await shot("01-clipped");

  // ── 3. /play: the source picker lists the clip; the session runs it by id ──
  await page.goto("/play");
  await expect(page.getByTestId("play-profile")).toBeVisible({ timeout: 30_000 });
  const profileSelect = page.getByTestId("play-profile");
  await profileSelect.selectOption(r1, { timeout: 30_000 });
  const clipSelect = page.getByTestId("play-replay-clip");
  await expect(clipSelect, "the developer-tier picker is on the form").toBeVisible();
  await expect(clipSelect).toContainText(`${CLIP_ID} · ${FRAMES} frames`);
  await clipSelect.selectOption(CLIP_ID);
  // Selecting a clip outranks the directory (the store is a replay root):
  // the directory field is inert while the clip is selected.
  await expect(page.getByTestId("play-replay-dir")).toBeDisabled();
  await page.getByTestId("play-start").click();
  await expect(page).toHaveURL(/\/play\?session=/);
  const sessionId = new URL(page.url()).searchParams.get("session") as string;
  expect(sessionId).toBeTruthy();

  await expect(page.getByTestId("play-state")).toHaveText("running", { timeout: 60_000 });
  // This server admits no replay roots at all (no GAME_NARRATOR_REPLAY_ROOTS
  // env on the webServer entry): the clip could only have run because the
  // managed store is a replay root by default (ADR-0073).
  await expect(page.getByTestId("play-source")).toContainText("replay · running", {
    timeout: 30_000,
  });
  await expect(page.getByTestId("play-source-error")).toHaveCount(0);
  // The clip commits its two pages in order; wait for both before stopping,
  // so the stop (or the source exhaustion that may end it first) settles on a
  // complete run rather than a partial one.
  await expect
    .poll(() => page.locator('[data-testid="recent-lines"] li').count(), {
      timeout: 120_000,
      intervals: [50],
    })
    .toBeGreaterThanOrEqual(2);

  const stop = page.getByTestId("play-stop");
  if (await stop.isVisible()) {
    await stop.click();
  }
  await expect(page.getByTestId("play-state")).toHaveText("stopped", { timeout: 180_000 });
  const finalSession = (await (
    await request.get(`${origin}/api/v1/sessions/${sessionId}`)
  ).json()) as { state: string; stats: { commits: number } };
  expect(finalSession.state).toBe("stopped");
  expect(finalSession.stats.commits, "the synthetic clip commits its two pages").toBe(2);
  await shot("02-played-from-clip");

  // ── 4. delete the clip: the store is empty again ───────────────────────────
  await page.goto("/replay-clips");
  await expect(row).toBeVisible({ timeout: 30_000 });
  await page.getByTestId(`clip-delete-${CLIP_ID}`).click();
  await expect(row).toHaveCount(0, { timeout: 30_000 });
  await expect(page.getByTestId("empty-state")).toContainText("No clips installed");
  await shot("03-deleted");

  expect(consoleErrors, "the walk left no console errors").toEqual([]);
  expectDefaultDataDirUntouched(run);
});
