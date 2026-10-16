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

// freshRowVersion is `profiles.row_version` for a revision that has just been
// inserted: 0004_publication.sql's DEFAULT 1, because creating the row is itself
// the first write. It is named rather than spelled so that a test's "the version
// is 1" is visibly a *row* version and can never be confused with — or greped for
// as — a hardcoded schema version (ADR-0015's acceptance gate).
const freshRowVersion int64 = 1

// blankSchemaVersion is what SchemaVersion reports for a file with no migrations
// applied. Zero is a property of an empty database, not of this build's migration
// set, so it is a constant and not a magic digit inside an assertion.
const blankSchemaVersion = 0

// priorSchemaVersion is the version an immediately-older build would have left a
// database at — the upgrade source for the migration tests below, derived from
// this build's set instead of hardcoded.
func priorSchemaVersion(t *testing.T) int {
	t.Helper()
	migs, err := Migrations()
	if err != nil {
		t.Fatalf("Migrations: %v", err)
	}
	if len(migs) < 2 {
		t.Skip("this build ships fewer than two migrations, so there is no prior version to upgrade from")
	}
	return migs[len(migs)-2].Version
}

// migrateTo applies exactly the first n embedded migrations, each in its own
// transaction and each recorded, and stops there. It is how a test builds the
// database an *older* build left behind, which is the only honest way to exercise
// an upgrade path (docs/spec/02 "Migration and backup acceptance" demands both a
// blank database and a prior one). It mirrors Migrate's own bookkeeping instead of
// shoving raw SQL at the file, so what it produces is byte-comparable with what
// shipped code produced.
func migrateTo(t *testing.T, s *Store, n int) []string {
	t.Helper()
	ctx := testCtx(t)
	migs, err := Migrations()
	if err != nil {
		t.Fatalf("Migrations: %v", err)
	}
	if n < 0 || n > len(migs) {
		t.Fatalf("migrateTo(%d): this build ships %d migrations", n, len(migs))
	}
	if _, err := s.db.ExecContext(ctx, schemaMigrationsDDL); err != nil {
		t.Fatalf("create %s: %v", schemaMigrationsTable, err)
	}
	applied := make([]string, 0, n)
	for _, mig := range migs[:n] {
		if err := applyMigration(ctx, s.db, mig); err != nil {
			t.Fatalf("apply %s: %v", mig.Name, err)
		}
		applied = append(applied, mig.Name)
	}
	if v, err := s.SchemaVersion(ctx); err != nil || v != n {
		t.Fatalf("SchemaVersion after migrateTo(%d) = %d, %v; want %d", n, v, err, n)
	}
	return applied
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
