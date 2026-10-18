package store

// Stress tests for the FTS5 index over messages (migration 0002).
//
// Field evidence: a fresh e2e data dir once failed the startup integrity check
// with `fts5: corruption found reading blob <n> from table "messages_fts"` —
// structural damage to the FTS5 inverted index while the content table was
// intact. These tests exercise, with the race detector on, every way the
// service can get there: concurrent writers on the pool (triggers + WAL +
// trigram merges), a process killed mid-transaction, and a full disk under an
// index-growing write. Each one must leave a database that reopens clean,
// because the FTS index is derived data: when it is damaged it must be
// rebuildable from messages, never the other way round.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/games"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// retryBusy re-runs a storage call until it succeeds, tolerating only the
// storage layer's "another writer holds the lock, try again" answer
// (SQLITE_BUSY at BEGIN IMMEDIATE). That is the retry contract repo_tx.go
// documents: BUSY is reported as itself rather than mapped to a conflict, so
// a client retries. The writers of this test are that client. Anything else
// — a constraint, a full disk, a real error — is returned unmodified.
func retryBusy[T any](t *testing.T, fn func() (T, error)) (T, error) {
	t.Helper()
	var zero T
	for attempt := 0; ; attempt++ {
		out, err := fn()
		if err == nil {
			return out, nil
		}
		if !isBusy(err) || attempt >= 50 {
			return zero, err
		}
		time.Sleep(time.Duration(5+2*attempt) * time.Millisecond)
	}
}

// isBusy reports whether the error is the driver's raw SQLITE_BUSY: the
// single-writer lock is held and the busy timeout (store.go: 5 s) expired.
// InTx wraps the driver error (%w), so this must unwrap, not type-assert.
func isBusy(err error) bool {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	return se.Code()&0xff == sqlite3.SQLITE_BUSY
}

// assertFTSIntact is the one assertion every FTS stress test ends with:
// the full PRAGMA integrity_check passes (this is the probe that caught the
// field corruption — the FTS5 module's own integrity-check command does NOT
// catch it for external-content tables), and the index carries no entry for
// a rowid the content table no longer has. The reverse direction is
// deliberately not asserted: a message with text too short for a trigram
// legitimately has no index entry.
func assertFTSIntact(t *testing.T, s *Store, label string) {
	t.Helper()
	if err := s.IntegrityCheck(testCtx(t)); err != nil {
		t.Fatalf("%s: PRAGMA integrity_check: %v", label, err)
	}
	var orphans int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM messages_fts f
		   LEFT JOIN messages m ON m.rowid = f.rowid
		  WHERE m.rowid IS NULL`,
	).Scan(&orphans); err != nil {
		t.Fatalf("%s: orphan-index probe: %v", label, err)
	}
	if orphans != 0 {
		t.Fatalf("%s: %d messages_fts entries reference rowids that are not in messages", label, orphans)
	}
}

// ftsLine is one message the stress writers keep on hand, with text that is
// deliberately repetitive across rows: the trigram index then holds many
// rowids per token, which is where a wrong 'delete' or a torn merge shows.
// The id is a fresh ULID (the domain validation requires one); writers that
// re-import change a message's text but keep the id it was seeded with.
func ftsLine(f *repoFixture, seq int) games.Message {
	text := fmt.Sprintf("line %02d of the harbour light keeper log", seq%17)
	m := f.message(text)
	m.NormalizedText = strings.ToLower(text)
	m.SpeechText = text + "!"
	return m
}

// TestConcurrentMessageWritersKeepFTSIntact is hypothesis (a): many
// goroutines driving the real repository paths — bulk import, re-import
// (the ON CONFLICT upsert that makes the AFTER UPDATE trigger run its
// delete-then-insert), speech-text edits, deletes — against one pooled
// connection, with readers and an FTS integrity probe interleaved. If the
// pool, the triggers or the WAL ever interleave badly, this leaves the index
// damaged and the final integrity check fails.
func TestConcurrentMessageWritersKeepFTSIntact(t *testing.T) {
	f := newRepoFixture(t)
	// The storm outlives the helper's 30 s test context on a loaded CI box;
	// give it its own generous deadline so a slow-but-correct storage layer is
	// never mistaken for a fault.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// A base revision so every writer's upsert and 'delete' command has
	// pre-existing index entries to remove, not just fresh inserts.
	base := make([]games.Message, 0, 200)
	for i := 0; i < 200; i++ {
		base = append(base, ftsLine(f, i))
	}
	if err := f.s.PutMessages(ctx, f.profile.ID, base, nil); err != nil {
		t.Fatalf("seed PutMessages: %v", err)
	}

	var (
		wg       sync.WaitGroup
		errBox   []error
		errsOnce sync.Once
		reads    atomic.Int64
	)
	failAll := func(err error) { errsOnce.Do(func() { errBox = append(errBox, err) }) }

	const writers = 8
	const rounds = 25

	// A reader that walks the search path the session runtime uses, so index
	// readers race the writers the way /play and the editor do. It rides a
	// second store (its own connection pool), the way the e2e harness, doctor
	// and a second process would share the file — readers must never stall the
	// writers, and vice versa.
	readerStore := openPath(t, f.s.Path())
	readerCtx, readerCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer readerCancel()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			if _, err := readerStore.SearchMessages(readerCtx, f.profile.ID, "harbour light", 5); err != nil {
				failAll(fmt.Errorf("reader: SearchMessages: %w", err))
				return
			}
			reads.Add(1)
			time.Sleep(time.Millisecond)
		}
	}()

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				switch (w + r) % 4 {
				case 0:
					// Bulk import of fresh lines (the first-import path).
					batch := make([]games.Message, 0, 15)
					for i := 0; i < 15; i++ {
						batch = append(batch, ftsLine(f, w*1000+r*15+i))
					}
					if _, err := retryBusy(t, func() (struct{}, error) {
						return struct{}{}, f.s.PutMessages(ctx, f.profile.ID, batch, nil)
					}); err != nil {
						failAll(fmt.Errorf("writer %d round %d: PutMessages: %w", w, r, err))
						return
					}
				case 1:
					// Re-import: seed ids (copied, text changed) — the upsert's
					// UPDATE arm runs the trigger's 'delete' command with the
					// OLD values, exactly the path the field corruption
					// implicates.
					batch := make([]games.Message, 0, 10)
					for i := 0; i < 10; i++ {
						m := base[(w*7+r*3+i)%200]
						m.SpeechText = "re-imported: " + m.SpeechText
						batch = append(batch, m)
					}
					if _, err := retryBusy(t, func() (struct{}, error) {
						return struct{}{}, f.s.PutMessages(ctx, f.profile.ID, batch, nil)
					}); err != nil {
						failAll(fmt.Errorf("writer %d round %d: re-import: %w", w, r, err))
						return
					}
				case 2:
					// An editor save on one line: optimistic-locked UPDATE.
					id := base[(w*13+r*5)%200].ID
					if _, err := retryBusy(t, func() (games.Message, error) {
						return f.s.UpdateMessageSpeechText(ctx, f.profile.ID, id,
							"edited: the lamp is burning", 1)
					}); err != nil {
						// Another writer may have deleted the line (not found)
						// or bumped the profile's row version first (the optimistic
						// lock loses the race): both are the service answering
						// correctly, not storage faults.
						if !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrRevisionConflict) {
							failAll(fmt.Errorf("writer %d round %d: UpdateMessageSpeechText: %w", w, r, err))
							return
						}
					}
				case 3:
					// A delete of one line, the way a profile-cascade
					// deletion reaches messages: a write transaction and the
					// AFTER DELETE trigger doing the FTS 'delete'. Other writers
					// re-import the same base ids, so a line deleted here comes
					// back — delete then re-insert is the other half of the
					// trigger contract.
					id := base[(w*17+r)%200].ID
					if _, err := retryBusy(t, func() (struct{}, error) {
						return struct{}{}, f.s.InTx(ctx, func(tx *Tx) error {
							_, err := tx.exec(ctx, "delete messages",
								"DELETE FROM messages WHERE id = ?", id)
							return err
						})
					}); err != nil {
						failAll(fmt.Errorf("writer %d round %d: delete: %w", w, r, err))
						return
					}
				}
				// Jitter: a synchronized herd of writers all entering
				// BEGIN IMMEDIATE at once is a test artefact; real curators and
				// session commits land at offsets. Per-writer offsets also keep
				// the pool from starving on a thundering herd.
				time.Sleep(time.Duration(200+300*w+50*r) * time.Microsecond)
			}
		}(w)
	}

	wg.Wait()
	if len(errBox) > 0 {
		t.Fatalf("concurrent writers failed: %v", errBox[0])
	}
	if reads.Load() == 0 {
		t.Fatal("the reader never completed a search: the FTS path was never exercised")
	}

	// A re-import of the whole seed revision (the e2e harness's last step)
	// over everything still present, then the final verdict on a *fresh*
	// connection pool: a torn index write is only certain after the writer's
	// pool is gone and the WAL is replayed on open.
	if err := f.s.PutMessages(ctx, f.profile.ID, base, nil); err != nil {
		t.Fatalf("final re-import: %v", err)
	}
	if err := f.s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened := openPath(t, f.s.Path())
	assertFTSIntact(t, reopened, "concurrent writers")
	t.Logf("fts stress: %d writers x %d rounds + reader + re-import; integrity ok after reopen", writers, rounds)
}

// TestFTSDiskFullFailsTypedAndLeavesNoCorruption is hypothesis (c), scoped to
// the FTS index: the existing disk-full test proves the queue path survives a
// SQLITE_FULL, but the field corruption sat in messages_fts, whose pages are
// written by the FTS5 module's own merges (REPLACE INTO *_data) inside the
// writer's transaction. The index-growing write must fail with the typed
// capacity error, roll back whole (content and index together), and reopen
// clean — a torn index page here is exactly the field failure.
func TestFTSDiskFullFailsTypedAndLeavesNoCorruption(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx

	seed := make([]games.Message, 0, 250)
	for i := 0; i < 250; i++ {
		seed = append(seed, ftsLine(f, i))
	}
	if err := f.s.PutMessages(ctx, f.profile.ID, seed, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Settle the index into data pages the way a checkpoint-driven merge does.
	mustExec(t, f.s.db, `INSERT INTO messages_fts(messages_fts) VALUES('merge')`)
	if err := f.s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen with the same one-connection pin the disk-full test uses: the
	// pragma is per-connection, and the pool must serve the capped one.
	s := openPath(t, f.s.Path())
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
		t.Fatalf("cap page count: %v", err)
	}
	conn.Close()
	var capped int
	if err := s.db.QueryRowContext(ctx, "PRAGMA max_page_count").Scan(&capped); err != nil || capped != pages {
		t.Fatalf("pool connection is not the capped one: max_page_count=%d err=%v (want %d)", capped, err, pages)
	}

	// A new line long enough that its trigrams cannot fit in existing page
	// slack: the write must grow the file, and growing past the cap is what
	// fails — with the FTS merge riding in the same transaction. The text
	// stays under the domain's 8000-character limit (Validate runs first).
	big := ftsLine(f, 999)
	big.OriginalText = strings.Repeat("the keeper winds the lamp on the harbour light ", 130)
	big.SpeechText = big.OriginalText
	if err := s.PutMessages(ctx, f.profile.ID, []games.Message{big}, nil); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("PutMessages on a full database: err = %v, want typed ErrNoSpace", err)
	}
	if err := s.IntegrityCheck(ctx); err != nil {
		t.Fatalf("integrity after SQLITE_FULL: %v", err)
	}
	// The index must still answer from the pre-failure state.
	if hits, err := s.SearchMessages(ctx, f.profile.ID, "keeper log", 5); err != nil || len(hits) == 0 {
		t.Fatalf("search after SQLITE_FULL: hits=%d err=%v, want the seeded lines", len(hits), err)
	}
	var orphans int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM messages_fts f LEFT JOIN messages m ON m.rowid = f.rowid WHERE m.rowid IS NULL`,
	).Scan(&orphans); err != nil || orphans != 0 {
		t.Fatalf("index after SQLITE_FULL: orphans=%d err=%v, want 0", orphans, err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen: the WAL recovered, the failed write is gone, the data is intact.
	s2 := openPath(t, f.s.Path())
	assertFTSIntact(t, s2, "fts disk full")
	var n int
	if err := s2.db.QueryRowContext(ctx, "SELECT count(*) FROM messages").Scan(&n); err != nil || n != 250 {
		t.Fatalf("messages after reopen: n=%d err=%v, want 250 (the failed write rolled back)", n, err)
	}
}

// TestKilledMidBulkImportLeavesNoCorruption is hypothesis (b): a second copy
// of this test binary runs the child's workload — migrate, then a long
// stream of bulk-import transactions — and the parent SIGKILLs it at several
// points after its READY marker. Every kill must leave a file that reopens
// with a passing integrity check and an index with no orphans: WAL recovery
// is what makes a mid-transaction or mid-commit kill safe, and a regression
// in it (driver, VFS, tmpfs) must fail here, not in the next e2e run's
// startup check.
func TestKilledMidBulkImportLeavesNoCorruption(t *testing.T) {
	if os.Getenv("GN_FTS_KILL_DB") != "" {
		t.Skip("child process: the parent drives this test")
	}
	bin, err := os.Executable()
	if err != nil {
		t.Fatalf("test binary path: %v", err)
	}

	const rounds = 8
	delays := []time.Duration{0, time.Millisecond, 3 * time.Millisecond, 8 * time.Millisecond,
		20 * time.Millisecond, 50 * time.Millisecond, 120 * time.Millisecond, 300 * time.Millisecond}
	for i := 0; i < rounds; i++ {
		dir := t.TempDir()
		dbPath := filepath.Join(dir, FileName)

		cmd := exec.Command(bin, "-test.count=1", "-test.run=^TestFTSKillHelper$")
		cmd.Env = append(os.Environ(), "GN_FTS_KILL_DB="+dbPath)
		stdout, pw := io.Pipe()
		cmd.Stdout = pw
		if err := cmd.Start(); err != nil {
			t.Fatalf("round %d: start child: %v", i, err)
		}

		// The reader goroutine owns the buffer; the test body only reads it
		// after cmd.Wait() (happens-before), so no sharing under the race
		// detector. READY closes the channel the kill wait blocks on.
		var outBuf bytes.Buffer
		ready := make(chan struct{})
		go func() {
			defer close(ready)
			defer pw.Close()
			scanner := bufio.NewScanner(stdout)
			scanner.Buffer(make([]byte, 64*1024), 64*1024)
			for scanner.Scan() {
				line := scanner.Text()
				outBuf.WriteString(line + "\n")
				if line == "READY" {
					return
				}
			}
		}()

		select {
		case <-ready:
		case <-time.After(60 * time.Second):
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatalf("round %d: child never printed READY:\n%s", i, outBuf.String())
		}

		time.Sleep(delays[i])
		if err := cmd.Process.Kill(); err != nil {
			t.Logf("round %d: kill (child already exiting): %v", i, err)
		}
		if err := cmd.Wait(); err == nil {
			// A clean exit means the child finished its stream before the kill
			// landed; that is a valid outcome too — the assertions below apply.
			t.Logf("round %d: child exited on its own", i)
		}

		s := openPath(t, dbPath)
		assertFTSIntact(t, s, fmt.Sprintf("kill at +%-4v", delays[i]))
	}
	t.Logf("fts stress: %d SIGKILL points across the import stream; every kill reopened clean", rounds)
}

// TestFTSKillHelper is the child half of TestKilledMidBulkImportLeavesNoCorruption.
// It runs only when GN_FTS_KILL_DB is set (the parent set it): it migrates the
// named database, prints READY once the first bulk import has committed, then
// streams bulk imports — one write transaction of ~300 messages each, the
// shape of a profile re-import — until the parent SIGKILLs it. The stream is
// deliberately uncommitted-friendly: each round is a single transaction, so
// a kill lands either mid-transaction (WAL frames without a commit) or in the
// commit window.
func TestFTSKillHelper(t *testing.T) {
	dbPath := os.Getenv("GN_FTS_KILL_DB")
	if dbPath == "" {
		t.Skip("not a kill-helper run")
	}
	s, err := Open(context.Background(), dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "kill helper: open: %v\n", err)
		os.Exit(1)
	}
	defer s.Close()
	if _, err := s.Migrate(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "kill helper: migrate: %v\n", err)
		os.Exit(1)
	}
	f, err := fixtureOnStore(t, s)
	if err != nil {
		fmt.Fprintf(os.Stderr, "kill helper: fixture: %v\n", err)
		os.Exit(1)
	}
	const batchSize = 300
	const maxRounds = 400
	for r := 0; r < maxRounds; r++ {
		batch := make([]games.Message, 0, batchSize)
		for i := 0; i < batchSize; i++ {
			batch = append(batch, ftsLine(f, r*batchSize+i))
		}
		if err := s.PutMessages(context.Background(), f.profile.ID, batch, nil); err != nil {
			fmt.Fprintf(os.Stderr, "kill helper: round %d: %v\n", r, err)
			os.Exit(1)
		}
		if r == 0 {
			fmt.Println("READY")
		}
		time.Sleep(time.Millisecond)
	}
}

// fixtureOnStore builds the game/edition/draft-profile a content repository
// call needs, on a store the caller owns (the kill helper migrates its own
// database path, which newRepoFixture cannot do).
func fixtureOnStore(t *testing.T, s *Store) (*repoFixture, error) {
	t.Helper()
	f := &repoFixture{t: t, s: s, ctx: testCtx(t)}
	f.game = games.Game{
		ID:        games.NewID(),
		Slug:      "synthetic-tide",
		Title:     "Synthetic Tide",
		Platform:  "gamecube",
		UserTags:  []byte(`["synthetic"]`),
		CreatedAt: nowRFC3339(),
	}
	if err := s.CreateGame(f.ctx, f.game); err != nil {
		return nil, fmt.Errorf("CreateGame: %w", err)
	}
	f.edition = games.Edition{
		ID:       games.NewID(),
		GameID:   f.game.ID,
		Region:   "US",
		Language: "en",
		Revision: "rev0",
		Locale:   "en_US",
	}
	if err := s.CreateEdition(f.ctx, f.edition); err != nil {
		return nil, fmt.Errorf("CreateEdition: %w", err)
	}
	p, err := s.CreateDraftProfile(f.ctx, f.edition.ID)
	if err != nil {
		return nil, fmt.Errorf("CreateDraftProfile: %w", err)
	}
	f.profile = p
	return f, nil
}
