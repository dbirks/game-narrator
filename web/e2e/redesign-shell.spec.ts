import AxeBuilder from "@axe-core/playwright";
import { expect, test, type Page } from "@playwright/test";
import { readFileSync } from "node:fs";

import { trackConsoleErrors } from "./support/console";
import { expectDefaultDataDirUntouched, serverRun } from "./support/servers";

/**
 * Plum & Peach shell review (docs/design/plum-peach plan.md, Phase 0 item 6):
 * the global chrome and the component-kit gallery, on a fresh packaged-binary
 * appliance the spec claims as admin.
 *
 * For /library, /settings and /dev/kit the spec settles the page at the
 * design's three widths (1440 / 1024 / 390), screenshots it into
 * test-results, and runs axe (WCAG 2.1 A/AA) — any serious or critical
 * violation fails the walk. The shell's own invariants (28px ink bar, dither
 * fade, focus rings, the ⌘J pill and the ⌘K palette seam) are asserted once
 * on /library.
 *
 * Model boundary: none — no session, audition or preview runs, so no model
 * instance is needed (AGENTS.md rule 3).
 */

const ADMIN = { username: "redesign-admin", password: "steady as the tide" };
/** The design's three review widths (docs/design/plum-peach README, IA). */
const WIDTHS = [1440, 1024, 390] as const;

interface PageStop {
  name: string;
  path: string;
  /** a testid proving the page's content settled, not just the chrome. */
  settled: (page: Page) => Promise<void>;
}

const STOPS: PageStop[] = [
  {
    name: "library",
    path: "/library",
    settled: (page) =>
      expect(
        page.locator('[data-testid="game-list"], [data-testid="new-game"]').first(),
      ).toBeVisible({ timeout: 20_000 }),
  },
  {
    name: "settings",
    path: "/settings",
    settled: (page) => expect(page.getByTestId("settings-roles")).toBeVisible({ timeout: 20_000 }),
  },
  {
    name: "dev-kit",
    path: "/dev/kit",
    settled: (page) => expect(page.getByTestId("devkit-page")).toBeVisible({ timeout: 20_000 }),
  },
];

/** WCAG 2.1 A/AA: the 2.0 A/AA rules plus the rules 2.1 adds at those levels. */
const WCAG_21_A_AA = ["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"] as const;

/** Wait until the bar's live string is the server-rendered one — present and
 *  not the neutral "· status ·" placeholder (the placeholder means the feed
 *  never delivered a frame: anonymous 401 or an unreachable feed). The
 *  predicate runs in the page (the e2e tsconfig has no DOM lib, so the
 *  document is reached through a typed globalThis cast).
 */
async function liveTextSettled(page: Page): Promise<void> {
  await page.waitForFunction(
    (selector: string) => {
      const globalWithDocument = globalThis as {
        document?: { querySelector(sel: string): { textContent: string | null } | null };
      };
      const el = globalWithDocument.document?.querySelector(selector) ?? null;
      const text = (el?.textContent ?? "").trim();
      return text.length > 0 && !text.includes("status ·");
    },
    "[data-testid='status-live']",
    { timeout: 15_000 },
  );
}

interface ScanReport {
  url: string;
  width: number;
  serious: string[];
  violations: Record<string, number>;
}

test("Plum & Peach shell: chrome invariants, screenshots and axe at three widths", async ({
  page,
}, testInfo) => {
  test.setTimeout(600_000);
  test.skip(testInfo.project.name !== "redesign", "runs on the redesign project's fresh appliance");
  const run = serverRun("redesign");
  // 503 joins 401 on the ignore list: on this appliance the capture layer is
  // unwired (no UVC dongle, no test seam), and the Settings capture section
  // renders that 503 as its own "no capture layer" note — the machine's
  // answer, not a fault of the walk.
  const consoleErrors = trackConsoleErrors(page, [401, 503]);

  // ── claim the appliance and sign in, through the page ──────────────────
  await page.goto("/");
  await expect(page.getByTestId("setup-form")).toBeVisible({ timeout: 30_000 });
  await page.getByTestId("setup-username").fill(ADMIN.username);
  await page.getByTestId("setup-password").fill(ADMIN.password);
  await page.getByTestId("setup-submit").click();
  await expect(page).toHaveURL(/\/login$/, { timeout: 20_000 });
  await page.getByTestId("login-username").fill(ADMIN.username);
  await page.getByTestId("login-password").fill(ADMIN.password);
  await page.getByTestId("login-submit").click();
  await expect(page.getByTestId("nav-user")).toContainText(ADMIN.username);
  await expect(page.getByTestId("nav-role")).toHaveText("admin");

  // ── the shell's invariants, asserted once on /library ───────────────────
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto("/library");
  await STOPS[0].settled(page);
  const bar = page.getByTestId("status-bar");
  await expect(bar).toBeVisible();
  await expect(page.getByTestId("status-wordmark")).toHaveText("GAME·NARRATOR");
  await expect(page.getByTestId("status-fade")).toBeVisible();
  const live = page.getByTestId("status-live");
  await expect(live).toBeVisible();
  // The live string read the real status endpoint (not the offline fallback):
  // the claimed appliance answers the stream with its canonical server-
  // rendered string (the idle health rollup or a session string).
  await liveTextSettled(page);
  // the ⌘J pill with its route chip; the ⌘K seam opens the empty palette.
  const pill = page.getByRole("button", { name: /agent/ });
  await expect(pill).toBeVisible();
  await expect(pill.getByText("⌘J")).toBeVisible();
  await expect(pill.getByText("library", { exact: true })).toBeVisible();
  await page.keyboard.press("Meta+k");
  const palette = page.getByTestId("palette");
  await expect(palette).toBeVisible();
  await expect(palette.getByRole("heading", { name: "Search" })).toBeVisible();
  await expect(page.getByTestId("palette-input")).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(palette).toHaveCount(0);
  // the footer's build line settled from the system-info endpoint
  // (version is the git description the server stamps at build time).
  await expect(page.getByTestId("status-footer")).toContainText(/^Build: \S+/, {
    timeout: 15_000,
  });

  // ── screenshots + axe, at the three design widths ───────────────────────
  const reports: ScanReport[] = [];
  for (const stop of STOPS) {
    for (const width of WIDTHS) {
      await page.setViewportSize({ width, height: 900 });
      await page.goto(stop.path);
      await stop.settled(page);
      // settle the chrome too so the shot shows a real status string.
      await liveTextSettled(page);
      // Settle the fonts: the self-hosted faces swap in asynchronously
      // (font-display: swap) and the fallback metrics are wider — measuring
      // (or shooting) before the swap lands would read the fallback layout,
      // not the design's.
      await page.evaluate(async () => {
        const g = globalThis as unknown as {
          document?: { fonts?: { ready?: Promise<unknown> } };
        };
        await g.document?.fonts?.ready;
      });
      // Phone width (the design's third review width): no horizontal page
      // scroll — wide tables scroll inside their own wrappers and the status
      // bar wraps, so the document must never be wider than the viewport
      // (docs/design/plum-peach IA, Phase 0 review fix 3).
      if (width === 390) {
        const widths = await page.evaluate(() => {
          // The e2e tsconfig has no DOM lib, so the document and viewport
          // are reached through a typed globalThis cast (the spec's pattern).
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
      const shot = `${stop.name}-${width}.png`;
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
        violations: Object.fromEntries(
          Object.entries(byRule).sort(([a], [b]) => a.localeCompare(b)),
        ),
        serious: violations
          .filter((violation) => violation.impact === "serious" || violation.impact === "critical")
          .map(
            (violation) =>
              `${violation.id} [${violation.impact}] ×${violation.nodes.length} — ${violation.help}`,
          ),
      });
      if (process.env.A11Y_DEBUG) {
        for (const violation of violations) {
          for (const node of violation.nodes) {
            const details = node.any.map(
              (check) =>
                `${check.id}:${check.data !== undefined ? JSON.stringify(check.data) : ""}`,
            );
            console.log(
              `A11Y-DEBUG ${stop.name}@${width} ${violation.id}[${violation.impact}] ${node.target
                .map((part) => String(part))
                .join(" >> ")} :: ${details.join(", ")}`,
            );
          }
        }
        if (stop.name === "dev-kit" && width === 1440) {
          const html = await page.evaluate(() => {
            const globalWithDocument = globalThis as {
              document?: { querySelector(sel: string): { outerHTML: string } | null };
            };
            const el =
              globalWithDocument.document?.querySelector('[id^="radix-"][id$="-trigger-shelf"]') ??
              null;
            return el?.outerHTML ?? "not found";
          });
          console.log(`A11Y-DEBUG dev-kit trigger html: ${html}`);
        }
      }
    }
  }
  await testInfo.attach("axe-reports.json", {
    body: JSON.stringify(reports, null, 2),
    contentType: "application/json",
  });

  // any serious or critical violation on any width fails the walk.
  const failing = reports.filter((report) => report.serious.length > 0);
  expect(
    failing,
    `serious/critical axe violations on the redesigned shell:\n${failing
      .map((report) => `— ${report.url} @${report.width}px:\n${report.serious.join("\n")}`)
      .join("\n\n")}`,
  ).toEqual([]);

  // the walk must not have left a console error behind, and must not have
  // touched the developer's real data directory.
  expect(consoleErrors).toEqual([]);
  expectDefaultDataDirUntouched(run);
});
