package runtime

// White-box tests for the speech fence's commit eligibility (issue #6,
// ADR-0092): a commit that is not speech-eligible (unknown message,
// non-narratable, abstained) must not fence — so it cannot cancel the line
// the player is listening to — while a speakable commit still fences (the
// stale-speech guarantee), terminal results are idempotent (a settled
// generation is never cancelled again), and a voice skip names its reason
// with the character id.
//
// These drive the wiring's onCommit directly with synthetic commit
// envelopes, so the commit stream (line, N noise commits, line) is exact —
// no wall-clock margin between commits can reorder what the test observes.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/matcher"
	"github.com/dbirks/game-narrator/internal/providers/tts"
	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/speech/dummy"
	"github.com/dbirks/game-narrator/internal/store"
	"github.com/dbirks/game-narrator/internal/voices"
)

// ── fakes ------------------------------------------------------------------

// fenceFakeTTS is the scripted Base checkpoint: it answers with a
// deterministic tone whose duration the test controls, so a line can be made
// long enough to still be playing when the next commit lands.
type fenceFakeTTS struct {
	toneS float64
}

func (f *fenceFakeTTS) tone() tts.Audio {
	wav := speech.MakeToneWAV(440, f.toneS, 16000)
	return tts.Audio{WAV: wav, SampleRate: 16000, Channels: 1, BitsPerSample: 16,
		Duration: time.Duration(f.toneS * float64(time.Second)), Model: "fake-base"}
}

func (f *fenceFakeTTS) Generate(_ context.Context, _ tts.GenerateRequest) (tts.Audio, error) {
	return f.tone(), nil
}

func (f *fenceFakeTTS) GenerateWithReference(_ context.Context, _ tts.GenerateRequest, _ []byte, _ string) (tts.Audio, error) {
	return f.tone(), nil
}

// ── environment -------------------------------------------------------------

const fenceTestVoiceName = "gn_fence_test_narrator_v1"

// fenceEnv is one white-box speech wiring with a seeded narrator voice, a
// programmable dummy sink, and an event recorder the test reads.
type fenceEnv struct {
	t         *testing.T
	w         *speechWiring
	sink      *dummy.Sink
	hub       *events.Hub
	mem       *events.MemStore
	st        *store.Store
	cache     *speech.Cache
	tts       *fenceFakeTTS
	character map[string]string // message id → speaker (for the skip test)
}

// newFenceEnv builds the white-box wiring. seedVoice gives the narrator an
// approved, registered voice. A non-nil speakerByMessage assigns characters
// to message ids.
func newFenceEnv(t *testing.T, toneS float64, seedVoice bool, speakerByMessage map[string]string) *fenceEnv {
	t.Helper()
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
	fake := &fenceFakeTTS{toneS: toneS}
	svc := &voices.Service{
		Store: st, Assets: a,
		Design: nil, Base: fake, Registry: nil,
		Speech: cache, InstanceID: "primary",
		BaseModelID:   "fake-base",
		BackendDigest: "test-backend-v1",
	}
	// No voices.New validation here: the speak path reads Store, Base,
	// Speech, InstanceID and BackendDigest directly, and QA is nil-safe (a
	// nil engine's QAInsert returns the result unchanged). Design and
	// Registry are not exercised by the fence tests.
	if seedVoice {
		seedFenceVoice(t, st, a)
	}

	sink := dummy.New(1, 5)
	endCtx, endCancel := context.WithCancel(ctx)
	w := &speechWiring{
		id:               "fence-test",
		log:              func(string, ...any) {},
		hub:              hub,
		voices:           svc,
		assets:           a,
		speakerByMessage: speakerByMessage,
		// The profile row's default (ADR-0101): the fence tests exercise the
		// historical narration behaviour, narrator fallback on.
		narratorFallback: true,
		sem:              make(chan struct{}, speechWorkerSlots),
		utterances:       map[string]*utteranceInfo{},
		byGeneration:     map[int64]string{},
		genCancel:        map[int64]*synthSlot{},
		settled:          map[int64]bool{},
		volume:           1.0,
		endCtx:           endCtx,
		endCancel:        endCancel,
		sink:             newGainSink(sink),
		fence:            &generationFence{},
	}
	w.sched = speech.NewScheduler(w.sink, w.fence, &speechEventPublisher{hub: hub, w: w}, speech.Options{
		FencePeriod: 20 * time.Millisecond,
	})
	w.sched.Start(endCtx)

	e := &fenceEnv{t: t, w: w, sink: sink, hub: hub, mem: mem, st: st, cache: cache, tts: fake, character: speakerByMessage}
	t.Cleanup(func() {
		endCancel()
		_ = w.sched.Stop()
		_ = st.Close()
		_ = hub.Close()
	})
	return e
}

// seedFenceVoice gives the narrator one approved, ready revision.
func seedFenceVoice(t *testing.T, st *store.Store, a *assets.Local) {
	t.Helper()
	ctx := context.Background()
	ref := speech.MakeToneWAV(220, 1.0, 16000)
	sum := sha256.Sum256(ref)
	refSHA := hex.EncodeToString(sum[:])
	id, _, err := a.PutStaged(ctx, bytes.NewReader(ref), refSHA)
	if err != nil {
		t.Fatalf("staging reference: %v", err)
	}
	if _, err := a.Commit(ctx, id); err != nil {
		t.Fatalf("committing reference: %v", err)
	}
	profID := games.NewID()
	now := time.Now().UTC()
	if err := st.CreateVoiceRevision(ctx, store.VoiceProfile{
		ID:                profID,
		Character:         narratorCharacter,
		Revision:          1,
		State:             store.VoiceStateApproved,
		DesignInstruction: "a calm test voice",
		ReferenceAssetID:  refSHA,
		RefTranscript:     voices.SyntheticAuditionLines[0],
		ModelID:           "fake-base",
		Language:          "en",
		ConsentProvenance: "original_synthetic_design (game-narrator automated test)",
		ApprovedBy:        "test",
		Fingerprint:       voices.Fingerprint(refSHA, voices.SyntheticAuditionLines[0], "fake-base"),
		CreatedAt:         now,
		UpdatedAt:         now,
	}); err != nil {
		t.Fatalf("creating approved revision: %v", err)
	}
	verified := time.Now().UTC()
	if err := st.UpsertVoiceRegistration(ctx, store.VoiceRegistration{
		VoiceProfileID: profID,
		InstanceID:     "primary",
		RegisteredName: fenceTestVoiceName,
		LastVerifiedAt: &verified,
		State:          store.VoiceRegReady,
	}); err != nil {
		t.Fatalf("recording registration: %v", err)
	}
}

// commit feeds the wiring one message.committed envelope.
func (e *fenceEnv) commit(t *testing.T, occ, msgID, region string, generation int64, text string, eligible bool) {
	t.Helper()
	data, _ := json.Marshal(map[string]any{
		"dialogue_occurrence_id": occ,
		"message_id":             msgID,
		"region_id":              region,
		"generation":             generation,
		"text":                   text,
		"speech_eligible":        eligible,
	})
	e.w.onCommit(events.Envelope{
		SchemaVersion: 1,
		SessionID:     e.w.id,
		TraceID:       "trace-" + occ,
		Name:          events.MessageCommitted,
		Data:          data,
	})
}

// sessionEvents reads the session's event log, newest last.
func (e *fenceEnv) sessionEvents() []events.Envelope {
	evs, err := e.mem.After(context.Background(), 0, events.Filter{SessionID: e.w.id}, 4096)
	if err != nil {
		e.t.Fatalf("read events: %v", err)
	}
	return evs
}

// waitEvents polls the event log until pred holds (or the deadline passes).
func (e *fenceEnv) waitEvents(t *testing.T, desc string, deadline time.Duration, pred func([]events.Envelope) bool) []events.Envelope {
	t.Helper()
	by := time.Now().Add(deadline)
	for {
		evs := e.sessionEvents()
		if pred(evs) {
			return evs
		}
		if time.Now().After(by) {
			t.Fatalf("%s: not satisfied within %s", desc, deadline)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// withGraph wires the wiring's successor prewarming (FR-11): the dialogue
// graph (the matcher index the ranker and the predictive tracker use) and
// the canonical TTS text per message. It must be called before the first
// commit.
func (e *fenceEnv) withGraph(t *testing.T, idx *matcher.Index, texts map[string]string) {
	t.Helper()
	e.w.initPrewarm(idx, texts, nil)
}

// blockingTTS is a Base checkpoint that honours the request context: the
// prewarm-cancel test needs a synthesis that is genuinely in flight when the
// fence moves, and the cancellation must be observable at the source (the
// call returns the context error and records it) — the production streaming
// seam's contract, mirrored.
type blockingTTS struct {
	mu       sync.Mutex
	inFlight int
	canceled int
	block    chan struct{} // Generate blocks until closed (or the ctx dies)
}

func (f *blockingTTS) tone() tts.Audio {
	wav := speech.MakeToneWAV(440, 0.2, 16000)
	return tts.Audio{WAV: wav, SampleRate: 16000, Channels: 1, BitsPerSample: 16,
		Duration: 200 * time.Millisecond, Model: "fake-base"}
}

func (f *blockingTTS) generate(ctx context.Context) (tts.Audio, error) {
	f.mu.Lock()
	f.inFlight++
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inFlight--
		f.mu.Unlock()
	}()
	select {
	case <-f.block:
		return f.tone(), nil
	case <-ctx.Done():
		f.mu.Lock()
		f.canceled++
		f.mu.Unlock()
		return tts.Audio{}, ctx.Err()
	}
}

func (f *blockingTTS) Generate(ctx context.Context, _ tts.GenerateRequest) (tts.Audio, error) {
	return f.generate(ctx)
}

func (f *blockingTTS) GenerateWithReference(ctx context.Context, _ tts.GenerateRequest, _ []byte, _ string) (tts.Audio, error) {
	return f.generate(ctx)
}

func (f *blockingTTS) inFlightCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inFlight
}

func (f *blockingTTS) canceledCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.canceled
}

// evPayload decodes one event's data into a map.
func evPayload(t *testing.T, ev events.Envelope) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(ev.Data, &m); err != nil {
		t.Fatalf("decode %s payload: %v", ev.Name, err)
	}
	return m
}

// ── tests -------------------------------------------------------------------

// TestSpeechNoiseCommitsDoNotFence is the issue #6 regression: a speakable
// line commits, 20 non-eligible noise commits follow, and the line still ends
// "ended" (natural), not "cancelled" — the noise must not fence. Then a real
// speakable commit cancels the (still playing) previous line: the stale
// guarantee holds.
func TestSpeechNoiseCommitsDoNotFence(t *testing.T) {
	// Part 1: line A (short tone) + 20 noise commits → A ends "ended".
	e := newFenceEnv(t, 0.2, true, nil)
	e.commit(t, "occ-a", "msg-a", "region-box", 1, "line a", true)

	// 20 non-eligible noise commits (the water-region OCR fragments of the
	// live run): unknown message, not eligible.
	for i := 2; i <= 21; i++ {
		e.commit(t, "occ-noise", "unknown", "region-water", int64(i), "w", false)
	}

	evs := e.waitEvents(t, "line A ended", 10*time.Second, func(evs []events.Envelope) bool {
		for _, ev := range evs {
			if ev.Name == events.SpeechEnded && genOfEv(t, ev) == 1 {
				return true
			}
		}
		return false
	})
	// A ends "ended" (natural), not "cancelled": the noise did not fence it.
	var aResult string
	for _, ev := range evs {
		if ev.Name == events.SpeechEnded && genOfEv(t, ev) == 1 {
			aResult, _ = evPayload(t, ev)["result"].(string)
		}
	}
	if aResult != "ended" {
		t.Fatalf("line A ended %q after 20 noise commits, want \"ended\" (the noise must not fence)", aResult)
	}
	// No cancellation of generation 1 at all.
	for _, ev := range evs {
		if ev.Name == events.SpeechCancelled && genOfEv(t, ev) == 1 {
			t.Fatalf("line A was cancelled (%s) although only noise commits followed: %s", ev.Name, ev.Data)
		}
	}

	// Part 2: the stale guarantee still holds — a real speakable commit
	// cancels the previous line while it is still playing. Line C (long tone)
	// commits and starts; line D commits and cancels C.
	e.tts.toneS = 3
	e.commit(t, "occ-c", "msg-c", "region-box", 22, "line c", true)
	e.waitEvents(t, "line C started", 10*time.Second, func(evs []events.Envelope) bool {
		for _, ev := range evs {
			if ev.Name == events.SpeechStarted && genOfEv(t, ev) == 22 {
				return true
			}
		}
		return false
	})
	e.commit(t, "occ-d", "msg-d", "region-box", 23, "line d", true)
	e.waitEvents(t, "line C cancelled and line D started", 10*time.Second, func(evs []events.Envelope) bool {
		var cancelled, started int
		for _, ev := range evs {
			switch ev.Name {
			case events.SpeechCancelled:
				if genOfEv(t, ev) == 22 {
					cancelled++
				}
			case events.SpeechStarted:
				if genOfEv(t, ev) == 23 {
					started++
				}
			}
		}
		return cancelled >= 1 && started >= 1
	})
	// C's cancellation is the advance's (new_commit), from line D's fence.
	for _, ev := range e.sessionEvents() {
		if ev.Name != events.SpeechCancelled || genOfEv(t, ev) != 22 {
			continue
		}
		switch evPayload(t, ev)["reason"] {
		case "new_commit", "stale_generation":
		default:
			t.Fatalf("line C cancellation reason = %v, want new_commit or stale_generation", evPayload(t, ev)["reason"])
		}
	}
}

// TestSpeechTerminalIdempotent is the issue #6 idempotency fix: a generation
// that reached a terminal is pruned and never cancelled again — a later
// commit must not emit a second terminal (speech.cancelled) for it.
func TestSpeechTerminalIdempotent(t *testing.T) {
	e := newFenceEnv(t, 0.2, true, nil)
	// Line A commits and ends naturally.
	e.commit(t, "occ-a", "msg-a", "region-box", 1, "line a", true)
	e.waitEvents(t, "line A ended", 10*time.Second, func(evs []events.Envelope) bool {
		for _, ev := range evs {
			if ev.Name == events.SpeechEnded && genOfEv(t, ev) == 1 {
				return true
			}
		}
		return false
	})
	// Generation 1 is now settled and pruned from the fence's cancel table.
	if !e.w.settledLocked(1) {
		t.Fatal("generation 1 is not settled after its terminal")
	}
	if e.w.byGenLocked(1) {
		t.Fatal("generation 1 is still in the fence's cancel table after its terminal")
	}

	// A later speakable commit must not re-cancel generation 1 (a second
	// terminal). It fences to gen 2; gen 1 is already settled.
	e.commit(t, "occ-b", "msg-b", "region-box", 2, "line b", true)
	e.waitEvents(t, "line B ended", 10*time.Second, func(evs []events.Envelope) bool {
		for _, ev := range evs {
			if ev.Name == events.SpeechEnded && genOfEv(t, ev) == 2 {
				return true
			}
		}
		return false
	})
	// Count the terminals for generation 1: exactly one (the natural end), no
	// speech.cancelled, no second speech.ended.
	var ended1, cancelled1 int
	for _, ev := range e.sessionEvents() {
		if genOfEv(t, ev) != 1 {
			continue
		}
		switch ev.Name {
		case events.SpeechEnded:
			ended1++
		case events.SpeechCancelled:
			cancelled1++
		}
	}
	if ended1 != 1 {
		t.Fatalf("generation 1 has %d speech.ended event(s), want exactly 1 (terminal is idempotent)", ended1)
	}
	if cancelled1 != 0 {
		t.Fatalf("generation 1 has %d speech.cancelled event(s), want 0 (a settled generation is never cancelled again)", cancelled1)
	}
}

// TestSpeechVoiceSkipNamesReason is the issue #6 skip-reason fix: a line whose
// speaker has no approved voice (and no configured narrator) is skipped with
// the reason "no_approved_voice" AND the character id, in the event.
func TestSpeechVoiceSkipNamesReason(t *testing.T) {
	// No narrator voice is seeded; the line's speaker is "link". Neither the
	// speaker nor the narrator has an approved voice.
	e := newFenceEnv(t, 0.2, false, map[string]string{"msg-a": "link"})
	e.commit(t, "occ-a", "msg-a", "region-box", 1, "line a", true)

	evs := e.waitEvents(t, "skipped utterance", 10*time.Second, func(evs []events.Envelope) bool {
		for _, ev := range evs {
			if ev.Name == events.SpeechEnded && genOfEv(t, ev) == 1 {
				return true
			}
		}
		return false
	})
	var found bool
	for _, ev := range evs {
		if ev.Name != events.SpeechEnded || genOfEv(t, ev) != 1 {
			continue
		}
		p := evPayload(t, ev)
		if p["result"] != "skipped" {
			t.Fatalf("line A result = %v, want \"skipped\" (no approved voice)", p["result"])
		}
		if p["reason"] != "no_approved_voice" {
			t.Fatalf("line A skip reason = %q, want \"no_approved_voice\" (a skip must name its reason)", p["reason"])
		}
		if p["character_id"] != "link" {
			t.Fatalf("line A skip character_id = %q, want \"link\" (the skip must name whose voice is missing)", p["character_id"])
		}
		found = true
	}
	if !found {
		t.Fatal("line A reached no speech.ended terminal")
	}
	// No audio, ever: no TTS request went out, nothing started.
	if n := e.sink.Starts(); n != 0 {
		t.Fatalf("no approved voice, but %d playback(s) started", n)
	}
}

// ── small helpers -------------------------------------------------------------

// genOfEv reads the "generation" field out of one event payload.
func genOfEv(t *testing.T, ev events.Envelope) int64 {
	t.Helper()
	f, ok := evPayload(t, ev)["generation"].(float64)
	if !ok {
		t.Fatalf("%s payload has no numeric generation: %s", ev.Name, ev.Data)
	}
	return int64(f)
}

// settledLocked reports whether a generation is settled (test seam; the
// wiring's lock is held only by the test's single-threaded setup here).
func (w *speechWiring) settledLocked(g int64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.settled[g]
}

// byGenLocked reports whether a generation is still in the fence's cancel
// table (test seam).
func (w *speechWiring) byGenLocked(g int64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, ok := w.byGeneration[g]
	return ok
}

// ── successor prewarming (FR-11) --------------------------------------------

// prewarmGraph builds a scripted dialogue graph for the prewarm tests: one
// message per node (the canonical text is the TTS text the prewarm
// synthesises) and the given weighted edges.
func prewarmGraph(t *testing.T, nodes []struct{ id, text string }, edges []games.MessageEdge) (*matcher.Index, map[string]string) {
	t.Helper()
	msgs := make([]games.Message, 0, len(nodes))
	texts := make(map[string]string, len(nodes))
	for _, n := range nodes {
		msgs = append(msgs, games.Message{ID: n.id, OriginalText: n.text, NormalizedText: n.text})
		texts[n.id] = n.text
	}
	idx, err := matcher.Build(msgs, edges, matcher.IndexOptions{})
	if err != nil {
		t.Fatalf("matcher.Build: %v", err)
	}
	return idx, texts
}

// TestSpeechPrewarmNextLineFromCache is the FR-11 contract on a scripted
// graph: when line a commits, its weighted successors (b, the dominant
// "next", and c, the branch) are prewarmed into the shared cache at
// background priority, and the later commit of line b resolves from the
// cache (from_cache:true) with the hit booked on the session's prewarm
// stats.
func TestSpeechPrewarmNextLineFromCache(t *testing.T) {
	e := newFenceEnv(t, 0.1, true, nil)
	idx, texts := prewarmGraph(t,
		[]struct{ id, text string }{
			{"m-a", "line a"},
			{"m-b", "line b"},
			{"m-c", "line c"},
		},
		[]games.MessageEdge{
			{FromID: "m-a", ToID: "m-b", EdgeType: games.EdgeNext, Confidence: 0.9},
			{FromID: "m-a", ToID: "m-c", EdgeType: games.EdgeBranch, Confidence: 0.2},
		})
	e.withGraph(t, idx, texts)

	e.commit(t, "occ-a", "m-a", "region-box", 1, "line a", true)

	// Let the prewarm batch actually fill the cache — a (on demand) plus b
	// and c (prewarmed) — before line b commits: the ordering is the test,
	// not a wall-clock margin.
	by := time.Now().Add(10 * time.Second)
	for e.cache.Stats().Generated < 3 {
		if time.Now().After(by) {
			t.Fatalf("the prewarm batch filled %d of 3 utterances within the deadline: %+v",
				e.cache.Stats().Generated, e.cache.Stats())
		}
		time.Sleep(5 * time.Millisecond)
	}

	e.commit(t, "occ-b", "m-b", "region-box", 2, "line b", true)
	e.waitEvents(t, "line b ready from the prewarm", 10*time.Second, func(evs []events.Envelope) bool {
		for _, ev := range evs {
			if ev.Name == events.SpeechReady && genOfEv(t, ev) == 2 {
				return evPayload(t, ev)["from_cache"] == true
			}
		}
		return false
	})

	st := e.w.audioInfo().Prewarm
	if st.Hit != 1 {
		t.Fatalf("prewarm hit = %d, want 1 (line b's commit resolved the prewarmed key): %+v", st.Hit, st)
	}
	if st.Requested < 2 {
		t.Fatalf("prewarm requested = %d, want >= 2 (b and c were enqueued): %+v", st.Requested, st)
	}
	if st.Bytes <= 0 {
		t.Fatalf("prewarm bytes = %d, want > 0 (the prewarmed audio was committed): %+v", st.Bytes, st)
	}
	// The hit is also the plain cache evidence: the session counted the
	// cache resolution of line b.
	if got := e.w.audioInfo().Speech.CacheHits; got < 1 {
		t.Fatalf("session cache_hits = %d, want >= 1: the prewarm's whole point is the hit", got)
	}
}

// TestSpeechPrewarmCancelledByNewCommit is the cancellation half of FR-11:
// a prewarm genuinely in flight (its synthesis blocked inside the provider
// call, the streaming seam's contract) when the fence moves is cancelled at
// the source — the provider call returns the context error, the job books
// as wasted — and the committed line still resolves: the singleflight
// leader died, and the committed speech retries as the fresh leader under
// its own context.
func TestSpeechPrewarmCancelledByNewCommit(t *testing.T) {
	e := newFenceEnv(t, 0.2, true, nil)
	blk := &blockingTTS{block: make(chan struct{})}
	e.w.voices.Base = blk
	idx, texts := prewarmGraph(t,
		[]struct{ id, text string }{{"m-a", "line a"}, {"m-b", "line b"}},
		[]games.MessageEdge{{FromID: "m-a", ToID: "m-b", EdgeType: games.EdgeNext, Confidence: 0.9}})
	e.withGraph(t, idx, texts)

	e.commit(t, "occ-a", "m-a", "region-box", 1, "line a", true)

	// Wait until line a's own synthesis AND the prewarm of line b are both
	// in flight in the provider: the prewarm is demonstrably mid-TTS-call
	// when the fence moves.
	by := time.Now().Add(10 * time.Second)
	for blk.inFlightCount() < 2 {
		if time.Now().After(by) {
			t.Fatalf("the prewarm of line b is not in flight (in flight: %d)", blk.inFlightCount())
		}
		time.Sleep(5 * time.Millisecond)
	}

	// The fence moves past the in-flight prewarm.
	e.commit(t, "occ-b", "m-b", "region-box", 2, "line b", true)
	// Release whatever synthesis line b itself needs.
	close(blk.block)

	e.waitEvents(t, "line b ready", 10*time.Second, func(evs []events.Envelope) bool {
		for _, ev := range evs {
			if ev.Name == events.SpeechReady && genOfEv(t, ev) == 2 {
				return true
			}
		}
		return false
	})

	// The prewarm's in-flight call was cancelled at the source, and the job
	// booked as wasted — not silently dropped.
	if n := blk.canceledCount(); n < 1 {
		t.Fatalf("the provider saw %d cancelled prewarm call(s), want >= 1: the fence move must cancel the in-flight prewarm at the source", n)
	}
	if st := e.w.audioInfo().Prewarm; st.Wasted < 1 {
		t.Fatalf("prewarm wasted = %d, want >= 1 (the cancelled in-flight job): %+v", st.Wasted, st)
	}
	if st := e.w.audioInfo().Prewarm; st.Hit != 0 {
		t.Fatalf("prewarm hit = %d, want 0 (the prewarm died before it filled the cache): %+v", st.Hit, st)
	}
}

// TestSpeechPrewarmByteQuotaSkipsBatch is docs/spec/07's byte quota: once
// the session's prewarmed bytes reach the quota, a new commit's batch is
// refused — the skipped batch prewarms nothing, and the line pays the
// round trip on commit.
func TestSpeechPrewarmByteQuotaSkipsBatch(t *testing.T) {
	e := newFenceEnv(t, 0.1, true, nil)
	// One 0.1 s tone is ~3.2 KiB; the quota below one tone means: the first
	// batch (b) fills, the second (c) is refused.
	idx, texts := prewarmGraph(t,
		[]struct{ id, text string }{{"m-a", "line a"}, {"m-b", "line b"}, {"m-c", "line c"}},
		[]games.MessageEdge{
			{FromID: "m-a", ToID: "m-b", EdgeType: games.EdgeNext, Confidence: 0.9},
			{FromID: "m-b", ToID: "m-c", EdgeType: games.EdgeNext, Confidence: 0.9},
		})
	e.withGraph(t, idx, texts)
	// The policy goes in AFTER withGraph: initPrewarm re-resolves the
	// config, so a value set before it would be clobbered by the defaults.
	e.w.prewarmCfg = prewarmConfig{topK: 3, maxEntropyBits: 8, byteQuota: 3000, jobDeadline: 5 * time.Second}

	e.commit(t, "occ-a", "m-a", "region-box", 1, "line a", true)
	// Let the first batch (b) commit its bytes against the quota.
	by := time.Now().Add(10 * time.Second)
	for e.w.audioInfo().Prewarm.Bytes == 0 {
		if time.Now().After(by) {
			t.Fatalf("the first prewarm batch never booked bytes: %+v", e.w.audioInfo().Prewarm)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Line b commits: its batch (c) is refused by the quota — synchronously,
	// in the commit path itself.
	e.commit(t, "occ-b", "m-b", "region-box", 2, "line b", true)
	if st := e.w.audioInfo().Prewarm; st.Skipped != 1 {
		t.Fatalf("prewarm skipped = %d, want 1 (the quota-refused batch): %+v", st.Skipped, st)
	}
	// c was never prewarmed: the cache holds a and b only. Line a's own
	// synthesis is async, so wait for both generated utterances to land
	// before counting (the skipped batch enqueued nothing, so the count is
	// stable once both have landed).
	by = time.Now().Add(10 * time.Second)
	for e.cache.Stats().Generated < 2 {
		if time.Now().After(by) {
			t.Fatalf("the cache holds %d utterances, want 2 (a, b — c was quota-skipped): %+v", e.cache.Stats().Generated, e.cache.Stats())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := e.cache.Stats().Generated; n != 2 {
		t.Fatalf("the cache holds %d utterances, want 2 (a, b — c was quota-skipped): %+v", n, e.cache.Stats())
	}

	// Line c commits and pays the round trip: no cache hit.
	e.commit(t, "occ-c", "m-c", "region-box", 3, "line c", true)
	e.waitEvents(t, "line c ready", 10*time.Second, func(evs []events.Envelope) bool {
		for _, ev := range evs {
			if ev.Name == events.SpeechReady && genOfEv(t, ev) == 3 {
				return evPayload(t, ev)["from_cache"] == false
			}
		}
		return false
	})
	// The prewarm hit is booked inside line b's own speak worker, just before
	// b's ready event — not in the commit path, so c's ready does not imply
	// it: under contention c's round trip can settle while b's worker is
	// still resolving. Wait on b's ready (the state the hit belongs to) before
	// reading the stats.
	e.waitEvents(t, "line b ready (its prewarm hit is booked)", 10*time.Second, func(evs []events.Envelope) bool {
		for _, ev := range evs {
			if ev.Name == events.SpeechReady && genOfEv(t, ev) == 2 {
				return evPayload(t, ev)["from_cache"] == true
			}
		}
		return false
	})
	if st := e.w.audioInfo().Prewarm; st.Skipped != 1 || st.Hit != 1 {
		t.Fatalf("after line c: prewarm = %+v, want skipped 1, hit 1 (b was the prewarmed hit; c paid the round trip)", st)
	}
}

// TestSpeechPrewarmEntropySkipsBatch is docs/spec/07's graph-entropy cap:
// when the committed line's successors are too uncertain (a flat
// distribution above the entropy cap), the whole batch is skipped and no
// successor is prewarmed.
func TestSpeechPrewarmEntropySkipsBatch(t *testing.T) {
	e := newFenceEnv(t, 0.1, true, nil)
	// Three equal-weight successors: the distribution is flat, the entropy
	// is log2(3) ≈ 1.585 bits — above the 1.5-bit default cap.
	idx, texts := prewarmGraph(t,
		[]struct{ id, text string }{
			{"m-a", "line a"},
			{"m-b", "line b"},
			{"m-c", "line c"},
			{"m-d", "line d"},
		},
		[]games.MessageEdge{
			{FromID: "m-a", ToID: "m-b", EdgeType: games.EdgeNext, Confidence: 0.8},
			{FromID: "m-a", ToID: "m-c", EdgeType: games.EdgeNext, Confidence: 0.8},
			{FromID: "m-a", ToID: "m-d", EdgeType: games.EdgeNext, Confidence: 0.8},
		})
	e.withGraph(t, idx, texts) // default config: the 1.5-bit cap

	e.commit(t, "occ-a", "m-a", "region-box", 1, "line a", true)

	// The batch was refused: nothing was requested, one batch was skipped.
	if st := e.w.audioInfo().Prewarm; st.Skipped != 1 || st.Requested != 0 {
		t.Fatalf("prewarm = %+v, want skipped 1, requested 0 (the flat successors are too uncertain to prewarm)", st)
	}
	// Line a's own synthesis is the session's only cache write: wait for it
	// to land, then confirm nothing else was generated (the skipped batch
	// enqueued nothing, so the count is stable once line a has landed).
	by := time.Now().Add(10 * time.Second)
	for e.cache.Stats().Generated < 1 {
		if time.Now().After(by) {
			t.Fatalf("the cache holds %d utterance(s), want 1 (a — no successor was prewarmed): %+v", e.cache.Stats().Generated, e.cache.Stats())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := e.cache.Stats().Generated; n != 1 {
		t.Fatalf("the cache holds %d utterance(s), want 1 (a — no successor was prewarmed): %+v", n, e.cache.Stats())
	}

	// The "next" line still commits — it simply pays the round trip.
	e.commit(t, "occ-b", "m-b", "region-box", 2, "line b", true)
	e.waitEvents(t, "line b ready", 10*time.Second, func(evs []events.Envelope) bool {
		for _, ev := range evs {
			if ev.Name == events.SpeechReady && genOfEv(t, ev) == 2 {
				return evPayload(t, ev)["from_cache"] == false
			}
		}
		return false
	})
}
