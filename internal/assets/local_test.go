package assets

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestPutCommitGetRoundTrip is the contract every other test leans on: staged
// bytes become readable at exactly the address their content hashes to, with the
// size and sniffed media type the catalogue stores.
func TestPutCommitGetRoundTrip(t *testing.T) {
	l := newStore(t)
	ctx := testCtx(t)
	content := []byte("Wind Waker dialogue page: \"Here's the custom, Hero!\"\n")

	id, staged := mustStage(t, l, content, "")
	if want := hashOf(content); staged.SHA256 != want {
		t.Fatalf("staged SHA256 = %s, want %s", staged.SHA256, want)
	}
	if staged.Size != int64(len(content)) {
		t.Fatalf("staged Size = %d, want %d", staged.Size, len(content))
	}
	if staged.MediaType != "text/plain; charset=utf-8" {
		t.Fatalf("staged MediaType = %q, want sniffed text/plain", staged.MediaType)
	}

	// Nothing is visible before Commit — a reader must never see a partial blob.
	if _, _, err := l.Get(ctx, staged.SHA256); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get before Commit: err = %v, want ErrNotFound", err)
	}
	if _, err := l.Stat(ctx, staged.SHA256); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat before Commit: err = %v, want ErrNotFound", err)
	}
	if got := stagingFiles(t, l); len(got) != 1 {
		t.Fatalf("staging after PutStaged = %v, want one entry", got)
	}

	committed := commitStage(t, l, id)
	if committed != staged {
		t.Fatalf("Commit Info = %+v, want %+v", committed, staged)
	}

	r, info, err := l.Get(ctx, staged.SHA256)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer r.Close()
	if got := readAll(t, r); !bytes.Equal(got, content) {
		t.Fatalf("Get body = %q, want %q", got, content)
	}
	if info != staged {
		t.Fatalf("Get Info = %+v, want %+v", info, staged)
	}
	if err := l.Verify(ctx, staged.SHA256); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got := blobHashes(t, l); len(got) != 1 || got[0] != staged.SHA256 {
		t.Fatalf("blobs on disk = %v, want [%s]", got, staged.SHA256)
	}
	if got := stagingFiles(t, l); len(got) != 0 {
		t.Fatalf("staging after Commit = %v, want empty", got)
	}
	// Layout: blobs/ab/cd/<hash> (docs/spec/02 asset store contract).
	if _, err := os.Lstat(blobPath(t, l, staged.SHA256)); err != nil {
		t.Fatalf("expected blob at blobs/%s/%s/%s: %v", staged.SHA256[:2], staged.SHA256[2:4], staged.SHA256, err)
	}
}

// TestBinaryContentTypeSniffed guards the audio/frame path: a WAV must not be
// catalogued as text just because a caller passed a name.
func TestBinaryContentTypeSniffed(t *testing.T) {
	l := newStore(t)
	wav := append([]byte("RIFF\x24\x00\x00\x00WAVEfmt "), make([]byte, 600)...)
	info := commitPut(t, l, wav)
	if !strings.HasPrefix(info.MediaType, "audio/") && info.MediaType != "application/octet-stream" {
		t.Fatalf("MediaType = %q, want a sniffed binary type", info.MediaType)
	}
	if info.MediaType == "text/plain; charset=utf-8" {
		t.Fatalf("MediaType = %q for binary content", info.MediaType)
	}
}

// TestCommitIsIdempotentForDuplicateContent is the deduplication requirement: the
// same bytes imported twice occupy one address and no staging copy survives.
func TestCommitIsIdempotentForDuplicateContent(t *testing.T) {
	l := newStore(t)
	content := []byte("identical source bytes")

	first := commitPut(t, l, content)

	id2, staged2 := mustStage(t, l, content, "")
	second := commitStage(t, l, id2)
	if second != first {
		t.Fatalf("second Commit Info = %+v, want %+v", second, first)
	}
	if staged2 != first {
		t.Fatalf("staged Info = %+v, want %+v", staged2, first)
	}
	if got := blobHashes(t, l); len(got) != 1 {
		t.Fatalf("blobs on disk = %v, want exactly one blob", got)
	}
	if got := stagingFiles(t, l); len(got) != 0 {
		t.Fatalf("staging after duplicate Commit = %v, want empty", got)
	}

	// Re-committing an ID whose content already exists must not truncate or
	// replace the existing blob.
	r, info, err := l.Get(testCtx(t), first.SHA256)
	if err != nil {
		t.Fatalf("Get after duplicate commit: %v", err)
	}
	defer r.Close()
	if got := readAll(t, r); !bytes.Equal(got, content) {
		t.Fatalf("body after duplicate commit = %q, want %q", got, content)
	}
	if info.Size != int64(len(content)) {
		t.Fatalf("Size = %d, want %d", info.Size, len(content))
	}
}

// TestExpectedHashMismatchKeepsNothing: a mislabelled import must leave neither a
// blob nor a staging file, because a half-trusted byte is worse than a failure.
func TestExpectedHashMismatchKeepsNothing(t *testing.T) {
	l := newStore(t)
	ctx := testCtx(t)
	content := []byte("actual bytes")
	wrong := hashOf([]byte("some other bytes"))

	before := treePaths(t, l.Root())
	id, info, err := l.PutStaged(ctx, bytesReader(content), wrong)
	if !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("PutStaged with wrong expected hash: err = %v, want ErrHashMismatch", err)
	}
	if id != "" {
		// An ID may be returned for logging, but it must not be committable.
		if _, err := l.Commit(ctx, id); err == nil {
			t.Fatalf("Commit of mismatched staging ID %s succeeded", id)
		}
	}
	if info != (Info{}) {
		t.Fatalf("Info on mismatch = %+v, want zero", info)
	}
	if got := treePaths(t, l.Root()); strings.Join(got, ",") != strings.Join(before, ",") {
		t.Fatalf("filesystem changed on hash mismatch:\n got %v\nwant %v", got, before)
	}
	if got := blobHashes(t, l); len(got) != 0 {
		t.Fatalf("blobs after mismatch = %v, want none", got)
	}
}

// TestExpectedHashMatchSucceeds shows the same path works when the caller knows
// the hash (import resume by content hash, docs/spec/02).
func TestExpectedHashMatchSucceeds(t *testing.T) {
	l := newStore(t)
	content := []byte("known hash content")
	id, info := mustStage(t, l, content, hashOf(content))
	if got := commitStage(t, l, id); got != info {
		t.Fatalf("Commit = %+v, want %+v", got, info)
	}
}

func TestAbortDiscardsStaging(t *testing.T) {
	l := newStore(t)
	ctx := testCtx(t)
	content := []byte("never published")
	id, info := mustStage(t, l, content, "")
	if err := l.Abort(ctx, id); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if got := stagingFiles(t, l); len(got) != 0 {
		t.Fatalf("staging after Abort = %v, want empty", got)
	}
	if _, _, err := l.Get(ctx, info.SHA256); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Abort: err = %v, want ErrNotFound", err)
	}
	// Aborting twice is not an error: callers run Abort in defer paths.
	if err := l.Abort(ctx, id); err != nil {
		t.Fatalf("second Abort: %v", err)
	}
}

func TestGetMissingIsErrNotFound(t *testing.T) {
	l := newStore(t)
	missing := hashOf([]byte("never stored"))
	if _, _, err := l.Get(testCtx(t), missing); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get: err = %v, want ErrNotFound", err)
	}
	if _, err := l.Stat(testCtx(t), missing); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat: err = %v, want ErrNotFound", err)
	}
	if err := l.Verify(testCtx(t), missing); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Verify: err = %v, want ErrNotFound", err)
	}
}

// TestVerifyDetectsFlippedByte is the "verify on read" gate from docs/spec/02:
// silent bit-rot must surface as ErrCorrupt rather than be narrated.
func TestVerifyDetectsFlippedByte(t *testing.T) {
	l := newStore(t)
	ctx := testCtx(t)
	content := []byte("integrity matters for audio caches too")
	info := commitPut(t, l, content)

	p := blobPath(t, l, info.SHA256)
	f, err := os.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open blob for tampering: %v", err)
	}
	tampered := []byte("INTegrity matters for audio caches too")
	if _, err := f.WriteAt(tampered[:1], 0); err != nil {
		f.Close()
		t.Fatalf("flip byte: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close blob: %v", err)
	}

	if err := l.Verify(ctx, info.SHA256); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Verify after tamper: err = %v, want ErrCorrupt", err)
	}
	// Stat still reports the entry (metadata is readable); it is Verify that
	// decides whether the bytes can be trusted.
	if _, err := l.Stat(ctx, info.SHA256); err != nil {
		t.Fatalf("Stat after tamper: %v", err)
	}
}

// TestCommitRejectsStagingContentThatChangedBetweenCalls covers the window
// between PutStaged and Commit: if the staged bytes change, the digest recorded
// at staging time no longer matches, and Commit must refuse rather than commit
// content it never hashed as a pair.
func TestCommitRejectsStagingContentThatChanged(t *testing.T) {
	l := newStore(t)
	ctx := testCtx(t)
	content := []byte("original staged content, long enough to overwrite")
	id, info := mustStage(t, l, content, "")

	p := stagingPath(t, l, id)
	f, err := os.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open staging for tampering: %v", err)
	}
	if _, err := f.WriteAt([]byte("ORIGINAL staged content, long enough to overwrite"), 0); err != nil {
		f.Close()
		t.Fatalf("overwrite staging: %v", err)
	}
	if err := f.Truncate(int64(len(content))); err != nil {
		f.Close()
		t.Fatalf("truncate staging: %v", err)
	}
	f.Close()

	if _, err := l.Commit(ctx, id); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Commit after staging tamper: err = %v, want ErrCorrupt", err)
	}
	if got := blobHashes(t, l); len(got) != 0 {
		t.Fatalf("blobs after rejected commit = %v, want none", got)
	}
	_ = info
}

func TestReaderErrorPropagates(t *testing.T) {
	l := newStore(t)
	if _, _, err := l.PutStaged(testCtx(t), oneByteErrReader(), ""); err == nil {
		t.Fatal("PutStaged with a failing reader: want error, got nil")
	}
	if got := stagingFiles(t, l); len(got) != 0 {
		t.Fatalf("staging after reader error = %v, want empty", got)
	}
	if got := blobHashes(t, l); len(got) != 0 {
		t.Fatalf("blobs after reader error = %v, want none", got)
	}
}

func TestCancellationDuringStageDiscards(t *testing.T) {
	l := newStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := l.PutStaged(ctx, deterministicReader(7, 1<<20), "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("PutStaged on a cancelled context: err = %v, want context.Canceled", err)
	}
	if got := stagingFiles(t, l); len(got) != 0 {
		t.Fatalf("staging after cancel = %v, want empty", got)
	}
}

// TestModesArePrivate is the docs/spec/02 requirement that asset directories are
// 0700: profiles, sources and voice references are user material and must not be
// group- or world-readable regardless of the daemon's umask.
func TestModesArePrivate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "assets")
	l, err := NewLocal(root)
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	if got := modePerm(t, root); got != 0o700 {
		t.Fatalf("root mode = %o, want 700", got)
	}
	for _, dir := range []string{"blobs", "staging", "blobs/00", "blobs/00/11"} {
		p := filepath.Join(root, filepath.FromSlash(dir))
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatalf("MkdirAll(%s): %v", dir, err)
		}
		if got := modePerm(t, p); got != 0o700 {
			t.Fatalf("%s mode = %o, want 700", dir, got)
		}
	}

	content := []byte("private asset bytes")
	id, info := mustStage(t, l, content, "")
	if got := modePerm(t, stagingPath(t, l, id)); got != 0o600 {
		t.Fatalf("staging file mode = %o, want 600", got)
	}
	commitStage(t, l, id)
	if got := modePerm(t, blobPath(t, l, info.SHA256)); got != 0o600 {
		t.Fatalf("blob mode = %o, want 600", got)
	}
	if got := modePerm(t, filepath.Dir(blobPath(t, l, info.SHA256))); got != 0o700 {
		t.Fatalf("blob fan-out dir mode = %o, want 700", got)
	}
}

// TestNewLocalCreatesRootUnderMissingParents: the store is configured with a data
// directory that does not exist yet on first run.
func TestNewLocalCreatesRootAndSubdirs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "deep", "nested", "assets")
	l, err := NewLocal(root)
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	for _, dir := range []string{"", "blobs", "staging"} {
		p := filepath.Join(root, dir)
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatalf("Lstat(%s): %v", p, err)
		}
		if !fi.IsDir() {
			t.Fatalf("%s is not a directory", p)
		}
		if got := fi.Mode().Perm(); got != 0o700 {
			t.Fatalf("%s mode = %o, want 700", p, got)
		}
	}
	// Reopening an existing root is fine and leaves it usable.
	if _, err := NewLocal(root); err != nil {
		t.Fatalf("NewLocal on existing root: %v", err)
	}
	if err := l.Verify(testCtx(t), hashOf([]byte("nothing"))); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Verify on fresh store: err = %v, want ErrNotFound", err)
	}
}

// TestConcurrentImportersDedupe: two jobs staging identical audio at the same
// time must produce one blob and no leftover staging file, and neither may see a
// half-written read (docs/spec/02 "Asset store contract").
func TestConcurrentImportersDedupe(t *testing.T) {
	l := newStore(t)
	ctx := testCtx(t)
	content := bytes.Repeat([]byte("concurrent utterance payload "), 4096)
	want := hashOf(content)

	const workers = 16
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, info, err := l.PutStaged(ctx, bytesReader(content), want)
			if err != nil {
				errs <- err
				return
			}
			if info.SHA256 != want {
				errs <- errors.New("staged hash mismatch")
				return
			}
			got, err := l.Commit(ctx, id)
			if err != nil {
				errs <- err
				return
			}
			if got.SHA256 != want || got.Size != int64(len(content)) {
				errs <- errors.New("committed info mismatch")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent import: %v", err)
	}

	if got := blobHashes(t, l); len(got) != 1 || got[0] != want {
		t.Fatalf("blobs = %v, want exactly [%s]", got, want)
	}
	if got := stagingFiles(t, l); len(got) != 0 {
		t.Fatalf("staging = %v, want empty", got)
	}
	if err := l.Verify(ctx, want); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	r, _, err := l.Get(ctx, want)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer r.Close()
	body, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(body, content) {
		t.Fatalf("body length = %d, want %d", len(body), len(content))
	}
}

func TestPutStagedRejectsNilReader(t *testing.T) {
	l := newStore(t)
	if _, _, err := l.PutStaged(testCtx(t), nil, ""); err == nil {
		t.Fatal("PutStaged(nil reader): want error, got nil")
	}
}

func TestGetCloseIsIdempotent(t *testing.T) {
	l := newStore(t)
	info := commitPut(t, l, []byte("close me twice"))
	r, _, err := l.Get(testCtx(t), info.SHA256)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
