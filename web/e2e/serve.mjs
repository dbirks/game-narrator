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
import { createWriteStream, readFileSync } from "node:fs";
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

// Provider-configuration hermeticity: Playwright merges this webServer's
// `env` block into the launcher's process.env before it spawns it, so the
// only reliable way to tell "the project declared this variable" from "the
// developer's shell exported it" is to re-read the config this webServer
// entry came from and collect the GAME_NARRATOR_* names the entry declares
// for this server. The child gets its inherited environment minus every
// other GAME_NARRATOR_* variable, so a spec's expectations about model-role
// states never depend on the host's live configuration (the settings page
// rendered the developer's own Clef/ASR/TTS wiring otherwise). Declared
// names keep their project values, which are already in process.env.
function declaredGameNarratorVars(name) {
  const cfgText = readFileSync(join(webRoot, "playwright.config.ts"), "utf8");
  const marker = `serve.mjs ${name} `;
  const start = cfgText.indexOf(marker);
  if (start === -1) return [];
  const rest = cfgText.slice(start + marker.length);
  // The entry ends where the next webServer command begins.
  const nextCommand = rest.indexOf("command:");
  const section = nextCommand === -1 ? rest : rest.slice(0, nextCommand);
  const envAt = section.indexOf("env:");
  if (envAt === -1) return [];
  const openAt = section.indexOf("{", envAt);
  if (openAt === -1) return [];
  // Brace-depth scan: template expressions like ${ports.x} are balanced,
  // so the scan stops at the env block's own closing brace.
  let depth = 0;
  let closeAt = -1;
  for (let i = openAt; i < section.length; i++) {
    const ch = section[i];
    if (ch === "{") depth++;
    else if (ch === "}") {
      depth--;
      if (depth === 0) {
        closeAt = i;
        break;
      }
    }
  }
  const block = closeAt === -1 ? section.slice(openAt) : section.slice(openAt, closeAt + 1);
  return [...new Set(block.match(/GAME_NARRATOR_[A-Z0-9_]+/g) ?? [])];
}

// The proof the isolation spec needs: what the *real* data directory looked like
// the instant before this server was allowed to exist.
const realDataDir = join(
  process.env.XDG_DATA_HOME ?? join(homedir(), ".local", "share"),
  "game-narrator",
);
const before = await snapshot(realDataDir);

// Scrub the inherited provider configuration before the manifest is written,
// keeping only the variables this server's project declares (names recorded,
// never values): the manifest records what the scrub did so a spec can assert
// the server's provider environment is exactly the project's, not the
// developer's.
const declaredProviderVars = declaredGameNarratorVars(name);
const childEnv = { ...process.env, ...goEnv };
const scrubbedProviderVars = [];
for (const key of Object.keys(childEnv)) {
  if (!key.startsWith("GAME_NARRATOR_")) continue;
  if (declaredProviderVars.includes(key)) continue;
  delete childEnv[key];
  scrubbedProviderVars.push(key);
}

// GN_E2E_NO_TAILSCALE (ADR-0109): the settings page's network section probes
// `tailscale` on the child's PATH, and a developer box that is on a tailnet
// would otherwise render a live, host-specific URL into a spec that asserts
// URL-free or not-configured settings DOMs. The child's PATH is prepended
// with this run's own `tailscale` shim (a no-output exit 127): the probe
// finds the shim first and answers its honest "did not answer" not-configured
// note without touching the real CLI, while the rest of the PATH — the
// toolchain a `go run` server needs — stays intact (the shim and the real
// CLI can share a directory, so a PATH filter cannot express this).
if (process.env.GN_E2E_NO_TAILSCALE === "1") {
  const shimDir = join(dataDir, "bin");
  await mkdir(shimDir, { recursive: true });
  const shim = join(shimDir, "tailscale");
  await writeFile(shim, "#!/bin/sh\nexit 127\n");
  await chmod(shim, 0o700);
  childEnv.PATH = `${shimDir}:${childEnv.PATH ?? ""}`;
}

// The per-run manifest + server log directory (ADR-0094): under this run's
// root, not the checkout's web/test-results — concurrent Playwright runs in
// the same checkout must not read or delete each other's records. e2e/
// support/servers.ts derives the identical path from the same environment
// variable (with the same fallback) to find the manifests.
const manifestDir = join(runRoot, "e2e-runs");
await mkdir(manifestDir, { recursive: true });
// The server's own log, beside the manifest: a webServer whose stderr only
// reaches Playwright's pipe loses its evidence when the run is interrupted
// (a killed orchestrator session, a model outage), which is exactly when the
// log is needed. Every line the server prints is appended here, in addition to
// the inherited stdio, for the lifetime of the process.
const logStream = createWriteStream(join(manifestDir, `${name}.log`), {
  flags: "a",
});
const manifest = join(manifestDir, `${name}.json`);
await writeFile(
  manifest,
  JSON.stringify(
    {
      name,
      listen,
      command,
      dataDir,
      homeDir,
      realDataDir,
      before,
      pid: process.pid,
      // What the provider-configuration scrub did: the names the project
      // declared for this server (kept, with the project's values) and the
      // names inherited from the developer's shell (dropped). Names only —
      // a manifest must never carry a credential (AGENTS.md rule 5).
      env: { declared: declaredProviderVars, scrubbed: scrubbedProviderVars },
    },
    null,
    2,
  ),
);

let cleaning = false;
// The restore walk's launcher opts out of the directory deletion (the spec
// restarts the appliance itself against this same data directory — its
// replacement process is the directory's owner for the rest of the walk, and
// a deletion racing it would unlink the plan, the pre-restore copy and the
// database under the process that must answer the post-restart reads). The
// per-run teardown still removes the whole run root when the run ends, so
// nothing accumulates either way: the two cleanups are deliberately
// redundant, and this flag only stops the early one (ADR-0090's restart-
// with-swap is the walk's point).
const keepDataDir = process.env.GN_E2E_KEEP_DATA_DIR === "1";
const cleanup = async () => {
  if (cleaning) return;
  cleaning = true;
  if (!keepDataDir) {
    await makeWritable(dataDir);
    await rm(dataDir, { recursive: true, force: true });
  }
  await rm(manifest, { force: true });
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
    ...childEnv,
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
      files.push({
        name: entry,
        size: info?.size ?? -1,
        mtimeMs: info?.mtimeMs ?? -1,
      });
    }
    return { exists: true, files };
  } catch {
    return { exists: false, files: [] };
  }
}
