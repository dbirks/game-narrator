// Package api — the region-set write (docs/spec/09 "/games/:id/regions"): the
// curator's calibration save. The whole set is one write, so a half-set cannot
// exist; the If-Match token is the profile's row version, the same token the
// message edit uses, so two open editors of the same draft collide on the row
// they both touched rather than on whichever region happens to be first.
//
// GET /api/v1/profiles/{id}/regions (the read) lives in profiles.go beside the
// other profile-scoped reads; this file owns the PUT.

package api

import (
	"encoding/json"
	"net/http"

	"github.com/dbirks/game-narrator/internal/games"
)

// regionRequest is one box as the editor sends it: geometry in normalized
// [0,1] frame fractions (the DB's own convention, so a 480i and a 1080p
// capture share the region), the class the OCR and speech defaults key off,
// and the capture constraints the preset picker fills in.
type regionRequest struct {
	ID           string          `json:"id,omitempty"`
	Label        string          `json:"label,omitempty"`
	RegionType   string          `json:"region_type"`
	DetectMethod string          `json:"detect_method,omitempty"`
	SceneID      string          `json:"scene_id,omitempty"`
	X            float64         `json:"x"`
	Y            float64         `json:"y"`
	W            float64         `json:"w"`
	H            float64         `json:"h"`
	MinAspect    *float64        `json:"min_aspect,omitempty"`
	MaxAspect    *float64        `json:"max_aspect,omitempty"`
	MinWidthPx   *int            `json:"min_width_px,omitempty"`
	MinHeightPx  *int            `json:"min_height_px,omitempty"`
	OCRSettings  json.RawMessage `json:"ocr_settings,omitempty"`
	MaskAssetIDs json.RawMessage `json:"mask_asset_ids,omitempty"`
	Reliability  *float64        `json:"reliability,omitempty"`
}

// putRegionsRequest replaces the revision's whole region set. The array is
// exhaustive on purpose: a PUT that "updates" two boxes and leaves the rest is
// a read-modify-write race the If-Match token exists to catch, and a body that
// names a subset cannot express "the boxes I see now, all of them".
type putRegionsRequest struct {
	Regions []regionRequest `json:"regions"`
}

type putRegionsResponse struct {
	Regions  []regionDTO `json:"regions"`
	Revision int         `json:"revision"`
}

// handleProfileRegionsPut serves PUT /api/v1/profiles/{id}/regions with
// If-Match. 412 when the token is stale, 409 when the revision is frozen,
// 422 with findings when a box is malformed — the same semantics the other
// draft writes answer.
func (s *Server) handleProfileRegionsPut(w http.ResponseWriter, r *http.Request) {
	if !s.catalogRequired(w, r) {
		return
	}
	p, version, ok := s.resolveProfile(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if !s.rejectIfProfileDeleted(w, r, p.DeletedAt) {
		return
	}
	var req putRegionsRequest
	if !s.decodeJSONBody(w, r, &req, maxJSONBodyBytes) {
		return
	}
	expected, ok := s.ifMatchRowVersion(w, r, version)
	if !ok {
		return
	}

	rows := make([]games.Region, 0, len(req.Regions))
	var findings []string
	for _, rr := range req.Regions {
		row := regionRequestToRow(p.ID, rr)
		// Validate runs on the row the store will get, so an unnamed box is
		// minted first and the same id travels into the 422 finding when the
		// box is malformed: what the error named is what the save wrote.
		if row.ID == "" {
			row.ID = games.NewID()
		}
		// The store restamps both inside its transaction (the set revision and
		// the write time), but Validate runs on the row as given — so the
		// handler supplies placeholders for the two audit fields, not for any
		// geometry or vocabulary the editor typed.
		if row.Revision == 0 {
			row.Revision = 1
		}
		if row.UpdatedAt == "" {
			row.UpdatedAt = nowStamp()
		}
		if err := row.Validate(); err != nil {
			// Name the box: a 422 that says "rect_x out of range" without
			// saying which of the nine boxes is useless in a full canvas.
			prefix := "region " + row.ID
			for _, f := range validationFindings(err) {
				findings = append(findings, prefix+": "+f)
			}
			continue
		}
		rows = append(rows, row)
	}
	if len(findings) > 0 {
		s.writeCodeProblemFindings(w, r, http.StatusUnprocessableEntity, "invalid-resource", codeValidationFailed,
			"Unprocessable Entity", "one or more boxes do not satisfy the region constraints", findings)
		return
	}

	saved, err := s.store.ReplaceRegions(r.Context(), p.ID, rows, expected)
	if err != nil {
		s.storeProblem(w, r, err, "replace regions", "profile")
		return
	}
	// ReplaceRegions bumps the row version exactly once, so the token the next
	// save must carry is the one the editor sent, plus one.
	setETag(w, expected+1)
	s.auditCatalogWrite(r, "catalog.regions.replace", map[string]any{
		"profile_id": p.ID, "regions": len(saved),
	})
	out := putRegionsResponse{Regions: make([]regionDTO, 0, len(saved)), Revision: 0}
	for _, rg := range saved {
		out.Regions = append(out.Regions, toRegionDTO(rg))
		out.Revision = max(out.Revision, rg.Revision)
	}
	writeJSON(w, http.StatusOK, out)
}

// regionRequestToRow is the wire→domain mapping: the editor sends a flat box
// and the store wants the validated row. Defaults are the schema's own —
// manual detection, empty OCR settings and masks — so a body that omits them
// stores what the import stores, not something new.
func regionRequestToRow(profileID string, rr regionRequest) games.Region {
	dm := rr.DetectMethod
	if dm == "" {
		dm = string(games.DetectManual)
	}
	return games.Region{
		ID:           rr.ID,
		ProfileID:    profileID,
		SceneID:      rr.SceneID,
		Label:        rr.Label,
		RegionType:   games.RegionType(rr.RegionType),
		DetectMethod: games.DetectMethod(dm),
		RectX:        rr.X,
		RectY:        rr.Y,
		RectW:        rr.W,
		RectH:        rr.H,
		MinAspect:    rr.MinAspect,
		MaxAspect:    rr.MaxAspect,
		MinWidthPx:   rr.MinWidthPx,
		MinHeightPx:  rr.MinHeightPx,
		OCRSettings:  rr.OCRSettings,
		MaskAssetIDs: rr.MaskAssetIDs,
		Reliability:  rr.Reliability,
	}
}
