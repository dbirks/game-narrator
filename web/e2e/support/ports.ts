// The ports the e2e servers listen on, in one place. playwright.config.ts
// reads them to start the servers; the specs read them to assert which server
// a project is actually talking to. A port that lives in only one of the two
// is the config/spec drift e2e/isolation.spec.ts exists to catch.
//
// Every port is ONE BASE plus a stable per-server offset, so several
// Playwright runs in the same checkout can share the box: each run takes a
// different base and the offsets keep the wiring (config commands <-> specs
// <-> webServer env blocks) intact. The selection of the base — probe and
// atomic claim for the unset/auto path, explicit number as-is — lives in
// e2e/support/portband.ts (ADR-0094, issue #43); this module applies it
// once at load and turns the base into the port table below.
//
// GN_E2E_PORT_BASE (read at config load, before the webServer commands and
// the workers start):
//   (unset)  probe for a free band at config load: the first candidate base
//            (18090, 18190, 18290, ...) whose whole band (base .. base+31)
//            and whose dev-server port (base + WEB_PORT_OFFSET) answer no
//            connection wins, and is then CLAIMED atomically (a mkdir lock
//            under os.tmpdir(), so two runs that probe the same instant
//            cannot take the same band). The default base 18090 reproduces
//            the historical fixed ports (18090-18119 plus the Vite dev port
//            5173) exactly, so a lone run behaves exactly like before (CI
//            and the docs are unchanged).
//   <number> that base, used as-is — an explicit base is an operator choice,
//            so it is validated and never probed.
//   auto     the same probe-and-claim as the unset path.
//
// The resolved base is written back to process.env as a number, so the
// webServer commands, the serve.mjs launchers and the worker processes all
// agree. The candidate bands (18090 + k*100) deliberately never enter
// 18120-18139, the band the Go live-play test owns
// (internal/e2e livePlayPortCandidates).
import {
  DEFAULT_PORT_BASE,
  PORT_BAND,
  releaseBand,
  resolvePortBase,
  WEB_PORT_OFFSET,
} from "./portband";

export { DEFAULT_PORT_BASE, PORT_BAND, WEB_PORT_OFFSET };

// Resolved once at module load, in the process that loaded playwright.config.ts.
// Workers inherit the resolved number through the environment, so they re-derive
// the same ports without probing or claiming.
const base = await resolvePortBase(process.env.GN_E2E_PORT_BASE);
// Release the auto band claim when THIS process exits: the run's servers are
// being torn down by then, and the band must be claimable again by the next
// auto run. (A kill -9'd run leaves the claim behind; the next run reclaims
// it through the holder-pid liveness check in portband.claimBand.)
if (base.lock !== null) {
  const lock = base.lock;
  process.on("exit", () => {
    try {
      releaseBand(lock);
    } catch {
      // best effort: the stale-claim reclamation is the backstop
    }
  });
}
// Export the resolved base (always a number, never "auto") so every process
// this run spawns — the webServer shells, serve.mjs, the workers — reads the
// same value whether or not the operator set the variable.
process.env.GN_E2E_PORT_BASE = String(base.base);

export const ports = {
  /** `go run ./cmd/game-narrator serve` behind the Vite dev server. */
  api: base.base + 0,
  /** Vite dev server — the chromium project's baseURL. */
  web: base.base + WEB_PORT_OFFSET,
  /** The setup wizard's own appliance (ADR-0087 G9): the claim is once per
   *  appliance (ADR-0016 §7) and the chromium dev-server appliance is claimed
   *  by m5-settings-tests' admin, so the wizard claims a fresh packaged-binary
   *  server it owns. */
  wizard: base.base + 1,
  /** the packaged binary the Health screen's walk drives (health.spec.ts,
   *  ADR-0111, plan.md Phase 1 item 8): the capture seam stands in for the
   *  dongle (GAME_NARRATOR_CAPTURE_FAKE, the capture-dongle walk's seam), so
   *  the UX-11 signal/preview tile reads a real FFmpeg testsrc2 feed without
   *  the hardware. Offset 4 — offsets 2-3 are unused band slots. */
  health: base.base + 4,
  /** the packaged binary serving the embedded UI (embedded.spec.ts). */
  embedded: base.base + 5,
  /** the packaged binary the catalogue walk-through claims (catalog.spec.ts). */
  catalog: base.base + 6,
  /** the packaged binary the developer inspector drives a replay on (developer.spec.ts). */
  developer: base.base + 7,
  /** the packaged binary the onboarding agent runs against (onboarding.spec.ts). */
  onboarding: base.base + 8,
  /** the scripted model gateway + malicious fixture page (web/e2e/fake-model.mjs). */
  fakeModel: base.base + 9,
  /** the scripted TTS instance (web/e2e/fake-tts.mjs) the voice spec points the
   *  speech roles and the registry at. */
  fakeTTS: base.base + 10,
  /** the packaged binary the voice workflow drives (voices.spec.ts). */
  voices: base.base + 11,
  /** the packaged binary the m5-part1 walk (play / characters / publish / diagnostics)
   *  drives, with the repo's testdata admitted to the replay allowlist. */
  play: base.base + 12,
  /** the packaged binary the m5 graph/regions walk drives: the replay allowlist
   *  admits the repo's testdata (the preview's fixture), so OCR runs against
   *  the synthetic textbox frame. */
  graphRegions: base.base + 13,
  /** the packaged binary the correction-labels / regression-fixture walk drives
   *  (corrections-fixtures.spec.ts): the editor's save records the label, and
   *  the developer's fixture view reads the same rows (ADR-0052). */
  corrections: base.base + 14,
  /** the packaged binary the SSE reconnect walk drives (sse-reconnect.spec.ts):
   *  the replay allowlist admits the repo's testdata so /play can run the
   *  synthetic clip, and the speech roles point at the scripted TTS instance
   *  the way the play walk does (ADR-0039). */
  sse: base.base + 15,
  /** the packaged binary the full-curation and voice-preview walks drive
   *  (curation-walk.spec.ts, voice-preview-audio.spec.ts): the replay
   *  allowlist admits the repo's testdata (the region preview's fixture) and
   *  the speech roles point at the scripted TTS instance (fake-tts.mjs), so
   *  one server hosts both walks. */
  curation: base.base + 16,
  /** the packaged binary the route-coverage walk drives
   *  (route-coverage.spec.ts): a fresh appliance for the first-run →
   *  claimed → unauthenticated guard matrix. */
  routes: base.base + 17,
  /** the packaged binary the accessibility (axe) walk drives (a11y.spec.ts):
   *  a fresh appliance, claimed and seeded with the synthetic game so every
   *  route is visited in the state the other specs establish. */
  a11y: base.base + 18,
  /** the scripted System One / Clef-Omni instance (web/e2e/fake-clef.mjs) the
   *  developer-decision walk configures as the decision role. */
  fakeClef: base.base + 19,
  /** the packaged binary the developer-decision walk drives (developer-decision.spec.ts):
   *  the inspector's replay with the ambiguous ranking sent to the scripted
   *  Clef instance and the endpoint-down degraded case. */
  developerDecision: base.base + 20,
  /** the scripted ASR instance (web/e2e/fake-asr.mjs) the voices walk points
   *  the speech_asr role at, so the audition QA badges run against the
   *  transcription wire with a synthetic stub (ADR 0061). */
  fakeASR: base.base + 21,
  /** the packaged binary the disc-import walk drives (disc-import-walk.spec.ts,
   *  ADR-0066): a fresh appliance whose disc-import allowlist is the repo's
   *  synthetic disc (testdata/disc), so the walk imports a disc image through
   *  the UI without a user's real one. */
  disc: base.base + 22,
  /** the packaged binary the managed-clip-store walk drives
   *  (replay-clips-walk.spec.ts, ADR-0073): NO replay allowlist at all — the
   *  managed clip store under the data dir is the only replay root, which is
   *  the point of the walk. */
  clips: base.base + 23,
  /** the packaged binary the player-controls walk drives
   *  (play-controls.spec.ts, docs/spec/08 "/pause, /resume, /settings"): the
   *  replay allowlist admits the repo's testdata and the speech roles point
   *  at the scripted TTS instance, the way the sse walk does, so /play can
   *  run the synthetic clip with real audio while the controls are asserted.
   */
  playControls: base.base + 24,
  /** the packaged binary the audio-output walk drives (audio-output.spec.ts,
   *  ADR-0074): the test seam stands in for the enumeration backend (a
   *  labelled-synthetic device list) and the filesink records the test tone,
   *  so the walk proves the product surface without hardware. */
  audio: base.base + 25,
  /** the packaged binary the capture-device walk drives (capture-device.spec.ts,
   *  ADR-0078): the test seam stands in for the dongle (GAME_NARRATOR_CAPTURE_FAKE) —
   *  a labelled-synthetic device fed by a real FFmpeg testsrc2 pipe — so the
   *  Settings capture section and a full v4l2 session run without hardware. */
  capture: base.base + 26,
  /** the packaged binary the developer inspector's per-event actions drive
   *  (developer-actions.spec.ts, ADR-0083): a replay session with the speech
   *  roles at the scripted TTS instance (fake-tts.mjs), so the utterance's
   *  audio is real (synthetic-stub) bytes the fixture, the waveform and the
   *  evidence bundle can carry. */
  developerActions: base.base + 27,
  /** the packaged binary the backup → mutate → restore walk drives
   *  (restore-walk.spec.ts, ADR-0087): the walk's own server, because the
   *  restore restarts the process — killing the shared `api` server would
   *  orphan every other project's run. The spec restarts the replacement
   *  from the server manifest (the per-run e2e-runs dir, ADR-0094). */
  restore: base.base + 28,
  /** the scripted GitHub-releases endpoint (web/e2e/fake-releases.mjs) the
   *  restore server's update checker polls (ADR-0088): the latest release it
   *  serves is the one the Settings section must name, with its notes link.
   */
  fakeReleases: base.base + 29,
  /** the packaged binary the Plum & Peach shell review drives
   *  (redesign-shell.spec.ts, docs/design/plum-peach plan.md Phase 0 item 6):
   *  a fresh appliance the spec claims, screenshotting the shell and the kit
   *  gallery at the design's three widths and axing them. Lives just past the
   *  30-port band, the way the Vite dev port does — base+30 is 18120 at the
   *  default base, the first port of the Dolphin live-play band that only
   *  binds under its explicit hardware gate, which never runs inside the
   *  Playwright suite. */
  redesign: base.base + 30,
  /** the packaged binary the Plum & Peach Library screen review drives
   *  (library.spec.ts, plan.md Phase 1 item 1): a fresh appliance with the
   *  agent_reasoning role at the scripted gateway, so the in-setup seed is a
   *  live (labelled-synthetic) onboarding run. Like the redesign port just
   *  past the 30-port band, base+31 (18121 at the default base) sits inside
   *  the Dolphin live-play band — safe here because that band only binds
   *  under its explicit hardware gate, which never runs inside the Playwright
   *  suite. */
  library: base.base + 31,
  /** the packaged binary the ⌘K palette walk drives (ksearch.spec.ts,
   *  docs/design/plum-peach plan.md Phase 1 item 2, ADR-0098): a fresh
   *  appliance the spec claims and seeds with the synthetic game, so the
   *  grouped results, the row actions and the Tab handoff run against a
   *  real catalogue. Same "just past the band" placement as the redesign
   *  review port above, one step further out so the review ports stay
   *  unique per project. */
  ksearch: base.base + 32,
  /** the packaged binary the agent-dock walk drives (agent-dock.spec.ts,
   *  ADR-0099, docs/design/plum-peach plan.md Phase 1 item 3): a fresh
   *  appliance whose agent_reasoning role is the scripted model gateway
   *  (fake-model.mjs) in its assistant scenario, so the dock's send proves
   *  the provider boundary without a real model. Same "just past the band"
   *  placement as the review ports above, one step further out so the
   *  review ports stay unique per project. */
  agentDock: base.base + 33,
  /** the scripted model gateway (web/e2e/fake-setup-model.mjs) the setup-board
   *  server's agent_reasoning role speaks: import → source quality → stage two
   *  characters → the first voice brief → stop. Same "just past the band"
   *  placement as the review ports above, one step further out so the
   *  review ports stay unique per project. */
  setupBoardModel: base.base + 34,
  /** the packaged binary the setup-board walk drives (setup-board.spec.ts,
   *  docs/design/plum-peach plan.md Phase 1 item 4): the onboarding agent
   *  (scripted gateway) plus the speech design role at the scripted TTS
   *  instance, so the board's voicing transition runs through the real
   *  audition job and the real StageObserver. */
  setupBoard: base.base + 35,
  /** the packaged binary the Traces screen drives (traces.spec.ts, ADR-0141):
   *  a replay session with the speech roles at the scripted TTS instance and
   *  the agent_reasoning role at the scripted assistant gateway
   *  (web/e2e/fake-assistant.mjs). Same "just past the band" placement as the
   *  review ports above, one step further out so the review ports stay
   *  unique per project. */
  traces: base.base + 36,
  /** the scripted in-app assistant gateway (web/e2e/fake-assistant.mjs) the
   *  traces server's agent_reasoning role speaks to: the UX-05 wrong-speaker
   *  flow (find_similar_lines → apply_speaker_fix behind the confirmation
   *  gate) with a synthetic, labelled-stub decision model. */
  fakeAssistant: base.base + 37,
  /** the packaged binary the settings-and-theme walk drives
   *  (settings-theme.spec.ts, plan.md Phase 1 item 9): the tailnet URL pin,
   *  the labelled-synthetic audio seam (HDMI/USB/Bluetooth sink categories)
   *  and the labelled-synthetic Bluetooth controller. Like the redesign port,
   *  it lives past the 30-port band. base+31/32 are the library and
   *  ksearch review ports, so the walk takes the next free offsets.
   *  */
  theme: base.base + 38,
  /** the packaged binary the same walk drives WITHOUT the tailnet pin, its
   *  PATH scrubbed of the tailscale CLI (GN_E2E_NO_TAILSCALE): the spec's
   *  not-configured-state assertion must hold whether or not the host has a
   *  running tailscaled. */
  themeNotCfg: base.base + 39,
  /** the packaged binary the Plum & Peach Play page walk drives
   *  (play-pc.spec.ts, docs/design/plum-peach 2e/2f/2g, ADR-0093): the replay
   *  allowlist admits the repo's testdata, the speech roles point at the
   *  scripted TTS instance and the capture seam stands in for the dongle, so
   *  the now-speaking / operator / kid views all run against a live session.
   *  Lives past the redesign port, the way the Vite dev port does. Same
   *  "just past the band" placement as the review ports above, one step
   *  further out so the review ports stay unique per project. */
  playPc: base.base + 40,
  /** the packaged binary the G12 second-game walk drives (second-game.spec.ts,
   *  ADR-0079 / issue #33): the committed bench-synthetic-002 profile imported
   *  through the API, the committed black-water-001 clip replayed through the
   *  real pipeline, asserted on the Library / Regions / developer-inspector
   *  pages. Same "just past the band" placement as the review ports above,
   *  one step further out so the review ports stay unique per project. */
  secondGame: base.base + 41,
} as const;
