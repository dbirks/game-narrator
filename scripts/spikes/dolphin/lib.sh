#!/usr/bin/env bash
# Shared helpers for the Dolphin headless capability spike (risk R-05).
#
# Contract for every script that sources this file:
#   - all evidence is written under $ART (artifacts/spikes/dolphin, gitignored)
#   - Dolphin ALWAYS runs with --user=$USER_DIR (isolated); the real user
#     directories under ~/.local/share/dolphin-emu are never touched.
#   - the game image path comes ONLY from $GAME_NARRATOR_GAME_IMAGE_PATH:
#     never hardcoded, never copied, only its hash is recorded.
#
# Sourced, not executed.

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
ART="${GAME_NARRATOR_SPIKE_ART:-$ROOT_DIR/artifacts/spikes/dolphin}"
USER_DIR="$ART/user"
PIPE_DIR="$USER_DIR/Pipes"
PIPE_NAME="${GAME_NARRATOR_SPIKE_PIPE:-pipe1}"
PIPE_PATH="$PIPE_DIR/$PIPE_NAME"

mkdir -p "$ART" "$USER_DIR"

log() { printf '[%s] %s\n' "$(date +%H:%M:%S)" "$*"; }
die() {
  log "FAIL: $*"
  exit 1
}

require_game_image() {
  [[ -n "${GAME_NARRATOR_GAME_IMAGE_PATH:-}" ]] ||
    die "GAME_NARRATOR_GAME_IMAGE_PATH is not set (deliberately never hardcoded)"
  [[ -r "$GAME_NARRATOR_GAME_IMAGE_PATH" ]] ||
    die "game image not readable: $GAME_NARRATOR_GAME_IMAGE_PATH"
  log "game image (referenced read-only, never copied): $GAME_NARRATOR_GAME_IMAGE_PATH"
  log "game image sha256: $(sha256sum "$GAME_NARRATOR_GAME_IMAGE_PATH" | cut -d' ' -f1)"
}

## ---------------------------------------------------------------- display --

# Start Xvfb on a free display. Sets DISPLAY_NUM / XVFB_PID.
start_xvfb() {
  local geom="${1:-1280x1024x24}" n
  DISPLAY_NUM=""
  for n in $(seq 91 110); do
    [[ -e "/tmp/.X11-unix/X$n" ]] && continue
    DISPLAY_NUM=":$n"
    break
  done
  [[ -n "$DISPLAY_NUM" ]] || die "no free X display in :91-:110"
  Xvfb "$DISPLAY_NUM" -screen 0 "$geom" -nolisten tcp >"$ART/xvfb${DISPLAY_NUM}.log" 2>&1 &
  XVFB_PID=$!
  for _ in $(seq 1 50); do
    [[ -S "/tmp/.X11-unix/X${DISPLAY_NUM#:}" ]] && break
    sleep 0.2
  done
  [[ -S "/tmp/.X11-unix/X${DISPLAY_NUM#:}" ]] || die "Xvfb $DISPLAY_NUM never came up"
  # These scripts own a private display, so it is safe (and required by tools
  # like xdotool/xwininfo) to export it for the whole script.
  export DISPLAY="$DISPLAY_NUM"
  log "Xvfb $DISPLAY_NUM up (pid $XVFB_PID, geometry $geom, DISPLAY=$DISPLAY)"
}

on_display() { DISPLAY="$DISPLAY_NUM" "$@"; }

## ---------------------------------------------------------------- dolphin --

# Launch Dolphin with the isolated user dir in the background.
# usage: dolphin_start <logfile> [dolphin args...]
dolphin_start() {
  local logfile=$1
  shift
  DISPLAY="$DISPLAY_NUM" dolphin-emu --user="$USER_DIR" "$@" >"$logfile" 2>&1 &
  DOLPHIN_PID=$!
  log "dolphin pid $DOLPHIN_PID args: --user=<isolated> $* -> $logfile"
}

# Wait for a Dolphin window; prints its window id (returns 1 on timeout).
dolphin_wait_window() {
  local timeout="${1:-20}" wid i
  for ((i = 0; i < timeout; i++)); do
    wid=$(xdotool search --onlyvisible --class 'dolphin-emu' 2>/dev/null | tail -1 || true)
    [[ -z "$wid" ]] && wid=$(xdotool search --class 'dolphin-emu' 2>/dev/null | tail -1 || true)
    if [[ -z "$wid" ]]; then
      # fallback: parse the window tree directly (works without EWMH hints)
      wid=$(xwininfo -root -tree 2>/dev/null |
        grep -iE '"[^"]*olphin[^"]*": *\(*"dolphin-emu"' |
        grep -oE '^ *0x[0-9a-f]+' | head -1 | tr -d ' ' || true)
    fi
    if [[ -n "$wid" ]]; then
      echo "$wid"
      return 0
    fi
    [[ -n "${DOLPHIN_PID:-}" ]] && ! kill -0 "$DOLPHIN_PID" 2>/dev/null &&
      die "dolphin exited before opening a window (see $ART/*.log)"
    sleep 1
  done
  return 1
}

dolphin_alive() { [[ -n "${DOLPHIN_PID:-}" ]] && kill -0 "$DOLPHIN_PID" 2>/dev/null; }

# Dolphin only writes Config/*.ini on a clean exit, so terminate politely first.
dolphin_stop() {
  local pid="${1:-${DOLPHIN_PID:-}}"
  [[ -n "$pid" ]] || return 0
  kill -0 "$pid" 2>/dev/null || return 0
  kill -TERM "$pid" 2>/dev/null || true
  for _ in $(seq 1 30); do
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.5
  done
  kill -9 "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
  DOLPHIN_PID=""
  log "dolphin pid $pid stopped"
}

cleanup_all() {
  dolphin_stop "${DOLPHIN_PID:-}"
  if [[ -n "${PIPE_WRITER_PID:-}" ]]; then
    kill "$PIPE_WRITER_PID" 2>/dev/null || true
    wait "$PIPE_WRITER_PID" 2>/dev/null || true
    PIPE_WRITER_PID=""
  fi
  if [[ -n "${XVFB_PID:-}" ]]; then
    kill "$XVFB_PID" 2>/dev/null || true
    wait "$XVFB_PID" 2>/dev/null || true
    XVFB_PID=""
  fi
}

## ------------------------------------------------------------------ screen --

shot() { # shot <outfile.png>
  import -display "$DISPLAY_NUM" -window root "$1" 2>/dev/null || die "screenshot failed: $1"
}

img_stats() { img_stats_target "$1"; }
img_stats_target() { # img_stats <png> [WxH+X+Y]
  local png=$1 crop=${2:-}
  if [[ -n "$crop" ]]; then
    magick "$png" -crop "$crop" +repage -format '%[entropy] %[standard-deviation]' info: 2>/dev/null
  else
    magick "$png" -format '%[entropy] %[standard-deviation]' info: 2>/dev/null
  fi
}

entropy_of() { img_stats "$1" "${2:-}" | awk '{print $1}'; }

# Absolute geometry of an X window as a crop spec: WxH+X+Y
window_crop() { # window_crop <windowid>
  local wid=$1 abs x y w h
  abs=$(xwininfo -display "$DISPLAY_NUM" -id "$wid" 2>/dev/null |
    awk '/Absolute upper-left X/{printf "%s", $4" "} /Absolute upper-left Y/{printf "%s", $4}')
  read -r x y <<<"$abs"
  w=$(xdotool getwindowgeometry --shell "$wid" 2>/dev/null | awk -F= '/^WIDTH/{print $2}')
  h=$(xdotool getwindowgeometry --shell "$wid" 2>/dev/null | awk -F= '/^HEIGHT/{print $2}')
  [[ -n "${x:-}" && -n "${y:-}" && -n "${w:-}" && -n "${h:-}" ]] || return 1
  echo "${w}x${h}+${x}+${y}"
}

diff_pixels() { # diff_pixels <a.png> <b.png> -> differing pixel count (AE)
  # IM7 prints AE as a float ("58463.7 (0.0446042)") and exits non-zero when
  # the images differ, so read stdout/stderr text rather than the exit code.
  local out
  out=$(magick compare -metric AE "$1" "$2" -compose src null: 2>&1 | awk '{print $1}' | head -1)
  [[ "$out" =~ ^[0-9]+(\.[0-9]+)?$ ]] && echo "$out" || echo "nan"
}

diff_fraction() { # diff_fraction <a.png> <b.png> -> differing fraction of pixels (0..1)
  local ae n
  ae=$(diff_pixels "$1" "$2")
  n=$(magick identify -format '%w %h' "$1" | head -1 | awk '{print $1*$2}')
  awk -v ae="$ae" -v n="$n" 'BEGIN{ if (n+0>0) printf "%.6f", ae/n; else print "nan" }'
}

# Crop a window region out of a root screenshot into its own file.
# usage: crop_shot <in.png> <out.png> <windowid>
crop_shot() {
  local crop
  crop=$(window_crop "$3") || { cp "$1" "$2"; return 0; }
  magick "$1" -crop "$crop" +repage "$2" 2>/dev/null || cp "$1" "$2"
}

is_rendered() { # is_rendered <png> [threshold] [crop]
  local e t=${2:-0.1}
  e=$(entropy_of "$1" "${3:-}")
  [[ -n "$e" ]] || return 1
  awk -v e="$e" -v t="$t" 'BEGIN{exit !(e+0 > t+0)}'
}

# Enable emulator logging so user/Logs/dolphin.log records the video backend
# actually selected (docs/spec/11: "record emulator build + config + shaders /
# video backend").
enable_emulator_logging() {
  # Real Logger.ini layout: [Logs] <Group> = <bool>, [Options] WriteToFile.
  mkdir -p "$USER_DIR/Config"
  cat >"$USER_DIR/Config/Logger.ini" <<-'INI'
[Logs]
BOOT = True
CORE = True
GP = True
Video = True
Host GPU = True
DVD = True
EXI = True
SI = True
DSPHLE = True
JIT = True
COMMON = True
MASTER = True
[Options]
WriteToConsole = False
WriteToFile = True
WriteToWindow = False
INI
  log "emulator logging enabled -> $USER_DIR/Logs/dolphin.log"
}

## ------------------------------------------------------------------- input --
# Controller input pipes, upstream Source/Core/InputCommon/ControllerInterface/
# Pipes/Pipes.cpp: every file in <user>/Pipes/ becomes device "Pipe/<i>/<name>".
# Commands (newline terminated, case sensitive):
#   PRESS|RELEASE {A,B,X,Y,Z,START,L,R,D_UP,D_DOWN,D_LEFT,D_RIGHT}
#   SET {L,R} [0,1]    SET {MAIN,C} [0,1] [0,1]

make_pipe() {
  mkdir -p "$PIPE_DIR"
  [[ -p "$PIPE_PATH" ]] || mkfifo "$PIPE_PATH"
  log "controller pipe ready: $PIPE_PATH"
}

# Keep one writer end open so Dolphin never sees EOF. The open() itself blocks
# until Dolphin opens the read end, hence the background process.
open_pipe_writer() {
  sleep infinity >"$PIPE_PATH" 2>/dev/null &
  PIPE_WRITER_PID=$!
  log "pipe writer holding $PIPE_PATH open (pid $PIPE_WRITER_PID)"
}

# Wait until the holding writer has actually connected (Dolphin opened the pipe).
wait_pipe_writer() {
  local timeout="${1:-30}" i fd
  for ((i = 0; i < timeout; i++)); do
    for fd in /proc/$PIPE_WRITER_PID/fd/*; do
      [[ "$(readlink "$fd" 2>/dev/null || true)" == "$PIPE_PATH" ]] && return 0
    done
    sleep 1
  done
  return 1
}

send_pipe() { # send_pipe <COMMAND ...>
  log "pipe cmd -> $1"
  timeout 10 bash -c "printf '%s\n' \"\$1\" > \"\$2\"" _ "$1" "$PIPE_PATH" ||
    die "could not write to $PIPE_PATH (no reader? dolphin not using the pipe)"
}
