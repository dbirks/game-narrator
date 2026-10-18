// settle.go grades pass slices the moment they settle and holds the
// harness's pass-timeline state to its bound (passstate.go). Grading is
// incremental — a settled slice is graded and dropped where it settles — so
// the retained memory is the unsettled horizon (passBound slices), not the
// run's pass count. The run-level stale-generation check that the old
// end-of-run evaluation ran per slice against the *final* pipeline stats is
// kept run-level in evaluate.go: that counter is cumulative, so attributing
// it to one pass slice was arbitrary to begin with (the old form failed
// every slice with the same global number).

package soak

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/pipeline"
	"github.com/dbirks/game-narrator/internal/scenario"
)

// passStateLocked returns the passState for stamp, creating it if needed.
// Called with h.passMu held.
func (h *harness) passStateLocked(stamp passStamp) *passState {
	ps := h.passStates[stamp]
	if ps == nil {
		ps = &passState{stamp: stamp, started: map[string]int64{}, terminal: map[string]bool{}}
		h.passStates[stamp] = ps
	}
	return ps
}

// closePassLocked marks the pass's feeding as finished, which is what makes
// its slice gradeable (a pass still being fed is never settled, no matter
// how many utterances have closed). Called with h.passMu held.
func (h *harness) closePassLocked(stamp passStamp) {
	if ps := h.passStates[stamp]; ps != nil {
		ps.closed = true
	}
}

// filePassEvent appends one event to the pass's slice, updates the
// utterance-lifecycle state the settlement test reads, and grades the slice
// when it is settled. Called with h.passMu held.
func (h *harness) filePassEvent(stamp passStamp, e events.Envelope) {
	ps := h.passStateLocked(stamp)
	ps.events = append(ps.events, e)
	switch e.Name {
	case events.MessageCommitted:
		// The settlement window runs from the latest commit: a commit's
		// utterance can still start up to settleHorizon after it.
		ps.lastCommit = time.Now()
	case events.SpeechReady, events.SpeechStarted:
		var p struct {
			Occurrence string `json:"dialogue_occurrence_id"`
			Generation int64  `json:"generation"`
		}
		if err := json.Unmarshal(e.Data, &p); err == nil && p.Occurrence != "" {
			ps.started[p.Occurrence] = p.Generation
		}
	case events.SpeechEnded, events.SpeechCancelled:
		var p struct {
			Occurrence string `json:"dialogue_occurrence_id"`
		}
		if err := json.Unmarshal(e.Data, &p); err == nil && p.Occurrence != "" {
			ps.terminal[p.Occurrence] = true
		}
	}
	h.settleOne(stamp, time.Now())
}

// settleOne grades and drops the slice for stamp when it is settled at now.
// Called with h.passMu held.
func (h *harness) settleOne(stamp passStamp, now time.Time) {
	ps := h.passStates[stamp]
	if ps == nil || !ps.settled(now) {
		return
	}
	h.dropLocked(ps)
}

// settlePasses grades every settled slice and, when the pending set has
// grown past its bound, force-grades the oldest unsettled slice. A slice
// whose utterances are still open at that point is a stuck utterance: grading
// it as-is fails the voice contract, which is exactly what a stuck utterance
// owes (the run-level stale-generation check still runs on the final stats in
// evaluate.go, so nothing the bound hides is unreported).
func (h *harness) settlePasses() {
	now := time.Now()
	h.passMu.Lock()
	defer h.passMu.Unlock()
	for _, ps := range h.passStates {
		if ps.settled(now) {
			h.dropLocked(ps)
		}
	}
	for len(h.passStates) > h.passBound {
		oldest := h.oldestUnsettledLocked(now)
		if oldest == nil {
			return // nothing left but settled slices: the loop above took them
		}
		h.dropLocked(oldest)
	}
}

// oldestUnsettledLocked picks the slice to force-grade when the pending set
// is over its bound: the lowest (clip, pass) stamp still unsettled at now
// among the closed passes — a pass still being fed is never force-graded (its
// commit events are still on the way), and at most one pass is being fed at a
// time, so excluding it cannot grow the pending set. It is the closed slice
// furthest from any event that could still arrive. Called with h.passMu held.
func (h *harness) oldestUnsettledLocked(now time.Time) *passState {
	var oldest *passState
	for _, ps := range h.passStates {
		if ps.settled(now) || !ps.closed {
			continue
		}
		if oldest == nil || stampLess(ps.stamp, oldest.stamp) {
			oldest = ps
		}
	}
	return oldest
}

// dropLocked grades one settled (or force-graded) slice and releases it:
// the slice and the occurrence routing of every occurrence that reached a
// terminal in it (a terminal occurrence will never emit another speech
// event, so its routing and its spoken-dedupe entry are dead — the spoken
// entry is reclaimed by the next pruneSpokenLocked sweep; dropLocked must
// not take spokenMu under passMu, the speak path holds them in the other
// order). Called with h.passMu held.
func (h *harness) dropLocked(ps *passState) {
	failures := h.gradeSlice(ps.stamp, ps.events)
	h.passFailuresMu.Lock()
	h.passFailures = append(h.passFailures, failures...)
	h.passFailuresMu.Unlock()
	h.passesGraded++
	if passHasCommit(ps.events) {
		h.passCommitSlices++
	}
	h.releaseSliceLocked(ps)
}

// releaseSliceLocked releases a graded slice's state: the slice's map entry
// and the occurrence routing of every occurrence the slice named — not just
// the started ones: a committed-but-abstained occurrence never emits a speech
// event, so it is not in ps.started and its routing entry would otherwise
// outlive the slice that owned it. A late event for a released occurrence
// finds no occPass entry and is dropped, which is the intended "this slice
// is graded" signal. Called with h.passMu held.
func (h *harness) releaseSliceLocked(ps *passState) {
	delete(h.passStates, ps.stamp)
	seen := make(map[string]struct{})
	for _, e := range ps.events {
		if occ := occurrenceOf(e); occ != "" {
			seen[occ] = struct{}{}
		}
	}
	for occ := range seen {
		delete(h.occPass, occ)
	}
	*ps = passState{} // release the slice's events and maps
}

// stampLess is the pass order the sidecar feeds: by clip, then pass.
func stampLess(a, b passStamp) bool {
	if a.clip != b.clip {
		return a.clip < b.clip
	}
	return a.pass < b.pass
}

// passHasCommit reports whether the slice carries a commit event.
func passHasCommit(evs []events.Envelope) bool {
	for _, e := range evs {
		if e.Name == events.MessageCommitted {
			return true
		}
	}
	return false
}

// occurrenceOf extracts the dialogue occurrence id from one slice event (the
// commit and speech payloads both carry dialogue_occurrence_id); empty when
// the payload does not name one.
func occurrenceOf(e events.Envelope) string {
	var p struct {
		Occurrence string `json:"dialogue_occurrence_id"`
	}
	if err := json.Unmarshal(e.Data, &p); err != nil {
		return ""
	}
	return p.Occurrence
}

// gradeSlice runs one pass slice's assertions: the scenario vocabulary
// (minus the run-level stale-generation check) plus the loop-aware
// stale-speech and voice-contract checks (see evaluate.go). The per-pass
// assertions read only the slice, so the stats argument is the zero value.
func (h *harness) gradeSlice(stamp passStamp, evs []events.Envelope) []string {
	if len(evs) == 0 {
		return nil
	}
	truth := h.clips[stamp.clip].truth
	sc := &scenario.Scenario{
		ID:         "soak-pass-" + itoa(stamp.clip) + "-" + itoa(stamp.pass),
		Assertions: perPassAssertions(h.lineIDs, evs),
	}
	var failures []string
	rep := sc.Evaluate(evs, pipeline.Stats{}, truth)
	for _, a := range rep.Results {
		if !a.Passed {
			failures = append(failures,
				fmt.Sprintf("clip %d pass %d %s: %s", stamp.clip, stamp.pass, a.Type, a.Detail))
		}
	}
	if off := staleSpeechOffenders(evs); len(off) > 0 {
		failures = append(failures, fmt.Sprintf("clip %d pass %d no_stale_speech: %s",
			stamp.clip, stamp.pass, join(off, 2)))
	}
	if passHasSpeech(evs) {
		if off := voiceContractOffenders(evs, h.cfg.Voice); len(off) > 0 {
			failures = append(failures, fmt.Sprintf("clip %d pass %d no_voice_substitution: %s",
				stamp.clip, stamp.pass, join(off, 2)))
		}
	}
	return failures
}
