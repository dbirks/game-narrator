# First boot

## Purpose

Bring a fresh host to a running, honestly self-reporting Game Narrator:
binary built with the UI embedded, database migrated, an admin account, the
service serving `/diagnostics`, and `doctor` reporting every component as
`OK` / `DEGRADED` / `BLOCKED` / `NOT CONFIGURED` / `FAIL`.

## Preconditions

- **Dev host (Arch Linux):** `sudo` (the bootstrap uses it), nothing else —
  Go, Node/pnpm, make, tesseract, ImageMagick, Xvfb and the Chromium
  system libraries are installed by the bootstrap script.
- **UNO Q (Debian arm64):** a built `.deb` from the dev host
  (`make release-snapshot`, artifact in `dist/`). UNO Q hardware is
  **NOT VERIFIED** (docs/release.md, docs/status/blocked.md): the arm64
  artifact is cross-compiled and file-verified only, and no UNO Q smoke
  has been run.

## Steps — dev host

```sh
scripts/bootstrap-arch.sh            # idempotent; --dry-run previews the pacman commands
pnpm --dir web exec playwright install chromium   # browser for make test-ui
make build                           # Vite build -> embedded into bin/game-narrator
bin/game-narrator migrate            # apply embedded SQLite migrations
printf '%s' '<password>' | bin/game-narrator admin create --username admin --role admin
bin/game-narrator serve              # http://127.0.0.1:8080
make diagnostics                     # rebuild + bin/game-narrator doctor --dev
```

The first admin can also be created from the browser at `/setup`
(`POST /api/v1/auth/setup` is loopback-only); afterwards log in at `/login`.
`serve` never migrates on boot — step 4 is deliberate so a running service
cannot be upgraded out from under itself.

## Steps — UNO Q

```sh
dpkg -i game-narrator_<version>_linux_arm64.deb
# create /etc/game-narrator/env from /etc/game-narrator/env.example, mode 0600
systemctl start game-narrator
game-narrator doctor                 # data dir pinned to /var/lib/game-narrator by the unit
```

`/etc/game-narrator/config.yaml` and `env` are `config|noreplace` /
admin-created, so upgrades preserve them (docs/release.md §3).

## Verification

- `curl -s http://127.0.0.1:8080/api/v1/health/live` and `.../health/ready`
  return 200; `/diagnostics` shows build version/commit, Go version,
  `os`/`arch` and readiness read live from the API.
- `bin/game-narrator doctor` exits 0 (no check FAILed). Missing optional
  components are reported `NOT CONFIGURED`, which is not a failure.
- `bin/game-narrator admin list` shows the new account.

## What NOT to do

- Never commit `.env` or any token; `.env` is gitignored, and every value in
  `.env.example` is a placeholder on a `.invalid` host.
- Never put credentials in a URL env var — the loader rejects userinfo in
  `GAME_NARRATOR_GATEWAY_URL` and the role `_URL` overrides; use the token
  env var instead.
- Do not run `serve` on an empty data directory before `migrate`.
- Do not treat `NOT CONFIGURED` model roles as a broken boot; the service is
  designed to run with unconfigured roles (04-inference-gateway-connection).
