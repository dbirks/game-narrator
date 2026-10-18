package scenario

import (
	"fmt"
	"os"
	"regexp"
	"strings"
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

// catalogRe is the shape of a docs/test-plan/scenario-catalog.md row id.
var catalogRe = regexp.MustCompile(`^GN-T[0-9]{3}$`)

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
	// Faults is the M7 fault-injection block (synthetic scenarios only):
	// typed adversities the harness injects into the frame stream and the
	// fake provider endpoints. A scenario that carries faults must be
	// origin:generated_synthetic — the loader refuses the combination.
	Faults []Fault
	// Status is the only value a scenario may report before running: Status
	// blocked with a Reason, or absent (runnable). A blocked scenario is
	// reported as BLOCKED, never passed (ADR-0037).
	Status string
	Reason string
	// Catalog is the docs/test-plan/scenario-catalog.md row (e.g. "GN-T034")
	// this scenario exercises, when the scenario was authored against a row.
	Catalog string
}

// Input names where the frames and their recording manifest live, as paths
// relative to the repository root. Load does not resolve them; the test does.
//
// For origin dolphin the fixture lives EXTERNALLY (AGENTS.md rule 5: captured
// frames of a commercial game are never committed): Fixture names the
// recorder's fixture id under $GAME_NARRATOR_FIXTURE_DIR (default the
// gitignored artifacts/fixtures/) and ManifestSHA256 is the SHA-256 of its
// manifest.json — the reference by which a replay proves it is running the
// recording the scenario was authored against. First/Last slice the replay to
// a frame range (0-based, inclusive; absent = the whole fixture), so one
// recording can back many scenarios. FramesDir/Timestamps stay the synthetic
// shape; the loader enforces exactly one of the two input forms per origin.
type Input struct {
	FramesDir      string
	Timestamps     string
	Fixture        string
	ManifestSHA256 string
	First          int
	Last           int
}

// Inference names which model providers the scenario expects to take part.
type Inference struct {
	Decision string
	TTS      string
	// Voice is the voice id the harness requests from the TTS layer. Required
	// when TTS is not "none": a scenario that asks for speech must name the
	// voice it expects, or its no_voice_substitution assertion has nothing to
	// protect.
	Voice string
}

// Assertion is one check Evaluate runs. Load validates the shape per type
// and remembers the source line, so a report can point at the authoring
// mistake.
type Assertion struct {
	Type       string
	MessageID  string
	MaxDelayMS int
	Reason     string
	Count      int
	Names      []string
	// M7 provider-fault assertions.
	Voice     string // no_voice_substitution: the voice every speech event must name
	Kind      string // fault_count: the fault kind counted
	Role      string // provider_degraded: the provider role watched
	Recovered bool   // provider_degraded: require the recovery event too
	Line      int
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
	"no_stale_speech":             true,
	"no_duplicate_speech":         true,
	"late_result_discarded":       true,
	"duplicate_result_discarded":  true,
	"no_voice_substitution":       true,
	"fault_count":                 true,
	"committed_only":              true,
	"provider_degraded":           true,
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
	"no_stale_speech":             {"message_id": true},
	"no_duplicate_speech":         {"message_id": true},
	"late_result_discarded":       {"message_id": true},
	"duplicate_result_discarded":  {"message_id": true},
	"no_voice_substitution":       {"voice": true},
	"fault_count":                 {"kind": true, "count": true},
	"committed_only":              {"names": true},
	"provider_degraded":           {"role": true, "recovered": true},
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
	// catalog is the docs/test-plan/scenario-catalog.md row this scenario
	// exercises (optional; the M2 baseline scenario predates the catalog).
	// It is a label, not a key: the loader validates its shape so a typo can
	// never silently detach a scenario from the row it claims to cover.
	if catalog, line, ok := root.scalarStr("catalog"); ok {
		if !catalogRe.MatchString(catalog) {
			return nil, fmt.Errorf("line %d: catalog must look like GN-T042 (got %q)", line, catalog)
		}
		s.Catalog = catalog
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
	// Exactly one input form per origin: the synthetic/captured/hardware
	// scenarios name a frames dir in the checkout; the dolphin scenarios name
	// an external fixture by id + manifest hash (the frames are gitignored).
	if s.Origin == OriginDolphin {
		f, fl, fok := input.scalarStr("fixture")
		if !fok || f == "" {
			return nil, fmt.Errorf("line %d: origin %q requires input.fixture (the recorder's fixture id)", fl, OriginDolphin)
		}
		if strings.ContainsAny(f, `\/`) || f == "." || f == ".." {
			return nil, fmt.Errorf("line %d: input.fixture %q is not a safe fixture id", fl, f)
		}
		s.Input.Fixture = f
		m, ml, mok := input.scalarStr("manifest_sha256")
		if !mok || !isHex64(m) {
			return nil, fmt.Errorf("line %d: origin %q requires input.manifest_sha256 (64 hex chars, the SHA-256 of the fixture's manifest.json)", ml, OriginDolphin)
		}
		s.Input.ManifestSHA256 = m
		first, fl1, fok1 := input.scalarInt("first")
		if fok1 {
			if first < 0 {
				return nil, fmt.Errorf("line %d: input.first must be a non-negative frame index", fl1)
			}
			s.Input.First = first
		}
		last, fl2, fok2 := input.scalarInt("last")
		if fok2 {
			if last < 0 {
				return nil, fmt.Errorf("line %d: input.last must be a non-negative frame index", fl2)
			}
			s.Input.Last = last
		}
		if s.Input.Last != 0 && s.Input.First > s.Input.Last {
			l, _ := input.child("first")
			return nil, fmt.Errorf("line %d: input.first (%d) is after input.last (%d)", l.line, s.Input.First, s.Input.Last)
		}
		if input.hasKey("frames_dir") || input.hasKey("timestamps") {
			l, _ := input.child("frames_dir")
			if !input.hasKey("frames_dir") {
				l, _ = input.child("timestamps")
			}
			return nil, fmt.Errorf("line %d: origin %q takes input.fixture, not input.frames_dir/timestamps", l.line, OriginDolphin)
		}
	} else {
		if input.hasKey("fixture") || input.hasKey("manifest_sha256") || input.hasKey("first") || input.hasKey("last") {
			l, _ := input.child("fixture")
			return nil, fmt.Errorf("line %d: input.fixture/manifest_sha256/first/last are only allowed for origin %q", l.line, OriginDolphin)
		}
		s.Input.FramesDir, line, ok = input.scalarStr("frames_dir")
		if !ok || s.Input.FramesDir == "" {
			return nil, fmt.Errorf("line %d: input.frames_dir is required (non-empty string)", line)
		}
		s.Input.Timestamps, line, ok = input.scalarStr("timestamps")
		if !ok || s.Input.Timestamps == "" {
			return nil, fmt.Errorf("line %d: input.timestamps is required (non-empty string)", line)
		}
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
	s.Inference.Voice, _, _ = inf.scalarStr("voice") // optional; the harness validates it when TTS is on

	// status / reason: the blocked register (ADR-0037). status is closed:
	// the only value the loader understands is "blocked".
	if status, l, ok := root.scalarStr("status"); ok {
		if status != StatusBlocked {
			return nil, fmt.Errorf("line %d: status must be %q (got %q)", l, StatusBlocked, status)
		}
		s.Status = status
	}
	if reason, l, ok := root.scalarStr("reason"); ok {
		if reason == "" {
			return nil, fmt.Errorf("line %d: reason is required (non-empty string) when present", l)
		}
		s.Reason = reason
	}
	if s.Status == StatusBlocked && s.Reason == "" {
		l, _ := root.child("status")
		return nil, fmt.Errorf("line %d: status %q requires a reason", l.line, StatusBlocked)
	}

	// faults: the M7 injection block. Provider faults are synthetic scenarios
	// only — a captured, dolphin or hardware scenario naming them would present
	// an injected adversity as a property of the real source. drop_frames is
	// the exception: it withholds frames from the REPLAY stream (a harness
	// knob the docs/spec/12 failure-injection gate applies to any origin), the
	// recording itself is untouched, so a dolphin-origin scenario may carry it
	// to exercise the fences on captured frames.
	if items, _, ok := root.listAt("faults"); ok {
		if s.Origin != OriginGeneratedSynthetic {
			for _, item := range items {
				kind, l, _ := item.scalarStr("kind")
				if kind != "drop_frames" {
					return nil, fmt.Errorf("line %d: fault kind %q is only allowed for origin %q (got %q); drop_frames is the replay-side exception", l, kind, OriginGeneratedSynthetic, s.Origin)
				}
			}
		}
		var ferr error
		s.Faults, ferr = parseFaults(items)
		if ferr != nil {
			return nil, ferr
		}
	}

	items, line, ok := root.listAt("assertions")
	if !ok {
		// A blocked scenario names what it would have checked once unblocked,
		// but the check is not owed today: the assertions are optional.
		if s.Status == StatusBlocked {
			return &s, nil
		}
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
	"schema_version": true, "id": true, "catalog": true, "origin": true, "profile": true,
	"mode": true, "input": true, "inference": true, "assertions": true,
	"faults": true, "status": true, "reason": true,
	// Carried by the spec's own example; accepted and ignored until a milestone
	// consumes it.
	"reports": true,
}

// isHex64 reports whether s is 64 lowercase hex chars (a SHA-256 as the
// recorder and the replay side both render it).
func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
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
