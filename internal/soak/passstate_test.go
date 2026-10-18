// passstate_test.go pins the soak harness's bounds (the ADR-0045 amendment
// fix): the pass-timeline state — the per-pass event slices, the occurrence
// routing and the spoken dedupe — must stay at their documented bounds no
// matter how many passes a run has, instead of growing with the pass count
// (the 1-hour soak's RSS growth was exactly that).

package soak

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
)

// boundedHarness builds the real harness (profile, clip, matcher, runner,
// session) without starting any load: the bookkeeping under test runs against
// the same grading code a live run uses.
func boundedHarness(t *testing.T) *harness {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	cfg := Config{
		Duration:    30 * time.Second,
		SampleEvery: 5 * time.Second,
		RepoRoot:    root,
		Clips:       []string{"testdata/synthetic/replay/dialogue-advance-001"},
		Profile:     "dialogue-advance-001",
	}
	if err := cfg.fillDefaults(); err != nil {
		t.Fatalf("fillDefaults: %v", err)
	}
	h, err := newHarness(cfg)
	if err != nil {
		t.Fatalf("newHarness: %v", err)
	}
	if err := h.build(); err != nil {
		t.Fatalf("build: %v", err)
	}
	return h
}

// TestPassTimelineBounded pushes N ≫ passBound passes through the settle
// path and asserts the bound holds at every step: the pending slice count
// never exceeds passBound (+ the one pass being fed), every pass still gets
// graded (nothing is lost, the force-grade path fails the stuck ones instead
// of dropping their verdict), and the occurrence routing does not grow with
// the pass count.
func TestPassTimelineBounded(t *testing.T) {
	h := boundedHarness(t)
	defer h.close()
	const n = 100 // ≫ h.passBound (~9 for the 2.3 s clip)
	for p := 0; p < n; p++ {
		stamp := passStamp{clip: 0, pass: p}
		occ := fmt.Sprintf("occ-%d", p)
		h.passMu.Lock()
		h.occPass[occ] = stamp
		h.passMu.Unlock()
		// One commit per pass (the sidecar's layer-B commitment of record),
		// plus a full utterance every other pass so the voice-contract check
		// has something to see.
		h.pub.addPassEvent(stamp, commitEvent("demo-page-1", occ, int64(p+1)))
		if p%2 == 0 {
			h.pub.trackPass(speechEvent(events.SpeechReady, occ, map[string]any{"voice_id": contractVoice}))
			h.pub.trackPass(speechEvent(events.SpeechStarted, occ, map[string]any{"voice_id": contractVoice}))
			h.pub.trackPass(speechEvent(events.SpeechEnded, occ, map[string]any{"voice_id": contractVoice, "result": "ended"}))
		}
		h.passMu.Lock()
		h.closePassLocked(stamp)
		h.passMu.Unlock()
		h.settlePasses()
		// The bound must hold at every step, not just at the end: the force-
		// grade path is what holds it while the settle window (20 s) has not
		// elapsed for the recent passes.
		h.passMu.Lock()
		pending := len(h.passStates)
		h.passMu.Unlock()
		if pending > h.passBound+1 {
			t.Fatalf("after pass %d/%d: %d pending pass slices (bound %d + 1 open)", p, n, pending, h.passBound)
		}
	}
	// The trailing ~passBound passes are still inside their settle window at
	// loop end — exactly the residue a real run keeps until finishUtterances
	// and the run-end grading: drain it the same way and assert nothing is
	// lost (every pass graded, settled or force-graded, stuck ones failed).
	if nResidual, _ := h.gradeRemaining(); nResidual == 0 {
		t.Fatalf("run-end residual grading graded nothing (the trailing passes never entered the settle path)")
	}
	if h.passesGraded != n {
		t.Fatalf("graded %d of %d passes (the bound must force-grade, not drop, the stuck ones)", h.passesGraded, n)
	}
	if h.passCommitSlices != n {
		t.Fatalf("graded %d commit slice(s), want %d (one commit per pass)", h.passCommitSlices, n)
	}
	h.passMu.Lock()
	pending := len(h.passStates)
	routed := len(h.occPass)
	h.passMu.Unlock()
	if pending != 0 || routed != 0 {
		t.Fatalf("after the run-end drain: %d pending slice(s) and %d occurrence routing(s) remain (want 0 and 0: the graded slices release theirs)", pending, routed)
	}
}

// TestPassStateSettled pins the settlement predicate itself: a slice is
// settled only when the pass stopped being fed, the last commit's utterance
// window has closed, and every started utterance has a terminal.
func TestPassStateSettled(t *testing.T) {
	now := time.Now()
	t.Run("an open pass is never settled", func(t *testing.T) {
		ps := &passState{started: map[string]int64{}, terminal: map[string]bool{}}
		if ps.settled(now) {
			t.Fatal("pass still being fed reported settled (its commits are still on the way)")
		}
	})
	t.Run("a closed commit-free pass settles", func(t *testing.T) {
		ps := &passState{closed: true, started: map[string]int64{}, terminal: map[string]bool{}}
		if !ps.settled(now) {
			t.Fatal("closed pass with no commits and no utterance reported unsettled")
		}
	})
	t.Run("a fresh commit holds the slice open", func(t *testing.T) {
		ps := &passState{closed: true, lastCommit: now, started: map[string]int64{}, terminal: map[string]bool{}}
		if ps.settled(now) {
			t.Fatal("slice settled while its last commit's utterance window is open")
		}
		if !ps.settled(now.Add(settleHorizon + time.Millisecond)) {
			t.Fatal("slice still unsettled after the settle window closed with no utterance started")
		}
	})
	t.Run("a started utterance without a terminal holds the slice open", func(t *testing.T) {
		ps := &passState{
			closed:   true,
			started:  map[string]int64{"occ-a": 1},
			terminal: map[string]bool{},
		}
		if ps.settled(now.Add(settleHorizon + time.Millisecond)) {
			t.Fatal("slice settled with a started utterance that never reached a terminal")
		}
		ps.terminal["occ-a"] = true
		if !ps.settled(now.Add(settleHorizon + time.Millisecond)) {
			t.Fatal("slice unsettled after the utterance's terminal filed")
		}
	})
}

// TestSpokenBounded pushes N ≫ spokenCap occurrences through the double-speak
// guard with every entry still live (routed, in an open slice) so the dead-
// entry sweep evicts nothing: the map must still stay at its bound (the
// clear fallback, whose safety the session's occurrence fence provides).
func TestSpokenBounded(t *testing.T) {
	h := boundedHarness(t)
	defer h.close()
	const n = spokenCap * 4
	for i := 0; i < n; i++ {
		occ := fmt.Sprintf("occ-%d", i)
		stamp := passStamp{clip: 0, pass: i}
		h.passMu.Lock()
		h.occPass[occ] = stamp
		h.passStateLocked(stamp) // open slice: the entry is not dead
		h.passMu.Unlock()
		if h.noteSpoken(occ, stamp) {
			t.Fatalf("occurrence %d reported already spoken on first note", i)
		}
		h.spokenMu.Lock()
		size := len(h.spoken)
		h.spokenMu.Unlock()
		if size > spokenCap {
			t.Fatalf("after %d occurrence(s): spoken map holds %d entries (bound %d)", i, size, spokenCap)
		}
	}
	// A second note for a live occurrence is still deduped.
	occ := fmt.Sprintf("occ-%d", n-1)
	if !h.noteSpoken(occ, passStamp{clip: 0, pass: n - 1}) {
		t.Fatal("second note for the last occurrence was not deduped")
	}
}
