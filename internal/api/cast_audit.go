package api

// The cast audit endpoint (ADR-0171): GET /api/v1/profiles/{id}/cast/audit.
// Read-only. For every roster row with an assigned voice it measures the
// reference's median F0 (the ADR-0162 YIN estimator) and reports the
// expected class from the character's profile data against the measured
// class. The response carries the character's own profile name, class
// labels and Hz numbers — never game text (AGENTS.md rule 5).

import (
	"net/http"

	"github.com/dbirks/game-narrator/internal/cast"
)

// castAuditRowDTO is one row of the audit report.
type castAuditRowDTO struct {
	Character    string  `json:"character"`
	VoiceID      string  `json:"voice_id,omitempty"`
	Expected     string  `json:"expected"`
	MeasuredF0Hz float64 `json:"measured_f0_hz"`
	Measured     string  `json:"measured"`
	Verdict      string  `json:"verdict"`
}

func (s *Server) handleCastAudit(w http.ResponseWriter, r *http.Request) {
	if !s.catalogRequired(w, r) {
		return
	}
	p, _, ok := s.resolveProfile(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	// The reference audio is read from the asset store: the import seam
	// normally carries it, the voice seam as fallback.
	var aud *cast.Auditor
	switch {
	case s.assets != nil:
		aud = cast.NewAuditor(s.store, s.assets)
	case s.voices != nil && s.voices.Service != nil:
		aud = cast.NewAuditor(s.store, s.voices.Service.Assets)
	default:
		s.writeCodeProblem(w, r, http.StatusServiceUnavailable, "assets-unwired", "INTERNAL",
			"Service Unavailable", "the asset store is not wired; the audit cannot read reference audio")
		return
	}
	rows, err := aud.Profile(r.Context(), p.ID)
	if err != nil {
		s.storeProblem(w, r, err, "audit cast", "character")
		return
	}
	out := make([]castAuditRowDTO, 0, len(rows))
	for _, row := range rows {
		out = append(out, castAuditRowDTO{
			Character: row.Character, VoiceID: row.VoiceID,
			Expected: row.Expected, MeasuredF0Hz: row.MeasuredF0Hz,
			Measured: row.Measured, Verdict: row.Verdict,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"profile": p.ID, "rows": out})
}
