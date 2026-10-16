package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// Helpers shared by the store tests. Every file lives under t.TempDir(); no
// test ever touches the real data directory.

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// openIn opens (and later closes) a fresh database inside a temp dir.
func openIn(t *testing.T) *Store {
	t.Helper()
	return openPath(t, filepath.Join(t.TempDir(), FileName))
}

func openPath(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(testCtx(t), path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// migratedStore returns an open store with the embedded migrations applied.
func migratedStore(t *testing.T) *Store {
	t.Helper()
	s := openIn(t)
	if _, err := s.Migrate(testCtx(t)); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return s
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}

func insertGame(t *testing.T, db *sql.DB, id, slug string) {
	t.Helper()
	mustExec(t, db,
		`INSERT INTO games (id, slug, title, platform, user_tags, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		id, slug, "Test Game", "gamecube", `["synthetic"]`, nowRFC3339())
}

// insertEdition adds a US/EN edition under gameID.
func insertEdition(t *testing.T, db *sql.DB, id, gameID string) {
	t.Helper()
	insertEditionIn(t, db, id, gameID, "US")
}

// insertEditionIn adds an edition differing in region, so several editions can
// sit under one game: editions_identity_idx forbids repeating
// (game_id, region, language, revision).
func insertEditionIn(t *testing.T, db *sql.DB, id, gameID, region string) {
	t.Helper()
	mustExec(t, db,
		`INSERT INTO editions (id, game_id, region, language, revision, locale, display_aspect)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, gameID, region, "en", "rev0", "en_US", 1.3333)
}

// insertProfile adds revision_num revision under editionID. parent may be nil.
func insertProfile(t *testing.T, db *sql.DB, id, editionID string, revisionNum int, status string, parent *int) {
	t.Helper()
	mustExec(t, db,
		`INSERT INTO profiles (id, edition_id, revision_num, parent_revision, status, publication_author)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		id, editionID, revisionNum, parent, status, "tester")
}

func insertSource(t *testing.T, db *sql.DB, id, profileID, sourceType, sha string) {
	t.Helper()
	mustExec(t, db,
		`INSERT INTO sources (id, profile_id, source_type, uri, retrieved_at, content_sha256, mime_type, rights_status, trust, parser)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, profileID, sourceType, "file:///user/supplied/script.csv", nowRFC3339(),
		sha, "text/csv", "user_supplied", 1, "generic-csv")
}

func insertBlob(t *testing.T, db *sql.DB, sha string, size int) {
	t.Helper()
	mustExec(t, db,
		`INSERT INTO source_blobs (sha256, size, media_type, owner_namespace, created_at, storage_locator)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		sha, size, "text/csv", "profile:test", nowRFC3339(), "blobs/"+sha[:2]+"/"+sha)
}

const (
	shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func countRows(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}
