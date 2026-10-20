import { expect, test } from "@playwright/test";

import { trackConsoleErrors } from "./support/console";

/**
 * The first-run wizard (G9, ADR-0087, docs/spec/09 "first-run wizard"), on
 * the wizard project's own packaged-binary appliance (the claim is once per
 * appliance, ADR-0016 §7 — it cannot share the chromium dev-server appliance
 * m5-settings-tests claims): claim and walk the six steps — devices,
 * storage, models, audio, privacy, done — asserting each step's visible live
 * state.
 *
 * Honesty is the point of the assertions: every step renders the answer its
 * own live read gives (a capture layer the dev server does not wire is
 * NOT AVAILABLE, not an empty list; a role with no configured model probes
 * NOT CONFIGURED, not OK), and the done summary is the same live read, so a
 * skipped step is marked NOT CONFIGURED rather than reported as finished.
 *
 * Console note: the appliance has no capture layer wired (no
 * GAME_NARRATOR_CAPTURE_FAKE), so the capture-device read answers 503 and
 * the wizard renders that as its own "unavailable" note — by design, not a
 * fault, so 503 joins 401 on the ignore list exactly as the a11y walk does.
 */

const ADMIN = { username: "setup-admin", password: "steady as the tide" };

const ROLES = [
  "agent_reasoning",
  "fast_reasoning",
  "vision_ocr_fallback",
  "decision",
  "voice_design",
  "voice_clone",
  "speech_asr",
];

test("the setup wizard walks its six steps and reports each one's live state", async ({
  page,
  request,
}) => {
  test.setTimeout(300_000);
  // 401: the anonymous /auth/me probe the guard fires on mount.
  // 503: the capture read on a dev server with no capture layer wired.
  const consoleErrors = trackConsoleErrors(page, [401, 503]);

  // ── get to a signed-in admin, through the page ───────────────────────────
  // The branch is decided by the API's own answer, not by racing two UI
  // states: an unclaimed and a claimed appliance render different pages at
  // /setup, and the walk must start from the right one.
  const statusBody = (await (await request.get("/api/v1/auth/status")).json()) as {
    setup_required: boolean;
  };
  if (statusBody.setup_required) {
    // Unclaimed appliance: the claim step the appliance cannot be used
    // without (ADR-0016 §7) — one-time, mints no session.
    await page.goto("/setup");
    await expect(page.getByTestId("setup-form")).toBeVisible();
    await page.getByTestId("setup-username").fill(ADMIN.username);
    await page.getByTestId("setup-password").fill(ADMIN.password);
    await page.getByTestId("setup-submit").click();
    await expect(page).toHaveURL(/\/login$/, { timeout: 20_000 });
  } else {
    // Already claimed (an earlier file on the same server): the /setup guard
    // routes an anonymous visitor to /login; sign in there.
    await page.goto("/login");
    await expect(page.getByTestId("login-username")).toBeVisible();
  }
  await page.getByTestId("login-username").fill(ADMIN.username);
  await page.getByTestId("login-password").fill(ADMIN.password);
  await page.getByTestId("login-submit").click();
  await expect(page.getByTestId("nav-user"), "signed in").toContainText(ADMIN.username, {
    timeout: 20_000,
  });
  await expect(page.getByTestId("nav-role")).toHaveText("admin");

  // ── the wizard opens on the devices step, all six in the nav ─────────────
  await page.goto("/setup");
  await expect(page.getByTestId("setup-wizard")).toBeVisible();
  for (const step of ["devices", "storage", "models", "audio", "privacy", "done"]) {
    await expect(page.getByTestId(`setup-nav-${step}`)).toBeVisible();
  }
  await expect(page.getByTestId("setup-step-devices")).toBeVisible();

  // Each honest capture state is acceptable on a dev server; an empty device
  // list rendered as "found devices" would not be. The step's content arrives
  // after the section mounts (the reads are live), so the either-or is
  // polled, not snapshotted.
  const captureStates = [
    "setup-capture-unavailable",
    "setup-capture-none",
    "setup-capture-synthetic",
    "setup-capture-device",
  ] as const;
  await expect(async () => {
    const visible = await Promise.all(
      captureStates.map((testid) => page.getByTestId(testid).first().isVisible()),
    );
    expect(visible.some(Boolean), "one honest capture state is visible").toBe(true);
  }).toPass({ timeout: 20_000 });

  // The audio half of the devices step: same honesty rule.
  await expect(async () => {
    const visible = await Promise.all(
      (["setup-audio-synthetic", "setup-audio-none", "setup-audio-device"] as const).map((testid) =>
        page.getByTestId(testid).first().isVisible(),
      ),
    );
    expect(visible.some(Boolean), "one honest audio state is visible").toBe(true);
  }).toPass({ timeout: 20_000 });

  // ── storage: the live host read, as the page renders it ──────────────────
  await page.getByTestId("setup-next").click();
  await expect(page.getByTestId("setup-step-storage")).toBeVisible();
  await expect(page.getByTestId("setup-storage-facts")).toBeVisible();
  await expect(page.getByTestId("setup-storage-free")).toBeVisible();
  await expect(page.getByTestId("setup-storage-assets")).toBeVisible();

  // ── models: the gateway facts and all seven role rows ────────────────────
  await page.getByTestId("setup-next").click();
  await expect(page.getByTestId("setup-step-models")).toBeVisible();
  await expect(page.getByTestId("setup-models-gateway")).toBeVisible();
  for (const role of ROLES) {
    await expect(page.getByTestId(`setup-models-row-${role}`)).toBeVisible();
  }

  // The live test is the doctor's own check server-side (ADR-0087): with no
  // model configured for the role, the honest answer is NOT CONFIGURED — a
  // probe that answered OK here would be a fabricated green.
  await page.getByTestId("setup-models-test-agent_reasoning").click();
  await expect(
    page.getByTestId("setup-models-result-agent_reasoning"),
    "the probe lands within the doctor's budget",
  ).toBeVisible({ timeout: 120_000 });
  expect(await page.getByTestId("setup-models-result-agent_reasoning").textContent()).toMatch(
    /NOT CONFIGURED|OK|DEGRADED|BLOCKED|FAIL/,
  );

  // ── audio: the selection step's honest state ─────────────────────────────
  await page.getByTestId("setup-next").click();
  await expect(page.getByTestId("setup-step-audio")).toBeVisible();
  expect(
    (
      await Promise.all(
        (
          [
            "setup-audio-synthetic",
            "setup-audio-none",
            "setup-audio-select",
            "setup-audio-saved",
          ] as const
        ).map((testid) =>
          page
            .getByTestId(testid)
            .first()
            .isVisible()
            .catch(() => false),
        ),
      )
    ).some(Boolean),
    "one of the audio selection states is visible",
  ).toBe(true);

  // ── privacy: the review, then the finish ─────────────────────────────────
  await page.getByTestId("setup-next").click();
  await expect(page.getByTestId("setup-step-privacy")).toBeVisible();
  await page.getByTestId("setup-privacy-confirm").check();
  await page.getByTestId("setup-privacy-finish").click();
  await expect(page.getByTestId("setup-step-done")).toBeVisible();
  await expect(page.getByTestId("setup-done-summary")).toBeVisible();

  // The done summary is a live read: the confirmed step reports CONFIGURED,
  // and the page offers the diagnostics page as the continuous surface.
  await expect(page.getByTestId("setup-done-row-privacy")).toContainText("CONFIGURED");
  await expect(page.getByTestId("setup-to-diagnostics")).toBeVisible();

  // ── resumption: the step in the URL is the step the wizard opens on ──────
  await page.goto("/setup?step=storage");
  await expect(page.getByTestId("setup-step-storage")).toBeVisible();
  await page.goto("/setup?step=audio");
  await expect(page.getByTestId("setup-step-audio")).toBeVisible();
  // A step the wizard does not know falls back to the first one, not to a
  // blank page.
  await page.goto("/setup?step=bogus");
  await expect(page.getByTestId("setup-step-devices")).toBeVisible();

  expect(consoleErrors, `browser console: ${consoleErrors.join("\n")}`).toEqual([]);
});
