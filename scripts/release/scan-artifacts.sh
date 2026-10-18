#!/usr/bin/env bash
# scan-artifacts.sh — release artifact scanner (AGENTS.md rule 5, docs/spec/15
# "Publishing restrictions", ADR 0033, ADR 0053).
#
# Fails the release when any release artifact in $DIST_DIR (tar.gz archives,
# .deb packages, .pkg.tar.zst Arch packages) or a docker image (layer contents)
# contains:
#   1. files matching game/voice/private name patterns, or
#   2. content matching secret/topology patterns.
#
# Home-path rule: a bare "/home/" substring is NOT sufficient (the support
# bundle's redactor legitimately carries the literal "/home/" inside its own
# regex source, and so would any shipped scanner pattern). The rule is
# "/home/" + a plausible username + a path separator, i.e. an absolute home
# path of a real user leaked into an artifact.
#
# Modes: run plain (default) or with --self-test, which builds a planted
# /home/<user> fixture (must fail) and a redactor-pattern fixture (must
# pass) in a temp dir and asserts both outcomes.
# Image scan: when docker is not usable, image layers are skipped with an
# explicit "NOT RUN: image scan (docker unavailable)" — never a failure. An
# image ref that does not exist locally is likewise NOT RUN (the build stage
# owns building; a missing image says nothing about artifact contents).
# Group membership is per-login session: if GN_SCAN_IMAGE is set but the
# session lacks the docker group (the user may still be a member), the
# scanner re-execs itself under the group (sg, or newgrp where sg is absent)
# so every docker call below is a plain `docker` — the same pattern
# smoke.sh uses.
#
# Environment (all optional; none of them is ever written into the repo):
#   GN_SCAN_DIST    artifacts directory (default: dist)
#   GN_SCAN_IMAGE   space-separated docker image refs to scan (default: none —
#                   image scan is skipped with a notice; make release-snapshot
#                   passes the local snapshot image)
#   GN_GATEWAY_HOSTNAME   private inference-gateway hostname to search for
#   GN_GATEWAY_TOKEN_PREFIX  gateway token prefix pattern to search for
#
# Token handling: a `GAME_NARRATOR_GATEWAY_TOKEN=` line in an artifact is
# treated as a real secret UNLESS the value is empty or exactly
# `replace-with-real-secret-outside-git` (the placeholder used in
# examples/config/env.example). Any config template shipped with a release
# must use that exact placeholder — other values fail the build deliberately
# (conservative: an unknown value could be a real token).
#
# Exit codes: 0 clean, 1 contamination found (or self-test assertion failed),
#         2 usage/environment error.
set -euo pipefail
SELF="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/$(basename "${BASH_SOURCE[0]}")"

failures=0
note()  { printf 'scan: %s\n' "$*"; }
fail()  { printf 'SCAN FAIL: %s\n' "$*"; failures=1; }

# ---------------------------------------------------------------------------
# --self-test: build both fixtures in a temp dir and assert pass/fail.
#   bad:  a tarball with a planted /home/someuser/x (binary-aware content)
#         -> the scan MUST fail (the rule still catches real leaks).
#   good: a tarball containing the support-bundle redactor's home-path regex
#         source, which contains a bare "/home/" -> the scan MUST pass
#         (no false positive on the redactor's own pattern).
# ---------------------------------------------------------------------------
if [ "${1:-}" = "--self-test" ]; then
  WORK="$(mktemp -d "${TMPDIR:-/tmp}/gn-scan-selftest.XXXXXX")"
  trap 'rm -rf "$WORK"' EXIT

  note 'self-test: building fixtures in a temp dir'
  mkdir -p "$WORK/bad" "$WORK/good"
  # Planted real home path, embedded in a binary file (NUL bytes) to prove
  # the check is binary-aware (grep -a).
  { printf 'lead\0\0'; printf '/home/someuser/x'; printf '\0trail\n'; } > "$WORK/bad/planted"
  # The exact regex source the support-bundle redactor compiles in
  # (internal/support/redact.go: homePathPattern). It contains "/home/" but
  # not a real home path: after "/home/" comes "(", not a username.
  printf '%s\n' '(?i)/home/([a-z0-9][a-z0-9._-]*)((?:/[^ ]*)?)' > "$WORK/good/redactor-pattern"
  mkdir -p "$WORK/dist-bad" "$WORK/dist-good"
  tar -C "$WORK" -czf "$WORK/dist-bad/fixture.tar.gz" bad
  tar -C "$WORK" -czf "$WORK/dist-good/fixture.tar.gz" good

  st=0
  note 'self-test 1/2: planted /home/someuser/x must FAIL the scan'
  if GN_SCAN_DIST="$WORK/dist-bad" GN_SCAN_IMAGE= "$SELF"; then
    echo 'SELF-TEST FAIL: scan passed on the planted /home/someuser/x fixture (must fail)' >&2
    st=1
  else
    note 'self-test 1/2: OK — planted home path caught'
  fi

  note 'self-test 2/2: redactor /home/ regex source must PASS the scan'
  if GN_SCAN_DIST="$WORK/dist-good" GN_SCAN_IMAGE= "$SELF"; then
    note 'self-test 2/2: OK — redactor pattern not flagged'
  else
    echo 'SELF-TEST FAIL: scan failed on the redactor-pattern fixture (false positive)' >&2
    st=1
  fi

  if [ "$st" -eq 0 ]; then
    note 'self-test PASS — planted home paths are caught, the redactor pattern is not flagged'
    exit 0
  fi
  note 'self-test FAIL — see above' >&2
  exit 1
fi

DIST_DIR="${GN_SCAN_DIST:-dist}"
# Optional positional override of the artifacts directory (e.g.
# `scan-artifacts.sh /path/to/dist`); GN_SCAN_DIST still works for the
# make pipeline.
if [ "$#" -ge 1 ]; then
  DIST_DIR="$1"
fi
WORK="$(mktemp -d "${TMPDIR:-/tmp}/gn-scan.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

if [ ! -d "$DIST_DIR" ]; then
  echo "scan: no $DIST_DIR directory (GN_SCAN_DIST=$DIST_DIR)" >&2
  exit 2
fi

# ---------------------------------------------------------------------------
# 1. Forbidden file-name patterns (basename match, per docs/spec/15: games,
#    savestates, voice samples, private data, credentials).
# ---------------------------------------------------------------------------
FILENAME_GLOBS=(
  '*.iso' '*.gcm' '*.ciso' '*.rvz' '*.bmg' '*.arc' '*.szs'
  '*.dtm' '*.sav' '*.wav' '*.sqlite' '*.sqlite-wal' '*.sqlite-shm'
  '.env*' '*.pem' 'id_*'
)

match_forbidden_name() {
  local base="$1" g
  for g in "${FILENAME_GLOBS[@]}"; do
    # shellcheck disable=SC2254
    case "$base" in $g) return 0 ;; esac
  done
  return 1
}

check_tree() {
  # $1 = extracted artifact root; $2 = artifact label (for reporting)
  local root="$1" label="$2" f base
  while IFS= read -r -d '' f; do
    base="$(basename "$f")"
    if match_forbidden_name "$base"; then
      fail "$label: forbidden file name: $f"
    fi
  done < <(find "$root" -type f -print0)
}

# ---------------------------------------------------------------------------
# 2. Content patterns (fixed-string and ERE), binary-aware via grep -a.
#    Home-path absolute paths leak developer-host topology; the gateway
#    hostname (when provided via env) is private infrastructure; a gateway
#    token value (when a prefix is provided via env) is a credential. The
#    env variable NAME GAME_NARRATOR_GATEWAY_TOKEN is documented and may
#    appear, but a real value must not.
# ---------------------------------------------------------------------------
# Fixed-string patterns (matched with grep -F; env-provided values must stay
# literal — they may contain regex metacharacters).
CONTENT_STRINGS=()
[ -n "${GN_GATEWAY_HOSTNAME:-}" ] && CONTENT_STRINGS+=("$GN_GATEWAY_HOSTNAME")
[ -n "${GN_GATEWAY_TOKEN_PREFIX:-}" ] && CONTENT_STRINGS+=("$GN_GATEWAY_TOKEN_PREFIX")

# ERE patterns (matched with grep -E, binary-aware via -a). The home-path
# rule: "/home/" + a plausible username (1..32 chars, lowercase start) + a
# path separator. Catches /home/someuser/x; deliberately does NOT match a
# bare "/home/" such as the redactor's own regex source or distribution
# files like /usr/sbin/update-passwd.
CONTENT_REGEXES=('/home/[a-z_][a-z0-9_.-]{0,31}/')

check_content() {
  # $1 = extracted artifact root; $2 = artifact label
  local root="$1" label="$2" pat
  for pat in ${CONTENT_STRINGS[@]+"${CONTENT_STRINGS[@]}"}; do
    while IFS= read -r -d '' f; do
      if grep -aqF -- "$pat" "$f"; then
        fail "$label: content pattern '$pat' in $f"
      fi
    done < <(find "$root" -type f -print0)
  done
  for pat in "${CONTENT_REGEXES[@]}"; do
    while IFS= read -r -d '' f; do
      if grep -aqE -- "$pat" "$f"; then
        fail "$label: content pattern '$pat' in $f"
      fi
    done < <(find "$root" -type f -print0)
  done
  # GAME_NARRATOR_GATEWAY_TOKEN with a non-placeholder value
  while IFS= read -r -d '' f; do
    if grep -aqF -- "GAME_NARRATOR_GATEWAY_TOKEN=" "$f" \
      && grep -aF -- "GAME_NARRATOR_GATEWAY_TOKEN=" "$f" \
        | grep -av "replace-with-real-secret-outside-git" \
        | grep -av "GAME_NARRATOR_GATEWAY_TOKEN=$" \
        | grep -q .; then
      fail "$label: real gateway token value in $f"
    fi
  done < <(find "$root" -type f -print0)
}

extract_targz() {
  # $1 = archive, $2 = dest dir
  local dest="$2"
  mkdir -p "$dest"
  tar -xzf "$1" -C "$dest"
}

extract_deb() {
  # $1 = .deb, $2 = dest dir (data + control tarballs extracted separately)
  local dest="$2"
  mkdir -p "$dest/data" "$dest/control"
  dpkg-deb -x "$1" "$dest/data"
  dpkg-deb --ctrl-tarfile "$1" 2>/dev/null | tar -x -C "$dest/control" || true
}

extract_pkg() {
  # $1 = .pkg.tar.zst (Arch Linux package), $2 = dest dir
  # A .pkg.tar.zst is a single zstd-compressed tar holding the payload files
  # plus the package metadata entries (.PKGINFO, .MTREE, and .INSTALL when the
  # package ships install hooks). Extracting the whole tar scans payload AND
  # metadata — the .INSTALL hooks and the .PKGINFO depend list included
  # (docs/spec/15: no private topology or secrets in any release artifact).
  local dest="$2"
  mkdir -p "$dest"
  tar -xf "$1" -C "$dest"
}

extract_image() {
  # $1 = image ref, $2 = dest dir. docker save produces a tar of image
  # layers (classically <id>/layer.tar, containerd store: blobs/sha256/*);
  # layer files may be plain tar or gzip-compressed tar.
  local ref="$1" dest="$2" f magic
  mkdir -p "$dest"
  docker save "$ref" -o "$dest/image.tar"
  tar -xf "$dest/image.tar" -C "$dest"
  while IFS= read -r -d '' f; do
    case "$f" in
      */image.tar) continue ;;
      */manifest.json|*/repositories|*/index.json) continue ;;
    esac
    magic="$(head -c 2 "$f" | od -An -tx1 | tr -d ' \n')"
    case "$magic" in
      1f8b) # gzip — decompress in place (could be a tar)
        mkdir -p "$dest/extracted-$$-${f//\//_}"
        if zcat "$f" 2>/dev/null | tar -x -C "$dest/extracted-$$-${f//\//_}" 2>/dev/null; then
          rm -f "$f"
        else
          mv "$f" "$dest/extracted-$$-${f//\//_}/raw-file"
        fi
        ;;
    esac
  done < <(find "$dest" -type f -print0)
}

scan_dir() {
  local dir="$1" label="$2"
  note "scanning $label ($dir)"
  check_tree "$dir" "$label"
  check_content "$dir" "$label"
}

scan_image_owned() {
  # Docker image layers contain the distribution's own packages (ffmpeg,
  # tesseract, bookworm system files). Their content is the distribution's
  # supply chain, covered by the pinned base digest + SBOM, NOT by this
  # scanner: Debian files legitimately contain '/home/' strings (e.g.
  # /usr/sbin/update-passwd, /etc/openal/alsoft.conf) and would false-positive.
  # We scan exactly the file this product owns in the image: the
  # game-narrator binary (the only thing our Dockerfile COPYs).
  local dest="$1" label="$2" f owned=0
  while IFS= read -r -d '' f; do
    [ "$(basename "$f")" = "game-narrator" ] || continue
    case "$f" in */game-narrator) : ;; *) continue ;; esac
    owned=1
    note "image-owned binary: $f"
    # content check on the binary itself
    local pat
    for pat in ${CONTENT_STRINGS[@]+"${CONTENT_STRINGS[@]}"}; do
      if grep -aqF -- "$pat" "$f"; then
        fail "$label: content pattern '$pat' in owned image file $f"
      fi
    done
    for pat in "${CONTENT_REGEXES[@]}"; do
      if grep -aqE -- "$pat" "$f"; then
        fail "$label: content pattern '$pat' in owned image file $f"
      fi
    done
    if grep -aqF -- "GAME_NARRATOR_GATEWAY_TOKEN=" "$f" \
      && grep -aF -- "GAME_NARRATOR_GATEWAY_TOKEN=" "$f" \
        | grep -av "replace-with-real-secret-outside-git" \
        | grep -av "GAME_NARRATOR_GATEWAY_TOKEN=$" \
        | grep -q .; then
      fail "$label: real gateway token value in $f"
    fi
  done < <(find "$dest" -type f -print0)
  [ "$owned" -eq 1 ] || fail "$label: no owned game-narrator binary found in image layers (layer extraction broken?)"
}

# ---------------------------------------------------------------------------
# Docker availability. Plain `docker info` first; if the session lacks the
# docker group while the user is a member, re-exec the whole scanner under
# the group when an image scan was requested (sg, or newgrp where sg is not
# installed). After the re-exec every docker call below is plain `docker`.
# ---------------------------------------------------------------------------
have_docker() { docker info >/dev/null 2>&1; }

if [ -n "${GN_SCAN_IMAGE:-}" ] \
   && [ -z "${GN_SCAN_IN_DOCKER_GROUP:-}" ] \
   && ! have_docker; then
  if command -v sg >/dev/null 2>&1 && sg docker -c 'docker info' >/dev/null 2>&1; then
    export GN_SCAN_IN_DOCKER_GROUP=1
    exec sg docker -c "exec $0 $*"
  elif command -v newgrp >/dev/null 2>&1 && newgrp docker -c 'docker info' >/dev/null 2>&1; then
    export GN_SCAN_IN_DOCKER_GROUP=1
    exec newgrp docker -c "exec $0 $*"
  fi
fi

# ---------------------------------------------------------------------------
# Run the scan over every artifact.
# ---------------------------------------------------------------------------
shopt -s nullglob
for archive in "$DIST_DIR"/*.tar.gz; do
  label="$(basename "$archive")"
  dest="$WORK/$(echo "$label" | tr '/' '_')"
  if ! extract_targz "$archive" "$dest"; then
    fail "$label: cannot extract archive"
    continue
  fi
  scan_dir "$dest" "$label"
done

for pkg in "$DIST_DIR"/*.deb; do
  label="$(basename "$pkg")"
  dest="$WORK/$(echo "$label" | tr '/' '_')"
  if ! extract_deb "$pkg" "$dest"; then
    fail "$label: cannot extract package"
    continue
  fi
  scan_dir "$dest" "$label"
done

for pkg in "$DIST_DIR"/*.pkg.tar.zst; do
  label="$(basename "$pkg")"
  dest="$WORK/$(echo "$label" | tr '/' '_')"
  if ! extract_pkg "$pkg" "$dest"; then
    fail "$label: cannot extract package"
    continue
  fi
  scan_dir "$dest" "$label"
done

if [ -n "${GN_SCAN_IMAGE:-}" ]; then
  if ! have_docker; then
    # Unusable docker is NOT a scan failure: the build stage decided not to
    # (or could not) build images, and artifact files are still scanned.
    note "NOT RUN: image scan (docker unavailable)"
  else
    for ref in $GN_SCAN_IMAGE; do
      label="$ref"
      if ! docker image inspect "$ref" >/dev/null 2>&1; then
        note "NOT RUN: image scan $ref (image not built locally)"
        continue
      fi
      dest="$WORK/img-$(echo "$ref" | tr '/:@' '___')"
      if ! extract_image "$ref" "$dest"; then
        fail "$label: cannot save/extract image"
        continue
      fi
      scan_image_owned "$dest" "$label"
    done
  fi
else
  note "GN_SCAN_IMAGE not set: skipping docker image layer scan"
fi

# ---------------------------------------------------------------------------
if [ "$failures" -ne 0 ]; then
  echo "scan: FAIL — contaminated artifacts found (see above)" >&2
  exit 1
fi
note "PASS — no forbidden files or content patterns found"
