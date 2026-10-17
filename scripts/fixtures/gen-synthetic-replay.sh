#!/usr/bin/env bash
# gen-synthetic-replay.sh — deterministic replay fixture for internal/capture
# (docs/spec/11 "Fixture origins": generated synthetic footage; docs/spec/12
# "Frame replay": the gate that runs on every PR).
#
# What it models is a *timeline*, which a directory of stills does not: an empty
# scene, a textbox appearing, one line revealed by a typewriter, the line held
# stable, an instant second page, and the box dismissed — 24 frames at a fixed
# 10 fps cadence, with the recorded capture timestamps that let a replay source
# be checked for pacing rather than just for pixels.
#
# Every frame and every sentence is invented for this script. No real game
# frames, no real game text (AGENTS.md rule 5); truth.json labels every row
# origin=generated_synthetic.
#
# Determinism: fixed noise seeds, no clock, PNG date/time chunks excluded, and a
# 24-colour palette with no dithering, so a re-run reproduces byte-identical
# files — which the milestone gate verifies with sha256sum -c. That property is
# not cosmetics: a fixture generator that drifts makes every frame hash in every
# recorded expectation wrong, and a test cannot tell "the pipeline changed" from
# "the generator changed" once the inputs are unstable.
#
# Usage: scripts/fixtures/gen-synthetic-replay.sh [outdir]
set -euo pipefail

OUT=${1:-testdata/synthetic/replay/dialogue-advance-001}
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

magick -version >/dev/null
command -v fc-match >/dev/null || { echo "need fc-match (fontconfig)" >&2; exit 1; }
FONT=$(fc-match -f '%{file}' "DejaVu Sans")
[ -f "$FONT" ] || { echo "font not found: DejaVu Sans (fc-match gave '$FONT')" >&2; exit 1; }

W=640
H=480
STEP_NS=100000000 # 10 fps, exactly; the replay source must reproduce this
PNG=(-define png:exclude-chunks=date,time -depth 8 -alpha off)

# Textbox geometry, as fractions of the active picture (docs/spec/04 "Visual
# profiles"): the same bottom box the vision fixtures use, so one profile
# describes both corpora.
BX=45
BY=346
BW=550
BH=110

# Invented lines. Deliberately similar in length and register to the sentences in
# the OCR corpus, so a replay through the real pipeline is comparable to the
# still-image accuracy numbers rather than to a different distribution.
LINE_A='The harbour bell rang twice before anyone looked up.'
LINE_B='Nobody in the village had seen that boat before.'

# Scene: one fixed seed for the whole sequence. Identical backgrounds are the
# point — the only thing that changes frame to frame is the textbox, which is
# what makes Frame.Hash equality a meaningful "nothing happened" signal and lets
# truth.json state box presence per frame without a footnote.
#
# Blur 0.9 + a 20-colour palette is what keeps an empty frame inside the 40 KB
# budget: per-pixel entropy is what a PNG pays for, and this corpus is 24 frames
# that CI regenerates. The text is drawn *after* the scene is quantised and is
# re-quantised with no dithering, which leaves glyph edges crisp enough for the
# OCR corpus thresholds in docs/spec/12 to still apply to it.
magick -size "${W}x${H}" "gradient:#2c4a6e-#9fc0d8" \
	-fill 'rgba(29,43,35,0.5)' -draw "stroke none rectangle 0,$((H * 2 / 3)) ${W},${H}" \
	-seed 7 -attenuate 0.22 +noise Gaussian \
	-blur 0x0.9 -dither none -colors 20 "${PNG[@]}" "$TMP/scene.png"

# draw_box <out> <alpha> — the rounded translucent textbox. <alpha> < 1 is how a
# fade-in is represented: two frames of an empty box before any glyph exists,
# which is the APPEARING state docs/spec/05 gives its own transition for.
draw_box() {
	local out=$1 a=$2 fill edge
	# rgba() carries the box's own alpha; a composited layer with a partial alpha
	# channel would let ImageMagick's quantiser map the box onto the scene behind
	# it and invert the fixture's contrast (learned the hard way in
	# gen-synthetic-textbox.sh).
	if ((a >= 100)); then
		fill='rgba(11,16,24,0.82)'
		edge='rgba(207,224,242,0.9)'
	else
		fill="rgba(11,16,24,$(awk -v a="$a" 'BEGIN { printf "%.2f", 0.82 * a / 100 }'))"
		edge="rgba(207,224,242,$(awk -v a="$a" 'BEGIN { printf "%.2f", 0.9 * a / 100 }'))"
	fi
	magick "$TMP/scene.png" \
		-fill "$fill" -draw "stroke none roundrectangle ${BX},${BY} $((BX + BW - 1)),$((BY + BH - 1)) 16,16" \
		-stroke "$edge" -strokewidth 2 -fill none \
		-draw "roundrectangle $((BX + 1)),$((BY + 1)) $((BX + BW - 2)),$((BY + BH - 2)) 15,15" \
		"${PNG[@]}" "$out"
}

# draw_text <inout> <text> — one line inside the box, sized to fit. Shrinking is
# measured rather than assumed: a prefix that overflows would be clipped by the
# box edge, and a truth row describing text that is not fully on screen would be
# a false label.
draw_text() {
	local inout=$1 text=$2 pt=24 avail=$((BW - 44)) w
	while ((pt > 14)); do
		w=$(magick -size 4000x200 xc:none -font "$FONT" -pointsize "$pt" -fill '#ffffff' \
			-gravity NorthWest -annotate +0+0 "$text" -trim -format '%w' info: 2>/dev/null || echo "")
		case "$w" in
		'' | *[!0-9]*) pt=$((pt - 1)) ;;
		*)
			((w <= avail)) && break
			pt=$((pt - 1))
			;;
		esac
	done
	magick "$inout" -font "$FONT" -pointsize "$pt" -fill '#f2f6fb' -stroke none \
		-gravity NorthWest -annotate +"$((BX + 22))"+$((BY + 18)) "$text" \
		"${PNG[@]}" "$inout"
}

# prefixes <line> <n> — the line in n whole-word chunks, for the typewriter.
# Word boundaries rather than characters because that is how a GameCube text
# engine actually reveals (a per-glyph reveal would be a different fixture), and
# because it guarantees no intermediate frame ever shows a half-word that truth
# would have to describe as something the OCR cannot return.
prefixes() {
	local line=$1 n=$2
	local -a words
	read -r -a words <<<"$line"
	local total=${#words[@]}
	local out=() i start end
	for ((i = 0; i < n; i++)); do
		start=0
		end=$((total * (i + 1) / n))
		((end < 1)) && end=1
		((end > total)) && end=total
		local s=''
		for ((j = start; j < end; j++)); do s+="${words[$j]} "; done
		out+=("${s% }")
	done
	printf '%s\n' "${out[@]}"
}

# emit <index> <box_alpha> <text> <variant> — render one frame and record it.
IDX=0
emit() {
	local alpha=$1 text=$2 variant=$3
	IDX=$((IDX + 1))
	local name out
	name=$(printf 'frame-%03d.png' "$IDX")
	out="$TMP/$name"
	if ((alpha == 0)); then
		cp "$TMP/scene.png" "$out"
	else
		draw_box "$out" "$alpha"
		[ -n "$text" ] && draw_text "$out" "$text"
	fi
	# Palette quantise last: 20 colours with no dithering keeps every frame well
	# inside the 40 KB budget the milestone sets for a corpus that CI regenerates.
	magick "$out" -dither none -colors 20 "${PNG[@]}" "$out"
	cp "$out" "$OUT/$name"
	printf '%s\t%s\t%s\t%s\n' "$name" "$alpha" "$text" "$variant" >>"$TMP/index"
}

mkdir -p "$OUT"
# The corpus is regenerated whole: a stale frame left behind would sit in a
# ground-truth directory with no truth row, which is what docs/spec/12's "stage
# origins must be recorded" rule forbids.
rm -f "$OUT"/*.png "$OUT/timestamps.json" "$OUT/truth.json"
: >"$TMP/index"

# --- sequence --------------------------------------------------------------
for _ in 1 2 3; do emit 0 '' empty_scene; done          # 1-3: no textbox at all
emit 45 '' box_appearing                                # 4: fade-in frame one
emit 100 '' box_appearing                               # 5: box solid, still no text
mapfile -t PRE_A < <(prefixes "$LINE_A" 8)
for p in "${PRE_A[@]}"; do emit 100 "$p" revealing; done # 6-13: typewriter
for _ in 1 2 3 4 5; do emit 100 "$LINE_A" stable; done   # 14-18: held, identical
for _ in 1 2 3 4; do emit 100 "$LINE_B" page_two; done   # 19-22: instant second page
for _ in 1 2; do emit 0 '' box_dismissed; done           # 23-24: gone

# --- timestamps.json: exact 100 ms steps, first frame at t=100 ms -----------
# The first frame is not at 0: internal/recognition refuses a non-positive
# capture timestamp, and a fixture that starts at zero would fail the very first
# observation it feeds the tracker (docs/spec/01 invariant 1).
python3 - "$TMP/index" "$STEP_NS" >"$OUT/timestamps.json" <<'PY'
import json, sys

step = int(sys.argv[2])
rows = [l.split("\t") for l in open(sys.argv[1], encoding="utf-8").read().splitlines()]
print(json.dumps(
    [{"file": r[0], "ts_ns": (i + 1) * step} for i, r in enumerate(rows)],
    indent=1))
PY

# --- truth.json -----------------------------------------------------------
python3 - "$TMP/index" "$STEP_NS" "$LINE_A" "$LINE_B" >"$OUT/truth.json" <<'PY'
import json, sys

step = int(sys.argv[2])
line_a, line_b = sys.argv[3], sys.argv[4]
rows = [l.rstrip("\n").split("\t") for l in open(sys.argv[1], encoding="utf-8").read().splitlines()]

# The region/box rects match the still-image corpus so one profile's geometry
# works for both (see scripts/fixtures/gen-synthetic-textbox.sh: the OCR region
# is the box with its decorative border masked out).
BOX = {"x": 0.07, "y": 0.72, "w": 0.86, "h": 0.23}
MARGIN = 17
W, H = 640, 480
bx, by, bw, bh = 45, 346, 550, 110
RECT = {
    "x": round((bx + MARGIN) / W, 6),
    "y": round((by + MARGIN) / H, 6),
    "w": round((bw - 2 * MARGIN) / W, 6),
    "h": round((bh - 2 * MARGIN) / H, 6),
}

full_a = " ".join(line_a.split())
full_b = " ".join(line_b.split())

entries = []
for i, (f, alpha, text, variant) in enumerate(rows):
    text = " ".join(text.split())
    entries.append({
        "file": f,
        "ts_ns": (i + 1) * step,
        "box_present": int(alpha) > 0,
        "box_alpha": int(alpha) / 100.0,
        # `text` is what a perfect extractor would return for this frame, which
        # during the reveal is a *prefix*. `text_is_full_line` separates the
        # reveal frames from the held ones, because docs/spec/05 insists a
        # typewriter prefix is not a new distinct utterance — an evaluator that
        # could not tell the two apart would score a correct tracker as wrong.
        "text": text,
        "text_is_full_line": text == full_a or text == full_b,
        "line": ("a" if text and text in full_a else "b" if text and text in full_b else ""),
        "variant": variant,
        "rect": RECT,
        "box": BOX,
        "size": {"w": W, "h": H},
        "fps": 10,
        "origin": "generated_synthetic",
    })
print(json.dumps(entries, indent=1, ensure_ascii=False))
PY

ls -1 "$OUT"/timestamps.json "$OUT"/truth.json "$OUT"/frame-*.png | sed "s|^$OUT/||"
magick identify -format '%f %wx%h %b\n' "$OUT"/frame-*.png | sort -k3 -h | tail -3
