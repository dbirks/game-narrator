package speech

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/providers/tts"
)

// This file proves the storage rows of docs/spec/13's resilience matrix that
// the speech cache sits in front of:
//
//   - "Disk full": a write that cannot be staged fails with a TYPED error, no
//     partial entry is indexed, and EvictTransient (LRU, transient entries
//     only) reclaims enough space for new writes to succeed again.
//   - "Asset store offline": reads of already-cached entries degrade with a
//     typed error, new writes are refused (the cache has no durable queue),
//     and recovery is transparent when the store comes back.

// ── fault-injecting stores ──────────────────────────────────────────────────

// budgetedReader fails with ENOSPC once budget bytes have been served — the
// store-side shape of a disk that ran out of space mid-stream. The underlying
// Local store performs a REAL partial staging write and cleans it up, so the
// failure path is the one a real full disk exercises.
type budgetedReader struct {
	r      io.Reader
	budget int64
	served int64
}

func (b *budgetedReader) Read(p []byte) (int, error) {
	if b.served >= b.budget {
		return 0, syscall.ENOSPC
	}
	n, err := b.r.Read(p)
	b.served += int64(n)
	return n, err
}

// capacityStore wraps a real assets.Local and accounts committed blob bytes
// against a fixed disk capacity. When a new staged write would push the disk
// past capacity, the stream fails with ENOSPC mid-write (exactly where a real
// full filesystem stops), so the store's own cleanup runs. This is the
// cache-level model of "disk full": steady-state usage is the committed blobs,
// and staging transiently adds to it.
type capacityStore struct {
	inner *assets.Local

	mu       sync.Mutex
	capacity int64
	used     int64
}

func (c *capacityStore) SetCapacity(n int64) {
	c.mu.Lock()
	c.capacity = n
	c.mu.Unlock()
}

func (c *capacityStore) PutStaged(ctx context.Context, r io.Reader, expectedSHA256 string) (assets.StagingID, assets.Info, error) {
	c.mu.Lock()
	budget := c.capacity - c.used
	if budget < 0 {
		budget = 0
	}
	c.mu.Unlock()
	id, info, err := c.inner.PutStaged(ctx, &budgetedReader{r: r, budget: budget}, expectedSHA256)
	if err != nil {
		if errors.Is(err, syscall.ENOSPC) {
			return "", assets.Info{}, fmt.Errorf("assets: disk full: %w", err)
		}
		return "", assets.Info{}, err
	}
	c.mu.Lock()
	c.used += info.Size
	c.mu.Unlock()
	return id, info, nil
}

func (c *capacityStore) Commit(ctx context.Context, id assets.StagingID) (assets.Info, error) {
	return c.inner.Commit(ctx, id)
}

func (c *capacityStore) Abort(ctx context.Context, id assets.StagingID) error {
	return c.inner.Abort(ctx, id)
}

func (c *capacityStore) Delete(ctx context.Context, hash string) error {
	info, err := c.inner.Stat(ctx, hash)
	if err != nil {
		return err
	}
	if err := c.inner.Delete(ctx, hash); err != nil {
		return err
	}
	c.mu.Lock()
	if c.used >= info.Size {
		c.used -= info.Size
	}
	c.mu.Unlock()
	return nil
}

// The rest of the Store interface is a plain delegation.

func (c *capacityStore) Get(ctx context.Context, hash string) (io.ReadCloser, assets.Info, error) {
	return c.inner.Get(ctx, hash)
}
func (c *capacityStore) Stat(ctx context.Context, hash string) (assets.Info, error) {
	return c.inner.Stat(ctx, hash)
}
func (c *capacityStore) Verify(ctx context.Context, hash string) error {
	return c.inner.Verify(ctx, hash)
}
func (c *capacityStore) Export(ctx context.Context, refs []string, w io.Writer) error {
	return c.inner.Export(ctx, refs, w)
}
func (c *capacityStore) DeleteUnreachable(ctx context.Context, reachable func(hash string) bool, dryRun bool) ([]string, error) {
	return c.inner.DeleteUnreachable(ctx, reachable, dryRun)
}
func (c *capacityStore) RecoverStaging(ctx context.Context) (int, error) {
	return c.inner.RecoverStaging(ctx)
}

// offlineStore refuses every operation with a typed sentinel while offline and
// delegates transparently while online — the asset-store side of the "Asset
// store offline" matrix row (the store vanishes; nothing else changes).
type offlineStore struct {
	inner   assets.Store
	offline atomic.Bool
}

var errStoreOffline = errors.New("assets: store offline")

func (o *offlineStore) gate() error {
	if o.offline.Load() {
		return errStoreOffline
	}
	return nil
}

func (o *offlineStore) PutStaged(ctx context.Context, r io.Reader, expectedSHA256 string) (assets.StagingID, assets.Info, error) {
	if err := o.gate(); err != nil {
		return "", assets.Info{}, err
	}
	return o.inner.PutStaged(ctx, r, expectedSHA256)
}
func (o *offlineStore) Commit(ctx context.Context, id assets.StagingID) (assets.Info, error) {
	if err := o.gate(); err != nil {
		return assets.Info{}, err
	}
	return o.inner.Commit(ctx, id)
}
func (o *offlineStore) Abort(ctx context.Context, id assets.StagingID) error {
	if err := o.gate(); err != nil {
		return err
	}
	return o.inner.Abort(ctx, id)
}
func (o *offlineStore) Get(ctx context.Context, hash string) (io.ReadCloser, assets.Info, error) {
	if err := o.gate(); err != nil {
		return nil, assets.Info{}, err
	}
	return o.inner.Get(ctx, hash)
}
func (o *offlineStore) Stat(ctx context.Context, hash string) (assets.Info, error) {
	if err := o.gate(); err != nil {
		return assets.Info{}, err
	}
	return o.inner.Stat(ctx, hash)
}
func (o *offlineStore) Verify(ctx context.Context, hash string) error {
	if err := o.gate(); err != nil {
		return err
	}
	return o.inner.Verify(ctx, hash)
}
func (o *offlineStore) Delete(ctx context.Context, hash string) error {
	if err := o.gate(); err != nil {
		return err
	}
	return o.inner.Delete(ctx, hash)
}
func (o *offlineStore) Export(ctx context.Context, refs []string, w io.Writer) error {
	if err := o.gate(); err != nil {
		return err
	}
	return o.inner.Export(ctx, refs, w)
}
func (o *offlineStore) DeleteUnreachable(ctx context.Context, reachable func(hash string) bool, dryRun bool) ([]string, error) {
	if err := o.gate(); err != nil {
		return nil, err
	}
	return o.inner.DeleteUnreachable(ctx, reachable, dryRun)
}
func (o *offlineStore) RecoverStaging(ctx context.Context) (int, error) {
	if err := o.gate(); err != nil {
		return 0, err
	}
	return o.inner.RecoverStaging(ctx)
}

// ── disk full: refuse, then recover via LRU eviction ───────────────────────

// refCheckerStub is a test stand-in for assets.ReferenceChecker: the real
// implementation is store.Store.AssetReferenced (one UNION over the
// catalogue's referencing tables, covered by internal/store's tests); this
// stub fixes the catalogue's answer so the cache's gating logic — referenced
// keeps the blob, error keeps the blob, no checker deletes nothing — is
// proven on its own. It is a stub, not a replay of the real catalogue.
// (AGENTS.md rule 3: say so, and say where the real one is tested.)
type refCheckerStub struct {
	referenced func(hash string) (bool, error)
}

func (r refCheckerStub) AssetReferenced(_ context.Context, sha256 string) (bool, error) {
	return r.referenced(sha256)
}

// unsharedChecker is a refCheckerStub answering "nothing else names this
// blob" — the catalogue's answer for a blob only the speech cache wrote.
func unsharedChecker() refCheckerStub {
	return refCheckerStub{referenced: func(string) (bool, error) { return false, nil }}
}

// TestDiskFullCacheRefusesThenEvictionRecovers is the speech half of the
// "Disk full" row: "refuse new assets, evict safe transient cache". Three
// entries are committed; the fourth cannot fit. It must fail with a typed
// error, leave NO partial entry behind, and the LRU eviction of the oldest
// transient entries must free enough space that the new write then succeeds.
func TestDiskFullCacheRefusesThenEvictionRecovers(t *testing.T) {
	ctx := context.Background()
	inner, err := assets.NewLocal(t.TempDir() + "/assets")
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	store := &capacityStore{inner: inner}
	idx := NewMemIndex()
	// A checker that answers "unshared" (stub; the real catalogue query lives
	// in internal/store): without one, eviction is index-only and frees no
	// bytes, which is exactly what the disk-full recovery needs to avoid.
	c := New(store, idx, 2, WithReferenceChecker(unsharedChecker()))

	// One utterance size for every key (different frequencies, same length):
	// the capacity math below is exact.
	tone := func(hz float64) tts.Audio { return tts.Audio{WAV: MakeToneWAV(hz, 0.3, 22050)} }
	size := int64(len(tone(440).WAV))
	if size <= 0 {
		t.Fatal("empty fixture audio")
	}
	// 3.5 entries of headroom: the third commit fits, the fourth cannot.
	store.SetCapacity(7 * size / 2)

	genFor := func(hz float64) GenerationFunc {
		return func(context.Context) (tts.Audio, error) { return tone(hz), nil }
	}
	ka, kb, kc := testKey(t, "a"), testKey(t, "b"), testKey(t, "c")
	for i, k := range []struct {
		key CacheKey
		hz  float64
	}{{ka, 440}, {kb, 550}, {kc, 660}} {
		if _, err := c.GetOrGenerate(ctx, k.key, PriorityCurrent, genFor(k.hz)); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
		time.Sleep(2 * time.Millisecond) // make the LRU order unambiguous
	}

	// The fourth utterance does not fit: typed refusal, nothing partial.
	kd := testKey(t, "d")
	_, err = c.GetOrGenerate(ctx, kd, PriorityCurrent, genFor(880))
	if err == nil {
		t.Fatal("GetOrGenerate on a full disk succeeded; want a typed capacity error")
	}
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("full-disk failure is not typed: err = %v, want an error wrapping ENOSPC", err)
	}
	if _, ok := idx.Lookup(kd); ok {
		t.Fatal("a failed generation left an index entry behind: a partial cache entry became visible")
	}
	// The store side agrees: exactly the three committed blobs exist, nothing
	// partial under a new name.
	if n := countBlobs(t, inner); n != 3 {
		t.Fatalf("store has %d blobs after a refused write, want 3 (no partial asset visible)", n)
	}
	if got := idxSnapshotKeys(t, idx); len(got) != 3 {
		t.Fatalf("index has %d entries after a refused write, want 3 (no partial entry)", len(got))
	}

	// Evict the two oldest transient entries: LRU order must be a, b.
	evicted, freed, err := c.EvictTransient(ctx, 2)
	if err != nil {
		t.Fatalf("EvictTransient: %v", err)
	}
	if evicted != 2 || freed != 2*size {
		t.Fatalf("EvictTransient evicted %d entries / %d bytes, want 2 / %d", evicted, freed, 2*size)
	}
	if _, ok := idx.Lookup(ka); ok {
		t.Fatal("LRU eviction did not drop the oldest entry (a)")
	}
	if _, ok := idx.Lookup(kb); ok {
		t.Fatal("LRU eviction did not drop the second-oldest entry (b)")
	}
	if _, ok := idx.Lookup(kc); !ok {
		t.Fatal("LRU eviction dropped a still-needed entry (c) before the oldest")
	}
	if r, ok, err := c.Get(ctx, kc, false); !ok || err != nil || !r.FromCache {
		t.Fatalf("survivor entry (c) is no longer servable: ok=%v err=%v", ok, err)
	}

	// The write that was refused now fits: eviction actually reclaimed space.
	if _, err := c.GetOrGenerate(ctx, kd, PriorityCurrent, genFor(880)); err != nil {
		t.Fatalf("write still refused after eviction: %v", err)
	}
	if _, ok := idx.Lookup(kd); !ok {
		t.Fatal("recovered write was not indexed")
	}
	if _, err := c.GetOrGenerate(ctx, kc, PriorityCurrent, genFor(660)); err != nil {
		// A hit: no regeneration, served from the store.
		t.Fatalf("survivor entry regenerated after eviction: %v", err)
	}

	t.Logf("resilience: disk_full speech cache: refused write is typed (ENOSPC), no partial entry, LRU eviction of %d transient entries reclaimed %d bytes, new write recovered", evicted, freed)
}

// TestEvictTransientNeverTouchesForeignBlobs is the safety half of the same
// row: eviction may only reclaim the cache's own blobs. A blob that the cache
// never wrote (an approved voice reference or imported source, committed
// directly by the catalogue) must survive a full eviction untouched — while
// the cache's OWN unshared blobs are actually deleted, which is what makes
// the eviction reclaim space at all.
func TestEvictTransientNeverTouchesForeignBlobs(t *testing.T) {
	ctx := context.Background()
	inner, err := assets.NewLocal(t.TempDir() + "/assets")
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	// A foreign blob: committed straight into the store, never through the
	// cache — the shape of an approved voice reference.
	foreign, ferr := commitBlob(t, inner, []byte("approved voice reference: never evicted"))
	if ferr != nil {
		t.Fatalf("foreign blob: %v", ferr)
	}

	store := &capacityStore{inner: inner}
	store.SetCapacity(1 << 20)
	idx := NewMemIndex()
	c := New(store, idx, 1, WithReferenceChecker(unsharedChecker()))
	ka, kb := testKey(t, "x"), testKey(t, "y")
	if _, err := c.GetOrGenerate(ctx, ka, PriorityCurrent, func(context.Context) (tts.Audio, error) { return toneAudio(), nil }); err != nil {
		t.Fatalf("commit a: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, err := c.GetOrGenerate(ctx, kb, PriorityCurrent, func(context.Context) (tts.Audio, error) { return toneAudio(), nil }); err != nil {
		t.Fatalf("commit b: %v", err)
	}

	evicted, _, err := c.EvictTransient(ctx, 0) // all of the cache's entries
	if err != nil {
		t.Fatalf("EvictTransient(all): %v", err)
	}
	if evicted != 2 {
		t.Fatalf("evicted %d, want 2 (the cache's own entries only)", evicted)
	}
	if err := inner.Verify(ctx, foreign.SHA256); err != nil {
		t.Fatalf("eviction touched a foreign blob: %v", err)
	}
	if _, ok := idx.Lookup(ka); ok {
		t.Fatal("eviction left an index entry behind")
	}
	// The cache's OWN unshared blobs were deleted (that is what reclaims
	// space); the store holds exactly the foreign blob now.
	if n := countBlobs(t, inner); n != 1 {
		t.Fatalf("store has %d blobs after eviction, want 1 (only the foreign blob)", n)
	}

	t.Logf("resilience: disk_full speech cache safety: full eviction removed %d transient entries and their unshared blobs, foreign blob %s… intact", evicted, foreign.SHA256[:12])
}

// ── asset store offline ─────────────────────────────────────────────────────

// TestAssetsOfflineDegradesAndRecovers is the "Asset store offline" row: while
// the store is gone, reads of already-cached entries degrade with a typed
// error (no panic, no stale bytes served from a dead store), new writes are
// refused — the cache has no durable queue, so nothing is silently enqueued —
// and when the store returns, everything works again without re-synthesis of
// what was already cached.
func TestAssetsOfflineDegradesAndRecovers(t *testing.T) {
	ctx := context.Background()
	inner, err := assets.NewLocal(t.TempDir() + "/assets")
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	store := &offlineStore{inner: inner}
	idx := NewMemIndex()
	c := New(store, idx, 1)

	ka, kb := testKey(t, "on"), testKey(t, "off")
	// Warm the cache while the store is up.
	r, err := c.GetOrGenerate(ctx, ka, PriorityCurrent, genOnce(toneAudio()))
	if err != nil {
		t.Fatalf("warm: %v", err)
	}
	if r.FromCache {
		t.Fatal("first generation reported FromCache")
	}

	store.offline.Store(true) // the store drops

	// Reading a cached entry degrades with a typed error.
	if _, ok, err := c.Get(ctx, ka, false); !errors.Is(err, errStoreOffline) {
		t.Fatalf("Get while offline: ok=%v err=%v, want the typed offline error", ok, err)
	}
	// A new write is refused, not queued: the error surfaces and no partial
	// state is recorded.
	if _, err := c.GetOrGenerate(ctx, kb, PriorityCurrent, genOnce(toneAudio())); !errors.Is(err, errStoreOffline) {
		t.Fatalf("GetOrGenerate while offline: err = %v, want the typed offline error (refusal, not a queue)", err)
	}
	if _, ok := idx.Lookup(kb); ok {
		t.Fatal("a refused write left an index entry behind")
	}

	// Recovery: the store comes back; the warm entry is served from the cache
	// again (no regeneration), and new writes commit.
	store.offline.Store(false)
	r, ok, err := c.Get(ctx, ka, false)
	if err != nil || !ok || !r.FromCache {
		t.Fatalf("Get after recovery: ok=%v err=%v, want a cache hit", ok, err)
	}
	if _, err := c.GetOrGenerate(ctx, kb, PriorityCurrent, genOnce(toneAudio())); err != nil {
		t.Fatalf("write after recovery: %v", err)
	}
	if _, ok := idx.Lookup(kb); !ok {
		t.Fatal("recovered write was not indexed")
	}

	t.Logf("resilience: offline assets: cached reads degraded with typed error, new writes refused (no durable queue — documented), transparent recovery after reconnect")
}

// ── small helpers ───────────────────────────────────────────────────────────

// genOnce is a GenerationFunc with a fixed audio and a call counter so a test
// can prove a generation did (or did not) run.
func genOnce(a tts.Audio) GenerationFunc {
	var calls atomic.Int64
	f := func(context.Context) (tts.Audio, error) {
		calls.Add(1)
		return a, nil
	}
	return f
}

// commitBlob stages and commits content directly into a local store (outside
// the cache), returning the committed info.
func commitBlob(t *testing.T, l *assets.Local, content []byte) (assets.Info, error) {
	t.Helper()
	id, _, err := l.PutStaged(context.Background(), bytes.NewReader(content), "")
	if err != nil {
		return assets.Info{}, err
	}
	return l.Commit(context.Background(), id)
}

// countBlobs counts committed blobs in a local store (test-only): a dry-run
// GC with an all-unreachable predicate reports exactly the blobs present.
func countBlobs(t *testing.T, l *assets.Local) int {
	t.Helper()
	all, err := l.DeleteUnreachable(context.Background(), func(string) bool { return false }, true)
	if err != nil {
		t.Fatalf("count blobs (dry run): %v", err)
	}
	return len(all)
}

// idxSnapshotKeys lists the current index keys (test-only).
func idxSnapshotKeys(t *testing.T, m *MemIndex) []CacheKey {
	t.Helper()
	out := make([]CacheKey, 0, len(m.Snapshot()))
	for k := range m.Snapshot() {
		out = append(out, k)
	}
	return out
}
