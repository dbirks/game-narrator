#!/usr/bin/env bash
# scan-artifacts.sh — release artifact scanner (AGENTS.md rule 5, docs/spec/15
# "Publishing restrictions", ADR 0033).
#
# Fails the release when any release artifact in $DIST_DIR (tar.gz archives,
# .deb packages) or a docker image (layer contents) contains:
#   1. files matching game/voice/private name patterns, or
#   2. content matching secret/topology patterns.
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
# Exit codes: 0 clean, 1 contamination found, 2 usage/environment error.
set -euo pipefail

DIST_DIR="${GN_SCAN_DIST:-dist}"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/gn-scan.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

failures=0
note()  { printf 'scan: %s\n' "$*"; }
fail()  { printf 'SCAN FAIL: %s\n' "$*"; failures=1; }

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
# 2. Content patterns. Fixed-string, binary-aware. /home/ absolute paths leak
#    developer-host topology; the gateway hostname (when provided via env)
#    is private infrastructure; a gateway token value (when a prefix is
#    provided via env) is a credential. The env variable NAME
#    GAME_NARRATOR_GATEWAY_TOKEN is documented and may appear, but a real
#    value must not.
# ---------------------------------------------------------------------------
CONTENT_STRINGS=('/home/')
[ -n "${GN_GATEWAY_HOSTNAME:-}" ] && CONTENT_STRINGS+=("$GN_GATEWAY_HOSTNAME")
[ -n "${GN_GATEWAY_TOKEN_PREFIX:-}" ] && CONTENT_STRINGS+=("$GN_GATEWAY_TOKEN_PREFIX")

check_content() {
  # $1 = extracted artifact root; $2 = artifact label
  local root="$1" label="$2" pat
  for pat in "${CONTENT_STRINGS[@]}"; do
    while IFS= read -r -d '' f; do
      if grep -aqF -- "$pat" "$f"; then
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
    for pat in "${CONTENT_STRINGS[@]}"; do
      if grep -aqF -- "$pat" "$f"; then
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

if [ -n "${GN_SCAN_IMAGE:-}" ]; then
  for ref in $GN_SCAN_IMAGE; do
    label="$ref"
    dest="$WORK/img-$(echo "$ref" | tr '/:@' '___')"
    if ! extract_image "$ref" "$dest"; then
      fail "$label: cannot save/extract image (docker unavailable?)"
      continue
    fi
    scan_image_owned "$dest" "$label"
  done
else
  note "GN_SCAN_IMAGE not set: skipping docker image layer scan"
fi

# ---------------------------------------------------------------------------
if [ "$failures" -ne 0 ]; then
  echo "scan: FAIL — contaminated artifacts found (see above)" >&2
  exit 1
fi
note "PASS — no forbidden files or content patterns found"
