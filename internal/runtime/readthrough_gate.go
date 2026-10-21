package runtime

// The read-through plausibility gate (ADR-0136, amendment "plausibility
// gate"): the text-plausibility half of the unmatched read-through. The
// confidence floor gates the *read*; this gate gates the *text* — a settled
// read of a screen with no dialogue box on it is still noise the moment the
// crop lands on the watermark, the border, or the corner of the sprite, and
// the ADR's original trim + 4-word-char floor let the noise speak.
//
// The rules are designed from the board's own spoken junk (the amendment's
// evidence table, a live session with no dialogue box up): trademark and
// registered-mark glyphs, currency and math symbols, one- or two-letter
// fragments, symbol-heavy text, and runs of one repeated letter. Every rule
// is one-sided in the direction the evidence demands — a rule that rejects
// junk may also reject a legitimate line (silence is the ADR's historic
// answer, and the settings switch stays the operator's way back); a rule
// that speaks junk does not.

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// readthroughMinAlphaShare is the minimum share of alphabetic characters
	// among the text's non-space characters: a dialogue line is letters with
	// punctuation at the edges, while a junk read is symbols, marks and
	// fragments. The floor sits just below the cleanest real line the gate
	// must keep — "Hmm... I wonder." (10 letters of 14 non-space characters,
	// 0.714): the ellipsis is dialogue's own texture, so the floor clears it
	// while the symbol-heavy evidence reads (0.33–0.67) stay rejected.
	readthroughMinAlphaShare = 0.70

	// readthroughRepeatRunLen is the run of one repeated letter (case
	// folded) that disqualifies the text: Tesseract's watermark and border
	// reads repeat a single glyph ("yyy"), and no dialogue line does.
	readthroughRepeatRunLen = 3
)

// isReadThroughPlausible reports whether s (the read-through text after
// cleanReadThroughText's trim) plausibly is a dialogue line. It is the
// gate's whole verdict — the caller (maybeUnmatchedReadThrough) treats a
// rejection exactly like the empty-after-trim answer: silent, logged, and
// named readthrough_implausible in the log.
//
// The rules, in rejection order:
//   - a token that STARTS with a non-dialogue symbol (trademark /
//     registered / copyright marks, currency or math symbols — the ™, ®, ©,
//     ¥, > and \ shapes of the evidence table);
//   - a run of one repeated letter of readthroughRepeatRunLen or more;
//   - an alphabetic share of the non-space characters below
//     readthroughMinAlphaShare;
//   - no alphabetic substance: the text needs two alphabetic word cores of
//     at least 3 runes each, or one of at least 5 (the word core strips
//     the punctuation the line carries, so "wonder." is still the 6-letter
//     word the screen drew).
func isReadThroughPlausible(s string) bool {
	tokens := strings.Fields(s)

	// Rule 1: the non-dialogue symbol at a token start. A word the screen
	// drew as dialogue begins with a letter (or, at best, the apostrophe of
	// a contraction); a token that opens on a mark glyph is the watermark,
	// the border or the sprite edge, not the line.
	for _, tok := range tokens {
		if r, _ := utf8.DecodeRuneInString(tok); isNonDialogueSymbol(r) {
			return false
		}
	}

	// Rule 2: the repeated-letter run, case folded — the "yyy" shape.
	if hasRepeatedLetterRun(s) {
		return false
	}

	// Rule 3: the alphabetic share of the non-space characters.
	if alphaShare(s) < readthroughMinAlphaShare {
		return false
	}

	// Rule 4: the alphabetic substance — two word cores of at least 3 runes
	// or one of at least 5. A core is the token's inner run of word runes
	// (letters and the apostrophes the contractions carry): the punctuation
	// a line wears at its edges never counts, and neither does a mark
	// glued into the middle of a fragment.
	c3, c5 := 0, 0
	for _, tok := range tokens {
		core := wordCore(tok)
		if core == nil {
			continue
		}
		if n := len(core); n >= 5 { // core is a []rune: len is the rune count
			c5++
		} else if n >= 3 {
			c3++
		}
	}
	return c3 >= 2 || c5 >= 1
}

// alphaShare is the text's alphabetic characters over its non-space
// characters (0 for a text with no non-space character at all): the rule-3
// measurement — a dialogue line is letters with punctuation at the edges,
// and a junk read is symbols, marks and fragments.
func alphaShare(s string) float64 {
	letters, nonSpace := 0, 0
	for _, r := range s {
		if unicode.IsSpace(r) {
			continue
		}
		nonSpace++
		if unicode.IsLetter(r) {
			letters++
		}
	}
	if nonSpace == 0 {
		return 0
	}
	return float64(letters) / float64(nonSpace)
}

// isNonDialogueSymbol reports whether r is a symbol a dialogue line never
// opens with: the "other symbol" class (the trademark, registered and
// copyright marks), the currency symbols, or the math symbols (the > and \
// shapes the crop picks up from the frame border).
func isNonDialogueSymbol(r rune) bool {
	return unicode.Is(unicode.So, r) ||
		unicode.Is(unicode.Sc, r) ||
		unicode.Is(unicode.Sm, r)
}

// hasRepeatedLetterRun reports whether s carries a run of the same letter
// (case folded) of readthroughRepeatRunLen or more, anywhere.
func hasRepeatedLetterRun(s string) bool {
	var prev rune // the lower-cased previous letter, 0 when there is none
	run := 0
	for _, r := range s {
		if !unicode.IsLetter(r) {
			prev, run = 0, 0
			continue
		}
		if l := unicode.ToLower(r); l == prev {
			run++
		} else {
			prev, run = l, 1
		}
		if run >= readthroughRepeatRunLen {
			return true
		}
	}
	return false
}

// wordCore is the token's inner run of word runes (letters and the
// apostrophes of the contractions), or nil when the token carries none.
// "wonder." keeps its word, "wonder™" keeps none, and a token that is pure
// punctuation (the "…" and "—" the trim left behind) is not a word at all.
func wordCore(tok string) []rune {
	rs := []rune(tok)
	lo, hi := -1, -1
	for i, r := range rs {
		if isWordRune(r) {
			lo = i
			break
		}
	}
	for i := len(rs) - 1; i >= 0; i-- {
		if isWordRune(rs[i]) {
			hi = i
			break
		}
	}
	if lo < 0 {
		return nil
	}
	for i := lo; i <= hi; i++ {
		if !isWordRune(rs[i]) {
			return nil
		}
	}
	return rs[lo : hi+1]
}

// isWordRune is the letter-or-apostrophe test the word core is built from:
// the ASCII apostrophe and the typographic right single quote (the one the
// text machine draws for the contractions).
func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || r == '\'' || r == '\u2019'
}

// hasWordChar reports whether s carries a letter or a digit: the test that
// separates "a line with noise at its edges" from "noise".
func hasWordChar(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// wordCharCount counts the letters and digits in s: an unmatched read-through
// requires at least 4 word characters so a 1–3 letter OCR noise fragment from
// an unoccupied region or the first frame of a revealing box does not speak.
func wordCharCount(s string) int {
	n := 0
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			n++
		}
	}
	return n
}
