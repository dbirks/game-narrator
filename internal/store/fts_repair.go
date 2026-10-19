package store

// Self-repair for the one derived table the schema owns: messages_fts.
//
// Migration 0002 keeps messages_fts (an FTS5 external-content index over
// messages) in sync with three triggers, and the index is stored in the
// shadow table messages_fts_data as page blobs that the FTS5 module writes
// with its own merges. A database that arrives with the messages content
// intact but a damaged index page — the field failure reported by
// `PRAGMA integrity_check` as
//
//	fts5: corruption found reading blob <n> from table "messages_fts"
//
// — is not data loss: every index entry is re-derivable from messages, and
// FTS5's own 'rebuild' command re-creates the index from the content table
// without touching messages. That is the repair this file implements.
//
// What it deliberately refuses: any integrity problem that names anything
// but the index (a damaged messages row, a broken foreign key, a bad page
// elsewhere) is not self-repairing — guessing there would destroy data, so
// those databases stay down with a clear message and the backup/restore
// path (AGENTS.md rule 10) as the recovery.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
)

// ftsIndexTable is the virtual table 0002_content.sql creates; its shadow
// tables all carry the "_fts" suffix (messages_fts_data, messages_fts_docsize,
// messages_fts_rowid, …), which is what the damage classifier keys on.
const ftsIndexTable = "messages_fts"

// integrityCheckConn opens a throwaway *sql.DB whose one job is a
// whole-database integrity check, and nothing else.
//
// Why not the shared pool: the FTS5 module in modernc.org/sqlite caches the
// inverted-index structure per connection, and that cache is not invalidated
// by commits made on other connections — only by the next statement on the
// connection itself. A pooled connection that read messages_fts and then sat
// idle while the index was rewritten elsewhere reports a false
// `fts5: corruption found reading blob <n> from table "messages_fts"` from
// PRAGMA integrity_check, while every fresh connection — and the file on
// disk — reports clean. That is the field failure: the readiness probe ran
// the check on pooled connections of an idle device, and each probe that hit
// a stale connection saw the same phantom damage (ADR-0062, 2026-10-19
// amendment; reproduced by TestIntegrityCheckIgnoresStalePooledFTSView).
//
// A dedicated *long-lived* connection is not safe either: running the check
// populates the very cache that goes stale, so a connection that checked once
// and then idled across a rewrite poisons itself identically. Only a
// brand-new physical connection is guaranteed clean: it has never run an
// FTS5 statement in this process, so its first structure read decodes the
// structure the last writer persisted — the committed, on-disk truth.
// (This is also exactly what the field restart proved: a new process, new
// connections, ready 200.)
//
// The cost is one SQLite open per check — milliseconds against the
// O(database) walk the check itself takes, and the probe runs on an interval,
// not a hot path.
func (s *Store) integrityCheckConn(ctx context.Context) (*sql.DB, error) {
	// The check must not create a file the store never opened: if the
	// database file disappeared out from under a running store, the check
	// reports that instead of silently opening a fresh empty database and
	// calling it "ok".
	if _, err := os.Stat(s.path); err != nil {
		return nil, fmt.Errorf("store: integrity_check on %s: %w", s.path, err)
	}
	db, err := sql.Open(driverName, dsn(s.path))
	if err != nil {
		return nil, fmt.Errorf("store: open integrity-check connection to %s: %w", s.path, err)
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// IntegrityProblems runs PRAGMA integrity_check and returns the non-"ok"
// report lines; an empty slice means the database is clean. It is the split
// of IntegrityCheck that needs the raw lines: a caller that wants pass/fail
// uses IntegrityCheck, a caller that wants to classify the damage (this
// file) needs to see what the check actually said.
//
// The check runs on a dedicated fresh connection (integrityCheckConn), never
// on the shared pool: a pooled connection's FTS5 structure view can be stale
// and report corruption the file does not have. Consequence for the guard
// below: every check RecoverDerivedFTS relies on — the pre-rebuild damage
// confirmation, the re-check under the repair lock, and the post-rebuild
// verification — confirms on a fresh connection, so a stale pooled view can
// never trigger a rebuild of a healthy index.
func (s *Store) IntegrityProblems(ctx context.Context) ([]string, error) {
	db, err := s.integrityCheckConn(ctx)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return nil, fmt.Errorf("store: integrity_check on %s: %w", s.path, err)
	}
	defer rows.Close()

	var problems []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return nil, fmt.Errorf("store: integrity_check on %s: %w", s.path, err)
		}
		if !strings.EqualFold(strings.TrimSpace(line), "ok") {
			problems = append(problems, line)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: integrity_check on %s: %w", s.path, err)
	}
	return problems, nil
}

// isFTSDamageLine reports whether one integrity_check line can be attributed
// to the messages_fts index: the FTS5 module names the table in its own
// lines ("fts5: … from table \"messages_fts\""), and the btree-level checks
// name the shadow tables (messages_fts_data, …). Anything else — including a
// line that merely mentions the content table messages — is not attributed,
// so the classifier only errs on the safe side (refusing a repair it cannot
// prove is index-only).
func isFTSDamageLine(line string) bool {
	lower := strings.ToLower(line)
	return strings.Contains(lower, "_fts") || strings.Contains(lower, "fts5")
}

// IsFTSOnlyDamage reports whether every reported problem is damage to the
// derived messages_fts index, i.e. damage that 'rebuild' can clear without
// touching any user row. No problems at all is not FTS-only damage: there is
// nothing to repair, and the caller should answer "clean" instead.
func IsFTSOnlyDamage(lines []string) bool {
	if len(lines) == 0 {
		return false
	}
	for _, line := range lines {
		if !isFTSDamageLine(line) {
			return false
		}
	}
	return true
}

// RebuildMessagesFTS re-derives messages_fts from messages with the FTS5
// 'rebuild' command (the pattern the FTS5 manual documents for exactly this
// failure class). It runs in a write transaction, so it serialises with the
// rest of the service the same way every other write does, and it never
// issues a statement against messages: after it, the index matches the
// content by construction, and the content is exactly what it was before.
func (s *Store) RebuildMessagesFTS(ctx context.Context) error {
	err := s.InTx(ctx, func(tx *Tx) error {
		_, err := tx.exec(ctx, "rebuild "+ftsIndexTable,
			"INSERT INTO "+ftsIndexTable+"("+ftsIndexTable+") VALUES('rebuild')")
		return err
	})
	if err != nil {
		return fmt.Errorf("store: rebuild %s from messages on %s: %w", ftsIndexTable, s.path, err)
	}
	return nil
}

// ftsRepairMu serialises the recover attempts: the readiness probe polls on
// an interval, and a damaged index would otherwise make several polls queue
// full rebuilds of the same derived table.
var ftsRepairMu sync.Mutex

// RecoverDerivedFTS is the one-shot self-repair for the self-repairing
// damage class: structural damage confined to messages_fts, with messages
// itself intact (the field failure). It reports three distinct outcomes so
// the caller can say the right thing:
//
//   - (true, nil)   the damage was index-only, the index was rebuilt from
//     messages, and the database now reports clean. No user row changed.
//   - (false, nil)  the database reports no problems; nothing to repair.
//   - (false, err)  the damage is not index-only (so it is not safe to
//     rebuild over), or the rebuild ran but the damage survived it. The
//     error names both facts; the operator path is backup/restore, never a
//     second guess.
func (s *Store) RecoverDerivedFTS(ctx context.Context) (bool, error) {
	problems, err := s.IntegrityProblems(ctx)
	if err != nil {
		// A check that cannot even complete is, by definition, damage that
		// cannot be verified as confined to the derived index: refuse rather
		// than rebuild over damage we cannot see.
		return false, fmt.Errorf("store: %s: %w; the damage cannot be verified as confined to the derived %s index, so no self-repair was attempted; restore from the last good backup",
			s.path, err, ftsIndexTable)
	}
	if len(problems) == 0 {
		return false, nil
	}
	if !IsFTSOnlyDamage(problems) {
		return false, fmt.Errorf(
			"store: %s: %d integrity problem(s) reach beyond the derived %s index: %s; "+
				"this is not self-repairing, restore from the last good backup (docs/spec/02 \"Migration and backup acceptance\")",
			s.path, len(problems), ftsIndexTable, joinLines(problems, maxReportedProblems))
	}

	ftsRepairMu.Lock()
	defer ftsRepairMu.Unlock()
	// Re-check under the lock: a concurrent recovery (or a previous poll's
	// rebuild) may have cleared the damage while we queued.
	problems, err = s.IntegrityProblems(ctx)
	if err != nil {
		return false, fmt.Errorf("store: %s: %w while re-checking under the repair lock; no self-repair attempted; restore from backup",
			s.path, err)
	}
	if len(problems) == 0 {
		return false, nil
	}
	if !IsFTSOnlyDamage(problems) {
		return false, fmt.Errorf(
			"store: %s: integrity damage changed while repairing: %s; not self-repairing, restore from backup",
			s.path, joinLines(problems, maxReportedProblems))
	}

	if err := s.RebuildMessagesFTS(ctx); err != nil {
		return false, err
	}
	// The rebuild is only accepted if the database now reports clean; a
	// rebuild that leaves problems behind (persistent media damage, a
	// different writer) must not be reported as a recovery.
	if problems, err := s.IntegrityProblems(ctx); err != nil {
		return false, err
	} else if len(problems) > 0 {
		return false, fmt.Errorf(
			"store: %s: %s was rebuilt from messages but %d problem(s) remain: %s; "+
				"the damage is not the derived index, restore from backup",
			s.path, ftsIndexTable, len(problems), joinLines(problems, maxReportedProblems))
	}
	return true, nil
}

// joinLines renders a bounded number of report lines for one error message
// (the same cap IntegrityCheck applies, so an operator sees the same shape
// in both places).
func joinLines(lines []string, max int) string {
	shown := lines
	suffix := ""
	if len(shown) > max {
		suffix = fmt.Sprintf(" (and %d more)", len(shown)-max)
		shown = shown[:max]
	}
	return strings.Join(shown, "; ") + suffix
}
