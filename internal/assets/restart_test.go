package assets

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestReopenRootServesExistingBlobsAndRecoversStaging is the restart half of the
// contract: the blob namespace is derived from the content on disk, not from
// process state, so a new handle over the same root sees the same blobs — and an
// import that was mid-flight when the previous process died is discarded by
// startup recovery rather than served half-written (docs/spec/02 "Crash
// recovery", "Migration and backup acceptance").
func TestReopenRootServesExistingBlobsAndRecoversStaging(t *testing.T) {
	root := filepath.Join(t.TempDir(), "assets")
	first, err := NewLocal(root)
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	ctx := testCtx(t)

	content := []byte("a canonical voice reference, 2 seconds")
	published := commitPut(t, first, content)
	// An interrupted import: staged in the previous process, never committed.
	interrupted, interruptedInfo := mustStage(t, first, []byte("download cut off by a restart"), "")

	// A new process opens the same root.
	second, err := NewLocal(root)
	if err != nil {
		t.Fatalf("reopen NewLocal: %v", err)
	}
	discarded, err := second.RecoverStaging(ctx)
	if err != nil {
		t.Fatalf("RecoverStaging after reopen: %v", err)
	}
	if discarded != 1 {
		t.Fatalf("RecoverStaging discarded %d, want 1", discarded)
	}
	if _, err := second.Commit(ctx, interrupted); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Commit of a recovered staging ID: err = %v, want ErrNotFound", err)
	}
	if _, _, err := second.Get(ctx, interruptedInfo.SHA256); !errors.Is(err, ErrNotFound) {
		t.Fatalf("interrupted import is visible after recovery: %v", err)
	}

	r, info, err := second.Get(ctx, published.SHA256)
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	defer r.Close()
	if !bytes.Equal(readAll(t, r), content) {
		t.Fatalf("content changed across restart for %s", published.SHA256)
	}
	if info.SHA256 != published.SHA256 || info.Size != published.Size || info.MediaType != published.MediaType {
		t.Fatalf("Info after reopen = %+v, want %+v", info, published)
	}
	if err := second.Verify(ctx, published.SHA256); err != nil {
		t.Fatalf("Verify after reopen: %v", err)
	}

	// And a fresh handle can keep importing into the same tree, deduplicating
	// against what a previous process wrote.
	again := commitPut(t, second, content)
	if again.SHA256 != published.SHA256 {
		t.Fatalf("re-committed content landed at %s, want %s", again.SHA256, published.SHA256)
	}
	if got := blobHashes(t, second); len(got) != 1 {
		t.Fatalf("blobs after restart import = %v, want exactly one", got)
	}
}

// TestCommitMakesBlobVisibleAtomicallyToAnotherHandle records the property the
// importer relies on: another Store instance over the same root sees a blob only
// after Commit, and sees it complete the moment it appears.
func TestCommitMakesBlobVisibleAtomicallyToAnotherHandle(t *testing.T) {
	root := filepath.Join(t.TempDir(), "assets")
	writer, err := NewLocal(root)
	if err != nil {
		t.Fatalf("NewLocal writer: %v", err)
	}
	reader, err := NewLocal(root)
	if err != nil {
		t.Fatalf("NewLocal reader: %v", err)
	}
	ctx := testCtx(t)

	content := bytes.Repeat([]byte("frame capture payload "), 512)
	want := hashOf(content)
	id, info := mustStage(t, writer, content, want)

	if _, err := reader.Stat(ctx, want); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reader saw an uncommitted blob: err = %v, want ErrNotFound", err)
	}
	commitStage(t, writer, id)
	if err := reader.Verify(ctx, want); err != nil {
		t.Fatalf("reader Verify after writer Commit: %v", err)
	}
	if info.SHA256 != want {
		t.Fatalf("staged Info = %s, want %s", info.SHA256, want)
	}
	// The staging directory is empty again: no garbage for the next GC to trip on.
	if got := stagingFiles(t, writer); len(got) != 0 {
		t.Fatalf("staging after commit = %v, want empty", got)
	}
}

// TestStoreInterfaceIsSatisfiedByLocal is a compile-time check that the local
// backend implements exactly the docs/spec/02 contract — a future method added
// to the interface must also be implemented here, not silently type-asserted
// away by callers.
func TestStoreInterfaceIsSatisfiedByLocal(t *testing.T) {
	var s Store = func() Store {
		l, err := NewLocal(filepath.Join(t.TempDir(), "assets"))
		if err != nil {
			t.Fatalf("NewLocal: %v", err)
		}
		return l
	}()
	if _, err := s.Stat(testCtx(t), hashOf([]byte("nothing"))); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat through the interface: err = %v, want ErrNotFound", err)
	}
}

// TestTamperedBlobIsDetectableBeforePlayback is the shape the runtime needs: a
// cached WAV that no longer matches its address must fail Verify so the caller
// can re-synthesize instead of speaking corrupted audio (docs/spec/02 "verify on
// read").
func TestTamperedBlobIsDetectableBeforePlayback(t *testing.T) {
	l := newStore(t)
	ctx := testCtx(t)
	content := bytes.Repeat([]byte("generated utterance pcm "), 256)
	info := commitPut(t, l, content)

	if err := l.Verify(ctx, info.SHA256); err != nil {
		t.Fatalf("Verify clean blob: %v", err)
	}

	p := blobPath(t, l, info.SHA256)
	f, err := os.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open blob to truncate it: %v", err)
	}
	// Truncation is the other corruption mode: same address, fewer bytes.
	if err := f.Truncate(int64(len(content)) - 16); err != nil {
		f.Close()
		t.Fatalf("truncate blob: %v", err)
	}
	f.Close()

	if err := l.Verify(ctx, info.SHA256); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Verify truncated blob: err = %v, want ErrCorrupt", err)
	}
	stat, err := l.Stat(ctx, info.SHA256)
	if err != nil {
		t.Fatalf("Stat truncated blob: %v", err)
	}
	if stat.Size != int64(len(content))-16 {
		t.Fatalf("Stat reported Size = %d, want %d (Stat reports what is there; Verify decides if it is right)", stat.Size, len(content)-16)
	}
}
