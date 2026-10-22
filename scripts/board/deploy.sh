#!/usr/bin/env bash
# deploy.sh — deploy a locally built game-narrator to a UNO Q board.
#
# Codifies the 2026-10-21 board incident (ADR-0150):
#   * A board carried TWO installs (the packaged systemd unit and a hand-run
#     user-level process). A deploy targeted one while the other held the
#     listen port, so the new build never served and the unit crash-looped.
#     This script refuses to continue while the OTHER install is active and
#     prints the one command that stops it.
#   * A `set -e` deploy aborted at the data backup because `tar czf` exits 1
#     when a file changes while being read (the service is live). The backup
#     below treats tar rc=1 as a warning and rc>1 as a stop.
#   * The old process is stopped by exact PID from `pgrep -x game-narrator`.
#     Never `pkill -f "game-narrator serve"`: the pattern matches the ssh
#     shell running the deploy itself.
#
# Usage:
#   GN_BOARD_SSH=user@host ./scripts/board/deploy.sh --user
#   GN_BOARD_SSH=user@host ./scripts/board/deploy.sh --systemd
#   GN_BOARD_SSH=user@host ./scripts/board/deploy.sh --user --dry-run
#
#   --user      the $HOME/gn-live layout (env file ~/gn-live/env, data in
#               ~/gn-live/data, started with setsid nohup)
#   --systemd   the packaged unit game-narrator.service (User=game-narrator,
#               EnvironmentFile=/etc/game-narrator/env, data in
#               /var/lib/game-narrator)
#   --dry-run   print every step; touch neither the repo nor the board
#
# Environment:
#   GN_BOARD_SSH   ssh target "user@host" — required, never defaulted
#   GN_VERSION     version stamp (default 0.1.0-<short commit>)
#   GN_SHA         short commit stamp (default: this checkout, else "unknown")
#
# No board address appears in this file; the target comes only from
# GN_BOARD_SSH. The health probe uses loopback on the board itself.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
REPO_ROOT=$(cd "$SCRIPT_DIR/../.." && pwd)
cd "$REPO_ROOT"

MODE=""
DRY=0
usage() {
  echo "usage: GN_BOARD_SSH=user@host $0 (--user | --systemd) [--dry-run]"
  echo "  --user      deploy the user-level install (\$HOME/gn-live layout)"
  echo "  --systemd   deploy the packaged systemd unit (game-narrator.service)"
  echo "  --dry-run   print every step without building, copying or touching the board"
  exit 2
}
while [ $# -gt 0 ]; do
  case "$1" in
  --user) [ -n "$MODE" ] && { echo "choose exactly one mode" >&2; exit 2; }; MODE="user" ;;
  --systemd) [ -n "$MODE" ] && { echo "choose exactly one mode" >&2; exit 2; }; MODE="systemd" ;;
  --dry-run) DRY=1 ;;
  -h|--help) usage ;;
  *) echo "unknown flag: $1" >&2; usage ;;
  esac
  shift
done
[ -n "$MODE" ] || usage

HOST=${GN_BOARD_SSH:?GN_BOARD_SSH must be set to user@host; the deploy target is never defaulted}
SHA=${GN_SHA:-$(git rev-parse --short HEAD 2>/dev/null || echo unknown)}
VERSION=${GN_VERSION:-0.1.0-$SHA}
OUTBIN="$REPO_ROOT/bin/deploy-game-narrator-$VERSION"
REMBIN="/tmp/game-narrator-$VERSION"
HEALTH_PATH="/api/v1/health/live"

log() { printf '[deploy %s] %s\n' "$MODE" "$*"; }
die() { printf '[deploy %s] ERROR: %s\n' "$MODE" "$*" >&2; exit 1; }

# run_remote TITLE REMOTE-ARG <<'EOF' ... EOF — run the heredoc script on the
# board via `sh -s REMOTE-ARG` (the quoted heredoc is unexpanded, so $ signs
# belong to the board), or print it under --dry-run. The argument carries the
# one value the step needs (the staged binary path for the install step,
# "" elsewhere); the script reads it as $1. ssh/scp use BatchMode: the
# deploy must fail loudly, never prompt.
run_remote() {
  local title=$1 rarg=$2 script
  script=$(cat)
  if [ "$DRY" -eq 1 ]; then
    printf 'DRY-RUN: %s\n%s\n\n' "$title" "$script"
    return 0
  fi
  log "$title"
  ssh -o BatchMode=yes "$HOST" sh -s "$rarg" <<<"$script"
}

# ---------------------------------------------------------------------------
log "host: $HOST (from GN_BOARD_SSH), mode: $MODE, version: $VERSION"

if [ "$DRY" -eq 0 ]; then
  [ -d internal/webassets/dist ] && [ -n "$(ls -A internal/webassets/dist 2>/dev/null)" ] \
    || die "web assets are not staged into internal/webassets/dist — run 'make web-build' first (the binary embeds them)"
fi

# 1. Build the arm64 binary with the version/commit stamp (cross-build from
#    the dev host; web assets must already be embedded via make web-build).
if [ "$DRY" -eq 1 ]; then
  printf 'DRY-RUN: build arm64 binary\n  CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-X github.com/dbirks/game-narrator/internal/bootstrap.version=%s -X github.com/dbirks/game-narrator/internal/bootstrap.commit=%s" -o %s ./cmd/game-narrator\n\n' \
    "$VERSION" "$SHA" "$OUTBIN"
else
  log "building arm64 binary"
  CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath \
    -ldflags "-X github.com/dbirks/game-narrator/internal/bootstrap.version=$VERSION -X github.com/dbirks/game-narrator/internal/bootstrap.commit=$SHA" \
    -o "$OUTBIN" ./cmd/game-narrator
fi

# 2. Copy the binary to the board.
if [ "$DRY" -eq 1 ]; then
  printf 'DRY-RUN: scp the binary\n  scp %s %s:%s\n\n' "$OUTBIN" "$HOST" "$REMBIN"
else
  log "copying the binary to the board"
  scp -o BatchMode=yes "$OUTBIN" "$HOST:$REMBIN"
fi

if [ "$MODE" = "user" ]; then
  # 3. Preflight; create the ~/gn-live layout if this is the first user-level
  #    install on the board (the layout the hand-run recipe uses).
  run_remote "preflight: arm64, passwordless sudo, ~/gn-live layout (created if absent)" "" <<'REMOTE'
set -eu
[ "$(uname -m)" = "aarch64" ] || { echo "board is not arm64: $(uname -m)"; exit 1; }
sudo -n true || { echo "passwordless sudo is not available (sudo -n true fails)"; exit 1; }
if [ ! -d "$HOME/gn-live" ]; then
  echo "note: creating $HOME/gn-live (user-level install layout)"
  mkdir -p "$HOME/gn-live/data"
fi
if [ ! -f "$HOME/gn-live/env" ]; then
  echo "note: creating $HOME/gn-live/env (data dir, listen default, placeholder gateway keys)"
  printf 'GAME_NARRATOR_DATA_DIR=%s/gn-live/data\nGAME_NARRATOR_LISTEN=127.0.0.1:8080\n# GAME_NARRATOR_GATEWAY_URL=  (placeholder; docs/manuals/04)\n# GAME_NARRATOR_GATEWAY_TOKEN= (placeholder; never in the repo)\n' "$HOME" >"$HOME/gn-live/env"
  chmod 600 "$HOME/gn-live/env"
fi
REMOTE

  # 4. The incident: refuse while the OTHER install (the systemd unit) is
  #    active, and print the one command that disables it.
  run_remote "refuse if the OTHER install (the systemd unit) is active" "" <<'REMOTE'
if command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet game-narrator 2>/dev/null; then
  echo "REFUSED: the systemd unit game-narrator is active and would hold the listen port."
  echo "Stop it first (the one command): sudo systemctl disable --now game-narrator"
  exit 1
fi
echo "ok: the systemd unit is not active"
REMOTE

  # 5. Data backup: tolerant of a live service (tar rc=1 warns, rc>1 stops)
  #    and of the ever-changing dev-trace blobs (excluded).
  run_remote "back up ~/gn-live/data (tolerant tar, dev-trace excluded)" "" <<'REMOTE'
set -eu
cd "$HOME/gn-live"
if [ -d data ]; then
  ts=$(date -u +%Y%m%dT%H%M%SZ)
  rc=0
  tar czf "data-backup-$ts.tar.gz" --exclude=data/dev-trace data 2>tar-backup.log || rc=$?
  if [ "$rc" -gt 1 ]; then echo "backup FAILED (tar rc=$rc): $(tail -n 5 tar-backup.log)"; exit 1; fi
  if [ "$rc" -eq 1 ]; then echo "note: tar rc=1 (file changed as we read it) — expected for a live service, backup usable"; fi
else
  echo "note: no data directory yet; nothing to back up"
fi
REMOTE

  # 6. Stop the user-level process(es) by exact PID. The refuse step above
  #    guarantees the unit is not active, so every `pgrep -x game-narrator`
  #    match here is the user-level install. Never `pkill -f`: the pattern
  #    matches the ssh shell running this deploy.
  run_remote "stop the user-level process(es) by exact PID (pgrep -x, never pkill -f)" "" <<'REMOTE'
set -eu
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
if systemctl --user is-enabled --quiet game-narrator.service 2>/dev/null; then
  # The user-level unit (packaging/systemd/user/game-narrator.service) owns
  # the process: stop it through systemd so Restart= does not revive it.
  systemctl --user stop game-narrator.service || true
fi
set -- $(pgrep -x game-narrator || true)
if [ "$#" -eq 0 ]; then echo "note: no game-narrator process running"; exit 0; fi
echo "stopping PID(s): $*"
for p in "$@"; do kill "$p" 2>/dev/null || true; done
i=0
while :; do
  alive=0
  for p in "$@"; do kill -0 "$p" 2>/dev/null && alive=1; done
  [ "$alive" -eq 0 ] && break
  i=$((i + 1))
  [ "$i" -ge 30 ] && break
  sleep 0.5
done
for p in "$@"; do
  if kill -0 "$p" 2>/dev/null; then echo "PID $p ignored SIGTERM; sending SIGKILL"; kill -9 "$p"; fi
done
sleep 0.2
echo stopped
REMOTE

  # 7. Keep the previous binary for rollback, install the new one.
  run_remote "keep the previous binary, install the new one (sudo -n install)" "$REMBIN" <<'REMOTE'
set -eu
if [ -f /usr/bin/game-narrator ]; then
  sudo -n install -m 755 /usr/bin/game-narrator /usr/bin/game-narrator.prev
  echo "previous binary kept at /usr/bin/game-narrator.prev (rollback: sudo -n install -m 755 /usr/bin/game-narrator.prev /usr/bin/game-narrator)"
fi
sudo -n install -m 755 "$1" /usr/bin/game-narrator
rm -f "$1"
REMOTE

  # 8. Start the user-level service the way the working recipe does.
  run_remote "start the user-level service (setsid nohup, env sourced)" "" <<'REMOTE'
cd "$HOME/gn-live"
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
if systemctl --user is-enabled --quiet game-narrator.service 2>/dev/null; then
  # Boot-persistent: the user unit (with lingering) restarts it after a
  # reboot; a setsid process would not.
  systemctl --user start game-narrator.service
  sleep 1
  echo "started via systemctl --user (pid $(pgrep -x -u "$(id -u)" game-narrator | tr '\n' ' '))"
  exit 0
fi
echo "note: no user unit installed — starting with setsid; it will NOT survive a reboot (install packaging/systemd/user/game-narrator.service, docs/manuals/19)"
set -a
. ./env
set +a
setsid nohup /usr/bin/game-narrator serve >>serve.log 2>&1 </dev/null &
# setsid forks, so $! is the short-lived setsid wrapper; report the real
# server process once it exists.
sleep 1
echo "started (pid $(pgrep -x -u "$(id -u)" game-narrator | tr '\n' ' '))"
REMOTE
else
  # 3. Preflight for the packaged unit.
  run_remote "preflight: arm64, passwordless sudo, the unit exists" "" <<'REMOTE'
set -eu
[ "$(uname -m)" = "aarch64" ] || { echo "board is not arm64: $(uname -m)"; exit 1; }
sudo -n true || { echo "passwordless sudo is not available (sudo -n true fails)"; exit 1; }
[ -f /etc/systemd/system/game-narrator.service ] || { echo "game-narrator.service is not installed; install the .deb first (manual 19)"; exit 1; }
REMOTE

  # 4. Refuse while the OTHER install (a user-level process run by the ssh
  #    login user) is running. The unit itself runs as its own service user,
  #    so `pgrep -x -u $USER` sees the hand-started process only.
  run_remote "refuse if the OTHER install (a user-level process) is running" "" <<'REMOTE'
pids=$(pgrep -x -u "$USER" game-narrator 2>/dev/null || true)
if [ -n "$pids" ]; then
  echo "REFUSED: a user-level game-narrator (PID(s) $pids) is running and would hold the listen port."
  echo "Stop it first (the one command): kill $(pgrep -x -u $USER game-narrator)"
  exit 1
fi
echo "ok: no user-level game-narrator process"
REMOTE

  # 5. Data backup as root (the data dir is /var/lib/game-narrator; the
  #    backup lands next to it, outside the tree it archives).
  run_remote "back up /var/lib/game-narrator (as root; tolerant tar)" "" <<'REMOTE'
set -eu
rc=0
sudo -n sh -c 'set -e; cd /var/lib; ts=$(date -u +%Y%m%dT%H%M%SZ); rc=0; tar czf "game-narrator-data-backup-$ts.tar.gz" --exclude=game-narrator/dev-trace game-narrator 2>backup.log || rc=$?; if [ "$rc" -gt 1 ]; then echo "backup FAILED (tar rc=$rc): $(tail -n 5 backup.log)"; exit 1; fi; if [ "$rc" -eq 1 ]; then echo "note: tar rc=1 (file changed as we read it) — expected for a live service, backup usable"; fi' || rc=$?
if [ "$rc" -ne 0 ]; then echo "data backup failed"; exit 1; fi
REMOTE

  # 6. Stop the unit — it is this install's own port holder.
  run_remote "stop the unit (it owns the port for this install)" "" <<'REMOTE'
sudo -n systemctl stop game-narrator
echo "unit stopped"
REMOTE

  # 7. Keep the previous binary, install the new one.
  run_remote "keep the previous binary, install the new one (sudo -n install)" "$REMBIN" <<'REMOTE'
set -eu
if [ -f /usr/bin/game-narrator ]; then
  sudo -n install -m 755 /usr/bin/game-narrator /usr/bin/game-narrator.prev
  echo "previous binary kept at /usr/bin/game-narrator.prev (rollback: sudo -n install -m 755 /usr/bin/game-narrator.prev /usr/bin/game-narrator)"
fi
sudo -n install -m 755 "$1" /usr/bin/game-narrator
rm -f "$1"
REMOTE

  # 8. Start the unit.
  run_remote "start the unit" "" <<'REMOTE'
sudo -n systemctl enable --now game-narrator
echo "unit started"
REMOTE
fi

# 9. Wait for the health endpoint, then print the running version. The
#    probe uses the listen address the mode's own env file configures (or
#    the documented default), always loopback on the board itself.
# The values below keep $HOME quoted so it expands on the BOARD (the local
# shell must not), which shellcheck reads as unexpanded text.
# shellcheck disable=SC2016
case "$MODE" in
user) envfile='"$HOME/gn-live/env"'; logcmd='tail -n 30 "$HOME/gn-live/serve.log"' ;;
systemd) envfile='"/etc/game-narrator/env"'; logcmd='sudo -n journalctl -u game-narrator -n 30 --no-pager' ;;
esac
run_remote "wait for $HEALTH_PATH (up to 30 s), then print the running version" "" <<REMOTE
set -eu
addr=
if [ -f $envfile ]; then
  addr=\$(grep -E '^[[:space:]]*GAME_NARRATOR_LISTEN=' $envfile 2>/dev/null | tail -n 1 | cut -d= -f2- || true)
fi
[ -n "\$addr" ] || addr=127.0.0.1:8080
echo "probing http://\$addr$HEALTH_PATH"
i=0
while :; do
  if curl -fsS "http://\$addr$HEALTH_PATH" >/dev/null 2>&1; then echo healthy; break; fi
  i=\$((i + 1))
  if [ "\$i" -ge 30 ]; then
    echo "health probe failed after 30 s; recent log:"
    $logcmd 2>/dev/null || true
    exit 1
  fi
  sleep 1
done
/usr/bin/game-narrator version
REMOTE

if [ "$DRY" -eq 1 ]; then
  printf 'DRY-RUN: complete — nothing was built, copied or touched on %s.\n' "$HOST"
else
  log "deploy of $VERSION finished: the board answered $HEALTH_PATH and printed its own version"
  rm -f "$OUTBIN"
fi
