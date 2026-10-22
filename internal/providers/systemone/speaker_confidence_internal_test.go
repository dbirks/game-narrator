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
