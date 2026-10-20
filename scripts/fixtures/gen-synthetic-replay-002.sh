#!/usr/bin/env bash
# gen-synthetic-replay-002.sh — G12: render the *second* synthetic replay clip
# (Black Water holdout) into testdata/synthetic/replay/black-water-001/.
#
# Deliberately different from gen-synthetic-replay.sh (the harbour clip),
# per ADR-0078:
#   * UPPER-LEFT square (sharp-corner) box, not bottom rounded rectangle;
#   * INSTANT page reveals (no typewriter, no alpha fade) — recognition
#     must come from box geometry + stability, not from a reveal trace;
#   * one TWO-LINE wrapped page and one NON-LATIN (Cyrillic) page — the
#     eng OCR model is expected to fail the Cyrillic page and the pipeline
#     is expected to abstain (commit "unknown", never fabricate a line);
#   * different background scene and noise seed, same 640x480 @ 10 fps
#     frame format and truth schema as the first clip.
#
# No game ROM, no game text, no network. All lines are invented by this
# project. Usage: scripts/fixtures/gen-synthetic-replay-002.sh [OUT_DIR]
set -euo pipefail
cd "$(dirname "$0")/../.."

OUT="${1:-testdata/synthetic/replay/black-water-001}"
rm -rf "$OUT"
mkdir -p "$OUT"

# PNG options for every encode. The exclude is load-bearing, not cosmetic:
# every copy/draw step re-encodes the frame, and without the define each
# encode re-injects date:create / date:modify / date:timestamp / tIME chunks,
# so a re-run is never byte-identical and the determinism fence
# (TestSecondReplayClipGeneratorDeterministic) cannot hold. The first clip's
# generator threads the same array through every write for the same reason.
PNG=(-define png:exclude-chunks=date,time -depth 8 -alpha off)

# ---------------------------------------------------------------------------
# Scene: vertical gradient (different hue from the harbour clip) plus a
# different Gaussian-noise seed.
# ---------------------------------------------------------------------------
magick -size 640x480 gradient:'#233d4d'-'#7fa3b5' \
  -seed 11 -attenuate 0.22 +noise Gaussian \
  -blur 0x0.9 -dither none -colors 20 \
  "${PNG[@]}" "$OUT/.scene.png"

# Invented lines (Black Water holdout; the first two are Latin, the third is
# Cyrillic on purpose — see ADR-0078).
PAGE_A="You paid the ferryman. The crossing is done now."
PAGE_B="This is not a toll. This is a habit."
PAGE_C="Вода помнит то, что люди прячут."

# The box (upper-left, square, sharp corners) matches bench set 002's 480p
# layout exactly, so the clip exercises the same geometry the bench set
# calibrates against.
BX=38; BY=77; BW=397; BH=96

# Fonts, resolved at render time from fontconfig the same way the bench
# renderer resolves them (generate.go): no font file is committed.
# Latin pages use Liberation Sans (a different family from the first clip's
# DejaVu Sans); the Cyrillic page uses Noto Sans, the bench set's foreign
# font (generate002.go secondForeignFont). IM's -font name lookup rejects
# CFF fonts, so Noto Sans is passed by file path.
FONT_LATIN="Liberation-Sans"
FONT_FOREIGN=$(fc-match -f "%{file}" "Noto Sans")

measure() { # rendered width of $1 at point size $2 in font $3, same font and
  # -annotate path the renderer uses below. A 100px pango:label canvas
  # clamped long lines to 100px, so they never wrapped and overflowed the
  # box (OCR saw truncated text); the trimmed %@ width is exact.
  magick -size 4000x200 xc:none -font "$3" -pointsize "$2" \
    -fill white -gravity center -annotate +0+0 "$1" \
    -format "%@" info: 2>/dev/null | head -n 1 | cut -d x -f 1
}

wrap_lines() { # $1 text  $2 pt  $3 font -> newline-separated wrapped lines
  local avail=$((BW - 36))
  local words=($1) cur="" out="" w trial
  for w in "${words[@]}"; do
    if [[ -z "$cur" ]]; then
      cur="$w"
    else
      trial="$cur $w"
      if [[ "$(measure "$trial" "$2" "$3")" -le "$avail" ]]; then
        cur="$trial"
      else
        out+="${out:+$'\n'}$cur"
        cur="$w"
      fi
    fi
  done
  [[ -n "$cur" ]] && out+="${out:+$'\n'}$cur"
  printf '%s' "$out"
}

max_line() { # $1 lines  $2 pt  $3 font -> widest line in pixels
  local max=0 w
  while IFS= read -r w; do
    [[ -z "$w" ]] && continue
    w=$(measure "$w" "$2" "$3")
    (( w > max )) && max=$w
  done <<< "$1"
  echo "$max"
}

draw_page() { # $1 out  $2 text  $3 font — greedy word wrap, shrunk to fit
  # Start at 26pt, not 24: at 24pt the engine chain (2x nearest + Otsu +
  # median) reads a lone "I" stem as "|" and smears "coin"; 26pt is the
  # smallest size at which both survive (verified with the real Tesseract
  # 5.5.3 through the pipeline's exact crop/preprocess, ADR-0079), and the
  # shrink loop below still lands every page inside the 96px box.
  local pt=30 lines n dx dy lead
  # -annotate offsets are relative to the image centre, so translate the
  # box centre into image-centre coordinates.
  dx=$((BX + BW / 2 - 320))
  dy=$((BY + BH / 2 - 240))
  lines=$(wrap_lines "$2" "$pt" "$3")
  while [[ "$(max_line "$lines" "$pt" "$3")" -gt $((BW - 36)) ]] && (( pt > 14 )); do
    pt=$((pt - 1))
    lines=$(wrap_lines "$2" "$pt" "$3")
  done
  n=$(awk 'END { print NR }' <<< "$lines")
  if (( n > 2 )); then
    echo "error: page wraps to $n lines; the 96px box holds at most two (shrink the text or grow the box)" >&2
    return 1
  fi
  lead=$((pt + 6))
  if (( n == 1 )); then
    magick "$OUT/.box.png" -font "$3" -pointsize "$pt" -fill '#e8edf0' \
      -stroke '#e8edf0' -strokewidth 1 -gravity center -annotate "+$dx+$dy" "$lines" -stroke none "${PNG[@]}" "$1"
  else
    mapfile -t L <<< "$lines"
    magick "$OUT/.box.png" -font "$3" -pointsize "$pt" -fill '#e8edf0' \
      -stroke '#e8edf0' -strokewidth 1 -gravity center -annotate "+$dx+$((dy - lead / 2))" "${L[0]}" \
      -gravity center -annotate "+$dx+$((dy + lead / 2))" "${L[1]}" -stroke none "${PNG[@]}" "$1"
  fi
}

# Frame schedule (10 fps, 24 frames): 1-3 empty, 4 box w/o text,
# 5-9 page A, 10-14 page B, 15-20 page C (Cyrillic), 21-24 dismissed.
ROWS=(
  "0  " "0  " "0  "
  "1  "
  "1 A" "1 A" "1 A" "1 A" "1 A"
  "1 B" "1 B" "1 B" "1 B" "1 B"
  "1 C" "1 C" "1 C" "1 C" "1 C" "1 C"
  "0  " "0  " "0  " "0  "
)

for i in "${!ROWS[@]}"; do
  f=$(printf "%02d" "$((i + 1))")
  read -r box page <<< "${ROWS[$i]}"
  if [[ "$box" != "1" ]]; then
    magick "$OUT/.scene.png" -alpha set "${PNG[@]}" "$OUT/frame-$f.png"
    magick "$OUT/frame-$f.png" -dither none -colors 20 "${PNG[@]}" "$OUT/frame-$f.png"
    continue
  fi
  # Sharp-cornered box: flat fill + plain rectangle stroke (no rounding).
  magick "$OUT/.scene.png" \
    -fill 'rgba(13,24,31,0.85)' -draw "rectangle $BX,$BY $((BX + BW)),$((BY + BH))" \
    -stroke '#b0c7d0' -strokewidth 2 -draw "rectangle $BX,$BY $((BX + BW)),$((BY + BH))" \
    -strokewidth 0 "${PNG[@]}" "$OUT/.box.png"
  if [[ -z "$page" ]]; then
    magick "$OUT/.box.png" -alpha set "${PNG[@]}" "$OUT/frame-$f.png"
  else
    case "$page" in
      A) draw_page "$OUT/frame-$f.png" "$PAGE_A" "$FONT_LATIN" ;;
      B) draw_page "$OUT/frame-$f.png" "$PAGE_B" "$FONT_LATIN" ;;
      C) draw_page "$OUT/frame-$f.png" "$PAGE_C" "$FONT_FOREIGN" ;;
    esac
  fi
  # Final re-quantise of every emitted frame (the first clip's same pass, its
  # gen-synthetic-replay.sh line "-dither none -colors 20"): the box's 0.85
  # alpha fill lets the scene's 8-bit noise bleed through behind the text,
  # and without the re-quantise the box interior keeps full 256-level noise
  # that the pipeline's Otsu + median chain reads as texture — the OCR saw
  # "Mol ool | oid not san" where the first clip sees its line at 0.91.
  # No dithering, so glyph edges stay as crisp as the quantised palette allows.
  magick "$OUT/frame-$f.png" -dither none -colors 20 "${PNG[@]}" "$OUT/frame-$f.png"
done

# timestamps.json: index -> frame file + the recorded capture time (10 fps),
# the same shape the replay source's manifest requires (ts_ns is the capture
# clock every delay assertion reads; the first clip's manifest carries it).
python3 - "$OUT" <<'PY'
import json, sys
out = sys.argv[1]
entries = [{"file": f"frame-{i:02d}.png", "ts_ns": i * 100_000_000} for i in range(1, 25)]
with open(f"{out}/timestamps.json", "w") as f:
    json.dump(entries, f, indent=1)
PY

# truth.json: same schema as the harbour clip; every text frame is a full
# line because the reveal is instant (no typewriter prefix exists here).
python3 - "$OUT" <<PY
import json, sys
out = sys.argv[1]
step = 100_000_000
W, H = 640, 480
MARGIN = 17
bx, by, bw, bh = $BX, $BY, $BW, $BH
BOX = {"x": round(bx / W, 6), "y": round(by / H, 6), "w": round(bw / W, 6), "h": round(bh / H, 6)}
RECT = {
    "x": round((bx + MARGIN) / W, 6),
    "y": round((by + MARGIN) / H, 6),
    "w": round((bw - 2 * MARGIN) / W, 6),
    "h": round((bh - 2 * MARGIN) / H, 6),
}
page_a = "$PAGE_A"
page_b = "$PAGE_B"
page_c = "$PAGE_C"
# (text, line) per frame index; frame 4 has the box but no text yet.
specs = [("", None)] * 3 + [("", None)] \
       + [(page_a, "a")] * 5 + [(page_b, "b")] * 5 \
       + [(page_c, "c")] * 6 + [("", None)] * 4
entries = []
for i, (text, line) in enumerate(specs):
    entries.append({
        "file": f"frame-{i + 1:02d}.png",
        "ts_ns": (i + 1) * step,
        "box_present": text != "" or i == 3,
        "box_alpha": 1.0,
        "text": text,
        "text_is_full_line": text in (page_a, page_b, page_c),
        "line": line or "",
        "variant": "instant",
        "rect": RECT,
        "box": BOX,
        "size": {"w": W, "h": H},
        "fps": 10,
        "origin": "generated_synthetic",
    })
with open(f"{out}/truth.json", "w") as f:
    json.dump(entries, f, indent=1, ensure_ascii=False)
PY

rm -f "$OUT/.scene.png" "$OUT/.box.png"
ls -1 "$OUT"/timestamps.json "$OUT"/truth.json "$OUT"/frame-*.png | sed "s|^$OUT/||"
magick identify -format '%f %wx%h %b\n' "$OUT"/frame-*.png | sort -k3 -h | tail -3
