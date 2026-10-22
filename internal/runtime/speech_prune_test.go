package runtime_test

// The speech-cache prune (ADR-0159, issue #49) on invented data: the reachable
// key set the live paths can produce for a published profile, and the prune
// that removes what that set does not cover.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/providers/tts"
	"github.com/dbirks/game-narrator/internal/runtime"
	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/store"
	"github.com/dbirks/game-narrator/internal/voices"
)

// pruneTestTTS is the scripted Base checkpoint for the prune tests: the same
// deterministic tone the the runtime's other speech fakes use, with no call
// bookkeeping — the prune never synthesizes, it only computes keys.
type pruneTestTTS struct{ toneS float64 }

func (f *pruneTestTTS) Generate(context.Context, tts.GenerateRequest) (tts.Audio, error) {
	return f.tone(), nil
}

func (f *pruneTestTTS) GenerateWithReference(context.Context, tts.GenerateRequest, []byte, string) (tts.Audio, error) {
	return f.tone(), nil
}

func (f *pruneTestTTS) tone() tts.Audio {
	return tts.Audio{WAV: speech.MakeToneWAV(440, f.toneS, 16000), SampleRate: 16000, Channels: 1, BitsPerSample: 16, Duration: time.Duration(f.toneS * float64(time.Second)), Model: "fake-base"}
}

// TestSpeechCachePruneUnreachableAfterKeyChange is the ADR-0159 shape: after
// the page-aware (ADR-0155) and shared-slot (ADR-0156) key changes orphaned the
// entries a whole-message / slot-linked cache held, the prune must keep the
// keys the live paths can still produce and drop what they cannot.
func TestSpeechCachePruneUnreachableAfterKeyChange(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, store.DBPath(t.TempDir()))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	mem := events.NewMemStore()
	hub := events.New(events.WithStore(mem))
	a, err := assets.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("assets: %v", err)
	}
	cache := speech.New(a, speech.NewMemIndex(), 4)
	svc := &voices.Service{
		Store: st, Assets: a, Design: nil, Base: &pruneTestTTS{toneS: 0.1}, Registry: nil,
		Speech: cache, InstanceID: "primary", BaseModelID: "fake-base", BackendDigest: "test-backend-v1",
	}
	seedNarratorVoice(t, st, a)
	mgr, err := runtime.New(runtime.Options{
		Store:  st,
		Hub:    hub,
		Log:    slog.New(slog.DiscardHandler),
		Speech: &runtime.SpeechOptions{Voices: svc, Assets: a},
	})
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}
	t.Cleanup(mgr.Close)
	t.Cleanup(func() { _ = st.Close() })
	t.Cleanup(func() { _ = hub.Close() })

	// Seed a published profile whose single message carries three pages.
	now := time.Now().UTC().Format(time.RFC3339)
	game := games.Game{ID: games.NewID(), Slug: "synthetic-demo", Title: "Synthetic Demo", Platform: "gamecube", UserTags: json.RawMessage(`["synthetic"]`), CreatedAt: now}
	if err := st.CreateGame(ctx, game); err != nil {
		t.Fatalf("CreateGame: %v", err)
	}
	edition := games.Edition{ID: games.NewID(), GameID: game.ID, Region: "US", Language: "en", Revision: "rev0", Locale: "en_US"}
	if err := st.CreateEdition(ctx, edition); err != nil {
		t.Fatalf("CreateEdition: %v", err)
	}
	draft, err := st.CreateDraftProfile(ctx, edition.ID)
	if err != nil {
		t.Fatalf("CreateDraftProfile: %v", err)
	}
	msgID := games.NewID()
	pages := []games.MessagePage{
		{MessageID: msgID, Ordinal: 1, PageText: "The lantern flickered across the harbour wall."},
		{MessageID: msgID, Ordinal: 2, PageText: "A stranger asked about the lost ferry."},
		{MessageID: msgID, Ordinal: 3, PageText: "The tide was already turning against the dock."},
	}
	msgs := []games.Message{{
		ID:                msgID,
		ProfileID:         draft.ID,
		SourceMessageID:   "multi-1",
		OriginalText:      "The lantern flickered across the harbour wall. A stranger asked about the lost ferry. The tide was already turning against the dock.",
		NormalizedText:    "the lantern flickered across the harbour wall a stranger asked about the lost ferry the tide was already turning against the dock",
		SpeechText:        "The lantern flickered across the harbour wall. A stranger asked about the lost ferry. The tide was already turning against the dock.",
		Language:          "en",
		TextKind:          games.TextDialogue,
		AttributionStatus: games.AttributionUnknown,
		Revision:          1,
		CreatedAt:         now,
		UpdatedAt:         now,
	}}
	if err := st.PutMessages(ctx, draft.ID, msgs, pages); err != nil {
		t.Fatalf("PutMessages: %v", err)
	}
	_, rev, err := st.GetProfile(ctx, draft.ID)
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	if err := st.SetProfileStatus(ctx, draft.ID, games.StatusReview, rev); err != nil {
		t.Fatalf("SetProfileStatus: %v", err)
	}
	_, rev, err = st.GetProfile(ctx, draft.ID) // SetProfileStatus moves the row version
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	published, err := st.Publish(ctx, draft.ID, rev, "test-author@local")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// The reachable set: one key per display page of the published revision
	// (ADR-0155) under the narrator voice's approved identity (ADR-0101/ADR-0156).
	keys, err := mgr.SpeechReachableKeys(ctx, published.ID)
	if err != nil {
		t.Fatalf("SpeechReachableKeys: %v", err)
	}
	if len(keys) != 3 {
		t.Fatalf("reachable keys = %d, want 3 (one per display page); the page-aware key change must hold", len(keys))
	}

	// Seed the cache with the reachable entries plus one orphaned entry —
	// the kind the pre-change whole-message / slot-linked keys produced.
	for i, k := range keys {
		if err := cache.Index.Put(k, speech.Entry{SHA256: sha256hex(t, fmt.Sprintf("reachable-%d", i)), Size: 64000, Duration: 1000000000, SampleRate: 16000, QA: speech.QAUnknown}); err != nil {
			t.Fatalf("put reachable %d: %v", i, err)
		}
	}
	orphanKey, err := speech.CacheKeyOf(speech.KeyInput{
		UtteranceText: "The lantern flickered across the harbour wall.", OriginalMessageID: "m-orphan",
		SpeechRulesRevision: 1, Language: "en", VoiceRevisionFingerprint: "v1", TTSModelID: "m@1", BackendConfigDigest: "d1", Format: "wav/pcm16",
	})
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	if err := cache.Index.Put(orphanKey, speech.Entry{SHA256: sha256hex(t, "orphan"), Size: 64000, Duration: 1000000000, SampleRate: 16000, QA: speech.QAUnknown}); err != nil {
		t.Fatalf("put orphan: %v", err)
	}

	// Dry run before the real prune: the same would-be answer, nothing deleted.
	entries, freed, err := mgr.PruneUnreachableSpeechCache(ctx, true)
	if err != nil {
		t.Fatalf("PruneUnreachableSpeechCache(dryRun): %v", err)
	}
	if entries != 1 || freed != 0 {
		t.Fatalf("dry run = %d entries / %d bytes, want 1 / 0", entries, freed)
	}
	if _, ok := cache.Index.Lookup(orphanKey); !ok {
		t.Fatal("the dry run deleted the orphaned entry: a dry run must delete nothing")
	}

	// The prune removes the orphaned entry and keeps the three reachable ones.
	entries, freed, err = mgr.PruneUnreachableSpeechCache(ctx, false)
	if err != nil {
		t.Fatalf("PruneUnreachableSpeechCache: %v", err)
	}
	// Without a reference checker the cache evicts index entries only (the
	// ADR-0041 safe default): the entry is dropped, the shared-blob bytes are
	// not reclaimed — that is the documented, safe answer.
	if entries != 1 || freed != 0 {
		t.Fatalf("prune = %d entries / %d bytes, want 1 / 0 (only the orphaned entry; index-only without a checker)", entries, freed)
	}
	for i, k := range keys {
		if _, ok := cache.Index.Lookup(k); !ok {
			t.Fatalf("reachable key %d was pruned away: %q", i, k)
		}
	}
	if _, ok := cache.Index.Lookup(orphanKey); ok {
		t.Fatal("the orphaned entry survived the prune")
	}
}

// sha256hex is a distinct, well-formed cache-blob digest per call — the prune
// only carries blob hashes, never content it reads.
func sha256hex(t *testing.T, salt string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(salt))
	return hex.EncodeToString(sum[:])
}
