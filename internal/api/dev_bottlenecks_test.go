package api

// Handler tests for the session bottleneck view
// (GET /api/v1/dev/sessions/{id}/bottlenecks, docs/spec/16 M7 "diagnostics
// expose bottlenecks"). The environment is the same production shape the
// other session tests use: a real replay session whose own pipeline stats the
// view reduces with the shared summary function. Every 200 body validates
// against the OpenAPI schema (clause c, via sessionEnv.send), and the named
// slowest stage is re-derived from the body's own rows rather than trusted:
// the view must agree with the rule it documents.

import (
	"net/http"
	"path/filepath"
	"testing"

	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/runtime"
)

func TestDevSessionBottlenecks(t *testing.T) {
	e := newSessionEnv(t)
	clipDir, _ := filepath.Abs(sessSyntheticDir)

	rec := e.send(auth.RoleDeveloper, http.MethodPost, "/api/v1/sessions",
		startPayload(e.profileID, "replay", clipDir, "conservative", false))
	if rec.Code != http.StatusCreated {
		t.Fatalf("start = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	id := decodeJSON(t, rec)["session_id"].(string)

	// The replay ends on its own; the view then reads the run's final stats.
	fin := e.waitTerminal(id)
	if fin["state"] != runtime.StateStopped {
		t.Fatalf("terminal state = %v, want stopped", fin["state"])
	}

	// The developer reads the view; the body is the shared summary's shape.
	bottled := e.mustOK(auth.RoleDeveloper, "/api/v1/dev/sessions/"+id+"/bottlenecks")
	if bottled["session_id"] != id {
		t.Fatalf("session_id = %v, want %s", bottled["session_id"], id)
	}
	if bottled["state"] != runtime.StateStopped {
		t.Fatalf("state = %v, want stopped", bottled["state"])
	}

	stages := bottled["stages"].([]any)
	if len(stages) != 3 {
		t.Fatalf("stages = %d rows, want 3 (every measured stage is reported)", len(stages))
	}
	wantOrder := []string{runtime.StageFrameToOCR, runtime.StageOCRToRanked, runtime.StageFrameToCommitted}
	for i, row := range stages {
		m := row.(map[string]any)
		if m["stage"] != wantOrder[i] {
			t.Fatalf("stage[%d] = %v, want %s (pipeline order)", i, m["stage"], wantOrder[i])
		}
		if n := m["count"].(float64); n < 0 {
			t.Fatalf("stage %v count = %v, want >= 0", m["stage"], n)
		}
		// The share is the ratio of two bucket edges over different sample
		// populations, so it is non-negative and 1 only for the end-to-end row
		// itself (a sub-ms run can put a sub-stage's edge above the
		// end-to-end's; the view reports that, it does not clamp it).
		if share := m["share_of_end_to_end"].(float64); share < 0 {
			t.Fatalf("stage %v share = %v, want >= 0", m["stage"], share)
		}
	}
	// The clip commits two pages, so the end-to-end stage was measured and
	// names itself with a full share.
	e2e := stages[2].(map[string]any)
	if e2e["count"].(float64) == 0 {
		t.Fatalf("end-to-end stage count = 0 for a replay that committed: %+v", e2e)
	}
	if e2e["share_of_end_to_end"].(float64) != 1 {
		t.Fatalf("end-to-end share = %v, want 1", e2e["share_of_end_to_end"])
	}

	// The named slowest stage must be the one its own rows say is slowest:
	// the sub-stage with the largest p95, the earlier stage winning a tie —
	// the end-to-end row is the baseline and never the answer when a
	// sub-stage was measured.
	slowest, _ := bottled["slowest_stage"].(string)
	if slowest == "" {
		t.Fatal("slowest_stage is empty for a run with measured stages")
	}
	winner, winnerP95 := "", -1.0
	for _, row := range stages {
		m := row.(map[string]any)
		stage := m["stage"].(string)
		if stage == runtime.StageFrameToCommitted || m["count"].(float64) == 0 {
			continue
		}
		p95 := m["p95_ns"].(float64)
		if winner == "" || p95 > winnerP95 {
			winner, winnerP95 = stage, p95
		}
	}
	if winner != slowest {
		t.Fatalf("slowest_stage = %q, but the body's own rows name %q (largest sub-stage p95)", slowest, winner)
	}
	t.Logf("bottlenecks view: slowest=%s end-to-end p95=%v ns", slowest, e2e["p95_ns"])

	// Unknown ids read 404 (as a developer: the role gate is the 403 half,
	// checked next), and a role below developer reads 403 (the view is
	// developer evidence even though the session snapshot itself is public).
	rec = e.send(auth.RoleDeveloper, http.MethodGet, "/api/v1/dev/sessions/01HAAAAAAAAAAAAAAAAAAAAAAA/bottlenecks", "")
	assertSessionProblem(t, rec, http.StatusNotFound, "session-not-found")
	rec = e.send(auth.RoleOperator, http.MethodGet, "/api/v1/dev/sessions/"+id+"/bottlenecks", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("operator read = %d, want 403 (developer-only evidence)", rec.Code)
	}
}
