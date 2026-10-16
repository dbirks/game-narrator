package textnorm

import (
	"fmt"
	"unicode/utf8"
)

// Fullwidth ASCII forms U+FF01–U+FF5E are a fixed offset from U+0021–U+007E.
// Japanese game scripts are full of them (a "！" is one keystroke on a Japanese
// keyboard), and OCR reports them for ASCII glyphs it renders the same way, so
// this is the single highest-value compatibility fold and it is arithmetic, not
// a table.
const (
	fullwidthFirst  = 0xFF01
	fullwidthLast   = 0xFF5E
	fullwidthOffset = 0xFF01 - 0x0021
)

// compatFold holds the one-to-one and one-to-many rewrites applied before case
// folding. Every value is either empty (delete the character) or made of runes
// that are *not themselves keys*, which is what makes this stage idempotent.
//
// Scope rule for this table: a character belongs here when it is (a) common in
// game scripts or in OCR of game text, and (b) equivalent for matching purposes
// to a plainer character. Anything that carries meaning — a combining accent, a
// CJK punctuation mark a Japanese reader would notice — stays.
var compatFold = map[rune]string{
	// Invisible. A BOM or a soft hyphen inside a line must not defeat a match,
	// and neither is ever displayed, so they are removed rather than mapped.
	0xFEFF: "", // ZERO WIDTH NO-BREAK SPACE (a stray BOM mid-string)
	0x200B: "", // ZERO WIDTH SPACE
	0x00AD: "", // SOFT HYPHEN
	// The zero-width joiner and non-joiner are kept: they are load-bearing in
	// emoji sequences and in Indic shaping, and dropping them would splice two
	// emoji into one.

	// Typographic punctuation to ASCII. docs/spec/02 lists punctuation among the
	// things normalized_text handles; an apostrophe that differs by codepoint
	// between the script file and the on-screen font is the most common single
	// cause of a failed exact match.
	0x2018: "'", // LEFT SINGLE QUOTATION MARK
	0x2019: "'", // RIGHT SINGLE QUOTATION MARK
	0x201A: "'", // SINGLE LOW-9 QUOTATION MARK
	0x201B: "'", // HIGH REVERSED SINGLE QUOTATION MARK
	0x2039: "'", // SINGLE LEFT-POINTING ANGLE QUOTATION MARK
	0x203A: "'", // SINGLE RIGHT-POINTING ANGLE QUOTATION MARK
	0x02BC: "'", // MODIFIER LETTER APOSTROPHE (OCR's reading of ’)
	0x02B9: "'", // MODIFIER LETTER PRIME
	0x201C: `"`, // LEFT DOUBLE QUOTATION MARK
	0x201D: `"`, // RIGHT DOUBLE QUOTATION MARK
	0x201E: `"`, // DOUBLE LOW-9 QUOTATION MARK
	0x201F: `"`, // HIGH REVERSED DOUBLE QUOTATION MARK
	0x00AB: `"`, // LEFT-POINTING DOUBLE ANGLE QUOTATION MARK
	0x00BB: `"`, // RIGHT-POINTING DOUBLE ANGLE QUOTATION MARK
	0x2033: `"`, // DOUBLE PRIME

	// Dashes of every length to the hyphen-minus. An en or em dash is a style
	// choice of the localisation, not a different word.
	0x2010: "-", // HYPHEN
	0x2011: "-", // NON-BREAKING HYPHEN
	0x2012: "-", // FIGURE DASH
	0x2013: "-", // EN DASH
	0x2014: "-", // EM DASH
	0x2015: "-", // HORIZONTAL BAR
	0x2043: "-", // HYPHEN BULLET
	0x2212: "-", // MINUS SIGN (OCR's reading of a hyphen)
	0x2E3A: "-", // TWO-EM DASH
	0x2E3B: "-", // THREE-EM DASH

	// Ellipsis to three dots: the sequence the script file usually contains and
	// always what a plain-text editor produces.
	0x2026: "...", // HORIZONTAL ELLIPSIS
	0x22EF: "...", // MIDLINE HORIZONTAL ELLIPSIS
	0xFE19: "...", // PRESENTATION FORM FOR VERTICAL HORIZONTAL ELLIPSIS

	// Ligatures that survive in a typeset script dump.
	0xFB00: "ff",
	0xFB01: "fi",
	0xFB02: "fl",
	0xFB03: "ffi",
	0xFB04: "ffl",
	0xFB05: "ft", // long s + t, folded to the modern reading
	0xFB06: "st",
	0x0132: "ij", // LATIN CAPITAL LIGATURE IJ (ToLower gives the small form)
	0x0133: "ij",
	0x00C6: "ae", // LATIN CAPITAL LIGATURE AE
	0x00E6: "ae",
	0x0152: "oe", // LATIN CAPITAL LIGATURE OE
	0x0153: "oe",
	0x00DF: "ss", // LATIN SMALL LETTER SHARP S

	// Units and common symbols a textbox can carry, folded only where the
	// meaning is unambiguous for matching.
	0x00A0: " ", // NO-BREAK SPACE (collapseSpaces would map it anyway; naming it
	//                    here documents that NBSP is deliberately not content)
}

// The halfwidth katakana block U+FF61-U+FF9F is the form Shift-JIS game data and
// OCR produce for kana. katakanaHalf lists the block in code point order and
// katakanaFull its NFKC target, one rune each. There is no arithmetic shortcut:
// the full-width forms are not evenly spaced, because the small kana (ァ ィ ゥ …)
// occupy code points between the full ones, so a constant offset silently maps
// ﾅ to ザ instead of ナ. That is exactly the kind of wrong-but-quiet bug a table
// plus a length check avoids.
//
// The values are NFKC's, which means ｧ folds to the small ァ (U+30A1) rather than
// to ア, and ﾞ folds to the combining voiced mark U+3099. NFKC would also compose
// カ+゙ into ガ; this package does not, which is the limitation named in the
// package doc and in NotFoldedEquivalents.
//
// init panics if the two strings ever differ in length, because a silent
// off-by-one in a hand-maintained pair like this would misfold every kana after
// it — a wrong answer, not a crash, which is worse. TestHalfwidthKanaIsNFKC
// rechecks every entry against the algorithm's intent.
const (
	katakanaHalf = "｡｢｣､･ｦｧｨｩｪｫｬｭｮｯｰｱｲｳｴｵｶｷｸｹｺｻｼｽｾｿﾀﾁﾂﾃﾄﾅﾆﾇﾈﾉﾊﾋﾌﾍﾎﾏﾐﾑﾒﾓﾔﾕﾖﾗﾘﾙﾚﾛﾜﾝﾞﾟ"
	katakanaFull = "。「」、・ヲァィゥェォャュョッーアイウエオカキクケコサシスセソタチツテトナニヌネノハヒフヘホマミムメモヤユヨラリルレロワン゙゚"
)

func init() {
	half := []rune(katakanaHalf)
	full := []rune(katakanaFull)
	if len(half) != len(full) {
		panic("textnorm: halfwidth katakana tables differ in length")
	}
	for i, r := range half {
		if _, ok := compatFold[r]; ok {
			panic("textnorm: halfwidth katakana key collides with an existing fold")
		}
		compatFold[r] = string(full[i])
	}
	// The halfwidth space is a space, not a kana.
	compatFold[0xFF60] = " "
}

// Equivalence documents one folded character class. The dev UI's "why did this
// candidate match" panel and the diagnostic report show these, so the reason a
// match succeeded against visibly different text is legible to a curator instead
// of being folklore (docs/spec/12 "developer inspector").
type Equivalence struct {
	From string `json:"from"`
	To   string `json:"to"`
	Note string `json:"note,omitempty"`
}

// FoldedEquivalents lists what this package treats as the same character.
var FoldedEquivalents = []Equivalence{
	{From: "‘ ’ ‛", To: "'"},
	{From: `“ ” „`, To: `"`},
	{From: "– — ― −", To: "-"},
	{From: "…", To: "..."},
	{From: "！-～ (fullwidth ASCII)", To: "!-~"},
	{From: "ｱ-ﾳ (halfwidth katakana)", To: "ア-ン"},
	{From: "ﬁ ﬀ æ œ ß", To: "fi ff ae oe ss"},
	{From: "U+00A0 U+3000 U+FEFF line breaks", To: "one space"},
	{From: "uppercase", To: "lowercase (simple case mapping)"},
}

// NotFoldedEquivalents lists the distinctions this package deliberately keeps,
// so a curator chasing a missed match can find the reason here. Each entry is
// unfixable without full NFC/NFKC or a language-specific fold, both of which are
// ADR decisions (see the package doc).
var NotFoldedEquivalents = []string{
	"ς ≠ σ (Greek final versus medial sigma: not a SimpleFold pair reachable from strings.ToLower)",
	"ё ≠ е (Cyrillic yo versus ie: distinct codepoints with no decomposition)",
	"é ≠ e + U+0301 (no canonical composition)",
	"ガ ≠ カ + ゛ (no canonical composition for kana voiced marks)",
	"Halfwidth Hangul ≠ compatibility jamo (no decomposition table here)",
	"U+200D ≠ removed (the zero-width joiner holds an emoji sequence together, so it is kept while U+200B is dropped)",
}

// MaxOutput returns a guaranteed upper bound on the byte length of text folded
// by this Folder from an input of n bytes, for use by callers that preallocate.
//
// Stages 1-6 expand by at most 3x — one invalid byte can become three U+FFFD
// bytes, and U+0130's lowercase is three bytes from two — and every other fold
// shortens the string, so 4n+16 is the documented bound with margin.
// Player-name substitution is the exception the package doc warns about: each
// match turns len(name) bytes into len(Placeholder), so a one-rune name can grow
// the output ninefold. When every configured name is at least 9 bytes, MaxOutput
// returns the standard bound.
func (f *Folder) MaxOutput(n int) int {
	bound := 4*n + 16
	for _, name := range f.names {
		if len(name) >= len(Placeholder) {
			continue
		}
		if scaled := n*(len(Placeholder)/len(name)) + 16; scaled > bound {
			bound = scaled
		}
	}
	return bound
}

// tableProblems reports the two structural properties the folding stage must
// satisfy for Normalize to be idempotent and total: no produced rune is itself a
// key, and every produced string is valid UTF-8. A test calls this so the
// invariant is asserted rather than argued in a comment — adding a table entry
// that maps to another entry is exactly the kind of edit that looks harmless and
// makes Normalize non-confluent.
func tableProblems() []string {
	var problems []string
	for from, to := range compatFold {
		if to == "" {
			continue
		}
		if !utf8.ValidString(to) {
			problems = append(problems, fmt.Sprintf("U+%04X folds to invalid UTF-8", from))
		}
		for _, r := range to {
			if next, ok := compatFold[r]; ok {
				problems = append(problems, fmt.Sprintf(
					"U+%04X folds to U+%04X, which itself folds to %q: not idempotent", from, r, next))
			}
		}
	}
	return problems
}
