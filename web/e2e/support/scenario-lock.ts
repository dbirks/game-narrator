import { closeSync, existsSync, openSync, readFileSync, unlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import type { TestInfo } from "@playwright/test";

/**
 * The suite's shared scripted model gateway (web/e2e/fake-model.mjs) holds ONE
 * global scenario, set with POST /_scenario by the specs whose servers point
 * the agent_reasoning role at it (onboarding, library, agent-dock — the
 * agent-dock config entry names the contract: "Its scenario is global, so
 * each spec sets its own before the first chat request and the one-worker
 * run keeps the specs serial").
 *
 * That contract holds only while the scenario-dependent walks run serially.
 * `fullyParallel: true` with more than one worker lets one walk's
 * setScenario land inside another walk's chat conversation, and the agent
 * answers from the wrong script (observed: the onboarding import walk's
 * timeline carrying the malicious-page scenario's refusal, and the walk
 * never calling import_user_asset).
 *
 * This lock serialises the scenario-dependent walks across the run's
 * workers: one file under the run's shared root (`<os.tmpdir()>/
 * <GN_E2E_RUN_ROOT>/` — the same root serve.mjs's manifests and the specs'
 * data dirs live in, ADR-0094): the config sets GN_E2E_RUN_ROOT once per
 * runner process and every worker and server of the run inherits it, so the
 * file is unique per Playwright run and visible to every worker (the run's
 * output dir, by contrast, is re-derived per process and would lock only
 * within one worker). Two concurrent runs in the same checkout carry
 * distinct run roots — and thus distinct gateways — so they never contend
 * on each other's lock.
 *
 * Scope: the holder acquires at the test's start (before the appliance
 * claim — the onboarding walks share one run appliance, and the same
 * parallelism contests its once-per-run claim) and keeps the lock until the
 * test ends (the run's chat conversation spans the walk).
 *
 * ADR-0120.
 */

const LOCK_NAME = ".scenario-lock.json";
/**
 * How long an acquirer waits for a live holder. Generous against the
 * scenario walks' real durations (the scripted model and the job queue make
 * them seconds-to-minutes); the holder's lease, not this, is the crash
 * bound.
 */
const WAIT_MS = 150_000;
/**
 * A holder's maximum hold. Larger than the scenario walks' longest test
 * timeout (the library walk's 600 s), so a timed-out holder unblocks the
 * next walk by expiry rather than dead-locking the run.
 */
const LEASE_MS = 660_000;
const POLL_MS = 250;

interface LockRecord {
  pid: number;
  scenario: string;
  test: string;
  acquiredAt: number;
  expiresAt: number;
}

/**
 * Acquires the scenario lock and returns the release function. The release
 * must run in a `finally` so a failing walk does not pin the gateway for
 * the rest of the run. Throws if the lock is still held by a live, unexpired
 * holder after WAIT_MS — the caller's test then fails with the lock's path
 * (whose JSON names the holder's pid, scenario and test title) instead of a
 * wrong-scenario agent walk.
 */
export async function acquireScenarioLock(
  testInfo: TestInfo,
  scenario: string,
): Promise<() => void> {
  const path = lockPath();

  const record: LockRecord = {
    pid: process.pid,
    scenario,
    test: testInfo.title,
    acquiredAt: 0,
    expiresAt: 0,
  };

  const deadline = Date.now() + WAIT_MS;
  for (;;) {
    const existing = readLock(path);
    if (!existing || Date.now() > existing.expiresAt) {
      // Steal-or-claim: the exclusive open is the atomic step; a lost race
      // means another worker claimed between the read and the open.
      try {
        record.acquiredAt = Date.now();
        record.expiresAt = record.acquiredAt + LEASE_MS;
        const fd = openSync(path, "wx");
        try {
          writeFileSync(fd, JSON.stringify(record, null, 2));
        } finally {
          closeSync(fd);
        }
        return releaseLock(path, record);
      } catch (error) {
        if ((error as NodeJS.ErrnoException).code !== "EEXIST") throw error;
      }
    }
    if (Date.now() > deadline) {
      const holder = readLock(path);
      throw new Error(
        `scenario lock: waited ${WAIT_MS} ms for the scenario walk held by ` +
          `${holder ? `pid ${holder.pid} (scenario "${holder.scenario}", test "${holder.test}", expires ${new Date(holder.expiresAt).toISOString()})` : "another worker"}; ` +
          `see ${path}. The shared fake-model gateway's global scenario is ` +
          `only safe while the scenario-dependent walks are serial (ADR-0120).`,
      );
    }
    await new Promise((resolve) => setTimeout(resolve, POLL_MS));
  }
}

function lockPath(): string {
  // The run's shared root, exactly as e2e/support/servers.ts and
  // e2e/serve.mjs resolve it (the config always sets the variable; the
  // fallback mirrors theirs for a direct spec import).
  const runRootName = process.env.GN_E2E_RUN_ROOT ?? `game-narrator-e2e-${process.pid}`;
  return join(tmpdir(), runRootName, LOCK_NAME);
}

function readLock(path: string): LockRecord | null {
  try {
    if (!existsSync(path)) return null;
    return JSON.parse(readFileSync(path, "utf8")) as LockRecord;
  } catch {
    // An unreadable lock is a crashed writer: treat it as absent so the
    // next acquirer re-claims (the lease still bounds any real holder).
    return null;
  }
}

/**
 * Removes the lock only if this holder still owns it (a re-claim by another
 * worker after this holder's lease expired must not be unlinked by the late
 * holder's finally).
 */
function releaseLock(path: string, record: LockRecord): () => void {
  return () => {
    const current = readLock(path);
    if (current && current.pid === record.pid && current.acquiredAt === record.acquiredAt) {
      try {
        unlinkSync(path);
      } catch {
        // A torn unlink is the next acquirer's to notice; the lease bounds it.
      }
    }
  };
}
