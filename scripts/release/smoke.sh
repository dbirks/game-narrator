#!/usr/bin/env bash
# smoke.sh — post-snapshot verification (docs/spec/15 "Release gate matrix",
# AGENTS.md "Minimum quality policy", ADR 0033).
#
# Runs AFTER `goreleaser release --snapshot --clean` (called by
# `make release-snapshot`). Steps:
#   1. artifact scanner (scripts/release/scan-artifacts.sh)
#   2. amd64 binary from the archive: `version`, `serve` on a temp port with a
#      temp data dir -> /api/v1/health/ready 200, /api/v1/health/live 200,
#      /diagnostics HTML from the embedded UI
#   3. arm64 binary: `file` shows ARM aarch64, statically linked
#   4. .deb: dpkg-deb --info / --contents sanity (unit + config paths/modes)
#   5. if docker is available: install the .deb in a debian:bookworm-slim
#      container (real apt deps) and run `game-narrator version` + `migrate`,
#      plus a static check of the shipped unit file (no systemd in the
#      container);
#      docker run the amd64 image, hit /api/v1/health/live, assert non-root
#      user; inspect the image (snapshot: per-arch local images, tagged
#      release: manifest list)
#
# Unavailable tools are reported as NOT RUN with a reason — never as pass.
# Exit: 0 all performed steps passed, 1 a performed step failed, 2 usage error.
set -euo pipefail
cd "$(dirname "$0")/../.."

DIST_DIR="${GN_SCAN_DIST:-dist}"

# Docker group handling: group membership is per-login session. If this
# session lacks the docker group but the user is a member, re-exec the whole
# script under it so every docker call below is a plain `docker`. Prefer sg
# (it preserves the full environment); fall back to newgrp where sg is not
# installed.
if [ -z "${GN_SMOKE_IN_DOCKER_GROUP:-}" ] \
   && ! docker info >/dev/null 2>&1; then
  if command -v sg >/dev/null 2>&1 && sg docker -c 'docker info' >/dev/null 2>&1; then
    export GN_SMOKE_IN_DOCKER_GROUP=1
    exec sg docker -c "exec $0"
  elif command -v newgrp >/dev/null 2>&1 && newgrp docker -c 'docker info' >/dev/null 2>&1; then
    export GN_SMOKE_IN_DOCKER_GROUP=1
    exec newgrp docker -c "exec $0"
  fi
fi

[ -d "$DIST_DIR" ] || { echo "smoke: no $DIST_DIR — run goreleaser release --snapshot first" >&2; exit 2; }

pass=0
notrun=0
step() { printf '\n=== %s ===\n' "$*"; }
ok()   { printf 'PASS: %s\n' "$*"; pass=$((pass+1)); }
skip() { printf 'NOT RUN: %s\n' "$*"; notrun=$((notrun+1)); }

http_get() { # $1=url ; prints body, returns curl/wget status
  if command -v curl >/dev/null 2>&1; then
    curl -fsS -m 10 "$1"
  else
    wget -qO- -T 10 "$1"
  fi
}

http_code() { # $1=url
  if command -v curl >/dev/null 2>&1; then
    curl -s -o /dev/null -w '%{http_code}' -m 10 "$1"
  else
    wget -S -O /dev/null -T 10 "$1" 2>&1 | grep -oE '[1-5][0-9][0-9]' | tail -1
  fi
}

# docker wrapper (re-exec above guarantees plain docker access if the user
# has docker group membership; otherwise have_docker reports NOT RUN).
have_docker() {
  docker info >/dev/null 2>&1
}
d() { docker "$@"; }

# ---------------------------------------------------------------------------
# Discover snapshot artifacts by name pattern (never assume the exact tag).
# ---------------------------------------------------------------------------
AMD_TARBALL="$(find "$DIST_DIR" -maxdepth 1 -name 'game-narrator_*_linux_amd64.tar.gz' | head -1)"
ARM_TARBALL="$(find "$DIST_DIR" -maxdepth 1 -name 'game-narrator_*_linux_arm64.tar.gz' | head -1)"
AMD_DEB="$(find "$DIST_DIR" -maxdepth 1 -name '*_amd64.deb' | head -1)"
ARM_DEB="$(find "$DIST_DIR" -maxdepth 1 -name '*_arm64.deb' | head -1)"
[ -n "$AMD_TARBALL" ] || { echo "smoke: no amd64 tarball in $DIST_DIR" >&2; exit 2; }
[ -n "$ARM_TARBALL" ] || { echo "smoke: no arm64 tarball in $DIST_DIR" >&2; exit 2; }
[ -n "$AMD_DEB" ]     || { echo "smoke: no amd64 .deb in $DIST_DIR" >&2; exit 2; }

VER="$(basename "$AMD_TARBALL" | sed -E 's/^game-narrator_(.*)_(linux).*/\1/')"
# The dockers_v2 image tag base is exactly the version string (snapshot:
# 0.0.0-snapshot.<commit-ts>.g<sha>; tagged release: v1.2.3) — same string
# that names the tarballs, so it is recovered from the artifact name, not
# recomputed.
TAG="$VER"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/gn-smoke.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

# ---------------------------------------------------------------------------
# 1. Artifact scanner (also scans the snapshot image when docker is present).
# ---------------------------------------------------------------------------
step "artifact scanner"
if have_docker; then
  # Only hand the scanner images the build stage actually produced (per-arch
  # local images in snapshot mode). If none exist — e.g. the build ran
  # without docker — scanning them would die on "No such image" without
  # saying anything about artifact contents: report NOT RUN instead.
  SCAN_IMAGES=""
  for ref in "game-narrator:$TAG-amd64" "game-narrator:$TAG-arm64"; do
    if d image inspect "$ref" >/dev/null 2>&1; then
      SCAN_IMAGES="$SCAN_IMAGES $ref"
    else
      skip "image layer scan of $ref (image not built locally)"
    fi
  done
  if [ -n "$SCAN_IMAGES" ]; then
    GN_SCAN_IMAGE="$SCAN_IMAGES" ./scripts/release/scan-artifacts.sh
  else
    skip "artifact scanner image layer scan (no local images; dockers_v2 disabled or build without docker)"
    ./scripts/release/scan-artifacts.sh
  fi
else
  skip "artifact scanner image layer scan (docker unavailable); running file scans only"
  ./scripts/release/scan-artifacts.sh
fi
ok "artifact scanner clean"

# ---------------------------------------------------------------------------
# 2. amd64 binary: version + serve + health + embedded UI.
# ---------------------------------------------------------------------------
step "amd64 binary smoke (archive: $(basename "$AMD_TARBALL"))"
mkdir -p "$WORK/amd"
tar -xzf "$AMD_TARBALL" -C "$WORK/amd"
BIN_AMD="$WORK/amd/game-narrator"   # tarball root is flat (no project prefix)
chmod +x "$BIN_AMD"

"$BIN_AMD" version
ok "amd64 binary runs and prints version"

PORT=$((20000 + RANDOM % 20000))
DATA_DIR="$WORK/amd-data"
mkdir -p "$DATA_DIR"
"$BIN_AMD" serve --listen "127.0.0.1:$PORT" --data-dir "$DATA_DIR" >"$WORK/serve.log" 2>&1 &
SERVE_PID=$!
trap 'kill "$SERVE_PID" 2>/dev/null || true; rm -rf "$WORK"' EXIT
deadline=$((SECONDS + 60))
READY=""
while [ $SECONDS -lt $deadline ]; do
  READY="$(http_code "http://127.0.0.1:$PORT/api/v1/health/ready" 2>/dev/null || true)"
  [ "$READY" = "200" ] && break
  sleep 1
done
[ "$READY" = "200" ] || { echo "serve log:"; cat "$WORK/serve.log"; exit 1; }
ok "GET /api/v1/health/ready -> 200"

LIVE="$(http_code "http://127.0.0.1:$PORT/api/v1/health/live")"
[ "$LIVE" = "200" ] || { echo "live returned $LIVE"; exit 1; }
ok "GET /api/v1/health/live -> 200"

DIAG_BODY="$(http_get "http://127.0.0.1:$PORT/diagnostics")"
case "$DIAG_BODY" in
  *"<html"*|*"<!DOCTYPE html"*) : ;;
  *) echo "/diagnostics did not serve HTML: ${DIAG_BODY:0:200}"; exit 1 ;;
esac
ok "GET /diagnostics serves embedded UI HTML"

kill "$SERVE_PID" 2>/dev/null || true
wait "$SERVE_PID" 2>/dev/null || true

# ---------------------------------------------------------------------------
# 3. arm64 binary: architecture + static linkage.
# ---------------------------------------------------------------------------
step "arm64 binary (archive: $(basename "$ARM_TARBALL"))"
if command -v file >/dev/null 2>&1; then
  mkdir -p "$WORK/arm"
  tar -xzf "$ARM_TARBALL" -C "$WORK/arm"
  FILE_OUT="$(file "$WORK/arm/game-narrator")"
  echo "$FILE_OUT"
  case "$FILE_OUT" in
    *"ARM aarch64"*|"aarch64"*) : ;;
    *) echo "unexpected arm64 file output: $FILE_OUT"; exit 1 ;;
  esac
  case "$FILE_OUT" in
    *"statically linked"*) ok "arm64 binary is ARM aarch64, statically linked" ;;
    *) echo "arm64 binary is not statically linked: $FILE_OUT"; exit 1 ;;
  esac
else
  skip "arm64 file inspection (file(1) not installed)"
fi

# ---------------------------------------------------------------------------
# 4. .deb metadata (both architectures).
# ---------------------------------------------------------------------------
step ".deb metadata (amd64: $(basename "$AMD_DEB"))"
INFO="$(dpkg-deb --info "$AMD_DEB")"
echo "$INFO" | head -15
case "$INFO" in
  *"Package: game-narrator"*) : ;;
  *) echo "package name unexpected"; exit 1 ;;
esac
ok "dpkg-deb --info: Package game-narrator"

CONTENTS="$(dpkg-deb --contents "$AMD_DEB")"
echo "$CONTENTS" | grep -E 'systemd|/etc/game-narrator|/usr/bin' || true
echo "$CONTENTS" | grep -q '/lib/systemd/system/game-narrator.service' || { echo "unit file missing from .deb"; exit 1; }
echo "$CONTENTS" | grep -q '/etc/game-narrator/config.yaml'            || { echo "config missing from .deb"; exit 1; }
echo "$CONTENTS" | grep -q '/usr/bin/game-narrator'                    || { echo "binary missing from .deb"; exit 1; }
ok ".deb contents: binary, systemd unit and config present"

# Static data-dir mode contract (ADR 0063): the shipped unit and tmpfiles must
# create /var/lib/game-narrator at 0700. `game-narrator doctor` DEGRADEDs on
# 0750, so a package that shipped 0750 would install its own failure state —
# assert the contract from the package itself, not from the source tree.
mkdir -p "$WORK/deb-meta"
dpkg-deb --fsys-tarfile "$AMD_DEB" | tar -x -C "$WORK/deb-meta" ./lib/systemd/system/game-narrator.service ./usr/lib/tmpfiles.d/game-narrator.conf
grep -q '^StateDirectoryMode=0700$' "$WORK/deb-meta/lib/systemd/system/game-narrator.service" \
  || { echo "shipped unit does not pin StateDirectoryMode=0700"; exit 1; }
grep -Eq '^d /var/lib/game-narrator 0700 game-narrator:game-narrator -' "$WORK/deb-meta/usr/lib/tmpfiles.d/game-narrator.conf" \
  || { echo "shipped tmpfiles line is not 0700 for /var/lib/game-narrator"; exit 1; }
ok ".deb ships the 0700 data-dir contract (unit StateDirectoryMode + tmpfiles)"
[ -n "$ARM_DEB" ] && { dpkg-deb --info "$ARM_DEB" >/dev/null; ok "arm64 .deb is a valid package ($(basename "$ARM_DEB"))"; }

# ---------------------------------------------------------------------------
# 5. Docker: container install of the .deb + image runtime smoke.
# ---------------------------------------------------------------------------
if ! have_docker; then
  skip "docker container install of the .deb (docker unavailable)"
  skip "docker image runtime smoke (docker unavailable)"
else
  step "install .deb in debian:bookworm-slim container"
  d pull debian:bookworm-slim >/dev/null
  # shellcheck disable=SC2016 # $(...) and the $-anchored grep must expand in the CONTAINER shell, not on the host; no host variable may leak in
  d run --rm \
      -v "$PWD/$AMD_DEB:/pkg.deb:ro" \
      debian:bookworm-slim \
      sh -c 'set -e
        apt-get update -qq
        apt-get install -y -qq --no-install-recommends ca-certificates ffmpeg tesseract-ocr tesseract-ocr-eng v4l-utils
        dpkg -i /pkg.deb
        dpkg -s game-narrator | head -3
        game-narrator version
        game-narrator migrate --data-dir /var/lib/game-narrator
        ls -ld /var/lib/game-narrator
        # Data-dir mode contract (ADR 0063): a clean install must be 0700,
        # the same mode `game-narrator doctor` requires, so the package
        # cannot install its own DEGRADED state on the real device.
        DMODE="$(stat -c "%-a %U" /var/lib/game-narrator)"
        [ "$DMODE" = "700 game-narrator" ] || { echo "data dir stat: $DMODE, want 700 game-narrator"; exit 1; }
        # Static unit check (the container has no systemd): the shipped unit
        # must pin GAME_NARRATOR_DATA_DIR to the StateDirectory, otherwise the
        # binary would fall back to $HOME/.local/share/game-narrator.
        grep -q "^Environment=GAME_NARRATOR_DATA_DIR=/var/lib/game-narrator$" /lib/systemd/system/game-narrator.service'
  ok ".deb installed in bookworm container; version + migrate + 0700 data dir + unit data-dir pin (static) succeeded"

  step "image runtime smoke (game-narrator:$TAG-amd64)"
  if ! d image inspect "game-narrator:$TAG-amd64" >/dev/null 2>&1; then
    skip "image $TAG-amd64 not found locally (dockers_v2 disabled or image build failed — check goreleaser output)"
  else
    ARCH="$(d image inspect "game-narrator:$TAG-amd64" --format '{{.Architecture}}')"
    [ "$ARCH" = "amd64" ] || { echo "image arch=$ARCH, want amd64"; exit 1; }
    ok "local image $TAG-amd64 is amd64"

    ID_OUT="$(d run --rm --entrypoint game-narrator "game-narrator:$TAG-amd64" version)"
    echo "$ID_OUT"
    case "$ID_OUT" in *linux/amd64*) : ;; *) echo "image binary reports wrong platform: $ID_OUT"; exit 1 ;; esac
    ok "docker run: game-narrator version inside image"

    RUN_ID="$(d run -d -p 127.0.0.1:$((PORT + 1)):8080 "game-narrator:$TAG-amd64")"
    trap 'd rm -f "$RUN_ID" >/dev/null 2>&1 || true; kill "$SERVE_PID" 2>/dev/null || true; rm -rf "$WORK"' EXIT
    deadline=$((SECONDS + 60))
    CODE=""
    while [ $SECONDS -lt $deadline ]; do
      CODE="$(http_code "http://127.0.0.1:$((PORT + 1))/api/v1/health/live" 2>/dev/null || true)"
      [ "$CODE" = "200" ] && break
      sleep 1
    done
    [ "$CODE" = "200" ] || { d logs "$RUN_ID" | tail -20; exit 1; }
    ok "docker run: GET /api/v1/health/live -> 200"

    UID_IN_IMG="$(d exec "$RUN_ID" id -u)"
    [ "$UID_IN_IMG" != "0" ] || { echo "image runs as root"; exit 1; }
    ok "image process runs as non-root (uid $UID_IN_IMG)"

    # HEALTHCHECK transition (interval 30s): allow up to 90s to become healthy.
    deadline=$((SECONDS + 90))
    HSTATUS="starting"
    while [ $SECONDS -lt $deadline ]; do
      HSTATUS="$(d inspect "$RUN_ID" --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}')"
      case "$HSTATUS" in healthy|unhealthy) break ;; esac
      sleep 5
    done
    [ "$HSTATUS" = "healthy" ] || { echo "healthcheck status: $HSTATUS"; exit 1; }
    ok "docker HEALTHCHECK reached healthy"
    d rm -f "$RUN_ID" >/dev/null 2>&1 || true

    if d image inspect "game-narrator:$TAG-arm64" >/dev/null 2>&1; then
      ARCH64="$(d image inspect "game-narrator:$TAG-arm64" --format '{{.Architecture}}')"
      ok "local arm64 image present ($ARCH64) — snapshot mode loads per-arch images; tagged release builds the manifest list"
    else
      skip "arm64 local image not built (buildx cross-compile unavailable?); amd64 verified"
    fi
  fi
fi

# ---------------------------------------------------------------------------
echo
echo "smoke: $pass step(s) passed, $notrun step(s) NOT RUN"
if [ "$notrun" -gt 0 ]; then
  echo "smoke: NOT RUN steps above are reported, not passed (AGENTS.md: no fake greens)"
fi
exit 0
