#!/usr/bin/env bash
# scripts/board/cpu-tune.sh — set the CPU frequency floor on an Arduino UNO Q
# (QRB2210) for the game-narrator capture session (issue #58 / ADR-0183).
#
# Community characterization of the board shows a light periodic workload
# (our ~10 fps tick with short OCR bursts) leaving the frequency policy modal
# at its ~614 MHz floor, while free-running work holds 2.016 GHz — the step
# time difference between the regimes is mostly clock. This script closes the
# gap by writing, per cpufreq policy:
#
#   --performance        the "performance" governor (pins the max clock); or
#   --governor g         the given governor (default: schedutil) ...
#   --min-freq kHz       ... with scaling_min_freq raised above the floor.
#
# The writes are persisted by a systemd oneshot unit
# (game-narrator-cpu.service) that re-applies them at boot, so the tuning
# survives a reboot the same way the game-narrator.service does.
#
# Safety: DRY-RUN BY DEFAULT. Without --apply the script only prints the
# before/after state and the unit it would install. It is idempotent:
# re-running with --apply rewrites the same unit with the same content.
#
# Usage:
#   sudo scripts/board/cpu-tune.sh                    # dry run, schedutil + raised floor
#   scripts/board/cpu-tune.sh --min-freq 1600000      # dry run with an explicit floor (kHz)
#   scripts/board/cpu-tune.sh --performance --apply   # pin the performance governor
#   scripts/board/cpu-tune.sh --apply                 # apply the defaults

set -euo pipefail

GOVERNOR="schedutil"
MIN_FREQ=""          # kHz; empty = keep whatever scaling_min_freq already is
PERFORMANCE=0
DRY_RUN=1
UNIT="/etc/systemd/system/game-narrator-cpu.service"
CPUFREQ_GLOB="/sys/devices/system/cpu/cpufreq/policy*"
THERMAL_GLOB="/sys/class/thermal/thermal_zone*/temp"

usage() {
    sed -n '2,32p' "$0" | sed 's/^# \{0,1\}//'
    exit "${1:-0}"
}

while [ $# -gt 0 ]; do
    case "$1" in
    --performance) PERFORMANCE=1 ;;
    --governor) GOVERNOR="${2:?--governor needs a value}"; shift ;;
    --governor=*) GOVERNOR="${1#--governor=}" ;;
    --min-freq) MIN_FREQ="${2:?--min-freq needs a value (kHz)}"; shift ;;
    --min-freq=*) MIN_FREQ="${1#--min-freq=}" ;;
    --apply) DRY_RUN=0 ;;
    -h|--help) usage 0 ;;
    *) echo "unknown flag: $1" >&2; usage 1 ;;
    esac
    shift
done

shopt -s nullglob
# shellcheck disable=SC2206 # deliberate: unquoted glob is the policy enumeration
policies=($CPUFREQ_GLOB)
if [ ${#policies[@]} -eq 0 ]; then
    echo "ERROR: no cpufreq policies found under /sys/devices/system/cpu/cpufreq/" >&2
    echo "       this script targets the UNO Q appliance; on a different host" >&2
    echo "       check that the cpufreq driver is loaded (lsmod | grep cpufreq)." >&2
    exit 1
fi

if [ -n "$MIN_FREQ" ]; then
    case "$MIN_FREQ" in
    ''|*[!0-9]*) echo "ERROR: --min-freq must be a kHz integer, got '$MIN_FREQ'" >&2; exit 1 ;;
    esac
fi

sudo=""
if [ "$(id -u)" -ne 0 ]; then
    if command -v sudo >/dev/null 2>&1; then
        sudo="sudo"
    else
        echo "ERROR: not root and no sudo; rerun with elevated privileges." >&2
        exit 1
    fi
fi

print_state() {
    local label="$1" d
    echo "== $label =="
    for d in $CPUFREQ_GLOB; do
        local base governor min max cur
        base="$(basename "$d")"
        governor="$(cat "$d/scaling_governor" 2>/dev/null || echo '?')"
        min="$(cat "$d/scaling_min_freq" 2>/dev/null || echo '?')"
        max="$(cat "$d/scaling_max_freq" 2>/dev/null || echo '?')"
        cur="$(cat "$d/scaling_cur_freq" 2>/dev/null || echo '?')"
        echo "  $base: governor=$governor min=${min}kHz max=${max}kHz cur=${cur}kHz"
    done
    local t temps="" zone milli c
    for t in $THERMAL_GLOB; do
        zone="$(basename "$(dirname "$t")")"
        milli="$(cat "$t" 2>/dev/null || echo "")"
        [ -n "$milli" ] || continue
        c="$(awk -v m="$milli" 'BEGIN {printf "%.1f", m/100}')"
        temps+="${zone}=${c}C "
    done
    [ -n "$temps" ] && echo "  thermal: ${temps% }"
    echo
}

echo "CPU tuning for the UNO Q (issue #58). Mode: $([ $DRY_RUN -eq 1 ] && echo "DRY RUN" || echo "APPLY")"
echo "Target: governor=$([ $PERFORMANCE -eq 1 ] && echo performance || echo "$GOVERNOR")" \
    "scaling_min_freq=$([ -n "$MIN_FREQ" ] && echo "${MIN_FREQ}kHz" || echo "unchanged")"
echo "Unit:   $UNIT"
echo

print_state "before"

if [ "$DRY_RUN" -eq 1 ]; then
    cat <<'EOF'
dry run: nothing was written. Re-run with --apply to install and start the
unit, e.g.   sudo $0 --apply
EOF
    exit 0
fi

# The oneshot body: write the governor first (scaling_min_freq is unreadable
# under the "performance" governor, and vice versa the governor write is the
# no-op that matters there), then the raised floor if one was given.
body() {
    echo "for d in /sys/devices/system/cpu/cpufreq/policy*; do"
    if [ "$PERFORMANCE" -eq 1 ]; then
        echo "    echo performance > \"\$d/scaling_governor\" || true"
    else
        echo "    echo $GOVERNOR > \"\$d/scaling_governor\" || true"
        if [ -n "$MIN_FREQ" ]; then
            echo "    echo $MIN_FREQ > \"\$d/scaling_min_freq\" || true"
        fi
    fi
    echo "done"
}

tmp="$(mktemp)"
cat > "$tmp" <<EOF
[Unit]
Description=Game Narrator CPU frequency floor (issue #58, ADR 0183)
After=multi-user.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/sh -c "$(body | tr '\n' ' ')"

[Install]
WantedBy=multi-user.target
EOF

# shellcheck disable=SC2086 # sudo is intentionally empty when root
$sudo cat "$tmp" > "$UNIT"
rm -f "$tmp"

# shellcheck disable=SC2086
$sudo systemctl daemon-reload
# shellcheck disable=SC2086
$sudo systemctl enable "$UNIT"
# shellcheck disable=SC2086
$sudo systemctl restart "$UNIT"
# shellcheck disable=SC2086
$sudo systemctl is-active --quiet "$UNIT" || {
    echo "ERROR: the unit did not come up; see 'journalctl -u $(basename "$UNIT")'" >&2
    exit 1
}

print_state "after"
echo "Applied. Re-measure capture fps / OCR latency / temperature before and"
echo "after this single change (docs/manuals/19-install-on-uno-q.md, CPU"
echo "frequency tuning). One variable at a time."
