package api

// TestM5NewOperationsAnswerValidatedSuccessBodies runs each operation the
// graph/regions milestone adds once through a real success path. It exists so
// the package-level coverage check (TestEveryCatalogOperationWasExercised, in
// profiles_test.go) already sees a validated success body for the two new
// writes when it runs: Go orders tests by file, and the detailed tests live in
// the regions*.go files that sort after profiles_test.go.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

func TestM5NewOperationsAnswerValidatedSuccessBodies(t *testing.T) {
	e := newCatalogEnv(t)
	_, _, profileID, etag := newDraft(t, e, "m5-contract", "m5c")

	// PUT regions: one manual box, the If-Match token the editor just read.
	rec := e.send(auth.RoleCurator, http.MethodPut,
		"/api/v1/profiles/"+profileID+"/regions",
		bytes.NewReader([]byte(`{"regions":[{"region_type":"textbox","x":0.07,"y":0.72,"w":0.86,"h":0.23}]}`)),
		"Content-Type", ContentTypeJSON, "If-Match", etag)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT regions = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	// POST regions/preview: the stub engine keeps the check hermetic, the
	// fixture comes from a replay root the test owns.
	roots := []string{t.TempDir()}
	frame := filepath.Join(roots[0], "frame.png")
	if err := os.WriteFile(frame, blackPNG(t, 640, 480), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	previewEnv(t, e, func(psm int) ocr.Engine { return stubEngine{text: "stub line"} }, roots)
	rec = e.send(auth.RoleCurator, http.MethodPost,
		"/api/v1/profiles/"+profileID+"/regions/preview",
		bytes.NewReader([]byte(`{"fixture":"frame.png","regions":[{"x":0.07,"y":0.72,"w":0.86,"h":0.23}]}`)),
		"Content-Type", ContentTypeJSON)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST preview = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	if doc["source"] != "fixture" {
		t.Errorf("source = %v, want fixture", doc["source"])
	}
}
