import { existsSync } from "node:fs";
import { resolve } from "node:path";

import { defineConfig, devices } from "@playwright/test";

// Real end-to-end run (docs/spec/12 "UI" mode): the actual Go API process plus
// the Vite dev server proxying /api to it. No mocks.
const apiUrl = "http://127.0.0.1:18090";
const webUrl = "http://127.0.0.1:5173";

// The second project (docs/spec/09 "Embedded binary test") exercises the
// single binary produced by `make build`: the UI comes from the embedded
// webassets FS, so no Vite process is involved at all.
const embeddedUrl = "http://127.0.0.1:18095";
const embeddedBinary = resolve(import.meta.dirname, "..", "bin", "game-narrator");

if (!existsSync(embeddedBinary)) {
  throw new Error(
    `playwright: ${embeddedBinary} not found — the embedded project tests the packaged binary, run \`make build\` first (or use \`make test-ui\`)`,
  );
}

export default defineConfig({
  testDir: "./e2e",
  fullyParallel: true,
  forbidOnly: Boolean(process.env.CI),
  retries: process.env.CI ? 1 : 0,
  outputDir: "test-results",
  reporter: process.env.CI ? [["list"], ["html", { open: "never" }]] : [["list"]],
  use: {
    baseURL: webUrl,
    trace: "on-first-retry",
  },
  projects: [
    {
      name: "chromium",
      use: { ...devices["Desktop Chrome"] },
      testIgnore: /embedded\.spec\.ts/,
    },
    {
      name: "embedded",
      // Only the embedded-binary spec: the dev-server project can never prove
      // the packaged UI, and this one can never prove the dev server.
      testMatch: /embedded\.spec\.ts/,
      use: { ...devices["Desktop Chrome"], baseURL: embeddedUrl },
    },
  ],
  webServer: [
    {
      command: "go run ./cmd/game-narrator serve --listen 127.0.0.1:18090",
      cwd: "..",
      url: `${apiUrl}/api/v1/health/live`,
      reuseExistingServer: !process.env.CI,
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
      command: `${embeddedBinary} serve --listen 127.0.0.1:18095`,
      cwd: "..",
      url: `${embeddedUrl}/api/v1/health/live`,
      reuseExistingServer: false,
      timeout: 30_000,
      stdout: "ignore",
      stderr: "pipe",
    },
  ],
});
