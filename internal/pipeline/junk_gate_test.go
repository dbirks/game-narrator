package pipeline_test

import (
	"encoding/json"
	"testing"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/pipeline"
)

// The clef-junk-gate tests (ADR-0164, issue #44 follow-up): a read that is not
// dialogue-like never reaches the Decider, and a garbled real read still does.
// The invented reads are the board's own junk shapes plus the garbled-line half
// of the gate's caveat — both sides of the two-words-of-three-letters floor.

// TestClefJunkGateSkipsJunkReads runs the full pipeline on junk reads and
// asserts the gate's contract: no decision is asked, the skip is counted, and
// the skip rides the abstain's handling (the decision.resolved event carries
// the skip's reason rather than an answer's).
func TestClefJunkGateSkipsJunkReads(t *testing.T) {
	for _, junk := range []string{"4 = ae", "ot |", "a . \\ a. dal"} {
		t.Run(junk, func(t *testing.T) {
			eng := &garbleOCR{text: junk}
			dec := &scriptedDecider{answer: func(_ int, req pipeline.DecisionRequest) pipeline.DecisionReply {
				return abstainReply()
			}}
			r, mem, hub, src := newEscalationHarness(t, eng, dec, nil, nil)
			st, evs := runEscalation(t, r, hub, mem, src)

			if n := len(dec.snapshot()); n != 0 {
				t.Fatalf("decisions asked = %d, want 0 (the clef-junk-gate must skip junk reads before asking): %+v", n, dec.snapshot())
			}
			if st.Decisions != 0 {
				t.Fatalf("decisions = %d, want 0 (a skipped read is not an offer that went out)", st.Decisions)
			}
			if st.DecisionsSkippedJunk == 0 {
				t.Fatalf("decisions_skipped_junk = %d, want > 0 (the gate must count its own skips)", st.DecisionsSkippedJunk)
			}
			// The reply the skip synthesises is the zero DecisionReply with its
			// reason, so the resolved event carries the skip — the abstain's
			// handling, not an answer's.
			var skipReason string
			for _, ev := range evs {
				if ev.Name != events.DecisionResolved {
					continue
				}
				var m map[string]any
				if err := json.Unmarshal(ev.Data, &m); err != nil {
					t.Fatalf("decode decision.resolved payload: %v", err)
				}
				if v, ok := m["decision_reason"].(string); ok {
					skipReason = v
				}
			}
			if skipReason == "" {
				t.Fatalf("decision.resolved carried no decision_reason; the skip reply must ride the abstain's event handling")
			}
		})
	}
}

// TestClefJunkGateStillAsksGarbledRealRead is the caveat half of the gate: a
// garbled real read ("hurry u@, beg brother grandma") still carries two words of
// three or more letters, so it is dialogue-like and the adjudication goes out.
func TestClefJunkGateStillAsksGarbledRealRead(t *testing.T) {
	eng := &garbleOCR{text: "hurry u@, beg brother grandma"}
	dec := &scriptedDecider{answer: func(_ int, req pipeline.DecisionRequest) pipeline.DecisionReply {
		return abstainReply()
	}}
	r, mem, hub, src := newEscalationHarness(t, eng, dec, nil, nil)
	st, _ := runEscalation(t, r, hub, mem, src)

	seen := dec.snapshot()
	if len(seen) == 0 {
		t.Fatalf("decisions asked = 0, want >= 1 (the garbled real read must still be offered to Clef): decisions_skipped_junk = %d", st.DecisionsSkippedJunk)
	}
	if st.DecisionsSkippedJunk != 0 {
		t.Fatalf("decisions_skipped_junk = %d, want 0 (a dialogue-like garbled read is not skipped)", st.DecisionsSkippedJunk)
	}
	// The asked read is the garbled one, and it reached the Decider.
	if got := seen[0].RawText; got != "hurry u@, beg brother grandma" {
		t.Fatalf("decision request raw_text = %q, want the garbled real read", got)
	}
	// The run settled on the abstain the Decider's scripted answer produced.
	if st.Decisions != 1 || st.DecisionApplied != 0 {
		t.Fatalf("decisions/applied = %d/%d, want 1/0 (the scripted answer abstained)", st.Decisions, st.DecisionApplied)
	}
}
