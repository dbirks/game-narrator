package pipeline_test

import (
	"context"
	"image"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/dbirks/game-narrator/internal/matcher"
	"github.com/dbirks/game-narrator/internal/pipeline"
	"github.com/dbirks/game-narrator/internal/scenario"
	"github.com/dbirks/game-narrator/internal/vision"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

// The two tests here are docs/spec/12's "Frame replay" failure injection from
// the OCR side: a replay that silently loses a frame's read is no longer the
// recording it claims to be, and the replay retry policy is what keeps a slow
// host from being read that way. The clip is the synthetic 24-frame corpus and
// the profile and scenario are the same ones the real-tesseract gate loads, so
// a run that survives a retry here must also pass the real scenario, and a run
// that degrades must fail it loudly.

// fullLineHash returns the preprocessed-crop hash of the first truth row that
// shows line in full — the identity the flaky engine is keyed on, computed with
// the same recipe as ocrScript so a recipe drift fails loudly here rather than
// silently never triggering the fault.
func fullLineHash(t *testing.T, line string) string {
	t.Helper()
	dir := corpusDir(t)
	for _, r := range readTruth(t) {
		if !r.Full || r.Text != line {
			continue
		}
		img := decodeFrame(t, dir, r.File)
		crop := vision.CropNormalized(img, vision.Rect{X: r.Rect.X, Y: r.Rect.Y, W: r.Rect.W, H: r.Rect.H}, vision.CropOptions{})
		if crop == nil || crop.Bounds().Empty() {
			t.Fatalf("%s: rect %v crops nothing", r.File, r.Rect)
		}
		return vision.ImageHash(vision.Preprocess(crop, steps()))
	}
	t.Fatalf("truth.json has no full-line row for %q", line)
	return ""
}

// flakyOCR is one scripted host-load spike in front of an otherwise truthful
// engine: the read of one specific crop (page one's full-line frames) fails as
// a timeout, and the rest of the clip is answered by the wrapped engine. The
// fault is scripted per call for that crop only:
//
//   - one scripted failure: exactly the first call for the crop fails; the
//     retry that follows inside the same job succeeds.
//   - ReplayOCRMaxAttempts scripted failures: the first job's whole attempt
//     budget fails, and the next frame's read of the same pixels succeeds.
//     That is the "frame N always fails" shape: frame N's read is lost even
//     after the retries, and the replay degrades on exactly that frame.
//
// The failure is a timeout, the class docs/spec/12 names (a Tesseract call
// that outlives its deadline under host load), not a cancellation: a
// cancellation this pipeline issued is a policy decision, not a fault the
// retries exist for.
type flakyOCR struct {
	inner    ocr.Engine
	failHash string

	mu           sync.Mutex
	failuresLeft int
}

func (f *flakyOCR) Name() string { return "flaky" }

func (f *flakyOCR) Recognize(ctx context.Context, img image.Image) (ocr.Result, error) {
	key := vision.ImageHash(img)
	if key == f.failHash {
		f.mu.Lock()
		fail := f.failuresLeft > 0
		if fail {
			f.failuresLeft--
		}
		f.mu.Unlock()
		if fail {
			return ocr.Result{}, &ocr.Failure{Kind: ocr.ErrTimeout, Engine: "flaky", Err: context.DeadlineExceeded}
		}
	}
	return f.inner.Recognize(ctx, img)
}

// profileIndex loads the synthetic profile the clip was generated against, so
// the commits carry the message ids the scenario YAML asserts on (demo-page-1/2)
// rather than the harness's own ids.
func profileIndex(t *testing.T) *matcher.Index {
	t.Helper()
	root := filepath.Join("..", "..")
	profile, err := scenario.LoadProfile(filepath.Join(root, "testdata", "synthetic", "profiles", "dialogue-advance-001.json"))
	if err != nil {
		t.Fatalf("load profile: %v", err)
	}
	idx, err := matcher.Build(profile.ToMessages(), profile.ToEdges(), matcher.IndexOptions{})
	if err != nil {
		t.Fatalf("matcher.Build: %v", err)
	}
	return idx
}

// newReplayHarness wires the harness in ModeReplay against the profile's index
// and regions — the shape the scenario gate runs.
func newReplayHarness(t *testing.T, eng ocr.Engine) *harness {
	t.Helper()
	root := filepath.Join("..", "..")
	profile, err := scenario.LoadProfile(filepath.Join(root, "testdata", "synthetic", "profiles", "dialogue-advance-001.json"))
	if err != nil {
		t.Fatalf("load profile: %v", err)
	}
	idx, err := matcher.Build(profile.ToMessages(), profile.ToEdges(), matcher.IndexOptions{})
	if err != nil {
		t.Fatalf("matcher.Build: %v", err)
	}
	return newHarness(t, eng, func(c *pipeline.Config) {
		c.Mode = pipeline.ModeReplay
		c.Index = idx
		c.Regions = profile.ToRegions()
	})
}

// clipScenario loads the clip's real scenario and truth, the same inputs the
// real-tesseract gate evaluates.
func clipScenario(t *testing.T) (*scenario.Scenario, []scenario.Truth) {
	t.Helper()
	root := filepath.Join("..", "..")
	sc, err := scenario.Load(filepath.Join(root, "testdata", "scenarios", "synthetic", "dialogue-advance-001.yaml"))
	if err != nil {
		t.Fatalf("load scenario: %v", err)
	}
	truth, err := scenario.LoadTruth(filepath.Join(root, sc.Input.FramesDir))
	if err != nil {
		t.Fatalf("load truth: %v", err)
	}
	return sc, truth
}

// findResult locates one assertion's verdict in a report.
func findResult(t *testing.T, rep scenario.Report, typ string) (scenario.Result, bool) {
	t.Helper()
	for _, r := range rep.Results {
		if r.Type == typ {
			return r, true
		}
	}
	return scenario.Result{}, false
}

// TestReplayOCRTimeoutRetriesAndCommitsUnchanged is the recovered half of the
// failure injection: one frame's OCR call times out on its first attempt, the
// replay retry policy answers it on the second, and the run must be
// indistinguishable from a clean one in its commitments — while the engine's
// fault stays visible in the counters' absence of a degradation.
func TestReplayOCRTimeoutRetriesAndCommitsUnchanged(t *testing.T) {
	inner := &fakeOCR{script: ocrScript(t), delay: fastScript, respectCtx: true}
	flaky := &flakyOCR{inner: inner, failHash: fullLineHash(t, lineA), failuresLeft: 1}
	h := newReplayHarness(t, flaky)
	st, evs, err := h.run(t, h.source(0, false))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := committed(t, evs); len(got) != 2 || got[0] != "demo-page-1" || got[1] != "demo-page-2" {
		t.Fatalf("committed = %v, want [demo-page-1 demo-page-2]: a recovered retry must not change the commits", got)
	}
	if st.ReplayDegradedFrames != 0 {
		t.Fatalf("replay_degraded_frames = %d (frames %v), want 0: the retry recovered the read", st.ReplayDegradedFrames, st.ReplayDegradedFrameSeqs)
	}
	if st.OCRErrors != 0 {
		t.Fatalf("ocr_errors = %d, want 0: the faulted call was retried, not lost", st.OCRErrors)
	}
	// The fault and its recovery are visible: the flaky crop was handed twice
	// (one failure, one success) and the wrapped engine answered the clip once
	// per distinct crop, as in the clean run.
	if inner.misses.Load() != 0 {
		t.Fatalf("fake OCR was handed %d crops that are not in the script", inner.misses.Load())
	}

	sc, truth := clipScenario(t)
	speechEvs := runSpeechSidecar(t, profileIndex(t), truth, sc.CatchUp)
	rep := sc.Evaluate(append(evs, speechEvs...), st, truth)
	for _, r := range rep.Results {
		if !r.Passed {
			t.Errorf("assertion %s failed: %s", r.Type, r.Detail)
		}
	}
	if !rep.Passed {
		t.Fatalf("scenario %s did not pass after a recovered OCR retry: %+v", sc.ID, rep.Results)
	}
	if r, ok := findResult(t, rep, "replay_not_degraded"); !ok || !r.Passed {
		t.Fatalf("implicit replay_not_degraded = %+v, want present and passing on a recovered run", r)
	}
}

// TestReplayOCRAlwaysFailingFrameIsNamedAndFailsTheReport is the unrecovered
// half: frame N's read fails on every attempt the retry policy allows, so the
// replay degrades on exactly that frame. The degradation must be named in
// Stats, and the scenario report must fail on the implicit assertion with the
// frame named — even though the rest of the clip recovers and commits.
func TestReplayOCRAlwaysFailingFrameIsNamedAndFailsTheReport(t *testing.T) {
	inner := &fakeOCR{script: ocrScript(t), delay: fastScript, respectCtx: true}
	// One job's whole attempt budget (initial + two retries) fails for the
	// crop; the next frame's read of the same pixels succeeds.
	flaky := &flakyOCR{inner: inner, failHash: fullLineHash(t, lineA), failuresLeft: pipeline.ReplayOCRMaxAttempts}
	h := newReplayHarness(t, flaky)
	st, evs, err := h.run(t, h.source(0, false))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if st.ReplayDegradedFrames != 1 {
		t.Fatalf("replay_degraded_frames = %d (frames %v), want 1: only the faulted frame's read was lost",
			st.ReplayDegradedFrames, st.ReplayDegradedFrameSeqs)
	}
	if len(st.ReplayDegradedFrameSeqs) != 1 {
		t.Fatalf("replay_degraded_frame_seqs = %v, want one named frame", st.ReplayDegradedFrameSeqs)
	}
	lost := st.ReplayDegradedFrameSeqs[0]
	if st.OCRErrors != 1 {
		t.Fatalf("ocr_errors = %d, want 1 (the unrecovered read of frame %d)", st.OCRErrors, lost)
	}
	// The clip still commits both pages — the next frame re-reads the same
	// pixels — which is exactly why the degradation must come from the stats
	// rather than from a missing commit: the commits alone would not say the
	// replay was not the recording it claims to be.
	if got := committed(t, evs); len(got) != 2 || got[0] != "demo-page-1" || got[1] != "demo-page-2" {
		t.Fatalf("committed = %v, want [demo-page-1 demo-page-2]: the following frame re-reads the same pixels", got)
	}

	sc, truth := clipScenario(t)
	speechEvs := runSpeechSidecar(t, profileIndex(t), truth, sc.CatchUp)
	rep := sc.Evaluate(append(evs, speechEvs...), st, truth)
	if rep.Passed {
		t.Fatalf("a replay that lost frame %d's OCR read must fail the report: %+v", lost, rep.Results)
	}
	bad, ok := findResult(t, rep, "replay_not_degraded")
	if !ok || bad.Passed {
		t.Fatalf("implicit replay_not_degraded = present=%v passed=%v, want present and failing", ok, bad.Passed)
	}
	if !strings.Contains(bad.Detail, strconv.Itoa(int(lost))) {
		t.Fatalf("failing detail %q does not name the degraded frame %d", bad.Detail, lost)
	}
	// Every authored assertion still passes: the failure is precisely the
	// implicit integrity check, not a second symptom of the same fault.
	for _, r := range rep.Results {
		if r.Type == "replay_not_degraded" {
			continue
		}
		if !r.Passed {
			t.Errorf("assertion %s failed unexpectedly: %s", r.Type, r.Detail)
		}
	}
}
