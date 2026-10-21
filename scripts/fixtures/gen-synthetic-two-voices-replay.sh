#!/usr/bin/env bash
# gen-synthetic-two-voices-replay.sh — deterministic replay fixture for the
# two-character, two-voice speech scenario (issue #36, gap row G3, ADR-0132;
# docs/spec/11 "Fixture origins": generated synthetic footage).
#
# What it models is a SHORT CONVERSATION: three textboxes, each revealed in
# one typewriter step and held for three frames before the next replaces it —
# a 400 ms box cadence. Box 1 and box 2 belong to two different characters,
# box 3 is unattributed (the narrator's line). The scenario
# (testdata/scenarios/synthetic/two-voices-001.yaml) declares the speaker
# attribution AND the per-character approved voices: the clip carries no
# character information, the attribution is the scenario's, not the clip's.
# With the session running the product's catch_up policy and the scenario's
# tone outlasting the 400 ms cadence (inference.tts_tone_ms), box 1 speaks
# at once in its character's voice and boxes 2 and 3 commit while the
# previous utterance is still playing, so they speak back-to-back as two
# separate speaker runs — each in its own voice, box 3 in the narrator's.
#
# Every frame and every sentence is invented for this script. No real game
# frames, no real game text (AGENTS.md rule 5); truth.json labels every row
# origin=generated_synthetic.
#
# Determinism: fixed noise seed, no clock, PNG date/time chunks excluded, and
# a 20-colour palette with no dithering — a re-run reproduces byte-identical
# files, the way gen-synthetic-catch-up-replay.sh's do.
#
# Usage: scripts/fixtures/gen-synthetic-two-voices-replay.sh [outdir]
set -euo pipefail

OUT=${1:-testdata/synthetic/replay/two-voices-001}
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

# Textbox geometry: the same bottom box the dialogue-advance and catch-up
# corpora use (see gen-synthetic-replay.sh), so one profile's geometry works
# for all three clips.
BX=45
BY=346
BW=550
BH=110

# Invented lines (the three boxes, in box order). Deliberately similar in
# length and register to the catch-up corpus's sentences, so a replay through
# the real pipeline is comparable to that clip's numbers. The speaker
# attribution and the per-character voices live in the scenario, not here.
LINE_1='The lighthouse keeper had not come down in three days.'
LINE_2='His lamp still burns, though the town has stopped watching.'
LINE_3='Somewhere past the breakwater a horn sounds twice.'

# Scene: one fixed seed for the whole sequence — identical backgrounds are
# the point (the only thing that changes frame to frame is the textbox). A
# different seed from the catch-up clip so the two corpora's frames are
# distinguishable byte-for-byte.
magick -size "${W}x${H}" "gradient:#2b4a5a-#8a9a8a" \
	-fill 'rgba(28,40,48,0.5)' -draw "stroke none rectangle 0,$((H * 2 / 3)) ${W},${H}" \
	-seed 17 -attenuate 0.22 +noise Gaussian \
	-blur 0x0.9 -dither none -colors 20 "${PNG[@]}" "$TMP/scene.png"

# draw_box <out> — the rounded translucent textbox (solid; the APPEARING
# state is not what this clip models).
draw_box() {
	magick "$TMP/scene.png" \
		-fill 'rgba(11,16,24,0.82)' -draw "stroke none roundrectangle ${BX},${BY} $((BX + BW - 1)),$((BY + BH - 1)) 16,16" \
		-stroke 'rgba(207,224,242,0.9)' -strokewidth 2 -fill none \
		-draw "roundrectangle $((BX + 1)),$((BY + 1)) $((BX + BW - 2)),$((BY + BH - 2)) 15,15" \
		"${PNG[@]}" "$1"
}

# draw_text <inout> <text> — one line inside the box, sized to fit (the
# shrinking is measured, the way gen-synthetic-replay.sh does it).
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

# prefix <line> — the typewriter step: a first word chunk, then the full
# line (the next frame shows the whole line). Word chunks (not characters)
# are how the synthetic corpora reveal, and a single step is the reveal this
# clip models.
prefix() {
	local line=$1
	local -a words
	read -r -a words <<<"$line"
	local n=${#words[@]}
	local half=$((n / 2))
	local s='' j
	for ((j = 0; j < half; j++)); do s+="${words[$j]} "; done
	printf '%s' "${s% }"
}

# emit <text> <variant> — render one frame and record it.
IDX=0
emit() {
	local text=$1 variant=$2
	IDX=$((IDX + 1))
	local name out
	name=$(printf 'frame-%03d.png' "$IDX")
	out="$TMP/$name"
	if [ -z "$text" ]; then
		cp "$TMP/scene.png" "$out"
	else
		draw_box "$out"
		draw_text "$out" "$text"
	fi
	magick "$out" -dither none -colors 20 "${PNG[@]}" "$out"
	cp "$out" "$OUT/$name"
	printf '%s\t%s\t%s\n' "$name" "$text" "$variant" >>"$TMP/index"
}

mkdir -p "$OUT"
# The corpus is regenerated whole: a stale frame left behind would sit in a
# ground-truth directory with no truth row (docs/spec/12 "stage origins must
# be recorded").
rm -f "$OUT"/*.png "$OUT/timestamps.json" "$OUT/truth.json"
: >"$TMP/index"

# --- sequence ----------------------------------------------------------------
# 18 frames at 10 fps: two empty frames, three boxes of four frames each
# (one typewriter step + three held), four held frames of the last box. The
# 400 ms box cadence is the commit cadence the scenario's tone (1000 ms)
# outlasts: a line committed while the previous utterance is still playing
# is the back-to-back line the per-character voices grade (issue #36,
# ADR-0132).
emit '' empty_scene
emit '' empty_scene
for L in 1 2 3; do
	full="LINE_$L"
	full=${!full}
	emit "$(prefix "$full")" "revealing"
	emit "$full" "stable"
	emit "$full" "stable"
	emit "$full" "stable"
done
# Hold box C's text for the tail: the last box stays on screen until the
# conversation ends (a real dialogue does not clear mid-conversation). The
# held frames re-OCR the same line, which the session's lastKey dedupe keeps
# from re-committing.
full_c="LINE_3"
full_c=${!full_c}
for _ in 1 2 3 4; do
	emit "$full_c" "held"
done

# --- timestamps.json: exact 100 ms steps, first frame at t=100 ms -----------
python3 - "$TMP/index" "$STEP_NS" >"$OUT/timestamps.json" <<'PY'
import json, sys

step = int(sys.argv[2])
rows = [l.split("\t") for l in open(sys.argv[1], encoding="utf-8").read().splitlines()]
print(json.dumps(
    [{"file": r[0], "ts_ns": (i + 1) * step} for i, r in enumerate(rows)],
    indent=1))
PY

# --- truth.json ---------------------------------------------------------------
python3 - "$TMP/index" "$STEP_NS" "$LINE_1" "$LINE_2" "$LINE_3" >"$OUT/truth.json" <<'PY'
import json, sys

step = int(sys.argv[2])
lines = [sys.argv[i] for i in (3, 4, 5)]
rows = [l.rstrip("\n").split("\t") for l in open(sys.argv[1], encoding="utf-8").read().splitlines()]

# The region/box rects match the catch-up and dialogue-advance corpora so one
# profile's geometry works for all three clips (the box with its decorative
# border masked out).
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

fulls = [" ".join(l.split()) for l in lines]

entries = []
for i, (f, text, variant) in enumerate(rows):
    text = " ".join(text.split())
    if text == "":
        line = ""
    else:
        # The held frames name the full line; the reveal frames are the
        # prefix of the box they reveal (the typewriter), so the label is
        # the box whose full line starts with the frame's text.
        line = ""
        for j, full in enumerate(fulls):
            if text == full or full.startswith(text):
                line = "abc"[j]
                break
        if line == "":
            raise SystemExit("truth: frame %s shows text that belongs to no box: %r" % (f, text))
    entries.append({
        "file": f,
        "ts_ns": (i + 1) * step,
        "box_present": text != "",
        # `text` is what a perfect extractor would return for this frame;
        # during the reveal it is a prefix (the typewriter step).
        # `text_is_full_line` separates the reveal frames from the held
        # ones (docs/spec/05: a typewriter prefix is not a new utterance).
        "text": text,
        "text_is_full_line": text in fulls,
        "line": line,
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
