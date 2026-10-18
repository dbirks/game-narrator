package store

// Regression tests for the derived-index self-repair (fts_repair.go).
//
// The corruptions here are deterministic and proven to produce the field
// signatures (probed against this exact driver build before the tests were
// written): zeroing one byte of a messages_fts_data index page yields
// `fts5: corruption found reading blob <n> from table "messages_fts"` from
// PRAGMA integrity_check, while the content table stays intact — the one
// damage class the service may repair by itself.

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/games"
)

// corruptFTSIndexPage makes the database file carry a structurally broken FTS
// index page: a byte of a messages_fts_data block is zeroed (or the block
// truncated), which breaks the varint framing of the inverted-index page
// exactly the way a torn write does. Which page a given attack lands on is
// a matter of the FTS5 page geometry (segment ids are folded into the page
// numbers, and full pages carry slack a zero byte can sit in without
// breaking a parse), so the helper walks the pages applying the attack and
// keeps the first one PRAGMA integrity_check actually reports as damaged —
// the same walk the repair is meant to clear.
//
// The store must be closed first so the damage lands in the main file after
// the WAL is checkpointed; the returned store is the one the test repairs
// (the service would see the same damage at startup).
func corruptFTSIndexPage(t *testing.T, s *Store) *Store {
	t.Helper()
	path := s.Path()
	if err := s.Close(); err != nil {
		t.Fatalf("close before corrupting: %v", err)
	}

	db, err := sql.Open(driverName, dsn(path))
	if err != nil {
		t.Fatalf("open for corruption: %v", err)
	}
	type pageRef struct {
		id    int
		block []byte
	}
	var pages []pageRef
	{
		rows, err := db.Query(`SELECT id, block FROM messages_fts_data ORDER BY id`)
		if err != nil {
			t.Fatalf("read index pages: %v", err)
		}
		for rows.Next() {
			var p pageRef
			if err := rows.Scan(&p.id, &p.block); err != nil {
				t.Fatalf("scan index page: %v", err)
			}
			pages = append(pages, p)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("read index pages: %v", err)
		}
		rows.Close()
	}
	if len(pages) < 3 {
		t.Fatalf("the index has %d data pages: the test needs a settled, merged index", len(pages))
	}

	tryDamage := func() ([]string, error) {
		rows, err := db.Query(`PRAGMA integrity_check`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var problems []string
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				return nil, err
			}
			if line != "ok" {
				problems = append(problems, line)
			}
		}
		return problems, rows.Err()
	}

	damaged := false
	for _, p := range pages {
		// Attack 1: zero a byte at the middle of the page body. On a page
		// whose middle is free slack this is invisible to the check, which is
		// why the helper walks pages rather than betting on one.
		if len(p.block) >= 16 {
			b := make([]byte, len(p.block))
			copy(b, p.block)
			b[len(b)/2] = 0
			if _, err := db.Exec(`UPDATE messages_fts_data SET block = ? WHERE id = ?`, b, p.id); err != nil {
				t.Fatalf("corrupt page %d: %v", p.id, err)
			}
			if problems, err := tryDamage(); err == nil && len(problems) > 0 {
				t.Logf("corrupted index page id=%d (zeroed middle byte): %v", p.id, problems)
				damaged = true
				break
			} else if err != nil {
				t.Fatalf("integrity probe: %v", err)
			}
			// Restore for the next attack.
			if _, err := db.Exec(`UPDATE messages_fts_data SET block = ? WHERE id = ?`, p.block, p.id); err != nil {
				t.Fatalf("restore page %d: %v", p.id, err)
			}
		}
		// Attack 2: truncate the page past its last cell: the header then
		// points at cells that are no longer there.
		if len(p.block) > 32 {
			b := p.block[:len(p.block)*60/100]
			if _, err := db.Exec(`UPDATE messages_fts_data SET block = ? WHERE id = ?`, b, p.id); err != nil {
				t.Fatalf("truncate page %d: %v", p.id, err)
			}
			if problems, err := tryDamage(); err == nil && len(problems) > 0 {
				t.Logf("corrupted index page id=%d (truncated): %v", p.id, problems)
				damaged = true
				break
			}
			if _, err := db.Exec(`UPDATE messages_fts_data SET block = ? WHERE id = ?`, p.block, p.id); err != nil {
				t.Fatalf("restore page %d: %v", p.id, err)
			}
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close corruption handle: %v", err)
	}
	if !damaged {
		t.Fatalf("no corruption attack was visible to PRAGMA integrity_check: the test cannot exercise the repair")
	}

	// Reopen as the service would: the damage must now be visible to the
	// startup probe.
	s2 := openPath(t, path)
	problems, err := s2.IntegrityProblems(testCtx(t))
	if err != nil {
		t.Fatalf("integrity after corruption: %v", err)
	}
	if len(problems) == 0 {
		t.Fatalf("corruption not visible after reopen: the test cannot exercise the repair")
	}
	if !IsFTSOnlyDamage(problems) {
		t.Fatalf("corruption lines are not FTS-only: %v", problems)
	}
	return s2
}

// corruptMessagesTablePage is the non-self-repairing half of the same file:
// one cell pointer of the messages table's root page is rewritten to point
// outside the page. PRAGMA integrity_check then reports a btree-level problem
// in the content itself, and the repair must refuse to run over it — no
// amount of index rebuilding can fix a damaged content page, and a repair
// that "succeeds" over damaged content would be lying to the operator.
func corruptMessagesTablePage(t *testing.T, s *Store) *Store {
	t.Helper()
	path := s.Path()

	var (
		pageSize, rootPage int
	)
	if err := s.db.QueryRow(`PRAGMA page_size`).Scan(&pageSize); err != nil {
		t.Fatalf("page_size: %v", err)
	}
	if err := s.db.QueryRow(`SELECT rootpage FROM sqlite_master WHERE type='table' AND name='messages'`).Scan(&rootPage); err != nil {
		t.Fatalf("messages rootpage: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close before corrupting: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	// Page N starts at (N-1)*pageSize: the 100-byte database header is the
	// first 100 bytes of page 1, not a preamble before it.
	off := (rootPage - 1) * pageSize
	if off+8 >= len(data) {
		t.Fatalf("page %d at offset %d is outside the %d-byte file", rootPage, off, len(data))
	}
	pageType := data[off]
	if pageType != 0x05 && pageType != 0x0D {
		t.Fatalf("messages root page %d has type 0x%02x, want a table btree page (0x05 interior / 0x0D leaf)", rootPage, pageType)
	}
	cellCount := binary.BigEndian.Uint16(data[off+3 : off+5])
	if cellCount == 0 {
		t.Fatalf("messages root page %d has no cells: nothing to point at", rootPage)
	}
	// Rewrite the first cell pointer (2 bytes at off+8) far outside the page:
	// any walk of this page then fails with an out-of-range offset.
	binary.BigEndian.PutUint16(data[off+8:off+10], 0xFFFF)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write corrupt page: %v", err)
	}

	s2 := openPath(t, path)
	problems, err := s2.IntegrityProblems(testCtx(t))
	if err == nil && len(problems) == 0 {
		t.Fatalf("content-page corruption not visible to PRAGMA integrity_check")
	}
	// Severe content damage can make the check itself fail with an error
	// instead of reporting lines; either outcome is "damaged, not FTS-only".
	if err == nil && IsFTSOnlyDamage(problems) {
		t.Fatalf("content damage misclassified as FTS-only: %v", problems)
	}
	return s2
}

// messagesDigest is the exact content the repair must preserve: every row's
// identity and both indexed texts, in rowid order.
func messagesDigest(t *testing.T, s *Store) string {
	t.Helper()
	var digest bytes.Buffer
	rows, err := s.db.Query(`SELECT rowid, id, normalized_text, speech_text FROM messages ORDER BY rowid`)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			rowid  int
			id     string
			norm   string
			speech string
		)
		if err := rows.Scan(&rowid, &id, &norm, &speech); err != nil {
			t.Fatalf("digest scan: %v", err)
		}
		fmt.Fprintf(&digest, "%d|%s|%s|%s\n", rowid, id, norm, speech)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("digest: %v", err)
	}
	return digest.String()
}

func TestRecoverDerivedFTSRepairsIndexOnlyDamage(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx

	seed := make([]games.Message, 0, 120)
	for i := 0; i < 120; i++ {
		seed = append(seed, ftsLine(f, i))
	}
	if err := f.s.PutMessages(ctx, f.profile.ID, seed, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Settle the index into data pages, the way a checkpoint-driven merge
	// does, so there is a page body to damage.
	mustExec(t, f.s.db, `INSERT INTO messages_fts(messages_fts) VALUES('merge')`)
	before := messagesDigest(t, f.s)

	s := corruptFTSIndexPage(t, f.s)

	// The probe that failed in the field must be the one reporting damage.
	if err := s.IntegrityCheck(ctx); err == nil {
		t.Fatal("corrupted index passed PRAGMA integrity_check: the guard would never fire")
	} else if !strings.Contains(strings.ToLower(err.Error()), "messages_fts") {
		t.Fatalf("integrity error does not name the FTS index: %v", err)
	}

	repaired, err := s.RecoverDerivedFTS(ctx)
	if !repaired || err != nil {
		t.Fatalf("RecoverDerivedFTS: repaired=%v err=%v, want (true, nil)", repaired, err)
	}
	if err := s.IntegrityCheck(ctx); err != nil {
		t.Fatalf("integrity after repair: %v", err)
	}
	if got := messagesDigest(t, s); got != before {
		t.Fatal("the repair changed the messages content: user rows must never be touched")
	}
	if hits, err := s.SearchMessages(ctx, f.profile.ID, "keeper log", 5); err != nil || len(hits) == 0 {
		t.Fatalf("search after repair: hits=%d err=%v, want the seeded lines", len(hits), err)
	}

	// A second recovery on the now-clean database reports nothing to do.
	repaired, err = s.RecoverDerivedFTS(ctx)
	if repaired || err != nil {
		t.Fatalf("second RecoverDerivedFTS on a clean db: repaired=%v err=%v, want (false, nil)", repaired, err)
	}
}

func TestRecoverDerivedFTSRefusesContentDamage(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx

	seed := make([]games.Message, 0, 120)
	for i := 0; i < 120; i++ {
		seed = append(seed, ftsLine(f, i))
	}
	if err := f.s.PutMessages(ctx, f.profile.ID, seed, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s := corruptMessagesTablePage(t, f.s)

	if err := s.IntegrityCheck(ctx); err == nil {
		t.Fatal("corrupted content page passed PRAGMA integrity_check: the test cannot exercise the refusal")
	}
	repaired, err := s.RecoverDerivedFTS(ctx)
	if repaired {
		t.Fatal("RecoverDerivedFTS claimed to repair damaged content: it must refuse")
	}
	if err == nil {
		t.Fatal("refusal without an error: the operator gets no explanation of why there is no self-repair")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "restore") {
		t.Fatalf("refusal error does not point at the operator path (backup/restore): %v", err)
	}
	// The database must still be reported damaged, not silently passed.
	if err := s.IntegrityCheck(ctx); err == nil {
		t.Fatal("the damaged content page is no longer reported after the refused repair")
	}
}

func TestIsFTSOnlyDamageClassifier(t *testing.T) {
	cases := []struct {
		lines []string
		want  bool
	}{
		{[]string{`fts5: corruption found reading blob 3 from table "messages_fts"`}, true},
		{[]string{`fts5: corrupt structure record for table "messages_fts"`}, true},
		{[]string{`malformed inverted index for FTS5 table main.messages_fts`}, true},
		{[]string{`row 12 missing from index messages_fts_rowid`}, true},
		// The content table named: "messages" is not "messages_fts".
		{[]string{`row 12 missing from index messages_profile_scene_kind_idx`}, false},
		// A btree-level line that names no table at all: the classifier must
		// err on the safe side (refuse), not guess.
		{[]string{`Tree 1 page 5: offset 33 out of range 0..512`}, false},
		{[]string{`ok`}, false},
		{nil, false},
		// Mixed damage is not self-repairing.
		{[]string{`fts5: corruption found reading blob 1 from table "messages_fts"`,
			`foreign key mismatch - "events" referencing "sessions"`}, false},
	}
	for i, c := range cases {
		if got := IsFTSOnlyDamage(c.lines); got != c.want {
			t.Errorf("case %d: IsFTSOnlyDamage(%v) = %v, want %v", i, c.lines, got, c.want)
		}
	}
}
