import { defineConfig, devices } from "@playwright/test";

/**
 * `live-play.playwright.config.ts` — the runner config for the live play path
 * (task browser-live-play), driven by the Go orchestrator
 * (internal/e2e TestLivePlayUI, the `make test-dolphin` gate's UI phase).
 *
 * Why a separate config: the `live-play` project of playwright.config.ts has
 * no webServer of its own, on purpose. Its server is the Go test's
 * in-process API + embedded SPA — the only process that can wire the dev
 * Dolphin bridge into the runtime (the production binary never does;
 * internal/api/dolphin.go answers 503 for an unwired bridge, exactly like
 * every other unwired subsystem), and TestLiveDolphin proves that wiring is
 * the product's dev-harness shape. A Playwright run always starts every
 * webServer entry of its config, so running the live-play spec through the
 * main config would boot the suite's other ~19 servers (ports 18090–18111),
 * which a concurrent gate on this dev box may already hold. This config
 * starts nothing: the Go test owns the server's port and passes it through
 * GN_LIVE_UI_URL, with the login credentials and the evidence bundle dir as
 * GN_LIVE_UI_ADMIN / GN_LIVE_UI_PASS / GN_LIVE_UI_BUNDLE.
 *
 * The main config still lists the project so `make test-ui` and CI see the
 * spec as NOT RUN (it self-skips without the env) rather than absent.
 *
 * Run (from web/, by the Go test — not by hand):
 *
 *   GN_LIVE_UI_URL=… GN_LIVE_UI_ADMIN=… GN_LIVE_UI_PASS=… \
 *     GN_LIVE_UI_BUNDLE=… pnpm exec playwright test \
 *     --config e2e/live-play.playwright.config.ts
 */
const liveUrl = process.env.GN_LIVE_UI_URL ?? "";

export default defineConfig({
  testDir: ".",
  testMatch: /live-play\.spec\.ts/,
  workers: 1,
  retries: 0,
  reporter: [["list"]],
  // The whole walk: session start, the screen-driven emulator drive to the
  // first narrated text (minutes), the speech pipeline and the inspector.
  timeout: 45 * 60 * 1000,
  expect: { timeout: 15_000 },
  // Under the suite's gitignored output root (web/test-results/, both
  // .gitignore files), namespaced so the live run's artifacts never mix with
  // the other projects' — the Go orchestrator copies this dir into the run's
  // evidence bundle (internal/e2e copyPlaywrightTrace) and clears it first.
  outputDir: "../test-results/live-play",
  use: {
    ...devices["Desktop Chrome"],
    baseURL: liveUrl || undefined,
    // Always capture: the trace is the evidence artifact (AGENTS.md rule 9),
    // copied into the run's bundle by the Go orchestrator.
    trace: "on",
    acceptDownloads: true,
  },
  projects: [
    {
      name: "live-play",
      use: {
        ...devices["Desktop Chrome"],
        baseURL: liveUrl || undefined,
        trace: "on",
      },
    },
  ],
});
