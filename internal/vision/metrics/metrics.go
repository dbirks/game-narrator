// Package metrics scores how well a hypothesis matches ground truth.
//
// docs/spec/12 "Benchmarks and ground truth" names CER and WER as the reported
// recognition measures, and requires both sides to go through the same
// normalization the matcher uses — comparing raw OCR text to a script string
// measures the normalizer as well as the OCR, and normalizer changes would then
// look like accuracy changes.
//
// The rates are ratios, so they can exceed 1: an insertion-only error in a
// one-character reference is 100% by definition. Nothing here clamps, because a
// clamped metric hides the worst cases.
package metrics

import (
	"strings"
	"unicode/utf8"

	"github.com/dbirks/game-narrator/internal/games/textnorm"
)

// CER is character error rate: edit distance over runes divided by the reference
// length, both strings folded first by textnorm.Normalize with default options.
//
// Runes, not bytes: a combining sequence or a CJK glyph is one character to a
// reader, and byte distance would report a nonsense rate for exactly the scripts
// docs/spec/12 requires coverage of ("non-English script").
//
// Convention for the empty reference, which OCR produces constantly on an empty
// textbox and which a naive divide-by-zero would answer with NaN:
//
//	ref empty, hyp empty   → 0   (nothing was said, correctly)
//	ref empty, hyp non-empty → 1 (the whole hypothesis is spurious)
//
// The 1 is a decision, not a measurement, and is exported as EmptyReferenceRate
// so a benchmark that needs to distinguish them can.
func CER(ref, hyp string) float64 {
	return errorRate(ref, hyp, splitRunes)
}

// WER is word error rate over whitespace-separated words of the same folded
// strings. It is the number docs/spec/12 reports alongside CER; it is kinder to
// an OCR engine that mangles one letter in a long word and harsher on spacing
// errors, which is why both are reported.
func WER(ref, hyp string) float64 {
	return errorRate(ref, hyp, splitWords)
}

// EmptyReferenceRate is CER's answer to "there was no text to read" when the
// hypothesis was not empty.
const EmptyReferenceRate = 1.0

// fold applies the same normalization the retrieval key uses. Options{} — the
// import-time setting — deliberately keeps punctuation, because "Wait." and
// "Wait!" are different lines and an error rate that calls them equal is
// measuring the wrong thing. OCRFold is off for the same reason: the metric
// scores the engine, and folding 0→o would score away a real confusion.
func fold(s string) string { return textnorm.Normalize(s, textnorm.Options{}) }

func errorRate(ref, hyp string, split func(string) []string) float64 {
	r, h := split(fold(ref)), split(fold(hyp))
	d := Distance(r, h)
	if len(r) == 0 {
		if len(h) == 0 {
			return 0
		}
		return EmptyReferenceRate
	}
	return float64(d) / float64(len(r))
}

func splitRunes(s string) []string {
	out := make([]string, 0, utf8.RuneCountInString(s))
	for _, r := range s {
		out = append(out, string(r))
	}
	return out
}

func splitWords(s string) []string {
	return strings.Fields(s)
}

// Distance is Levenshtein distance over a sequence of comparable units, with a
// two-row rolling window so a long page costs length, not length squared.
//
// docs/spec/05 warns "Levenshtein alone is not adequate" for *candidate
// retrieval* — that is a statement about ranking ambiguity on short repeated
// strings, not about scoring a labeled transcription. For a benchmark against
// ground truth it is the standard measure, and it is what makes the CER number
// comparable with other systems'.
func Distance[T comparable](a, b []T) int {
	n, m := len(a), len(b)
	if n == 0 {
		return m
	}
	if m == 0 {
		return n
	}
	prev := make([]int, m+1)
	cur := make([]int, m+1)
	for j := 0; j <= m; j++ {
		prev[j] = j
	}
	for i := 1; i <= n; i++ {
		cur[0] = i
		for j := 1; j <= m; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			best := prev[j-1] + cost
			if v := prev[j] + 1; v < best {
				best = v
			}
			if v := cur[j-1] + 1; v < best {
				best = v
			}
			cur[j] = best
		}
		prev, cur = cur, prev
	}
	return prev[m]
}
