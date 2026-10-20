import { expect, test } from "@playwright/test";
import { spawn, type ChildProcess } from "node:child_process";
import { createWriteStream } from "node:fs";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";

import { trackConsoleErrors } from "./support/console";
import { ports } from "./support/ports";
import { serverRun } from "./support/servers";

/**
 * The backup → mutate → restore walk (ADR-0090), with the update
 * notification's surface (ADR-0088), on the walk's own packaged-binary
 * server (playwright.config.ts project `settings-restore`, port
 * ports.restore):
 *
 *   1. claim the throwaway appliance and sign in;
 *   2. /settings' update section: the scripted releases endpoint
 *      (web/e2e/fake-releases.mjs) serves v9.9.9 — the section names it and
 *      links its notes after the manual re-check, and the endpoint's own
 *      request counter proves the check reached it; "no auto-update" holds;
 *   3. the backup download is a real SQLite file (16-byte header asserted
 *      on the bytes);
 *   4. a mutation is made visible: a new game in the library;
 *   5. the restore upload: the file's hash and size are shown before the
 *      confirm button, and the 202 repeats the staged hash and the
 *      pre-restore copy's name;
 *   6. the appliance shuts down to apply the plan (the restarter runs the
 *      normal graceful shutdown; with no supervisor the walk restarts the
 *      replacement itself from the server manifest — the same command, the
 *      same data directory, the same environment the manifest's launcher used;
 *      the launcher's own exit-cleanup must not delete that directory first —
 *      its GN_E2E_KEEP_DATA_DIR flag keeps the per-run teardown the backstop,
 *      the replacement process is the directory's owner for the rest of the walk);
 *   7. after the restart the plan is APPLIED: /settings' banner says so
 *      with the schema movement, and the mutation is gone from /library.
 *
 * The walk's binary is a packaged build; the walk asserts what a real
 * deployment shows. The comparison's honesty on a dev-stamped build and
 * every server-side refusal is the Go tests' business (internal/updates,
 * internal/restore, internal/api/restore_test.go).
 */

const ADMIN = { username: "rw-admin", password: "steady as the tide" };
const MUTATION = { slug: "restore-walk-mutation", title: "Restore Walk Mutation" };
const FAKE_RELEASE_TAG = "v9.9.9";
const FAKE_STATS_URL = `http://127.0.0.1:${ports.fakeReleases}/_stats`;
// The repo root the replacement is spawned from: the spec runs with the web
// directory as its cwd (playwright's rootDir), one level up.
const REPO_ROOT = resolve("..");

const readManifest = async () => serverRun("restore");

const fetchJson = async (url: string) =>
  (await (await fetch(url)).json()) as Record<string, unknown>;

const stats = async (): Promise<number> =>
  Number((await fetchJson(FAKE_STATS_URL)).releases_requests ?? 0);

test("restore walk: backup, mutate, restore, state visible after the restart", async ({
  page,
}, testInfo) => {
  test.setTimeout(480_000);
  test.skip(testInfo.project.name !== "settings-restore", "runs on the walk's own server");

  // The walk stops the appliance on purpose (ADR-0090's restart-with-swap),
  // so the page's in-flight reads die with it: that transport churn is the
  // walk's own doing, exempted by text — every other console fault still
  // fails the walk. 503 joins 401 on the status ignore list: the walk's
  // server wires no capture layer (no GAME_NARRATOR_CAPTURE_FAKE), so the
  // /settings page's capture-device read answers 503 by design and renders
  // its honest "unavailable" state (the same 503 the setup-wizard and a11y
  // walks ignore for the same reason; ADR-0078's seam is opt-in per server).
  const consoleErrors = trackConsoleErrors(page, [401, 503], [/ERR_CONNECTION_REFUSED/]);
  const shot = (step: string) =>
    page.screenshot({ path: `test-results/restore-walk-${step}.png`, fullPage: true });

  const manifest = await readManifest();
  let replacement: ChildProcess | null = null;
  let replacementLog: ReturnType<typeof createWriteStream> | null = null;
  const workDir = await mkdtemp(join(tmpdir(), "gn-restore-walk-"));
  const backupPath = join(workDir, "walk-backup.sqlite");

  try {
    // ── 1. claim the throwaway appliance and sign in ────────────────────────
    await page.goto("/");
    await expect(page).toHaveURL(/\/setup$/);
    await page.getByTestId("setup-form").waitFor();
    await page.getByTestId("setup-username").fill(ADMIN.username);
    await page.getByTestId("setup-password").fill(ADMIN.password);
    await page.getByTestId("setup-submit").click();
    await expect(page).toHaveURL(/\/login$/);
    await page.getByTestId("login-username").fill(ADMIN.username);
    await page.getByTestId("login-password").fill(ADMIN.password);
    await page.getByTestId("login-submit").click();
    await expect(page).toHaveURL(/\/library$/);

    // ── 2. /settings: the update section against the scripted endpoint ──────
    await page.goto("/settings");
    await expect(page.getByTestId("settings-version")).toContainText("no auto-update");
    await expect(page.getByTestId("settings-version")).toContainText("GAME_NARRATOR_UPDATE_CHECK");

    // The manual re-check forces the check now; the answer must name the
    // scripted release and link its notes.
    const before = await stats();
    await page.getByTestId("updates-refresh").click();
    const notesLink = page.getByTestId("update-notes-link");
    await expect(notesLink).toHaveText(FAKE_RELEASE_TAG, { timeout: 30_000 });
    await expect(notesLink).toHaveAttribute(
      "href",
      new RegExp(`/repos/[^/]+/[^/]+/releases/tag/${FAKE_RELEASE_TAG}$`),
    );
    await shot("updates");
    expect(await stats(), "the check reached the scripted endpoint").toBeGreaterThan(before);

    // ── 3. the backup download is a real SQLite file ───────────────────────
    const [download] = await Promise.all([
      page.waitForEvent("download"),
      page.getByTestId("backup-download").click(),
    ]);
    await download.saveAs(backupPath);
    const backupBytes = await readFile(backupPath);
    expect(backupBytes.length).toBeGreaterThan(100_000);
    expect(backupBytes.subarray(0, 16).toString("utf8")).toBe("SQLite format 3\0");
    await shot("backup");

    // ── 4. the mutation: a new game the restore must undo ──────────────────
    await page.goto("/library");
    await page.getByTestId("new-game").click();
    await page.getByTestId("new-game-slug").fill(MUTATION.slug);
    await page.getByTestId("new-game-title").fill(MUTATION.title);
    await page.getByTestId("new-game-platform").fill("GameCube");
    await page.getByTestId("new-game-submit").click();
    const gameCard = page.getByTestId(`game-${MUTATION.slug}`);
    await expect(gameCard).toContainText(MUTATION.title, { timeout: 30_000 });
    await shot("mutation");

    // ── 5. the restore upload: hash review, then the 202 ───────────────────
    await page.goto("/settings");
    await page.getByTestId("restore-file").setInputFiles(backupPath);
    await expect(page.getByTestId("restore-review")).toContainText(`${backupBytes.length} bytes`);
    await expect(page.getByTestId("restore-review")).toContainText("SHA-256");
    await shot("restore-review");

    await page.getByTestId("restore-upload").click();
    const scheduled = page.getByTestId("restore-scheduled");
    await expect(scheduled).toBeVisible({ timeout: 30_000 });
    await expect(scheduled).toContainText("pre-restore");
    await expect(scheduled).toContainText(/staged SHA-256 [0-9a-f]{16}…/);
    await shot("restore-scheduled");

    // ── 6. the appliance shuts down to apply the plan ──────────────────────
    // Wait for the listener to actually close (connection refused), not
    // merely drain: a drain still answers health, and the replacement must
    // not race the original for the port. The wait runs from THIS process,
    // not the page: the page's pooled keep-alive connections to the origin
    // die of idleness (the server's ReadHeaderTimeout closes them) while the
    // appliance still listens, and a fetch that reuses a dead pooled
    // connection rejects exactly like a refused one — the walk once spawned
    // its replacement 70ms after such a false rejection, into a port the
    // original was still holding (the bind's EADDRINUSE was the proof). A
    // fresh node-side connection per poll can only see a real listener; a
    // few consecutive refusals keep one transient reset from passing for
    // the shutdown.
    const healthUrl = `http://${manifest.listen}/api/v1/health/live`;
    const waitStart = Date.now();
    let consecutiveRefusals = 0;
    while (consecutiveRefusals < 3) {
      try {
        await fetch(healthUrl, { cache: "no-store" });
        consecutiveRefusals = 0; // the appliance still answers: not down
      } catch {
        consecutiveRefusals += 1; // connection refused: the listener is gone
      }
      if (Date.now() - waitStart > 60_000) {
        throw new Error(
          `the appliance did not close its listener within 60s (refusals: ${consecutiveRefusals})`,
        );
      }
      await new Promise((resolve) => setTimeout(resolve, 250));
    }

    // The walk stands in for the supervisor: the same command, the same data
    // directory, the same environment the manifest's launcher used.
    // The replacement's boot-time update check (ADR-0088) fires during its
    // own startup — before its listener is even up — so the counter baseline
    // must predate the spawn: a baseline sampled after the restart has the
    // boot check already counted in it, and the poll below would wait the
    // 6h refresh interval for the next tick.
    const counterBeforeReplacement = await stats();
    const childEnv = {
      ...process.env,
      HOME: manifest.homeDir,
      XDG_DATA_HOME: `${manifest.homeDir}/.local/share`,
      GAME_NARRATOR_DATA_DIR: manifest.dataDir,
      GAME_NARRATOR_UPDATE_BASE_URL: `http://127.0.0.1:${ports.fakeReleases}`,
    };
    replacement = spawn(
      manifest.command[0],
      [...manifest.command.slice(1), "--listen", manifest.listen, "--data-dir", manifest.dataDir],
      {
        cwd: REPO_ROOT,
        env: childEnv,
        stdio: ["ignore", "pipe", "pipe"],
      },
    );
    // The replacement process's own log, durable on disk: the output string
    // below only survives into a report if the probe assertion itself fails —
    // a replacement that answered one probe and then died would otherwise
    // leave no evidence at all (the walk IS the supervisor, and the
    // replacement's stdout goes nowhere else).
    const replacementLogStream = createWriteStream("test-results/restore-walk-replacement.log", {
      flags: "w",
    });
    replacementLog = replacementLogStream;
    let output = "";
    replacement.stdout?.on("data", (chunk: Buffer) => {
      output += chunk.toString();
      replacementLogStream.write(chunk);
    });
    replacement.stderr?.on("data", (chunk: Buffer) => {
      output += chunk.toString();
      replacementLogStream.write(chunk);
    });
    // The probe log is the evidence channel: on a failure the assertion below
    // carries what each probe actually saw plus the replacement's output, so
    // a stuck shutdown is readable from the report without a rerun.
    const probes: string[] = [];
    const probe = async () => {
      try {
        const r = await fetch(`http://${manifest.listen}/api/v1/health/live`);
        return `${r.status}`;
      } catch (e) {
        return `err:${String(e).slice(0, 120)}`;
      }
    };
    const started = await (async () => {
      let verdict = await probe();
      probes.push(`p0 ${verdict}`);
      for (let i = 1; i <= 240 && !verdict.startsWith("2"); i += 1) {
        await new Promise((resolve) => setTimeout(resolve, 500));
        verdict = await probe();
        if (i % 20 === 0 || verdict.startsWith("2")) probes.push(`p${i} ${verdict}`);
      }
      return verdict.startsWith("2");
    })();
    expect(
      started,
      `the replacement answered (probes: ${probes.join(" | ")} | output: ${output})`,
    ).toBe(true);
    // The supervisor's first duty is to notice a dead replacement: a process
    // that answered a probe and then exited must not pass the restart step —
    // the walk once failed exactly that way with no evidence, because the
    // death surfaced only at the later page navigation.
    expect(
      replacement.exitCode,
      `the replacement exited right after answering (probes: ${probes.join(" | ")} | output: ${output})`,
    ).toBeNull();
    await shot("restarted");

    // ── 7. the plan is APPLIED: the banner, the schema, the undo ───────────
    await page.goto("/");
    // The app's redirect to the signed-in default (or the login page) happens
    // client-side after load: settle the URL before branching on it, or the
    // branch reads the pre-redirect "" and looks for a login form the
    // already-authenticated app will never render (the walk timed out exactly
    // that way: the page WAS on /library, the branch waited for /login).
    await expect(page).toHaveURL(/\/(library|login)$/, { timeout: 20_000 });
    if (!page.url().endsWith("/library")) {
      // The session did not survive the restart (or the claim is fresh): the
      // credentials are in the backup, so the sign-in still works.
      await page.getByTestId("login-username").fill(ADMIN.username);
      await page.getByTestId("login-password").fill(ADMIN.password);
      await page.getByTestId("login-submit").click();
      await expect(page).toHaveURL(/\/library$/);
    }

    await page.goto("/settings");
    const banner = page.getByTestId("restore-banner-applied");
    await expect(banner).toBeVisible({ timeout: 30_000 });
    await expect(banner).toContainText(/Restore applied/);
    await expect(banner).toContainText(/schema \d+ → \d+/);
    await shot("applied");

    // The mutation the restore was meant to undo is gone.
    await page.goto("/library");
    await expect(page.getByTestId(`game-${MUTATION.slug}`)).toHaveCount(0);

    // The restarted process re-checked on its own schedule: the endpoint's
    // counter — baselined before the spawn — moved only because the
    // replacement ran its boot-time check (ADR-0088), not the manual one.
    await expect
      .poll(async () => stats(), { timeout: 60_000, intervals: [2000] })
      .toBeGreaterThan(counterBeforeReplacement);

    // No console error may have ridden through the walk (the server's
    // shutdown mid-page is expected network churn, not a page error).
    expect(consoleErrors).toEqual([]);
  } finally {
    replacementLog?.end();
    if (replacement && replacement.exitCode === null) {
      replacement.kill();
      await new Promise((resolve) => replacement!.on("exit", resolve));
    }
    await rm(workDir, { recursive: true, force: true });
  }
});
