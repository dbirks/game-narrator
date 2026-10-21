package runtime

// The read-through plausibility gate's tests (ADR-0136, amendment
// "plausibility gate"): the evidence table — every string the board actually
// read out loud in the live session with no dialogue box on screen is
// rejected — and the real-dialogue half of the tuning — short plain lines
// the gate must keep passing, ellipses and exclamation marks included. The
// strings are the gate's input: the cleaned text exactly as it reached the
// TTS request.

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// boardSpokenJunk is the amendment's evidence table: the OCR noise the
// read-through SPOKE on the board in the live 2026-10-21 session (Link
// sailing, no dialogue box on screen; the text-presence check misfired and
// the crop read the watermark, the border and the sprite corner). Every one
// of these strings reached the TTS request and must now be rejected.
var boardSpokenJunk = []string{
	"™ a a",
	"a",
	"™ a v",
	"i.",
	"j",
	"A",
	"Ww",
	"v™.",
	"@1",
	"“A",
	"4",
	"® ub, ok eww, nol.",
	"™m™ a ry",
	"- r- ra x",
	"gh\"",
	"oo. y mary.",
	"> ™ ve ™ 1 mey.",
	"yyy",
	"wy, ¥",
	"\\ at 3",
}

// realReadThroughLines is the dialogue half of the tuning: plain, short,
// exactly the shape the box's OCR read gives when a line really is on
// screen — the gate keeps these speaking.
var realReadThroughLines = []string{
	"Well, then?",
	"Hey! Over here!",
	"You found it!",
	"Hmm... I wonder.",
	"Thank you so much",
}

func TestReadThroughGateRejectsBoardSpokenJunk(t *testing.T) {
	for _, s := range boardSpokenJunk {
		if isReadThroughPlausible(s) {
			t.Errorf("the gate accepts the board-spoken junk %q", s)
		}
	}
}

func TestReadThroughGateKeepsRealDialogue(t *testing.T) {
	for _, s := range realReadThroughLines {
		if !isReadThroughPlausible(s) {
			t.Errorf("the gate rejects the real line %q", s)
		}
	}
}

// opensOnNonDialogueSymbol reports whether any of s's tokens opens on a
// non-dialogue symbol: the rule-1 measurement the bucket test runs against
// its evidence shapes.
func opensOnNonDialogueSymbol(s string) bool {
	for _, tok := range strings.Fields(s) {
		if r, _ := utf8.DecodeRuneInString(tok); isNonDialogueSymbol(r) {
			return true
		}
	}
	return false
}

// TestReadThroughGateRuleBuckets pins which rule owns each rejection, so a
// later tuning cannot silently move a junk shape from one rule's coverage
// into a gap: the symbol at the token start, the repeated-letter run, the
// alphabetic share and the word-core substance are distinct teeth. Each
// bucket's shapes are the evidence strings the earlier rules provably do
// not own, so the owning rule is the one keeping them silent.
func TestReadThroughGateRuleBuckets(t *testing.T) {
	// Rule 1 — a token that opens on a non-dialogue symbol.
	for _, s := range []string{"™ a a", "® ub, ok eww, nol.", "> ™ ve ™ 1 mey.", "wy, ¥"} {
		if !opensOnNonDialogueSymbol(s) {
			t.Errorf("test setup: %q carries no token that opens on a symbol", s)
		}
		if isReadThroughPlausible(s) {
			t.Errorf("the symbol rule missed %q", s)
		}
	}

	// Rule 2 — the repeated-letter run, case folded, the "yyy" shape.
	if !hasRepeatedLetterRun("yyy") || !hasRepeatedLetterRun("YyY") {
		t.Error("hasRepeatedLetterRun: the evidence shape (case folded) is not a run")
	}
	if hasRepeatedLetterRun("Well, then?") {
		t.Error("hasRepeatedLetterRun: a real line is not a run")
	}

	// Rule 3 — the alphabetic share. The shapes the symbol and run rules do
	// not own and whose share sits below the floor.
	for _, s := range []string{"v™.", "i.", "“A", "- r- ra x", "gh\"", "\\ at 3"} {
		if opensOnNonDialogueSymbol(s) {
			t.Errorf("test setup: %q is owned by rule 1, not the share", s)
		}
		if hasRepeatedLetterRun(s) {
			t.Errorf("test setup: %q is owned by rule 2, not the share", s)
		}
		if a := alphaShare(s); a >= readthroughMinAlphaShare {
			t.Errorf("test setup: alphaShare(%q) = %v, want below the floor %v", s, a, readthroughMinAlphaShare)
		}
		if isReadThroughPlausible(s) {
			t.Errorf("the share rule missed %q", s)
		}
	}
	if a := alphaShare("Hmm... I wonder."); a < readthroughMinAlphaShare {
		t.Errorf("the share rule eats the real line: alphaShare = %v, want at least %v",
			a, readthroughMinAlphaShare)
	}

	// Rule 4 — the word-core substance: no symbol, no run, share at the
	// floor, but the cores are too short (not two of 3 or one of 5). The
	// wiring's word-character floor catches the single-letter shapes first
	// in the live path; the gate keeps its own tooth independent of it.
	for _, s := range []string{"a", "j", "A", "Ww", "oo. y mary."} {
		if opensOnNonDialogueSymbol(s) {
			t.Errorf("test setup: %q is owned by rule 1, not the substance", s)
		}
		if hasRepeatedLetterRun(s) {
			t.Errorf("test setup: %q is owned by rule 2, not the substance", s)
		}
		if a := alphaShare(s); a < readthroughMinAlphaShare {
			t.Errorf("test setup: alphaShare(%q) = %v, want at least the floor %v", s, a, readthroughMinAlphaShare)
		}
		if isReadThroughPlausible(s) {
			t.Errorf("the substance rule missed %q", s)
		}
	}
}

func TestWordCoreStripsEdgePunctuation(t *testing.T) {
	cases := map[string]string{
		"wonder.": "wonder",
		"mary.":   "mary",
		"eww,":    "eww",
		"He's":    "He's",
		"“A":      "A",
		"™m™":     "m",
		"i.":      "i",
		"…":       "",
		"":        "",
	}
	for in, want := range cases {
		if got := string(wordCore(in)); got != want {
			t.Errorf("wordCore(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHasWordCharAndWordCharCount(t *testing.T) {
	for _, s := range []string{"the", "123", "a…"} {
		if !hasWordChar(s) {
			t.Errorf("hasWordChar(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "……", "----"} {
		if hasWordChar(s) {
			t.Errorf("hasWordChar(%q) = true, want false", s)
		}
	}
	if n := wordCharCount("The wild"); n != 7 {
		t.Errorf("wordCharCount = %d, want 7", n)
	}
	if n := wordCharCount("4 $o?"); n != 2 {
		t.Errorf("wordCharCount = %d, want 2", n)
	}
}
