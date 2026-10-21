package pipeline_test

import (
	"context"
	"sync"
	"testing"

	"github.com/dbirks/game-narrator/internal/pipeline"
	"github.com/dbirks/game-narrator/internal/session"
)

// hookOrdering captures the ordering the OnAdjudicating requirement proves:
// the hook is invoked on the rank worker synchronously, just before the Clef
// call goes out, so by the time Decider.Decide runs the hook has already fired
// for that same ranking — and it fired before Decide returns.
//
// The two slices pair one-to-one: the pipeline calls the hook once, on the rank
// worker, immediately before each Decider.Decide, so the i-th hook call and the
// i-th Decide call answer the same ranking. All fields move under mu: the hook
// and the decider run on the pipeline's rank worker (one goroutine, one after
// the other), and the test's own goroutine reads them after the run, so the
// lock is what keeps the race detector honest.
type hookOrdering struct {
	mu                sync.Mutex
	hookMsgs          []string // every hook call's message id, in call order
	decideMsgs        []string // every Decide's Candidates[0].MessageID, in order
	hookFiredAtDecide bool     // captured by Decide at its entry
}

// hookDecider is the scripted Clef for the OnAdjudicating tests: at the moment
// Decide is called it records whether the hook has fired and the request's top
// candidate, then answers with the local top-1 (speakable) so the run commits
// cleanly and the assertions read the ordering, not a failure path.
type hookDecider struct {
	o *hookOrdering
}

func (d *hookDecider) Decide(ctx context.Context, req pipeline.DecisionRequest) pipeline.DecisionReply {
	_ = ctx
	d.o.mu.Lock()
	d.o.hookFiredAtDecide = len(d.o.hookMsgs) > 0 // the hook ran before this call
	if len(req.Ranking.Candidates) > 0 {
		d.o.decideMsgs = append(d.o.decideMsgs, req.Ranking.Candidates[0].MessageID)
	}
	d.o.mu.Unlock()
	if len(req.Ranking.Candidates) == 0 {
		return pipeline.DecisionReply{Provider: "scripted", Reason: "abstained"}
	}
	pick := req.Ranking.Candidates[0].MessageID
	return pipeline.DecisionReply{
		Msg: &session.DecisionMsg{
			RegionID:       req.RegionID,
			MessageID:      pick,
			SpeechEligible: true,
			Accepted:       true,
			Provider:       "scripted",
			Reason:         "answered",
		},
		Provider: "scripted",
		Reason:   "answered",
		ChosenID: pick,
	}
}

// newOnAdjudicatingHarness wires the corpus's replay harness with the
// OnAdjudicating seam and the scripted decider, with the adjudication on at a
// margin (1.0) no accepted ranking can clear: every committed line is offered
// to Clef, so the hook and the decider both run on the first READY box instead
// of the test hunting for the corpus's one ambiguous read (the same lever the
// runtime's decision test uses).
func newOnAdjudicatingHarness(t *testing.T, o *hookOrdering, dec *hookDecider) *harness {
	t.Helper()
	return newHarness(t, &fakeOCR{script: ocrScript(t), delay: fastScript, respectCtx: true}, func(c *pipeline.Config) {
		c.Mode = pipeline.ModeReplay
		c.Reducer.Adjudicate = true
		c.Reducer.AdjudicateMargin = 1.0
		c.Decider = dec
		// The seam under test: the runtime wires this to the speech prewarm.
		c.OnAdjudicating = func(messageID string) {
			o.mu.Lock()
			o.hookMsgs = append(o.hookMsgs, messageID)
			o.mu.Unlock()
		}
	})
}

// TestPipelineOnAdjudicatingHookFiresBeforeDecide is the clef-skip overlap's
// contract at the pipeline seam: when a ranking needs adjudication and the
// Decider is called, the OnAdjudicating hook has already run with that ranking's
// top candidate (Candidates[0].MessageID), so the runtime can prewarm that
// line's TTS while the Clef request is in flight.
func TestPipelineOnAdjudicatingHookFiresBeforeDecide(t *testing.T) {
	o := &hookOrdering{}
	dec := &hookDecider{o: o}
	h := newOnAdjudicatingHarness(t, o, dec)
	st, _, err := h.run(t, h.source(0, false))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.decideMsgs) == 0 {
		t.Fatalf("Decider.Decide was never called (decisions=%d): the adjudication must be asked for the hook to matter; the policy is on at a margin no accepted ranking can clear", st.Decisions)
	}
	if !o.hookFiredAtDecide {
		t.Fatalf("the hook had not fired by the first Decider.Decide call: the runtime prewarms the top candidate while the Clef request is in flight, so the hook must fire before Decide returns")
	}
	if len(o.hookMsgs) != len(o.decideMsgs) {
		t.Fatalf("hook fired %d times but Decide ran %d times: the pipeline calls the hook once, on the rank worker, immediately before each Decide", len(o.hookMsgs), len(o.decideMsgs))
	}
	for i, want := range o.decideMsgs {
		if got := o.hookMsgs[i]; got != want {
			t.Fatalf("hook call %d carried %q, want the same top candidate the Decide saw (%q): the hook must prewarm the exact line the ranking ranked first", i+1, got, want)
		}
		if got := o.hookMsgs[i]; got == "" {
			t.Fatalf("hook call %d carried an empty message id: the hook must carry the ranking's top candidate (Candidates[0].MessageID)", i+1)
		}
	}
}

// TestPipelineOnAdjudicatingNotFiredWithoutAdjudication is the gate the hook
// must respect: when the local policy does not need a second opinion (the
// adjudication off), the Decider is never called and neither is the hook — a
// confident local match must not trigger a speculative prewarm it will fence
// and waste.
func TestPipelineOnAdjudicatingNotFiredWithoutAdjudication(t *testing.T) {
	o := &hookOrdering{}
	dec := &hookDecider{o: o}
	h := newHarness(t, &fakeOCR{script: ocrScript(t), delay: fastScript, respectCtx: true}, func(c *pipeline.Config) {
		c.Mode = pipeline.ModeReplay
		// The Decider is wired but the adjudication is off (the default): a
		// confident local match commits on the local evidence alone.
		c.Decider = dec
		c.OnAdjudicating = func(messageID string) {
			o.mu.Lock()
			o.hookMsgs = append(o.hookMsgs, messageID)
			o.mu.Unlock()
		}
	})
	if _, _, err := h.run(t, h.source(0, false)); err != nil {
		t.Fatalf("run: %v", err)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.decideMsgs) != 0 {
		t.Fatalf("Decider.Decide called %d times with the adjudication off: a confident local match must not consult Clef", len(o.decideMsgs))
	}
	if len(o.hookMsgs) != 0 {
		t.Fatalf("OnAdjudicating fired %d times with the adjudication off: a confident local match must not trigger a speculative prewarm", len(o.hookMsgs))
	}
}
