// Package textnorm produces the string retrieval compares against.
//
// docs/spec/02 "Normalization and markup" splits game text into three
// renderings stored side by side: original_text is byte-faithful, normalized_text
// is the retrieval key, speech_text is what the narrator says. This package owns
// only the middle one. Nothing here may be shown to the user or sent to TTS —
// folding "Don't" to "dont" and a player name to "{player}" is exactly what the
// matcher needs and exactly what must never reach a speaker (docs/spec/04
// "renderSpeechText" is a separate, adapter-owned step).
//
// The pipeline is fixed and each stage is idempotent, so
// Normalize(Normalize(x)) == Normalize(x) holds (docs/spec/12 "Property-based and
// fuzz tests" asserts it under fuzzing):
//
//  1. sanitize      — invalid UTF-8 becomes U+FFFD, counted, never silently dropped
//  2. compatibility — typographic punctuation, fullwidth ASCII, halfwidth kana,
//     ligatures, invisible formatting characters
//  3. case          — Unicode simple lowercase
//  4. whitespace    — every Unicode space, including U+00A0, U+3000 and line
//     breaks, collapses to one space; the result is trimmed
//  5. punctuation   — only when Options.StripPunctuation is set, then whitespace
//     is collapsed again
//  6. OCR fold      — 0↔o, 1↔l↔i, rn→m, only when Options.OCRFold is set
//  7. player name   — configured names become {player}
//
// The order of 5 and 6 is load-bearing and was found by the fuzzer rather than
// by reading: the OCR "rn"→"m" rule has to see the text *after* punctuation is
// removed, because "r!n" only becomes "rn" at stage 5. Folding first made a
// second call differ from the first.
//
// Known limitations, stated rather than hidden:
//
//   - No NFC/NFKC. golang.org/x/text is not a dependency of this project
//     (ADR-0008's dependency rule), and the standard library carries no
//     composition tables. Consequence: text that differs only by combining
//     sequences ("e" + U+0301 vs U+00E9), by precomposed Hangul versus jamo, or
//     by Japanese voiced-sound decomposition (ガ vs カ+゛) does not compare
//     equal. The compatibility stage in tables.go folds the character classes
//     that actually show up in game scripts and OCR output — fullwidth forms,
//     halfwidth kana, typographic punctuation, ligatures — which covers the
//     overwhelming majority of real mismatches. Adding x/text is an ADR, not an
//     import.
//   - Case folding uses strings.ToLower, not the smallest rune in a
//     unicode.SimpleFold cycle. SimpleFold's cycle minimum is sometimes the
//     *capital* of a pair (U+01C4 Ǆ is smaller than its lowercase U+01C6), so
//     "fold to cycle minimum" is not idempotent once a later stage re-reads the
//     result through ToLower. The cost is that Greek final sigma (ς) does not
//     fold to medial sigma (σ), and Cyrillic ё does not fold to е; both are
//     listed in NotFoldedEquivalents for callers that need to know.
//   - Output length is bounded by 4*len(input)+16 for every stage above except
//     the player-name substitution, which grows the text whenever a configured
//     name is shorter than Placeholder (9 bytes). See Folder.MaxOutput.
package textnorm

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Placeholder replaces every configured player name in normalized text. It is
// exported because the matcher, the message editor and the dev UI all display
// it, and because an adapter that resolves its own name placeholders before
// calling Normalize needs to know what it will be compared against.
const Placeholder = "{player}"

// Options selects which stages run. The zero value is the conservative
// normalization used at import time: fold script and layout differences, keep
// punctuation, do not guess at OCR errors, do not assume anyone is the player.
type Options struct {
	// StripPunctuation removes Unicode punctuation and symbol categories.
	// Retrieval turns it on for fuzzy candidate matching; import leaves it off
	// so a message's key still distinguishes "Wait!" from "Wait." (docs/spec/02
	// keeps both, and an ellipsis is sometimes the only pause cue there is).
	StripPunctuation bool

	// OCRFold folds the confusions an OCR engine actually makes: zero for
	// capital-o, one for lowercase-L and lowercase-i, and the two-character
	// run "rn" for m. It must be applied to *both* sides of every comparison —
	// the stored key and the freshly read screen text — or it makes matching
	// strictly worse than doing nothing. That is why import stores keys folded
	// without it and the recognition stage turns it on at match time
	// (docs/spec/12 "candidate ranking"). It is applied after punctuation
	// stripping, so the adjacency it folds on is the one a repeated fold sees.
	OCRFold bool

	// PlayerNames lists the strings that refer to the player character, in any
	// spelling; each is folded through this package's own pipeline before it is
	// matched, so a caller may pass the display forms ("Player One"). Matching
	// is case- and whitespace-insensitive and requires a word boundary in
	// scripts that use spaces (see replacePlayers). Empty means no placeholder
	// substitution — the honest default when a profile has not named its
	// player, because inventing one would attribute lines wrongly.
	PlayerNames []string
}

// Report counts what normalization changed, so a caller can log why two strings
// it expected to match did not, and so the fuzz test can assert that nothing is
// lost without a trace (docs/spec/12 "no panics or illegal byte drops").
type Report struct {
	// InvalidUTF8 counts maximal invalid byte sequences replaced by U+FFFD.
	InvalidUTF8 int
	// Folded counts compatibility characters rewritten or removed.
	Folded int
	// SpacesCollapsed counts whitespace runs joined to a single space.
	SpacesCollapsed int
	// OCRFolded counts OCR-confusion rewrites.
	OCRFolded int
	// PunctuationCut counts punctuation and symbol characters removed.
	PunctuationCut int
	// PlayersReplaced counts player-name substitutions made.
	PlayersReplaced int
}

// Normalize folds s for matching under opts.
func Normalize(s string, opts Options) string {
	_, out := NormalizeWithReport(s, opts)
	return out
}

// NormalizeWithReport returns the folded string and a count of what changed.
func NormalizeWithReport(s string, opts Options) (Report, string) {
	return NewFolder(opts).Run(s)
}

// Folder applies one fixed Options to many strings, doing the per-options setup
// (folding the configured player names once) instead of per string. Indexing a
// 10k-message profile reuses one Folder for all of them (docs/spec/16 M1
// "search 10k synthetic messages without memory blowup").
type Folder struct {
	opts  Options
	names []string // folded player names, longest first, placeholder excluded
}

// NewFolder prepares opts for repeated use.
func NewFolder(opts Options) *Folder {
	f := &Folder{opts: opts}
	seen := make(map[string]bool, len(opts.PlayerNames))
	for _, name := range opts.PlayerNames {
		// Names go through the same first four stages as text, so the caller can
		// pass the spelling from the script rather than a folded form. Stages 5-7
		// are excluded: a name must not be OCR-folded (the text it is matched
		// against is folded separately) and must not eat itself.
		name = f.fold(name, &Report{}, false)
		if name == "" || name == Placeholder || seen[name] {
			continue
		}
		seen[name] = true
		f.names = append(f.names, name)
	}
	// Longest first, so "Player Two" wins over "Player" at the same offset.
	sortStringsDesc(f.names)
	return f
}

// Options reports the options this Folder applies.
func (f *Folder) Options() Options { return f.opts }

// Normalize folds s.
func (f *Folder) Normalize(s string) string {
	_, out := f.Run(s)
	return out
}

// Run folds s and reports what changed.
func (f *Folder) Run(s string) (Report, string) {
	var rep Report
	out := f.fold(s, &rep, true)
	return rep, out
}

// fold runs stages 1-6, then 7 when substitute is set. The bool is false only
// while preparing the configured names themselves: substituting into a name
// would let one name eat another before it is ever matched.
func (f *Folder) fold(s string, rep *Report, substitute bool) string {
	out := sanitize(s, rep)
	out = compatibility(out, rep)
	out = strings.ToLower(out)
	out = collapseSpaces(out, rep)
	if f.opts.StripPunctuation {
		out = cutPunctuation(out, rep)
		// Re-collapse: stripping punctuation can weld two spaces together
		// ("wait , now" → "wait  now"), and without this second pass a second
		// Normalize would shorten the string again, which is an idempotency bug
		// the fuzz corpus looks for.
		out = collapseSpaces(out, rep)
	}
	// OCR folding runs after punctuation for the reason in the package doc: it
	// must see the same character adjacency a repeated fold would see.
	if f.opts.OCRFold {
		out = ocrFold(out, rep)
	}
	if substitute && len(f.names) > 0 {
		out = f.replacePlayers(out, rep)
	}
	return out
}

// Equal reports whether a and b fold to the same key under opts. It exists so no
// caller hand-rolls a comparison and quietly folds only one side, which is the
// failure docs/spec/12 warns about for candidate ranking: half-folded text does
// not merely fail to match, it matches the wrong things.
func Equal(a, b string, opts Options) bool {
	f := NewFolder(opts)
	return f.Normalize(a) == f.Normalize(b)
}

// HasChanges reports whether folding altered the input at all. A caller that
// stores normalized text beside the original uses it to notice the rare line
// where the two differ more than expected — a corrupt byte, a stray BOM — instead
// of discovering it when a match fails.
func (r Report) HasChanges() bool {
	return r.InvalidUTF8 != 0 || r.Folded != 0 || r.SpacesCollapsed != 0 ||
		r.OCRFolded != 0 || r.PunctuationCut != 0 || r.PlayersReplaced != 0
}

//
// ── stages ──────────────────────────────────────────────────────────────────
//

// sanitize replaces each maximal invalid UTF-8 sequence with U+FFFD.
//
// Replacing rather than dropping is a deliberate information-preserving choice:
// a dropped byte makes "Hero′s sword" and "Heros sword" collide in the index,
// and the collision is invisible (docs/spec/12 "no illegal byte drops"). The
// count goes in the Report so a caller can warn about a corrupt source file
// instead of quietly indexing mojibake.
func sanitize(s string, rep *Report) string {
	if utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + utf8.RuneLen(utf8.RuneError))
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		if r == utf8.RuneError && size <= 1 {
			rep.InvalidUTF8++
			b.WriteRune(utf8.RuneError)
			s = s[1:]
			continue
		}
		b.WriteString(s[:size])
		s = s[size:]
	}
	return b.String()
}

// isASCII reports whether s holds only bytes below 0x80, the fast path that
// lets the Unicode-aware stages skip work on plain English script lines, which
// is most of what a first-party import contains.
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// compatibility folds the characters whose NFKC mappings matter for game text,
// then removes the invisible ones. See the package doc for why this is a table
// and not a normalization algorithm.
func compatibility(s string, rep *Report) string {
	if isASCII(s) {
		return s
	}
	var b strings.Builder
	changed := false
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		j := i + size
		i = j
		if r >= fullwidthFirst && r <= fullwidthLast {
			rep.Folded++
			changed = true
			b.WriteByte(byte(r - fullwidthOffset))
			continue
		}
		repl, ok := compatFold[r]
		if !ok {
			b.WriteString(s[j-size : j])
			continue
		}
		rep.Folded++
		changed = true
		b.WriteString(repl)
	}
	if !changed {
		return s
	}
	return b.String()
}

// collapseSpaces joins every run of Unicode whitespace to one space and trims
// the ends. Line wraps are the important case: a textbox breaks a sentence at a
// different word than the script file does, and both must index identically
// (docs/spec/02 lists "line wraps" among the things normalized_text handles).
//
// unicode.IsSpace covers U+00A0, U+1680, U+2000–U+200A, U+2028, U+2029,
// U+202F, U+205F, U+3000 and U+0085 in addition to the ASCII controls, which is
// what a game script dumped from Shift-JIS or copied from a wiki produces.
func collapseSpaces(s string, rep *Report) string {
	if isASCII(s) && !strings.ContainsAny(s, " \t\n\r\v\f") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	pending := false
	wrote := false
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		if unicode.IsSpace(r) {
			// Every run counts, including the leading and trailing ones this
			// function removes: the Report is how a caller learns the key differs
			// from the stored text, and an uncounted trim is a silent change.
			if !pending {
				rep.SpacesCollapsed++
				pending = true
			}
			continue
		}
		if pending && wrote {
			b.WriteByte(' ')
		}
		pending = false
		wrote = true
		b.WriteString(s[i-size : i])
	}
	return b.String()
}

// ocrFold folds the confusions listed on Options.OCRFold.
//
// Case folding has already run, so only the lowercase forms need entries. The
// rewrites are chosen to be confluent: i→l, 1→l, l→l all land on l and l is not
// a source anywhere, so a second pass changes nothing; "rn"→"m" cannot
// re-trigger because m is not a source and "rn" cannot be produced by the map.
func ocrFold(s string, rep *Report) string {
	if !strings.ContainsAny(s, "01il") && !strings.Contains(s, "rn") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == '0':
			rep.OCRFolded++
			b.WriteByte('o')
			i += size
		case r == '1':
			rep.OCRFolded++
			b.WriteByte('l')
			i += size
		case r == 'i':
			rep.OCRFolded++
			b.WriteByte('l')
			i += size
		case r == 'r' && i+size < len(s) && strings.HasPrefix(s[i+size:], "n"):
			// "rn" and "m" are indistinguishable in the Dolphin textbox font at
			// the resolution the capture actually delivers (docs/spec/12 lists
			// OCR confusion fuzzing for exactly this reason).
			rep.OCRFolded++
			b.WriteByte('m')
			i += size + 1
		default:
			b.WriteString(s[i : i+size])
			i += size
		}
	}
	return b.String()
}

// cutPunctuation removes Unicode punctuation and symbols, leaving letters,
// marks, numbers and spaces.
//
// Symbols go too (™, ★, ➔): OCR produces them from icon glyphs, and a star in a
// menu entry is not part of the words. Two things are protected:
//
//   - The Placeholder literal — otherwise StripPunctuation would eat its braces,
//     and the substitution stage would then re-wrap the exposed word in a new
//     placeholder on every call.
//   - U+FFFD, the replacement character. It is category So and would otherwise be
//     cut, which would make a corrupt source fold to the same key as clean text
//     — the exact silent byte drop docs/spec/12 forbids. A marker that survives
//     folding is what makes the Report's InvalidUTF8 count actionable.
func cutPunctuation(s string, rep *Report) string {
	if isASCII(s) && !strings.ContainsAny(s, "!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~") && !strings.Contains(s, Placeholder) && !strings.ContainsRune(s, '\uFFFD') {
		return s
	}
	protected := placeholderSpans(s)
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		j := i + size
		i = j
		if r == utf8.RuneError || inSpans(protected, j-size) {
			b.WriteString(s[j-size : j])
			continue
		}
		if unicode.IsPunct(r) || unicode.IsSymbol(r) {
			rep.PunctuationCut++
			continue
		}
		b.WriteString(s[j-size : j])
	}
	return b.String()
}

// replacePlayers substitutes the prepared names with Placeholder.
//
// Two guards keep this honest:
//
//   - Longest name wins at a given offset (NewFolder sorts them), so a profile
//     naming both "Player" and "Player Two" folds the longer first.
//   - A match must not sit inside a placeholder that is already in the text.
//     Without that, a name that happens to occur inside "{player}" ("Player")
//     would re-wrap on every call and grow the string forever, which the
//     fuzz test would catch as an idempotency failure.
//
// The boundary rule is asymmetric on purpose: a match may not touch an ASCII
// alphanumerical neighbour ("Anna" must not lose "Ann"), but a non-ASCII
// neighbour is allowed, because Japanese, Chinese and Korean write no word
// separators and requiring one would make a Japanese player name unmatchable.
func (f *Folder) replacePlayers(s string, rep *Report) string {
	protected := placeholderSpans(s)
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if inSpans(protected, i) {
			j := i
			for j < len(s) {
				r, size := utf8.DecodeRuneInString(s[j:])
				if inSpans(protected, j) {
					b.WriteRune(r)
					j += size
					continue
				}
				break
			}
			if j == i { // defensive: cannot happen, keeps the loop advancing
				_, size := utf8.DecodeRuneInString(s[i:])
				b.WriteString(s[i : i+size])
				i += size
			} else {
				i = j
			}
			continue
		}
		if name, at := f.matchNameAt(s, i, protected); at >= 0 {
			rep.PlayersReplaced++
			b.WriteString(Placeholder)
			i = at + len(name)
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		b.WriteRune(r)
		i += size
	}
	return b.String()
}

// matchNameAt reports the longest prepared name matching s at offset i, and
// whether the boundary rule permits it. It returns ("", -1) when nothing is
// allowed there, which the caller reads as "copy one rune and move on".
func (f *Folder) matchNameAt(s string, i int, protected []bool) (string, int) {
	for _, name := range f.names {
		if !strings.HasPrefix(s[i:], name) {
			continue
		}
		end := i + len(name)
		if inSpans(protected, i) || inSpans(protected, end-1) || inSpans(protected, end) {
			continue
		}
		if boundaryBlocks(s, i, end) {
			continue
		}
		return name, i
	}
	return "", -1
}

// boundaryBlocks reports whether an ASCII alphanumerical neighbour makes this
// match a substring hit rather than a name.
func boundaryBlocks(s string, start, end int) bool {
	if end < len(s) {
		if c := s[end]; c < utf8.RuneSelf && isAlnumByte(c) {
			return true
		}
	}
	if start > 0 {
		if c := s[start-1]; c < utf8.RuneSelf && isAlnumByte(c) {
			return true
		}
	}
	return false
}

func isAlnumByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

//
// ── small helpers ───────────────────────────────────────────────────────────
//

// placeholderSpans marks the byte offsets covered by an existing Placeholder
// literal, left to right and non-overlapping. A bitset rather than a list: the
// substitution loop asks this question at every offset, and a list would make
// text dense with placeholders quadratic.
func placeholderSpans(s string) []bool {
	if !strings.Contains(s, Placeholder) {
		return nil
	}
	spans := make([]bool, len(s)+1)
	for off := 0; ; {
		k := strings.Index(s[off:], Placeholder)
		if k < 0 {
			return spans
		}
		start := off + k
		for b := start; b < start+len(Placeholder); b++ {
			spans[b] = true
		}
		off = start + len(Placeholder)
	}
}

func inSpans(spans []bool, off int) bool {
	return off >= 0 && off < len(spans) && spans[off]
}

// sortStringsDesc orders names longest-first, then bytewise, so substitution is
// deterministic regardless of the order the profile listed names in.
func sortStringsDesc(names []string) {
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && namesLess(names[j], names[j-1]); j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
}

func namesLess(a, b string) bool {
	if len(a) != len(b) {
		return len(a) > len(b)
	}
	return a < b
}
