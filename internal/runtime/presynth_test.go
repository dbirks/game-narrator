package runtime

// Profile pre-synthesis (ADR 0153): a pre-synthesized line is a cache hit
// for the live commit that speaks it (the shared key identity, lineCacheKey),
// a second pass costs no provider call, and a voiceless line is skipped.

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/speech/dummy"
	"github.com/dbirks/game-narrator/internal/store"
	"github.com/dbirks/game-narrator/internal/voices"
)

func TestPresynthOneThenCommitHitsTheCache(t *testing.T) {
	e := newCatchUpEnv(t, 0.2, "catch_up", 0, 0, nil)
	ctx := context.Background()
	text := "A line voiced before anyone plays."

	hit, n, skipped, err := e.w.presynthOne(ctx, "m-pre", text)
	if err != nil || skipped || hit || n == 0 {
		t.Fatalf("first presynth = hit %v, %d bytes, skipped %v, err %v; want a fresh synthesis", hit, n, skipped, err)
	}
	if got, _ := e.tts.requests(); len(got) != 1 {
		t.Fatalf("provider requests after the first pass = %d, want 1", len(got))
	}
	// A second pass is a cache lookup, not a provider call (resumability).
	hit, _, _, err = e.w.presynthOne(ctx, "m-pre", text)
	if err != nil || !hit {
		t.Fatalf("second presynth = hit %v, err %v; want a cache hit", hit, err)
	}
	if got, _ := e.tts.requests(); len(got) != 1 {
		t.Fatalf("provider requests after the second pass = %d, want still 1", len(got))
	}

	// The live commit of the same line under the same canonical text speaks
	// from the cache: the key the job booked is the key the commit computes.
	e.commit(t, "occ-pre", "m-pre", "bottom", 1, text)
	e.waitEvents(t, "the committed line was ready from the cache", 5*time.Second, func(evs []events.Envelope) bool {
		for _, ev := range evs {
			if ev.Name != events.SpeechReady {
				continue
			}
			var d map[string]any
			_ = json.Unmarshal(ev.Data, &d)
			if d["dialogue_occurrence_id"] == "occ-pre" {
				return d["from_cache"] == true
			}
		}
		return false
	})
	if got, _ := e.tts.requests(); len(got) != 1 {
		t.Fatalf("provider requests after the commit = %d, want still 1 (the commit must hit the presynthesized audio)", len(got))
	}
}

func TestPresynthOneSkipsAVoicelessSpeaker(t *testing.T) {
	e := newCatchUpEnv(t, 0.2, "catch_up", 0, 0, map[string]string{"m-x": "ch-without-voice"})
	e.w.narratorFallback = false
	_, _, skipped, err := e.w.presynthOne(context.Background(), "m-x", "Nobody voices this speaker.")
	if err != nil || !skipped {
		t.Fatalf("presynth of a voiceless speaker = skipped %v, err %v; want skipped", skipped, err)
	}
	if got, _ := e.tts.requests(); len(got) != 0 {
		t.Fatalf("provider requests = %d, want 0", len(got))
	}
}

// TestPresynthProfileSynthesizesPerPage is ADR-0155's pre-synthesis half: a
// profile whose multi-page message contributes one cache entry per display
// page, and a single-page message one entry under the bare message id — the
// same key the live commit path computes, so a pre-synthesized page is a hit
// for the commit that speaks it.
func TestPresynthProfileSynthesizesPerPage(t *testing.T) {
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
	fake := &catchUpTTS{toneS: 0.1}
	svc := &voices.Service{
		Store: st, Assets: a,
		Design: nil, Base: fake, Registry: nil,
		Speech: cache, InstanceID: "primary",
		BaseModelID:   "fake-base",
		BackendDigest: "test-backend-v1",
	}
	seedFenceVoice(t, st, a)
	m := &Manager{
		opts: Options{
			Store: st, Hub: hub, Log: slog.New(slog.DiscardHandler),
			Speech: &SpeechOptions{
				Voices:      svc,
				Assets:      a,
				Sink:        SinkDummy,
				SinkFactory: func() (speech.AudioSink, error) { return dummy.New(0, 0), nil },
			},
		},
		log: slog.New(slog.DiscardHandler),
	}

	// Seed a published profile whose single message carries three pages.
	game := games.Game{ID: games.NewID(), Slug: "synthetic-demo", Title: "Synthetic Demo", Platform: "gamecube", UserTags: json.RawMessage(`["synthetic"]`), CreatedAt: time.Now().UTC().Format(time.RFC3339)}
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
	now := time.Now().UTC().Format(time.RFC3339)
	msgID := games.NewID()
	msgs := []games.Message{{
		ID:                msgID,
		ProfileID:         draft.ID,
		SourceMessageID:   "multi-1",
		OriginalText:      multiPageText,
		NormalizedText:    "the lantern flickered across the harbour wall a stranger asked about the lost ferry the tide was already turning against the dock",
		SpeechText:        multiPageText,
		Language:          "en",
		TextKind:          games.TextDialogue,
		AttributionStatus: games.AttributionUnknown,
		Revision:          1,
		CreatedAt:         now,
		UpdatedAt:         now,
	}}
	pages := []games.MessagePage{
		{MessageID: msgID, Ordinal: 1, PageText: pageOneText},
		{MessageID: msgID, Ordinal: 2, PageText: pageTwoText},
		{MessageID: msgID, Ordinal: 3, PageText: pageThreeText},
	}
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
	if published.ID == "" {
		t.Fatal("the published profile has no id")
	}

	p, err := m.PresynthesizeProfile(ctx, published.ID, PresynthOptions{Concurrency: 1}, func(PresynthProgress) {})
	if err != nil {
		t.Fatalf("PresynthesizeProfile: %v", err)
	}
	if p.Total != 3 {
		t.Fatalf("presynth total = %d, want 3 (one per display page)", p.Total)
	}
	if p.Synthesized != 3 {
		t.Fatalf("presynth synthesized = %d, want 3 (one per display page)", p.Synthesized)
	}
	if p.Failed != 0 {
		t.Fatalf("presynth failed = %d, want 0", p.Failed)
	}
	if got, _ := fake.requests(); len(got) != 3 {
		t.Fatalf("provider requests = %d, want 3 (one per page)", len(got))
	}
}
