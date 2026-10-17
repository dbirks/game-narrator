package pipeline_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/capture/replay"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/matcher"
	"github.com/dbirks/game-narrator/internal/pipeline"
	"github.com/dbirks/game-narrator/internal/recognition"
	"github.com/dbirks/game-narrator/internal/scenario"
	"github.com/dbirks/game-narrator/internal/session"
	"github.com/dbirks/game-narrator/internal/vision"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

// TestSyntheticFrameReplayRealTesseract is docs/spec/12's "Frame replay" gate
// on the synthetic clip: real recorded frames, real Tesseract, real matcher,
// real pipeline — no scripted OCR answers — with the scenario's assertions
// evaluated over the run's events, stats and the clip's truth.
//
// The "Synthetic" label is load-bearing: every frame and sentence is generated
// (docs/spec/11 "Fixture origins"), and this test must never be read as
// real-game coverage. The one piece that is not a live component is named as
// such below: the M2 pipeline owns no TTS scheduler, so the speech assertion is
// checked against a session-level sidecar (real reducer, real matcher,
// scripted TTS feedback), not against a voice endpoint.
func TestSyntheticFrameReplayRealTesseract(t *testing.T) {
	if _, err := exec.LookPath("tesseract"); err != nil {
		t.Skip("NOT RUN: tesseract not installed")
	}
	root := filepath.Join("..", "..")
	sc, err := scenario.Load(filepath.Join(root, "testdata", "scenarios", "synthetic", "dialogue-advance-001.yaml"))
	if err != nil {
		t.Fatalf("load scenario: %v", err)
	}
	profile, err := scenario.LoadProfile(filepath.Join(root, "testdata", "synthetic", "profiles", "dialogue-advance-001.json"))
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

	// The same runner shape the fake-OCR harness proved: lossless replay queue
	// (a fixture must be the recording it claims to be) and a tracker tuned to
	// the clip's 10 Hz cadence — documented settings, not fixture branches.
	cfg := pipeline.Config{
		SessionID:       "synthsession",
		ProfileRevision: "1",
		Regions:         profile.ToRegions(),
		OCR:             ocr.Tesseract{},
		Index:           idx,
		Preprocess:      vision.DefaultSteps(),
		Trace:           true,
		Queue:           pipeline.QueueLossless,
		OCRTimeout:      ocr.DefaultTimeout,
		Tracker: recognition.Config{
			VoteWindow:   3,
			StableWindow: 100 * time.Millisecond,
		},
	}
	runner, err := pipeline.New(cfg)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}

	mem := events.NewMemStore()
	hub := events.New(events.WithStore(mem))
	t.Cleanup(func() { _ = hub.Close() })
	src := &replay.Source{Dir: clipDir, Speed: 0}
	t.Cleanup(func() { _ = src.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	if err := runner.Run(ctx, src, hub); err != nil {
		t.Fatalf("pipeline run: %v", err)
	}
	st := runner.Stats()
	t.Logf("stats: frames=%d processed=%d ocr_calls=%d ocr_skipped=%d ocr_cancelled=%d ocr_stale=%d rankings=%d commits=%d stale_applied=%d",
		st.FramesDelivered, st.FramesProcessed, st.OCRCalls, st.OCRSkippedByHash, st.OCRCancelled, st.OCRStale, st.Rankings, st.Commits, st.StaleApplied)

	speechEvs := runSpeechSidecar(t, idx, truth)

	evs, err := mem.After(context.Background(), 0, events.Filter{}, 4096)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	all := append(evs, speechEvs...)

	// The evidence-bundle timeline: frame capture ts -> OCR text -> top
	// candidate with probability -> events, straight from the runner's trace.
	t.Logf("timeline (capture clock; candidate probability in parens):")
	t.Logf("%-12s %-13s %-40s %-28s %s", "frame_ts", "state", "ocr_text", "top_candidate", "events")
	for _, e := range runner.Trace() {
		ts := "-"
		if e.CaptureTimestampNS > 0 {
			ts = fmt.Sprintf("%d ms", e.CaptureTimestampNS/1e6)
		}
		text := e.AcceptedText
		if len(text) > 38 {
			text = text[:38] + "…"
		}
		top := "-"
		if e.TopCandidate != "" {
			top = fmt.Sprintf("%s (%.3f)", e.TopCandidate, e.TopScore)
		}
		t.Logf("%-12s %-13s %-40q %-28s %s", ts, orDash(e.State), text, top, e.Event)
	}

	rep := sc.Evaluate(all, st, truth)
	for _, r := range rep.Results {
		status := "PASS"
		if !r.Passed {
			status = "FAIL"
		}
		t.Logf("assertion %-28s %s: %s", r.Type, status, r.Detail)
		if !r.Passed {
			t.Errorf("assertion %s failed: %s", r.Type, r.Detail)
		}
	}
	if !rep.Passed {
		t.Fatalf("scenario %s did not pass", sc.ID)
	}
}

// orDash renders the empty state (ranking trace rows carry none) as a dash.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// --- speech sidecar -----------------------------------------------------------

// recPub is the test-local session.Publisher: it numbers, stamps and validates
// envelopes the way events.Hub does and keeps them for the assertion.
type recPub struct {
	mu  sync.Mutex
	seq int64
	out []events.Envelope
}

func (p *recPub) Publish(_ context.Context, e events.Envelope) (events.Envelope, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq++
	if e.EventID == "" {
		e.EventID = games.NewID()
	}
	e.Seq = p.seq
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now().UTC()
	}
	if e.SchemaVersion == 0 {
		e.SchemaVersion = events.SchemaVersion
	}
	if err := e.Validate(); err != nil {
		return e, err
	}
	p.out = append(p.out, e)
	return e, nil
}

// runSpeechSidecar replays the clip's ground truth through the session reducer
// with real matcher rankings and a scripted "TTS ready" answer for each
// committed page — docs/spec/07's speech scheduler in miniature. It exists
// because the M2 pipeline owns no TTS worker, so the pipeline's reducer can
// never move an utterance past PhaseNone and can never emit speech.cancelled
// on its own. What is real here: the session state machine, the matcher's
// ranking of the clip's lines, and the clip's timing. What is scripted: the
// TTS worker's readiness. The returned speech.* events are the evidence the
// scenario's speech assertion is evaluated over.
func runSpeechSidecar(t *testing.T, idx *matcher.Index, truth []scenario.Truth) []events.Envelope {
	t.Helper()
	pub := &recPub{}
	r, err := session.New("synthsession", "1", pub)
	if err != nil {
		t.Fatalf("session.New (sidecar): %v", err)
	}
	ctx := context.Background()
	const (
		region = "bottom"
		sessID = "synthsession"
		rev    = "1"
	)
	var seq int64
	next := func() int64 { seq++; return seq }
	var lastKey string

	for i, row := range truth {
		conf := 0.0
		if row.Text != "" {
			if row.Full {
				conf = 0.91 // measured Tesseract word confidences on this clip
			} else {
				conf = 0.85
			}
		}
		res, err := r.Apply(ctx, session.ObservationMsg{
			Header:   session.Header{SessionID: sessID, Seq: next(), Generation: r.Generation(), ProfileRevision: rev},
			RegionID: region,
			Observation: recognition.Observation{
				FrameSeq:           int64(i + 1),
				CaptureTimestampNS: row.TsNS,
				SourceClock:        "replay",
				RegionID:           region,
				RegionPresent:      row.Present,
				RegionHash:         fmt.Sprintf("truth-%03d", i+1),
				OCR:                truthOCR(row.Text, conf),
			},
		})
		if err != nil {
			t.Fatalf("sidecar obs %d: %v", i+1, err)
		}
		stt := r.State()
		if len(stt.Regions) == 0 || stt.Regions[0].TextState != recognition.StateReady || stt.Regions[0].Text == "" {
			continue
		}
		rs := stt.Regions[0]
		if rs.NormalizedKey == lastKey {
			continue // this exact reading was already answered
		}
		lastKey = rs.NormalizedKey
		rank := idx.Rank(matcher.Observation{Text: rs.RawText, OCRConf: rs.Confidence, TextKindHint: "dialogue"}, matcher.RankOptions{})
		res, err = r.Apply(ctx, session.RankingMsg{
			Header:         session.Header{SessionID: sessID, Seq: next(), Generation: r.Generation(), ProfileRevision: rev},
			RegionID:       region,
			ObservationSeq: rs.ObservationSeq,
			Text:           rs.Text,
			Ranking:        rank,
		})
		if err != nil {
			t.Fatalf("sidecar rank %d: %v", i+1, err)
		}
		for _, e := range res.Events {
			if e.Name != events.MessageCommitted {
				continue
			}
			var cp struct {
				DialogueOccurrenceID string `json:"dialogue_occurrence_id"`
				Generation           int64  `json:"generation"`
			}
			if err := json.Unmarshal(e.Data, &cp); err != nil || cp.DialogueOccurrenceID == "" {
				continue
			}
			// The TTS worker answers ready for the committed page, the way the
			// speech scheduler does: by the time the page turns, page one's
			// utterance is live and page two's is requested on its commit.
			if _, err := r.Apply(ctx, session.SpeechMsg{
				Header:       session.Header{SessionID: sessID, Seq: next(), Generation: cp.Generation, ProfileRevision: rev},
				RegionID:     region,
				OccurrenceID: cp.DialogueOccurrenceID,
				Kind:         session.SpeechReady,
				VoiceID:      "synthetic-narrator",
			}); err != nil {
				t.Fatalf("sidecar speech ready %d: %v", i+1, err)
			}
		}
	}

	// Keep only the speech lifecycle events: the sidecar's own commits and
	// observations are the unit-mode evidence for this function, and merging
	// them into the pipeline's log would double-count the real run's commits.
	var out []events.Envelope
	for _, e := range pub.out {
		if e.Name == events.SpeechReady || e.Name == events.SpeechStarted || e.Name == events.SpeechCancelled || e.Name == events.SpeechEnded {
			out = append(out, e)
		}
	}
	return out
}

// truthOCR is one scripted OCR answer shaped the way the real engine's result
// arrives: text and mean confidence; empty reads carry no lines.
func truthOCR(text string, conf float64) *ocr.Result {
	res := ocr.Result{Text: text, Engine: "truth", MeanConf: conf}
	if text == "" {
		return &res
	}
	res.Lines = []ocr.Line{{Text: text, Conf: conf}}
	return &res
}
