package voices

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// filler returns n+ characters of plain words (no multi-byte runes, no
// punctuation) so a word-boundary assertion is unambiguous.
func filler(n int) string {
	words := []string{"warm", "clear", "measured", "gentle", "steady", "bright", "soft", "calm", "rounded", "mellow"}
	var b strings.Builder
	for b.Len() < n {
		b.WriteString(words[b.Len()%len(words)])
		b.WriteString(" ")
	}
	return strings.TrimSpace(b.String())
}

// longStructuredBrief is the analyze_character output shape (ADR-0046): a
// JSON object of the six creative brief fields, each long enough that the
// whole brief runs well past the 500-rune cap (~900 chars, like the live
// run that exposed the gateway cap).
func longStructuredBrief() string {
	return `{"timbre":"` + filler(150) +
		`","vocal_age_impression":"` + filler(150) +
		`","cadence":"` + filler(150) +
		`","emotional_baseline":"` + filler(150) +
		`","language_notes":"` + filler(150) +
		`","pronunciation_notes":"` + filler(150) + `"}`
}

func TestDesignInstructionStructuredBriefFitsCap(t *testing.T) {
	brief := longStructuredBrief()
	if n := utf8.RuneCountInString(brief); n <= DesignInstructionCap {
		t.Fatalf("test brief is %d runes; the fixture must exceed the cap for this test to mean anything", n)
	}
	out := DesignInstruction(brief, 0)
	if n := utf8.RuneCountInString(out); n > DesignInstructionCap {
		t.Fatalf("instruction is %d runes, want <= %d", n, DesignInstructionCap)
	}
	if !strings.HasPrefix(out, "timbre: ") {
		t.Fatalf("instruction must start with the top-priority design field, got %q…", out[:32])
	}
	if !utf8.ValidString(out) {
		t.Fatal("instruction is not valid UTF-8")
	}
	if r := []rune(out); len(r) == 0 || !(unicode.IsLetter(r[len(r)-1]) || unicode.IsDigit(r[len(r)-1])) {
		t.Fatalf("instruction ends mid-word or with punctuation: %q", out[len(out)-32:])
	}
}

func TestDesignInstructionDropsProseBeforeCuttingDesignFields(t *testing.T) {
	// The five voice-design fields fit the cap; the prose notes do not.
	// The derivation must keep all five design fields whole and drop the
	// prose instead of cutting a design field.
	brief := `{"timbre":"a warm clear tone","vocal_age_impression":"a young adult impression","cadence":"measured and unhurried","emotional_baseline":"calm with quiet warmth","language_notes":"soft diphthongs, no clipped endings","pronunciation_notes":"` + filler(350) + `"}`
	out := DesignInstruction(brief, 0)
	for _, want := range []string{"timbre: a warm clear tone", "age: a young adult impression", "pace: measured and unhurried", "emotion: calm with quiet warmth", "accent: soft diphthongs, no clipped endings"} {
		if !strings.Contains(out, want) {
			t.Fatalf("derived instruction lost a whole design field %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "pronunciation") {
		t.Fatalf("prose should be dropped when the design fields fit the cap:\n%s", out)
	}
	if n := utf8.RuneCountInString(out); n > DesignInstructionCap {
		t.Fatalf("instruction is %d runes, want <= %d", n, DesignInstructionCap)
	}
}

func TestDesignInstructionIsStable(t *testing.T) {
	// Go map iteration is random: the same fields in different JSON key
	// orders (and repeated calls) must yield the byte-identical instruction.
	a := `{"timbre":"ta","vocal_age_impression":"va","cadence":"ca","emotional_baseline":"eb","language_notes":"ln","pronunciation_notes":"pn"}`
	b := `{"pronunciation_notes":"pn","language_notes":"ln","emotional_baseline":"eb","cadence":"ca","vocal_age_impression":"va","timbre":"ta"}`
	want := DesignInstruction(a, 0)
	if got := DesignInstruction(b, 0); got != want {
		t.Fatalf("key order changed the instruction:\n%s\nvs\n%s", got, want)
	}
	for i := 0; i < 20; i++ {
		if got := DesignInstruction(longStructuredBrief(), 0); got != DesignInstruction(longStructuredBrief(), 0) {
			t.Fatal("repeated derivations of the same brief differ")
		}
	}
}

func TestDesignInstructionMultiByteBrief(t *testing.T) {
	// Multi-byte runes (CJK + emoji) must count as characters, not bytes:
	// the cap is in runes, and a cut may never split a rune.
	cjk := strings.Repeat("温暖的声线 圆润的低音 沉稳的节奏 ", 40) // 12 runes each, 480 total
	brief := cjk + "再长一点 再长一点 再长一点 再长一点 🙂🙂🙂🙂🙂🙂🙂🙂"
	if n := utf8.RuneCountInString(brief); n <= DesignInstructionCap {
		t.Fatalf("fixture is %d runes; it must exceed the cap", n)
	}
	out := DesignInstruction(brief, 0)
	if n := utf8.RuneCountInString(out); n > DesignInstructionCap {
		t.Fatalf("instruction is %d runes, want <= %d", n, DesignInstructionCap)
	}
	if !utf8.ValidString(out) {
		t.Fatal("cut split a multi-byte rune")
	}
	// A structured multi-byte brief: the cut inside a value must land on a
	// rune boundary and keep the top field's label.
	structured := `{"timbre":"` + cjk + `","cadence":"` + filler(200) + `"}`
	out2 := DesignInstruction(structured, 0)
	if !strings.HasPrefix(out2, "timbre: ") {
		t.Fatalf("structured multi-byte cut lost the top field label: %q", out2[:32])
	}
	if !utf8.ValidString(out2) || utf8.RuneCountInString(out2) > DesignInstructionCap {
		t.Fatalf("structured multi-byte instruction invalid or over cap: %d runes", utf8.RuneCountInString(out2))
	}
}

func TestDesignInstructionProseCutAtWordBoundary(t *testing.T) {
	words := strings.Split(filler(900), " ")
	prose := strings.Join(words, " ") // ~900 chars of prose, no JSON
	out := DesignInstruction(prose, 0)
	if n := utf8.RuneCountInString(out); n > DesignInstructionCap {
		t.Fatalf("prose instruction is %d runes, want <= %d", n, DesignInstructionCap)
	}
	// Word-boundary cut: the last word of the output must be a whole word
	// from the input.
	parts := strings.Split(out, " ")
	last := parts[len(parts)-1]
	found := false
	for _, w := range words {
		if w == last {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("prose cut landed mid-word: output ends %q", last)
	}
}

func TestDesignInstructionCapConfigurable(t *testing.T) {
	brief := longStructuredBrief()
	for _, cap := range []int{100, 250, 500} {
		out := DesignInstruction(brief, cap)
		if n := utf8.RuneCountInString(out); n > cap {
			t.Fatalf("cap %d: instruction is %d runes", cap, n)
		}
		if !strings.HasPrefix(out, "timbre: ") {
			t.Fatalf("cap %d: lost the top field: %q", cap, out[:32])
		}
	}
	// A cap so small that even the first field must be cut: the cut, not an
	// empty instruction, wins.
	out := DesignInstruction(brief, 10)
	if n := utf8.RuneCountInString(out); n > 10 {
		t.Fatalf("cap 10: instruction is %d runes", n)
	}
	if out == "" {
		t.Fatal("a tiny cap must cut the top field, not empty the instruction")
	}
}

func TestDesignInstructionEdgeCases(t *testing.T) {
	cases := []struct {
		name  string
		brief string
		cap   int
		want  string
	}{
		{"empty", "", 0, ""},
		{"whitespace", "   \n\t ", 0, ""},
		{"short prose passthrough", "a calm warm voice", 0, "a calm warm voice"},
		{"wrapped voice_brief key", `{"voice_brief":{"timbre":"ta","cadence":"ca"}}`, 0, "timbre: ta; pace: ca"},
		{"json with no string fields is prose", `{"count":3}`, 0, `{"count":3}`},
		{"edited brief keeps unknown string fields", `{"timbre":"ta","delivery":"crisp, forward"}`, 0, "timbre: ta; delivery: crisp, forward"},
		{"single word longer than cap hard-cuts", "uncharacteristically", 4, "unch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DesignInstruction(tc.brief, tc.cap)
			if got != tc.want {
				t.Fatalf("DesignInstruction(%q, %d) = %q, want %q", tc.brief, tc.cap, got, tc.want)
			}
			if n := utf8.RuneCountInString(got); tc.cap > 0 && n > tc.cap {
				t.Fatalf("got %d runes, cap is %d", n, tc.cap)
			}
		})
	}
}

// TestCreateAuditionsSendsCappedInstruction locks the service-level wiring:
// the Design call must carry the derived capped instruction while the
// audition rows keep the full brief (the UI and the audit trail see the
// untrimmed text; the provider sees the capped derivation).
func TestCreateAuditionsSendsCappedInstruction(t *testing.T) {
	svc, tt, _, _ := newTestService(t)
	brief := longStructuredBrief()
	rev, _, err := svc.CreateAuditions(ctx(t), "char-cap", brief, 3)
	if err != nil {
		t.Fatalf("CreateAuditions: %v", err)
	}
	want := DesignInstruction(brief, 0)
	if got := tt.lastDesignInstructions; got != want {
		t.Fatalf("Design got %q…\nwant %q…", got[:32], want[:32])
	}
	if rev.DesignInstruction != brief {
		t.Fatal("the revision row must keep the full brief, not the capped derivation")
	}
}

// fillDraft-level check with the cap exercised through the real store path.
func TestFillDraftHonoursInstructionCap(t *testing.T) {
	svc, tt, _, st := newTestService(t)
	brief := longStructuredBrief()
	svc.InstructionCap = 100
	rev, auditions, err := svc.CreateAuditions(ctx(t), "char-cap100", brief, 1)
	if err != nil {
		t.Fatalf("CreateAuditions: %v", err)
	}
	if n := utf8.RuneCountInString(tt.lastDesignInstructions); n > 100 {
		t.Fatalf("Design got a %d-rune instruction, want <= 100 (InstructionCap)", n)
	}
	// The audition row keeps the FULL brief, not the derived instruction.
	got, err := st.GetAudition(ctx(t), auditions[0].ID)
	if err != nil {
		t.Fatalf("GetAudition: %v", err)
	}
	if got.Brief != brief {
		t.Fatal("the audition row must record the full brief, not the capped derivation")
	}
	if rev.DesignInstruction != brief {
		t.Fatal("the revision row must keep the full brief, not the capped derivation")
	}
}
