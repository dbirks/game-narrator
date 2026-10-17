package speech

// Reference-check-before-delete tests for EvictTransient
// (ADR-0041 "Reference check before delete"). Assets are content-addressed
// and shared, so the blob a cache entry addresses may be the same blob a
// catalogue record (approved voice reference, audition clip, portrait,
// imported source) names. These tests pin the cache's gating logic with a
// refCheckerStub (see resilience_test.go); the real catalogue query
// (store.AssetReferenced, one UNION over every referencing table) is proven
// in internal/store's tests — together they cover the whole path.

import (
	"context"
	"errors"
	"testing"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/providers/tts"
)

var errCheckerDown = errors.New("store: reference check unavailable")

// TestEvictTransientKeepsBlobSharedWithVoiceReference is the review finding
// itself: a voice reference row whose sha256 EQUALS the cache entry's blob.
// After eviction the cache entry is gone (evicted) but the blob must still
// verify byte-for-byte, and its size must NOT be counted as freed.
func TestEvictTransientKeepsBlobSharedWithVoiceReference(t *testing.T) {
	ctx := context.Background()
	inner, err := assets.NewLocal(t.TempDir() + "/assets")
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	idx := NewMemIndex()
	// The checker reads `shared` at call time: the entry is generated first
	// (so its blob is a genuinely cache-committed one), and only then does the
	// "voice reference row" start naming that exact hash.
	shared := ""
	c := New(inner, idx, 1, WithReferenceChecker(refCheckerStub{
		referenced: func(h string) (bool, error) { return shared != "" && h == shared, nil },
	}))

	ka := testKey(t, "shared")
	if _, err := c.GetOrGenerate(ctx, ka, PriorityCurrent, func(context.Context) (tts.Audio, error) { return toneAudio(), nil }); err != nil {
		t.Fatalf("commit: %v", err)
	}
	e, ok := idx.Lookup(ka)
	if !ok {
		t.Fatal("the just-committed entry is not in the index")
	}
	shared = e.SHA256

	evicted, freed, err := c.EvictTransient(ctx, 0) // all entries
	if err != nil {
		t.Fatalf("EvictTransient: %v", err)
	}
	if evicted != 1 {
		t.Fatalf("evicted = %d, want 1 (the entry IS evicted even when the blob is kept)", evicted)
	}
	if freed != 0 {
		t.Fatalf("freed = %d, want 0 (a shared blob's bytes were not reclaimed)", freed)
	}
	if _, ok := idx.Lookup(ka); ok {
		t.Fatal("eviction left the index entry behind")
	}
	// The catalogue's copy of the bytes must still verify — a re-read proves
	// the blob was not merely unlinked in error.
	if err := inner.Verify(ctx, shared); err != nil {
		t.Fatalf("shared blob no longer verifies after eviction: %v (catalogue content destroyed)", err)
	}
	if got := c.Stats().RefusedDeletes; got != 1 {
		t.Fatalf("RefusedDeletes = %d, want 1 (the refusal is a decision the report can see)", got)
	}

	t.Logf("resilience: eviction reference check: entry evicted, shared blob %s… still verifies, freed 0 bytes", shared[:12])
}

// TestEvictTransientWithoutCheckerIsIndexOnly is the safe-by-default half:
// with no checker configured the cache cannot prove a blob unshared, so it
// must never delete one — the index entries go, the bytes stay, and every
// skipped deletion is counted as a refusal.
func TestEvictTransientWithoutCheckerIsIndexOnly(t *testing.T) {
	ctx := context.Background()
	inner, err := assets.NewLocal(t.TempDir() + "/assets")
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	idx := NewMemIndex()
	c := New(inner, idx, 1) // no WithReferenceChecker

	hashes := []string{}
	for i, id := range []string{"n1", "n2"} {
		k := testKey(t, id)
		if _, err := c.GetOrGenerate(ctx, k, PriorityCurrent, func(context.Context) (tts.Audio, error) { return toneAudio(), nil }); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
		if e, ok := idx.Lookup(k); !ok {
			t.Fatalf("entry %d missing", i)
		} else {
			hashes = append(hashes, e.SHA256)
		}
	}

	evicted, freed, err := c.EvictTransient(ctx, 0)
	if err != nil {
		t.Fatalf("EvictTransient: %v", err)
	}
	if evicted != 2 {
		t.Fatalf("evicted = %d, want 2 (index entries are still evicted)", evicted)
	}
	if freed != 0 {
		t.Fatalf("freed = %d, want 0 (without a checker no blob may be deleted)", freed)
	}
	if got := c.Stats().RefusedDeletes; got != 2 {
		t.Fatalf("RefusedDeletes = %d, want 2 (each skipped deletion is counted)", got)
	}
	for _, h := range hashes {
		if err := inner.Verify(ctx, h); err != nil {
			t.Fatalf("blob %s… deleted without a reference checker: %v", h[:12], err)
		}
	}

	t.Logf("resilience: eviction without checker: index-only eviction, both blobs intact, %d refusals counted", evicted)
}

// TestEvictTransientCheckerErrorKeepsBlob is the failure half: a checker that
// errors is not a green light. The entry is evicted, the blob is kept, the
// refusal is counted, and the eviction reports success (the entries WERE
// evicted; the bytes were deliberately not touched).
func TestEvictTransientCheckerErrorKeepsBlob(t *testing.T) {
	ctx := context.Background()
	inner, err := assets.NewLocal(t.TempDir() + "/assets")
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	idx := NewMemIndex()
	c := New(inner, idx, 1, WithReferenceChecker(refCheckerStub{
		referenced: func(string) (bool, error) { return false, errCheckerDown },
	}))

	ka := testKey(t, "err")
	if _, err := c.GetOrGenerate(ctx, ka, PriorityCurrent, func(context.Context) (tts.Audio, error) { return toneAudio(), nil }); err != nil {
		t.Fatalf("commit: %v", err)
	}
	e, _ := idx.Lookup(ka)

	evicted, freed, err := c.EvictTransient(ctx, 1)
	if err != nil {
		t.Fatalf("EvictTransient: %v (a failed check is a refusal, not an error)", err)
	}
	if evicted != 1 || freed != 0 {
		t.Fatalf("evicted=%d freed=%d, want 1/0", evicted, freed)
	}
	if got := c.Stats().RefusedDeletes; got != 1 {
		t.Fatalf("RefusedDeletes = %d, want 1", got)
	}
	if err := inner.Verify(ctx, e.SHA256); err != nil {
		t.Fatalf("blob deleted despite a failed reference check: %v", err)
	}

	t.Logf("resilience: eviction reference check failure: entry evicted, blob %s… kept, refusal counted", e.SHA256[:12])
}
