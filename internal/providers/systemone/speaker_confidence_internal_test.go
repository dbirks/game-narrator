package systemone

// The ADR-0156 speaker confidence: Interpret carries the adjudicator's
// confidence for the chosen speaker option (the chosen option's probability)
// onto the adjudication, and an answer that named no speaker reads as no
// confidence. Internal-package tests: the gate the runtime applies lives in
// the runtime, and this pins the signal's source.

import "testing"

func TestInterpretSpeakerConfidence(t *testing.T) {
	dec := Decision{
		Model: "clef-omni",
		Answers: map[string]Answer{
			QIsTextbox:   {Type: TypeNoul, Noul: 0.9},
			QTextKind:    {Type: TypeChoice, Choice: TextKindDialogue, Confidence: 0.9, Probabilities: map[string]float64{TextKindDialogue: 0.9, TextKindSign: 0.05, TextKindNone: 0.05}},
			QSpeaker:     {Type: TypeChoice, Choice: "sp1", Confidence: 0.8, Probabilities: map[string]float64{"sp1": 0.2, "sp2": 0.8, SpeakerUnknown: 0.0}},
			QCandidate:   {Type: TypeChoice, Choice: "c1", Confidence: 0.9, Probabilities: map[string]float64{"c1": 0.9, CandidateNone: 0.1}},
			QIsComplete:  {Type: TypeNoul, Noul: 0.9},
			QSafeToSpeak: {Type: TypeNoul, Noul: 0.9},
		},
	}
	adj := Interpret(dec, []Candidate{{ID: "m-8", Text: "invented line"}}, []Speaker{{ID: "ww:spk:7"}})
	if adj.Speaker != "ww:spk:7" {
		t.Fatalf("Interpret.Speaker = %q, want the mapped-back key %q", adj.Speaker, "ww:spk:7")
	}
	if adj.SpeakerConfidence != 0.8 {
		t.Fatalf("Interpret.SpeakerConfidence = %v, want the chosen option's probability 0.8", adj.SpeakerConfidence)
	}
}

func TestInterpretSpeakerConfidenceUnknownIsZero(t *testing.T) {
	// An answer that named no speaker (unknown) carries no speaker
	// confidence the runtime may act on: the named-speaker gate reads it as
	// no name. The answer's own probability is recorded, but the gate treats
	// the unknown option as unnamed regardless.
	dec := Decision{
		Answers: map[string]Answer{
			QSpeaker: {Type: TypeChoice, Choice: SpeakerUnknown, Confidence: 0.4, Probabilities: map[string]float64{SpeakerUnknown: 0.4}},
		},
	}
	adj := Interpret(dec, nil, nil)
	if adj.Speaker != SpeakerUnknown {
		t.Fatalf("Interpret.Speaker = %q, want %q", adj.Speaker, SpeakerUnknown)
	}
}

func TestInterpretSpeakerKindConfidence(t *testing.T) {
	// ADR-0158: the kind answer's confidence is the chosen kind option's
	// probability, and the kind gate reads it the same way the speaker gate
	// reads the speaker answer's (the chosen option's probability; the
	// runtime's floor is SpeakerConfidenceFloor).
	dec := Decision{
		Answers: map[string]Answer{
			QIsTextbox:   {Type: TypeNoul, Noul: 0.9},
			QTextKind:    {Type: TypeChoice, Choice: TextKindDialogue, Confidence: 0.9, Probabilities: map[string]float64{TextKindDialogue: 0.9, TextKindSign: 0.05, TextKindNone: 0.05}},
			QSpeaker:     {Type: TypeChoice, Choice: "sp1", Confidence: 0.8, Probabilities: map[string]float64{"sp1": 0.2, "sp2": 0.8, SpeakerUnknown: 0.0}},
			QSpeakerKind: {Type: TypeChoice, Choice: SpeakerKindAdultMale, Confidence: 0.75, Probabilities: map[string]float64{SpeakerKindAdultMale: 0.75, SpeakerKindAdultFemale: 0.05, SpeakerKindBoy: 0.05, SpeakerKindGirl: 0.03, SpeakerKindElderMale: 0.04, SpeakerKindElderFemale: 0.03, SpeakerKindCreature: 0.05, SpeakerUnknown: 0.01}},
			QCandidate:   {Type: TypeChoice, Choice: "c1", Confidence: 0.9, Probabilities: map[string]float64{"c1": 0.9, CandidateNone: 0.1}},
			QIsComplete:  {Type: TypeNoul, Noul: 0.9},
			QSafeToSpeak: {Type: TypeNoul, Noul: 0.9},
		},
	}
	adj := Interpret(dec, []Candidate{{ID: "m-8", Text: "invented line"}}, []Speaker{{ID: "ww:spk:7"}})
	if adj.SpeakerKind != SpeakerKindAdultMale {
		t.Fatalf("Interpret.SpeakerKind = %q, want %q", adj.SpeakerKind, SpeakerKindAdultMale)
	}
	if adj.SpeakerKindConfidence != 0.75 {
		t.Fatalf("Interpret.SpeakerKindConfidence = %v, want the chosen option's probability 0.75", adj.SpeakerKindConfidence)
	}
}

func TestInterpretSpeakerKindAbsentIsZeroAndUnknownPassesThrough(t *testing.T) {
	// An absent kind answer reads as no kind and no confidence: the runtime's
	// kind gate then keeps the narrator fallback (ADR-0158). An answer that
	// picked the unknown option keeps the unknown meaning and its own
	// confidence, which the gate treats as no kind.
	adj := Interpret(Decision{Answers: map[string]Answer{
		QIsTextbox:   {Type: TypeNoul, Noul: 0.9},
		QTextKind:    {Type: TypeChoice, Choice: TextKindDialogue, Confidence: 0.9, Probabilities: map[string]float64{TextKindDialogue: 0.9, TextKindSign: 0.05, TextKindNone: 0.05}},
		QSpeaker:     {Type: TypeChoice, Choice: SpeakerUnknown, Confidence: 0.4, Probabilities: map[string]float64{SpeakerUnknown: 0.4}},
		QIsComplete:  {Type: TypeNoul, Noul: 0.9},
		QSafeToSpeak: {Type: TypeNoul, Noul: 0.9},
	}}, nil, nil)
	if adj.SpeakerKind != "" {
		t.Fatalf("Interpret.SpeakerKind = %q, want empty when the kind answer is absent", adj.SpeakerKind)
	}
	if adj.SpeakerKindConfidence != 0 {
		t.Fatalf("Interpret.SpeakerKindConfidence = %v, want 0 when the kind answer is absent", adj.SpeakerKindConfidence)
	}
	adj = Interpret(Decision{Answers: map[string]Answer{
		QIsTextbox:   {Type: TypeNoul, Noul: 0.9},
		QTextKind:    {Type: TypeChoice, Choice: TextKindDialogue, Confidence: 0.9, Probabilities: map[string]float64{TextKindDialogue: 0.9, TextKindSign: 0.05, TextKindNone: 0.05}},
		QSpeaker:     {Type: TypeChoice, Choice: SpeakerUnknown, Confidence: 0.4, Probabilities: map[string]float64{SpeakerUnknown: 0.4}},
		QSpeakerKind: {Type: TypeChoice, Choice: SpeakerUnknown, Confidence: 0.3, Probabilities: map[string]float64{SpeakerKindAdultMale: 0.1, SpeakerKindAdultFemale: 0.1, SpeakerKindBoy: 0.1, SpeakerKindGirl: 0.1, SpeakerKindElderMale: 0.1, SpeakerKindElderFemale: 0.1, SpeakerKindCreature: 0.1, SpeakerUnknown: 0.3}},
		QIsComplete:  {Type: TypeNoul, Noul: 0.9},
		QSafeToSpeak: {Type: TypeNoul, Noul: 0.9},
	}}, nil, nil)
	if adj.SpeakerKind != SpeakerUnknown {
		t.Fatalf("Interpret.SpeakerKind = %q, want %q when the kind answer is unknown", adj.SpeakerKind, SpeakerUnknown)
	}
	if adj.SpeakerKindConfidence != 0.3 {
		t.Fatalf("Interpret.SpeakerKindConfidence = %v, want the unknown option's probability 0.3", adj.SpeakerKindConfidence)
	}
}
