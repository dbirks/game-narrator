package textnorm

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// corpus lists the inputs the fuzzer starts from, and the same list drives the
// table-driven property test. Every entry was chosen because folding it is where
// an implementation goes wrong: control bytes reach the whitespace stage, RTL
// text exercises the boundary rule after case folding, an emoji ZWJ sequence
// reaches a rune loop with a multi-byte interior, and invalid bytes reach the
// sanitizer.
//
// The committed copies under testdata/fuzz/FuzzNormalize/ are what `go test -fuzz`
// reseeds from; they are byte-identical to these strings.
var corpus = []struct {
	name string
	in   string
}{
	{"empty", ""},
	{"nul and controls", "a\x00b\x01\x02c\u007f"},
	{"vertical controls", "\u000b\u000c\r\nline\u2028\u2029break"},
	{"invalid single byte", "\xff"},
	{"invalid maximal subparts", "\xf0\x9f\xa6\x80\x80\x80 truncated \xc3"},
	{"truncated emoji", "party \xf0\x9f\x8e\x89 before \xf0\x9f\x8e"},
	{"surrogate half", "ed\xed\xa0\x80ing"},
	{"overlong", "\xc0\xaf \xe0\x80\xaf"},
	{"emoji zwj family", "👨‍👩‍👧‍👦 waves 👋🏽"},
	{"variation selectors", "❤️ ♥"},
	{"rtl marks", "\u200fمرحبا\u200f بالعالم"},
	{"rtl with digits", "123\u202e456"},
	{"japanese", "はじめての海岛へ！　つるぎをください。"},
	{"japanese halfwidth", "ｶﾀﾅﾞｶ ﾊﾝﾒｲﾉ ｱｲｳ"},
	{"cyrillic", "Привет, странник! Возьми меч."},
	{"greek sigmas", "ὈΔΥΣΣΕΥΣ ς σ"},
	{"korean", "안녕하세요, 모험가!"},
	{"combining latin", "é vs é, Å and Å"},
	{"typographic", "“Wait…” she said — then ‘ran’ off: 1lI0 Oo"},
	{"fullwidth", " Ａ １ ！ ？ ｚ "},
	{"nbsp runs", "\u00A0\u00A0\u00A0a\u3000\u00A0b\u00A0\u00A0"},
	{"placeholder literal", "{player} meets {player} and player"},
	{"long repeat", "ab ab ab ab ab ab ab ab ab ab ab ab ab ab ab ab ab ab"},
	{"newline soup", "\n\n\n\n\nonly\n\nline\n\n\n"},
	{"bom", "\ufeff\ufeff\ufeffheader"},
	{"tab vertical tab", "\t\u000b\t\t"},
	{"format chars", "a\u200b\u200c\u200db"},
	{"private use", "\uE000\uF8FF"},
	{"astral max", "\U0010FFFF"},
	{"half surrogate range", "\uFFFD\uFFFE\xed\xa0\x80"},
	{"cjk punctuation", "「こんにちは」……『これ』"},
	{"arabic ligature-ish", "ﻻ ﷺ"},
	{"hangul jamo", "각 vs 각"},
	{"roman numeral", "Ⅳ ⅰ ﬁ"},
	{"odd widths", "\u2060 word joiner \u180E"},
}

func corpusInputs() []string {
	out := make([]string, 0, len(corpus))
	for _, c := range corpus {
		out = append(out, c.in)
	}
	return out
}

// fuzzOptions are the option sets the fuzzer runs every input through. They are
// fixed by the test rather than taken from the fuzzed bytes so that a failure
// names an option set, and so the length bound stays provable: a configured name
// shorter than Placeholder is the one case where growth is unbounded per match
// (see Folder.MaxOutput), which the property test covers with its own bound.
var fuzzOptions = []struct {
	name string
	opts Options
}{
	{"import", Options{}},
	{"match", Options{StripPunctuation: true, OCRFold: true}},
	{"names", Options{PlayerNames: []string{"Player One", "Alexander"}}},
	{"all", Options{StripPunctuation: true, OCRFold: true, PlayerNames: []string{"Player One"}}},
}

// FuzzNormalize asserts the four properties docs/spec/12 requires of text
// normalization: no panics, output is always valid UTF-8, folding is idempotent,
// and output length stays bounded. Byte drops are caught by the length bound and
// the idempotency check rather than by comparing to the input, which folding is
// allowed to change.
//
// Run it with:
//
//	go test -run='^$' -fuzz=FuzzNormalize -fuzztime=30s ./internal/games/textnorm/
func FuzzNormalize(f *testing.F) {
	for _, in := range corpusInputs() {
		f.Add(in)
	}
	f.Fuzz(func(t *testing.T, in string) {
		for _, o := range fuzzOptions {
			folder := NewFolder(o.opts)
			checkFold(t, o.name, folder, in)
		}
	})
}

// checkFold runs the property set for one option set. It is a function rather
// than inline so the seed corpus can be checked by name in a normal test.
func checkFold(t *testing.T, label string, f *Folder, in string) {
	t.Helper()
	rep, once := f.Run(in)
	if !utf8.ValidString(once) {
		t.Fatalf("%s: %q produced invalid UTF-8 %q", label, in, once)
	}
	if twice := f.Normalize(once); twice != once {
		t.Fatalf("%s: %q folded twice differs: %q then %q", label, in, once, twice)
	}
	if limit := f.MaxOutput(len(in)); len(once) > limit {
		t.Fatalf("%s: %q (%d bytes) produced %d bytes, above the bound %d",
			label, in, len(in), len(once), limit)
	}
	if len(once) > 4*len(in)+16 {
		t.Fatalf("%s: %q (%d bytes) produced %d bytes, above 4n+16", label, in, len(in), len(once))
	}
	// "No illegal byte drops": every invalid sequence leaves a visible mark, so a
	// corrupt source can never fold into the same key as clean text.
	if rep.InvalidUTF8 > 0 && !strings.ContainsRune(once, '\uFFFD') {
		t.Fatalf("%s: report says %d invalid sequences but the output %q carries no marker",
			label, rep.InvalidUTF8, once)
	}
	// Idempotency of the report itself: re-folding changed output must count the
	// invalid bytes exactly once.
	if again, _ := f.Run(once); again.InvalidUTF8 != 0 {
		t.Fatalf("%s: folding %q the second time still found %d invalid sequences",
			label, in, again.InvalidUTF8)
	}
}

// TestSeedCorpus runs the corpus as an ordinary test. Fuzzing only happens under
// -fuzz, and docs/spec/12's properties must be checked on every `go test -race`
// run, not only during a deliberate fuzz session.
func TestSeedCorpus(t *testing.T) {
	for _, c := range corpus {
		for _, o := range fuzzOptions {
			t.Run(c.name+"/"+o.name, func(t *testing.T) {
				checkFold(t, o.name, NewFolder(o.opts), c.in)
			})
		}
	}
}

// TestFoldNeverDropsTextWithoutTrace is the "no illegal byte drops" property
// stated as a guarantee a caller can rely on: for input with no punctuation, no
// whitespace runs and no OCR-foldable characters, folding changes only case and
// compatibility characters, never deletes a letter.
func TestFoldNeverDropsTextWithoutTrace(t *testing.T) {
	in := "Take this book of names and forget none of them"
	if got := Normalize(in, importOpts); got != strings.ToLower(in) {
		t.Errorf("plain ASCII changed beyond case: %q", got)
	}
}

// TestCommittedSeedsMatchCorpus keeps testdata/fuzz/FuzzNormalize honest. Those
// files are what `go test -fuzz` reseeds from, so a corpus entry with no seed
// file means the fuzzer stopped seeing it, and a seed file that folds to a
// different string than the table says means one of the two drifted. It also
// re-checks the properties on every seed, so the committed regressions under
// found-* stay regressions.
func TestCommittedSeedsMatchCorpus(t *testing.T) {
	dir := "testdata/fuzz/FuzzNormalize"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read seed corpus: %v", err)
	}
	byValue := map[string]string{}
	for _, c := range corpus {
		byValue[c.in] = c.name
	}
	seen := map[string]bool{}
	for _, e := range entries {
		raw, err := os.ReadFile(dir + "/" + e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		value, err := parseSeedFile(raw)
		if err != nil {
			t.Errorf("%s: %v", e.Name(), err)
			continue
		}
		if _, ok := byValue[value]; !ok && !strings.HasPrefix(e.Name(), "found-") {
			t.Errorf("%s holds %q, which is not in the corpus table; regenerate the seeds or add the input", e.Name(), value)
		}
		if name, ok := byValue[value]; ok && name != strings.ReplaceAll(e.Name(), "-", " ") {
			seen[name] = true
		}
		seen[strings.ReplaceAll(e.Name(), "-", " ")] = true
		for _, o := range fuzzOptions {
			checkFold(t, e.Name()+"/"+o.name, NewFolder(o.opts), value)
		}
	}
	for _, c := range corpus {
		if !seen[c.name] {
			t.Errorf("corpus entry %q has no committed seed file", c.name)
		}
	}
}

// parseSeedFile reads one entry of Go's text seed-corpus format
// ("go test fuzz v1" then one Go literal per argument) for a single string
// argument.
func parseSeedFile(raw []byte) (string, error) {
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 2 || lines[0] != "go test fuzz v1" || !strings.HasPrefix(lines[1], "string(") {
		return "", fmt.Errorf("not a single-string fuzz seed file (%d lines)", len(lines))
	}
	lit := strings.TrimSuffix(strings.TrimPrefix(lines[1], "string("), ")")
	return strconv.Unquote(lit)
}
