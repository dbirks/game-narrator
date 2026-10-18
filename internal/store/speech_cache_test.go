package store

// Tests for the store-backed TTS speech-cache index (migration 0011,
// ADR-0039 amendment): the unit contract (put/lookup/delete/load-once, QA
// state and created-at preservation) and the AGENTS.md restart requirement —
// generate, close the app's store, re-open it, and the cache Get is a hit
// that makes no TTS call.

import (
	"context"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/providers/tts"
	"github.com/dbirks/game-narrator/internal/speech"
)

// speechTestKeyInputs are the cache identities the index tests use (the key
// content does not matter to the index; the table only stores the 64-char
// hex form, and distinct inputs give distinct keys).
var speechTestKeyInputs = []speech.KeyInput{
	{UtteranceText: "the harbour bell rang", Language: "en", Format: "wav/pcm16", TTSModelID: "fake@1"},
	{UtteranceText: "nobody had seen the boat", Language: "en", Format: "wav/pcm16", TTSModelID: "fake@1"},
	{UtteranceText: "the harbour bell rang", Language: "en", Format: "wav/pcm16", TTSModelID: "fake@2"},
}

func speechTestKey(i int) speech.CacheKey {
	k, err := speech.CacheKeyOf(speechTestKeyInputs[i])
	if err != nil {
		panic(err)
	}
	return k
}

func TestSpeechCacheIndexRoundTrip(t *testing.T) {
	s := migratedStore(t)
	ctx := testCtx(t)
	idx := NewSpeechCacheIndex(s)

	entries := []speech.Entry{
		{SHA256: "aa" + tail62('a'), Size: 1024, Duration: 1_000_000, SampleRate: 16000, QA: speech.QAUnknown},
		{SHA256: "bb" + tail62('b'), Size: 2048, Duration: 2_000_000, SampleRate: 22050, QA: speech.QAPassed},
		{SHA256: "cc" + tail62('c'), Size: 4096, Duration: 3_000_000, SampleRate: 16000, QA: speech.QAFailed},
	}
	for i, e := range entries {
		if err := idx.Put(speechTestKey(i), e); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}

	for i, want := range entries {
		got, ok := idx.Lookup(speechTestKey(i))
		if !ok {
			t.Fatalf("Lookup %d: miss after Put", i)
		}
		if got.SHA256 != want.SHA256 || got.Size != want.Size ||
			got.Duration != want.Duration || got.SampleRate != want.SampleRate || got.QA != want.QA {
			t.Errorf("entry %d = %+v, want %+v", i, got, want)
		}
	}
	if _, ok := idx.Lookup(speech.CacheKey("deadbeef")); ok {
		t.Error("Lookup of an unknown key must miss")
	}

	// A second Put of the same key is an upsert: the QA state moves, and a
	// fresh index over the same store sees the same row (the row is in the
	// database, not the process).
	first, ok := idx.Lookup(speechTestKey(0))
	if !ok {
		t.Fatal("Lookup before upsert: miss")
	}
	if err := idx.Put(speechTestKey(0), speech.Entry{
		SHA256: first.SHA256, Size: first.Size, Duration: first.Duration,
		SampleRate: first.SampleRate, QA: speech.QAPassed,
	}); err != nil {
		t.Fatalf("upsert Put: %v", err)
	}
	again, ok := idx.Lookup(speechTestKey(0))
	if !ok || again.QA != speech.QAPassed {
		t.Fatalf("upsert = %+v ok=%v, want QA passed", again, ok)
	}
	fresh := NewSpeechCacheIndex(s)
	loaded, err := fresh.LoadAll(ctx)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(loaded) != 3 {
		t.Fatalf("LoadAll = %d entries, want 3", len(loaded))
	}
	if got := loaded[speechTestKey(0)]; got.QA != speech.QAPassed {
		t.Errorf("loaded QA for key 0 = %v, want passed (the upsert must survive the read)", got.QA)
	}
	// LoadAll is load-once: the second call returns nil (the interface
	// contract; the cache uses Lookup for per-key reads).
	if m, err := fresh.LoadAll(ctx); m != nil || err != nil {
		t.Errorf("second LoadAll = (%v, %v), want (nil, nil)", m, err)
	}

	// Delete is the eviction/withdrawal path: the row goes away, a second
	// delete is a no-op, and the remaining rows are untouched.
	if err := idx.Delete(speechTestKey(1)); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := idx.Lookup(speechTestKey(1)); ok {
		t.Error("Lookup after Delete must miss")
	}
	if err := idx.Delete(speechTestKey(1)); err != nil {
		t.Fatalf("second Delete must be a no-op, got %v", err)
	}
	if n := countRows(t, s.db, "speech_cache"); n != 2 {
		t.Errorf("speech_cache rows = %d, want 2", n)
	}
}

// tail62 pads a two-character prefix to a 64-character hash-shaped string
// (the CHECK is a length check; the app layer validates the hex).
func tail62(c byte) string {
	b := make([]byte, 62)
	for i := range b {
		b[i] = c
	}
	return string(b)
}

func TestSpeechCacheIndexRejectsBadRows(t *testing.T) {
	s := migratedStore(t)
	idx := NewSpeechCacheIndex(s)

	// A non-64-char key cannot be a cache key: the table refuses it, and the
	// index must surface the write error rather than store it.
	if err := idx.Put(speech.CacheKey("too-short"), speech.Entry{
		SHA256: "aa" + tail62('a'), Size: 1, Duration: 1, SampleRate: 1,
	}); err == nil {
		t.Fatal("Put of a malformed key must fail")
	}
	if n := countRows(t, s.db, "speech_cache"); n != 0 {
		t.Errorf("speech_cache rows = %d, want 0", n)
	}
}

// TestSpeechCacheSurvivesRestart is the AGENTS.md restart requirement for the
// voice cache: generate a line, close the app's store, re-open it, and the
// Get is an asset-backed hit — the TTS function is never called a second
// time. MemIndex is what made this test impossible before migration 0011:
// the previous run's index was process memory and died with it.
func TestSpeechCacheSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	assetRoot := t.TempDir()
	dbPath := DBPath(dir)

	a, err := assets.NewLocal(assetRoot)
	if err != nil {
		t.Fatalf("assets.NewLocal: %v", err)
	}

	calls := 0
	gen := func(ctx context.Context) (tts.Audio, error) {
		calls++
		wav := speech.MakeToneWAV(440, 0.5, 16000)
		return tts.Audio{WAV: wav, SampleRate: 16000, Channels: 1, BitsPerSample: 16,
			Duration: 500 * time.Millisecond}, nil
	}
	key := speechTestKey(0)

	// Run one: open, migrate, generate.
	st, err := Open(testCtx(t), dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := testCtx(t)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	cache := speech.New(a, NewSpeechCacheIndex(st), 4)
	res, err := cache.GetOrGenerate(ctx, key, speech.PriorityCurrent, gen)
	if err != nil {
		t.Fatalf("GetOrGenerate: %v (calls=%d)", err, calls)
	}
	if res.FromCache || calls != 1 {
		t.Fatalf("first run: FromCache=%v calls=%d, want a fresh synthesis (calls=1)", res.FromCache, calls)
	}
	if n := countRows(t, st.db, "speech_cache"); n != 1 {
		t.Fatalf("speech_cache rows = %d, want 1", n)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("closing the app's store: %v", err)
	}

	// Run two: re-open the same database. The index loads from the table and
	// the hit comes from the asset store — no TTS call.
	st2, err := Open(testCtx(t), dbPath)
	if err != nil {
		t.Fatalf("re-opening the store: %v", err)
	}
	t.Cleanup(func() { _ = st2.Close() })
	cache2 := speech.New(a, NewSpeechCacheIndex(st2), 4)
	hit, ok, err := cache2.Get(ctx, key, false)
	if err != nil {
		t.Fatalf("Get after restart: %v", err)
	}
	if !ok || !hit.FromCache {
		t.Fatalf("Get after restart = ok:%v FromCache:%v, want a cache hit", ok, hit.FromCache)
	}
	if calls != 1 {
		t.Fatalf("TTS called %d times across the restart, want 1 (the point of a persistent index)", calls)
	}
	if hit.SHA256 != res.SHA256 {
		t.Fatalf("hit blob %s, want the generated blob %s", hit.SHA256, res.SHA256)
	}
}
