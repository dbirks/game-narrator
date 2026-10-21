import AxeBuilder from "@axe-core/playwright";
import { expect, test, type APIRequestContext, type Page } from "@playwright/test";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

import { trackConsoleErrors } from "./support/console";
import { apiAsUser, expectDefaultDataDirUntouched, serverRun } from "./support/servers";

/**
 * The merged Cast screen (ADR-0093, docs/design/plum-peach 2d) end to end
 * against the packaged binary with the scripted speech providers
 * (web/e2e/fake-tts.mjs): the real API process, the real job queue, the real
 * store, the real speech cache — only the *models* are scripted (AGENTS.md
 * rule 3, the same boundary ADR-0022 draws).
 *
 * The walk asserts the merge's acceptance criteria:
 *
 *   1. one screen for the roster and the voice work: the old /characters and
 *      /voices paths redirect here, query included;
 *   2. the auto-cast dial is on by default (ADR-0109, UX-03): generating a
 *      take enqueues one design run and the job auto-approves it — exactly
 *      one approved revision, zero pending, the consent attributed to the
 *      profile default, and no per-character prompt;
 *   3. the brief edit + regeneration is a new current version; the previous
 *      take is not deleted and stays playable, and is one step away (the
 *      "keep old take" undo, UX-03d); a second undo is a 409;
 *   4. the `?character=` selection is the URL and survives a reload;
 *   5. the list/cards toggle persists across reloads (UX-08).
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

const ADMIN = { username: "voices-admin", password: "steady as she goes" }; // the one `voices` server the voices, voices-qa and cast specs share — the appliance claim (setup) happens once, so all three must name the same admin
const CHARACTER = "Boy"; // the voices project's specs share one appliance and voice revisions are keyed by character name across it (UNIQUE (character, revision), migration 0009): voices.spec.ts owns Marla, voices-qa.spec.ts owns Corvin, this walk owns Boy — the spec's character is one of the CSV's speaker column values
const CONSENT_DEFAULT = "original_synthetic_design";
/** The design's three review widths (docs/design/plum-peach README, IA). */
const WIDTHS = [1440, 1024, 390] as const;
/** WCAG 2.1 A/AA: the 2.0 A/AA rules plus the rules 2.1 adds at those levels. */
const WCAG_21_A_AA = ["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"] as const;

interface ScanReport {
  url: string;
  width: number;
  serious: string[];
  violations: Record<string, number>;
}

interface VoiceRevision {
  id: string;
  revision: number;
  state: string;
  ref_transcript: string;
  fingerprint: string;
  consent_provenance: string;
  design_instruction: string;
}

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
  // The `request` fixture is its own context with its own cookie jar.
  return page.context().cookies();
}

/** Wait for the audition job and hand back its result (the auto-cast
 *  outcome rides on it: auto_approved, ADR-0109). */
async function jobResult(request: APIRequestContext, jobId: string) {
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
  const detail = (await (await request.get(`/api/v1/jobs/${jobId}`)).json()) as {
    result: Record<string, unknown>;
  };
  return detail.result;
}

async function marlaVoices(request: APIRequestContext, origin: string): Promise<VoiceRevision[]> {
  const list = (await (
    await request.get(`${origin}/api/v1/voices?character=${encodeURIComponent(CHARACTER)}`)
  ).json()) as { voices: VoiceRevision[] };
  return list.voices.sort((a, b) => b.revision - a.revision);
}

test("the cast screen: auto-cast default, regenerate + one-step undo, selection, toggle", async ({
  page,
  request,
  baseURL,
}) => {
  test.setTimeout(600_000);
  // The cast walk shares the voices project's server (the same fake speech
  // roles), so its isolation record is the voices run's.
  const run = serverRun("voices");
  const origin = new URL(baseURL as string).origin;
  const api = apiAsUser(request, origin);

  // 401 is allowlisted: the unauthenticated /auth/me probe the guard makes on
  // mount. Every other console error fails the cleanliness check at the end.
  const consoleErrors = trackConsoleErrors(page, [401]);

  // ── 1. first run: claim the appliance, sign in ────────────────────────────
  const cookies = await signIn(page);
  await request.addCookies(cookies);

  // ── 2. a game, an edition, a draft revision ────────────────────────────────
  await page.getByTestId("new-game").click();
  await page.getByTestId("new-game-slug").fill("cast-harbour");
  await page.getByTestId("new-game-title").fill("Cast Harbour");
  await page.getByTestId("new-game-platform").fill("GameCube");
  await page.getByTestId("new-game-submit").click();
  await expect(page.getByTestId("game-cast-harbour")).toContainText("Cast Harbour");

  await page.getByTestId("game-cast-harbour").click();
  await expect(page.getByTestId("game-title")).toHaveText("Cast Harbour");
  const gameId = new URL(page.url()).pathname.split("/")[2] ?? "";
  expect(gameId).not.toBe("");

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

  // ── 4. the merged screen: one header, the dial on, the roster ─────────────
  await page.getByTestId("cast-1").click();
  await expect(page).toHaveURL(/\/cast\?profile=/);
  const profileId = (await page.url()).match(/profile=([^&]+)/)?.[1] ?? "";
  expect(profileId).not.toBe("");

  await expect(page.getByTestId("cast-title")).toHaveText("Cast Harbour");
  await expect(page.getByTestId("cast-tab-cast")).toHaveAttribute("aria-current", "page");
  // The header carries the game's way out and the play entry (ADR-0093 (a)).
  await expect(page.getByTestId("cast-tab-overview")).toBeVisible();
  await expect(page.getByTestId("cast-start-narrating")).toBeVisible();
  // The dial is on by default (ADR-0109, UX-03): auto-cast is the product
  // default for new setups.
  await expect(page.getByTestId("auto-cast-switch")).toBeChecked();

  // ── 5. the roster: selection is the URL ───────────────────────────────────
  await expect(page.getByTestId("voice-characters")).toBeVisible();
  await expect(page.getByTestId("character-table")).toBeVisible();
  await expect(page.getByTestId(`character-row-${CHARACTER}`)).toBeVisible();
  await page.getByTestId(`cast-row-name-${CHARACTER}`).click();
  await expect(page).toHaveURL(new RegExp(`character=${encodeURIComponent(CHARACTER)}$`));
  await expect(page.getByTestId("cast-detail-name")).toHaveText(CHARACTER);

  // The selection survives a reload: the URL carries it, the panel re-opens.
  await page.reload();
  await expect(page).toHaveURL(new RegExp(`character=${encodeURIComponent(CHARACTER)}$`));
  await expect(page.getByTestId("cast-detail-name")).toHaveText(CHARACTER, { timeout: 20_000 });

  // ── 6. auto-cast, on: one take, auto-approved, no prompt ──────────────────
  // The roster shows the character's take lifecycle by testid (UX-03):
  // queued while the design job runs, voiced once the take is approved. The
  // window is short with the scripted providers, so assert the chip reaches
  // a ready state rather than racing the draft frame.
  const chip = page.getByTestId(`character-voice-state-${CHARACTER}`);
  // The briefs are the fake ASR's seeded briefs (playwright.config.ts,
  // FAKE_ASR_BRIEF, ";;"-separated): the stub recovers the spoken line from
  // the tone's frequency, and the seed couples line + brief + model, so an
  // unseeded brief would fail every QA read-back and the auto-cast would
  // refuse the take. The edit is a genuinely different string — the run the
  // spec regenerates with — seeded as the second entry.
  const brief1 = "A calm harbor voice, unhurried, with a slight salt air in it.";
  await page.getByTestId("voice-brief").fill(brief1);
  const accepted = page.waitForResponse(
    (r) => r.request().method() === "POST" && r.url().includes("/api/v1/voices/auditions"),
  );
  await page.getByTestId("generate-auditions").click();
  const createResponse = (await (await accepted).json()) as {
    job_id: string;
    voice_id: string;
    revision: number;
    state: string;
    auto_cast: boolean | null;
  };
  expect(createResponse.state).toBe("draft");
  expect(createResponse.revision).toBe(1);
  // The response echoes the dial as the API read it at enqueue (ADR-0109).
  expect(createResponse.auto_cast).toBe(true);
  // The queued state is real, not UI theatre: the draft revision is
  // addressable the instant the 202 lands. With the scripted providers the
  // design job may already be filling it by the time this read lands, so the
  // assertion is on existence and liveness, not on the tick the fill left.
  const draftNow = (await (
    await request.get(`${origin}/api/v1/voices/${createResponse.voice_id}`)
  ).json()) as {
    state: string;
  };
  expect(["draft", "auditioning", "approved"]).toContain(draftNow.state);

  const result = await jobResult(request, createResponse.job_id);
  expect(result.auto_approved, "the run auto-approves its take").toBe(true);

  // The page caught up: the take is approved, and the run never asked a
  // human anything — no audition cards, no consent prompt, no approve
  // button (UX-03: no per-character prompt).
  await expect(page.getByTestId("voice-state-badge")).toHaveText("approved", {
    timeout: 30_000,
  });
  await expect(page.getByTestId("audition-card-0")).toHaveCount(0);
  await expect(page.getByTestId("approve-voice")).toHaveCount(0);
  await expect(page.getByTestId("consent-input")).toHaveCount(0);
  await expect(chip).toHaveText("voiced · v1", { timeout: 30_000 });

  // Exactly one approved version and zero pending, from outside the page.
  const voices1 = await marlaVoices(request, origin);
  expect(voices1).toHaveLength(1);
  expect(voices1[0].state).toBe("approved");
  // The consent is attributed to the profile default, not a click (ADR-0109).
  const consent = `${CONSENT_DEFAULT}:profile-default:${profileId}`;
  expect(voices1[0].consent_provenance).toBe(consent);
  await expect(page.getByTestId("voice-consent")).toContainText(consent);
  // The current take plays in the panel (the peach pill of docs/design 2d).
  await expect(page.getByTestId("cast-take-player")).toBeVisible();
  await expect(page.getByTestId("cast-take-audio")).toBeVisible();

  // ── 7. regenerate: the brief edit is a new current version ────────────────
  // The button label moves with the state (docs/design/plum-peach 2d).
  await expect(page.getByTestId("generate-auditions")).toHaveText("↻ Regenerate voice");
  const brief2 = "A steadier take: slower, with the salt further out.";
  await page.getByTestId("voice-brief").fill(brief2);
  const regen = page.waitForResponse(
    (r) => r.request().method() === "POST" && r.url().includes("/api/v1/voices/auditions"),
  );
  await page.getByTestId("generate-auditions").click();
  const regenResponse = (await (await regen).json()) as {
    job_id: string;
    voice_id: string;
    revision: number;
  };
  expect(regenResponse.revision).toBe(2);
  const regenResult = await jobResult(request, regenResponse.job_id);
  expect(regenResult.auto_approved).toBe(true);

  await expect(page.getByTestId("voice-state-badge")).toHaveText("approved", {
    timeout: 30_000,
  });
  const voices2 = await marlaVoices(request, origin);
  expect(voices2).toHaveLength(2);
  const [second, first] = voices2;
  expect(second.state).toBe("approved");
  // The previous take is not deleted and stays approved (it stays playable;
  // the runtime speaks the highest approved, so v1 is silent until undone).
  expect(first.state).toBe("approved");
  // A genuinely different take: new fingerprint, same consent rule.
  expect(second.fingerprint).not.toBe(first.fingerprint);
  expect(second.design_instruction).toBe(brief2);
  expect(second.consent_provenance).toBe(consent);
  await expect(chip).toHaveText("voiced · v2", { timeout: 30_000 });

  // ── 8. the one-step undo: keep the old take ───────────────────────────────
  await expect(page.getByTestId("keep-old-take")).toBeVisible();
  const undo = page.waitForResponse(
    (r) => r.request().method() === "POST" && r.url().includes("/undo"),
  );
  await page.getByTestId("keep-old-take").click();
  expect(await (await undo).status()).toBe(200);

  const voices3 = await marlaVoices(request, origin);
  const [undone, restored] = voices3;
  expect(undone.id).toBe(second.id);
  expect(undone.state).toBe("retired");
  expect(restored.id).toBe(first.id);
  expect(restored.state).toBe("approved");
  // The panel agrees: v1 is the take again, and the undo is gone (one step).
  await expect(chip).toHaveText("voiced · v1", { timeout: 30_000 });
  await expect(page.getByTestId("keep-old-take")).toHaveCount(0);
  // The restored take still plays: nothing was deleted, only reordered.
  await expect(page.getByTestId("cast-take-player")).toBeVisible();
  await expect(page.getByTestId("cast-take-audio")).toBeVisible();

  // A second undo has nothing to undo: the domain refuses it (409).
  const nothingToUndo = await api.post(`/api/v1/voices/${first.id}/undo`);
  expect(nothingToUndo.status()).toBe(409);
  const refusal = (await nothingToUndo.json()) as { code: string };
  expect(refusal.code).toBe("NOTHING_TO_UNDO");

  // ── 9. the old routes redirect, query included (ADR-0093) ─────────────────
  await page.goto(`/games/${gameId}/voices?profile=${profileId}&character=${CHARACTER}`);
  await expect(page).toHaveURL(new RegExp(`/cast\\?profile=${profileId}&character=${CHARACTER}`));
  await expect(page.getByTestId("cast-detail-name")).toHaveText(CHARACTER, { timeout: 20_000 });

  await page.goto(`/games/${gameId}/characters?profile=${profileId}`);
  await expect(page).toHaveURL(new RegExp(`/cast\\?profile=${profileId}`));
  await expect(page.getByTestId("character-table")).toBeVisible({ timeout: 20_000 });

  // ── 10. the view toggle persists (UX-08) ──────────────────────────────────
  await expect(page.getByTestId("character-table")).toBeVisible();
  const toggle = page.getByTestId("cast-view-toggle");
  await toggle.getByRole("tab", { name: "cards" }).click();
  await expect(page.getByTestId("cast-cards")).toBeVisible();
  await expect(page.getByTestId("character-table")).toHaveCount(0);
  const stored = await page.evaluate(() => {
    const ls = (globalThis as { localStorage?: { getItem(k: string): string | null } })
      .localStorage;
    return ls?.getItem("gn.cast.view") ?? null;
  });
  expect(stored).toBe("cards");
  await page.reload();
  await expect(page.getByTestId("cast-cards")).toBeVisible({ timeout: 20_000 });
  await expect(page.getByTestId("character-table")).toHaveCount(0);

  // ── 11. nothing leaked, nothing was touched elsewhere ─────────────────────
  expect(consoleErrors, `browser console: ${consoleErrors.join("\n")}`).toEqual([]);
  expectDefaultDataDirUntouched(run);
});

/**
 * The same screen, the design's three review widths (docs/design/plum-peach
 * plan, Phase 1 item 5): the settled roster with the walk's take selected,
 * screenshot into test-results at 1440 / 1024 / 390, axe (WCAG 2.1 A/AA) on
 * every width — any serious or critical violation fails the walk — and no
 * horizontal page scroll at 390 (wide cells scroll inside the roster, the
 * document never does).
 *
 * Runs after the walk above, on the walk's game and take: the screenshots
 * show the real screen state (a voiced character, a reference line, the
 * current-take player), not an empty shell.
 */
test("the cast screen: screenshots and axe at the design's three widths", async ({
  page,
}, testInfo) => {
  test.setTimeout(300_000);
  const run = serverRun("voices");
  const consoleErrors = trackConsoleErrors(page, [401]);

  // The walk's game and take already exist on this server; sign in and open
  // the merged screen with the walk's character selected (its approved take
  // is the one the walk left current).
  await signIn(page);
  await page.goto("/library");
  await page.getByTestId("game-cast-harbour").click();
  await expect(page.getByTestId("game-title")).toHaveText("Cast Harbour");
  await page.getByTestId("cast-1").click();
  await expect(page).toHaveURL(/\/cast\?profile=/);
  await page.getByTestId(`cast-row-name-${CHARACTER}`).click();
  await expect(page.getByTestId("cast-detail-name")).toHaveText(CHARACTER);
  // The take's facts settled (the player fetched the real clip).
  await expect(page.getByTestId("cast-take-player")).toBeVisible({ timeout: 30_000 });

  const reports: ScanReport[] = [];
  for (const width of WIDTHS) {
    await page.setViewportSize({ width, height: 900 });
    await expect(page.getByTestId("character-table")).toBeVisible({ timeout: 15_000 });
    await expect(page.getByTestId("cast-detail-name")).toHaveText(CHARACTER);
    // Settle the fonts: the self-hosted faces swap in asynchronously
    // (font-display: swap) and the fallback metrics are wider — measuring
    // (or shooting) before the swap lands would read the fallback layout.
    await page.evaluate(async () => {
      const g = globalThis as unknown as {
        document?: { fonts?: { ready?: Promise<unknown> } };
      };
      await g.document?.fonts?.ready;
    });
    // Reset the scroll before any in-viewport measurement: the review pass
    // measures the page as it first renders, at the top.
    await page.evaluate(() => {
      const g = globalThis as { scrollTo?: (x: number, y: number) => void };
      g.scrollTo?.(0, 0);
    });

    // The review fixes (run-3937210), asserted so they stay fixed:
    // the auto-cast dial is the kit's switch — a checkbox with role=switch,
    // not a plain checkbox with a browser accent colour.
    await expect(page.getByTestId("auto-cast-switch")).toHaveAttribute("role", "switch");
    await expect(page.getByTestId("auto-cast-switch")).toBeChecked();

    // The facts are a two-column definition list; the long consent record
    // carries the full text in the title and a copy button beside the
    // truncated line (the click works headless through the clipboard
    // fallback and flips the button's label).
    expect(await page.getByTestId("voice-state").evaluate((el) => el.tagName)).toBe("DL");
    const profileId = page.url().match(/profile=([^&]+)/)?.[1] ?? "";
    expect(profileId).not.toBe("");
    const consent = `${CONSENT_DEFAULT}:profile-default:${profileId}`;
    const consentCode = page.getByTestId("voice-consent").locator("code");
    await expect(consentCode).toHaveAttribute("title", consent);
    const copy = page.getByTestId("consent-copy");
    await expect(copy).toBeVisible();
    await copy.click();
    await expect(copy).toHaveText("copied ✓");

    // The gain is the styled range (coral thumb on the ink track) with its
    // value and the "applied at playback" note on its own line — the note's
    // top sits at or below the slider's bottom, so nothing overlaps.
    await expect(page.getByTestId("voice-gain-slider")).toBeVisible();
    const sliderBox = (await page.getByTestId("voice-gain-slider").boundingBox()) ?? {
      y: 0,
      height: 0,
    };
    const noteBox = (await page.getByTestId("voice-gain-note").boundingBox()) ?? {
      y: sliderBox.y + sliderBox.height,
    };
    expect(
      noteBox.y,
      `the gain note overlaps the slider (note top ${noteBox.y} < slider bottom ${
        sliderBox.y + sliderBox.height
      })`,
    ).toBeGreaterThanOrEqual(sliderBox.y + sliderBox.height - 1);

    // Phone width (the design's third review width): no horizontal page
    // scroll — the roster's wide cells stay inside the page, so the document
    // must never be wider than the viewport (docs/design/plum-peach IA).
    if (width === 390) {
      const widths = await page.evaluate(() => {
        const g = globalThis as {
          document?: { documentElement?: { scrollWidth?: number } };
          innerWidth?: number;
        };
        return {
          scrollWidth: g.document?.documentElement?.scrollWidth ?? 0,
          innerWidth: g.innerWidth ?? 0,
        };
      });
      expect(
        widths.scrollWidth,
        `cast @390px scrolls horizontally: documentElement.scrollWidth ${widths.scrollWidth} > innerWidth ${widths.innerWidth}`,
      ).toBeLessThanOrEqual(widths.innerWidth);
      // The cast table reflows into stacked row cards under 600px (the
      // Voice column was being cut off inside its row): the table itself is
      // display:block, the head is gone, and the voice cell's label comes
      // from the data-th attribute.
      expect(
        await page.getByTestId("character-table").evaluate((el) => {
          const view = ((el as { ownerDocument?: { defaultView?: unknown } }).ownerDocument
            ?.defaultView ?? undefined) as
            | {
                getComputedStyle?: (node: unknown) => { display?: string };
              }
            | undefined;
          return view?.getComputedStyle?.(el).display ?? "";
        }),
      ).toBe("block");
      const voiceCell = page.getByTestId(`character-voice-${CHARACTER}`);
      expect(
        await voiceCell.evaluate((el) => {
          const view = ((el as { ownerDocument?: { defaultView?: unknown } }).ownerDocument
            ?.defaultView ?? undefined) as
            | {
                getComputedStyle?: (node: unknown, pseudo?: string) => { content?: string };
              }
            | undefined;
          return view?.getComputedStyle?.(el, "::before").content ?? "";
        }),
      ).toBe('"voice"');
      // The voice cell (the column that used to be cut off) sits fully
      // inside the viewport, top to bottom.
      const voiceBox = (await voiceCell.boundingBox()) ?? { x: 0, width: 0 };
      expect(voiceBox.x + voiceBox.width).toBeLessThanOrEqual(390);
    }

    // The detail panel grows with the page (no inner scroll clip): at the
    // 1440×900 review width the coral Regenerate action, its companions and
    // the sample lines are visible without scrolling.
    if (width === 1440) {
      const regenerate = (await page.getByTestId("generate-auditions").boundingBox()) ?? null;
      expect(regenerate, "the Regenerate action is missing at 1440×900").not.toBeNull();
      expect(
        regenerate!.y,
        `Regenerate is above the viewport top (y ${regenerate!.y}) at 1440×900`,
      ).toBeGreaterThanOrEqual(0);
      expect(
        regenerate!.y + regenerate!.height,
        `Regenerate is below the 900px fold (bottom ${
          regenerate!.y + regenerate!.height
        }) at 1440×900 — the panel must show it without scrolling`,
      ).toBeLessThanOrEqual(900);
      await expect(page.getByTestId("more-takes")).toBeInViewport();
      await expect(page.getByTestId("hear-lines")).toBeInViewport();
      // The sample lines sit below the preview panel at this width: the
      // review's point is that the panel no longer clips them in its own
      // scroll — they are in the page flow (the full-page screenshot shows
      // them), not hidden past an inner max-height.
      await expect(page.getByTestId("cast-sample-lines")).toBeVisible();
    }
    const shot = `cast-${width}.png`;
    const shotPath = `${testInfo.outputDir}/${shot}`;
    await page.screenshot({ path: shotPath, fullPage: true });
    await testInfo.attach(shot, { body: readFileSync(shotPath), contentType: "image/png" });

    const results = await new AxeBuilder({ page }).withTags([...WCAG_21_A_AA]).analyze();
    const violations = results.violations ?? [];
    const byRule: Record<string, number> = {};
    for (const violation of violations) {
      byRule[violation.id] = (byRule[violation.id] ?? 0) + violation.nodes.length;
    }
    reports.push({
      url: page.url(),
      width,
      violations: Object.fromEntries(Object.entries(byRule).sort(([a], [b]) => a.localeCompare(b))),
      serious: violations
        .filter((violation) => violation.impact === "serious" || violation.impact === "critical")
        .map(
          (violation) =>
            `${violation.id} [${violation.impact}] ×${violation.nodes.length} — ${violation.help}`,
        ),
    });
  }
  await testInfo.attach("axe-reports.json", {
    body: JSON.stringify(reports, null, 2),
    contentType: "application/json",
  });

  // any serious or critical violation on any width fails the walk.
  const failing = reports.filter((report) => report.serious.length > 0);
  expect(
    failing,
    `serious/critical axe violations on the cast screen:\n${failing
      .flatMap((report) => report.serious.map((rule) => `@${report.width}: ${rule}`))
      .join("\n")}`,
  ).toEqual([]);
  expect(consoleErrors, `browser console: ${consoleErrors.join("\n")}`).toEqual([]);
  expectDefaultDataDirUntouched(run);
});
