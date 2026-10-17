import { chmod, readdir, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

/**
 * Removes this run's tree under os.tmpdir(). Playwright runs a global teardown
 * before it stops the `webServer` processes, so each `e2e/serve.mjs` also deletes
 * its own directory after the server it spawned has exited — the two are
 * deliberately redundant, because a test run that is interrupted should not
 * accumulate databases in /tmp either.
 *
 * An interrupted run is exactly the case where read-only trees survive: the Go
 * toolchain marks module-cache trees dr-xr-xr-x, and unlink needs write
 * permission on the parent. e2e/serve.mjs keeps this run's HOME free of those
 * by pinning GOMODCACHE/GOCACHE at the developer's real home, but a teardown
 * that rm's the run root must not assume it either — so the tree is made
 * writable first (it is this run's own throwaway directory).
 */
async function makeWritable(dir: string): Promise<void> {
  const entries = await readdir(dir, { withFileTypes: true }).catch(() => []);
  for (const entry of entries) {
    const path = join(dir, entry.name);
    if (entry.isDirectory() && !entry.isSymbolicLink()) await makeWritable(path);
    await chmod(path, entry.isDirectory() && !entry.isSymbolicLink() ? 0o700 : 0o600).catch(
      () => {},
    );
  }
  await chmod(dir, 0o700).catch(() => {});
}

export default async function globalTeardown(): Promise<void> {
  const runRoot = process.env.GN_E2E_RUN_ROOT;
  if (!runRoot) return;
  const path = join(tmpdir(), runRoot);
  await makeWritable(path);
  await rm(path, { recursive: true, force: true });
}
