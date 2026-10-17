package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestDiskFullSQLiteFullIsTypedAndLeavesNoCorruption is the storage half of
// docs/spec/13's "Disk full" row: when the database cannot grow, the failing
// write returns a typed capacity error (not an opaque driver message), the
// database reports integrity, and everything committed before the failure
// survives — including a close+reopen, i.e. the WAL is recovered, not lost.
//
// Injection is the VFS-free approach: the file's growth is capped at its
// current page count with PRAGMA max_page_count, so the very next page-growth
// fails with SQLITE_FULL exactly as a full disk would. The pool is pinned to
// one connection (the pragma is per-connection), so every repository write
// through the Store meets the limit.
func TestDiskFullSQLiteFullIsTypedAndLeavesNoCorruption(t *testing.T) {
	ctx := testCtx(t)
	path := filepath.Join(t.TempDir(), FileName)

	// Phase 1: normal operation commits real data.
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.Migrate(ctx); err != nil {
		s.Close()
		t.Fatalf("Migrate: %v", err)
	}
	js := NewJobsStore(s)
	job1, _, err := js.Enqueue(ctx, EnqueueParams{Kind: "narrate", MaxAttempts: 3})
	if err != nil {
		s.Close()
		t.Fatalf("Enqueue committed job: %v", err)
	}
	_ = s.Close()

	// Phase 2: reopen with a fresh pool, cap the file at its current size.
	s = openPath(t, path)
	s.db.SetMaxOpenConns(1)
	conn, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatalf("pin connection: %v", err)
	}
	var pages int
	if err := conn.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
		t.Fatalf("read page_count: %v", err)
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA max_page_count=%d", pages)); err != nil {
		t.Fatalf("set max_page_count: %v", err)
	}
	conn.Close()
	// Prove the pool really serves the capped connection: the repository paths
	// (Enqueue → inWrite → pool connection) must be the ones that meet the wall.
	var capped int
	if err := s.db.QueryRowContext(ctx, "PRAGMA max_page_count").Scan(&capped); err != nil || capped != pages {
		t.Fatalf("pool connection is not the capped one: max_page_count=%d err=%v (want %d)", capped, err, pages)
	}
	js = NewJobsStore(s) // the queue must point at THIS store, not the closed phase-1 one

	// A new write must fail with the typed capacity error. The payload is large
	// enough that the row cannot fit in any existing page's slack: the file
	// must grow, and growing past the cap is exactly what fails. (Small rows
	// can keep fitting for a while, which would make the test flaky.)
	bigPayload := json.RawMessage(`"` + strings.Repeat("narration payload ", 65536) + `"`) // ~1 MiB
	_, _, err = js.Enqueue(ctx, EnqueueParams{Kind: "narrate", Payload: bigPayload})
	if !errors.Is(err, ErrNoSpace) {
		t.Fatalf("Enqueue on a full database: err = %v, want typed ErrNoSpace", err)
	}
	if err := s.IntegrityCheck(ctx); err != nil {
		t.Fatalf("integrity after SQLITE_FULL: %v", err)
	}
	got, err := js.GetJob(ctx, job1.ID)
	if err != nil {
		t.Fatalf("committed job unreadable after SQLITE_FULL: %v", err)
	}
	if got.State != JobQueued || got.Kind != "narrate" || got.MaxAttempts != 3 {
		t.Fatalf("committed job changed after SQLITE_FULL: %+v", got)
	}
	_ = s.Close()

	// Phase 3: WAL recovery — close+reopen, committed data intact.
	s2 := openPath(t, path)
	if err := s2.IntegrityCheck(ctx); err != nil {
		t.Fatalf("integrity after reopen: %v", err)
	}
	got2, err := NewJobsStore(s2).GetJob(ctx, job1.ID)
	if err != nil {
		t.Fatalf("committed job lost across the full-disk failure: %v", err)
	}
	if got2.ID != job1.ID {
		t.Fatalf("job id changed: got %s want %s", got2.ID, job1.ID)
	}
	t.Logf("resilience: disk_full sqlite SQLITE_FULL mapped to typed ErrNoSpace; integrity ok; committed job intact after close+reopen (WAL recovered)")
}
