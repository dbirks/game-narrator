package runtime

// The unmatched read-through's text half (ADR-0136): the unit tests for the
// untrusted-game-content path — cleanReadThroughText joins the OCR line
// breaks, drops the control characters, trims the box-edge glyph noise and
// bounds the TTS text length (docs/spec/14 "Treat OCR text as hostile
// string", "Bound … TTS text length"). These are internal-package tests:
// the functions are unexported, and the text that reaches the TTS request
// is the very string under test, so the assertions run on the same package
// that will send it.

import (
	"strings"
	"testing"
)

func TestCleanReadThroughTextJoinsLinesAndDropsControlChars(t *testing.T) {
	in := "The wild pigs are\nhungry today.\u0007 More noise follows"
	got := cleanReadThroughText(in)
	if want := "The wild pigs are hungry today. More noise follows"; got != want {
		t.Fatalf("clean = %q, want %q", got, want)
	}
}

func TestCleanReadThroughTextTrimsEdgeNoise(t *testing.T) {
	// The box-edge glyph noise (ADR-0115's trim): the border and the speaker
	// icon reads at the edges, with the line itself intact inside them.
	in := "4 $o? The wild pigs are hungry today. o? 4"
	got := cleanReadThroughText(in)
	if got != "The wild pigs are hungry today." {
		t.Fatalf("clean = %q, want the line with the edge noise trimmed", got)
	}
}

func TestCleanReadThroughTextAllNoiseIsSilent(t *testing.T) {
	// A read that is all glyph noise after the trim has nothing to say: the
	// empty answer is the silent one (the caller logs it). The inputs are
	// the trim's own shape (ADR-0115): a read the trim cannot eat (it must
	// never eat the line — one token survives the trim, digits included)
	// that still carries no letter or digit is the silent one.
	for _, in := range []string{"……", "----", ""} {
		if got := cleanReadThroughText(in); got != "" {
			t.Fatalf("clean(%q) = %q, want the empty (silent) answer", in, got)
		}
	}
}

func TestCleanReadThroughTextCapsAtTheWordBoundary(t *testing.T) {
	// The TTS length bound (docs/spec/14): beyond the cap the line is cut at
	// a word boundary — a shorter line, never a line ending mid-word.
	long := strings.Repeat("word ", 100) + "tail" // 504 runes
	got := cleanReadThroughText(long)
	if n := len([]rune(got)); n > unmatchedMaxRunes {
		t.Fatalf("clean length = %d runes, bound %d", n, unmatchedMaxRunes)
	}
	if !strings.HasSuffix(got, "word") && !strings.HasSuffix(got, "tail") {
		t.Fatalf("clean ends mid-word: %q", got[len(got)-12:])
	}
	if strings.HasSuffix(got, " ") {
		t.Fatalf("trailing space after the word-boundary cut: %q", got[len(got)-12:])
	}
}

func TestTruncateReadThroughUnbrokenRunLandsAtN(t *testing.T) {
	// No word boundary in reach (an unbroken run of glyphs): the cut lands at
	// n, the documented fallback.
	got := truncateReadThrough(strings.Repeat("x", 50), 10)
	if len(got) != 10 || got != strings.Repeat("x", 10) {
		t.Fatalf("truncate = %q (len %d), want the first %d runes", got, len(got), 10)
	}
}

func TestHasWordChar(t *testing.T) {
	for _, s := range []string{"the", "123", "a…"} {
		if !hasWordChar(s) {
			t.Fatalf("hasWordChar(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "……", "----"} {
		if hasWordChar(s) {
			t.Fatalf("hasWordChar(%q) = true, want false", s)
		}
	}
}
