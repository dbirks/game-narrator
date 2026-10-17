package generic

import (
	"testing"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/games/adapter"
	"github.com/dbirks/game-narrator/internal/games/textnorm"
)

// speakerByLabel indexes a parse result by its per-message speaker key, the
// way the importer consumes the sidecar.
type speakerTable map[string]int

// sidecarInvariants checks the lockstep contract between Messages, SpeakerKeys
// and Speakers on a whole result: a key on a message must be a known speaker,
// and every reported speaker must be used by at least one message.
func sidecarInvariants(t *testing.T, res *adapter.ParseResult) speakerTable {
	t.Helper()
	if len(res.SpeakerKeys) != len(res.Messages) {
		t.Fatalf("SpeakerKeys has %d entries for %d messages; the sidecar must stay parallel",
			len(res.SpeakerKeys), len(res.Messages))
	}
	known := map[string]bool{}
	for _, s := range res.Speakers {
		if s.Key == "" || s.DisplayName == "" {
			t.Fatalf("speaker %s has an empty key or display name: %+v", s.Key, s)
		}
		if !known[s.Key] {
			known[s.Key] = true
		} else {
			t.Fatalf("two Speakers entries share key %q; keys must be distinct", s.Key)
		}
	}
	used := map[string]bool{}
	for i, m := range res.Messages {
		key := res.SpeakerKeys[i]
		if key == "" {
			if m.AttributionStatus == games.AttributionUnknown && m.SpeakerID != "" {
				t.Errorf("message %q has no speaker key but carries a speaker id", m.SourceMessageID)
			}
			continue
		}
		if !known[key] {
			t.Fatalf("message %q carries key %q that no Speakers entry defines", m.SourceMessageID, key)
		}
		used[key] = true
	}
	for _, s := range res.Speakers {
		if !used[s.Key] {
			t.Fatalf("speaker %q is reported but no imported message carries it; Speakers must match the messages", s.Key)
		}
	}
	out := make(speakerTable, len(res.Speakers))
	for _, s := range res.Speakers {
		out[s.Key] = 0
	}
	return out
}

// TestParseResultNamesDistinctSpeakers feeds the synthetic CSV fixture — which
// has a speaker column with five named people and 18 unnamed rows — and checks
// the result carries exactly those five, with the right per-message keys.
func TestParseResultNamesDistinctSpeakers(t *testing.T) {
	res := parseFixture(t, "synthetic-adventure.csv", nil)
	byKey := sidecarInvariants(t, res)

	wantKeys := map[string]string{
		"marla":       "Marla",
		"corvin":      "Corvin",
		"boy":         "Boy",
		"têvance":     "Têvance",
		"chef ondine": "Chef Ondine",
	}
	if len(res.Speakers) != len(wantKeys) {
		t.Fatalf("speakers = %d, want %d: %+v", len(res.Speakers), len(wantKeys), res.Speakers)
	}
	for _, s := range res.Speakers {
		want, ok := wantKeys[s.Key]
		if !ok {
			t.Errorf("unexpected speaker %q (display %q)", s.Key, s.DisplayName)
			continue
		}
		if s.DisplayName != want {
			t.Errorf("speaker %q display = %q, want the source's own first spelling %q", s.Key, s.DisplayName, want)
		}
		if len(s.Aliases) != 0 {
			t.Errorf("speaker %q has aliases %v, the fixture spells each name one way", s.Key, s.Aliases)
		}
	}
	if res.SpeakerColumn != "speaker" {
		t.Errorf("SpeakerColumn = %q, want the header the labels came from", res.SpeakerColumn)
	}

	// Per-message: named rows carry the key, unnamed rows carry "".
	var named, unnamed int
	for i, m := range res.Messages {
		key := res.SpeakerKeys[i]
		if key == "" {
			unnamed++
			if m.AttributionStatus != games.AttributionUnknown {
				t.Errorf("unnamed row %q has attribution %q, want unknown", m.SourceMessageID, m.AttributionStatus)
			}
			continue
		}
		named++
		byKey[key]++
	}
	if named != 22 || unnamed != 18 {
		t.Errorf("named = %d, unnamed = %d; the fixture has 22 named and 18 unnamed rows", named, unnamed)
	}
	for key, want := range map[string]int{"marla": 6, "corvin": 6, "boy": 4, "têvance": 4, "chef ondine": 2} {
		if byKey[key] != want {
			t.Errorf("key %q is on %d messages, want %d", key, byKey[key], want)
		}
	}
}

// TestSpeakerVariantsFoldToOneKey proves the matching rule: labels that differ
// only in case or whitespace are one speaker, with the first spelling as the
// display name and the rest as aliases.
func TestSpeakerVariantsFoldToOneKey(t *testing.T) {
	res, err := parseString(t,
		"line,speaker\nFirst,Marla\nclose the door,marla\nThe third, MARLA \n",
		func(o *adapter.ParseOptions) { o.Format = FormatCSV })
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	sidecarInvariants(t, res)
	if len(res.Speakers) != 1 {
		t.Fatalf("speakers = %d, want one: %+v", len(res.Speakers), res.Speakers)
	}
	s := res.Speakers[0]
	if s.Key != "marla" {
		t.Errorf("key = %q, want the textnorm fold %q", s.Key, textnorm.Normalize("Marla", textnorm.Options{}))
	}
	if s.DisplayName != "Marla" {
		t.Errorf("display = %q, want the first spelling as written", s.DisplayName)
	}
	if len(s.Aliases) != 2 || s.Aliases[0] != "marla" || s.Aliases[1] != "MARLA" {
		t.Errorf("aliases = %v, want the other spellings in first-occurrence order", s.Aliases)
	}
	for i, k := range res.SpeakerKeys {
		if k != "marla" {
			t.Errorf("message %d key = %q, want marla", i, k)
		}
	}
}

// TestNoSpeakerColumnMeansNoSpeakers is the half of the "never invent a
// speaker" rule that pre-existed: a file without a speaker column reports no
// speakers at all, and every sidecar entry is empty.
func TestNoSpeakerColumnMeansNoSpeakers(t *testing.T) {
	res, err := parseString(t, "line,kind\nOne,dialogue\nTwo,sign\n",
		func(o *adapter.ParseOptions) { o.Format = FormatCSV })
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	sidecarInvariants(t, res)
	if len(res.Speakers) != 0 {
		t.Errorf("speakers = %+v, want none: the adapter must not invent speakers", res.Speakers)
	}
	if res.SpeakerColumn != "" {
		t.Errorf("SpeakerColumn = %q, want empty when the source has no speaker column", res.SpeakerColumn)
	}
	for i, k := range res.SpeakerKeys {
		if k != "" {
			t.Errorf("message %d key = %q, want empty", i, k)
		}
	}
}

// TestExplicitSpeakerColumnMapping drives the JSON path with an explicit
// ColumnMap.Speaker, so the sidecar is proven on the second reader family and
// with a caller-named (not auto-mapped) column.
func TestExplicitSpeakerColumnMapping(t *testing.T) {
	body := `[{"utterance_id": 1, "who_speaks": "The Ferryman", "line": "No coin, no crossing."},
		{"utterance_id": 2, "who_speaks": null, "line": "A sign, half sunk."},
		{"utterance_id": 3, "who_speaks": "the ferryman", "line": "The rule is the whole of it."}]`
	res, err := parseString(t, body, func(o *adapter.ParseOptions) {
		o.Format = FormatJSON
		o.ColumnMap = adapter.ColumnMap{Text: "line", ID: "utterance_id", Speaker: "who_speaks"}
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	sidecarInvariants(t, res)
	if len(res.Speakers) != 1 {
		t.Fatalf("speakers = %d, want one: %+v", len(res.Speakers), res.Speakers)
	}
	s := res.Speakers[0]
	if s.DisplayName != "The Ferryman" || s.Key != "the ferryman" {
		t.Errorf("speaker = %+v, want display The Ferryman, key the ferryman", s)
	}
	if len(s.Aliases) != 1 || s.Aliases[0] != "the ferryman" {
		t.Errorf("aliases = %v, want the second spelling", s.Aliases)
	}
	if res.SpeakerColumn != "who_speaks" {
		t.Errorf("SpeakerColumn = %q, want the explicitly mapped column", res.SpeakerColumn)
	}
	want := []string{"the ferryman", "", "the ferryman"}
	for i := range want {
		if res.SpeakerKeys[i] != want[i] {
			t.Errorf("message %d key = %q, want %q", i, res.SpeakerKeys[i], want[i])
		}
	}
	// The null speaker must not become the string "null" either in the sidecar.
	if res.SpeakerKeys[1] != "" || res.Messages[1].AttributionStatus != games.AttributionUnknown {
		t.Errorf("null speaker: key %q, status %q", res.SpeakerKeys[1], res.Messages[1].AttributionStatus)
	}
}

// TestSpeakerLabelThatFoldsToNothing is the edge a label-only column can reach
// under a punctuation-stripping text norm: a name made purely of punctuation
// folds to the empty string, so it is kept as evidence (proposed attribution,
// label in flags) but has no key to reconcile.
func TestSpeakerLabelThatFoldsToNothing(t *testing.T) {
	res, err := parseString(t, "line,speaker\nFirst,!\nSecond,\n",
		func(o *adapter.ParseOptions) {
			o.Format = FormatCSV
			o.TextNorm = textnorm.Options{StripPunctuation: true}
		})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	sidecarInvariants(t, res)
	if len(res.Speakers) != 0 {
		t.Errorf("a label that folds to nothing reported a speaker: %+v", res.Speakers)
	}
	if res.Messages[0].AttributionStatus != games.AttributionProposed {
		t.Errorf("label-only row = %q, want proposed with the label kept as evidence", res.Messages[0].AttributionStatus)
	}
	if res.SpeakerKeys[0] != "" {
		t.Errorf("key for the punctuation label = %q, want empty", res.SpeakerKeys[0])
	}
}
