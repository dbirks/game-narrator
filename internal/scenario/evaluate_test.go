// evaluate_test.go exercises the assertion evaluators over hand-built
// event lists: each evaluator is probed with a passing and a failing shape,
// so the pass/fail detail strings are exercised, not just the booleans.

package scenario

import (
	"encoding/json"
	"fmt"
	"strings"
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

// TestEvalMessageDetectedCorpusAnchor pins the engine-free anchor (the CI
// version drift: the CI distro's tesseract 5.3 and the dev host's 5.5 read
// the same preprocessed fixture crop differently — "crossing" vs
// "crossina" on the Black Water clip — same fixture, same commit decision,
// different glyphs): with the profile's corpus, the committed corpus line
// anchors the truth match, not the run's OCR text; without it, or for a
// commit the corpus cannot name, the OCR text is the anchor, as before.
func TestEvalMessageDetectedCorpusAnchor(t *testing.T) {
	a := Assertion{Type: "message_detected", MessageID: "bw-a", MaxDelayMS: 1500}
	truth := []Truth{{File: "frame-005.png", TsNS: 500_000_000, Text: "You paid the ferryman. The crossing is done now.", Line: "a", Full: true}}
	corpus := map[string]string{"bw-a": fold("You paid the ferryman. The crossing is done now.")}
	// A commit whose OCR text the truth never shows: the engine's glyph
	// error on the fixture's line.
	misread := []events.Envelope{commitEventWithText("bw-a", "you paid the ferryman. the crossina is done now.", 1_500_000_000)}

	// Without the corpus the OCR text is the anchor: the truth never showed
	// it, so the check fails — the version-sensitive shape.
	if r := sc(a).Evaluate(misread, pipeline.Stats{}, truth); r.Passed {
		t.Fatalf("without the corpus, the misread OCR text must not match the truth: %+v", r.Results)
	}
	// With the corpus, the committed corpus line is the anchor: the truth
	// shows it in full and the commit lands 1000 ms after it — pass.
	if r := sc(a).EvaluateWith(misread, pipeline.Stats{}, truth, EvaluateOpts{Corpus: corpus}); !r.Passed {
		t.Fatalf("the corpus line must anchor the commit regardless of the engine's read: %+v", r.Results)
	}
	// A commit the corpus cannot name keeps the OCR text as its anchor.
	unknown := []events.Envelope{commitEventWithText("unknown", "you paid the ferryman. the crossina is done now.", 1_500_000_000)}
	if r := sc(a).EvaluateWith(unknown, pipeline.Stats{}, truth, EvaluateOpts{Corpus: corpus}); r.Passed {
		t.Fatalf("a commit the corpus cannot name must still anchor on the OCR text: %+v", r.Results)
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

// TestEvalCommittedCountEngineScope covers the engines: scoping: a check
// scoped to one OCR engine is graded when the run declares that engine (or
// none), and is reported not-applicable — a passing row, never a failure —
// when the run's engine is a different one.
func TestEvalCommittedCountEngineScope(t *testing.T) {
	a := Assertion{Type: "message_committed_count", MessageID: "demo-page-1", Count: 1, Engines: []string{"tesseract"}}
	none := []events.Envelope{}

	// A run that declares no engine (the bare Evaluate path) grades the check
	// like before: zero commits fails count=1.
	if r := sc(a).Evaluate(none, pipeline.Stats{}, clipTruth); r.Passed {
		t.Fatalf("scoped check must still be graded when no engine is declared")
	}
	// A run on the scoped engine grades it: one commit passes count=1.
	ones := []events.Envelope{commitEvent("demo-page-1", 1, 1, "occ-1")}
	if r := sc(a).EvaluateWithOCREngine("tesseract", ones, pipeline.Stats{}, clipTruth); !r.Passed {
		t.Fatalf("scoped check must pass under its own engine: %+v", r.Results)
	}
	// A run on the other engine reports it not-applicable: a passing row that
	// says why it was not graded, never a failure for a property the run's
	// engine does not have.
	if r := sc(a).EvaluateWithOCREngine("truth", ones, pipeline.Stats{}, clipTruth); !r.Passed {
		t.Fatalf("out-of-scope engine must not fail the check: %+v", r.Results)
	} else {
		found := false
		for _, res := range r.Results {
			if res.Type == "message_committed_count" && !strings.Contains(res.Detail, "not applicable") {
				t.Fatalf("not-applicable row must say why, got %q", res.Detail)
			}
			if strings.Contains(res.Detail, "not applicable") {
				found = true
			}
		}
		if !found {
			t.Fatalf("no not-applicable row reported: %+v", r.Results)
		}
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
	// Two author assertions plus the implicit replay_not_degraded check.
	if r.Passed || len(r.Results) != 3 || !r.Results[2].Passed || !r.Results[1].Passed || r.Results[0].Passed {
		t.Fatalf("report = %+v", r)
	}
	if r.Results[2].Type != "replay_not_degraded" {
		t.Fatalf("implicit check type = %q, want replay_not_degraded", r.Results[2].Type)
	}
}

// TestEvalReplayNotDegradedIsImplicit covers both halves of the implicit
// assertion: a clean run passes with the number stated, and a degraded run
// fails the whole report with the frames named.
func TestEvalReplayNotDegradedIsImplicit(t *testing.T) {
	a := Assertion{Type: "no_duplicate_commit"}
	clean := sc(a).Evaluate(nil, pipeline.Stats{}, clipTruth)
	if !clean.Passed {
		t.Fatalf("a clean run must pass: %+v", clean)
	}
	if len(clean.Results) != 2 {
		t.Fatalf("results = %+v, want the author assertion plus the implicit check", clean.Results)
	}
	st := pipeline.Stats{ReplayDegradedFrames: 2, ReplayDegradedFrameSeqs: []int64{7, 12}}
	r := sc(a).Evaluate(nil, st, clipTruth)
	if r.Passed {
		t.Fatal("a replay that lost two OCR reads must fail the report")
	}
	last := r.Results[len(r.Results)-1]
	if last.Type != "replay_not_degraded" || last.Passed {
		t.Fatalf("implicit result = %+v, want a failing replay_not_degraded", last)
	}
	for _, seq := range []int64{7, 12} {
		if !strings.Contains(last.Detail, fmt.Sprintf("%d", seq)) {
			t.Fatalf("detail %q does not name frame %d", last.Detail, seq)
		}
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
