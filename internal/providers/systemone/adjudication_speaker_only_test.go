package systemone

// The ADR-0160 early-speaker adjudication: the speaker-only bundle and the
// answer mapping it carries. Internal-package tests: the bundle shape and the
// answer mapping are the two halves of the early ask — the bundle renders the
// attribution questions a confident local top candidate on a shared slot owes,
// and Interpret maps the answers back to the speaker + kind the runtime's gates
// read.

import "testing"

func TestBuildAdjudicationSpeakerOnlyCarriesAttributionOnly(t *testing.T) {
	bundle := BuildAdjudicationSpeakerOnly(
		Context{Scene: "outset", OCR: "the boy shouts back", LastMessage: "m-7"},
		[]Speaker{
			{ID: "char-boy", Description: "a boy"},
			{ID: "char-villager", Description: "a villager"},
		},
	)
	bundle.Model = "clef-omni"
	if err := bundle.validate(limits{}.defaults()); err != nil {
		t.Fatalf("the speaker-only bundle is not wire-valid: %v", err)
	}
	if len(bundle.Questions) != 2 {
		t.Fatalf("speaker-only bundle has %d questions, want 2 (speaker + speaker_kind)", len(bundle.Questions))
	}
	if bundle.Questions[0].ID != QSpeaker || bundle.Questions[0].Type != TypeChoice {
		t.Fatalf("question 0 = %s/%s, want speaker/choice", bundle.Questions[0].ID, bundle.Questions[0].Type)
	}
	if bundle.Questions[1].ID != QSpeakerKind || bundle.Questions[1].Type != TypeChoice {
		t.Fatalf("question 1 = %s/%s, want speaker_kind/choice", bundle.Questions[1].ID, bundle.Questions[1].Type)
	}
	if bundle.Questions[0].Instructions == "" || bundle.Questions[1].Instructions == "" {
		t.Fatalf("a question without instructions is invalid: %q / %q", bundle.Questions[0].Instructions, bundle.Questions[1].Instructions)
	}
	if len(bundle.Questions[0].Criteria) != 3 {
		t.Fatalf("speaker options = %d, want the speakers plus the unknown fallback (%+v)", len(bundle.Questions[0].Criteria), bundle.Questions[0].Criteria)
	}
	wantIDs := []string{"sp1", "sp2", SpeakerUnknown}
	for i, opt := range bundle.Questions[0].Criteria {
		if opt.ID != wantIDs[i] {
			t.Fatalf("speaker option %d id = %q, want %q", i, opt.ID, wantIDs[i])
		}
	}
	if len(bundle.Questions[1].Criteria) != len(SpeakerKindOptions()) {
		t.Fatalf("speaker_kind options = %d, want %d", len(bundle.Questions[1].Criteria), len(SpeakerKindOptions()))
	}
	seen := map[string]bool{}
	for _, o := range bundle.Questions[1].Criteria {
		if seen[o.ID] {
			t.Fatalf("speaker_kind option id %q is not unique", o.ID)
		}
		seen[o.ID] = true
	}
	// The early ask carries no candidate question: the line the commit will
	// speak is the local policy's confident top candidate, and the answer may
	// not narrow it.
	for _, q := range bundle.Questions {
		switch q.ID {
		case QCandidate, QIsTextbox, QTextKind, QIsComplete, QSafeToSpeak:
			t.Fatalf("the speaker-only bundle must not carry the %q question", q.ID)
		}
	}
}

func TestInterpretSpeakerOnlyAnswers(t *testing.T) {
	// The answers a speaker-only bundle's model produces: the speaker answer
	// (the spN alias the model picked, with the chosen option's probability as
	// the confidence) and the kind answer (the kind id, with its probability).
	dec := Decision{
		Model: "clef-omni",
		Answers: map[string]Answer{
			QSpeaker: {Type: TypeChoice, Choice: "sp1", Confidence: 0.9, Probabilities: map[string]float64{"sp1": 0.9, "sp2": 0.1, SpeakerUnknown: 0.0}},
			QSpeakerKind: {Type: TypeChoice, Choice: SpeakerKindBoy, Confidence: 0.9, Probabilities: map[string]float64{
				SpeakerKindAdultMale: 0.1, SpeakerKindAdultFemale: 0.05, SpeakerKindBoy: 0.9, SpeakerKindGirl: 0.03,
				SpeakerKindElderMale: 0.02, SpeakerKindElderFemale: 0.02, SpeakerKindCreature: 0.01, SpeakerUnknown: 0.01,
			}},
		},
	}
	adj := Interpret(dec, []Candidate{{ID: "m-8", Text: "the boy shouts back"}}, []Speaker{{ID: "char-boy", Description: "a boy"}})
	if adj.Speaker != "char-boy" {
		t.Fatalf("Interpret.Speaker = %q, want the mapped-back speaker key %q", adj.Speaker, "char-boy")
	}
	if adj.SpeakerConfidence != 0.9 {
		t.Fatalf("Interpret.SpeakerConfidence = %v, want the chosen option's probability 0.9", adj.SpeakerConfidence)
	}
	if adj.SpeakerKind != SpeakerKindBoy {
		t.Fatalf("Interpret.SpeakerKind = %q, want the kind answer %q", adj.SpeakerKind, SpeakerKindBoy)
	}
	if adj.SpeakerKindConfidence != 0.9 {
		t.Fatalf("Interpret.SpeakerKindConfidence = %v, want the chosen kind option's probability 0.9", adj.SpeakerKindConfidence)
	}
	// No candidate question was asked, so the answer may not name a line.
	if adj.Candidate != "" {
		t.Fatalf("Interpret.Candidate = %q, want empty for a speaker-only answer", adj.Candidate)
	}
	if adj.CandidateProbabilities != nil && len(adj.CandidateProbabilities) > 0 {
		t.Fatalf("Interpret.CandidateProbabilities = %v, want none for a speaker-only answer", adj.CandidateProbabilities)
	}
}
