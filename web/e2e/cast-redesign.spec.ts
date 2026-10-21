import { expect, test, type Page } from "@playwright/test";
import { randomUUID } from "node:crypto";

import { trackConsoleErrors } from "./support/console";
import { buildManifest, type SeedProfile } from "./support/manifest";
import {
  apiAsUser,
  claimAppliance,
  expectDefaultDataDirUntouched,
  serverRun,
} from "./support/servers";

/**
 * The Cast redesign (ADR-0135), end to end against the packaged binary with
 * the scripted speech providers (web/e2e/fake-tts.mjs + fake-asr.mjs): the
 * real API process, the real job queue and the real store behind; only the
 * *models* are scripted (AGENTS.md rule 3, the boundary ADR-0022 draws).
 *
 * The seed is the 67-row roster the redesign's order and filters exist for:
 * ten named speaking characters, forty-five unnamed placeholders (the
 * import's proposed rows carrying the source's own source_field linkage) and
 * twelve retired rows — the board that reads "a horrible interface to go
 * pick voices" before the change. The walk asserts the change:
 *
 *   1. the roster's order: named first, line count descending, the retired
 *      rows hidden behind their own chip (`All` hides them by default);
 *   2. the filter chips and the name search, each with its own count;
 *   3. the shared voiced count: the header chip reads the same pair the
 *      Library read takes from the server (0 of 55 at rest);
 *   4. the picker: the row's one action opens it, a take plays (the
 *      audition clip's blob), and "Approve this take" with the prefilled
 *      consent default approves it through the panel;
 *   5. the bulk: "Voice everyone with auto-cast" enqueues the named,
 *      unvoiced characters one at a time and reports n of m as the jobs
 *      land (the chip and the server's read catch up to 10 of 55);
 *   6. "Name this speaker": the placeholder's lines, a name, and the
 *      speaker-link that resolves it (a new confirmed character; the
 *      placeholder retires behind its chip);
 *   7. the 2026-10-21 review's structural-contrast gate (axe's
 *      color-contrast cannot see a fill-on-fill control — it has no text —
 *      so the review's finding escaped it): the active tab's label against
 *      its effective background, and the solid primary fill against the
 *      page ground, both at or above 3:1, asserted in BOTH themes (the
 *      default Plum and Lilac Night, the persisted-theme seam); and the
 *      390 viewport-fit gate with the picker open: no document overflow,
 *      and the full-screen sheet and its fields inside the viewport
 *      (fixed elements do not stretch document.scrollWidth, so the
 *      bounding boxes are checked as well as the scroll width).
 *
 * The spec runs on the `voices` project's server and signs in with its admin
 * (the appliance claim is once per appliance, ADR-0016 §7; a full-suite run
 * finds it claimed by cast/voices/voices-qa). Its character names are
 * invented and disjoint from those specs' (voices.spec owns Marla,
 * voices-qa Corvin, cast Boy) — voice revisions are keyed by name across the
 * appliance (UNIQUE (character, revision), migration 0009).
 */

const EDITION = {
  slug: "cast-redesign",
  title: "Redesign Harbour",
  region: "us",
  language: "en",
  revision: "rd-1",
};
const ADMIN = { username: "voices-admin", password: "steady as she goes" };
const CONSENT_DEFAULT = "original_synthetic_design";
// The page's default brief (castShared's DEFAULT_BRIEF) is the bulk work's
// brief: it is seeded into the scripted ASR (playwright.config.ts's
// FAKE_ASR_BRIEF, third entry) so its QA read-backs pass and the auto-cast
// approves.

// The ten named characters, line counts descending — the roster's
// needs-a-voice group in its own order (Tessa leads, Ada and Bosun Kip, one
// line each, end it). Invented names: the appliance-wide voice surface is
// keyed by name, and the shared server's other specs own other names.
const NAMED: [name: string, lines: number][] = [
  ["Tessa", 12],
  ["Old Bram", 9],
  ["Maren", 7],
  ["Pip", 5],
  ["Sela", 4],
  ["Cato", 3],
  ["Wren", 2],
  ["Fen", 2],
  ["Ada", 1],
  ["Bosun Kip", 1],
];
const UNNAMED = 45; // the import's placeholder rows (spk:1..spk:45)
const RETIRED = 12; // the dead rows, hidden behind their own chip

/** The 67-row roster the redesign's screen exists for: named first (the
 *  work's order), the import's unnamed placeholders (each line carrying the
 *  source's own source_field linkage — the data-driven "unnamed" predicate),
 *  and the retired rows (some with lines, to prove they do not count). */
function seed(): SeedProfile {
  const characters: SeedProfile["characters"] = [];
  const messages: SeedProfile["messages"] = [];
  for (const [name, lines] of NAMED) {
    const id = `chr-${name.toLowerCase().replace(/[^a-z0-9]/g, "-")}`;
    characters.push({ id, name, status: "confirmed" });
    for (let i = 0; i < lines; i++) {
      messages.push({
        id: `m-${id}-${i}`,
        original_text: `A line of the ${name} scene, take ${i + 1}.`,
        speaker: id,
        attribution: { status: "confirmed", confidence: 1 },
      });
    }
  }
  for (let i = 1; i <= UNNAMED; i++) {
    const id = `chr-spk-${i}`;
    const name = `spk:${i}`;
    const lines = ((i - 1) % 12) + 1;
    characters.push({ id, name, status: "proposed" });
    for (let k = 0; k < lines; k++) {
      messages.push({
        id: `m-${id}-${k}`,
        original_text: `A line the source hands to speaker ${i}.`,
        speaker: id,
        attribution: { status: "proposed", confidence: 0.5 },
        evidence: [
          { method: "source_field", field: "speaker", key: name, source_id: "seed", posterior: 1 },
        ],
      });
    }
  }
  for (let i = 1; i <= RETIRED; i++) {
    const id = `chr-dead-${i}`;
    const name = `Retired ${i}`;
    characters.push({ id, name, status: "retired" });
    if (i % 2 === 0) {
      for (let k = 0; k < 2; k++) {
        messages.push({
          id: `m-${id}-${k}`,
          original_text: `A dead line from retired character ${i}.`,
          speaker: id,
          attribution: { status: "confirmed", confidence: 1 },
        });
      }
    }
  }
  return {
    id: "cast-redesign-profile",
    origin: "synthetic",
    description: "The cast redesign's 67-row roster fixture (ADR-0135).",
    text_kind: "dialogue",
    messages,
    edges: [],
    regions: [],
    characters,
  };
}

async function signIn(page: Page) {
  await page.goto("/library");
  // The claim is once per appliance (ADR-0016 §7): a standalone run claims
  // here; a parallel-suite run finds it claimed by a sibling walk (the
  // helper's losing submit just walks to the sign-in).
  await claimAppliance(page, ADMIN);
  await page.getByTestId("login-username").fill(ADMIN.username);
  await page.getByTestId("login-password").fill(ADMIN.password);
  await page.getByTestId("login-submit").click();
  await expect(page.getByTestId("nav-user")).toContainText(ADMIN.username);
  return page.context().cookies();
}

/** The roster's first row's name, in the view's own row order. */
function firstName(page: Page): Promise<string> {
  return page
    .locator("[data-testid^='cast-row-name-']")
    .first()
    .textContent()
    .then((t) => t ?? "");
}

/** The DOM surface the in-page probes need, reached the suite's way (the
 * e2e tsconfig has no DOM lib; the document and viewport come through a
 * typed globalThis cast). */
type CastEl = {
  getBoundingClientRect: () => { right: number };
  parentElement: CastEl | null;
};
type CastGlobal = {
  document: {
    querySelector: (sel: string) => CastEl | null;
    querySelectorAll: (sel: string) => ArrayLike<CastEl>;
    documentElement: { dataset: Record<string, string | undefined>; scrollWidth: number };
    body: CastEl;
  };
  getComputedStyle: (el: CastEl) => { color: string; backgroundColor: string };
  innerWidth: number;
};

type ThemeContrasts = {
  theme: string;
  tabTextBg: number;
  pillBgPage: number;
  tabColor: string;
  tabBg: string;
  pillBg: string;
  pageBg: string;
};

/** The review's structural-contrast gate (2026-10-21): axe's color-contrast
 * rule has no text to measure on a solid control, so the fill-on-fill
 * control (Lilac Night's ink fill on the ink ground — the "no fill"
 * finding) reads 1.0:1 to the eye and 0 to axe. Measure the two
 * relationships the finding broke, explicitly: the active tab's LABEL
 * against the effective background behind it (the ancestor alphas
 * composited, not the nearest opaque), and the solid primary FILL against
 * the page ground — both at or above the 3:1 WCAG large/UI threshold the
 * review set. Runs per theme; the theme attribute is part of the result so
 * a seam that fails to apply the persisted theme cannot make a pass
 * vacuous. */
async function assertThemeContrasts(page: Page, expectTheme: string): Promise<void> {
  const result = (await page.evaluate(() => {
    const g = globalThis as unknown as CastGlobal;
    const parse = (v: string) => {
      const m = v.match(/rgba?\(([^)]+)\)/);
      if (!m) return null;
      const n = m[1].split(",").map((x) => parseFloat(x.trim()));
      if (n.length < 3 || n.some((x) => Number.isNaN(x))) return null;
      return { r: n[0], g: n[1], b: n[2], a: n.length > 3 ? n[3] : 1 };
    };
    const luminance = (c: { r: number; g: number; b: number }) => {
      const f = (u: number) => {
        const v = u / 255;
        return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4);
      };
      return 0.2126 * f(c.r) + 0.7152 * f(c.g) + 0.0722 * f(c.b);
    };
    const ratio = (
      a: { r: number; g: number; b: number },
      b: { r: number; g: number; b: number },
    ) => {
      const la = luminance(a);
      const lb = luminance(b);
      const hi = la >= lb ? la : lb;
      const lo = la >= lb ? lb : la;
      return (hi + 0.05) / (lo + 0.05);
    };
    // The effective background under el: the ancestor backgrounds
    // composited from the farthest (bottom) toward el, alpha-blended over
    // the canvas.
    const effectiveBg = (el: CastEl) => {
      const layers: Array<{ r: number; g: number; b: number; a: number }> = [];
      let node: CastEl | null = el;
      while (node) {
        const c = parse(g.getComputedStyle(node).backgroundColor);
        if (c) layers.push(c);
        node = node.parentElement;
      }
      let out: { r: number; g: number; b: number } | null = null;
      for (let i = layers.length - 1; i >= 0; i -= 1) {
        const c = layers[i];
        out =
          out === null
            ? c.a >= 1
              ? { r: c.r, g: c.g, b: c.b }
              : {
                  r: c.r * c.a + 255 * (1 - c.a),
                  g: c.g * c.a + 255 * (1 - c.a),
                  b: c.b * c.a + 255 * (1 - c.a),
                }
            : {
                r: c.r * c.a + out.r * (1 - c.a),
                g: c.g * c.a + out.g * (1 - c.a),
                b: c.b * c.a + out.b * (1 - c.a),
              };
      }
      return out || { r: 255, g: 255, b: 255 };
    };
    const tab =
      g.document.querySelector('.cast-tabs a[aria-current="page"]') ||
      g.document.querySelector(".cast-tabs .cast-tab-active");
    const pill = g.document.querySelector('[data-testid="cast-start-narrating"]');
    const body = g.document.body;
    if (!tab || !pill) {
      throw new Error(
        `the contrast gate needs the active tab and the primary pill (tab=${Boolean(tab)}, pill=${Boolean(pill)})`,
      );
    }
    const tabColor = parse(g.getComputedStyle(tab).color);
    const pillBg = parse(g.getComputedStyle(pill).backgroundColor);
    const pageBg = parse(g.getComputedStyle(body).backgroundColor);
    if (!tabColor || !pillBg || !pageBg) {
      throw new Error(
        `the contrast gate needs solid computed colours (tab=${Boolean(tabColor)}, pill=${Boolean(pillBg)}, page=${Boolean(pageBg)})`,
      );
    }
    const tabBgRgb = effectiveBg(tab);
    return {
      theme: g.document.documentElement.dataset.theme || "(default)",
      tabColor: g.getComputedStyle(tab).color,
      tabBg: `rgb(${tabBgRgb.r}, ${tabBgRgb.g}, ${tabBgRgb.b})`,
      tabTextBg: ratio(tabColor, tabBgRgb),
      pillBg: g.getComputedStyle(pill).backgroundColor,
      pageBg: g.getComputedStyle(body).backgroundColor,
      pillBgPage: ratio(pillBg, pageBg),
    };
  })) as ThemeContrasts;
  expect(result.theme, "the persisted theme applied on this load").toBe(expectTheme);
  expect(
    result.tabTextBg,
    `the active tab's label against its background in ${expectTheme} is ${result.tabTextBg.toFixed(
      2,
    )}:1 (label ${result.tabColor} on ${result.tabBg}); the review's gate is 3:1 or more — under 3 means the label sits in its own background (the Lilac Night "underline with no text" finding)`,
  ).toBeGreaterThanOrEqual(3);
  expect(
    result.pillBgPage,
    `the primary fill against the page ground in ${expectTheme} is ${result.pillBgPage.toFixed(
      2,
    )}:1 (fill ${result.pillBg} on ${result.pageBg}); the review's gate is 3:1 or more — at 1.0 the fill IS the ground (the "no visible fill" finding)`,
  ).toBeGreaterThanOrEqual(3);
}

/** The review's 390 viewport-fit gate with the picker open: the document
 * must not overflow the viewport, and neither may the full-screen sheet or
 * any of its interactive fields. The bounding-box checks are the
 * load-bearing ones: fixed-position elements do not stretch
 * document.scrollWidth (a content-box sheet at width:100% overflows the
 * viewport by its padding and border while scrollWidth still reads the
 * viewport width), so scroll width alone cannot catch the review's
 * clipped-corner finding. */
async function assertViewportFit(page: Page): Promise<void> {
  const result = await page.evaluate(() => {
    const g = globalThis as unknown as CastGlobal;
    const sheet = g.document.querySelector(".cast-detail");
    const controls = sheet
      ? Array.from(g.document.querySelectorAll("button, input, textarea, select, a"))
      : [];
    const rightEdges = controls
      .map((el) => el.getBoundingClientRect().right)
      .concat(sheet ? [sheet.getBoundingClientRect().right] : []);
    return {
      scrollWidth: g.document.documentElement.scrollWidth,
      innerWidth: g.innerWidth,
      sheetRight: sheet ? sheet.getBoundingClientRect().right : null,
      controlMaxRight: rightEdges.length ? Math.max(...rightEdges) : null,
    };
  });
  expect(
    result.scrollWidth,
    `the document overflows the ${result.innerWidth}px viewport by ${result.scrollWidth - result.innerWidth}px with the picker open`,
  ).toBeLessThanOrEqual(result.innerWidth);
  expect(
    result.sheetRight,
    `the picker sheet's right edge is at ${result.sheetRight}px, past the ${result.innerWidth}px viewport (the review's clipped close button)`,
  ).toBeLessThanOrEqual(result.innerWidth);
  expect(
    result.controlMaxRight,
    `the sheet's interactive fields reach ${result.controlMaxRight}px, past the ${result.innerWidth}px viewport (the review's clipped inputs and buttons)`,
  ).toBeLessThanOrEqual(result.innerWidth);
}

test("the cast redesign: order, filters, the shared count, the picker, the bulk", async ({
  page,
  request,
  baseURL,
}, testInfo) => {
  test.setTimeout(600_000);
  test.skip(
    testInfo.project.name !== "voices",
    "runs on the voices project's scripted-TTS backend",
  );
  const run = serverRun("voices");
  const origin = new URL(baseURL as string).origin;
  // 401: the unauthenticated /auth/me probe the guard makes on mount. 409: the
  // one-shot claim's conflict when a parallel walk claimed the appliance first
  // (ADR-0016 §7). Every other console error fails the check at the end.
  const consoleErrors = trackConsoleErrors(page, [401, 409]);

  // ── 1. sign in (claim the appliance on a standalone run) ───────────────
  const cookies = await signIn(page);
  await request.addCookies(cookies);
  const api = apiAsUser(request, origin);

  // ── 2. the game, the edition, the 67-row roster (the manifest import) ───
  const game = await api.post("/api/v1/games", {
    data: { slug: EDITION.slug, title: EDITION.title },
    headers: { "idempotency-key": randomUUID() },
  });
  expect(game.status(), `create game: ${await game.text()}`).toBe(201);
  const gameId = (await game.json()) as { id: string };

  const edition = await api.post(`/api/v1/games/${gameId.id}/editions`, {
    data: { region: EDITION.region, language: EDITION.language, revision: EDITION.revision },
    headers: { "idempotency-key": randomUUID() },
  });
  expect(edition.status(), `create edition: ${await edition.text()}`).toBe(201);
  const editionId = (await edition.json()) as { id: string };

  const { doc } = buildManifest(seed(), { edition: EDITION });
  const imported = await api.post(`/api/v1/editions/${editionId.id}/profiles/import-manifest`, {
    data: doc,
  });
  expect(imported.status(), `import-manifest: ${await imported.text()}`).toBe(201);
  const { profile } = (await imported.json()) as { profile: { id: string } };

  // ── 3. the cast screen: the roster in the work's order ──────────────────
  await page.goto(`/games/${gameId.id}/cast?profile=${encodeURIComponent(profile.id)}`);
  await expect(page.getByTestId("character-table")).toBeVisible({ timeout: 30_000 });

  // The roster's lines and the shared count both read the message fetch; let
  // it settle (Tessa's twelve lines) before the order and count assertions.
  await expect(page.getByTestId("character-lines-Tessa")).toHaveText("12", {
    timeout: 30_000,
  });

  // The compact row (ADR-0135): avatar · name · line count · state · voice
  // state · one action — no status badge of the row's own after the name
  // (the 2026-10-21 review's stray-ring finding: the faint pill that read as
  // a stray "(" after every row's name). The status a row showed there reads
  // off the name, the action and the chips instead.
  await expect(page.locator('[data-testid="cast-row-name-Tessa"] .badge')).toHaveCount(0);
  await expect(page.locator('[data-testid="cast-row-name-spk:1"] .badge')).toHaveCount(0);

  // ── 3b. the review's structural-contrast gate, both themes ────────────
  // The 2026-10-21 review's Lilac Night finding (the active tab's label
  // invisible on the ground, the primary fills unread) is exactly what
  // axe's color-contrast cannot flag — no text on a fill-on-fill control —
  // so the walk asserts the two relationships itself. The persisted-theme
  // seam applies the stored theme on the next full load.
  await assertThemeContrasts(page, "(default)");
  await page.evaluate(() => {
    globalThis.localStorage.setItem("gn-theme", "lilac-night");
  });
  await page.reload();
  await expect(page.getByTestId("character-table")).toBeVisible({ timeout: 30_000 });
  await expect(page.getByTestId("character-lines-Tessa")).toHaveText("12", {
    timeout: 30_000,
  });
  await assertThemeContrasts(page, "lilac-night");
  // Back to the default theme for the walk's remaining steps.
  await page.evaluate(() => {
    globalThis.localStorage.removeItem("gn-theme");
  });
  await page.reload();
  await expect(page.getByTestId("character-table")).toBeVisible({ timeout: 30_000 });
  await expect(page.getByTestId("character-lines-Tessa")).toHaveText("12", {
    timeout: 30_000,
  });

  // The named rows lead, line count descending: Tessa (12) first, the one
  // -line Ada and Bosun Kip last among them — and the retired rows are not
  // in the default view at all (the "All" chip hides them).
  await expect(await firstName(page)).toContain("Tessa");
  await expect(page.getByTestId("character-row-Retired%201")).toHaveCount(0);
  await expect(page.getByTestId("character-row-Retired%2012")).toHaveCount(0);

  // The filter chips: All hides the retired rows (55 live speakers), and
  // each chip carries its own count.
  await expect(page.getByTestId("cast-filter-all")).toContainText("All 55");
  await expect(page.getByTestId("cast-filter-needs")).toContainText("Needs a voice 55");
  await expect(page.getByTestId("cast-filter-voiced")).toContainText("Voiced 0");
  await expect(page.getByTestId("cast-filter-unnamed")).toContainText("Unnamed 45");
  await expect(page.getByTestId("cast-filter-retired")).toContainText("Retired 12");

  // The Retired chip is where the dead rows live.
  await page.getByTestId("cast-filter-retired").click();
  await expect(page.getByTestId("character-row-Retired%201")).toBeVisible();
  await expect(page.getByTestId("character-row-Retired%2012")).toBeVisible();
  await expect(page.getByTestId("character-row-Tessa")).toHaveCount(0);
  await page.getByTestId("cast-filter-all").click();

  // The name search: a case-insensitive substring of the row's display name.
  await page.getByTestId("cast-name-search").fill("tessa");
  await expect(page.getByTestId("character-row-Tessa")).toBeVisible();
  await expect(page.getByTestId("character-row-Maren")).toHaveCount(0);
  await page.getByTestId("cast-name-search").fill("");
  await expect(page.getByTestId("character-row-Maren")).toBeVisible();

  // ── 4. the shared voiced count: the header chip and the Library read ────
  await expect(page.getByTestId("cast-voiced-count")).toContainText("0 of 55 voiced");
  const library = (await (
    await request.get(`${origin}/api/v1/games/${gameId.id}/library`)
  ).json()) as { characters: number; voiced: number };
  expect(library.characters, "the Library read's denominator is the shared one").toBe(55);
  expect(library.voiced).toBe(0);

  // ── 5. the picker: the row's one action, a take that plays, the approval
  // Ada (one line, the named group's tail) is the row the manual walk takes:
  // with the dial off the take lands auditioning and the panel owns the
  // approval — the brief, the take, the consent, the button.
  await page.getByTestId("auto-cast-switch").click();
  await expect(page.getByTestId("auto-cast-switch")).not.toBeChecked();
  await page.getByTestId("cast-row-action-Ada").click();
  await expect(page).toHaveURL(/character=Ada$/);
  await expect(page.getByTestId("cast-detail-name")).toHaveText("Ada");

  const brief1 = "A calm harbor voice, unhurried, with a slight salt air in it.";
  await page.getByTestId("voice-brief").fill(brief1);
  const accepted = page.waitForResponse(
    (r) => r.request().method() === "POST" && r.url().includes("/api/v1/voices/auditions"),
  );
  await page.getByTestId("generate-auditions").click();
  const createResponse = (await (await accepted).json()) as { job_id: string; state: string };
  expect(createResponse.state).toBe("draft");
  await expect
    .poll(
      async () => {
        const body = (await (
          await request.get(`${origin}/api/v1/jobs/${createResponse.job_id}`)
        ).json()) as { state: string };
        if (body.state === "failed") throw new Error(`the audition job failed`);
        return body.state;
      },
      { message: "the audition job reached a terminal state", timeout: 120_000 },
    )
    .toBe("succeeded");
  await expect(page.getByTestId("voice-state-badge")).toHaveText("auditioning", {
    timeout: 30_000,
  });
  await expect(page.getByTestId("audition-card-0")).toBeVisible({ timeout: 30_000 });

  // The take plays: the audition clip's audio element holds the server's
  // blob (the scripted TTS's real synthetic bytes).
  await expect(page.getByTestId("audition-player-0")).toBeVisible();
  await expect(page.getByTestId("audition-player-0")).toHaveAttribute("src", /^blob:/);

  // The approval: the consent record is the profile's consent default,
  // prefilled (the auto-cast's own record, ADR-0109) — the required note is
  // gone, and the button is live once a take is selected.
  await expect(page.getByTestId("consent-input")).toHaveValue(CONSENT_DEFAULT);
  await expect(page.getByTestId("consent-required-note")).toHaveCount(0);
  await expect(page.getByTestId("approve-voice")).toBeDisabled();
  await page.getByTestId("audition-approve-0").click();
  await expect(page.getByTestId("audition-approve-0")).toHaveText("Selected");
  await expect(page.getByTestId("approve-voice")).toBeEnabled();
  await page.getByTestId("approve-voice").click();
  await expect(page.getByTestId("voice-state-badge")).toHaveText("approved", { timeout: 30_000 });
  await expect(page.getByTestId("character-voice-state-Ada")).toHaveText("voiced · v1", {
    timeout: 30_000,
  });

  // The take work's other escape hatch (ADR-0101): the panel's narrator dial
  // and the header's are the profile's one flag — the two stay in agreement.
  await expect(page.getByTestId("cast-narrator-toggle")).toBeChecked();
  await page.getByTestId("cast-narrator-toggle").click();
  await expect(page.getByTestId("cast-narrator-toggle")).not.toBeChecked();
  await expect(page.getByTestId("narrator-fallback-toggle")).not.toBeChecked();
  await page.getByTestId("narrator-fallback-toggle").click();
  await expect(page.getByTestId("cast-narrator-toggle")).toBeChecked();

  // The order moves with the state: Ada (named, voiced) now leads the
  // roster — the voiced group outranks the named needs-a-voice group
  // whatever its line counts.
  await expect(await firstName(page)).toContain("Ada");

  // ── 6. the bulk: voice everyone with auto-cast ───────────────────────────
  // The dial back on: the nine named, unvoiced characters each get a design
  // run with the page's default brief, enqueued one at a time, and the bar
  // reports n of m as the jobs land.
  await page.getByTestId("auto-cast-switch").click();
  await expect(page.getByTestId("auto-cast-switch")).toBeChecked();
  await expect(page.getByTestId("cast-bulk-voice-all")).toBeEnabled();
  await page.getByTestId("cast-bulk-voice-all").click();
  await expect(page.getByTestId("cast-bulk-progress")).toBeVisible({ timeout: 10_000 });
  await expect(page.getByTestId("cast-bulk-progress")).toContainText("of 9", {
    timeout: 180_000,
  });
  // Settled: the progress reads the work's outcome, and the shared count —
  // the chip and the server's own read — agree on ten of fifty-five.
  await expect(page.getByTestId("cast-bulk-progress")).toContainText("voiced 9 of 9", {
    timeout: 180_000,
  });
  await expect(page.getByTestId("cast-voiced-count")).toContainText("10 of 55 voiced", {
    timeout: 60_000,
  });
  const libraryAfter = (await (
    await request.get(`${origin}/api/v1/games/${gameId.id}/library`)
  ).json()) as { characters: number; voiced: number };
  expect(libraryAfter.voiced, "the server's read catches up with the chip").toBe(10);
  expect(libraryAfter.characters).toBe(55);
  await expect(page.getByTestId("cast-filter-voiced")).toContainText("Voiced 10");
  // The voiced group now leads by line count: Tessa (12) over Ada (1).
  await expect(await firstName(page)).toContain("Tessa");

  // ── 7. name the speakers: the picker's Name flow on a placeholder row ───
  // spk:2's lines, a name, and the speaker-link: a new confirmed character
  // row, the placeholder retired behind its chip, the selection following
  // the name the row now carries.
  await page.getByTestId("cast-filter-unnamed").click();
  await expect(page.getByTestId("character-row-spk%3A2")).toBeVisible();
  await page.getByTestId("cast-row-action-spk%3A2").click();
  await expect(page.getByTestId("cast-detail-name")).toHaveText("spk:2");
  await expect(page.getByTestId("speaker-name")).toBeVisible();
  await expect(page.getByTestId("speaker-name-input")).toBeVisible();
  await page.getByTestId("speaker-name-input").fill("The Lighthouse Keeper");
  await page.getByTestId("speaker-name-save").click();
  // The link lands: the selection follows the name the row now carries (the
  // panel moves onto the new row; the roster's change is the confirmation
  // itself — a new named row, the placeholder gone from the live chips).
  await expect(page).toHaveURL(/character=The(%20|\+)Lighthouse(%20|\+)Keeper$/);
  await expect(page.getByTestId("cast-detail-name")).toHaveText("The Lighthouse Keeper", {
    timeout: 30_000,
  });

  // The roster agrees: the new row is named and confirmed, the placeholder
  // is retired (out of the live chips, into the Retired one).
  await page.getByTestId("cast-filter-all").click();
  await expect(page.getByTestId("cast-row-name-The%20Lighthouse%20Keeper")).toBeVisible();
  await expect(page.getByTestId("character-row-spk%3A2")).toHaveCount(0);
  await expect(page.getByTestId("cast-filter-unnamed")).toContainText("Unnamed 44");
  await expect(page.getByTestId("cast-filter-retired")).toContainText("Retired 13");

  // ── 8. the review's 390 viewport-fit gate, the picker open ─────────────
  // At phone width the detail panel is the full-screen sheet; the review
  // found its corner clipped (the sheet's content-box width ran past the
  // viewport with the close button and the fields cut off). The picker is
  // open on the newly named character from step 7.
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(page.getByTestId("cast-detail-name")).toBeVisible({ timeout: 10_000 });
  await assertViewportFit(page);

  expect(consoleErrors).toEqual([]);
  expectDefaultDataDirUntouched(run);
});
