package bench

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/dbirks/game-narrator/internal/matcher"
)

// Calibration is the output of `bench calibrate` (calibration.json): the
// temperature T fitted on the tuning split, with enough context to re-run the
// fit and to attribute a later report to the settings that produced it
// (ADR-0035 "Calibration": the revision is carried wherever the matcher's
// settings feed a cache key or fixture identity).
type Calibration struct {
	SchemaVersion int     `json:"schema_version"`
	Set           string  `json:"set"`
	Split         string  `json:"split"` // always "tuning": the fit never touches the holdout
	Temperature   float64 `json:"temperature"`
	// CalibrationRevision is the revision this fit would define if adopted as
	// the matcher's default (the current default is matcher.CalibrationRevision,
	// so the next one is that + 1).
	CalibrationRevision int    `json:"calibration_revision"`
	Method              string `json:"method"`
	SamplesUsed         int    `json:"samples_used"`
	// SamplesSkipped counts tuning samples the fit cannot use: no reading
	// (empty box) or a retrieval miss (the truth message is not in the returned
	// candidate set). Skips are counted, never silently dropped.
	SamplesSkipped int     `json:"samples_skipped"`
	NLLAtT1        float64 `json:"nll_at_t1"`
	NLLAtFit       float64 `json:"nll_at_fit"`
	SetHash        string  `json:"set_hash"`
	GeneratedAt    string  `json:"generated_at"`
}

// Calibrate fits the matcher's temperature on the set's tuning split
// (docs/spec/12 "Split into tuning vs holdout"; ADR-0035 "Calibration"):
//
//  1. render the tuning samples' frames (same generator as the run);
//  2. replay each through the real pipeline (real Tesseract, real matcher,
//     real tracker) exactly as runSample does, so the fit sees the pipeline's
//     own stable readings, not an approximation of them;
//  3. re-rank each sample's committed stable reading against the same index.
//     The re-rank is faithful to the pipeline's rankWorker observation — same
//     text, same empty scene belief, same text-kind hint, same default
//     weights — with OCR confidence read as neutral. The visibility weight is
//     an observation-level constant (it is the same for every candidate of
//     one observation), so it shifts all raw scores equally and cancels in
//     the softmax; the candidate set and the probability distribution are
//     exactly the pipeline's.
//  4. minimise the NLL of the truth candidate over the returned candidate set
//     (matcher.FitTemperature, with the default sufficiency guards) on those
//     samples and write calibration.json.
//
// The fit refuses, with the matcher's typed errors and no calibration.json
// written, when the tuning set cannot calibrate a temperature:
// ErrInsufficientCalibrationData (fewer than MinIncorrect incorrect top-1
// examples or fewer than MinSamples samples — the synthetic tuning set's
// shape: 100% top-1 accuracy, so the NLL has no interior optimum) or
// ErrFitHitRangeBound (the NLL minimum sits on the search range's boundary).
// A degenerate optimum is a refusal, never a T (ADR-0035 "Calibration").
//
// The holdout split is never loaded into the fit: tuningSamples filters on the
// split recorded in truth.json, which the loader re-derives from the sample id
// and refuses to trust on mismatch.
func Calibrate(ctx context.Context, setDir, outDir string) (*Calibration, error) {
	ts, err := LoadTruth(setDir)
	if err != nil {
		return nil, err
	}
	prof, err := LoadScriptProfile(setDir)
	if err != nil {
		return nil, err
	}
	benchProfile = prof
	idx, err := prof.BuildIndex()
	if err != nil {
		return nil, fmt.Errorf("bench: matcher index: %v", err)
	}
	regions := prof.BuildRegions()

	kept := tuningSamples(ts)
	if len(kept) == 0 {
		return nil, fmt.Errorf("bench: no samples in split %q", SplitTuning)
	}
	runSet := *ts
	runSet.Samples = kept

	framesDir, err := os.MkdirTemp("", "bench-calibrate-frames-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(framesDir)
	dirs, err := RenderSamples(&runSet, framesDir)
	if err != nil {
		return nil, err
	}
	workers := 4
	if len(kept) < workers {
		workers = len(kept)
	}
	jobs := make(chan int)
	var (
		mu       sync.Mutex
		outcomes = make([]sampleOutcomes, len(kept))
		wg       sync.WaitGroup
	)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				s := kept[i]
				out, err := runSample(ctx, dirs[s.ID], s, idx, regions)
				if err != nil {
					fmt.Fprintf(os.Stderr, "bench: calibrate: sample %s NOT RUN: %v\n", s.ID, err)
					continue
				}
				mu.Lock()
				outcomes[i] = out
				mu.Unlock()
			}
		}()
	}
	for i := range kept {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	var fit []matcher.TemperatureSample
	var used, skipped int
	for _, o := range outcomes {
		if o.sample.id == "" {
			continue // NOT RUN slot
		}
		if o.sample.msgID == "" {
			skipped++ // empty box: nothing to calibrate against
			continue
		}
		text := o.commitText
		if text == "" {
			text = o.ocrText
		}
		if text == "" {
			skipped++
			continue
		}
		rank := idx.Rank(matcher.Observation{Text: text, OCRConf: 0, TextKindHint: "dialogue"}, matcher.RankOptions{})
		pos := -1
		for i, c := range rank.Candidates {
			if c.MessageID == o.sample.msgID {
				pos = i
				break
			}
		}
		if pos < 0 {
			// Retrieval miss: the truth is not in the returned set, so no
			// temperature can change its (zero) probability. Skip, counted.
			skipped++
			continue
		}
		scores := make([]float64, len(rank.Candidates))
		for i, c := range rank.Candidates {
			scores[i] = c.Score
		}
		fit = append(fit, matcher.TemperatureSample{Scores: scores, Correct: pos})
		used++
	}
	if len(fit) == 0 {
		return nil, fmt.Errorf("bench: no tuning sample produced a rankable reading (%d skipped); nothing to fit", skipped)
	}
	T, err := matcher.FitTemperature(fit, matcher.FitOptions{})
	if err != nil {
		// A refusal (insufficient data, range bound) is returned as-is so the
		// CLI can print NOT FITTED with the matcher's counts; no
		// calibration.json is written on either path.
		return nil, err
	}
	cal := &Calibration{
		SchemaVersion:       SchemaVersion,
		Set:                 ts.Set,
		Split:               SplitTuning,
		Temperature:         T,
		CalibrationRevision: matcher.CalibrationRevision + 1,
		Method:              "NLL of the truth candidate over the returned candidate set, deterministic grid fit over T in [0.05, 8] (log-spaced coarse grid + linear refinement)",
		SamplesUsed:         used,
		SamplesSkipped:      skipped,
		NLLAtT1:             matcher.TemperatureNLL(fit, 1.0),
		NLLAtFit:            matcher.TemperatureNLL(fit, T),
		GeneratedAt:         time.Now().UTC().Format(time.RFC3339),
	}
	if h, err := SetHash(filepath.Join(setDir, "truth.json")); err == nil {
		cal.SetHash = h
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	if err := writeJSON(filepath.Join(outDir, "calibration.json"), cal); err != nil {
		return nil, err
	}
	return cal, nil
}

// tuningSamples returns the set's tuning-split samples in set order. This
// filter is the entire holdout isolation of the fit (docs/spec/12: thresholds
// tuned on the tuning split; the holdout only ever grades). It is a pure
// function of the loaded truth and is tested as such (TestTuningSamplesNever
// ReadsHoldout).
func tuningSamples(ts *TruthSet) []Sample {
	var out []Sample
	for _, s := range ts.Samples {
		if s.Split == SplitTuning {
			out = append(out, s)
		}
	}
	return out
}

// matcherCalibrationDigest is the one-line identity of the matcher settings a
// run used, for the report manifest: which temperature and which calibration
// revision its probabilities were produced under (ADR-0035 "Calibration").
func matcherCalibrationDigest() string {
	return fmt.Sprintf("temperature %.6g (revision %d)", matcher.DefaultTemperature, matcher.CalibrationRevision)
}
