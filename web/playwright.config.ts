import { defineConfig, devices } from "@playwright/test";

// Real end-to-end run (docs/spec/12 "UI" mode): the actual Go API process plus
// the Vite dev server proxying /api to it. No mocks.
const apiUrl = "http://127.0.0.1:18090";
const webUrl = "http://127.0.0.1:5173";

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
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
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
  ],
});
