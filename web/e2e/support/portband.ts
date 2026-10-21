// The per-run e2e port-band selection (ADR-0094, issue #43): one base port
// per Playwright run, every server port being base + a stable offset
// (web/e2e/support/ports.ts is the single source of truth for the offsets).
//
// A band is the base plus PORT_BAND contiguous ports (base .. base + 29),
// plus the dev-server port (base + WEB_PORT_OFFSET) and the review ports just
// past the band (base + 30 .. base + 41): every port one run would bind. The
// candidate bases (18090 + k*100) deliberately never enter 18120-18139, the
// band the Go live-play test owns (internal/e2e livePlayPortCandidates).
//
// This module holds the selection logic only. It reads no environment and
// performs no module-load work, so the unit test can resolve several "runs"
// in one process and assert they reserve disjoint bands; ports.ts applies
// it once at config load and publishes the result to the environment.
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
/** The offsets of the ports that live just past the contiguous band: the
 *  review ports in ports.ts (base+30 .. base+41, redesign through
 *  second-game). At the default base base+30..base+34 sit inside the Go
 *  live-play band, which only binds under its explicit hardware gate. */
const PAST_BAND_OFFSETS = [30, 31, 32, 33, 34, 35, 36, 37, 38, 39, 40, 41];

/** The candidate bases the auto path probes, in order. */
export function candidateBases(): number[] {
  const bases: number[] = [];
  for (let k = 0; k < AUTO_CANDIDATES; k++) bases.push(DEFAULT_PORT_BASE + k * AUTO_STRIDE);
  return bases;
}

/** The lock directory under which a claimed base is recorded: a mkdir
 *  atomic on POSIX, so two runs that probe the same instant cannot take
 *  the same band. */
export function bandLockDir(base: number): string {
  return join(tmpdir(), `gn-e2e-portband-${base}.lock`);
}

/** A loopback port is free when a connect attempt is refused. */
export function isPortFree(port: number): Promise<boolean> {
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

/** Every port the run would bind for this base: the contiguous band, the
 *  dev-server port and the past-band review ports. */
export function runPorts(base: number): number[] {
  const ports: number[] = [];
  for (let offset = 0; offset < PORT_BAND; offset++) ports.push(base + offset);
  ports.push(base + WEB_PORT_OFFSET);
  for (const offset of PAST_BAND_OFFSETS) ports.push(base + offset);
  return ports;
}

/** Every port the run would bind for this base is free. */
export async function bandFree(base: number): Promise<boolean> {
  return (await Promise.all(runPorts(base).map(isPortFree))).every(Boolean);
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
 * "nobody answers right now" — two runs that probe at the same instant
 * both see a free band, so the claim itself is a mkdir race (atomic on
 * POSIX): the second claimer gets EEXIST and moves on to the next candidate.
 * A leftover claim from a crashed run is reclaimed when the holder process
 * it records is gone or the claim is older than the TTL. Returns the lock
 * directory on success, null when the band is already claimed by a live
 * holder.
 */
export function claimBand(base: number): string | null {
  const lockDir = bandLockDir(base);
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

/** Best-effort release of a claim (the process exit path): the stale-claim
 *  reclamation is the backstop for a kill -9'd run that never gets here. */
export function releaseBand(lockDir: string): void {
  try {
    rmSync(lockDir, { recursive: true, force: true });
  } catch {
    // best effort
  }
}

export interface PortBaseResolution {
  /** The resolved base (always a number, never "auto"): every server port
   *  is this base plus its stable offset in ports.ts. */
  base: number;
  /** The band-claim lock directory when the resolution probed and claimed
   *  (the unset/auto path), null when the base is an explicit operator
   *  choice (never probed or claimed). */
  lock: string | null;
}

/**
 * Resolve the run's port base. `raw` is the GN_E2E_PORT_BASE value as read
 * from the environment:
 *   (unset or empty) — probe candidate bases 18090 + k*100 (k = 0..9): the
 *     first whose whole band, dev-server port and review ports answer no
 *     loopback connection AND is not already claimed wins, claimed atomically
 *     (a mkdir lock under os.tmpdir(), so two runs that probe the same
 *     instant cannot take the same band — issue #43: with a fixed default
 *     base two concurrent runs bound the same ports and the second run's
 *     health check was answered by the first run's appliance). The first
 *     candidate reproduces the historical fixed ports exactly (18090-18119
 *     plus the Vite dev port 5173), so a lone run behaves as before.
 *   "auto" — the same probe-and-claim.
 *   <number> — used as-is after validation (an explicit base is an operator
 *     choice, so it is never probed or claimed).
 */
export async function resolvePortBase(raw: string | undefined): Promise<PortBaseResolution> {
  if (raw === undefined || raw === "" || raw === "auto") {
    for (const base of candidateBases()) {
      if (!(await bandFree(base))) continue;
      const lock = claimBand(base);
      if (lock !== null) return { base, lock };
    }
    throw new Error(
      `GN_E2E_PORT_BASE: none of the candidate bands ` +
        `(${DEFAULT_PORT_BASE}..${DEFAULT_PORT_BASE + (AUTO_CANDIDATES - 1) * AUTO_STRIDE}) ` +
        `has its band, dev-server port and review ports all free and unclaimed — ` +
        `set GN_E2E_PORT_BASE to a free base explicitly`,
    );
  }
  const base = Number(raw);
  assertValidBase(base);
  return { base, lock: null };
}
