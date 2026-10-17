package ocr

import (
	"strings"
	"unicode"
)

// NormaliseForCompare is the comparison form the discrepancy measure works in
// (ADR-0039 §4). Two readings that say the same thing must compare equal even
// when their surfaces differ, so the form is case-folded (unicode-aware
// lowercasing) and stripped of everything that is not a letter or a digit:
// punctuation, quotes, the ellipsis a model appends, and all whitespace.
//
// It is deliberately lossier than internal/games' textnorm, which preserves
// punctuation for matching: here the question is "do the two readers see the
// same words", and punctuation is noise in that question. It is not a
// substitute for the raw read: Results keep the engines' verbatim text and
// only the discrepancy number is computed on this form.
//
// Known gap, documented rather than hidden: no NFKC compatibility fold is
// applied (this toolchain's stdlib has no unicode/normalize), so a reading in
// which a ligature or a fullwidth glyph was recognised as a distinct rune
// compares as different. Both engines in this product read Latin script from
// rendered pixels, where a ligature comes back as its two letters or not at
// all; the ADR records the residual risk and the fix (vendor a small
// compatibility table) if a live profile shows it matters.
func NormaliseForCompare(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// normalizedDistance is the normalised Levenshtein distance between two
// NormaliseForCompare forms: the minimum edit script (insert, delete,
// substitute, each costing 1) divided by the length of the longer input, in
// [0,1]. 0 means the readers agree on every character; 1 means they share
// nothing. Empty-versus-non-empty is 1 by that ratio, which is why the
// caller (FallbackEngine.decide) reports 0 instead when one side is empty —
// an empty reading is "no comparison possible", not "total disagreement",
// and conflating the two would make an empty VLM answer look like the
// strongest disagreement the fallback has ever seen.
func normalizedDistance(a, b string) float64 {
	ra, rb := []rune(a), []rune(b)
	la, lb := len(ra), len(rb)
	if la == 0 && lb == 0 {
		return 0
	}
	if la == 0 || lb == 0 {
		return 1
	}
	// One-row Levenshtein: the crop is a textbox line, so the inputs are tens
	// of runes, not paragraphs, and O(n·m) with a single rolling row is both
	// fast and allocation-bounded.
	prev := make([]int, lb+1)
	cur := make([]int, lb+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		cur[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min3(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return float64(prev[lb]) / float64(max(la, lb))
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}
