package runtime

// White-box tests for catch-up narration and the no-backlog speech policy
// (issue #25, ADR-0122): the lines committed while an utterance is playing
// are batched, merged into one TTS request per speaker run, and spoken
// back-to-back; anything older than the batch window is fenced; no utterance
// starts past its max age; and the latest_only policy keeps the ADR-0092
// immediate shape. The tests drive the wiring's onCommit directly with
// synthetic commit envelopes on a controlled clock, the way the fence tests
// do (speech_fence_test.go) — no wall-clock margin between commits can
// reorder what the test observes.
//
// The policy is the product default (catch_up) wherever the test does not
// say otherwise; the ADR-0092 immediate-shape coverage stays on the
// harness-default latest_only suite (speech_fence_test.go and the
// replay-harness tests), which newVoiceEnv pins.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/providers/tts"
	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/speech/dummy"
	"github.com/dbirks/game-narrator/internal/store"
	"github.com/dbirks/game-narrator/internal/voices"
)

// ── fakes ------------------------------------------------------------------

// catchUpTTS is the scripted Base checkpoint for the catch-up tests: a
// deterministic tone whose duration the test controls (so a line can be
// made long enough to still be playing when the next commit lands), a
// synthesise latency the test controls (so the max-age drop can be put
// past its bound), and a record of every request's text and voice — the
// merge assertion reads the text: one request per speaker run, the run's
// lines joined with the core's sentence spacing.
type catchUpTTS struct {
	toneS   float64
	latency time.Duration
	mu      sync.Mutex
	texts   []string
	voices  []string
}

func (f *catchUpTTS) tone() tts.Audio {
	if f.latency > 0 {
		time.Sleep(f.latency)
	}
	return fenceTone(f.toneS)
}

func (f *catchUpTTS) Generate(_ context.Context, req tts.GenerateRequest) (tts.Audio, error) {
	f.record(req.Text, req.Voice)
	return f.tone(), nil
}

func (f *catchUpTTS) GenerateWithReference(_ context.Context, req tts.GenerateRequest, _ []byte, _ string) (tts.Audio, error) {
	f.record(req.Text, req.Voice)
	return f.tone(), nil
}

func (f *catchUpTTS) record(text, voice string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.texts = append(f.texts, text)
	f.voices = append(f.voices, voice)
}

func (f *catchUpTTS) requests() (texts, vs []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.texts...), append([]string(nil), f.voices...)
}

// fenceTone is the deterministic tone the fence and catch-up harnesses
// share (the fence tests' MakeToneWAV shape, one audio).
func fenceTone(toneS float64) tts.Audio {
	wav := speech.MakeToneWAV(440, toneS, 16000)
	return tts.Audio{WAV: wav, SampleRate: 16000, Channels: 1, BitsPerSample: 16,
		Duration: time.Duration(toneS * float64(time.Second)), Model: "fake-base"}
}

// ── environment -------------------------------------------------------------

// catchUpEnv is one white-box speech wiring with the catch-up policy the
// test names, a seeded narrator voice, the text-recording TTS and a
// programmable dummy sink.
type catchUpEnv struct {
	t    *testing.T
	w    *speechWiring
	sink *dummy.Sink
	hub  *events.Hub
	mem  *events.MemStore
	tts  *catchUpTTS
}

// newCatchUpEnv builds the white-box wiring. policy is the catch-up policy
// word (catch_up or latest_only); window and maxAge 0 take the core's
// documented defaults. speakers maps message id → speaker character.
func newCatchUpEnv(t *testing.T, toneS float64, policy string, window, maxAge time.Duration, speakers map[string]string) *catchUpEnv {
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
	fake := &catchUpTTS{toneS: toneS}
	svc := &voices.Service{
		Store: st, Assets: a,
		Design: nil, Base: fake, Registry: nil,
		Speech: cache, InstanceID: "primary",
		BaseModelID:   "fake-base",
		BackendDigest: "test-backend-v1",
	}
	seedFenceVoice(t, st, a)

	sink := dummy.New(1, 5)
	endCtx, endCancel := context.WithCancel(ctx)
	cu, err := (speech.CatchUpOptions{
		Policy:          policy,
		BatchWindow:     window,
		MaxUtteranceAge: maxAge,
	}).NormalizeCatchUpOptions()
	if err != nil {
		t.Fatalf("catch-up options: %v", err)
	}
	w := &speechWiring{
		id:               "catchup-test",
		log:              func(string, ...any) {},
		hub:              hub,
		voices:           svc,
		assets:           a,
		speakerByMessage: speakers,
		narratorFallback: true,
		sem:              make(chan struct{}, speechWorkerSlots),
		utterances:       map[string]*utteranceInfo{},
		byGeneration:     map[int64]string{},
		genCancel:        map[int64]*synthSlot{},
		settled:          map[int64]bool{},
		catchUp:          cu,
		catchUpRuns:      map[int64]*utteranceInfo{},
		lineClaim:        map[string]int64{},
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

	e := &catchUpEnv{t: t, w: w, sink: sink, hub: hub, mem: mem, tts: fake}
	t.Cleanup(func() {
		endCancel()
		_ = w.sched.Stop()
		_ = st.Close()
		_ = hub.Close()
	})
	return e
}

// commit feeds the wiring one message.committed envelope (the fence tests'
// seam, the same payload shape the session reducer's committed event
// carries).
func (e *catchUpEnv) commit(t *testing.T, occ, msgID, region string, generation int64, text string) {
	t.Helper()
	data, _ := json.Marshal(map[string]any{
		"dialogue_occurrence_id": occ,
		"message_id":             msgID,
		"region_id":              region,
		"generation":             generation,
		"text":                   text,
		"speech_eligible":        true,
	})
	e.w.onCommit(events.Envelope{
		SchemaVersion: 1,
		SessionID:     e.w.id,
		TraceID:       "trace-" + occ,
		Name:          events.MessageCommitted,
		Data:          data,
	})
}

// events returns the session's event log, newest last.
func (e *catchUpEnv) events() []events.Envelope {
	evs, err := e.mem.After(context.Background(), 0, events.Filter{SessionID: e.w.id}, 4096)
	if err != nil {
		e.t.Fatalf("read events: %v", err)
	}
	return evs
}

// waitEvents polls the event log until pred holds (or the deadline passes).
func (e *catchUpEnv) waitEvents(t *testing.T, desc string, deadline time.Duration, pred func([]events.Envelope) bool) []events.Envelope {
	t.Helper()
	by := time.Now().Add(deadline)
	for {
		evs := e.events()
		if pred(evs) {
			return evs
		}
		if time.Now().After(by) {
			t.Fatalf("%s: not satisfied within %s (events: %s)", desc, deadline, namesOf(evs))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// namesOf renders the event log compactly for a failure message.
func namesOf(evs []events.Envelope) string {
	var parts []string
	for _, e := range evs {
		parts = append(parts, e.Name)
	}
	return strings.Join(parts, ",")
}

// requestedEvents returns the session's speech.requested envelopes in order.
func requestedEvents(evs []events.Envelope) []events.Envelope {
	var out []events.Envelope
	for _, e := range evs {
		if e.Name == events.SpeechRequested {
			out = append(out, e)
		}
	}
	return out
}

// startedEvents counts the speech.started envelopes per occurrence.
func startedCounts(evs []events.Envelope) map[string]int {
	out := map[string]int{}
	for _, e := range evs {
		if e.Name != events.SpeechStarted {
			continue
		}
		var p struct {
			Occ string `json:"dialogue_occurrence_id"`
		}
		_ = json.Unmarshal(e.Data, &p)
		out[p.Occ]++
	}
	return out
}

// lineTerminals returns, per occurrence, the (result, reason) of the line's
// settled terminal: the speech.ended envelope the wiring emits for the line
// (result skipped/ended/failed with the reason the policy gave).
func lineTerminals(evs []events.Envelope) map[string][2]string {
	out := map[string][2]string{}
	for _, e := range evs {
		if e.Name != events.SpeechEnded {
			continue
		}
		var p struct {
			Occ    string `json:"dialogue_occurrence_id"`
			Result string `json:"result"`
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(e.Data, &p); err != nil {
			continue
		}
		if p.Occ == "" {
			continue
		}
		out[p.Occ] = [2]string{p.Result, p.Reason}
	}
	return out
}

// ── tests -------------------------------------------------------------------

// TestSpeechCatchUpSpeaksEachPageOnItsOwn is the ADR-0175 amendment's
// white-box form of the issue's replay shape: four rapid boxes from two
// speakers (A, A, B, B). The first line speaks immediately (nothing is
// playing when it commits); the next three commit while audio is still in
// the air, so they batch. The flush speaks EVERY page as its own
// utterance, in commit order — each page's own cached clip, back-to-back —
// never a re-synthesis of a merged text (the merge is a cache miss), and
// never a stale drop of a page the player passed. Every line settles
// exactly once with result ended.
func TestSpeechCatchUpSpeaksEachPageOnItsOwn(t *testing.T) {
	speakers := map[string]string{
		"line-a1": "keeper",
		"line-a2": "keeper",
		"line-b1": "muse",
		"line-b2": "muse",
	}
	// A long window (2 s) so the window's own timer cannot flush between the
	// commits: the flush the test asserts on is the playback terminal
	// (the first line's 1.2 s tone running out), not the timer.
	e := newCatchUpEnv(t, 1.2, "catch_up", 2*time.Second, 0, speakers)

	texts := map[string]string{
		"line-a1": "The keeper set the lamp on the sill.",
		"line-a2": "He had set it there every night for a week.",
		"line-b1": "The muse asked who was keeping the light.",
		"line-b2": "Nobody, she said, only the harbour.",
	}

	// The four rapid boxes: the first speaks immediately; the next three
	// commit at 150 ms intervals, while the first's tone (1.2 s) is still
	// playing, so all three take the batch.
	e.commit(t, "occ-a1", "line-a1", "bottom", 1, texts["line-a1"])
	time.Sleep(150 * time.Millisecond)
	e.commit(t, "occ-a2", "line-a2", "bottom", 2, texts["line-a2"])
	time.Sleep(150 * time.Millisecond)
	e.commit(t, "occ-b1", "line-b1", "bottom", 3, texts["line-b1"])
	time.Sleep(150 * time.Millisecond)
	e.commit(t, "occ-b2", "line-b2", "bottom", 4, texts["line-b2"])

	// Every line settles: four line terminals, each result ended, no
	// skipped and no failed — nothing is lost to staleness anymore.
	evs := e.waitEvents(t, "all four lines settled", 15*time.Second, func(evs []events.Envelope) bool {
		term := lineTerminals(evs)
		for _, occ := range []string{"occ-a1", "occ-a2", "occ-b1", "occ-b2"} {
			if term[occ][0] != "ended" {
				return false
			}
		}
		return true
	})

	// One TTS request per page, each carrying the page's OWN text — no
	// merged text anywhere (the per-page key is the presynth's cache key:
	// a hit, not a re-synthesis). The requests land in no guaranteed order
	// (the per-page workers synthesise in parallel) — the order is the
	// playback's, asserted on the sink below — so the batch's texts are
	// checked as a set, each exactly once.
	textsOut, _ := e.tts.requests()
	if len(textsOut) != 4 {
		t.Fatalf("TTS requests = %d (%v), want 4 (one per page, unmerged)", len(textsOut), textsOut)
	}
	if textsOut[0] != texts["line-a1"] {
		t.Errorf("first request = %q, want the first page spoken at once", textsOut[0])
	}
	seen := map[string]int{}
	for _, txt := range textsOut[1:] {
		seen[txt]++
	}
	for _, want := range []string{texts["line-a2"], texts["line-b1"], texts["line-b2"]} {
		if seen[want] != 1 {
			t.Errorf("batch texts %v: %q missing or doubled (one unmerged request per page, in commit order at playback)", seen, want)
		}
	}

	// No merged request: every speech.requested carries the page's own bare
	// message id, the presynth cache key form, and merged=false.
	for _, r := range requestedEvents(evs) {
		var p struct {
			Merged    bool   `json:"merged"`
			MessageID string `json:"message_id"`
		}
		if err := json.Unmarshal(r.Data, &p); err != nil {
			t.Fatalf("unmarshal request: %v", err)
		}
		if p.Merged {
			t.Errorf("a speech.requested is merged (%q): the amendment speaks per-page clips, not merged re-syntheses", p.MessageID)
		}
		if speech.IsMergedOriginalID(p.MessageID) {
			t.Errorf("request message_id = %q, want the page's own bare id (the presynth cache key)", p.MessageID)
		}
	}

	// One playback per page, in commit order (the sink's record per
	// generation: a1 is generation 1, the batch's pages ride on
	// increasing generations in commit order).
	if n := e.sink.Starts(); n != 4 {
		t.Fatalf("the sink started %d playbacks, want 4 (one per page)", n)
	}
	byGen := map[int64]bool{}
	for _, rec := range e.sink.Records() {
		if rec.Started.IsZero() {
			continue
		}
		if _, dup := byGen[rec.Generation]; dup {
			t.Errorf("two playbacks of generation %d: a page spoke more than once", rec.Generation)
		}
		byGen[rec.Generation] = true
	}
	if len(byGen) != 4 {
		t.Fatalf("playback generations = %v, want 4 distinct pages", byGen)
	}

	// Every line settles exactly once, and as spoken (ended): the
	// one-terminal-per-line rule (ADR-0092, carried onto the batch's lines
	// by the line's claim).
	for _, occ := range []string{"occ-a1", "occ-a2", "occ-b1", "occ-b2"} {
		n := 0
		for _, ev := range evs {
			if ev.Name != events.SpeechEnded {
				continue
			}
			var p struct {
				Occ string `json:"dialogue_occurrence_id"`
				Res string `json:"result"`
			}
			if err := json.Unmarshal(ev.Data, &p); err != nil {
				continue
			}
			if p.Occ == occ && p.Res == "ended" {
				n++
			}
		}
		if n != 1 {
			t.Errorf("line %s ended terminals = %d, want 1 (every message spoken exactly once)", occ, n)
		}
	}
}

// TestSpeechCatchUpMashSixPagesClipsBackToBack is the owner's mash in
// white-box form: six cached pages committed on a fast cadence through the
// whole conversation, independent of the speech state. Every page is spoken
// exactly once, in commit order, as its own clip — none dropped — and the
// gap between one clip's end and the next clip's start stays under 150 ms:
// the back-to-back playback behind "cue up all the speech really quick, and
// then just say all that".
func TestSpeechCatchUpMashSixPagesClipsBackToBack(t *testing.T) {
	e := newCatchUpEnv(t, 0.3, "catch_up", 2*time.Second, 0, nil)
	for i := 1; i <= 6; i++ {
		e.commit(t, fmt.Sprintf("occ-m%d", i), fmt.Sprintf("line-m%d", i), "bottom", int64(i), fmt.Sprintf("Page %d of the mash.", i))
		if i < 6 {
			time.Sleep(150 * time.Millisecond)
		}
	}
	evs := e.waitEvents(t, "all six pages settled", 15*time.Second, func(evs []events.Envelope) bool {
		term := lineTerminals(evs)
		for i := 1; i <= 6; i++ {
			if term[fmt.Sprintf("occ-m%d", i)][0] != "ended" {
				return false
			}
		}
		return true
	})
	// None dropped, in order, as its own clip: six distinct playbacks on
	// increasing generations in commit order, gaps under the bound.
	records := e.sink.Records()
	byGen := map[int64]dummy.Record{}
	for _, rec := range records {
		if !rec.Started.IsZero() {
			byGen[rec.Generation] = rec
		}
	}
	if len(byGen) != 6 {
		t.Fatalf("playback generations = %d, want 6 (every page's own clip, none dropped)", len(byGen))
	}
	var gens []int64
	for g := range byGen {
		gens = append(gens, g)
	}
	sort.Slice(gens, func(i, j int) bool { return gens[i] < gens[j] })
	for i := 1; i < len(gens); i++ {
		if !byGen[gens[i]].Started.After(byGen[gens[i-1]].Started) {
			t.Fatalf("page generation %d started before %d: the pages must play in commit order", gens[i], gens[i-1])
		}
		if gap := byGen[gens[i]].Started.Sub(byGen[gens[i-1]].Ended); gap > 150*time.Millisecond {
			t.Errorf("gap between clip %d and clip %d = %s, want at most 150 ms (back-to-back)", gens[i-1], gens[i], gap)
		}
	}
	_ = evs
}

// TestSpeechCatchUpNoStaleDrop is the ADR-0175 amendment's half of the old
// no-backlog policy: a line the player passed is NEVER dropped for
// staleness. The shapes that used to drop — a commit more than the batch
// window after the pending line, and a flush that lands more than the
// per-utterance max age after the commit — now both speak the line.
func TestSpeechCatchUpNoStaleDrop(t *testing.T) {
	t.Run("window_stale_spoken", func(t *testing.T) {
		// The window (300 ms) is shorter than the gap between the commits:
		// the old window's fence would have dropped b1 as catch_up_stale on
		// the newer commit's arrival; the amendment speaks it instead.
		e := newCatchUpEnv(t, 0.4, "catch_up", 300*time.Millisecond, 0, nil)
		e.commit(t, "occ-a1", "line-a1", "bottom", 1, "The first line holds the playback.")
		time.Sleep(150 * time.Millisecond)
		// Batches: the first line's tone (0.4 s) is still playing.
		e.commit(t, "occ-b1", "line-b1", "bottom", 2, "The second line waits in the batch.")
		time.Sleep(250 * time.Millisecond)
		// More than the window after b1, while b1 is still pending.
		e.commit(t, "occ-c1", "line-c1", "bottom", 3, "A third line arrives past the window.")

		e.waitEvents(t, "the batch spoke every line", 8*time.Second, func(evs []events.Envelope) bool {
			term := lineTerminals(evs)
			return term["occ-b1"][0] == "ended" && term["occ-c1"][0] == "ended"
		})
		if n := e.sink.Starts(); n != 3 {
			t.Fatalf("the sink started %d playbacks, want 3 (no stale drop)", n)
		}
	})

	t.Run("flush_past_max_age_spoken", func(t *testing.T) {
		// The flush lands more than the (tiny) max age after the commit:
		// the old flush's age drop would have fenced it; the amendment
		// speaks it.
		e := newCatchUpEnv(t, 0.4, "catch_up", 2*time.Second, 100*time.Millisecond, nil)
		e.commit(t, "occ-a1", "line-a1", "bottom", 1, "The first line holds the playback.")
		time.Sleep(150 * time.Millisecond)
		e.commit(t, "occ-b1", "line-b1", "bottom", 2, "The second line waits in the batch.")

		e.waitEvents(t, "the batch spoke", 8*time.Second, func(evs []events.Envelope) bool {
			return lineTerminals(evs)["occ-b1"][0] == "ended"
		})
		if n := e.sink.Starts(); n != 2 {
			t.Fatalf("the sink started %d playbacks, want 2", n)
		}
	})
}

// TestSpeechCatchUpBacklogCap is the amendment's safety bound: a page the
// player passed is spoken, but the narrator must not lag unboundedly. The
// cap (4 s of estimated audio) bounds the pending batch: the OLDEST pages
// beyond it are dropped as skipped/catch_up_backlog_cap, counted and
// logged; the rest speak in order.
func TestSpeechCatchUpBacklogCap(t *testing.T) {
	e := newCatchUpEnv(t, 1.2, "catch_up", 30*time.Second, 0, nil)
	e.w.catchUp.BacklogCap = 4 * time.Second // ten words at the estimate's 400 ms/word
	// Five-word pages: 2 s of estimated audio each. c1 holds playback with
	// its 1.2 s tone while c2..c6 commit, so all five take the batch.
	e.commit(t, "occ-c1", "line-c1", "bottom", 1, "first page of the backlog cap test")
	for i := 2; i <= 6; i++ {
		time.Sleep(20 * time.Millisecond)
		e.commit(t, fmt.Sprintf("occ-c%d", i), fmt.Sprintf("line-c%d", i), "bottom", int64(i),
			fmt.Sprintf("page %d five words in length", i))
	}
	evs := e.waitEvents(t, "the backlog settled", 12*time.Second, func(evs []events.Envelope) bool {
		term := lineTerminals(evs)
		return term["occ-c2"][0] != "" && term["occ-c6"][0] == "ended" && term["occ-c5"][0] == "ended"
	})
	term := lineTerminals(evs)
	// The oldest pages beyond the cap are the cap's drops — and no other
	// page is lost: every commit re-measures the pending batch (five
	// words = 2 s of estimate each, cap 4 s), so c4's arrival caps c2,
	// c5's caps c3 and c6's caps c4; c5 and c6 stay inside the bound and
	// speak, c1 played from the start.
	for _, occ := range []string{"occ-c2", "occ-c3", "occ-c4"} {
		if term[occ] != [2]string{"skipped", "catch_up_backlog_cap"} {
			t.Errorf("%s terminal = %v, want skipped/catch_up_backlog_cap (the cap's oldest-first drop)", occ, term[occ])
		}
	}
	for _, occ := range []string{"occ-c1", "occ-c5", "occ-c6"} {
		if term[occ][0] != "ended" {
			t.Errorf("%s terminal = %v, want ended (within the cap, or already playing)", occ, term[occ])
		}
	}
	if n := int(e.w.catchUpBacklogDropped); n != 3 {
		t.Fatalf("backlog cap drops = %d, want 3 (counted)", n)
	}
	// The capped pages were never synthesised; the in-bound ones spoke.
	textsOut, _ := e.tts.requests()
	for _, txt := range textsOut {
		if strings.Contains(txt, "page 2 ") || strings.Contains(txt, "page 3 ") || strings.Contains(txt, "page 4 ") {
			t.Errorf("TTS requested the capped page in %q: the cap drops before synthesis", txt)
		}
	}
	if n := e.sink.Starts(); n != 3 {
		t.Fatalf("the sink started %d playbacks, want 3 (c1 and c5..c6)", n)
	}
	_ = evs
}

// TestSpeechCatchUpRecommitWhilePlayingIsADuplicate is the ADR-0175
// amendment's live finding (the 2026-10-24 catch-up bundle, run
// catchup-mash-20261024T035836Z): a page whose first copy is still AUDIBLE
// is not passed again — its re-commit (the typewriter's partial→settled
// re-read, 3.3 s past the 3 s cross-region window) skips as
// duplicate_region and speaks nothing. The window alone cannot catch it
// (it is a time bound, and the live re-commits landed past it — the second
// one from the same region no less).
func TestSpeechCatchUpRecommitWhilePlayingIsADuplicate(t *testing.T) {
	e := newCatchUpEnv(t, 4, "catch_up", 30*time.Second, 0, nil)
	e.commit(t, "occ-a1", "line-a1", "bottom", 1, "The first copy of the page holds the playback.")
	e.waitEvents(t, "the first copy started", 5*time.Second, func(evs []events.Envelope) bool {
		for _, ev := range evs {
			if ev.Name == events.SpeechStarted {
				return true
			}
		}
		return false
	})
	// Past the 3 s cross-region window, while the tone still plays.
	time.Sleep(3200 * time.Millisecond)
	e.commit(t, "occ-b1", "line-a1", "bottom", 2, "The re-commit of the same page while its copy still plays.")
	ev := e.waitEvents(t, "the re-commit settled", 8*time.Second, func(evs []events.Envelope) bool {
		return lineTerminals(evs)["occ-b1"][0] != ""
	})
	if term := lineTerminals(ev)["occ-b1"]; term != [2]string{"skipped", "duplicate_region"} {
		t.Fatalf("re-commit terminal = %v, want skipped/duplicate_region (the page is still audible — the player did not pass it again)", term)
	}
	// The tone runs to its end (the sink records the record at the end).
	e.waitEvents(t, "the first copy ended", 8*time.Second, func(evs []events.Envelope) bool {
		return lineTerminals(evs)["occ-a1"][0] == "ended"
	})
	if n := e.sink.Starts(); n != 1 {
		t.Fatalf("the sink started %d playbacks, want 1 (the duplicate speaks nothing)", n)
	}
}

// TestSpeechCatchUpBoxChangeFlushes is the box-change priority hook
// (ADR-0122 decision 7): a box change flushes the pending batch on the spot,
// ahead of the window — the merged playback starts on the new box instead of
// at the window's end. The window is 30 s (it would never fire first); the
// box change at ~200 ms is what flushes.
func TestSpeechCatchUpBoxChangeFlushes(t *testing.T) {
	e := newCatchUpEnv(t, 0.4, "catch_up", 30*time.Second, 0, nil)
	e.commit(t, "occ-a1", "line-a1", "bottom", 1, "The first line holds the playback.")
	time.Sleep(150 * time.Millisecond)
	e.commit(t, "occ-b1", "line-b1", "bottom", 2, "The second line waits in the batch.")
	// Still in the batch at 200 ms: the TTS has not been asked for the second
	// line yet (the window would not fire for 30 s, the first tone ends at
	// 400 ms — neither has happened).
	if textsOut, _ := e.tts.requests(); len(textsOut) != 1 {
		t.Fatalf("TTS requests before the box change = %d, want 1 (the batch is still pending)", len(textsOut))
	}
	// The player advanced to a new box: the batch flushes on the spot.
	e.w.NotifyBoxChange()
	e.waitEvents(t, "the batch flushed on the box change", 5*time.Second, func(evs []events.Envelope) bool {
		return lineTerminals(evs)["occ-b1"][0] == "ended"
	})
	if n := e.sink.Starts(); n != 2 {
		t.Fatalf("the sink started %d playbacks, want 2", n)
	}
}

// TestSpeechCatchUpPolicySwitch is the settings control's live move (issue
// #25: the policy is a session setting): catch_up → latest_only with a batch
// pending flushes the batch now (the committed lines speak; the later commits
// run the immediate shape), and latest_only keeps batching nothing.
func TestSpeechCatchUpPolicySwitch(t *testing.T) {
	e := newCatchUpEnv(t, 0.4, "catch_up", 30*time.Second, 0, nil)
	e.commit(t, "occ-a1", "line-a1", "bottom", 1, "The first line holds the playback.")
	time.Sleep(150 * time.Millisecond)
	e.commit(t, "occ-b1", "line-b1", "bottom", 2, "The second line waits in the batch.")

	// The operator chooses the immediate shape with the batch still pending:
	// the batch is committed speech, so it speaks (flushed on the switch)
	// instead of being dropped.
	e.w.setCatchUp("latest_only")
	e.waitEvents(t, "the pending batch spoke after the switch", 5*time.Second, func(evs []events.Envelope) bool {
		return lineTerminals(evs)["occ-b1"][0] == "ended"
	})

	// A later commit runs the immediate shape: no batch, no merge — the TTS
	// is asked for the line at once, and the older audio is fenced at the
	// commit (the ADR-0092 behaviour the switch restores).
	time.Sleep(500 * time.Millisecond)
	e.commit(t, "occ-c1", "line-c1", "bottom", 3, "The third line speaks at once.")
	e.waitEvents(t, "the third line spoke at once", 5*time.Second, func(evs []events.Envelope) bool {
		return lineTerminals(evs)["occ-c1"][0] == "ended"
	})
	textsOut, _ := e.tts.requests()
	if len(textsOut) != 3 {
		t.Fatalf("TTS requests = %d (%v), want 3 (one per line: the switch restored the immediate shape)", len(textsOut), textsOut)
	}
	for i, want := range []string{"The first line holds the playback.", "The second line waits in the batch.", "The third line speaks at once."} {
		if textsOut[i] != want {
			t.Errorf("request %d = %q, want %q (no merge under latest_only)", i, textsOut[i], want)
		}
	}
}

// TestSpeechCatchUpPauseSettlesTheBatch is the pause's half of the policy: a
// pause with a batch pending terminalises the pending lines (skipped, the
// session's pause is the reason they did not speak) — the batch is not
// spoken after the resume of a session that is paused, and no line is left
// without its terminal.
func TestSpeechCatchUpPauseSettlesTheBatch(t *testing.T) {
	e := newCatchUpEnv(t, 0.4, "catch_up", 30*time.Second, 0, nil)
	e.commit(t, "occ-a1", "line-a1", "bottom", 1, "The first line holds the playback.")
	time.Sleep(150 * time.Millisecond)
	e.commit(t, "occ-b1", "line-b1", "bottom", 2, "The second line waits in the batch.")

	e.w.pause()

	e.waitEvents(t, "the batch settled on the pause and the playing line stopped", 5*time.Second, func(evs []events.Envelope) bool {
		if lineTerminals(evs)["occ-b1"][0] == "" {
			return false
		}
		// a1's playback stop (the scheduler's terminal for it) must be in the
		// log too: the sink's record lands with the stop, and the starts count
		// asserts on it.
		for _, ev := range evs {
			if ev.Name != events.SpeechEnded {
				continue
			}
			var p struct {
				Occ string `json:"dialogue_occurrence_id"`
			}
			if err := json.Unmarshal(ev.Data, &p); err == nil && p.Occ == "occ-a1" {
				return true
			}
		}
		return false
	})
	term := lineTerminals(e.events())
	if term["occ-b1"][0] != "skipped" {
		t.Fatalf("batched line terminal on pause = %v, want skipped (the paused session does not speak the batch)", term["occ-b1"])
	}
	if n := e.sink.Starts(); n != 1 {
		t.Fatalf("the sink started %d playbacks, want 1 (the first line only)", n)
	}
}

// TestSpeechCatchUpLatestOnlyIsImmediate pins the other policy word in this
// suite: with latest_only the wiring batches nothing — every commit takes
// the immediate path (the ADR-0092 shape), and the fence cancels the older
// generation at the new commit the way it always has.
func TestSpeechCatchUpLatestOnlyIsImmediate(t *testing.T) {
	e := newCatchUpEnv(t, 1.2, "latest_only", 0, 0, nil)
	e.commit(t, "occ-a1", "line-a1", "bottom", 1, "The first line is cut by the second.")
	time.Sleep(150 * time.Millisecond)
	e.commit(t, "occ-b1", "line-b1", "bottom", 2, "The second line speaks at once.")

	e.waitEvents(t, "the second line spoke", 8*time.Second, func(evs []events.Envelope) bool {
		return lineTerminals(evs)["occ-b1"][0] == "ended"
	})
	evs := e.events()
	// The first line's audio is cancelled at the advance (the ADR-0092
	// reason), not batched and not spoken to the end.
	var cancelled bool
	for _, ev := range evs {
		if ev.Name != events.SpeechCancelled {
			continue
		}
		var p struct {
			Occ string `json:"dialogue_occurrence_id"`
		}
		_ = json.Unmarshal(ev.Data, &p)
		if p.Occ == "occ-a1" {
			cancelled = true
		}
	}
	if !cancelled {
		t.Errorf("the first line was not cancelled at the advance: the latest_only shape must keep the ADR-0092 cancel")
	}
	textsOut, _ := e.tts.requests()
	if len(textsOut) != 2 {
		t.Fatalf("TTS requests = %d (%v), want 2 (one per line, unmerged)", len(textsOut), textsOut)
	}
}

// TestSpeechCatchUpIdleBatchFlushesFast is the idle flush (#41): a line that
// batches while NOTHING is audible — the earlier line is still being
// synthesised, not playing — has no playback to wait behind, so the batch
// flushes after the short coalescing delay instead of the window. The window
// here is 30 s: before the fix the newest line waited for it (the A/V harness
// measured the 900 ms default as an idle line's settled→audio).
func TestSpeechCatchUpIdleBatchFlushesFast(t *testing.T) {
	e := newCatchUpEnv(t, 0.4, "catch_up", 30*time.Second, 0, nil)
	e.tts.latency = 300 * time.Millisecond // line A is in synthesis, not playing
	e.commit(t, "occ-a1", "line-a1", "bottom", 1, "The first line is still being voiced.")
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	e.commit(t, "occ-b1", "line-b1", "bottom", 2, "The second line must not wait the window.")
	e.waitEvents(t, "the idle batch spoke", 5*time.Second, func(evs []events.Envelope) bool {
		return lineTerminals(evs)["occ-b1"][0] == "ended"
	})
	// The flush (120 ms) + one 300 ms synthesis + the 0.4 s tone, far inside
	// the 30 s window the old path waited for.
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("the idle batch took %s to speak; it waited for the window", d)
	}
}

// TestCommitPrecedesItsSpeechEvents: through the commit watcher (the real
// publisher chain), a commit is in the stream before the speech events it
// causes — the speech.requested of a spoken line and the skipped terminal of
// a cross-region duplicate. Routing first put them ahead of their commit, and
// the live catch-up test lost a duplicate's terminal.
func TestCommitPrecedesItsSpeechEvents(t *testing.T) {
	e := newCatchUpEnv(t, 0.2, "catch_up", 0, 0, nil)
	pub := &commitWatcher{w: e.w, inner: e.hub}
	commit := func(occ, region string, gen int64) {
		data, _ := json.Marshal(map[string]any{
			"dialogue_occurrence_id": occ, "message_id": "m-order", "region_id": region,
			"generation": gen, "text": "A line spoken once.", "speech_eligible": true,
		})
		if _, err := pub.Publish(context.Background(), events.Envelope{
			SchemaVersion: 1, SessionID: e.w.id, Name: events.MessageCommitted,
			OccurredAt: time.Now().UTC(), Data: data,
		}); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}
	commit("occ-a", "bottom", 1)
	commit("occ-b", "middle", 2) // the same line from another region: a duplicate
	evs := e.waitEvents(t, "the duplicate's terminal", 5*time.Second, func(evs []events.Envelope) bool {
		for _, ev := range evs {
			if ev.Name == events.SpeechEnded && strings.Contains(string(ev.Data), "occ-b") {
				return true
			}
		}
		return false
	})
	seen := map[string]bool{}
	for _, ev := range evs {
		var d struct {
			Occ string `json:"dialogue_occurrence_id"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		switch ev.Name {
		case events.MessageCommitted:
			seen[d.Occ] = true
		case events.SpeechRequested, events.SpeechEnded:
			if d.Occ != "" && !seen[d.Occ] {
				t.Fatalf("%s for %s precedes its message.committed in the stream (%s)", ev.Name, d.Occ, namesOf(evs))
			}
		}
	}
}
