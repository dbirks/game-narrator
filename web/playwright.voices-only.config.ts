// A memory-capped variant of playwright.config.ts that starts ONLY the three
// servers the `voices` project needs (the scripted TTS, the scripted ASR and
// the packaged binary). The full config starts every suite's webServer (~19 Go
// processes + the providers), which exceeds a 3 GB scope on a loaded host.
//
// Reuses the same port table (e2e/support/ports.ts) and run-root convention
// (GN_E2E_RUN_ROOT, ADR-0094) as the main config, so the spec's port/manifest
// lookups are unchanged. Run it with:
//   npx playwright test -c playwright.voices-only.config.ts \
//       --project=voices cast-voice-check.spec.ts
//
// The webServer env blocks mirror the `fakeTTS`, `fakeASR` and `voices`
// entries in playwright.config.ts exactly (the FAKE_ASR_BRIEF seed includes the
// three ADR-0171 briefs this spec plants).

import { join, resolve } from "node:path";

import { defineConfig, devices } from "@playwright/test";

import { ports } from "./e2e/support/ports";

const embeddedBinary = resolve(import.meta.dirname, "..", "bin", "game-narrator");
const voicesUrl = `http://127.0.0.1:${ports.voices}`;

process.env.GN_E2E_RUN_ROOT ??= `game-narrator-e2e-${process.pid}`;

export default defineConfig({
  testDir: "./e2e",
  fullyParallel: false,
  forbidOnly: false,
  workers: 1,
  retries: 0,
  outputDir: join("test-results", `run-${process.pid}`),
  reporter: [["list"]],
  globalSetup: "./e2e/global-setup.ts",
  globalTeardown: "./e2e/global-teardown.ts",
  use: {
    acceptDownloads: true,
  },
  projects: [
    {
      name: "voices",
      use: { ...devices["Desktop Chrome"], baseURL: voicesUrl },
      testMatch: /cast-voice-check\.spec\.ts|kind-voices\.spec\.ts/,
    },
  ],
  webServer: [
    {
      command: `node web/e2e/fake-tts.mjs ${ports.fakeTTS}`,
      cwd: "..",
      url: `http://127.0.0.1:${ports.fakeTTS}/healthz`,
      reuseExistingServer: false,
      timeout: 30_000,
      stdout: "ignore",
      stderr: "pipe",
    },
    {
      command: `node web/e2e/fake-asr.mjs ${ports.fakeASR}`,
      cwd: "..",
      url: `http://127.0.0.1:${ports.fakeASR}/healthz`,
      env: {
        FAKE_ASR_BRIEF:
          "A calm harbor voice, unhurried, with a slight salt air in it.;;A steadier take: slower, with the salt further out.;;A clear, warm narrator voice with a steady cadence, suited to original synthetic dialogue.;;An adult woman, warm and calm, with a steady mid-range voice.;;A bright young child, small and high-pitched.;;A mature adult man, a deep resonant voice, unmistakably male and low-pitched, plain and level.;;An adult woman, a mid-range voice, clearly female, low-key and unforced.;;An elderly man, a deep baritone, unmistakably male and low-pitched, unhurried and natural.;;An elderly woman, a mid-range voice with the weight of age, clearly female, clear and unhurried.;;A young boy, a small child's voice, high-pitched and bright, steady and warm.;;A young girl, a mid-range voice, clearly female and youthful, steady and warm.;;A non-human creature: an alien, inhuman timbre, neither a man's nor a woman's voice.",
        FAKE_ASR_FAIL_LINE: "3",
      },
      reuseExistingServer: false,
      timeout: 30_000,
      stdout: "ignore",
      stderr: "pipe",
    },
    {
      command: `node web/e2e/serve.mjs voices 127.0.0.1:${ports.voices} ${embeddedBinary} serve`,
      cwd: "..",
      url: `${voicesUrl}/api/v1/health/live`,
      env: {
        GAME_NARRATOR_GATEWAY_URL: `http://127.0.0.1:${ports.fakeTTS}/v1`,
        GAME_NARRATOR_GATEWAY_TOKEN: "fake-e2e-token",
        GAME_NARRATOR_ROLE_VOICE_DESIGN_MODEL: "fake-voicedesign",
        GAME_NARRATOR_ROLE_VOICE_CLONE_MODEL: "fake-base",
        GAME_NARRATOR_TTS_BASE_DIRECT_URL: `http://127.0.0.1:${ports.fakeTTS}/v1`,
        GAME_NARRATOR_ROLE_SPEECH_ASR_MODEL: "fake-asr",
        GAME_NARRATOR_ROLE_SPEECH_ASR_URL: `http://127.0.0.1:${ports.fakeASR}/v1`,
      },
      reuseExistingServer: false,
      timeout: 60_000,
      stdout: "ignore",
      stderr: "pipe",
    },
  ],
});
