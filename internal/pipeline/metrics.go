package pipeline

import (
	"encoding/json"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
)

// frameVoiceAnchorCap bounds the frame→voice anchor table. The lines that
// anchor here are the commits the run makes, and the bound is far above any
// box's lifetime: a table at its bound is not keeping up, and a reset is the
// same honest shape the session's own commit→voice sampler takes (a reset
// rather than an unbounded table; the lines shed measure nothing).
const frameVoiceAnchorCap = 64

// noteFrameVoiceAnchor books one committed line's frame-entry anchor, keyed
// by the dialogue occurrence id the speech wiring's audio-start event carries
// back. The anchor is the frame-entry time in the cfg.Now() domain — the same
// domain the pipeline's own latency table uses — so the interval the voice
// wiring later books is frame→voice, not commit→voice (the session's own
// sampler owns that one, docs/spec/13 "first audible audio").
func (r *Runner) noteFrameVoiceAnchor(evs []events.Envelope, entered time.Time) {
	occ := ""
	for _, e := range evs {
		if e.Name != events.MessageCommitted {
			continue
		}
		var p struct {
			Occurrence string `json:"dialogue_occurrence_id"`
		}
		if err := json.Unmarshal(e.Data, &p); err != nil {
			continue
		}
		occ = p.Occurrence
		break
	}
	if occ == "" {
		return // no occurrence id on the payload: nothing to key on, nothing to book
	}
	r.frameVoiceMu.Lock()
	defer r.frameVoiceMu.Unlock()
	if len(r.frameVoiceAnchors) >= frameVoiceAnchorCap {
		r.frameVoiceAnchors = make(map[string]time.Time, 8)
	}
	r.frameVoiceAnchors[occ] = entered
}

// ObserveFrameToVoice books one frame→voice sample: from the frame that
// entered the pipeline for a committed line to the line's audio starting
// (docs/spec/13 "first audible audio"). The voice wiring calls it from the
// speech.started event it publishes; a line whose commit carried no anchor
// (telemetry unwired, an unparseable payload, or a table reset) books
// nothing and reports false. Nil-safe: a runner built without a catalog is
// a permanent no-op.
func (r *Runner) ObserveFrameToVoice(occurrenceID string) bool {
	r.frameVoiceMu.Lock()
	entered, ok := r.frameVoiceAnchors[occurrenceID]
	if ok {
		delete(r.frameVoiceAnchors, occurrenceID)
	}
	r.frameVoiceMu.Unlock()
	if !ok || r.metrics == nil {
		return false
	}
	r.metrics.FrameToVoice(r.mLabels, r.cfg.Now().Sub(entered))
	return true
}
