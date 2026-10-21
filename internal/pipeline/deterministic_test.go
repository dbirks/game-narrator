package pipeline_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/pipeline"
)

// slowDecider is a pipeline.Decider whose answer is deliberately slow: it
// answers only after d (or the run's context dies), with an honest abstain.
// It stands in for the one component of the rank reply path the test cannot
// slow any other way — the local matcher's Rank is a few milliseconds of pure
// CPU — and it is the component docs/spec/05 gives its own bounded deadline.
// An abstain (nil Msg) keeps the commit on the local evidence, so the test
// asserts the ordering of the evidence, not the content of an answer.
type slowDecider struct {
	d     time.Duration
	calls atomic.Int64
}

func (d *slowDecider) Decide(ctx context.Context, req pipeline.DecisionRequest) pipeline.DecisionReply {
	d.calls.Add(1)
	t := time.NewTimer(d.d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
	return pipeline.DecisionReply{Reason: "skipped"}
}

// The corpus's stable lines rank confidently (no abstention), so the margin
// is what routes every rank reply through the slow Decider:
// AdjudicateMargin above any realistic margin makes NeedsAdjudication true
// for every ranking, which is the component docs/spec/05 gives its own
// bounded deadline. The Decider's abstain (nil Msg) keeps the commit on the
// local evidence, so the test asserts the ordering of the evidence, not the
// content of an answer.
func deterministicMut(dec pipeline.Decider, det bool) func(*pipeline.Config) {
	return func(c *pipeline.Config) {
		c.Mode = pipeline.ModeReplay
		c.Deterministic = det
		c.Decider = dec
		c.Reducer.Adjudicate = true
		c.Reducer.AdjudicateMargin = 1e6
		// The wall-clock belts, sized small so they actually fire against the
		// slow reply: the retrieval-hold budget is OCRTimeout (60 ms, far under
		// the Decider's 300 ms), and the per-call deadline stays generous so
		// the engine (instant fake) never outruns it — what the test changes
		// is the reply's latency, never the read's. Both wall constants scale
		// by the package's race factor (issue #22): the ratio the test proves
		// with — the reply 5x slower than the hold budget — is the load
		// sensitivity, so it must survive the race build unchanged. Scaled,
		// the plain arm's tail reply (300 ms × scale) also outlives the
		// end-of-stream grace (OCRTimeout + 500 ms) under -race, so the
		// counterfactual's state is the same load-invariant run-to-run there
		// instead of a scheduling coin flip: the commit delta, not the fence
		// count, is what carries the dependence.
		c.OCRTimeout = 60 * time.Millisecond * ocrTimeoutScale
		c.ReplayOCRTimeout = 5 * time.Second * ocrTimeoutScale
	}
}

// TestDeterministicReplaySlowRankReplyDoesNotFence is the unit proof for
// issue #17 / ADR-0064 amendment 2: in the bench's deterministic replay mode
// a rank reply slower than the retrieval-hold budget is applied before the
// region's next observation instead of being fenced stale, so the outcome —
// the committed messages and their capture timestamps — is the fast run's
// outcome no matter how slow the reply was. The same run in plain ModeReplay
// fences the late reply (the hold budget expires against the Decider's
// 300 ms x the race factor), which is the wall-clock dependence the
// deterministic mode removes. Both wall constants of the test scale by the
// package's race factor (issue #22), so the 5x ratio the dependence is
// measured in holds in the race build too.
func TestDeterministicReplaySlowRankReplyDoesNotFence(t *testing.T) {
	script := ocrScript(t)

	// The reference: plain replay, fast engine, no Decider — the clip's own
	// outcome. Two commits (the typewriter's line, its page turn), stamped
	// with the recording's capture clock.
	ref := newHarness(t, &fakeOCR{script: script, delay: fastScript, respectCtx: true},
		func(c *pipeline.Config) { c.Mode = pipeline.ModeReplay })
	_, evsRef, err := ref.run(t, ref.source(0, false))
	if err != nil {
		t.Fatalf("reference run: %v", err)
	}
	want := commitStamps(t, evsRef)
	if len(want) != 2 {
		t.Fatalf("reference run committed %v, want two commits (the clip's two lines)", want)
	}

	for _, mode := range []struct {
		name string
		det  bool
	}{{
		name: "deterministic",
		det:  true,
	}, {
		name: "plain-replay",
		det:  false,
	}} {
		t.Run(mode.name, func(t *testing.T) {
			dec := &slowDecider{d: 300 * time.Millisecond * ocrTimeoutScale}
			h := newHarness(t, &fakeOCR{script: script, delay: fastScript, respectCtx: true},
				deterministicMut(dec, mode.det))
			st, evs, err := h.run(t, h.source(0, false))
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			// The slow reply was really on the critical path: without a Decider
			// call this test is asserting nothing about slow replies.
			if dec.calls.Load() == 0 {
				t.Fatalf("decider was never called (margin below the adjudication threshold?): the rank reply must be slow for this test to mean anything")
			}

			got := commitStamps(t, evs)
			if mode.det {
				if !slicesEqual(got, want) {
					t.Errorf("committed %v, want the reference run's %v: the outcome must not depend on the rank reply's wall latency", got, want)
				}
				if st.RankingStale != 0 || st.StaleApplied != 0 {
					t.Errorf("deterministic replay fenced a late reply: ranking_stale=%d stale_applied=%d — the reply must land before the region's next observation, not race it", st.RankingStale, st.StaleApplied)
				}
			} else {
				// The counterfactual: plain ModeReplay's outcome must depend on
				// the reply's wall latency — the late reply is fenced (the hold
				// budget expired) and/or the commit lands late or is lost. This
				// is the dependence the deterministic mode removes, measured.
				depended := st.RankingStale > 0 || !slicesEqual(got, want)
				t.Logf("plain replay with the %s reply: committed %v (reference %v), ranking_stale=%d", 300*time.Millisecond*ocrTimeoutScale, got, want, st.RankingStale)
				if !depended {
					t.Errorf("plain replay's outcome was unaffected by a reply 5x slower than its hold budget (committed %v, ranking_stale=0): the counterfactual is missing and this test proves nothing", got)
				}
			}

			// The mode changes the ordering, not the recording: a clean run
			// drops and cancels nothing in either arm.
			if st.OCRCancelled != 0 || st.OCRStale != 0 || st.FramesDropped != 0 {
				t.Errorf("run left drops behind: cancelled=%d stale=%d frames_dropped=%d", st.OCRCancelled, st.OCRStale, st.FramesDropped)
			}
		})
	}
}

// TestDeterministicIsReplayOnly is the config contract: the deterministic
// semantic is the bench's load-invariant replay, not a live-mode option the
// appliance can opt into (live mode's wall-clock belts are its freshness
// guarantee, docs/spec/01).
func TestDeterministicIsReplayOnly(t *testing.T) {
	cfg := pipeline.Config{
		SessionID:       "detsession",
		ProfileRevision: "1",
		Regions:         []games.Region{corpusRegion(t)},
		OCR:             &fakeOCR{script: ocrScript(t), delay: fastScript, respectCtx: true},
		Index:           index(t),
		Preprocess:      steps(),
		Mode:            pipeline.ModeLive,
		Deterministic:   true,
	}
	if _, err := pipeline.New(cfg); err == nil {
		t.Fatal("pipeline.New accepted Deterministic with ModeLive: the deterministic semantic is ModeReplay-only")
	}
	cfg.Mode = pipeline.ModeReplay
	if _, err := pipeline.New(cfg); err != nil {
		t.Fatalf("pipeline.New refused Deterministic with ModeReplay: %v", err)
	}
}
