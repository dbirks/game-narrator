package systemone

import (
	"fmt"
	"time"
)

// This file is the glue between the runtime's recognition loop
// (docs/spec/05 "Clef adjudication and commit policy") and the System One
// wire protocol: BuildAdjudication renders one observation plus its ranked
// candidates as the bundled typed questions the spec mandates, and Interpret
// maps a Decision back to message ids. Neither calibrates anything — the
// probabilities come back raw, because docs/spec/06 says they are signals for
// a per-game/per-scene calibrated commitment policy, not reliability
// guarantees.

// The bundled question ids. They are the stable names the runtime's event
// trace (docs/spec/17 decision.resolved) and the cassette tests refer to.
const (
	QIsTextbox   = "is_textbox"
	QTextKind    = "text_kind"
	QSpeaker     = "speaker"
	QCandidate   = "candidate_message"
	QIsComplete  = "is_complete"
	QSafeToSpeak = "safe_to_speak"
)

// The text_kind options, from the System One example in docs/spec/06.
const (
	TextKindDialogue = "dialogue"
	TextKindSign     = "sign"
	TextKindNone     = "none"
)

// The fallback option ids every bundled choice question carries.
const (
	SpeakerUnknown = "unknown"
	CandidateNone  = "none"
)

// Context is the observation context sent as the request's "state"
// (free-form; the spec example uses scene / ocr / last_message).
type Context struct {
	// Scene is the scene hint (for example "outset").
	Scene string
	// OCR is the normalised observed text of the current frame.
	OCR string
	// LastMessage is the id of the last committed message, when known.
	LastMessage string
}

func (c Context) state() map[string]any {
	st := make(map[string]any, 3)
	if c.Scene != "" {
		st["scene"] = c.Scene
	}
	if c.OCR != "" {
		st["ocr"] = c.OCR
	}
	if c.LastMessage != "" {
		st["last_message"] = c.LastMessage
	}
	return st
}

// Candidate is one ranked message candidate. ID is the message id the runtime
// knows it by (for example "m-12"); Text is the normalised candidate text.
type Candidate struct {
	ID   string
	Text string
}

// Speaker is a declared speaker option for the speaker question.
type Speaker struct {
	ID          string
	Description string
}

// maxBundleCandidates caps the candidate_message options so the bundled
// request stays under the option cap even after adding the "none" option
// (docs/spec/05 "Limit candidate options to supported request cap").
const maxBundleCandidates = MaxOptions - 1

// BuildAdjudication builds the bundled decision request for one observation:
// is_textbox (noul), text_kind (choice), speaker (choice incl. unknown),
// candidate_message (choice incl. none, with deterministic candidate ids
// c1..cN), is_complete (noul), safe_to_speak (noul) — in that order, which is
// the order the questions object is serialised in.
//
// The returned Request has an empty Model; Decide fills it from the client's
// model. Candidates beyond maxBundleCandidates are dropped (the ranking is
// the caller's job); empty speaker ids are replaced by unknown.
func BuildAdjudication(obs Context, candidates []Candidate, speakers []Speaker) Request {
	questions := make([]Question, 0, 6)
	questions = append(questions, Question{
		ID:           QIsTextbox,
		Type:         TypeNoul,
		Instructions: "Is the visible UI region a textbox (narratable dialogue) in this screenshot?",
	})
	questions = append(questions, Question{
		ID:           QTextKind,
		Type:         TypeChoice,
		Instructions: "What kind of text is visible?",
		Criteria: []Option{
			{ID: TextKindDialogue, Description: "a character speaks"},
			{ID: TextKindSign, Description: "an interacted sign or document"},
			{ID: TextKindNone, Description: "no narratable text"},
		},
	})
	speakerOpts := make([]Option, 0, len(speakers)+1)
	for _, s := range speakers {
		id := s.ID
		if id == "" {
			id = SpeakerUnknown
		}
		speakerOpts = append(speakerOpts, Option{ID: id, Description: s.Description})
	}
	speakerOpts = append(speakerOpts, Option{ID: SpeakerUnknown, Description: "cannot be established from the frame"})
	questions = append(questions, Question{
		ID:           QSpeaker,
		Type:         TypeChoice,
		Instructions: "Who is speaking, if anyone is?",
		Criteria:     speakerOpts,
	})
	n := len(candidates)
	if n > maxBundleCandidates {
		n = maxBundleCandidates
	}
	candOpts := make([]Option, 0, n+1)
	for i := 0; i < n; i++ {
		candOpts = append(candOpts, Option{ID: fmt.Sprintf("c%d", i+1), Description: candidates[i].Text})
	}
	candOpts = append(candOpts, Option{ID: CandidateNone, Description: "none of the candidates matches the visible text"})
	questions = append(questions, Question{
		ID:           QCandidate,
		Type:         TypeChoice,
		Instructions: "Which candidate matches the visible text?",
		Criteria:     candOpts,
	})
	questions = append(questions, Question{
		ID:           QIsComplete,
		Type:         TypeNoul,
		Instructions: "Is the visible textbox content complete (not cut off by the frame or a page break)?",
	})
	questions = append(questions, Question{
		ID:           QSafeToSpeak,
		Type:         TypeNoul,
		Instructions: "Is it safe to speak this message now (no critical choice or warning on screen)?",
	})
	return Request{
		State:     obs.state(),
		Questions: questions,
	}
}

// Adjudication is Interpret's output: the bundled answers mapped back to
// message ids. The float fields are the raw model signals (nil when the
// answer is absent); Candidate is the message id chosen by the model, or ""
// when the model picked "none" or the answer was missing. No threshold is
// applied anywhere here.
type Adjudication struct {
	// Model and provenance of the underlying decision.
	Model     string
	Latency   time.Duration
	RequestID string
	Origin    string
	// IsTextbox is the raw noul signal for is_textbox.
	IsTextbox *float64
	// TextKind and Speaker are the chosen option ids.
	TextKind string
	Speaker  string
	// Candidate is the message id ("" = none / missing).
	Candidate string
	// CandidateProbabilities maps message ids to raw probabilities.
	CandidateProbabilities map[string]float64
	// IsComplete and SafeToSpeak are the raw noul signals.
	IsComplete  *float64
	SafeToSpeak *float64
}

// Interpret maps a Decision for a BuildAdjudication request back to message
// ids. The c1..cN candidate option ids are positional: cN is the Nth element
// of the candidates slice the request was built with, so that slice is the
// second argument; pass nil (or leave the candidate answer missing) and the
// raw cN ids come through instead. No threshold is applied anywhere — the
// floats are the raw model signals (docs/spec/06) and a missing answer simply
// leaves its field empty.
func Interpret(d Decision, candidates []Candidate) Adjudication {
	out := Adjudication{
		Model:                  d.Model,
		Latency:                d.Latency,
		RequestID:              d.RequestID,
		Origin:                 d.Origin,
		CandidateProbabilities: map[string]float64{},
	}
	repl := make(map[string]string, len(candidates))
	for i, c := range candidates {
		repl[fmt.Sprintf("c%d", i+1)] = c.ID
	}
	mapID := func(opt string) string {
		if id, ok := repl[opt]; ok {
			return id
		}
		return opt
	}
	if a, ok := d.Answers[QCandidate]; ok && a.Type == TypeChoice {
		for opt, p := range a.Probabilities {
			if opt == CandidateNone {
				continue
			}
			out.CandidateProbabilities[mapID(opt)] = p
		}
		if a.Choice != "" && a.Choice != CandidateNone {
			out.Candidate = mapID(a.Choice)
		}
	}
	setNoul := func(dst **float64, id string) {
		if a, ok := d.Answers[id]; ok && a.Type == TypeNoul {
			v := a.Noul
			*dst = &v
		}
	}
	setChoice := func(dst *string, id string) {
		if a, ok := d.Answers[id]; ok && a.Type == TypeChoice {
			*dst = a.Choice
		}
	}
	setNoul(&out.IsTextbox, QIsTextbox)
	setChoice(&out.TextKind, QTextKind)
	setChoice(&out.Speaker, QSpeaker)
	setNoul(&out.IsComplete, QIsComplete)
	setNoul(&out.SafeToSpeak, QSafeToSpeak)
	return out
}
