package api

// The region-set write (docs/spec/09 "/games/:id/regions"): If-Match on the
// profile's row version (428 absent, 412 stale), draft-only (409 frozen),
// geometry findings (422 naming the box), and the set-revision advance the
// editor sees on the next read.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/games"
)

// regionsDoc is the PUT response shape; the GET list shares the region rows.
type regionsDoc struct {
	Regions []struct {
		ID         string  `json:"id"`
		RegionType string  `json:"region_type"`
		RectX      float64 `json:"rect_x"`
		Revision   int     `json:"revision"`
	} `json:"regions"`
	Revision int `json:"revision"`
}

func putRegions(t *testing.T, e *catalogEnv, role auth.Role, profileID, etag, body string) *httptest.ResponseRecorder {
	t.Helper()
	return e.send(role, http.MethodPut, "/api/v1/profiles/"+profileID+"/regions",
		strings.NewReader(body), "Content-Type", ContentTypeJSON, "If-Match", etag)
}

func TestRegionReplaceRequiresIfMatch(t *testing.T) {
	e := newCatalogEnv(t)
	_, _, profileID, _ := newDraft(t, e, "regions-nomatch", "rn")
	_ = profileID

	rec := e.send(auth.RoleCurator, http.MethodPut,
		"/api/v1/profiles/"+profileID+"/regions",
		strings.NewReader(`{"regions":[{"region_type":"textbox","x":0.1,"y":0.7,"w":0.8,"h":0.2}]}`),
		"Content-Type", ContentTypeJSON)
	requireProblem(t, rec, http.StatusPreconditionRequired, codeRevisionConflict)
}

func TestRegionReplaceSavesAndBumpsTheToken(t *testing.T) {
	e := newCatalogEnv(t)
	_, _, profileID, etag := newDraft(t, e, "regions-save", "rs")
	body := `{"regions":[
		{"region_type":"textbox","x":0.07,"y":0.72,"w":0.86,"h":0.23,"label":"the line"},
		{"region_type":"speaker_label","x":0.05,"y":0.55,"w":0.3,"h":0.1,
		 "min_aspect":1.33,"max_aspect":1.34,"min_width_px":640,"min_height_px":480}
	]}`
	rec := putRegions(t, e, auth.RoleCurator, profileID, etag, body)
	if rec.Code != http.StatusOK {
		e.t.Fatalf("PUT regions = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var saved regionsDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		e.t.Fatalf("decode: %v", err)
	}
	if len(saved.Regions) != 2 {
		t.Fatalf("saved %d regions, want 2", len(saved.Regions))
	}
	if saved.Revision != 1 {
		t.Errorf("set revision = %v, want 1 for the first save", saved.Revision)
	}
	newETag := rec.Header().Get("ETag")
	if newETag == "" || newETag == etag {
		t.Errorf("the save did not move the token: etag %q, old %q", newETag, etag)
	}

	// The saved set reads back through the existing GET with the geometry intact.
	list := e.mustOK(auth.RoleCurator, "/api/v1/profiles/"+profileID+"/regions")
	rows, _ := list["regions"].([]any)
	if len(rows) != 2 {
		t.Fatalf("GET regions returned %d rows, want 2", len(rows))
	}
	for _, raw := range rows {
		row := raw.(map[string]any)
		if rx, ok := row["rect_x"].(float64); !ok || rx < 0 || rx > 1 {
			t.Errorf("rect_x %v out of [0,1] after a save", row["rect_x"])
		}
		if rev, ok := row["revision"].(float64); !ok || int(rev) != 1 {
			t.Errorf("stored revision %v, want the set revision 1", row["revision"])
		}
	}

	// A second save advances the set revision exactly one further.
	rec = putRegions(t, e, auth.RoleCurator, profileID, newETag, body)
	if rec.Code != http.StatusOK {
		e.t.Fatalf("second PUT = %d (body %s)", rec.Code, rec.Body.String())
	}
	freshETag := rec.Header().Get("ETag")
	var second regionsDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &second); err != nil {
		e.t.Fatalf("decode: %v", err)
	}
	if second.Revision != 2 {
		t.Errorf("second save stamped revision %v, want 2", second.Revision)
	}

	// The stale token from the first read now loses, with the fresh token in
	// the header so the editor's recovery is one re-read.
	rec = putRegions(t, e, auth.RoleCurator, profileID, etag, body)
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale PUT = %d, want 412", rec.Code)
	}
	if fresh := rec.Header().Get("ETag"); fresh != freshETag {
		t.Errorf("412 ETag = %q, want the current token %q", fresh, freshETag)
	}
}

func TestRegionReplaceRefusesOffFrameGeometry(t *testing.T) {
	e := newCatalogEnv(t)
	_, _, profileID, etag := newDraft(t, e, "regions-geom", "rg")

	// x + w runs past the right edge: legal fractions describing an impossible
	// rectangle, and the 422 names the box that is wrong.
	body := `{"regions":[
		{"region_type":"textbox","x":0.6,"y":0.2,"w":0.5,"h":0.2},
		{"region_type":"textbox","x":0.1,"y":0.9,"w":0.2,"h":0.2}
	]}`
	rec := putRegions(t, e, auth.RoleCurator, profileID, etag, body)
	doc := requireProblem(t, rec, http.StatusUnprocessableEntity, codeValidationFailed)
	if len(doc.Findings) == 0 {
		t.Errorf("the 422 carried no findings: the editor cannot fix an unnamed box")
	}

	// Nothing may have been written: a partial set is exactly the corruption
	// the transaction exists to prevent.
	list := e.mustOK(auth.RoleCurator, "/api/v1/profiles/"+profileID+"/regions")
	rows, _ := list["regions"].([]any)
	if len(rows) != 0 {
		t.Errorf("a refused save stored %d regions", len(rows))
	}
}

func TestRegionReplaceRefusesBogusVocabulary(t *testing.T) {
	e := newCatalogEnv(t)
	_, _, profileID, etag := newDraft(t, e, "regions-vocab", "rv")

	rec := putRegions(t, e, auth.RoleCurator, profileID, etag,
		`{"regions":[{"region_type":"subtitle_box","x":0.1,"y":0.1,"w":0.1,"h":0.1}]}`)
	requireProblem(t, rec, http.StatusUnprocessableEntity, codeValidationFailed)
}

func TestRegionReplaceOnAPublishedRevisionIsAConflict(t *testing.T) {
	e := newCatalogEnv(t)
	_, _, profileID, etag := newDraft(t, e, "regions-published", "rp")

	// Draft → review → publish through the same store the handlers use.
	version := mustAtoi(strings.Trim(etag, `"`))
	if err := e.st.SetProfileStatus(e.ctx, profileID, games.StatusReview, version); err != nil {
		e.t.Fatalf("SetProfileStatus: %v", err)
	}
	v2 := version + 1
	if _, err := e.st.Publish(e.ctx, profileID, v2, "test"); err != nil {
		e.t.Fatalf("Publish: %v", err)
	}

	rec := putRegions(t, e, auth.RoleCurator, profileID, e.etag(profileID),
		`{"regions":[{"region_type":"textbox","x":0.1,"y":0.1,"w":0.1,"h":0.1}]}`)
	requireProblem(t, rec, http.StatusConflict, codeImmutable)
}
