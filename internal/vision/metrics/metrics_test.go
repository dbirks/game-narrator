package metrics

import (
	"math"
	"strings"
	"testing"
)

func TestCER(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ref, hyp string
		want     float64
	}{
		{"identical", "The old lighthouse keeps no flame.", "The old lighthouse keeps no flame.", 0},
		{"both empty", "", "", 0},
		{"empty ref nonempty hyp", "", "anything", EmptyReferenceRate},
		{"one substitution", "cat", "cot", 1.0 / 3},
		{"one deletion", "cat", "ct", 1.0 / 3},
		{"one insertion", "cat", "cart", 1.0 / 3},
		{"complete miss", "cat", "dog", 1},
		// Whitespace and case are folded the same way retrieval folds them, so a
		// line-wrap difference is not an accuracy difference (docs/spec/12 scores
		// the OCR, not the OCR plus a formatter).
		{"line wrap", "the keeper\nswore", "the keeper swore", 0},
		{"case", "Bring The Chart", "bring the chart", 0},
		{"trailing space", "hello ", "hello", 0},
		// Punctuation is kept by default options, so these are real errors:
		{"dropped apostrophe", "it wasn't the captain", "it wasnt the captain", 1.0 / 21},
		{"dropped full stop", "Say when.", "Say when", 1.0 / 9},
		// Unicode: distance is per rune, so one accent is one error, not two bytes.
		{"accent", "L'épée du vieux phare", "L'epée du vieux phare", 1.0 / 21},
		{"cjk one glyph", "はじめまして", "はじめまてて", 1.0 / 6},
		{"cjk all wrong", "はじめまして", "こんにちは", 1},
		{"emoji", "👋 hello", "👋 helo", 1.0 / 7},
		// Insertions can push the ratio above 1; clamping would hide that.
		{"more text than reference", "cat", "catastrophe", 8.0 / 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CER(tc.ref, tc.hyp)
			if math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("CER(%q, %q) = %.6f, want %.6f", tc.ref, tc.hyp, got, tc.want)
			}
		})
	}
}

func TestWER(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ref, hyp string
		want     float64
	}{
		{"identical", "the old lighthouse keeps", "the old lighthouse keeps", 0},
		{"one wrong word", "the old lighthouse keeps", "the old lampkeep keeps", 0.25},
		{"one extra word", "the old lighthouse keeps", "the old lighthouse always keeps", 0.25},
		{"one missing word", "the old lighthouse keeps", "the lighthouse keeps", 0.25},
		// The distinction from CER that makes both worth reporting: one wrong
		// letter makes a word "wrong" to WER (1.0 on a one-word reference) while CER
		// scores it at 1/11. Losing a letter and losing a line are different failures
		// for a narrator, and docs/spec/12 asks for both numbers.
		{"letter slip in a long word", "waterlogged", "wateriogged", 1},
		{"both empty", "", "", 0},
		{"empty ref", "", "anything", EmptyReferenceRate},
		{"newlines are spaces", "one\ntwo three", "one two three", 0},
		{"all wrong", "a b c d", "x y z w", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := WER(tc.ref, tc.hyp); math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("WER(%q, %q) = %.6f, want %.6f", tc.ref, tc.hyp, got, tc.want)
			}
		})
	}
}

func TestDistance(t *testing.T) {
	cases := []struct {
		a, b []rune
		want int
	}{
		{nil, nil, 0},
		{[]rune("kitten"), []rune("sitting"), 3},
		{[]rune("flaw"), []rune("lawn"), 2},
		{[]rune("same"), []rune("same"), 0},
		{[]rune(""), []rune("abc"), 3},
		{[]rune("abcdef"), []rune(""), 6},
		// Symmetry is a property a buggy rolling array often loses.
		{[]rune("Saturday"), []rune("Sunday"), 3},
	}
	for _, c := range cases {
		if got := Distance(c.a, c.b); got != c.want {
			t.Errorf("Distance(%q, %q) = %d, want %d", string(c.a), string(c.b), got, c.want)
		}
		if got, rev := Distance(c.a, c.b), Distance(c.b, c.a); got != rev {
			t.Errorf("Distance is not symmetric for %q/%q: %d vs %d", string(c.a), string(c.b), got, rev)
		}
	}
}

// TestFoldMatchesRetrieval pins the claim in the package doc: the metric's fold is
// textnorm's *import-time* fold, so a CER that improves because punctuation was
// stripped cannot be mistaken for an engine improvement.
func TestFoldMatchesRetrieval(t *testing.T) {
	if folded := fold("Wait… he said “Don't.”"); !strings.Contains(folded, "don't") {
		t.Errorf("fold removed punctuation that the default Options keep: %q", folded)
	}
	if folded := fold("M0BILE"); strings.Contains(folded, "mobile") {
		t.Errorf("fold applied OCRFold; a 0→o confusion would stop being an error: %q", folded)
	}
}

// TestCERUsesFoldedInput checks that the rate is computed after folding rather than
// being handed raw strings — the failing case is a metric that scores the
// normalizer as well as the engine.
func TestCERUsesFoldedInput(t *testing.T) {
	if got := CER("  THE   LIGHT  ", "the light"); got != 0 {
		t.Errorf("CER = %v, want 0: folding must happen before comparison", got)
	}
}
