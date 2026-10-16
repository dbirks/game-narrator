package textnorm

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// opts is the import-time configuration: fold layout and script differences,
// keep punctuation, do not guess at OCR errors.
var importOpts = Options{}

// matchOpts is what the recognition stage compares with: everything folded.
var matchOpts = Options{StripPunctuation: true, OCRFold: true, PlayerNames: []string{"Player One"}}

func TestTableIsConfluent(t *testing.T) {
	if problems := tableProblems(); len(problems) > 0 {
		for _, p := range problems {
			t.Error(p)
		}
	}
	if n := utf8.RuneCountInString(katakanaHalf); n != utf8.RuneCountInString(katakanaFull) {
		t.Errorf("halfwidth kana tables are out of step (%d runes)", n)
	}
	// Every entry must match the NFKC target the table claims. This is the check
	// that catches the "constant offset" temptation: halfwidth ﾅ is ナ, not ザ.
	half, full := []rune(katakanaHalf), []rune(katakanaFull)
	for i, r := range half {
		want := string(full[i])
		if got := Normalize(string(r), importOpts); got != want {
			t.Errorf("U+%04X folded to %q, want %q", r, got, want)
		}
	}
}

func TestCompatibilityFolds(t *testing.T) {
	cases := []struct {
		name, in, want string
		opts           Options
	}{
		{"typographic apostrophe", "Hero’s sword", "hero's sword", importOpts},
		{"typographic quotes", "She said “wait”", `she said "wait"`, importOpts},
		{"en dash", "wait – go", "wait - go", importOpts},
		{"em dash", "wait — go", "wait - go", importOpts},
		{"ellipsis", "Wait…", "wait...", importOpts},
		{"fullwidth ascii", "ＡＢＣ！！？", "abc!!?", importOpts},
		{"halfwidth katakana", "カタカナ", "カタカナ", importOpts},
		{"ligatures", "ﬁnd the ﬂower", "find the flower", importOpts},
		{"sharp s", "Straße", "strasse", importOpts},
		{"bom mid-string", "he\uFEFFllo", "hello", importOpts},
		{"soft hyphen", "oth\u00ADerwise", "otherwise", importOpts},
		{"nbsp and ideographic space", "a\u00A0b\u3000c", "a b c", importOpts},
		{"line wraps", "the hero's\n  sword\n", "the hero's sword", importOpts},
		{"case fold cyrillic", "ПРИВЕТ", "привет", importOpts},
		{"zwj preserved", "👨‍👩‍👧 family", "👨‍👩‍👧 family", importOpts},
		{"strip punctuation", "Wait... what?!", "walt what", matchOpts},
		{"ocr fold zero", "T0tal", "total", Options{OCRFold: true}},
		{"ocr fold one and i", "Il1 and 1lI", "lll and lll", Options{OCRFold: true}},
		{"ocr fold rn", "arnm", "amm", Options{OCRFold: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Normalize(tc.in, tc.opts)
			if got != tc.want {
				t.Errorf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("output for %q is not valid UTF-8: %q", tc.in, got)
			}
			// Idempotency is the property the fuzz target asserts; checking it on
			// the hand-written cases too means a regression names the case.
			if twice := Normalize(got, tc.opts); twice != got {
				t.Errorf("not idempotent: %q -> %q -> %q", tc.in, got, twice)
			}
		})
	}
}

// TestNonLatinPassesThrough is the requirement that folding must not mangle a
// script it does not understand: Japanese and Cyrillic survive byte-for-byte
// apart from the folds listed above.
func TestNonLatinPassesThrough(t *testing.T) {
	japanese := "はじめての 海岛へ ようこそ！　つるぎを 持ってきてください。"
	if got := Normalize(japanese, importOpts); got != strings.Join([]string{
		"はじめての 海岛へ ようこそ!", "つるぎを 持ってきてください。",
	}, " ") {
		t.Errorf("Japanese was altered beyond its punctuation:\n got %q\nwant %q",
			got, "はじめての 海岛へ ようこそ! つるぎを 持ってきてください。")
	}
	cyrillic := "Привет, странник! Возьми меч."
	if got := Normalize(cyrillic, importOpts); got != strings.ToLower(cyrillic) {
		t.Errorf("Cyrillic changed beyond case: got %q want %q", got, strings.ToLower(cyrillic))
	}
	greek := "Χαίρε, ὦ ξένε."
	if got := Normalize(greek, importOpts); got != strings.ToLower(greek) {
		t.Errorf("Greek changed beyond case: got %q want %q", got, strings.ToLower(greek))
	}
	// The documented limitations: these are the distinctions we do NOT fold.
	if Equal("σ", "ς", importOpts) {
		t.Error("final sigma folded: the package doc says it does not, and a caller may rely on that")
	}
	if Equal("е", "ё", importOpts) {
		t.Error("Cyrillic yo folded: the package doc says it does not")
	}
	if !Equal("é", "é", importOpts) {
		t.Error("identical precomposed text stopped matching")
	}
}

func TestPlayerNamePlaceholder(t *testing.T) {
	opts := Options{PlayerNames: []string{"Player One", "Alex", "Ann"}}
	cases := []struct{ in, want string }{
		{"Alex finds a key", "{player} finds a key"},
		// A name inside a longer word is not a name.
		{"Annalise the ledger", "annalise the ledger"},
		{"ALEX and alex", "{player} and {player}"},
		// "Ann" must not come out of "Annalise".
		{"Annalise met Ann", "annalise met {player}"},
		// An already-folded string is stable: the guard against a name that
		// occurs inside the placeholder literal.
		{"{player}", "{player}"},
		{"a", "a"},
	}
	for _, tc := range cases {
		got := Normalize(tc.in, opts)
		if got != tc.want {
			t.Errorf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if twice := Normalize(got, opts); twice != got {
			t.Errorf("not idempotent: %q -> %q -> %q", tc.in, got, twice)
		}
	}

	// A profile that names its player "Player" must not have the placeholder
	// eaten by its own name — this is the case a naive strings.Replace fails.
	selfRef := Options{PlayerNames: []string{"Player"}}
	for i, in := range []string{"Player says hi", "{player} says hi", "{{player}}"} {
		once := Normalize(in, selfRef)
		if twice := Normalize(once, selfRef); twice != once {
			t.Errorf("case %d (%q): %q -> %q, not idempotent", i, in, once, twice)
		}
	}
	if got := Normalize("Player says hi", selfRef); got != "{player} says hi" {
		t.Errorf("got %q", got)
	}

	// Longest name wins.
	long := Options{PlayerNames: []string{"Player", "Player Two"}}
	if got := Normalize("player two and player", long); got != "{player} and {player}" {
		t.Errorf("longest name should win: got %q", got)
	}

	// Japanese names abut other words with no space; the ASCII boundary rule must
	// not block them.
	jp := Options{PlayerNames: []string{"リンク"}}
	if got := Normalize("リンクはつるぎをふる", jp); got != "{player}はつるぎをふる" {
		t.Errorf("got %q", got)
	}

	// StripPunctuation must not eat the placeholder's braces, or substitution
	// would grow on every call.
	both := Options{StripPunctuation: true, PlayerNames: []string{"alex"}}
	got := Normalize("Alex, wait!", both)
	if got != "{player} wait" {
		t.Errorf("got %q", got)
	}
	if twice := Normalize(got, both); twice != got {
		t.Errorf("not idempotent: %q -> %q", got, twice)
	}
}

func TestInvalidUTF8IsReplacedAndCounted(t *testing.T) {
	in := "hero\uFFFDs\xFF sword\x80\x81"
	rep, got := NormalizeWithReport(in, importOpts)
	if !utf8.ValidString(got) {
		t.Fatalf("output is invalid UTF-8: %q", got)
	}
	if rep.InvalidUTF8 != 3 {
		t.Errorf("InvalidUTF8 = %d, want 3 (one 0xFF and the maximal subpart pair 0x80 0x81)", rep.InvalidUTF8)
	}
	if n := strings.Count(got, "\uFFFD"); n == 0 {
		t.Errorf("nothing marks the replaced bytes: %q", got)
	}
	// The replacement must not make two different inputs compare equal: this is
	// the reason the byte is replaced rather than dropped.
	other := Normalize("hero\uFFFDs sword", importOpts)
	if got == other {
		t.Errorf("a corrupt byte silently produced the same key as clean text: %q", got)
	}
	if twice := Normalize(got, importOpts); twice != got {
		t.Errorf("not idempotent: %q -> %q", got, twice)
	}
}

func TestReportAccountsForWork(t *testing.T) {
	rep, _ := NormalizeWithReport("A\u00A0B…  C\xFF", Options{StripPunctuation: true})
	if !rep.HasChanges() {
		t.Fatal("report claims nothing changed")
	}
	if rep.InvalidUTF8 != 1 || rep.Folded == 0 || rep.SpacesCollapsed == 0 || rep.PunctuationCut == 0 {
		t.Errorf("report under-counts: %+v", rep)
	}
	if rep.PlayersReplaced != 0 {
		t.Errorf("no names configured, yet %d substitutions", rep.PlayersReplaced)
	}
}

// TestFolderReusesNamePreparation checks the point of Folder: the same answers,
// prepared once.
func TestFolderReusesNamePreparation(t *testing.T) {
	opts := Options{StripPunctuation: true, PlayerNames: []string{"  Alex  ", "alex", "", "{player}", "Alexandra"}}
	f := NewFolder(opts)
	if len(f.names) != 2 {
		t.Fatalf("prepared names = %v, want the duplicates, blanks and the placeholder removed", f.names)
	}
	if f.names[0] != "alexandra" {
		t.Errorf("names must be longest-first, got %v", f.names)
	}
	for _, s := range []string{"Alexandra met Alex", "alex!", "nothing here"} {
		if a, b := f.Normalize(s), Normalize(s, opts); a != b {
			t.Errorf("Folder disagrees with Normalize for %q: %q vs %q", s, a, b)
		}
	}
}

// TestBoundsAcrossOptions walks every option combination over a set of awkward
// inputs, which is what the fuzz target does with generated ones. Running it as
// an ordinary test keeps the property checked in -race and in CI where long fuzz
// runs do not belong.
func TestBoundsAcrossOptions(t *testing.T) {
	inputs := append(corpusInputs(), strings.Repeat("a", 5000))
	var all []Options
	for _, strip := range []bool{false, true} {
		for _, fold := range []bool{false, true} {
			for _, names := range [][]string{nil, {"Player One"}, {"a"}, {"link", "リンク"}} {
				all = append(all, Options{StripPunctuation: strip, OCRFold: fold, PlayerNames: names})
			}
		}
	}
	for _, in := range inputs {
		for _, opts := range all {
			f := NewFolder(opts)
			rep, once := f.Run(in)
			if !utf8.ValidString(once) {
				t.Fatalf("opts %+v input %q produced invalid UTF-8 %q", opts, in, once)
			}
			if twice := f.Normalize(once); twice != once {
				t.Errorf("opts %+v input %q: not idempotent (%q then %q)", opts, in, once, twice)
			}
			if limit := f.MaxOutput(len(in)); len(once) > limit {
				t.Errorf("opts %+v input %d bytes produced %d bytes, above the %d bound",
					opts, len(in), len(once), limit)
			}
			// The headline bound from the task: 4n+16 holds whenever no name is
			// shorter than the placeholder.
			shortName := false
			for _, n := range f.names {
				if len(n) < len(Placeholder) {
					shortName = true
				}
			}
			if !shortName && len(once) > 4*len(in)+16 {
				t.Errorf("opts %+v input %q: %d bytes out exceeds 4n+16", opts, in, len(once))
			}
			if rep.InvalidUTF8 != 0 && utf8.ValidString(in) {
				t.Errorf("report claims %d invalid sequences for valid input %q", rep.InvalidUTF8, in)
			}
		}
	}
}

// TestFoldIsNotLossyForDisplay is a documentation-turned-assertion: normalized
// text is a key, and the function that must never be used for rendering is this
// one. If someone wires Normalize into TTS, this test is the smell.
func TestFoldIsNotLossyForDisplay(t *testing.T) {
	original := "DON’T—WAIT (NOW)!"
	if Normalize(original, matchOpts) == original {
		t.Skip("nothing to demonstrate")
	}
	if Normalize(original, importOpts) == original {
		t.Fatal("folding did nothing; the matchOpts comparison below is meaningless")
	}
	if got := Normalize(original, matchOpts); strings.ContainsAny(got, "().!—’") {
		t.Errorf("match-folded text still carries punctuation that display code might rely on: %q", got)
	}
	// And the import-time fold keeps it readable-looking, which is why neither
	// rendering is speech_text.
	if got := Normalize(original, importOpts); !strings.Contains(got, "(") {
		t.Errorf("import fold stripped punctuation it must keep: %q", got)
	}
}
