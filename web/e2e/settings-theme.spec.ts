import AxeBuilder from "@axe-core/playwright";
import { readFileSync } from "node:fs";
import { expect, test, type Page } from "@playwright/test";

import { ports } from "./support/ports";
import { acquireScenarioLock } from "./support/scenario-lock";

/**
 * The settings-and-theme walk (plan.md Phase 1 item 9, ADR-0109): the theme
 * picker (UX-09), the audio output selector's sink categories with the test
 * tone and the unpaired-Bluetooth pairing prompt (UX-12), and the tailnet
 * URL (UX-13) — each asserted on its own packaged-binary server so the walk
 * owns its appliance claim.
 *
 * Model boundary: class (a) synthetic stubs (AGENTS.md rule 3) — the audio
 * device list and the test tone run against the labelled-synthetic seam
 * (GAME_NARRATOR_AUDIO_FAKE_DEVICES, the filesink records the tone's WAV as
 * the evidence), the Bluetooth inventory against the labelled-synthetic
 * controller (GAME_NARRATOR_BT_FAKE), and the tailnet URL against the
 * operator's pin (GAME_NARRATOR_TAILNET_URL). The second server runs the
 * same seams WITHOUT the pin and its PATH scrubbed of the tailscale CLI
 * (GN_E2E_NO_TAILSCALE), so the not-configured state is asserted on any
 * host. No real hardware and no model provider are involved.
 */

const ADMIN = { username: "settings-theme-admin", password: "steady as the tide" };
const NOT_CFG_URL = `http://127.0.0.1:${ports.themeNotCfg}`;
/** The design's three review widths (docs/design/plum-peach README, IA). */
const WIDTHS = [1440, 1024, 390] as const;
/** WCAG 2.1 A/AA (the a11y suite's tag set). */
const WCAG_21_A_AA = ["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"] as const;

/** The synthetic audio inventory (the project's GAME_NARRATOR_AUDIO_FAKE_DEVICES). */
const HDMI_ID = "hdmi-out";
/** The walk to the siblings' completion barrier (ADR-0130, the 2026-10-21
 *  full-suite gate): the same scenario-lock primitive the setup-board and
 *  cast walks use — the siblings sign in on the walk's appliance.
 */
const SETTINGS_THEME_LOCK = "settings-theme-walk";
/** The walk's test timeout (600 s) plus a scheduling margin: a walk that
 *  legitimately runs its full length never trips the barrier's wait.
 */
const SETTINGS_THEME_BARRIER_MS = 630_000;
const USB_ID = "jbl-usb-dac";
const HEADSET_SINK_ID = "bluez_output.AA:BB:CC:DD:EE:FF.1";
/** The synthetic Bluetooth inventory (internal/bt's DefaultFake). */
const PAIRABLE_MAC = "11:22:33:44:55:66";
const PAIRABLE_NAME = "Synthetic Pairable Buds";

/** Claim the fresh appliance through the first-run form (the account is
 *  admin); the claim lands on the sign-in page, so the walk signs in. */
async function claim(page: Page, base?: string): Promise<void> {
  await page.goto(base ?? "/", { waitUntil: "domcontentloaded" });
  await expect(page).toHaveURL(/\/setup$/, { timeout: 30_000 });
  await expect(page.getByTestId("setup-form")).toBeVisible({ timeout: 20_000 });
  await page.getByTestId("setup-username").fill(ADMIN.username);
  await page.getByTestId("setup-password").fill(ADMIN.password);
  await page.getByTestId("setup-submit").click();
  await expect(page).toHaveURL(/\/login$/, { timeout: 30_000 });
  await signIn(page, base);
}

/** Sign in on an already-claimed appliance (base: the appliance's own URL —
 *  a relative goto would land on this project's baseURL appliance, and its
 *  session cookie does not authenticate the other server). */
async function signIn(page: Page, base?: string): Promise<void> {
  await page.goto(base ? `${base}/login` : "/login");
  await expect(page.getByTestId("login-username")).toBeVisible({ timeout: 20_000 });
  await page.getByTestId("login-username").fill(ADMIN.username);
  await page.getByTestId("login-password").fill(ADMIN.password);
  await page.getByTestId("login-submit").click();
  await expect(page.getByTestId("nav-role")).toHaveText("admin", { timeout: 20_000 });
}

/** Wait until the settings page's content settled (not just the chrome). */
async function settingsSettled(page: Page): Promise<void> {
  await expect(page.getByTestId("settings-roles")).toBeVisible({ timeout: 20_000 });
}

/** One token of the applied theme on :root (the e2e tsconfig has no DOM lib,
 *  so the document is reached through a typed globalThis cast). */
async function rootToken(page: Page, slot: string): Promise<string> {
  return page.evaluate((name: string) => {
    const g = globalThis as {
      document?: { documentElement?: unknown };
      getComputedStyle?: (el: unknown) => { getPropertyValue: (n: string) => string };
    };
    const el = g.document?.documentElement;
    if (!el || !g.getComputedStyle) return "";
    return g.getComputedStyle(el).getPropertyValue(name).trim();
  }, `--${slot}`);
}

/** Settle the fonts (font-display: swap) so a shot reads the design's layout. */
async function fontsSettled(page: Page): Promise<void> {
  await page.evaluate(async () => {
    const g = globalThis as { document?: { fonts?: { ready?: Promise<unknown> } } };
    await g.document?.fonts?.ready;
  });
}

test("UX-13: the tailnet URL is the API's own answer, and the theme card links the picker", async ({
  page,
}, testInfo) => {
  test.setTimeout(600_000);

  // The run lock (ADR-0130, the 2026-10-21 full-suite gate): the sibling tests
  // sign in on this walk’s appliance, and fullyParallel lets one of them reach the
  // sign-in before the claim lands (its 2026-10-21 flake: /login rendered the
  // first-run setup form and the login fields never appeared). Held for the whole
  // walk (released in the finally); the siblings take it as a completion barrier
  // before they sign in.
  const releaseWalkLock = await acquireScenarioLock(testInfo, SETTINGS_THEME_LOCK);
  try {
    await claim(page);

    await page.goto("/settings");
    await settingsSettled(page);

    // UX-13: the displayed URL equals the running system's own answer — read
    // the API and assert text equality (the pin's value, verbatim).
    const api = await page.request.get("/api/v1/settings");
    expect(api.status(), "GET /api/v1/settings answers 200").toBe(200);
    const body = (await api.json()) as {
      network: { tailnet_url: string | null; source: string | null; note: string };
    };
    expect(body.network.tailnet_url, "the API reports the pinned tailnet URL").not.toBeNull();
    await expect(page.getByTestId("tailnet-url")).toHaveText(body.network.tailnet_url as string);
    await expect(page.getByTestId("tailnet-note")).toContainText(body.network.note);
    expect(body.network.note.length, "the note is rendered, never a blank field").toBeGreaterThan(
      0,
    );

    // The theme card: the current theme named and the link to the picker.
    await expect(page.getByTestId("settings-theme-current")).toHaveText("Plum & Peach");
    await expect(page.getByTestId("theme-page-link")).toHaveAttribute("href", "/settings/theme");

    // UX-09: the picker re-themes the whole app without a reload and persists
    // the choice across a reload.
    const plumInk = await rootToken(page, "ink");
    const plumPaper = await rootToken(page, "paper");
    expect(plumInk, "the plum theme has an --ink on :root").not.toBe("");
    expect(plumInk).not.toBe(plumPaper);

    await page.goto("/settings/theme");
    await expect(page.getByTestId("theme-grid")).toBeVisible({ timeout: 20_000 });
    await expect(page.getByTestId("theme-selected-plum")).toHaveText("selected");

    await page.getByTestId("theme-pick-lilac-night").click();
    // Live: <html> carries the theme attribute and :root's tokens switched —
    // no reload (the whole app re-themed at once).
    await expect.poll(async () => rootToken(page, "ink")).not.toBe(plumInk);
    await expect.poll(async () => rootToken(page, "paper")).not.toBe(plumPaper);
    await expect(page.getByTestId("theme-selected-lilac-night")).toBeVisible();
    await expect(page.getByTestId("theme-pick-lilac-night")).toBeDisabled();

    // Persistence: the choice survives a reload (localStorage seam, Phase 0).
    await page.reload();
    await expect(page.getByTestId("theme-grid")).toBeVisible({ timeout: 20_000 });
    await expect(page.getByTestId("theme-selected-lilac-night")).toBeVisible();
    expect(
      await rootToken(page, "ink"),
      "the reloaded page still carries Lilac Night's --ink",
    ).not.toBe(plumInk);

    // The token slot table: every theme's eight slots, the handoff's derived
    // slots flagged (README "Themes": the coral/sand/hair of Ink Blue & Apricot
    // and the muted/sand/hair of Sherbet were not in the mocks).
    await expect(page.getByTestId("theme-slot-table")).toBeVisible();
    for (const derived of [
      "theme-slot-derived-ink-apricot-coral",
      "theme-slot-derived-ink-apricot-sand",
      "theme-slot-derived-ink-apricot-hair",
      "theme-slot-derived-sherbet-muted",
      "theme-slot-derived-sherbet-sand",
      "theme-slot-derived-sherbet-hair",
    ]) {
      await expect(page.getByTestId(derived), `${derived} is flagged derived`).toBeVisible();
    }
    const values = await page.locator("[data-testid^='theme-slot-'] code").allTextContents();
    expect(
      values.every((value) => value.startsWith("#")),
      `the slot table read its values from the computed style (got ${values.slice(0, 3).join(", ")}…)`.slice(
        0,
        80,
      ),
    ).toBe(true);

    // The slot table is a REAL grid — four theme rows of eight slot columns,
    // the cells of a row side by side, not stacked: the a11y/visual review
    // caught the display:grid-on-the-td regression that collapsed the body
    // into one ~32-row column. The row boxes stay single-row height.
    const grid = await page.evaluate(() => {
      interface SlotEl {
        getBoundingClientRect: () => { x: number; height: number };
        querySelectorAll: (selector: string) => ArrayLike<SlotEl>;
      }
      const g = globalThis as {
        document?: {
          querySelector: (selector: string) => {
            querySelectorAll: (selector: string) => ArrayLike<SlotEl>;
          } | null;
        };
      };
      const table = g.document?.querySelector("[data-testid='theme-slot-table']");
      if (!table) return null;
      const rows = table.querySelectorAll("tbody tr");
      const rowCount = rows.length;
      const cellXs: number[] = [];
      const firstRowCells = rowCount > 0 ? rows[0].querySelectorAll("td") : null;
      if (firstRowCells) {
        for (let i = 0; i < firstRowCells.length; i++) {
          cellXs.push(firstRowCells[i].getBoundingClientRect().x);
        }
      }
      let maxRowHeight = 0;
      for (let i = 0; i < rowCount; i++) {
        maxRowHeight = Math.max(maxRowHeight, rows[i].getBoundingClientRect().height);
      }
      return {
        rowCount,
        cellCount: cellXs.length,
        sideBySide: cellXs.every((x, i) => i === 0 || x > cellXs[i - 1]),
        maxRowHeight,
      };
    });
    expect(grid, "the slot table rendered its theme rows").not.toBeNull();
    expect(grid?.rowCount, "one row per theme").toBe(4);
    expect(grid?.cellCount, "one column per slot").toBe(8);
    expect(grid?.sideBySide, "the row's cells sit side by side, not stacked").toBe(true);
    expect(grid?.maxRowHeight, "each theme row is one row, not a stack of cells").toBeLessThan(90);
  } finally {
    releaseWalkLock();
  }
});

test("UX-12: the selector lists the sink categories, persists the HDMI pick, the tone records its WAV, and the unpaired Bluetooth device shows the pairing prompt", async ({
  page,
}, testInfo) => {
  // The barrier's wait (the walk's full test length) plus this test's own
  // work (ADR-0130): the walk's claim must land before this test signs in.
  test.setTimeout(SETTINGS_THEME_BARRIER_MS + 150_000);
  // Stage one: the walk's run lock — held from its start to its end, so
  // acquiring it proves the claim (and the walk's settings state) landed.
  // The release is immediate: a barrier, not a claim (this test is
  // read-only over the walk's appliance).
  const release = await acquireScenarioLock(
    testInfo,
    SETTINGS_THEME_LOCK,
    SETTINGS_THEME_BARRIER_MS,
  );
  release();
  await signIn(page);
  await page.goto("/settings");
  await settingsSettled(page);

  // The selector's shape is the appliance's sink categories (UX-12): one
  // optgroup each, the fake inventory's HDMI/USB/Bluetooth rows inside.
  // An optgroup renders no box of its own (Playwright reports it hidden), so
  // the shape is asserted on the DOM: the group exists under the right label.
  const select = page.getByTestId("audio-device-select");
  for (const category of ["hdmi", "usb", "bluetooth", "other"]) {
    const group = select.locator(`optgroup[data-testid="audio-device-category-${category}"]`);
    await expect(group, `the ${category} category is the selector's shape`).toHaveCount(1);
  }
  await expect(select.locator(`option[value="${HDMI_ID}"]`)).toHaveCount(1);
  await expect(select.locator(`option[value="${USB_ID}"]`)).toHaveCount(1);
  await expect(select.locator(`option[value="${HEADSET_SINK_ID}"]`)).toHaveCount(1);

  // Selecting the HDMI sink persists it, and the API reads it back.
  await select.selectOption(HDMI_ID);
  await expect(page.getByTestId("audio-output-note")).toContainText("effective: device hdmi-out");
  const devices = (await (await page.request.get("/api/v1/audio/devices")).json()) as {
    selected: { device: string; device_source: string };
  };
  expect(devices.selected.device, "the API reports the persisted selection").toBe(HDMI_ID);
  expect(devices.selected.device_source).toBe("store");
  await expect(page.getByTestId(`audio-device-${HDMI_ID}`)).toContainText("selected");

  // The test tone request fires; the filesink's recorded WAV is the evidence
  // (a machine with no audible output still gets proof).
  await page.getByTestId("audio-test-tone").click();
  await expect(page.getByTestId("audio-tone-result")).toContainText("tone played through filesink");
  await expect(page.getByTestId("audio-tone-result")).toContainText("evidence:");
  await expect(page.getByTestId("audio-tone-error")).toHaveCount(0);

  // The unpaired Bluetooth device (ADR-0091's inventory) appears in the
  // Bluetooth category as a "pair first" option; selecting it renders the
  // pairing prompt instead of persisting an id that has no sink. The fake
  // seeds it discoverable-only (the way a real put-in-pairing-mode device
  // would), so the walk scans first — the shared ["bluetooth-devices"] query
  // then feeds both halves of the audio surface.
  await page.getByTestId("bt-scan").click();
  await expect(
    page.getByTestId(`bt-device-state-${PAIRABLE_MAC}`),
    "the scan surfaced the unpaired pairable device",
  ).toBeVisible({ timeout: 30_000 });
  const pairFirst = select.locator(`option[value="pair-first:${PAIRABLE_MAC}"]`);
  await expect(pairFirst).toHaveText(`${PAIRABLE_NAME} — pair first`);
  await select.selectOption(`pair-first:${PAIRABLE_MAC}`);
  await expect(page.getByTestId("pairing-prompt")).toBeVisible();
  await expect(page.getByTestId("pairing-prompt-name")).toHaveText(PAIRABLE_NAME);
  await expect(page.getByTestId("pairing-prompt")).toContainText("no A2DP sink");

  // The Pair action drives the existing pair action (ADR-0091): the note
  // lands and the Bluetooth inventory reports the device paired.
  await page.getByTestId("pairing-prompt-pair").click();
  await expect(page.getByTestId("pairing-prompt-note")).toContainText("paired", {
    timeout: 20_000,
  });
  await expect(page.getByTestId(`bt-device-state-${PAIRABLE_MAC}`)).toContainText("paired");
  await expect(page.getByTestId("pairing-prompt-error")).toHaveCount(0);

  // The pairing did not touch the persisted selection (the prompt, not a
  // persist, is what selecting the pair-first option does).
  const after = (await (await page.request.get("/api/v1/audio/devices")).json()) as {
    selected: { device: string };
  };
  expect(after.selected.device, "the selection still names the HDMI sink").toBe(HDMI_ID);
});

test("UX-13: the un-pinned server renders the explicit not-configured tailnet state", async ({
  page,
}) => {
  test.setTimeout(600_000);
  await claim(page, NOT_CFG_URL);

  await page.goto(`${NOT_CFG_URL}/settings`);
  await settingsSettled(page);

  // No pin, no tailscale on the child's PATH (GN_E2E_NO_TAILSCALE): the API
  // answers a null URL with the machine's reason, and the page renders the
  // note verbatim — an explicit not-configured state, never a blank field.
  const api = await page.request.get(`${NOT_CFG_URL}/api/v1/settings`);
  expect(api.status()).toBe(200);
  const body = (await api.json()) as { network: { tailnet_url: string | null; note: string } };
  expect(body.network.tailnet_url, "no invented URL").toBeNull();
  const note = body.network.note;
  expect(note).toContain("Tailscale is not configured");
  expect(note).toContain("GAME_NARRATOR_TAILNET_URL");
  await expect(page.getByTestId("tailnet-url")).toHaveText("not configured");
  await expect(page.getByTestId("tailnet-note")).toHaveText(note);
});

test("Settings and theme page: screenshots and axe at 1440/1024/390, no horizontal scroll at 390", async ({
  page,
}, testInfo) => {
  // The barrier's wait (the walk's full test length) plus this test's own
  // work (ADR-0130): the walk's claim must land before this test signs in.
  test.setTimeout(SETTINGS_THEME_BARRIER_MS + 150_000);
  // Stage one: the walk's run lock — held from its start to its end, so
  // acquiring it proves the walk (and its appliance claim) finished. The
  // release is immediate: a barrier, not a claim (this test is read-only
  // over the walk's state).
  const release = await acquireScenarioLock(
    testInfo,
    SETTINGS_THEME_LOCK,
    SETTINGS_THEME_BARRIER_MS,
  );
  release();
  await signIn(page);

  // The default theme for the settings shots; the dark theme for the theme
  // page shots (the picker's own state after the walk above).
  await page.goto("/settings/theme");
  await expect(page.getByTestId("theme-grid")).toBeVisible({ timeout: 20_000 });
  const plumPick = page.getByTestId("theme-pick-plum");
  if (await plumPick.isEnabled()) {
    await plumPick.click();
  }

  interface ScanReport {
    url: string;
    width: number;
    serious: string[];
    violations: Record<string, number>;
    sampleNodes: string[];
  }
  const reports: ScanReport[] = [];
  const stops = [
    { name: "settings", path: "/settings" },
    { name: "theme", path: "/settings/theme" },
  ];
  for (const stop of stops) {
    for (const width of WIDTHS) {
      await page.setViewportSize({ width, height: 900 });
      await page.goto(stop.path);
      if (stop.name === "settings") {
        await settingsSettled(page);
      } else {
        await expect(page.getByTestId("theme-grid")).toBeVisible({ timeout: 20_000 });
        if (width === 1440) {
          // The dark theme at the desktop width: the picker's selection is
          // the evidence the theme re-themed the whole app.
          const pick = page.getByTestId("theme-pick-lilac-night");
          if (await pick.isEnabled()) {
            await pick.click();
          }
        }
      }
      await fontsSettled(page);
      if (width === 390) {
        const widths = await page.evaluate(() => {
          // The e2e tsconfig has no DOM lib: the document and viewport are
          // reached through a typed globalThis cast (the suite's pattern).
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
          `${stop.path} @390px scrolls horizontally: documentElement.scrollWidth ${widths.scrollWidth} > innerWidth ${widths.innerWidth}`,
        ).toBeLessThanOrEqual(widths.innerWidth);
      }
      const shot = `p1-settings-${stop.name}-${width}.png`;
      const shotPath = `${testInfo.outputDir}/${shot}`;
      await page.screenshot({ path: shotPath, fullPage: true });
      await testInfo.attach(shot, { body: readFileSync(shotPath), contentType: "image/png" });

      const results = await new AxeBuilder({ page }).withTags([...WCAG_21_A_AA]).analyze();
      const violations = results.violations ?? [];
      const byRule: Record<string, number> = {};
      for (const violation of violations) {
        byRule[violation.id] = (byRule[violation.id] ?? 0) + violation.nodes.length;
      }
      const seriousViolations = violations.filter(
        (violation) => violation.impact === "serious" || violation.impact === "critical",
      );
      reports.push({
        url: stop.path,
        width,
        violations: Object.fromEntries(
          Object.entries(byRule).sort(([a], [b]) => a.localeCompare(b)),
        ),
        serious: seriousViolations.map(
          (violation) =>
            `${violation.id} [${violation.impact}] ×${violation.nodes.length} — ${violation.help}`,
        ),
        sampleNodes: seriousViolations
          .flatMap((violation) => violation.nodes.slice(0, 4).map((node) => node.target.join(" ")))
          .slice(0, 6),
      });
    }
    if (stop.name === "theme") {
      // Back to the default theme before the settings shots: the settings
      // evidence is the default Plum & Peach look (the dark theme is the
      // theme page's evidence, above).
      const back = page.getByTestId("theme-pick-plum");
      if (await back.isEnabled()) {
        await back.click();
      }
    }
  }

  const failing = reports.filter((report) => report.serious.length > 0);
  const nodeDump = failing
    .map((report) => `${report.url} @${report.width}:\n${report.sampleNodes.join("\n")}`)
    .join("\n");
  expect(
    failing,
    `axe serious/critical violations on the settings-and-theme walk:\n${failing
      .map((report) => `${report.url} @${report.width}: ${report.serious.join("; ")}`)
      .join("\n")}\n\nviolating nodes:\n${nodeDump}`,
  ).toEqual([]);
});
