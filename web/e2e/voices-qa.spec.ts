import { expect, test, type Page } from "@playwright/test";
import { resolve } from "node:path";

import { expectDefaultDataDirUntouched, serverRun } from "./support/servers";
import { trackConsoleErrors } from "./support/console";

/**
 * The ASR QA badges on the audition cards (FR-10, ADR 0061) against the
 * packaged binary: the speech roles point at the scripted TTS instance
 * (web/e2e/fake-tts.mjs) and the speech_asr role at the scripted ASR instance
 * (web/e2e/fake-asr.mjs, a SYNTHETIC STUB — AGENTS.md rule 3), so the whole
 * audition read-back runs through the real binary, the real job queue and the
 * real /audio/transcriptions wire with no model. The stub recovers the spoken
 * line from the tone's frequency and answers the line's text (WER 0) — except
 * the line FAKE_ASR_FAIL_LINE names (the third), where it answers a planted
 * mishearing, the flagged case this spec asserts.
 *
 * The walk: the same curation setup as voices.spec.ts (a different character),
 * generate three auditions, assert the two passed badges and the flagged
 * badge with its WER and the "ASR heard" evidence, approve the flagged
 * audition and take the 409 AUDITION_FLAGGED the backend gate returns, then
 * approve a passing one and take the approved state.
 */

const TESTDATA = resolve(
  import.meta.dirname,
  "..",
  "..",
  "internal",
  "games",
  "adapter",
  "generic",
  "testdata",
);
const ADVENTURE_CSV = resolve(TESTDATA, "synthetic-adventure.csv");

const ADMIN = { username: "voices-admin", password: "steady as she goes" }; // the
// same admin voices.spec.ts claims: the suite runs serially (workers: 1) on
// one shared server, so whichever spec runs first claims the appliance and
// the other signs in.
const CHARACTER = "Corvin"; // one of the CSV's speaker column values
// Must match the FAKE_ASR_BRIEF the fake ASR is started with
// (playwright.config.ts): the seed couples line + brief + model, so a
// mismatch makes every audition unreadable.
const BRIEF = "A calm harbor voice, unhurried, with a slight salt air in it.";
const MISHEARING = "the harbour lights were out that evening";
const CONSENT = "original_synthetic_design";

async function signIn(page: Page) {
  await page.goto("/library");
  const setupForm = page.getByTestId("setup-form");
  const loginUsername = page.getByTestId("login-username");
  await expect(setupForm.or(loginUsername)).toBeVisible({ timeout: 15_000 });
  if (await setupForm.isVisible()) {
    await page.getByTestId("setup-username").fill(ADMIN.username);
    await page.getByTestId("setup-password").fill(ADMIN.password);
    await page.getByTestId("setup-submit").click();
    await expect(page).toHaveURL(/\/login$/);
  }
  await loginUsername.fill(ADMIN.username);
  await page.getByTestId("login-password").fill(ADMIN.password);
  await page.getByTestId("login-submit").click();
  await expect(page.getByTestId("nav-user")).toBeVisible();
  await expect(page.getByTestId("nav-role")).toHaveText("admin");
}

test("audition ASR QA: the passed badges, the flagged badge and the approval gate", async ({
  page,
  request,
}) => {
  test.setTimeout(300_000);
  const run = serverRun("voices");

  // 409 is allowlisted by design: the spec deliberately approves the flagged
  // audition and asserts the AUDITION_FLAGGED rejection (FR-10); the 401 is
  // the unauthenticated /auth/me probe the guard makes on mount.
  const consoleErrors = trackConsoleErrors(page, [401, 409]);

  // ── 1. sign in ─────────────────────────────────────────────────────────────
  await signIn(page);
  await request.addCookies(await page.context().cookies());

  // ── 2. a game, an edition, a draft revision ────────────────────────────────
  await page.getByTestId("new-game").click();
  await page.getByTestId("new-game-slug").fill("voice-qa-adventure");
  await page.getByTestId("new-game-title").fill("Voice QA Adventure");
  await page.getByTestId("new-game-platform").fill("GameCube");
  await page.getByTestId("new-game-submit").click();
  await expect(page.getByTestId("game-voice-qa-adventure")).toContainText("Voice QA Adventure");

  await page.getByTestId("game-voice-qa-adventure").click();
  await expect(page.getByTestId("game-title")).toHaveText("Voice QA Adventure");

  await page.getByTestId("new-edition").click();
  await page.getByTestId("edition-region").fill("us");
  await page.getByTestId("edition-language").fill("en");
  await page.getByTestId("edition-revision").fill("disc-1");
  await page.getByTestId("edition-submit").click();
  await expect(page.getByRole("tab", { name: "us / en / disc-1" })).toBeVisible();
  await page
    .getByTestId(/edition-panel-/)
    .getByTestId("new-draft")
    .click();
  await expect(page.getByTestId("revision-1")).toBeVisible();

  // ── 3. the script: its speaker column becomes the roster ──────────────────
  const revision1 = page.getByTestId("revision-1");
  await revision1.getByTestId("import-file").setInputFiles(ADVENTURE_CSV);
  await revision1.getByTestId("import-submit").click();
  await expect(page.getByTestId("job-progress")).toContainText("succeeded", { timeout: 60_000 });

  // ── 4. the voice page, the character, the brief ────────────────────────────
  await page.getByTestId("voices-1").click();
  await expect(page).toHaveURL(/\/voices\?profile=/);
  const corvin = page.getByTestId("voice-characters").locator("label", { hasText: CHARACTER });
  await corvin.getByRole("radio").click();
  await expect(page).toHaveURL(new RegExp(`character=${encodeURIComponent(CHARACTER)}$`));
  await expect(page.getByTestId("no-voice")).toContainText(`No voice for ${CHARACTER} yet`);

  // ── 5. generate three auditions (202 + job) ────────────────────────────────
  await page.getByTestId("voice-brief").fill(BRIEF);
  const accepted = page.waitForResponse(
    (r) => r.request().method() === "POST" && r.url().includes("/api/v1/voices/auditions"),
  );
  await page.getByTestId("generate-auditions").click();
  const createResponse = (await (await accepted).json()) as {
    job_id: string;
    voice_id: string;
  };
  await expect
    .poll(
      async () => {
        const body = (await (
          await request.get(`/api/v1/jobs/${createResponse.job_id}`)
        ).json()) as {
          state: string;
        };
        if (body.state === "failed") throw new Error(`the audition job failed: ${body.state}`);
        return body.state;
      },
      { message: "the audition job reached a terminal state", timeout: 90_000 },
    )
    .toBe("succeeded");

  // ── 6. the QA verdicts: two passed, one flagged ───────────────────────────
  await expect(page.getByTestId("voice-state-badge")).toHaveText("auditioning", {
    timeout: 30_000,
  });
  const cards = page.getByTestId(/^audition-card-/);
  await expect(cards).toHaveCount(3);
  // Auditions 1 and 2: the read-back matched the line within the bound — the
  // passed badge carries the measured WER.
  for (const i of [0, 1]) {
    const badge = page.getByTestId(`qa-pass-${i}`);
    await expect(badge).toBeVisible();
    await expect(badge).toContainText("QA passed");
    await expect(badge).toContainText("wer ");
  }
  // Audition 3: the stub's planted mishearing is far over the bound — the
  // flagged badge carries the WER, and the "ASR heard" line is the review
  // evidence the spec (docs/spec/12 "retain diagnostic evidence") asks to
  // retain on the card.
  const flag = page.getByTestId("qa-flag-2");
  await expect(flag).toBeVisible();
  await expect(flag).toContainText("QA flagged");
  await expect(page.getByTestId("qa-transcript-2")).toContainText(MISHEARING);

  // ── 7. the approval gate refuses the flagged audition ─────────────────────
  await page.getByTestId("audition-approve-2").click();
  await expect(page.getByTestId("audition-approve-2")).toHaveText("Selected");
  await page.getByTestId("consent-input").fill(CONSENT);
  const flagResponse = page.waitForResponse(
    (r) => r.request().method() === "POST" && r.url().endsWith("/approve"),
  );
  await page.getByTestId("approve-voice").click();
  const flagStatus = await (await flagResponse).status();
  expect(flagStatus).toBe(409);
  const flagged = (await (await flagResponse).json()) as { code: string; detail: string };
  expect(flagged.code).toBe("AUDITION_FLAGGED");
  expect(flagged.detail).toContain("QA");
  await expect(page.getByTestId("error-state")).toContainText("AUDITION_FLAGGED");
  // The revision is still auditioning: the refused approval changed nothing.
  await expect(page.getByTestId("voice-state-badge")).toHaveText("auditioning");

  // ── 8. a passing audition still approves ──────────────────────────────────
  await page.getByTestId("audition-approve-0").click();
  const okResponse = page.waitForResponse(
    (r) => r.request().method() === "POST" && r.url().endsWith("/approve"),
  );
  await page.getByTestId("approve-voice").click();
  expect(await (await okResponse).status()).toBe(200);
  await expect(page.getByTestId("voice-state-badge")).toHaveText("approved");

  // The isolation and cleanliness contracts.
  expectDefaultDataDirUntouched(run);
  expect(consoleErrors, `browser console: ${consoleErrors.join("\n")}`).toEqual([]);
});
