import { readdir, rm, stat } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

/**
 * Runs before any test (and, as it happens, after `webServer` has started the
 * Go processes — the temporary data directories are therefore owned by
 * `e2e/serve.mjs`, which creates them before the server it spawns can touch
 * them, rather than here).
 *
 * What belongs here is the mess a killed run left behind: a root under
 * os.tmpdir() still holding a database, an asset store and a fake HOME. Anything
 * newer than the current run is somebody else's live run, so the age test is not
 * politeness, it is correctness.
 */
const STALE_AFTER_MS = 12 * 60 * 60 * 1000;

export default async function globalSetup(): Promise<void> {
  const current = process.env.GN_E2E_RUN_ROOT ?? "";
  const entries = await readdir(tmpdir()).catch(() => [] as string[]);
  for (const entry of entries) {
    if (!entry.startsWith("game-narrator-e2e-") || entry === current) continue;
    const path = join(tmpdir(), entry);
    const info = await stat(path).catch(() => null);
    if (!info) continue;
    if (Date.now() - info.mtimeMs > STALE_AFTER_MS) {
      await rm(path, { recursive: true, force: true });
      process.stdout.write(`playwright: removed stale run directory ${path}\n`);
    }
  }
}
