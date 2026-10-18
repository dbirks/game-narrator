package systemone

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/recorded"
)

// responsePayload is the wire envelope of a System One answer (ADR-0021).
type responsePayload struct {
	Model   string                     `json:"model"`
	Answers map[string]json.RawMessage `json:"answers"`
	Usage   Usage                      `json:"usage"`
}

// answerPayload is one raw answer. The score fields are the documented
// assumption (ADR-0021): the level name under "level" with an optional
// "confidence".
type answerPayload struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice"`
	Confidence    *float64           `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
	Noul          *float64           `json:"noul"`
	Level         string             `json:"level"`
}

const (
	probabilitySumTolerance = 0.02
	confidenceTolerance     = 1e-3
)

// toDecision validates the response against the declared questions
// (docs/spec/06: "Response parser validates declared IDs, finite probability
// [0,1], complete choice sets and schema version; rejects unknown answer
// types and model mismatch") and normalises it.
func (p responsePayload) toDecision(requestedModel string, req Request, header http.Header, latency time.Duration, reqID, reqIDSource string) (Decision, error) {
	model := p.Model
	if model == "" {
		model = requestedModel
	}
	if p.Model != "" && p.Model != requestedModel {
		e := providers.NewError(providers.KindWrongCheckpoint, http.StatusOK,
			fmt.Sprintf("systemone: response model %q does not match requested model %q", p.Model, requestedModel))
		e.Model = p.Model
		e.RequestID = reqID
		e.Retryable = false
		return Decision{}, e
	}
	asked := make(map[string]bool, len(req.Questions))
	for _, q := range req.Questions {
		asked[q.ID] = true
	}
	if len(p.Answers) == 0 {
		return Decision{}, p.schemaError("response carries no answers", requestedModel, reqID)
	}
	var problems []string
	answers := make(map[string]Answer, len(p.Answers))
	for id, raw := range p.Answers {
		if !asked[id] {
			problems = append(problems, fmt.Sprintf("unexpected answer for question %q (it was not asked)", id))
			continue
		}
		var ap answerPayload
		if err := json.Unmarshal(raw, &ap); err != nil {
			problems = append(problems, fmt.Sprintf("answer for question %q is not a JSON object: %v", id, err))
			continue
		}
		q := findQuestion(req.Questions, id)
		a, qProblems := validateAnswer(q, ap)
		problems = append(problems, qProblems...)
		if len(qProblems) == 0 {
			answers[id] = a
		}
	}
	for _, q := range req.Questions {
		if _, ok := p.Answers[q.ID]; !ok {
			problems = append(problems, fmt.Sprintf("no answer for asked question %q", q.ID))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return Decision{}, p.schemaError(strings.Join(problems, "; "), model, reqID)
	}
	origin := recorded.OriginFromHeader(header)
	if origin == recorded.OriginUnknown {
		// No harness label: the response came straight from the network.
		origin = recorded.OriginLive
	}
	return Decision{
		Model:           model,
		Answers:         answers,
		Usage:           p.Usage,
		Latency:         latency,
		RequestID:       reqID,
		RequestIDSource: reqIDSource,
		Origin:          origin,
	}, nil
}

func (p responsePayload) schemaError(msg, model, reqID string) *providers.Error {
	e := providers.NewError(providers.KindBadSchema, http.StatusOK, "systemone: invalid response: "+msg)
	e.Model = model
	e.RequestID = reqID
	e.Retryable = false
	return e
}

func findQuestion(questions []Question, id string) *Question {
	for i := range questions {
		if questions[i].ID == id {
			return &questions[i]
		}
	}
	return nil
}

// validateAnswer checks one answer against its declared question and returns
// the normalised answer plus every problem found (empty when clean).
func validateAnswer(q *Question, ap answerPayload) (Answer, []string) {
	var problems []string
	if ap.Type != string(q.Type) {
		problems = append(problems, fmt.Sprintf("answer for %q has type %q, want %q", q.ID, ap.Type, q.Type))
		return Answer{Type: QuestionType(ap.Type)}, problems
	}
	a := Answer{Type: q.Type}
	switch q.Type {
	case TypeChoice:
		declared := make(map[string]bool, len(q.Criteria))
		for _, opt := range q.Criteria {
			declared[opt.ID] = true
		}
		if ap.Choice == "" || !declared[ap.Choice] {
			problems = append(problems, fmt.Sprintf("choice %q for %q is not one of the declared options", ap.Choice, q.ID))
		} else {
			a.Choice = ap.Choice
		}
		problems = append(problems, validateProbabilities(q.ID, ap.Probabilities, declared)...)
		if ap.Confidence == nil {
			problems = append(problems, fmt.Sprintf("answer for %q has no confidence", q.ID))
		} else {
			a.Confidence = *ap.Confidence
			if !finiteInRange(a.Confidence, 0, 1) {
				problems = append(problems, fmt.Sprintf("confidence %v for %q is not finite or not in [0,1]", a.Confidence, q.ID))
			} else if chosen, ok := ap.Probabilities[ap.Choice]; ok && math.Abs(*ap.Confidence-chosen) > confidenceTolerance {
				problems = append(problems, fmt.Sprintf("confidence %v for %q differs from the chosen option's probability %v by more than %v",
					*ap.Confidence, q.ID, chosen, confidenceTolerance))
			}
		}
		if len(problems) == 0 {
			a.Probabilities = ap.Probabilities
		}
	case TypeNoul:
		if ap.Noul == nil {
			problems = append(problems, fmt.Sprintf("answer for %q has no noul value", q.ID))
		} else {
			a.Noul = *ap.Noul
			if !finiteInRange(a.Noul, 0, 1) {
				problems = append(problems, fmt.Sprintf("noul %v for %q is not finite or not in [0,1]", a.Noul, q.ID))
			}
		}
	case TypeScore:
		// Unverified live (ADR-0021): the level name must be one of the
		// declared ordered levels.
		declared := make(map[string]bool, len(q.Levels))
		for _, lv := range q.Levels {
			declared[lv] = true
		}
		if ap.Level == "" || !declared[ap.Level] {
			problems = append(problems, fmt.Sprintf("level %q for %q is not one of the declared levels", ap.Level, q.ID))
		} else {
			a.Score = ap.Level
		}
		if ap.Confidence != nil && !finiteInRange(*ap.Confidence, 0, 1) {
			problems = append(problems, fmt.Sprintf("confidence %v for %q is not finite or not in [0,1]", *ap.Confidence, q.ID))
		}
	}
	return a, problems
}

// validateProbabilities enforces the docs/spec/06 rules on a choice answer's
// probability map: keys cover exactly the declared options, every value is
// finite and in [0,1], and the sum is 1 within tolerance.
func validateProbabilities(id string, probs map[string]float64, declared map[string]bool) []string {
	var problems []string
	if len(probs) == 0 {
		return []string{fmt.Sprintf("answer for %q has no probabilities", id)}
	}
	sum := 0.0
	for opt, p := range probs {
		if !declared[opt] {
			problems = append(problems, fmt.Sprintf("probability for undeclared option %q in answer %q", opt, id))
			continue
		}
		if !isFinite(p) {
			problems = append(problems, fmt.Sprintf("probability %v for option %q in answer %q is not finite", p, opt, id))
			continue
		}
		if p < 0 || p > 1 {
			problems = append(problems, fmt.Sprintf("probability %v for option %q in answer %q is outside [0,1]", p, opt, id))
			continue
		}
		sum += p
	}
	for opt := range declared {
		if _, ok := probs[opt]; !ok {
			problems = append(problems, fmt.Sprintf("probability for declared option %q missing from answer %q", opt, id))
		}
	}
	if len(problems) == 0 && math.Abs(sum-1) > probabilitySumTolerance {
		problems = append(problems, fmt.Sprintf("probabilities for answer %q sum to %v, want 1 within ±%v", id, round6(sum), probabilitySumTolerance))
	}
	return problems
}

func finiteInRange(v, lo, hi float64) bool {
	return isFinite(v) && v >= lo && v <= hi
}

func isFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

func round6(v float64) float64 {
	return math.Round(v*1e6) / 1e6
}
