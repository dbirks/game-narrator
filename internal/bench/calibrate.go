package bench

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/matcher"
)

// Calibration is the output of `bench calibrate` (calibration.json): the
// temperature T fitted on the fit's data, with enough context to re-run the
// fit and to attribute a later report to the settings that produced it
// (ADR-0035 "Calibration": the revision is carried wherever the matcher's
// settings feed a cache key or fixture identity).
type Calibration struct {
	SchemaVersion int     `json:"schema_version"`
	Set           string  `json:"set"`
	Split         string  `json:"split"` // the fit never touches any holdout: synthetic data is the tuning split, captured data is graded only
	Temperature   float64 `json:"temperature"`
	// CalibrationRevision is the revision this fit would define if adopted as
	// the matcher's default (the current default is matcher.CalibrationRevision,
	// so the next one is that + 1).
	CalibrationRevision int    `json:"calibration_revision"`
	Method              string `json:"method"`
	SamplesUsed         int    `json:"samples_used"`
	// SamplesSkipped counts samples the fit cannot use: no reading (empty box)
	// or a retrieval miss (the truth message is not in the returned candidate
	// set). Skips are counted, never silently dropped.
	SamplesSkipped int `json:"samples_skipped"`
	// Sources is the per-source breakdown of the fit's data (one row per
	// synthetic tuning split and per captured fixture), so a refusal can say
	// exactly where the samples and the errors are.
	Sources     []CalibrationSource `json:"sources,omitempty"`
	NLLAtT1     float64             `json:"nll_at_t1"`
	NLLAtFit    float64             `json:"nll_at_fit"`
	SetHash     string              `json:"set_hash"`
	GeneratedAt string              `json:"generated_at"`
}

// CalibrationSource is one fit source's counts: the samples it contributed
// to the fit, its incorrect top-1 examples (the fit's raw material) and the
// skips it suffered. The class is the AGENTS.md rule-3 provenance class of
// the source's frames.
type CalibrationSource struct {
	Name          string `json:"name"`
	Class         string `json:"class"`
	Samples       int    `json:"samples"`
	IncorrectTop1 int    `json:"incorrect_top1"`
	Skipped       int    `json:"skipped"`
	// NotRun counts the samples the harness failed on (deadline, frame
	// source) for this source: the accounting line (used + skipped + not-run
	// = the source's sample count) must close, or a source that lost every
	// sample would read as "0 used, 0 skipped" — indistinguishable from a
	// source with nothing to calibrate.
	NotRun int `json:"not_run,omitempty"`
}

// Calibrate fits the matcher's temperature on the set's tuning split
// (docs/spec/12 "Split into tuning vs holdout"; ADR-0035 "Calibration"). It
// is the synthetic-only entry point: equivalent to CalibrateCombined with no
// captured sources. The fit runs every sample through the same real pipeline
// as the bench (real Tesseract, real matcher, real tracker) and re-ranks the
// sample's committed reading, so the fit sees the pipeline's own stable
// readings, not an approximation of them; the re-rank is faithful to the
// pipeline's rankWorker observation (same text, same default weights, OCR
// confidence read as neutral) and uses the candidate set and probability
// distribution exactly as the pipeline computed them. A degenerate fit is a
// refusal, never a T (ADR-0035 "Calibration").
func Calibrate(ctx context.Context, setDir, outDir string) (*Calibration, error) {
	return CalibrateCombined(ctx, setDir, nil, "", outDir)
}

// fitSource is one data source of a temperature fit: where its samples come
// from, the index their readings are re-ranked against, and the frame source
// each sample replays from. Sources run one after another (the shared
// benchProfile sidecar script is swapped between phases, never mid-phase —
// runFitSource installs the source's own profile before its samples run, so
// the sidecar always answers from the script the samples were resolved
// against).
type fitSource struct {
	name    string
	class   string
	samples []Sample
	// frames resolves one sample to its frames dir and the replay manifest
	// override ("" = the default <dir>/timestamps.json). ok=false marks the
	// source as unrunnable (render failure) — the whole fit is then NOT RUN,
	// not partially fit.
	frames  func(id string) (dir, timestamps string, ok bool)
	idx     *matcher.Index
	regions []games.Region
	// profile is the sidecar script this source's samples answer from (the
	// synthetic set's profile or the fixture's imported script).
	profile *ScriptProfile
	// prepare renders this source's frames on demand (the class b′ degraded
	// variants; nil for sources whose frames are ready at fit start). The fit
	// loop calls it immediately before the source runs; an error NOT RUNs the
	// whole fit (the same discipline as a synthetic render failure).
	prepare func() error
	// cleanup frees this source's temp frames. The fit loop calls it
	// immediately after the source runs, so the disk peak is one source's
	// frames, never all sources' (ADR-0064: six dense-fixture degraded variants
	// prepared up front did not fit the shared /tmp).
	cleanup func()
}

// syntheticFitSource prepares the synthetic set's tuning split as a fit
// source: loads the truth and the script, renders the tuning samples' frames
// (same generator as the run) and installs the script for the sidecar. The
// returned cleanup must run (deferred by the caller) before the fit ends.
func syntheticFitSource(ctx context.Context, setDir string) (*fitSource, *TruthSet, func(), error) {
	ts, err := LoadTruth(setDir)
	if err != nil {
		return nil, nil, nil, err
	}
	prof, err := LoadScriptProfile(setDir)
	if err != nil {
		return nil, nil, nil, err
	}
	benchProfile = prof
	idx, err := prof.BuildIndex()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("bench: matcher index: %v", err)
	}
	regions := prof.BuildRegions()

	kept := tuningSamples(ts)
	if len(kept) == 0 {
		return nil, nil, nil, fmt.Errorf("bench: no samples in split %q", SplitTuning)
	}
	runSet := *ts
	runSet.Samples = kept

	framesDir, err := os.MkdirTemp("", "bench-calibrate-frames-")
	if err != nil {
		return nil, nil, nil, err
	}
	dirs, err := RenderSamples(&runSet, framesDir)
	if err != nil {
		os.RemoveAll(framesDir)
		return nil, nil, nil, err
	}
	src := &fitSource{
		name:    fmt.Sprintf("%s (%s split, %d samples)", ts.Set, SplitTuning, len(kept)),
		class:   ClassSynthetic,
		samples: kept,
		frames: func(id string) (string, string, bool) {
			d, ok := dirs[id]
			return d, "", ok
		},
		idx:     idx,
		regions: regions,
		profile: prof,
	}
	return src, ts, func() { os.RemoveAll(framesDir) }, nil
}

// checkCapturedImageSHA is the fixture-replay identity gate the fit shares
// with RunCapturedBench: a fixture recorded against a different image would
// grade the wrong frames, so the image's SHA-256 must match the manifest's.
func checkCapturedImageSHA(fx *CapturedFixture, imagePath string) error {
	sha, err := ImageSHA256(imagePath)
	if err != nil {
		return fmt.Errorf("NOT RUN: bench calibrate cannot read the game image %s: %v", filepath.Base(imagePath), err)
	}
	if fx.Manifest.GameImageSHA256 != "" && sha != fx.Manifest.GameImageSHA256 {
		return fmt.Errorf("NOT RUN: bench calibrate: the image's SHA-256 does not match fixture %s's manifest (re-record the fixture or point GAME_NARRATOR_GAME_IMAGE_PATH at the recorded image)", fx.ID)
	}
	return nil
}

// capturedFitSource prepares one loaded fixture as a fit source: the
// fixture's dialogue segments and the fixture's frames dir as the frame
// source. The caller loads the fixture and checks the image SHA once (the
// degraded variants reuse the same load: VerifyFixture hashes every frame,
// so a second load of the same dir would pay that cost twice).
func capturedFitSource(fx *CapturedFixture, samples []Sample, msgs []games.Message, idx *matcher.Index) (*fitSource, func(), error) {
	prof := CapturedProfile(fx, msgs)
	benchProfile = prof
	work, err := os.MkdirTemp("", "bench-calibrate-captured-")
	if err != nil {
		return nil, nil, err
	}
	framesDir := filepath.Join(fx.Dir, "frames")
	return &fitSource{
		name:    fmt.Sprintf("%s (captured, %d samples)", fx.ID, len(samples)),
		class:   ClassCaptured,
		samples: samples,
		frames: func(id string) (string, string, bool) {
			for _, s := range samples {
				if s.ID != id {
					continue
				}
				tsPath := filepath.Join(work, "ts-"+id+".json")
				if err := writeManifest(tsPath, s.Frames); err != nil {
					return "", "", false
				}
				return framesDir, tsPath, true
			}
			return "", "", false
		},
		idx:     idx,
		regions: prof.BuildRegions(),
		profile: prof,
	}, func() { os.RemoveAll(work) }, nil
}

// collectFit re-ranks each completed sample's committed stable reading
// (falling back to its last OCR reading) against the sample's index and
// returns the fit's raw material plus the counts: used, skipped (empty box
// or retrieval miss — counted, never silently dropped) and the incorrect
// top-1 examples (the fit's raw material: a fit needs wrong top-1s to have
// an interior optimum).
func collectFit(outcomes []sampleOutcomes, idx *matcher.Index) (fit []matcher.TemperatureSample, used, skipped, incorrect int) {
	for _, o := range outcomes {
		if o.sample.id == "" {
			continue // NOT RUN slot
		}
		if o.sample.msgID == "" {
			skipped++ // empty box (or a truth line not in the script): nothing to calibrate against
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
		if pos != 0 {
			incorrect++
		}
	}
	return fit, used, skipped, incorrect
}

// runFitSource runs one source's samples through the real pipeline (the same
// runSample the bench uses) and returns the per-sample outcomes in source
// order, NOT RUN slots dropped, plus the count of those NOT RUN slots (a
// source's not-run samples must stay visible in the fit's accounting, never
// vanish). It installs the source's own sidecar profile
// first: the sources run one after another and the sidecar answers from the
// installed script, so a source's samples must never be answered from the
// previous source's profile.
func runFitSource(ctx context.Context, s *fitSource) ([]sampleOutcomes, int) {
	if s.profile != nil {
		benchProfile = s.profile
	}
	// Bounded parallelism (ADR-0064): the degraded dense-fixture sources replay
	// the fixture's whole frame list each, and the concurrent Tesseract
	// processes must not starve each other's per-call deadline — unbounded
	// workers on an 8-core box oversubscribe the host and the bench
	// manufactures the load the replay's retry budget exists to tolerate.
	// Scheduler detail only: per-frame OCR is deterministic, results do not
	// depend on the worker count.
	workers := replayWorkers(len(s.samples))
	jobs := make(chan int)
	var (
		mu       sync.Mutex
		notRun   int
		outcomes = make([]sampleOutcomes, len(s.samples))
		wg       sync.WaitGroup
	)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				sa := s.samples[i]
				dir, tsFile, ok := s.frames(sa.ID)
				if !ok {
					fmt.Fprintf(os.Stderr, "bench: calibrate: sample %s NOT RUN (frame source)\n", sa.ID)
					mu.Lock()
					notRun++
					mu.Unlock()
					continue
				}
				out, err := runSample(ctx, dir, tsFile, sa, s.idx, s.regions)
				if err != nil {
					fmt.Fprintf(os.Stderr, "bench: calibrate: sample %s NOT RUN: %v\n", sa.ID, err)
					mu.Lock()
					notRun++
					mu.Unlock()
					continue
				}
				mu.Lock()
				outcomes[i] = out
				mu.Unlock()
			}
		}()
	}
	for i := range s.samples {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	var ok []sampleOutcomes
	for _, o := range outcomes {
		if o.sample.id != "" {
			ok = append(ok, o)
		}
	}
	return ok, notRun
}

// CalibrateCombined fits the matcher's temperature on the combined fit data:
// the synthetic set's tuning split plus the dialogue segments of the named
// captured fixtures (their frames replayed through the same real pipeline).
// The captured data is the "real-frame observations" of the fit — class (b)
// frames, the synthetic stand-in for the labelled errors a fit needs. The
// holdout discipline is preserved: no synthetic holdout sample enters the
// fit (tuningSamples), and a captured sample enters as an observation of the
// same pipeline, not as a tuned threshold (docs/spec/12: thresholds tuned on
// the tuning split; the captured holdout itself is graded separately by
// RunCapturedBench and never feeds a threshold).
//
// The fit refuses, with the matcher's typed errors and no calibration.json
// written, when the combined data cannot calibrate a temperature:
// ErrInsufficientCalibrationData (fewer than MinIncorrect incorrect top-1
// examples or fewer than MinSamples samples) or ErrFitHitRangeBound (the NLL
// minimum on the fit range's boundary). A degenerate optimum is a refusal,
// never a T (ADR-0035 "Calibration"). A refusal wraps the matcher's error
// with the per-source breakdown (ErrCalibrationInsufficient) so the operator
// sees exactly how many samples and errors exist where.
func CalibrateCombined(ctx context.Context, synthSetDir string, fixtureDirs []string, imagePath, outDir string) (*Calibration, error) {
	pinReplayEngineThreads()
	var (
		sources  []fitSource
		synthSet *TruthSet
	)
	cleanups := make([]func(), 0, len(fixtureDirs)+1)
	cleanupAll := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
	if synthSetDir != "" {
		src, ts, cleanup, err := syntheticFitSource(ctx, synthSetDir)
		if err != nil {
			cleanupAll()
			return nil, err
		}
		src.cleanup = cleanup
		sources = append(sources, *src)
		synthSet = ts
		cleanups = append(cleanups, cleanup)
	}
	if len(fixtureDirs) > 0 {
		if imagePath == "" {
			return nil, fmt.Errorf("NOT RUN: bench calibrate with captured fixtures needs the game image (set GAME_NARRATOR_GAME_IMAGE_PATH, or pass --image)")
		}
		msgs, err := ImportScript(ctx, imagePath)
		if err != nil {
			cleanupAll()
			return nil, err
		}
		idx, err := BuildCapturedIndex(msgs)
		if err != nil {
			cleanupAll()
			return nil, fmt.Errorf("bench: captured matcher index: %v", err)
		}
		for _, dir := range fixtureDirs {
			fx, err := LoadCapturedFixture(dir)
			if err != nil {
				cleanupAll()
				return nil, err
			}
			if err := checkCapturedImageSHA(fx, imagePath); err != nil {
				cleanupAll()
				return nil, err
			}
			samples := fx.Segments(resolveTruth(msgs))
			src, cleanup, err := capturedFitSource(fx, samples, msgs, idx)
			if err != nil {
				cleanupAll()
				return nil, err
			}
			src.cleanup = cleanup
			sources = append(sources, *src)
			cleanups = append(cleanups, cleanup)
			// The degraded-render sources (class b′, ADR-0058 amendment 1):
			// the same samples replayed over the fixture's frames re-degraded
			// at bench time (deterministic JPEG / downscale / seeded noise /
			// crop offset — never stored, labelled b′, never clean captures).
			// They are the fit's raw material: the labelled frames where the
			// matcher's top-1 genuinely errs. Their temp frames are prepared
			// and freed one variant at a time by the fit loop (ADR-0064).
			dsrcs, err := degradedFitSources(fx, samples, msgs, idx)
			if err != nil {
				cleanupAll()
				return nil, err
			}
			for i := range dsrcs {
				sources = append(sources, dsrcs[i])
			}
		}
	}
	defer cleanupAll()
	if len(sources) == 0 {
		return nil, fmt.Errorf("bench: calibrate needs a synthetic set and/or captured fixtures")
	}

	var fit []matcher.TemperatureSample
	srcRows := make([]CalibrationSource, 0, len(sources))
	totalUsed, totalSkipped := 0, 0
	for i := range sources {
		s := &sources[i]
		// Stream the sources' temp frames: prepare this one, run it, free it
		// (ADR-0064). A render failure here NOT RUNs the whole fit, as it did
		// when the renders were prepared up front.
		if s.prepare != nil {
			if err := s.prepare(); err != nil {
				cleanupAll()
				return nil, err
			}
		}
		outcomes, notRun := runFitSource(ctx, s)
		f, used, skipped, incorrect := collectFit(outcomes, s.idx)
		fit = append(fit, f...)
		totalUsed += used
		totalSkipped += skipped
		srcRows = append(srcRows, CalibrationSource{
			Name: s.name, Class: s.class,
			Samples: used, IncorrectTop1: incorrect, Skipped: skipped, NotRun: notRun,
		})
		if s.cleanup != nil {
			s.cleanup()
		}
	}
	if len(fit) == 0 {
		return nil, fmt.Errorf("bench: no sample produced a rankable reading (%d skipped); nothing to fit", totalSkipped)
	}
	T, err := matcher.FitTemperature(fit, matcher.FitOptions{})
	if err != nil {
		// A refusal is returned with the per-source breakdown attached so the
		// operator can say exactly how many samples/errors exist and what
		// recording would provide them; no calibration.json is written on
		// either refusal path.
		return nil, &ErrCalibrationInsufficient{Inner: err, Sources: srcRows}
	}
	set := "combined"
	if synthSet != nil {
		set = synthSet.Set
	}
	cal := &Calibration{
		SchemaVersion:       SchemaVersion,
		Set:                 set,
		Split:               SplitTuning,
		Temperature:         T,
		CalibrationRevision: matcher.CalibrationRevision + 1,
		Method:              "NLL of the truth candidate over the returned candidate set, deterministic grid fit over T in [0.05, 8] (log-spaced coarse grid + linear refinement)",
		SamplesUsed:         totalUsed,
		SamplesSkipped:      totalSkipped,
		Sources:             srcRows,
		NLLAtT1:             matcher.TemperatureNLL(fit, 1.0),
		NLLAtFit:            matcher.TemperatureNLL(fit, T),
		GeneratedAt:         time.Now().UTC().Format(time.RFC3339),
	}
	if synthSet != nil && synthSet.Set != "" {
		if h, err := SetHash(filepath.Join(synthSetDir, "truth.json")); err == nil {
			cal.SetHash = h
		}
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	if err := writeJSON(filepath.Join(outDir, "calibration.json"), cal); err != nil {
		return nil, err
	}
	return cal, nil
}

// ErrCalibrationInsufficient is the combined fit's refusal: the matcher's
// typed refusal (ErrInsufficientCalibrationData or ErrFitHitRangeBound, both
// un-wrapped so errors.As/errors.Is still find them) plus the per-source
// breakdown of the fit's data. The breakdown is the evidence the refusal
// asks for: how many samples and how many incorrect top-1s each source
// contributed, so the unblock plan (more recorded dialogue lines, more
// fixtures) is a number, not a hope.
type ErrCalibrationInsufficient struct {
	Inner   error
	Sources []CalibrationSource
}

func (e *ErrCalibrationInsufficient) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "bench: %v", e.Inner)
	if len(e.Sources) > 0 {
		b.WriteString("; fit data per source:")
		for _, s := range e.Sources {
			fmt.Fprintf(&b, " [%s class %s: %d used, %d incorrect top-1, %d skipped, %d not-run]", s.Name, s.Class, s.Samples, s.IncorrectTop1, s.Skipped, s.NotRun)
		}
	}
	return b.String()
}

// Unwrap exposes the matcher's typed error so the CLI's NOT FITTED handling
// (errors.As on ErrInsufficientCalibrationData / ErrFitHitRangeBound) keeps
// working unchanged.
func (e *ErrCalibrationInsufficient) Unwrap() error { return e.Inner }

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
