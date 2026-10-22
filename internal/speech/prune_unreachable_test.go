package speech

// PruneUnreachable tests (ADR-0159, issue #49): the keep-set prune — the
// operator-command half of the speech-cache story. The reachable half is the
// runtime's reachable-key walk (internal/runtime); here the cache's own prune
// is proven on invented data: a keep predicate that names the reachable keys
// keeps them, an unnamed key goes, dry run deletes nothing, and a blob a
// catalogue row still names survives its entry's drop.

import (
	"context"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/providers/tts"
)

func toneAudioAt(freq float64) tts.Audio {
	return tts.Audio{WAV: MakeToneWAV(freq, 0.3, 22050)}
}

// TestPruneUnreachableKeepsReachableDeletesUnreachable is the core shape: two
// committed entries — one the keep set names (reachable), one it does not (the
// pre-change key the page-aware / shared-slot changes orphaned). The prune
// keeps the reachable one and frees the unreachable one's bytes.
func TestPruneUnreachableKeepsReachableDeletesUnreachable(t *testing.T) {
	ctx := context.Background()
	inner, err := assets.NewLocal(t.TempDir() + "/assets")
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	idx := NewMemIndex()
	c := New(inner, idx, 2, WithReferenceChecker(unsharedChecker()))

	ka := testKey(t, "reachable")
	kb := testKey(t, "unreachable")
	if _, err := c.GetOrGenerate(ctx, ka, PriorityCurrent, func(context.Context) (tts.Audio, error) { return toneAudio(), nil }); err != nil {
		t.Fatalf("commit ka: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, err := c.GetOrGenerate(ctx, kb, PriorityCurrent, func(context.Context) (tts.Audio, error) { return toneAudioAt(550), nil }); err != nil {
		t.Fatalf("commit kb: %v", err)
	}
	sizeOf := func(k CacheKey) int64 {
		e, ok := idx.Lookup(k)
		if !ok {
			t.Fatalf("entry %v missing before prune", k)
		}
		return e.Size
	}
	unreachableSize := sizeOf(kb)

	keep := func(k CacheKey) bool { return k == ka }
	entries, freed, err := c.PruneUnreachable(ctx, keep, false)
	if err != nil {
		t.Fatalf("PruneUnreachable: %v", err)
	}
	if entries != 1 || freed != unreachableSize {
		t.Fatalf("PruneUnreachable = %d entries / %d bytes, want 1 / %d (the unreachable one's bytes; freed counts only what was actually reclaimed)", entries, freed, unreachableSize)
	}
	if _, ok := idx.Lookup(ka); !ok {
		t.Fatal("the reachable entry was pruned away: the keep set must survive")
	}
	if _, ok := idx.Lookup(kb); ok {
		t.Fatal("the unreachable entry survived the prune: the keep set must exclude it")
	}
	r, ok, err := c.Get(ctx, ka, false)
	if !ok || err != nil || !r.FromCache {
		t.Fatalf("the kept entry is no longer servable: ok=%v err=%v", ok, err)
	}
	if _, ok := idx.Lookup(kb); ok {
		t.Fatal("the unreachable entry is still in the index")
	}

	t.Logf("prune: keep-set pruned 1 unreachable entry / %d bytes, kept the reachable entry", freed)
}

// TestPruneUnreachableDryRunDeletesNothing: dry run reports the would-be
// answer — the entries and bytes — without touching the index, the blobs or
// the LRU evidence.
func TestPruneUnreachableDryRunDeletesNothing(t *testing.T) {
	ctx := context.Background()
	inner, err := assets.NewLocal(t.TempDir() + "/assets")
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	idx := NewMemIndex()
	c := New(inner, idx, 2, WithReferenceChecker(unsharedChecker()))

	ka := testKey(t, "dryrun-keep")
	kb := testKey(t, "dryrun-drop")
	if _, err := c.GetOrGenerate(ctx, ka, PriorityCurrent, func(context.Context) (tts.Audio, error) { return toneAudio(), nil }); err != nil {
		t.Fatalf("commit ka: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, err := c.GetOrGenerate(ctx, kb, PriorityCurrent, func(context.Context) (tts.Audio, error) { return toneAudioAt(550), nil }); err != nil {
		t.Fatalf("commit kb: %v", err)
	}
	sizeOf := func(k CacheKey) int64 {
		e, ok := idx.Lookup(k)
		if !ok {
			t.Fatalf("entry %v missing before prune", k)
		}
		return e.Size
	}
	unreachableSize := sizeOf(kb)

	keep := func(k CacheKey) bool { return k == ka }
	entries, freed, err := c.PruneUnreachable(ctx, keep, true)
	if err != nil {
		t.Fatalf("PruneUnreachable(dryRun): %v", err)
	}
	if entries != 1 || freed != unreachableSize {
		t.Fatalf("dry run = %d entries / %d bytes, want 1 / %d", entries, freed, unreachableSize)
	}
	if _, ok := idx.Lookup(ka); !ok {
		t.Fatal("the dry run dropped the reachable entry: a dry run must delete nothing")
	}
	if _, ok := idx.Lookup(kb); !ok {
		t.Fatal("the dry run dropped the unreachable entry: a dry run must delete nothing")
	}
	if got := c.Stats().RefusedDeletes; got != 0 {
		t.Fatalf("RefusedDeletes = %d, want 0 (a dry run must touch nothing)", got)
	}
	r, ok, err := c.Get(ctx, ka, false)
	if !ok || err != nil || !r.FromCache {
		t.Fatalf("the dry run broke the kept entry: ok=%v err=%v", ok, err)
	}
	if n := countBlobs(t, inner); n != 2 {
		t.Fatalf("the dry run deleted blobs: %d blobs remain, want 2", n)
	}
}

// TestPruneUnreachableDropsUnreachableSharedBlobKeepsBlob is the ADR-0041
// half of the prune: an entry the keep set rejects may be dropped (its entry
// goes), but a blob a catalogue row still names — the approved voice reference
// the old slot-linked keys pointed at — keeps its bytes and its size is not
// counted as freed.
func TestPruneUnreachableDropsUnreachableSharedBlobKeepsBlob(t *testing.T) {
	ctx := context.Background()
	inner, err := assets.NewLocal(t.TempDir() + "/assets")
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	idx := NewMemIndex()
	shared := ""
	c := New(inner, idx, 1, WithReferenceChecker(refCheckerStub{
		referenced: func(h string) (bool, error) { return shared != "" && h == shared, nil },
	}))

	k := testKey(t, "voice-ref")
	if _, err := c.GetOrGenerate(ctx, k, PriorityCurrent, func(context.Context) (tts.Audio, error) { return toneAudio(), nil }); err != nil {
		t.Fatalf("commit: %v", err)
	}
	e, ok := idx.Lookup(k)
	if !ok {
		t.Fatal("the just-committed entry is not in the index")
	}
	shared = e.SHA256

	// The keep set rejects the entry (nothing published may speak it), so the
	// prune drops it — and must keep the shared blob the catalogue names.
	entries, freed, err := c.PruneUnreachable(ctx, func(CacheKey) bool { return false }, false)
	if err != nil {
		t.Fatalf("PruneUnreachable: %v", err)
	}
	if entries != 1 || freed != 0 {
		t.Fatalf("PruneUnreachable = %d entries / %d bytes, want 1 / 0 (a shared blob is not freed)", entries, freed)
	}
	if _, ok := idx.Lookup(k); ok {
		t.Fatal("the unreachable entry survived the prune")
	}
	if err := inner.Verify(ctx, shared); err != nil {
		t.Fatalf("the shared blob no longer verifies after the prune: %v (catalogue content destroyed)", err)
	}
	if got := c.Stats().RefusedDeletes; got != 1 {
		t.Fatalf("RefusedDeletes = %d, want 1 (the kept-blob refusal is a counted decision)", got)
	}

	t.Logf("prune: dropped the unreachable entry, kept the still-referenced blob %s… intact", shared[:12])
}
