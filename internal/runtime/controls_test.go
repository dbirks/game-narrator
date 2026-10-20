package runtime_test

// The operator's live controls on a real replay session (docs/spec/08
// "…/pause, …/resume, …/settings"; ADR-0075): pause stops the commitment and
// fences the audio still playing, resume commits the waiting line exactly
// once, and the settings control moves the session's policy with its
// snapshot and its event. The environment is the production shape with the
// two documented seams (the scripted OCR truth, the fake TTS base); the
// frames are the captured synthetic clip replayed through the real pipeline
// (AGENTS.md rule 3 class (b)).

import (
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/runtime"
	"github.com/dbirks/game-narrator/internal/speech/dummy"
)

// seqOf finds the first event with the name and returns its sequence.
func seqOf(t *testing.T, evs []events.Envelope, name string) int64 {
	t.Helper()
	for _, ev := range evs {
		if ev.Name == name {
			return ev.Seq
		}
	}
	t.Fatalf("no %s event in the log", name)
	return 0
}

// countBetween counts events of the name strictly between two sequences.
func countBetween(evs []events.Envelope, name string, lo, hi int64) int {
	n := 0
	for _, ev := range evs {
		if ev.Name == name && ev.Seq > lo && ev.Seq < hi {
			n++
		}
	}
	return n
}

// TestReplayPauseMutesSpeech is the pause half of the player controls: a
// replay session with a real voice pipeline (the approved narrator voice, the
// scripted TTS base, the dummy sink) speaks its first line; the operator's
// pause cancels the utterance still playing, stops the commitment (the second
// line commits nothing while paused, and the audio pipeline requests nothing
// new), and the resume picks up where the evidence stood — the second line
// commits and speaks exactly once. A pause can never be bursted: the paused
// window's observations were real and were decided once, on the resume.
func TestReplayPauseMutesSpeech(t *testing.T) {
	sink := dummy.New(0, 0)
	// The OCR delay paces the pipeline against the fast-draining replay, so
	// the session is alive for seconds: long enough to press pause mid-line.
	// The tone is long (3 s) so the first utterance is genuinely still playing
	// when the pause lands — the fence the test asserts needs an in-flight line.
	e := newVoiceEnv(t, 250*time.Millisecond, 3*time.Second, true, sink, "", nil)
	id := e.startReplay()

	// The first line is genuinely playing on the sink before the pause.
	e.waitEvents(t, id, "the first line started", 60*time.Second,
		func(evs []events.Envelope) bool { return countBetween(evs, events.SpeechStarted, 0, 1<<62) >= 1 })

	// The pause: the control's own snapshot carries the operator's state, the
	// button's half of the contract.
	info, err := e.mgr.Pause(id)
	if err != nil {
		t.Fatalf("pause: %v", err)
	}
	if !info.Paused {
		t.Fatalf("pause snapshot = paused %v, want true (the button reads the server's state)", info.Paused)
	}

	evs := e.waitEvents(t, id, "the pause fenced the in-flight line", 30*time.Second,
		func(evs []events.Envelope) bool {
			paused := false
			fenced := false
			for _, ev := range evs {
				if ev.Name == events.SessionPaused {
					paused = true
				}
				if ev.Name == events.SpeechCancelled && payload(t, ev)["reason"] == "paused" {
					fenced = true
				}
			}
			return paused && fenced
		})
	pauseSeq := seqOf(t, evs, events.SessionPaused)

	// Idempotent: a second pause is the same snapshot, not a second event.
	if again, err := e.mgr.Pause(id); err != nil || !again.Paused {
		t.Fatalf("second pause = %v / %v, want the same paused snapshot", again, err)
	}
	if n := countBetween(e.sessionEvents(id), events.SessionPaused, 0, 1<<62); n != 1 {
		t.Fatalf("session.paused events = %d, want 1 (a duplicate is not an event)", n)
	}

	// The resume: the session continues from the next commit. The control lands
	// on the writer's chain, so the event is the async half — poll for it the
	// way the pause above was polled (the snapshot is the answer, the event is
	// the log).
	info, err = e.mgr.Resume(id)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if info.Paused {
		t.Fatalf("resume snapshot = paused %v, want false", info.Paused)
	}
	evs = e.waitEvents(t, id, "the resume landed on the writer", 30*time.Second,
		func(evs []events.Envelope) bool { return countBetween(evs, events.SessionResumed, 0, 1<<62) >= 1 })
	resumeSeq := seqOf(t, evs, events.SessionResumed)

	// The run settles: source end, both lines, each committed exactly once —
	// the paused window's observations were observed and decided, not replayed.
	final := waitState(t, e.mgr, id, 180*time.Second)
	if final.State != runtime.StateStopped || final.Reason != runtime.StopReasonSourceEnd {
		t.Fatalf("terminal = %s/%s, want stopped/source_end (stats %+v)", final.State, final.Reason, final.Stats)
	}
	if final.Stats.Commits != 2 {
		t.Fatalf("commits = %d, want 2 (one per line of the clip, no burst)", final.Stats.Commits)
	}
	evs = e.sessionEvents(id)
	if got := countBetween(evs, events.MessageCommitted, 0, pauseSeq); got != 1 {
		t.Fatalf("commits before the pause = %d, want 1 (the line that was speaking)", got)
	}
	if got := countBetween(evs, events.MessageCommitted, pauseSeq, 1<<62); got != 1 {
		t.Fatalf("commits after the pause = %d, want exactly 1 (the resume bursted the paused window)", got)
	}
	if got := countBetween(evs, events.SpeechStarted, pauseSeq, resumeSeq); got != 0 {
		t.Fatalf("speech.started between pause and resume = %d, want 0 (pause mutes new speech)", got)
	}
	if got := countBetween(evs, events.SpeechRequested, pauseSeq, 1<<62); got != 1 {
		t.Fatalf("speech.requested after the pause = %d, want 1 (the resumed line, once)", got)
	}
	// The sink saw the fence: the first playback was interrupted, the second
	// ran.
	if sink.Starts() < 2 {
		t.Fatalf("the sink started %d playbacks, want the two lines", sink.Starts())
	}

	// Terminal sessions refuse the control: the pause is not dropped.
	if _, err := e.mgr.Pause(id); err == nil {
		t.Fatal("pause after the terminal state answered success; the documented answer is a refusal")
	}
}

// TestReplaySettingsMovesThePolicy is the settings half: the mid-session
// operator policy (mode + narrated classes) is one control on the session's
// chain — the snapshot carries the applied, normalized policy, the event
// names it, a no-op names nothing, and a value outside the vocabulary is a
// 400-shaped error before any event exists. The eligibility filter the class
// set drives is proven at the reducer (internal/session, the layer that
// decides it): this corpus's lines are all dialogue, and the scenario loader
// admits no other class.
func TestReplaySettingsMovesThePolicy(t *testing.T) {
	sink := dummy.New(0, 0)
	e := newVoiceEnv(t, 250*time.Millisecond, time.Second, true, sink, "", nil)
	id := e.startReplay()
	e.waitEvents(t, id, "the first line committed", 60*time.Second,
		func(evs []events.Envelope) bool { return countBetween(evs, events.MessageCommitted, 0, 1<<62) >= 1 })

	// An out-of-vocabulary value is refused before the session sees it.
	if _, err := e.mgr.SetSettings(id, runtime.SessionSettings{Mode: strPtr("turbo")}); err == nil {
		t.Fatal("mode \"turbo\" was accepted; the documented vocabulary is conservative|predictive")
	}
	if _, err := e.mgr.SetSettings(id, runtime.SessionSettings{NarratedClasses: &[]string{"graffiti"}}); err == nil {
		t.Fatal("class \"graffiti\" was accepted; the vocabulary is the text-kind words")
	}
	if got := countBetween(e.sessionEvents(id), events.SessionSettings, 0, 1<<62); got != 0 {
		t.Fatalf("the refused requests left %d settings events", got)
	}

	// The default snapshot: conservative, dialogue alone.
	if info, ok := e.mgr.Get(id); !ok || info.Mode != runtime.ModeConservative || len(info.NarratedClasses) != 1 || info.NarratedClasses[0] != "dialogue" {
		t.Fatalf("default snapshot = %+v / %v, want conservative with dialogue alone", info, ok)
	}

	// A real change: predictive mode plus the signs class.
	info, err := e.mgr.SetSettings(id, runtime.SessionSettings{
		Mode:            strPtr(runtime.ModePredictive),
		NarratedClasses: &[]string{"sign", "dialogue"},
	})
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	if info.Mode != runtime.ModePredictive {
		t.Fatalf("applied mode = %q, want predictive", info.Mode)
	}
	if len(info.NarratedClasses) != 2 || info.NarratedClasses[0] != "dialogue" || info.NarratedClasses[1] != "sign" {
		t.Fatalf("applied classes = %v, want the normalized [dialogue sign]", info.NarratedClasses)
	}
	// The control is applied by the writer's single chain, so the event is the
	// async half of the contract — poll for it (the snapshot is the answer,
	// the event is the log), exactly the way the pause and resume are polled.
	evs := e.waitEvents(t, id, "the settings landed on the writer", 30*time.Second,
		func(evs []events.Envelope) bool { return countBetween(evs, events.SessionSettings, 0, 1<<62) == 1 })
	var last *events.Envelope
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Name == events.SessionSettings {
			last = &evs[i]
			break
		}
	}
	if last == nil {
		t.Fatal("no settings event found to inspect")
	}
	p := payload(t, *last)
	if got := p["mode"]; got != "predictive" {
		t.Fatalf("event mode = %v, want predictive", got)
	}
	classes, _ := p["narrated_classes"].([]any)
	if len(classes) != 2 || classes[0].(string) != "dialogue" || classes[1].(string) != "sign" {
		t.Fatalf("event narrated_classes = %v, want the normalized [dialogue sign]", p["narrated_classes"])
	}

	// A request that names the state the session already has is a no-op:
	// same snapshot, no second event — a change that changed nothing is not
	// an event.
	before := len(e.sessionEvents(id))
	if info, err := e.mgr.SetSettings(id, runtime.SessionSettings{
		Mode:            strPtr(runtime.ModePredictive),
		NarratedClasses: &[]string{"dialogue", "sign"},
	}); err != nil || info.Mode != runtime.ModePredictive {
		t.Fatalf("no-op settings = %v / %v, want the same snapshot", info, err)
	}
	if after := len(e.sessionEvents(id)); after != before {
		t.Fatalf("the no-op settings added %d events, want 0", after-before)
	}

	// The run settles with the policy the operator set: the terminal state is
	// untouched by the policy (it is not a stop), and the snapshot kept it.
	final := waitState(t, e.mgr, id, 180*time.Second)
	if final.State != runtime.StateStopped {
		t.Fatalf("terminal state = %s, want stopped (stats %+v)", final.State, final.Stats)
	}
}

func strPtr(s string) *string { return &s }
