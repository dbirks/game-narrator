package bench

import (
	"fmt"
)

// The second game's bench set (ADR-0079, gap audit G12): the same harness, the
// same real pipeline, a structurally different game. "Black Water" — the
// synthetic second game whose portability fixture the generic adapter and the
// profile import tests already carry (internal/games/adapter/generic
// synthetic-other-game, TestTwoGamePortability). Every line below is that
// fixture's invented dialogue; nothing here is game-derived (AGENTS.md rule 5).
//
// What differs from the first set, deliberately:
//
//   - font: Liberation Sans for the Latin lines (a different family from the
//     first set's DejaVu Sans) and Noto Sans for the non-Latin line;
//   - box position and shape: upper-left, square corners, a wider/shorter
//     aspect than the first set's bottom box (the Layout the profile carries);
//   - reveal behaviour: instant page reveal — the page is whole on its first
//     frame (instantFrameSchedule), never a typewriter prefix;
//   - two-line wrapped boxes for the long lines (layoutText wraps >40 runes);
//   - one non-Latin (Cyrillic) line in the script: the configured OCR model
//     (eng) cannot read it, so the matcher is graded on its graceful path —
//     abstain, commit no speech — not on a lucky exact match.
const (
	secondSetID = "bench-synthetic-002"
	// secondSetSeedBase is the deterministic seed base the second set records
	// in truth.json (a different base from the first set's, so the two sets'
	// noise draws are independent).
	secondSetSeedBase = 20261019
	// secondLatinFont / secondForeignFont: the set's font treatment matrix.
	// Both families ship with the distro's fontconfig (fc-match resolves them
	// at render time; no font file is committed, same as the first set).
	secondLatinFont   = "Liberation Sans"
	secondForeignFont = "Noto Sans"
)

// secondSetLayout is the second game's box: upper-left, square corners,
// 0.62 × 0.20 (wider, shorter than the first set's 0.86 × 0.23 bottom box).
// A var, not a const: a struct value is not a Go constant.
var secondSetLayout = Layout{BoxX: 0.06, BoxY: 0.16, BoxW: 0.62, BoxH: 0.20, Rounded: false}

// secondSetRegion is the ROI the second set's profile watches: inside the box
// with the border margin, normalized at 480p the same way the renderer's
// pixel rects are derived (0.06·640→38, 0.16·480→77, 0.62·640→397,
// 0.20·480→96; a 20 px margin covers the 17 px border inset at 480p and keeps
// the crop inside the dark panel at the 720p scale).
var secondSetRegion = Region{ID: "upper-left", Type: "textbox", X: (38.0 + 20) / 640, Y: (77.0 + 20) / 480, W: (397.0 - 40) / 640, H: (96.0 - 40) / 480}

// secondGameLine is one scripted line of the second set.
type secondGameLine struct {
	id, speaker, text, font string
}

// secondGameLines is the second set's script, in fixed order (ids and the
// split assignment must stay stable across regenerations). bw-05 is the
// non-Latin line: its truth is the Cyrillic text on screen; the pipeline's
// expected behaviour on it is the graceful abstain, graded by the metrics,
// not by a special case.
var secondGameLines = []secondGameLine{
	{"bw-01", "ferryman", "No coin, no crossing. That is the rule and the whole of it.", secondLatinFont},
	{"bw-02", "ferryman", "You paid. I did not say the coin was yours.", secondLatinFont},
	{"bw-03", "wren", "There is a second ferry. There is always a second ferry.", secondLatinFont},
	{"bw-04", "wren", "So the story goes. Stories are cheaper than coins.", secondLatinFont},
	{"bw-05", "ferryman", "Вода помнит то, что люди прячут.", secondForeignFont},
	{"bw-06", "ferryman", "This is not a toll. This is a habit.", secondLatinFont},
}

// GenerateSecondGameSamples builds the second set's ground truth and script
// profile (ADR-0079). The sample matrix: every line in its own font at every
// capture resolution (instant-reveal schedule), the analog-noisy variant of
// every Latin line at 4:3 (the foreign line's noise variant would just add
// unreadable noise to already-unreadable glyphs), and three empty boxes
// (precision coverage: the box is there, there is nothing to say).
func GenerateSecondGameSamples(seedBase uint64) (*TruthSet, *ScriptProfile, error) {
	if seedBase == 0 {
		seedBase = secondSetSeedBase
	}
	prof := &ScriptProfile{
		SchemaVersion: SchemaVersion,
		ID:            secondSetID,
		Origin:        "generated_synthetic",
		Description:   "Synthetic benchmark script for the structurally different second game (ADR-0079): the invented Black Water dialogue, upper-left square-cornered box, instant page reveal, one non-Latin line. No real game content.",
		Scene:         SceneID,
		Layout:        secondSetLayout,
		Regions:       []Region{secondSetRegion},
	}
	for _, l := range secondGameLines {
		prof.Messages = append(prof.Messages, ScriptLine{ID: l.id, Text: l.text, Speaker: l.speaker})
	}
	ts := &TruthSet{
		SchemaVersion: SchemaVersion,
		Set:           prof.ID,
		Origin:        "generated_synthetic",
		SeedBase:      seedBase,
		ProfileFile:   "profile.json",
	}
	n := 0
	add := func(text string, msgID *string, speaker, kind string, intended bool, font string, w, h int, noise float64) {
		n++
		id := fmt.Sprintf("bn%04d", n)
		ts.Samples = append(ts.Samples, Sample{
			ID:             id,
			Frames:         instantFrameSchedule(),
			SourceText:     text,
			MessageID:      msgID,
			Speaker:        speaker,
			TextKind:       kind,
			Scene:          SceneID,
			IntendedSpeech: intended,
			Split:          SplitForID(id),
			Variant:        Variant{Font: font, Resolution: fmt.Sprintf("%dx%d", w, h), Noise: noise},
		})
	}
	for _, l := range secondGameLines {
		for _, r := range Resolutions {
			add(l.text, &l.id, l.speaker, "dialogue", true, l.font, r.W, r.H, 0)
		}
	}
	for _, l := range secondGameLines { // analog-noisy Latin lines at 4:3
		if l.font != secondLatinFont {
			continue
		}
		add(l.text, &l.id, l.speaker, "dialogue", true, l.font, 640, 480, 1)
	}
	for i := 0; i < 3; i++ { // empty boxes: present box, nothing to read
		r := Resolutions[i%len(Resolutions)]
		add("", nil, SpeakerUnknown, "empty", false, "", r.W, r.H, 0)
	}
	if err := ts.Validate(); err != nil {
		return nil, nil, fmt.Errorf("bench: generated second set failed validation: %v", err)
	}
	return ts, prof, nil
}
