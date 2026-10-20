import { existsSync, readFileSync, readdirSync, statSync } from "node:fs";
import { homedir, tmpdir } from "node:os";
import { join } from "node:path";
import { expect, type APIRequestContext } from "@playwright/test";

/**
 * The handle on the server a spec is talking to.
 *
 * `e2e/serve.mjs` starts every webServer in playwright.config.ts with a data
 * directory it created under os.tmpdir() and records it in the per-run
 * manifest directory, `<os.tmpdir()>/<GN_E2E_RUN_ROOT>/e2e-runs/<name>.json`
 * (ADR-0094: one manifest set per Playwright run, so concurrent runs in the
 * same checkout never read each other's records). Specs read that record for
 * two jobs: finding the asset store the server actually wrote to, and proving
 * — as an assertion, not as a configuration value — that the appliance under
 * test is not the developer's own ~/.local/share/game-narrator.
 */
export interface ServerRun {
  name: string;
  listen: string;
  command: string[];
  dataDir: string;
  homeDir: string;
  realDataDir: string;
  before: Snapshot;
  pid: number;
  /** Provider-env scrub record; empty object on manifests written before the scrub landed. */
  env?: ProviderEnv;
}

export interface Snapshot {
  exists: boolean;
  files: { name: string; size: number; mtimeMs: number }[];
}

/**
 * What the provider-configuration scrub did to the server's environment:
 * `declared` are the GAME_NARRATOR_* variables the project declares for this
 * server (the child receives them, with the project's values) and `scrubbed`
 * the names inherited from the developer's shell that the child must not see.
 * Names only — a manifest must never carry a credential (AGENTS.md rule 5).
 */
export interface ProviderEnv {
  declared: string[];
  scrubbed: string[];
}

// The run root playwright.config.ts exports (one per Playwright process):
// the manifests and their server logs live under it, next to the data dirs,
// so two concurrent runs in the same checkout never share or wipe each
// other's records. The fallback mirrors e2e/serve.mjs's exactly (the config
// always sets the variable; the mirror keeps a direct spec import honest).
const runRootName = process.env.GN_E2E_RUN_ROOT ?? `game-narrator-e2e-${process.pid}`;
const runsDir = join(tmpdir(), runRootName, "e2e-runs");

export function serverRun(name: string): ServerRun {
  const file = join(runsDir, `${name}.json`);
  if (!existsSync(file)) {
    throw new Error(
      `no e2e run manifest at ${file}: the server was not started by web/e2e/serve.mjs ` +
        `for this run (GN_E2E_RUN_ROOT=${runRootName}), so nothing here knows which data ` +
        "directory it owns. That is the configuration bug that let UI tests write into " +
        "the developer's own ~/.local/share/game-narrator.",
    );
  }
  return JSON.parse(readFileSync(file, "utf8")) as ServerRun;
}

/** The appliance files must exist under THIS run's temporary root. */
export async function expectIsolatedDataDir(name: string): Promise<ServerRun> {
  const run = serverRun(name);
  expect(
    run.dataDir.startsWith(join(tmpdir(), runRootName)),
    `${run.dataDir} is inside this run's root under the OS temp root, not another run's ` +
      `or the user's data directory`,
  ).toBe(true);
  await expect
    .poll(() => existsSync(join(run.dataDir, "game-narrator.sqlite")), {
      message: `serve ran its migrations into ${run.dataDir}`,
    })
    .toBe(true);
  return run;
}

export function defaultDataDirSnapshot(run: ServerRun): Snapshot {
  const dir = run.realDataDir || join(homedir(), ".local", "share", "game-narrator");
  if (!existsSync(dir)) return { exists: false, files: [] };
  return {
    exists: true,
    files: readdirSync(dir)
      .sort()
      .map((name) => {
        const info = statSync(join(dir, name));
        return { name, size: info.size, mtimeMs: info.mtimeMs };
      }),
  };
}

/**
 * The provider-configuration hermeticity assertion: the server must not have
 * inherited any GAME_NARRATOR_* variable the spec's own process has, except
 * the ones the project declares for it. The launcher records both lists in
 * the manifest; a variable that is neither declared nor scrubbed would have
 * reached the server unrecorded — the state that let a spec's expectations
 * depend on the developer's live Clef/ASR/TTS wiring.
 */
export function assertHermeticProviderEnv(name: string): ProviderEnv {
  const run = serverRun(name);
  const env = run.env ?? { declared: [], scrubbed: [] };
  const inherited = Object.keys(process.env).filter((key) => key.startsWith("GAME_NARRATOR_"));
  const unrecorded = inherited.filter(
    (key) => !env.declared.includes(key) && !env.scrubbed.includes(key),
  );
  expect(
    unrecorded,
    `these GAME_NARRATOR_* variables are set in the spec's environment but the "${name}" manifest records neither keeping nor dropping them: ${unrecorded.join(
      ", ",
    )} — the server may have inherited them`,
  ).toEqual([]);
  return env;
}

/**
 * Nothing a test run did may be visible in the developer's own appliance
 * directory. The comparison is against the snapshot serve.mjs took immediately
 * before spawning the process under test, so it covers the whole window in which
 * that server existed — including the migration it ran on the way up.
 */
export function expectDefaultDataDirUntouched(run: ServerRun): void {
  expect(
    defaultDataDirSnapshot(run),
    `${run.realDataDir} must be exactly as it was before "${run.name}" started`,
  ).toEqual(run.before);
}

/**
 * A request made *as the browser is*, for the cases whose whole point is that the
 * page did not do it: a second curator's write landing while an editor is open.
 * The session cookie is already in the context's jar; the CSRF token comes from
 * the non-HttpOnly twin cookie, which is how the SPA reads it too (ADR-0016 §4).
 */
export function apiAsUser(request: APIRequestContext, origin: string) {
  const csrf = async () =>
    (await request.cookies(origin)).find((cookie) => cookie.name === "gn_csrf")?.value ?? "";

  return {
    get: (path: string) => request.get(`${origin}${path}`),
    post: (
      path: string,
      options: { data?: unknown; etag?: string; headers?: Record<string, string> } = {},
    ) =>
      csrf().then((token) =>
        request.post(`${origin}${path}`, {
          data: options.data,
          headers: {
            ...(options.data === undefined ? {} : { "content-type": "application/json" }),
            "x-csrf-token": token,
            ...(options.etag ? { "if-match": options.etag } : {}),
            ...options.headers,
          },
        }),
      ),
    patch: (path: string, data: unknown, etag: string) =>
      csrf().then((token) =>
        request.patch(`${origin}${path}`, {
          data,
          headers: { "content-type": "application/json", "x-csrf-token": token, "if-match": etag },
        }),
      ),
  };
}

/** The revision token a fresh page would have read. */
export async function currentProfileEtag(
  request: APIRequestContext,
  origin: string,
  profileId: string,
) {
  const response = await request.get(`${origin}/api/v1/profiles/${profileId}`);
  expect(response.ok(), `GET /api/v1/profiles/${profileId} answered 2xx`).toBe(true);
  const etag = response.headers()["etag"];
  expect(etag, "a profile read carries an ETag").toBeTruthy();
  return etag as string;
}

/** The game id and profile id in the current URL, for a spec that navigated by clicking. */
export function idsFromUrl(url: string): { gameId: string; profileId: string | null } {
  const parsed = new URL(url);
  const game = /\/games\/([^/]+)\//.exec(parsed.pathname);
  return { gameId: game ? game[1] : "", profileId: parsed.searchParams.get("profile") };
}

/**
 * Which serve.mjs instance a project is talking to. The mapping lives next to the
 * config's projects rather than in each spec, so a new project cannot accidentally
 * assert isolation against somebody else's data directory.
 */
export function serverNameForProject(project: string): string {
  switch (project) {
    case "chromium":
      return "api";
    case "embedded":
      return "embedded";
    case "catalog":
      return "catalog";
    case "developer":
      return "developer";
    case "onboarding":
      return "onboarding";
    case "voices":
      return "voices";
    case "play":
      return "play";
    case "graph-regions":
      return "graph-regions";
    case "corrections":
      return "corrections";
    case "sse":
      return "sse";
    case "curation":
    case "voice-preview":
      return "curation";
    case "routes":
      return "routes";
    case "a11y":
      return "a11y";
    case "redesign":
      return "redesign";
    case "library":
      return "library";
    case "ksearch":
      return "ksearch";
    case "agent-dock":
      return "agent-dock";
    case "developer-decision":
      return "developer-decision";
    case "developer-actions":
      return "developer-actions";
    case "traces":
      return "traces";
    case "settings-restore":
      return "restore";
    case "disc":
      return "disc";
    case "clips":
      return "clips";
    case "play-controls":
      return "play-controls";
    case "audio":
      return "audio";
    case "setup-board":
      return "setup-board";
    case "health":
      return "health";
    default:
      throw new Error(`playwright project "${project}" has no webServer in playwright.config.ts`);
  }
}

/** The origin of a project's baseURL, for building absolute API URLs. */
export function originOf(baseURL: string | undefined): string {
  if (!baseURL) throw new Error("the project has no baseURL");
  return new URL(baseURL).origin;
}
