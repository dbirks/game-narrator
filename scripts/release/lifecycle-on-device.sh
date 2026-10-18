#!/usr/bin/env bash
# lifecycle-on-device.sh SSH_TARGET PREV_DEB CUR_DEB
#
# Drives the docs/spec/16 M8 lifecycle on a UNO Q appliance: clean install
# of a "previous" package over whatever dirty build is installed, verified
# upgrade to "current", rollback to "previous", re-upgrade, and a
# purge + clean-install cycle with data preserved (docs/release.md
# sections 3-5). The packages must have been built from clean committed
# trees (docs/release.md section 1) and carry verified checksums.
#
# Every step prints PASS/FAIL/SKIP; the device is left running CUR_DEB.
# The script never prints /etc/game-narrator/env, its contents, or any
# credential: the env file is verified only by mode, owner and line count,
# and API tokens/passwords stay in shell variables.
#
# Device writes are confined to: /tmp/gn-lc (transport + this driver),
# /home/arduino/gn-data.bak-<ts> (pre-flight data backup), and the
# package-managed paths (dpkg, systemd units, /etc/game-narrator).
#
# Usage (from the repo root):
#   scripts/release/lifecycle-on-device.sh <user>@<uno-q-host> \
#     dist-prev/game-narrator_*_linux_arm64.deb \
#     dist-cur/game-narrator_*_linux_arm64.deb
#
# Exit: 0 every step PASS and the device runs CUR_DEB; 1 a step failed or
# the device does not end on CUR_DEB; 2 usage error.

set -euo pipefail

if [ $# -ne 3 ]; then
  echo "usage: $0 SSH_TARGET PREV_DEB CUR_DEB" >&2
  echo "  e.g. $0 <user>@<uno-q-host> prev.deb cur.deb" >&2
  exit 2
fi
target=$1
prev_deb=$2
cur_deb=$3

[ -f "$prev_deb" ] || { echo "lifecycle: prev package not found: $prev_deb" >&2; exit 2; }
[ -f "$cur_deb" ]  || { echo "lifecycle: cur package not found: $cur_deb"  >&2; exit 2; }
command -v ssh >/dev/null 2>&1  || { echo "lifecycle: ssh not on PATH" >&2; exit 2; }
command -v dpkg-deb >/dev/null 2>&1 || { echo "lifecycle: dpkg-deb not on PATH" >&2; exit 2; }
for d in "$prev_deb" "$cur_deb"; do
  dpkg-deb --info "$d" >/dev/null 2>&1 || { echo "lifecycle: not a .deb: $d" >&2; exit 2; }
  arch=$(dpkg-deb --field "$d" Architecture)
  [ "$arch" = "arm64" ] || { echo "lifecycle: package is $arch, the UNO Q needs arm64: $d" >&2; exit 2; }
done
prev_ver=$(dpkg-deb --field "$prev_deb" Version)
cur_ver=$(dpkg-deb --field "$cur_deb" Version)
if [ "$prev_ver" = "$cur_ver" ]; then
  echo "lifecycle: prev and cur have the same package version ($prev_ver); no upgrade to verify" >&2
  exit 2
fi

ts=$(date +%Y%m%d-%H%M%S)
local_tmp=$(mktemp -d) || exit 2
trap 'rm -rf "$local_tmp"' EXIT

echo "lifecycle: target=$target prev=$prev_ver cur=$cur_ver"
echo "lifecycle: pre-flight: ssh + passwordless sudo + tooling"
ssh -o BatchMode=yes "$target" '
  set -e
  sudo -n true
  command -v apt-get >/dev/null
  command -v curl >/dev/null
  . /etc/os-release
  echo "lifecycle: device: $PRETTY_NAME arm64"
' >/dev/null || { echo "FAIL: preflight (ssh/sudo/apt/curl)" >&2; exit 1; }

# Generate the remote driver with the resolved versions and timestamp.
sed -e "s|__PREV_VER__|${prev_ver}|g" \
    -e "s|__CUR_VER__|${cur_ver}|g" \
    -e "s|__TS__|${ts}|g" \
    "$(dirname "$0")/lifecycle-remote.sh" > "$local_tmp/run.sh"

echo "lifecycle: transferring packages + driver to $target:/tmp/gn-lc/"
ssh -o BatchMode=yes "$target" 'mkdir -p /tmp/gn-lc'
scp -q -o BatchMode=yes "$prev_deb" "$target:/tmp/gn-lc/prev.deb" || exit 1
scp -q -o BatchMode=yes "$cur_deb"  "$target:/tmp/gn-lc/cur.deb"  || exit 1
scp -q -o BatchMode=yes "$local_tmp/run.sh" "$target:/tmp/gn-lc/run.sh" || exit 1

echo "lifecycle: running the lifecycle on $target (this takes a while)"
rc=0
ssh -o BatchMode=yes "$target" 'bash /tmp/gn-lc/run.sh' || rc=$?

if [ "$rc" -eq 0 ]; then
  echo "lifecycle: all steps passed; device runs $cur_ver"
else
  echo "lifecycle: FAILED (rc=$rc); the device may be mid-cycle — inspect the output above" >&2
fi
exit "$rc"
