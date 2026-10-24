package api

// The generic kind voices (ADR-0158, issue #51, ADR-0185): the seven roster
// keys `kind:adult_male` … `kind:creature` the unnamed speakers speak with.
// The cast page's "Generic kind voices" section reads GET /api/v1/voices/kinds
// (coverage: missing / pending / approved plus the measured F0 and class per
// kind) and the one-click "Create missing" posts to
// POST /api/v1/voices/kinds/create-missing, which reuses the existing
// audition path (internal/voices) and the cast audit's pitch classifier
// (internal/cast) — it does not duplicate either. The run leaves the passing
// takes PENDING: the approve click is the operator's (AGENTS.md rule 6).

import (
	"net/http"

	"github.com/dbirks/game-narrator/internal/cast"
)

// kindManager builds the cast orchestrator over the wired voice subsystem.
// The voice service satisfies both the audition seam (CreateAuditions) and
// its own store seam (ListVoiceProfiles / ListAuditions), and its asset
// store is where the take WAVs live.
func (s *Server) kindManager() *cast.KindManager {
	svc := s.voices.Service
	return cast.NewKindManager(svc, svc.Store, svc.Assets)
}

func (s *Server) handleKindVoices(w http.ResponseWriter, r *http.Request) {
	if !s.voiceRequired(w, r) {
		return
	}
	rows, err := s.kindManager().Coverage(r.Context())
	if err != nil {
		s.writeCodeProblem(w, r, http.StatusInternalServerError, "kind-coverage-fault", "INTERNAL",
			"Internal Server Error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"kinds": rows})
}

func (s *Server) handleKindVoicesCreateMissing(w http.ResponseWriter, r *http.Request) {
	if !s.voiceRequired(w, r) {
		return
	}
	// The run is synchronous by design: one take per missing kind (up to the
	// orchestrator's MaxKindAttempts), each a single VoiceDesign call. The
	// operator clicked once and wants the answer on this click, not a job to
	// poll; the per-kind failures are reported on the rows, not the status.
	rows, err := s.kindManager().CreateMissing(r.Context())
	if err != nil {
		s.writeCodeProblem(w, r, http.StatusInternalServerError, "kind-create-fault", "INTERNAL",
			"Internal Server Error", err.Error())
		return
	}
	s.auditCatalogWrite(r, "voice.kinds.create_missing", map[string]any{
		"created": countKindCreated(rows),
	})
	writeJSON(w, http.StatusOK, map[string]any{"kinds": rows})
}

func countKindCreated(rows []cast.KindRow) int {
	n := 0
	for _, r := range rows {
		if r.State == cast.KindStatePending && r.Attempts > 0 {
			n++
		}
	}
	return n
}
