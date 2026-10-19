package matcher

import (
	"errors"
	"fmt"
	"math"
)

// Temperature scaling calibrates the probabilities the ranker reports. The raw
// log-linear score is a weighted sum of logs on an unbounded scale: with the
// default weights its spread is far wider than a softmax interprets as
// certainty, so a correct top-1 can carry a probability of ~0.6 — badly
// under-confident. The commit policy and the Clef escalation thresholds
// (docs/spec/05 "Clef adjudication and commit policy") consume that
// probability, so a systematic under-read turns real content into needless
// Clef calls and abstentions.
//
// Temperature scaling is the minimal honest fix: divide every raw score by one
// scalar T > 0 before the softmax. Because the division is a common factor
// across the candidate set, the ordering (hence the top-1) is unchanged for
// every T — asserted by TestTemperatureKeepsRankOrder — and only the spread of
// the probabilities moves. No weights, no thresholds, no component is re-tuned.
//
// T is fitted by minimising the negative log-likelihood (NLL) of the truth
// candidate over the returned candidate set on the tuning split only
// (internal/bench owns the split; ADR-0038 keeps the holdout out of the fit).
// FitTemperature refuses to produce a T when the tuning set cannot support
// one: fewer than FitOptions.MinIncorrect incorrect top-1 examples or fewer
// than FitOptions.MinSamples samples (ErrInsufficientCalibrationData), or an
// NLL minimum on the search range's boundary (ErrFitHitRangeBound) — a
// degenerate optimum is an error, not a result.
//
// CalibrationRevision numbers the default temperature so a fixture, cache key
// or report that embeds the matcher's settings can say which probability
// scale it was produced under: revision 1 is T = 1 (plain softmax, ADR-0035
// as written); its would-be revision 2 (the NLL fit on the bench-synthetic-001
// tuning split) was rejected as a degenerate optimum. Revision 2 is the T
// fitted on the real-content mixture with the class (b″) hard set's labelled
// errors and adopted 2026-10-19 (ADR-0058 amendment 7, ADR-0035
// "Calibration"). A fixture scored at one revision must not silently stand in
// for another.
const (
	// CalibrationRevision is the revision of the default temperature the
	// matcher reports. Revision 1 was T = 1, plain softmax (ADR-0035 as
	// written). The NLL fit on the synthetic bench set (a would-be revision 2
	// on the synthetic-only data) was refused as a degenerate optimum: the
	// synthetic tuning split has no incorrect top-1 examples, so minimising
	// the NLL drives T to the range floor. The revision-2 default adopted
	// 2026-10-19 is the NLL fit on the real-content mixture — the (a + b) +
	// b′ + b″ fit data, with the class (b″) hard set's 22 labelled incorrect
	// top-1 examples — which passed every guard (≥50 samples, ≥10 incorrect
	// top-1, interior NLL optimum; ADR-0058 amendments 6–7, ADR-0035
	// "Calibration").
	CalibrationRevision = 2

	// DefaultTemperature is the revision-2 default: the T fitted on the
	// (a + b) + b′ + b″ mixture (270 fit samples, 101 skipped, 22 labelled
	// errors, all from the b″ hard set — ADR-0058 amendment 7). It sits
	// inside the fit range [0.05, 8], 0.0228 above the floor; the
	// linear-refinement minimum sits at the centre of its ±one-coarse-step
	// window, so the data, not the range, pins it — with the documented
	// caveat that it is a heavy-sharpening T (207/270 fit samples sit in the
	// [0.9, 1.0) reliability bin at mean probability 0.9992). The acceptance
	// gate is unaffected: score.go keeps its second softmax pinned at T = 1
	// (revision 1), so a calibration revision can never move an accept/abstain
	// decision (ADR-0035 "Acceptance gate unchanged"). A profile may override
	// via RankOptions.Temperature (sanitised into [minTemperature,
	// maxTemperature]); reverting the adoption is Temperature = 1 — no stored
	// data is affected, because probabilities are computed at rank time and
	// the audio cache key does not embed matcher settings (ADR-0035).
	DefaultTemperature = 0.07280236748934843
)

// minTemperature and maxTemperature bound every usable T. T → 0 makes the
// softmax one-hot (a numerical tie becomes absolute certainty); T → ∞ flattens
// it to uniform. The fit grid spans exactly this range, and an out-of-range
// override is a caller bug that is repaired to the range rather than
// propagated — the same repair philosophy as the weight sanitizer.
const (
	minTemperature = 0.05
	maxTemperature = 8.0
)

// TemperatureSample is one calibration observation: the raw log-linear scores
// of the returned candidate set, in ranking order, and the index of the truth
// candidate among them. A sample whose truth is not in the returned set is
// unusable for the fit by construction — the softmax is over the returned set
// and cannot represent a class it never returned — and is skipped, not zeroed
// (a temperature cannot fix a retrieval miss, and pretending otherwise would
// bias the fit).
type TemperatureSample struct {
	// Scores holds each returned candidate's raw log-linear score, in ranking
	// order. Every value must be finite.
	Scores []float64
	// Correct is the index into Scores of the truth candidate.
	Correct int
}

// TemperatureNLL is the negative log-likelihood of the truth candidate at T
// over the samples: Σ -log softmax_T(truth). The stabilising max-subtraction
// is the same one the ranker's softmax uses, so the fit evaluates exactly the
// distribution the ranker emits, not a re-derivation of it. Degenerate input
// (a non-finite or empty sample) yields +Inf rather than a false optimum.
func TemperatureNLL(samples []TemperatureSample, T float64) float64 {
	if T <= 0 || math.IsNaN(T) {
		return math.Inf(1)
	}
	var nll float64
	for _, s := range samples {
		max := math.Inf(-1)
		for _, sc := range s.Scores {
			if v := sc / T; v > max {
				max = v
			}
		}
		sum := 0.0
		for _, sc := range s.Scores {
			sum += math.Exp(sc/T - max)
		}
		if sum <= 0 || math.IsInf(sum, 1) || math.IsNaN(sum) {
			return math.Inf(1)
		}
		p := math.Exp(s.Scores[s.Correct]/T-max) / sum
		if p <= 0 || math.IsNaN(p) {
			return math.Inf(1)
		}
		nll -= math.Log(p)
	}
	return nll
}

// FitOptions configures the fit's sufficiency guards (FitTemperature). Both
// thresholds default from DefaultFitOptions when unset (zero or negative): a
// caller that disables the guard is the caller bug the guard exists to catch —
// a fit on 100% correct data is a boundary optimum, not a calibration.
type FitOptions struct {
	// MinIncorrect is the smallest number of tuning samples whose top-1 is
	// wrong (the truth candidate is not best-ranked) that a fit may see.
	// Without wrong top-1s the NLL has no interior optimum: every margin is
	// one way and the minimum runs to the range floor.
	MinIncorrect int

	// MinSamples is the smallest tuning-set size a fit may use: a temperature
	// fitted on a handful of samples is noise, not calibration.
	MinSamples int
}

// DefaultFitOptions are the sufficiency guards: at least 10 incorrect top-1
// examples and at least 50 tuning samples. They are the minimum for a fit to
// be evidence rather than an artefact of the data (ADR-0035 "Calibration").
func DefaultFitOptions() FitOptions {
	return FitOptions{MinIncorrect: 10, MinSamples: 50}
}

func (o FitOptions) sanitize() FitOptions {
	d := DefaultFitOptions()
	if o.MinIncorrect <= 0 {
		o.MinIncorrect = d.MinIncorrect
	}
	if o.MinSamples <= 0 {
		o.MinSamples = d.MinSamples
	}
	return o
}

// ErrInsufficientCalibrationData reports that the tuning set cannot calibrate
// the temperature: it has fewer samples, or fewer incorrect top-1 examples,
// than the fit's sufficiency guards allow. It carries the counts the fit saw
// (Samples, of which Incorrect have a wrong top-1) and the thresholds they
// failed, so `bench calibrate` can print the reason and the unblock plan
// (collect real frames with labelled errors — the Dolphin fixture factory,
// M6) instead of shipping a T the data did not earn.
type ErrInsufficientCalibrationData struct {
	Samples      int
	Incorrect    int
	MinSamples   int
	MinIncorrect int
}

func (e *ErrInsufficientCalibrationData) Error() string {
	return fmt.Sprintf("matcher: insufficient calibration data: %d tuning samples with %d incorrect top-1, need at least %d samples and %d incorrect top-1 — collect frames with labelled errors (the Dolphin fixture factory, M6) before a fit can be evidence",
		e.Samples, e.Incorrect, e.MinSamples, e.MinIncorrect)
}

// ErrFitHitRangeBound reports that the NLL minimum sits on the fit range's
// boundary (T = minTemperature or maxTemperature): the data wants a sharper
// or flatter spread than the documented range allows, so the boundary — not
// the data — decided the fit. That is a degenerate optimum, and the fit fails
// with it rather than returning a boundary T as if it were a measured result.
var ErrFitHitRangeBound = errors.New("matcher: fit hit range bound")

// FitTemperature fits T by minimising the NLL on the tuning samples with a
// deterministic grid search: a log-spaced coarse grid over
// [minTemperature, maxTemperature], then a linear refinement grid around the
// coarse winner. Ties resolve to the smaller T (the sharper probabilities),
// and the search is a pure function of the input, so the same data always
// yields the same T.
//
// Two guards turn degenerate fits into errors rather than results:
//
//   - the tuning set must contain at least opts.MinIncorrect samples whose
//     top-1 is wrong and at least opts.MinSamples samples; fewer, and the NLL
//     has no interior optimum to identify — every margin is one way and the
//     minimum runs to the range floor, which would report "calibrated" when
//     the data cannot calibrate (ErrInsufficientCalibrationData, with the
//     counts);
//   - a minimum that lands on the search range's boundary
//     (ErrFitHitRangeBound) is the same degenerate optimum the grid can no
//     longer hide: the data says "further" and the documented range refuses,
//     so the fit fails instead of returning a boundary T as if it were a
//     measured result.
func FitTemperature(samples []TemperatureSample, opts FitOptions) (float64, error) {
	if err := validateTemperatureSamples(samples); err != nil {
		return 0, err
	}
	opts = opts.sanitize()
	incorrect := 0
	for _, s := range samples {
		if s.Correct != 0 {
			incorrect++
		}
	}
	if len(samples) < opts.MinSamples || incorrect < opts.MinIncorrect {
		return 0, &ErrInsufficientCalibrationData{
			Samples: len(samples), Incorrect: incorrect,
			MinSamples: opts.MinSamples, MinIncorrect: opts.MinIncorrect,
		}
	}
	best, bestT := TemperatureNLL(samples, minTemperature), minTemperature
	// Coarse log grid: 1601 points over two orders of magnitude, so the
	// resolution near the optimum is ~0.3% before refinement. The divisor is
	// written as a float division on purpose: in a constant expression
	// 1/(coarse-1) would be integer division (zero), collapsing the grid to a
	// single point at minTemperature.
	const coarse = 1601
	ratio := math.Pow(maxTemperature/minTemperature, 1.0/float64(coarse-1))
	for i := 1; i < coarse; i++ {
		T := minTemperature * math.Pow(ratio, float64(i))
		if T > maxTemperature {
			T = maxTemperature // rounding at the endpoint must stay inside the range
		}
		if nll := TemperatureNLL(samples, T); nll < best {
			best, bestT = nll, T
		}
	}
	// Linear refinement across one coarse step either side of the winner.
	lo, hi := bestT/ratio, bestT*ratio
	if lo < minTemperature {
		lo = minTemperature
	}
	if hi > maxTemperature {
		hi = maxTemperature
	}
	const fine = 1601
	for i := 0; i < fine; i++ {
		T := lo + (hi-lo)*float64(i)/float64(fine-1)
		if nll := TemperatureNLL(samples, T); nll < best {
			best, bestT = nll, T
		}
	}
	// The only values the grids can produce exactly at the range limits are
	// the endpoints themselves (every other point is strictly interior), so
	// an exact comparison is the boundary test: the NLL minimum ran to the
	// edge of the search range and the data, not the fit, chose it.
	if bestT <= minTemperature || bestT >= maxTemperature {
		return 0, fmt.Errorf("%w at T = %.6g: the NLL minimum sits on the fit range's boundary, so the range, not the data, decided the fit — collect data with labelled errors (the Dolphin fixture factory, M6) instead of shipping a boundary T", ErrFitHitRangeBound, bestT)
	}
	return bestT, nil
}

func validateTemperatureSamples(samples []TemperatureSample) error {
	if len(samples) == 0 {
		return fmt.Errorf("matcher: FitTemperature needs at least one sample")
	}
	for i, s := range samples {
		if len(s.Scores) == 0 {
			return fmt.Errorf("matcher: FitTemperature sample %d: no scores", i)
		}
		if s.Correct < 0 || s.Correct >= len(s.Scores) {
			return fmt.Errorf("matcher: FitTemperature sample %d: correct index %d out of range for %d scores", i, s.Correct, len(s.Scores))
		}
		for _, sc := range s.Scores {
			if math.IsNaN(sc) || math.IsInf(sc, 0) {
				return fmt.Errorf("matcher: FitTemperature sample %d: non-finite score %v", i, sc)
			}
		}
	}
	return nil
}
