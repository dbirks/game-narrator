#!/usr/bin/env bash
# probe.sh — spike steps 2 + 3 + 5 of the Dolphin headless capability probe.
#
#   2. what GL / Vulkan does an Xvfb display actually expose on this host
#   3. does the Dolphin GUI render into that display (entropy check, not just
#      "the process existed")
#   5. can FFmpeg x11grab capture those frames
#
# Exits non-zero unless the rendered content is measurably non-blank
# (ImageMagick entropy > 0.1). All evidence lands in artifacts/spikes/dolphin/.
#
# Never touches the real user Dolphin directories: --user is always the
# isolated spike directory.

set -euo pipefail
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
# shellcheck source=lib.sh
source scripts/spikes/dolphin/lib.sh

ENTROPY_MIN=0.1
trap 'log "cleanup"; cleanup_all' EXIT

log "== 1. versions =="
{
  pacman -Q dolphin-emu mesa vulkan-intel xorg-server-xvfb 2>&1 || true
  echo "kernel: $(uname -r)"
  echo "ffmpeg: $(ffmpeg -version 2>&1 | head -1)"
  echo "imagemagick: $(magick --version | head -1)"
  echo "gpu: $(lspci 2>/dev/null | grep -i -E 'vga|display|3d' || echo unknown)"
} | tee "$ART/versions.txt"

log "== 2. virtual display GL / Vulkan capabilities =="
start_xvfb 1280x1024x24
on_display glxinfo -B >"$ART/glxinfo.txt" 2>&1 || log "glxinfo failed (see glxinfo.txt)"
on_display vulkaninfo --summary >"$ART/vulkaninfo-summary.txt" 2>&1 ||
  log "vulkaninfo failed (see vulkaninfo-summary.txt)"
GL_RENDERER=$(grep -m1 'OpenGL renderer string' "$ART/glxinfo.txt" | cut -d: -f2- | sed 's/^ //' || echo "none")
GL_ACCEL=$(grep -m1 'Accelerated:' "$ART/glxinfo.txt" | awk '{print $2}' || echo "unknown")
log "GL renderer: ${GL_RENDERER:-none} (Accelerated: ${GL_ACCEL:-unknown})"
log "GL/Vulkan evidence: $ART/glxinfo.txt, $ART/vulkaninfo-summary.txt"

log "== 3. Dolphin GUI under the virtual display =="
dolphin_start "$ART/dolphin-gui.log"
if ! WIN=$(dolphin_wait_window 20); then
  dolphin_stop
  die "no Dolphin window after 20s on $DISPLAY_NUM"
fi
log "Dolphin window id $WIN: $(on_display xdotool getwindowname "$WIN" 2>/dev/null)"
on_display xwininfo -root -tree >"$ART/xwininfo-tree.txt" 2>&1 || true
sleep 3
shot "$ART/gui-root.png"
GUI_ENTROPY=$(entropy_of "$ART/gui-root.png")
log "gui-root.png entropy=$GUI_ENTROPY (threshold $ENTROPY_MIN)"

log "== 4. FFmpeg x11grab frame capture =="
rm -f "$ART"/x11grab-*.png
ffmpeg -hide_banner -loglevel error -y -f x11grab -video_size 1280x1024 -framerate 10 \
  -i "$DISPLAY_NUM" -frames:v 20 "$ART/x11grab-%02d.png" 2>"$ART/x11grab-stderr.log" ||
  log "x11grab returned non-zero (see x11grab-stderr.log)"
CAPTURED=$(find "$ART" -maxdepth 1 -name 'x11grab-*.png' | wc -l)
if [[ "$CAPTURED" -gt 0 ]]; then
  CAP_ENTROPY=$(entropy_of "$ART/x11grab-01.png")
  log "x11grab frames: $CAPTURED, first frame entropy=$CAP_ENTROPY"
else
  CAP_ENTROPY=0
  log "x11grab produced no frames"
fi

log "== result =="
printf 'display=%s\ngl_renderer=%s\ngl_accelerated=%s\ndolphin_window=%s\ngui_entropy=%s\nx11grab_frames=%s\nx11grab_entropy=%s\n' \
  "$DISPLAY_NUM" "$GL_RENDERER" "$GL_ACCEL" "$WIN" "$GUI_ENTROPY" "$CAPTURED" "$CAP_ENTROPY" |
  tee "$ART/probe-result.txt"

awk -v e="$GUI_ENTROPY" -v t="$ENTROPY_MIN" 'BEGIN{exit !(e+0>t+0)}' ||
  die "GUI entropy $GUI_ENTROPY <= $ENTROPY_MIN — nothing rendered"
awk -v e="$CAP_ENTROPY" -v t="$ENTROPY_MIN" 'BEGIN{exit !(e+0>t+0)}' ||
  die "x11grab entropy $CAP_ENTROPY <= $ENTROPY_MIN — capture is blank"
[[ "$CAPTURED" -ge 20 ]] || die "x11grab captured only $CAPTURED/20 frames"
log "PROBE PASS: Dolphin renders under Xvfb and x11grab captures it"
