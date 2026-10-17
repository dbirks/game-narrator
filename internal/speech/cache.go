package speech

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/providers/tts"
)

// Result is one resolved utterance: the playable WAV bytes plus the metadata
// the player, the QA loop and the event log correlate on.
type Result struct {
	WAV        []byte
	SHA256     string
	Duration   time.Duration
	SampleRate int
	QA         QAState
	// FromCache is true when the bytes came from the asset store rather than
	// a fresh provider call (evidence the cache actually worked — docs/spec/11
	// "avoiding false positives": a WAV is not proof of anything by itself).
	FromCache bool
}

// GenerationFunc produces one utterance. It owns the provider call and its
// deadline (docs/spec/01: the router owns cancellation, not the cache); the
// cache only observes the returned Audio and validates it.
type GenerationFunc func(ctx context.Context) (tts.Audio, error)

// lruState is the eviction bookkeeping for one cache key. It is deliberately
// separate from the Index: the index is the durability contract (who owns what
// bytes) and may be persistent; lruState is process-local recency evidence
// only, and losing it on a restart costs nothing — the next run re-learns
// order from LoadAll and from its own accesses.
type lruState struct {
	accessed time.Time
	size     int64 // committed blob size; 0 when unknown (loaded, not generated here)
}

// call is one in-flight generation, shared by every caller that singleflights
// on the same key.
type call struct {
	mu     sync.Mutex
	done   chan struct{}
	result Result
	err    error
	once   sync.Once
}

func (c *call) finish(r Result, err error) {
	c.once.Do(func() {
		c.mu.Lock()
		c.result, c.err = r, err
		c.mu.Unlock()
		close(c.done)
	})
}

// ErrQAFailed reports a cache entry that failed the ASR quality check. Callers
// may retry with an explicit override; the error carries no hidden state.
var ErrQAFailed = errors.New("speech: cache entry failed QA (use override to play anyway)")

// Cache is the singleflight, priority-bounded TTS cache in front of the asset
// store (docs/spec/07 "Cache"). Concurrency model:
//
//   - GetOrGenerate deduplicates per key with singleflight: N concurrent
//     callers for one key produce exactly ONE provider call; the rest wait on
//     the shared result.
//   - Provider calls are additionally bounded by a priority limiter
//     (Current > Next > Background; docs/spec/07 "bounded concurrent
//     generation", "obey inference server load").
//   - Only validated audio (tts.ValidateAudio) is ever committed to the asset
//     store; the commit is atomic (stage → commit), so a crash mid-write
//     leaves no half-entry (docs/spec/02 "Transaction workflows").
//   - QA-failed entries are skipped by default; an explicit override plays
//     them (docs/spec/07 "Skip QC-failed cache entries by default unless
//     explicit user override").
//
// The cache never blocks the session event loop: every entry point takes a
// context and returns; limiter waits are cancellable.
type Cache struct {
	// Assets is the content-addressed store that holds the WAV bytes.
	Assets assets.Store
	// Index is the key→Entry persistence hook (MemIndex for single process).
	Index Index

	mu       sync.Mutex
	inflight map[CacheKey]*call
	lim      *genLimiter

	loadOnce sync.Once
	loadErr  error

	// Stats are decision counters (docs/spec/13: counters a report can act
	// on), not bookkeeping ticks.
	hits      int64
	misses    int64
	qaSkips   int64
	generated int64
	failed    int64

	// LRU evidence for EvictTransient (docs/spec/13 "Disk full": evict safe
	// transient cache). lruMu guards lru; it is a separate lock so eviction
	// never contends with the inflight singleflight map.
	lruMu sync.Mutex
	lru   map[CacheKey]lruState
}

// CacheStats is a snapshot of Cache's decision counters.
type CacheStats struct {
	Hits       int64
	Misses     int64
	QASkips    int64
	Generated  int64
	Failed     int64
	PendingGen int64
}

// New opens a cache. maxConcurrentGenerations bounds simultaneous provider
// calls (≤ 0 means 1). The index is loaded lazily on first use.
func New(Assets assets.Store, Index Index, maxConcurrentGenerations int) *Cache {
	if Assets == nil {
		panic("speech: New requires a non-nil assets.Store")
	}
	if Index == nil {
		panic("speech: New requires a non-nil Index")
	}
	return &Cache{
		Assets:   Assets,
		Index:    Index,
		inflight: map[CacheKey]*call{},
		lim:      newGenLimiter(maxConcurrentGenerations),
		lru:      make(map[CacheKey]lruState),
	}
}

// ensureLoaded runs the index LoadAll exactly once.
func (c *Cache) ensureLoaded(ctx context.Context) error {
	c.loadOnce.Do(func() {
		loaded, err := c.Index.LoadAll(ctx)
		c.loadErr = err
		if err == nil {
			// Seed the LRU with everything a previous run left behind, all at
			// the load instant: entries this process has not yet touched are,
			// by construction, the least recently used, which is exactly the
			// order an eviction must start from.
			now := time.Now()
			c.lruMu.Lock()
			for k := range loaded {
				c.lru[k] = lruState{accessed: now}
			}
			c.lruMu.Unlock()
		}
	})
	return c.loadErr
}

// touchLRU records that key was just served or written. Size is the committed
// blob size when known (generation, or a read) and 0 otherwise; eviction
// re-stats a blob it is about to remove, so an unknown size only costs one
// metadata read, never correctness.
func (c *Cache) touchLRU(key CacheKey, size int64) {
	c.lruMu.Lock()
	defer c.lruMu.Unlock()
	st := c.lru[key]
	st.accessed = time.Now()
	if size > 0 {
		st.size = size
	}
	c.lru[key] = st
}

// Get resolves a cache hit without generating anything. It returns the WAV
// bytes loaded from the asset store plus metadata. ok is false when the key is
// unknown or its entry is QA-failed and override is false (in that case the
// error is ErrQAFailed). A hit on a QA-failed entry WITH override is returned
// with QA still reporting QAFailed, so the event log shows the override
// actually happened (docs/spec/14: reviewable, not silent).
func (c *Cache) Get(ctx context.Context, key CacheKey, override bool) (Result, bool, error) {
	if err := c.ensureLoaded(ctx); err != nil {
		return Result{}, false, err
	}
	e, ok := c.lookup(key)
	if !ok {
		return Result{}, false, nil
	}
	if e.QA == QAFailed && !override {
		c.bump(&c.qaSkips)
		return Result{}, false, ErrQAFailed
	}
	wav, err := c.loadBytes(ctx, e.SHA256)
	if err != nil {
		return Result{}, false, err
	}
	c.bump(&c.hits)
	c.touchLRU(key, int64(len(wav)))
	return Result{
		WAV:        wav,
		SHA256:     e.SHA256,
		Duration:   time.Duration(e.Duration),
		SampleRate: e.SampleRate,
		QA:         e.QA,
		FromCache:  true,
	}, true, nil
}

// GetOrGenerate resolves key, generating (and caching) the audio when it is
// not already usable. prio orders admission into the bounded generation pool.
//
// Semantics:
//   - usable cache entry → returned without any provider call (singleflight is
//     then a no-op for the key);
//   - generation in flight for the key → the shared result is returned (N
//     concurrent callers, 1 generation);
//   - otherwise this caller wins: it acquires the limiter under prio, runs
//     gen, validates the audio, commits it atomically and records the index
//     entry. The winner's ctx bounds the whole shared call; if it is cancelled
//     the call finishes with that error for all waiters, and the (aborted)
//     generation is not cached.
func (c *Cache) GetOrGenerate(ctx context.Context, key CacheKey, prio Priority, gen GenerationFunc) (Result, error) {
	if gen == nil {
		return Result{}, errors.New("speech: GetOrGenerate requires a generation function")
	}
	if err := c.ensureLoaded(ctx); err != nil {
		return Result{}, err
	}
	if r, ok, err := c.Get(ctx, key, false); err != nil {
		// ErrQAFailed propagates on purpose: the cache does not silently
		// regenerate over a failed entry. The QA loop owns that decision
		// (bounded retries with alternative seeds → a NEW key, docs/spec/07
		// "ASR loop"). Callers that want the failed bytes use Get with
		// override; callers that want a fresh try Delete the entry first.
		return Result{}, err
	} else if ok {
		return r, nil
	}

	// Singleflight registration.
	c.mu.Lock()
	if cl, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		// Another caller owns the generation for this key: wait on the
		// shared result (this is the singleflight path — N callers, 1 gen).
		select {
		case <-cl.done:
			cl.mu.Lock()
			r, err := cl.result, cl.err
			cl.mu.Unlock()
			if err == nil {
				c.touchLRU(key, int64(len(r.WAV)))
			}
			return r, err
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}
	cl := &call{done: make(chan struct{})}
	c.inflight[key] = cl
	c.bump(&c.misses)
	c.mu.Unlock()

	// Leader path: own the lifetime of the shared call.
	defer func() {
		c.mu.Lock()
		if c.inflight[key] == cl {
			delete(c.inflight, key)
		}
		c.mu.Unlock()
	}()

	if err := c.lim.acquire(ctx, prio); err != nil {
		cl.finish(Result{}, err)
		return Result{}, err
	}
	defer c.lim.release()

	r, err := c.generate(ctx, key, gen)
	if err != nil {
		c.bump(&c.failed)
		cl.finish(Result{}, err)
		return Result{}, err
	}
	c.bump(&c.generated)
	cl.finish(r, nil)
	return r, nil
}

// generate runs the generation function once, validates, commits and indexes.
func (c *Cache) generate(ctx context.Context, key CacheKey, gen GenerationFunc) (Result, error) {
	audio, err := gen(ctx)
	if err != nil {
		return Result{}, err
	}
	info, err := tts.ValidateAudio(audio.WAV)
	if err != nil {
		return Result{}, fmt.Errorf("speech: refusing to cache unvalidated audio: %w", err)
	}
	// Atomic commit: stage (streaming hash) → commit (visible under hash).
	id, staged, err := c.Assets.PutStaged(ctx, bytes.NewReader(audio.WAV), "")
	if err != nil {
		return Result{}, fmt.Errorf("speech: staging audio: %w", err)
	}
	commit, err := c.Assets.Commit(ctx, id)
	if err != nil {
		_ = c.Assets.Abort(ctx, id)
		return Result{}, fmt.Errorf("speech: committing audio: %w", err)
	}
	entry := Entry{
		SHA256:     commit.SHA256,
		Duration:   info.Duration.Nanoseconds(),
		SampleRate: info.SampleRate,
		QA:         QAUnknown,
	}
	if err := c.Index.Put(key, entry); err != nil {
		return Result{}, fmt.Errorf("speech: recording index entry: %w", err)
	}
	c.touchLRU(key, staged.Size)
	return Result{
		WAV:        audio.WAV,
		SHA256:     commit.SHA256,
		Duration:   info.Duration,
		SampleRate: info.SampleRate,
		QA:         QAUnknown,
		FromCache:  false,
	}, nil
}

// MarkQA records the ASR quality verdict for a key (the ASR loop calls this
// after checking a generated utterance; docs/spec/07 "ASR loop"). Unknown
// keys are a no-op error so a QA result for a GC'd entry does not fail the
// pipeline.
func (c *Cache) MarkQA(key CacheKey, qa QAState) error {
	if err := c.ensureLoaded(context.Background()); err != nil {
		return err
	}
	e, ok := c.lookup(key)
	if !ok {
		return fmt.Errorf("speech: MarkQA: key unknown")
	}
	e.QA = qa
	return c.Index.Put(key, e)
}

// lookup reads the index entry without touching the limiter.
func (c *Cache) lookup(key CacheKey) (Entry, bool) {
	return c.Index.Lookup(key)
}

// loadBytes reads a committed blob back into memory.
func (c *Cache) loadBytes(ctx context.Context, hash string) ([]byte, error) {
	r, _, err := c.Assets.Get(ctx, hash)
	if err != nil {
		return nil, fmt.Errorf("speech: loading cached audio %s: %w", hash, err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("speech: reading cached audio %s: %w", hash, err)
	}
	return b, nil
}

// EvictTransient reclaims disk by evicting up to maxEntries of the cache's
// OWN entries, least recently used first (maxEntries <= 0 evicts all). For
// each evicted key it removes the index entry and the blob it addresses.
//
// This is the "evict safe transient cache" half of docs/spec/13's Disk-full
// row. The safety argument, in one line: the index holds only generated TTS
// utterances, so every key in it is re-synthesizable, and nothing outside this
// index names these blobs. Approved voice references and imported source data
// live in the catalogue's own tables and are never touched — EvictTransient
// cannot see them and Delete reaches only the exact hash of the entry being
// evicted. The documented quota is the caller's: the runtime (or operator)
// decides how many entries to release when a write fails with a capacity
// error; this method is the mechanism, not the policy.
//
// A blob that is already gone (Stat → ErrNotFound) is not a failure: the
// index entry is still removed, the freed count simply does not include it.
// Already-removed keys are idempotent no-ops.
func (c *Cache) EvictTransient(ctx context.Context, maxEntries int) (evicted int, freed int64, err error) {
	if err := c.ensureLoaded(ctx); err != nil {
		return 0, 0, err
	}

	// Snapshot the eviction set under the LRU lock, ordered least-recently
	// used first. Ties (entries loaded in the same instant) break on the key
	// so the order is deterministic across runs — eviction order is evidence,
	// and nondeterministic evidence is hard to audit.
	type victim struct {
		key  CacheKey
		time time.Time
	}
	c.lruMu.Lock()
	victims := make([]victim, 0, len(c.lru))
	for k, st := range c.lru {
		victims = append(victims, victim{key: k, time: st.accessed})
	}
	c.lruMu.Unlock()
	sort.Slice(victims, func(i, j int) bool {
		if !victims[i].time.Equal(victims[j].time) {
			return victims[i].time.Before(victims[j].time)
		}
		return string(victims[i].key) < string(victims[j].key)
	})
	if maxEntries > 0 && len(victims) > maxEntries {
		victims = victims[:maxEntries]
	}

	for _, v := range victims {
		e, ok := c.Index.Lookup(v.key)
		if !ok {
			c.dropLRU(v.key)
			continue // already evicted (by this process or a persistent index GC)
		}
		size := c.lruSize(v.key)
		c.dropLRU(v.key)
		if err := c.Index.Delete(v.key); err != nil {
			return evicted, freed, fmt.Errorf("speech: evict %s: %w", v.key, err)
		}
		evicted++
		if size <= 0 {
			if si, serr := c.Assets.Stat(ctx, e.SHA256); serr == nil {
				size = si.Size
			}
		}
		err := c.Assets.Delete(ctx, e.SHA256)
		if err != nil && !errors.Is(err, assets.ErrNotFound) {
			return evicted, freed, fmt.Errorf("speech: evict %s (blob %s…): %w", v.key, e.SHA256[:12], err)
		}
		freed += size
	}
	return evicted, freed, nil
}

// dropLRU removes one key's LRU state (evicted or gone from the index).
func (c *Cache) dropLRU(key CacheKey) {
	c.lruMu.Lock()
	delete(c.lru, key)
	c.lruMu.Unlock()
}

// lruSize returns the recorded blob size for key; zero when unknown.
func (c *Cache) lruSize(key CacheKey) int64 {
	c.lruMu.Lock()
	defer c.lruMu.Unlock()
	return c.lru[key].size
}

// Stats returns the decision counters plus the limiter's pending count.
func (c *Cache) Stats() CacheStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return CacheStats{
		Hits:       c.hits,
		Misses:     c.misses,
		QASkips:    c.qaSkips,
		Generated:  c.generated,
		Failed:     c.failed,
		PendingGen: int64(c.lim.pending()),
	}
}

// bump increments a counter. Small wrapper so the counters stay lock-free
// int64s without a per-bump mutex (torn reads of a 64-bit counter are the
// only cost, and these are diagnostics, not decisions).
func (c *Cache) bump(p *int64) {
	*p++
}
