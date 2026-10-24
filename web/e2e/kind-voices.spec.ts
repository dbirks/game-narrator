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
 * The generic kind voices (ADR-0158, issue #51, ADR-0185) on the packaged
 * binary with the scripted speech providers (fake-tts.mjs / fake-asr.mjs):
 * the Cast page's "Generic kind voices" section lists the seven kind:* keys
 * with their state and measured pitch class, and the one-click
 * "Create missing" auditions each missing kind, measures the take with the
 * cast audit's classifier, retries the out-of-class takes, and leaves the
 * passing takes PENDING for the operator's approve click.
 *
 * The walk seeds nothing that the run does not create itself: the fixed
 * kind briefs live in internal/cast (Briefs), and the scripted design's tone
 * frequency is a pure function of (line, brief, model) — `160 + djb2(seed)
 * % 400` Hz — so the spec computes each brief's take F0 before it exists and
 * asserts up front that every brief lands in its kind's expected class
 * (girl: female range, boy: child range — the cast audit's mapping, not a
 * boy/girl symmetry). A change in the stub's tone law is a spec failure, not
 * a silently flipped verdict. The briefs must also be seeded into
 * FAKE_ASR_BRIEF (playwright.config.ts) so the QA read-back passes.
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

const ADMIN = { username: "voices-admin", password: "steady as she goes" }; // the shared `voices` server: the first spec to run claims it, so every voices spec signs in as the same admin

// Must match internal/cast Briefs (the create-missing run's fixed briefs) —
// the fake ASR seeds on the exact string, and the tone law hashes it.
const BRIEFS: Record<string, string> = {
  adult_male:
    "A mature adult man, a deep resonant voice, unmistakably male and low-pitched, plain and level.",
  adult_female: "An adult woman, a mid-range voice, clearly female, low-key and unforced.",
  elder_male:
    "An elderly man, a deep baritone, unmistakably male and low-pitched, unhurried and natural.",
  elder_female:
    "An elderly woman, a mid-range voice with the weight of age, clearly female, clear and unhurried.",
  boy: "A young boy, a small child's voice, high-pitched and bright, steady and warm.",
  girl: "A young girl, a mid-range voice, clearly female and youthful, steady and warm.",
  creature: "A non-human creature: an alien, inhuman timbre, neither a man's nor a woman's voice.",
};

// The kind id -> the class the cast audit's KindToClass expects.
const KIND_CLASS: Record<string, string> = {
  adult_male: "low (adult male)",
  elder_male: "low (adult male)",
  adult_female: "mid (adult female)",
  elder_female: "mid (adult female)",
  girl: "mid (adult female)",
  boy: "high (child)",
  creature: "unmeasured", // no class to match: any measured class is acceptable
};

// The first synthetic audition line (count 1: one take per kind).
const LINE = "Hello there. Let me tell you a little about today.";

// fake-tts.mjs's tone law: 160 + djb2(`${input}|${instructions}|${model}`) % 400.
function djb2(text: string): number {
  let h = 5381;
  for (let i = 0; i < text.length; i++) h = ((h << 5) + h + text.charCodeAt(i)) >>> 0;
  return h >>> 0;
}
const freqOf = (brief: string): number => 160 + (djb2(`${LINE}|${brief}|fake-voicedesign`) % 400);

// Spec invariants: every kind brief's take lands in the class the audit
// expects, so the run's first attempt is the passing one (the retry leg is
// the Go tests' business on this walk; here the pitch must be right on the
// first take or the walk would spend its retries on unseeded briefs).
for (const [kind, brief] of Object.entries(BRIEFS)) {
  const f = freqOf(brief);
  const inClass =
    KIND_CLASS[kind] === "low (adult male)"
      ? f < 165
      : KIND_CLASS[kind] === "mid (adult female)"
        ? f >= 165 && f <= 255
        : KIND_CLASS[kind] === "high (child)"
          ? f > 255
          : true;
  if (!inClass) {
    throw new Error(
      `spec invariant: ${kind}'s brief lands at ${f} Hz, outside its expected class ${KIND_CLASS[kind]}; re-derive the briefs`,
    );
  }
}

interface KindRow {
  kind: string;
  character: string;
  state: string;
  voice_id?: string;
  measured_f0_hz: number;
  measured: string;
  attempts?: number;
  note?: string;
}

test("the cast page's generic kind voices section runs the one-click create with a visible pitch class per kind", async ({
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
  await page.getByTestId("new-game-slug").fill("kind-voices");
  await page.getByTestId("new-game-title").fill("Kind Voices");
  await page.getByTestId("new-game-platform").fill("GameCube");
  await page.getByTestId("new-game-submit").click();
  await expect(page.getByTestId("game-kind-voices")).toContainText("Kind Voices");
  await page.getByTestId("game-kind-voices").click();

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

  // ── 3. the cast page ──────────────────────────────────────────────────────
  await page.getByTestId("cast-1").click();
  await expect(page.getByTestId("cast-page")).toBeVisible();

  // The section lists all seven kinds, each a row with a state and a pitch.
  const section = page.getByTestId("kind-voices-section");
  await expect(section).toBeVisible();
  for (const kind of Object.keys(BRIEFS)) {
    await expect(page.getByTestId(`kind-voice-row-${kind}`)).toBeVisible();
    await expect(page.getByTestId(`kind-voice-state-${kind}`)).toBeVisible();
    await expect(page.getByTestId(`kind-voice-pitch-${kind}`)).toBeVisible();
  }

  // ── 4. the one-click run ──────────────────────────────────────────────────
  const coverage = async (): Promise<KindRow[]> => {
    const body = (await (await api.get("/api/v1/voices/kinds")).json()) as { kinds: KindRow[] };
    return body.kinds;
  };
  const before = await coverage();
  expect(before.length).toBe(7);
  const allMissing = before.every((r) => r.state === "missing");

  if (allMissing) {
    // A fresh appliance: the button is offered and the run is synchronous.
    const button = page.getByTestId("kind-voices-create-missing");
    await expect(button).toBeVisible();
    await button.click();
    // The run auditions seven kinds (one take each): wait for the page to
    // read back the pending takes.
    await expect(page.getByTestId("kind-voice-state-adult_male")).toContainText("pending", {
      timeout: 120_000,
    });
  } else {
    // A rerun on the shared appliance: the kinds already carry their takes.
    const button = page.getByTestId("kind-voices-create-missing");
    const missing = before.filter((r) => r.state === "missing").length;
    if (missing === 0) await expect(button).toHaveCount(0);
  }

  // ── 5. the result: a visible pitch class per kind, pending for approval ──
  const after = await coverage();
  const byKind = Object.fromEntries(after.map((r) => [r.kind, r]));
  for (const kind of Object.keys(BRIEFS)) {
    const row = byKind[kind];
    expect(row, `the ${kind} row is on the wire`).toBeTruthy();
    expect(["pending", "approved"], `the ${kind} state`).toContain(row.state);
    // The measured F0 is the scripted tone (a stub with a changed tone law
    // would show up here, not as a silently wrong class).
    expect(Math.abs(row.measured_f0_hz - freqOf(BRIEFS[kind]))).toBeLessThan(2);
  }
  // The class the audit measured is the class the kind expects (creature is
  // unclassable — its row just carries a measured tone).
  for (const kind of ["adult_male", "elder_male", "adult_female", "elder_female", "girl", "boy"]) {
    expect(byKind[kind].measured, `${kind} measured class`).toBe(KIND_CLASS[kind]);
  }
  expect(byKind["creature"].measured_f0_hz, "the creature's measured tone").toBeGreaterThan(0);

  // And the page shows it: the pitch cell carries the class and the Hz.
  for (const kind of ["adult_male", "elder_male", "adult_female", "elder_female", "girl", "boy"]) {
    await expect(page.getByTestId(`kind-voice-pitch-${kind}`)).toContainText(KIND_CLASS[kind]);
    await expect(page.getByTestId(`kind-voice-pitch-${kind}`)).toContainText(/Hz/);
  }
  // The passing takes are pending: the approve click is the operator's
  // (AGENTS.md rule 6 — the run never auto-approves).
  for (const kind of Object.keys(BRIEFS)) {
    if (byKind[kind].state === "pending") {
      await expect(page.getByTestId(`kind-voice-state-${kind}`)).toContainText("pending");
    }
  }

  // The walk did not churn the page: no console errors beyond the allowlist.
  expect(consoleErrors).toEqual([]);

  // Isolation: nothing this walk did may have touched the real data directory.
  expectDefaultDataDirUntouched(run);
});
