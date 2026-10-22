package pipeline_test

// The ADR-0160 early-speaker ask: a confident local top candidate on a shared
// speaker slot owes the adjudicator's speaker attribution even though the local
// policy committed the line on its own (the live shape that committed
// MATCH_FUZZY with high confidence and never asked Clef). The rank worker must
// send the speaker-only adjudication for that top candidate — the speaker +
// speaker_kind questions, the frame attached — and the commit for the
// occurrence must carry the answer's attribution while keeping the local
// line's own verdict.

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/pipeline"
	"github.com/dbirks/game-narrator/internal/session"
)

// earlySpeakerDecider answers every decision with the ADR-0160 shape: the
// speaker-only attribution answer (a confident named speaker plus a confident
// apparent kind), overlaid on the local top candidate through the
// attribution-only path.
type earlySpeakerDecider struct {
	mu        sync.Mutex
	asked     []pipeline.DecisionRequest
	abstained bool
}

func (d *earlySpeakerDecider) Decide(ctx context.Context, req pipeline.DecisionRequest) pipeline.DecisionReply {
	_ = ctx
	d.mu.Lock()
	defer d.mu.Unlock()
	d.asked = append(d.asked, req)
	if d.abstained {
		return pipeline.DecisionReply{Provider: "scripted", Reason: "abstained"}
	}
	top := ""
	if len(req.Ranking.Candidates) > 0 {
		top = req.Ranking.Candidates[0].MessageID
	}
	return pipeline.DecisionReply{
		Msg: &session.DecisionMsg{
			RegionID:              req.RegionID,
			ObservationSeq:        req.ObservationSeq,
			MessageID:             top,
			SpeakerID:             "char-boy",
			SpeakerKnown:          true,
			SpeakerConfidence:     0.9,
			SpeakerKind:           "boy",
			SpeakerKindConfidence: 0.9,
			SpeechEligible:        true,
			Accepted:              true,
			Provider:              "scripted",
			Model:                 "scripted-decider",
			Reason:                "answered",
			AttributionOnly:       true,
		},
		Provider:              "scripted",
		RequestID:             "scripted-req",
		Model:                 "scripted-decider",
		Accepted:              true,
		Reason:                "answered",
		ChosenID:              top,
		Speaker:               "char-boy",
		SpeakerConfidence:     0.9,
		SpeakerKnown:          true,
		SpeakerKind:           "boy",
		SpeakerKindConfidence: 0.9,
	}
}

func (d *earlySpeakerDecider) snapshot() []pipeline.DecisionRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]pipeline.DecisionRequest, len(d.asked))
	copy(out, d.asked)
	return out
}

// TestEarlySpeakerAskFiresForConfidentSharedSlotTop: the first read whose top
// local candidate is a shared-slot line asks the early-speaker adjudication
// (ADR-0160), before the commit; the answer overlays the speaker + kind onto
// the local line, and the commit keeps the local verdict's reason.
func TestEarlySpeakerAskFiresForConfidentSharedSlotTop(t *testing.T) {
	dec := &earlySpeakerDecider{}
	h := newHarness(t, &fakeOCR{script: ocrScript(t), delay: fastScript, respectCtx: true}, func(c *pipeline.Config) {
		c.Mode = pipeline.ModeReplay
		c.Decider = dec
		c.SharedSpeakerMessages = map[string]bool{msgIDLineA: true}
	})
	st, evs, err := h.run(t, h.source(0, false))
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	reqs := dec.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("early speaker-only asks = %d (%+v), want exactly 1 (one per box occurrence, at most)", len(reqs), reqs)
	}
	if !reqs[0].SpeakerOnly {
		t.Fatalf("the early ask is not speaker-only: %+v", reqs[0])
	}
	if got := reqs[0].Ranking.Candidates[0].MessageID; got != msgIDLineA {
		t.Fatalf("the early ask's top candidate = %q, want %q", got, msgIDLineA)
	}
	if st.Decisions != 1 {
		t.Fatalf("decisions = %d, want 1 (the single confident shared-slot line asked once)", st.Decisions)
	}

	// The early answer's attribution rode the commit; the line choice and the
	// local verdict's reason stand (no CLEF_DISAMBIGUATED: the answer asked for
	// attribution only, never narrowed the confident local top).
	var cp struct {
		MessageID       string  `json:"message_id"`
		Reason          string  `json:"reason"`
		SpeakerID       string  `json:"speaker_id"`
		SpeakerKnown    bool    `json:"speaker_known"`
		SpeakerConf     float64 `json:"speaker_confidence"`
		SpeakerKind     string  `json:"speaker_kind"`
		SpeakerKindConf float64 `json:"speaker_kind_confidence"`
	}
	found := false
	for _, e := range evs {
		if e.Name != events.MessageCommitted {
			continue
		}
		var p struct {
			MessageID       string  `json:"message_id"`
			Reason          string  `json:"reason"`
			SpeakerID       string  `json:"speaker_id"`
			SpeakerKnown    bool    `json:"speaker_known"`
			SpeakerConf     float64 `json:"speaker_confidence"`
			SpeakerKind     string  `json:"speaker_kind"`
			SpeakerKindConf float64 `json:"speaker_kind_confidence"`
		}
		_ = json.Unmarshal(e.Data, &p)
		if p.MessageID == msgIDLineA {
			cp, found = p, true
			break
		}
	}
	if !found {
		t.Fatalf("no message.committed for %q in the event log", msgIDLineA)
	}
	if cp.Reason == "CLEF_DISAMBIGUATED" {
		t.Fatalf("the early-speaker answer must not narrow the confident local commit: reason = %q", cp.Reason)
	}
	if cp.Reason != "MATCH_EXACT" {
		t.Fatalf("commit reason = %q, want the local confident verdict MATCH_EXACT", cp.Reason)
	}
	if !cp.SpeakerKnown || cp.SpeakerID != "char-boy" || cp.SpeakerConf != 0.9 {
		t.Fatalf("commit speaker attribution = %+v, want the early answer's {char-boy, 0.9}", cp)
	}
	if cp.SpeakerKind != "boy" || cp.SpeakerKindConf != 0.9 {
		t.Fatalf("commit speaker kind = %q/%.1f, want the early answer's boy/0.9", cp.SpeakerKind, cp.SpeakerKindConf)
	}
}

// TestEarlySpeakerAskNotFiredForNonSharedTop: a confident local top candidate
// that is not on a shared slot keeps the pre-ADR-0160 behaviour — no early ask,
// the commit rides the local evidence alone.
func TestEarlySpeakerAskNotFiredForNonSharedTop(t *testing.T) {
	dec := &earlySpeakerDecider{}
	h := newHarness(t, &fakeOCR{script: ocrScript(t), delay: fastScript, respectCtx: true}, func(c *pipeline.Config) {
		c.Mode = pipeline.ModeReplay
		c.Decider = dec
		// No shared-slot line: nobody owes an early ask.
		c.SharedSpeakerMessages = map[string]bool{}
	})
	st, evs, err := h.run(t, h.source(0, false))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(dec.asked) != 0 {
		t.Fatalf("early speaker-only asks = %d, want 0 (no shared slot on this corpus)", len(dec.asked))
	}
	if st.Decisions != 0 {
		t.Fatalf("decisions = %d, want 0 (no locally ambiguous ranking was offered)", st.Decisions)
	}
	var commits int
	for _, e := range evs {
		if e.Name != events.MessageCommitted {
			continue
		}
		var p struct {
			MessageID string `json:"message_id"`
		}
		_ = json.Unmarshal(e.Data, &p)
		if p.MessageID != "" {
			commits++
		}
	}
	if commits < 2 {
		t.Fatalf("commits = %d, want both clip lines", commits)
	}
}
