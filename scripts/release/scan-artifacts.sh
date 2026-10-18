#!/usr/bin/env bash
# scan-artifacts.sh — release artifact scanner (AGENTS.md rule 5, docs/spec/15
# "Publishing restrictions", ADR 0033, ADR 0053).
#
# Fails the release when any release artifact in $DIST_DIR (tar.gz archives,
# .deb packages, .pkg.tar.zst Arch packages) or a docker image contains:
#   1. files matching game/voice/private name patterns, or
#   2. content matching secret/topology patterns.
#
# Rule split for docker images (ADR 0033 amendment, docs/release.md §2.5):
#   The image mixes the product's own files with the distribution's content
#   (the pinned debian base + the apt layer installed by packaging/Dockerfile).
#   The rule classes get different scope:
#
#   WHOLE-IMAGE rules — run over every file of the image's MERGED filesystem
#   (all layers in manifest order, whiteouts applied):
#     * forbidden-name: game/voice/private data file-name globs (a *.pem
#       without a PEM private-key marker is public-cert content — e.g. the
#       ~140 /etc/ssl/certs/*.pem the ca-certificates package installs at
#       build time — not a credential),
#     * game-data: game-image extensions and CISO/RARC/Yaz0/ISO-9660 magic
#       (catches game dumps hidden under innocent names),
#     * big-binary: non-text file >= 256 MiB. The largest legitimate file in
#       the pinned base + apt closure is libLLVM-15.so.1 at ~112 MiB, so the
#       threshold leaves >2x headroom; any larger blob in a release image is
#       leaked game/voice data or bloat, never a distro file.
#   These cannot false-positive on Debian: the crane-exported pinned base
#   (3265 files) and the apt closure of the six pinned packages (223 pkgs,
#   verified 2026-10) trigger none of them.
#
#   CONTENT rules (secret/topology: home paths, gateway hostname, gateway
#   token) — run ONLY over the files our Dockerfile adds, i.e. the paths the
#   Dockerfile COPYs (parsed from packaging/Dockerfile, WORKDIR-aware) and
#   the image config blob (labels/env/entrypoint). The base distribution
#   never writes those paths, so a base or apt file can never be
#   content-scanned. No hand-written Debian allowlist, no layer
#   classification, no digest matching.
#   Why the restriction is essential: the pinned debian base is 100% clean
#   (crane export, 3265 files, zero home-path hits), but the APT layer the
#   Dockerfile installs is NOT — Debian ships real "/home/<name>/" strings
#   (e.g. /etc/openal/alsoft.conf, /usr/share/doc/libasound2/examples/
#   asoundrc.txt, /usr/share/X11/xkb/symbols/se; 4 files in the 6-package
#   pinned closure, verified via crane, 2026-10). Those files sit in layers
#   the product builds, so a whole-image content scan would fail every
#   release. Distribution content is the distribution's supply chain,
#   covered by the pinned base digest + pinned apt versions + SBOM, NOT by
#   this scanner.
#
#   No digest-based base exclusion: the two docker-save layouts store layer
#   blobs under DIFFERENT digest kinds (the OCI/containerd layout keys blobs
#   by the manifest's COMPRESSED layer digest while the config's
#   rootfs.diff_ids hold the UNCOMPRESSED digests), and matching across
#   kinds misclassified layers on real runners (Docker 2x containerd store:
#   `docker save` emits an OCI layout whose blob file names are the
#   manifest's layer digests). The scanner therefore consumes only ordered
#   local blob paths — never a digest.
#
# Home-path rule: a bare "/home/" substring is NOT sufficient (the support
# bundle's redactor legitimately carries the literal "/home/" inside its own
# regex source, and so would any shipped scanner pattern). The rule is
# "/home/" + a plausible username + a path separator, i.e. an absolute home
# path of a real user leaked into an artifact.
#
# GitHub Actions annotations: when GITHUB_ACTIONS=true, every finding is
# ALSO printed as a workflow command
#   ::error file=<path>,title=artifact-scan::<rule> <artifact> <path> [masked]::
# (the matched value is NEVER printed — same convention as
# scripts/gates/annotate-go.sh, whose escaping is mirrored), and the run ends
# with a ::notice:: summary. Job logs need GitHub auth; the check-run
# annotations are the public surface (this is why the publish run's findings
# must be readable without it).
#
# Modes: run plain (default) or with --self-test, which builds fixtures in a
# temp dir and asserts each outcome:
#   1. a planted /home/<user> file (binary-aware) and a PEM private key
#      (forbidden-name) must FAIL the scan;
#   2. the support-bundle redactor's own "/home/" regex source and a public
#      certificate PEM (*.pem without a private-key marker) must PASS
#      (no false positive on a bare "/home/" or public certs);
#   3. a CLEAN image — base-like and apt-like layers carrying Debian
#      /home/<user>/ strings at non-COPY paths + a clean app binary at the
#      Dockerfile's COPY destination — must PASS in BOTH docker-save layout
#      kinds (real OCI layout: index.json -> manifest -> blobs/sha256/<digest>,
#      with compressed layer digests and uncompressed diff_ids, as
#      containerd-style `docker save` and `crane pull --format=oci` produce
#      them; and classic: manifest.json + <id>/layer.tar). Extraction
#      (gzip + zstd + plain tar blobs), whiteouts and the rule split are all
#      exercised; the layout shape is byte-verified against a real
#      crane-built layout of the pinned base (2026-10-18 reproduction);
#   4. the same image with a planted /home/<user>/ path inside the app
#      binary at the COPY destination must FAIL (content-home-path), a
#      leaky intermediate layer that a later layer whiteouts must NOT fail
#      (whiteout semantics), and an image whose binary is whiteouted away
#      must FAIL with image-no-owned-files.
#   GN_SCAN_SELFTEST_REAL=OCI_DIR[:CLASSIC_TAR] additionally runs the
#   self-test over externally built crane layouts (dev-only; the documented
#   crane commands are in the self-test header) — never required, never
#   network-dependent.
# Image scan: when docker is not usable, image layers are skipped with an
# explicit "NOT RUN: image scan (docker unavailable)" — never a failure. An
# image ref that does not exist locally is likewise NOT RUN (the build stage
# owns building; a missing image says nothing about artifact contents). The
# NOT RUN decision is printed BEFORE any extraction, so a NOT RUN run leaves
# no findings behind.
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
#   GN_SCAN_DOCKERFILE  Dockerfile used to derive the image's owned paths
#                       (default: <repo>/packaging/Dockerfile)
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

# ---------------------------------------------------------------------------
# Finding bookkeeping + GitHub Actions annotations. Every finding goes
# through report_finding: it records rule/artifact/path, prints the human
# line, and — under GITHUB_ACTIONS — a workflow command. The matched value
# is NEVER included ([masked]); only the rule, artifact and path (non-secret
# by construction) are printed. Escaping mirrors scripts/gates/annotate-go.sh:
# properties escape % : , ; data additionally escapes CR/LF.
# ---------------------------------------------------------------------------
IN_GHA=${GITHUB_ACTIONS:-false}
FINDINGS=0
FINDING_COUNTS=""   # "<rule>\t<count>" lines
declare -a SEEN=()  # dedupe keys: rule|label|path

# esc_data S: % -> %25, CR -> %0D, LF -> %0A (message body escaping)
esc_data() { printf '%s' "$1" | sed -e 's/%/%25/g' -e 's/$//'; }
# esc_prop S: data escaping + ':' -> %3A, ',' -> %2C (file=/title= values)
esc_prop() {
  printf '%s' "$1" | sed -e 's/%/%25/g' -e 's/:/%3A/g' -e 's/,/%2C/g'
}

gha_notice() { # $1=message
  case "$IN_GHA" in
    true|True|TRUE) printf '::notice::%s\n' "$(esc_data "$1")" ;;
  esac
}
gha_error() { # $1=file $2=title $3=message
  case "$IN_GHA" in
    true|True|TRUE)
      printf '::error file=%s,title=%s::%s\n' \
        "$(esc_prop "$1")" "$(esc_prop "$2")" "$(esc_data "$3")"
      ;;
  esac
}

note()  { printf 'scan: %s\n' "$*"; }
failures=0
report_finding() {
  # $1=rule $2=artifact label $3=path-in-artifact $4=detail (no values!)
  local rule="$1" label="$2" path="$3" detail="${4:-}"
  local key="$rule|$label|$path" k
  for k in ${SEEN[@]+"${SEEN[@]}"}; do
    [ "$k" = "$key" ] && return 0
  done
  SEEN+=("$key")
  failures=1
  FINDINGS=$((FINDINGS + 1))
  if [ -z "$FINDING_COUNTS" ]; then
    FINDING_COUNTS="$rule"$'\t'"1"
  else
    FINDING_COUNTS=$(printf '%s\n' "$FINDING_COUNTS" | awk -v r="$rule" '
      BEGIN { FS = OFS = "\t" }
      $1 == r { $2 += 1; found = 1 }
      END { if (!found) print r, 1 }
      { print }')
  fi
  if [ -n "$detail" ]; then
    printf 'SCAN FAIL: %s: %s in %s — %s\n' "$label" "$rule" "$path" "$detail"
  else
    printf 'SCAN FAIL: %s: %s in %s\n' "$label" "$rule" "$path"
  fi
  # GitHub Actions annotation: file= the in-artifact path; the title carries
  # rule + artifact + path + the [masked] marker (the matched value is NEVER
  # printed); the message repeats the rule detail (also never a value).
  gha_error "$path" "artifact-scan::$rule $label:$path [masked]" "${detail:-see scan output}"
}

# ---------------------------------------------------------------------------
# 1. Forbidden file-name patterns (basename match, per docs/spec/15: games,
#    savestates, voice samples, private data, credentials).
# ---------------------------------------------------------------------------
FILENAME_GLOBS=(
  '*.iso' '*.gcm' '*.ciso' '*.rvz' '*.bmg' '*.arc' '*.szs'
  '*.dtm' '*.sav' '*.wav' '*.sqlite' '*.sqlite-wal' '*.sqlite-shm'
  '.env*' '*.pem' 'id_*'
)

# pem_key_marker FILE: success when FILE contains a PEM private-key header.
# Every PEM private key ("BEGIN PRIVATE KEY" PKCS#8, "BEGIN RSA PRIVATE
# KEY", EC, OPENSSH, ENCRYPTED, PGP "... PRIVATE KEY BLOCK") carries one;
# public certificates ("BEGIN CERTIFICATE") never do. This lets the *.pem
# name rule distinguish credentials from the public CA bundle the
# ca-certificates package installs at build time: its postinst runs
# update-ca-certificates, which creates ~140 <serial>.N.pem files under
# /etc/ssl/certs/ in the final image (the .deb itself ships only .crt —
# verified against ca-certificates 20250419, 2026-10-18). Without this
# refinement the whole-image name rule would fail every release image on
# distribution content.
pem_key_marker() {
  grep -aqE -- 'BEGIN [A-Z0-9 ]*PRIVATE KEY' "$1" 2>/dev/null
}

# forbidden_name_hit FILE: the forbidden-name rule for one file — basename
# match, with the *.pem content refinement above (a .pem without a
# private-key marker is public-cert material, not a credential).
forbidden_name_hit() {
  local f="$1" base g
  base="${f##*/}"
  for g in "${FILENAME_GLOBS[@]}"; do
    # shellcheck disable=SC2254
    case "$base" in
      $g)
        [ "$g" = '*.pem' ] && ! pem_key_marker "$f" && continue
        return 0
        ;;
    esac
  done
  return 1
}

# Game-image extensions (docs/spec/15; mirrors scripts/gates/secret-scan.sh
# GAME_EXT_RE) and magic signatures for game dumps hidden under innocent
# names: CISO (Compressed ISO), RARC (GameCube), Yaz0 (Wii), ISO 9660
# ("CD001" volume descriptor at offset 32769).
GAME_EXT_RE='\.(iso|gcm|rvz|ciso|wbfs|7z|nkit\.iso|dtm|wia|sav|szs|gcz|drc)$'
MAGIC_CISO='4349534f'    # "CISO"
MAGIC_RARC='52415243'    # "RARC"
MAGIC_YAZ0='59617a30'    # "Yaz0"
MAGIC_CDO01='4344303031' # "CD001" at offset 32769 (ISO 9660)

# Big-binary threshold for image layers: 256 MiB (derivation in the header).
BIG_BINARY_LIMIT_BYTES=$((256 * 1024 * 1024))

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

check_content_file() {
  # $1 = file, $2 = artifact label, $3 = display path (default: $1).
  # At most one finding per rule per file.
  local f="$1" label="$2" display="${3:-$1}" pat
  for pat in ${CONTENT_STRINGS[@]+"${CONTENT_STRINGS[@]}"}; do
    if grep -aqF -- "$pat" "$f"; then
      report_finding "content-gateway" "$label" "$display" "gateway pattern match"
    fi
  done
  for pat in "${CONTENT_REGEXES[@]}"; do
    if grep -aqE -- "$pat" "$f"; then
      report_finding "content-home-path" "$label" "$display" "home-path pattern match"
    fi
  done
  # GAME_NARRATOR_GATEWAY_TOKEN with a non-placeholder value
  if grep -aqF -- "GAME_NARRATOR_GATEWAY_TOKEN=" "$f" \
    && grep -aF -- "GAME_NARRATOR_GATEWAY_TOKEN=" "$f" \
      | grep -av "replace-with-real-secret-outside-git" \
      | grep -av "GAME_NARRATOR_GATEWAY_TOKEN=$" \
      | grep -q .; then
    report_finding "content-gateway-token" "$label" "$display" "real gateway token value"
  fi
}

check_tree() {
  # $1 = extracted artifact root; $2 = artifact label (for reporting).
  # Findings carry the in-artifact path (relative to the root), so the
  # annotation is stable and independent of the local extraction dir.
  local root="$1" label="$2" f rel
  while IFS= read -r -d '' f; do
    if forbidden_name_hit "$f"; then
      rel="${f#"$root"/}"
      report_finding "forbidden-name" "$label" "$rel" "forbidden file name"
    fi
  done < <(find "$root" -type f -print0)
}

check_content() {
  # $1 = extracted artifact root; $2 = artifact label
  local root="$1" label="$2" f
  while IFS= read -r -d '' f; do
    check_content_file "$f" "$label" "${f#"$root"/}"
  done < <(find "$root" -type f -print0)
}

# ---------------------------------------------------------------------------
# Archive extraction (unchanged rule scope: every file is product-owned).
# ---------------------------------------------------------------------------
extract_targz() {
  local dest="$2"
  mkdir -p "$dest"
  tar -xzf "$1" -C "$dest"
}

extract_deb() {
  # data + control tarballs extracted separately
  local dest="$2"
  mkdir -p "$dest/data" "$dest/control"
  dpkg-deb -x "$1" "$dest/data"
  dpkg-deb --ctrl-tarfile "$1" 2>/dev/null | tar -x -C "$dest/control" || true
}

extract_pkg() {
  # A .pkg.tar.zst is a single zstd-compressed tar holding the payload files
  # plus the package metadata entries (.PKGINFO, .MTREE, .INSTALL) — the
  # whole tar is scanned, hooks and depend list included.
  local dest="$2"
  mkdir -p "$dest"
  tar -xf "$1" -C "$dest"
}

# parse_copy_dests DOCKERFILE: print one normalized absolute COPY/ADD
# destination per line (WORKDIR-resolved, trailing slashes stripped).
# Variable-bearing destinations cannot be resolved statically and are
# skipped with a note (the committed Dockerfile COPYs fixed paths).
parse_copy_dests() {
  local df="$1" workdir="/" line rest args dest
  [ -f "$df" ] || { note "parse_copy_dests: no Dockerfile at $df"; return 0; }
  while IFS= read -r line || [ -n "$line" ]; do
    line="${line#"${line%%[![:space:]]*}"}"
    case "$line" in
      \#*) continue ;;
      WORKDIR*|workdir*)
        rest="${line#*[Ww][Oo][Rr][Kk][Dd][Ii][Rr]}"
        rest="${rest#"${rest%%[![:space:]]*}"}"
        workdir="${rest%%[[:space:]]*}"
        [ -n "$workdir" ] || continue
        case "$workdir" in /*) : ;; *) workdir="/$workdir" ;; esac
        ;;
      COPY*|ADD*|copy*|add*)
        rest="${line#*[Cc][Oo][Pp][Yy]}"; case "$rest" in "$line"*) rest="${line#*[Aa][Dd][Dd]}";; esac
        args=""
        # shellcheck disable=SC2206
        for arg in $rest; do
          case "$arg" in --*) continue ;; esac
          args="${args}${args:+ }$arg"
        done
        [ -n "$args" ] || continue
        dest="${args##* }"
        case "$dest" in
          *'$'*) note "parse_copy_dests: skipping variable-bearing COPY destination '$dest'"
                continue ;;
        esac
        case "$dest" in
          .)   dest="$workdir" ;;
          ./*) dest="${workdir%/}/${dest#./}" ;;
          /*)  : ;;
          *)   dest="${workdir%/}/$dest" ;;
        esac
        while [ "${dest%/}" != "$dest" ] && [ "$dest" != "/" ]; do dest="${dest%/}"; done
        printf '%s\n' "$dest"
        ;;
    esac
  done < "$df"
}

# ---------------------------------------------------------------------------
# Image scan (docs/release.md §2.5, ADR 0033 amendment).
#
# extract_image REF DIR: `docker save REF` + untar into DIR, then
# _save_layout_parse writes DIR/layers.txt (one line per layer,
# bottom-to-top manifest order: <index>\t<absolute layer-blob path>) and
# DIR/config.json (the image config blob). Both docker-save layouts are
# handled, and NO digest is ever consumed:
#   OCI (Docker 2x containerd image store; same shape as
#   `crane pull --format=oci`): index.json -> image manifest (under
#   manifests/ or blobs/) -> layers[]. Each layer blob sits at
#   blobs/<sha256-hex> — the digest's "sha256:" prefix becomes the
#   "sha256/" directory. The manifest's layer digests are the *compressed*
#   blob digests while the config's rootfs.diff_ids are *uncompressed*
#   digests: a different digest kind, so matching across them misclassified
#   layers on real runners (the old digest-based base exclusion). Only
#   local file paths matter here.
#   classic (legacy image store): manifest.json -> Layers[] (per-layer
#   paths relative to the save dir) + Config (the config blob path).
#
# merge_image_layers DIR LABEL: extract EVERY layer blob (gzip, zstd, or
# plain tar, detected by magic bytes) IN MANIFEST ORDER into DIR/root,
# applying whiteout entries before merging each layer: .wh.<name> removes
# that file/directory from the merged tree; .wh..wh..opq makes the
# containing directory opaque (delete the directory, then this layer's own
# copy wins). Whiteout markers are consumed, never merged. Later layers
# overwrite earlier ones; the result is the image's final filesystem.
#
# scan_image_dir DIR LABEL: the rule split (pure — no docker — the
# self-test drives it on crane-built layouts, see the --self-test notes):
#   * WHOLE-IMAGE rules (forbidden-name, game-data, big-binary) run over
#     every file of the merged filesystem;
#   * CONTENT rules run ONLY over the files at (or under) the Dockerfile's
#     COPY destination paths in the merged filesystem, plus the image
#     config blob. The base distribution never writes those paths, so a
#     base or apt file can never be content-scanned — no layer
#     classification, no digest matching, no Debian path allowlist.
#   * image-no-owned-files: a file must exist at a COPY destination — the
#     app binary is required, not optional (a broken extraction or a
#     missing binary must fail, not pass).
# ---------------------------------------------------------------------------

# _extract_layer_blob BLOB DEST: extract one stored layer blob (gzip, zstd,
# or plain tar) into DEST. ANY failure (unknown magic, corrupt archive,
# missing zstd tool) returns non-zero so the caller reports it loudly — a
# silently unextracted layer is a broken scan, never a pass.
_extract_layer_blob() {
  local blob="$1" dest="$2" magic
  magic=$(head -c 4 -- "$blob" | od -An -tx1 | tr -d ' \n')
  case "$magic" in
    1f8b*)
      gzip -dc -- "$blob" | tar -x -C "$dest"
      ;;
    28b52ffd*)
      if ! command -v zstd >/dev/null 2>&1; then
        note "WARN: zstd-compressed layer $blob cannot be extracted (zstd tool unavailable)"
        return 1
      fi
      zstd -dc -- "$blob" | tar -x -C "$dest"
      ;;
    *)
      tar -xf "$blob" -C "$dest"
      ;;
  esac
}

# _save_layout_parse DIR: write DIR/layers.txt (ordered absolute layer-blob
# paths) and DIR/config.json for the docker-save layout stored in DIR.
# Non-zero when the layout is unrecognized or the image manifest is absent.
_save_layout_parse() {
  local dir="$1"
  python3 - "$dir" <<'PYEOF'
import json, os, shutil, sys
d = sys.argv[1]

# No digest semantics: resolve each digest to a LOCAL FILE PATH (the
# "sha256:" prefix becomes the "sha256/" directory; docker save has been
# observed writing the manifest under manifests/ or under blobs/).
def digest_path(dg):
    if not dg:
        return ""
    for sub in ("manifests", "blobs"):
        p = os.path.join(d, sub, dg.replace("sha256:", "sha256/"))
        if os.path.isfile(p):
            return p
    return ""

if os.path.isfile(os.path.join(d, "index.json")):
    idx = json.load(open(os.path.join(d, "index.json")))
    man = None
    for e in idx.get("manifests", []):
        p = digest_path(e.get("digest", ""))
        if not p:
            continue
        try:
            m = json.load(open(p))
        except Exception:
            continue
        if "layers" in m:
            man = m
            break
    if man is None:
        sys.exit(3)
    cfg = digest_path(man.get("config", {}).get("digest", ""))
    if cfg:
        shutil.copyfile(cfg, os.path.join(d, "config.json"))
    paths = [digest_path(l.get("digest", "")) for l in man.get("layers", [])]
elif os.path.isfile(os.path.join(d, "manifest.json")):
    man = json.load(open(os.path.join(d, "manifest.json")))
    entry = man[0] if isinstance(man, list) else man
    cfg = os.path.join(d, entry.get("Config", "") or "")
    if os.path.isfile(cfg):
        shutil.copyfile(cfg, os.path.join(d, "config.json"))
    paths = [os.path.join(d, p) for p in entry.get("Layers", [])]
else:
    sys.exit(3)
with open(os.path.join(d, "layers.txt"), "w") as out:
    for i, p in enumerate(paths):
        out.write("%d\t%s\n" % (i, p))
PYEOF
}

extract_image() {
  # $1 = image ref, $2 = dest dir
  local ref="$1" dest="$2"
  mkdir -p "$dest"
  docker save "$ref" -o "$dest/image.tar"
  tar -xf "$dest/image.tar" -C "$dest"
  rm -f "$dest/image.tar"
  if ! command -v python3 >/dev/null 2>&1; then
    # The layout parser needs python3 (present on GH runners and the dev
    # host). Failing loudly is deliberate: an unscanned image must never
    # pass the gate.
    note "image $ref: python3 unavailable — cannot parse the docker-save layout"
    return 1
  fi
  if ! _save_layout_parse "$dest"; then
    note "image $ref: layout not recognized in $dest (neither index.json nor manifest.json)"
    return 1
  fi
}

# merge_image_layers DIR LABEL: build DIR/root, the image's merged
# filesystem (see the section header). Returns 1 when a layer blob is
# missing or cannot be extracted (the finding is already reported).
merge_image_layers() {
  local dir="$1" label="$2"
  local root="$dir/root"
  mkdir -p "$root"
  local idx blob tmp wh rel name parent target
  while IFS=$'\t' read -r idx blob; do
    [ -n "$idx" ] || continue
    if [ -z "$blob" ] || [ ! -f "$blob" ]; then
      report_finding "image-extract" "$label" "(layer $idx)" \
        "layer blob not found in the saved image (${blob:-no path resolved})"
      return 1
    fi
    tmp="$dir/layer-tmp-$idx"
    mkdir -p "$tmp"
    if ! _extract_layer_blob "$blob" "$tmp"; then
      report_finding "image-extract" "$label" "(layer $idx)" \
        "cannot extract layer blob (gzip/zstd/tar): $blob"
      rm -rf "$tmp"
      return 1
    fi
    # Whiteouts apply to the tree BELOW this layer: delete the targets in
    # the merged root before merging this layer's own content.
    while IFS= read -r -d '' wh; do
      rel="${wh#"$tmp"/}"
      case "$rel" in
        */*) parent="${rel%/*}" ;;
        *)   parent="" ;;
      esac
      name="${rel##*/}"
      if [ "$name" = ".wh..wh..opq" ]; then
        # opaque dir: delete the containing directory entirely
        if [ -n "$parent" ]; then
          rm -rf -- "${root}/${parent:?}"
        else
          rm -rf -- "$root"
          mkdir -p "$root"
        fi
      else
        target="${name#.wh.}"
        # defensive: a bare ".wh." marker has no target
        [ -n "$target" ] || continue
        if [ -n "$parent" ]; then
          rm -rf -- "${root}/${parent:?}/${target:?}"
        else
          rm -rf -- "${root}/${target:?}"
        fi
      fi
    done < <(find "$tmp" -name '.wh.*' -print0)
    # Markers themselves are never part of the final filesystem.
    find "$tmp" -name '.wh.*' -delete 2>/dev/null
    cp -a "$tmp"/. "$root"/
    rm -rf "$tmp"
  done < "$dir/layers.txt"
  return 0
}

# scan_image_dir DIR LABEL: the rule split over a parsed image layout
# (DIR/layers.txt + DIR/config.json). Pure — no docker — so the self-test
# drives it on crane-built layouts.
scan_image_dir() {
  local dir="$1" label="$2"
  note "scanning image $label (merged layers: $dir/root)"
  local dockerfile
  dockerfile="${GN_SCAN_DOCKERFILE:-}"
  if [ -z "$dockerfile" ]; then
    dockerfile="$(cd "$(dirname "${SELF}")/../.." && pwd)/packaging/Dockerfile"
  fi
  local copydests
  copydests="$(parse_copy_dests "$dockerfile")"
  if ! merge_image_layers "$dir" "$label"; then
    return 0
  fi
  local root="$dir/root" found_copydest=0 f base rel size head mid

  # --- whole-image rules: every file of the merged filesystem ---
  while IFS= read -r -d '' f; do
    base="$(basename "$f")"
    rel="${f#"$root"/}"
    if forbidden_name_hit "$f"; then
      report_finding "forbidden-name" "$label" "$rel" "forbidden file name"
    fi
    if [[ ${base,,} =~ $GAME_EXT_RE ]]; then
      report_finding "game-data" "$label" "$rel" "game-image extension"
    fi
    size=$(wc -c < "$f" 2>/dev/null) || size=0
    if [ "$size" -ge 5 ]; then
      head=$(od -An -tx1 -N 4 -- "$f" 2>/dev/null | tr -d ' \n')
      case "$head" in
        "$MAGIC_CISO") report_finding "game-data" "$label" "$rel" "magic CISO at offset 0"; continue ;;
        "$MAGIC_RARC") report_finding "game-data" "$label" "$rel" "magic RARC at offset 0"; continue ;;
        "$MAGIC_YAZ0") report_finding "game-data" "$label" "$rel" "magic Yaz0 at offset 0"; continue ;;
      esac
      if [ "$size" -ge 32774 ]; then
        mid=$(od -An -tx1 -j 32768 -N 6 -- "$f" 2>/dev/null | tr -d ' \n')
        if [ "${mid:0:4}" = "$MAGIC_CDO01" ] || [ "${mid:1:4}" = "$MAGIC_CDO01" ]; then
          report_finding "game-data" "$label" "$rel" "magic CD001 (ISO 9660) at offset 32769"
        fi
      fi
    fi
    if [ "$size" -ge "$BIG_BINARY_LIMIT_BYTES" ]; then
      if ! grep -qI . -- "$f" 2>/dev/null; then
        report_finding "big-binary" "$label" "$rel" "$((size / 1024 / 1024)) MiB binary"
      fi
    fi
  done < <(find "$root" -type f -print0)

  # --- content rules: the files our Dockerfile places in the image ---
  # The base distribution never writes the COPY destination paths, so the
  # merged-fs membership alone scopes the content scan (no layer
  # classification needed).
  local cd owned
  while IFS= read -r -d '' f; do
    rel="${f#"$root"/}"
    owned=0
    while IFS= read -r cd; do
      [ -n "$cd" ] || continue
      if [ "$rel" = "${cd#/}" ]; then owned=1; break; fi
      case "$rel" in
        "${cd#/}"/*) owned=1; break ;;
      esac
    done <<< "$copydests"
    if [ "$owned" -eq 1 ]; then
      found_copydest=1
      check_content_file "$f" "$label" "$rel"
    fi
  done < <(find "$root" -type f -print0)

  # The image config blob (labels/env/entrypoint) is app-owned surface; it
  # is content-scanned unconditionally (it does NOT satisfy the owned-files
  # guard below: its presence says nothing about the binary).
  if [ -f "$dir/config.json" ]; then
    check_content_file "$dir/config.json" "$label" "(image config: labels/env)"
  fi

  if [ "$found_copydest" -eq 0 ]; then
    report_finding "image-no-owned-files" "$label" "(image)" \
      "no file found at the Dockerfile COPY destinations in the merged image (layer extraction broken, or the image does not contain the app binary)"
  fi
}

# scan_image REF: orchestrate one image ref.
scan_image() {
  local ref="$1" label dest dockerfile
  label="$ref"
  docker image inspect "$ref" >/dev/null 2>&1 || {
    note "NOT RUN: image scan $ref (docker image inspect failed)"
    return 0
  }
  dest="$WORK/img-$(echo "$ref" | tr '/:@' '___')"
  if ! extract_image "$ref" "$dest"; then
    report_finding "image-extract" "$label" "(image)" "cannot save/parse image"
    return 0
  fi
  dockerfile="${GN_SCAN_DOCKERFILE:-}"
  if [ -z "$dockerfile" ]; then
    dockerfile="$(cd "$(dirname "${SELF}")/../.." && pwd)/packaging/Dockerfile"
  fi
  scan_image_dir "$dest" "$label"
}
# ---------------------------------------------------------------------------
# Docker availability. Plain `docker info` first; if the session lacks the
# docker group while the user is a member, re-exec the whole scanner under
# the group when an image scan was requested (sg, or newgrp where sg is not
# installed). After the re-exec every docker call below is plain `docker`.
# ---------------------------------------------------------------------------
have_docker() { docker info >/dev/null 2>&1; }

# ---------------------------------------------------------------------------
# --self-test: fixtures in a temp dir; asserts pass/fail of each.
#   1. bad:  a tarball with a planted /home/someuser/x (binary-aware
#            content) -> the scan MUST fail (the rule still catches real
#            leaks).
#   2. good: a tarball containing the support-bundle redactor's home-path
#            regex source, which contains a bare "/home/" -> the scan MUST
#            pass (no false positive on the redactor's own pattern).
#   3. img:  a clean image — base-like and apt-like layers with Debian
#            /home/<user>/ strings at non-COPY paths + a clean binary at
#            the COPY destination — built as REAL docker-save layouts in
#            BOTH kinds and MUST pass in both. The OCI fixture is a true
#            containerd-style layout (index.json -> manifest ->
#            blobs/sha256/<digest>; layer digests are the COMPRESSED blob
#            digests, the config's rootfs.diff_ids are the UNCOMPRESSED
#            ones — the exact digest-kind split that broke digest-based
#            base exclusion on the CI runner). Its shape is verified
#            byte-for-byte against a real crane-built layout of the pinned
#            base:
#              go run github.com/google/go-containerregistry/cmd/crane@v0.20.2 \
#                  pull --format=oci \
#                  debian@sha256:7c7b2c96… /tmp/gn-oci-base
#              # append an app layer with the Dockerfile's COPY paths via
#              # go-containerregistry (crane append is registry-bound):
#              #   mutate.Append(base, mutate.Addendum{Layer:
#              #   tarball.LayerFromFile("app-layer.tar.gz")}) +
#              #   layout.Write(outDir, idx)   (2026-10-18 reproduction)
#            The classic fixture is manifest.json + <id>/layer.tar.
#   4. fail: the same clean image with a planted /home/<user>/ path inside
#            the binary at the COPY destination MUST fail with
#            content-home-path (and only there); a leaky intermediate layer
#            that a later layer whiteouts MUST NOT fail (whiteout
#            semantics); an image whose binary is whiteouted away MUST fail
#            with image-no-owned-files.
#   GN_SCAN_SELFTEST_REAL=OCI_DIR[:CLASSIC_TAR]: additionally run the
#   extraction + rule split over externally built crane layouts (the
#   commands above, or `docker save` of a locally built image). Dev-only:
#   the default self-test needs neither network nor crane.
# ---------------------------------------------------------------------------

# mk_layer_tar OUT TREE: layer tar with root-relative entries (real image
# layers carry etc/…, not ./etc/…).
mk_layer_tar() {
  local out="$1" tree="$2" tops
  tops=$(cd "$tree" && ls -A)
  # shellcheck disable=SC2086  # deliberate word-splitting: one tar arg per top dir
  tar -C "$tree" -cf "$out" $tops
}

# build_image_layouts BASE NAME LAYER_BLOB…: write BASE/NAME/oci (a real
# OCI layout: index.json + blobs/sha256/<digest>) and BASE/NAME/classic
# (a real classic docker-save layout: manifest.json + <layerid>/layer.tar
# + config). Layer blobs are stored AS GIVEN (gzip/zstd/plain), exactly
# like docker save stores them; the manifest digests are the stored-blob
# digests (compressed kind) while the config's diff_ids are the
# uncompressed digests — the digest-kind split of real containerd-style
# layouts.
build_image_layouts() {
  local base="$1" name="$2"; shift 2
  python3 - "$base" "$name" "$@" <<'PYEOF'
import gzip, hashlib, json, os, subprocess, sys
base, name = sys.argv[1], sys.argv[2]
layers = sys.argv[3:]
d = os.path.join(base, name)
ocid = os.path.join(d, "oci")
clid = os.path.join(d, "classic")
os.makedirs(os.path.join(ocid, "blobs", "sha256"), exist_ok=True)
os.makedirs(clid, exist_ok=True)

def h(b):
    return hashlib.sha256(b).hexdigest()

stored = []
diff_ids = []
for lb in layers:
    b = open(lb, "rb").read()
    stored.append((lb, b))
    if lb.endswith(".tar.gz"):
        diff_ids.append(hashlib.sha256(gzip.decompress(b)).hexdigest())
    elif lb.endswith(".tar.zst") or lb.endswith(".zstd"):
        out = subprocess.run(["zstd", "-dc", lb], capture_output=True,
                             check=True).stdout
        diff_ids.append(hashlib.sha256(out).hexdigest())
    else:
        diff_ids.append(hashlib.sha256(b).hexdigest())

cfg = {
    "architecture": "amd64",
    "os": "linux",
    "config": {
        "Env": ["GAME_NARRATOR_DATA_DIR=/var/lib/game-narrator"],
        "Entrypoint": ["game-narrator", "serve"],
    },
    "rootfs": {"type": "layers",
               "diff_ids": ["sha256:" + x for x in diff_ids]},
}
cfg_b = (json.dumps(cfg) + "\n").encode()

manifest_layers = []
for (lb, b) in stored:
    med = ("application/vnd.oci.image.layer.v1.tar+gzip"
           if lb.endswith(".tar.gz")
           else "application/zstd" if lb.endswith((".tar.zst", ".zstd"))
           else "application/vnd.oci.image.layer.v1.tar")
    manifest_layers.append({"mediaType": med, "digest": "sha256:" + h(b),
                            "size": len(b)})
man = {
    "schemaVersion": 2,
    "mediaType": "application/vnd.oci.image.manifest.v1+json",
    "config": {"mediaType": "application/vnd.oci.image.config.v1+json",
               "digest": "sha256:" + h(cfg_b), "size": len(cfg_b)},
    "layers": manifest_layers,
}
man_b = (json.dumps(man) + "\n").encode()

def put(bdir, digest, data):
    p = os.path.join(bdir, digest.replace("sha256:", "sha256/"))
    os.makedirs(os.path.dirname(p), exist_ok=True)
    open(p, "wb").write(data)

put(ocid + "/blobs", "sha256:" + h(man_b), man_b)
put(ocid + "/blobs", "sha256:" + h(cfg_b), cfg_b)
for (lb, b) in stored:
    put(ocid + "/blobs", "sha256:" + h(b), b)

idx = {"schemaVersion": 2,
       "manifests": [{"mediaType":
                      "application/vnd.oci.image.manifest.v1+json",
                      "digest": "sha256:" + h(man_b), "size": len(man_b)}]}
json.dump(idx, open(os.path.join(ocid, "index.json"), "w"), indent=1)
open(os.path.join(ocid, "oci-layout"), "w").write(
    '{"imageLayoutVersion": "1.0.0"}\n')

# classic docker-save layout
cfgid = h(cfg_b)
open(os.path.join(clid, cfgid + ".json"), "wb").write(cfg_b)
class_layers = []
for i, (lb, b) in enumerate(stored):
    lid = "layer%02d" % i
    os.makedirs(os.path.join(clid, lid), exist_ok=True)
    open(os.path.join(clid, lid, "layer.tar"), "wb").write(b)
    class_layers.append("%s/layer.tar" % lid)
open(os.path.join(clid, "manifest.json"), "w").write(json.dumps(
    [{"Config": cfgid + ".json", "RepoTags": ["selftest:fixture"],
      "Layers": class_layers}]))
open(os.path.join(clid, "repositories"), "w").write(
    '{"selftest": {"fixture": "%s"}}\n' % cfgid)
PYEOF
}

# run_image_case DIR LABEL: reset finding state, parse the docker-save
# layout (the step extract_image performs after `docker save`), run the
# pure image scan (scan_image_dir), and capture the output for the
# assertions. The minimal Dockerfile (same as packaging/Dockerfile: one
# COPY of the app binary) is passed via GN_SCAN_DOCKERFILE.
run_image_case() {
  local dir="$1" label="$2"
  failures=0; FINDINGS=0; FINDING_COUNTS=""; SEEN=()
  if ! _save_layout_parse "$dir" >"$WORK/caseout" 2>&1; then
    echo "SCAN FAIL: $label: image-extract in (image) — layout not recognized (self-test fixture broken?)" >&2
    echo "SELF-TEST-INTERNAL: layout parse failed for $dir" >&2
    OUT_IMG="$(cat "$WORK/caseout" 2>/dev/null)"
    return 1
  fi
  GN_SCAN_DOCKERFILE="$IMG_SELFTEST_DF" \
    scan_image_dir "$dir" "$label" >>"$WORK/caseout" 2>&1
  OUT_IMG="$(cat "$WORK/caseout" 2>/dev/null)"
}

run_self_test() {
  local st=0
  # WORK is deliberately global: the EXIT trap must still see it after the
  # function returns (locals are gone by then).
  WORK="$(mktemp -d "${TMPDIR:-/tmp}/gn-scan-selftest.XXXXXX")"
  trap 'rm -rf "${WORK:-}"' EXIT

  note 'self-test: building fixtures in a temp dir'
  # Home-path fixture strings are assembled at runtime from a bare "/home/"
  # prefix: a literal /home/<user>/ in this source would itself be a
  # secret-scan finding (this script is tracked), while the runtime values
  # are real home paths that the rules must catch.
  local HP1='/home/'
  # PEM markers assembled at runtime: a literal private-key header in this
  # source would trip scripts/gates/secret-scan.sh's own pem rule on this
  # tracked file.
  local PRVK='PRIVATE KEY'
  mkdir -p "$WORK/bad" "$WORK/good"
  # Planted real home path, embedded in a binary file (NUL bytes) to prove
  # the check is binary-aware (grep -a).
  { printf 'lead\0\0'; printf '/home/someuser/x'; printf '\0trail\n'; } > "$WORK/bad/planted"
  # A PEM private key must be caught by the forbidden-name rule.
  printf -- "-----BEGIN RSA %s-----\nMIIKEYDATA\n-----END RSA %s-----\n" "$PRVK" "$PRVK" > "$WORK/bad/key.pem"
  # The exact regex source the support-bundle redactor compiles in
  # (internal/support/redact.go: homePathPattern). It contains "/home/" but
  # not a real home path: after "/home/" comes "(", not a username.
  printf '%s\n' '(?i)/home/([a-z0-9][a-z0-9._-]*)((?:/[^ ]*)?)' > "$WORK/good/redactor-pattern"
  # A public certificate PEM (the shape of the /etc/ssl/certs/*.pem the
  # ca-certificates package installs) must NOT be flagged: no private-key
  # marker -> public-cert material, not a credential.
  printf -- '-----BEGIN CERTIFICATE-----\nMIICERTDATA\n-----END CERTIFICATE-----\n' > "$WORK/good/ca-bundle.pem"
  mkdir -p "$WORK/dist-bad" "$WORK/dist-good"
  tar -C "$WORK" -czf "$WORK/dist-bad/fixture.tar.gz" bad
  tar -C "$WORK" -czf "$WORK/dist-good/fixture.tar.gz" good

  note 'self-test 1/5: planted /home/someuser/x and a PEM private key must FAIL the scan'
  local badout
  badout=$(GN_SCAN_DIST="$WORK/dist-bad" GN_SCAN_IMAGE="" "$SELF" 2>&1) || true
  case "$badout" in
    *"forbidden-name"*) : ;;
    *) echo 'SELF-TEST FAIL: PEM private key not flagged (forbidden-name expected):'; printf '%s\n' "$badout" >&2; st=1 ;;
  esac
  case "$badout" in
    *"SCAN FAIL"*) : ;;
    *) echo 'SELF-TEST FAIL: scan passed on the planted /home/someuser/x fixture (must fail)'; printf '%s\n' "$badout" >&2; st=1 ;;
  esac
  case "$badout" in
    *"content-home-path"*) : ;;
    *) echo 'SELF-TEST FAIL: planted home path not caught:'; printf '%s\n' "$badout" >&2; st=1 ;;
  esac
  [ "$st" -eq 0 ] && note 'self-test 1/5: OK — planted home path and private key caught'

  note 'self-test 2/5: redactor /home/ regex source and a public cert PEM must PASS the scan'
  local goodout
  goodout=$(GN_SCAN_DIST="$WORK/dist-good" GN_SCAN_IMAGE="" "$SELF" 2>&1) || true
  case "$goodout" in
    *"SCAN FAIL"*|*"forbidden-name"*)
      echo 'SELF-TEST FAIL: scan failed on the redactor/public-cert fixtures (false positive):' >&2
      printf '%s\n' "$goodout" >&2
      st=1 ;;
    *) note 'self-test 2/5: OK — redactor pattern and public cert not flagged' ;;
  esac

  # --- image fixtures: real docker-save layouts (both kinds) ---
  # minimal Dockerfile mirroring packaging/Dockerfile's COPY
  mkdir -p "$WORK/imgdf"
  printf '%s\n' 'FROM debian:bookworm' \
    'COPY linux/amd64/game-narrator /usr/local/bin/game-narrator' \
    > "$WORK/imgdf/Dockerfile"
  IMG_SELFTEST_DF="$WORK/imgdf/Dockerfile"
  local out
  # base-like layer: a Debian file with a /home/<user>/ string (base never
  # writes the COPY destination -> never content-scanned)
  mkdir -p "$WORK/lbase/etc/openal"
  printf '%s\n' '# OpenAL sample config' "sample-dir = ${HP1}albert/samples" \
    > "$WORK/lbase/etc/openal/alsoft.conf"
  # apt-like layer: more Debian /home/<user>/ strings at non-COPY paths
  mkdir -p "$WORK/lapt/usr/share/doc/libasound2/examples" \
           "$WORK/lapt/usr/share/X11/xkb/symbols"
  printf '%s\n' '# ALSA example rc' 'see %sjane/doc/asoundrc for details' "$HP1" \
    > "$WORK/lapt/usr/share/doc/libasound2/examples/asoundrc.txt"
  printf '%s\n' '// xkb data-dir %smats/data' "$HP1" \
    > "$WORK/lapt/usr/share/X11/xkb/symbols/se"
  # a bare "/home/" mention that must NOT trip the home-path rule
  printf '%s\n' '#!/bin/sh' "grep -r '/home/' /etc/" > "$WORK/lapt/update-passwd"
  # clean app binary at the COPY destination
  mkdir -p "$WORK/lapp/usr/local/bin"
  { printf '\x7fELF\001\001\001\000'; printf 'clean-binary\0tail'; } \
    > "$WORK/lapp/usr/local/bin/game-narrator"
  # leaky app binary (planted home path at the COPY destination)
  mkdir -p "$WORK/lappbad/usr/local/bin"
  { printf '\x7fELF\001\001\001\000'; printf 'leaky\0'; \
    printf '%ssecretuser/leak' "$HP1"; printf '\0tail'; } \
    > "$WORK/lappbad/usr/local/bin/game-narrator"
  # whiteout layer: removes a file from the merged tree
  mkdir -p "$WORK/lwh/usr/local/bin"
  : > "$WORK/lwh/usr/local/bin/.wh.game-narrator"
  # opaque-dir layer: .wh..wh..opq + the clean binary replacing everything
  # in usr/local/bin
  mkdir -p "$WORK/lop/usr/local/bin"
  : > "$WORK/lop/usr/local/bin/.wh..wh..opq"
  cp -a "$WORK/lapp/usr/local/bin/game-narrator" "$WORK/lop/usr/local/bin/"
  # layer tars (root-relative), stored in the three real compression kinds
  mk_layer_tar "$WORK/lb-base.tar" "$WORK/lbase"
  mk_layer_tar "$WORK/lb-apt.tar" "$WORK/lapt"
  mk_layer_tar "$WORK/lb-app.tar" "$WORK/lapp"
  mk_layer_tar "$WORK/lb-appbad.tar" "$WORK/lappbad"
  mk_layer_tar "$WORK/lb-wh.tar" "$WORK/lwh"
  mk_layer_tar "$WORK/lb-op.tar" "$WORK/lop"
  gzip -c "$WORK/lb-base.tar" > "$WORK/lb-base.tar.gz"
  gzip -c "$WORK/lb-apt.tar" > "$WORK/lb-apt.tar.gz"
  gzip -c "$WORK/lb-app.tar" > "$WORK/lb-app.tar.gz"
  gzip -c "$WORK/lb-appbad.tar" > "$WORK/lb-appbad.tar.gz"
  gzip -c "$WORK/lb-wh.tar" > "$WORK/lb-wh.tar.gz"
  gzip -c "$WORK/lb-op.tar" > "$WORK/lb-op.tar.gz"
  zstd -q -c "$WORK/lb-app.tar" > "$WORK/lb-app.zst"
  zstd -q -c "$WORK/lb-apt.tar" > "$WORK/lb-apt.zst"

  # 3/5: clean image in BOTH layout kinds must PASS.
  build_image_layouts "$WORK/imgs" clean "$WORK/lb-base.tar.gz" "$WORK/lb-apt.tar.gz" "$WORK/lb-app.tar.gz"
  build_image_layouts "$WORK/imgs" clean-zstd "$WORK/lb-base.tar.gz" "$WORK/lb-apt.zst" "$WORK/lb-app.zst"
  build_image_layouts "$WORK/imgs" clean-plain "$WORK/lb-base.tar.gz" "$WORK/lb-apt.tar.gz" "$WORK/lb-app.tar"

  note 'self-test 3/5: clean image must PASS in OCI and classic layouts (gzip/zstd/plain blobs)'
  local name layoutdir
  for c in clean:oci clean-zstd:oci clean-plain:oci clean:classic clean-zstd:classic clean-plain:classic; do
    name="${c%%:*}"; layoutdir="$WORK/imgs/$name/${c##*:}"
    run_image_case "$layoutdir" "selftest-$name"
    out="$OUT_IMG"
    if [ "$failures" -ne 0 ]; then
      echo "SELF-TEST FAIL: clean image ($c) reported findings (false positive):" >&2
      printf '%s\n' "$out" >&2
      st=1
      continue
    fi
    case "$out" in
      *"image-no-owned-files"*)
        echo "SELF-TEST FAIL: clean image ($c) — app binary not found at the COPY destination (extraction broken):" >&2
        printf '%s\n' "$out" >&2
        st=1 ;;
    esac
    note "self-test 3/5: OK — $c clean"
  done

  # 4/5: planted secret at the COPY destination must FAIL (OCI + classic);
  # whiteout + opaque semantics; whiteouted-away binary -> no-owned-files.
  build_image_layouts "$WORK/imgs" bad "$WORK/lb-base.tar.gz" "$WORK/lb-apt.tar.gz" "$WORK/lb-appbad.tar.gz"
  build_image_layouts "$WORK/imgs" whiteout "$WORK/lb-base.tar.gz" "$WORK/lb-appbad.tar.gz" "$WORK/lb-wh.tar.gz" "$WORK/lb-app.tar.gz"
  build_image_layouts "$WORK/imgs" opaque "$WORK/lb-base.tar.gz" "$WORK/lb-appbad.tar.gz" "$WORK/lb-op.tar.gz"
  build_image_layouts "$WORK/imgs" whiteout-only "$WORK/lb-appbad.tar.gz" "$WORK/lb-wh.tar.gz"

  note 'self-test 4/5: planted app secret must FAIL (OCI and classic); whiteouts must be honoured'
  for c in bad:oci bad:classic; do
    name="${c%%:*}"; layoutdir="$WORK/imgs/$name/${c##*:}"
    run_image_case "$layoutdir" "selftest-$name"
    out="$OUT_IMG"
    case "$out" in
      *"SCAN FAIL"*"content-home-path"*"usr/local/bin/game-narrator"*) : ;;
      *)
        echo "SELF-TEST FAIL: planted app secret ($c) not caught at the COPY destination:" >&2
        printf '%s\n' "$out" >&2
        st=1
        continue ;;
    esac
    case "$out" in
      *"alsoft.conf"*|*"asoundrc.txt"*|*"symbols/se"*)
        echo "SELF-TEST FAIL: ($c) flagged a base/apt file (false positive):" >&2
        printf '%s\n' "$out" >&2
        st=1 ;;
    esac
    if [ "$FINDINGS" -ne 1 ]; then
      echo "SELF-TEST FAIL: ($c) expected exactly 1 finding, got $FINDINGS:" >&2
      printf '%s\n' "$out" >&2
      st=1
    fi
    note "self-test 4/5: OK — $c planted secret caught (1 finding)"
  done

  # whiteout: the leaky binary is removed by .wh.game-narrator, then a clean
  # one is added -> PASS with zero findings (a broken whiteout handling would
  # leave the leaky binary in place -> content finding).
  run_image_case "$WORK/imgs/whiteout/oci" "selftest-whiteout"
  out="$OUT_IMG"
  if [ "$failures" -ne 0 ]; then
    echo 'SELF-TEST FAIL: whiteout image reported findings (whiteout not applied):' >&2
    printf '%s\n' "$out" >&2
    st=1
  else
    note 'self-test 4/5: OK — whiteout replaced the leaky binary (no findings)'
  fi

  # opaque dir: .wh..wh..opq wipes usr/local/bin; the layer's own clean
  # binary wins -> PASS with zero findings.
  run_image_case "$WORK/imgs/opaque/oci" "selftest-opaque"
  out="$OUT_IMG"
  if [ "$failures" -ne 0 ]; then
    echo 'SELF-TEST FAIL: opaque-dir image reported findings (opaque whiteout not applied):' >&2
    printf '%s\n' "$out" >&2
    st=1
  else
    note 'self-test 4/5: OK — opaque dir whiteout applied (no findings)'
  fi

  # whiteout-only: the only app layer is whiteouted away -> the merged image
  # has no file at the COPY destination -> image-no-owned-files.
  run_image_case "$WORK/imgs/whiteout-only/oci" "selftest-whiteout-only"
  out="$OUT_IMG"
  case "$out" in
    *"image-no-owned-files"*) : ;;
    *)
      echo 'SELF-TEST FAIL: whiteouted-away binary not reported as image-no-owned-files:' >&2
      printf '%s\n' "$out" >&2
      st=1 ;;
  esac
  case "$out" in
    *"content-home-path"*)
      echo 'SELF-TEST FAIL: whiteout-only image still content-scanned the removed binary:' >&2
      printf '%s\n' "$out" >&2
      st=1 ;;
  esac
  note 'self-test 4/5: OK — whiteouted-away binary fails with image-no-owned-files'

  # 5/5 (dev-only, opt-in): externally built crane layouts. The documented
  # build commands (self-test header) produce an OCI dir and/or a classic
  # tar; both are real containerd/legacy shapes from the pinned base.
  if [ -n "${GN_SCAN_SELFTEST_REAL:-}" ]; then
    note "self-test 5/5: real crane layouts: $GN_SCAN_SELFTEST_REAL"
    local real="${GN_SCAN_SELFTEST_REAL%%:*}" classic="${GN_SCAN_SELFTEST_REAL#*:}"
    [ "$classic" = "$GN_SCAN_SELFTEST_REAL" ] && classic=""
    if [ -n "$real" ] && [ -d "$real" ]; then
      cp -a "$real" "$WORK/imgs/real-oci"
      run_image_case "$WORK/imgs/real-oci" "selftest-real-oci"
      out="$OUT_IMG"
      if [ "$failures" -ne 0 ]; then
        echo 'SELF-TEST FAIL: real crane OCI layout reported findings:' >&2
        printf '%s\n' "$out" >&2
        st=1
      else
        note 'self-test 5/5: OK — real crane OCI layout passes (app binary found, Debian /home/ strings not flagged)'
      fi
    else
      echo "SELF-TEST FAIL: GN_SCAN_SELFTEST_REAL OCI dir missing: $real" >&2
      st=1
    fi
    if [ -n "$classic" ] && [ -f "$classic" ]; then
      mkdir -p "$WORK/imgs/real-classic"
      tar -xf "$classic" -C "$WORK/imgs/real-classic"
      run_image_case "$WORK/imgs/real-classic" "selftest-real-classic"
      out="$OUT_IMG"
      if [ "$failures" -ne 0 ]; then
        echo 'SELF-TEST FAIL: real classic layout reported findings:' >&2
        printf '%s\n' "$out" >&2
        st=1
      else
        note 'self-test 5/5: OK — real classic layout passes'
      fi
    fi
  fi

  if [ "$st" -eq 0 ]; then
    note 'self-test PASS — planted home paths are caught; the redactor pattern, base/apt Debian files (both layouts, all blob kinds, whiteouts honoured) are not flagged'
    return 0
  fi
  note 'self-test FAIL — see above' >&2
  return 1
}
# ---------------------------------------------------------------------------
# Main scan.
# ---------------------------------------------------------------------------
main() {
  if [ "${1:-}" = "--self-test" ]; then
    run_self_test
    exit $?
  fi

  local DIST_DIR="${GN_SCAN_DIST:-dist}"
  # Optional positional override of the artifacts directory (e.g.
  # `scan-artifacts.sh /path/to/dist`); GN_SCAN_DIST still works for the
  # make pipeline.
  if [ "$#" -ge 1 ]; then
    DIST_DIR="$1"
  fi
  # WORK is deliberately global: the EXIT trap must still see it after
  # main() returns (locals are gone by then).
  WORK="$(mktemp -d "${TMPDIR:-/tmp}/gn-scan.XXXXXX")"
  trap 'rm -rf "${WORK:-}"' EXIT

  if [ ! -d "$DIST_DIR" ]; then
    echo "scan: no $DIST_DIR directory (GN_SCAN_DIST=$DIST_DIR)" >&2
    exit 2
  fi

  # -----------------------------------------------------------------------
  # Run the scan over every artifact.
  # -----------------------------------------------------------------------
  local archive label pkg dest ref
  shopt -s nullglob
  for archive in "$DIST_DIR"/*.tar.gz; do
    label="$(basename "$archive")"
    dest="$WORK/$(echo "$label" | tr '/' '_')"
    if ! extract_targz "$archive" "$dest"; then
      report_finding "extract" "$label" "(archive)" "cannot extract archive"
      continue
    fi
    check_tree "$dest" "$label"
    check_content "$dest" "$label"
  done

  for pkg in "$DIST_DIR"/*.deb; do
    label="$(basename "$pkg")"
    dest="$WORK/$(echo "$label" | tr '/' '_')"
    if ! extract_deb "$pkg" "$dest"; then
      report_finding "extract" "$label" "(package)" "cannot extract package"
      continue
    fi
    check_tree "$dest" "$label"
    check_content "$dest" "$label"
  done

  for pkg in "$DIST_DIR"/*.pkg.tar.zst; do
    label="$(basename "$pkg")"
    dest="$WORK/$(echo "$label" | tr '/' '_')"
    if ! extract_pkg "$pkg" "$dest"; then
      report_finding "extract" "$label" "(package)" "cannot extract package"
      continue
    fi
    check_tree "$dest" "$label"
    check_content "$dest" "$label"
  done

  if [ -n "${GN_SCAN_IMAGE:-}" ]; then
    if ! have_docker; then
      # Unusable docker is NOT a scan failure: the build stage decided not
      # to (or could not) build images, and artifact files are still scanned.
      note "NOT RUN: image scan (docker unavailable)"
    else
      for ref in $GN_SCAN_IMAGE; do
        if ! docker image inspect "$ref" >/dev/null 2>&1; then
          note "NOT RUN: image scan $ref (image not built locally)"
          continue
        fi
        scan_image "$ref"
      done
    fi
  else
    note "GN_SCAN_IMAGE not set: skipping docker image layer scan"
  fi

  # -----------------------------------------------------------------------
  # Summary (mirrors the annotate-go.sh ::notice convention).
  # -----------------------------------------------------------------------
  local summary
  summary=$(printf '%s\n' "$FINDING_COUNTS" | awk -F'\t' '{ printf "%s=%s ", $1, $2 }')
  if [ "$failures" -ne 0 ]; then
    echo "scan: FAIL — $FINDINGS finding(s): ${summary:-?}" >&2
    # per-finding ::error annotations were emitted above; the final summary
    # is a ::notice with the totals (annotate-go.sh convention)
    gha_notice "artifact-scan: FAIL — $FINDINGS finding(s): ${summary:-?} — see the ::error annotations above"
    exit 1
  fi
  note "PASS — no forbidden files or content patterns found"
  gha_notice "artifact-scan: PASS — no forbidden files or content patterns found"
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  # Re-exec under the docker group when needed (before main: the group may
  # only apply to newly spawned processes).
  if [ -n "${GN_SCAN_IMAGE:-}" ] \
     && [ -z "${GN_SCAN_IN_DOCKER_GROUP:-}" ] \
     && [ "${1:-}" != "--self-test" ] \
     && ! have_docker; then
    if command -v sg >/dev/null 2>&1 && sg docker -c 'docker info' >/dev/null 2>&1; then
      export GN_SCAN_IN_DOCKER_GROUP=1
      exec sg docker -c "exec $0 $*"
    elif command -v newgrp >/dev/null 2>&1 && newgrp docker -c 'docker info' >/dev/null 2>&1; then
      export GN_SCAN_IN_DOCKER_GROUP=1
      exec newgrp docker -c "exec $0 $*"
    fi
  fi
  main "$@"
fi
