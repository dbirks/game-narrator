import { expect, test } from "@playwright/test";

import {
  expectDefaultDataDirUntouched,
  expectIsolatedDataDir,
  serverNameForProject,
} from "./support/servers";
import { ports } from "./support/ports";

/**
 * The bug this spec exists to keep fixed: `webServer` used to start `serve` with
 * no `--data-dir`, so every UI test wrote into the developer's own
 * ~/.local/share/game-narrator — the real library, the real sessions (AGENTS.md
 * rule 5: a test may not touch user state). The config cannot express "a fresh
 * directory" on its own, so e2e/serve.mjs owns one per server and this spec
 * asserts the consequence from the outside: the database exists where that server
 * said it would, on the port this project is talking to, and the developer's own
 * directory has not moved by a single inode.
 */
test("this project's server runs against its own temporary data dir", async ({
  request,
  baseURL,
}, testInfo) => {
  test.skip(baseURL === undefined, "the project has no baseURL");
  // Where this project's API traffic lands. For the binary projects that is
  // the baseURL itself; for the dev-server project it is the Go origin behind
  // Vite's /api proxy (playwright.config.ts points the proxy there), not Vite's
  // own port.
  const origin =
    testInfo.project.name === "chromium"
      ? `http://127.0.0.1:${ports.api}`
      : new URL(baseURL as string).origin;
  const name = serverNameForProject(testInfo.project.name);

  const live = await request.get(`${origin}/api/v1/health/live`);
  expect(live.ok(), "the server under test is the one the config started").toBe(true);

  const run = await expectIsolatedDataDir(name);
  expect(
    run.listen,
    `the recorded --listen (${run.listen}) is the address this project's API traffic reaches (${origin})`,
  ).toContain(new URL(origin).port);

  expectDefaultDataDirUntouched(run);
});
