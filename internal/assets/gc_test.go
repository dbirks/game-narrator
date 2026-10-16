package assets

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestRecoverStagingDiscardsLeftovers is the crash-recovery step from
// docs/spec/02 "Transaction workflows": an interrupted process leaves staged
// bytes, and startup must clear them rather than accumulate them.
func TestRecoverStagingDiscardsLeftovers(t *testing.T) {
	l := newStore(t)
	ctx := testCtx(t)

	// A staged-but-never-committed entry (the interrupted import).
	liveID, liveInfo := mustStage(t, l, []byte("content of an interrupted import"), "")
	// A raw leftover file, planted as a crashed process would leave it — with an
	// ID this process has no record of.
	planted := StagingID(strings.Repeat("cd", 16))
	if err := os.WriteFile(stagingPath(t, l, planted), []byte("half written download"), 0o600); err != nil {
		t.Fatalf("plant staging leftover: %v", err)
	}
	// A committed blob must survive staging recovery.
	committed := commitPut(t, l, []byte("already published content"))

	discarded, err := l.RecoverStaging(ctx)
	if err != nil {
		t.Fatalf("RecoverStaging: %v", err)
	}
	if discarded < 2 {
		t.Fatalf("RecoverStaging discarded %d files, want at least 2", discarded)
	}
	if got := stagingFiles(t, l); len(got) != 0 {
		t.Fatalf("staging after recovery = %v, want empty", got)
	}
	if _, _, err := l.Get(ctx, liveInfo.SHA256); !errors.Is(err, ErrNotFound) {
		t.Fatalf("uncommitted content is readable after recovery: %v", err)
	}
	// The discarded ID can no longer be committed — recovery, not resurrection.
	if _, err := l.Commit(ctx, liveID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Commit after recovery: err = %v, want ErrNotFound", err)
	}
	if got := blobHashes(t, l); len(got) != 1 || got[0] != committed.SHA256 {
		t.Fatalf("blobs after recovery = %v, want [%s]", got, committed.SHA256)
	}
	// Idempotent, and the staging directory itself must survive.
	again, err := l.RecoverStaging(ctx)
	if err != nil {
		t.Fatalf("second RecoverStaging: %v", err)
	}
	if again != 0 {
		t.Fatalf("second RecoverStaging discarded %d, want 0", again)
	}
	if fi, err := os.Lstat(filepath.Join(l.Root(), "staging")); err != nil || !fi.IsDir() {
		t.Fatalf("staging directory removed: %v", err)
	}
}

// TestRecoverStagingOnFreshStoreIsQuiet.
func TestRecoverStagingOnFreshStoreIsQuiet(t *testing.T) {
	l := newStore(t)
	discarded, err := l.RecoverStaging(testCtx(t))
	if err != nil {
		t.Fatalf("RecoverStaging on fresh store: %v", err)
	}
	if discarded != 0 {
		t.Fatalf("discarded = %d, want 0", discarded)
	}
}

// TestDeleteUnreachableDryRunThenDelete is the GC contract: report first, delete
// only what the reachability predicate rejects, and keep everything else.
func TestDeleteUnreachableDryRunThenDelete(t *testing.T) {
	l := newStore(t)
	ctx := testCtx(t)

	reachable := commitPut(t, l, []byte("referenced by a source_blobs row"))
	orphanA := commitPut(t, l, []byte("orphan A: no row points here"))
	orphanB := commitPut(t, l, []byte("orphan B: left by a failed import"))

	seen := map[string]bool{}
	predicate := func(h string) bool {
		seen[h] = true
		return h == reachable.SHA256
	}

	listed, err := l.DeleteUnreachable(ctx, predicate, true)
	if err != nil {
		t.Fatalf("DeleteUnreachable dry run: %v", err)
	}
	if got, want := sorted(listed), sorted([]string{orphanA.SHA256, orphanB.SHA256}); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("dry run listed %v, want %v", got, want)
	}
	if len(seen) != 3 {
		t.Fatalf("predicate saw %d blobs, want all 3 (%v)", len(seen), seen)
	}
	// A dry run must not touch anything.
	if got, want := blobHashes(t, l), sorted([]string{reachable.SHA256, orphanA.SHA256, orphanB.SHA256}); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("blobs after dry run = %v, want %v", got, want)
	}

	deleted, err := l.DeleteUnreachable(ctx, predicate, false)
	if err != nil {
		t.Fatalf("DeleteUnreachable: %v", err)
	}
	if got, want := sorted(deleted), sorted([]string{orphanA.SHA256, orphanB.SHA256}); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("deleted %v, want %v", got, want)
	}
	if got := blobHashes(t, l); len(got) != 1 || got[0] != reachable.SHA256 {
		t.Fatalf("blobs after GC = %v, want [%s]", got, reachable.SHA256)
	}
	// The reachable blob is still intact and verified.
	if err := l.Verify(ctx, reachable.SHA256); err != nil {
		t.Fatalf("Verify surviving blob: %v", err)
	}
	// Fan-out directories for deleted blobs are pruned; the blobs root stays.
	if _, err := os.Lstat(filepath.Dir(blobPath(t, l, orphanA.SHA256))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty fan-out dir for %s kept: %v", orphanA.SHA256, err)
	}
	if fi, err := os.Lstat(filepath.Join(l.Root(), "blobs")); err != nil || !fi.IsDir() {
		t.Fatalf("blobs root removed: %v", err)
	}
}

// TestDeleteUnreachableKeepsStagingAndForeignFiles is the "never deletes anything
// outside blobs/" boundary: GC is scoped, and a stray file is not garbage.
func TestDeleteUnreachableKeepsStagingAndForeignFiles(t *testing.T) {
	l := newStore(t)
	ctx := testCtx(t)

	orphan := commitPut(t, l, []byte("unreachable blob to be collected"))
	keep := commitPut(t, l, []byte("reachable blob"))

	// A staged write in flight must not be collected by a GC pass.
	stagedID, _ := mustStage(t, l, []byte("in-flight import"), "")
	// Files that are not blobs in the expected layout, inside the root.
	stray := filepath.Join(l.Root(), "diagnostics.log")
	if err := os.WriteFile(stray, []byte("log"), 0o600); err != nil {
		t.Fatalf("write stray file: %v", err)
	}
	junk := filepath.Join(l.Root(), "blobs", "junk.txt")
	if err := os.WriteFile(junk, []byte("not a blob"), 0o600); err != nil {
		t.Fatalf("write non-blob file under blobs/: %v", err)
	}
	// A file that looks like a blob but sits at the wrong depth.
	misplaced := filepath.Join(l.Root(), "blobs", orphan.SHA256)
	if err := os.WriteFile(misplaced, []byte("wrong layout"), 0o600); err != nil {
		t.Fatalf("write misplaced file: %v", err)
	}

	deleted, err := l.DeleteUnreachable(ctx, func(h string) bool { return h == keep.SHA256 }, false)
	if err != nil {
		t.Fatalf("DeleteUnreachable: %v", err)
	}
	if len(deleted) != 1 || deleted[0] != orphan.SHA256 {
		t.Fatalf("deleted = %v, want [%s]", deleted, orphan.SHA256)
	}
	if _, err := os.Lstat(stagingPath(t, l, stagedID)); err != nil {
		t.Fatalf("GC deleted an in-flight staging file: %v", err)
	}
	if _, err := os.Lstat(stray); err != nil {
		t.Fatalf("GC deleted a file outside blobs/: %v", err)
	}
	if _, err := os.Lstat(junk); err != nil {
		t.Fatalf("GC deleted a non-blob file under blobs/: %v", err)
	}
	if _, err := os.Lstat(misplaced); err != nil {
		t.Fatalf("GC deleted a hash-shaped file at the wrong depth: %v", err)
	}
	if got := blobHashes(t, l); len(got) != 1 || got[0] != keep.SHA256 {
		t.Fatalf("blobs after GC = %v, want [%s]", got, keep.SHA256)
	}
}

// TestDeleteUnreachableAllOrNothingPredicate covers the "catalogue is empty"
// case, e.g. a fresh restore with blobs left behind.
func TestDeleteUnreachableEverythingAndPruneTree(t *testing.T) {
	l := newStore(t)
	ctx := testCtx(t)
	for _, c := range []string{"prune me one", "prune me two", "prune me three"} {
		commitPut(t, l, []byte(c))
	}
	deleted, err := l.DeleteUnreachable(ctx, func(string) bool { return false }, false)
	if err != nil {
		t.Fatalf("DeleteUnreachable: %v", err)
	}
	if len(deleted) != 3 {
		t.Fatalf("deleted %d blobs, want 3", len(deleted))
	}
	if got := blobHashes(t, l); len(got) != 0 {
		t.Fatalf("blobs remain after full GC: %v", got)
	}
	// Every fan-out directory emptied out and was pruned; only blobs/ remains.
	entries, err := os.ReadDir(filepath.Join(l.Root(), "blobs"))
	if err != nil {
		t.Fatalf("read blobs dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("blobs/ still contains %v after pruning", entries)
	}
	// The store keeps working afterwards.
	info := commitPut(t, l, []byte("store still writable after pruning"))
	if err := l.Verify(ctx, info.SHA256); err != nil {
		t.Fatalf("Verify after pruning: %v", err)
	}
}

func TestDeleteUnreachableRequiresPredicate(t *testing.T) {
	l := newStore(t)
	if _, err := l.DeleteUnreachable(testCtx(t), nil, true); err == nil {
		t.Fatal("DeleteUnreachable(nil predicate): want error, got nil")
	}
}

func TestDeleteUnreachableOnFreshStoreIsQuiet(t *testing.T) {
	l := newStore(t)
	deleted, err := l.DeleteUnreachable(testCtx(t), func(string) bool { return false }, false)
	if err != nil {
		t.Fatalf("DeleteUnreachable on fresh store: %v", err)
	}
	if len(deleted) != 0 {
		t.Fatalf("deleted = %v, want none", deleted)
	}
}

func TestDeleteUnreachableHonoursCancellation(t *testing.T) {
	l := newStore(t)
	commitPut(t, l, []byte("cancellation probe blob"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := l.DeleteUnreachable(ctx, func(string) bool { return false }, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("DeleteUnreachable on cancelled context: err = %v, want context.Canceled", err)
	}
	if len(blobHashes(t, l)) != 1 {
		t.Fatalf("cancelled GC deleted blobs: %v", blobHashes(t, l))
	}
}

// TestDeleteUnreachableIgnoresSymlinks: a link planted inside the blob tree is a
// tampering signal. GC must neither follow it nor delete through it.
func TestDeleteUnreachableIgnoresSymlinks(t *testing.T) {
	l := newStore(t)
	ctx := testCtx(t)
	target := filepath.Join(t.TempDir(), "important.bin")
	if err := os.WriteFile(target, []byte("do not delete this"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	// A hash-named symlink at the correct fan-out depth.
	fake := hashOf([]byte("symlinked blob entry"))
	p := blobPath(t, l, fake)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatalf("create fan-out dir: %v", err)
	}
	if err := os.Symlink(target, p); err != nil {
		t.Fatalf("plant symlink blob: %v", err)
	}

	deleted, err := l.DeleteUnreachable(ctx, func(string) bool { return false }, false)
	if err != nil {
		t.Fatalf("DeleteUnreachable: %v", err)
	}
	if len(deleted) != 0 {
		t.Fatalf("GC claimed to delete %v, which is a symlink", deleted)
	}
	if _, err := os.Lstat(target); err != nil {
		t.Fatalf("GC deleted through a symlink: %v", err)
	}
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
