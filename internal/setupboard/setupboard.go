// Package setupboard is the backend of the plum-peach setup board
// (docs/design/plum-peach product-notes UX-06, ADR-0108): the typed SSE
// stage events that move a character card between the board's four columns
// (FINDING · BRIEF · VOICING · DONE) and the run-level progress event that
// feeds the six-cell stage rail and the three stat tiles.
//
// One rule of the surface: an event is emitted only at a real, durable
// transition — a character row staged, a brief tool call succeeded, TTS
// takes generated, a voice revision approved — never on a timer. The stage a
// transition reports is re-derived from durable rows before every emission
// (derivation.go), so a stale retry or a re-run cannot move a card backwards
// and the board snapshot (Board) is equal to the board a client would
// assemble by replaying the events (ADR-0108, "snapshot ≡ replay").
package setupboard

import (
	"strings"

	"github.com/dbirks/game-narrator/internal/events"
)

// Stage is one column of the setup board's live board (UX-06:
// stage ∈ {finding, brief, voicing, done}).
type Stage string

const (
	// StageFinding: the character was discovered or staged; no brief yet.
	StageFinding Stage = "finding"
	// StageBrief: a voice brief was written for the character.
	StageBrief Stage = "brief"
	// StageVoicing: a TTS take was requested (audition generated).
	StageVoicing Stage = "voicing"
	// StageDone: a take was approved (human or auto-cast) and is current.
	StageDone Stage = "done"
)

// allStages is the closed vocabulary, in board order.
var allStages = []Stage{StageFinding, StageBrief, StageVoicing, StageDone}

// Valid reports whether s is a board stage.
func (s Stage) Valid() bool {
	for _, st := range allStages {
		if st == s {
			return true
		}
	}
	return false
}

// rank is the board order's index: the monotonicity guard compares ranks.
func (s Stage) rank() int {
	for i, st := range allStages {
		if st == s {
			return i
		}
	}
	return -1
}

// Event names (ADR-0108). They join the events vocabulary at package init,
// the same idempotent pattern the assistant dock uses for its card events:
// a second process boot re-registers the same names and no-op's.
const (
	// EventCharacterStage: one character moved stage. Data: CharacterStage.
	EventCharacterStage = "setup.character_stage"
	// EventProgress: the run-level tile/rail state. Data: Progress.
	EventProgress = "setup.progress"
)

func init() {
	// The registration is deliberately not checked for failure: the names are
	// package constants this file's init validates by construction, and a
	// second boot's duplicate registration is the success case.
	_ = events.Register(EventCharacterStage, EventProgress)
}

// CharacterStage is the typed payload of a setup.character_stage event
// (ADR-0108): one card's move, with everything the board needs to place it.
type CharacterStage struct {
	// RunID names the onboarding run the board belongs to. The SSE stream is
	// global, so a payload without it would be unboardable by a client.
	RunID string `json:"run_id"`
	// CharacterID is the characters.id of the moved card.
	CharacterID string `json:"character_id"`
	// Name is the character's display name (the card's tab).
	Name string `json:"name"`
	// Stage is the column the card moved to.
	Stage Stage `json:"stage"`
	// Lines is the character's cited-line count (the card's line count).
	Lines int `json:"lines"`
	// At is the RFC 3339 time the transition was observed.
	At string `json:"at"`
}

// CellState is one state of a stage-rail cell.
type CellState string

const (
	CellPending CellState = "pending"
	CellActive  CellState = "active"
	CellDone    CellState = "done"
)

// RailCell is one of the six cells of the setup page's stage rail (plan.md
// item 4: Find game · Dialogue · Characters · Briefs · Voices · Publish).
type RailCell struct {
	// ID is the cell's stable identifier.
	ID string `json:"id"`
	// Label is the cell's display name.
	Label string `json:"label"`
	// State is the cell's position in the run.
	State CellState `json:"state"`
	// Value is the cell's number, the rail's "value" slot (e.g. "4,212
	// lines", "3/5 briefs", the profile status on the publish cell).
	Value string `json:"value,omitempty"`
}

// The rail's cells in board order (plan.md item 4).
const (
	RailFindGame   = "find_game"
	RailDialogue   = "dialogue"
	RailCharacters = "characters"
	RailBriefs     = "briefs"
	RailVoices     = "voices"
	RailPublish    = "publish"
)

// railOrder is the six cells, in the order the strip renders them.
var railOrder = []struct{ id, label string }{
	{RailFindGame, "Find game"},
	{RailDialogue, "Dialogue"},
	{RailCharacters, "Characters"},
	{RailBriefs, "Briefs"},
	{RailVoices, "Voices"},
	{RailPublish, "Publish"},
}

// Progress is the typed payload of a setup.progress event (ADR-0108): the
// run-level state behind the setup page's three stat tiles (lines read ·
// characters · voiced) and its six-cell stage rail.
type Progress struct {
	// RunID names the onboarding run (see CharacterStage.RunID).
	RunID string `json:"run_id"`
	// LinesRead is the profile's total dialogue line count.
	LinesRead int `json:"lines_read"`
	// Characters is the character count the board shows cards for.
	Characters int `json:"characters"`
	// Voiced counts characters at stage voicing or later: a TTS take exists
	// (done means the take is approved and current).
	Voiced int `json:"voiced"`
	// Total is the character count the voiced bar divides by.
	Total int `json:"total"`
	// Elapsed is whole seconds since the run was created.
	Elapsed int64 `json:"elapsed"`
	// StageRail is the six-cell rail, in board order.
	StageRail []RailCell `json:"stage_rail"`
}

// Board is the GET /api/v1/onboarding/runs/{id}/board snapshot (ADR-0108):
// the page load that cannot trust the stream (a fresh load, a pruned log)
// reconstructs the board from this body instead. Its fields are the same
// durable facts the setup.* events carry, so a snapshot is equal to the
// board a client assembles by replaying the events.
type Board struct {
	// RunID names the run.
	RunID string `json:"run_id"`
	// State is the run's durable state (queued/running/…/succeeded).
	State string `json:"state"`
	// CreatedAt is the run's creation time (RFC 3339).
	CreatedAt string `json:"created_at"`
	// Characters is one entry per character on the run's profile, in stable
	// (name) order.
	Characters []BoardCharacter `json:"characters"`
	// Progress is the setup.progress payload, computed the same way the
	// events carry it.
	Progress Progress `json:"progress"`
}

// BoardCharacter is one card of the snapshot's live board.
type BoardCharacter struct {
	// CharacterID is the characters.id.
	CharacterID string `json:"character_id"`
	// Name is the display name.
	Name string `json:"name"`
	// Stage is the card's column, derived from the durable rows.
	Stage Stage `json:"stage"`
	// Lines is the character's cited-line count.
	Lines int `json:"lines"`
	// At is the observed time of the character's newest
	// setup.character_stage event (RFC 3339), empty when the event log
	// holds none (the hub was unwired when the transitions happened).
	At string `json:"at,omitempty"`
}

// clampLines keeps a derived count non-negative: a store fault that would
// render "-3 lines" on a card is worse than "0 lines".
func clampLines(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// clipName bounds a name echo in an event payload (the events package caps
// identifier-like fields but not display strings; a 400-rune name is not a
// card tab).
func clipName(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		return s[:200]
	}
	return s
}
