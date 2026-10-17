package matcher

import (
	"math"

	"github.com/dbirks/game-narrator/internal/games/textnorm"
)

// IndexOptions configures Build. The zero value is what a profile should use:
// retrieval keys are OCR-folded and punctuation-stripped, which is what
// internal/games/textnorm documents for the matching side ("Retrieval turns it
// on for fuzzy candidate matching; import leaves it off"). Both sides of every
// comparison go through the same folder, so the flag cannot half-fold a
// comparison — the failure docs/spec/12 warns about.
type IndexOptions struct {
	// KeepPunctuation stores retrieval keys with punctuation intact. Off by
	// default because the OCR engine drops terminal periods and decorative
	// glyphs far more often than it reads them correctly, and because
	// docs/spec/12 lists "punctuation-only difference" as a case that must match
	// rather than abstain. Turn it on only for a game whose punctuation is
	// semantically rendered on screen and reliably read.
	KeepPunctuation bool

	// NoOCRFold disables the rn/0/1/l glyph-confusion fold. Kept as an opt-out
	// rather than an opt-in so the default is the safe one; a profile whose text
	// is machine-supplied (a replay fixture with exact text) may set it.
	NoOCRFold bool

	// PlayerNames are substituted with textnorm's placeholder before indexing, so
	// a line containing the player's name is findable from the OCR text that
	// still shows the placeholder (or the other way round). Empty means no
	// substitution, the honest default per textnorm's own documentation.
	PlayerNames []string

	// Edition names the edition this index represents (docs/spec/04 editions).
	// Messages carry no edition column of their own, so this profile-level label
	// is what the edition component compares against, with an optional
	// per-message override in Message.Flags under the key "edition". Empty makes
	// the edition component neutral (1) for every candidate, which is correct for
	// a single-edition profile and wrong to invent.
	Edition string
}

// foldOptions renders these options as textnorm options.
func (o IndexOptions) foldOptions() textnorm.Options {
	return textnorm.Options{
		StripPunctuation: !o.KeepPunctuation,
		OCRFold:          !o.NoOCRFold,
		PlayerNames:      o.PlayerNames,
	}
}

// Weights are the multipliers of the log-linear combination
// (docs/spec/05 "Local candidate retrieval": "a tunable log-linear scorer").
// Their absolute scale matters, because the sum is exponentiated into a
// probability; their ratios matter more, because softmax cancels a common
// factor across candidates.
//
// Two ratios are deliberate and asserted by tests rather than left to taste:
//
//   - Text outweighs edge prior enough that a full-text match on a remote node
//     beats the graph successor with no text support at all. docs/spec/04: "Never
//     force the most likely graph edge over strong contrary OCR text." With
//     W.Text > W.EdgePrior, the largest reachable edge term cannot cover the gap
//     between a text score of 1 and one of 0.
//   - Visibility, text kind, speaker context, edition and source reliability are
//     all small. They are context that should tilt a close call, not evidence
//     that should decide a line on its own — the calibration data docs/spec/17
//     wants does not exist yet, so the conservative coefficients are the honest
//     ones.
type Weights struct {
	Text              float64
	PrefixCoverage    float64
	Scene             float64
	EdgePrior         float64
	Visibility        float64
	TextKind          float64
	SpeakerContext    float64
	Edition           float64
	SourceReliability float64
}

// DefaultWeights are the coefficients described above. They are a starting point
// for per-game calibration, not a tuned result, and nothing in this package
// claims otherwise.
func DefaultWeights() Weights {
	return Weights{
		Text:              1.6,
		PrefixCoverage:    0.5,
		Scene:             0.35,
		EdgePrior:         0.55,
		Visibility:        0.25,
		TextKind:          0.2,
		SpeakerContext:    0.1,
		Edition:           0.15,
		SourceReliability: 0.1,
	}
}

// sanitize clamps every coefficient into [0, maxWeight] and replaces a
// non-finite or wholly-zero set with the defaults. It cannot return an error
// because Rank has none: a ranking call that received a NaN weight must still
// produce finite numbers (docs/spec/12 "candidate ranking's NaN/infinite
// values"), so a bad value is repaired and the caller keeps a deterministic
// result rather than a crash.
func (w Weights) sanitize() Weights {
	fields := []*float64{
		&w.Text, &w.PrefixCoverage, &w.Scene, &w.EdgePrior, &w.Visibility,
		&w.TextKind, &w.SpeakerContext, &w.Edition, &w.SourceReliability,
	}
	for _, f := range fields {
		*f = clamp(*f, 0, maxWeight)
	}
	if w.Text+w.PrefixCoverage+w.Scene+w.EdgePrior+w.Visibility+w.TextKind+
		w.SpeakerContext+w.Edition+w.SourceReliability <= 0 {
		return DefaultWeights()
	}
	return w
}

const maxWeight = 8.0

// RankOptions bounds one ranking call. As with Weights, an unset field means
// "use the default", never "use zero": a caller constructing RankOptions{TopK: 3}
// must not silently disable the abstain thresholds, because thresholds that
// vanish under a half-filled struct are how a runtime starts speaking a wrong
// line.
type RankOptions struct {
	// TopK caps the returned candidates. Default 10, max 50: the decision layer
	// sends at most a bounded candidate list to Clef (docs/spec/05 "Limit
	// candidate options to supported request cap"), so more is waste.
	TopK int

	// Weights overrides the log-linear coefficients. The zero value means
	// DefaultWeights.
	Weights Weights

	// MinTextLenForPredict is the smallest number of non-space characters an
	// observation must show before a prefix match may be reported as
	// PREDICTIVE_PREFIX at all. docs/spec/05 "Use holdoff for snippets like
	// 'Yes.' or 'No.'": three revealed characters are not evidence, they are the
	// first frame of a typewriter animation.
	MinTextLenForPredict int

	// MinProbability is the acceptance floor for the winner's softmax
	// probability; below it the ranking abstains.
	MinProbability float64

	// MinMargin is the required gap between the winner and the runner-up
	// (docs/spec/17 "with a margin against runner-up").
	MinMargin float64

	// MinSpecificChars is the smallest number of non-space characters that can
	// carry a match on text alone. Below it the ranking abstains unless graph or
	// scene evidence singles out one candidate.
	MinSpecificChars int

	// ExactAmbiguityMax is how many messages may share the observation's exact
	// key before the observation counts as low-specificity (docs/spec/05
	// "Short strings and overlapping prefixes require more evidence"). "Yes."
	// matching four NPCs is the case this exists for.
	ExactAmbiguityMax int

	// GraphEvidenceFloor is the smallest edge_prior value treated as real graph
	// support, i.e. enough to release a short-phrase abstain.
	GraphEvidenceFloor float64

	// RetrievalK is how many text-retrieval candidates are kept for scoring
	// before graph expansion. Larger values cost more per frame; the default
	// comfortably covers the observed distribution of OCR near-miss rivals.
	RetrievalK int

	// MaxPostingWork caps how many posting-list entries one query may touch.
	// This is the hard bound on retrieval cost on a pathological corpus where a
	// trigram appears in every message.
	MaxPostingWork int

	// MaxPrefixScan caps how many index keys one prefix lookup may inspect.
	MaxPrefixScan int

	// MaxRecent bounds the recent-message list; docs/spec/05 "Track previous
	// N=10 confirmed messages (configurable)". Zero means unset (= 10); to
	// evaluate an observation with no graph context at all, set
	// Weights.EdgePrior = 0, which is the supported way to switch the axis off.
	MaxRecent int

	// Temperature is the calibration scalar T applied to the raw log-linear
	// scores before the softmax (calibrate.go). It rescales the probabilities
	// without touching the ordering: a top-1 that is correct stays top-1 for
	// every T, and only the reported confidence moves. Zero means unset (= the
	// default, DefaultTemperature, calibration revision
	// CalibrationRevision); a per-profile override — for instance a T fitted on
	// a set with labelled errors (FitTemperature) — is sanitised into
	// [minTemperature, maxTemperature].
	Temperature float64
}

// DefaultRankOptions returns the conservative defaults described above.
func DefaultRankOptions() RankOptions {
	return RankOptions{
		TopK:                 10,
		Weights:              DefaultWeights(),
		MinTextLenForPredict: 12,
		MinProbability:       0.35,
		MinMargin:            0.12,
		MinSpecificChars:     5,
		ExactAmbiguityMax:    2,
		GraphEvidenceFloor:   0.15,
		RetrievalK:           40,
		MaxPostingWork:       40000,
		MaxPrefixScan:        256,
		MaxRecent:            10,
		Temperature:          DefaultTemperature,
	}
}

// sanitize fills unset or unsafe fields. Deterministic by construction: no field is
// derived from map iteration or randomness.
//
// Zero is "unset" for every int here, so the smallest reachable value of
// ExactAmbiguityMax is 1 ("abstain whenever two messages share the key") rather
// than 0 ("never abstain on multiplicity"). That is a deliberate restriction: an
// observation that matches the whole script identically must not be answerable on
// text alone, and there is no profile for which that should be allowed.
func (o RankOptions) sanitize() RankOptions {
	d := DefaultRankOptions()
	o.TopK = pickInt(o.TopK, d.TopK, 1, 50)
	o.MinTextLenForPredict = pickInt(o.MinTextLenForPredict, d.MinTextLenForPredict, 1, math.MaxInt16)
	o.MinSpecificChars = pickInt(o.MinSpecificChars, d.MinSpecificChars, 1, math.MaxInt16)
	o.ExactAmbiguityMax = pickInt(o.ExactAmbiguityMax, d.ExactAmbiguityMax, 0, math.MaxInt16)
	o.RetrievalK = pickInt(o.RetrievalK, d.RetrievalK, 1, 500)
	o.MaxPostingWork = pickInt(o.MaxPostingWork, d.MaxPostingWork, 1, 4_000_000)
	o.MaxPrefixScan = pickInt(o.MaxPrefixScan, d.MaxPrefixScan, 1, 100_000)
	o.MaxRecent = pickInt(o.MaxRecent, d.MaxRecent, 0, 100)
	o.MinProbability = pickFloat(o.MinProbability, d.MinProbability, 0, 1)
	o.MinMargin = pickFloat(o.MinMargin, d.MinMargin, 0, 1)
	o.GraphEvidenceFloor = pickFloat(o.GraphEvidenceFloor, d.GraphEvidenceFloor, 0, 1)
	o.Temperature = pickFloat(o.Temperature, d.Temperature, minTemperature, maxTemperature)
	o.Weights = o.Weights.sanitize()
	return o
}

// pickInt returns v when it is a usable override, else def. "Usable" means set
// (non-zero) and inside [lo, hi]; a negative or oversized value is a caller bug
// and is repaired rather than propagated, because a truncated-at-zero candidate
// budget would look like a legitimate empty ranking.
func pickInt(v, def, lo, hi int) int {
	if v == 0 {
		return def
	}
	return clampInt(v, lo, hi)
}

func pickFloat(v, def, lo, hi float64) float64 {
	if v == 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return def
	}
	return clamp(v, lo, hi)
}
