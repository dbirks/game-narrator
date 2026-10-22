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
	QSpeakerKind = "speaker_kind"
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

// The speaker_kind options (ADR-0158): the adjudicator's apparent-kind
// answer for a line whose speaker the shared BMG slot could not name. The
// kind is the neutral ground the generic voice rides: a man must not hear a
// woman's voice, a child must not hear an old man's. The ids are the wire
// option ids ([a-z0-9_]{1,32}) and the roster's opaque kind keys are the same
// ids under the "kind:" prefix (docs/spec/06's option grammar).
const (
	SpeakerKindAdultMale   = "adult_male"
	SpeakerKindAdultFemale = "adult_female"
	SpeakerKindBoy         = "boy"
	SpeakerKindGirl        = "girl"
	SpeakerKindElderMale   = "elder_male"
	SpeakerKindElderFemale = "elder_female"
	SpeakerKindCreature    = "creature"
)

// SpeakerKindOptions renders the speaker_kind question's declared options,
// in the order the criteria object is serialised in. The last option is the
// question's own fallback: the kind could not be established from the frame.
func SpeakerKindOptions() []Option {
	return []Option{
		{ID: SpeakerKindAdultMale, Description: "an adult man"},
		{ID: SpeakerKindAdultFemale, Description: "an adult woman"},
		{ID: SpeakerKindBoy, Description: "a boy"},
		{ID: SpeakerKindGirl, Description: "a girl"},
		{ID: SpeakerKindElderMale, Description: "an elderly man"},
		{ID: SpeakerKindElderFemale, Description: "an elderly woman"},
		{ID: SpeakerKindCreature, Description: "a creature or other non-human speaker"},
		{ID: SpeakerUnknown, Description: "cannot be established from the frame"},
	}
}

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
// is_textbox (noul), text_kind (choice), speaker (choice incl. unknown, with
// deterministic speaker ids sp1..spN), speaker_kind (choice incl. unknown, with
// the apparent-kind options), candidate_message (choice incl. none, with
// deterministic candidate ids c1..cN), is_complete (noul), safe_to_speak (noul)
// — in that order, which is the order the questions object is serialised in.
//
// The returned Request has an empty Model; Decide fills it from the client's
// model. Candidates beyond maxBundleCandidates are dropped (the ranking is
// the caller's job). Candidate and speaker options are both positional wire
// aliases (c1..cN / sp1..spN, ADR-0072): the option-id grammar is
// [a-z0-9_]{1,32}, and neither message ids nor adapter speaker keys (for
// example the windwaker dump key "ww:spk:2", ADR-0070) may be assumed to fit
// it — the caller's ids stay the mapping keys, the wire never carries them.
func BuildAdjudication(obs Context, candidates []Candidate, speakers []Speaker) Request {
	questions := make([]Question, 0, 7)
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
	for i, s := range speakers {
		// Positional alias, exactly the candidate pattern (ADR-0072): the
		// speaker's own id is an opaque identity (ADR-0070), not a wire id.
		speakerOpts = append(speakerOpts, Option{ID: fmt.Sprintf("sp%d", i+1), Description: s.Description})
	}
	speakerOpts = append(speakerOpts, Option{ID: SpeakerUnknown, Description: "cannot be established from the frame"})
	questions = append(questions, Question{
		ID:           QSpeaker,
		Type:         TypeChoice,
		Instructions: "Who is speaking, if anyone is?",
		Criteria:     speakerOpts,
	})
	questions = append(questions, Question{
		ID:           QSpeakerKind,
		Type:         TypeChoice,
		Instructions: "What apparent kind is the speaker?",
		Criteria:     SpeakerKindOptions(),
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
	Model           string
	Latency         time.Duration
	RequestID       string
	RequestIDSource string
	Origin          string
	// IsTextbox is the raw noul signal for is_textbox.
	IsTextbox *float64
	// TextKind and Speaker are the chosen option ids.
	TextKind string
	// SpeakerConfidence is the adjudicator's confidence for the chosen
	// speaker option — the chosen option's probability (docs/spec/06: the raw
	// model signal, not a reliability guarantee). Zero when the answer is
	// absent or the speaker is unknown, which is what the runtime's speaker
	// gate reads (ADR-0156): a speaker named below the floor is not a name.
	SpeakerConfidence float64
	// Speaker is the chosen option id.
	Speaker string
	// SpeakerKind is the adjudicator's apparent-kind answer for the line
	// (ADR-0158): the chosen speaker_kind option's id — one of the
	// SpeakerKind* kind ids, or SpeakerUnknown when the kind could not be
	// established from the frame. Empty when the answer is absent.
	SpeakerKind string
	// SpeakerKindConfidence is the adjudicator's confidence for the chosen
	// speaker_kind option — the chosen option's probability (docs/spec/06: the
	// raw model signal, not a reliability guarantee). Zero when the answer is
	// absent, which is what the runtime's kind gate reads (ADR-0158): a kind
	// named below the floor is not a kind.
	SpeakerKindConfidence float64
	// Candidate is the message id ("" = none / missing).
	Candidate string
	// CandidateProbabilities maps message ids to raw probabilities.
	CandidateProbabilities map[string]float64
	// IsComplete and SafeToSpeak are the raw noul signals.
	IsComplete  *float64
	SafeToSpeak *float64
}

// Interpret maps a Decision for a BuildAdjudication request back to ids.
// The c1..cN candidate option ids and the sp1..spN speaker option ids are
// positional (ADR-0072): cN / spN is the Nth element of the candidates /
// speakers slice the request was built with, so those slices are the second
// and third arguments; pass nil (or leave the answer missing) and the raw
// wire ids come through instead. No threshold is applied anywhere — the
// floats are the raw model signals (docs/spec/06) and a missing answer simply
// leaves its field empty.
func Interpret(d Decision, candidates []Candidate, speakers []Speaker) Adjudication {
	out := Adjudication{
		Model:                  d.Model,
		Latency:                d.Latency,
		RequestID:              d.RequestID,
		RequestIDSource:        d.RequestIDSource,
		Origin:                 d.Origin,
		CandidateProbabilities: map[string]float64{},
	}
	repl := make(map[string]string, len(candidates))
	for i, c := range candidates {
		repl[fmt.Sprintf("c%d", i+1)] = c.ID
	}
	// The speaker alias map (ADR-0072): spN is the Nth element of the
	// speakers slice, the same positional contract as cN for candidates.
	spkRepl := make(map[string]string, len(speakers))
	for i, s := range speakers {
		spkRepl[fmt.Sprintf("sp%d", i+1)] = s.ID
	}
	mapID := func(opt string) string {
		if id, ok := repl[opt]; ok {
			return id
		}
		return opt
	}
	mapSpeaker := func(opt string) string {
		if id, ok := spkRepl[opt]; ok {
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
	// The spN alias the model picked maps back to the speaker's own id;
	// unknown and unmapped ids pass through (unknown keeps its meaning).
	if out.Speaker != "" {
		out.Speaker = mapSpeaker(out.Speaker)
	}
	// The confidence of the answer that named the speaker: the chosen
	// option's probability, carried on the answer the same way the candidate
	// choice carries its own (both questions are choice questions and share
	// the answer shape). An absent answer reads as no confidence, so the
	// runtime's speaker gate falls back rather than guessing (ADR-0156).
	if a, ok := d.Answers[QSpeaker]; ok && a.Type == TypeChoice {
		out.SpeakerConfidence = a.Confidence
	}
	// The confidence of the answer that named the speaker's apparent kind
	// (ADR-0158): the chosen speaker_kind option's probability. An absent
	// answer reads as no confidence, so the runtime's kind gate falls back
	// rather than guessing.
	if a, ok := d.Answers[QSpeakerKind]; ok && a.Type == TypeChoice {
		out.SpeakerKind = a.Choice
		out.SpeakerKindConfidence = a.Confidence
	}
	setNoul(&out.IsComplete, QIsComplete)
	setNoul(&out.SafeToSpeak, QSafeToSpeak)
	return out
}
