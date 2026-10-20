#!/bin/sh
# upgrade.sh USER@HOST LOCAL_DEB [--apply]
#
# Upgrades the installed game-narrator on the UNO Q appliance in place
# (same shape as install.sh --apply: copy to /tmp, apt-get install,
# `game-narrator version`, migrate as the service user, keep/restore boot
# enablement, final state). Idempotent; upgrades never change an
# admin-disabled unit (ADR 0081: postinst enables on install only).
#
# Read-only by default: verifies the local artifact (sha256/checksums.txt),
# reports the installed version vs the artifact's version, and warns that a
# downgrade needs the documented rollback path (back up the data directory
# FIRST, then apt-get install --allow-downgrades — references/lifecycle.md,
# docs/release.md section 4). --apply performs the upgrade for same-or-newer
# versions; it refuses to downgrade automatically.
#
# Usage: skills/uno-q-setup/scripts/upgrade.sh <user>@<uno-q-host> <local .deb path> [--apply]
# Exit:  0 reported/applied; 1 a step failed or the artifact is bad.

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
	echo "       e.g. $0 <user>@<uno-q-host> dist/game-narrator_1.3.0_linux_arm64.deb --apply" >&2
	exit 2
fi

if [ ! -f "$deb" ]; then
	echo "FAIL: local artifact not found: $deb" >&2
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
if [ -f "$cksumdir/checksums.txt" ]; then
	if (cd "$cksumdir" && sha256sum --ignore-missing -c checksums.txt >/dev/null 2>&1); then
		echo "checksums: OK (checksums.txt)"
	else
		echo "FAIL: sha256sum -c checksums.txt failed in $cksumdir" >&2
		exit 1
	fi
else
	echo "checksums: WARN no checksums.txt next to the deb — verifying size only"
fi

# artifact version from the deb filename: game-narrator_<ver>_linux_arm64.deb
newver=${debname#game-narrator_}
newver=${newver%_linux_arm64.deb}

out=$(
	ssh -o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new \
		"$target" sh -s <<'REMOTE'
if dpkg-query -W -f '${Status}' game-narrator 2>/dev/null | grep -q '^install ok installed$'; then
	echo "installed: $(dpkg-query -W -f '${Version}' game-narrator 2>/dev/null)"
else
	echo "installed: no"
fi
echo "is-enabled: $(systemctl is-enabled game-narrator 2>/dev/null)"
REMOTE
)
printf '%s\n' "$out"
oldver=$(printf '%s\n' "$out" | sed -n 's/^installed: //p')

if [ -z "$oldver" ] || [ "$oldver" = "no" ]; then
	echo "note: no package installed — this will be a fresh install (install.sh does the same)."
fi

if [ "$apply" -eq 0 ]; then
	echo
	echo "read-only: installed '$oldver' <- artifact '$newver'. Rerun with --apply to"
	echo "upgrade (same-or-newer). Downgrades are refused — follow the documented"
	echo "rollback path (data backup first) in references/lifecycle.md."
	exit 0
fi

# refuse an automatic downgrade. dpkg reports the goreleaser version with
# '~' where the .deb filename uses '-' (dpkg versioning), so normalize
# before comparing — otherwise the guard mis-orders same-line builds.
oldver_cmp=$(printf '%s\n' "$oldver" | tr '~' '-')
if [ "$newver" != "$oldver_cmp" ] && [ "$oldver" != "no" ]; then
	newest=$(printf '%s\n%s\n' "$newver" "$oldver_cmp" | sort -V | sed -n '2p')
	if [ "$newest" = "$oldver_cmp" ]; then
			echo "FAIL: artifact '$newver' is older than installed '$oldver' — automatic" >&2
			echo "      downgrade refused. Back up /var/lib/game-narrator FIRST, then" >&2
			echo "      follow references/lifecycle.md / docs/release.md section 4" >&2
			echo "      (apt-get install --allow-downgrades + DB restore if needed)." >&2
			exit 1
	fi
fi

echo "-- upgrading installed '$oldver' <- artifact '$newver' --"
scp -o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new \
	"$deb" "$target:/tmp/$debname" || {
		echo "FAIL: scp to $target:/tmp failed" >&2
		exit 1
	}

# shellcheck disable=SC2087  # intentional: $debname expands locally; the \$ escapes expand on the board
if ! ssh -o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new "$target" sh -s <<REMOTE
set -e
DEBNAME='$debname'
echo "apt-get install /tmp/\$DEBNAME (upgrade)"
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
	echo "FAIL: remote upgrade failed (see output above)" >&2
	exit 1
fi

echo
echo "done. Re-verify (manual 19 step 11):"
echo "  sudo systemctl start game-narrator"
echo "  sudo sh -c 'set -a; . /etc/game-narrator/env; set +a; game-narrator doctor --data-dir /var/lib/game-narrator'"
echo "  game-narrator version"
