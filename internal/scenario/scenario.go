package scenario

import (
	"fmt"
	"os"
)

// Origin values (docs/spec/12 "provenance"): where the frames and their labels
// actually came from. The field is required and closed, because a scenario that
// does not state its origin can present synthetic coverage as real-game cover.
const (
	OriginGeneratedSynthetic = "generated_synthetic"
	OriginCaptured           = "captured"
	OriginDolphin            = "dolphin"
	OriginHardware           = "hardware"
)

var origins = map[string]bool{
	OriginGeneratedSynthetic: true,
	OriginCaptured:           true,
	OriginDolphin:            true,
	OriginHardware:           true,
}

// ModeFrameReplay is the mode this milestone runs: recorded frames through the
// real pipeline (docs/spec/11 "Mode A: replay pure frames"). Dolphin and
// hardware modes reuse the same assertion vocabulary later.
const ModeFrameReplay = "frame_replay"

// Inference provider policies, one per provider the scenario names. "none" is
// a first-class value: the local chain alone is a legitimate E2E mode.
const (
	InferenceNone     = "none"
	InferenceRecorded = "recorded"
	InferenceLive     = "live"
)

var inferenceKinds = map[string]bool{InferenceNone: true, InferenceRecorded: true, InferenceLive: true}

// SchemaVersion is the only scenario schema this loader understands.
const SchemaVersion = 1

// Scenario is one docs/spec/17 "E2E scenario YAML" document.
type Scenario struct {
	SchemaVersion int
	ID            string
	Origin        string
	Profile       string
	Mode          string
	Input         Input
	Inference     Inference
	Assertions    []Assertion
}

// Input names where the frames and their recording manifest live, as paths
// relative to the repository root. Load does not resolve them; the test does.
type Input struct {
	FramesDir  string
	Timestamps string
}

// Inference names which model providers the scenario expects to take part.
type Inference struct {
	Decision string
	TTS      string
}

// Assertion is one check Evaluate runs. Load validates the shape per type and
// remembers the source line, so a report can point at the authoring mistake.
type Assertion struct {
	Type       string
	MessageID  string
	MaxDelayMS int
	Reason     string
	Count      int
	Names      []string
	Line       int
}

// knownAssertionTypes is the closed vocabulary. An unknown type is a load
// error: a typo'd assertion must not silently pass the scenario.
var knownAssertionTypes = map[string]bool{
	"message_detected":            true,
	"message_committed_count":     true,
	"speech_cancelled":            true,
	"no_duplicate_commit":         true,
	"no_stale_generation_applied": true,
	"events_order":                true,
}

// allowedFields is the exact field set each type accepts besides type. Extra
// keys are a load error, because a field nobody reads is a promise nobody
// keeps; missing required ones are errors too (see parseAssertions).
var allowedFields = map[string]map[string]bool{
	"message_detected":            {"message_id": true, "max_delay_ms": true},
	"message_committed_count":     {"message_id": true, "count": true},
	"speech_cancelled":            {"reason": true, "count": true},
	"no_duplicate_commit":         {},
	"no_stale_generation_applied": {},
	"events_order":                {"names": true},
}

// Load reads the scenario YAML at path and validates it. Every error carries
// the document line that caused it.
func Load(path string) (*Scenario, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("scenario: read %s: %v", path, err)
	}
	root, err := parseYAML(string(raw))
	if err != nil {
		return nil, fmt.Errorf("scenario %s: %v", path, err)
	}
	return fromNode(root)
}

// fromNode walks the parsed tree. The shape is the docs/spec/17 list, strictly:
// unknown top-level keys are errors except reports, which the spec's own
// example carries and this milestone does not consume.
func fromNode(root *ynode) (*Scenario, error) {
	var s Scenario

	v, line, ok := root.scalarInt("schema_version")
	if !ok {
		return nil, fieldErr(line, "schema_version is required (integer)")
	}
	if v != SchemaVersion {
		return nil, fmt.Errorf("line %d: unsupported schema_version %d (this loader understands %d)", line, v, SchemaVersion)
	}
	s.SchemaVersion = v

	for _, key := range root.keys {
		if !topLevelKeys[key] {
			l, _ := root.child(key)
			return nil, fmt.Errorf("line %d: unknown scenario field %q", l.line, key)
		}
	}
	s.ID, _, _ = root.scalarStr("id")
	if !root.hasKey("id") || s.ID == "" {
		return nil, fmt.Errorf("line %d: id is required (non-empty string)", root.vals["id"].line)
	}
	origin, line, ok := root.scalarStr("origin")
	if !ok || !origins[origin] {
		return nil, fmt.Errorf("line %d: origin is required and must be one of generated_synthetic, captured, dolphin, hardware (got %q)", line, origin)
	}
	s.Origin = origin
	s.Profile, _, _ = root.scalarStr("profile")
	if !root.hasKey("profile") || s.Profile == "" {
		return nil, fmt.Errorf("line %d: profile is required (non-empty string)", root.vals["profile"].line)
	}
	mode, line, ok := root.scalarStr("mode")
	if !ok || mode != ModeFrameReplay {
		return nil, fmt.Errorf("line %d: mode must be %q (got %q)", line, ModeFrameReplay, mode)
	}
	s.Mode = mode

	input, line, ok := inNode(root, "input")
	if !ok {
		return nil, fmt.Errorf("line %d: input is required (a mapping)", line)
	}
	s.Input.FramesDir, line, ok = input.scalarStr("frames_dir")
	if !ok || s.Input.FramesDir == "" {
		return nil, fmt.Errorf("line %d: input.frames_dir is required (non-empty string)", line)
	}
	s.Input.Timestamps, line, ok = input.scalarStr("timestamps")
	if !ok || s.Input.Timestamps == "" {
		return nil, fmt.Errorf("line %d: input.timestamps is required (non-empty string)", line)
	}

	inf, line, ok := inNode(root, "inference")
	if !ok {
		return nil, fmt.Errorf("line %d: inference is required (a mapping)", line)
	}
	s.Inference.Decision, line, ok = inf.scalarStr("decision")
	if !ok || !inferenceKinds[s.Inference.Decision] {
		return nil, fmt.Errorf("line %d: inference.decision must be one of none, recorded, live (got %q)", line, s.Inference.Decision)
	}
	s.Inference.TTS, line, ok = inf.scalarStr("tts")
	if !ok || !inferenceKinds[s.Inference.TTS] {
		return nil, fmt.Errorf("line %d: inference.tts must be one of none, recorded, live (got %q)", line, s.Inference.TTS)
	}

	items, line, ok := root.listAt("assertions")
	if !ok {
		return nil, fmt.Errorf("line %d: assertions is required (a list)", line)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("line %d: assertions must name at least one check", line)
	}
	var err error
	s.Assertions, err = parseAssertions(items)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

var topLevelKeys = map[string]bool{
	"schema_version": true, "id": true, "origin": true, "profile": true,
	"mode": true, "input": true, "inference": true, "assertions": true,
	// Carried by the spec's own example; accepted and ignored until a milestone
	// consumes it.
	"reports": true,
}

// inNode returns the value of a key that must be a mapping.
func inNode(m *ynode, k string) (*ynode, int, bool) {
	v, ok := m.child(k)
	if !ok {
		return nil, 0, false
	}
	if v.kind != ymap {
		return nil, v.line, false
	}
	return v, v.line, true
}

// fieldErr renders a missing/invalid-key error with the line when one exists.
func fieldErr(line int, msg string) error {
	if line == 0 {
		return fmt.Errorf("scenario: %s", msg)
	}
	return fmt.Errorf("line %d: %s", line, msg)
}
