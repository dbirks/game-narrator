import { expect, test } from "@playwright/test";
import { resolve } from "node:path";

import { trackConsoleErrors } from "./support/console";
import {
  apiAsUser,
  claimAppliance,
  expectDefaultDataDirUntouched,
  serverRun,
} from "./support/servers";

/**
 * The cast audit's voice-check badge (ADR-0171) end to end on the packaged
 * binary with the scripted speech providers (fake-tts.mjs / fake-asr.mjs):
 * the same boundary the cast walk draws (AGENTS.md rule 3).
 *
 * The walk seeds a DELIBERATE mismatch: the scripted design's tone frequency
 * is a pure function of (line, brief, model) — `160 + djb2(seed) % 400` Hz —
 * so the spec computes each brief's take F0 before it exists. The mismatch
 * character gets a female brief whose tone lands at 418 Hz (child class):
 * expected female, measured child. The control character gets a child brief
 * whose tone lands at 468 Hz (child class): expected child, measured child —
 * so its badge reads OK and the badge is not painting every row red.
 * The auto-cast dial approves both takes (the references land), the audit
 * endpoint measures them, and the Cast page's small "Voice check" badge
 * shows the verdict.
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

// The shared-admin contract (ADR-0016 §7): the voices project's specs share
// one appliance; the claim happens once, so the spec names the same admin.
const ADMIN = { username: "voices-admin", password: "steady as she goes" };

// The roster speakers this walk owns: the cast walk owns Boy, voices owns
// Marla, voices-qa owns Corvin — voice revisions are keyed by character name
// across the appliance (migration 0009), so these two are free.
const MISMATCH_CHARACTER = "Chef Ondine";
const OK_CHARACTER = "Têvance";

// The design's fixed synthetic line, first in voices.SyntheticAuditionLines
// (the auto-cast run is one take).
const LINE = "Hello there. Let me tell you a little about today.";

// The briefs this walk seeds into FAKE_ASR_BRIEF (playwright.voices-only/
// main config): the stub recovers the spoken line from the tone's frequency,
// so an unseeded brief would fail the QA read-back and the auto-cast would
// refuse the take.
const BRIEF_FEMALE = "An adult woman, warm and calm, with a steady mid-range voice.";
const BRIEF_CHILD = "A bright young child, small and high-pitched.";

// fake-tts.mjs's tone law: 160 + djb2(`${input}|${instructions}|${model}`) % 400.
function djb2(text: string): number {
  let h = 5381;
  for (let i = 0; i < text.length; i++) h = ((h << 5) + h + text.charCodeAt(i)) >>> 0;
  return h >>> 0;
}
const freqOf = (brief: string): number => 160 + (djb2(`${LINE}|${brief}|fake-voicedesign`) % 400);

// The mismatch row: the female brief's tone lands at 418 Hz — child class,
// not female. The control row: the child brief's tone lands at 468 Hz —
// child class, as the brief states. Both are asserted up front: a change in
// the stub's tone law is a spec failure, not a silently flipped verdict.
const mismatch = {
  character: MISMATCH_CHARACTER,
  brief: BRIEF_FEMALE,
  expected: "mid (adult female)",
};
const okRow = { character: OK_CHARACTER, brief: BRIEF_CHILD, expected: "high (child)" };

if (freqOf(BRIEF_FEMALE) <= 255) {
  throw new Error(
    `spec invariant: the female brief's tone ${freqOf(BRIEF_FEMALE)} Hz is not child-class; re-derive the briefs`,
  );
}
if (freqOf(BRIEF_CHILD) <= 255) {
  throw new Error(
    `spec invariant: the child brief's tone ${freqOf(BRIEF_CHILD)} Hz is not child-class; re-derive the briefs`,
  );
}

interface AuditRow {
  character: string;
  voice_id?: string;
  expected: string;
  measured_f0_hz: number;
  measured: string;
  verdict: string;
}

async function jobResult(request: import("@playwright/test").APIRequestContext, jobId: string) {
  await expect
    .poll(
      async () => {
        const b = (await (await request.get(`/api/v1/jobs/${jobId}`)).json()) as {
          state: string;
        };
        if (b.state === "failed") throw new Error(`the audition job failed: ${JSON.stringify(b)}`);
        return b.state;
      },
      { message: "the audition job reached a terminal state", timeout: 90_000 },
    )
    .toBe("succeeded");
}

test("the cast page shows the voice-check badge for a seeded mismatch and an OK row", async ({
  page,
  request,
  baseURL,
}) => {
  test.setTimeout(300_000);
  const run = serverRun("voices");
  const origin = new URL(baseURL as string).origin;
  const api = apiAsUser(request, origin);
  const consoleErrors = trackConsoleErrors(page, [401, 409]);

  // ── 1. sign in (the shared appliance the voices project's server owns) ────
  await page.goto("/library");
  await claimAppliance(page, ADMIN);
  await page.getByTestId("login-username").fill(ADMIN.username);
  await page.getByTestId("login-password").fill(ADMIN.password);
  await page.getByTestId("login-submit").click();
  await expect(page.getByTestId("nav-user")).toBeVisible();
  const cookies = await page.context().cookies();
  await request.addCookies(cookies);

  // ── 2. game, edition, draft, the script ───────────────────────────────────
  await page.getByTestId("new-game").click();
  await page.getByTestId("new-game-slug").fill("cast-voice-check");
  await page.getByTestId("new-game-title").fill("Cast Voice Check");
  await page.getByTestId("new-game-platform").fill("GameCube");
  await page.getByTestId("new-game-submit").click();
  await expect(page.getByTestId("game-cast-voice-check")).toContainText("Cast Voice Check");
  await page.getByTestId("game-cast-voice-check").click();
  const gameId = new URL(page.url()).pathname.split("/")[2] ?? "";
  expect(gameId).not.toBe("");

  await page.getByTestId("new-edition").click();
  await page.getByTestId("edition-region").fill("us");
  await page.getByTestId("edition-language").fill("en");
  await page.getByTestId("edition-revision").fill("dvc-1");
  await page.getByTestId("edition-submit").click();
  await expect(page.getByRole("tab", { name: "us / en / dvc-1" })).toBeVisible();
  await page
    .getByTestId(/edition-panel-/)
    .getByTestId("new-draft")
    .click();
  await expect(page.getByTestId("revision-1")).toBeVisible();

  const revision1 = page.getByTestId("revision-1");
  await revision1.getByTestId("import-file").setInputFiles(ADVENTURE_CSV);
  await revision1.getByTestId("import-submit").click();
  await expect(page.getByTestId("job-progress")).toContainText("succeeded", { timeout: 60_000 });
  await expect(page.getByTestId("import-inserted")).toHaveText("40");

  await page.getByTestId("cast-1").click();
  const profileId = (await page.url()).match(/profile=([^&]+)/)?.[1] ?? "";
  expect(profileId).not.toBe("");

  // ── 3. seed the takes: the auto-cast dial approves them (one take each) ───
  for (const row of [mismatch, okRow]) {
    const created = (await (
      await api.post("/api/v1/voices/auditions", {
        data: {
          profile_id: profileId,
          character: row.character,
          brief: row.brief,
          count: 1,
        },
      })
    ).json()) as { job_id: string; state: string; auto_cast: boolean | null };
    expect(created.state).toBe("draft");
    expect(created.auto_cast, "the dial is on: the run is the cast's").toBe(true);
    await jobResult(request, created.job_id);
  }

  // ── 4. the audit endpoint: the seeded mismatch is MISMATCH, the control OK ─
  const audit = (await (await api.get(`/api/v1/profiles/${profileId}/cast/audit`)).json()) as {
    profile: string;
    rows: AuditRow[];
  };
  expect(audit.profile).toBe(profileId);

  const mismatchRow = audit.rows.find((r) => r.character === mismatch.character);
  expect(mismatchRow, "the mismatch character is on the roster").toBeTruthy();
  const mismatchFreq = freqOf(mismatch.brief);
  expect(mismatchRow!.verdict).toBe("MISMATCH");
  expect(mismatchRow!.expected).toBe(mismatch.expected);
  expect(mismatchRow!.measured).toBe("high (child)");
  expect(Math.abs(mismatchRow!.measured_f0_hz - mismatchFreq)).toBeLessThan(2);
  expect(mismatchRow!.measured).not.toBe(mismatch.expected);

  const okAuditRow = audit.rows.find((r) => r.character === okRow.character);
  expect(okAuditRow, "the control character is on the roster").toBeTruthy();
  expect(okAuditRow!.verdict).toBe("OK");
  expect(Math.abs(okAuditRow!.measured_f0_hz - freqOf(okRow.brief))).toBeLessThan(2);

  // ── 5. the Cast page's badge is the verdict, visible on the row ───────────
  // The takes were created through the API, not the page's mutation, so the
  // screen does not know the voice list moved: the operator returns to the
  // cast (a reload) and the page reads the fresh voices and the fresh audit.
  await page.reload();
  await expect(page.getByTestId("character-table")).toBeVisible();
  await expect(
    page.getByTestId(`character-voice-check-${encodeURIComponent(mismatch.character)}`),
  ).toContainText("mismatch", { timeout: 30_000 });
  await expect(
    page.getByTestId(`character-voice-check-${encodeURIComponent(okRow.character)}`),
  ).toContainText("OK");

  // The badge is small: a cell, not a section — the row keeps its name and
  // the rest of the column layout (the Voice column still shows the take).
  await expect(
    page.getByTestId(`character-voice-${encodeURIComponent(mismatch.character)}`),
  ).toContainText(/voiced/);

  // The walk did not churn the page: no console errors beyond the allowlist.
  expect(consoleErrors).toEqual([]);

  // Isolation: nothing this walk did may have touched the real data directory.
  expectDefaultDataDirUntouched(run);
});
