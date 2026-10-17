package matcher

import (
	"math"
	"sort"
	"strings"

	"github.com/dbirks/game-narrator/internal/games"
)

// Component blends inside the `text` score. They are stated here rather than
// buried in the expression because docs/spec/05 asks for "weighted
// character/token similarities" and a reader of a trace must be able to predict
// the number from the parts: character-trigram Dice 0.5, token-set Dice 0.3,
// bounded Levenshtein similarity 0.2.
//
// Levenshtein gets the smallest share on purpose. On a 30-character line one
// misread glyph is a similarity of 0.97 and a wholly different line starting with
// the same words is 0.6 — it barely separates the cases that matter, and it is the
// most expensive of the three. Trigram Dice is the workhorse; the token set keeps
// word-order-preserving anagrams from scoring like near-misses; Levenshtein breaks
// ties among strings that share most of their characters.
const (
	trigramWeight     = 0.5
	tokenWeight       = 0.3
	levenshteinWeight = 0.2
)

// componentEpsilon is the floor under every component inside the logarithm. It is
// not a smoothing courtesy: a component of exactly 0 is very common (no graph
// edge, wrong scene), and log(0) is -Inf, which would turn one missing signal into
// an unanswerable ranking. With epsilon at 1e-3 the *difference* between "no
// evidence" and "certainty" is capped at 6.9 per unit weight, which is what makes
// the weight-ratio argument in Weights' documentation arithmetic rather than hope.
const componentEpsilon = 1e-3

// neutralScore is the documented placeholder value for components this milestone
// cannot honestly compute: speaker_context (no speaker hypothesis is plumbed in
// yet) and source_reliability (no source provenance score exists in the schema).
// Half is neutral in the sense that it contributes the same constant to every
// candidate and therefore cancels in the softmax; it is not "half as true".
const neutralScore = 0.5

// scoreOne fills c.comps in ComponentOrder. Every returned value is in [0,1].
func (r *ranker) scoreOne(c *candidate) {
	idx := r.idx
	key := idx.keys[c.ord]

	// text — character and token similarity over the OCR-folded strings.
	// The shared-trigram count is recomputed exactly here rather than reused from
	// retrieval: a candidate that arrived by prefix or along a graph edge was never
	// scored by the trigram stage, and treating its evidence as zero because of the
	// route it entered would be an artifact of the algorithm rather than of the
	// text. The merge is over two sorted key lists and reuses one scratch buffer.
	// key is already bounded by maxTextRunes at build time (see IndexOptions), which
	// is what makes this set identical to the one the inverted index was populated
	// from: intersecting a differently-sized set against a stored trigram count
	// would quietly depress the score of every long line.
	obsTris := len(r.qKeys)
	candKeys := sortUniqueKeys(trigramKeys([]rune(key), r.triScratch))
	r.triScratch = candKeys[:0]
	shared := intersectCount(r.qKeys, candKeys)
	diceScore := dice(shared, obsTris, int(idx.triCount[c.ord]))
	candToks := idx.tokensOf(c.ord)
	tokens := dice(intersectCount(r.qToks, candToks), len(r.qToks), len(candToks))
	lev := levenshteinSimilarity(r.obsRunes, levRunes(key))
	text := unit(trigramWeight*diceScore + tokenWeight*tokens + levenshteinWeight*lev)

	prefix := 0.0
	if c.prefix {
		prefix = unit(c.prefixCov)
	}

	scene := r.sceneScore(c)
	kindScore := kindScore(r.obs.TextKindHint, idx.kinds[c.ord])
	edition := editionScore(r.obs.Edition, idx.editions[c.ord])

	c.comps[compIndex(ComponentText)] = text
	c.comps[compIndex(ComponentPrefixCoverage)] = prefix
	c.comps[compIndex(ComponentScene)] = scene
	c.comps[compIndex(ComponentEdgePrior)] = unit(c.edgePrior)
	c.comps[compIndex(ComponentVisibility)] = r.visibility
	c.comps[compIndex(ComponentTextKind)] = kindScore
	c.comps[compIndex(ComponentSpeakerContext)] = neutralScore
	c.comps[compIndex(ComponentEdition)] = edition
	c.comps[compIndex(ComponentSourceReliability)] = neutralScore

	c.raw = r.combine(c.comps)
}

// combine is the log-linear scorer: Σ wᵢ·log(ε + cᵢ). It is log-linear because a
// missing piece of evidence should multiply the odds down rather than subtract a
// fixed amount — the difference between "no scene information" (0.5) and "the
// wrong scene" (0) should scale with how much evidence there is overall, which is
// what a sum of logs does and a weighted sum of scores does not (docs/spec/05
// "Combined score through a tunable log-linear scorer").
func (r *ranker) combine(comps [componentCount]float64) float64 {
	w := r.weights
	sum := 0.0
	add := func(weight, value float64) {
		if weight == 0 {
			return
		}
		// unit() has already run, but the sanitizer is applied again here so this
		// function is total on its own: no caller has to trust that the component
		// array was built by scoreOne (docs/spec/12's NaN fuzz target).
		sum += weight * math.Log(componentEpsilon+unit(value))
	}
	add(w.Text, comps[compIndex(ComponentText)])
	add(w.PrefixCoverage, comps[compIndex(ComponentPrefixCoverage)])
	add(w.Scene, comps[compIndex(ComponentScene)])
	add(w.EdgePrior, comps[compIndex(ComponentEdgePrior)])
	add(w.Visibility, comps[compIndex(ComponentVisibility)])
	add(w.TextKind, comps[compIndex(ComponentTextKind)])
	add(w.SpeakerContext, comps[compIndex(ComponentSpeakerContext)])
	add(w.Edition, comps[compIndex(ComponentEdition)])
	add(w.SourceReliability, comps[compIndex(ComponentSourceReliability)])
	if math.IsNaN(sum) || math.IsInf(sum, 0) {
		// Unreachable with the sanitizers above; kept because a future component
		// must not be able to smuggle a NaN into a probability.
		return math.Log(componentEpsilon)
	}
	return sum
}

// sceneScore: 1 same scene, 0.5 when either side is unknown, 0 when they differ
// (docs/spec/05's component list fixes these three cases). An observation with no
// scene belief must not be penalised, so unknown is neutral rather than 0.
func (r *ranker) sceneScore(c *candidate) float64 {
	want, got := r.obs.SceneID, r.idx.scenes[c.ord]
	if want == "" || got == "" {
		return 0.5
	}
	if want == got {
		return 1
	}
	return 0
}

// kindScore compares the classifier's hint with the stored message's class.
// Unknown hint is neutral; a mismatch is 0 rather than negative, because a sign
// misclassified as dialogue is weak evidence of the wrong thing, not proof of a
// different line.
func kindScore(hint, got string) float64 {
	if hint == "" || got == "" {
		return 0.5
	}
	if !games.TextKind(hint).Valid() || !games.TextKind(got).Valid() {
		return 0.5
	}
	if strings.EqualFold(hint, got) {
		return 1
	}
	return 0
}

// editionScore: 1 when the editions agree or either is unspecified, 0 on a real
// conflict. See IndexOptions.Edition for where a message's label comes from.
func editionScore(want, got string) float64 {
	if want == "" || got == "" {
		return 1
	}
	if strings.EqualFold(want, got) {
		return 1
	}
	return 0
}

// levRunes converts a stored key to runes for the edit-distance term, bounded by
// the term's own window so a long line costs nothing extra.
func levRunes(key string) []rune {
	return []rune(truncateBounds(key, levMaxRunes))
}

func compIndex(component string) int {
	for i, name := range ComponentOrder {
		if name == component {
			return i
		}
	}
	return 0
}

// decide scores, orders, normalizes, and applies the acceptance policy.
func (r *ranker) decide() Ranking {
	list := make([]*candidate, 0, len(r.pool))
	for _, c := range r.pool {
		r.scoreOne(c)
		list = append(list, c)
	}
	// Deterministic order: best raw score, then the lower message ordinal, then the
	// ID (docs/spec/12 "rank ordering deterministic under same inputs/settings").
	// Ordinals are unique, so the ID term never fires in practice; it is written
	// because the property test asserts the whole comparator, not an approximation.
	sort.Slice(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if a.raw != b.raw {
			return a.raw > b.raw
		}
		if a.ord != b.ord {
			return a.ord < b.ord
		}
		return r.idx.ids[a.ord] < r.idx.ids[b.ord]
	})
	if len(list) > r.opts.TopK {
		list = list[:r.opts.TopK]
	}
	r.softmax(list)

	out := make([]Candidate, 0, len(list))
	for _, c := range list {
		comps := make(map[string]float64, len(ComponentOrder))
		for i, name := range ComponentOrder {
			comps[name] = c.comps[i]
		}
		out = append(out, Candidate{
			MessageID:   r.idx.ids[c.ord],
			Score:       c.raw,
			Probability: c.prob,
			Components:  comps,
		})
	}

	top := list[0]
	// The margin is reported on the revision-1 (T = 1) probability scale — the
	// scale the acceptance thresholds and the session's MinPredictiveMargin
	// were calibrated on (ADR-0035 "Calibration") — so a calibration change
	// moves the reported probabilities without moving this gate.
	margin := top.gateProb
	if len(list) > 1 {
		margin = top.gateProb - list[1].gateProb
	}
	margin = unit(margin)

	reason, abstain := r.policy(top, list[1:])
	return Ranking{
		Candidates: out,
		Margin:     margin,
		Reason:     reason,
		Abstain:    abstain,
	}
}

// softmax converts log-linear scores into probabilities over the returned set,
// in two passes (calibrate.go):
//
//   - c.prob is the calibrated temperature (RankOptions.Temperature, revision
//     CalibrationRevision): p(i) ∝ exp(raw(i)/T). This is what the commit
//     policy, the Clef escalation thresholds and the developer UI consume
//     (Candidate.Probability). The division is a common factor across the set,
//     so the ordering is unchanged for every T — only the spread moves.
//   - c.gateProb is the same softmax at T = 1 (revision 1): the scale on which
//     the acceptance thresholds (MinProbability, MinMargin) and the session's
//     MinPredictiveMargin were calibrated. The acceptance gate keeps operating
//     on that scale until the thresholds are re-tuned on labeled data
//     (docs/spec/17; ADR-0035 "Calibration"), so adopting a new calibration
//     revision can never silently move an accept/abstain decision — the
//     calibrated probabilities change what the decision layer sees, not what
//     the local policy commits on.
//
// The set is deliberately the *returned* candidates rather than everything that
// was retrieved: docs/spec/12's property test asserts the probabilities sum to 1,
// and a top-K cut-off applied after normalization would leave a sum that silently
// depends on how much mass was dropped. The cost is that the acceptance thresholds
// are calibrated for a candidate list of this size — stated here because
// DefaultRankOptions.TopK is the only honest companion to DefaultRankOptions'
// MinProbability.
func (r *ranker) softmax(list []*candidate) {
	r.softmaxT(list, r.opts.Temperature, func(c *candidate) *float64 { return &c.prob })
	r.softmaxT(list, 1.0, func(c *candidate) *float64 { return &c.gateProb })
}

// softmaxT is one softmax pass at temperature t into the destination field.
func (r *ranker) softmaxT(list []*candidate, t float64, dst func(*candidate) *float64) {
	if t <= 0 || math.IsNaN(t) || math.IsInf(t, 0) {
		// sanitize() guarantees a valid T; this is defense in depth for a future
		// caller that builds a ranker directly (docs/spec/12's NaN fuzz target).
		t = DefaultTemperature
	}
	maxRaw := math.Inf(-1)
	for _, c := range list {
		if c.raw > maxRaw {
			maxRaw = c.raw
		}
	}
	if math.IsNaN(maxRaw) || math.IsInf(maxRaw, -1) {
		maxRaw = 0
	}
	sum := 0.0
	for _, c := range list {
		p := math.Exp(c.raw/t - maxRaw/t)
		if math.IsNaN(p) {
			p = 0
		}
		*dst(c) = p
		sum += p
	}
	if sum <= 0 || math.IsInf(sum, 1) || math.IsNaN(sum) {
		even := 1 / float64(len(list))
		for _, c := range list {
			*dst(c) = even
		}
		return
	}
	for _, c := range list {
		*dst(c) = unit(*dst(c) / sum)
	}
}

// fallbackTextFloor is the minimum text score that lets a candidate which arrived
// only through the global fallback be accepted. The fallback picks by string
// length, which is not evidence of anything: on a one-message profile a wholly
// unrelated observation would otherwise come back as a 1.00 probability match,
// because a softmax over a single candidate is total by definition. Requiring
// corroboration from the text itself is what keeps "we had nothing" from looking
// like "we are sure".
const fallbackTextFloor = 0.5

// policy turns scores into speak/don't-speak. Abstaining is cheap and being wrong
// is not: the runtime either stays silent or asks Clef, and both are recoverable,
// whereas speaking a line the player never saw is the failure mode docs/spec/05
// puts at the top of the list.
func (r *ranker) policy(top *candidate, rest []*candidate) (string, bool) {
	if top.fallback && !top.exact && !top.prefix && top.edgePrior <= 0 &&
		top.comps[compIndex(ComponentText)] < fallbackTextFloor {
		return ReasonAbstained, true
	}
	// The acceptance gate runs on the revision-1 (T = 1) probability scale —
	// the same gateProb the reported Margin is computed from (ADR-0035
	// "Calibration") — so adopting a calibration revision moves the reported
	// probabilities without moving the accept/abstain decision.
	if top.gateProb < r.opts.MinProbability {
		return ReasonAbstained, true
	}
	margin := top.gateProb
	if len(rest) > 0 {
		margin = top.gateProb - rest[0].gateProb
	}
	if margin < r.opts.MinMargin {
		return ReasonAbstained, true
	}

	// Low specificity: a three-character fragment, or a phrase the script contains
	// four times, cannot carry a match on its own. docs/spec/05 "Short strings and
	// overlapping prefixes require more evidence" and "Use holdoff for snippets
	// like 'Yes.'".
	// A revealing (partial) observation also has to clear the predictive length
	// floor before text alone may answer at all: half a line is a prefix of many
	// lines, and accepting the shortest completion of "Beyond" on six characters is
	// how a narrator starts speaking a sentence the player has not seen.
	revealedEnough := !r.obs.IsPrefix || nonSpaceLen(r.obsKey) >= r.opts.MinTextLenForPredict
	lowSpecificity := nonSpaceLen(r.obsKey) < r.opts.MinSpecificChars ||
		!revealedEnough ||
		r.exactN > r.opts.ExactAmbiguityMax ||
		(r.obs.IsPrefix && r.prefixN > r.opts.ExactAmbiguityMax)
	graphDecides := top.edgePrior >= r.opts.GraphEvidenceFloor
	runnerUpTextClose := len(rest) > 0 && top.comps[compIndex(ComponentText)]-rest[0].comps[compIndex(ComponentText)] < 0.05
	sceneDecides := top.comps[compIndex(ComponentScene)] >= 1 &&
		(len(rest) == 0 || rest[0].comps[compIndex(ComponentScene)] < 1)

	if lowSpecificity && !(graphDecides || sceneDecides) {
		return ReasonAbstained, true
	}

	switch {
	case r.obs.IsPrefix && r.prefixN == 1 && !r.prefixTruncated &&
		top.prefix && nonSpaceLen(r.obsKey) >= r.opts.MinTextLenForPredict:
		return ReasonPredictivePrefix, false
	case graphDecides && (r.exactN > 1 || runnerUpTextClose || !top.exact):
		return ReasonGraphNeighbour, false
	case top.exact:
		return ReasonExact, false
	default:
		return ReasonFuzzy, false
	}
}
