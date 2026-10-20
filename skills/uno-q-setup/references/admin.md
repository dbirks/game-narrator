# Admin bootstrap and UI access

The API is **loopback-bound by design** (docs/spec/10). Until an admin
account exists, `GET /api/v1/auth/status` reports `setup_required`.
Manual 19 step 7 is the operator version; the condensed path:

## First-run admin (from the board)

The setup endpoint is loopback-only and the CLI runs directly against
the database — no running server needed:

```sh
printf '%s' '<password>' | sudo -u game-narrator \
  game-narrator admin create --data-dir /var/lib/game-narrator --username admin --role admin
```

Run as the `game-narrator` user so the SQLite file it touches is owned
by the service user. The password is piped from stdin (the only form
that works over ssh); the CLI's password policy is length-based — do
not strip whitespace. Or, with the SSH tunnel from below, create the
admin in the browser at `/setup`. Afterwards log in at `/login`; the
diagnostics page is `/diagnostics`.

## Operator token for the web UI

A long-lived (or bounded) token for an account that already exists:

```sh
sudo -u game-narrator game-narrator admin token \
  --data-dir /var/lib/game-narrator --username admin --name uno-q-operator \
  --expires-days 365
```

`--expires-days 0` = never expires (keep the bound if the operator
prefers a rotating credential). The token is printed once, to stdout —
deliver it out of band, never into the repo, an issue or a screenshot
(AGENTS.md rule 5). A token may not exceed its owner's role.

## Reaching the UI from the LAN, safely

Preferred (no configuration change — the loopback-only first-run
endpoint stays loopback-only):

```sh
ssh -N -L 8080:127.0.0.1:8080 <user>@<uno-q-host>
```

then open <http://127.0.0.1:8080> on the laptop.

Alternative — bind the LAN and accept the exposure: set
`GAME_NARRATOR_LISTEN=0.0.0.0:8080` in `/etc/game-narrator/env`,
`sudo systemctl daemon-reload && sudo systemctl restart game-narrator`,
open `http://<uno-q-host>:8080`. docs/spec/10 "Security defaults for
LAN deployment" recommends a TLS reverse proxy / Tailscale over a bare
LAN bind; set `GAME_NARRATOR_TRUSTED_PROXY_CIDRS` for the proxy that
terminates TLS (see `game-narrator serve --help`).

## Smoke after bootstrap

1. `/diagnostics` shows build version/commit, Go version, `os`/`arch`
   and readiness live.
2. `/api/v1/auth/status` no longer reports `setup_required`.
3. Log in as the admin; the operator token authenticates the web UI.
4. `game-narrator doctor` (env sourced) is green or self-explaining.
