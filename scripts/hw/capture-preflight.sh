#!/bin/sh
# capture-preflight.sh USER@HOST
#
# Read-only preflight for the HDMI capture path on the UNO Q appliance
# (docs/manuals/18-hdmi-capture-on-uno-q.md). Run from the dev host over
# ssh; prints a PASS/WARN/FAIL table for the first-boot checks of manual 18
# (USB host bus, capture node + formats, USB link speed, UAC audio, service
# + doctor).
#
# Read-only guarantee: the remote probe only reads /sys, /proc and runs
# lsusb, v4l2-ctl, udevadm, dmesg, systemctl is-active,
# `game-narrator doctor` (the app's own diagnostics; it probes configured
# components and does not mutate service state). It writes no file on the
# device, and never touches sshd, sshd_config, authorized_keys, the
# network, /etc/game-narrator, the USB role or the service.
#
# Usage (from anywhere):
#
#   scripts/hw/capture-preflight.sh <user>@<uno-q-host>
#
# Exit codes: 0 = no FAIL row; 1 = at least one FAIL (or ssh failed).
# WARN rows do not fail the run (a machine without the dongle attached is
# the normal pre-arrival state and exits 1 only via its FAIL rows).

set -u

if [ $# -ne 1 ]; then
	echo "usage: $0 USER@HOST  (e.g. $0 <user>@<uno-q-host>)" >&2
	exit 1
fi

target=$1

fails=0
warns=0
rows=""

# add_row CHECK RESULT DETAIL
add_row() {
	case $2 in
	FAIL) fails=$((fails + 1));;
	WARN) warns=$((warns + 1));;
	esac
	rows="${rows}${1}	${2}	${3}
"
}

# get T:<tag>:<value> from the remote probe output
field() {
	printf '%s\n' "$out" | sed -n "s/^T:${1}://p" | head -n 1
}

out=$(
	ssh -o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new \
		"$target" sh -s <<'REMOTE'
# --- remote probe (read-only; POSIX sh) ------------------------------
os=$(uname -sr 2>/dev/null)
[ -r /etc/os-release ] && os="$os;$(. /etc/os-release && echo "${PRETTY_NAME:-}")"
echo "T:arch:$(uname -m 2>/dev/null)"
echo "T:os:$os"
echo "T:kernel:$(uname -r 2>/dev/null)"

# dwc3-qcom host-mode quirk status (manual 18 §"Is my board affected?"):
# image generation + current USB role + debugfs knob presence/value.
# data_role is unreliable (always shows "host [device]" per Arduino staff);
# the usb_role_switch symlinks are the observable role.
bi=NONE
if [ -r /etc/buildinfo ]; then
	bi=$(grep -h '^BUILD_ID=' /etc/buildinfo 2>/dev/null | head -n 1)
fi
[ -n "$bi" ] || bi=NONE
echo "T:buildinfo:$bi"

ur=unknown
for d in /sys/class/usb_role/*/; do
	[ -e "${d}host" ] && ur=host
	[ -e "${d}device" ] && ur=device
done
echo "T:usb_role:$ur"

tp=$(cat /sys/class/typec/port0/power_role 2>/dev/null)
echo "T:typec_power:${tp:-unknown}"

mk=absent
if [ -e /sys/kernel/debug/usb/4e00000.usb/mode ]; then
	mk=$(cat /sys/kernel/debug/usb/4e00000.usb/mode 2>/dev/null)
	[ -n "$mk" ] || mk=unreadable
fi
echo "T:modeknob:$mk"

lc=MISSING
le=0
ls=-
if command -v lsusb >/dev/null 2>&1; then
	r=$(lsusb 2>/dev/null)
	lc=$(printf '%s\n' "$r" | grep -c '^Bus')
	# entries that are not the built-in root hubs (1d6b = Linux Foundation)
	le=$(printf '%s\n' "$r" | grep '^Bus' | grep -vc 'ID 1d6b:')
	ls=$(printf '%s\n' "$r" | head -n 8 | tr '\n' '|')
fi
echo "T:lsusb_count:$lc"
echo "T:lsusb_ext:$le"
echo "T:lsusb:$ls"

v4=MISSING
dl=-
if command -v v4l2-ctl >/dev/null 2>&1; then
	v4=OK
	dl=$(v4l2-ctl --list-devices 2>/dev/null | sed -e 's/^[[:space:]]*//' | head -n 20 | tr '\n' '|')
fi
echo "T:v4l2:$v4"
echo "T:devlist:$dl"

# capture node = first /dev/video* whose formats include Video Capture and
# YUYV or MJPG (the UVC card; the Venus codec nodes do not enumerate these)
cap=-
if [ "$v4" = OK ]; then
	for d in /dev/video*; do
		[ -e "$d" ] || continue
		f=$(v4l2-ctl -d "$d" --list-formats-ext 2>/dev/null) || continue
		if printf '%s' "$f" | grep -q 'Video Capture' && \
			printf '%s' "$f" | grep -qE 'YUYV|MJPG'; then
			cap=$d
			break
		fi
	done
fi

cname=-
cf=-
if [ "$cap" != "-" ]; then
	cname=$(v4l2-ctl --list-devices 2>/dev/null | awk -v d="$cap" '
		/:[[:space:]]*$/ { name = $0 }
		/\/dev\/video[0-9]+$/ {
			t = $0
			gsub(/[[:space:]]+/, "", t)
			if (t == d) { print name; exit }
		}')
	cf=$(v4l2-ctl -d "$cap" --list-formats-ext 2>/dev/null \
		| grep -E "Pixel Format|Size:|Interval:" | head -n 60 | tr '\n' '|')
fi
echo "T:cap:$cap"
echo "T:capname:$cname"
echo "T:capfmts:$cf"

# USB link speed of the capture node's device (sysfs, via udevadm path)
sp=UNKNOWN
if [ "$cap" != "-" ] && command -v udevadm >/dev/null 2>&1; then
	p=$(udevadm info -q path -n "$cap" 2>/dev/null)
	if [ -n "$p" ]; then
		# <...>/usbX/X-Y/X-Y:I.J/video4linuxN  ->  <...>/usbX/X-Y
		ddir=$(printf '%s' "$p" | sed -E 's|/[^/]+:[0-9]+\.[0-9]+(/.*)?$||')
		[ -r "$ddir/speed" ] && sp=$(cat "$ddir/speed" 2>/dev/null)
	fi
fi
echo "T:speed:$sp"

ac=absent
[ -r /proc/asound/cards ] && ac=$(head -n 6 /proc/asound/cards | sed -e 's/^[[:space:]]*//' | tr '\n' '|')
echo "T:audio:$ac"

dm=$(dmesg 2>/dev/null | grep -iE \
	'over-current|port [0-9]+: (unauthorized|not ready)|device descriptor read' \
	| tail -n 3 | tr '\n' '|')
[ -n "$dm" ] || dm=none
echo "T:dmesg:$dm"

sv=$(systemctl is-active game-narrator 2>/dev/null) || sv=unknown
echo "T:service:$sv"

dr=-
dt=-
if command -v game-narrator >/dev/null 2>&1; then
	dout=$(timeout 180 game-narrator doctor 2>&1)
	dr=$?
	dt=$(printf '%s\n' "$dout" | tail -n 4 | tr '\n' '|' | head -c 400)
fi
echo "T:doctor_rc:$dr"
echo "T:doctor_tail:$dt"
REMOTE
)
ssh_rc=$?

echo "capture-preflight: $target"
if [ $ssh_rc -ne 0 ]; then
	add_row ssh FAIL "ssh to $target failed (rc=$ssh_rc): check reachability and that ssh is in device mode (manual 18)"
	printf '%-14s %-6s %s\n' "CHECK" "RESULT" "DETAIL"
	printf '%s' "$rows"
	exit 1
fi
add_row ssh PASS "connected (arch=$(field arch), $(field os))"

# platform (informational: only the appliance profile is expected)
case $(field arch) in
aarch64) add_row platform PASS "arm64 ($(field os))";;
*) add_row platform WARN "not arm64 ($(field arch)) — expected on the UNO Q";;
esac

# USB host bus
lc=$(field lsusb_count)
le=$(field lsusb_ext)
case $lc in
MISSING) add_row usb-host FAIL "lsusb missing on device: apt install usbutils";;
0) add_row usb-host FAIL "USB host bus not up (no lsusb entries) — UNO Q is in device mode; attach the powered USB-C hub (manual 18 §Wiring)";;
*)
	if [ "$le" -gt 0 ]; then
		add_row usb-host PASS "host bus up: $lc entries, $le external: $(field lsusb | head -c 120)"
	else
		add_row usb-host WARN "only built-in hubs listed — attach the powered hub + dongle"
	fi
	;;
esac

# USB host role + dwc3-qcom quirk (manual 18 §"Is my board affected?")
ur=$(field usb_role)
bi=$(field buildinfo)
knob=$(field modeknob)
kr=$(field kernel)
if [ "$bi" = NONE ] || [ -z "$bi" ]; then
	if [ "$ur" = host ]; then
		add_row usb-role WARN "no /etc/buildinfo (pre-2025-11 image, normally stuck in device mode) but role=host — record kernel $kr in the run notes"
	else
		add_row usb-role FAIL "older image (no /etc/buildinfo) in $ur mode — affected by the dwc3-qcom device-mode quirk: update the image with arduino-flasher-cli or bridge JCTL VOL_DOWN-GND at boot (manual 18 §\"Is my board affected?\")"
	fi
else
	case $ur in
	host)
		add_row usb-role PASS "role=host, image $bi (newer than the quirk; debugfs knob: $knob)"
		;;
	device)
		add_row usb-role WARN "role=device (expected while cabled to the dev PC); image $bi (kernel $kr) is newer than the quirk and should take host role when the powered hub is attached; knob $knob — after wiring, re-run and expect role=host; if it stays device: boot the board before attaching the hub, then apply the manual 18 remediation steps (user approval required)"
		;;
	*)
		add_row usb-role WARN "USB role unreadable ($ur) on image $bi — check /sys/class/usb_role/ by hand (manual 18 §\"Is my board affected?\")"
		;;
	esac
fi

# v4l2 tooling
case $(field v4l2) in
OK) add_row v4l2-tooling PASS "v4l2-ctl present: $(field devlist | head -c 120)";;
*) add_row v4l2-tooling FAIL "v4l2-ctl missing on device: apt install v4l-utils";;
esac

# capture node
cap=$(field cap)
if [ "$cap" = "-" ]; then
	add_row capture-node FAIL "no capture device with YUYV/MJPG modes (expected until the dongle is attached)"
	add_row formats SKIP "n/a (no capture node)"
	add_row usb-speed SKIP "n/a (no capture node)"
else
	add_row capture-node PASS "$cap — $(field capname)"
	case $(field capfmts) in
	*YUYV*1920x1080*) add_row formats PASS "YUYV incl. 1920x1080: $(field capfmts | head -c 160)";;
	*YUYV*) add_row formats WARN "YUYV present but no 1920x1080 listed (v4l2-ctl may under-enumerate MS2130 modes — verify with yavta --enum-formats /dev/videoN; expect 1080p50/30/20/10 or 720p60)";;
	*MJPG*) add_row formats WARN "MJPEG only (no YUYV) — this is a USB2-class MS2109 card or a degraded link; use 1080p30 MJPG";;
	*) add_row formats FAIL "no usable capture formats: $(field capfmts | head -c 160)";;
	esac

	sp=$(field speed)
	case $sp in
	5000|10000|20000) add_row usb-speed PASS "SuperSpeed link ($sp Mb/s) — YUYV 1080p60 is feasible";;
	480|12|1.5) add_row usb-speed FAIL "USB 2 link ($sp Mb/s): YUYV 1080p60 will drop frames — move the hub to a USB 3 port or capture 720p/MJPG (manual 18 §Modes)";;
	*) add_row usb-speed WARN "link speed unreadable ($sp) — check with lsusb -t; a USB 2 link degrades 1080p60 YUYV";;
	esac
fi

# UAC audio (the card exposes HDMI audio as a USB audio card)
ac=$(field audio)
case $ac in
absent|*"no soundcards"*)
	add_row audio WARN "no sound cards on the device (expected before the DAC/dongle are attached; recheck after wiring)"
	;;
*)
	if [ "$cap" != "-" ]; then
		capshort=$(printf '%s' "$(field capname)" | head -c 12)
		if [ -n "$capshort" ] && printf '%s' "$ac" | grep -qi "$capshort"; then
			add_row audio PASS "UAC card present: $(printf '%s' "$ac" | head -c 140)"
		else
			add_row audio WARN "no sound card matching the capture device — HDMI audio may not be routed (or the card name differs); verify with: pactl list sources short"
		fi
	else
		add_row audio PASS "audio cards present (no capture device to match yet): $(printf '%s' "$ac" | head -c 140)"
	fi
	;;
esac

# USB power/link errors in dmesg (read-only; needs permissions on some hosts)
dm=$(field dmesg)
case $dm in
none) add_row usb-power PASS "no USB power/link errors in dmesg";;
*) add_row usb-power WARN "dmesg USB errors — possible power/link problem (hub port? powered hub?): $dm";;
esac

# service + doctor
case $(field service) in
active) add_row service PASS "game-narrator service is active";;
inactive|failed|unknown) add_row service FAIL "game-narrator service not active ($(field service)): systemctl status game-narrator";;
*) add_row service WARN "service state: $(field service)";;
esac

dr=$(field doctor_rc)
if [ "$dr" = "-" ]; then
	add_row doctor WARN "game-narrator not on PATH — run the binary's doctor manually (manual 01)"
else
	if [ "$dr" = "0" ]; then
		add_row doctor PASS "game-narrator doctor rc=0: $(field doctor_tail | head -c 140)"
	else
		add_row doctor FAIL "game-narrator doctor rc=$dr: $(field doctor_tail | head -c 160)"
	fi
fi

printf '\n%-14s %-6s %s\n' "CHECK" "RESULT" "DETAIL"
printf '%s' "$rows"
printf -- '-\n'
echo "summary: $fails FAIL, $warns WARN — exit $([ $fails -eq 0 ] && echo 0 || echo 1)"
[ $fails -eq 0 ]
