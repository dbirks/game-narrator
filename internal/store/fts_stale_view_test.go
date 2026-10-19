package store

// Regression tests for the FTS5 per-connection stale structure view
// (modernc.org/sqlite v1.60.1, SQLite 3.53.4) behind the 2026-10-19 field
// incident: an idle device whose readiness probe reported
// `fts5: corruption found reading blob <n> from table "messages_fts"` for
// seven minutes, while the file on disk — verified after a clean stop —
// passed every check and was never damaged (ADR-0062, 2026-10-19 amendment).
//
// The mechanism, reproduced deterministically in this build: the FTS5 module
// caches the inverted-index structure per connection, and that cache is not
// invalidated by commits made on other connections — only by the next
// statement on the connection itself. A pooled connection that read
// messages_fts and then sat idle while the index was rewritten elsewhere
// reports the phantom corruption from PRAGMA integrity_check; a fresh
// connection and the file on disk report clean. Running the check itself
// populates the cache, so even a dedicated long-lived connection poisons
// itself the same way after an idle rewrite.
//
// The consequence these tests pin: Store's integrity probe and the
// self-repair guard must only ever read a fresh connection's view, so a
// stale pooled view can neither take the service down nor trigger a rebuild
// of a healthy index.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/games"
)

// seedLinesOn imports n messages into profileID through the repository path,
// on store st — so migration 0002's FTS triggers run on st's own pool.
func seedLinesOn(t *testing.T, st *Store, profileID string, n, base int) {
	t.Helper()
	text := func(i int) string {
		return fmt.Sprintf("harbour light line %03d the keeper climbed the tower at dusk", base+i)
	}
	msgs := make([]games.Message, 0, n)
	for i := 0; i < n; i++ {
		msgs = append(msgs, games.Message{
			ID:                games.NewID(),
			ProfileID:         profileID,
			OriginalText:      text(i),
			NormalizedText:    strings.ToLower(text(i)),
			SpeechText:        text(i) + "!",
			Language:          "en",
			TextKind:          games.TextDialogue,
			AttributionStatus: games.AttributionUnknown,
			Revision:          1,
			CreatedAt:         nowRFC3339(),
			UpdatedAt:         nowRFC3339(),
		})
	}
	if err := st.PutMessages(testCtx(t), profileID, msgs, nil); err != nil {
		t.Fatalf("seed %d messages on %s: %v", n, st.Path(), err)
	}
}

// seedLines is the fixture-flavoured form of seedLinesOn.
func seedLines(t *testing.T, f *repoFixture, n, base int) {
	seedLinesOn(t, f.s, f.profile.ID, n, base)
}

// staleFTSSetup pins the store's pool down to one physical connection, makes
// that connection read the FTS index, and rewrites the index on a different
// connection (a second store on the same file — the field journal's
// import/session/cleanup ran on different pooled connections). Afterwards the
// store's only pooled connection carries a stale FTS5 structure view while
// the file is clean.
func staleFTSSetup(t *testing.T, s *Store, f *repoFixture) *Store {
	t.Helper()
	seedLines(t, f, 250, 0) // the journal's import phase

	// One physical connection for the pool: the FTS read below and any
	// statement the pool then runs must land on that same connection.
	s.db.SetMaxOpenConns(1)

	var n int
	if err := s.db.QueryRowContext(testCtx(t), "SELECT count(*) FROM messages_fts").Scan(&n); err != nil {
		t.Fatalf("FTS read on the pool: %v", err)
	}

	other, err := Open(testCtx(t), s.Path())
	if err != nil {
		t.Fatalf("second store on %s: %v", s.Path(), err)
	}
	t.Cleanup(func() { _ = other.Close() })
	seedLinesOn(t, other, f.profile.ID, 250, 250) // the rewrite, on another connection
	return other
}

// integrityCheckLines runs PRAGMA integrity_check on q and returns the
// report lines ("ok" for a clean database).
func integrityCheckLines(t *testing.T, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) []string {
	t.Helper()
	rows, err := q.QueryContext(testCtx(t), "PRAGMA integrity_check")
	if err != nil {
		t.Fatalf("PRAGMA integrity_check: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan integrity_check line: %v", err)
		}
		out = append(out, line)
	}
	return out
}

// isPhantomFTSDamage reports whether the lines are the field's phantom
// signature: FTS5 naming the derived index, nothing else.
func isPhantomFTSDamage(lines []string) bool {
	return len(lines) > 0 && IsFTSOnlyDamage(lines)
}

// TestIntegrityCheckIgnoresStalePooledFTSView is the field repro, asserted
// against the store's probe:
//
//   - mechanism (pinned for the record): the stale pooled connection reports
//     the exact field signature while the file and a fresh connection report
//     clean;
//   - behaviour: IntegrityCheck reports the committed truth. Pre-fix the
//     check ran on the pool and surfaced the phantom damage (failing this
//     test); post-fix it runs on a dedicated fresh connection and reports ok.
func TestIntegrityCheckIgnoresStalePooledFTSView(t *testing.T) {
	s := migratedStore(t)
	f := fixtureOn(t, s)
	other := staleFTSSetup(t, s, f)

	// The mechanism: the stale pooled connection lies, the file is clean.
	stale, err := s.db.Conn(testCtx(t))
	if err != nil {
		t.Fatalf("pin the pooled connection: %v", err)
	}
	defer stale.Close()
	staleLines := integrityCheckLines(t, stale)
	if !isPhantomFTSDamage(staleLines) {
		t.Fatalf("the stale pooled connection reported %v, want the field's phantom fts5 damage: the repro is not in effect on this driver build — if modernc.org/sqlite fixed the FTS5 structure cache, update this test and ADR-0062", staleLines)
	}
	if lines := integrityCheckLines(t, other.db); len(lines) != 1 || lines[0] != "ok" {
		t.Fatalf("fresh connection reported %v, want ok: the test's premise (a clean file) failed", lines)
	}

	// The probe must not surface the stale view.
	if err := s.IntegrityCheck(testCtx(t)); err != nil {
		t.Fatalf("IntegrityCheck surfaced the stale pooled FTS view as damage: %v", err)
	}
}

// TestRecoverDerivedFTSNeverRebuildsOnAStaleView: a stale pooled view must
// not make the guard "repair" a healthy index. Pre-fix the guard's pre-check
// read the phantom damage from the pool, ran a 'rebuild' over a healthy
// index, and reported repaired — the dc29274 guard doing its worst case
// (a wasted O(database) rebuild of derived state the service must never
// touch without real damage). Post-fix the guard confirms the damage on a
// fresh connection, finds nothing, and rebuilds nothing.
func TestRecoverDerivedFTSNeverRebuildsOnAStaleView(t *testing.T) {
	s := migratedStore(t)
	f := fixtureOn(t, s)
	_ = staleFTSSetup(t, s, f)

	repaired, err := s.RecoverDerivedFTS(testCtx(t))
	if repaired {
		t.Fatalf("RecoverDerivedFTS reported a repair: the database only ever had a stale pooled FTS view, the file was clean; the guard must confirm damage on a fresh connection before rebuilding")
	}
	if err != nil {
		t.Fatalf("RecoverDerivedFTS on a clean database: %v", err)
	}
}
