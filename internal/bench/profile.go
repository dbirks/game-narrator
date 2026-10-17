package bench

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/games/textnorm"
	"github.com/dbirks/game-narrator/internal/matcher"
)

// ScriptProfile is the bench set's script: the messages the matcher ranks
// against (with speaker attribution, which the docs/spec/04 profile JSON the
// scenario harness loads does not carry), the region to watch, and the edges.
// It is the synthetic stand-in for a reviewed game profile (docs/spec/02); the
// ADR records that real sets will import reviewed profile rows instead.
type ScriptProfile struct {
	SchemaVersion int          `json:"schema_version"`
	ID            string       `json:"id"`
	Origin        string       `json:"origin"`
	Description   string       `json:"description"`
	Scene         string       `json:"scene"`
	Regions       []Region     `json:"regions"`
	Messages      []ScriptLine `json:"messages"`
	Edges         []Edge       `json:"edges"`
}

// ScriptLine is one scripted line with its speaker.
type ScriptLine struct {
	ID      string `json:"id"`
	Text    string `json:"text"`
	Speaker string `json:"speaker"`
}

// Region is one ROI in normalized frame coordinates.
type Region struct {
	ID   string  `json:"id"`
	Type string  `json:"type"`
	X    float64 `json:"x"`
	Y    float64 `json:"y"`
	W    float64 `json:"w"`
	H    float64 `json:"h"`
}

// Edge is one message-edges row.
type Edge struct {
	From       string  `json:"from"`
	To         string  `json:"to"`
	Type       string  `json:"type"`
	Confidence float64 `json:"confidence"`
}

// matchOpts is the retrieval fold the profile's normalized keys use: the same
// options the matcher defaults to, so a key this profile computes is the key
// the index stores (docs/spec/05 "candidate ranking").
var matchOpts = textnorm.Options{StripPunctuation: true, OCRFold: true}

// LoadScriptProfile reads and validates one set's profile.json.
func LoadScriptProfile(dir string) (*ScriptProfile, error) {
	path := filepath.Join(dir, "profile.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("bench: read profile: %v", err)
	}
	var p ScriptProfile
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("bench: parse %s: %v", path, err)
	}
	if p.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("profile %s: unsupported schema_version %d (want %d)", path, p.SchemaVersion, SchemaVersion)
	}
	if p.ID == "" || p.Scene == "" || p.Origin != "generated_synthetic" {
		return nil, fmt.Errorf("profile %s: id, scene and origin=generated_synthetic are required", path)
	}
	if len(p.Regions) == 0 {
		return nil, fmt.Errorf("profile %s: at least one region is required", path)
	}
	if len(p.Messages) == 0 {
		return nil, fmt.Errorf("profile %s: at least one message is required", path)
	}
	seen := make(map[string]bool, len(p.Messages))
	for i, m := range p.Messages {
		if m.ID == "" || m.Text == "" {
			return nil, fmt.Errorf("profile %s: message %d: id and text are required", path, i)
		}
		if m.Speaker == "" {
			return nil, fmt.Errorf("profile %s: message %s: speaker is required (use %q)", path, m.ID, SpeakerUnknown)
		}
		if seen[m.ID] {
			return nil, fmt.Errorf("profile %s: duplicate message id %q", path, m.ID)
		}
		seen[m.ID] = true
	}
	for i, r := range p.Regions {
		if r.ID == "" || r.Type == "" || r.W <= 0 || r.H <= 0 || r.X < 0 || r.Y < 0 || r.X+r.W > 1 || r.Y+r.H > 1 {
			return nil, fmt.Errorf("profile %s: region %d: id, type and a normalized rect inside [0,1] are required", path, i)
		}
	}
	return &p, nil
}

// SpeakerOf returns the script speaker of a message id, or ok=false when the
// id is not in the script.
func (p *ScriptProfile) SpeakerOf(id string) (string, bool) {
	for _, m := range p.Messages {
		if m.ID == id {
			return m.Speaker, true
		}
	}
	return "", false
}

// TextOf returns the script text of a message id.
func (p *ScriptProfile) TextOf(id string) string {
	for _, m := range p.Messages {
		if m.ID == id {
			return m.Text
		}
	}
	return ""
}

// InScript reports whether a message id is part of this script.
func (p *ScriptProfile) InScript(id string) bool {
	for _, m := range p.Messages {
		if m.ID == id {
			return true
		}
	}
	return false
}

// BuildIndex compiles the script into the matcher index the pipeline and the
// sidecar share.
func (p *ScriptProfile) BuildIndex() (*matcher.Index, error) {
	msgs := make([]games.Message, 0, len(p.Messages))
	for _, m := range p.Messages {
		msgs = append(msgs, games.Message{
			ID:             m.ID,
			ProfileID:      p.ID,
			OriginalText:   m.Text,
			NormalizedText: textnorm.Normalize(m.Text, matchOpts),
			SpeechText:     m.Text,
			SceneID:        p.Scene,
			SpeakerID:      m.Speaker,
			TextKind:       games.TextDialogue,
		})
	}
	edges := make([]games.MessageEdge, 0, len(p.Edges))
	for _, e := range p.Edges {
		edges = append(edges, games.MessageEdge{
			FromID:     e.From,
			ToID:       e.To,
			EdgeType:   games.EdgeType(e.Type),
			Confidence: e.Confidence,
		})
	}
	return matcher.Build(msgs, edges, matcher.IndexOptions{})
}

// BuildRegions projects the profile's regions into the pipeline's ROI shape.
func (p *ScriptProfile) BuildRegions() []games.Region {
	out := make([]games.Region, 0, len(p.Regions))
	for _, r := range p.Regions {
		out = append(out, games.Region{
			ID:         r.ID,
			ProfileID:  p.ID,
			RegionType: games.RegionType(r.Type),
			RectX:      r.X,
			RectY:      r.Y,
			RectW:      r.W,
			RectH:      r.H,
		})
	}
	return out
}
