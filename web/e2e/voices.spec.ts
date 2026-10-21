import { expect, test, type Page } from "@playwright/test";
import { resolve } from "node:path";

import { ports } from "./support/ports";
import {
  apiAsUser,
  claimAppliance,
  expectDefaultDataDirUntouched,
  serverRun,
} from "./support/servers";
import { trackConsoleErrors } from "./support/console";

/**
 * The voice setup workflow end to end (docs/spec/07, docs/spec/08, docs/spec/14)
 * against the packaged binary with the speech roles pointed at a scripted TTS
 * instance (web/e2e/fake-tts.mjs). No mocked workflow: the real API process,
 * the real job queue, the real store, the real asset store, the real speech
 * cache — only the *model* is scripted, the same boundary ADR-0022 draws for
 * the milestone's evidence. The fake keeps the registry honest (a Base
 * synthesis for an unregistered voice is a 400, exactly like the checkpoint),
 * so the MISS→HIT preview is the server's cache, not a lenient stub.
 *
 * The walk: claim the appliance, sign in, name a title, bind an edition, open
 * a revision, import a script (the roster comes from its speaker column), pick
 * a character, generate three auditions (202 + job), refuse approval without
 * consent and take it with consent, register on the instance, and preview the
 * same line twice — MISS, then HIT.
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

const ADMIN = { username: "voices-admin", password: "steady as she goes" };
const CHARACTER = "Marla"; // one of the CSV's speaker column values
const CONSENT = "original_synthetic_design";

async function signIn(page: Page) {
  await page.goto("/library");
  // The claim is once per appliance (ADR-0016 §7): a standalone run claims
  // here; a parallel-suite run finds it claimed by a sibling walk (the
  // helper's losing submit just walks to the sign-in).
  await claimAppliance(page, ADMIN);
  await page.getByTestId("login-username").fill(ADMIN.username);
  await page.getByTestId("login-password").fill(ADMIN.password);
  await page.getByTestId("login-submit").click();
  await expect(page.getByTestId("nav-user")).toBeVisible();
  await expect(page.getByTestId("nav-role")).toHaveText("admin");
  // The `request` fixture is its own context with its own cookie jar.
  return page.context().cookies();
}

test("voice setup: auditions, consent-gated approval, registration, preview cache", async ({
  page,
  request,
  baseURL,
}) => {
  test.setTimeout(300_000);
  const run = serverRun("voices");
  const origin = new URL(baseURL as string).origin;
  const api = apiAsUser(request, origin);

  // 401 is allowlisted: the unauthenticated /auth/me probe the guard makes on
  // mount. 409 is the one-shot claim's conflict when a parallel walk claimed
  // the appliance first (ADR-0016 §7). Every other console error — a CSP
  // block, a missing bundle, a page fault — fails the cleanliness check at the
  // end.
  const consoleErrors = trackConsoleErrors(page, [401, 409]);

  // ── 1. first run: claim the appliance, sign in ────────────────────────────
  const cookies = await signIn(page);
  await request.addCookies(cookies);

  // ── 2. a game, an edition, a draft revision ────────────────────────────────
  await page.getByTestId("new-game").click();
  await page.getByTestId("new-game-slug").fill("voice-adventure");
  await page.getByTestId("new-game-title").fill("Voice Adventure");
  await page.getByTestId("new-game-platform").fill("GameCube");
  await page.getByTestId("new-game-submit").click();
  await expect(page.getByTestId("game-voice-adventure")).toContainText("Voice Adventure");

  await page.getByTestId("game-voice-adventure").click();
  await expect(page.getByTestId("game-title")).toHaveText("Voice Adventure");

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
  await expect(page.getByTestId("import-inserted")).toHaveText("40");

  // ── 4. the cast page: the roster, then a character with no voice ─────────
  // Cast merged the voices page (ADR-0093). The dial is switched off: this
  // spec drives the manual, consent-gated approval (three takes, the server
  // refuses the flag-less click), and with the dial on the job would
  // auto-approve (that path is cast.spec.ts's).
  await page.getByTestId("cast-1").click();
  await expect(page).toHaveURL(/\/cast\?profile=/);
  await expect(page.getByTestId("voice-characters")).toBeVisible();
  await page.getByTestId("auto-cast-switch").click();
  await expect(page.getByTestId("auto-cast-switch")).not.toBeChecked();
  await page.getByTestId(`cast-row-name-${CHARACTER}`).click();
  // The selection is a URL change (the page derives `character` from the
  // query param), so assert on the param rather than the row's selected
  // state, which React owns.
  await expect(page).toHaveURL(new RegExp(`character=${encodeURIComponent(CHARACTER)}$`));

  // No design attempt yet: the page says so, and the setup block offers the
  // brief and the generate button.
  await expect(page.getByTestId("no-voice")).toContainText(`No voice for ${CHARACTER} yet`);
  await expect(page.getByTestId("voice-brief")).toBeVisible();

  // ── 5. brief + generate: 202, the job, the audition panel ─────────────────
  const brief = "A calm harbor voice, unhurried, with a slight salt air in it.";
  await page.getByTestId("voice-brief").fill(brief);
  const accepted = page.waitForResponse(
    (r) => r.request().method() === "POST" && r.url().includes("/api/v1/voices/auditions"),
  );
  await page.getByTestId("generate-auditions").click();
  const createResponse = (await (await accepted).json()) as {
    job_id: string;
    voice_id: string;
    revision: number;
    state: string;
  };
  expect(createResponse.state).toBe("draft");
  expect(createResponse.revision).toBe(1);

  // The slow part is a durable job (docs/spec/08): its own report says how
  // many clips the VoiceDesign checkpoint returned.
  await expect
    .poll(
      async () => {
        const body = (await (
          await request.get(`/api/v1/jobs/${createResponse.job_id}`)
        ).json()) as {
          state: string;
          result?: { audition_count: number; audition_ids: string[] };
        };
        if (body.state === "failed")
          throw new Error(`the audition job failed: ${JSON.stringify(body)}`);
        return body.state;
      },
      { message: "the audition job reached a terminal state", timeout: 90_000 },
    )
    .toBe("succeeded");
  const jobDetail = (await (await request.get(`/api/v1/jobs/${createResponse.job_id}`)).json()) as {
    result: { audition_count: number; audition_ids: string[] };
  };
  expect(jobDetail.result.audition_count).toBe(3);

  // The page caught up with the job: three candidates, each with its clip and
  // its transcript — the text equivalent of the audio (docs/spec/09).
  await expect(page.getByTestId("voice-state-badge")).toHaveText("auditioning", {
    timeout: 30_000,
  });
  const cards = page.getByTestId(/^audition-card-/);
  await expect(cards).toHaveCount(3);
  for (let i = 0; i < 3; i++) {
    await expect(page.getByTestId(`audition-player-${i}`)).toBeVisible();
    await expect(page.getByTestId(`audition-transcript-${i}`)).not.toBeEmpty();
  }
  // The fake model reports itself on every clip: the provenance is stored and
  // shown, not invented by the page.
  await expect(page.getByTestId("audition-card-0")).toContainText("fake-voicedesign");

  // ── 6. approval: the record is the profile's consent default, taken with
  // a selected take (ADR-0135) ────────────────────────────────────────────────
  // The consent record is prefilled with the profile's consent default (the
  // record the auto-cast approval attributes, ADR-0109), so the required
  // note is gone and the button's one gate is a selected take; the server
  // still answers an empty record with CONSENT_REQUIRED.
  await expect(page.getByTestId("consent-input")).toHaveValue(CONSENT);
  await expect(page.getByTestId("consent-required-note")).toHaveCount(0);
  await expect(page.getByTestId("approve-voice")).toBeDisabled();
  // Pick candidate 2: the approval freezes *that* clip, and the page must
  // show the frozen transcript and the consent record afterwards.
  await page.getByTestId("audition-approve-1").click();
  await expect(page.getByTestId("audition-approve-1")).toHaveText("Selected");
  const approvedClipTranscript = (await page
    .getByTestId("audition-transcript-1")
    .textContent()) as string;
  // With a clip selected, the consent record is the last gate.
  await page.getByTestId("consent-input").fill(CONSENT);
  await expect(page.getByTestId("approve-voice")).toBeEnabled();

  await page.getByTestId("approve-voice").click();
  await expect(page.getByTestId("voice-state-badge")).toHaveText("approved");
  await expect(page.getByTestId("voice-fingerprint")).toBeVisible();
  await expect(page.getByTestId("voice-consent")).toContainText(CONSENT);
  await expect(page.getByTestId("voice-state")).toContainText(approvedClipTranscript);

  // ── 7. registration: the pending row the approval opened, then ready ──────
  // Approving already computed the immutable name and opened a pending row.
  await expect(page.getByTestId(/^registration-state-/)).toHaveText("pending");
  const registerResponse = page.waitForResponse(
    (r) => r.request().method() === "POST" && r.url().includes("/api/v1/voices/"),
  );
  await page.getByTestId("register-voice").click();
  const registration = (await (await registerResponse).json()) as {
    instance_id: string;
    registered_name: string;
    state: string;
  };
  expect(registration.state).toBe("ready");
  expect(registration.registered_name).toMatch(/^gn_/);
  await expect(page.getByTestId(/^registration-state-/)).toHaveText("ready");
  await expect(page.getByTestId("registration-name")).toContainText(registration.registered_name);

  // The registry itself says the name exists (outside the page: the fake is
  // the "instance", and its /audio/voices is the registry contract).
  const listed = await (
    await request.get(`http://127.0.0.1:${ports.fakeTTS}/v1/audio/voices`)
  ).json();
  expect((listed as { voices: string[] }).voices).toContain(registration.registered_name);

  // ── 8. preview: the same line twice — MISS, then HIT ──────────────────────
  await expect(page.getByTestId("preview-panel")).toBeVisible();
  const previewLine = "The road ahead was long, but the light held.";
  await expect(page.getByTestId("preview-line")).toHaveValue(previewLine);

  const firstPreview = page.waitForResponse(
    (r) => r.request().method() === "POST" && r.url().includes("/preview"),
  );
  await page.getByTestId("preview-button").click();
  const firstHeaders = (await firstPreview).headers();
  expect(firstHeaders["x-cache"]).toBe("MISS");
  await expect(page.getByTestId("preview-player")).toBeVisible();
  await expect(page.getByTestId("preview-cache")).toHaveText("MISS");

  // The identical line is the same cache key: the server serves the second
  // preview from the speech cache (docs/spec/07 step 7) and says so.
  const secondPreview = page.waitForResponse(
    (r) => r.request().method() === "POST" && r.url().includes("/preview"),
  );
  await page.getByTestId("preview-button").click();
  const secondHeaders = (await secondPreview).headers();
  expect(secondHeaders["x-cache"]).toBe("HIT");
  await expect(page.getByTestId("preview-cache")).toHaveText("HIT");

  // A *different* line is a different key and must miss again: the HIT above
  // was a key match, not "the cache is on".
  await page.getByTestId("preview-line").fill("And the sea, in the end, was patient.");
  const thirdPreview = page.waitForResponse(
    (r) => r.request().method() === "POST" && r.url().includes("/preview"),
  );
  await page.getByTestId("preview-button").click();
  expect((await thirdPreview).headers()["x-cache"]).toBe("MISS");

  // ── 9. the frozen state, from outside the page ────────────────────────────
  const voice = (await (await api.get(`/api/v1/voices/${createResponse.voice_id}`)).json()) as {
    state: string;
    ref_transcript: string;
    fingerprint: string;
    consent_provenance: string;
    auditions: { id: string }[];
  };
  expect(voice.state).toBe("approved");
  expect(voice.ref_transcript).toBe(approvedClipTranscript);
  expect(voice.fingerprint).toMatch(/^[0-9a-f]{64}$/);
  expect(voice.consent_provenance).toBe(CONSENT);
  expect(voice.auditions).toHaveLength(3);

  // ── 10. nothing leaked, nothing was touched elsewhere ─────────────────────
  expect(consoleErrors, `browser console: ${consoleErrors.join("\n")}`).toEqual([]);
  expectDefaultDataDirUntouched(run);
});
