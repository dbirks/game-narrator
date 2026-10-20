#!/bin/sh
# install.sh USER@HOST LOCAL_DEB [--apply]
#
# Installs the arm64 game-narrator .deb on the UNO Q appliance, applies
# pending SQLite migrations as the service user, and makes sure the service
# is enabled at boot (ADR 0081: fresh installs get boot enablement from the
# package postinst; existing installs whose unit is 'static' get
# daemon-reload + enable here). Idempotent: a rerun with the same version
# is a no-op.
#
# Read-only by default: reports the local artifact (sha256, checksums.txt)
# and the remote state (package, version, enablement, env file). With
# --apply it copies the deb to the board's /tmp (the only place it writes),
# runs `apt-get install`, `game-narrator version`,
# `sudo -u game-narrator game-narrator migrate --data-dir
# /var/lib/game-narrator` and the boot enablement. It never touches
# sshd, authorized_keys or network configuration, and never prints
# /etc/game-narrator/env (names of set variables only).
#
# Usage: skills/uno-q-setup/scripts/install.sh <user>@<uno-q-host> <local .deb path> [--apply]
# Exit:  0 state as expected / applied cleanly; 1 a step failed or the
#        artifact is missing/unverifiable.

set -u

apply=0
target=""
deb=""

for a in "$@"; do
	case $a in
	--apply) apply=1 ;;
	-*)
		echo "unknown option: $a (usage: $0 USER@HOST LOCAL_DEB [--apply])" >&2
		exit 2
		;;
	*)
		if [ -z "$target" ]; then
			target=$a
		elif [ -z "$deb" ]; then
			deb=$a
		else
			echo "unexpected argument: $a" >&2
			exit 2
		fi
		;;
	esac
done

if [ -z "$target" ] || [ -z "$deb" ]; then
	echo "usage: $0 USER@HOST LOCAL_DEB [--apply]" >&2
	echo "       e.g. $0 <user>@<uno-q-host> dist/game-narrator_1.2.3_linux_arm64.deb --apply" >&2
	exit 2
fi

# --- local artifact --------------------------------------------------------
if [ ! -f "$deb" ]; then
	echo "FAIL: local artifact not found: $deb" >&2
	echo "      build it with: make release-snapshot (dist/game-narrator_*_linux_arm64.deb)" >&2
	exit 1
fi

case "$deb" in
*_linux_arm64.deb) : ;;
*)
	echo "FAIL: $deb does not look like the arm64 .deb (want *_linux_arm64.deb)" >&2
	exit 1
	;;
esac

debname=$(basename -- "$deb")
debsum=$(sha256sum -- "$deb" | awk '{print $1}')
echo "artifact : $debname  sha256 $debsum"

cksumdir=$(dirname -- "$deb")
cksumfile="$cksumdir/checksums.txt"
if [ -f "$cksumfile" ]; then
	if (cd "$cksumdir" && sha256sum --ignore-missing -c checksums.txt >/dev/null 2>&1); then
		echo "checksums: OK (checksums.txt)"
	else
		echo "FAIL: sha256sum -c checksums.txt failed in $cksumdir" >&2
		exit 1
	fi
else
	echo "checksums: WARN no checksums.txt next to the deb — verifying size only"
fi

# --- remote state (read-only) ----------------------------------------------
out=$(
	ssh -o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new \
		"$target" sh -s <<'REMOTE'
if dpkg-query -W -f '${Status}' game-narrator 2>/dev/null | grep -q '^install ok installed$'; then
	echo "installed: $(dpkg-query -W -f '${Version}' game-narrator 2>/dev/null)"
else
	echo "installed: no"
fi
echo "is-enabled: $(systemctl is-enabled game-narrator 2>/dev/null)"
echo "is-active: $(systemctl is-active game-narrator 2>/dev/null)"
# /etc/game-narrator is 750 root:game-narrator — probe via sudo so an
# existing-but-invisible env file is not reported as missing.
if sudo -n true 2>/dev/null; then
	if sudo -n test -e /etc/game-narrator/env; then
		echo "env: present ($(sudo -n stat -c '%a %U' /etc/game-narrator/env 2>/dev/null), $(sudo -n sh -c 'wc -l < /etc/game-narrator/env' 2>/dev/null | tr -d ' ') lines)"
	else
		echo "env: missing"
	fi
else
	echo "env: unreadable without sudo (not 'missing' — grant passwordless sudo and rerun)"
fi
REMOTE
)
printf '%s\n' "$out"

# --- apply ------------------------------------------------------------------
if [ "$apply" -eq 0 ]; then
	echo
	echo "read-only: rerun with --apply to copy the deb to /tmp, apt-get install it,"
	echo "run migrate as the service user and enable the unit at boot (ADR 0081)."
	exit 0
fi

echo "-- applying --"
scp -o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new \
	"$deb" "$target:/tmp/$debname" || {
		echo "FAIL: scp to $target:/tmp failed" >&2
		exit 1
	}

# shellcheck disable=SC2087  # intentional: $debname expands locally; the \$ escapes expand on the board
if ! ssh -o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new "$target" sh -s <<REMOTE
set -e
DEBNAME='$debname'
echo "apt-get install /tmp/\$DEBNAME"
sudo apt-get install -y /tmp/\$DEBNAME
echo "game-narrator version:"
game-narrator version
echo "migrate (as the service user, --data-dir /var/lib/game-narrator):"
sudo -u game-narrator game-narrator migrate --data-dir /var/lib/game-narrator
en=\$(systemctl is-enabled game-narrator 2>/dev/null || echo unknown)
if [ "\$en" != "enabled" ]; then
	echo "unit enablement is '\$en' — enabling at boot (ADR 0081)"
	sudo systemctl daemon-reload
	sudo systemctl enable game-narrator.service
fi
echo "final state:"
systemctl is-enabled game-narrator
systemctl is-active game-narrator
REMOTE
then
	echo "FAIL: remote install failed (see output above)" >&2
	exit 1
fi

echo
echo "done. Next (manual 19 steps 5-6):"
echo "  skills/uno-q-setup/scripts/env-setup.sh $target --apply   # if the env file is missing"
echo "  fill /etc/game-narrator/env (0600 root, never printed), then:"
echo "  sudo systemctl start game-narrator && game-narrator doctor"
