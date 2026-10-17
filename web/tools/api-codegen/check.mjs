// `pnpm check:api` — regenerate the contract types into a temp file and fail
// when the committed src/api/schema.d.ts no longer matches
// internal/api/openapi/openapi.json. Wired into `pnpm typecheck`, so
// `make test-ui` cannot pass with stale frontend types (AGENTS.md rule 8:
// typed interfaces plus compatibility tests, not assumptions).

import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, resolve } from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = resolve(here, "..", "..", "..");
const committed = resolve(here, "..", "..", "src/api/schema.d.ts");
const source = process.env.OPENAPI_SPEC ?? resolve(repoRoot, "internal/api/openapi/openapi.json");

const dir = await mkdtemp(resolve(tmpdir(), "gn-api-gen-"));
const fresh = resolve(dir, "schema.d.ts");

const gen = spawnSync(process.execPath, [resolve(here, "gen.mjs"), source, fresh], {
  encoding: "utf8",
});
if (gen.status !== 0) {
  process.stderr.write(gen.stderr || gen.stdout || "openapi-typescript failed\n");
  process.exit(1);
}

let before, after;
try {
  [before, after] = await Promise.all([readFile(committed, "utf8"), readFile(fresh, "utf8")]);
} catch (error) {
  if (error.code === "ENOENT") {
    process.stderr.write(
      `src/api/schema.d.ts is missing — run \`pnpm gen:api\` and commit the result\n`,
    );
    process.exit(1);
  }
  throw error;
}

if (before !== after) {
  process.stderr.write(
    "src/api/schema.d.ts is stale: it differs from the generated output of " +
      `${source.replace(`${repoRoot}/`, "")}. Run \`pnpm gen:api\` and commit the result.\n`,
  );
  process.exit(1);
}

await rm(dir, { recursive: true, force: true });
process.stdout.write("src/api/schema.d.ts matches the OpenAPI contract\n");
