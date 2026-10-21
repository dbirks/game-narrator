package runtime_test

// The unmatched read-through's session-level tests (ADR-0136): a box that
// settles but the corpus cannot name (the matcher abstains) is spoken from
// its OCR text instead of staying silent — with the event naming why (the
// speech.requested reason unmatched_read_through and the OCR confidence),
// the session's policy gating it (the per-session setting, default ON), and
// the matched line's path untouched.
//
// The environment is the production shape with the two documented seams
// (the scripted OCR engine and the fake TTS), plus the explicitly-driven
// manual frame source (docs/spec/12's deterministic delay-injection suite):
// the test pushes the clip's frames at its own pace, so the order in which
// the lines commit and speak is the order the test decides, and the
// read-through's ON / OFF switch is applied with no wall-clock margin.
//
// The unmatched line is made by re-keying the truth script: the clip's
// second line (frames 19..22) reads, to the scripted engine, as a sentence
// the corpus does not contain — the matcher's honest answer is abstention,
// and the commit is the session's "unknown / COMMITTED_ABSTAINED" record.
// The first line is left on the truth script, so the matched path (the
// Clef decision's shape: a known message id, no reason on the request)
// runs in the same session and the two halves can be asserted side by side.

import (
	"fmt"
	"image"
	"image/draw"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/runtime"
	"github.com/dbirks/game-narrator/internal/scenario"
	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/speech/dummy"
	"github.com/dbirks/game-narrator/internal/store"
	"github.com/dbirks/game-narrator/internal/vision"
	"github.com/dbirks/game-narrator/internal/voices"
)

// unmatchedTestText is the second line's re-keyed read: a sentence the
// synthetic corpus does not contain (no message, no distractor, shares no
// distinctive prefix), so the matcher's only honest verdict is abstention.
// It is plain, well-read text — the live case the user named ("the text is
// plain as day"), not a wobbly read.
const unmatchedTestText = "The tide came in early this morning."

// unmatchedFrames is the clip's second line, stable across the range
// (frames 19..22 of dialogue-advance-001): the box's lifetime is one, the
// read is settled, and the line is the corpus's demo-page-2 — which this
// file re-keys to something the corpus does not name.
var unmatchedFrames = []int{19, 20, 21, 22}

// frameCropHash is the crop's identity for frame n under the pipeline's
// recipe — the same computation truthScript runs per frame (normalise to
// RGBA, crop the profile's textbox rect with the runtime's default options,
// preprocess with the default steps, hash): the scripted engine's key is
// the preprocessed crop's hash, and a recipe drift here is a miss on the
// re-keyed frame, which the tests would see as "the unmatched line never
// committed".
func frameCropHash(t *testing.T, n int) string {
	t.Helper()
	corpusPresent(t)
	profile, err := scenario.LoadProfile(profilePath)
	if err != nil {
		t.Fatalf("load profile: %v", err)
	}
	if len(profile.Regions) != 1 {
		t.Fatalf("fixture has %d regions, want 1", len(profile.Regions))
	}
	r := profile.Regions[0]
	f, err := os.Open(filepath.Join(syntheticDir, fmt.Sprintf("frame-%03d.png", n)))
	if err != nil {
		t.Fatalf("open frame %d: %v", n, err)
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		t.Fatalf("decode frame %d: %v", n, err)
	}
	rgba := image.NewRGBA(img.Bounds())
	draw.Draw(rgba, rgba.Bounds(), img, img.Bounds().Min, draw.Src)
	crop := vision.CropNormalized(rgba, vision.Rect{X: r.X, Y: r.Y, W: r.W, H: r.H}, runtime.DefaultCropOptions())
	if crop == nil || crop.Bounds().Empty() {
		t.Fatalf("frame %d: crop is empty", n)
	}
	return vision.ImageHash(vision.Preprocess(crop, vision.DefaultSteps()))
}

// unmatchedScript is the truth script with the second line's crops re-keyed
// to the corpus's-unmatched read.
func unmatchedScript(t *testing.T) map[string]string {
	t.Helper()
	script := truthScript(t)
	for _, n := range unmatchedFrames {
		script[frameCropHash(t, n)] = unmatchedTestText
	}
	return script
}

// unmatchedVoiceEnv is newVoiceEnv's shape with the OCR script supplied by
// the test: the production wiring (real store, hub, voices, TTS cache, the
// dummy sink) with the scripted engine reading the re-keyed line. The
// builder mirrors newVoiceEnv's body — if that harness's wiring moves, this
// one moves with it.
func unmatchedVoiceEnv(t *testing.T, ocrDelay, toneS time.Duration, sink speech.AudioSink, script map[string]string, manual func() (capture.Source, error)) *voiceEnv {
	t.Helper()
	corpusPresent(t)
	ctx := t.Context()

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
	voiceID := seedNarratorVoice(t, st, a)

	e := &voiceEnv{t: t, ctx: ctx, st: st, hub: hub, mem: mem, svc: svc, assets: a, tts: fake, voiceID: voiceID}
	opts := runtime.SpeechOptions{Voices: svc, Assets: a}
	// The harness default (the ADR-0092 immediate shape, latest_only): the
	// read-through lines route through the same fence the matched lines do,
	// so a second unmatched box would cancel the first's audio the way a
	// second matched line does.
	opts.CatchUp = &runtime.CatchUpOptions{Policy: speech.CatchUpPolicyLatestOnly}
	opts.SinkFactory = func() (speech.AudioSink, error) { return sink, nil }
	mgr, err := runtime.New(runtime.Options{
		Store:        st,
		Hub:          hub,
		AssetsRoot:   t.TempDir(),
		ReplayRoots:  []string{absPath(t, syntheticDir)},
		OCR:          &fakeOCR{script: script, delay: ocrDelay},
		Speech:       &opts,
		ManualSource: manual,
		Log:          slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}
	e.mgr = mgr
	e.profileID, _ = seedSyntheticProfile(t, st, true)
	t.Cleanup(func() { _ = st.Close() })
	t.Cleanup(func() { _ = e.hub.Close() })
	t.Cleanup(mgr.Close)
	return e
}

// startManual starts the session on the explicitly-driven source.
func (e *voiceEnv) startManual(t *testing.T) string {
	t.Helper()
	info, err := e.mgr.Start(e.ctx, runtime.StartRequest{
		ProfileID:   e.profileID,
		FrameSource: runtime.FrameSource{Kind: runtime.SourceKindManual},
		Mode:        runtime.ModeConservative,
	})
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	return info.ID
}

// findEvent returns the first event in evs matching the name and whose
// payload satisfies the predicate (ok is false while it has not landed —
// the poll predicates use that).
func findEvent(t *testing.T, evs []events.Envelope, name string, pred func(map[string]any) bool) (events.Envelope, bool) {
	for _, ev := range evs {
		if ev.Name != name {
			continue
		}
		m := payload(t, ev)
		if pred == nil || pred(m) {
			return ev, true
		}
	}
	return events.Envelope{}, false
}

// TestSpeechUnmatchedReadThroughSpeaksOCRText is the requirement's shape:
// the box settles, the matcher abstains (the corpus cannot name the line),
// and the session speaks the OCR text anyway — with the event naming why.
// The session's policy is the default (ON: no settings call), and the
// matched first line runs in the same session, so the Clef path's shape
// (a known message id, no reason on the request) is asserted against the
// read-through's (the reason and the OCR confidence) side by side.
func TestSpeechUnmatchedReadThroughSpeaksOCRText(t *testing.T) {
	sink := dummy.New(0, 0)
	src := newManualSource(64)
	e := unmatchedVoiceEnv(t, 10*time.Millisecond, 3*time.Second, sink, unmatchedScript(t),
		func() (capture.Source, error) { return src, nil })
	sid := e.startManual(t)
	defer waitState(t, e.mgr, sid, 30*time.Second)
	defer func() { _ = src.Close() }()

	// The snapshot carries the policy at its documented default: ON, with no
	// settings call in flight — the Play toggle reads the server's value.
	if info, ok := e.mgr.Get(sid); !ok || !info.UnmatchedReadThrough {
		t.Fatalf("the snapshot does not carry the read-through default ON: %+v", info)
	}

	// Phase 1: the matched line (frames 4..18: the typewriter reveal, then
	// the stable line). The matched path is the Clef shape: a known message
	// id, eligible, and no reason on the request.
	for n := 4; n <= 18; n++ {
		src.push(t, manualClipFrame(t, n))
	}
	evs := e.waitEvents(t, sid, "line one spoken", 60*time.Second, func(evs []events.Envelope) bool {
		committed := false
		started := false
		for _, ev := range evs {
			switch ev.Name {
			case events.MessageCommitted:
				m := payload(t, ev)
				if m["message_id"] != "unknown" && m["speech_eligible"] == true {
					committed = true
				}
			case events.SpeechStarted:
				started = true
			}
		}
		return committed && started
	})
	if _, ok := findEvent(t, evs, events.SpeechRequested, func(m map[string]any) bool {
		// The matched line's request: a known message id and no reason —
		// the Clef path's shape, asserted against the read-through's below.
		return m["message_id"] != "unknown" && m["reason"] == nil
	}); !ok {
		t.Fatal("the matched line's request is missing or carries a reason: the Clef path moved")
	}

	// Phase 2: the unmatched line (frames 19..22). The commit is the
	// session's abstain record; the read-through is the speech's answer.
	for n := 19; n <= 22; n++ {
		src.push(t, manualClipFrame(t, n))
	}
	evs = e.waitEvents(t, sid, "unmatched line committed", 60*time.Second, func(evs []events.Envelope) bool {
		_, ok := findEvent(t, evs, events.MessageCommitted, func(m map[string]any) bool {
			return m["message_id"] == "unknown" && m["reason"] == "COMMITTED_ABSTAINED"
		})
		return ok
	})
	commit := mustFindEvent(t, evs, events.MessageCommitted, func(m map[string]any) bool {
		return m["message_id"] == "unknown" && m["reason"] == "COMMITTED_ABSTAINED"
	})
	cm := payload(t, commit)
	occ, _ := cm["dialogue_occurrence_id"].(string)
	if occ == "" {
		t.Fatal("the abstained commit carries no occurrence id")
	}

	// The read-through's utterance ends cleanly on the line's own
	// occurrence (line one was the matched line's, and the latest_only
	// advance cancelled it with a different result).
	evs = e.waitEvents(t, sid, "unmatched line spoken", 60*time.Second, func(evs []events.Envelope) bool {
		_, ok := findEvent(t, evs, events.SpeechEnded, func(m map[string]any) bool {
			return m["utterance_id"] == occ && m["result"] == "ended"
		})
		return ok
	})

	// The commit's own record: unknown, abstained, not eligible on the
	// commitment (the reducer's contract is untouched), the confidence the
	// read-through gated on.
	if cm["speech_eligible"] != false {
		t.Fatalf("the abstained commit carries speech_eligible = %v, want false (the reducer's contract)", cm["speech_eligible"])
	}
	if conf, _ := cm["confidence"].(float64); conf < 0.75 {
		t.Fatalf("the commit's OCR confidence = %v, below the read-through's floor (the gate's own input)", conf)
	}

	// The request names the policy's verdict (the requirement: "an event
	// naming it … so traces show why"): the reason and the OCR confidence,
	// the occurrence's identity, the cleaned text, the narrator's character
	// (the commit carries no known attribution — no decision was asked).
	req := mustFindEvent(t, e.sessionEvents(sid), events.SpeechRequested, func(m map[string]any) bool {
		return m["reason"] == "unmatched_read_through"
	})
	rm := payload(t, req)
	if rm["dialogue_occurrence_id"] != occ {
		t.Fatalf("the read-through's request is for occurrence %v, the commit's is %v", rm["dialogue_occurrence_id"], occ)
	}
	if rm["text"] != unmatchedTestText {
		t.Fatalf("the read-through speaks %q, want the OCR text %q", rm["text"], unmatchedTestText)
	}
	if rm["character_id"] != "narrator" {
		t.Fatalf("the unattributed read-through speaks as %v, want the narrator fallback", rm["character_id"])
	}
	if conf, _ := rm["ocr_confidence"].(float64); conf < 0.75 {
		t.Fatalf("the request's OCR confidence = %v, below the floor it was gated on", conf)
	}

	// Exactly one read-through request in the whole session (the dedupe:
	// the same unmatched box is not re-spoken — the reducer commits one
	// occurrence once and the wiring's table refuses to route it twice).
	count := 0
	for _, ev := range e.sessionEvents(sid) {
		if ev.Name != events.SpeechRequested {
			continue
		}
		if m := payload(t, ev); m["reason"] == "unmatched_read_through" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("%d read-through requests, want exactly one (the box is not re-spoken)", count)
	}

	// The audio actually played: the started/ended pair is the line's, the
	// sink saw the second playback, and every TTS call went to the
	// narrator's registered voice (the unattributed line's voice is the
	// narrator's, the profile's fallback the requirement names). The call
	// count is a floor, not exact: the matched line's successor prewarm
	// (FR-11) spends its own background call, and the prewarm is the
	// matched path's work, not the read-through's.
	started := mustFindEvent(t, e.sessionEvents(sid), events.SpeechStarted, func(m map[string]any) bool {
		return m["utterance_id"] == occ
	})
	if payload(t, started)["generation"] != cm["generation"] {
		t.Fatalf("the read-through's generation %v is not the commit's %v",
			payload(t, started)["generation"], cm["generation"])
	}
	if sink.Starts() < 2 {
		t.Fatalf("the sink saw %d playbacks, want at least two (the matched line and the read-through)", sink.Starts())
	}
	if e.tts.callCount() < 2 {
		t.Fatalf("the TTS saw %d calls, want at least two (the matched line's and the read-through's)", e.tts.callCount())
	}
	for _, v := range e.tts.voicesUsed() {
		if v != testVoiceName {
			t.Fatalf("TTS request for voice %q; want the narrator's registered name %s", v, testVoiceName)
		}
	}
}

// mustFindEvent is findEvent's failing form: the event is asserted present
// (the waitEvents above proved it landed, so a miss here is a decode drift,
// not a timing question).
func mustFindEvent(t *testing.T, evs []events.Envelope, name string, pred func(map[string]any) bool) events.Envelope {
	t.Helper()
	ev, ok := findEvent(t, evs, name, pred)
	if !ok {
		t.Fatalf("no %s matching the predicate", name)
	}
	return ev
}

// TestSpeechUnmatchedReadThroughOffStaysSilent is the other half of the
// per-session setting: the operator turns the read-through off (the
// settings call, before any frame is pushed — the manual source makes the
// order exact), and the abstained box keeps the historic silence. The
// matched line still speaks: the policy gates only the unmatched box, and
// the Clef decision path is untouched.
func TestSpeechUnmatchedReadThroughOffStaysSilent(t *testing.T) {
	sink := dummy.New(0, 0)
	src := newManualSource(64)
	e := unmatchedVoiceEnv(t, 10*time.Millisecond, 3*time.Second, sink, unmatchedScript(t),
		func() (capture.Source, error) { return src, nil })
	sid := e.startManual(t)
	defer waitState(t, e.mgr, sid, 30*time.Second)
	defer func() { _ = src.Close() }()

	// The operator's switch, applied before the first frame: the snapshot
	// carries the applied state (the toggle reads the server's value).
	off := false
	next, err := e.mgr.SetSettings(sid, runtime.SessionSettings{UnmatchedReadThrough: &off})
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	if next.UnmatchedReadThrough {
		t.Fatalf("the snapshot still carries the read-through ON after the switch: %+v", next)
	}

	// The matched line still speaks (the setting gates only the unmatched
	// box): its commit and its request land the way they always have.
	for n := 4; n <= 18; n++ {
		src.push(t, manualClipFrame(t, n))
	}
	evs := e.waitEvents(t, sid, "line one committed and spoken", 60*time.Second, func(evs []events.Envelope) bool {
		committed, _ := findEvent(t, evs, events.MessageCommitted, func(m map[string]any) bool {
			return m["message_id"] != "unknown"
		})
		_, requested := findEvent(t, evs, events.SpeechRequested, func(m map[string]any) bool {
			return m["message_id"] != "unknown"
		})
		_ = committed
		return requested
	})

	// The unmatched line commits (the reducer's record is unchanged — the
	// policy is the speech layer's, not the commitment's) and stays silent.
	for n := 19; n <= 22; n++ {
		src.push(t, manualClipFrame(t, n))
	}
	evs = e.waitEvents(t, sid, "unmatched line committed", 60*time.Second, func(evs []events.Envelope) bool {
		_, ok := findEvent(t, evs, events.MessageCommitted, func(m map[string]any) bool {
			return m["message_id"] == "unknown" && m["reason"] == "COMMITTED_ABSTAINED"
		})
		return ok
	})
	occEv, _ := findEvent(t, evs, events.MessageCommitted, func(m map[string]any) bool {
		return m["message_id"] == "unknown" && m["reason"] == "COMMITTED_ABSTAINED"
	})
	occID, _ := payload(t, occEv)["dialogue_occurrence_id"].(string)

	// End the run (the empty frames dismiss the box, the source's EOF ends
	// the session), then assert the silence on the complete log: no speech
	// event of any kind for the unmatched occurrence, no read-through reason
	// anywhere, and the TTS's single call is the matched line's.
	for n := 23; n <= 24; n++ {
		src.push(t, manualClipFrame(t, n))
	}
	_ = src.Close()
	waitState(t, e.mgr, sid, 30*time.Second)
	// The silence is the event record: no speech event of any kind for the
	// unmatched occurrence, no read-through reason anywhere. The TTS's
	// background calls (the matched line's successor prewarm, FR-11) are the
	// matched path's work — the contract the test proves is that the
	// unmatched box itself was never routed into the audio pipeline.
	for _, ev := range e.sessionEvents(sid) {
		switch ev.Name {
		case events.SpeechRequested, events.SpeechStarted, events.SpeechEnded, events.SpeechCancelled:
			m := payload(t, ev)
			if m["utterance_id"] == occID || m["dialogue_occurrence_id"] == occID {
				t.Fatalf("the read-through is OFF but %s carries the unmatched occurrence: %s", ev.Name, ev.Data)
			}
		}
		if ev.Name == events.SpeechRequested {
			if m := payload(t, ev); m["reason"] == "unmatched_read_through" {
				t.Fatalf("a read-through request with the policy OFF: %s", ev.Data)
			}
		}
	}
}

// TestSessionSettingsReadThroughRoundTrip is the settings surface itself:
// the default is ON in the snapshot, the switch moves it mid-session, a
// body that does not name it keeps the session's value (the partial-body
// rule), and the switch on re-applies.
func TestSessionSettingsReadThroughRoundTrip(t *testing.T) {
	e := newVoiceEnv(t, 10*time.Millisecond, 100*time.Millisecond, true, dummy.New(0, 0), "", nil)
	sid := e.startReplay()
	defer waitState(t, e.mgr, sid, 30*time.Second)

	// The default: a started session reads ON from the snapshot without a
	// settings call.
	if info, ok := e.mgr.Get(sid); !ok || !info.UnmatchedReadThrough {
		t.Fatalf("the started session does not carry the read-through default ON: %+v", info)
	}

	off := false
	next, err := e.mgr.SetSettings(sid, runtime.SessionSettings{UnmatchedReadThrough: &off})
	if err != nil {
		t.Fatalf("switch off: %v", err)
	}
	if next.UnmatchedReadThrough {
		t.Fatalf("the switch off did not apply: %+v", next)
	}

	// A body that does not name the field keeps the session's value (the
	// partial-body rule): a mode-only call leaves the read-through off.
	mode := runtime.ModePredictive
	next, err = e.mgr.SetSettings(sid, runtime.SessionSettings{Mode: &mode})
	if err != nil {
		t.Fatalf("mode-only settings: %v", err)
	}
	if next.UnmatchedReadThrough {
		t.Fatalf("the mode-only call moved the read-through: %+v", next)
	}

	on := true
	next, err = e.mgr.SetSettings(sid, runtime.SessionSettings{UnmatchedReadThrough: &on})
	if err != nil {
		t.Fatalf("switch on: %v", err)
	}
	if !next.UnmatchedReadThrough {
		t.Fatalf("the switch on did not apply: %+v", next)
	}
}
