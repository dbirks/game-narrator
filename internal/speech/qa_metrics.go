package speech

import (
	"strings"
	"unicode"
)

// ── Normalization and metrics (ADR 0061) ───────────────────────────────────
//
// The ASR endpoint normalizes spelling on its way out ("harbour" -> "harbor",
// verified 2026-10-18), so comparing raw text would fail a clip whose only
// divergence is the endpoint's own normalization. Both sides are therefore
// normalized before WER/CER: case, punctuation, whitespace, and a small
// British/American spelling map applied symmetrically.

// spellingMap canonicalizes the frequent British/American divergences the
// endpoint applies (and its own output drifts on). Every key maps to the
// canonical American spelling; both sides are mapped, so "colour" and
// "color" normalize to the same token whichever side the variant came from.
// It is deliberately small: an open thesaurus here would swallow real
// divergences the QA loop exists to catch.
var spellingMap = map[string]string{
	"harbour":     "harbor",
	"colour":      "color",
	"coloured":    "colored",
	"colourful":   "colorful",
	"favour":      "favor",
	"favourite":   "favorite",
	"behaviour":   "behavior",
	"labour":      "labor",
	"neighbour":   "neighbor",
	"grey":        "gray",
	"defence":     "defense",
	"offence":     "offense",
	"centre":      "center",
	"metre":       "meter",
	"litre":       "liter",
	"organise":    "organize",
	"organised":   "organized",
	"organising":  "organizing",
	"recognise":   "recognize",
	"recognised":  "recognized",
	"recognising": "recognizing",
	"realise":     "realize",
	"realised":    "realized",
	"realising":   "realizing",
	"travelled":   "traveled",
	"travelling":  "traveling",
	"jewellery":   "jewelry",
	"plough":      "plow",
	"defenceless": "defenseless",
}

// normalizeText reduces one text to its QA comparison form: lowercase, every
// non-letter/non-digit run collapsed to a single space (the endpoint drops
// punctuation entirely — a comma or period is a word separator, so it must
// never count as an edit), with the contraction apostrophe REMOVED (the
// endpoint turns "Let's" into "lets", not "let s"), and each word passed
// through spellingMap.
//
// (No NFKC fold: this Go build strips unicode/norm, and the endpoint's output
// is ASCII after its own normalization, so case + punctuation + whitespace +
// the spelling map cover the divergences that actually occur. A future
// deployment emitting full-width or ligature forms must re-tune the bound,
// ADR 0061.)
func normalizeText(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		case r == '\'':
			// Contraction glue: "Let's" -> "lets" (the endpoint's own form).
			// Removed, not spaced, so it does not split the word.
		default:
			b.WriteByte(' ')
		}
	}
	fields := strings.Fields(b.String())
	for i, f := range fields {
		if canon, ok := spellingMap[f]; ok {
			fields[i] = canon
		}
	}
	return strings.Join(fields, " ")
}

// levenshteinWords is the word-level edit distance: the classic DP distance
// (two rows, O(n*m)) over the word sequences. Utterances are one dialogue
// line, so the quadratic cost is a non-issue at QA frequency.
func levenshteinWords(a, b []string) int {
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := range a {
		cur[0] = i + 1
		for j := range b {
			cost := 1
			if a[i] == b[j] {
				cost = 0
			}
			m := prev[j] + cost // substitution (0 when equal)
			if cur[j]+1 < m {   // insertion into b
				m = cur[j] + 1
			}
			if prev[j+1]+1 < m { // deletion from a
				m = prev[j+1] + 1
			}
			cur[j+1] = m
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// levenshteinRunes is the character-level distance over the normalized text.
func levenshteinRunes(a, b []rune) int {
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := range a {
		cur[0] = i + 1
		for j := range b {
			cost := 1
			if a[i] == b[j] {
				cost = 0
			}
			m := prev[j] + cost
			if cur[j]+1 < m {
				m = cur[j] + 1
			}
			if prev[j+1]+1 < m {
				m = prev[j+1] + 1
			}
			cur[j+1] = m
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// wer is the word error rate on normalized text: word-sequence edit distance
// over the expected word count. An empty expected text is a caller error the
// QA loop guards upstream; it reports 0 (nothing to verify), not a
// division by zero.
func wer(expected, actual string) float64 {
	e := strings.Fields(expected)
	if len(e) == 0 {
		return 0
	}
	return float64(levenshteinWords(e, strings.Fields(actual))) / float64(len(e))
}

// cer is the character error rate on the normalized text: rune edit distance
// over the expected rune count.
func cer(expected, actual string) float64 {
	e := []rune(expected)
	if len(e) == 0 {
		return 0
	}
	return float64(levenshteinRunes(e, []rune(actual))) / float64(len(e))
}
