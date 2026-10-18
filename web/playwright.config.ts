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
const developerUrl = `http://127.0.0.1:${ports.developer}`;
const onboardingUrl = `http://127.0.0.1:${ports.onboarding}`;
const voicesUrl = `http://127.0.0.1:${ports.voices}`;
const playUrl = `http://127.0.0.1:${ports.play}`;
const graphRegionsUrl = `http://127.0.0.1:${ports.graphRegions}`;
const correctionsUrl = `http://127.0.0.1:${ports.corrections}`;
const sseUrl = `http://127.0.0.1:${ports.sse}`;
const curationUrl = `http://127.0.0.1:${ports.curation}`;
const routesUrl = `http://127.0.0.1:${ports.routes}`;
const embeddedBinary = resolve(import.meta.dirname, "..", "bin", "game-narrator");
// The replay allowlist for the inspector's server: the repo's testdata, named
// explicitly (ADR-0024 "replay allowlist") rather than relying on the
// dev-checkout cwd default, so the synthetic clip is admitted wherever the
// checkout lives. This config lives in web/, so the repo root is one ".." up.
const replayRoots = resolve(import.meta.dirname, "..", "testdata");

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
  // Four projects each spin up a Go server and a browser; the developer
  // project additionally runs real Tesseract OCR, which is CPU-bound. When it
  // shares the box with the catalog spec the two starve each other
  // nondeterministically: the replay clip drops its second commit, or the
  // catalog export download times out. One worker runs the suite serially so
  // the CPU-bound tests get the whole box; the light specs cost little.
  workers: 1,
  retries: process.env.CI ? 1 : 0,
  outputDir: "test-results",
  reporter: process.env.CI ? [["list"], ["html", { open: "never" }]] : [["list"]],
  globalSetup: "./e2e/global-setup.ts",
  globalTeardown: "./e2e/global-teardown.ts",
  use: {
    baseURL: webUrl,
    trace: "on-first-retry",
    // Playwright does not capture browser downloads unless the context
    // accepts them (the default is `false`). The catalog export step and any
    // future file download need the `download` event to fire.
    acceptDownloads: true,
  },
  projects: [
    {
      name: "chromium",
      use: { ...devices["Desktop Chrome"] },
      testIgnore:
        /embedded\.spec\.ts|catalog\.spec\.ts|onboarding\.spec\.ts|voices\.spec\.ts|m5-part1\.spec\.ts|m5-graph-regions\.spec\.ts|corrections-fixtures\.spec\.ts/,
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
    {
      name: "developer",
      // The milestone 2 inspector: a replay session on the packaged binary with
      // the synthetic clip admitted to the replay allowlist (GAME_NARRATOR_REPLAY_ROOTS).
      testMatch: /developer\.spec\.ts/,
      use: { ...devices["Desktop Chrome"], baseURL: developerUrl },
    },
    {
      name: "onboarding",
      // The onboarding agent (ADR-0028): the packaged binary with the
      // agent_reasoning role pointed at the scripted model gateway, and the
      // dev private-endpoint allowlist admitting the gateway's fixture page
      // (test-only; a production appliance admits none).
      testMatch: /onboarding\.spec\.ts/,
      use: { ...devices["Desktop Chrome"], baseURL: onboardingUrl },
    },
    {
      name: "voices",
      // The voice setup workflow (docs/spec/07): the packaged binary with the
      // speech roles and the registry pointed at the scripted TTS instance
      // (web/e2e/fake-tts.mjs), so auditions, approval, registration and the
      // preview cache-hit are driven through the real binary with no model.
      testMatch: /voices\.spec\.ts/,
      use: { ...devices["Desktop Chrome"], baseURL: voicesUrl },
    },
    {
      name: "play",
      // The milestone 5 player walk (docs/spec/16 M5): the packaged binary with
      // the repo's testdata admitted to the replay allowlist (GAME_NARRATOR_REPLAY_ROOTS,
      // ADR-0024), so /play can start a real replay session on the synthetic clip.
      testMatch: /m5-part1\.spec\.ts/,
      use: { ...devices["Desktop Chrome"], baseURL: playUrl },
    },
    {
      name: "graph-regions",
      // The milestone 5 graph/regions walk (docs/spec/09 "/games/:id/graph" and
      // "/games/:id/regions"): the packaged binary with the repo's testdata on the
      // replay allowlist, so the region preview can run the real OCR engine over
      // the synthetic textbox frame (test-on-fixture, ADR-0024).
      testMatch: /m5-graph-regions\.spec\.ts/,
      use: { ...devices["Desktop Chrome"], baseURL: graphRegionsUrl },
    },
    {
      name: "corrections",
      // The correction labels and the regression fixtures they are (ADR-0052):
      // the editor's save records the label on the packaged binary, and the
      // developer's fixture view reads the same rows. No replay session runs —
      // the walk is the curation surface, so no replay allowlist is needed.
      testMatch: /corrections-fixtures\.spec\.ts/,
      use: { ...devices["Desktop Chrome"], baseURL: correctionsUrl },
    },
    {
      // The SSE reconnect walk (docs/spec/16 M5: "SSE reconnect reflects
      // accurate state"): the packaged binary with the repo's testdata on the
      // replay allowlist and the scripted TTS roles, exactly as the play walk
      // runs, so /play can drive the synthetic clip while the spec tears the
      // event stream down and checks the resume.
      name: "sse",
      testMatch: /sse-reconnect\.spec\.ts/,
      use: { ...devices["Desktop Chrome"], baseURL: sseUrl },
    },
    {
      // The full curation walk (docs/spec/16 M5: "curate a full synthetic
      // game with no CLI"), UI only end to end: create, import, edit,
      // characters, voice, regions, publish, export, then re-import the
      // export. The speech roles are the scripted TTS instance and the replay
      // allowlist admits the repo's testdata (the region preview's fixture).
      name: "curation",
      testMatch: /curation-walk\.spec\.ts/,
      use: { ...devices["Desktop Chrome"], baseURL: curationUrl },
    },
    {
      // The voice preview audio (docs/spec/16 M5: "voice preview works"):
      // the same curation server (its speech roles and registry are the
      // scripted instance). In a full-suite run the curation walk claims the
      // appliance first and this spec signs in; standalone it claims it
      // itself. The games are disjoint either way.
      name: "voice-preview",
      testMatch: /voice-preview-audio\.spec\.ts/,
      use: { ...devices["Desktop Chrome"], baseURL: curationUrl },
    },
    {
      // The route × first-run matrix (docs/spec/09 "Routes and page
      // requirements"): a fresh appliance of the packaged binary, so the
      // claim → login → guard redirects can be asserted in their real order.
      name: "routes",
      testMatch: /route-coverage\.spec\.ts/,
      use: { ...devices["Desktop Chrome"], baseURL: routesUrl },
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
    {
      command: `node web/e2e/serve.mjs developer 127.0.0.1:${ports.developer} ${embeddedBinary} serve`,
      cwd: "..",
      url: `${developerUrl}/api/v1/health/live`,
      env: { GAME_NARRATOR_REPLAY_ROOTS: replayRoots },
      reuseExistingServer: false,
      timeout: 60_000,
      stdout: "ignore",
      stderr: "pipe",
    },
    {
      // The scripted model gateway (web/e2e/fake-model.mjs) and its malicious
      // fixture page. Started before the onboarding server so the role URL it
      // is given is already listening when the first attempt runs.
      command: `node web/e2e/fake-model.mjs ${ports.fakeModel}`,
      cwd: "..",
      url: `http://127.0.0.1:${ports.fakeModel}/healthz`,
      reuseExistingServer: false,
      timeout: 30_000,
      stdout: "ignore",
      stderr: "pipe",
    },
    {
      // The onboarding agent on the packaged binary: the agent_reasoning role
      // is the scripted gateway, and the private-endpoint allowlist admits
      // exactly that one fixture host:port (the dev-mode opt-out of
      // docs/spec/14, used here so the fetch_page tool can reach the test's
      // malicious page; the allowlist is the test-only deviation, per ADR-0028).
      command: `node web/e2e/serve.mjs onboarding 127.0.0.1:${ports.onboarding} ${embeddedBinary} serve`,
      cwd: "..",
      url: `${onboardingUrl}/api/v1/health/live`,
      env: {
        GAME_NARRATOR_ROLE_AGENT_REASONING_MODEL: "fake/onboarding",
        GAME_NARRATOR_ROLE_AGENT_REASONING_URL: `http://127.0.0.1:${ports.fakeModel}/v1`,
        GAME_NARRATOR_WEB_ALLOW_PRIVATE: "1",
        GAME_NARRATOR_WEB_PRIVATE_ALLOW: `127.0.0.1:${ports.fakeModel}`,
      },
      reuseExistingServer: false,
      timeout: 60_000,
      stdout: "ignore",
      stderr: "pipe",
    },
    {
      // The scripted TTS instance (web/e2e/fake-tts.mjs): the VoiceDesign and
      // Base checkpoints plus the /audio/voices registry. Started before the
      // voices server so the role URLs it is given are already listening.
      command: `node web/e2e/fake-tts.mjs ${ports.fakeTTS}`,
      cwd: "..",
      url: `http://127.0.0.1:${ports.fakeTTS}/healthz`,
      reuseExistingServer: false,
      timeout: 30_000,
      stdout: "ignore",
      stderr: "pipe",
    },
    {
      // The voice workflow on the packaged binary: the speech roles speak to
      // the scripted instance, and the registry is the direct Base service —
      // the same instance, which is exactly how the port-forwarded deployment
      // resolves (docs/spec/06: the registry lives only on the Base service).
      command: `node web/e2e/serve.mjs voices 127.0.0.1:${ports.voices} ${embeddedBinary} serve`,
      cwd: "..",
      url: `${voicesUrl}/api/v1/health/live`,
      env: {
        GAME_NARRATOR_GATEWAY_URL: `http://127.0.0.1:${ports.fakeTTS}/v1`,
        GAME_NARRATOR_GATEWAY_TOKEN: "fake-e2e-token",
        GAME_NARRATOR_ROLE_VOICE_DESIGN_MODEL: "fake-voicedesign",
        GAME_NARRATOR_ROLE_VOICE_CLONE_MODEL: "fake-base",
        GAME_NARRATOR_TTS_BASE_DIRECT_URL: `http://127.0.0.1:${ports.fakeTTS}/v1`,
      },
      reuseExistingServer: false,
      timeout: 60_000,
      stdout: "ignore",
      stderr: "pipe",
    },
    {
      // The m5-part1 player walk on the packaged binary: the replay allowlist
      // admits the repo's testdata, exactly as the developer project does, so
      // /play can run the synthetic clip through the real pipeline. The speech
      // roles point at the same scripted TTS instance the voices walk uses, so
      // the session's audio pipeline is wired and /play's audio controls are
      // real (ADR-0039); the seeded walk has no approved voice, so the
      // committed lines end skipped with the reason — the counters show it.
      command: `node web/e2e/serve.mjs play 127.0.0.1:${ports.play} ${embeddedBinary} serve`,
      cwd: "..",
      url: `${playUrl}/api/v1/health/live`,
      env: {
        GAME_NARRATOR_REPLAY_ROOTS: replayRoots,
        GAME_NARRATOR_GATEWAY_URL: `http://127.0.0.1:${ports.fakeTTS}/v1`,
        GAME_NARRATOR_GATEWAY_TOKEN: "fake-e2e-token",
        GAME_NARRATOR_ROLE_VOICE_DESIGN_MODEL: "fake-voicedesign",
        GAME_NARRATOR_ROLE_VOICE_CLONE_MODEL: "fake-base",
        GAME_NARRATOR_TTS_BASE_DIRECT_URL: `http://127.0.0.1:${ports.fakeTTS}/v1`,
      },
      reuseExistingServer: false,
      timeout: 60_000,
      stdout: "ignore",
      stderr: "pipe",
    },
    {
      // The m5 graph/regions walk on the packaged binary: the replay allowlist
      // admits the repo's testdata so the region preview's `fixture` admits the
      // synthetic textbox frame, and the preview runs the real tesseract over it
      // (no speech roles are needed — no session runs in this walk).
      command: `node web/e2e/serve.mjs graph-regions 127.0.0.1:${ports.graphRegions} ${embeddedBinary} serve`,
      cwd: "..",
      url: `${graphRegionsUrl}/api/v1/health/live`,
      env: { GAME_NARRATOR_REPLAY_ROOTS: replayRoots },
      reuseExistingServer: false,
      timeout: 60_000,
      stdout: "ignore",
      stderr: "pipe",
    },
    {
      // The correction-labels walk on the packaged binary (ADR-0052): the
      // seeded profile is the same synthetic one — the walk needs a stored
      // message row to correct, not a session.
      command: `node web/e2e/serve.mjs corrections 127.0.0.1:${ports.corrections} ${embeddedBinary} serve`,
      cwd: "..",
      url: `${correctionsUrl}/api/v1/health/live`,
      reuseExistingServer: false,
      timeout: 60_000,
      stdout: "ignore",
      stderr: "pipe",
    },
    {
      // The SSE reconnect walk on the packaged binary: the same wiring as the
      // play server (testdata replay + scripted TTS roles) — /play needs a
      // session to run, and the audio pipeline keeps the page honest.
      command: `node web/e2e/serve.mjs sse 127.0.0.1:${ports.sse} ${embeddedBinary} serve`,
      cwd: "..",
      url: `${sseUrl}/api/v1/health/live`,
      env: {
        GAME_NARRATOR_REPLAY_ROOTS: replayRoots,
        GAME_NARRATOR_GATEWAY_URL: `http://127.0.0.1:${ports.fakeTTS}/v1`,
        GAME_NARRATOR_GATEWAY_TOKEN: "fake-e2e-token",
        GAME_NARRATOR_ROLE_VOICE_DESIGN_MODEL: "fake-voicedesign",
        GAME_NARRATOR_ROLE_VOICE_CLONE_MODEL: "fake-base",
        GAME_NARRATOR_TTS_BASE_DIRECT_URL: `http://127.0.0.1:${ports.fakeTTS}/v1`,
      },
      reuseExistingServer: false,
      timeout: 60_000,
      stdout: "ignore",
      stderr: "pipe",
    },
    {
      // The curation + voice-preview walks on one packaged binary: the
      // scripted TTS roles and registry (the voice half) plus the testdata
      // replay allowlist (the region preview's fixture). Two specs, one data
      // directory, disjoint games — the second signs in, it cannot re-claim.
      command: `node web/e2e/serve.mjs curation 127.0.0.1:${ports.curation} ${embeddedBinary} serve`,
      cwd: "..",
      url: `${curationUrl}/api/v1/health/live`,
      env: {
        GAME_NARRATOR_REPLAY_ROOTS: replayRoots,
        GAME_NARRATOR_GATEWAY_URL: `http://127.0.0.1:${ports.fakeTTS}/v1`,
        GAME_NARRATOR_GATEWAY_TOKEN: "fake-e2e-token",
        GAME_NARRATOR_ROLE_VOICE_DESIGN_MODEL: "fake-voicedesign",
        GAME_NARRATOR_ROLE_VOICE_CLONE_MODEL: "fake-base",
        GAME_NARRATOR_TTS_BASE_DIRECT_URL: `http://127.0.0.1:${ports.fakeTTS}/v1`,
      },
      reuseExistingServer: false,
      timeout: 60_000,
      stdout: "ignore",
      stderr: "pipe",
    },
    {
      // The route-coverage walk on the packaged binary: a fresh appliance,
      // nothing else pointed at it.
      command: `node web/e2e/serve.mjs routes 127.0.0.1:${ports.routes} ${embeddedBinary} serve`,
      cwd: "..",
      url: `${routesUrl}/api/v1/health/live`,
      reuseExistingServer: false,
      timeout: 60_000,
      stdout: "ignore",
      stderr: "pipe",
    },
  ],
});
