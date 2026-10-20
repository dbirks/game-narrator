package api

// GET /api/v1/onboarding/runs/{id}/board — the setup board snapshot
// (docs/design/plum-peach product-notes UX-06, ADR-0108). The live half of
// the board is the SSE stream (setup.character_stage / setup.progress,
// replayable via Last-Event-ID); this is the half a page load that cannot
// trust the stream — a fresh load, a pruned or ring-evicted log — rebuilds
// from. The snapshot is derived from the same durable rows the events are
// emitted from, so it is equal to the board a client assembles by replaying
// the events (ADR-0108, "snapshot ≡ replay").

import (
	"net/http"

	"github.com/dbirks/game-narrator/internal/setupboard"
)

// WithSetupBoard wires the setup board snapshot route. Nil (or a server
// without the onboarding deps) answers 503, the same honest-degradation rule
// the other onboarding routes follow: a board that answered 200 from
// nothing would let a setup page believe it was watching a run it was not.
func WithSetupBoard(em *setupboard.Emitter) Option {
	return func(s *Server) {
		s.setupBoard = em
	}
}

// handleOnboardingBoard serves GET /api/v1/onboarding/runs/{id}/board.
func (s *Server) handleOnboardingBoard(w http.ResponseWriter, r *http.Request) {
	if !s.onboardingRequired(w, r) {
		return
	}
	if s.setupBoard == nil {
		s.writeProblem(w, r, http.StatusServiceUnavailable, "setup-board-unavailable",
			"Setup Board Unavailable",
			"the setup board snapshot is not wired in this process; the SSE stage events remain available")
		return
	}
	runID, ok := s.onbRunIDFromPath(w, r)
	if !ok {
		return
	}
	if _, ok := s.onbLoadRun(w, r, runID); !ok {
		return
	}
	board, err := s.setupBoard.Board(r.Context(), runID)
	if err != nil {
		s.storeProblem(w, r, err, "assemble board", "board")
		return
	}
	writeJSON(w, http.StatusOK, board)
}
