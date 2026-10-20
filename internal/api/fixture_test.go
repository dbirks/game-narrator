package api

// Shared test-fixture machinery for the api package (issue #15).
//
// The api suite spends most of its wall time re-migrating a fresh SQLite
// database for every test: newTestAuth (and the routes it backs) used to
// store.Open a new file and run all 16 migrations per test, ~1.4s each under
// -race, and the package has 100+ such fixtures. The template built here
// migrates once per package (TestMain) and every test copies that single
// self-contained file into its own t.TempDir(), so the per-test cost drops to
// a file copy plus store.Open (tens of ms). This changes no test's behaviour:
// each test still gets a private, already-migrated store in its own temp dir,
// and no test ever shares a database file with another.

import (
	"context"
	"fmt"
	"io"
	"os"
	"testing"

	"github.com/dbirks/game-narrator/internal/store"
)

// testTemplateDB is the path to the fully-migrated template database file,
// built once in TestMain and read (never written) by the fixtures below.
var testTemplateDB string

// TestMain builds the shared migrated-DB template once per package, then runs
// the package's tests. It adds the template and nothing else: it does not set
// environment, skip, or otherwise alter any test. The template directory is
// removed only after every test has run.
func TestMain(m *testing.M) {
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "gn-api-test-template")
	if err != nil {
		fmt.Fprintln(os.Stderr, "template: tempdir:", err)
		os.Exit(1)
	}
	st, err := store.Open(ctx, store.DBPath(dir))
	if err != nil {
		fmt.Fprintln(os.Stderr, "template: open:", err)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	if _, err := st.Migrate(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "template: migrate:", err)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	// Close cleanly so the WAL is checkpointed into the main file: the copy we
	// hand to tests is that one self-contained file (no -wal/-shm sidecars).
	if err := st.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "template: close:", err)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	testTemplateDB = store.DBPath(dir)

	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// openTemplateStore gives the caller a private, already-migrated store backed
// by a copy of the shared template. The copy lives in the caller's t.TempDir()
// so it is isolated and auto-cleaned; the returned store is closed via t.Cleanup.
func openTemplateStore(t testing.TB) *store.Store {
	t.Helper()
	ctx := context.Background()
	if testTemplateDB == "" {
		t.Fatal("shared test template not built (TestMain did not run?)")
	}
	dst := store.DBPath(t.TempDir())
	src, err := os.Open(testTemplateDB)
	if err != nil {
		t.Fatalf("open template db: %v", err)
	}
	defer func() { _ = src.Close() }()
	out, err := os.Create(dst)
	if err != nil {
		t.Fatalf("create db file: %v", err)
	}
	if _, err := io.Copy(out, src); err != nil {
		_ = out.Close()
		t.Fatalf("copy template db: %v", err)
	}
	if err := out.Close(); err != nil {
		t.Fatalf("close db file: %v", err)
	}
	st, err := store.Open(ctx, dst)
	if err != nil {
		t.Fatalf("open copied db: %v", err)
	}
	// Defensive: the template is migrated and the copy is byte-identical, so
	// this is a cheap no-op (~2ms) that proves the copy is a valid migrated DB.
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate copied db: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}
