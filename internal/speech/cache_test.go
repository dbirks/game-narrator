package speech

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/providers/tts"
)

func newTestCache(t *testing.T, maxGen int) (*Cache, *assets.Local, *MemIndex) {
	t.Helper()
	store, err := assets.NewLocal(t.TempDir() + "/assets")
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	idx := NewMemIndex()
	return New(store, idx, maxGen), store, idx
}

func toneAudio() tts.Audio {
	return tts.Audio{WAV: MakeToneWAV(440, 0.3, 22050)}
}

func silentAudio() tts.Audio {
	// All-zero samples: ValidateAudio rejects (silence check).
	return tts.Audio{WAV: EncodeWAV(make([]byte, 22050*2), 22050, 1, 16)}
}

func testKey(t *testing.T, id string) CacheKey {
	t.Helper()
	k, err := CacheKeyOf(KeyInput{
		UtteranceText: "line " + id, OriginalMessageID: "m-" + id,
		SpeechRulesRevision: 1, Language: "en", VoiceRevisionFingerprint: "v1",
		TTSModelID: "m@1", BackendConfigDigest: "d1", Format: "wav/pcm16",
	})
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	return k
}

// TestCacheSingleflightFiftyConcurrent: N concurrent callers for one key →
// exactly ONE generation (docs/spec/07 "Avoid duplicate synthesis via
// singleflight per cache key").
func TestCacheSingleflightFiftyConcurrent(t *testing.T) {
	c, _, _ := newTestCache(t, 2)
	ctx := context.Background()
	key := testKey(t, "sf")

	var gens atomic.Int64
	gen := func(context.Context) (tts.Audio, error) {
		gens.Add(1)
		time.Sleep(150 * time.Millisecond) // widen the race window
		return toneAudio(), nil
	}

	const n = 50
	results := make([]Result, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = c.GetOrGenerate(ctx, key, PriorityCurrent, gen)
		}(i)
	}
	close(start)
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if results[i].SHA256 == "" || len(results[i].WAV) == 0 {
			t.Fatalf("caller %d: empty result", i)
		}
	}
	if got := gens.Load(); got != 1 {
		t.Fatalf("generation ran %d times, want exactly 1 (singleflight)", got)
	}
	for i := 1; i < n; i++ {
		if results[i].SHA256 != results[0].SHA256 {
			t.Fatalf("caller %d got a different asset than caller 0", i)
		}
	}

	// A subsequent call must be a cache hit and must not generate again.
	before := gens.Load()
	r, ok, err := c.Get(ctx, key, false)
	if err != nil || !ok {
		t.Fatalf("Get after generation: ok=%v err=%v", ok, err)
	}
	if !r.FromCache || r.SHA256 != results[0].SHA256 {
		t.Fatalf("second Get is not a cache hit: %+v", r)
	}
	if gens.Load() != before {
		t.Fatal("cache hit generated audio again")
	}
}

// TestCacheQAFailedSkippedUnlessOverride (docs/spec/07 "Skip QC-failed cache
// entries by default unless explicit user override").
func TestCacheQAFailedSkippedUnlessOverride(t *testing.T) {
	c, _, _ := newTestCache(t, 1)
	ctx := context.Background()
	key := testKey(t, "qa")

	if _, err := c.GetOrGenerate(ctx, key, PriorityCurrent, func(context.Context) (tts.Audio, error) {
		return toneAudio(), nil
	}); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if err := c.MarkQA(key, QAFailed); err != nil {
		t.Fatalf("MarkQA: %v", err)
	}

	if _, ok, err := c.Get(ctx, key, false); ok || !errors.Is(err, ErrQAFailed) {
		t.Fatalf("QA-failed entry must be skipped by default (ok=%v err=%v)", ok, err)
	}
	if _, err := c.GetOrGenerate(ctx, key, PriorityCurrent, func(context.Context) (tts.Audio, error) {
		t.Fatal("must not regenerate over a QA-failed entry")
		return toneAudio(), nil
	}); !errors.Is(err, ErrQAFailed) {
		t.Fatalf("GetOrGenerate must refuse a QA-failed entry: %v", err)
	}

	r, ok, err := c.Get(ctx, key, true)
	if err != nil || !ok {
		t.Fatalf("override Get: ok=%v err=%v", ok, err)
	}
	if r.QA != QAFailed {
		t.Fatalf("override must still report the failed QA state (got %v), the override is reviewable, not silent", r.QA)
	}
}

// TestCachePriorityOrderUnderSemaphoreOne: with one generation slot, a
// background caller holding the slot must hand it to the CURRENT waiter
// before the NEXT waiter (docs/spec/07 "Prioritize current committed >
// likely next > batch background").
func TestCachePriorityOrderUnderSemaphoreOne(t *testing.T) {
	c, _, _ := newTestCache(t, 1)
	ctx := context.Background()

	starts := make(chan string, 3)
	bgHolding := make(chan struct{})
	startedGen := func(tag string, atStart ...func()) GenerationFunc {
		return func(ctx context.Context) (tts.Audio, error) {
			starts <- tag
			for _, f := range atStart {
				f()
			}
			time.Sleep(120 * time.Millisecond)
			return toneAudio(), nil
		}
	}

	// Background takes the single slot first.
	bgCtx, bgCancel := context.WithCancel(ctx)
	bgDone := make(chan error, 1)
	go func() {
		_, err := c.GetOrGenerate(bgCtx, testKey(t, "bg"), PriorityBackground, startedGen("bg", func() { close(bgHolding) }))
		bgDone <- err
	}()

	// Wait until background actually holds the slot, then enqueue next+current.
	<-bgHolding
	time.Sleep(30 * time.Millisecond) // ensure both queue while bg runs
	nextDone := make(chan error, 1)
	go func() {
		_, err := c.GetOrGenerate(ctx, testKey(t, "next"), PriorityNext, startedGen("next"))
		nextDone <- err
	}()
	time.Sleep(20 * time.Millisecond) // ensure NEXT is queued before CURRENT arrives
	curDone := make(chan error, 1)
	go func() {
		_, err := c.GetOrGenerate(ctx, testKey(t, "cur"), PriorityCurrent, startedGen("cur"))
		curDone <- err
	}()

	if err := <-bgDone; err != nil {
		t.Fatalf("background gen: %v", err)
	}
	if err := <-curDone; err != nil {
		t.Fatalf("current gen: %v", err)
	}
	if err := <-nextDone; err != nil {
		t.Fatalf("next gen: %v", err)
	}
	bgCancel()

	var got []string
	for i := 0; i < 3; i++ {
		got = append(got, <-starts)
	}
	want := []string{"bg", "cur", "next"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("admission order = %v, want %v (Current > Next > Background)", got, want)
		}
	}
}

// TestCacheInvalidAudioNotCached: generation returning audio that fails
// tts.ValidateAudio is never committed (quarantine, docs/spec/07), and the
// next call retries generation instead of serving a poisoned entry.
func TestCacheInvalidAudioNotCached(t *testing.T) {
	c, _, _ := newTestCache(t, 1)
	ctx := context.Background()
	key := testKey(t, "bad")

	var gens atomic.Int64
	gen := func(context.Context) (tts.Audio, error) {
		gens.Add(1)
		return silentAudio(), nil
	}
	if _, err := c.GetOrGenerate(ctx, key, PriorityCurrent, gen); err == nil {
		t.Fatal("silent audio must fail validation, not be cached")
	}
	if _, ok, _ := c.Get(ctx, key, false); ok {
		t.Fatal("invalid audio was indexed")
	}
	// The store must hold nothing: re-generate with good audio and the bytes
	// must be the good ones.
	if _, err := c.GetOrGenerate(ctx, key, PriorityCurrent, func(context.Context) (tts.Audio, error) {
		gens.Add(1)
		return toneAudio(), nil
	}); err != nil {
		t.Fatalf("retry generate: %v", err)
	}
	if gens.Load() != 2 {
		t.Fatalf("generation count = %d, want 2 (failed attempt retried, not served)", gens.Load())
	}
}

// TestCacheRestartPersistsEntry: an index snapshot from one process, fed to a
// new cache, resolves the same key WITHOUT regenerating (AGENTS.md restart
// tests: persistent cache across restarts).
func TestCacheRestartPersistsEntry(t *testing.T) {
	c1, store, idx1 := newTestCache(t, 1)
	ctx := context.Background()
	key := testKey(t, "restart")
	r1, err := c1.GetOrGenerate(ctx, key, PriorityCurrent, func(context.Context) (tts.Audio, error) {
		return toneAudio(), nil
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	// Simulate the restart: fresh index preloaded with the old snapshot.
	idx2 := NewMemIndex()
	for k, e := range idx1.Snapshot() {
		if err := idx2.Put(k, e); err != nil {
			t.Fatalf("preload: %v", err)
		}
	}
	c2 := New(store, idx2, 1)
	var gens atomic.Int64
	r2, err := c2.GetOrGenerate(ctx, key, PriorityCurrent, func(context.Context) (tts.Audio, error) {
		gens.Add(1)
		return toneAudio(), nil
	})
	if err != nil {
		t.Fatalf("post-restart resolve: %v", err)
	}
	if gens.Load() != 0 {
		t.Fatalf("restart re-generated audio: the cache is not persistent")
	}
	if !r2.FromCache || r2.SHA256 != r1.SHA256 {
		t.Fatalf("post-restart result differs: %+v vs %+v", r2, r1)
	}
}

// TestCacheStatsCountDecisions: hits/misses/generation counters tell the
// story the acceptance report needs.
func TestCacheStatsCountDecisions(t *testing.T) {
	c, _, _ := newTestCache(t, 1)
	ctx := context.Background()
	key := testKey(t, "stats")
	if _, err := c.GetOrGenerate(ctx, key, PriorityCurrent, func(context.Context) (tts.Audio, error) {
		return toneAudio(), nil
	}); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if _, err := c.GetOrGenerate(ctx, key, PriorityCurrent, func(context.Context) (tts.Audio, error) {
		t.Fatal("must not generate on hit")
		return tts.Audio{}, fmt.Errorf("unreachable")
	}); err != nil {
		t.Fatalf("hit: %v", err)
	}
	st := c.Stats()
	if st.Misses != 1 || st.Generated != 1 || st.Hits < 1 {
		t.Fatalf("stats = %+v, want 1 miss, 1 generation, ≥1 hit", st)
	}
}
