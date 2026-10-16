import { expect, test } from "@playwright/test";

// docs/spec/09 "Embedded binary test": the packaged binary alone (no Vite
// process) must serve the real SPA, answer deep links with index.html, and run
// under the Content-Security-Policy the Go server sends. Selected for the
// `embedded` project only (see playwright.config.ts testMatch).

const csp = "default-src 'self'";

test("embedded binary serves the SPA and its deep links", async ({ page, request }) => {
  const consoleErrors: string[] = [];
  page.on("console", (message) => {
    if (message.type() === "error") {
      consoleErrors.push(message.text());
    }
  });
  page.on("pageerror", (error) => {
    consoleErrors.push(String(error));
  });

  // Deep link into a client-side route that no server route owns. The server
  // must answer with index.html (200, text/html) and the client router must
  // then render its own "Not found" page — proof the SPA, not a Go template,
  // produced the DOM.
  const deepLink = await page.goto("/games/demo/dialogue");
  expect(deepLink?.status()).toBe(200);
  expect(deepLink?.headers()["content-type"]).toContain("text/html");
  expect(deepLink?.headers()["content-security-policy"]).toContain(csp);
  expect(deepLink?.headers()["cache-control"]).toBe("no-cache");
  await expect(page.getByRole("heading", { name: "Not found" })).toBeVisible();
  await expect(page.getByRole("link", { name: "Back to Diagnostics" })).toBeVisible();

  // The bundle referenced by index.html is served by the same binary with a
  // JS media type and immutable caching (a wrong type would break the module
  // script and show up as a console error below).
  const indexHtml = await (await request.get("/games/demo/dialogue")).text();
  const scriptMatch = /\/assets\/index-[\w-]+\.js/.exec(indexHtml);
  expect(scriptMatch, "index.html must reference a built module script").not.toBeNull();
  const script = scriptMatch![0];
  const asset = await request.get(script);
  expect(asset.status()).toBe(200);
  expect(asset.headers()["content-type"]).toBe("text/javascript");
  expect(asset.headers()["cache-control"]).toBe("public, max-age=31536000, immutable");
  expect(asset.headers()["content-security-policy"]).toContain(csp);

  // A missing hashed asset stays a problem+json 404 rather than index.html.
  const missing = await request.get("/assets/index-doesnotexist.js");
  expect(missing.status()).toBe(404);
  expect(missing.headers()["content-type"]).toContain("application/problem+json");

  // /diagnostics is served from the bundle and reads live data from the same
  // process, i.e. the embedded UI and the API agree.
  await page.goto("/diagnostics");
  await expect(page.getByRole("heading", { name: "Diagnostics" })).toBeVisible();

  const info = (await (await request.get("/api/v1/system/info")).json()) as {
    version: string;
    commit: string;
    os: string;
    arch: string;
  };
  await expect(page.getByTestId("version")).toHaveText(info.version);
  await expect(page.getByTestId("commit")).toHaveText(info.commit);
  await expect(page.getByTestId("readiness")).toHaveText("ok");
  await expect(page.getByTestId("platform")).toHaveText(`${info.os}/${info.arch}`);

  // `make build` stamps these with ldflags; "dev"/"unknown" would mean this
  // run fell back to the `go run` defaults and is not the packaged binary.
  expect(info.version).not.toBe("dev");
  expect(info.commit).not.toBe("unknown");

  await page.screenshot({ path: "test-results/embedded-diagnostics.png", fullPage: true });

  // CSP must not break the app: no blocked scripts, styles or fetches.
  expect(consoleErrors).toEqual([]);
});
