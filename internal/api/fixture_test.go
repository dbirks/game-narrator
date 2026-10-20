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
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"runtime/pprof"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/store"
)

// testTemplateDB is the path to the fully-migrated template database file,
// built once in TestMain and read (never written) by the fixtures below.
var testTemplateDB string

// TestMain builds the shared migrated-DB template once per package, then runs
// the package's tests. It adds the template and the package-level goroutine
// leak gate (issue #18) and nothing else: it does not set environment, skip,
// or otherwise alter any test. The template directory is removed only after
// every test has run.
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

	// The leak gate's baseline (issue #18): a pprof goroutine dump taken
	// before any test runs. The verdict is on the delta of the stacks'
	// multiset, so a stack that existed before the tests cancels out (the
	// known background workers — the test runner, the runtime, a store's sql
	// connectionOpener still draining). What the gate catches is the rest:
	// a goroutine a test started that never stopped (a capture push watcher
	// that lost its context, a session loop nobody stopped). A bounded grace
	// lets transient workers settle after the last cleanup before the
	// verdict. There is no acknowledgement list: the two process-lived
	// workers that were acknowledged (the status engine, issue #19, and the
	// scenario worker, issue #20) are now stopped by their fixtures — the
	// status env closes its hub, and the worker returns from a finished job
	// at once and stops on a closed store — so the gate waits on them.
	before := goroutineStackCounts()
	code := m.Run()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if leakedGoroutineBlocks(before, goroutineStackCounts()) == "" || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if leaked := leakedGoroutineBlocks(before, goroutineStackCounts()); leaked != "" {
		fmt.Fprintf(os.Stderr, "leak: goroutines the tests started are still alive after every test's cleanups ran (issue #18):\n%s\n", leaked)
		code = 1
	}
	os.RemoveAll(dir)
	os.Exit(code)
}

// goroutineStackCounts takes a pprof goroutine dump and counts the goroutines
// per normalised stack (the block text with its goroutine id stripped, so the
// same stack under different ids is one key). Debug 2 is the legacy
// per-goroutine stack format (Go 1.24 and later write the collapsed or
// binary pprof formats for debug 0/1, which carry no per-goroutine blocks and
// no "created by" line the operator needs to find the leaking start site).
func goroutineStackCounts() map[string]int {
	var buf bytes.Buffer
	if err := pprof.Lookup("goroutine").WriteTo(&buf, 2); err != nil {
		fmt.Fprintf(os.Stderr, "leak gate: goroutine profile unavailable: %v (skipping the gate)\n", err)
		return nil
	}
	counts := map[string]int{}
	var cur []string
	flush := func() {
		if len(cur) == 0 {
			return
		}
		cur[0] = normaliseGoroutineHeader(cur[0])
		block := strings.Join(cur, "\n")
		cur = nil
		counts[block]++
	}
	for _, line := range strings.Split(buf.String(), "\n") {
		if isGoroutineHeader(line) {
			flush()
			// The main goroutine (id 1, the test runner) is the one running
			// this very gate: its stack is "waiting in m.Run" in one dump
			// and "inside goroutineStackCounts" in the other, so it can
			// never cancel out. The gate counts the goroutines the tests
			// start, not the runner that runs the gate.
			if strings.HasPrefix(line, "goroutine 1 [") {
				continue
			}
			cur = append(cur, line)
		} else if cur != nil {
			cur = append(cur, line)
		}
	}
	flush()
	return counts
}

// The gate's known-background-worker list used to live here
// (isIgnoredGoroutine). It is empty and has been removed: its last two
// entries were the status engine (its test env now closes the hub, issue
// #19) and the scenario worker (a finished job releases its worker at once
// and a closed store stops the claim loop on the first answer, issue #20),
// both of which are now enforced by the gate. If a new process-lived worker
// appears, fix the fixture that starts it rather than re-adding an
// acknowledgement.

// isGoroutineHeader is a dump's per-goroutine line ("goroutine 12 [select]:"),
// not the profile's total ("goroutine profile: total 12").
func isGoroutineHeader(line string) bool {
	rest, ok := strings.CutPrefix(line, "goroutine ")
	if !ok {
		return false
	}
	for i, r := range rest {
		switch {
		case r >= '0' && r <= '9':
		case i > 0 && r == ' ':
			return strings.Contains(rest[i:], "[")
		default:
			return false
		}
	}
	return false
}

// normaliseGoroutineHeader drops the goroutine id so the same stack under
// different ids counts as one stack.
func normaliseGoroutineHeader(line string) string {
	if i := strings.IndexByte(line, ' '); i >= 0 {
		if j := strings.IndexByte(line[i+1:], ' '); j >= 0 {
			return line[:i] + line[i+1+j:]
		}
	}
	return line
}

// leakedGoroutineBlocks reports the stacks whose count grew between the two
// dumps — the goroutines the tests started that never exited — formatted as
// the failing test's output (each leaking block with its count, and nothing
// else: the unchanged stacks are the known background workers). Empty when
// nothing leaked; empty counts (an unavailable profile) disable the gate.
func leakedGoroutineBlocks(before, after map[string]int) string {
	if before == nil || after == nil {
		return ""
	}
	var leaked []string
	for stack, n := range after {
		if d := n - before[stack]; d > 0 {
			var b strings.Builder
			fmt.Fprintf(&b, "%d goroutine(s) on stack:\n", d)
			fmt.Fprint(&b, stack)
			if !strings.HasSuffix(stack, "\n") {
				b.WriteString("\n")
			}
			leaked = append(leaked, b.String())
		}
	}
	if len(leaked) == 0 {
		return ""
	}
	sort.Strings(leaked)
	return strings.Join(leaked, "\n")
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
