// The cast audit (ADR-0171): checks that each character's assigned voice
// actually sounds like the character. The measurement leg reuses the ADR-0162
// YIN pitch estimator (internal/speech.MeasurePitch) on the voice's reference
// WAV; this package holds the pure comparison leg — classifying a
// character's expected voice class from its profile data (description, role,
// voice brief text, or the ADR-0158 speaker_kind id of a catalog voice) and
// judging the expected class against the measured F0.
//
// Every log-safe value here is the character's own profile name plus class
// labels and Hz numbers; nothing here ever carries game text (AGENTS.md
// rule 5).

package cast

import (
	"regexp"
	"strings"
)

// Voice classes — the same vocabulary internal/speech.VoiceClass emits for a
// measured F0, so a verdict comparison is a plain string equality.
const (
	ClassMale    = "low (adult male)"
	ClassFemale  = "mid (adult female)"
	ClassChild   = "high (child)"
	ClassUnknown = "unknown" // no gender words found, or a creature
)

// Verdicts for one character's audit row.
const (
	VerdictOK       = "OK"       // expected and measured classes agree
	VerdictMismatch = "MISMATCH" // expected a class, measured a different one
	VerdictUnknown  = "UNKNOWN"  // no expected class, or the F0 was unmeasurable
)

// expectedWords is the profile-text keyword table: each keyword implies a
// class. Precedence follows the text, not the table: the match at the latest
// position in the profile text wins, so a later descriptor ("... an old
// man, not a child") overrides an earlier one.
var expectedWords = []struct {
	keyword string
	class   string
}{
	{"male", ClassMale},
	{"man", ClassMale},
	{"female", ClassFemale},
	{"woman", ClassFemale},
	{"girl", ClassFemale},
	{"lady", ClassFemale},
	{"boy", ClassChild},
	{"child", ClassChild},
	{"kid", ClassChild},
	{"creature", ClassUnknown}, // a creature has no gender expectation
}

// wordPattern guards the matching to whole words: "man" must not fire on
// "woman", "child" on "children's wardrobe".
var wordPattern = map[string]*regexp.Regexp{}

func wordMatch(keyword string) *regexp.Regexp {
	if re, ok := wordPattern[keyword]; ok {
		return re
	}
	re := regexp.MustCompile(`\b` + keyword + `s?\b`)
	wordPattern[keyword] = re
	return re
}

// ExpectedClass classifies a character's expected voice class from its
// profile text (description / role / voice brief, lowercased or raw — the
// matching is case-insensitive). It returns ClassUnknown when no gender word
// occurs, or when a creature word is the strongest descriptor (a creature
// has no gender to match against).
func ExpectedClass(profileText string) string {
	text := strings.ToLower(profileText)
	class, best := ClassUnknown, -1
	for _, ew := range expectedWords {
		loc := wordMatch(ew.keyword).FindStringIndex(text)
		if loc != nil && loc[0] > best {
			class, best = ew.class, loc[0]
		}
	}
	return class
}

// KindToClass maps an ADR-0158 speaker_kind id (with or without the roster's
// "kind:" prefix) to a voice class. Unknown or empty ids map to ClassUnknown.
func KindToClass(kindID string) string {
	k := strings.TrimPrefix(kindID, "kind:")
	switch k {
	case "adult_male", "elder_male":
		return ClassMale
	case "adult_female", "elder_female", "girl":
		return ClassFemale
	case "boy":
		return ClassChild
	default: // "creature", "unknown", "" — no gender to match against
		return ClassUnknown
	}
}

// Verdict judges one character's expected class against its voice's measured
// median F0 (Hz; 0 = the estimator measured nothing).
func Verdict(expected string, measuredF0 float64) string {
	if expected == ClassUnknown {
		return VerdictUnknown
	}
	if measuredF0 <= 0 {
		return VerdictUnknown // an unmeasurable clip is not evidence
	}
	switch {
	case expected == ClassMale && measuredF0 < 165.0:
		return VerdictOK
	case expected == ClassFemale && measuredF0 >= 165.0 && measuredF0 <= 255.0:
		return VerdictOK
	case expected == ClassChild && measuredF0 > 255.0:
		return VerdictOK
	default:
		return VerdictMismatch
	}
}

// Character is one auditable roster row: the profile name (the only log-safe
// string), the concatenated profile text the expected class reads, the
// speaker_kind id when the voice came from a catalog entry, and the measured
// F0 (0 = unmeasurable).
type Character struct {
	ProfileName  string
	ProfileText  string  // description + role + voice brief
	KindID       string  // ADR-0158 speaker_kind id, "" when none
	MeasuredF0   float64 // median voiced-frame F0 in Hz, 0 when unmeasurable
	VoicedFrames int
	HasVoice     bool
	VoiceID      string // the assigned voice revision, "" when none
}

// Row is one character's audit result.
type Row struct {
	Character    string  `json:"character"`
	VoiceID      string  `json:"voice_id,omitempty"`
	Expected     string  `json:"expected"`
	MeasuredF0Hz float64 `json:"measured_f0_hz"`
	Measured     string  `json:"measured"`
	Verdict      string  `json:"verdict"`
}

// RowFor fills the audit row for one character. The expected class prefers
// the profile text; a catalog kind id is the fallback when the text carries
// no gender words. The measured class uses the same vocabulary
// (internal/speech.VoiceClass) so the two are directly comparable.
func RowFor(c Character) Row {
	expected := ExpectedClass(c.ProfileText)
	if expected == ClassUnknown && c.KindID != "" {
		expected = KindToClass(c.KindID)
	}
	measured := ClassUnknown
	if c.MeasuredF0 > 0 {
		measured = ClassOfF0(c.MeasuredF0)
	}
	return Row{
		Character:    c.ProfileName,
		VoiceID:      c.VoiceID,
		Expected:     expected,
		MeasuredF0Hz: c.MeasuredF0,
		Measured:     measured,
		Verdict:      Verdict(expected, c.MeasuredF0),
	}
}

// ClassOfF0 mirrors internal/speech.VoiceClass's vocabulary without taking a
// dependency on its decoder (pure comparison leg, ADR-0171).
func ClassOfF0(f0 float64) string {
	switch {
	case f0 < 165.0:
		return ClassMale
	case f0 <= 255.0:
		return ClassFemale
	default:
		return ClassChild
	}
}
