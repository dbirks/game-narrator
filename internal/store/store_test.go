package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestOpenReadsBackRequiredPragmas covers docs/spec/02 "SQLite schema
// principles": foreign keys ON, WAL, busy_timeout, synchronous. Each value is
// read from the server, not assumed from the DSN.
func TestOpenReadsBackRequiredPragmas(t *testing.T) {
	s := openIn(t)
	ctx := testCtx(t)

	cases := map[string]string{
		"foreign_keys": fmt.Sprint(requiredForeignKeys),
		"journal_mode": requiredJournalMode,
		"busy_timeout": fmt.Sprint(requiredBusyTimeout),
		"synchronous":  fmt.Sprint(requiredSynchronous),
	}
	for pragma, want := range cases {
		conn, err := s.db.Conn(ctx)
		if err != nil {
			t.Fatalf("conn: %v", err)
		}
		var got string
		if err := conn.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&got); err != nil {
			t.Fatalf("pragma %s: %v", pragma, err)
		}
		conn.Close()
		if got != want {
			t.Errorf("PRAGMA %s = %q, want %q", pragma, got, want)
		}
	}

	if v, err := s.SQLiteVersion(ctx); err != nil || v == "" {
		t.Fatalf("SQLiteVersion = %q, %v", v, err)
	}
}

// TestOpenCreatesPrivateDir: the data directory must not be world-readable.
func TestOpenCreatesPrivateDir(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "data", "nested", FileName)

	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected a missing parent dir, got %v", err)
	}

	s := openPath(t, path)
	if _, err := s.Migrate(testCtx(t)); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	for _, dir := range []string{filepath.Dir(path), filepath.Dir(filepath.Dir(path))} {
		st, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if perm := st.Mode().Perm(); perm != 0o700 {
			t.Errorf("%s mode = %o, want 700", dir, perm)
		}
	}
}

// TestOpenRejectsNonDatabaseFile is the first half of "corruption is visible at
// Open, not at the first query".
func TestOpenRejectsNonDatabaseFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-db.sqlite")
	if err := os.WriteFile(path, []byte(strings.Repeat("Game Narrator ", 512)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(testCtx(t), path); err == nil {
		t.Fatal("Open on a non-database file = nil error, want failure")
	}
}

func TestOpenRequiresPath(t *testing.T) {
	if _, err := Open(testCtx(t), ""); err == nil {
		t.Fatal("Open(\"\") = nil error, want failure")
	}
}

// TestCatalogTablesAreExactlyTheFirstFive pins 0001_init.sql to docs/spec/02 "Entity
// catalog": the first migration creates exactly the five catalogue tables that
// document names for it, no more and no less, and they are all present after a
// migrate.
//
// It is a statement about *that file*, deliberately not about the whole database.
// The earlier version of this test compared the whole table set of a migrated
// database against a hardcoded list, which meant any additive migration — 0002's
// content tables, 0004's publish_actions — had to be accompanied by an edit to an
// unrelated schema test. That coupling is how a worker adding a real column gets
// talked into storing it somewhere the schema does not name it (ADR-0015), so the
// whole-database drift check lives in TestMigratedDatabaseHoldsExactlyTheMigrationTables
// instead, where it is derived from the migrations rather than typed out by hand.
func TestCatalogTablesAreExactlyTheFirstFive(t *testing.T) {
	const firstMigration = "0001_init.sql"
	// docs/spec/02's catalogue in this file, sorted; `schema_migrations` is
	// bookkeeping that migrate.go creates outside any migration file.
	want := []string{"editions", "games", "profiles", "source_blobs", "sources"}

	got := append([]string(nil), migrationTables(t).byFile["0001_init.sql"]...)
	slices.Sort(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%s creates %v, want the five catalog tables %v", firstMigration, got, want)
	}

	// And they really exist in a migrated database — a file that names a table in a
	// comment but never creates it would otherwise pass a pure text comparison.
	s := migratedStore(t)
	ctx := testCtx(t)
	for _, name := range want {
		var n int
		if err := s.db.QueryRowContext(ctx,
			"SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?", name).Scan(&n); err != nil {
			t.Fatalf("look for %s: %v", name, err)
		}
		if n != 1 {
			t.Errorf("%s is not exactly one table of the migrated database (found %d)", name, n)
		}
	}
}

// TestMigratedDatabaseHoldsExactlyTheMigrationTables is the drift check the
// hardcoded list was reaching for, expressed as a rule instead of a snapshot: every
// table in the database was created by some embedded migration (or by migrate.go's
// own bookkeeping), every table some migration creates is present, and the only
// extras are the shadow tables SQLite maintains beside an FTS5 virtual table.
func TestMigratedDatabaseHoldsExactlyTheMigrationTables(t *testing.T) {
	s := migratedStore(t)
	ctx := testCtx(t)

	want := map[string]bool{schemaMigrationsTable: true}
	virtual := []string{}
	created := migrationTables(t)
	for file, tables := range created.byFile {
		for _, name := range tables {
			want[name] = true
			if created.virtual[file][name] {
				virtual = append(virtual, name)
			}
		}
	}

	rows, err := s.db.QueryContext(ctx,
		"SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var found []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		found = append(found, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	for _, name := range found {
		if want[name] {
			continue
		}
		// An FTS5 virtual table is named by its CREATE statement; its shadow tables
		// are not, and they are SQLite's, not this schema's.
		shadow := false
		for _, base := range virtual {
			if strings.HasPrefix(name, base+"_") {
				shadow = true
				break
			}
		}
		if !shadow {
			t.Errorf("table %q exists in the migrated database but no embedded migration creates it", name)
		}
	}
	for name := range want {
		if !slices.Contains(found, name) {
			t.Errorf("a migration creates %q but the migrated database has no such table", name)
		}
	}
}

// createTablePattern matches the table a CREATE TABLE statement names, with or
// without the VIRTUAL/IF NOT EXISTS noise in between.
var createTablePattern = regexp.MustCompile("(?i)^CREATE\\s+(?:VIRTUAL\\s+)?TABLE\\s+(?:IF\\s+NOT\\s+EXISTS\\s+)?([A-Za-z0-9_]+)")

// alterTablePattern matches the table an ALTER TABLE ... RENAME TO statement
// renames: the rebuild migration 0019 widens a CHECK by renaming a freshly
// created table over the old shape, and the drift check must read the surviving
// name, not the transient one.
var alterTablePattern = regexp.MustCompile("(?i)^ALTER\\s+TABLE\\s+([A-Za-z0-9_]+)\\s+RENAME\\s+TO\\s+([A-Za-z0-9_]+)")

// createdTables answers "which tables does each embedded migration create?", plus the
// subset that came from a CREATE VIRTUAL TABLE statement — whose shadow tables SQLite
// adds by itself and no migration names.
type createdTables struct {
	byFile  map[string][]string
	virtual map[string]map[string]bool
}

// migrationTables reads those names out of the *statements* splitSQL produces rather
// than out of the raw text: a migration whose comments discuss a table it does not
// create is the normal case in this schema, and a text search would count the
// discussion.
func migrationTables(t *testing.T) createdTables {
	t.Helper()
	migs, err := Migrations()
	if err != nil {
		t.Fatalf("Migrations: %v", err)
	}
	out := createdTables{
		byFile:  map[string][]string{},
		virtual: map[string]map[string]bool{},
	}
	for _, mig := range migs {
		statements, err := splitSQL(mig.SQL)
		if err != nil {
			t.Fatalf("splitSQL(%s): %v", mig.Name, err)
		}
		out.virtual[mig.Name] = map[string]bool{}
		renames := map[string]string{}
		created := []string{}
		for _, stmt := range statements {
			code := stripLeadComments(stmt)
			m := createTablePattern.FindStringSubmatch(code)
			if m == nil {
				if rm := alterTablePattern.FindStringSubmatch(code); rm != nil {
					renames[rm[1]] = rm[2]
				}
				continue
			}
			created = append(created, m[1])
			if strings.HasPrefix(strings.ToUpper(code), "CREATE VIRTUAL TABLE") {
				out.virtual[mig.Name][m[1]] = true
			}
		}
		// A rebuild (create, copy, drop, rename) leaves the surviving name on
		// the database, so the migration's contribution is the renamed shape.
		for _, name := range created {
			if next, ok := renames[name]; ok {
				name = next
			}
			out.byFile[mig.Name] = append(out.byFile[mig.Name], name)
		}
	}
	return out
}

// TestForeignKeyEnforcement proves `PRAGMA foreign_keys = ON` actually bites:
// an edition cannot reference a game that does not exist.
func TestForeignKeyEnforcement(t *testing.T) {
	s := migratedStore(t)
	db := s.db

	if _, err := db.Exec(`INSERT INTO editions (id, game_id, region, language, revision)
		VALUES ('ed-1', 'game-does-not-exist', 'US', 'en', 'rev0')`); err == nil {
		t.Fatal("edition with an unknown game_id inserted; foreign keys are not enforced")
	} else if !strings.Contains(strings.ToUpper(err.Error()), "FOREIGN KEY") {
		t.Fatalf("error = %v, want a FOREIGN KEY violation", err)
	}
}

// TestCheckConstraints covers the enum CHECKs and the timestamp/JSON CHECKs.
func TestCheckConstraints(t *testing.T) {
	s := migratedStore(t)
	db := s.db
	ctx := testCtx(t)

	insertGame(t, db, "game-1", "test-game")
	insertEdition(t, db, "ed-1", "game-1")
	// A valid parent row, so that a rejected sources insert is rejected by the
	// CHECK under test and not by an unrelated foreign key failure.
	insertProfile(t, db, "p0", "ed-1", 1, "draft", nil)

	cases := []struct {
		name  string
		query string
		args  []any
	}{
		{
			name:  "profiles.status must be one of draft|review|published|archived",
			query: `INSERT INTO profiles (id, edition_id, revision_num, status) VALUES ('p-bad', 'ed-1', 2, 'bogus')`,
		},
		{
			name:  "sources.source_type is an enum",
			query: `INSERT INTO sources (id, profile_id, source_type, uri, retrieved_at, content_sha256, rights_status) VALUES ('s-bad', 'p0', 'pirated_dump', 'x', ?, '', 'unknown')`,
			args:  []any{nowRFC3339()},
		},
		{
			name:  "sources.rights_status is an enum",
			query: `INSERT INTO sources (id, profile_id, source_type, uri, retrieved_at, content_sha256, rights_status) VALUES ('s-bad', 'p0', 'manual', 'x', ?, '', 'stolen')`,
			args:  []any{nowRFC3339()},
		},
		{
			name:  "sources.content_sha256 is empty or 64 characters",
			query: `INSERT INTO sources (id, profile_id, source_type, uri, retrieved_at, content_sha256, rights_status) VALUES ('s-bad', 'p0', 'manual', 'x', ?, 'abc', 'unknown')`,
			args:  []any{nowRFC3339()},
		},
		{
			name:  "sources.trust stays on [0,1]",
			query: `INSERT INTO sources (id, profile_id, source_type, uri, retrieved_at, content_sha256, rights_status, trust) VALUES ('s-bad', 'p0', 'manual', 'x', ?, '', 'unknown', 42)`,
			args:  []any{nowRFC3339()},
		},
		{
			name:  "games.slug is unique",
			query: `INSERT INTO games (id, slug, title, created_at) VALUES ('game-2', 'test-game', 'Duplicate', ?)`,
			args:  []any{nowRFC3339()},
		},
		{
			name:  "profiles.revision_num starts at 1",
			query: `INSERT INTO profiles (id, edition_id, revision_num, status) VALUES ('p-bad', 'ed-1', 0, 'draft')`,
		},
		{
			name:  "games.user_tags must be JSON",
			query: `INSERT INTO games (id, slug, title, user_tags, created_at) VALUES ('game-3', 'g3', 'Tags', 'not json', ?)`,
			args:  []any{nowRFC3339()},
		},
		{
			name:  "timestamps must parse as dates",
			query: `INSERT INTO games (id, slug, title, created_at) VALUES ('game-4', 'g4', 'When', 'yesterday-ish')`,
		},
		{
			name:  "editions.display_aspect must be positive",
			query: `INSERT INTO editions (id, game_id, region, language, revision, display_aspect) VALUES ('ed-bad', 'game-1', 'EU', 'de', 'rev0', 0)`,
		},
		{
			name:  "source_blobs.sha256 is 64 characters",
			query: `INSERT INTO source_blobs (sha256, size, owner_namespace, created_at, storage_locator) VALUES ('nope', 1, 'profile:test', ?, 'blobs/nope')`,
			args:  []any{nowRFC3339()},
		},
		{
			name:  "source_blobs.size cannot be negative",
			query: `INSERT INTO source_blobs (sha256, size, owner_namespace, created_at, storage_locator) VALUES (?, -1, 'profile:test', ?, 'blobs/x')`,
			args:  []any{shaA, nowRFC3339()},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.ExecContext(ctx, tc.query, tc.args...); err == nil {
				t.Fatalf("statement accepted, want a CHECK/UNIQUE violation: %s", tc.query)
			} else if !strings.Contains(strings.ToUpper(err.Error()), "CONSTRAINT") {
				t.Fatalf("error = %v, want a constraint violation", err)
			}
		})
	}

	// The mirror image: valid values must be accepted, or the CHECK is just
	// broken rather than strict.
	insertProfile(t, db, "p-valid", "ed-1", 2, "draft", nil)
	for _, status := range []string{"draft", "review", "published", "archived"} {
		if _, err := db.Exec(`UPDATE profiles SET status = ? WHERE id = 'p-valid'`, status); err != nil {
			t.Errorf("status %q rejected: %v", status, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO sources (id, profile_id, source_type, uri, retrieved_at, content_sha256, rights_status)
		VALUES ('s-ok', 'p0', 'manual', 'note://none', ?, '', 'user_supplied')`, nowRFC3339()); err != nil {
		t.Errorf("manual source with no payload rejected: %v", err)
	}
	if n := countRows(t, db, "profiles"); n != 2 {
		t.Fatalf("profiles rows = %d, want 2 (p0 and p-valid)", n)
	}
}

// TestProfilesEditionRevisionUnique is the required UNIQUE index on
// profiles(edition_id, revision_num).
func TestProfilesEditionRevisionUnique(t *testing.T) {
	s := migratedStore(t)
	db := s.db

	insertGame(t, db, "game-1", "test-game")
	insertEdition(t, db, "ed-1", "game-1")
	insertProfile(t, db, "p-1", "ed-1", 1, "draft", nil)

	var unique int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master
		WHERE type = 'index' AND tbl_name = 'profiles' AND name = 'profiles_edition_revision_idx' AND
		sql LIKE '%UNIQUE%'`).Scan(&unique); err != nil {
		t.Fatal(err)
	}
	if unique != 1 {
		t.Fatal("profiles(edition_id, revision_num) is not backed by a UNIQUE index")
	}

	// Same edition + same revision number must collide; a different edition may
	// reuse revision 1.
	if _, err := db.Exec(`INSERT INTO profiles (id, edition_id, revision_num, status)
		VALUES ('p-1-dup', 'ed-1', 1, 'draft')`); err == nil {
		t.Fatal("duplicate (edition_id, revision_num) accepted")
	}
	insertEditionIn(t, db, "ed-2", "game-1", "EU")
	insertProfile(t, db, "p-2", "ed-2", 1, "draft", nil)
}

// TestEditionIdentityIsUnique documents editions_identity_idx: re-importing the
// same disc must reconcile with the existing row, not create a twin.
func TestEditionIdentityIsUnique(t *testing.T) {
	s := migratedStore(t)
	db := s.db

	insertGame(t, db, "game-1", "test-game")
	insertEdition(t, db, "ed-us", "game-1")

	if _, err := db.Exec(`INSERT INTO editions (id, game_id, region, language, revision)
		VALUES ('ed-us-twin', 'game-1', 'US', 'en', 'rev0')`); err == nil {
		t.Fatal("a second US/en/rev0 edition of the same game was accepted")
	}
	// A different revision of the same disc set is a distinct edition.
	mustExec(t, db, `INSERT INTO editions (id, game_id, region, language, revision)
		VALUES ('ed-us-rev1', 'game-1', 'US', 'en', 'rev1')`)
}

// TestRevisionChainStaysInsideItsEdition covers the self-referential composite
// key on profiles.parent_revision.
func TestRevisionChainStaysInsideItsEdition(t *testing.T) {
	s := migratedStore(t)
	db := s.db

	insertGame(t, db, "game-1", "test-game")
	insertEdition(t, db, "ed-1", "game-1")
	insertEditionIn(t, db, "ed-2", "game-1", "EU")
	insertProfile(t, db, "p-1", "ed-1", 1, "published", nil)
	insertProfile(t, db, "p-2", "ed-1", 2, "draft", intPtr(1))

	// Revision 3 drawn from a revision number that does not exist in ed-1.
	if _, err := db.Exec(`INSERT INTO profiles (id, edition_id, revision_num, parent_revision, status)
		VALUES ('p-3', 'ed-1', 3, 99, 'draft')`); err == nil {
		t.Fatal("parent_revision 99 accepted; the composite foreign key is not enforced")
	}
	// A parent number that exists, but in the other edition, must not link.
	if _, err := db.Exec(`INSERT INTO profiles (id, edition_id, revision_num, parent_revision, status)
		VALUES ('p-4', 'ed-2', 2, 1, 'draft')`); err == nil {
		t.Fatal("parent_revision from another edition accepted")
	}
	// Deleting a revision another one descends from is refused.
	if _, err := db.Exec(`DELETE FROM profiles WHERE id = 'p-1'`); err == nil {
		t.Fatal("deleting a parent revision succeeded; RESTRICT is not enforced")
	}
}

// TestDeleteBehaviourMatchesTheMigrationComments checks the ON DELETE choices
// argued in 0001_init.sql, so the comments cannot silently rot.
func TestDeleteBehaviourMatchesTheMigrationComments(t *testing.T) {
	s := migratedStore(t)
	db := s.db
	ctx := testCtx(t)

	insertGame(t, db, "game-1", "test-game")
	insertGame(t, db, "game-2", "other-game")
	insertEdition(t, db, "ed-1", "game-1")
	insertEdition(t, db, "ed-empty", "game-2")
	insertProfile(t, db, "p-1", "ed-1", 1, "published", nil)
	insertSource(t, db, "src-1", "p-1", "user_file", shaA)
	insertBlob(t, db, shaA, 1024)

	// sources CASCADE with the profile: provenance of a deleted profile
	// describes nothing.
	if _, err := db.Exec(`DELETE FROM profiles WHERE id = 'p-1'`); err != nil {
		t.Fatalf("deleting a childless profile: %v", err)
	}
	if n := countRows(t, db, "sources"); n != 0 {
		t.Fatalf("sources rows = %d after the profile was deleted, want 0 (CASCADE)", n)
	}
	// ... while the content-addressed blob survives on purpose.
	if n := countRows(t, db, "source_blobs"); n != 1 {
		t.Fatalf("source_blobs rows = %d, want 1: blob lifetime is GC, not row cascades", n)
	}

	// profiles RESTRICT: an edition holding revisions cannot be dropped, and
	// neither can the game above it, because RESTRICT aborts the cascade.
	insertProfile(t, db, "p-2", "ed-1", 1, "draft", nil)
	if _, err := db.Exec(`DELETE FROM editions WHERE id = 'ed-1'`); err == nil {
		t.Fatal("deleting an edition with profile revisions succeeded; want RESTRICT")
	}
	if _, err := db.Exec(`DELETE FROM games WHERE id = 'game-1'`); err == nil {
		t.Fatal("deleting a game whose profiles still exist succeeded; want RESTRICT to abort the cascade")
	}

	// An edition with no revisions cascades from its game.
	if _, err := db.ExecContext(ctx, `DELETE FROM games WHERE id = 'game-2'`); err != nil {
		t.Fatalf("deleting a profile-free game: %v", err)
	}
	if n := countRows(t, db, "editions"); n != 1 {
		t.Fatalf("editions rows = %d, want only ed-1 to survive", n)
	}
}

func intPtr(v int) *int { return &v }
