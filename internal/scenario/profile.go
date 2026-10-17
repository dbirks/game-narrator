// profile.go loads the profile JSON (docs/spec/04 "Profile structure")
// and the fixture ground truth, and projects the profile into the pipeline,
// matcher and tracker shapes the rest of the system consumes.

package scenario

import (
	"encoding/json"
	"fmt"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/games/textnorm"
	"os"
	"path/filepath"
)

// --- ground truth ------------------------------------------------------------

// Truth is one frame's ground truth: the per-frame rows the fixture generator
// writes to truth.json next to the clip (docs/spec/11 "Fixture specification").
type Truth struct {
	File    string // frame file name
	TsNS    int64  // capture timestamp on the clip's source clock
	Present bool   // box_present: the textbox is on screen in this ROI
	Text    string // the text the frame shows ("" = none)
	Line    string // the fixture's line label ("a", "b", "")
	Full    bool   // text_is_full_line: the whole line is on screen
}

// LoadTruth reads <dir>/truth.json.
func LoadTruth(dir string) ([]Truth, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "truth.json"))
	if err != nil {
		return nil, fmt.Errorf("truth: read %s: %v", dir, err)
	}
	var rows []struct {
		File    string `json:"file"`
		TsNS    int64  `json:"ts_ns"`
		Present bool   `json:"box_present"`
		Text    string `json:"text"`
		Line    string `json:"line"`
		Full    bool   `json:"text_is_full_line"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("truth: parse %s/truth.json: %v", dir, err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("truth: %s/truth.json names no frames", dir)
	}
	out := make([]Truth, 0, len(rows))
	for _, r := range rows {
		out = append(out, Truth{File: r.File, TsNS: r.TsNS, Present: r.Present, Text: r.Text, Line: r.Line, Full: r.Full})
	}
	return out, nil
}

// --- synthetic profile --------------------------------------------------------

// Profile is a synthetic dialogue profile as stored under
// testdata/synthetic/profiles: the games tables' columns in JSON (docs/spec/02
// "Import, review, publish"), scoped to what a frame-replay scenario needs.
// Every line is invented (docs/spec/11 "Fixture origins").
type Profile struct {
	ID          string           `json:"id"`
	Origin      string           `json:"origin"`
	Description string           `json:"description"`
	TextKind    string           `json:"text_kind"`
	Messages    []ProfileMessage `json:"messages"`
	Edges       []ProfileEdge    `json:"edges"`
	Regions     []ProfileRegion  `json:"regions"`
}

// ProfileMessage is one row of the messages table.
type ProfileMessage struct {
	ID           string `json:"id"`
	OriginalText string `json:"original_text"`
	SceneID      string `json:"scene_id"`
}

// ProfileEdge is one row of the message_edges table.
type ProfileEdge struct {
	From       string  `json:"from"`
	To         string  `json:"to"`
	Type       string  `json:"type"`
	Confidence float64 `json:"confidence"`
}

// ProfileRegion is one row of the regions table; the rect is in normalized
// frame coordinates, the same space games.Region stores.
type ProfileRegion struct {
	ID   string  `json:"id"`
	Type string  `json:"type"`
	X    float64 `json:"x"`
	Y    float64 `json:"y"`
	W    float64 `json:"w"`
	H    float64 `json:"h"`
}

// matchOpts are the textnorm options retrieval compares with (docs/spec/12
// "candidate ranking"): punctuation stripped, OCR glyph confusions folded.
// Both sides of every comparison go through these options, per textnorm's own
// contract.
var matchOpts = textnorm.Options{StripPunctuation: true, OCRFold: true}

// fold is the package's comparison fold.
func fold(s string) string { return textnorm.Normalize(s, matchOpts) }

// LoadProfile reads and sanity-checks one profile JSON.
func LoadProfile(path string) (*Profile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("profile: read %s: %v", path, err)
	}
	var p Profile
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("profile: parse %s: %v", path, err)
	}
	if p.ID == "" {
		return nil, fmt.Errorf("profile: %s: id is required", path)
	}
	if len(p.Messages) == 0 {
		return nil, fmt.Errorf("profile: %s: at least one message is required", path)
	}
	ids := make(map[string]bool, len(p.Messages))
	for _, m := range p.Messages {
		if m.ID == "" || m.OriginalText == "" {
			return nil, fmt.Errorf("profile: %s: every message needs an id and original_text", path)
		}
		if ids[m.ID] {
			return nil, fmt.Errorf("profile: %s: message id %q appears twice", path, m.ID)
		}
		ids[m.ID] = true
	}
	for i, e := range p.Edges {
		if !ids[e.From] || !ids[e.To] {
			return nil, fmt.Errorf("profile: %s: edge %d references an unknown message (%q -> %q)", path, i, e.From, e.To)
		}
		if e.Type == "" {
			return nil, fmt.Errorf("profile: %s: edge %d has no type", path, i)
		}
		if e.Confidence < 0 || e.Confidence > 1 {
			return nil, fmt.Errorf("profile: %s: edge %d confidence %v is outside [0,1]", path, i, e.Confidence)
		}
	}
	if len(p.Regions) == 0 {
		return nil, fmt.Errorf("profile: %s: at least one region is required", path)
	}
	for i, r := range p.Regions {
		if r.ID == "" || r.Type != "textbox" {
			return nil, fmt.Errorf("profile: %s: region %d must be a textbox with an id", path, i)
		}
		if r.X < 0 || r.Y < 0 || r.W <= 0 || r.H <= 0 || r.X+r.W > 1.0001 || r.Y+r.H > 1.0001 {
			return nil, fmt.Errorf("profile: %s: region %d rect (%.3f, %.3f, %.3f, %.3f) does not fit the frame", path, i, r.X, r.Y, r.W, r.H)
		}
	}
	if p.TextKind == "" {
		p.TextKind = "dialogue"
	}
	if games.TextKind(p.TextKind) != games.TextDialogue {
		return nil, fmt.Errorf("profile: %s: text_kind %q is not supported by this loader", path, p.TextKind)
	}
	return &p, nil
}

// Messages renders the rows as games.Message: original text is kept for
// display, normalized text is the retrieval key (docs/spec/02 stores the
// renderings side by side), speech text is what a narrator would say.
func (p *Profile) ToMessages() []games.Message {
	out := make([]games.Message, 0, len(p.Messages))
	for _, m := range p.Messages {
		out = append(out, games.Message{
			ID:             m.ID,
			ProfileID:      p.ID,
			OriginalText:   m.OriginalText,
			NormalizedText: fold(m.OriginalText),
			SpeechText:     m.OriginalText,
			SceneID:        m.SceneID,
			TextKind:       games.TextKind(p.TextKind),
		})
	}
	return out
}

// Edges renders the rows as games.MessageEdge.
func (p *Profile) ToEdges() []games.MessageEdge {
	out := make([]games.MessageEdge, 0, len(p.Edges))
	for _, e := range p.Edges {
		out = append(out, games.MessageEdge{
			FromID:     e.From,
			ToID:       e.To,
			EdgeType:   games.EdgeType(e.Type),
			Confidence: e.Confidence,
		})
	}
	return out
}

// Regions renders the rows as games.Region, in the normalized coordinates the
// pipeline's ROI stage crops with.
func (p *Profile) ToRegions() []games.Region {
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
