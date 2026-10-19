package systemone

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
)

// Table-driven coverage of every validation rule in docs/spec/06 for the
// System One client, on both sides of the wire.

// --- request side ----------------------------------------------------------

func validRequest() Request {
	return Request{
		Model: "clef-omni",
		State: map[string]any{"ocr": "hello"},
		Questions: []Question{
			{ID: "kind", Type: TypeChoice, Instructions: "kind?",
				Criteria: []Option{{ID: "dialogue", Description: "a"}, {ID: "none", Description: "b"}}},
			{ID: "is_textbox", Type: TypeNoul, Instructions: "textbox?"},
		},
	}
}

var requestValidationCases = []struct {
	name     string
	mutate   func(*Request)
	wantKind providers.Kind
	wantMsg  string // substring that must appear in the error
	wantGone string // substring that must NOT appear (checks the right rule fired)
}{
	{name: "valid request passes", mutate: func(*Request) {}, wantKind: ""},
	{
		name:     "empty model",
		mutate:   func(r *Request) { r.Model = "  " },
		wantKind: providers.KindBadSchema, wantMsg: "model is required",
	},
	{
		name:     "nil state",
		mutate:   func(r *Request) { r.State = nil },
		wantKind: providers.KindBadSchema, wantMsg: "state is required",
	},
	{
		name:     "empty state map",
		mutate:   func(r *Request) { r.State = map[string]any{} },
		wantKind: providers.KindBadSchema, wantMsg: "state is required",
	},
	{
		name:     "question id uppercase",
		mutate:   func(r *Request) { r.Questions[0].ID = "Kind" },
		wantKind: providers.KindBadSchema, wantMsg: "invalid id \"Kind\"",
	},
	{
		name:     "question id too long",
		mutate:   func(r *Request) { r.Questions[0].ID = strings.Repeat("a", 33) },
		wantKind: providers.KindBadSchema, wantMsg: "invalid id",
	},
	{
		name:     "question id with dash",
		mutate:   func(r *Request) { r.Questions[0].ID = "text-kind" },
		wantKind: providers.KindBadSchema, wantMsg: "invalid id \"text-kind\"",
	},
	{
		name:     "duplicate question ids",
		mutate:   func(r *Request) { r.Questions[1].ID = "kind" },
		wantKind: providers.KindBadSchema, wantMsg: "not unique",
	},
	{
		name:     "choice with one option",
		mutate:   func(r *Request) { r.Questions[0].Criteria = []Option{{ID: "a", Description: "x"}} },
		wantKind: providers.KindBadSchema, wantMsg: "at least 2 options",
	},
	{
		name: "choice over the option cap",
		mutate: func(r *Request) {
			var opts []Option
			for i := 0; i < 17; i++ {
				opts = append(opts, Option{ID: string(rune('a' + i)), Description: "x"})
			}
			r.Questions[0].Criteria = opts
		},
		wantKind: providers.KindBadSchema, wantMsg: "over the 16 option cap",
	},
	{
		name: "duplicate option ids",
		mutate: func(r *Request) {
			r.Questions[0].Criteria = []Option{{ID: "a", Description: "x"}, {ID: "a", Description: "y"}}
		},
		wantKind: providers.KindBadSchema, wantMsg: "option id \"a\" is not unique",
	},
	{
		name:     "invalid option id",
		mutate:   func(r *Request) { r.Questions[0].Criteria[0].ID = "Dialogue" },
		wantKind: providers.KindBadSchema, wantMsg: "option 0 has an invalid id",
	},
	{
		name:     "missing instructions",
		mutate:   func(r *Request) { r.Questions[0].Instructions = "" },
		wantKind: providers.KindBadSchema, wantMsg: "has no instructions",
	},
	{
		name: "noul carrying criteria",
		mutate: func(r *Request) {
			r.Questions[1].Criteria = []Option{{ID: "a", Description: "x"}, {ID: "b", Description: "y"}}
		},
		wantKind: providers.KindBadSchema, wantMsg: "noul and must not carry criteria",
	},
	{
		name: "score with one level",
		mutate: func(r *Request) {
			r.Questions = append(r.Questions, Question{ID: "risk", Type: TypeScore, Instructions: "risk?", Levels: []string{"low"}})
		},
		wantKind: providers.KindBadSchema, wantMsg: "at least 2 ordered levels",
	},
	{
		name: "score with duplicate levels",
		mutate: func(r *Request) {
			r.Questions = append(r.Questions, Question{ID: "risk", Type: TypeScore, Instructions: "risk?", Levels: []string{"low", "low"}})
		},
		wantKind: providers.KindBadSchema, wantMsg: "level \"low\" is not unique",
	},
	{
		name: "choice carrying levels",
		mutate: func(r *Request) {
			r.Questions[0].Levels = []string{"low", "high"}
		},
		wantKind: providers.KindBadSchema, wantMsg: "must not carry score levels",
	},
	{
		name:     "unknown question type",
		mutate:   func(r *Request) { r.Questions[1].Type = "maybe" },
		wantKind: providers.KindBadSchema, wantMsg: "unknown type \"maybe\"",
	},
	{
		name:     "no questions",
		mutate:   func(r *Request) { r.Questions = nil },
		wantKind: providers.KindBadSchema, wantMsg: "at least one question",
	},
	{
		name: "too many images",
		mutate: func(r *Request) {
			j := goldenJPEG
			for i := 0; i < 5; i++ {
				r.Images = append(r.Images, j)
			}
		},
		wantKind: providers.KindBadSchema, wantMsg: "at most 4 images",
	},
	{
		name:     "empty image",
		mutate:   func(r *Request) { r.Images = [][]byte{{}} },
		wantKind: providers.KindBadSchema, wantMsg: "image 0 is empty",
	},
	{
		name: "oversized image is payload_too_large",
		mutate: func(r *Request) {
			r.Images = [][]byte{make([]byte, MaxImageBytes+1)}
		},
		wantKind: providers.KindPayloadTooLarge, wantMsg: "over the",
	},
	{
		name:     "non-image bytes",
		mutate:   func(r *Request) { r.Images = [][]byte{[]byte("definitely not an image")} },
		wantKind: providers.KindBadSchema, wantMsg: "image 0 is not a valid JPEG or PNG",
	},
	{
		name:     "multiple problems are listed together",
		mutate:   func(r *Request) { r.Model = ""; r.Questions[1].ID = "Kind" },
		wantKind: providers.KindBadSchema, wantMsg: "model is required", wantGone: "",
	},
}

func TestRequestValidation(t *testing.T) {
	for _, tc := range requestValidationCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			r := validRequest()
			tc.mutate(&r)
			err := r.validate(limits{maxImages: MaxImages, maxImageBytes: MaxImageBytes, maxOptions: MaxOptions})
			if tc.wantKind == "" {
				if err != nil {
					t.Fatalf("valid request rejected: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want a %s error, got nil", tc.wantKind)
			}
			var pe *providers.Error
			if !errors.As(err, &pe) {
				t.Fatalf("err = %T, want *providers.Error", err)
			}
			if pe.Kind != tc.wantKind {
				t.Fatalf("Kind = %q, want %q (%s)", pe.Kind, tc.wantKind, pe)
			}
			if tc.wantMsg != "" && !strings.Contains(pe.Msg, tc.wantMsg) {
				t.Fatalf("Msg = %q, want it to contain %q", pe.Msg, tc.wantMsg)
			}
			if tc.name == "multiple problems are listed together" {
				if !strings.Contains(pe.Msg, "invalid id") {
					t.Fatalf("the second problem was not listed: %q", pe.Msg)
				}
			}
		})
	}
}

// --- response side ----------------------------------------------------------

func baselineRequest() Request {
	return Request{
		Model: "clef-omni",
		State: map[string]any{"ocr": "hello"},
		Questions: []Question{
			{ID: "kind", Type: TypeChoice, Instructions: "kind?",
				Criteria: []Option{{ID: "dialogue", Description: "a"}, {ID: "sign", Description: "b"}, {ID: "none", Description: "c"}}},
			{ID: "is_textbox", Type: TypeNoul, Instructions: "textbox?"},
			{ID: "risk", Type: TypeScore, Instructions: "risk?", Levels: []string{"low", "high"}},
		},
	}
}

// baselineAnswers renders a fully valid answer set for baselineRequest.
func baselineAnswers() map[string]json.RawMessage {
	mk := func(v any) json.RawMessage {
		b, _ := json.Marshal(v)
		return b
	}
	return map[string]json.RawMessage{
		"kind": mk(map[string]any{
			"type":          "choice",
			"choice":        "dialogue",
			"confidence":    0.7,
			"probabilities": map[string]float64{"dialogue": 0.7, "sign": 0.2, "none": 0.1},
		}),
		"is_textbox": mk(map[string]any{"type": "noul", "noul": 0.97}),
		"risk":       mk(map[string]any{"type": "score", "level": "low", "confidence": 0.8}),
	}
}

func decideForTest(t *testing.T, req Request, answers map[string]json.RawMessage, model string) (Decision, error) {
	t.Helper()
	raw, err := json.Marshal(responsePayload{Model: model, Answers: answers, Usage: Usage{Input: 10, Output: 0}})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var p responsePayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	// The synthetic header carries no provider id, so the effective id is the
	// client-minted one, exactly as doRaw would resolve it.
	clientID := newRequestID()
	reqID, reqIDSource := RecordRequestID("", clientID)
	return p.toDecision(req.Model, req, http.Header{}, 5*time.Millisecond, reqID, reqIDSource)
}

var responseValidationCases = []struct {
	name     string
	mutate   func(map[string]json.RawMessage)
	wantKind providers.Kind
	wantMsg  string
}{
	{name: "valid bundle passes", mutate: func(map[string]json.RawMessage) {}, wantKind: ""},
	{
		name:     "missing asked id",
		mutate:   func(m map[string]json.RawMessage) { delete(m, "is_textbox") },
		wantKind: providers.KindBadSchema, wantMsg: "no answer for asked question \"is_textbox\"",
	},
	{
		name:     "extra unasked id",
		mutate:   func(m map[string]json.RawMessage) { m["sneaky"] = []byte(`{"type":"noul","noul":0.5}`) },
		wantKind: providers.KindBadSchema, wantMsg: "unexpected answer for question \"sneaky\"",
	},
	{
		name:     "answer type mismatch",
		mutate:   func(m map[string]json.RawMessage) { m["is_textbox"] = []byte(`{"type":"choice","choice":"a"}`) },
		wantKind: providers.KindBadSchema, wantMsg: "has type \"choice\", want \"noul\"",
	},
	{
		name: "choice not declared",
		mutate: func(m map[string]json.RawMessage) {
			m["kind"] = []byte(`{"type":"choice","choice":"narrator","confidence":0.5,"probabilities":{"dialogue":0.5,"sign":0.3,"none":0.2}}`)
		},
		wantKind: providers.KindBadSchema, wantMsg: "choice \"narrator\" for \"kind\" is not one of the declared options",
	},
	{
		name: "probability sum 0.7",
		mutate: func(m map[string]json.RawMessage) {
			m["kind"] = []byte(`{"type":"choice","choice":"dialogue","confidence":0.4,"probabilities":{"dialogue":0.4,"sign":0.2,"none":0.1}}`)
		},
		wantKind: providers.KindBadSchema, wantMsg: "sum to 0.7",
	},
	{
		name: "probability negative",
		mutate: func(m map[string]json.RawMessage) {
			m["kind"] = []byte(`{"type":"choice","choice":"dialogue","confidence":0.4,"probabilities":{"dialogue":-0.1,"sign":0.9,"none":0.2}}`)
		},
		wantKind: providers.KindBadSchema, wantMsg: "outside [0,1]",
	},
	{
		name: "probability outside range",
		mutate: func(m map[string]json.RawMessage) {
			m["kind"] = []byte(`{"type":"choice","choice":"dialogue","confidence":0.4,"probabilities":{"dialogue":1.4,"sign":0.2,"none":0.1}}`)
		},
		wantKind: providers.KindBadSchema, wantMsg: "outside [0,1]",
	},
	{
		name: "probability for undeclared option",
		mutate: func(m map[string]json.RawMessage) {
			m["kind"] = []byte(`{"type":"choice","choice":"dialogue","confidence":0.4,"probabilities":{"dialogue":0.4,"sign":0.2,"none":0.2,"phantom":0.2}}`)
		},
		wantKind: providers.KindBadSchema, wantMsg: "undeclared option \"phantom\"",
	},
	{
		name: "declared option missing from probabilities",
		mutate: func(m map[string]json.RawMessage) {
			m["kind"] = []byte(`{"type":"choice","choice":"dialogue","confidence":0.8,"probabilities":{"dialogue":0.8,"none":0.2}}`)
		},
		wantKind: providers.KindBadSchema, wantMsg: "declared option \"sign\" missing",
	},
	{
		name: "confidence far from chosen probability",
		mutate: func(m map[string]json.RawMessage) {
			m["kind"] = []byte(`{"type":"choice","choice":"dialogue","confidence":0.5,"probabilities":{"dialogue":0.9,"sign":0.05,"none":0.05}}`)
		},
		wantKind: providers.KindBadSchema, wantMsg: "differs from the chosen option's probability",
	},
	{
		name: "missing confidence on choice",
		mutate: func(m map[string]json.RawMessage) {
			m["kind"] = []byte(`{"type":"choice","choice":"dialogue","probabilities":{"dialogue":0.7,"sign":0.2,"none":0.1}}`)
		},
		wantKind: providers.KindBadSchema, wantMsg: "has no confidence",
	},
	{
		name:     "noul above 1",
		mutate:   func(m map[string]json.RawMessage) { m["is_textbox"] = []byte(`{"type":"noul","noul":1.5}`) },
		wantKind: providers.KindBadSchema, wantMsg: "noul 1.5",
	},
	{
		name:     "noul missing",
		mutate:   func(m map[string]json.RawMessage) { m["is_textbox"] = []byte(`{"type":"noul"}`) },
		wantKind: providers.KindBadSchema, wantMsg: "has no noul value",
	},
	{
		name:     "score level not declared",
		mutate:   func(m map[string]json.RawMessage) { m["risk"] = []byte(`{"type":"score","level":"extreme"}`) },
		wantKind: providers.KindBadSchema, wantMsg: "level \"extreme\"",
	},
	{
		name:     "model mismatch is wrong_checkpoint",
		mutate:   func(map[string]json.RawMessage) {},
		wantKind: providers.KindWrongCheckpoint, wantMsg: "does not match",
	},
	{
		name: "no answers at all",
		mutate: func(m map[string]json.RawMessage) {
			for k := range m {
				delete(m, k)
			}
		},
		wantKind: providers.KindBadSchema, wantMsg: "carries no answers",
	},
}

func TestResponseValidation(t *testing.T) {
	for _, tc := range responseValidationCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			req := baselineRequest()
			answers := baselineAnswers()
			tc.mutate(answers)
			model := "clef-omni"
			if tc.name == "model mismatch is wrong_checkpoint" {
				model = "some-other-checkpoint"
			}
			d, err := decideForTest(t, req, answers, model)
			if tc.wantKind == "" {
				if err != nil {
					t.Fatalf("valid response rejected: %v", err)
				}
				if d.Origin != "live" {
					t.Fatalf("Origin = %q, want live (no harness label means the network)", d.Origin)
				}
				if d.Model != "clef-omni" || d.Latency != 5*time.Millisecond {
					t.Fatalf("provenance not carried: %+v", d)
				}
				if d.Answers["kind"].Probabilities["sign"] != 0.2 {
					t.Fatalf("raw probabilities not preserved: %+v", d.Answers["kind"])
				}
				if d.Usage.Input != 10 {
					t.Fatalf("usage not carried: %+v", d.Usage)
				}
				return
			}
			if err == nil {
				t.Fatalf("want a %s error, got success %+v", tc.wantKind, d)
			}
			var pe *providers.Error
			if !errors.As(err, &pe) {
				t.Fatalf("err = %T, want *providers.Error", err)
			}
			if pe.Kind != tc.wantKind {
				t.Fatalf("Kind = %q, want %q (%s)", pe.Kind, tc.wantKind, pe)
			}
			if tc.wantMsg != "" && !strings.Contains(pe.Msg, tc.wantMsg) {
				t.Fatalf("Msg = %q, want it to contain %q", pe.Msg, tc.wantMsg)
			}
			if tc.wantKind == providers.KindBadSchema {
				if pe.Retryable {
					t.Fatal("a schema violation must not be retried against the same request")
				}
			}
		})
	}
}

// TestProbabilityValidationRejectsNonFinite covers the NaN/Inf rule that the
// JSON table cannot express (JSON has no non-finite literals): a gateway that
// emits them through some lenient parser must still be refused.
func TestProbabilityValidationRejectsNonFinite(t *testing.T) {
	declared := map[string]bool{"a": true, "b": true}
	for _, tc := range []struct {
		name    string
		probs   map[string]float64
		wantMsg string
	}{
		{"NaN", map[string]float64{"a": math.NaN(), "b": 1}, "not finite"},
		{"+Inf", map[string]float64{"a": 0.5, "b": math.Inf(1)}, "not finite"},
		{"-Inf", map[string]float64{"a": math.Inf(-1), "b": 0.5}, "not finite"},
	} {
		problems := validateProbabilities("q", tc.probs, declared)
		if len(problems) == 0 {
			t.Fatalf("%s: no problems reported", tc.name)
		}
		if !strings.Contains(problems[0], tc.wantMsg) {
			t.Fatalf("%s: problems = %v, want %q", tc.name, problems, tc.wantMsg)
		}
	}
	// And the noul side of the same rule.
	q := &Question{ID: "n", Type: TypeNoul}
	one := 1.0
	a, problems := validateAnswer(q, answerPayload{Type: "noul", Noul: &one})
	if len(problems) > 0 || a.Noul != 1.0 {
		t.Fatalf("valid noul 1.0 rejected: %v", problems)
	}
	nan := math.NaN()
	_, problems = validateAnswer(q, answerPayload{Type: "noul", Noul: &nan})
	if len(problems) == 0 || !strings.Contains(problems[0], "not finite") {
		t.Fatalf("NaN noul accepted: %v", problems)
	}
}

// TestResponseValidationListsEveryProblem pins the "list every problem" rule:
// two violations in one response must both appear in one bad_schema error.
func TestResponseValidationListsEveryProblem(t *testing.T) {
	req := baselineRequest()
	answers := baselineAnswers()
	delete(answers, "risk")
	answers["kind"] = []byte(`{"type":"choice","choice":"phantom","confidence":0.5,"probabilities":{"dialogue":0.4,"sign":0.2,"none":0.1}}`)
	_, err := decideForTest(t, req, answers, "clef-omni")
	var pe *providers.Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %T, want *providers.Error", err)
	}
	if pe.Kind != providers.KindBadSchema {
		t.Fatalf("Kind = %q, want bad_schema", pe.Kind)
	}
	for _, frag := range []string{`no answer for asked question "risk"`, "choice \"phantom\"", "sum to 0.7"} {
		if !strings.Contains(pe.Msg, frag) {
			t.Fatalf("Msg = %q, want it to contain %q", pe.Msg, frag)
		}
	}
}

// TestAdjudicationBundleOpaqueSpeakerKeys (ADR-0072): the windwaker adapter's
// speaker identity is the opaque dump key "ww:spk:<n>" (ADR-0070), which
// fails the option-id grammar. The live run of 2026-10-19 found the
// consequence: the bundle rendered the raw keys as option ids, every
// adjudication failed local request validation as bad_schema in 0 ms, and
// the session committed every line locally with no provider request id. The
// bundle must render positional spN aliases instead, and Interpret must map
// the chosen alias back to the caller's key.
func TestAdjudicationBundleOpaqueSpeakerKeys(t *testing.T) {
	speakers := []Speaker{
		{ID: "ww:spk:2", Description: ""},
		{ID: "ww:spk:7", Description: "another slot"},
	}
	candidates := []Candidate{{ID: "m-8", Text: "a line read from the frame"}}
	bundle := BuildAdjudication(
		Context{Scene: "outset", OCR: "a line read from the frame", LastMessage: "m-7"},
		candidates, speakers,
	)

	// Wire-valid: the raw keys never reach the option ids. Decide fills the
	// model from the client before validating, so do the same here.
	bundle.Model = "clef-omni"
	if err := bundle.validate(limits{}.defaults()); err != nil {
		t.Fatalf("the bundle with opaque speaker keys is not wire-valid: %v", err)
	}
	var speakerQ *Question
	for i := range bundle.Questions {
		if bundle.Questions[i].ID == QSpeaker {
			speakerQ = &bundle.Questions[i]
		}
	}
	if speakerQ == nil {
		t.Fatalf("no speaker question in the bundle")
	}
	wantIDs := []string{"sp1", "sp2", SpeakerUnknown}
	if len(speakerQ.Criteria) != len(wantIDs) {
		t.Fatalf("speaker options = %d, want %d (%+v)", len(speakerQ.Criteria), len(wantIDs), speakerQ.Criteria)
	}
	for i, opt := range speakerQ.Criteria {
		if opt.ID != wantIDs[i] {
			t.Fatalf("speaker option %d id = %q, want %q (the raw keys stay mapping keys, not wire ids)", i, opt.ID, wantIDs[i])
		}
	}
	for _, opt := range speakerQ.Criteria {
		if opt.ID == "ww:spk:2" || opt.ID == "ww:spk:7" {
			t.Fatalf("the raw speaker key %q leaked onto the wire", opt.ID)
		}
	}

	// The model picks sp2: Interpret maps it back to the caller's key.
	dec := Decision{
		Model: "clef-omni",
		Answers: map[string]Answer{
			QIsTextbox:   {Type: TypeNoul, Noul: 0.9},
			QTextKind:    {Type: TypeChoice, Choice: TextKindDialogue, Confidence: 0.9, Probabilities: map[string]float64{TextKindDialogue: 0.9, TextKindSign: 0.05, TextKindNone: 0.05}},
			QSpeaker:     {Type: TypeChoice, Choice: "sp2", Confidence: 0.8, Probabilities: map[string]float64{"sp1": 0.2, "sp2": 0.8, SpeakerUnknown: 0.0}},
			QCandidate:   {Type: TypeChoice, Choice: "c1", Confidence: 0.9, Probabilities: map[string]float64{"c1": 0.9, CandidateNone: 0.1}},
			QIsComplete:  {Type: TypeNoul, Noul: 0.9},
			QSafeToSpeak: {Type: TypeNoul, Noul: 0.9},
		},
	}
	adj := Interpret(dec, candidates, speakers)
	if adj.Speaker != "ww:spk:7" {
		t.Fatalf("Interpret.Speaker = %q, want the mapped-back key %q", adj.Speaker, "ww:spk:7")
	}
	if adj.Candidate != "m-8" {
		t.Fatalf("Interpret.Candidate = %q, want m-8", adj.Candidate)
	}

	// A model that answers unknown keeps the unknown meaning.
	unknownAns := dec.Answers[QSpeaker]
	unknownAns.Choice = SpeakerUnknown
	dec.Answers[QSpeaker] = unknownAns
	if adj := Interpret(dec, candidates, speakers); adj.Speaker != SpeakerUnknown {
		t.Fatalf("Interpret.Speaker = %q, want %q", adj.Speaker, SpeakerUnknown)
	}
}
