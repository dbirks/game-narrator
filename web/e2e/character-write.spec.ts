import { expect, test, type Page } from "@playwright/test";

import { trackConsoleErrors } from "./support/console";
import { apiAsUser, claimAppliance, currentProfileEtag } from "./support/servers";

/**
 * The roster's direct write surface (GitHub #42, ADR-0180) on the Cast page:
 * the "Add a character" form mints a row with its editorial half — the role,
 * the voice brief and the appearance descriptors (the cast audit reads the
 * character's gender/age description from here) — and the created character
 * appears in the roster. The same scripted-speech server the cast walk runs
 * on (web/e2e/fake-tts.mjs): the real API process and the real store, only
 * the models scripted (AGENTS.md rule 3).
 */

const ADMIN = { username: "voices-admin", password: "steady as she goes" }; // the one `voices` server the voices, voices-qa, cast and this spec share
const NAME = "Harbormaster"; // one name this spec owns: the roster is per-profile, so it cannot collide with the cast walk's characters
const APPEARANCE = ["an elderly woman", "about seventy years old"];

async function signIn(page: Page) {
  await page.goto("/library");
  await claimAppliance(page, ADMIN);
  await page.getByTestId("login-username").fill(ADMIN.username);
  await page.getByTestId("login-password").fill(ADMIN.password);
  await page.getByTestId("login-submit").click();
  await expect(page.getByTestId("nav-user")).toBeVisible();
  await expect(page.getByTestId("nav-role")).toHaveText("admin");
  return page.context().cookies();
}

test("the cast page mints a character with its editorial half and the row appears in the roster", async ({
  page,
  request,
  baseURL,
}) => {
  const origin = new URL(baseURL as string).origin;
  const api = apiAsUser(request, origin);
  const consoleErrors = trackConsoleErrors(page, [401, 409]);

  const cookies = await signIn(page);
  await request.addCookies(cookies);

  // A game, an edition, a draft revision — the revision the roster lives on.
  await page.getByTestId("new-game").click();
  await page.getByTestId("new-game-slug").fill("char-write-harbour");
  await page.getByTestId("new-game-title").fill("Char Write Harbour");
  await page.getByTestId("new-game-platform").fill("GameCube");
  await page.getByTestId("new-game-submit").click();
  await expect(page.getByTestId("game-char-write-harbour")).toContainText("Char Write Harbour");
  await page.getByTestId("game-char-write-harbour").click();
  await expect(page.getByTestId("game-title")).toHaveText("Char Write Harbour");

  await page.getByTestId("new-edition").click();
  await page.getByTestId("edition-region").fill("us");
  await page.getByTestId("edition-language").fill("en");
  await page.getByTestId("edition-revision").fill("disc-1");
  await page.getByTestId("edition-submit").click();
  await expect(page.getByRole("tab", { name: "us / en / disc-1" })).toBeVisible();
  await page
    .getByTestId(/edition-panel-/)
    .getByTestId("new-draft")
    .click();
  await expect(page.getByTestId("revision-1")).toBeVisible();

  // The Cast screen: the form shows on a draft the operator may move.
  await page.getByTestId("cast-1").click();
  await expect(page).toHaveURL(/\/cast\?profile=/);
  await expect(page.getByTestId("cast-add-character-toggle")).toBeVisible();
  await page.getByTestId("cast-add-character-toggle").click();

  await page.getByTestId("cast-add-name").fill(NAME);
  await page.getByTestId("cast-add-role").selectOption("narrator");
  await page.getByTestId("cast-add-brief").fill("A calm, weathered voice; speaks slowly.");
  await page.getByTestId("cast-add-aliases").fill("harbourmaster, harbour pilot");
  await page.getByTestId("cast-add-appearance").fill(APPEARANCE.join(", "));
  await page.getByTestId("cast-add-submit").click();

  // The created character appears in the roster the screen shows.
  await expect(page.getByTestId(`character-row-${NAME}`)).toBeVisible();
  await expect(page.getByTestId(`character-row-${NAME}`)).toContainText(NAME);

  // The wire agrees: the read the roster renders carries the editorial half
  // the form submitted — the role, the voice brief and the appearance
  // descriptors (the cast audit's gender/age description, ADR-0180).
  const profileId = (await page.url()).match(/profile=([^&]+)/)?.[1] ?? "";
  expect(profileId).not.toBe("");
  const roster = (await (await api.get(`/api/v1/profiles/${profileId}/characters`)).json()) as {
    characters: {
      id: string;
      name: string;
      role: string;
      personality_summary: string;
      aliases: string[];
      visual_descriptors: string[];
      status: string;
    }[];
  };
  const row = roster.characters.find((c) => c.name === NAME);
  expect(row, "the roster read carries the created character").toBeTruthy();
  expect(row?.role).toBe("narrator");
  expect(row?.personality_summary).toBe("A calm, weathered voice; speaks slowly.");
  expect(row?.aliases).toEqual(["harbourmaster", "harbour pilot"]);
  expect(row?.visual_descriptors).toEqual(APPEARANCE);
  expect(row?.status).toBe("proposed");

  // The row is editable over the same surface (the PUT the cast audit's
  // downstream work relies on): the role moves, the rest keeps its value.
  const etag = await currentProfileEtag(request, origin, profileId);
  const csrf =
    (await request.cookies(origin)).find((cookie) => cookie.name === "gn_csrf")?.value ?? "";
  const putResponse = await request.put(
    `${origin}/api/v1/profiles/${profileId}/characters/${row?.id}`,
    {
      data: { role: "lead" },
      headers: { "content-type": "application/json", "x-csrf-token": csrf, "if-match": etag },
    },
  );
  expect(putResponse.status(), await putResponse.text()).toBe(200);
  const updated = (await (await api.get(`/api/v1/profiles/${profileId}/characters`)).json()) as {
    characters: { name: string; role: string; personality_summary: string }[];
  };
  const after = updated.characters.find((c) => c.name === NAME);
  expect(after?.role).toBe("lead");
  expect(after?.personality_summary).toBe("A calm, weathered voice; speaks slowly.");

  expect(consoleErrors).toEqual([]);
});
