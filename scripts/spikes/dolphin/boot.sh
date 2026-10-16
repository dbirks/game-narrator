#!/usr/bin/env bash
# boot.sh — spike step 6b: does the user-supplied GameCube image actually boot
# and render GAME frames inside Dolphin on this headless host?
#
# usage: boot.sh [OGL|Vulkan] [budget-seconds]
#
# PASS requires, per docs/spec/12 ("Dolphin boot and screenshot") and
# docs/spec/11 ("Do not declare test passed because Dolphin process existed"):
#   * a screenshot whose entropy > 0.1
#   * that screenshot differs from the bare Dolphin GUI screenshot
#   * two successive screenshots differ (the emulated GPU is producing motion)
# The game image is only ever referenced by path; its hash is recorded.

set -euo pipefail
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
# shellcheck source=lib.sh
source scripts/spikes/dolphin/lib.sh

BACKEND="${1:-OGL}"
BUDGET="${2:-180}"
ENTROPY_MIN=0.1
TAG="boot-$BACKEND"
BASELINE="$ART/gui-root.png"

require_game_image
[[ -f "$BASELINE" ]] || log "WARN: baseline GUI screenshot missing ($BASELINE); run probe.sh first"

trap 'log "cleanup"; cleanup_all' EXIT
start_xvfb 1280x1024x24

EXTRA_ARGS=()
if [[ "${GAME_NARRATOR_SPIKE_LOWRES:-1}" == "1" ]]; then
  # llvmpipe is a CPU rasteriser: keep the internal resolution at 1x (native
  # GC 640x527) and never block on vsync, otherwise boot takes forever.
  EXTRA_ARGS=(--config=GPU.Settings.InternalResolution=1 --config=GPU.Hardware.VSync=False)
fi

log "== launching $TAG =="
dolphin_start "$ART/$TAG.log" --batch --exec="$GAME_NARRATOR_GAME_IMAGE_PATH" \
  --video_backend="$BACKEND" "${EXTRA_ARGS[@]}"
START=$(date +%s)

WIN=""
for ((i = 1; i <= 60; i++)); do
  WIN=$(dolphin_wait_window 1 2>/dev/null || true)
  [[ -n "$WIN" ]] && break
  dolphin_alive || break
done
if [[ -n "$WIN" ]]; then
  log "render window $WIN $(on_display xdotool getwindowname "$WIN" 2>/dev/null)"
  CROP=$(window_crop "$WIN" 2>/dev/null || true)
else
  log "no window yet — capturing root only"
  CROP=""
fi

PREV=""
PREV_CROP=""
RESULT_FILE="$ART/$TAG-frames.tsv"
printf 't_sec\tshot\tentropy\tvs_prev_AE\tvs_baseline_AE\tverdict\n' >"$RESULT_FILE"
FIRST_GAME_FRAME=""

for ((n = 1; n <= 999; n++)); do
  T=$(( $(date +%s) - START ))
  (( T > BUDGET )) && break
  png="$ART/$TAG-$(printf '%02d' "$n").png"
  dolphin_alive || { log "dolphin exited at t=${T}s"; break; }
  shot "$png"
  # re-resolve the window/crop each iteration: batch mode re-parents the
  # framebuffer window when the game takes over the screen
  if [[ -z "$WIN" ]] || ! on_display xdotool getwindowname "$WIN" >/dev/null 2>&1; then
    WIN=$(dolphin_wait_window 1 2>/dev/null || true)
  fi
  if [[ -n "$WIN" ]]; then
    CROP=$(window_crop "$WIN" 2>/dev/null || echo "")
  fi
  ENT=$(entropy_of "$png" "$CROP")
  [[ -n "$ENT" ]] || ENT=0
  PREV_AE="nan"
  BASE_AE="nan"
  if [[ -n "$PREV" ]]; then
    if [[ -n "$CROP" ]]; then
      a="$ART/.prev-$TAG.png"; b="$ART/.cur-$TAG.png"
      magick "$PREV" -crop "$CROP" +repage "$a" 2>/dev/null || cp "$PREV" "$a"
      magick "$png" -crop "$CROP" +repage "$b" 2>/dev/null || cp "$png" "$b"
      PREV_AE=$(diff_pixels "$a" "$b")
    else
      PREV_AE=$(diff_pixels "$PREV" "$png")
    fi
  fi
  if [[ -f "$BASELINE" ]]; then
    BASE_AE=$(diff_pixels "$png" "$BASELINE")
  fi
  VERDICT="menu/black"
  if awk -v e="$ENT" -v t="$ENTROPY_MIN" 'BEGIN{exit !(e+0>t+0)}' &&
    awk -v a="$BASE_AE" -v n=1000 'BEGIN{exit !(a+0>n)}' &&
    awk -v a="$PREV_AE" -v n=0 'BEGIN{exit !(a+0>n)}'; then
    VERDICT="GAME FRAME"
    [[ -z "$FIRST_GAME_FRAME" ]] && FIRST_GAME_FRAME=$T
  fi
  printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$T" "$(basename "$png")" "$ENT" "$PREV_AE" "$BASE_AE" "$VERDICT" >>"$RESULT_FILE"
  log "t=${T}s $(basename "$png") entropy=$ENT vs_prev=$PREV_AE vs_gui=$BASE_AE -> $VERDICT"
  PREV="$png"
  sleep 5
done

dolphin_stop

log "== $TAG result =="
if [[ -n "$FIRST_GAME_FRAME" ]]; then
  echo "$FIRST_GAME_FRAME" >"$ART/$TAG-first-frame-seconds.txt"
  log "first game frame at t=${FIRST_GAME_FRAME}s — see $ART/$TAG-$(printf '%02d' "$n").png / $ART/$TAG-frames.tsv"
  log "BOOT PASS ($BACKEND)"
else
  log "no screenshot satisfied entropy>0.1 AND differs-from-GUI AND motion — BOOT FAIL ($BACKEND)"
  log "tail of dolphin log:"
  tail -20 "$ART/$TAG.log" 2>/dev/null || true
  exit 1
fi
