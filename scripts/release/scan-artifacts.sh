#!/usr/bin/env bash
# scan-artifacts.sh — release artifact scanner (AGENTS.md rule 5, docs/spec/15
# "Publishing restrictions", ADR 0033, ADR 0053).
#
# Fails the release when any release artifact in $DIST_DIR (tar.gz archives,
# .deb packages, .pkg.tar.zst Arch packages) or a docker image contains:
#   1. files matching game/voice/private name patterns, or
#   2. content matching secret/topology patterns.
#
# Rule split for docker images (ADR 0033 amendment, docs/release.md §2.1):
#   The image mixes the product's own files with the distribution's packages
#   (the pinned debian base + the apt layer installed by packaging/Dockerfile).
#   The rule classes get different scope:
#
#   WHOLE-IMAGE rules — run over every file in every layer:
#     * forbidden-name: game/voice/private data file-name globs,
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
#   token) — run ONLY over the files our Dockerfile adds: the app binary at
#   the COPY destination(s), and the image config blob (labels/env/
#   entrypoint).
#   Why the restriction is essential: the pinned debian base is 100% clean
#   (crane export, 3265 files, zero home-path hits), but the APT layer the
#   Dockerfile installs is NOT — Debian ships real "/home/<name>/" strings
#   (e.g. /etc/openal/alsoft.conf, /usr/share/doc/libasound2/examples/
#   asoundrc.txt, /usr/share/X11/xkb/symbols/se; 4 files in the 6-package
#   pinned closure, verified via crane + dpkg-deb, 2026-10). Those apt files
#   are in "ours" layers (the base-digest exclusion does NOT remove them),
#   so the COPY-destination restriction is what keeps them out of the content
#   scan: an apt file is never at a path the product COPYs. Distribution
#   content is the distribution's supply chain, covered by the pinned base
#   digest + pinned apt versions + SBOM, NOT by this scanner.
#   The owned-file set is derived from the Dockerfile, never from a
#   hand-written allowlist of Debian paths:
#     a) base layers are identified by digest: the pinned base ref is parsed
#        from packaging/Dockerfile and its layer digests fetched via
#        `docker manifest inspect` (registry, no local pull); fallback: the
#        local base image's layer count as a layer-index prefix; fallback:
#        unknown. A layer whose blob digest matches a base layer digest is
#        excluded from content scanning.
#     b) content scanning is further restricted to the paths the Dockerfile
#        COPY/ADDs (parsed from packaging/Dockerfile, WORKDIR-aware) — the
#        exact set of paths the product places in the image.
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
#   1. a planted /home/<user> file (binary-aware) must FAIL the scan;
#   2. the support-bundle redactor's own "/home/" regex source must PASS
#      (no false positive on a bare "/home/");
#   3. a synthetic image tree — a base-classified layer carrying a
#      Debian-style /home/<user>/ string + an app layer with a planted
#      /home/<secretuser>/ path at the Dockerfile's COPY destination — the
#      planted app secret must FAIL, the base-like string must NOT;
#   4. the real CI bug — an "ours" (non-base) APT layer with a Debian
#      /home/<user>/ string at a non-COPY path + a clean app binary at the
#      COPY destination must PASS (the COPY-destination restriction, not
#      just base-digest exclusion, stops the apt false-positives).
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

match_forbidden_name() {
  local base="$1" g
  for g in "${FILENAME_GLOBS[@]}"; do
    # shellcheck disable=SC2254
    case "$base" in $g) return 0 ;; esac
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
  local root="$1" label="$2" f base rel
  while IFS= read -r -d '' f; do
    base="$(basename "$f")"
    if match_forbidden_name "$base"; then
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

# ---------------------------------------------------------------------------
# Image scan (docs/release.md §2.1, ADR 0033 amendment).
#
# extract_image REF DIR: `docker save` + per-layer extraction into
# DIR/layers/<NNNN>/ (bottom-to-top), the image config blob to
# DIR/config.json, and the per-layer blob digests (bottom-to-top) to
# DIR/layer-digests.txt as <index>\t<digest-or-> lines. Both `docker save`
# layouts are handled:
#   legacy (classic image store): manifest.json array; per-layer
#     <layerid>/layer.tar (the stored blob; its sha256 is the registry
#     manifest's layer digest).
#   OCI (containerd image store): index.json + blobs/sha256/* (blob file
#     name is the digest itself).
# ---------------------------------------------------------------------------

# _extract_layer_blob BLOB DESTDIR: extract one stored layer blob (gzip,
# zstd, or plain tar) into DESTDIR. Returns 1 when the blob format is not
# supported (the layer is then left unscanned — reported, never silent).
_extract_layer_blob() {
  local blob="$1" dest="$2" magic
  magic=$(head -c 4 -- "$blob" | od -An -tx1 | tr -d ' \n')
  case "$magic" in
    1f8b*)
      gzip -dc -- "$blob" 2>/dev/null | tar -x -C "$dest" 2>/dev/null \
        || tar -xzf "$blob" -C "$dest" 2>/dev/null
      ;;
    28b52ffd*)
      if command -v zstd >/dev/null 2>&1; then
        zstd -dc -- "$blob" 2>/dev/null | tar -x -C "$dest" 2>/dev/null
      else
        note "WARN: zstd layer $blob not extracted (zstd tool unavailable)"
        return 1
      fi
      ;;
    *)
      tar -xf "$blob" -C "$dest" 2>/dev/null
      ;;
  esac
}

# _save_layout_parse DIR: write DIR/layer-digests.txt and DIR/config.json
# for whichever docker-save layout DIR holds. Returns 0 on success.
_save_layout_parse() {
  local dir="$1"
  if [ -f "$dir/index.json" ] && [ -d "$dir/blobs" ]; then
    _save_layout_parse_oci "$dir"
  elif [ -f "$dir/manifest.json" ]; then
    _save_layout_parse_legacy "$dir"
  else
    return 1
  fi
}

_save_layout_parse_oci() {
  local dir="$1"
  python3 - "$dir" <<'PYEOF'
import json, os, shutil, sys
d = sys.argv[1]
idx = json.load(open(os.path.join(d, "index.json")))
# Locate the image manifest among the index entries (docker save writes it
# under manifests/ or blobs/; pick the entry whose file exists and parses
# as an image manifest, i.e. carries "layers").
man = None
for e in idx.get("manifests", []):
    dg = e.get("digest", "")
    for cand in (os.path.join(d, "manifests", dg.replace("sha256:", "sha256/")),
                 os.path.join(d, "blobs", dg.replace("sha256:", "sha256/"))):
        if not os.path.isfile(cand):
            continue
        try:
            m = json.load(open(cand))
        except Exception:
            continue
        if "layers" in m:
            man = m
            break
    if man:
        break
if man is None:
    sys.exit(3)
cfg = man.get("config", {}).get("digest", "")
cfg_file = os.path.join(d, "blobs", cfg.replace("sha256:", "sha256/"))
if cfg and os.path.isfile(cfg_file):
    shutil.copy(cfg_file, os.path.join(d, "config.json"))
with open(os.path.join(d, "layer-digests.txt"), "w") as out:
    for i, l in enumerate(man.get("layers", [])):
        dg = l.get("digest", "")
        out.write(f"{i}\t{dg or '-'}\n")
PYEOF
}

_save_layout_parse_legacy() {
  local dir="$1"
  python3 - "$dir" <<'PYEOF'
import json, os, hashlib, sys, shutil
d = sys.argv[1]
man = json.load(open(os.path.join(d, "manifest.json")))
entry = man[0] if isinstance(man, list) else man
cfg = entry.get("Config", "")
if cfg:
    src = os.path.join(d, cfg)
    if os.path.isfile(src):
        shutil.copy(src, os.path.join(d, "config.json"))
with open(os.path.join(d, "layer-digests.txt"), "w") as out:
    for i, lpath in enumerate(entry.get("Layers", [])):
        blob = os.path.join(d, lpath)
        dg = "-"
        if os.path.isfile(blob):
            h = hashlib.sha256()
            with open(blob, "rb") as f:
                for chunk in iter(lambda: f.read(1 << 20), b""):
                    h.update(chunk)
            dg = "sha256:" + h.hexdigest()
        out.write(f"{i}\t{dg}\n")
PYEOF
}

extract_image() {
  # $1 = image ref, $2 = dest dir
  local ref="$1" dest="$2"
  mkdir -p "$dest/layers"
  docker save "$ref" -o "$dest/image.tar"
  tar -xf "$dest/image.tar" -C "$dest"
  rm -f "$dest/image.tar"
  if ! command -v python3 >/dev/null 2>&1; then
    # The layout parsers need python3 (present on GH runners and the dev
    # host). Failing loudly is deliberate: an unscanned image must never
    # pass the gate.
    note "image $ref: python3 unavailable — cannot parse the docker-save layout"
    return 1
  fi
  if ! _save_layout_parse "$dest"; then
    note "image $ref: layout not recognized in $dest (neither index.json+blobs nor manifest.json)"
    return 1
  fi
  # Decompress every layer blob into its layers/<NNNN>/ dir.
  local idx digest blob dir
  while IFS=$'\t' read -r idx digest; do
    [ -n "$idx" ] || continue
    local dir
    dir="$dest/layers/$(printf '%04d' "$idx")"
    mkdir -p "$dir"
    blob=""
    if [ -d "$dest/blobs" ] && [ "$digest" != "-" ]; then
      blob="$dest/blobs/${digest}"
    else
      # legacy: the blob path is in manifest.json; recover it via python
      blob=$(python3 - "$dest" "$idx" <<'PYEOF'
import json, os, sys
d, i = sys.argv[1], int(sys.argv[2])
man = json.load(open(os.path.join(d, "manifest.json")))
entry = man[0] if isinstance(man, list) else man
print(os.path.join(d, entry["Layers"][i]) if i < len(entry.get("Layers", [])) else "")
PYEOF
) || blob=""
    fi
    if [ -n "$blob" ] && [ -f "$blob" ]; then
      if ! _extract_layer_blob "$blob" "$dir"; then
        note "WARN: layer $idx of $ref not fully extracted (format unsupported); its files are NOT scanned"
      fi
    fi
  done < "$dest/layer-digests.txt"
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

# parse_base_ref DOCKERFILE: print the pinned base image ref of the final
# FROM (honouring `ARG BASE_IMAGE=...` + `FROM ${BASE_IMAGE}`), or nothing.
parse_base_ref() {
  local df="$1" argref="" fromline="" line ref
  [ -f "$df" ] || return 0
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in
      *ARG*)
        case "$line" in
          *ARG\ BASE_IMAGE=*)
            argref="${line#*ARG\ BASE_IMAGE=}" # strip inline comment, take first token
            argref="${argref%%#*}"
            argref="${argref%%[[:space:]]*}"
            ;;
        esac
        ;;
    esac
    case "$line" in
      *FROM*)
        # keep the last FROM (multi-stage: the final stage owns the image)
        case "$line" in
          FROM*|from*) fromline="$line" ;;
        esac
        ;;
    esac
  done < "$df"
  [ -n "$fromline" ] || return 0
  # strip everything up to and including FROM, first token only
  ref="${fromline#*FROM}"
  ref="${ref#"${ref%%[![:space:]]*}"}"
  ref="${ref%%[[:space:]]*}"
  # shellcheck disable=SC2016  # single quotes deliberate: literal $BASE_IMAGE
  if [ "$ref" = '${BASE_IMAGE}' ] || [ "$ref" = '$BASE_IMAGE' ]; then
    [ -n "$argref" ] && printf '%s\n' "$argref"
  elif [ "$ref" != "scratch" ]; then
    printf '%s\n' "$ref"
  fi
}

# scan_image_base_classify DIR REF ARCH: write DIR/meta.txt with one line
# per layer index (bottom-to-top): <classification>\t<digest-or->,
# classification in {base, ours, unknown}. Strategy (most robust first):
#   1. registry: `docker manifest inspect` of the base ref (following a
#      manifest list to the child for ARCH) -> base layer digests; a saved
#      layer whose blob digest matches is a base layer. (The FROM copies the
#      base layers verbatim, so the digests match exactly.)
#   2. local base image: `docker image inspect` RootFS layer count K; the
#      first K layers are base layers.
#   3. neither: every layer unknown (the COPY-destination restriction then
#      alone determines the content-scan set — still correct for this
#      Dockerfile, since the distribution owns no /usr/local path).
scan_image_base_classify() {
  local dir="$1" ref="$2" arch="$3"
  [ -n "$ref" ] || { _meta_all "$dir" unknown; return 0; }
  local ndigests
  # Step 1: registry manifest (index-aware).
  ndigests=$(docker manifest inspect "$ref" 2>/dev/null | python3 -c '
import json, sys
try:
    m = json.load(sys.stdin)
except Exception:
    sys.exit(3)
if "manifests" in m:
    arch = sys.argv[1]
    pick = None
    for e in m["manifests"]:
        p = e.get("platform", {})
        if p.get("os") == "linux" and p.get("architecture") == arch:
            pick = e["digest"]; break
    if pick is None:
        for e in m["manifests"]:
            if e.get("platform", {}).get("os") == "linux":
                pick = e["digest"]; break
    if pick:
        print(pick)
    sys.exit(0)
for l in m.get("layers", []):
    print(l["digest"])
sys.exit(0)
' "${arch:-amd64}" 2>/dev/null) || ndigests=""
  if [ -n "$ndigests" ]; then
    # If the ref is a manifest list, the line above is the child-manifest
    # digest: fetch that manifest and collect ITS layers.
    local lines
    lines=$(printf '%s\n' "$ndigests" | grep -c . || true)
    if [ "$lines" = "1" ]; then
      local repo="${ref%%@*}"; repo="${repo%%:*}"
      local childjson
      if childjson=$(docker manifest inspect "${repo}@${ndigests}" 2>/dev/null) \
         && printf '%s' "$childjson" | python3 -c '
import json, sys
try:
    m = json.load(sys.stdin)
except Exception:
    sys.exit(3)
sys.exit(0 if "layers" in m else 3)
' 2>/dev/null; then
        ndigests=$(printf '%s' "$childjson" | python3 -c '
import json, sys
m = json.load(sys.stdin)
for l in m.get("layers", []):
    print(l["digest"])') || ndigests=""
      fi
    fi
  fi
  if [ -n "$ndigests" ]; then
    local digest cls
    : > "$dir/meta.txt"
    while IFS=$'\t' read -r _idx digest; do
      [ -n "$_idx" ] || continue
      cls=ours
      if [ -n "$digest" ] && printf '%s\n' "$ndigests" | grep -qF -- "$digest"; then
        cls=base
      fi
      printf '%s\t%s\n' "$cls" "${digest:--}" >> "$dir/meta.txt"
    done < "$dir/layer-digests.txt"
    return 0
  fi
  # Step 2: local base image layer count.
  local k
  if k=$(docker image inspect "$ref" --format '{{len .RootFS.DiffIds}}' 2>/dev/null) \
     && [ -n "$k" ] && [ "$k" -ge 0 ] 2>/dev/null; then
    local i=0 digest
    : > "$dir/meta.txt"
    while IFS=$'\t' read -r _idx digest; do
      [ -n "$_idx" ] || continue
      if [ "$i" -lt "$k" ]; then
        printf 'base\t%s\n' "${digest:--}"
      else
        printf 'ours\t%s\n' "${digest:--}"
      fi
      i=$((i + 1))
    done < "$dir/layer-digests.txt"
    return 0
  fi
  # Step 3: unknown.
  _meta_all "$dir" unknown
}

_meta_all() { # DIR CLASS
  local dir="$1" cls="$2" digest
  : > "$dir/meta.txt"
  while IFS=$'\t' read -r _idx digest; do
    [ -n "$_idx" ] || continue
    printf '%s\t%s\n' "$cls" "${digest:--}" >> "$dir/meta.txt"
  done < "$dir/layer-digests.txt"
}

# scan_image_tree DIR LABEL: the rule split over an extracted image tree
# (DIR/layers/<NNNN>/ + DIR/meta.txt + DIR/config.json). Pure — no docker —
# so the self-test drives it on a synthetic tree.
scan_image_tree() {
  local dir="$1" label="$2"
  note "scanning image $label (layers: $dir/layers)"
  local dockerfile="${GN_SCAN_DOCKERFILE:-}"
  if [ -z "$dockerfile" ]; then
    dockerfile="$(cd "$(dirname "${SELF}")/../.." && pwd)/packaging/Dockerfile"
  fi
  local copydests
  copydests="$(parse_copy_dests "$dockerfile")"
  local found_copydest=0 f rel base size head mid layerdir cls cd owned

  # --- whole-image rules: every file in every layer ---
  local i=0
  for layerdir in "$dir"/layers/*/; do
    [ -d "${layerdir%/}" ] || continue
    while IFS= read -r -d '' f; do
      base="$(basename "$f")"
      rel="${f#"$layerdir"}"
      if match_forbidden_name "$base"; then
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
    done < <(find "$layerdir" -type f -print0)
    i=$((i + 1))
  done

  # --- content rules: app-owned files only ---
  # Owned = at a COPY destination (exact path, or under a COPY-destination
  # directory) in a layer that is NOT classified base.
  i=0
  for layerdir in "$dir"/layers/*/; do
    [ -d "${layerdir%/}" ] || continue
    cls=$(awk -v n="$i" 'NR == n + 1 { print $1; exit }' FS='\t' "$dir/meta.txt" 2>/dev/null)
    [ -n "$cls" ] || cls=unknown
    i=$((i + 1))
    if [ "$cls" = "base" ]; then
      continue  # distribution content: pinned base digest + SBOM own this
    fi
    while IFS= read -r -d '' f; do
      rel="${f#"$layerdir"}"
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
    done < <(find "$layerdir" -type f -print0)
  done
  # The image config blob (labels/env/entrypoint) is app-owned surface; it
  # is content-scanned unconditionally (it does NOT satisfy the owned-files
  # guard below: its presence says nothing about the binary layer).
  if [ -f "$dir/config.json" ]; then
    check_content_file "$dir/config.json" "$label" "(image config: labels/env)"
  fi

  if [ "$found_copydest" -eq 0 ]; then
    report_finding "image-no-owned-files" "$label" "(image)" \
      "no app-owned files found at the Dockerfile COPY destinations in non-base layers (layer extraction broken, or the image does not contain the app binary)"
  fi
}

# scan_image REF: orchestrate one image ref.
scan_image() {
  local ref="$1" label arch dest dockerfile baseref
  label="$ref"
  arch=$(docker image inspect "$ref" --format '{{.Architecture}}' 2>/dev/null) || {
    note "NOT RUN: image scan $ref (docker image inspect failed)"
    return 0
  }
  dest="$WORK/img-$(echo "$ref" | tr '/:@' '___')"
  if ! extract_image "$ref" "$dest"; then
    report_finding "image-extract" "$label" "(image)" "cannot save/extract image"
    return 0
  fi
  dockerfile="${GN_SCAN_DOCKERFILE:-}"
  if [ -z "$dockerfile" ]; then
    dockerfile="$(cd "$(dirname "${SELF}")/../.." && pwd)/packaging/Dockerfile"
  fi
  baseref="$(parse_base_ref "$dockerfile")"
  if [ -n "$baseref" ]; then
    scan_image_base_classify "$dest" "$baseref" "$arch"
    note "image $label: base layers classified (base ref $baseref, arch $arch)"
  else
    _meta_all "$dest" unknown
    note "image $label: no base ref parsed from $dockerfile; all layers unknown (content scan rests on the COPY-destination restriction)"
  fi
  scan_image_tree "$dest" "$label"
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
#   bad:  a tarball with a planted /home/someuser/x (binary-aware content)
#         -> the scan MUST fail (the rule still catches real leaks).
#   good: a tarball containing the support-bundle redactor's home-path regex
#         source, which contains a bare "/home/" -> the scan MUST pass
#         (no false positive on the redactor's own pattern).
#   img:  a synthetic image tree — a base-classified layer carrying a
#         Debian-style /home/<user>/ string (must NOT fail) and an app layer
#         with a planted /home/<secretuser>/ path at the Dockerfile's COPY
#         destination (MUST fail). Proves the image rule split without
#         docker: scan_image_tree is pure.
# ---------------------------------------------------------------------------
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

  note 'self-test 1/4: planted /home/someuser/x must FAIL the scan'
  if GN_SCAN_DIST="$WORK/dist-bad" GN_SCAN_IMAGE="" "$SELF"; then
    echo 'SELF-TEST FAIL: scan passed on the planted /home/someuser/x fixture (must fail)' >&2
    st=1
  else
    note 'self-test 1/4: OK — planted home path caught'
  fi

  note 'self-test 2/4: redactor /home/ regex source must PASS the scan'
  if GN_SCAN_DIST="$WORK/dist-good" GN_SCAN_IMAGE="" "$SELF"; then
    note 'self-test 2/4: OK — redactor pattern not flagged'
  else
    echo 'SELF-TEST FAIL: scan failed on the redactor-pattern fixture (false positive)' >&2
    st=1
  fi

  # 3/4: synthetic image tree exercising the image rule split, in-process.
  mkdir -p "$WORK/img/layers/0000/etc/openal" \
           "$WORK/img/layers/0001/usr/local/bin" \
           "$WORK/img/df"
  # base-classified layer: a Debian-style file with a /home/<user>/ string
  # (must NOT be content-scanned -> must NOT fail)
  printf '%s\n' '# OpenAL sample config' "sample-dir = ${HP1}user/samples" \
    > "$WORK/img/layers/0000/etc/openal/alsoft.conf"
  # app layer: the binary at the Dockerfile's COPY destination with a
  # planted home path (MUST fail)
  { printf 'lead\0'; printf '%ssecretuser/leak' "$HP1"; printf '\0tail'; } \
    > "$WORK/img/layers/0001/usr/local/bin/game-narrator"
  printf 'base\tsha256:aaaa\n' > "$WORK/img/meta.txt"
  printf 'ours\tsha256:bbbb\n' >> "$WORK/img/meta.txt"
  # image config blob (labels/env) — always content-scanned
  printf '{"config":{"Env":["GAME_NARRATOR_DATA_DIR=/var/lib/game-narrator"],"Labels":{"org.opencontainers.image.version":"selftest"}}}\n' \
    > "$WORK/img/config.json"
  # minimal Dockerfile: the COPY destination must match the app layer path
  printf '%s\n' 'FROM debian:bookworm' \
    'COPY linux/amd64/game-narrator /usr/local/bin/game-narrator' \
    > "$WORK/img/df/Dockerfile"

  note 'self-test 3/4: image split — planted app secret must FAIL; base-like /home/ must NOT'
  failures=0; FINDINGS=0; FINDING_COUNTS=""; SEEN=()
  local out rc=0
  # run in the current shell (failures/SEEN are shell state); capture the
  # output via a temp file instead of a subshell
  GN_SCAN_DOCKERFILE="$WORK/img/df/Dockerfile" \
    scan_image_tree "$WORK/img" "synthetic-image" >"$WORK/imgout" 2>&1 || rc=1
  out=$(cat "$WORK/imgout" 2>/dev/null)
  case "$out" in
    *"SCAN FAIL"*"content-home-path"*"usr/local/bin/game-narrator"*) : ;;
    *) echo 'SELF-TEST FAIL: image tree did not catch the planted app secret:'; echo "$out" >&2; st=1 ;;
  esac
  case "$out" in
    *alsoft.conf*)
      echo 'SELF-TEST FAIL: image tree flagged a base-classified Debian file (false positive):'
      echo "$out" >&2; st=1 ;;
  esac
  if [ "$rc" -eq 0 ] && [ "$failures" -eq 0 ]; then
    echo 'SELF-TEST FAIL: image tree scan reported no failure (planted secret missed)' >&2
    st=1
  fi

  # 4/4: the real CI bug — an "ours" (non-base) APT layer carrying a Debian
  # /home/<user>/ string that is NOT at the COPY destination must NOT be
  # content-flagged, while a clean app binary at the COPY dest must PASS.
  mkdir -p "$WORK/imgapt/layers/0000/etc/openal" \
           "$WORK/imgapt/layers/0001/usr/local/bin" \
           "$WORK/imgapt/df"
  # apt layer (ours, NOT digest-excluded): Debian file with a real home-path
  # string, at a path the product never COPYs -> must NOT be content-scanned
  printf '%s\n' '# OpenAL sample config' "sample-dir = ${HP1}user/samples" \
    > "$WORK/imgapt/layers/0000/etc/openal/alsoft.conf"
  # app layer: the binary at the COPY destination, clean (no home path)
  { printf 'ELFlead\0'; printf 'clean-binary\0tail'; } \
    > "$WORK/imgapt/layers/0001/usr/local/bin/game-narrator"
  printf 'ours\tsha256:cccc\n' > "$WORK/imgapt/meta.txt"
  printf 'ours\tsha256:dddd\n' >> "$WORK/imgapt/meta.txt"
  printf '{"config":{"Env":["GAME_NARRATOR_DATA_DIR=/var/lib/game-narrator"]}}\n' \
    > "$WORK/imgapt/config.json"
  printf '%s\n' 'FROM debian:bookworm' \
    'COPY linux/amd64/game-narrator /usr/local/bin/game-narrator' \
    > "$WORK/imgapt/df/Dockerfile"

  note 'self-test 4/4: apt-layer Debian home-path (non-COPY path) must NOT be flagged; clean app binary must PASS'
  failures=0; FINDINGS=0; FINDING_COUNTS=""; SEEN=(); rc=0
  GN_SCAN_DOCKERFILE="$WORK/imgapt/df/Dockerfile" \
    scan_image_tree "$WORK/imgapt" "synthetic-apt-image" >"$WORK/aptout" 2>&1 || rc=1
  out=$(cat "$WORK/aptout" 2>/dev/null)
  case "$out" in
    *"SCAN FAIL"*)
      echo 'SELF-TEST FAIL: image tree flagged a non-COPY Debian file or a clean binary (false positive):'
      echo "$out" >&2; st=1 ;;
  esac
  if [ "$failures" -ne 0 ]; then
    echo 'SELF-TEST FAIL: apt-layer image reported a finding (false positive):' >&2
    echo "$out" >&2; st=1
  fi

  if [ "$st" -eq 0 ]; then
    note 'self-test PASS — planted home paths are caught; the redactor pattern, base-classified files and non-COPY apt files are not flagged'
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
