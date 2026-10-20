import { expect, test } from "@playwright/test";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { randomUUID } from "node:crypto";

import { trackConsoleErrors } from "./support/console";
import { buildManifest, type SeedProfile } from "./support/manifest";
import { ports } from "./support/ports";
import { expectDefaultDataDirUntouched, serverRun } from "./support/servers";

/**
 * The capture-dongle product surface (ADR-0078, docs/spec/08/10), on the
 * packaged binary with the labelled-synthetic seam standing in for the UVC
 * HDMI dongle (GAME_NARRATOR_CAPTURE_FAKE): a fixed device fed by a real
 * FFmpeg testsrc2 pipe — loopback-free, real frames through the production
 * consumer, every answer flagged synthetic:
 *
 *   1. /settings "Capture" lists the device (stable id, never /dev/videoN)
 *      with its link, state and the mode a session would ask for, LABELLED
 *      synthetic in the DOM; the selection persists (the API reads it back);
 *   2. the live preview takes a real JPEG frame through the production
 *      pipeline (the browser's <img> is the polling loop);
 *   3. the capture test (probe) reports the frame stats and the OCR block;
 *   4. /play offers the selected capture device as a source and starts a
 *      real kind v4l2 session that reaches running with the fake device's
 *      frames — the session snapshot names the device and the mode it asked.
 *
 * The synthetic frames carry no game text, so the session is asserted on its
 * state and its source identity, not on a commit (the deterministic
 * commit-through-v4l2 path is the Go session test's push-based source, which
 * is labelled as such there). The real-dongle run is NOT RUN until the card
 * is attached (ADR-0078's validation, scripts/hw/sustained-on-device.sh).
 */

const EDITION = {
  slug: "capture-harbour",
  title: "Harbour Light",
  region: "us",
  language: "en",
  revision: "rev0",
};
const ADMIN = { username: "capture-admin", password: "steady as the tide" };
// The seam's fixed device (internal/capture/stack): the spec names the row by
// the exact id the answer carries.
const FAKE_ID = "usb-GameNarrator_SyntheticHDMI_0001";

interface CaptureDevicesAnswer {
  synthetic: boolean;
  selected_id?: string;
  devices: {
    id: string;
    name: string;
    path: string;
    state: string;
    link?: { speed_mbps: number };
  }[];
}

interface SessionSnapshot {
  state: string;
  frame_source: string;
  capture_device?: string;
  capture_mode?: string;
  stats: { frames_delivered: number };
}

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

test("the capture surface lists, previews, probes and runs a v4l2 session", async ({
  page,
  request,
}, testInfo) => {
  test.setTimeout(300_000);
  test.skip(
    testInfo.project.name !== "capture-device",
    "runs on the capture-device project's own packaged-binary backend",
  );
  const run = serverRun("capture");
  const origin = `http://127.0.0.1:${ports.capture}`;
  // 503 on the ignore list with the sibling walks' capture-reason: the walk's
  // server wires no Bluetooth surface (no GAME_NARRATOR_BT_FAKE, and the dev
  // box has no reachable bus), so /settings' bluetooth-state read answers 503
  // by design — the page's documented honest degradation hides the section
  // on exactly that answer (Settings.tsx BluetoothSection). The walk asserts
  // nothing about Bluetooth; every other fault still counts.
  const consoleErrors = trackConsoleErrors(page, [401, 503]);

  // ── 1. claim the throwaway appliance and seed a published profile ────────
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
    { "if-match": await etagOf(r1), "x-csrf-token": await csrf() },
  );
  const published = await post(`/api/v1/profiles/${r1}/publish`, undefined, {
    "if-match": await etagOf(r1),
    "x-csrf-token": await csrf(),
  });
  expect(published.status(), `publish: ${await published.text()}`).toBe(200);

  // ── 2. /settings: the device list, labelled synthetic, with the selection ─
  // The appliance is already claimed (the API setup above): a protected page
  // is where the auth guard redirects the unauthenticated browser to /login.
  await page.goto(`${origin}/settings`);
  await expect(page).toHaveURL(/\/login/);
  await page.getByTestId("login-username").fill(ADMIN.username);
  await page.getByTestId("login-password").fill(ADMIN.password);
  await page.getByTestId("login-submit").click();
  await expect(page).toHaveURL(/\/settings$/);

  const section = page.getByTestId("settings-capture");
  await expect(section).toBeVisible();

  // The seam list is labelled synthetic in the DOM — the walk's reason for
  // being honest is that this banner exists.
  await expect(page.getByTestId("capture-synthetic")).toHaveText(/Synthetic device list/);

  // The one seam row: the stable id (never the raw node path), ready state,
  // and the node path shown for report only.
  const row = page.getByTestId(`capture-device-${FAKE_ID}`);
  await expect(row).toBeVisible();
  await expect(row).toContainText("/dev/video-fake0");
  await expect(row).toContainText("ready");

  // The API agrees with the DOM: the synthetic flag the banner reads.
  const before = (await (
    await request.get(`${origin}/api/v1/capture/devices`)
  ).json()) as CaptureDevicesAnswer;
  expect(before.synthetic).toBe(true);
  expect(before.devices.map((device) => device.id)).toContain(FAKE_ID);
  expect(before.selected_id).toBeUndefined();

  // The selection persists: the API answer reads it back from the store.
  await page.getByTestId("capture-device-select").selectOption(FAKE_ID);
  await page.getByTestId("capture-save").click();
  await expect(page.getByTestId("capture-note")).toHaveText(
    new RegExp(`saved — device ${FAKE_ID}`),
  );
  const saved = (await (
    await request.get(`${origin}/api/v1/capture/devices`)
  ).json()) as CaptureDevicesAnswer;
  expect(saved.selected_id).toBe(FAKE_ID);

  // ── 3. the live preview: a real JPEG frame through the production pipeline ─
  await page.getByTestId("capture-preview-toggle").click();
  const previewImg = page.getByTestId("capture-preview-img");
  await expect(previewImg).toBeVisible({ timeout: 30_000 });
  expect(await previewImg.getAttribute("src")).toMatch(/^blob:/);
  await page.getByTestId("capture-preview-toggle").click();

  // ── 4. the capture test: the frame stats and the OCR block ────────────────
  await page.getByTestId("capture-probe").click();
  const probeResult = page.getByTestId("capture-probe-result");
  await expect(probeResult).toBeVisible({ timeout: 60_000 });
  await expect(probeResult).toContainText("grabbed 20/20");
  await expect(probeResult).toContainText("state running");
  // The OCR block is always present after frames: either the engine's read
  // (testsrc2 carries no text, so usually an empty read) or its install
  // action — the degradable half the contract names.
  await expect(page.getByTestId("capture-probe-ocr")).toContainText("OCR (");

  // ── 5. /play: the selected capture device starts a real v4l2 session ──────
  await page.goto(`${origin}/play`);
  await expect(page).toHaveURL(/\/play$/);
  await page.getByTestId("play-profile").selectOption(r1, { timeout: 20_000 });
  await page.getByTestId("play-source-kind").selectOption("v4l2");

  // The source picker offers the seam device (preselected from the persisted
  // selection) and says the list is synthetic.
  const devicePicker = page.getByTestId("play-capture-device");
  await expect(devicePicker).toBeVisible();
  await expect(devicePicker).toHaveValue(FAKE_ID);
  await expect(page.getByTestId("play-capture-note")).toContainText(
    "Synthetic device list (test seam)",
  );

  await page.getByTestId("play-start").click();
  await expect(page).toHaveURL(/\/play\?session=/);
  const sessionId = new URL(page.url()).searchParams.get("session") as string;
  expect(sessionId).toBeTruthy();
  await expect(page.getByTestId("play-state")).toHaveText("running", { timeout: 60_000 });

  // The live view says what the session runs: the kind, the source's own
  // state, and the device + mode the resolution settled on (the snapshot's
  // fields, not the click).
  await expect(page.getByTestId("play-source")).toContainText("v4l2");
  await expect(page.getByTestId("play-capture-device-info")).toContainText(FAKE_ID);
  await expect(page.getByTestId("play-capture-device-info")).toContainText("640x480");

  // The fake device's frames are flowing through the production consumer.
  // (expect.poll resolves immediately unless a matcher is chained — the
  // wait must be real: the spec once stopped the session ~150ms after start
  // through a poll that never ran, and asserted a frame count the session
  // never had the time to accrue.)
  await expect
    .poll(
      async () => {
        const text = await page.getByTestId("play-frames").innerText();
        return Number(text.match(/^(\d+) delivered/)?.[1] ?? 0);
      },
      { timeout: 30_000 },
    )
    .toBeGreaterThan(0);

  // The session is stopped by the operator's action; the synthetic frames
  // carry no game text, so the honest assertion is the run, not a commit.
  await page.getByTestId("play-stop").click();
  await expect(page.getByTestId("play-state")).not.toHaveText("running", { timeout: 30_000 });

  // The server's snapshot agrees with the page: the run was a v4l2 one on the
  // seam device, and the frames it delivered are the testsrc2 frames.
  const snapshot = (await (
    await request.get(`${origin}/api/v1/sessions/${sessionId}`)
  ).json()) as SessionSnapshot;
  expect(snapshot.frame_source).toBe("v4l2");
  expect(snapshot.capture_device).toBe(FAKE_ID);
  expect(snapshot.capture_mode).toMatch(/640x480/);
  expect(snapshot.stats.frames_delivered).toBeGreaterThan(0);

  expect(consoleErrors, `the browser logged faults: ${consoleErrors.join(" | ")}`).toHaveLength(0);
  expectDefaultDataDirUntouched(run);
});
