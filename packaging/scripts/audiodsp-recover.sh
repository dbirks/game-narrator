#!/bin/sh
# Game Narrator - UNO Q audio-DSP recovery (issue #24 / ADR 0118).
#
# Runs once per boot from game-narrator-audiodsp-recover.service (root,
# after a short settle). It is BOUNDED and IDEMPOTENT: on a healthy boot
# (the card is in /proc/asound/cards) and on non-UNO Q hardware it is a
# logged no-op; it restarts the ADSP remoteproc at most ONCE and waits at
# most GN_AUDIO_WAIT_SECS for the card. The outcome is logged to the
# journal (systemd captures stdout/stderr of the oneshot).
#
# The decision, with all evidence, is:
#   1. the board is a UNO Q — /proc/device-tree/compatible carries
#      "arduino,imola" (verified read-only on the real board, issue #24);
#   2. the ArduinoImolaHPH card is ABSENT from /proc/asound/cards;
#   3. dmesg carries the lpass deferred-probe signature
#      "Failed to get clk 'audio'" (the ADSP booted but its APR services
#      never registered, so the pinctrl → soundwire → codecs → sound probe
#      chain stayed deferred).
# All three hold => restart the ADSP once (stop, then start on its
# /sys/class/remoteproc/<adsp>/state file) and wait for the card.
#
# The roots and timings are env-overridable so the decision logic can be
# tested with fake /proc and /sys trees (internal/packaging tests) and the
# settle can be shortened when the unit is started by hand:
#   GN_AUDIO_PROC_ROOT      (default /proc)
#   GN_AUDIO_SYS_ROOT       (default /sys)
#   GN_AUDIO_SETTLE_SECS    (default 10 — good boots register the card a few
#                           seconds after the ADSP comes up; wait before
#                           declaring it missing)
#   GN_AUDIO_WAIT_SECS      (default 20 — bounded wait for the card after
#                           the restart)
#   GN_AUDIO_SIGNATURE_WAIT_SECS (default 60 — how long to wait, with the
#                           card absent, for the kernel's deferred-probe
#                           signature, which it logs ~40 s after boot)
#   GN_AUDIO_POLL_SECS      (default 1)
#   GN_AUDIO_DMESG_FILE     (default: run `dmesg`)
#
# Exit: 0 healthy no-op / other-hardware no-op / no-signature no-op /
# recovery succeeded / recovery attempted but the card came up late enough
# that nothing more can be done here. 1 the card is still absent after the
# restart (the journal and `systemctl status` show the failure; the doctor
# audio.dsp row carries the cause and the next step).
set -u

PROC_ROOT=${GN_AUDIO_PROC_ROOT:-/proc}
SYS_ROOT=${GN_AUDIO_SYS_ROOT:-/sys}
SETTLE_SECS=${GN_AUDIO_SETTLE_SECS:-10}
WAIT_SECS=${GN_AUDIO_WAIT_SECS:-20}
SIG_WAIT_SECS=${GN_AUDIO_SIGNATURE_WAIT_SECS:-60}
POLL_SECS=${GN_AUDIO_POLL_SECS:-1}
DMESG_FILE=${GN_AUDIO_DMESG_FILE:-}
CARD=ArduinoImolaHPH
SIGNATURE="Failed to get clk 'audio'"

log() { printf 'audiodsp-recover: %s\n' "$*"; }

# Numeric guards on the operator-overridable knobs (a stray value must
# degrade to the defaults, never feed `sleep` garbage).
case $SETTLE_SECS in ''|*[!0-9]*) SETTLE_SECS=10 ;; esac
case $WAIT_SECS in ''|*[!0-9]*) WAIT_SECS=20 ;; esac
case $SIG_WAIT_SECS in ''|*[!0-9]*) SIG_WAIT_SECS=60 ;; esac
case $POLL_SECS in ''|*[!0-9]*) POLL_SECS=1 ;; esac
[ "$POLL_SECS" -gt 0 ] || POLL_SECS=1

card_present() {
    [ -r "$PROC_ROOT/asound/cards" ] && grep -q "$CARD" "$PROC_ROOT/asound/cards"
}

# --- 0. short settle --------------------------------------------------------
# Good boots register the APR services ~1 s after the ADSP comes up and the
# card follows within a few seconds; wait before declaring the card missing
# (issue #24: healthy boots vs the 2026-10-20 bad cold boots).
if [ "$SETTLE_SECS" -gt 0 ]; then
    log "settling ${SETTLE_SECS}s after boot before checking the $CARD card"
    sleep "$SETTLE_SECS"
fi

# --- 1. UNO Q? ---------------------------------------------------------------
compatible=$(tr '\0' ' ' < "$PROC_ROOT/device-tree/compatible" 2>/dev/null || true)
case $compatible in
arduino,imola*|*arduino,imola*) ;;
*)
    log "not a UNO Q (/proc/device-tree/compatible does not carry arduino,imola); no-op"
    exit 0
    ;;
esac

# --- 2. card present? ---------------------------------------------------------
if card_present; then
    log "$CARD card present — healthy boot, no action"
    exit 0
fi
log "$CARD card NOT present in $PROC_ROOT/asound/cards"

# --- 3. the deferred-probe signature? ------------------------------------------
# The kernel logs "deferred probe pending: … Failed to get clk 'audio'" only
# when its deferred-probe timeout fires — ~40 s after boot, well after the
# settle (2026-10-22 cold boot: checked at ~20 s, signature at 42 s, so the
# unit gave up on a recoverable boot). Poll dmesg for up to SIG_WAIT_SECS
# while the card stays absent; a card that appears meanwhile is a slow
# healthy boot.
dmesg_text() {
    if [ -n "$DMESG_FILE" ]; then
        cat "$DMESG_FILE" 2>/dev/null || true
    else
        dmesg 2>/dev/null || true
    fi
}
sig_remaining=$SIG_WAIT_SECS
while :; do
    case $(dmesg_text) in
    *"$SIGNATURE"*) break ;;
    esac
    if card_present; then
        log "$CARD card appeared while waiting for the deferred-probe signature — slow healthy boot, no action"
        exit 0
    fi
    if [ "$sig_remaining" -le 0 ]; then
        log "card absent but no deferred-probe signature ($SIGNATURE) in dmesg after ${SIG_WAIT_SECS}s — a different fault; not acting"
        exit 0
    fi
    sleep "$POLL_SECS"
    sig_remaining=$((sig_remaining - POLL_SECS))
done
log "deferred-probe signature present in dmesg (the ADSP's APR services never registered)"

# --- 4. find the ADSP remoteproc -----------------------------------------------
adsp=""
for rp in "$SYS_ROOT/class/remoteproc"/remoteproc*; do
    [ -d "$rp" ] || continue
    name=$(cat "$rp/name" 2>/dev/null || true)
    case $name in
    *adsp*) adsp=$rp; break ;;
    esac
done
if [ -z "$adsp" ]; then
    log "no ADSP remoteproc under $SYS_ROOT/class/remoteproc — cannot restart it; no-op (check the board state manually: doctor audio.dsp)"
    exit 0
fi
state_file=$adsp/state
log "recovering: restarting the ADSP remoteproc ($adsp, current state: $(cat "$state_file" 2>/dev/null || echo unknown))"

# --- 5. stop, then start — once -----------------------------------------------
if ! echo stop >"$state_file" 2>/dev/null; then
    log "writing 'stop' to $state_file failed; not starting it (the board state is now indeterminate — a reboot is the clean next step)"
    exit 1
fi
if ! echo start >"$state_file" 2>/dev/null; then
    log "writing 'start' to $state_file failed"
    exit 1
fi
log "ADSP stop/start issued; waiting up to ${WAIT_SECS}s for the $CARD card"

# --- 6. bounded wait for the card ----------------------------------------------
# A countdown (not $SECONDS, which dash does not have): the loop runs while
# the budget is positive, sleeping POLL_SECS per poll — bounded by
# ceil(WAIT_SECS / POLL_SECS) sleeps.
remaining=$WAIT_SECS
while [ "$remaining" -gt 0 ]; do
    if card_present; then
        log "success: $CARD card present after the ADSP restart"
        exit 0
    fi
    sleep "$POLL_SECS"
    remaining=$((remaining - POLL_SECS))
done

log "FAILURE: the $CARD card is still absent after waiting ${WAIT_SECS}s for the ADSP restart (state: $(cat "$state_file" 2>/dev/null || echo unknown)) — re-run the doctor (audio.dsp row) and reboot if it persists"
exit 1
