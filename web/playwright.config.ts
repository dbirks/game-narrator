import { existsSync } from "node:fs";
import { resolve } from "node:path";

import { defineConfig, devices } from "@playwright/test";

import { ports } from "./e2e/support/ports";

// Real end-to-end runs (docs/spec/12 "UI" mode): the actual Go API process plus
// the Vite dev server proxying /api to it. No mocks.
//
// Every server here runs against a throwaway data directory, never the developer's
// own. `serve` defaults to the appliance location ~/.local/share/game-narrator
// (dataDir() in internal/bootstrap), so a webServer started without --data-dir
// writes tests into the user's real database. `e2e/serve.mjs` owns the temporary
// directory, records it for e2e/isolation.spec.ts, and removes it when the server
// exits; e2e/global-teardown.ts removes the run root and fails the run if the real
// data directory moved at all.
const apiUrl = `http://127.0.0.1:${ports.api}`;
const webUrl = `http://127.0.0.1:${ports.web}`;

// The `embedded` and `catalog` projects (docs/spec/09 "Embedded binary test")
// exercise the single binary produced by `make build`: the UI comes from the
// embedded webassets FS, so no Vite process is involved at all. Two separate
// servers, because the catalogue spec claims the appliance and creates data, and
// the embedded spec must stay a test of serving, not of ordering.
const embeddedUrl = `http://127.0.0.1:${ports.embedded}`;
const catalogUrl = `http://127.0.0.1:${ports.catalog}`;
const embeddedBinary = resolve(import.meta.dirname, "..", "bin", "game-narrator");

if (!existsSync(embeddedBinary)) {
  throw new Error(
    `playwright: ${embeddedBinary} not found — the embedded project tests the packaged binary, run \`make build\` first (or use \`make test-ui\`)`,
  );
}

// One root per run, shared by every server this config starts (the spawned shells
// inherit it), so one teardown removes all of it. This is set before webServer is
// read, and e2e/serve.mjs creates the directories inside it itself: Playwright
// starts the servers before it runs globalSetup, so a setup hook could not hand
// them a path in time.
process.env.GN_E2E_RUN_ROOT ??= `game-narrator-e2e-${process.pid}`;

export default defineConfig({
  testDir: "./e2e",
  fullyParallel: true,
  forbidOnly: Boolean(process.env.CI),
  retries: process.env.CI ? 1 : 0,
  outputDir: "test-results",
  reporter: process.env.CI ? [["list"], ["html", { open: "never" }]] : [["list"]],
  globalSetup: "./e2e/global-setup.ts",
  globalTeardown: "./e2e/global-teardown.ts",
  use: {
    baseURL: webUrl,
    trace: "on-first-retry",
  },
  projects: [
    {
      name: "chromium",
      use: { ...devices["Desktop Chrome"] },
      testIgnore: /embedded\.spec\.ts|catalog\.spec\.ts/,
    },
    {
      name: "embedded",
      // Only the embedded-binary spec: the dev-server project can never prove
      // the packaged UI, and this one can never prove the dev server.
      testMatch: /embedded\.spec\.ts/,
      use: { ...devices["Desktop Chrome"], baseURL: embeddedUrl },
    },
    {
      name: "catalog",
      // The catalogue walk-through, on the packaged binary: the same SPA, served
      // by the same process that answers its API calls, with a data directory
      // nothing else on the machine can reach.
      testMatch: /catalog\.spec\.ts/,
      use: { ...devices["Desktop Chrome"], baseURL: catalogUrl },
    },
  ],
  webServer: [
    {
      command: "node web/e2e/serve.mjs api 127.0.0.1:18090 go run ./cmd/game-narrator serve",
      cwd: "..",
      url: `${apiUrl}/api/v1/health/live`,
      // Never reuse: a server from an earlier run points at an earlier (already
      // deleted) data directory, and testing against it would prove nothing about
      // the binary under test.
      reuseExistingServer: false,
      timeout: 120_000,
      stdout: "ignore",
      stderr: "pipe",
    },
    {
      command: "pnpm dev",
      url: webUrl,
      env: { GAME_NARRATOR_API: apiUrl },
      reuseExistingServer: !process.env.CI,
      timeout: 120_000,
      stdout: "ignore",
      stderr: "pipe",
    },
    {
      command: `node web/e2e/serve.mjs embedded 127.0.0.1:18095 ${embeddedBinary} serve`,
      cwd: "..",
      url: `${embeddedUrl}/api/v1/health/live`,
      reuseExistingServer: false,
      timeout: 60_000,
      stdout: "ignore",
      stderr: "pipe",
    },
    {
      command: `node web/e2e/serve.mjs catalog 127.0.0.1:18096 ${embeddedBinary} serve`,
      cwd: "..",
      url: `${catalogUrl}/api/v1/health/live`,
      reuseExistingServer: false,
      timeout: 60_000,
      stdout: "ignore",
      stderr: "pipe",
    },
  ],
});
