package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestMigrateFromBlankDBIsRepeatable is the contract-mode gate from
// docs/spec/12: migrate a blank database, prove 0001 landed, prove a second run
// changes nothing, and prove the recorded version says so.
func TestMigrateFromBlankDBIsRepeatable(t *testing.T) {
	s := openIn(t)
	ctx := testCtx(t)

	if v, err := s.SchemaVersion(ctx); err != nil || v != 0 {
		t.Fatalf("SchemaVersion of a blank database = %d, %v", v, err)
	}

	applied, err := s.Migrate(ctx)
	if err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	if want := []string{"0001_init.sql", "0002_content.sql", "0003_jobs_events.sql"}; strings.Join(applied, ",") != strings.Join(want, ",") {
		t.Fatalf("first Migrate applied %v, want %v", applied, want)
	}
	if v, err := s.SchemaVersion(ctx); err != nil || v != 3 {
		t.Fatalf("SchemaVersion = %d, %v; want 3", v, err)
	}

	applied, err = s.Migrate(ctx)
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if len(applied) != 0 {
		t.Fatalf("second Migrate applied %v, want nothing", applied)
	}
	if v, _ := s.SchemaVersion(ctx); v != 3 {
		t.Fatalf("SchemaVersion after a no-op run = %d, want 3", v)
	}

	// The bookkeeping row records who applied what, when, and with which hash.
	var name, checksum, appliedAt string
	if err := s.db.QueryRow(`SELECT name, checksum, applied_at FROM schema_migrations WHERE version = 1`).
		Scan(&name, &checksum, &appliedAt); err != nil {
		t.Fatal(err)
	}
	migs, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	if name != migs[0].Name || checksum != migs[0].Checksum {
		t.Fatalf("recorded (%s, %s), want (%s, %s)", name, checksum, migs[0].Name, migs[0].Checksum)
	}
	if !strings.HasSuffix(appliedAt, "Z") && !strings.Contains(appliedAt, "+") {
		t.Fatalf("applied_at %q is not an RFC 3339 timestamp", appliedAt)
	}
}

// TestMigrationsAreDenseAndNamed pins the embedded set: the three migrations
// shipped so far, named and hashed.
func TestMigrationsAreDenseAndNamed(t *testing.T) {
	migs, err := Migrations()
	if err != nil {
		t.Fatalf("Migrations: %v", err)
	}
	if len(migs) != 3 {
		t.Fatalf("embedded migrations = %d, want 3 (%v)", len(migs), names(migs))
	}
	if migs[0].Version != 1 || migs[0].Name != "0001_init.sql" {
		t.Fatalf("first migration = %d/%s, want 1/0001_init.sql", migs[0].Version, migs[0].Name)
	}
	if len(migs[0].Checksum) != 64 || migs[0].Checksum != strings.ToLower(migs[0].Checksum) {
		t.Fatalf("checksum %q is not 64 lowercase hex characters", migs[0].Checksum)
	}
	if !strings.Contains(migs[0].SQL, "CREATE TABLE games") {
		t.Fatal("0001_init.sql does not create the games table")
	}
}

// TestMigrateRefusesTamperedChecksum is the tamper gate: an applied migration
// whose embedded file no longer matches must stop startup, not "continue".
func TestMigrateRefusesTamperedChecksum(t *testing.T) {
	s := migratedStore(t)
	ctx := testCtx(t)

	// Simulate a build whose 0001_init.sql drifted after being applied.
	const tampered = "0000000000000000000000000000000000000000000000000000000000000000"
	mustExec(t, s.db, `UPDATE schema_migrations SET checksum = ? WHERE version = 1`, tampered)

	applied, err := s.Migrate(ctx)
	if err == nil {
		t.Fatal("Migrate accepted a checksum mismatch")
	}
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("Migrate error = %v, want ErrChecksumMismatch", err)
	}
	if len(applied) != 0 {
		t.Fatalf("Migrate applied %v while the history was inconsistent", applied)
	}
	if !strings.Contains(err.Error(), "0001_init.sql") ||
		!strings.Contains(err.Error(), tampered) {
		t.Fatalf("error does not name the migration and both hashes: %v", err)
	}

	// And the mismatch is fatal for every later call too, not just the first.
	if _, err := s.Migrate(ctx); err == nil {
		t.Fatal("second Migrate accepted the tampered history")
	}
}

// TestMigrateRefusesUnknownRecordedVersion covers the other direction: the data
// is newer than the binary.
func TestMigrateRefusesUnknownRecordedVersion(t *testing.T) {
	s := migratedStore(t)
	ctx := testCtx(t)

	mustExec(t, s.db,
		`INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (99, '0099_future.sql', ?, ?)`,
		strings.Repeat("f", 64), nowRFC3339())

	if _, err := s.Migrate(ctx); err == nil {
		t.Fatal("Migrate accepted a version this build has no file for")
	} else if !strings.Contains(err.Error(), "99") {
		t.Fatalf("error = %v, want it to name version 99", err)
	}
}

// TestApplyMigrationRollsBackCompletely is the interrupted-migration case from
// docs/spec/02 "Migration and backup acceptance": a file that fails halfway
// must leave neither objects nor a bookkeeping row behind.
func TestApplyMigrationRollsBackCompletely(t *testing.T) {
	s := migratedStore(t)
	ctx := testCtx(t)

	breaking := Migration{
		Version:  4,
		Name:     "0004_breaking.sql",
		Checksum: strings.Repeat("1", 64),
		SQL:      "CREATE TABLE interrupted_here (id TEXT PRIMARY KEY); INSERT INTO no_such_table VALUES (1);",
	}
	if err := applyMigration(ctx, s.db, breaking); err == nil {
		t.Fatal("applyMigration accepted a failing migration")
	} else if !strings.Contains(err.Error(), "0004_breaking.sql") {
		t.Fatalf("error %v does not name the failing migration", err)
	}

	if n := countTablesNamed(t, s, "interrupted_here"); n != 0 {
		t.Fatalf("interrupted migration left %d interrupted_here object(s) behind", n)
	}
	if n := countRows(t, s.db, "schema_migrations"); n != 3 {
		t.Fatalf("schema_migrations rows = %d, want the three applied rows", n)
	}
	if v, _ := s.SchemaVersion(ctx); v != 3 {
		t.Fatalf("SchemaVersion = %d after a rolled-back migration, want 3", v)
	}
}

func countTablesNamed(t *testing.T, s *Store, name string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(),
		"SELECT count(*) FROM sqlite_master WHERE name = ?", name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func names(migs []Migration) []string {
	out := make([]string, len(migs))
	for i, m := range migs {
		out[i] = m.Name
	}
	return out
}

// TestSplitSQL keeps statement boundaries where SQL keeps them: semicolons in
// comments and string literals do not end a statement.
func TestSplitSQL(t *testing.T) {
	cases := []struct {
		name   string
		script string
		want   []string
	}{
		{
			name:   "two statements",
			script: "CREATE TABLE a (x);\nCREATE TABLE b (y);\n",
			want:   []string{"CREATE TABLE a (x)", "CREATE TABLE b (y)"},
		},
		{
			name:   "line comment containing a semicolon",
			script: "-- delete this; keep that\nCREATE TABLE a (x);\n",
			want:   []string{"-- delete this; keep that\nCREATE TABLE a (x)"},
		},
		{
			name:   "trailing line comment without newline",
			script: "CREATE TABLE a (x); -- note\n",
			want:   []string{"CREATE TABLE a (x)"},
		},
		{
			name:   "block comment containing a semicolon",
			script: "/* one; two */ CREATE TABLE a (x);\n",
			want:   []string{"/* one; two */ CREATE TABLE a (x)"},
		},
		{
			name:   "semicolon inside a string literal",
			script: "INSERT INTO t VALUES ('a;b');\n",
			want:   []string{"INSERT INTO t VALUES ('a;b')"},
		},
		{
			name:   "doubled quote escapes are not terminators",
			script: "INSERT INTO t VALUES ('it''s; fine');\n",
			want:   []string{"INSERT INTO t VALUES ('it''s; fine')"},
		},
		{
			name:   "quoted identifiers",
			script: "CREATE TABLE \"we;ird\" (x);\n",
			want:   []string{"CREATE TABLE \"we;ird\" (x)"},
		},
		{
			name:   "comment-only file yields no statements",
			script: "-- nothing here\n/* nor here */\n",
			want:   nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := splitSQL(tc.script)
			if tc.want == nil {
				if err == nil {
					t.Fatalf("splitSQL(%q) = %v, want an error for an empty script", tc.script, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("splitSQL(%q): %v", tc.script, err)
			}
			if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
				t.Fatalf("splitSQL(%q) =\n%#v\nwant\n%#v", tc.script, got, tc.want)
			}
		})
	}
}

func TestSplitSQLRejectsUnbalancedInput(t *testing.T) {
	for _, script := range []string{
		"CREATE TABLE a (x TEXT DEFAULT 'unterminated);\n",
		"/* never closed\nCREATE TABLE a (x);\n",
	} {
		if _, err := splitSQL(script); err == nil {
			t.Errorf("splitSQL(%q) = nil error, want failure", script)
		}
	}
}

// TestSplitSQLHandlesTheRealInitFile proves the shipped migration is executed
// statement by statement, not swallowed as one blob or cut in the wrong place.
func TestSplitSQLHandlesTheRealInitFile(t *testing.T) {
	migs, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	statements, err := splitSQL(migs[0].SQL)
	if err != nil {
		t.Fatalf("splitSQL(0001_init.sql): %v", err)
	}
	// 5 CREATE TABLE + 5 indexes (2 plain, 2 unique, 1 per-table UNIQUE on
	// games.slug is a constraint, not a separate statement).
	if len(statements) < 6 {
		t.Fatalf("0001_init.sql split into %d statements, want one CREATE per table and index", len(statements))
	}

	counts := map[string]int{}
	for _, stmt := range statements {
		code := strings.ToUpper(stripLeadComments(stmt))
		switch {
		case strings.HasPrefix(code, "CREATE TABLE"):
			counts["table"]++
		case strings.HasPrefix(code, "CREATE UNIQUE INDEX"):
			counts["unique index"]++
		case strings.HasPrefix(code, "CREATE INDEX"):
			counts["index"]++
		default:
			t.Errorf("statement does not start with a recognised CREATE: %.80q", stmt)
		}
	}
	if counts["table"] != 5 {
		t.Errorf("CREATE TABLE statements = %d, want the 5 catalog tables", counts["table"])
	}
	if counts["unique index"] < 1 {
		t.Errorf("no CREATE UNIQUE INDEX statement found")
	}
}

// stripLeadComments drops the leading comment lines of a statement so the DDL
// verb can be inspected.
func stripLeadComments(stmt string) string {
	lines := strings.Split(stmt, "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "--") || strings.TrimSpace(line) == "" {
			continue
		}
		return strings.Join(lines[i:], "\n")
	}
	return ""
}
