// The ports the e2e servers listen on, in one place. playwright.config.ts
// reads them to start the servers; the specs read them to assert which server
// a project is actually talking to. A port that lives in only one of the two
// is the config/spec drift e2e/isolation.spec.ts exists to catch.
//
// Every port is ONE BASE plus a stable per-server offset, so several
// Playwright runs in the same checkout can share the box: each run takes a
// different base and the offsets keep the wiring (config commands <-> specs
// <-> webServer env blocks) intact. The default base 18090 reproduces the
// historical fixed ports (18090-18119 plus the Vite dev port 5173) exactly,
// so a run that does not set GN_E2E_PORT_BASE behaves exactly like before
// (CI and the docs are unchanged).
//
// GN_E2E_PORT_BASE (read at config load, before the webServer commands and
// the workers start):
//   (unset)  the default base, 18090.
//   <number> that base, used as-is — an explicit base is an operator choice,
//            so it is validated and never probed.
//   auto     probe for a free band at config load: the first candidate base
//            (18090, 18190, 18290, ...) whose whole band (base .. base+29)
//            and whose dev-server port (base + WEB_PORT_OFFSET) answer no
//            connection wins, and is then CLAIMED atomically (a mkdir lock
//            under os.tmpdir(), so two auto runs that probe the same instant
//            cannot take the same band). The resolved base is written back
//            to process.env as a number, so the webServer commands, the
//            serve.mjs launchers and the worker processes all agree.
//
// The candidate bands (18090 + k*100) deliberately never enter 18120-18139,
// the band the Go live-play test owns (internal/e2e livePlayPortCandidates).
import { existsSync, mkdirSync, readFileSync, rmSync, statSync, writeFileSync } from "node:fs";
import { createConnection } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";

export const DEFAULT_PORT_BASE = 18090;
/** The contiguous band one run owns: base .. base + PORT_BAND - 1. The
 *  default band is 18090-18119 — 18120-18139 belongs to the Go live-play
 *  test (internal/e2e livePlayPortCandidates), so the default band stops
 *  exactly where that one starts. */
export const PORT_BAND = 30;
/** The Vite dev server's historical port 5173, as an offset from the base
 *  (5173 - 18090 = -12917): the default base reproduces 5173, and when the
 *  base moves the dev-server port moves by the same delta (the first auto
 *  step: 5173 -> 5273), so a second concurrent run owns its own Vite instead
 *  of sharing — or failing strictPort: on — the first run's. */
export const WEB_PORT_OFFSET = 5173 - DEFAULT_PORT_BASE;
/** auto: how many candidate bands to probe before giving up. */
const AUTO_CANDIDATES = 10;
/** auto: the stride between candidate bases (wider than PORT_BAND, so two
 *  concurrent auto runs never pick overlapping bands). */
const AUTO_STRIDE = 100;
/** auto: how long a single probe connection may take before it is treated
 *  as "in use" (conservative: an unknown answer must not become a collision). */
const PROBE_TIMEOUT_MS = 300;
/** auto: how long a band claim may outlive its holder process before it is
 *  reclaimed (a crashed run releases the claim sooner via the liveness check
 *  on the holder pid recorded in the lock directory). */
const BAND_LOCK_TTL_MS = 2 * 60 * 60 * 1000;

/** A loopback port is free when a connect attempt is refused. */
function isPortFree(port: number): Promise<boolean> {
  return new Promise((resolve) => {
    const socket = createConnection({ host: "127.0.0.1", port });
    let settled = false;
    const finish = (free: boolean) => {
      if (settled) return;
      settled = true;
      socket.destroy();
      resolve(free);
    };
    socket.once("connect", () => finish(false)); // something answers: in use
    socket.once(
      "error",
      (error: NodeJS.ErrnoException) => finish(error.code === "ECONNREFUSED"), // anything else: treat as in use
    );
    socket.setTimeout(PROBE_TIMEOUT_MS, () => finish(false));
  });
}

/** Every port the run would bind for this base is free. */
async function bandFree(base: number): Promise<boolean> {
  const ports: number[] = [];
  for (let offset = 0; offset < PORT_BAND; offset++) ports.push(base + offset);
  ports.push(base + WEB_PORT_OFFSET, base + 30); // the dev port and the redesign review port
  return (await Promise.all(ports.map(isPortFree))).every(Boolean);
}

function assertValidBase(base: number): void {
  if (!Number.isInteger(base) || base < 1024) {
    throw new Error(`GN_E2E_PORT_BASE: ${String(base)} is not a valid port`);
  }
  const webPort = base + WEB_PORT_OFFSET;
  if (webPort < 1024) {
    throw new Error(
      `GN_E2E_PORT_BASE: ${String(base)} would put the dev-server port at ${webPort}, ` +
        `below the allowed range — the base must be at least ${1024 - WEB_PORT_OFFSET}`,
    );
  }
  if (base + PORT_BAND - 1 > 65535 || webPort > 65535) {
    throw new Error(
      `GN_E2E_PORT_BASE: ${String(base)} leaves no room for the ${PORT_BAND}-port band ` +
        `(and the dev-server port ${webPort})`,
    );
  }
}

function pidAlive(pid: number): boolean {
  if (!Number.isInteger(pid) || pid <= 0) return false;
  try {
    process.kill(pid, 0);
    return true;
  } catch (error) {
    return (error as NodeJS.ErrnoException).code === "EPERM"; // exists, not ours
  }
}

/**
 * Atomically claim a candidate band for this run. The probe above only says
 * "nobody answers right now" — two auto runs that probe at the same instant
 * both see a free band, so the claim itself is a mkdir race (atomic on
 * POSIX): the second claimer gets EEXIST and moves on to the next candidate.
 * A leftover claim from a crashed run is reclaimed when the holder process
 * it records is gone or the claim is older than the TTL.
 */
function claimBand(base: number): string | null {
  const lockDir = join(tmpdir(), `gn-e2e-portband-${base}.lock`);
  if (existsSync(lockDir)) {
    let stale = true;
    try {
      const holder = Number(readFileSync(join(lockDir, "holder"), "utf8"));
      stale = Date.now() - statSync(lockDir).mtimeMs > BAND_LOCK_TTL_MS || !pidAlive(holder);
    } catch {
      // an unreadable holder cannot prove liveness: reclaim
    }
    if (!stale) return null;
    try {
      rmSync(lockDir, { recursive: true, force: true });
    } catch {
      return null;
    }
  }
  try {
    mkdirSync(lockDir);
  } catch {
    return null; // someone else claimed it between the check and the mkdir
  }
  try {
    writeFileSync(join(lockDir, "holder"), String(process.pid));
  } catch {
    // best effort: the directory itself is the claim
  }
  return lockDir;
}

async function resolvePortBase(raw: string | undefined): Promise<number> {
  if (raw === undefined || raw === "") return DEFAULT_PORT_BASE;
  if (raw === "auto") {
    for (let k = 0; k < AUTO_CANDIDATES; k++) {
      const base = DEFAULT_PORT_BASE + k * AUTO_STRIDE;
      if (!(await bandFree(base))) continue;
      const lockDir = claimBand(base);
      if (lockDir !== null) {
        bandLock = lockDir;
        return base;
      }
    }
    throw new Error(
      `GN_E2E_PORT_BASE=auto: none of the candidate bands ` +
        `(${DEFAULT_PORT_BASE}..${DEFAULT_PORT_BASE + (AUTO_CANDIDATES - 1) * AUTO_STRIDE}) ` +
        `has its band, dev-server port and redesign port all free and unclaimed — set GN_E2E_PORT_BASE to a free base explicitly`,
    );
  }
  const base = Number(raw);
  assertValidBase(base);
  return base;
}

// Resolved once at module load, in the process that loaded playwright.config.ts.
// Workers inherit the resolved number through the environment, so they re-derive
// the same ports without probing or claiming.
let bandLock: string | null = null;
const base = await resolvePortBase(process.env.GN_E2E_PORT_BASE);
// Release the auto band claim when THIS process exits: the run's servers are
// being torn down by then, and the band must be claimable again by the next
// auto run. (A kill -9'd run leaves the claim behind; the next auto run
// reclaims it through the holder-pid liveness check.)
if (bandLock !== null) {
  const lock = bandLock;
  process.on("exit", () => {
    try {
      rmSync(lock, { recursive: true, force: true });
    } catch {
      // best effort: the stale-claim reclamation is the backstop
    }
  });
}
// Export the resolved base (always a number, never "auto") so every process
// this run spawns — the webServer shells, serve.mjs, the workers — reads the
// same value whether or not the operator set the variable.
process.env.GN_E2E_PORT_BASE = String(base);

export const ports = {
  /** `go run ./cmd/game-narrator serve` behind the Vite dev server. */
  api: base + 0,
  /** Vite dev server — the chromium project's baseURL. */
  web: base + WEB_PORT_OFFSET,
  /** The setup wizard's own appliance (ADR-0087 G9): the claim is once per
   *  appliance (ADR-0016 §7) and the chromium dev-server appliance is claimed
   *  by m5-settings-tests' admin, so the wizard claims a fresh packaged-binary
   *  server it owns. */
  wizard: base + 1,
  /** the packaged binary serving the embedded UI (embedded.spec.ts). */
  embedded: base + 5,
  /** the packaged binary the catalogue walk-through claims (catalog.spec.ts). */
  catalog: base + 6,
  /** the packaged binary the developer inspector drives a replay on (developer.spec.ts). */
  developer: base + 7,
  /** the packaged binary the onboarding agent runs against (onboarding.spec.ts). */
  onboarding: base + 8,
  /** the scripted model gateway + malicious fixture page (web/e2e/fake-model.mjs). */
  fakeModel: base + 9,
  /** the scripted TTS instance (web/e2e/fake-tts.mjs) the voice spec points the
   *  speech roles and the registry at. */
  fakeTTS: base + 10,
  /** the packaged binary the voice workflow drives (voices.spec.ts). */
  voices: base + 11,
  /** the packaged binary the m5-part1 walk (play / characters / publish / diagnostics)
   *  drives, with the repo's testdata admitted to the replay allowlist. */
  play: base + 12,
  /** the packaged binary the m5 graph/regions walk drives: the replay allowlist
   *  admits the repo's testdata (the preview's fixture), so OCR runs against
   *  the synthetic textbox frame. */
  graphRegions: base + 13,
  /** the packaged binary the correction-labels / regression-fixture walk drives
   *  (corrections-fixtures.spec.ts): the editor's save records the label, and
   *  the developer's fixture view reads the same rows (ADR-0052). */
  corrections: base + 14,
  /** the packaged binary the SSE reconnect walk drives (sse-reconnect.spec.ts):
   *  the replay allowlist admits the repo's testdata so /play can run the
   *  synthetic clip, and the speech roles point at the scripted TTS instance
   *  the way the play walk does (ADR-0039). */
  sse: base + 15,
  /** the packaged binary the full-curation and voice-preview walks drive
   *  (curation-walk.spec.ts, voice-preview-audio.spec.ts): the replay
   *  allowlist admits the repo's testdata (the region preview's fixture) and
   *  the speech roles point at the scripted TTS instance (fake-tts.mjs), so
   *  one server hosts both walks. */
  curation: base + 16,
  /** the packaged binary the route-coverage walk drives
   *  (route-coverage.spec.ts): a fresh appliance for the first-run →
   *  claimed → unauthenticated guard matrix. */
  routes: base + 17,
  /** the packaged binary the accessibility (axe) walk drives (a11y.spec.ts):
   *  a fresh appliance, claimed and seeded with the synthetic game so every
   *  route is visited in the state the other specs establish. */
  a11y: base + 18,
  /** the scripted System One / Clef-Omni instance (web/e2e/fake-clef.mjs) the
   *  developer-decision walk configures as the decision role. */
  fakeClef: base + 19,
  /** the packaged binary the developer-decision walk drives (developer-decision.spec.ts):
   *  the inspector's replay with the ambiguous ranking sent to the scripted
   *  Clef instance and the endpoint-down degraded case. */
  developerDecision: base + 20,
  /** the scripted ASR instance (web/e2e/fake-asr.mjs) the voices walk points
   *  the speech_asr role at, so the audition QA badges run against the
   *  transcription wire with a synthetic stub (ADR 0061). */
  fakeASR: base + 21,
  /** the packaged binary the disc-import walk drives (disc-import-walk.spec.ts,
   *  ADR-0066): a fresh appliance whose disc-import allowlist is the repo's
   *  synthetic disc (testdata/disc), so the walk imports a disc image through
   *  the UI without a user's real one. */
  disc: base + 22,
  /** the packaged binary the managed-clip-store walk drives
   *  (replay-clips-walk.spec.ts, ADR-0073): NO replay allowlist at all — the
   *  managed clip store under the data dir is the only replay root, which is
   *  the point of the walk. */
  clips: base + 23,
  /** the packaged binary the player-controls walk drives
   *  (play-controls.spec.ts, docs/spec/08 "/pause, /resume, /settings"): the
   *  replay allowlist admits the repo's testdata and the speech roles point
   *  at the scripted TTS instance, the way the sse walk does, so /play can
   *  run the synthetic clip with real audio while the controls are asserted.
   */
  playControls: base + 24,
  /** the packaged binary the audio-output walk drives (audio-output.spec.ts,
   *  ADR-0074): the test seam stands in for the enumeration backend (a
   *  labelled-synthetic device list) and the filesink records the test tone,
   *  so the walk proves the product surface without hardware. */
  audio: base + 25,
  /** the packaged binary the capture-device walk drives (capture-device.spec.ts,
   *  ADR-0078): the test seam stands in for the dongle (GAME_NARRATOR_CAPTURE_FAKE) —
   *  a labelled-synthetic device fed by a real FFmpeg testsrc2 pipe — so the
   *  Settings capture section and a full v4l2 session run without hardware. */
  capture: base + 26,
  /** the packaged binary the developer inspector's per-event actions drive
   *  (developer-actions.spec.ts, ADR-0083): a replay session with the speech
   *  roles at the scripted TTS instance (fake-tts.mjs), so the utterance's
   *  audio is real (synthetic-stub) bytes the fixture, the waveform and the
   *  evidence bundle can carry. */
  developerActions: base + 27,
  /** the packaged binary the backup → mutate → restore walk drives
   *  (restore-walk.spec.ts, ADR-0087): the walk's own server, because the
   *  restore restarts the process — killing the shared `api` server would
   *  orphan every other project's run. The spec restarts the replacement
   *  from the server manifest (the per-run e2e-runs dir, ADR-0094). */
  restore: base + 28,
  /** the scripted GitHub-releases endpoint (web/e2e/fake-releases.mjs) the
   *  restore server's update checker polls (ADR-0088): the latest release it
   *  serves is the one the Settings section must name, with its notes link.
   */
  fakeReleases: base + 29,
  /** the packaged binary the Plum & Peach shell review drives
   *  (redesign-shell.spec.ts, docs/design/plum-peach plan.md Phase 0 item 6):
   *  a fresh appliance the spec claims, screenshotting the shell and the kit
   *  gallery at the design's three widths and axing them. Lives just past the
   *  30-port band, the way the Vite dev port does — base+30 is 18120 at the
   *  default base, the first port of the Dolphin live-play band that only
   *  binds under its explicit hardware gate, which never runs inside the
   *  Playwright suite. */
  redesign: base + 30,
} as const;
