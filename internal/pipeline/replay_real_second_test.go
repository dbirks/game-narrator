package pipeline_test

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/capture/replay"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/matcher"
	"github.com/dbirks/game-narrator/internal/pipeline"
	"github.com/dbirks/game-narrator/internal/recognition"
	"github.com/dbirks/game-narrator/internal/scenario"
	"github.com/dbirks/game-narrator/internal/vision"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

// TestSyntheticFrameReplaySecondGameRealTesseract is the G12 / ADR-0078 gate:
// the *second* synthetic game (Black Water) through the real pipeline — real
// recorded frames, real Tesseract, real matcher, real commit policy — with the
// scenario's assertions over the run's events, stats and the clip's truth.
//
// Structurally different from the first clip on every axis the first one pins:
// upper-left square-cornered box (a non-default region position the profile
// must carry, not the renderer assume), instant page reveals (no typewriter
// prefix — box geometry + stability must be enough), a two-line wrapped page,
// and a non-Latin (Cyrillic) page the eng OCR model cannot read. The
// load-bearing assertion is the negative one: bw-c must commit *zero* times —
// the pipeline abstains on an unreadable page and never fabricates a line.
//
// The "Synthetic" label is load-bearing, as in the first clip's test: every
// frame and sentence is generated (docs/spec/11 "Fixture origins"); this is
// never real-game coverage. The speech sidecar is the same real-reducer,
// scripted-TTS shadow the first test runs (the M2 pipeline owns no TTS
// scheduler), replaying this clip's truth; its truth-driven commit of page c
// is why the scenario counts two dialogue-advanced cancellations (a→b from
// the pipeline's own commit, b→c from the sidecar's).
//
// ModeReplay, the scaled budgets and the three-identical-runs determinism
// assertion carry the same meaning as in the first clip's test: the outcome
// is a function of the frames and the engine only, never of machine speed.
func TestSyntheticFrameReplaySecondGameRealTesseract(t *testing.T) {
	if testing.Short() {
		t.Skip("NOT RUN: the real-Tesseract second-game replay gate is skipped under -short; run without -short for the docs/spec/12 frame-replay evidence")
	}
	if _, err := exec.LookPath("tesseract"); err != nil {
		t.Skip("NOT RUN: tesseract not installed")
	}
	root := filepath.Join("..", "..")
	sc, err := scenario.Load(filepath.Join(root, "testdata", "scenarios", "synthetic", "black-water-001.yaml"))
	if err != nil {
		t.Fatalf("load scenario: %v", err)
	}
	profile, err := scenario.LoadProfile(filepath.Join(root, "testdata", "synthetic", "profiles", "black-water-001.json"))
	if err != nil {
		t.Fatalf("load profile: %v", err)
	}
	clipDir := filepath.Join(root, sc.Input.FramesDir)
	truth, err := scenario.LoadTruth(clipDir)
	if err != nil {
		t.Fatalf("load truth: %v", err)
	}
	idx, err := matcher.Build(profile.ToMessages(), profile.ToEdges(), matcher.IndexOptions{})
	if err != nil {
		t.Fatalf("matcher.Build: %v", err)
	}

	// The same documented runner shape as the first clip's test: lossless
	// replay queue, the tracker tuned to the clip's 10 Hz cadence, ModeReplay
	// (frame-ordered, never-superseded OCR), and the production OCR budgets
	// scaled for the test build (ocrTimeoutScale).
	cfg := pipeline.Config{
		SessionID:       "synthsession2",
		ProfileRevision: "1",
		Regions:         profile.ToRegions(),
		OCR:             ocr.Tesseract{Timeout: ocr.DefaultTimeout * ocrTimeoutScale},
		Index:           idx,
		Preprocess:      vision.DefaultSteps(),
		Trace:           true,
		Mode:            pipeline.ModeReplay,
		OCRTimeout:      ocr.DefaultTimeout * ocrTimeoutScale,
		Tracker: recognition.Config{
			VoteWindow:   3,
			StableWindow: 100 * time.Millisecond,
		},
	}

	speechEvs := runSpeechSidecar(t, idx, truth)

	type secondRunOutcomes struct {
		commits []commitStamp
		stats   pipeline.Stats
	}
	var outcomes []secondRunOutcomes
	for run := 1; run <= 3; run++ {
		runner, err := pipeline.New(cfg)
		if err != nil {
			t.Fatalf("pipeline.New (run %d): %v", run, err)
		}
		mem := events.NewMemStore()
		hub := events.New(events.WithStore(mem))
		t.Cleanup(func() { _ = hub.Close() })
		src := &replay.Source{Dir: clipDir, Speed: 0}
		t.Cleanup(func() { _ = src.Close() })

		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second*ocrTimeoutScale)
		defer cancel()
		if err := runner.Run(ctx, src, hub); err != nil {
			t.Fatalf("pipeline run %d: %v", run, err)
		}
		st := runner.Stats()
		t.Logf("run %d: frames=%d processed=%d ocr_calls=%d ocr_stale=%d rankings=%d abstained=%d commits=%d",
			run, st.FramesDelivered, st.FramesProcessed, st.OCRCalls, st.OCRStale, st.Rankings, st.RankingsAbstained, st.Commits)

		// A clean replay drops and cancels nothing; the counters are the proof
		// that the live supersede path did not leak into the replay.
		if st.OCRCancelled != 0 || st.OCRStale != 0 || st.FramesDropped != 0 {
			t.Errorf("run %d: clean replay left drops behind: cancelled=%d stale=%d frames_dropped=%d",
				run, st.OCRCancelled, st.OCRStale, st.FramesDropped)
		}

		evs, err := mem.After(context.Background(), 0, events.Filter{}, 4096)
		if err != nil {
			t.Fatalf("read events (run %d): %v", run, err)
		}
		all := append(evs, speechEvs...)

		if run == 1 {
			t.Logf("timeline (capture clock; candidate probability in parens):")
			for _, e := range runner.Trace() {
				ts := "-"
				if e.CaptureTimestampNS > 0 {
					ts = fmt.Sprintf("%d ms", e.CaptureTimestampNS/1e6)
				}
				text := e.AcceptedText
				if len(text) > 44 {
					text = text[:44] + "…"
				}
				top := "-"
				if e.TopCandidate != "" {
					top = fmt.Sprintf("%s (%.3f)", e.TopCandidate, e.TopScore)
				}
				t.Logf("%-10s %-13s %-46q %-24s %s", ts, orDash(e.State), text, top, e.Event)
			}
		}

		// The corpus is the anchor message_detected matches the truth against:
		// the profile's own lines, engine-free, so the assertion grades the
		// run's decision and promptness, never the engine's glyph accuracy —
		// the CI distro's tesseract 5.3 and the dev host's 5.5 read the
		// Black Water clip's preprocessed crop differently ("crossing" vs
		// "crossina"), and that must not move which truth line a commit
		// belongs to.
		rep := sc.EvaluateWith(all, st, truth, scenario.EvaluateOpts{Corpus: profile.CorpusFold()})
		for _, r := range rep.Results {
			status := "PASS"
			if !r.Passed {
				status = "FAIL"
			}
			t.Logf("run %d: assertion %-28s %s: %s", run, r.Type, status, r.Detail)
			if !r.Passed {
				t.Errorf("run %d: assertion %s failed: %s", run, r.Type, r.Detail)
			}
		}
		if !rep.Passed {
			t.Fatalf("scenario %s did not pass (run %d)", sc.ID, run)
		}

		outcomes = append(outcomes, secondRunOutcomes{commits: commitStamps(t, evs), stats: st})
	}

	// docs/spec/12 "determinism under identical inputs": three replays of the
	// same clip through the same engine decide the same thing at the same
	// capture times — including the abstention on the Cyrillic page, which
	// must be just as stable as the two commits.
	for run := 1; run < len(outcomes); run++ {
		first, got := outcomes[0], outcomes[run]
		if !slicesEqual(first.commits, got.commits) {
			t.Fatalf("run 1 committed %+v, run %d committed %+v: a replay must not depend on machine speed", first.commits, run+1, got.commits)
		}
		if a, b := stableStats(first.stats), stableStats(got.stats); !statsEqual(a, b) {
			t.Fatalf("run 1 stats %v != run %d stats %v: deterministic counters moved between replays", a, b, run+1)
		}
	}
	t.Logf("deterministic over %d runs: commits=%+v", len(outcomes), outcomes[0].commits)
}
