import { existsSync } from "node:fs";
import { join, resolve } from "node:path";

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
const wizardUrl = `http://127.0.0.1:${ports.wizard}`;

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
const discUrl = `http://127.0.0.1:${ports.disc}`;
const clipsUrl = `http://127.0.0.1:${ports.clips}`;
const routesUrl = `http://127.0.0.1:${ports.routes}`;
const a11yUrl = `http://127.0.0.1:${ports.a11y}`;
const redesignUrl = `http://127.0.0.1:${ports.redesign}`;
const libraryUrl = `http://127.0.0.1:${ports.library}`;
const ksearchUrl = `http://127.0.0.1:${ports.ksearch}`;
const agentDockUrl = `http://127.0.0.1:${ports.agentDock}`;
const playControlsUrl = `http://127.0.0.1:${ports.playControls}`;
const audioUrl = `http://127.0.0.1:${ports.audio}`;
const setupBoardUrl = `http://127.0.0.1:${ports.setupBoard}`;
const captureUrl = `http://127.0.0.1:${ports.capture}`;
const developerDecisionUrl = `http://127.0.0.1:${ports.developerDecision}`;
const developerActionsUrl = `http://127.0.0.1:${ports.developerActions}`;
const restoreUrl = `http://127.0.0.1:${ports.restore}`;
const fakeReleasesUrl = `http://127.0.0.1:${ports.fakeReleases}`;
const embeddedBinary = resolve(import.meta.dirname, "..", "bin", "game-narrator");
// The replay allowlist for the inspector's server: the repo's testdata, named
// explicitly (ADR-0024 "replay allowlist") rather than relying on the
// dev-checkout cwd default, so the synthetic clip is admitted wherever the
// checkout lives. This config lives in web/, so the repo root is one ".." up.
const replayRoots = resolve(import.meta.dirname, "..", "testdata");
// The disc-import walk's import directory (ADR-0066): the repo's synthetic
// disc (testdata/disc), whose bytes TestSyntheticDiscFixture guards.
const discImportDir = resolve(import.meta.dirname, "..", "testdata", "disc");

if (!existsSync(embeddedBinary)) {
  throw new Error(
    `playwright: ${embeddedBinary} not found — the embedded project tests the packaged binary, run \`make build\` first (or use \`make test-ui\`)`,
  );
}

// One root per run, shared by every server this config starts (the spawned shells
// inherit it), so one teardown removes all of it: the servers' data directories
// AND their manifests/logs (the run's e2e-runs dir, ADR-0094 — e2e/serve.mjs
// writes them here and e2e/support/servers.ts reads them from here, so two
// concurrent runs in the same checkout never read each other's records). This is
// set before webServer is read, and e2e/serve.mjs creates the directories inside
// it itself: Playwright starts the servers before it runs globalSetup, so a setup
// hook could not hand them a path in time.
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
  // Per-run, so the "clear output" wipe at run start touches only this run's
  // artifacts — a second Playwright run in the same checkout must not wipe the
  // first run's traces, screenshots or evidence mid-flight (ADR-0094).
  outputDir: join("test-results", `run-${process.pid}`),
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
        /embedded\.spec\.ts|catalog\.spec\.ts|onboarding\.spec\.ts|voices\.spec\.ts|voices-qa\.spec\.ts|m5-part1\.spec\.ts|m5-graph-regions\.spec\.ts|corrections-fixtures\.spec\.ts|live-play\.spec\.ts|play-controls\.spec\.ts|audio-output\.spec\.ts|bluetooth-output\.spec\.ts|capture-device\.spec\.ts|restore-walk\.spec\.ts|a11y\.spec\.ts|setup-wizard\.spec\.ts|redesign-shell\.spec\.ts|library\.spec\.ts|ksearch\.spec\.ts|agent-dock\.spec\.ts|setup-board\.spec\.ts/,
    },
    {
      // The first-run wizard (G9, ADR-0087) on its own packaged-binary
      // appliance (the wizard server below): the claim is once per appliance
      // (ADR-0016 §7), so the wizard cannot share the chromium dev-server
      // appliance m5-settings-tests claims — its "sign in to an already
      // claimed appliance" branch would sign in with an admin that never
      // exists there. No seams are wired: the walk asserts the honest
      // unconfigured states (the capture read 503s into its unavailable note,
      // the role probes answer NOT CONFIGURED), the labelled-honesty the
      // ADR's evidence demands.
      name: "setup-wizard",
      testMatch: /setup-wizard\.spec\.ts/,
      use: { ...devices["Desktop Chrome"], baseURL: wizardUrl },
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
      // (web/e2e/fake-tts.mjs), and the speech_asr role at the scripted ASR
      // instance (web/e2e/fake-asr.mjs), so auditions, the ASR QA badges,
      // approval, registration and the preview cache-hit are driven through
      // the real binary with no model (ADR 0061).
      testMatch: /voices.*\.spec\.ts/,
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
      // The player-controls walk (docs/spec/08 "/pause, /resume, /settings",
      // docs/spec/09 "/play"): the packaged binary with the repo's testdata on
      // the replay allowlist and the scripted TTS roles, exactly as the sse
      // walk runs, so /play can drive the synthetic clip with real (synthetic,
      // scripted-stub) audio while the pause/resume, narrated-class and
      // decision-mode controls are asserted against the server's own
      // snapshots (ADR-0039, ADR-0075).
      name: "play-controls",
      testMatch: /play-controls\.spec\.ts/,
      use: { ...devices["Desktop Chrome"], baseURL: playControlsUrl },
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
      // The disc-import walk (ADR-0066): a fresh appliance whose disc-import
      // allowlist is the repo's synthetic disc (testdata/disc), so the walk
      // imports a disc image through the UI without a user's real one
      // (synthetic fixture with guarded provenance, AGENTS.md rules 3 and 5).
      name: "disc",
      testMatch: /(disc-import-walk|speaker-link)\.spec\.ts/,
      use: { ...devices["Desktop Chrome"], baseURL: discUrl },
    },
    {
      // The managed replay clip store walk (ADR-0073) on the packaged binary,
      // with NO replay allowlist: the managed clip store under the data
      // directory is the only replay root the server admits, which is the
      // point of the walk (the env-file step the device run needed is gone).
      name: "clips",
      testMatch: /replay-clips-walk\.spec\.ts/,
      use: { ...devices["Desktop Chrome"], baseURL: clipsUrl },
    },
    {
      // The route × first-run matrix (docs/spec/09 "Routes and page
      // requirements"): a fresh appliance of the packaged binary, so the
      // claim → login → guard redirects can be asserted in their real order.
      name: "routes",
      testMatch: /route-coverage\.spec\.ts/,
      use: { ...devices["Desktop Chrome"], baseURL: routesUrl },
    },
    {
      // The accessibility walk (AGENTS.md rule 9: "capture traces/screenshots
      // and accessibility results"): axe-core over every route in
      // docs/status/ui-coverage.md, on a fresh packaged binary that the spec
      // claims and seeds with the synthetic game (no model is needed — no
      // session, audition or preview runs in this walk).
      name: "a11y",
      testMatch: /a11y\.spec\.ts/,
      use: { ...devices["Desktop Chrome"], baseURL: a11yUrl },
    },
    {
      // The Plum & Peach shell review (docs/design/plum-peach plan.md Phase 0
      // item 6): the global chrome and the component kit gallery on a fresh
      // packaged-binary appliance — screenshots at the design's three widths
      // (1440 / 1024 / 390) plus axe, zero serious or critical. The spec
      // claims the appliance as admin (the kit gallery's developer gate and
      // the settings page's admin gate both open to it).
      name: "redesign",
      testMatch: /redesign-shell\.spec\.ts/,
      use: { ...devices["Desktop Chrome"], baseURL: redesignUrl },
    },
    {
      // The Plum & Peach Library screen (plan.md Phase 1 item 1): a fresh
      // packaged-binary appliance with the agent_reasoning role at the
      // scripted gateway (the in-setup seed is a live labelled-synthetic
      // onboarding run at its publish review), screenshots at the design's
      // three widths plus axe, and a real session start through the hero's
      // ▶ (the test_pattern frame source, ADR-0109).
      name: "library",
      testMatch: /library\.spec\.ts/,
      use: { ...devices["Desktop Chrome"], baseURL: libraryUrl },
    },
    {
      name: "ksearch",
      testMatch: /ksearch\.spec\.ts/,
      use: { ...devices["Desktop Chrome"], baseURL: ksearchUrl },
    },
    {
      // The agent dock (ADR-0099, docs/design/plum-peach plan.md Phase 1
      // item 3): the packaged binary with the agent_reasoning role at the
      // scripted model gateway's assistant scenario, so the dock's send makes
      // a genuine provider request the scripted instance answers. The spec
      // asserts the outgoing context payload (UX-01) through a redacted
      // capture of its own request, the chip against the selection, and the
      // ⌘J reachability over the six primary routes.
      name: "agent-dock",
      testMatch: /agent-dock\.spec\.ts/,
      use: { ...devices["Desktop Chrome"], baseURL: agentDockUrl },
    },
    {
      // The setup board (docs/design/plum-peach plan.md Phase 1 item 4,
      // ADR-0093 / ADR-0108, UX-06): the packaged binary with the
      // agent_reasoning role at the scripted setup gateway (web/e2e/
      // fake-setup-model.mjs) and the speech roles at the scripted TTS
      // instance, so the board's live half runs through the real SSE stream
      // (setup.character_stage / setup.progress) and the real audition job
      // (the StageObserver's voicing and done transitions). One project,
      // one server, one data directory: the walk signs in, starts the run,
      // watches the cards glide, approves the publish boundary and lands
      // on the Cast seam.
      name: "setup-board",
      testMatch: /setup-board\.spec\.ts/,
      use: { ...devices["Desktop Chrome"], baseURL: setupBoardUrl },
    },
    {
      // The developer inspector's decision half (docs/spec/13 "did it speak,
      // and why not"; ADR-0055): the replay session's ambiguous ranking is
      // sent to the scripted System One instance configured as the decision
      // role (a synthetic stub, labelled as such), and the same appliance's
      // endpoint-down case shows the degraded local commit. One project,
      // one server, one data directory: the second test toggles the scripted
      // instance into down mode and drives a fresh session.
      name: "developer-decision",
      testMatch: /developer-decision\.spec\.ts/,
      use: { ...devices["Desktop Chrome"], baseURL: developerDecisionUrl },
    },
    {
      // The developer inspector's per-event actions (ADR-0083, docs/spec/09):
      // the full wiring of the developer-decision server — the scripted Clef
      // instance resolves the seed's ambiguous rankings (a fully wired run
      // with a decision trace to re-run) and the scripted TTS instance makes
      // the utterance's audio real (synthetic-stub) bytes the waveform, the
      // fixture and the evidence bundle carry.
      name: "developer-actions",
      testMatch: /developer-actions\.spec\.ts/,
      use: { ...devices["Desktop Chrome"], baseURL: developerActionsUrl },
    },
    {
      // The backup → mutate → restore walk (ADR-0090) and the update
      // notification's surface (ADR-0088): its own server (the restore
      // restarts the process — a shared server would die under every other
      // project) with the update checker pointed at the scripted releases
      // endpoint. The spec restarts the replacement itself from the server
      // manifest; see the header of restore-walk.spec.ts.
      name: "settings-restore",
      testMatch: /restore-walk\.spec\.ts/,
      use: { ...devices["Desktop Chrome"], baseURL: restoreUrl },
    },
    {
      // The capture-dongle walk (ADR-0078, docs/spec/08/10): the packaged
      // binary with the test seam standing in for the UVC HDMI dongle
      // (GAME_NARRATOR_CAPTURE_FAKE) — a labelled-synthetic device fed by a
      // real FFmpeg testsrc2 pipe (loopback-free, real frames through the
      // production consumer) — so the Settings capture section (device list,
      // selection, the throttled preview, the capture test) and a full kind
      // v4l2 session run without hardware, labelled synthetic throughout.
      name: "capture-device",
      testMatch: /capture-device\.spec\.ts/,
      use: { ...devices["Desktop Chrome"], baseURL: captureUrl },
    },
    {
      // The audio-output walk (ADR-0074, docs/spec/10): the packaged binary
      // with the test seam standing in for the enumeration backend — a
      // labelled-synthetic device list (the page must say so) — and the
      // filesink recording the test tone, so the walk proves the product
      // surface (device selection, the consent-gated tone, the master
      // volume) without hardware and with the tone's WAV as the evidence.
      // GAME_NARRATOR_BT_FAKE (ADR-0091) wires the labelled-synthetic
      // Bluetooth controller so the Bluetooth output section walks the same
      // way; the fake device list carries one bluez_output.<MAC>.1 sink so
      // the selection walk can pick the Bluetooth sink. The addresses are
      // the bt package's obviously-fake fixtures (the public-repo rule).
      name: "audio",
      testMatch: /audio-output\.spec\.ts|bluetooth-output\.spec\.ts/,
      use: { ...devices["Desktop Chrome"], baseURL: audioUrl },
    },
    {
      // The live play path (task browser-live-play, AGENTS.md rule 3 class
      // (d)+(e)): a real Dolphin emulator and the real gateway driven through
      // the packaged /play and /developer pages. No webServer here on
      // purpose — its server is the Go orchestrator's in-process API +
      // embedded SPA (internal/e2e TestLivePlayUI): the production binary
      // never wires the dev Dolphin bridge into the process (internal/api/
      // dolphin.go answers 503 unwired), so only the test process that owns
      // the bridge can serve a live kind:dolphin session. The Go test runs
      // the spec through web/e2e/live-play.playwright.config.ts (no
      // webServer entries: the suite's other servers own ports this run must
      // not fight for) with the GN_LIVE_UI_* env; without that env the spec
      // is NOT RUN, which is how this project — and make test-ui / CI —
      // skip it.
      name: "live-play",
      testMatch: /live-play\.spec\.ts/,
      use: {
        ...devices["Desktop Chrome"],
        baseURL: process.env.GN_LIVE_UI_URL,
        // The trace is the evidence artifact (AGENTS.md rule 9): always on
        // for this project, copied into the run's bundle by the Go test.
        trace: "on",
      },
    },
  ],
  webServer: [
    {
      command: `node web/e2e/serve.mjs api 127.0.0.1:${ports.api} go run ./cmd/game-narrator serve`,
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
      // The dev-server port is passed explicitly (vite.config.ts pins 5173
      // with strictPort): on a default-base run this is exactly 5173 — today's
      // behaviour — and on a moved base each run owns its own Vite (ADR-0094).
      command: `pnpm dev --port ${ports.web}`,
      url: webUrl,
      env: { GAME_NARRATOR_API: apiUrl },
      reuseExistingServer: !process.env.CI,
      timeout: 120_000,
      stdout: "ignore",
      stderr: "pipe",
    },
    {
      command: `node web/e2e/serve.mjs embedded 127.0.0.1:${ports.embedded} ${embeddedBinary} serve`,
      cwd: "..",
      url: `${embeddedUrl}/api/v1/health/live`,
      reuseExistingServer: false,
      timeout: 60_000,
      stdout: "ignore",
      stderr: "pipe",
    },
    {
      command: `node web/e2e/serve.mjs catalog 127.0.0.1:${ports.catalog} ${embeddedBinary} serve`,
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
      // The scripted ASR instance (web/e2e/fake-asr.mjs): the transcription
      // wire the speech_asr role speaks, with a synthetic stub transcript
      // (ADR 0061). Started before the voices server so the role URL it is
      // given is already listening.
      command: `node web/e2e/fake-asr.mjs ${ports.fakeASR}`,
      cwd: "..",
      url: `http://127.0.0.1:${ports.fakeASR}/healthz`,
      env: {
        // The brief the specs use when they create auditions: the stub
        // recovers the spoken line from the tone's frequency, and the seed
        // couples line + brief + model, so a mismatch would make every
        // audition unreadable. FAKE_ASR_FAIL_LINE is the planted mishearing
        // the QA badge spec asserts (the flagged audition).
        FAKE_ASR_BRIEF: "A calm harbor voice, unhurried, with a slight salt air in it.",
        FAKE_ASR_FAIL_LINE: "3",
      },
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
        // The ASR QA loop (FR-10, ADR 0061): the speech_asr role speaks to
        // the scripted ASR instance on its own URL — the dedicated-endpoint
        // path, so the gateway token is not (and must not be) needed.
        GAME_NARRATOR_ROLE_SPEECH_ASR_MODEL: "fake-asr",
        GAME_NARRATOR_ROLE_SPEECH_ASR_URL: `http://127.0.0.1:${ports.fakeASR}/v1`,
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
      // The scripted setup gateway (web/e2e/fake-setup-model.mjs): the
      // onboarding agent's scripted flow for the setup-board walk (import →
      // source quality → stage two characters → the first voice brief →
      // stop, plus the publish token branch). Started before the
      // setup-board server so the role URL it is given is already listening.
      command: `node web/e2e/fake-setup-model.mjs ${ports.setupBoardModel}`,
      cwd: "..",
      url: `http://127.0.0.1:${ports.setupBoardModel}/healthz`,
      reuseExistingServer: false,
      timeout: 30_000,
      stdout: "ignore",
      stderr: "pipe",
    },
    {
      // The setup-board server (setup-board.spec.ts): the agent_reasoning role
      // is the scripted setup gateway above; the speech roles are the scripted
      // TTS instance the voices walk uses, so the Marla audition that drives
      // the board's voicing → done card moves runs the real job-backed voice
      // workflow (ADR-0108's StageObserver). No ASR role: the QA gate stays
      // off and the approval path is the walk's.
      command: `node web/e2e/serve.mjs setup-board 127.0.0.1:${ports.setupBoard} ${embeddedBinary} serve`,
      cwd: "..",
      url: `${setupBoardUrl}/api/v1/health/live`,
      env: {
        GAME_NARRATOR_ROLE_AGENT_REASONING_MODEL: "fake/setup-board",
        GAME_NARRATOR_ROLE_AGENT_REASONING_URL: `http://127.0.0.1:${ports.setupBoardModel}/v1`,
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
      // The player-controls walk server: the same wiring as the sse server
      // (testdata replay + scripted TTS roles) — /play needs a running
      // session with real audio for the pause/resume and narrated-class
      // assertions to have something to fence (ADR-0039, ADR-0075).
      command: `node web/e2e/serve.mjs play-controls 127.0.0.1:${ports.playControls} ${embeddedBinary} serve`,
      cwd: "..",
      url: `${playControlsUrl}/api/v1/health/live`,
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
      // The disc-import walk (ADR-0066) on the packaged binary: the disc
      // import's allowlist is the repo's synthetic disc directory — the
      // operator-configured directory the dir shape reads from in place.
      command: `node web/e2e/serve.mjs disc 127.0.0.1:${ports.disc} ${embeddedBinary} serve`,
      cwd: "..",
      url: `${discUrl}/api/v1/health/live`,
      env: { GAME_NARRATOR_IMPORT_DIR: discImportDir },
      reuseExistingServer: false,
      timeout: 60_000,
      stdout: "ignore",
      stderr: "pipe",
    },
    {
      // The managed clip store walk (ADR-0073): deliberately no
      // GAME_NARRATOR_REPLAY_ROOTS — the managed store inside this server's
      // data directory is the only replay root, and the walk asserts the
      // synthetic clip runs from there.
      command: `node web/e2e/serve.mjs clips 127.0.0.1:${ports.clips} ${embeddedBinary} serve`,
      cwd: "..",
      url: `${clipsUrl}/api/v1/health/live`,
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
    {
      // The accessibility walk on the packaged binary: a fresh appliance the
      // spec claims and seeds through the UI; no env is needed because the
      // walk never runs a session, an audition or a preview.
      command: `node web/e2e/serve.mjs a11y 127.0.0.1:${ports.a11y} ${embeddedBinary} serve`,
      cwd: "..",
      url: `${a11yUrl}/api/v1/health/live`,
      reuseExistingServer: false,
      timeout: 60_000,
      stdout: "ignore",
      stderr: "pipe",
    },
    {
      // The Plum & Peach shell review on the packaged binary: a fresh
      // appliance the spec claims; no session runs (no env needed).
      command: `node web/e2e/serve.mjs redesign 127.0.0.1:${ports.redesign} ${embeddedBinary} serve`,
      cwd: "..",
      url: `${redesignUrl}/api/v1/health/live`,
      reuseExistingServer: false,
      timeout: 60_000,
      stdout: "ignore",
      stderr: "pipe",
    },
    {
      // The Plum & Peach Library screen on the packaged binary: the
      // agent_reasoning role at the scripted gateway (the same wiring as the
      // onboarding server), so the spec's in-setup seed is a live onboarding
      // run the status bar and the tile chip both read. No private-endpoint
      // allowlist: the scenario's import is a staged file, not a fetch.
      command: `node web/e2e/serve.mjs library 127.0.0.1:${ports.library} ${embeddedBinary} serve`,
      cwd: "..",
      url: `${libraryUrl}/api/v1/health/live`,
      env: {
        GAME_NARRATOR_ROLE_AGENT_REASONING_MODEL: "fake/onboarding",
        GAME_NARRATOR_ROLE_AGENT_REASONING_URL: `http://127.0.0.1:${ports.fakeModel}/v1`,
      },
      reuseExistingServer: false,
      timeout: 60_000,
      stdout: "ignore",
      stderr: "pipe",
    },
    {
      // The ⌘K command-palette walk on the packaged binary: a fresh appliance
      // the spec claims and seeds (create → edition → draft → import) so the
      // palette's grouped results run against a real catalogue. The voice
      // roles are the project's fake (synthetic stub, named for what it is):
      // the regenerate-action row's assertion is the 202 that queues the
      // audition, and the fake gateway lets the queued job run without a real
      // model (AGENTS.md rule 3).
      command: `node web/e2e/serve.mjs ksearch 127.0.0.1:${ports.ksearch} ${embeddedBinary} serve`,
      cwd: "..",
      url: `${ksearchUrl}/api/v1/health/live`,
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
      // The scripted System One / Clef-Omni instance (web/e2e/fake-clef.mjs),
      // a synthetic stub named for what it is. Started before the
      // developer-decision server so the decision role URL it is given is
      // already listening when the session's first ambiguous ranking lands.
      command: `node web/e2e/fake-clef.mjs ${ports.fakeClef}`,
      cwd: "..",
      url: `http://127.0.0.1:${ports.fakeClef}/healthz`,
      reuseExistingServer: false,
      timeout: 30_000,
      stdout: "ignore",
      stderr: "pipe",
    },
    {
      // The developer inspector's decision walk on the packaged binary
      // (ADR-0055): the same wiring as the developer server (the replay
      // allowlist admits the synthetic clip) plus the decision role pointed
      // at the scripted Clef instance and the speech roles at the scripted TTS
      // instance, so an ambiguous ranking gets a real Clef call and the
      // committed line's speech rows ride the same trace.
      command: `node web/e2e/serve.mjs developer-decision 127.0.0.1:${ports.developerDecision} ${embeddedBinary} serve`,
      cwd: "..",
      url: `${developerDecisionUrl}/api/v1/health/live`,
      env: {
        GAME_NARRATOR_REPLAY_ROOTS: replayRoots,
        GAME_NARRATOR_ROLE_DECISION_URL: `http://127.0.0.1:${ports.fakeClef}`,
        GAME_NARRATOR_ROLE_DECISION_MODEL: "fake-clef",
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
      // The developer inspector's actions walk on the packaged binary
      // (ADR-0083): the replay allowlist admits the synthetic clip, the
      // speech roles at the scripted TTS instance (fake-tts.mjs), so the
      // committed lines speak and the utterance's WAV is what the fixture,
      // the waveform and the evidence bundle carry. The seed commits locally
      // (the clip's OCR reading is an exact corpus match — the actions need a
      // fully committed, fully spoken trace, not a provider call; the
      // decision chain's walk is the developer-decision project's), and the
      // decision role still points at the scripted Clef instance so the walk
      // runs against a fully wired appliance.
      command: `node web/e2e/serve.mjs developer-actions 127.0.0.1:${ports.developerActions} ${embeddedBinary} serve`,
      cwd: "..",
      url: `${developerActionsUrl}/api/v1/health/live`,
      env: {
        GAME_NARRATOR_REPLAY_ROOTS: replayRoots,
        GAME_NARRATOR_ROLE_DECISION_URL: `http://127.0.0.1:${ports.fakeClef}`,
        GAME_NARRATOR_ROLE_DECISION_MODEL: "fake-clef",
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
      // The audio-output walk server (ADR-0074): the test seam stands in for
      // the enumeration backend and the filesink is the recorded sink — the
      // test tone leaves its WAV under the server's data directory instead of
      // needing a speaker, which is exactly the documented degraded path on
      // a machine with no audible output.
      command: `node web/e2e/serve.mjs audio 127.0.0.1:${ports.audio} ${embeddedBinary} serve`,
      cwd: "..",
      url: `${audioUrl}/api/v1/health/live`,
      env: {
        GAME_NARRATOR_AUDIO_FAKE_DEVICES:
          "auto_null:null:48000,JBL-USB-DAC:usb:48000,bluez_output.AA:BB:CC:DD:EE:FF.1:bluetooth:48000",
        GAME_NARRATOR_AUDIO_SINK: "filesink",
        GAME_NARRATOR_BT_FAKE: "1",
      },
      reuseExistingServer: false,
      timeout: 60_000,
      stdout: "ignore",
      stderr: "pipe",
    },
    {
      // The capture-dongle walk server (ADR-0078): the test seam stands in
      // for the dongle — one labelled-synthetic device whose frames are a
      // real FFmpeg testsrc2 pipe — so the preview, the probe and a full
      // v4l2 session run the production consumer without the hardware
      // (which is NOT RUN until the card is attached).
      command: `node web/e2e/serve.mjs capture 127.0.0.1:${ports.capture} ${embeddedBinary} serve`,
      cwd: "..",
      url: `${captureUrl}/api/v1/health/live`,
      env: { GAME_NARRATOR_CAPTURE_FAKE: "1" },
      reuseExistingServer: false,
      timeout: 60_000,
      stdout: "ignore",
      stderr: "pipe",
    },
    {
      // The scripted GitHub-releases endpoint (ADR-0088), started before the
      // restore server so its boot-time update check finds it listening. The
      // walk's binary is a dev build (no version stamp), so the assertion is
      // on the section's honesty — it names the endpoint's latest release
      // and links its notes — while the comparison's logic is the Go
      // tests' business (internal/updates, the fake release server there).
      command: `node web/e2e/fake-releases.mjs ${ports.fakeReleases}`,
      cwd: "..",
      url: `${fakeReleasesUrl}/healthz`,
      reuseExistingServer: false,
      timeout: 30_000,
      stdout: "ignore",
      stderr: "pipe",
    },
    {
      // The restore walk's server: the packaged binary on its own data
      // directory, with the update checker pointed at the scripted endpoint.
      // GAME_NARRATOR_UPDATE_BASE_URL is declared here so the scrub keeps it
      // (web/e2e/serve.mjs re-reads this entry for the declared names).
      // GN_E2E_KEEP_DATA_DIR: the walk's spec restarts the appliance itself
      // against this data directory, so the launcher must not delete it when
      // the first process exits (the per-run teardown is the backstop).
      command: `node web/e2e/serve.mjs restore 127.0.0.1:${ports.restore} ${embeddedBinary} serve`,
      cwd: "..",
      url: `${restoreUrl}/api/v1/health/live`,
      env: {
        GAME_NARRATOR_UPDATE_BASE_URL: fakeReleasesUrl,
        GN_E2E_KEEP_DATA_DIR: "1",
      },
      reuseExistingServer: false,
      timeout: 60_000,
      stdout: "ignore",
      stderr: "pipe",
    },
    {
      // The setup wizard's own appliance (ADR-0087 G9): a fresh packaged
      // binary the spec claims. No seams and no model env: the walk asserts
      // the honest unconfigured states (the capture read 503s into its
      // "unavailable" note, the role probes answer NOT CONFIGURED), which is
      // exactly the labelled-honesty the ADR demands. Its own server because
      // the claim is once per appliance (ADR-0016 §7) and the chromium
      // dev-server appliance is claimed by m5-settings-tests' admin.
      command: `node web/e2e/serve.mjs setup-wizard 127.0.0.1:${ports.wizard} ${embeddedBinary} serve`,
      cwd: "..",
      url: `${wizardUrl}/api/v1/health/live`,
      reuseExistingServer: false,
      timeout: 60_000,
      stdout: "ignore",
      stderr: "pipe",
    },
    {
      // The agent-dock walk on the packaged binary (ADR-0099): the
      // agent_reasoning role is the scripted model gateway's assistant
      // scenario — one plain final reply, no tool calls — so the dock's send
      // proves the provider boundary with no real model (AGENTS.md rule 3).
      // The gateway is the suite's shared fake-model.mjs (started earlier in
      // this list); its scenario is global, so each spec sets its own before
      // the first chat request and the one-worker run keeps the specs serial.
      command: `node web/e2e/serve.mjs agent-dock 127.0.0.1:${ports.agentDock} ${embeddedBinary} serve`,
      cwd: "..",
      url: `${agentDockUrl}/api/v1/health/live`,
      env: {
        GAME_NARRATOR_ROLE_AGENT_REASONING_MODEL: "fake/assistant",
        GAME_NARRATOR_ROLE_AGENT_REASONING_URL: `http://127.0.0.1:${ports.fakeModel}/v1`,
      },
      reuseExistingServer: false,
      timeout: 60_000,
      stdout: "ignore",
      stderr: "pipe",
    },
  ],
});
