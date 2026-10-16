# Game Narrator web UI

Vite + React + TypeScript front end for the Go `game-narrator` service
(docs/spec/09-ui-and-workflows.md). Built assets are embedded into the Go
binary by a later milestone; today `web/dist` is produced by `pnpm build` and
not yet consumed by Go.

## Requirements

- pnpm (the lockfile is `pnpm-lock.yaml`; never use npm/yarn here)
- Go toolchain on `PATH` for the Playwright `webServer` that starts the real API
- Chromium managed by Playwright, with its system libraries and fonts from
  `scripts/bootstrap-arch.sh` at the repo root

## Commands

| Command          | What it does                                             |
| ---------------- | -------------------------------------------------------- |
| `pnpm install`   | install dependencies from the lockfile                   |
| `pnpm dev`       | Vite dev server on `http://127.0.0.1:5173` (strict port) |
| `pnpm build`     | `tsc -b` then `vite build` into `dist/`                  |
| `pnpm typecheck` | `tsc -b --noEmit`                                        |
| `pnpm lint`      | oxlint                                                   |
| `pnpm fmt`       | oxfmt (write)                                            |
| `pnpm fmt:check` | oxfmt `--check`                                          |
| `pnpm test`      | Vitest unit tests in jsdom (stubbed `fetch` fakes only)  |
| `pnpm test:e2e`  | Playwright against the real Go API + Vite (no mocks)     |

`pnpm dev` proxies `/api` to the Go API at `http://127.0.0.1:8080`, or to
`GAME_NARRATOR_API` when that variable is set. Start the API with
`make dev` from the repo root, or let Playwright start both processes.

## Routes

- `/` → redirects to `/diagnostics`
- `/diagnostics` — build version/commit, Go version, `os/arch` and readiness
  from `GET /api/v1/system/info` and `GET /api/v1/health/ready`
- anything else → "Not found" page linking back to Diagnostics

## Playwright browser dependencies

Install the browser build itself with Playwright, and its system libraries and
fonts with the repo-root bootstrap script:

```sh
pnpm exec playwright install chromium
scripts/bootstrap-arch.sh   # from the repo root
```

On Arch, Chromium's shared-library dependencies (`at-spi2-core`,
`libxcomposite`, `libxdamage`, `nss`, `libxkbcommon`, `libxrandr`, `alsa-lib`)
and the fonts that make UI text render in screenshots (`ttf-dejavu`,
`noto-fonts`, `ttf-liberation`) are installed as real system packages by
`scripts/bootstrap-arch.sh` (run from the repo root). Nothing is extracted into a
user-local directory and `playwright.config.ts` performs no dynamic-loader
override — the browser uses the system loader path exactly like any other
installed program.
