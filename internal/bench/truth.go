package bench

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
)

// The truth.json split values (docs/spec/12 "Split into tuning vs holdout").
const (
	SplitTuning  = "tuning"
	SplitHoldout = "holdout"
	SplitAll     = "all"
)

// truthSplits is the closed split vocabulary. The split is decided by a hash of
// the sample id at generation time and written into truth.json, so a run can
// never re-split a labelled sample onto the side its thresholds were tuned on.
var truthSplits = map[string]bool{SplitTuning: true, SplitHoldout: true}

// textKinds is the closed text-kind vocabulary for a bench sample.
var textKinds = map[string]bool{"dialogue": true, "empty": true}

// SceneID is the one scene the synthetic set lives in.
const SceneID = "synthetic-harbour"

// SpeakerUnknown is the truth speaker value for lines the script does not
// attribute ("unknown cases don't receive a wrong forced speaker").
const SpeakerUnknown = "unknown"

// Frame is one frame of one sample in the replay manifest.
type Frame struct {
	File string `json:"file"`
	TsNS int64  `json:"ts_ns"`
}

// Variant labels how a sample's frame was rendered; it is a breakdown axis in
// the report, never a decision input.
type Variant struct {
	Font       string  `json:"font"` // fontconfig family, or "" for no text
	Resolution string  `json:"resolution"`
	Noise      float64 `json:"noise"` // 0 = clean, >0 = analog-noisy parameters
}

// Sample is one ground-truth row: a labelled frame sequence and the dialogue
// it was rendered from.
type Sample struct {
	ID             string  `json:"id"`
	Frames         []Frame `json:"frames"`
	SourceText     string  `json:"source_text"`
	MessageID      *string `json:"message_id"` // null when not in the script
	Speaker        string  `json:"speaker"`    // or "unknown"
	TextKind       string  `json:"text_kind"`  // dialogue | empty
	Scene          string  `json:"scene"`
	IntendedSpeech bool    `json:"intended_speech"`
	Split          string  `json:"split"` // tuning | holdout
	Variant        Variant `json:"variant"`
}

// TruthSet is the whole ground-truth document.
type TruthSet struct {
	SchemaVersion int      `json:"schema_version"`
	Set           string   `json:"set"`
	Origin        string   `json:"origin"`
	SeedBase      uint64   `json:"seed_base"`
	ProfileFile   string   `json:"profile_file"`
	Samples       []Sample `json:"samples"`
}

// MessageInScript returns the sample's message id, or "" when the sample is
// not part of the script (nil MessageID).
func (s Sample) MessageInScript() string {
	if s.MessageID == nil {
		return ""
	}
	return *s.MessageID
}

// SplitForID decides the split of one sample id. The decision is a function of
// the id alone — never of a result — so re-running the generator cannot move a
// sample between tuning and holdout, and no threshold may be tuned on the
// holdout side (docs/spec/12).
func SplitForID(id string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(id))
	if h.Sum64()%100 < 30 {
		return SplitHoldout
	}
	return SplitTuning
}

// LoadTruth reads and validates <dir>/truth.json. Every error names the
// offending sample, so a corrupt row is fixable without regenerating the set.
func LoadTruth(dir string) (*TruthSet, error) {
	path := filepath.Join(dir, "truth.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("bench: read truth: %v", err)
	}
	var ts TruthSet
	if err := json.Unmarshal(raw, &ts); err != nil {
		return nil, fmt.Errorf("bench: parse %s: %v", path, err)
	}
	if err := ts.Validate(); err != nil {
		return nil, fmt.Errorf("bench: %s: %v", path, err)
	}
	return &ts, nil
}

// Validate checks the closed schema. It is separate from LoadTruth so the
// generator can run it on a set before it touches the filesystem.
func (ts *TruthSet) Validate() error {
	if ts.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported schema_version %d (want %d)", ts.SchemaVersion, SchemaVersion)
	}
	if ts.Set == "" {
		return fmt.Errorf("set is required (non-empty)")
	}
	if ts.Origin != "generated_synthetic" {
		return fmt.Errorf("origin must be generated_synthetic (the bench is synthetic-only, ADR-0038), got %q", ts.Origin)
	}
	if ts.SeedBase == 0 {
		return fmt.Errorf("seed_base is required (non-zero)")
	}
	if ts.ProfileFile == "" {
		return fmt.Errorf("profile_file is required")
	}
	if len(ts.Samples) == 0 {
		return fmt.Errorf("no samples")
	}
	seen := make(map[string]bool, len(ts.Samples))
	for i, s := range ts.Samples {
		if err := s.Validate(); err != nil {
			return fmt.Errorf("sample %d: %v", i, err)
		}
		if seen[s.ID] {
			return fmt.Errorf("sample %d: duplicate id %q", i, s.ID)
		}
		seen[s.ID] = true
	}
	return nil
}

// Validate checks one sample's fields against the closed schema.
func (s Sample) Validate() error {
	if s.ID == "" || !strings.HasPrefix(s.ID, "bn") {
		return fmt.Errorf("id is required and must look like bn0123 (got %q)", s.ID)
	}
	if len(s.Frames) == 0 {
		return fmt.Errorf("frames must name at least one frame")
	}
	var last int64
	for i, f := range s.Frames {
		if f.File == "" || !strings.HasSuffix(f.File, ".png") {
			return fmt.Errorf("frame %d: file must be a .png name (got %q)", i, f.File)
		}
		if f.TsNS <= 0 {
			return fmt.Errorf("frame %d: ts_ns must be > 0", i)
		}
		if f.TsNS < last {
			return fmt.Errorf("frame %d: timestamps must be monotonic non-decreasing", i)
		}
		last = f.TsNS
	}
	if !truthSplits[s.Split] {
		return fmt.Errorf("split must be tuning or holdout (got %q)", s.Split)
	}
	if s.Split != SplitForID(s.ID) {
		return fmt.Errorf("split %q does not match the hash of id %q — the split is decided by the id, never edited", s.Split, s.ID)
	}
	if !textKinds[s.TextKind] {
		return fmt.Errorf("text_kind must be dialogue or empty (got %q)", s.TextKind)
	}
	if s.Scene != SceneID {
		return fmt.Errorf("scene must be %q (got %q)", SceneID, s.Scene)
	}
	if s.Speaker == "" {
		return fmt.Errorf("speaker is required (use %q for unattributed lines)", SpeakerUnknown)
	}
	switch {
	case s.TextKind == "empty":
		if s.SourceText != "" {
			return fmt.Errorf("empty samples carry no source_text")
		}
		if s.MessageID != nil {
			return fmt.Errorf("empty samples carry no message_id")
		}
		if s.IntendedSpeech {
			return fmt.Errorf("empty samples are never intended to be spoken")
		}
	default:
		if s.MessageID == nil {
			return fmt.Errorf("dialogue samples must name their message_id (or set text_kind empty)")
		}
		if *s.MessageID == "" {
			return fmt.Errorf("message_id must be non-empty")
		}
		if s.SourceText == "" {
			return fmt.Errorf("source_text is required for dialogue samples")
		}
	}
	return nil
}

// SetHash is the sha256 of truth.json, the identity the manifest carries.
func SetHash(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
