package runtime_test

// The speech wiring's unit tests (ADR-0039): a committed line resolves the
// speaker's approved voice, synthesises through the shared TTS cache and
// plays on the generation-fenced scheduler onto the configured sink. The
// environment is the production shape — real store, hub, pipeline and
// synthetic replay — with two seams: the OCR engine (the pipeline's own
// scripted truth harness) and the TTS client (a fake that returns
// deterministic tones and records its requests). The sinks are the real
// packages: the programmable dummy for lifecycle assertions, the file sink
// where the assertion is on the samples themselves.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/providers/tts"
	"github.com/dbirks/game-narrator/internal/runtime"
	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/speech/dummy"
	"github.com/dbirks/game-narrator/internal/store"
	"github.com/dbirks/game-narrator/internal/voices"
)

// ── fakes ------------------------------------------------------------------

// fakeTTS is the scripted Base checkpoint: it records every request (the
// voice identity is the assertion) and answers with a deterministic tone —
// a real, validated WAV whose duration the test controls, so a fast advance
// can interrupt a playback that is genuinely still going.
type fakeTTS struct {
	mu    sync.Mutex
	refs  []string // the voice of every request, in order
	calls int
	toneS float64
}

func (f *fakeTTS) Generate(_ context.Context, req tts.GenerateRequest) (tts.Audio, error) {
	f.record(req.Voice)
	return f.toneAudio(), nil
}

func (f *fakeTTS) GenerateWithReference(_ context.Context, req tts.GenerateRequest, _ []byte, _ string) (tts.Audio, error) {
	f.record(req.Voice + ":ref")
	return f.toneAudio(), nil
}

func (f *fakeTTS) record(v string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.refs = append(f.refs, v)
}

func (f *fakeTTS) toneAudio() tts.Audio {
	wav := speech.MakeToneWAV(440, f.toneS, 16000)
	return tts.Audio{WAV: wav, SampleRate: 16000, Channels: 1, BitsPerSample: 16,
		Duration: time.Duration(f.toneS * float64(time.Second)), Model: "fake-base"}
}

func (f *fakeTTS) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeTTS) voicesUsed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.refs))
	copy(out, f.refs)
	return out
}

// fakeDesign is the unexercised VoiceDesign seam (the wiring never designs).
type fakeDesign struct{ tts *fakeTTS }

func (f fakeDesign) Design(context.Context, tts.DesignRequest) (tts.Audio, error) {
	return f.tts.toneAudio(), nil
}

// fakeRegistry is the unexercised registry seam (the registration row is
// seeded directly, the state the approval flow leaves behind).
type fakeRegistry struct{}

func (fakeRegistry) Ensure(context.Context, []tts.Voice) (tts.EnsureReport, error) {
	return tts.EnsureReport{}, nil
}

func (fakeRegistry) List(context.Context) (tts.ListResult, error) {
	return tts.ListResult{}, nil
}

// ── environment -------------------------------------------------------------

const testVoiceName = "gn_test_narrator_v1"

type voiceEnv struct {
	t         *testing.T
	ctx       context.Context
	st        *store.Store
	hub       *events.Hub
	mem       *events.MemStore
	mgr       *runtime.Manager
	profileID string
	svc       *voices.Service
	assets    *assets.Local
	tts       *fakeTTS
	voiceID   string
}

// newVoiceEnv builds the speech environment: the production shape with the
// OCR engine and the TTS client as seams. seedVoice gives the narrator
// character an approved, registered voice. A non-nil sink is supplied
// through the test seam; nil selects the file sink under fileSinkDir.
// manual is the explicitly-driven frame source seam (Options.ManualSource,
// docs/spec/12's deterministic delay-injection suite); nil (the default)
// leaves the kind refused and the session runs the synthetic replay.
func newVoiceEnv(t *testing.T, ocrDelay, toneS time.Duration, seedVoice bool, sink speech.AudioSink, fileSinkDir string, manual func() (capture.Source, error)) *voiceEnv {
	t.Helper()
	corpusPresent(t)
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
	fake := &fakeTTS{toneS: toneS.Seconds()}
	svc := &voices.Service{
		Store: st, Assets: a,
		Design: fakeDesign{tts: fake}, Base: fake, Registry: fakeRegistry{},
		Speech: cache, InstanceID: "primary",
		BaseModelID:   "fake-base",
		BackendDigest: "test-backend-v1",
	}
	if err := voices.New(svc); err != nil {
		t.Fatalf("voices.New: %v", err)
	}

	voiceID := ""
	if seedVoice {
		voiceID = seedNarratorVoice(t, st, a)
	}

	e := &voiceEnv{t: t, ctx: ctx, st: st, hub: hub, mem: mem, svc: svc, assets: a, tts: fake, voiceID: voiceID}
	opts := runtime.SpeechOptions{Voices: svc, Assets: a}
	if sink != nil {
		opts.SinkFactory = func() (speech.AudioSink, error) { return sink, nil }
	} else {
		opts.Sink = runtime.SinkFile
		opts.FileSinkDir = fileSinkDir
	}
	mgr, err := runtime.New(runtime.Options{
		Store:        st,
		Hub:          hub,
		AssetsRoot:   t.TempDir(),
		ReplayRoots:  []string{absPath(t, syntheticDir)},
		OCR:          &fakeOCR{script: truthScript(t), delay: ocrDelay},
		Speech:       &opts,
		ManualSource: manual,
		Log:          slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}
	e.mgr = mgr
	e.profileID, _ = seedSyntheticProfile(t, st, true)
	// Cleanups run LIFO: the manager before the hub, the hub before the store.
	t.Cleanup(func() { _ = st.Close() })
	t.Cleanup(func() { _ = e.hub.Close() })
	t.Cleanup(mgr.Close)
	return e
}

// seedNarratorVoice gives the narrator character one approved, ready
// revision: the reference is a committed tone asset, the registration row is
// the "ready on this instance" state the approval flow leaves behind.
func seedNarratorVoice(t *testing.T, st *store.Store, a *assets.Local) string {
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
	err = st.CreateVoiceRevision(ctx, store.VoiceProfile{
		ID:                profID,
		Character:         "narrator",
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
	})
	if err != nil {
		t.Fatalf("creating approved revision: %v", err)
	}
	verified := time.Now().UTC()
	err = st.UpsertVoiceRegistration(ctx, store.VoiceRegistration{
		VoiceProfileID: profID,
		InstanceID:     "primary",
		RegisteredName: testVoiceName,
		LastVerifiedAt: &verified,
		State:          store.VoiceRegReady,
	})
	if err != nil {
		t.Fatalf("recording registration: %v", err)
	}
	return profID
}

// startReplay starts the clip on the env's manager; the profile is seeded
// once per env (the store's slug is unique), so the second run of the same
// clip reuses the same published profile — which is also what makes the
// cache key identical across runs.
func (e *voiceEnv) startReplay() string {
	t := e.t
	info, err := e.mgr.Start(e.ctx, runtime.StartRequest{
		ProfileID:   e.profileID,
		FrameSource: runtime.FrameSource{Kind: runtime.SourceKindReplay, Dir: syntheticClipDir()},
		Mode:        runtime.ModeConservative,
	})
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	return info.ID
}

// sessionEvents reads the session's event log, newest last.
func (e *voiceEnv) sessionEvents(sessionID string) []events.Envelope {
	t := e.t
	evs, err := e.mem.After(e.ctx, 0, events.Filter{SessionID: sessionID}, 4096)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	return evs
}

// waitEvents polls the session's event log until the predicate holds (or the
// deadline passes): the speech workers run beside the pipeline, so a
// terminal session state alone does not prove the audio settled.
func (e *voiceEnv) waitEvents(t *testing.T, sessionID, desc string, deadline time.Duration, pred func([]events.Envelope) bool) []events.Envelope {
	t.Helper()
	by := time.Now().Add(deadline)
	for {
		evs := e.sessionEvents(sessionID)
		if pred(evs) {
			return evs
		}
		if time.Now().After(by) {
			for _, ev := range evs {
				t.Logf("   %d %s %s", ev.Seq, ev.Name, string(ev.Data))
			}
			t.Fatalf("%s: not satisfied within %s (have %d events)", desc, deadline, len(evs))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// payload decodes one event's data into a map.
func payload(t *testing.T, ev events.Envelope) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(ev.Data, &m); err != nil {
		t.Fatalf("decode %s payload: %v", ev.Name, err)
	}
	return m
}

// genOf reads the "generation" field out of one of the events that carry
// it (message.committed and the speech events all do).
func genOf(t *testing.T, ev events.Envelope) int64 {
	t.Helper()
	f, ok := payload(t, ev)["generation"].(float64)
	if !ok {
		t.Fatalf("%s payload has no numeric generation: %v", ev.Name, payload(t, ev))
	}
	return int64(f)
}

// commitGenOf is the committing generation of the n-th (1-based) commit;
// ok is false while it has not landed yet (poll predicates use that).
func commitGenOf(t *testing.T, evs []events.Envelope, n int) (int64, bool) {
	t.Helper()
	i := 0
	for _, ev := range evs {
		if ev.Name != events.MessageCommitted {
			continue
		}
		i++
		if i == n {
			return genOf(t, ev), true
		}
	}
	return 0, false
}

func mustCommitGen(t *testing.T, evs []events.Envelope, n int) int64 {
	t.Helper()
	g, ok := commitGenOf(t, evs, n)
	if !ok {
		t.Fatalf("the log has no %d-th commit", n)
	}
	return g
}

// ── tests -------------------------------------------------------------------

// TestSpeechCommitLifecycle is the core contract: a committed line resolves
// the narrator's approved voice, synthesises it, and the scheduler plays it
// — one started and one ended, carrying the right voice, the committing
// generation and the committing frame's trace id.
func TestSpeechCommitLifecycle(t *testing.T) {
	sink := dummy.New(0, 0)
	e := newVoiceEnv(t, 10*time.Millisecond, 100*time.Millisecond, true, sink, "", nil)
	sid := e.startReplay()
	defer waitState(t, e.mgr, sid, 30*time.Second)

	evs := e.waitEvents(t, sid, "first utterance ended", 20*time.Second,
		func(evs []events.Envelope) bool {
			for _, ev := range evs {
				if ev.Name == events.SpeechEnded && payload(t, ev)["result"] == "ended" {
					return true
				}
			}
			return false
		})

	var commits, started, ended []events.Envelope
	for _, ev := range evs {
		switch ev.Name {
		case events.MessageCommitted:
			commits = append(commits, ev)
		case events.SpeechStarted:
			started = append(started, ev)
		case events.SpeechEnded:
			if payload(t, ev)["result"] == "ended" {
				ended = append(ended, ev)
			}
		}
	}
	if len(commits) < 1 || len(started) < 1 || len(ended) < 1 {
		t.Fatalf("want a commit, a start and a natural end; have %d commits, %d starts, %d ends",
			len(commits), len(started), len(ended))
	}

	// The pair: the started and the ended describe one utterance.
	s, sEnd := payload(t, started[0]), payload(t, ended[0])
	if s["utterance_id"] == nil || sEnd["utterance_id"] == nil || s["utterance_id"] != sEnd["utterance_id"] {
		t.Fatalf("started/ended do not describe one utterance: %v / %v", s, sEnd)
	}
	if s["generation"] != sEnd["generation"] {
		t.Fatalf("started/ended disagree on the generation: %v / %v", s["generation"], sEnd["generation"])
	}
	// The voice: the narrator's approved profile on the event, the
	// registered name on the wire (the fake TTS recorded the exact request).
	if s["voice_id"] != e.voiceID {
		t.Fatalf("started voice_id = %v, want the narrator profile %s", s["voice_id"], e.voiceID)
	}
	for _, v := range e.tts.voicesUsed() {
		if v != testVoiceName {
			t.Fatalf("TTS request for voice %q; want the registered name %s (never a substitute)", v, testVoiceName)
		}
	}
	// The correlation: the committing frame's trace id rides on every speech
	// event and matches the commit's own trace.
	if s["trace_id"] == nil || s["trace_id"] == "" {
		t.Fatalf("started event carries no trace id: %v", s)
	}
	if started[0].TraceID == "" {
		t.Fatal("the started envelope lost the committing frame's trace id")
	}
	if genOf(t, commits[0]) != int64(s["generation"].(float64)) {
		t.Fatalf("the speech generation %v is not the commit's generation %v", s["generation"], genOf(t, commits[0]))
	}
	if occ, _ := payload(t, commits[0])["dialogue_occurrence_id"].(string); s["utterance_id"] != occ {
		t.Fatalf("the speech occurrence %v is not the commit's %v", s["utterance_id"], occ)
	}
	// The sink actually played the utterance.
	if sink.Starts() < 1 {
		t.Fatalf("the dummy sink reports no playback started")
	}
}

// TestSpeechRapidAdvanceCancelsStale is docs/spec/12's "zero stale audio
// starts": a faster commit cancels the older utterance (the event and the
// sink agree), and the stale generation never starts after it.
//
// Deterministic by construction — docs/spec/12's "deterministic
// delay-injection suite": the session runs on the explicitly-driven manual
// frame source (Options.ManualSource), and the test pushes frames at its
// own pace, waiting on the specific events between pushes:
//
//  1. push the line-a frames → wait for line a's commit (g1) and for g1's
//     speech.started: the stale utterance is demonstrably in playback;
//  2. push the line-b frames (the rapid advance) → wait for g1's
//     speech.cancelled (the fence moved past it: reason new_commit or
//     stale_generation, the tick's and the explicit cancel's reasons) and
//     g2's speech.started.
//
// Every ordering asserted below therefore holds on the event stream, and no
// wall-clock margin — however loaded the machine is — can reorder what the
// test observes. (The replay-driven version of this test raced the speak
// path's publish order against the clip's pipeline-speed arrival of the
// second commit, and flaked once in ~24 runs under load.)
func TestSpeechRapidAdvanceCancelsStale(t *testing.T) {
	sink := dummy.New(0, 0)
	src := newManualSource(64)
	e := newVoiceEnv(t, 10*time.Millisecond, 3*time.Second, true, sink, "",
		func() (capture.Source, error) { return src, nil })

	info, err := e.mgr.Start(e.ctx, runtime.StartRequest{
		ProfileID:   e.profileID,
		FrameSource: runtime.FrameSource{Kind: runtime.SourceKindManual},
		Mode:        runtime.ModeConservative,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	sid := info.ID
	defer waitState(t, e.mgr, sid, 30*time.Second)
	// The source's EOF ends the session; defer it (LIFO: it runs before the
	// waitState above) so a mid-test failure still lets the session reach a
	// terminal state instead of outliving the 30 s wait.
	defer func() { _ = src.Close() }()

	// Phase 1: line a. The clip's line-a frames (4..18: box appearing,
	// typewriter reveal, then stable) on the clip's own 10 fps source clock.
	for n := 4; n <= 18; n++ {
		src.push(t, manualClipFrame(t, n))
	}
	evCommit1 := e.waitEvents(t, sid, "line a committed", 60*time.Second, func(evs []events.Envelope) bool {
		for _, ev := range evs {
			if ev.Name == events.MessageCommitted {
				return true
			}
		}
		return false
	})
	g1 := mustCommitGen(t, evCommit1, 1)

	// The stale utterance is in playback — wait on the specific event before
	// the advance is pushed, so its start is observed, not assumed.
	e.waitEvents(t, sid, "stale utterance started", 60*time.Second, func(evs []events.Envelope) bool {
		for _, ev := range evs {
			if ev.Name == events.SpeechStarted && genOf(t, ev) == g1 {
				return true
			}
		}
		return false
	})

	// Phase 2: line b — the rapid advance. Frames 19..22 carry the second
	// line on the same source clock.
	for n := 19; n <= 22; n++ {
		src.push(t, manualClipFrame(t, n))
	}

	evsDone := e.waitEvents(t, sid, "stale utterance cancelled and successor started", 60*time.Second,
		func(evs []events.Envelope) bool {
			var cancelled, started2 int
			for _, ev := range evs {
				switch ev.Name {
				case events.SpeechCancelled:
					if genOf(t, ev) == g1 {
						cancelled++
					}
				case events.SpeechStarted:
					if genOf(t, ev) != g1 {
						started2++
					}
				}
			}
			return cancelled >= 1 && started2 >= 1
		})

	// The record: both generations, the causal orderings, the sink.
	g2 := mustCommitGen(t, evsDone, 2)
	if g2 == g1 {
		t.Fatalf("the two commits share generation %d; the clip should advance", g1)
	}
	var startSeq1, startSeq2, firstCancelSeq1, lastCancelSeq1 int64
	var staleStartsAfterCancel int
	for _, ev := range e.sessionEvents(sid) {
		switch ev.Name {
		case events.SpeechStarted:
			switch genOf(t, ev) {
			case g1:
				startSeq1 = ev.Seq
				if firstCancelSeq1 != 0 && ev.Seq >= firstCancelSeq1 {
					staleStartsAfterCancel++
				}
			case g2:
				startSeq2 = ev.Seq
			}
		case events.SpeechCancelled:
			if genOf(t, ev) == g1 {
				if firstCancelSeq1 == 0 {
					firstCancelSeq1 = ev.Seq
				}
				lastCancelSeq1 = ev.Seq
			}
		}
	}
	if startSeq1 == 0 {
		t.Fatal("the first utterance never started")
	}
	if startSeq2 == 0 {
		t.Fatal("the second utterance never started")
	}
	if firstCancelSeq1 == 0 {
		t.Fatal("the first utterance was never cancelled")
	}
	// Zero stale audio starts: the stale generation has exactly one start and
	// it predates the first cancellation — a start at or after a cancel would
	// be a stale start (the scheduler's fence drops it; this counts them).
	if staleStartsAfterCancel != 0 {
		t.Fatalf("%d stale start(s) of generation %d at/after its cancellation (seq %d)",
			staleStartsAfterCancel, g1, firstCancelSeq1)
	}
	if startSeq1 > lastCancelSeq1 {
		t.Fatalf("stale generation started after its cancellation (seq %d > %d)", startSeq1, lastCancelSeq1)
	}
	// The successor plays (startSeq2 != 0 is asserted above). Its publish
	// order against the commit's own publish is NOT asserted, no more than
	// the cancel's: the commit watcher runs in-band, before the commit's hub
	// publish takes its sequence, and the speech workers are async — neither
	// ordering is a contract. The contract is the one above: the stale start
	// strictly predates its cancellation, no stale start exists at or after
	// it, the successor plays, and the sink agrees.
	// Every stale cancellation carries the advance's reason: either the
	// explicit new-commit cancel or the scheduler's own fence tick stopping
	// the stale playback (stopStale) — the same product fact (the fence moved
	// past the generation), and under load the 100 ms tick can beat the
	// cancel request, so the test accepts both and pins no other.
	for _, ev := range e.sessionEvents(sid) {
		if ev.Name != events.SpeechCancelled || genOf(t, ev) != g1 {
			continue
		}
		switch payload(t, ev)["reason"] {
		case "new_commit", "stale_generation":
		default:
			t.Fatalf("stale cancellation reason = %v, want new_commit or stale_generation (the advance): %s", payload(t, ev)["reason"], ev.Data)
		}
	}
	// The sink agrees: the stale playback was interrupted, not played out.
	stale := false
	for _, r := range sink.Records() {
		if r.Generation == g1 && r.Interrupted {
			stale = true
		}
	}
	if !stale {
		t.Fatalf("the sink kept playing the stale generation to the end: %+v", sink.Records())
	}
}

// TestSpeechNoApprovedVoice is the voice-resolution policy's floor: no
// approved voice and no configured narrator means silence with the reason —
// and no TTS call, ever (never a builtin or a different voice).
func TestSpeechNoApprovedVoice(t *testing.T) {
	sink := dummy.New(0, 0)
	e := newVoiceEnv(t, 10*time.Millisecond, 100*time.Millisecond, false, sink, "", nil)
	sid := e.startReplay()
	defer waitState(t, e.mgr, sid, 30*time.Second)

	evs := e.waitEvents(t, sid, "skipped utterances", 20*time.Second, func(evs []events.Envelope) bool {
		var requested, skipped int
		for _, ev := range evs {
			switch ev.Name {
			case events.SpeechRequested:
				requested++
			case events.SpeechEnded:
				if p := payload(t, ev); p["result"] == "skipped" && p["reason"] == "no_approved_voice" {
					skipped++
				}
			}
		}
		return requested >= 2 && skipped >= 2
	})
	for _, ev := range evs {
		if ev.Name == events.SpeechReady || ev.Name == events.SpeechStarted {
			t.Fatalf("no voice was approved, but the pipeline still emitted %s", ev.Name)
		}
	}
	if n := e.tts.callCount(); n != 0 {
		t.Fatalf("no approved voice, but %d TTS calls went out", n)
	}
}

// TestSpeechAudioControls is the operator surface: mute skips the playback
// but still emits (result "muted"), the delay waits before start, and the
// volume scales the samples in the sink path — the last asserted on the file
// sink's own WAV bytes.
//
// Timing is driven, not hoped for. The OCR delay paces the replay at two
// recognitions per ocrDelay (the pipeline's worker pool), so when the second
// line commits at least one full OCR cycle is still in flight and the
// lossless replay source cannot reach its end before the delayed utterance
// starts: the 150 ms delay plus the voice resolution fits inside the
// guaranteed tail with a wide margin, on any machine (a slow machine slows
// every cycle; the margin is in cycles, not milliseconds). And once the
// audio is asserted the test stops the session itself, so the source end
// never decides the run's fate.
func TestSpeechAudioControls(t *testing.T) {
	dir := t.TempDir()
	// 500 ms per OCR recognition (two workers) paces the 24-frame clip at
	// ~250 ms per frame: ~6 s of run. The last cycle outlives the 150 ms
	// delay, so the ready→start gap is asserted on the real 150 ms wait —
	// the assertion is on the gap, not on the delay value.
	e := newVoiceEnv(t, 500*time.Millisecond, 150*time.Millisecond, true, nil, dir, nil)
	sid := e.startReplay()
	defer waitState(t, e.mgr, sid, 90*time.Second)

	// Mute as soon as the run is live: every utterance from here on resolves
	// (the cache fills) but never starts.
	waitRunning(t, e.mgr, sid)
	if _, err := e.mgr.SetAudio(sid, runtime.AudioSettings{Muted: ptrBool(true)}); err != nil {
		t.Fatalf("set muted: %v", err)
	}

	// Commit 1: requested → ready → ended(muted), no start.
	e.waitEvents(t, sid, "muted utterance", 40*time.Second, func(evs []events.Envelope) bool {
		for _, ev := range evs {
			if ev.Name == events.SpeechEnded && payload(t, ev)["result"] == "muted" {
				return true
			}
		}
		return false
	})
	for _, ev := range e.sessionEvents(sid) {
		if ev.Name == events.SpeechStarted {
			t.Fatal("the session is muted, but a playback started")
		}
	}
	if n := e.tts.callCount(); n < 1 {
		t.Fatal("mute skipped the synthesis; the cache should still fill")
	}

	// Unmute with a delay and a halved volume: commit 2 waits before start,
	// and its samples land at half amplitude.
	if _, err := e.mgr.SetAudio(sid, runtime.AudioSettings{
		Muted: ptrBool(false), SpeechDelayMS: ptrInt(150), Volume: ptrFloat(0.5),
	}); err != nil {
		t.Fatalf("set controls: %v", err)
	}
	evs := e.waitEvents(t, sid, "delayed second utterance", 40*time.Second, func(evs []events.Envelope) bool {
		g2, ok := commitGenOf(t, evs, 2)
		if !ok {
			return false
		}
		for _, ev := range evs {
			if ev.Name == events.SpeechStarted && genOf(t, ev) == g2 {
				return true
			}
		}
		return false
	})
	var readyAt, startAt time.Time
	g2 := mustCommitGen(t, evs, 2)
	for _, ev := range evs {
		if ev.Name == events.SpeechReady && genOf(t, ev) == g2 {
			readyAt = ev.OccurredAt
		}
		if ev.Name == events.SpeechStarted && genOf(t, ev) == g2 {
			startAt = ev.OccurredAt
		}
	}
	if readyAt.IsZero() || startAt.IsZero() {
		t.Fatalf("second utterance: ready %v, started %v", readyAt, startAt)
	}
	if d := startAt.Sub(readyAt); d < 100*time.Millisecond {
		t.Fatalf("the 150 ms delay was not honoured: start came %s after ready", d)
	}

	// The snapshot reports the applied controls and the counters.
	info, ok := e.mgr.Get(sid)
	if !ok || info.Audio == nil {
		t.Fatalf("the session snapshot lost its audio half")
	}
	if info.Audio.Muted || info.Audio.Volume != 0.5 || info.Audio.SpeechDelayMS != 150 {
		t.Fatalf("snapshot controls = muted=%v volume=%v delay=%d; want false / 0.5 / 150",
			info.Audio.Muted, info.Audio.Volume, info.Audio.SpeechDelayMS)
	}
	if got := info.Audio.Speech.Ready; got < 2 {
		t.Fatalf("ready counter = %d, want ≥ 2 (both commits resolved)", got)
	}
	if got := info.Audio.Speech.Ended; got < 1 {
		t.Fatalf("ended counter = %d, want ≥ 1 (the muted utterance)", got)
	}

	// The volume on the samples: the file sink wrote commit 2 at half gain.
	// The evidence file is written when the playback finishes; it is short
	// (the 150 ms tone) and lands just after the scheduler's Start returns —
	// poll for it.
	var wavPath string
	var err error
	by := time.Now().Add(10 * time.Second)
	for wavPath == "" {
		wavPath, err = newestWAV(dir)
		if wavPath != "" || time.Now().After(by) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if wavPath == "" {
		t.Fatalf("no WAV evidence from the file sink: %v", err)
	}
	got, want, err := peakRatio(t, wavPath, 0.5)
	if err != nil {
		t.Fatalf("decoding the file-sink WAV: %v", err)
	}
	if got < want*0.85 || got > want*1.15 {
		t.Fatalf("recorded peak ratio = %.3f, want ≈ %.3f (the volume must scale the samples)", got, want)
	}

	// The audio is fully asserted; the test ends the run on its own schedule
	// (the session's finalisation drains the speech pipeline, so no audio
	// event can land after this).
	if _, err := e.mgr.Stop(sid); err != nil {
		t.Fatalf("stop session: %v", err)
	}
}

// TestSpeechSessionEndTerminatesPending is the terminal-event contract
// (docs/spec/13 "did it speak, and why not"): every requested utterance
// reaches a terminal event with a reason, and it lands before the session's
// own terminal event — including when the session ends while the utterance
// is still in the TTS call or the operator's delay.
//
// Deterministic by construction — docs/spec/12's "deterministic
// delay-injection suite": the session runs on the explicitly-driven manual
// frame source (Options.ManualSource), and the test pushes frames at its own
// pace, waiting on the specific events between pushes:
//
//  1. apply the maximum delay (2000 ms, the API's documented maximum,
//     speechDelayMaxMS) before pushing any frame: with the manual source no
//     commit can exist before the first push, so "the delay is live before
//     the first requested utterance" is a protocol fact, not a wall-clock
//     hope;
//  2. push line a's frames → wait for line a's commit and its
//     speech.requested; push line b's frames → wait for the same on line b;
//  3. close the source: EOF ends the session while the requested utterances
//     are still pending (each in the TTS call or the operator's delay), and
//     every one of them reaches its terminal before session.stopped.
//
// The replay-driven form of this test raced two wall-clock margins the test
// does not own: SetAudio against the pipeline's own commit pace (the
// ~100 ms assumption), and the pipeline's commit pace against the 2000 ms
// delay itself. Under CPU contention the second margin inverts: line a's
// delay expires before line b's commit lands, line a submits to the scheduler
// and is fenced stale by the advance (speech.cancelled new_commit +
// speech.ended stale_generation — the correct product terminal for a stale
// utterance), and only line b ends session_ended. The contract the test owns
// is the terminal one, which no wall-clock margin can reorder: every
// requested occurrence is terminated, with a reason, before session.stopped.
func TestSpeechSessionEndTerminatesPending(t *testing.T) {
	sink := dummy.New(0, 0)
	src := newManualSource(64)
	e := newVoiceEnv(t, 10*time.Millisecond, 100*time.Millisecond, true, sink, "",
		func() (capture.Source, error) { return src, nil })

	info, err := e.mgr.Start(e.ctx, runtime.StartRequest{
		ProfileID:   e.profileID,
		FrameSource: runtime.FrameSource{Kind: runtime.SourceKindManual},
		Mode:        runtime.ModeConservative,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	sid := info.ID
	defer waitState(t, e.mgr, sid, 30*time.Second)
	// The source's EOF ends the session; defer it (LIFO: it runs before the
	// waitState above) so a mid-test failure still lets the session reach a
	// terminal state instead of outliving the 30 s wait.
	defer func() { _ = src.Close() }()

	// The maximum delay is applied before any frame is pushed: nothing
	// commits before the first push, so the delay is live before the first
	// requested utterance, by construction.
	if _, err := e.mgr.SetAudio(sid, runtime.AudioSettings{SpeechDelayMS: ptrInt(2000)}); err != nil {
		t.Fatalf("set delay: %v", err)
	}

	// Line a: the clip's frames 4..18 (box appearing, typewriter reveal, then
	// stable) on the clip's own 10 fps source clock.
	for n := 4; n <= 18; n++ {
		src.push(t, manualClipFrame(t, n))
	}
	evs1 := e.waitEvents(t, sid, "line a committed and requested", 60*time.Second, func(evs []events.Envelope) bool {
		var committed, requested bool
		for _, ev := range evs {
			switch ev.Name {
			case events.MessageCommitted:
				committed = true
			case events.SpeechRequested:
				requested = true
			}
		}
		return committed && requested
	})
	g1 := mustCommitGen(t, evs1, 1)
	if g1 == 0 {
		t.Fatal("line a committed with no generation")
	}

	// Line b: frames 19..22 carry the second line on the same source clock.
	for n := 19; n <= 22; n++ {
		src.push(t, manualClipFrame(t, n))
	}
	evs2 := e.waitEvents(t, sid, "line b committed and requested", 60*time.Second, func(evs []events.Envelope) bool {
		var commits, requested int
		for _, ev := range evs {
			switch ev.Name {
			case events.MessageCommitted:
				commits++
			case events.SpeechRequested:
				requested++
			}
		}
		return commits >= 2 && requested >= 2
	})
	g2 := mustCommitGen(t, evs2, 2)
	if g2 == g1 {
		t.Fatalf("the two commits share generation %d; the clip should advance", g1)
	}

	// The source ends now: EOF finalises the session while both requested
	// utterances are still pending — each in the TTS call or the operator's
	// delay, none terminated yet (the waits above observed the requested
	// events and no terminal). Close is idempotent; the deferred form covers
	// a mid-test failure.
	if err := src.Close(); err != nil {
		t.Fatalf("close source: %v", err)
	}
	waitState(t, e.mgr, sid, 30*time.Second) // the source's EOF ends the run
	evs := e.sessionEvents(sid)

	// The terminal contract (docs/spec/13): every requested occurrence is
	// terminated, with a reason, before session.stopped. The terminal sets
	// the product owes are per generation:
	//
	//   g1 (stale — fenced by the advance): speech.cancelled(new_commit),
	//     the fence's own event at the advance, whether g1 is still in the
	//     delay, already submitted or playing; and, when g1 had submitted,
	//     the scheduler's speech.ended(failed, stale_generation) drop.
	//   g2 (current): a terminal before session.stopped —
	//     speech.ended(failed, session_ended) when it was still in the TTS
	//     call or the operator's delay when the source ended (the shape this
	//     test sets up: the maximum delay, the source closed milliseconds
	//     after the last commit), or the scheduler's terminal for its
	//     playback when, on a starved machine, the delay expired first.
	//
	// Which wall-clock branch wins is not a contract — the 2000 ms delay and
	// the test's own progress race the machine's speed, exactly the race the
	// replay-driven form lost. That every requested generation is terminated
	// at all, with a reason, before the session's terminal event, is.
	var stoppedSeq int64
	requestedGens := map[int64]bool{}
	terminals := map[int64]bool{}
	var sessionEnded int
	for _, ev := range evs {
		switch ev.Name {
		case events.SpeechRequested:
			requestedGens[genOf(t, ev)] = true
		case events.SpeechEnded, events.SpeechCancelled:
			// The log is in seq order: a terminal after session.stopped would
			// mean the audio did not settle before the session's terminal
			// event, which the wiring's drain exists to prevent.
			if stoppedSeq > 0 && ev.Seq > stoppedSeq {
				t.Fatalf("%s (seq %d) lands after session.stopped (seq %d)", ev.Name, ev.Seq, stoppedSeq)
			}
			terminals[genOf(t, ev)] = true
			if ev.Name == events.SpeechEnded {
				if p := payload(t, ev); p["result"] == "failed" && p["reason"] == "session_ended" {
					sessionEnded++
				}
			}
		case events.SessionStopped:
			stoppedSeq = ev.Seq
		}
	}
	if len(requestedGens) != 2 {
		t.Fatalf("the clip's two lines requested %d utterance(s), want 2: %v", len(requestedGens), requestedGens)
	}
	if stoppedSeq == 0 {
		t.Fatal("the session never published session.stopped")
	}
	for g := range requestedGens {
		if !terminals[g] {
			t.Fatalf("generation %d was requested but reached no terminal event (speech.ended or speech.cancelled)", g)
		}
	}
	// The source ended while the requested utterances were still pending —
	// in the TTS call or the operator's delay, whose maximum is live on all
	// of them: at least one owes the session_ended terminal. (On a fast
	// machine both do; on a starved one the advance fences g1 stale and g2
	// carries the session_ended.)
	if sessionEnded < 1 {
		t.Fatalf("no utterance ended failed/session_ended although the source ended while the requested utterances were pending")
	}
	// Zero stale audio starts (docs/spec/12): no generation starts at or
	// after its own cancellation, nothing starts after session.stopped, and
	// the current generation (g2) never starts at all — its 2000 ms delay
	// outlasts the source end by construction (the source closes
	// milliseconds after g2's requested event), so its terminal is the
	// session_ended counted above. A g2 start would mean the delay expired
	// before the source closed on a starved machine — the degenerate shape
	// the setup does not owe a terminal for (a playback still active at the
	// scheduler's Stop gets no terminal event), named here so a future
	// reader sees it.
	firstCancel := map[int64]int64{}
	for _, ev := range evs {
		if ev.Name == events.SpeechCancelled && firstCancel[genOf(t, ev)] == 0 {
			firstCancel[genOf(t, ev)] = ev.Seq
		}
	}
	startedEvents := 0
	for _, ev := range evs {
		if ev.Name != events.SpeechStarted {
			continue
		}
		startedEvents++
		g := genOf(t, ev)
		if g == g2 {
			t.Fatalf("the current generation %d started playback; its delay outlasts the source end by construction, so it never submits (sink: %d start(s))", g2, sink.Starts())
		}
		if c, ok := firstCancel[g]; ok && ev.Seq >= c {
			t.Fatalf("generation %d started (seq %d) at/after its cancellation (seq %d) — a stale start", g, ev.Seq, c)
		}
		if ev.Seq > stoppedSeq {
			t.Fatalf("speech.started (seq %d) landed after session.stopped (seq %d)", ev.Seq, stoppedSeq)
		}
	}
	// The sink agrees with the log: a start the events do not name is a
	// stale playback the fence let through.
	if n := sink.Starts(); n > startedEvents {
		t.Fatalf("the sink reports %d start(s) but the log carries %d speech.started event(s)", n, startedEvents)
	}
}

// TestSpeechCacheHitSecondSession is docs/spec/07's miss-then-hit through the
// session path: the second run of the same clip resolves the same lines from
// the cache, with no second TTS request.
func TestSpeechCacheHitSecondSession(t *testing.T) {
	sink := dummy.New(0, 0)
	e := newVoiceEnv(t, 10*time.Millisecond, 100*time.Millisecond, true, sink, "", nil)

	first := e.startReplay()
	// Let both lines resolve (and commit to the cache) before the run ends.
	e.waitEvents(t, first, "both utterances ready", 30*time.Second, func(evs []events.Envelope) bool {
		var ready int
		for _, ev := range evs {
			if ev.Name == events.SpeechReady {
				ready++
			}
		}
		return ready >= 2
	})
	waitState(t, e.mgr, first, 30*time.Second)
	callsAfterFirst := e.tts.callCount()
	if callsAfterFirst < 2 {
		t.Fatalf("first run made %d TTS calls; want the two lines generated", callsAfterFirst)
	}

	second := e.startReplay()
	e.waitEvents(t, second, "cached second run", 30*time.Second, func(evs []events.Envelope) bool {
		var hits int
		for _, ev := range evs {
			if ev.Name == events.SpeechReady && payload(t, ev)["from_cache"] == true {
				hits++
			}
		}
		return hits >= 2
	})
	waitState(t, e.mgr, second, 30*time.Second)
	if got := e.tts.callCount(); got != callsAfterFirst {
		t.Fatalf("second run made %d further TTS calls (total %d, was %d); the cache should have served the identical lines",
			got-callsAfterFirst, got, callsAfterFirst)
	}
}

// TestSpeechAudioValidation is the control contract: out-of-range values are
// refused, an unknown session gets its sentinel, and a session with no
// speech pipeline reports the audio unavailable.
func TestSpeechAudioValidation(t *testing.T) {
	sink := dummy.New(0, 0)
	e := newVoiceEnv(t, 10*time.Millisecond, 100*time.Millisecond, true, sink, "", nil)
	sid := e.startReplay()
	defer waitState(t, e.mgr, sid, 30*time.Second)

	if _, err := e.mgr.SetAudio(sid, runtime.AudioSettings{Volume: ptrFloat(1.5)}); !isErr(err, runtime.ErrInvalidAudioSettings) {
		t.Fatalf("volume 1.5: err = %v, want %v", err, runtime.ErrInvalidAudioSettings)
	}
	if _, err := e.mgr.SetAudio(sid, runtime.AudioSettings{SpeechDelayMS: ptrInt(2001)}); !isErr(err, runtime.ErrInvalidAudioSettings) {
		t.Fatalf("delay 2001: err = %v, want %v", err, runtime.ErrInvalidAudioSettings)
	}
	if _, err := e.mgr.SetAudio("01ARZ3NDEKTSV4RRFFQ69E5VB0", runtime.AudioSettings{Muted: ptrBool(true)}); !isErr(err, runtime.ErrSessionNotFound) {
		t.Fatalf("unknown session: err = %v, want %v", err, runtime.ErrSessionNotFound)
	}

	// A runtime without the speech pipeline: the control answers the
	// documented 503 sentinel, and the snapshot has no audio half.
	plain, err := runtime.New(runtime.Options{
		Store:       e.st,
		Hub:         e.hub,
		AssetsRoot:  t.TempDir(),
		ReplayRoots: []string{absPath(t, syntheticDir)},
		OCR:         &fakeOCR{script: truthScript(e.t), delay: 10 * time.Millisecond},
		Log:         slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("plain runtime.New: %v", err)
	}
	t.Cleanup(plain.Close)
	pinfo, err := plain.Start(e.ctx, runtime.StartRequest{
		ProfileID:   e.profileID,
		FrameSource: runtime.FrameSource{Kind: runtime.SourceKindReplay, Dir: syntheticClipDir()},
		Mode:        runtime.ModeConservative,
	})
	if err != nil {
		t.Fatalf("plain start: %v", err)
	}
	if _, err := plain.SetAudio(pinfo.ID, runtime.AudioSettings{Muted: ptrBool(true)}); !isErr(err, runtime.ErrAudioUnavailable) {
		t.Fatalf("unwired session: err = %v, want %v", err, runtime.ErrAudioUnavailable)
	}
	if snap, ok := plain.Get(pinfo.ID); ok && snap.Audio != nil {
		t.Fatalf("the unwired session reports an audio half: %+v", snap.Audio)
	}
	if _, err := plain.Stop(pinfo.ID); err != nil {
		t.Fatalf("plain stop: %v", err)
	}
}

// ── small helpers -------------------------------------------------------------

func ptrBool(b bool) *bool        { return &b }
func ptrInt(i int) *int           { return &i }
func ptrFloat(f float64) *float64 { return &f }

func isErr(err error, target error) bool {
	return err != nil && strings.Contains(err.Error(), target.Error())
}

// newestWAV finds the most recently written file-sink WAV under the session
// directory the sink root holds.
func newestWAV(root string) (string, error) {
	var newest string
	var newestM time.Time
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d == nil || d.IsDir() || !strings.HasSuffix(p, ".wav") {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if fi.ModTime().After(newestM) {
			newestM = fi.ModTime()
			newest = p
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if newest == "" {
		return "", os.ErrNotExist
	}
	return newest, nil
}

// peakRatio decodes one file-sink WAV and compares its sample peak with the
// peak the fake TTS tone carries at the given gain.
func peakRatio(t *testing.T, path string, gain float64) (float64, float64, error) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	pcm, err := speech.DecodeWAV(b)
	if err != nil {
		return 0, 0, err
	}
	if pcm.BitsPerSample != 16 {
		return 0, 0, fmt.Errorf("unexpected bit depth %d", pcm.BitsPerSample)
	}
	var got float64
	for i := 0; i+1 < len(pcm.Data); i += 2 {
		v := math.Abs(float64(int16(uint16(pcm.Data[i]) | uint16(pcm.Data[i+1])<<8)))
		if v > got {
			got = v
		}
	}
	// The fake tone is MakeToneWAV: amplitude 0.25 of full scale.
	want := 0.25 * 32767 * gain
	return got / 32767, want / 32767, nil
}
