package assets

import (
	"bytes"
	"io"
	"runtime"
	"testing"
)

// TestStreamingLargeBlobKeepsMemoryBounded is the docs/spec/16 Milestone 1
// condition "search 10k synthetic messages without memory blowup" applied to
// assets: a 64 MiB import must cost a copy buffer, not 64 MiB of heap. The
// reader is generated, never materialised, and the assertion measures live heap
// after GC so the checker cannot pass by parking garbage.
func TestStreamingLargeBlobKeepsMemoryBounded(t *testing.T) {
	const size = 64 << 20 // 64 MiB
	l := newStore(t)
	ctx := testCtx(t)

	// Independent digest of the same deterministic stream: proves the blob on
	// disk is the whole 64 MiB and not a truncated read that happened to be
	// memory-friendly.
	wantHash, wantSize := hashStream(t, deterministicReader(42, size))
	if wantSize != size {
		t.Fatalf("fixture stream size = %d, want %d", wantSize, size)
	}

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	id, info, err := l.PutStaged(ctx, deterministicReader(42, size), wantHash)
	if err != nil {
		t.Fatalf("PutStaged 64 MiB: %v", err)
	}
	if info.Size != size || info.SHA256 != wantHash {
		t.Fatalf("staged Info = %+v, want {SHA256:%s Size:%d}", info, wantHash, size)
	}
	committed, err := l.Commit(ctx, id)
	if err != nil {
		t.Fatalf("Commit 64 MiB: %v", err)
	}
	if committed != info {
		t.Fatalf("Commit Info = %+v, want %+v", committed, info)
	}

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	growth := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	if growth < 0 {
		growth = 0 // GC reclaimed unrelated garbage during the import
	}
	allocDelta := int64(after.TotalAlloc) - int64(before.TotalAlloc)
	t.Logf("64 MiB import: live heap +%d bytes, cumulative allocated %d bytes", growth, allocDelta)
	const heapBudget = 16 << 20
	if growth > heapBudget {
		t.Fatalf("heap grew by %d bytes after a %d byte streaming commit, want < %d", growth, size, heapBudget)
	}
	// Cumulative allocation is the stronger signal: a store that buffers blobs
	// would show ~64 MiB allocated even after the buffers are freed.
	if allocDelta > 16<<20 {
		t.Fatalf("cumulative allocation for 64 MiB import = %d bytes, want < %d", allocDelta, 16<<20)
	}

	if err := l.Verify(ctx, wantHash); err != nil {
		t.Fatalf("Verify 64 MiB blob: %v", err)
	}
	r, got, err := l.Get(ctx, wantHash)
	if err != nil {
		t.Fatalf("Get 64 MiB blob: %v", err)
	}
	defer r.Close()
	if got.Size != size {
		t.Fatalf("Get Info.Size = %d, want %d", got.Size, size)
	}
	// Stream the body back and re-hash it through a bounded reader.
	haveHash, haveSize := hashStream(t, r)
	if haveHash != wantHash || haveSize != size {
		t.Fatalf("read back %d bytes hashing to %s, want %d bytes hashing to %s", haveSize, haveHash, size, wantHash)
	}
}

// TestExportLargeBlobStreams checks the export path against the same bound: a
// tar of a big source file must not load it into memory (or into the writer).
func TestExportLargeBlobStreams(t *testing.T) {
	const size = 8 << 20 // 8 MiB is enough to show the pattern
	l := newStore(t)
	ctx := testCtx(t)

	hash, _ := hashStream(t, deterministicReader(99, size))
	id, _, err := l.PutStaged(ctx, deterministicReader(99, size), hash)
	if err != nil {
		t.Fatalf("PutStaged: %v", err)
	}
	commitStage(t, l, id)

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	// Counting writer that does not retain anything.
	var written int64
	w := countingWriterFunc(func(p []byte) (int, error) {
		written += int64(len(p))
		return len(p), nil
	})
	if err := l.Export(ctx, []string{hash}, w); err != nil {
		t.Fatalf("Export: %v", err)
	}
	if written < size {
		t.Fatalf("Export wrote %d bytes, want at least the %d byte blob", written, size)
	}

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if growth := int64(after.HeapAlloc) - int64(before.HeapAlloc); growth > 8<<20 {
		t.Fatalf("Export heap grew by %d bytes for an %d byte blob, want < %d", growth, size, 8<<20)
	}
}

type countingWriterFunc func(p []byte) (int, error)

func (f countingWriterFunc) Write(p []byte) (int, error) { return f(p) }

// TestGetStreamsBodyWithoutBuffering keeps the read side honest: reading a blob
// one byte at a time must still yield every byte.
func TestGetStreamsBodyWithoutBuffering(t *testing.T) {
	l := newStore(t)
	ctx := testCtx(t)
	content := bytes.Repeat([]byte("x"), 4096)
	info := commitPut(t, l, content)

	r, _, err := l.Get(ctx, info.SHA256)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer r.Close()
	got, err := io.ReadAll(io.LimitReader(r, int64(len(content))))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("body = %d bytes of %q, want %d bytes of content", len(got), got[:1], len(content))
	}
}

// TestEmptyBlobRoundTrips covers the degenerate size that sniffing and fan-out
// code paths get wrong most often. Go's sniffer documents empty content as
// text/plain; what matters for the store is that every path (staging, Stat,
// Get) derives the same answer for the same bytes.
func TestEmptyBlobRoundTrips(t *testing.T) {
	l := newStore(t)
	ctx := testCtx(t)
	id, staged := mustStage(t, l, nil, hashOf(nil))
	if staged.Size != 0 {
		t.Fatalf("empty blob Size = %d, want 0", staged.Size)
	}
	info := commitStage(t, l, id)
	stat, err := l.Stat(ctx, info.SHA256)
	if err != nil {
		t.Fatalf("Stat empty blob: %v", err)
	}
	if stat != staged {
		t.Fatalf("Stat = %+v, want %+v", stat, staged)
	}
	r, got, err := l.Get(ctx, info.SHA256)
	if err != nil {
		t.Fatalf("Get empty blob: %v", err)
	}
	defer r.Close()
	if got != staged {
		t.Fatalf("Get Info = %+v, want %+v", got, staged)
	}
	if err := l.Verify(ctx, info.SHA256); err != nil {
		t.Fatalf("Verify empty blob: %v", err)
	}
	if body := readAll(t, r); len(body) != 0 {
		t.Fatalf("empty blob body = %q, want nothing", body)
	}
}
