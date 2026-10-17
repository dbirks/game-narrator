import { existsSync, readFileSync, readdirSync, statSync } from "node:fs";
import { homedir } from "node:os";
import { join, resolve } from "node:path";
import { expect, type APIRequestContext } from "@playwright/test";

/**
 * The handle on the server a spec is talking to.
 *
 * `e2e/serve.mjs` starts every webServer in playwright.config.ts with a data
 * directory it created under os.tmpdir() and records it in
 * test-results/e2e-runs/<name>.json. Specs read that record for two jobs: finding
 * the asset store the server actually wrote to, and proving — as an assertion, not
 * as a configuration value — that the appliance under test is not the developer's
 * own ~/.local/share/game-narrator.
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
}

export interface Snapshot {
  exists: boolean;
  files: { name: string; size: number; mtimeMs: number }[];
}

const webRoot = resolve(import.meta.dirname, "..", "..");
const runsDir = join(webRoot, "test-results", "e2e-runs");

export function serverRun(name: string): ServerRun {
  const file = join(runsDir, `${name}.json`);
  if (!existsSync(file)) {
    throw new Error(
      `no e2e run manifest at ${file}: the server was not started by web/e2e/serve.mjs, ` +
        "so nothing here knows which data directory it owns. That is the configuration bug " +
        "that let UI tests write into the developer's own ~/.local/share/game-narrator.",
    );
  }
  return JSON.parse(readFileSync(file, "utf8")) as ServerRun;
}

/** The appliance files must exist under the temporary directory. */
export async function expectIsolatedDataDir(name: string): Promise<ServerRun> {
  const run = serverRun(name);
  expect(
    run.dataDir.startsWith(resolve(process.env.TMPDIR ?? "/tmp")),
    `${run.dataDir} is inside the OS temp root, not the user's data directory`,
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
    default:
      throw new Error(`playwright project "${project}" has no webServer in playwright.config.ts`);
  }
}

/** The origin of a project's baseURL, for building absolute API URLs. */
export function originOf(baseURL: string | undefined): string {
  if (!baseURL) throw new Error("the project has no baseURL");
  return new URL(baseURL).origin;
}
