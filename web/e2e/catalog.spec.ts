import { expect, test } from "@playwright/test";
import { existsSync, readFileSync } from "node:fs";
import { resolve } from "node:path";

import {
  apiAsUser,
  currentProfileEtag,
  expectDefaultDataDirUntouched,
  serverRun,
} from "./support/servers";
import { trackConsoleErrors } from "./support/console";

/**
 * The catalogue's end-to-end lifecycle, driven through the pages a curator uses,
 * against the real binary and a data directory nothing else can reach
 * (docs/spec/12 "UI" mode, docs/spec/09 "E2E UI verification requirements").
 * This is the milestone's evidence that the API, the SPA and the job queue agree:
 * no mocks, no fixtures intercepted, nothing labelled end-to-end that was stubbed.
 *
 * One test, deliberately: first run is a sequence with no restarts in it — claim
 * the appliance, sign in, name a title, bind an edition, open a revision, put a
 * script in it, correct a line, and ship it. Each step asserts the *visible* state
 * (docs/spec/09 "UX robustness"), and the steps that can only be proven from
 * outside the page — a second editor landing mid-session, an asset written to the
 * content-addressed store — are asserted from outside the page.
 */

const TESTDATA = resolve(
  import.meta.dirname,
  "..",
  "..",
  "internal",
  "games",
  "adapter",
  "generic",
  "testdata",
);
const ADVENTURE_CSV = resolve(TESTDATA, "synthetic-adventure.csv");
const OTHER_GAME_JSON = resolve(TESTDATA, "synthetic-other-game.json");

const ADMIN = { username: "curator-admin", password: "correct horse battery" };

test("first run to published revision, through the browser", async ({ page, request, baseURL }) => {
  test.setTimeout(240_000);
  const origin = new URL(baseURL as string).origin;
  const run = serverRun("catalog");
  const api = apiAsUser(request, origin);

  const consoleErrors = trackConsoleErrors(page, [401, 409, 412, 422]);
  // The walk-through rejects four requests on purpose, and the browser logs
  // each failed status as a console error. Each status below is allowlisted
  // because one named step below makes it, and no other step may:
  //   401 — step 1's unauthenticated /auth/me probe on mount, and step 2's
  //         wrong password, which the login form has to explain;
  //   412 — step 7's stale save after the rival editor moved the revision;
  //   409 — step 9's save into the published, frozen revision;
  //   422 — step 8's publish refusal: the empty draft carries the "no
  //         messages" finding the review gate exists to list (catalog.go
  //         answers unpublishable as 422).
  // Any other console error — a CSP block, a missing bundle, a page fault —
  // still fails the cleanliness check at the end of the spec.
  const shot = (step: string) =>
    page.screenshot({ path: `test-results/catalog-${step}.png`, fullPage: true });

  // ── 1. first run: the guard sends an unclaimed appliance to /setup ──────────
  await page.goto("/library");
  await expect(page).toHaveURL(/\/setup$/);
  await expect(page.getByTestId("setup-form")).toBeVisible();
  await page.getByTestId("setup-username").fill(ADMIN.username);
  await page.getByTestId("setup-password").fill(ADMIN.password);
  await page.getByTestId("setup-submit").click();

  // Setup mints no session (ADR-0016 §7): the wizard must sign in afterwards.
  await expect(page).toHaveURL(/\/login$/);
  await shot("01-setup");

  // ── 2. sign in, including the refusal the form has to explain ──────────────
  await page.getByTestId("login-username").fill(ADMIN.username);
  await page.getByTestId("login-password").fill("wrong password entirely");
  await page.getByTestId("login-submit").click();
  // The alert says the 401 twice over: the refusal itself, and the note that
  // the server deliberately does not say which half was wrong. Both are part
  // of the page, so both are asserted — as separate elements, since the
  // server title is user-facing copy and the note is ours.
  const loginError = page.getByTestId("login-error");
  await expect(loginError).toBeVisible();
  await expect(loginError.locator("p").first()).toHaveText("Username or password is not right.");
  await expect(loginError.locator("p").last()).toHaveText(
    "The server does not say which half was wrong, and the attempt is in its audit log.",
  );

  await page.getByTestId("login-password").fill(ADMIN.password);
  await page.getByTestId("login-submit").click();
  await expect(page).toHaveURL(/\/library$/);
  // The `request` fixture is its own context with its own cookie jar: the
  // session was minted for the page, so hand it over before anything below
  // talks to the API the way a second editor would (a 401 there would read as
  // "the job never ran" when the truth is "this caller never signed in").
  await request.addCookies(await page.context().cookies());
  await expect(page.getByTestId("nav-user")).toContainText(ADMIN.username);
  await expect(page.getByTestId("nav-role")).toHaveText("admin");
  await shot("02-library-empty");

  // ── 3. the library's empty state, then a game ──────────────────────────────
  await expect(page.getByTestId("empty-state")).toContainText("No games yet");
  await page.getByTestId("new-game").click();
  await expect(page.getByTestId("modal")).toBeVisible();
  await page.getByTestId("new-game-slug").fill("synthetic-adventure");
  await page.getByTestId("new-game-title").fill("Synthetic Adventure");
  await page.getByTestId("new-game-platform").fill("GameCube");
  await page.getByTestId("new-game-submit").click();
  await expect(page.getByTestId("game-list")).toBeVisible();
  await expect(page.getByTestId("game-synthetic-adventure")).toContainText("Synthetic Adventure");
  await shot("03-library-one-game");

  // ── 4. overview: an edition, then a draft revision ─────────────────────────
  await page.getByTestId("game-synthetic-adventure").click();
  await expect(page.getByTestId("game-title")).toHaveText("Synthetic Adventure");
  await expect(page.getByTestId("empty-state")).toContainText("No editions yet");

  await page.getByTestId("new-edition").click();
  await page.getByTestId("edition-region").fill("us");
  await page.getByTestId("edition-language").fill("en");
  await page.getByTestId("edition-revision").fill("disc-1");
  await page.getByTestId("edition-submit").click();
  await expect(page.getByRole("tab", { name: "us / en / disc-1" })).toBeVisible();

  const editionPanel = page.getByTestId(/edition-panel-/);
  await editionPanel.getByTestId("new-draft").click();
  await expect(page.getByTestId("revision-1")).toBeVisible();
  await expect(page.getByTestId("revision-1").getByTestId("badge-draft")).toBeVisible();
  await shot("04-revision-draft");

  // ── 5. import: 202 → job → the importer's own report ────────────────────────
  const revision1 = page.getByTestId("revision-1");
  await revision1.getByTestId("import-file").setInputFiles(ADVENTURE_CSV);
  await expect(revision1.getByTestId("import-rights")).toHaveValue("user_supplied");
  await revision1.getByTestId("import-submit").click();
  // The job is picked up within a beat of being queued, so the "queued" label
  // is not guaranteed to be painted; what *is* guaranteed is that the progress
  // line appears and then reports the importer's own verdict. A failure here
  // would be a real fault: the CSV is the adapter's own synthetic fixture.
  await expect(page.getByTestId("job-progress")).toBeVisible();
  await expect(page.getByTestId("job-progress")).toContainText("succeeded", { timeout: 60_000 });
  await expect(page.getByTestId("import-inserted")).toHaveText("40");
  await shot("05-import-succeeded");

  // A second source, so the explorer has more than one page of messages and the
  // pager can be *seen* rather than asserted to be absent (docs/spec/09 "don't
  // load thousands of messages into React memory"). The page's progress line can
  // still be rendering the *first* job's report when the second import's verdict
  // lands, so the second job's own terminal state is proven from outside the
  // page — the same way the second editor's write is proven further down.
  const secondAccepted = page.waitForResponse(
    (response) => response.request().method() === "POST" && response.status() === 202,
  );
  // The second source is deliberately unlike the CSV (docs/spec/04 "two-game
  // portability"): different key names, numbers where the CSV has strings. The
  // mapping is done in the form's column fields, not by the adapter guessing.
  await revision1.getByTestId("import-mapping").locator("summary").click();
  await revision1.getByTestId("import-col-text").fill("line_of_dialogue");
  await revision1.getByTestId("import-col-id").fill("utterance_id");
  await revision1.getByTestId("import-col-speaker").fill("who_speaks");
  await revision1.getByTestId("import-col-kind").fill("sort_of_text");
  await revision1.getByTestId("import-col-scene").fill("stage");
  await revision1.getByTestId("import-col-page").fill("leaf_number");
  await revision1.getByTestId("import-file").setInputFiles(OTHER_GAME_JSON);
  await revision1.getByTestId("import-submit").click();
  const secondJob = (await (await secondAccepted).json()) as { job_id: string };
  await expect
    .poll(
      async () => {
        const body = (await (await request.get(`/api/v1/jobs/${secondJob.job_id}`)).json()) as {
          state: string;
        };
        if (body.state === "failed")
          throw new Error(`the second import job failed: ${JSON.stringify(body)}`);
        return body.state;
      },
      { message: "the second import job reached a terminal state", timeout: 60_000 },
    )
    .toBe("succeeded");
  await expect(page.getByTestId("job-progress")).toContainText("succeeded");

  // The upload is content-addressed into the asset store beside the database
  // (ADR-0014), which is a fact about this server's data directory.
  expect(existsSync(resolve(run.dataDir, "assets")), "the asset store lives in the temp dir").toBe(
    true,
  );

  // ── 6. the dialogue explorer: pagination, search, and no 10k-row dump ──────
  await page.getByTestId("dialogue-1").click();
  await expect(page.getByTestId("message-table")).toBeVisible();
  await expect(page.getByTestId("messages-page")).toHaveText("page 1");
  const firstPageRows = page.getByTestId(/message-row-/);
  await expect(firstPageRows).toHaveCount(50);
  // Exactly a display page, not the whole script: docs/spec/09 "UX robustness".
  expect(await page.locator("tbody tr").count()).toBe(50);

  await page.getByTestId("messages-next").click();
  await expect(page.getByTestId("messages-page")).toHaveText("page 2");
  // 53 messages in total: the CSV's 40 plus the JSON's 15 entries, of which two
  // pairs share a source id and merge into multi-page messages (13 rows).
  await expect(firstPageRows).toHaveCount(3);
  await shot("06-dialogue-page-two");
  await page.getByTestId("messages-prev").click();
  await expect(page.getByTestId("messages-page")).toHaveText("page 1");
  await expect(firstPageRows).toHaveCount(50);

  await page.getByTestId("dialogue-search").fill("harbour");
  await page.getByTestId("dialogue-filter-submit").click();
  await expect(page.getByText(/Search results for “harbour”/)).toBeVisible();
  const hits = page.getByTestId(/message-row-/);
  await expect(hits.first()).toBeVisible();
  expect(await hits.count()).toBeGreaterThan(0);
  expect(await hits.count()).toBeLessThanOrEqual(50);
  await expect(page.getByText(/harbourmaster is counting crates/)).toBeVisible();
  await shot("07-dialogue-search");

  await page.getByTestId("dialogue-search").fill("");
  await page.getByTestId("dialogue-filter-submit").click();

  // ── 7. a correction, then the correction that arrived first ────────────────
  const row = page.getByTestId("message-row-l-002");
  await expect(row).toBeVisible();
  await row.getByTestId(/select-/).click();
  await expect(page.getByTestId("message-detail")).toBeVisible();
  await expect(page.getByTestId("original-text")).toHaveText(
    "The harbourmaster is counting crates again. Pay her no mind.",
  );
  await expect(page.getByTestId("normalized-text")).toHaveText(
    "the harbourmaster is counting crates again. pay her no mind.",
  );

  const speech = page.getByTestId("speech-text");
  await speech.fill("Pay the harbourmaster no mind.");
  await expect(page.getByTestId("speech-save")).toBeEnabled();
  await page.getByTestId("speech-save").click();
  await expect(page.getByTestId("speech-save")).toBeDisabled();
  await expect(page.getByTestId("conflict-stale")).toHaveCount(0);
  await shot("08-dialogue-saved");

  // Another curator saves to the same revision while this editor is open. The
  // token this page holds is now the old one, and the only correct answer is to
  // refuse the write and say so (ADR-0015: the revision is the unit of
  // concurrency, so there is no per-row merge to attempt).
  const url = new URL(page.url());
  const profileId = url.searchParams.get("profile") as string;
  // The PATCH addresses the internal message id, which the table shows as a label
  // and not as a path — so the rival editor resolves it the way the API does.
  const listed = (await (
    await api.get(`/api/v1/profiles/${profileId}/messages?limit=50`)
  ).json()) as {
    messages: { id: string; source_message_id: string | null }[];
  };
  const rival = listed.messages.find((m) => m.source_message_id === "l-002");
  expect(rival, "l-002 belongs to this revision").toBeTruthy();
  const liveEtag = await currentProfileEtag(request, origin, profileId);
  const overwritten = await api.patch(
    `/api/v1/profiles/${profileId}/messages/${rival?.id}`,
    { speech_text: "Someone else was here first." },
    liveEtag,
  );
  expect(overwritten.status(), "the concurrent write succeeded").toBe(200);

  await speech.fill("And yet this editor kept typing.");
  await page.getByTestId("speech-save").click();
  await expect(page.getByTestId("conflict-stale")).toContainText("This revision changed — reload");
  await shot("09-dialogue-conflict");
  // The unsaved text is still in the box: a conflict must not eat a paragraph.
  await expect(speech).toHaveValue("And yet this editor kept typing.");

  await page.getByTestId("conflict-reload").click();
  await expect(page.getByTestId("conflict-stale")).toHaveCount(0);
  await page.getByTestId("speech-save").click();
  await expect(page.getByTestId("speech-save")).toBeDisabled();
  await expect(speech).toHaveValue("And yet this editor kept typing.");
  await shot("10-dialogue-conflict-resolved");

  // ── 8. review, the gate, and publication ───────────────────────────────────
  await page.getByTestId("overview-link").click();
  await expect(page.getByTestId("game-title")).toHaveText("Synthetic Adventure");
  await page.getByTestId("status-review").click();
  await expect(page.getByTestId("revision-1").getByTestId("badge-review")).toBeVisible();

  // An empty second draft, so the gate has something honest to refuse: a
  // revision with no messages is exactly the finding docs/spec/03's review screen
  // exists to list, and a publish that always succeeds would never render it.
  await page.getByTestId("new-draft").click();
  await expect(page.getByTestId("revision-2")).toBeVisible();
  await page.getByTestId("revision-2").getByTestId("status-review").click();
  await page.getByTestId("revision-2").getByTestId("publish").click();
  await expect(page.getByTestId("problem-list")).toBeVisible();
  await expect(page.getByTestId("problem-list")).toContainText("no messages");
  await shot("11-publish-refused");

  await page.getByTestId("revision-1").getByTestId("publish").click();
  await expect(page.getByTestId("revision-1").getByTestId("badge-published")).toBeVisible();
  await shot("12-revision-published");

  // ── 9. a published revision is immutable, and says so ──────────────────────
  await page.getByTestId("dialogue-1").click();
  await expect(page.getByTestId("message-table")).toBeVisible();
  await page
    .getByTestId("message-row-l-002")
    .getByTestId(/select-/)
    .click();
  await expect(page.getByTestId("immutable-notice")).toContainText("frozen");
  await speech.fill("This cannot be written.");
  await page.getByTestId("speech-save").click();
  await expect(page.getByTestId("conflict-immutable")).toBeVisible();
  await expect(page.getByTestId("conflict-immutable")).toContainText("frozen");
  await expect(page.getByTestId("derive-revision")).toBeVisible();
  await shot("13-dialogue-immutable");

  // Deriving is the only way forward through a frozen revision (ADR-0015), and
  // the page follows the caller there.
  await page.getByTestId("derive-revision").click();
  // The URL already carries a `profile` param, so assert on the move itself:
  // the revision id changes, which only the successful derive does.
  await expect
    .poll(() => new URL(page.url()).searchParams.get("profile"), {
      timeout: 10_000,
      message: "the dialogue moved to the derived revision",
    })
    .not.toBe(profileId);
  await expect(page.getByTestId("message-table")).toBeVisible();

  // ── 10. export, through the browser's own download path ────────────────────
  await page.getByTestId("overview-link").click();
  // Wait for the download *while* clicking: a download that starts during
  // the click fires its event before a subsequent `waitForEvent` is
  // registered, so the two must overlap or the fast path is missed.
  const [download] = await Promise.all([
    page.waitForEvent("download", { timeout: 20_000 }),
    page.getByTestId("export-1").click(),
  ]);
  expect(download.suggestedFilename()).toMatch(/\.gnprofile\.json$/);
  const exportPath = resolve("test-results", download.suggestedFilename());
  await download.saveAs(exportPath);

  // What the browser saved is the same document the API serves, and it is the
  // versioned manifest of docs/spec/05 rather than an ad-hoc dump.
  // 1.1 since ADR-0101 (the manifest gained the voices section and the
  // per-profile narrator fallback; the version bump is its contract).
  const downloaded = JSON.parse(readFileSync(exportPath, "utf8"));
  expect(downloaded.schema_version).toBe("1.1");
  expect(downloaded.publication.status).toBe("published");

  const manifest = await (await api.get(`/api/v1/profiles/${profileId}/export`)).json();
  expect(manifest.schema_version).toBe("1.1");
  expect(manifest.profile_id).toBe(profileId);
  expect(manifest.publication.status).toBe("published");
  expect(manifest.messages.length).toBe(53);

  // ── 11. sign out: the routes go away with the session ──────────────────────
  await page.getByTestId("nav-logout").click();
  await expect(page).toHaveURL(/\/login/);
  await page.goto("/library");
  await expect(page).toHaveURL(/\/login\?next=%2Flibrary$/);

  // ── 12. nothing leaked, nothing was touched elsewhere ──────────────────────
  expect(consoleErrors, `browser console: ${consoleErrors.join("\n")}`).toEqual([]);
  expectDefaultDataDirUntouched(run);
});
