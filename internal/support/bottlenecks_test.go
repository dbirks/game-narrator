package support

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/pipeline"
	"github.com/dbirks/game-narrator/internal/store"
)

// TestBottlenecksSectionNamesTheSlowOCRStage is the support half of
// docs/spec/16 M7 ("diagnostics expose bottlenecks"): a finished session whose
// persisted terminal-event stats carry a slow capture→OCR interval must show
// up in the bundle's bottlenecks.json with OCR named as the slowest stage —
// the shared summary function's reading, never a re-derivation in the bundle.
func TestBottlenecksSectionNamesTheSlowOCRStage(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()

	st, err := store.Open(ctx, store.DBPath(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	// The terminal event the way the runtime persists it: the run's stats
	// under "stats", plus a trace row the section must never copy (the
	// bundle's privacy rule, docs/spec/13 — stage names, counts, latencies).
	sentinel := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	payload, err := json.Marshal(map[string]any{
		"reason": "source_end",
		"stats": pipeline.Stats{
			FramesDelivered: 100,
			OCRCalls:        100,
			Latency: pipeline.Latency{
				FrameToOCR: pipeline.LatencySummary{
					Count: 100, P50: 50 * time.Millisecond, P95: 500 * time.Millisecond, Max: time.Second,
				},
				OCRToRanked: pipeline.LatencySummary{
					Count: 100, P50: 2 * time.Millisecond, P95: 10 * time.Millisecond, Max: 30 * time.Millisecond,
				},
				FrameToCommitted: pipeline.LatencySummary{
					Count: 90, P50: 60 * time.Millisecond, P95: 520 * time.Millisecond, Max: 1100 * time.Millisecond,
				},
			},
		},
		"trace": []any{map[string]any{"frame_sha256": sentinel}},
	})
	if err != nil {
		t.Fatal(err)
	}
	es := store.NewEventStore(st)
	if _, err := es.Append(ctx, events.Envelope{
		SessionID:  "01TESTSESSIONBOTTLENECKS00000001",
		Name:       events.SessionStopped,
		OccurredAt: time.Date(2026, 10, 19, 5, 0, 0, 0, time.UTC),
		Data:       json.RawMessage(payload),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "bundle.tar.gz")
	if _, err := Build(ctx, Options{DataDir: dataDir}, out); err != nil {
		t.Fatalf("Build: %v", err)
	}
	entries := extractArchive(t, out)

	doc, ok := entries[BottlenecksPath]
	if !ok {
		t.Fatalf("bundle carries no %s entry: %v", BottlenecksPath, keys(entries))
	}
	var bd bottlenecksDoc
	mustJSON(t, doc, &bd)

	if len(bd.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(bd.Sessions))
	}
	s := bd.Sessions[0]
	if s.SessionID != "01TESTSESSIONBOTTLENECKS00000001" {
		t.Errorf("session_id = %q", s.SessionID)
	}
	if s.Event != events.SessionStopped {
		t.Errorf("event = %q, want %q", s.Event, events.SessionStopped)
	}
	if s.SlowestStage != "frame_to_ocr" {
		t.Errorf("slowest_stage = %q, want frame_to_ocr: the slow capture→OCR stage must be named", s.SlowestStage)
	}
	if len(s.Stages) != 3 {
		t.Fatalf("stages = %d rows, want 3 (pipeline order, e2e baseline last)", len(s.Stages))
	}
	wantOrder := []string{"frame_to_ocr", "ocr_to_ranked", "frame_to_committed"}
	for i, want := range wantOrder {
		if s.Stages[i].Stage != want {
			t.Errorf("stage %d = %q, want %q", i, s.Stages[i].Stage, want)
		}
	}
	if s.Stages[2].ShareOfEndToEnd != 1 {
		t.Errorf("end-to-end share = %v, want 1 (the baseline)", s.Stages[2].ShareOfEndToEnd)
	}
	if s.Stages[0].P95NS != int64(500*time.Millisecond) {
		t.Errorf("frame_to_ocr p95 = %d ns, want %d (the terminal event's own stats)", s.Stages[0].P95NS, int64(500*time.Millisecond))
	}

	// Privacy: the terminal event's trace rows are never copied into the
	// section — the sentinel hash must not survive into the bundle.
	if strings.Contains(string(doc), sentinel) {
		t.Error("bottlenecks section carries a trace-row hash: the section is stage names, counts and latencies only")
	}
}

// keys names a map's keys for a failure message.
func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
