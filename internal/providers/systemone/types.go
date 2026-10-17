// Package systemone implements the client adapter for the System One
// (Clef-Omni) decision protocol — the typed classifier served at the
// gateway's /systemone route (docs/spec/06 "Provider registry", "System One /
// Clef-Omni protocol").
//
// Clef-Omni is a joint-schema pooling model, not a chat model: one request
// carries the observation state plus a set of typed questions (choice, noul,
// score) and one forward pass answers all of them. The client never sends a
// top-level "instructions" field — the gateway rejects it for this
// checkpoint — and it validates the answer envelope the way docs/spec/06
// requires: every asked question answered and no extras, types matching,
// choices inside the declared options, finite probabilities in [0,1] that
// cover exactly the declared options and sum to 1 (±0.02), confidence equal to
// the chosen option's probability (±1e-3), and the model echo matching the
// request (mismatch is wrong_checkpoint, not bad_schema).
//
// The probability values are signals for the runtime's commitment policy, not
// reliability guarantees: nothing in this package calibrates them
// (docs/spec/05 "Clef adjudication and commit policy").
package systemone

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
)

// Wire limits. The gateway's real limits are negotiated by preflight
// (docs/spec/06: "probe negotiated limits rather than assume upstream
// limits"); these are the defaults this client enforces locally so an
// oversized request fails fast with a taxonomy kind instead of an opaque
// 413 after a multi-second stall.
const (
	// MaxImageBytes caps one image. JPEG or PNG bytes; larger crops must be
	// re-encoded or downsampled by the caller.
	MaxImageBytes = 4 << 20
	// MaxImages caps the images per request.
	MaxImages = 4
	// MaxOptions is the per-choice option cap (docs/spec/05 "limit candidate
	// options to supported request cap").
	MaxOptions = 16
)

// QuestionType is one of the System One question types.
type QuestionType string

// The question types. Score exists in docs/spec/06 ("score is ordered levels")
// but was not observed against the live gateway as of ADR-0021; the wire shape
// for it is a documented assumption, not a verified contract.
const (
	TypeChoice QuestionType = "choice"
	TypeNoul   QuestionType = "noul"
	TypeScore  QuestionType = "score"
)

// [a-z0-9_]{1,32}. Lowercase keeps the keys unambiguous in logs and events.

// Option is one declared choice option. Order is meaningful: it is the order
// the criteria object is serialised in, and Interpret maps answers back by id.
type Option struct {
	ID          string
	Description string
}

// Question is one typed question. Choice questions carry Criteria (ordered,
// deterministic); score questions carry Levels (ordered); noul carries
// neither. Instructions is the per-question prompt and is the only place
// prompt text belongs: the System One protocol has no top-level instructions.
type Question struct {
	ID           string
	Type         QuestionType
	Instructions string
	Criteria     []Option
	Levels       []string
}

// Request is one System One decision call.
//
// Model may be left empty by helper builders (for example BuildAdjudication)
// and is then filled from the client's model at Decide time. Key is local
// metadata used only for the consecutive-doubtful-decision budget; it is never
// serialised onto the wire.
type Request struct {
	Model     string
	State     map[string]any
	Images    [][]byte
	Questions []Question
	Key       string
}

type limits struct {
	maxImages     int
	maxImageBytes int
	maxOptions    int
}

func (l limits) defaults() limits {
	return limits{maxImages: MaxImages, maxImageBytes: MaxImageBytes, maxOptions: MaxOptions}
}

// validate checks the request against the protocol before anything leaves the
// process. Every violation is collected and reported in one bad_schema error
// (payload_too_large when an image is over the byte cap), so a caller sees the
// whole problem instead of fixing errors one round trip at a time.
func (r Request) validate(l limits) error {
	if l.maxImages <= 0 || l.maxImageBytes <= 0 || l.maxOptions <= 0 {
		l = l.defaults()
	}
	var problems []string
	kind := providers.KindBadSchema
	if strings.TrimSpace(r.Model) == "" {
		problems = append(problems, "model is required")
	}
	if r.State == nil || len(r.State) == 0 {
		problems = append(problems, "state is required")
	}
	if len(r.Images) > l.maxImages {
		problems = append(problems, fmt.Sprintf("at most %d images per request, got %d", l.maxImages, len(r.Images)))
	}
	for i, img := range r.Images {
		if len(img) == 0 {
			problems = append(problems, fmt.Sprintf("image %d is empty", i))
			continue
		}
		if len(img) > l.maxImageBytes {
			problems = append(problems, fmt.Sprintf("image %d is %d bytes, over the %d byte limit", i, len(img), l.maxImageBytes))
			kind = providers.KindPayloadTooLarge
			continue
		}
		if mime, ok := imageMIME(img); !ok {
			problems = append(problems, fmt.Sprintf("image %d is not a valid JPEG or PNG", i))
		} else {
			_ = mime
		}
	}
	if len(r.Questions) == 0 {
		problems = append(problems, "at least one question is required")
	}
	seen := make(map[string]bool, len(r.Questions))
	for i, q := range r.Questions {
		label := q.ID
		if label == "" {
			label = fmt.Sprintf("#%d", i)
		}
		if !idPattern.MatchString(q.ID) {
			problems = append(problems, fmt.Sprintf("question %d has an invalid id %q (want [a-z0-9_]{1,32})", i, q.ID))
		}
		if seen[q.ID] {
			problems = append(problems, fmt.Sprintf("question id %q is not unique", q.ID))
		}
		seen[q.ID] = true
		if err := q.validate(l, label); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return providers.NewError(kind, 0, "systemone: invalid request: "+strings.Join(problems, "; "))
}

var idPattern = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)

func (q Question) validate(l limits, label string) error {
	var problems []string
	if q.Instructions == "" {
		problems = append(problems, fmt.Sprintf("question %s has no instructions", label))
	}
	switch q.Type {
	case TypeChoice:
		if len(q.Criteria) < 2 {
			problems = append(problems, fmt.Sprintf("question %s is a choice and needs at least 2 options, got %d", label, len(q.Criteria)))
		}
		if len(q.Criteria) > l.maxOptions {
			problems = append(problems, fmt.Sprintf("question %s has %d options, over the %d option cap", label, len(q.Criteria), l.maxOptions))
		}
		if len(q.Levels) > 0 {
			problems = append(problems, fmt.Sprintf("question %s is a choice and must not carry score levels", label))
		}
		optSeen := make(map[string]bool, len(q.Criteria))
		for j, opt := range q.Criteria {
			if !idPattern.MatchString(opt.ID) {
				problems = append(problems, fmt.Sprintf("question %s option %d has an invalid id %q (want [a-z0-9_]{1,32})", label, j, opt.ID))
			}
			if optSeen[opt.ID] {
				problems = append(problems, fmt.Sprintf("question %s option id %q is not unique", label, opt.ID))
			}
			optSeen[opt.ID] = true
		}
	case TypeNoul:
		if len(q.Criteria) > 0 || len(q.Levels) > 0 {
			problems = append(problems, fmt.Sprintf("question %s is a noul and must not carry criteria or levels", label))
		}
	case TypeScore:
		if len(q.Levels) < 2 {
			problems = append(problems, fmt.Sprintf("question %s is a score and needs at least 2 ordered levels, got %d", label, len(q.Levels)))
		}
		if len(q.Levels) > l.maxOptions {
			problems = append(problems, fmt.Sprintf("question %s has %d levels, over the %d cap", label, len(q.Levels), l.maxOptions))
		}
		if len(q.Criteria) > 0 {
			problems = append(problems, fmt.Sprintf("question %s is a score and must not carry choice criteria", label))
		}
		levelSeen := make(map[string]bool, len(q.Levels))
		for _, lv := range q.Levels {
			if strings.TrimSpace(lv) == "" {
				problems = append(problems, fmt.Sprintf("question %s has an empty level name", label))
				continue
			}
			if levelSeen[lv] {
				problems = append(problems, fmt.Sprintf("question %s level %q is not unique", label, lv))
			}
			levelSeen[lv] = true
		}
	default:
		problems = append(problems, fmt.Sprintf("question %s has an unknown type %q (want choice, noul or score)", label, q.Type))
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(problems, "; "))
}

// imageMIME sniffs the container so the data URL media type is honest; only
// JPEG and PNG are accepted by this client (the live contract was verified
// with those two).
func imageMIME(b []byte) (string, bool) {
	switch {
	case len(b) >= 3 && b[0] == 0xff && b[1] == 0xd8 && b[2] == 0xff:
		return "image/jpeg", true
	case len(b) >= 8 && bytes.Equal(b[:8], []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}):
		return "image/png", true
	default:
		return "", false
	}
}

func dataURL(mime string, b []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(b)
}

// encodeRequest renders the wire body. The contract pins three things the
// encoder must get right, and the golden test pins them:
//
//  1. model and state are required top-level fields;
//  2. questions is an object keyed by question id, serialised in the
//     deterministic order of Request.Questions (map keys would not be);
//  3. there is no top-level "instructions" — that field is rejected by the
//     gateway for this checkpoint.
//
// state itself is a map, which encoding/json serialises with sorted keys, so
// two requests that are logically equal encode byte-identically.
func (r Request) encodeRequest() ([]byte, error) {
	images := make([]string, 0, len(r.Images))
	for _, img := range r.Images {
		mime, _ := imageMIME(img) // validated before encoding
		images = append(images, dataURL(mime, img))
	}
	wire := wireRequest{
		Model:     r.Model,
		State:     r.State,
		Images:    images,
		Questions: make(wireQuestions, len(r.Questions)),
	}
	for i, q := range r.Questions {
		wq := wireQuestion{Type: string(q.Type), Instructions: q.Instructions}
		switch q.Type {
		case TypeChoice:
			criteria := make(map[string]string, len(q.Criteria))
			for _, opt := range q.Criteria {
				criteria[opt.ID] = opt.Description
			}
			wq.Criteria = criteria
		case TypeScore:
			levels := append([]string(nil), q.Levels...)
			wq.Levels = levels
		}
		wire.Questions[i] = wireQuestionEntry{ID: q.ID, Q: wq}
	}
	buf, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("systemone: cannot encode request: %w", err)
	}
	return buf, nil
}

type wireQuestion struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria,omitempty"`
	Levels       []string          `json:"levels,omitempty"`
}

// wireQuestions serialises as the protocol's questions object: keys are the
// question ids in slice order (deterministic for a given Request), never in
// Go map order.
type wireQuestionEntry struct {
	ID string
	Q  wireQuestion
}

type wireQuestions []wireQuestionEntry

func (qs wireQuestions) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, e := range qs {
		id, err := json.Marshal(e.ID)
		if err != nil {
			return nil, err
		}
		body, err := json.Marshal(e.Q)
		if err != nil {
			return nil, err
		}
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(id)
		buf.WriteByte(':')
		buf.Write(body)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

type wireRequest struct {
	Model     string         `json:"model"`
	State     map[string]any `json:"state"`
	Images    []string       `json:"images,omitempty"`
	Questions wireQuestions  `json:"questions"`
}

// Usage reports the token accounting the gateway attaches to the response.
type Usage struct {
	Input  int `json:"input_tokens"`
	Output int `json:"output_tokens"`
}

// Answer is one normalised question answer. Fields not used by the answer's
// type are zero values: Choice/Confidence/Probabilities for choice, Noul for
// noul, Score (the chosen level) for score.
type Answer struct {
	Type          QuestionType
	Choice        string
	Confidence    float64
	Probabilities map[string]float64
	Noul          float64
	Score         string
}

// Decision is the normalised result of one Decide call: the answers the model
// actually produced, the model that produced them, and the provenance the
// runtime's event trace records (docs/spec/17 decision.resolved carries the
// model and per-question probabilities from these fields).
type Decision struct {
	Model     string
	Answers   map[string]Answer
	Usage     Usage
	Latency   time.Duration
	RequestID string
	// Origin is the recorded-transport provenance label: "live" or
	// "recorded" (internal/providers/recorded). A response with no label came
	// straight from the network and is reported as live.
	Origin string
}
