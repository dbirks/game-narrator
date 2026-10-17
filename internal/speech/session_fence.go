package speech

import (
	"github.com/dbirks/game-narrator/internal/session"
)

// sessionFence adapts the session reducer to the Fence contract. Only the
// event reducer can approve a playback token (docs/spec/07 "Audio
// scheduler"); the scheduler asks it here before starting and at every
// chunk boundary.
//
// IsCurrent is EQUALITY, not "≤": a clip synthesized for generation 11 that
// arrives after generation 12 committed is stale and must never play
// (docs/spec/01 invariant 2: commit_id and utterance_generation fence stale
// asynchronous work).
type sessionFence struct {
	r *session.Reducer
}

// NewSessionFence returns a Fence that approves exactly the generation the
// session reducer has currently committed.
func NewSessionFence(r *session.Reducer) Fence {
	if r == nil {
		panic("speech: NewSessionFence requires a non-nil *session.Reducer")
	}
	return &sessionFence{r: r}
}

func (f *sessionFence) IsCurrent(generation int64) bool {
	return f.r.Generation() == generation
}
