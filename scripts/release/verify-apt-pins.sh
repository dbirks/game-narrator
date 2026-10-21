#!/usr/bin/env bash
# verify-apt-pins.sh — drift guard for the OCI image's apt version pins (ADR 0131).
#
# packaging/Dockerfile pins exact Debian package versions AND points apt at
# immutable snapshot.debian.org timestamps (the live Debian archive only
# keeps the newest version of each package, so an upstream update silently
# breaks exact pins — this is what broke the edge image build on 2026-10-21:
# ca-certificates moved 20250419~deb12u1 -> 20250419~deb12u2 upstream and the
# pinned version stopped resolving).
#
# This guard parses packaging/Dockerfile and verifies in two layers:
#
#  1. INDEX CHECK (always; network, no docker): for every pinned
#     `pkg=version`, the exact version is present in the frozen snapshot
#     index for BOTH linux/amd64 and linux/arm64, in the union of the
#     suites the Dockerfile enables (bookworm, bookworm-updates,
#     bookworm-security — component main). Fails with the version(s) that
#     ARE available and the fix recipe.
#
#  2. STAGE BUILD (when docker is usable): `docker buildx build
#     --target apt-pins` of the apt stage for linux/amd64 — the real
#     `apt-get update` + exact-version install, including GPG verification
#     of the frozen InRelease. The apt stage copies nothing from the build
#     context, so the build runs against an empty context dir (fast, and
#     immune to local dist//artifacts/ noise). With binfmt arm64 emulation
#     the same stage build runs for linux/arm64 too; without emulation that
#     arch is reported NOT RUN (never as pass). The stage build is the full
#     resolution check (all dependencies); the index check is the fast
#     detector for the direct pins.
#
# Wired into `make release-snapshot` and the publish workflow (publish.yml)
# so drift fails early, before any release work happens.
#
# Env: GN_DOCKERFILE (default packaging/Dockerfile).
# Exit: 0 all performed checks passed (no docker => index check only, with
#       NOT RUN reported); 1 a performed check failed; 2 usage/parse error.
set -euo pipefail
cd "$(dirname "$0")/../.."

# Docker group handling: group membership is per-login session. If this
# session lacks the docker group but the user is a member, re-exec the whole
# script under it so every docker call below is a plain `docker`. (Same
# pattern as scripts/release/smoke.sh.)
if [ -z "${GN_APT_PINS_IN_DOCKER_GROUP:-}" ] && ! docker info >/dev/null 2>&1; then
  if command -v sg >/dev/null 2>&1 && sg docker -c 'docker info' >/dev/null 2>&1; then
    export GN_APT_PINS_IN_DOCKER_GROUP=1
    exec sg docker -c "exec $0"
  elif command -v newgrp >/dev/null 2>&1 && newgrp docker -c 'docker info' >/dev/null 2>&1; then
    export GN_APT_PINS_IN_DOCKER_GROUP=1
    exec newgrp docker -c "exec $0"
  fi
fi

DOCKERFILE="${GN_DOCKERFILE:-packaging/Dockerfile}"
[ -f "$DOCKERFILE" ] || { echo "verify-apt-pins: $DOCKERFILE not found" >&2; exit 2; }

# --- parse the Dockerfile (single source of truth) ---------------------------
arg_default() { # arg_default NAME -> default value of the first matching ARG
  sed -n -E "s/^ARG[[:space:]]+$1=([^[:space:]]+).*/\1/p" "$DOCKERFILE" | head -n1
}
BASE_IMAGE="$(arg_default BASE_IMAGE)"
DEBIAN_TS="$(arg_default DEBIAN_SNAPSHOT_TS)"
DEBIAN_SEC_TS="$(arg_default DEBIAN_SECURITY_SNAPSHOT_TS)"
if [ -z "$BASE_IMAGE" ] || [ -z "$DEBIAN_TS" ] || [ -z "$DEBIAN_SEC_TS" ]; then
  echo "verify-apt-pins: could not parse BASE_IMAGE / DEBIAN_SNAPSHOT_TS /" >&2
  echo "DEBIAN_SECURITY_SNAPSHOT_TS ARGs from $DOCKERFILE (ADR 0131 layout expected)" >&2
  exit 2
fi

# The pinned install block: from 'apt-get install' to 'apt-get clean'.
pins="$(
  awk '/apt-get install/ {f=1; next} /apt-get clean/ {f=0} f' "$DOCKERFILE" \
    | grep -oE '\b[a-z][a-z0-9+.-]*=[0-9][A-Za-z0-9.:+~-]*' || true
)"
[ -n "$pins" ] || { echo "verify-apt-pins: no pkg=version pins in the apt-get install block of $DOCKERFILE" >&2; exit 2; }

echo "verify-apt-pins: base image ${BASE_IMAGE}"
echo "verify-apt-pins: frozen sources debian@${DEBIAN_TS}, debian-security@${DEBIAN_SEC_TS}"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# --- 1. index check -----------------------------------------------------------
# Fetch a snapshot Packages index (gz or xz, whichever exists) and store it
# unpacked at $TMP/<suite>_<arch>.idx.
fetch_index() { # fetch_index <archive> <ts> <suite> <arch>
  local base="http://snapshot.debian.org/archive/$1/$2/dists/$3/main/binary-$4/Packages" ext
  for ext in gz xz; do
    # The first extension 404s on suites that only serve the other one;
    # expected, not an error.
    if curl -fsSL --max-time 300 "${base}.${ext}" -o "$TMP/$3_$4.$ext" 2>/dev/null; then
      if [ "$ext" = gz ]; then gzip -dc "$TMP/$3_$4.$ext" > "$TMP/$3_$4.idx"
      else xz -dc "$TMP/$3_$4.$ext" > "$TMP/$3_$4.idx"; fi
      return 0
    fi
  done
  return 1
}

# idx_version <suite> <arch> <package> -> versions listed (one per line)
idx_version() { # idx_version <suite> <arch> <pkg>
  awk -v p="$3" '
    /^Package: / { cur = $2 }
    /^Version: / { if (cur == p) print $2 }
  ' "$TMP/$1_$2.idx"
}

# Fetch the union of enabled suites for both arches (6 indices).
declare -A ARCHIVE_OF=( [bookworm]="debian" [bookworm-updates]="debian" [bookworm-security]="debian-security" )
declare -A TS_OF=( [bookworm]="$DEBIAN_TS" [bookworm-updates]="$DEBIAN_TS" [bookworm-security]="$DEBIAN_SEC_TS" )
for suite in bookworm bookworm-updates bookworm-security; do
  for arch in amd64 arm64; do
    if ! fetch_index "${ARCHIVE_OF[$suite]}" "${TS_OF[$suite]}" "$suite" "$arch"; then
      echo "verify-apt-pins: FAIL: cannot fetch snapshot index for suite '$suite' arch '$arch'" >&2
      echo "  (http://snapshot.debian.org/archive/${ARCHIVE_OF[$suite]}/${TS_OF[$suite]}/dists/$suite/main/binary-$arch/Packages.{gz,xz})" >&2
      echo "  Fix: the pinned snapshot timestamp ${TS_OF[$suite]} no longer serves that suite/arch —" >&2
      echo "  pick a real snapshot from https://snapshot.debian.org/ that matches the base image" >&2
      echo "  build date and update the ARG in $DOCKERFILE, then re-run this guard." >&2
      exit 1
    fi
  done
done

echo "verify-apt-pins: snapshot indices fetched (bookworm/bookworm-updates@${DEBIAN_TS}, bookworm-security@${DEBIAN_SEC_TS}; amd64 + arm64)"

fail=0
while IFS= read -r pin; do
  pkg="${pin%%=*}"; want="${pin#*=}"
  for arch in amd64 arm64; do
    where=""
    for suite in bookworm bookworm-updates bookworm-security; do
      if idx_version "$suite" "$arch" "$pkg" | grep -qx "$want"; then
        where="$suite"
        break
      fi
    done
    if [ -z "$where" ]; then
      avail="$(for suite in bookworm bookworm-updates bookworm-security; do
                 idx_version "$suite" "$arch" "$pkg"; done | sort -uV | tr '\n' ' ')"
      echo "verify-apt-pins: FAIL: pin '${pin}' does not resolve in the frozen snapshot" >&2
      echo "  arch: linux/${arch}   expected: ${want}" >&2
      echo "  available: ${avail:-<none>}" >&2
      echo "  Fix: update the pin in $DOCKERFILE to an available version (verify it for BOTH" >&2
      echo "  linux/amd64 and linux/arm64 before editing), or re-pin the snapshot timestamps" >&2
      echo "  if the base image also changed. Then re-run: scripts/release/verify-apt-pins.sh" >&2
      fail=1
    else
      echo "verify-apt-pins: OK: ${pin} (linux/${arch}, suite ${where})"
    fi
  done
done <<< "$pins"
[ "$fail" -eq 0 ] || exit 1

# --- 2. stage build (real apt resolution, when docker is usable) --------------
if ! docker info >/dev/null 2>&1; then
  echo "verify-apt-pins: NOT RUN: apt-pins stage build (docker daemon not usable in this session);"
  echo "  the index check above verified the pins. On CI (publish.yml) the stage build runs."
  exit 0
fi

EMPTY_CTX="$(mktemp -d)"
trap 'rm -rf "$TMP" "$EMPTY_CTX"' EXIT
STAGE_LOG="$TMP/stage-build.log"

stage_build() { # stage_build <platform>
  local platform="$1"
  if ! docker buildx build --platform "$platform" --target apt-pins \
        -t gn-apt-pins-check:"${platform#linux/}" \
        -f "$DOCKERFILE" --progress=plain \
        "$EMPTY_CTX" > "$STAGE_LOG" 2>&1; then
    if grep -qE 'exec format error|unknown unix binary format|binfmt' "$STAGE_LOG"; then
      echo "verify-apt-pins: NOT RUN: apt-pins stage build for ${platform} (no binfmt emulation; install with e.g. 'docker run --rm --privileged tonistiigi/binfmt --install arm64')"
      return 0
    fi
    echo "verify-apt-pins: FAIL: apt-pins stage build for ${platform} (last 40 lines of the build log):" >&2
    tail -n 40 "$STAGE_LOG" >&2
    echo "verify-apt-pins: full log: $STAGE_LOG" >&2
    return 1
  fi
  # Catch GPG/index warnings BuildKit would otherwise swallow as success.
  # Plain-progress lines are prefixed "#<step> <t>s " so match a warning
  # after that prefix (or at line start for unprefixed docker output).
  if grep -E '(^|[0-9]) (W|E|Err): ' "$STAGE_LOG"; then
    echo "verify-apt-pins: FAIL: apt-pins stage build for ${platform} produced apt warnings/errors:" >&2
    grep -E '(^|[0-9]) (W|E|Err): ' "$STAGE_LOG" >&2
    return 1
  fi
  echo "verify-apt-pins: OK: apt-pins stage build for ${platform} (apt-get update + exact-version install, GPG-verified)"
}

stage_build linux/amd64 || exit 1
stage_build linux/arm64 || exit 1

echo "verify-apt-pins: all checks passed"
