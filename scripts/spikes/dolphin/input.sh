#!/usr/bin/env bash
# input.sh — spike step 6c: prove that a controller event written to Dolphin's
# controller "Pipes" FIFO is ACCEPTED IN-GAME: it moves the user-supplied
# GameCube game off its title screen, while an identical no-input control window
# on the same title screen does not.
#
# Gate: docs/spec/12-test-strategy.md "Emulator/hardware" — "live input pipe /
# controller event accepted". Decision record: docs/adr/0007. Write-up:
# docs/spikes/dolphin-capability.md.
#
# ## What the first version got wrong (and why this one is shaped like this)
#
# 1. It compared raw pixel-delta ratios (2.27% no-input vs 2.65% START) — the
#    title screen is a slow camera orbit of the Great Sea with the logo overlaid,
#    so frame-to-frame pixel deltas are dominated by camera motion and the
#    blinking "PRESS START", and never see a scene change.
# 2. It waited for the screen to go "stable". Meaningless here: the game reaches
#    its title screen at t≈15-65 s, and ~50-90 s later the opening "legend" movie
#    starts BY ITSELF (attract mode), plays ~3.5 min and returns to the title.
#    Static frames happen inside that movie, not at the title.
# 3. Root cause of "input does nothing": Xvfb has no window manager, so the render
#    window never gets X input focus, and Dolphin's InputManager does not poll
#    ANY device (the Pipes FIFO included) while unfocused unless
#    [Input] BackgroundInput = True. This script sets it in
#    <user>/Config/Dolphin.ini before launch AND passes
#    --config=Dolphin.Input.BackgroundInput=True, and records the ini line.
#
# ## Scene classifier (calibrated at run start on retained evidence)
#   dist      = normalised RMSE between the normalised render-window crop and the
#               closest of ~10 known interactive-title framings (the title camera
#               orbits, so several framings are needed)
#   lb        = mean brightness of the top and bottom 30-row bands, the lower of
#               the two. The title screen fills the frame (lb ≈ 0.32-0.37: sky and
#               sea); the opening legend movie is letterboxed (lb ≈ 0.000). This
#               is camera-motion immune and semantically unambiguous.
#   on title  = dist < ON_TITLE_TH and lb > LB_TITLE_MIN
#   movie     = lb < LB_MOVIE_MAX and dist > SCENE_TH and entropy > ENT_MIN
#   SCENE_TH  = midpoint(max(ON_TITLE_TH, control_max), movie_min) — recomputed
#               from the measured control so the camera orbit can never fake it
#
# ## Test shape
#   phase 1  seek an interactive-title framing (no input sent while seeking)
#   phase 2  CONTROL  — 15 samples 1 s apart, no input: no sample may be
#            letterboxed/off-title. If the attract timer fires inside the window
#            the episode is discarded and the next title visit is used.
#   phase 3  INPUT    — PRESS START / hold 0.5 s / RELEASE START, then 15 samples
#            1 s apart: PASS needs 2 consecutive samples that crossed the scene
#            threshold (i.e. the scene changed and stayed changed). Fallback
#            ladder, stopped at the first pass: focus the render window with
#            xdotool, 2 s hold, A instead of START.
#   phase 4  bonus    — press START during the legend movie. The attract loop
#            never skips it and never shows the file-select screen, so a movie
#            that ends ~3 min early is independent proof (the no-input baseline
#            in boot-OGL-frames.tsv shows the scroll running for 120+ s).
#
# Input path only — newline commands to <user>/Pipes/pipe1 == device
# Pipe/0/pipe1 (upstream InputCommon/ControllerInterface/Pipes/Pipes.cpp):
#   PRESS|RELEASE {A,B,X,Y,Z,START,L,R,D_UP,D_DOWN,D_LEFT,D_RIGHT}
#   SET {L,R} [0,1] | SET {MAIN,C} [0,1] [0,1]
# No XTEST key injection is used for game input (docs/spec/11).
#
# usage: input.sh [budget-seconds]        (default 900 s total)
# env:   GAME_NARRATOR_GAME_IMAGE_PATH (required, referenced read-only, never
#        copied), GAME_NARRATOR_SPIKE_BACKEND=OGL|Vulkan,
#        GAME_NARRATOR_SPIKE_REF_DIR (where the reference screenshots live)
# exit:  0 = the press changed the scene and the control did not; 1 = not proven
set -euo pipefail
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
# shellcheck source=lib.sh
source scripts/spikes/dolphin/lib.sh

BUDGET="${1:-900}"
BACKEND="${GAME_NARRATOR_SPIKE_BACKEND:-OGL}"
REF_DIR="${GAME_NARRATOR_SPIKE_REF_DIR:-$ART}"
RUN="$ART/run-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$RUN"
TAG="input-$BACKEND"
REPORT="$RUN/$TAG-report.txt"
TIMELINE="$RUN/$TAG-timeline.tsv"
CAL="$RUN/$TAG-calibration.tsv"
grep -n BackgroundInput "$USER_DIR/Config/Dolphin.ini" >"$RUN/$TAG-backgroundinput-before.txt" 2>/dev/null ||
  : >"$RUN/$TAG-backgroundinput-before.txt"

SAMPLES=15           # control and observation windows: 15 samples 1 s apart
SAMPLE_SEC=1
EPISODE_MAX=3        # title visits to try (the attract loop returns to the title)
HOLD_DEFAULT=0.5     # START hold: several emulated frames at llvmpipe speed
NORM="320x240!"      # normalise every crop before comparing
LB_BAND=30           # rows sampled at the top and bottom for the letterbox test
LB_TITLE_MIN=0.25    # title screen shows sky/sea there (measured 0.32-0.37)
LB_MOVIE_MAX=0.10    # letterboxed opening movie is black there (measured 0.000)
ENT_MIN=0.20         # a rendered scene, not a black/crashed window

# Interactive-title framings (retained evidence, gitignored — every one of these
# was inspected and shows the ZELDA logo, "PRESS START" and "© 2002, 2003
# Nintendo" while the camera orbits Outset Island).
TITLE_REFS=(boot-OGL-04.png boot-OGL-05.png boot-OGL-08.png boot-OGL-12.png
  boot-OGL-16.png boot-OGL-19.png input-OGL-settle-359.png input-OGL-settle-367.png
  input-OGL-control-1-05.png input-OGL-control-1-10.png input-OGL-control-1-15.png
  input-OGL-title-1.png)
# Held-out frames used only to calibrate, never part of the ref set:
CAL_TITLE=(input-OGL-before.png input-OGL-settle-389.png)      # title framings
CAL_MOVIE=(input-OGL-settle-118.png boot-OGL-26.png)           # legend movie

TMP_ROOT="$RUN/.shot-root.png"
TMP_CUR="$RUN/.shot-crop.png"
TMP_PREV="$RUN/.shot-prev.png"

rel() { printf '%s' "${1#"$ROOT_DIR"/}"; }

## ------------------------------------------------------------- config helpers --
# Dolphin's InputManager::Update() does not poll devices while the render window
# is unfocused unless BackgroundInput is set — fatal on a WM-less Xvfb.
set_background_input() {
  mkdir -p "$USER_DIR/Config"
  python3 - "$USER_DIR/Config/Dolphin.ini" <<'PY'
import os, re, sys
path = sys.argv[1]
txt = open(path).read() if os.path.exists(path) else ''
if re.search(r'(?mi)^\s*BackgroundInput\s*=', txt):
    txt = re.sub(r'(?mi)^\s*BackgroundInput\s*=.*$', 'BackgroundInput = True', txt)
elif re.search(r'(?mi)^\[Input\]', txt):
    txt = re.sub(r'(?mi)^(\[Input\][ \t]*)$', r'\1\nBackgroundInput = True', txt, count=1)
else:
    txt = txt.rstrip('\n') + '\n[Input]\nBackgroundInput = True\n'
open(path, 'w').write(txt)
PY
  log "wrote $(grep -n BackgroundInput "$USER_DIR/Config/Dolphin.ini" | tr -d '\n')"
}

install_pipe_pad() { # install_pipe_pad <on|off> — patch [GCPad1], keep ini format
  local on=$1 f="$USER_DIR/Config/GCPadNew.ini"
  [[ -f "$f" ]] || die "no generated $f — run probe.sh (GUI launch) once first"
  [[ -f "$ART/GCPadNew.ini.default" ]] || cp "$f" "$ART/GCPadNew.ini.default"
  python3 - "$f" "$on" "$PIPE_NAME" <<'PY'
import re, sys
path, on, pipe = sys.argv[1], sys.argv[2], sys.argv[3]
blocks = re.split(r'(?m)^(?=\[)', open(path).read())
out, patched = [], False
for b in blocks:
    if b.startswith('[GCPad1]'):
        lines = [l for l in b.rstrip('\n').split('\n')
                 if not re.match(r'(?i)\s*(Device|Buttons/(A|B|Start))\s*=', l)]
        if on == 'on':
            lines += [f'Device = Pipe/0/{pipe}', 'Buttons/A = `Button A`',
                      'Buttons/B = `Button B`', 'Buttons/Start = `Button START`']
        b = '\n'.join(lines) + '\n'
        patched = True
    out.append(b)
if on == 'on' and not patched:
    out.append(f'[GCPad1]\nDevice = Pipe/0/{pipe}\nButtons/A = `Button A`\n'
               'Buttons/B = `Button B`\nButtons/Start = `Button START`\n')
open(path, 'w').write(''.join(out))
PY
  log "GCPadNew.ini [GCPad1] -> Pipe/0/$PIPE_NAME (on=$on)"
}

## ------------------------------------------------------------------ pipe write --
pipe_write() { # pipe_write <PRESS|RELEASE> <TOKEN>
  timeout 10 bash -c 'printf "%s %s\n" "$2" "$3" > "$1"' _ "$PIPE_PATH" "$1" "$2" 2>/dev/null
}
send_hold() { # send_hold <TOKEN> <hold-seconds>
  local tok=$1 hold=$2
  pipe_write PRESS "$tok" || die "could not write to $(rel "$PIPE_PATH") (no reader?)"
  sleep "$hold"
  pipe_write RELEASE "$tok" || die "could not release $tok on $(rel "$PIPE_PATH")"
  log "  pipe: PRESS $tok / hold ${hold}s / RELEASE $tok"
}

## --------------------------------------------------------------- measurement ----
rmse() { # rmse <a.png> <b.png>  (magick compare exits non-zero when they differ)
  local out
  out=$( { magick compare -metric RMSE "$1" "$2" null: 2>&1 || true; } |
         tr -d '()' | awk '{printf "%.4f", $2+0}')
  [[ -n "$out" ]] || out=nan
  printf '%s' "$out"
}
normalise() { # normalise <src.png> <dst.png>
  magick "$1" -crop "$CROP" +repage -resize "$NORM" "$2" 2>/dev/null ||
    die "could not crop $1 to $CROP"
}
grab() { # grab <dst.png> — screenshot now, crop to the render window, normalise
  shot "$TMP_ROOT"
  normalise "$TMP_ROOT" "$1"
}
title_dist() { # title_dist <normalised.png> -> RMSE to the closest title framing
  local best="" d
  for r in "${REF_NORM[@]}"; do
    d=$(rmse "$r" "$1")
    [[ -z "$best" ]] && best=$d
    [[ "$(awk -v a="$best" -v b="$d" 'BEGIN{print (b+0<a+0)?1:0}')" == 1 ]] && best=$d
  done
  printf '%s' "$best"
}
# Lower of the mean brightness of the top and bottom bands: the title screen
# shows sky/sea (≈0.35), the letterboxed opening movie shows black (≈0.000).
letter_score() {
  local t b
  t=$(magick "$1" -crop "320x${LB_BAND}+0+0" +repage -colorspace Gray -format '%[fx:mean]' info: 2>/dev/null)
  b=$(magick "$1" -crop "320x${LB_BAND}+0+210" +repage -colorspace Gray -format '%[fx:mean]' info: 2>/dev/null)
  awk -v t="$t" -v b="$b" 'BEGIN{printf "%.4f", (t+0<b+0 ? t+0 : b+0)}'
}
gt() { awk -v a="$1" -v b="$2" 'BEGIN{exit !(a+0>b+0)}'; }
lt() { awk -v a="$1" -v b="$2" 'BEGIN{exit !(a+0<b+0)}'; }
# Keep a true 1 s sampling cadence: the title visit lasts only tens of seconds.
cadence() { # cadence <start(epoch.ns)> <period>
  local used
  used=$(awk -v a="$(date +%s.%N)" -v b="$1" 'BEGIN{printf "%.3f", a-b}')
  if gt "$2" "$used"; then
    sleep "$(awk -v u="$used" -v p="$2" 'BEGIN{d=p-u; if(d<0.02)d=0.02; printf "%.3f", d}')"
  fi
}

# measure <normalised.png> -> sets DIST, LB, ENT
measure() { DIST=$(title_dist "$1"); LB=$(letter_score "$1"); ENT=$(entropy_of "$1"); }

printf 'phase\tt_sec\ttitle_dist\tletterband\tframe_step\tentropy\tcrossed\tframe\n' >"$TIMELINE"
row() { printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$1" "$2" "$3" "$4" "$5" "$6" "$7" "$(rel "$8")" >>"$TIMELINE"; }

## ------------------------------------------------------------------- boot up ----
trap 'log "cleanup"; cleanup_all' EXIT
require_game_image
enable_emulator_logging
set_background_input
make_pipe
open_pipe_writer
install_pipe_pad on
start_xvfb 1280x1024x24

T0=$(date +%s)
dolphin_start "$RUN/$TAG.log" --batch --exec="$GAME_NARRATOR_GAME_IMAGE_PATH" \
  --video_backend="$BACKEND" \
  --config=Dolphin.Input.BackgroundInput=True \
  --config=GPU.Settings.InternalResolution=1 \
  --config=GPU.Hardware.VSync=False

WIN=""
for _ in $(seq 1 30); do
  WIN=$(dolphin_wait_window 1 || true)
  [[ -n "$WIN" ]] && break
done
[[ -n "$WIN" ]] || die "no Dolphin render window appeared"
CROP=""
for _ in $(seq 1 10); do
  CROP=$(window_crop "$WIN" || true)
  [[ -n "$CROP" ]] && break
  sleep 1
done
[[ -n "$CROP" ]] || die "could not resolve the geometry of render window $WIN"
log "render window $WIN geometry=$CROP name='$(xdotool getwindowname "$WIN" 2>/dev/null || echo '?')'"
shot "$RUN/$TAG-boot.png"
log "t=$(( $(date +%s) - T0 ))s boot frame $(rel "$RUN/$TAG-boot.png") entropy=$(entropy_of "$RUN/$TAG-boot.png" "$CROP")"
wait_pipe_writer 20 || log "WARN: pipe writer not connected to $PIPE_PATH yet"
log "pipe fd proof: $(ls -l /proc/$DOLPHIN_PID/fd 2>/dev/null | grep -c "$PIPE_NAME") dolphin fd on $(rel "$PIPE_PATH")"

## ------------------------------------------------- reference set + calibration --
REF_NORM=()
for r in "${TITLE_REFS[@]}"; do
  [[ -s "$REF_DIR/$r" ]] || die "title reference missing: $(rel "$REF_DIR/$r") (gitignored evidence; keep it, or point GAME_NARRATOR_SPIKE_REF_DIR at it)"
  normalise "$REF_DIR/$r" "$RUN/$TAG-ref-$r"
  REF_NORM+=("$RUN/$TAG-ref-$r")
  lt "0.10" "$(entropy_of "$RUN/$TAG-ref-$r")" ||
    die "title reference $(rel "$r") is not a rendered scene — crop $CROP does not match the retained captures"
done

printf 'kind\tpair\tdistance_to_title_refs\tletterband\n' >"$CAL"
TITLE_MAX_CAL=0
for r in "${CAL_TITLE[@]}"; do
  [[ -s "$REF_DIR/$r" ]] || die "calibration reference missing: $(rel "$REF_DIR/$r")"
  normalise "$REF_DIR/$r" "$TMP_CUR"
  d=$(title_dist "$TMP_CUR"); measure "$TMP_CUR"
  printf 'title-vs-title\t%s\t%s\t%s\n' "$r" "$d" "$LB" >>"$CAL"
  gt "$d" "$TITLE_MAX_CAL" && TITLE_MAX_CAL=$d
  lt "$LB" "$LB_TITLE_MIN" && die "calibration title frame $r is letterboxed (lb=$LB) — reference set is wrong"
done
MOVIE_MIN_CAL=1
for r in "${CAL_MOVIE[@]}"; do
  [[ -s "$REF_DIR/$r" ]] || die "movie reference missing: $(rel "$REF_DIR/$r")"
  normalise "$REF_DIR/$r" "$TMP_CUR"
  d=$(title_dist "$TMP_CUR"); measure "$TMP_CUR"
  printf 'title-vs-movie\t%s\t%s\t%s\n' "$r" "$d" "$LB" >>"$CAL"
  gt "$MOVIE_MIN_CAL" "$d" && MOVIE_MIN_CAL=$d
  gt "$LB" "$LB_MOVIE_MAX" && die "calibration movie frame $r is not letterboxed (lb=$LB)"
done
ON_TITLE_TH=$(awk -v a="$TITLE_MAX_CAL" 'BEGIN{printf "%.3f", a+0.03}')
SCENE_TH_BASE=$(awk -v a="$ON_TITLE_TH" -v b="$MOVIE_MIN_CAL" 'BEGIN{printf "%.3f", (a+b)/2}')
SCENE_TH=$SCENE_TH_BASE
log "calibration ($(rel "$CAL")): title-vs-title max=$TITLE_MAX_CAL, title-vs-movie min=$MOVIE_MIN_CAL"
log "thresholds: on title if dist<$ON_TITLE_TH and letterband>$LB_TITLE_MIN ; movie if letterband<$LB_MOVIE_MAX"
log "scene-change threshold starts at $SCENE_TH_BASE and is recomputed from the control"
gt "$MOVIE_MIN_CAL" "$(awk -v a="$TITLE_MAX_CAL" 'BEGIN{printf "%.3f", a+0.10}')" ||
  die "title/movie separation too small to trust ($TITLE_MAX_CAL vs $MOVIE_MIN_CAL)"

## --------------------------------------------------- phase 1: find the title ---
seek_title() { # seek_title -> sets T_TITLE or returns 1. NO input is sent here.
  local d t tick
  T_TITLE=""
  while (( $(date +%s) - T0 < BUDGET )); do
    dolphin_alive || die "dolphin exited at t=$(( $(date +%s) - T0 ))s (see $(rel "$RUN/$TAG.log"))"
    tick=$(date +%s.%N); t=$(( $(date +%s) - T0 ))
    grab "$TMP_CUR"; measure "$TMP_CUR"; d=$DIST
    if lt "$d" "$ON_TITLE_TH" && gt "$LB" "$LB_TITLE_MIN"; then
      sleep 0.4                    # one quick re-check, then start measuring now
      grab "$TMP_CUR"; measure "$TMP_CUR"
      if lt "$DIST" "$ON_TITLE_TH" && gt "$LB" "$LB_TITLE_MIN"; then
        T_TITLE=$t; cp "$TMP_CUR" "$RUN/$TAG-title.png"; return 0
      fi
      log "  t=${t}s dist=$d letter=$LB — title candidate rejected on re-check ($DIST)"
    else
      cp "$TMP_CUR" "$RUN/$TAG-seek-$t.png"
      row seek "$t" "$d" "$LB" "" "$ENT" "" "$RUN/$TAG-seek-$t.png"
      log "  t=${t}s dist=$d letter=$LB entropy=$ENT — not an interactive title framing"
    fi
    cadence "$tick" "$SAMPLE_SEC"
  done
  return 1
}

# OUT: W_MAX W_STEP W_LBMIN W_CROSS W_CROSS_T — one window of n samples, 1 s apart
W_MAX=0 W_STEP=0 W_LBMIN=1 W_CROSS="" W_CROSS_T="" PREV_CROSS=0
sample_window() { # sample_window <label> <n> [press-token] [hold]
  local label=$1 n=$2 tok=${3:-NONE} hold=${4:-$HOLD_DEFAULT} i t d step f cross far
  W_MAX=0 W_STEP=0 W_LBMIN=1 W_CROSS="" W_CROSS_T="" PREV_CROSS=0
  [[ "$tok" == "NONE" ]] || send_hold "$tok" "$hold"
  : >"$TMP_PREV"
  local tick
  for ((i = 1; i <= n; i++)); do
    tick=$(date +%s.%N); t=$(( $(date +%s) - T0 ))
    grab "$TMP_CUR"
    measure "$TMP_CUR"; d=$DIST
    if [[ -s "$TMP_PREV" ]]; then step=$(rmse "$TMP_PREV" "$TMP_CUR"); else step=0; fi
    f="$RUN/$TAG-$label-$(printf '%02d' "$i").png"; cp "$TMP_CUR" "$f"
    # A scene change must be (a) far from every title framing in two consecutive
    # samples — so a transition/flicker frame cannot fake it — and (b) either the
    # letterboxed opening movie (black bands, rendered pixels) or a *different
    # settled* full-frame screen (stable to <0.10 between samples, unlike the
    # title camera orbit, which moves 0.10-0.40 per sample).
    cross=0
    far=$(awk -v d="$d" -v s="$SCENE_TH" 'BEGIN{print (d+0>s+0)?1:0}')
    if (( PREV_CROSS )) && (( far )); then
      if lt "$LB" "$LB_MOVIE_MAX" && gt "$ENT" "$ENT_MIN"; then cross=1; fi
      if gt "$LB" "$LB_TITLE_MIN" && lt "$step" "0.10"; then cross=1; fi
    fi
    PREV_CROSS=$far
    row "$label" "$t" "$d" "$LB" "$step" "$ENT" "$cross" "$f"
    gt "$d" "$W_MAX" && W_MAX=$d
    gt "$step" "$W_STEP" && W_STEP=$step
    gt "$W_LBMIN" "$LB" && W_LBMIN=$LB
    [[ -z "$W_CROSS" ]] || true
    if [[ -z "$W_CROSS" && "$cross" == 1 ]]; then
      W_CROSS=$i; W_CROSS_T=$t
      cp "$TMP_CUR" "$RUN/$TAG-after.png"
      log "  t=${t}s $label #$i dist=$d letter=$LB step=$step -> SCENE CHANGED (threshold $SCENE_TH)"
    fi
    log "  t=${t}s $label #$i dist=$d letter=$LB step=$step entropy=$ENT cross=$cross"
    cp "$TMP_CUR" "$TMP_PREV"
    cadence "$tick" "$SAMPLE_SEC"
  done
}

log "== phases 1+2: seek the title screen, then a no-input CONTROL run inside that visit =="
EPISODE=0 CTRL_OK=0 CONTROL_MAX="" CONTROL_LBMIN=""
while (( EPISODE < EPISODE_MAX )) && (( CTRL_OK == 0 )); do
  EPISODE=$((EPISODE + 1))
  seek_title || die "no interactive title framing found within ${BUDGET}s (episode $EPISODE) — see $(rel "$TIMELINE")"
  cp "$RUN/$TAG-title.png" "$RUN/$TAG-title-$EPISODE.png"
  log "episode $EPISODE: title framing at t=${T_TITLE}s -> $(rel "$RUN/$TAG-title-$EPISODE.png")"
  log "== phase 2: CONTROL — $SAMPLES samples ${SAMPLE_SEC}s apart, no input =="
  sample_window "control-$EPISODE" "$SAMPLES"
  cp "$RUN/$TAG-control-$EPISODE-01.png" "$RUN/$TAG-before.png"
  cp "$RUN/$TAG-control-$EPISODE-$(printf '%02d' "$SAMPLES").png" "$RUN/$TAG-control.png"
  CONTROL_MAX=$W_MAX CONTROL_LBMIN=$W_LBMIN
  log "CONTROL (episode $EPISODE): max dist=$CONTROL_MAX (min letterband $CONTROL_LBMIN), max step=$W_STEP"
  # Recompute the scene-change threshold from what the orbit actually did.
  SCENE_TH=$(awk -v a="$ON_TITLE_TH" -v c="$CONTROL_MAX" -v m="$MOVIE_MIN_CAL" \
    'BEGIN{x=(c>a)?c:a; printf "%.3f", x+(m-x)/2}')
  log "scene-change threshold recomputed for this episode: $SCENE_TH"
  if gt "$CONTROL_MAX" "$SCENE_TH" || lt "$CONTROL_LBMIN" "$LB_MOVIE_MAX"; then
    log "episode $EPISODE unusable: the attract loop crossed on its own (max dist $CONTROL_MAX, min letterband $CONTROL_LBMIN) before any input was sent — waiting for the next title visit"
    continue
  fi
  CTRL_OK=1
done
[[ "$CTRL_OK" == 1 ]] || die "no no-input control window stayed on the title screen (max dist $CONTROL_MAX vs threshold $SCENE_TH, min letterband $CONTROL_LBMIN) — a scene change cannot be attributed to input"

## ----------------------------------------------------- phase 3: the input run ---
ATTEMPTS=("START|$HOLD_DEFAULT|nofocus" "START|$HOLD_DEFAULT|focus" "START|2|nofocus" "A|$HOLD_DEFAULT|nofocus")
PASSED="" USED_TOKEN="" USED_HOLD="" USED_FOCUS="" CROSS_T="" CROSS_SAMPLE=""
for a in "${ATTEMPTS[@]}"; do
  [[ -z "$PASSED" ]] || break
  IFS='|' read -r tok hold focus <<<"$a"
  grab "$RUN/$TAG-check.png"; measure "$RUN/$TAG-check.png"
  # Still the same scene as the control window? (the title camera orbit keeps
  # drifting, so the test is "has not crossed the scene-change threshold and is
  # not letterboxed", not the tight framing threshold used to find the visit.)
  if ! lt "$DIST" "$SCENE_TH" || ! gt "$LB" "$LB_TITLE_MIN"; then
    log "no longer on the title screen (dist=$DIST >= $SCENE_TH or letter=$LB) — no further input attempts"
    break
  fi
  if [[ "$focus" == focus ]]; then
    on_display xdotool windowactivate --sync "$WIN" 2>/dev/null || true
    on_display xdotool windowfocus --sync "$WIN" 2>/dev/null || true
    log "fallback: focused/raised render window $WIN with xdotool before pressing"
  fi
  log "== phase 3: INPUT — PRESS $tok / hold ${hold}s / RELEASE $tok over Pipe/0/$PIPE_NAME =="
  sample_window "after-${tok}-${focus}" "$SAMPLES" "$tok" "$hold"
  log "INPUT ($tok/$focus): max dist=$W_MAX, min letterband=$W_LBMIN, first crossing sample='${W_CROSS:-none}'"
  if [[ -n "$W_CROSS" ]]; then
    PASSED="$tok hold=${hold}s focus=$focus"; USED_TOKEN=$tok USED_HOLD=$hold USED_FOCUS=$focus
    CROSS_SAMPLE=$W_CROSS CROSS_T=$W_CROSS_T
  fi
done
[[ -n "$PASSED" ]] || die "no input attempt changed the scene — input NOT proven accepted (see $(rel "$TIMELINE"))"

## ------------------------- phase 4 (bonus): skip the opening movie on purpose --
# The attract loop never skips the legend movie, and the file-select screen it
# protects is never shown without input, so a movie that ends on a press is
# independent proof and is recorded as such.
log "== phase 4: pressing START during the opening movie (movie skip -> file select) =="
MOVIE_SKIPS=0 STABLE=0
for k in $(seq 1 6); do
  t=$(( $(date +%s) - T0 ))
  pipe_write PRESS START || true; sleep 0.4; pipe_write RELEASE START || true
  MOVIE_SKIPS=$((MOVIE_SKIPS + 1))
  sleep 3
  grab "$RUN/$TAG-skip-$k.png"; measure "$RUN/$TAG-skip-$k.png"
  row skip "$t" "$DIST" "$LB" "" "$ENT" "" "$RUN/$TAG-skip-$k.png"
  log "  t=${t}s PRESS START #$MOVIE_SKIPS -> $(rel "$RUN/$TAG-skip-$k.png") dist=$DIST letter=$LB entropy=$ENT"
  if gt "$LB" "$LB_TITLE_MIN" && lt "$DIST" "$ON_TITLE_TH"; then
    log "  back on a title framing — the movie was skipped or ended"
    cp "$RUN/$TAG-skip-$k.png" "$RUN/$TAG-after-skip.png"; STABLE=1; break
  fi
  if gt "$LB" "$LB_TITLE_MIN" && gt "$DIST" "$SCENE_TH"; then
    cp "$RUN/$TAG-skip-$k.png" "$RUN/$TAG-fileselect-candidate.png"
    log "  letterbox gone on a non-title scene -> $(rel "$RUN/$TAG-fileselect-candidate.png")"
    STABLE=1; break
  fi
done
cp "$RUN/.shot-crop.png" "$RUN/$TAG-end.png"
log "phase 4 done: $MOVIE_SKIPS presses during the off-title scene, last frame $(rel "$RUN/$TAG-end.png")"

## ------------------------------------------------------------------- evidence ----
grep -n BackgroundInput "$USER_DIR/Config/Dolphin.ini" >"$RUN/$TAG-backgroundinput.txt" 2>/dev/null ||
  : >"$RUN/$TAG-backgroundinput.txt"
{
  echo "result=PASS"
  echo "attempt=$PASSED"
  echo "backend=$BACKEND  render_window=$WIN crop=$CROP"
  echo "pipe_device=Pipe/0/$PIPE_NAME"
  echo "input_command=PRESS $USED_TOKEN / hold ${USED_HOLD}s / RELEASE $USED_TOKEN (window focus: $USED_FOCUS)"
  echo "background_input_ini=$(tr '\n' ' ' <"$RUN/$TAG-backgroundinput.txt")"
  echo "config_flags=--config=Dolphin.Input.BackgroundInput=True"
  echo "calibration_title_vs_title_max=$TITLE_MAX_CAL"
  echo "calibration_title_vs_movie_min=$MOVIE_MIN_CAL"
  echo "threshold_on_title=$ON_TITLE_TH  threshold_scene_change=$SCENE_TH"
  echo "threshold_letterband_title_min=$LB_TITLE_MIN  threshold_letterband_movie_max=$LB_MOVIE_MAX"
  echo "episode=$EPISODE title_recognised_at_sec=$T_TITLE"
  echo "control_max_title_distance=$CONTROL_MAX control_min_letterband=$CONTROL_LBMIN (no control frame crossed)"
  echo "input_first_crossing_sample=$CROSS_SAMPLE at_sec=${CROSS_T:-n/a} max_title_distance=$W_MAX min_letterband=$W_LBMIN"
  echo "movie_skip_presses=$MOVIE_SKIPS"
  echo "before=$(rel "$RUN/$TAG-before.png")"
  echo "control=$(rel "$RUN/$TAG-control.png")"
  echo "after=$(rel "$RUN/$TAG-after.png")"
  echo "title=$(rel "$RUN/$TAG-title-$EPISODE.png")"
  echo "end=$(rel "$RUN/$TAG-end.png")"
  echo "timeline=$(rel "$TIMELINE")"
  echo "calibration_file=$(rel "$CAL")"
} | tee "$REPORT"

if ! ( gt "$W_MAX" "$SCENE_TH" && lt "$CONTROL_MAX" "$SCENE_TH" && gt "$CONTROL_LBMIN" "$LB_MOVIE_MAX" ); then
  die "internal assertion failed: after=$W_MAX control=$CONTROL_MAX threshold=$SCENE_TH control_letterband=$CONTROL_LBMIN"
fi
log "INPUT PASS: $USED_TOKEN over Pipe/0/$PIPE_NAME moved the game off its title screen (dist $W_MAX > $SCENE_TH, letterband $W_LBMIN) while $SAMPLES no-input samples 1 s apart did not (max dist $CONTROL_MAX, letterband $CONTROL_LBMIN)"
log "evidence bundle: $(rel "$RUN")"
