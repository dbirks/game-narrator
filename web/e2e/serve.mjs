#!/usr/bin/env node
// Start one Game Narrator server for Playwright against a throwaway data dir.
//
// Why this exists: `serve` without `--data-dir` uses the real appliance location
// (~/.local/share/game-narrator — see dataDir() in internal/bootstrap), and the
// webServer commands this repository used to configure did exactly that, so every
// UI test wrote into the developer's own database. The launcher owns a fresh
// directory under os.tmpdir(), records it where the tests can read it, gives the
// server a temporary HOME as well (so nothing that resolves `~` can reach the
// real one), and deletes the directory when the server exits.
//
// usage: node web/e2e/serve.mjs <name> <host:port> <command...>

import { spawn } from "node:child_process";
import { createWriteStream } from "node:fs";
import { chmod, mkdir, mkdtemp, readdir, rm, stat, writeFile } from "node:fs/promises";
import { homedir, tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const webRoot = resolve(here, "..");
const repoRoot = resolve(webRoot, "..");

const [name, listen, ...command] = process.argv.slice(2);
if (!name || !listen || command.length === 0) {
  process.stderr.write("usage: node web/e2e/serve.mjs <name> <host:port> <command...>\n");
  process.exit(2);
}

// One root per Playwright run (set by playwright.config.ts and inherited by the
// spawned shell), one directory per server: two servers on one database would
// fight over SQLite's single writer (docs/adr/0008), and a shared directory would
// make "which server wrote this row?" unanswerable.
const runRoot = join(tmpdir(), process.env.GN_E2E_RUN_ROOT ?? `game-narrator-e2e-${process.pid}`);
await mkdir(runRoot, { recursive: true });
const dataDir = await mkdtemp(join(runRoot, `${name}-`));
const homeDir = join(dataDir, "home");
await mkdir(homeDir, { recursive: true });

// The Go toolchain caches pin to the developer's real home, not the
// throwaway one: `go run` would otherwise unpack its module cache into
// $HOME/go/pkg/mod *inside this run's tree* with read-only directories
// (dr-xr-xr-x), which Node's rm refuses to unlink and which is exactly the
// state an EACCES at teardown leaves behind. The caches are build state, not
// appliance state, so sharing them is both safe and what a plain `go run`
// does; the fake HOME still protects everything the appliance writes.
const realHome = process.env.HOME ?? homedir();
const goEnv = {
  GOMODCACHE: join(realHome, "go", "pkg", "mod"),
  GOCACHE: join(realHome, ".cache", "go-build"),
};

// The proof the isolation spec needs: what the *real* data directory looked like
// the instant before this server was allowed to exist.
const realDataDir = join(
  process.env.XDG_DATA_HOME ?? join(homedir(), ".local", "share"),
  "game-narrator",
);
const before = await snapshot(realDataDir);

const manifestDir = join(webRoot, "test-results", "e2e-runs");
await mkdir(manifestDir, { recursive: true });
// The server's own log, beside the manifest: a webServer whose stderr only
// reaches Playwright's pipe loses its evidence when the run is interrupted
// (a killed orchestrator session, a model outage), which is exactly when the
// log is needed. Every line the server prints is appended here, in addition to
// the inherited stdio, for the lifetime of the process.
const logStream = createWriteStream(join(manifestDir, `${name}.log`), { flags: "a" });
const manifest = join(manifestDir, `${name}.json`);
await writeFile(
  manifest,
  JSON.stringify(
    { name, listen, command, dataDir, homeDir, realDataDir, before, pid: process.pid },
    null,
    2,
  ),
);

let cleaning = false;
const cleanup = async () => {
  if (cleaning) return;
  cleaning = true;
  await makeWritable(dataDir);
  await rm(manifest, { force: true });
  await rm(dataDir, { recursive: true, force: true });
};

/**
 * Go's module cache marks its trees read-only (directories dr-xr-xr-x), and
 * unlink needs write permission on the parent, so a plain rm -rf can leave a
 * half-deleted tree behind with an EACCES. This tree is this run's own
 * throwaway directory, so making every node writable before deleting it
 * cannot damage user state; it only keeps the guarantee in the file header.
 */
async function makeWritable(dir) {
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

const child = spawn(command[0], [...command.slice(1), "--listen", listen, "--data-dir", dataDir], {
  cwd: repoRoot,
  stdio: ["inherit", "pipe", "pipe"],
  env: {
    ...process.env,
    ...goEnv,
    HOME: homeDir,
    XDG_DATA_HOME: join(homeDir, ".local", "share"),
    GAME_NARRATOR_DATA_DIR: dataDir,
  },
});

// Tee: the inherited stdio (whatever the harness pipes or ignores) and the
// durable per-server log. Two consumers on one stream are legal in Node.
for (const pair of [
  [child.stdout, process.stdout],
  [child.stderr, process.stderr],
]) {
  pair[0].pipe(pair[1]);
  pair[0].pipe(logStream, { end: false });
}

// Playwright starts webServer commands as a process-group leader (`detached: true`
// in playwright-core's launchProcess), so the signal it sends reaches this process
// too. Forward it and wait: the server has a drain path (net/http.Shutdown plus
// the job drain in cmd/game-narrator), and deleting its database underneath that
// drain would be its own kind of test.
for (const signal of ["SIGINT", "SIGTERM", "SIGQUIT"]) {
  process.on(signal, () => {
    if (child.exitCode === null && child.signalCode === null) {
      child.kill(signal === "SIGQUIT" ? "SIGTERM" : signal);
    }
  });
}

child.on("exit", (code, signalCode) => {
  logStream.end();
  void cleanup().finally(() => process.exit(signalCode ? 0 : (code ?? 1)));
});

child.on("error", (error) => {
  process.stderr.write(`serve.mjs ${name}: ${error.message}\n`);
  void cleanup().finally(() => process.exit(1));
});

/** Identity of a directory's entries: name + size + mtime, or absent. */
async function snapshot(dir) {
  try {
    const entries = await readdir(dir);
    const files = [];
    for (const entry of entries.sort()) {
      const info = await stat(join(dir, entry)).catch(() => null);
      files.push({ name: entry, size: info?.size ?? -1, mtimeMs: info?.mtimeMs ?? -1 });
    }
    return { exists: true, files };
  } catch {
    return { exists: false, files: [] };
  }
}
