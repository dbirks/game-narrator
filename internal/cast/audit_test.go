package cast

import (
	"testing"

	"github.com/dbirks/game-narrator/internal/speech"
)

func TestExpectedClass(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		{"A tall man in a red cap, the player character.", ClassMale},
		{"An old man with a deep voice.", ClassMale},
		{"A female knight, young and confident.", ClassFemale},
		{"An old woman who sings lullabies.", ClassFemale},
		{"A little girl with a bell on her collar.", ClassFemale},
		{"A boy who sells fruit.", ClassChild},
		{"A child sprite with a high voice.", ClassChild},
		{"A creature with no words of its own.", ClassUnknown},
		{"A mysterious traveller whose sex is never mentioned.", ClassUnknown},
		{"A child, grown now into an old man.", ClassMale}, // later descriptor wins
		{"a WOMAN of modest means.", ClassFemale},          // case-insensitive
	}
	for _, c := range cases {
		if got := ExpectedClass(c.text); got != c.want {
			t.Errorf("ExpectedClass(%q) = %q, want %q", c.text, got, c.want)
		}
	}
}

func TestKindToClass(t *testing.T) {
	cases := map[string]string{
		"adult_male":        ClassMale,
		"kind:elder_male":   ClassMale,
		"adult_female":      ClassFemale,
		"kind:elder_female": ClassFemale,
		"girl":              ClassFemale,
		"kind:boy":          ClassChild,
		"creature":          ClassUnknown,
		"unknown":           ClassUnknown,
		"":                  ClassUnknown,
	}
	for id, want := range cases {
		if got := KindToClass(id); got != want {
			t.Errorf("KindToClass(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestVerdict(t *testing.T) {
	cases := []struct {
		expected string
		f0       float64
		want     string
	}{
		{ClassMale, 110, VerdictOK},
		{ClassFemale, 220, VerdictOK},
		{ClassChild, 320, VerdictOK},
		{ClassMale, 220, VerdictMismatch},   // a female voice on a man
		{ClassFemale, 110, VerdictMismatch}, // a male voice on a woman
		{ClassChild, 110, VerdictMismatch},
		{ClassMale, 255, VerdictMismatch}, // adult-female ceiling, not a child
		{ClassMale, 256, VerdictMismatch}, // child floor
		{ClassUnknown, 220, VerdictUnknown},
		{ClassMale, 0, VerdictUnknown}, // unmeasurable is not evidence
	}
	for _, c := range cases {
		if got := Verdict(c.expected, c.f0); got != c.want {
			t.Errorf("Verdict(%q, %v) = %q, want %q", c.expected, c.f0, got, c.want)
		}
	}
}

// TestMeasuredClassesFromSyntheticTones drives the real YIN estimator with
// synthetic 16-bit tones at 110 / 220 / 320 Hz and asserts the measured F0
// lands in the expected class — the audit's measurement leg, end to end,
// with no recorded audio.
func TestMeasuredClassesFromSyntheticTones(t *testing.T) {
	cases := []struct {
		freq float64
		want string
	}{
		{110, ClassMale},
		{220, ClassFemale},
		{320, ClassChild},
	}
	for _, c := range cases {
		wav := speech.MakeToneWAV(c.freq, 1.5, 44100)
		p, err := speech.MeasurePitch(wav)
		if err != nil {
			t.Fatalf("MeasurePitch(%g Hz tone): %v", c.freq, err)
		}
		if p.F0 <= 0 {
			t.Fatalf("MeasurePitch(%g Hz tone): no voiced frames", c.freq)
		}
		if got := ClassOfF0(p.F0); got != c.want {
			t.Errorf("class of %g Hz tone = %q (F0 %.1f), want %q", c.freq, got, p.F0, c.want)
		}
	}
}

func TestRowFor(t *testing.T) {
	// A mismatch: profile text says man, catalog kind says woman, measured
	// F0 is female-range.
	r := RowFor(Character{
		ProfileName:  "Test Knight",
		ProfileText:  "A brave man and knight.",
		MeasuredF0:   215,
		VoicedFrames: 40,
		HasVoice:     true,
		VoiceID:      "vn_test",
	})
	if r.Expected != ClassMale || r.Verdict != VerdictMismatch {
		t.Errorf("RowFor man/female-f0 = %+v, want expected male verdict MISMATCH", r)
	}
	// No gender words: falls back to the catalog kind.
	r2 := RowFor(Character{
		ProfileName: "Mystery Sprite",
		KindID:      "kind:adult_female",
		MeasuredF0:  210,
		HasVoice:    true,
	})
	if r2.Expected != ClassFemale || r2.Verdict != VerdictOK {
		t.Errorf("RowFor kind fallback = %+v, want expected female verdict OK", r2)
	}
	// Neither text nor kind: UNKNOWN.
	r3 := RowFor(Character{ProfileName: "Silent One", MeasuredF0: 210, HasVoice: true})
	if r3.Verdict != VerdictUnknown {
		t.Errorf("RowFor no descriptor = %+v, want UNKNOWN", r3)
	}
}
