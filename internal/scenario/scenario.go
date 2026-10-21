package scenario

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/providers/tts"
	"github.com/dbirks/game-narrator/internal/session"
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
	// NarratedKinds is the operator's narrated-class setting the harness's
	// two session layers run with (docs/spec/09 "/play": dialogue, +signs,
	// +items). The commitment policy's narrated-class veto reads it, so a
	// sign that commits in a session set to dialogue alone is speech-
	// ineligible in the timeline exactly as it would be in a live session.
	// Optional: the default is dialogue alone, the product's conservative
	// default (session.DefaultConfig).
	NarratedKinds []string
	// CatchUp is the session's catch-up narration policy the harness's
	// speech sidecar runs (issue #25, ADR-0122). The harness default is
	// latest_only — the historical immediate shape every scenario written
	// before the catch-up policy grades (a new commit cancels the playing
	// line); a scenario that runs the product's catch_up default states it
	// explicitly, so a silent default can never re-shape an existing
	// scenario's graded behaviour. The session the sidecar builds carries
	// the same word, so the session state the timeline records matches the
	// audio behaviour the run exhibits.
	CatchUp string
	// Speakers is the synthetic speaker attribution the catch-up merge reads
	// (issue #25, ADR-0122): message id to speaker label, invented with the
	// scenario's own lines (AGENTS.md rule 5 — the synthetic corpus is all
	// invented text). It exists because the synthetic profiles carry no
	// character rows: the harness has no speaker link to resolve a speaker
	// from, so the scenario declares the attribution the merge-under-test
	// needs. A committed message with no declared speaker is the narrator,
	// the same fallback the runtime's speaker resolution applies to an
	// unattributed line. Read only when CatchUp is catch_up: under
	// latest_only the merge (and with it the speaker split) never runs.
	Speakers map[string]string
	// Voices is the per-character approved voices (issue #36, gap row G3,
	// ADR-0132): speaker label to voice name, keyed by the labels Speakers
	// declares. It exists for the same reason Speakers does — the synthetic
	// profiles carry no voice roster, so the scenario declares the roster
	// the resolution-under-test needs. The voice a line's speaker resolves
	// to, when one is declared; a speaker with no entry — and every
	// unattributed line (the narrator) — takes Inference.Voice, the
	// scenario's narrator voice (ADR-0101's fallback, at harness scale): a
	// character's line never falls back to another character's voice. Every
	// entry is validated at load against the registry's name scheme
	// (tts.ValidateName): a voice the registry would refuse is a load error,
	// the way a typo'd catalog row is. Like Speakers, read only when CatchUp
	// is catch_up: under latest_only it would be dead config.
	Voices map[string]string
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
	// TTSToneMS is the duration of the tone the fake TTS endpoint answers
	// with (the harness's TTS layer, ADR-0037's recorded inference: the
	// endpoint's real job is to exercise the client's WAV validation and the
	// session's lifecycle on real bytes). 0 (absent) keeps the historical
	// 100 ms — short on purpose, so a scenario's graded playback interleave
	// is the one the suite has always run. A scenario that needs an
	// utterance still playing when the next line commits (the catch-up
	// policy's batched path, issue #25) names the overlap it models: a tone
	// longer than the clip's commit cadence is what makes "a line committed
	// while an utterance is playing" a property of the run rather than a
	// scheduling coin flip.
	TTSToneMS int
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
	// Engines scopes the assertion to the OCR engine the run used (the
	// closed vocabulary ocrEngineNames). A harness that declares its engine
	// via EvaluateWithOCREngine reports an out-of-scope assertion
	// not-applicable instead of grading it: a property of one engine (the
	// eng model's blindness to a non-Latin page) is not a property of a run
	// whose scripted engine answers from the clip's truth. Empty (the usual
	// case) means every engine; Evaluate without a declared engine grades
	// every assertion, scoped or not.
	Engines []string
	// M7 provider-fault assertions.
	Voice     string // no_voice_substitution: the voice every speech event must name
	Kind      string // fault_count: the fault kind counted
	Role      string // provider_degraded: the provider role watched
	Recovered bool   // provider_degraded: require the recovery event too
	Line      int
	// Catch-up narration assertions (issue #25, ADR-0122).
	// MessageIDs is the ordered message-id list the catch-up merge is graded
	// on (speech_merged: the run that speaks exactly these lines, in this
	// order, in one utterance).
	MessageIDs []string
}

// ocrEngineNames is the closed vocabulary an assertion's engines: scope may
// name — the OCR engines a scenario run is graded under. "tesseract" is the
// real engine (the M2 replay gate, ocr.Tesseract.Name); "truth" is the M7
// harness's scripted engine that answers every read from the clip's
// truth.json (truthocr.go, truthEngine.Name). A scope naming anything else
// is a load error: a promise nobody's harness can satisfy is a typo.
var ocrEngineNames = map[string]bool{"tesseract": true, "truth": true}

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
	"committed_kind":              true,
	"provider_degraded":           true,
	"speech_spoken_count":         true,
	"speech_merged":               true,
	"speech_skipped":              true,
	"speech_voice":                true,
}

// allowedFields is the exact field set each type accepts besides type. Extra
// keys are a load error, because a field nobody reads is a promise nobody
// keeps; missing required ones are errors too (see parseAssertions).
var allowedFields = map[string]map[string]bool{
	"message_detected":            {"message_id": true, "max_delay_ms": true},
	"message_committed_count":     {"message_id": true, "count": true, "engines": true},
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
	"committed_kind":              {"message_id": true, "kind": true},
	"provider_degraded":           {"role": true, "recovered": true},
	"speech_spoken_count":         {"message_id": true, "count": true},
	"speech_merged":               {"message_ids": true},
	"speech_skipped":              {"message_id": true, "reason": true, "count": true},
	"speech_voice":                {"message_id": true, "voice": true},
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
	// narrated_kinds: the operator's narrated-class setting for the run
	// (docs/spec/09 "/play"): a list of content-class names the session's
	// commitment policy may narrate. dialogue is force-included by the
	// session's own normalizer (it is the product), so a list that forgets it
	// still runs with dialogue plus what it names.
	if items, line, ok := root.listAt("narrated_kinds"); ok {
		if len(items) == 0 {
			return nil, fmt.Errorf("line %d: narrated_kinds must name at least one content class", line)
		}
		for _, item := range items {
			if item.kind != yscalar || item.str == "" {
				return nil, fmt.Errorf("line %d: narrated_kinds entries must be non-empty class names", item.line)
			}
			if _, err := games.ParseTextKind(item.str); err != nil {
				return nil, fmt.Errorf("line %d: narrated_kinds entry %q is not a known content class (%v)", item.line, item.str, err)
			}
			s.NarratedKinds = append(s.NarratedKinds, item.str)
		}
	}
	s.Profile, _, _ = root.scalarStr("profile")
	if !root.hasKey("profile") || s.Profile == "" {
		return nil, fmt.Errorf("line %d: profile is required (non-empty string)", root.vals["profile"].line)
	}
	mode, line, ok := root.scalarStr("mode")
	if !ok || mode != ModeFrameReplay {
		return nil, fmt.Errorf("line %d: mode must be %q (got %q)", line, ModeFrameReplay, mode)
	}
	s.Mode = mode

	// The blocked flag is read early: a blocked scenario is a registration of
	// what will be checked once unblocked, and its input reference is not yet
	// knowable (a dolphin fixture that has not been recorded has no manifest
	// hash to pin), so the input shape it must satisfy is the relaxed one.
	statusVal, _, _ := root.scalarStr("status")
	blocked := statusVal == StatusBlocked

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
		if blocked {
			// The fixture does not exist yet, so its manifest has no hash to pin:
			// the reference is completed when the recording lands and the
			// scenario unblocks. A present value is still checked, so a stale
			// pin cannot hide in a blocked row.
			if m, ml, mok := input.scalarStr("manifest_sha256"); mok && !isHex64(m) {
				return nil, fmt.Errorf("line %d: origin %q input.manifest_sha256 must be 64 hex chars when present (got %q)", ml, OriginDolphin, m)
			}
			s.Input.ManifestSHA256, _, _ = input.scalarStr("manifest_sha256")
		} else {
			m, ml, mok := input.scalarStr("manifest_sha256")
			if !mok || !isHex64(m) {
				return nil, fmt.Errorf("line %d: origin %q requires input.manifest_sha256 (64 hex chars, the SHA-256 of the fixture's manifest.json)", ml, OriginDolphin)
			}
			s.Input.ManifestSHA256 = m
		}
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
	// tts_tone_ms: the fake TTS tone's duration (optional; the historical
	// 100 ms when absent). Only meaningful with a TTS layer — a scenario
	// with no voice layer has no tone to size, so the field is refused
	// there (a field nobody reads is a promise nobody keeps).
	if v, line, ok := inf.scalarInt("tts_tone_ms"); ok {
		if s.Inference.TTS == InferenceNone {
			return nil, fmt.Errorf("line %d: inference.tts_tone_ms is only allowed when inference.tts is not %q", line, InferenceNone)
		}
		if v <= 0 {
			return nil, fmt.Errorf("line %d: inference.tts_tone_ms must be a positive integer", line)
		}
		s.Inference.TTSToneMS = v
	}

	// catch_up: the session's catch-up narration policy (issue #25,
	// ADR-0122). Absent is the harness's latest_only pin (the historical
	// shape the pre-catch-up scenarios grade); present must name one of the
	// policy's two words, like the settings route validates it live.
	if v, line, ok := root.scalarStr("catch_up"); ok {
		switch v {
		case session.CatchUpPolicyCatchUp, session.CatchUpPolicyLatestOnly:
			s.CatchUp = v
		default:
			return nil, fmt.Errorf("line %d: catch_up must be %q or %q (got %q)", line, session.CatchUpPolicyCatchUp, session.CatchUpPolicyLatestOnly, v)
		}
	}
	if s.CatchUp == "" {
		s.CatchUp = session.CatchUpPolicyLatestOnly
	}

	// speakers: the synthetic speaker attribution (message id to speaker
	// label) the catch-up merge reads; a mapping of non-empty scalars.
	if m, ok := root.child("speakers"); ok {
		if m.kind != ymap {
			return nil, fmt.Errorf("line %d: speakers must be a mapping (message id to speaker label)", m.line)
		}
		if len(m.keys) == 0 {
			return nil, fmt.Errorf("line %d: speakers must name at least one message", m.line)
		}
		s.Speakers = make(map[string]string, len(m.keys))
		for _, k := range m.keys {
			v, vok := m.child(k)
			if !vok || v.kind != yscalar || v.str == "" {
				return nil, fmt.Errorf("line %d: speakers[%s] must be a non-empty speaker label", m.line, k)
			}
			s.Speakers[k] = v.str
		}
	}

	// voices: the per-character approved voices (issue #36, gap G3, ADR-0132);
	// a mapping of speaker label to voice name. Allowed only with
	// catch_up: the speaker attribution it keys on is read only under that
	// policy, and a voices block under latest_only would be dead config. Every
	// entry must pass the registry's name validation (the production gn_…
	// scheme or the automated-test gn_test_<hex8> scheme): a voice the
	// registry would refuse is a load error, the loader's usual "a promise
	// nobody can keep is a typo".
	if m, ok := root.child("voices"); ok {
		if m.kind != ymap {
			return nil, fmt.Errorf("line %d: voices must be a mapping (speaker label to voice name)", m.line)
		}
		if len(m.keys) == 0 {
			return nil, fmt.Errorf("line %d: voices must name at least one speaker", m.line)
		}
		if s.CatchUp != session.CatchUpPolicyCatchUp {
			return nil, fmt.Errorf("line %d: voices is only allowed with catch_up %q (the speaker attribution it keys on is read only under that policy)", m.line, session.CatchUpPolicyCatchUp)
		}
		s.Voices = make(map[string]string, len(m.keys))
		for _, k := range m.keys {
			if k == "narrator" {
				return nil, fmt.Errorf("line %d: voices[\"narrator\"] is the narrator's slot: the narrator's voice is inference.voice (the scenario's default), the single source the unattributed lines take", m.line)
			}
			v, vok := m.child(k)
			if !vok || v.kind != yscalar || v.str == "" {
				return nil, fmt.Errorf("line %d: voices[%s] must be a non-empty voice name", m.line, k)
			}
			if err := tts.ValidateName(v.str); err != nil {
				return nil, fmt.Errorf("line %d: voices[%s] %q is not a registry-approved voice name (%v)", m.line, k, v.str, err)
			}
			s.Voices[k] = v.str
		}
	}

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
	"faults": true, "status": true, "reason": true, "narrated_kinds": true,
	"catch_up": true, "speakers": true, "voices": true,
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
