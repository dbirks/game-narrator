// evaluate_test.go exercises the assertion evaluators over hand-built
// event lists: each evaluator is probed with a passing and a failing shape,
// so the pass/fail detail strings are exercised, not just the booleans.

package scenario

import (
	"encoding/json"
	"testing"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/pipeline"
)

// --- evaluators ---------------------------------------------------------------

func ev(name string, data map[string]any) events.Envelope {
	b, err := json.Marshal(data)
	if err != nil {
		panic(err)
	}
	return events.Envelope{SchemaVersion: 1, Name: name, Data: b}
}

func commitEvent(id string, tsNS int64, gen int64, occurrence string) events.Envelope {
	return ev(events.MessageCommitted, map[string]any{
		"message_id":             id,
		"text":                   "T",
		"normalized_text":        "t",
		"region_id":              "bottom",
		"generation":             gen,
		"dialogue_occurrence_id": occurrence,
		"transitions": []map[string]any{{
			"from": "ready", "to": "committed",
			"frame_seq": 15, "capture_timestamp_ns": tsNS,
		}},
	})
}

func sc(assertions ...Assertion) *Scenario { return &Scenario{Assertions: assertions} }

func TestEvalMessageDetected(t *testing.T) {
	a := Assertion{Type: "message_detected", MessageID: "demo-page-1", MaxDelayMS: 1500}
	evs := []events.Envelope{commitEventWithText("demo-page-1", lineA, 1_500_000_000)}
	if p, d := sc(a).Evaluate(evs, pipeline.Stats{}, clipTruth).Results[0].Passed, detailOf(sc(a).Evaluate(evs, pipeline.Stats{}, clipTruth)); !p {
		t.Fatalf("want pass, got fail: %s", d)
	}
	late := []events.Envelope{commitEventWithText("demo-page-1", lineA, 3_000_000_000)}
	if r := sc(a).Evaluate(late, pipeline.Stats{}, clipTruth); r.Passed {
		t.Fatalf("1700 ms delay must fail the 1500 ms limit")
	}
	if r := sc(a).Evaluate(nil, pipeline.Stats{}, clipTruth); r.Passed {
		t.Fatalf("a missing commit must fail")
	}
}

func TestEvalCommittedCount(t *testing.T) {
	a := Assertion{Type: "message_committed_count", MessageID: "demo-page-1", Count: 1}
	two := []events.Envelope{
		commitEvent("demo-page-1", 1, 1, "occ-1"),
		commitEvent("demo-page-1", 2, 1, "occ-1"),
	}
	if r := sc(a).Evaluate(two, pipeline.Stats{}, clipTruth); r.Passed {
		t.Fatalf("two commits must fail count=1")
	}
	if r := sc(Assertion{Type: a.Type, MessageID: a.MessageID, Count: 2}).Evaluate(two, pipeline.Stats{}, clipTruth); !r.Passed {
		t.Fatalf("two commits must pass count=2")
	}
}

func TestEvalSpeechCancelled(t *testing.T) {
	a := Assertion{Type: "speech_cancelled", Reason: "dialogue_advanced", Count: 1}
	matches := []events.Envelope{ev(events.SpeechCancelled, map[string]any{"reason": "dialogue_advanced", "message_id": "demo-page-1"})}
	if r := sc(a).Evaluate(matches, pipeline.Stats{}, clipTruth); !r.Passed {
		t.Fatalf("want pass: %s", detailOf(sc(a).Evaluate(matches, pipeline.Stats{}, clipTruth)))
	}
	other := []events.Envelope{ev(events.SpeechCancelled, map[string]any{"reason": "paused"})}
	if r := sc(a).Evaluate(other, pipeline.Stats{}, clipTruth); r.Passed {
		t.Fatalf("a different reason must not count")
	}
	two := append(append([]events.Envelope{}, matches...), ev(events.SpeechCancelled, map[string]any{"reason": "dialogue_advanced"}))
	if r := sc(a).Evaluate(two, pipeline.Stats{}, clipTruth); r.Passed {
		t.Fatalf("two cancellations must fail count=1")
	}
}

func TestEvalNoDuplicateCommit(t *testing.T) {
	sameOcc := []events.Envelope{
		commitEvent("demo-page-1", 1, 1, "occ-1"),
		commitEvent("demo-page-1", 2, 1, "occ-1"),
	}
	if r := sc(Assertion{Type: "no_duplicate_commit"}).Evaluate(sameOcc, pipeline.Stats{}, clipTruth); r.Passed {
		t.Fatalf("the same occurrence committed twice must fail")
	}
	diffOcc := []events.Envelope{
		commitEvent("demo-page-1", 1, 1, "occ-1"),
		commitEvent("demo-page-2", 2, 2, "occ-2"),
	}
	if r := sc(Assertion{Type: "no_duplicate_commit"}).Evaluate(diffOcc, pipeline.Stats{}, clipTruth); !r.Passed {
		t.Fatalf("two distinct occurrences must pass")
	}
	// A commit with no occurrence id falls back to (message, region, generation).
	noID := []events.Envelope{
		commitEvent("demo-page-1", 1, 1, ""),
		commitEvent("demo-page-1", 2, 1, ""),
	}
	if r := sc(Assertion{Type: "no_duplicate_commit"}).Evaluate(noID, pipeline.Stats{}, clipTruth); r.Passed {
		t.Fatalf("the fallback identity must catch the duplicate")
	}
}

func TestEvalNoStaleGeneration(t *testing.T) {
	a := Assertion{Type: "no_stale_generation_applied"}
	if r := sc(a).Evaluate(nil, pipeline.Stats{}, clipTruth); !r.Passed {
		t.Fatalf("a clean Stats must pass")
	}
	st := pipeline.Stats{StaleApplied: 1}
	if r := sc(a).Evaluate(nil, st, clipTruth); r.Passed {
		t.Fatalf("StaleApplied=1 must fail")
	}
}

func TestEvalEventsOrder(t *testing.T) {
	a := Assertion{Type: "events_order", Names: []string{events.TextObserved, events.MessageCommitted}}
	inOrder := []events.Envelope{
		ev(events.FrameObserved, map[string]any{}),
		ev(events.TextObserved, map[string]any{}),
		ev(events.MessageCandidates, map[string]any{}),
		ev(events.MessageCommitted, map[string]any{}),
	}
	if r := sc(a).Evaluate(inOrder, pipeline.Stats{}, clipTruth); !r.Passed {
		t.Fatalf("a subsequence in order must pass")
	}
	reversed := []events.Envelope{ev(events.MessageCommitted, map[string]any{}), ev(events.TextObserved, map[string]any{})}
	if r := sc(a).Evaluate(reversed, pipeline.Stats{}, clipTruth); r.Passed {
		t.Fatalf("reversed events must fail")
	}
	missing := []events.Envelope{ev(events.TextObserved, map[string]any{})}
	if r := sc(a).Evaluate(missing, pipeline.Stats{}, clipTruth); r.Passed {
		t.Fatalf("a missing event must fail")
	}
}

func TestEvaluateAggregates(t *testing.T) {
	a := Assertion{Type: "message_detected", MessageID: "demo-page-1", MaxDelayMS: 1500}
	b := Assertion{Type: "no_stale_generation_applied"}
	r := sc(a, b).Evaluate(nil, pipeline.Stats{}, clipTruth)
	if r.Passed || len(r.Results) != 2 || !r.Results[1].Passed || r.Results[0].Passed {
		t.Fatalf("report = %+v", r)
	}
}

// commitEventWithText is a commit whose carried text is a real line, for the
// message_detected matching against truth.
func commitEventWithText(id, text string, tsNS int64) events.Envelope {
	return ev(events.MessageCommitted, map[string]any{
		"message_id":             id,
		"text":                   text,
		"normalized_text":        fold(text),
		"region_id":              "bottom",
		"generation":             1,
		"dialogue_occurrence_id": "occ-" + id,
		"transitions": []map[string]any{{
			"from": "ready", "to": "committed",
			"frame_seq": 15, "capture_timestamp_ns": tsNS,
		}},
	})
}

func detailOf(r Report) string {
	if len(r.Results) == 0 {
		return ""
	}
	return r.Results[0].Detail
}
