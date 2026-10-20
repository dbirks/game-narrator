#!/bin/sh
# bluetooth-audio.sh USER@HOST [AUDIO_USER] [--apply]
#
# Reports (read-only) the Bluetooth audio state on the UNO Q appliance, and
# with --apply applies the two user-level, reversible prerequisites of the
# proven recipe (issue #2/#3, 2026-10-19): `loginctl enable-linger` for the
# audio-owning user and the WirePlumber headless override
# (monitor.bluez.seat-monitoring = disabled) — without which a headless
# board over SSH pairs the headset but creates NO audio sink. Pairing,
# trust and connect you drive yourself with bluetoothctl (the full recipe
# and the known A2DP SBC stutter are in references/audio.md).
#
# Read-only guarantee (default): reads bluetoothctl state, linger, the
# override file (existence only), the default sink and ALSA cards. It never
# prints /etc/game-narrator/env and never touches sshd or network config.
# --apply only: enable-linger (idempotent), writes the user-level override
# conf (overwrites its own file only), restarts that user's wireplumber.
#
# AUDIO_USER defaults to the SSH user.
# Usage: skills/uno-q-setup/scripts/bluetooth-audio.sh <user>@<uno-q-host> [audio-user] [--apply]
# Exit:  0; the report is the result (a missing sink is a finding, not an
#        error — the USB DAC stays the baseline).

set -u

apply=0
target=""
audio_user=""

for a in "$@"; do
	case $a in
	--apply) apply=1 ;;
	-*)
		echo "unknown option: $a (usage: $0 USER@HOST [AUDIO_USER] [--apply])" >&2
		exit 2
		;;
	*)
		if [ -z "$target" ]; then
			target=$a
		elif [ -z "$audio_user" ]; then
			audio_user=$a
		else
			echo "unexpected argument: $a" >&2
			exit 2
		fi
		;;
	esac
done

if [ -z "$target" ]; then
	echo "usage: $0 USER@HOST [AUDIO_USER] [--apply]" >&2
	exit 2
fi
# AUDIO_USER must be a plain identifier (it is interpolated into the remote
# shell command); validate it here.
case "$audio_user" in
''|*[!A-Za-z0-9_-]*)
	if [ -n "$audio_user" ]; then
		echo "invalid AUDIO_USER: $audio_user" >&2
		exit 2
	fi
	audio_user=${target%%@*}
	;;
esac

# shellcheck disable=SC2087  # intentional: $audio_user expands locally; the \$ escapes expand on the board
out=$(
	ssh -o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new \
		"$target" sh -s <<REMOTE
AUDIO_USER='$audio_user'
# shellcheck disable=SC2153  # remote variable set on the line above
u=\$AUDIO_USER
echo "audio-user: \$u"
echo "home: \$(getent passwd "\$u" 2>/dev/null | cut -d: -f6)"
echo "radio: \$(bluetoothctl show 2>/dev/null | sed -n 's/^\s*Powered: //p')"
echo "paired: \$(bluetoothctl devices Paired 2>/dev/null | tr '\n' ' ')"
echo "connected: \$(bluetoothctl devices Connected 2>/dev/null | tr '\n' ' ')"
echo "linger: \$(loginctl show-user "\$u" -p Linger --value 2>/dev/null)"
h=\$(getent passwd "\$u" 2>/dev/null | cut -d: -f6)
if [ -n "\$h" ] && sudo -n test -f "\$h/.config/wireplumber/wireplumber.conf.d/80-bluez-headless.conf" 2>/dev/null; then
	echo "headless-override: present"
else
	echo "headless-override: missing-or-unreadable (needed for the sink on a headless board)"
fi
ds=\$(wpctl get-default sink 2>/dev/null | sed -n 1p)
case "\$ds" in
Usage:*|"") ds="n/a" ;;
esac
echo "default-sink: \$ds"
echo "alsa-cards: \$(grep -c . /proc/asound/cards 2>/dev/null)"
sed -n "s/^[[:space:]]*[0-9][[:space:]]*\[[^]]*\]:[[:space:]]*//p" /proc/asound/cards 2>/dev/null | sed "s/^/alsa-card: /"
REMOTE
)
printf '%s\n' "$out"

if [ "$apply" -eq 0 ]; then
	echo
	echo "read-only. The proven recipe (references/audio.md):"
	echo "  1. bluetoothctl --timeout 25 scan on; bluetoothctl devices"
	printf '%s\n' "  2. printf 'agent NoInputNoOutput\\\\ndefault-agent\\\\npair <MAC>\\\\ntrust <MAC>\\\\nconnect <MAC>\\\\n' | bluetoothctl"
	echo "  3. rerun with --apply (linger + headless override), then verify:"
	echo "     wpctl get-default sink   # want bluez_output.<MAC>.1 (a2dp-sink, SBC)"
	echo "     pw-play <chime.wav> && pw-play <tts-line.wav>"
	echo "  4. if a dual-mode headset comes back LE-only: remove <MAC>, rescan with"
	echo "     'menu scan' -> transport bredr, then pair/trust/connect again."
	echo "  Known: A2DP SBC stutter (issue #3) — clock.force-quantum 256 helped;"
	echo "  USB DAC remains the dependable baseline."
	exit 0
fi

echo "-- applying for audio user '$audio_user' --"
# shellcheck disable=SC2087  # $AUDIO_USER is set locally; the \$ escapes expand on the board
if ! ssh -o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new "$target" sh -s <<REMOTE
set -e
AUDIO_USER='$audio_user'
u=\$AUDIO_USER
h=\$(getent passwd "\$u" 2>/dev/null | cut -d: -f6)
[ -n "\$h" ] || { echo "FAIL: no passwd entry for \$u" >&2; exit 1; }
echo "sudo loginctl enable-linger \$u"
sudo loginctl enable-linger "\$u"
mkdir -p /tmp/gn-bt-override
cat > /tmp/gn-bt-override/80-bluez-headless.conf <<'EOF'
# game-narrator: headless UNO Q has no local seat; without this override
# WirePlumber creates no Bluetooth audio sink (issue #2/#3). User-level,
# reversible: delete this file + systemctl --user restart wireplumber.
wireplumber.profiles = { main = { monitor.bluez.seat-monitoring = disabled } }
EOF
sudo install -d -m 700 -o "\$u" -g "\$u" "\$h/.config/wireplumber/wireplumber.conf.d"
sudo install -m 600 -o "\$u" -g "\$u" /tmp/gn-bt-override/80-bluez-headless.conf \
  "\$h/.config/wireplumber/wireplumber.conf.d/80-bluez-headless.conf"
sudo rm -rf /tmp/gn-bt-override
echo "override written: \$h/.config/wireplumber/wireplumber.conf.d/80-bluez-headless.conf"
echo "systemctl --user restart wireplumber (as \$u)"
sudo -u "\$u" systemctl --user restart wireplumber
REMOTE
then
	echo "FAIL: apply step failed (see above)" >&2
	exit 1
fi

echo
echo "applied (linger + headless override, wireplumber restarted). Now pair"
echo "and verify per references/audio.md — expect the default sink to become"
echo "bluez_output.<MAC>.1 (a2dp-sink, SBC); test with pw-play. Remember the"
echo "known A2DP SBC stutter (issue #3) and the USB DAC baseline."
