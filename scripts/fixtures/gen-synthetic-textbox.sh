#!/usr/bin/env bash
# gen-synthetic-textbox.sh — deterministic synthetic textbox fixtures for
# internal/vision (docs/spec/11 "Fixture origins" item 3: "use generated
# synthetic footage for open CI"; docs/spec/12 "Benchmarks and ground truth").
#
# Every frame is invented: a game-like scene background, a game-like textbox and
# sentences written for this script. No real game text, no screenshots
# (AGENTS.md rule 5); every truth entry is labelled origin=generated_synthetic.
#
# Determinism: fixed noise seeds, no clock, and PNG writes exclude the date/time
# chunks, so a re-run reproduces byte-identical files (verified by
# `sha256sum -c` in the milestone gate). Needs ImageMagick 7 (`magick`),
# fontconfig (`fc-match`) and the DejaVu Sans / Noto Serif / Liberation Mono
# families.
#
# Usage: scripts/fixtures/gen-synthetic-textbox.sh [outdir]
set -euo pipefail

OUT=${1:-testdata/synthetic/textbox}
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

magick -version >/dev/null
command -v fc-match >/dev/null || { echo "need fc-match (fontconfig)" >&2; exit 1; }

font_path() { # font_path <family> -> absolute font file, or fail loudly
	local f
	f=$(fc-match -f '%{file}' "$1")
	[ -f "$f" ] || { echo "font not found: $1 (fc-match gave '$f')" >&2; exit 1; }
	printf '%s\n' "$f"
}
DEJAVU=$(font_path "DejaVu Sans")
NOTO=$(font_path "Noto Serif")
MONO=$(font_path "Liberation Mono")

PNG=(-define png:exclude-chunks=date,time -depth 8 -alpha off)

# Normalized textbox geometry — docs/spec/04 "Visual profiles": a bottom
# dialogue box.
# RX/RY/RW/RH are fractions of the *active picture*, so the same numbers describe
# the 4:3 and the pillarboxed fixtures. MARGIN is the border mask in pixels: it
# must clear the 2px stroke and the 16px corner radius, both of which are inside
# the box outline.
RX=0.07
RY=0.72
RW=0.86
RH=0.23
MARGIN=17

box_px() { # box_px <w> <h> -> "x y w h" in pixels
	awk -v w="$1" -v h="$2" -v rx="$RX" -v ry="$RY" -v rw="$RW" -v rh="$RH" \
		'BEGIN { printf "%d %d %d %d", int(rx * w + 0.5), int(ry * h + 0.5), int(rw * w + 0.5), int(rh * h + 0.5) }'
}

# scene <seed> <out> <w> <h> — sky gradient, a darker horizon third, seeded
# analog noise, light blur, then quantised so the file stays small.
#
# Translucent parts are drawn as rgba() fills, never as a composited layer with
# an alpha channel. Keeping every frame fully opaque is not cosmetic: with a
# partial alpha channel present, ImageMagick's `-colors` quantiser mapped a light
# textbox onto the dark scene behind it and silently inverted that fixture's
# contrast — a truth row that no longer describes its own pixels is worse than no
# truth row at all.
scene() {
	local seed=$1 out=$2 w=$3 h=$4
	magick -size "${w}x${h}" "gradient:#2c4a6e-#9fc0d8" \
		-fill 'rgba(29,43,35,0.5)' -draw "stroke none rectangle 0,$((h * 2 / 3)) ${w},${h}" \
		-seed "$seed" -attenuate 0.22 +noise Gaussian \
		-blur 0x0.7 \
		-dither none -colors 24 \
		"${PNG[@]}" "$out"
}

# textbox <inout> <x> <y> <bw> <bh> <style> — rounded semi-transparent box at an
# absolute frame position; sets TEXT_COLOR for the text that goes on it. Styles
# follow docs/spec/12 "negative contrast" (dark box, light text) and its inverse.
# The rectangle coordinates are absolute because -draw works in canvas space: the
# first version of this function drew the box at the origin of every frame, which
# an OCR test notices only when the crop region lands outside the box and finds
# nothing but sky.
TEXT_COLOR=''
textbox() {
	local inout=$1 x=$2 y=$3 bw=$4 bh=$5 style=$6 fill edge
	local x1=$((x + bw - 1)) y1=$((y + bh - 1)) x2=$((x + 1)) y2=$((y + 1))
	local x3=$((x + bw - 2)) y3=$((y + bh - 2))
	# rgba() carries the box's own alpha of 0.82, per docs/spec/04's
	# "background/text color hints" for a semi-transparent box.
	case "$style" in
	dark) fill='rgba(11,16,24,0.82)' edge='rgba(207,224,242,0.9)' TEXT_COLOR='#f2f6fb' ;;
	light) fill='rgba(244,241,230,0.82)' edge='rgba(74,68,54,0.9)' TEXT_COLOR='#14181f' ;;
	*) echo "unknown box style: $style" >&2; return 1 ;;
	esac
	magick "$inout" \
		-fill "$fill" -draw "stroke none roundrectangle ${x},${y} ${x1},${y1} 16,16" \
		-stroke "$edge" -strokewidth 2 -fill none \
		-draw "roundrectangle ${x2},${y2} ${x3},${y3} 15,15" \
		"${PNG[@]}" "$inout"
}

# text_width <font> <pt> <line> — rendered ink width in pixels. Measured rather
# than guessed, because a sentence that overflows the textbox is clipped by the
# frame edge and the fixture would then be a lie about its own ground truth.
text_width() {
	local w
	w=$(magick -size 4000x200 xc:none -font "$1" -pointsize "$2" -fill '#ffffff' \
		-gravity NorthWest -annotate +0+0 "$3" -trim -format '%w' info: 2>/dev/null || echo "")
	case "$w" in
	'' | *[!0-9]*) echo 4000 ;; # unreadable metric: pretend it overflows and shrink
	*) echo "$w" ;;
	esac
}

# write_text <inout> <font> <maxpt> <availw> <x> <y> <line...> — one -annotate
# per line; this ImageMagick build has no -line-spacing, so leading is computed
# here and stays identical on every host. The point size shrinks until every
# line fits the box, which keeps the rendered text inside the region rect that
# truth.json records.
write_text() {
	local inout=$1 font=$2 pt=$3 avail=$4 x=$5 y=$6
	shift 6
	local line w fit
	while ((pt > 12)); do
		fit=1
		for line in "$@"; do
			if (($(text_width "$font" "$pt" "$line") > avail)); then
				fit=0
				break
			fi
		done
		((fit == 1)) && break
		pt=$((pt - 1))
	done
	local i=0 lead=$((pt + 5))
	for line in "$@"; do
		magick "$inout" -font "$font" -pointsize "$pt" \
			-fill "$TEXT_COLOR" -stroke none \
			-gravity NorthWest -annotate +"$x"+"$((y + i * lead))" "$line" \
			"${PNG[@]}" "$inout"
		i=$((i + 1))
	done
}

# degrade <inout> <seed> — motion blur + analog grain. The second, wider blur
# models the capture chain's own low-pass (and keeps the PNG inside the size
# budget: per-pixel entropy is what makes a frame expensive to store).
degrade() {
	magick "$1" -blur 0x0.6 -seed "$2" -attenuate 0.4 +noise Gaussian \
		-blur 0x1.2 "${PNG[@]}" "$1"
}

# finalize <inout> — quantise to a small palette with no dithering. Text
# contrast survives; per-pixel noise does not multiply into hundreds of colours.
finalize() {
	magick "$1" -dither none -colors 24 "${PNG[@]}" "$1"
}

# letterbox <inout> <w> — paste the 4:3 picture into a wide frame with black
# pillar bars, as a 16:9 capture of a 4:3 console output looks.
letterbox() {
	local inout=$1 w=$2 h bar
	h=$(magick identify -format '%h' "$inout")
	bar=$((w - h * 4 / 3))
	((bar < 0)) && bar=0
	magick -size "${w}x${h}" xc:black "$inout" -geometry "+$(((bar + 1) / 2))+0" \
		-compose over -composite "${PNG[@]}" "$inout"
}

emit() { # emit <file> <variant> <font> <text> <w> <h> <letterboxed> <boxrect> <regionrect>
	printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$@" >>"$TMP/index"
}

# frame <name> <framew> <w> <h> <seed> <style> <font> <pt> <lb> <degrade> <variant> [line...]
# <w>x<h> is the *active picture*; <framew> is the capture frame width, larger
# than <w> only for the pillarboxed fixtures (they are the same height). <lb>
# is 1 when the frame is letterboxed and is recorded in truth.json. pt 0 means
# "no text" — the empty-textbox fixture.
frame() {
	local name=$1 fw=$2 w=$3 h=$4 seed=$5 style=$6 font=$7 pt=$8 lb=$9 deg=${10}
	shift 10
	local variant=$1
	shift
	local bx by bw bh out="$TMP/$name.png" text=''
	# The rect is a fraction of the active picture (w x h), never of the wider
	# capture frame, so the same geometry works for both fixture families.
	read -r bx by bw bh <<<"$(box_px "$w" "$h")"
	scene "$seed" "$out" "$w" "$h"
	textbox "$out" "$bx" "$by" "$bw" "$bh" "$style"
	if ((pt > 0)); then
		write_text "$out" "$font" "$pt" "$((bw - 44))" "$((bx + 22))" "$((by + 18))" "$@"
		text=$(printf '%s ' "$@")
		text=${text% }
	fi
	((deg == 1)) && degrade "$out" "$((seed + 100))"
	if ((lb == 1)); then letterbox "$out" "$fw"; fi
	finalize "$out"
	cp "$out" "$OUT/$name.png"
	local label='none'
	[ "$font" != none ] && label=$(basename "$font" .ttf)
	# Two rects are recorded, because they answer different questions. `box` is
	# where the textbox was drawn; `rect` is the OCR region — the same box inset
	# by the border mask. docs/spec/04 gives a region "margins" and
	# "exclusion masks", and docs/spec/05 says to mask "decorative borders": a
	# profile that cropped a textbox flush to its rounded outline would feed the
	# engine a huge closed curve, which is exactly the shape an OCR engine will
	# confidently turn into letters (see TestSyntheticFixturesOCR's empty-box
	# assertion).
	local boxrect regionrect
	boxrect="$RX,$RY,$RW,$RH"
	regionrect=$(awk -v bx="$bx" -v by="$by" -v bw="$bw" -v bh="$bh" -v w="$w" -v h="$h" -v m="$MARGIN" \
		'BEGIN { printf "%.6f,%.6f,%.6f,%.6f", (bx+m)/w, (by+m)/h, (bw-2*m)/w, (bh-2*m)/h }')
	emit "$name.png" "$variant" "$label" "$text" "$fw" "$h" "$lb" "$boxrect" "$regionrect"
}

rm -f "$TMP/index"
mkdir -p "$OUT"
# The corpus is regenerated as a whole. Removing stale frames is not tidiness: a
# renamed fixture would otherwise stay in the directory with no truth row, and an
# unlabelled frame in a ground-truth directory is exactly the thing docs/spec/12
# forbids ("stage origins" must be recorded). TestFixtureCorpusBudget asserts the
# other half of the invariant — no PNG without a row.
rm -f "$OUT"/*.png

# --- fixtures: 640x480, the 4:3 picture the capture card delivers directly ---
frame plain-dejavu-01 640 640 480 11 dark "$DEJAVU" 25 0 0 plain \
	"The old lighthouse keeps no flame these days."
frame plain-dejavu-02 640 640 480 23 dark "$DEJAVU" 25 0 0 plain \
	"Bring the waterlogged chart to the harbour master."
frame serif-narration-01 640 640 480 31 dark "$NOTO" 25 0 0 plain \
	"Night settled over the bay like a held breath."
frame serif-question-02 640 640 480 43 dark "$NOTO" 24 0 0 plain \
	"You saw the lantern from the reef? Say when."
frame mono-cmd-01 640 640 480 53 dark "$MONO" 23 0 0 plain \
	"Set sail at first light, and take the small boat."
frame twoline-dejavu-01 640 640 480 61 dark "$DEJAVU" 22 0 0 two_line \
	"The keeper swore the light came back on its own, and" \
	"that no one in the village had climbed the stair in years."
frame threeline-mono-01 640 640 480 71 dark "$MONO" 16 0 0 three_line \
	"Every ledger in the customs house records the" \
	"same ship arriving twice on the same wet night," \
	"and the harbour master has stopped asking."
frame punct-noto-01 640 640 480 83 dark "$NOTO" 24 0 0 punctuation \
	"\"Wait...\" he whispered, \"it wasn't the captain's fault!\""
frame punct-dejavu-02 640 640 480 97 dark "$DEJAVU" 24 0 0 punctuation \
	"Don't light it! Not tonight - the tide's wrong."
frame lightbox-dejavu-01 640 640 480 103 light "$DEJAVU" 25 0 0 light_box \
	"A ledger names the ship, but not the crew."
frame accent-noto-01 640 640 480 113 light "$NOTO" 24 0 0 non_ascii \
	"L'épée du vieux phare brille encor' ce soir."
frame noisy-dejavu-01 640 640 480 127 dark "$DEJAVU" 25 0 1 noisy_blur \
	"The door was barred from the other side."
frame noisy-noto-02 640 640 480 139 dark "$NOTO" 25 0 1 noisy_blur \
	"Nothing in the harbour remembered the wreck."
frame empty-box-01 640 640 480 151 dark none 0 0 0 empty_box

# --- fixtures: 854x480, the same 4:3 picture pillarboxed into a wide frame ---
frame letterbox-dejavu-01 854 640 480 163 dark "$DEJAVU" 25 1 0 plain \
	"Two sails, running before a black wind."
frame letterbox-noisy-02 854 640 480 179 dark "$DEJAVU" 24 1 1 noisy_blur \
	"The boy counted the buoys and lost track at nine."

# --- truth.json -----------------------------------------------------------
python3 - "$TMP/index" >"$OUT/truth.json" <<'PY'
import json, sys


def rect(s):
    x, y, w, h = (float(v) for v in s.split(","))
    return {"x": round(x, 6), "y": round(y, 6), "w": round(w, 6), "h": round(h, 6)}


entries = []
for line in open(sys.argv[1], encoding="utf-8"):
    f, variant, font, text, w, h, lb, boxrect, regionrect = line.rstrip("\n").split("\t")
    entries.append({
        "file": f,
        "text": text,
        # rect is the OCR region: the textbox with its decorative border masked
        # out. box is where the textbox was drawn. Both are fractions of the
        # active picture, not of a possibly wider capture frame.
        "rect": rect(regionrect),
        "box": rect(boxrect),
        "size": {"w": int(w), "h": int(h)},
        "letterboxed": lb == "1",
        "variant": variant,
        "font": font if font != "none" else "",
        "origin": "generated_synthetic",
    })
print(json.dumps(entries, indent=1, ensure_ascii=False))
PY

ls -1 "$OUT"/truth.json "$OUT"/*.png
