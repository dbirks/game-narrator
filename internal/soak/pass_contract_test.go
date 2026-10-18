// pass_contract_test.go pins the two harness defects behind the
// 20261017-222119-chaos failure (ADR-0045): the pass-3 slice shape — one
// commit whose decision lost the race to the advance (no voice events) plus
// the last line's utterance cancelled by the clip's own box dismissal — must
// pass the loop-aware voice contract, while a genuinely stuck utterance and a
// foreign voice must still fail it; and the sidecar must pace every pass at
// the clip's own span, not a growing dead sleep.

package soak

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/pipeline"
	"github.com/dbirks/game-narrator/internal/scenario"
)

const contractVoice = "synthetic-narrator"

func contractEvent(name string, fields map[string]any) events.Envelope {
	data, err := json.Marshal(fields)
	if err != nil {
		panic(fmt.Sprintf("soak test fixture: marshal %s: %v", name, err))
	}
	return events.Envelope{Name: name, Data: data}
}

func commitEvent(msgID, occ string, gen int64) events.Envelope {
	return contractEvent(events.MessageCommitted, map[string]any{
		"message_id":             msgID,
		"dialogue_occurrence_id": occ,
		"generation":             gen,
		"region_id":              "textbox",
		"transitions": []map[string]any{
			{"to": "committed", "capture_timestamp_ns": gen * 1000000000},
		},
	})
}

func speechEvent(name, occ string, extra map[string]any) events.Envelope {
	f := map[string]any{
		"dialogue_occurrence_id": occ,
		"message_id":             "demo-page-2",
		"region_id":              "textbox",
	}
	for k, v := range extra {
		f[k] = v
	}
	return contractEvent(name, f)
}

// passThreeShape is the exact event shape of the failing slice from the
// 20261017-222119-chaos report (clip 0 pass 3): page-1 committed with no
// voice events (its decision lost the race to the advance — an abstain), and
// page-2's utterance started and was cancelled by the box dismissal at the
// clip's end before its tone end fired.
func passThreeShape() []events.Envelope {
	return []events.Envelope{
		commitEvent("demo-page-1", "occ-a", 7),
		commitEvent("demo-page-2", "occ-b", 8),
		speechEvent(events.SpeechReady, "occ-b", map[string]any{"voice_id": contractVoice, "result": "ready"}),
		speechEvent(events.SpeechStarted, "occ-b", map[string]any{"voice_id": contractVoice, "result": "started"}),
		speechEvent(events.SpeechCancelled, "occ-b", map[string]any{"reason": "region_vanished", "speech_phase": "started"}),
	}
}

func TestVoiceContractOffenders(t *testing.T) {
	t.Run("pass-three shape is clean", func(t *testing.T) {
		if off := voiceContractOffenders(passThreeShape(), contractVoice); len(off) > 0 {
			t.Fatalf("healthy pass-3 shape flagged: %v", off)
		}
	})
	t.Run("cancelled utterance is a terminal outcome", func(t *testing.T) {
		evs := passThreeShape()
		// The old scenario evaluator is the check that failed the run: on this
		// same slice it owes a good speech.ended that the pass does not have.
		sc := &scenario.Scenario{Assertions: []scenario.Assertion{{Type: "no_voice_substitution", Voice: contractVoice}}}
		rep := sc.Evaluate(evs, pipeline.Stats{}, nil)
		if rep.Passed {
			t.Fatalf("expected the old no_voice_substitution evaluator to fail the pass-3 shape (that was the bug); it passed: %v", rep.Results)
		}
	})
	t.Run("healthy ended pass is clean", func(t *testing.T) {
		evs := passThreeShape()
		evs = append(evs, speechEvent(events.SpeechEnded, "occ-b", map[string]any{"voice_id": contractVoice, "result": "ended"}))
		if off := voiceContractOffenders(evs, contractVoice); len(off) > 0 {
			t.Fatalf("healthy ended pass flagged: %v", off)
		}
	})
	t.Run("tts failure with a reason is a terminal outcome", func(t *testing.T) {
		evs := []events.Envelope{
			commitEvent("demo-page-2", "occ-c", 9),
			speechEvent(events.SpeechReady, "occ-c", map[string]any{"voice_id": contractVoice, "result": "ready"}),
			speechEvent(events.SpeechStarted, "occ-c", map[string]any{"voice_id": contractVoice, "result": "started"}),
			speechEvent(events.SpeechEnded, "occ-c", map[string]any{"voice_id": contractVoice, "result": "failed", "error": "unavailable: " + contractVoice}),
		}
		if off := voiceContractOffenders(evs, contractVoice); len(off) > 0 {
			t.Fatalf("silence-with-a-reason flagged: %v", off)
		}
	})
	t.Run("stuck utterance still fails", func(t *testing.T) {
		evs := []events.Envelope{
			commitEvent("demo-page-2", "occ-d", 10),
			speechEvent(events.SpeechReady, "occ-d", map[string]any{"voice_id": contractVoice, "result": "ready"}),
			speechEvent(events.SpeechStarted, "occ-d", map[string]any{"voice_id": contractVoice, "result": "started"}),
		}
		off := voiceContractOffenders(evs, contractVoice)
		if len(off) != 1 {
			t.Fatalf("want exactly 1 offender for a stuck utterance, got %v", off)
		}
	})
	t.Run("foreign voice still fails", func(t *testing.T) {
		evs := []events.Envelope{
			commitEvent("demo-page-2", "occ-e", 11),
			speechEvent(events.SpeechReady, "occ-e", map[string]any{"voice_id": contractVoice, "result": "ready"}),
			contractEvent(events.SpeechStarted, map[string]any{
				"dialogue_occurrence_id": "occ-e",
				"message_id":             "demo-page-2",
				"voice_id":               "builtin-narrator",
				"result":                 "started",
			}),
			contractEvent(events.SpeechEnded, map[string]any{
				"dialogue_occurrence_id": "occ-e",
				"voice_id":               "builtin-narrator",
				"result":                 "ended",
			}),
		}
		off := voiceContractOffenders(evs, contractVoice)
		if len(off) != 2 {
			t.Fatalf("want one offender per foreign-voice event (2), got %v", off)
		}
	})
}

// TestSidecarPassPacingIsFlat pins the pacing fix: every pass paces the clip's
// own span (~2.3 s of truth rows) regardless of the pass number. With the old
// formula the pass p wall time was clip span + p*stride, so only 2 passes
// completed in 12 s; the fixed sidecar completes at least 3 (each with both
// commits, 6 sidecar commits minimum).
func TestSidecarPassPacingIsFlat(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	cfg := Config{
		Duration:    12 * time.Second,
		SampleEvery: 5 * time.Second,
		RepoRoot:    root,
		Clips:       []string{"testdata/synthetic/replay/dialogue-advance-001"},
		Profile:     "dialogue-advance-001",
		Chaos:       ChaosConfig{Enabled: true, Seed: 7},
	}
	if err := cfg.fillDefaults(); err != nil {
		t.Fatal(err)
	}
	h, err := newHarness(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.build(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.start(ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = h.sidecar(ctx)
	}()

	time.Sleep(12 * time.Second)
	cancel()
	_ = waitTimeout(waitGroupDone(&h.wg), 2*providerTimeout)
	h.finishUtterances(context.Background())

	// The settle path grades (and drops) a pass slice when it settles, but the
	// trailing passes are still inside their settle window at 12 s; the run-end
	// drain (gradeRemaining, what collect runs in a real run) grades the
	// residue. Run it here so the graded count is the sidecar's full output.
	_, _ = h.gradeRemaining()
	committedSlices := h.passCommitSlices
	commits := h.sidecarCommits.Load()
	h.close()

	if committedSlices < 3 {
		t.Fatalf("sidecar completed %d pass slice(s) with commits in 12 s (want >= 3: ~2.3 s per pass; the pre-fix pacing completes only 2)",
			committedSlices)
	}
	if commits < 6 {
		t.Fatalf("sidecar committed %d occurrence(s) in 12 s (want >= 6: 2 lines per pass x 3 passes)", commits)
	}
}
