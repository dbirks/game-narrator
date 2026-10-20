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
	// Layout is the set's textbox geometry (ADR-0079): the box fractions and
	// corner shape the frame renderer draws and the region derivation reads.
	// Data, not code: a set whose box sits elsewhere or has square corners is
	// a different profile row, never a branch in the renderer. The zero value
	// is the first set's box (defaultLayout), so pre-ADR-0079 sets keep
	// rendering byte-identically.
	Layout Layout `json:"layout,omitempty"`
}

// Layout is one set's textbox geometry in normalized frame coordinates
// (ADR-0079). BoxX/BoxY/BoxW/BoxH are the drawn box's fractions; Rounded
// selects the rounded corners the first set renders. Zero Box* means
// defaultLayout (the first set's box), which is also what Effective returns
// for the zero value — the compatibility rule that keeps set 001's committed
// frames stable.
type Layout struct {
	BoxX    float64 `json:"box_x,omitempty"`
	BoxY    float64 `json:"box_y,omitempty"`
	BoxW    float64 `json:"box_w,omitempty"`
	BoxH    float64 `json:"box_h,omitempty"`
	Rounded bool    `json:"rounded,omitempty"`
}

// defaultLayout is the first set's box: the bottom box scripts/fixtures/
// gen-synthetic-textbox.sh draws. It is the renderer's fallback and the
// zero Layout's meaning, so a profile without a layout renders exactly the
// frames the pre-ADR-0079 constants rendered.
var defaultLayout = Layout{BoxX: 0.07, BoxY: 0.72, BoxW: 0.86, BoxH: 0.23, Rounded: true}

// Effective returns the layout with the zero-value fallback applied. A
// partially filled layout is a profile error (checked at load), so this only
// decides whole-zero → default.
func (l Layout) Effective() Layout {
	if l.BoxX == 0 && l.BoxY == 0 && l.BoxW == 0 && l.BoxH == 0 {
		return defaultLayout
	}
	return l
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
	// The layout, when present, must be a complete box inside the frame: the
	// renderer derives pixel rects from these fractions, and a partially
	// filled or out-of-range layout would render a box the region never sees.
	l := p.Layout
	if l.BoxX == 0 && l.BoxY == 0 && l.BoxW == 0 && l.BoxH == 0 {
		// Zero layout: the first set's default box. Accepted, not rewritten —
		// the on-disk profile keeps rendering by fallback (ADR-0079).
	} else if l.BoxX <= 0 || l.BoxY <= 0 || l.BoxW <= 0 || l.BoxH <= 0 ||
		l.BoxX+l.BoxW > 1 || l.BoxY+l.BoxH > 1 {
		return nil, fmt.Errorf("profile %s: layout is required to be a complete box inside the frame (got %+v)", path, l)
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
