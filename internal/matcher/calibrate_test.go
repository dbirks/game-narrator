package matcher

import (
	"errors"
	"math"
	"strings"
	"testing"
)

// TestTemperatureDefaultsUnchanged pins the shipped default: the temperature
// stays at T = 1 (calibration revision 1). The synthetic-set fit was rejected
// as a degenerate optimum (ADR-0035 "Calibration"), so the default must not
// move to a fitted value; a per-profile override (RankOptions.Temperature)
// remains the supported way to adopt a real fit.
func TestTemperatureDefaultsUnchanged(t *testing.T) {
	if DefaultTemperature != 1.0 {
		t.Errorf("DefaultTemperature = %v, want 1.0 (the synthetic fit is degenerate and was not shipped)", DefaultTemperature)
	}
	if CalibrationRevision != 1 {
		t.Errorf("CalibrationRevision = %d, want 1 (plain softmax; a real fit would define a new revision)", CalibrationRevision)
	}
	// The default rank options carry the default temperature, and an unset
	// temperature sanitises to it.
	if got := DefaultRankOptions().Temperature; got != DefaultTemperature {
		t.Errorf("DefaultRankOptions().Temperature = %v, want the default %v", got, DefaultTemperature)
	}
	if got := (RankOptions{}).sanitize().Temperature; got != DefaultTemperature {
		t.Errorf("unset Temperature sanitised to %v, want the default %v", got, DefaultTemperature)
	}
}

// TestFitTemperatureKnownOptimum fits on a set whose optimum is established by
// an independent method. The samples are three shapes (one truth leading,
// one trailing, one middle-ranked truth) repeated with weights so the set
// carries 50 samples with 10 incorrect top-1 examples — inside the
// sufficiency guard — and the weighted NLL minimum is interior to the grid.
// The NLL is strictly convex in x = 1/T — its second derivative is the
// weighted variance Σ(s_j−s_t)²q_j(1−q_j) > 0 — so the minimum is unique and
// its location T* is found by bisection on the (numerical) derivative, a
// method independent of the grid fit. The fit must recover T*.
func TestFitTemperatureKnownOptimum(t *testing.T) {
	// Weights: 30 correct-leading (pulls T down), 10 correct-trailing (pulls
	// T up), 10 incorrect-middle-ranked (10 incorrect top-1 examples).
	base := []TemperatureSample{
		{Scores: []float64{2, 0}, Correct: 0},      // truth leads by 2 (pulls T down)
		{Scores: []float64{0.5, 1.5}, Correct: 0},  // truth trails by 1 (pulls T up)
		{Scores: []float64{2.5, 2, 0}, Correct: 1}, // truth middle (pulls down at large T, up at small)
	}
	weights := []int{30, 10, 10}
	samples := make([]TemperatureSample, 0, 50)
	for i, b := range base {
		for k := 0; k < weights[i]; k++ {
			samples = append(samples, b)
		}
	}
	if len(samples) != 50 {
		t.Fatalf("test setup: %d samples, want 50", len(samples))
	}
	nllDeriv := func(T float64) float64 {
		h := 1e-4 * T
		return (TemperatureNLL(samples, T+h) - TemperatureNLL(samples, T-h)) / (2 * h)
	}
	// The derivative must change sign inside the grid (interior optimum);
	// without it this test would be asserting a boundary case by accident.
	lo, hi := minTemperature, maxTemperature
	if nllDeriv(lo) > 0 || nllDeriv(hi) < 0 {
		t.Fatalf("test setup: no interior optimum (deriv at %.2f = %.4f, at %.2f = %.4f)", lo, nllDeriv(lo), hi, nllDeriv(hi))
	}
	for i := 0; i < 100; i++ {
		mid := (lo + hi) / 2
		if nllDeriv(mid) < 0 {
			lo = mid
		} else {
			hi = mid
		}
	}
	tStar := (lo + hi) / 2

	T, err := FitTemperature(samples, FitOptions{})
	if err != nil {
		t.Fatalf("FitTemperature: %v", err)
	}
	d := T - tStar
	if d < 0 {
		d = -d
	}
	if d > 0.01 {
		t.Fatalf("T = %v, want %v ± 0.01 (bisection reference)", T, tStar)
	}
	// The fit must not be worse than the reference minimum.
	if nll := TemperatureNLL(samples, T); nll > TemperatureNLL(samples, tStar)+1e-9 {
		t.Errorf("NLL(T=%v) = %v > NLL(T*) = %v", T, nll, TemperatureNLL(samples, tStar))
	}
}

// TestFitTemperatureDegenerateSet: a tuning set without incorrect top-1
// examples cannot calibrate the temperature — every margin is one way, so the
// NLL keeps falling as T falls and the only "optimum" is the range floor,
// which would ship a near-certain matcher. The fit must refuse with the
// typed error and its counts, never produce a T, and honour the configurable
// thresholds.
func TestFitTemperatureDegenerateSet(t *testing.T) {
	// 50 samples, every top-1 correct (the synthetic bench set's shape): each
	// truth leads by a wide margin, so no sample is an incorrect top-1.
	samples := make([]TemperatureSample, 0, 50)
	for i := 0; i < 50; i++ {
		samples = append(samples, TemperatureSample{Scores: []float64{10, 1, 0}, Correct: 0})
	}
	// Defaults: 50 samples passes the size guard, 0 incorrect top-1 fails the
	// quality guard.
	if _, err := FitTemperature(samples, FitOptions{}); err == nil {
		t.Fatal("FitTemperature on an all-correct set succeeded, want ErrInsufficientCalibrationData")
	} else if insuf, ok := err.(*ErrInsufficientCalibrationData); !ok || insuf.Samples != 50 || insuf.Incorrect != 0 || insuf.MinSamples != 50 || insuf.MinIncorrect != 10 {
		t.Fatalf("error = %v (%T), want *ErrInsufficientCalibrationData{Samples: 50, Incorrect: 0, MinSamples: 50, MinIncorrect: 10}", err, err)
	}
	// A smaller set fails the size guard first.
	small := samples[:49]
	if _, err := FitTemperature(small, FitOptions{}); err == nil {
		t.Fatal("FitTemperature on 49 samples succeeded, want ErrInsufficientCalibrationData")
	} else if insuf, ok := err.(*ErrInsufficientCalibrationData); !ok || insuf.Samples != 49 || insuf.Incorrect != 0 {
		t.Fatalf("error = %v (%T), want *ErrInsufficientCalibrationData{Samples: 49, Incorrect: 0}", err, err)
	}
	// An all-correct set can never pass the guard under any usable options
	// (MinIncorrect ≤ 0 sanitises back to the default 10), so its degenerate
	// NLL minimum can never be shipped as a T.
}

// TestFitTemperatureConfigurableGuards: the sufficiency thresholds are
// configurable, not constants — tightening either one refuses a set the
// defaults accept, and loosening them accepts a set the defaults refuse.
func TestFitTemperatureConfigurableGuards(t *testing.T) {
	// The known-optimum shape: 50 samples, 10 incorrect top-1 (the defaults
	// accept it).
	base := []TemperatureSample{
		{Scores: []float64{2, 0}, Correct: 0},
		{Scores: []float64{0.5, 1.5}, Correct: 0},
		{Scores: []float64{2.5, 2, 0}, Correct: 1},
	}
	weights := []int{30, 10, 10}
	samples := make([]TemperatureSample, 0, 50)
	for i, b := range base {
		for k := 0; k < weights[i]; k++ {
			samples = append(samples, b)
		}
	}
	if _, err := FitTemperature(samples, FitOptions{}); err != nil {
		t.Fatalf("defaults: FitTemperature: %v (want a fit)", err)
	}
	for name, opts := range map[string]FitOptions{
		"tighter incorrect": {MinIncorrect: 11, MinSamples: 10},
		"tighter samples":   {MinIncorrect: 10, MinSamples: 51},
	} {
		if _, err := FitTemperature(samples, opts); err == nil {
			t.Errorf("%s: FitTemperature succeeded, want ErrInsufficientCalibrationData", name)
		} else if insuf, ok := err.(*ErrInsufficientCalibrationData); !ok || insuf.Samples != 50 || insuf.Incorrect != 10 {
			t.Errorf("%s: error = %v (%T), want *ErrInsufficientCalibrationData{Samples: 50, Incorrect: 10, ...}", name, err, err)
		}
	}
	// Loosened: the same set fits (its optimum is interior, established by
	// TestFitTemperatureKnownOptimum).
	if _, err := FitTemperature(samples, FitOptions{MinIncorrect: 1, MinSamples: 10}); err != nil {
		t.Errorf("loosened: FitTemperature: %v (want a fit)", err)
	}
}

// TestFitTemperatureRangeBound: a set that passes the sufficiency guard (50
// samples, 10 incorrect top-1) whose NLL minimum still sits outside the
// documented range — the correct-leading majority drags the optimum below
// minTemperature (≈0.047, verified by fine scan). Hitting the range bound is
// a failure, not a result: the fit must return the bound error, not a T at
// the floor.
func TestFitTemperatureRangeBound(t *testing.T) {
	samples := make([]TemperatureSample, 0, 50)
	for i := 0; i < 40; i++ {
		// Truth leads by 0.5: each sample's NLL still falls as T falls at the
		// range floor, pulling the optimum below minTemperature.
		samples = append(samples, TemperatureSample{Scores: []float64{0.5, 0}, Correct: 0})
	}
	for i := 0; i < 10; i++ {
		// Truth trails by 0.0001: a wrong top-1 (the set is not degenerate),
		// but its NLL is essentially flat across the whole range, so it cannot
		// drag the optimum back inside the range. Scores in ranking order, so
		// the truth sits at index 1.
		samples = append(samples, TemperatureSample{Scores: []float64{0.0001, 0}, Correct: 1})
	}
	T, err := FitTemperature(samples, FitOptions{})
	if err == nil {
		t.Fatalf("FitTemperature: T = %v, want the range-bound error (optimum below %.2f)", T, minTemperature)
	}
	if !errors.Is(err, ErrFitHitRangeBound) {
		t.Fatalf("error = %v, want ErrFitHitRangeBound", err)
	}
	if !strings.Contains(err.Error(), "fit hit range bound") {
		t.Fatalf("error %q does not name the failure", err)
	}
}

// TestFitTemperatureInvalid: the fit is a total function that refuses to
// pretend — bad input is an error, never a silent T.
func TestFitTemperatureInvalid(t *testing.T) {
	cases := map[string][]TemperatureSample{
		"no samples":           {},
		"no scores":            {{Scores: nil, Correct: 0}},
		"correct out of range": {{Scores: []float64{1, 0}, Correct: 2}},
		"correct negative":     {{Scores: []float64{1, 0}, Correct: -1}},
		"nan score":            {{Scores: []float64{1, math.NaN()}, Correct: 0}},
		"inf score":            {{Scores: []float64{1, math.Inf(1)}, Correct: 0}},
		"empty scores slice":   {{Scores: []float64{}, Correct: 0}},
	}
	for name, samples := range cases {
		if _, err := FitTemperature(samples, FitOptions{}); err == nil {
			t.Errorf("%s: FitTemperature succeeded, want an error", name)
		}
	}
	// A non-finite T is +Inf NLL, not a number the grid could chase.
	if nll := TemperatureNLL([]TemperatureSample{{Scores: []float64{1, 0}, Correct: 0}}, math.NaN()); math.IsInf(nll, -1) || !math.IsInf(nll, 1) {
		t.Errorf("NLL at NaN T = %v, want +Inf", nll)
	}
}

// TestTemperatureKeepsRankOrder is the top-1 invariant the calibration rests
// on: T is a common factor across the candidate set, so the ordering (and
// therefore the top-1) must be identical for every T, while the reported
// confidence is allowed — and meant — to move.
func TestTemperatureKeepsRankOrder(t *testing.T) {
	msgs, idx := miniIndex(t)
	observations := []Observation{
		{Text: textOf(msgs, "m01"), OCRConf: 0.94, SceneID: "scene.harbour"},
		{Text: "We sail for the Gull-King's rock on the rnorning tide.", OCRConf: 0.72, SceneID: "scene.harbour"},
		{Text: "Where to", OCRConf: 0.8, SceneID: "scene.harbour"},
		{Text: "gull", OCRConf: 0.5},
	}
	temps := []float64{minTemperature, 0.3, 1, 2, maxTemperature}
	moved := false
	for obsNum, obs := range observations {
		ref := idx.Rank(obs, DefaultRankOptions())
		if len(ref.Candidates) == 0 {
			t.Fatalf("observation %d: no candidates at all: %+v", obsNum, obs)
		}
		refIDs := candidateIDs(ref)
		refTop := ref.Candidates[0].Probability
		for _, T := range temps {
			o := DefaultRankOptions()
			o.Temperature = T
			r := idx.Rank(obs, o)
			if got := candidateIDs(r); len(got) != len(refIDs) || !equalStrings(got, refIDs) {
				t.Fatalf("observation %d T=%v: order changed: %v, want %v (temperature must not move the top-1)", obsNum, T, got, refIDs)
			}
			sum := 0.0
			for _, c := range r.Candidates {
				sum += c.Probability
			}
			if math.Abs(sum-1) > 1e-9 {
				t.Fatalf("observation %d T=%v: probabilities sum to %v, want 1", obsNum, T, sum)
			}
			if T == minTemperature && r.Candidates[0].Probability < refTop-1e-9 {
				// Sharper than the default: the winner must be at least as
				// certain, never less (softmax concentrates on the max as T falls).
				t.Errorf("observation %d: winner prob %.4f at T=%v < %.4f at default T — monotonicity broken", obsNum, r.Candidates[0].Probability, T, refTop)
			}
			if T == maxTemperature && r.Candidates[0].Probability > refTop+1e-9 {
				// Flatter than the default: the winner must be at most as certain.
				t.Errorf("observation %d: winner prob %.4f at T=%v > %.4f at default T — monotonicity broken", obsNum, r.Candidates[0].Probability, T, refTop)
			}
		}
		// T must actually move the confidence somewhere (a no-op temperature is
		// not a calibration): on a non-uniform candidate set the winner's
		// probability strictly increases as T shrinks.
		sharp := DefaultRankOptions()
		sharp.Temperature = minTemperature
		flat := DefaultRankOptions()
		flat.Temperature = maxTemperature
		ps := idx.Rank(obs, sharp).Candidates[0].Probability
		pf := idx.Rank(obs, flat).Candidates[0].Probability
		if ps > pf+1e-9 {
			moved = true
		}
	}
	if !moved {
		t.Fatal("temperature moved no probability on any observation — the calibration would be a no-op")
	}
}

// candidateIDs is the MessageID column of a ranking, for order comparison.
func candidateIDs(r Ranking) []string {
	out := make([]string, 0, len(r.Candidates))
	for _, c := range r.Candidates {
		out = append(out, c.MessageID)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
