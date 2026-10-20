#!/bin/sh
# preflight.sh USER@HOST
#
# Read-only, whole-appliance preflight for bringing a stock UNO Q up as a
# Game Narrator appliance (skills/uno-q-setup/SKILL.md, step 1). Runs from
# the dev host over ssh (BatchMode); it only reads board state — no file
# writes, no service changes, no package operations, and it never prints
# /etc/game-narrator/env contents (mode, owner, line count and the *names*
# of set variables only).
#
# The USB/capture/audio/doctor rows come from the repo's own read-only
# preflight (scripts/hw/capture-preflight.sh); this script adds the
# appliance-level rows: ssh, sudo, arch, os, package, service, env-file,
# data-dir, hdmichain.
#
# Usage: skills/uno-q-setup/scripts/preflight.sh <user>@<uno-q-host>
# Exit:  0 no FAIL rows; 1 at least one FAIL (or ssh unreachable).

set -u

if [ $# -ne 1 ]; then
	echo "usage: $0 USER@HOST  (e.g. $0 <user>@<uno-q-host>)" >&2
	exit 1
fi
target=$1

fails=0
warns=0

row() {
	printf '%s\t%s\t%s\n' "$1" "$2" "$3"
	case $2 in
	FAIL) fails=$((fails + 1)) ;;
	WARN) warns=$((warns + 1)) ;;
	esac
}

# --- one read-only remote probe ------------------------------------------
if ! out=$(
	ssh -o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new \
		"$target" sh -s <<'REMOTE'
echo "T:arch:$(uname -m 2>/dev/null)"
if [ -r /etc/os-release ]; then
	. /etc/os-release
	echo "T:os:${PRETTY_NAME:-unknown}"
else
	echo "T:os:unknown"
fi
if sudo -n true 2>/dev/null; then echo "T:sudo:yes"; else echo "T:sudo:no"; fi
if dpkg-query -W -f '${Status}' game-narrator 2>/dev/null | grep -q '^install ok installed$'; then
	echo "T:pkg:installed"
	echo "T:pkgver:$(game-narrator version 2>/dev/null | head -n 1)"
else
	echo "T:pkg:missing"
	echo "T:pkgver:"
fi
echo "T:svc-enabled:$(systemctl is-enabled game-narrator 2>/dev/null)"
echo "T:svc-active:$(systemctl is-active game-narrator 2>/dev/null)"
# /etc/game-narrator is 750 root:game-narrator — the ssh user cannot see
# the env file, so probe through passwordless sudo (names only, never
# values). Without sudo the file is not "missing", only unreadable.
if sudo -n true 2>/dev/null; then
	if sudo -n test -e /etc/game-narrator/env; then
		echo "T:env:present"
		echo "T:env-meta:$(sudo -n stat -c '%a %U' /etc/game-narrator/env 2>/dev/null)"
		echo "T:env-lines:$(sudo -n sh -c 'wc -l < /etc/game-narrator/env' 2>/dev/null | tr -d ' ')"
		echo "T:env-vars:$(sudo -n sh -c "grep -o '^[A-Za-z_][A-Za-z_0-9]*=' /etc/game-narrator/env" 2>/dev/null | sed 's/=$//' | sort -u | tr '\n' ' ')"
	else
		echo "T:env:missing"
	fi
else
	echo "T:env:unreadable"
fi
if [ -d /var/lib/game-narrator ]; then echo "T:datadir:present"; else echo "T:datadir:missing"; fi
REMOTE
)
then
	row ssh FAIL "ssh to $target failed — key auth / BatchMode? (manual 19 Preconditions; never touch sshd/authorized_keys)"
	row sudo FAIL "not reachable"
	row arch FAIL "not reachable"
	row os FAIL "not reachable"
	row package FAIL "not reachable"
	row service FAIL "not reachable"
	row env-file FAIL "not reachable"
	row data-dir WARN "not reachable"
	echo
	echo "preflight: $fails FAIL, $warns WARN"
	exit 1
fi

# get T:<tag>:<value> from the probe output
field() {
	printf '%s\n' "$out" | sed -n "s/^T:${1}://p" | head -n 1
}

row ssh PASS "key auth, BatchMode"

case "$(field sudo)" in
yes) row sudo PASS "passwordless sudo" ;;
*) row sudo FAIL "sudo -n failed — lifecycle/preflight scripts require passwordless sudo (manual 19 Preconditions)" ;;
esac

case "$(field arch)" in
aarch64) row arch PASS "$(field arch)" ;;
*) row arch FAIL "unexpected arch '$(field arch)' — the appliance is arm64 (Debian 13)" ;;
esac

case "$(field os)" in
*trixie*|*13*) row os PASS "$(field os)" ;;
*) row os WARN "not Debian 13 (trixie): '$(field os)' — the skill targets the stock Debian 13 image" ;;
esac

case "$(field pkg)" in
installed) row package PASS "v$(field pkgver)" ;;
*) row package FAIL "not installed — skills/uno-q-setup/scripts/install.sh <user>@<uno-q-host> <arm64.deb> --apply" ;;
esac

# service row: only meaningful once the package is installed
if [ "$(field pkg)" = installed ]; then
	case "$(field svc-enabled)" in
	enabled)
		if [ "$(field svc-active)" = "active" ]; then
			row service PASS "enabled, active"
		else
			row service WARN "enabled but not active ($(field svc-active)) — sudo systemctl start game-narrator; needs /etc/game-narrator/env, then doctor (manual 19 step 6)"
		fi
		;;
	*)
		if [ "$(field svc-enabled)" = "static" ]; then
			row service FAIL "unit '$(field svc-enabled)' — installed build predates ADR 0081 (unit has no [Install]): upgrade to a build with ADR 0081, or sudo systemctl enable after the upgrade"
		else
			row service FAIL "unit '$(field svc-enabled)' — not enabled at boot: install.sh --apply (ADR 0081) or sudo systemctl enable game-narrator"
		fi
		;;
	esac
else
	row service WARN "no package yet — enablement is set by the install (ADR 0081)"
fi

case "$(field env)" in
present)
	if [ "$(field env-meta)" = "600 root" ]; then
		row env-file PASS "0600 root, $(field env-lines) lines; set: $(field env-vars)"
	else
		row env-file FAIL "wrong mode/owner '$(field env-meta)' (want 600 root) — sudo chmod 0600 /etc/game-narrator/env && sudo chown root:root /etc/game-narrator/env"
	fi
	;;
unreadable)
	row env-file WARN "unreadable without sudo (/etc/game-narrator is 750 root:game-narrator) — grant passwordless sudo (manual 19 Preconditions) and rerun"
	;;
*)
	if [ "$(field pkg)" = installed ]; then
		row env-file FAIL "missing — skills/uno-q-setup/scripts/env-setup.sh <user>@<uno-q-host> --apply, then fill values (manual 19 step 5; never printed)"
	else
		row env-file WARN "missing (package not installed yet — after install: env-setup.sh --apply)"
	fi
	;;
esac

case "$(field datadir)" in
present) row data-dir PASS "/var/lib/game-narrator" ;;
*) row data-dir WARN "absent — systemd creates it at service start (StateDirectory); normal before first start" ;;
esac

# --- HDMI chain via the repo's own read-only preflight ---------------------
here=$(unset CDPATH; cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(unset CDPATH; cd -- "$here/../../.." && pwd)
cp="$repo_root/scripts/hw/capture-preflight.sh"
if [ -f "$cp" ]; then
	echo "-- HDMI chain: $cp (read-only) --"
	sh "$cp" "$target"
	rc=$?
	if [ "$rc" -eq 0 ]; then
		row hdmichain PASS "no FAIL rows (see table above)"
	else
		row hdmichain FAIL "FAIL rows above — fix per manual 18, rerun"
	fi
else
	row hdmichain WARN "repo scripts/hw/capture-preflight.sh not found at $cp — run it from the repo root separately"
fi

echo
echo "preflight: $fails FAIL, $warns WARN"
if [ "$fails" -gt 0 ]; then
	exit 1
fi
exit 0
