package matcher

import (
	"sort"
	"strings"

	"github.com/dbirks/game-narrator/internal/games"
)

// Observation is one textbox reading handed to the matcher. It is a value, not a
// pointer into shared runtime state, because docs/spec/05 requires "immutable
// scene observations" — a ranking that could be changed underneath it by the
// capture loop would make a committed decision unexplainable after the fact.
type Observation struct {
	// Text is the observed OCR text, raw (un-normalized). It is truncated to a
	// documented bound before use; see maxTextRunes.
	Text string

	// OCRConf is the engine's mean confidence for the read, expected in [0,1]. It
	// feeds the visibility component only: it is not multiplied into the final
	// probability, because OCR confidences are not calibrated and docs/spec/05
	// warns against assuming model probabilities are. A NaN, a negative value or a
	// zero (i.e. an unset field) is read as "unknown" (0.5), never propagated.
	OCRConf float64

	// IsPrefix marks a typewriter prefix — text that is still being revealed and
	// is therefore legitimately shorter than the stored line. Without it a prefix
	// is scored as a complete observation and will (correctly) fail to match
	// anything, which is the safe direction: docs/spec/05 makes commitment on a
	// prefix an explicit, opt-in policy rather than a default.
	IsPrefix bool

	// SceneID is the currently believed scene; empty means unknown.
	SceneID string

	// RecentMessageIDs are recently confirmed messages, newest first. Only the
	// first MaxRecent are used (docs/spec/05 "Track previous N=10 confirmed
	// messages"). Duplicates are ignored, keeping the first position.
	RecentMessageIDs []string

	// TextKindHint is the classifier's guess at the class of the box (dialogue,
	// sign, item popup…). Empty means unknown.
	TextKindHint string

	// Edition restricts matching to one edition when a profile carries several.
	// Empty means unspecified, which scores as agreement.
	Edition string
}

// Candidate is one ranked message with the full evidence decomposition, so the
// developer UI and the calibration dataset can see why it ranked (docs/spec/05
// "Expose top N and constituent scores in dev mode").
type Candidate struct {
	MessageID string
	// Score is the raw log-linear sum. It is unbounded below and lives on a scale
	// where only differences matter; it is kept because a trace with only the
	// probability cannot be recalibrated later.
	Score float64
	// Probability is Score softmax-normalized across the returned candidate set,
	// so the list sums to 1 and the thresholds in RankOptions are comparable
	// across corpus sizes.
	Probability float64
	// Components holds one entry per key in ComponentOrder, each in [0,1].
	Components map[string]float64
}

// Ranking is the answer to one observation.
type Ranking struct {
	// Candidates is ordered best-first, length ≤ TopK. On an abstain it is still
	// populated: the decision layer may forward the shortlist to Clef, which is
	// the whole point of abstaining locally instead of guessing (docs/spec/05
	// "Fast LLM/VLM called only for unresolved OCR").
	Candidates []Candidate
	// Margin is the winner's probability minus the runner-up's (or minus zero
	// when there is a single candidate — there is no other hypothesis to beat,
	// which is strength rather than missing evidence).
	Margin float64
	// Reason is one of the Reason* codes.
	Reason string
	// Abstain says the local evidence is not sufficient to speak; no candidate
	// should be committed on this ranking without a second opinion.
	Abstain bool
}

// Rank retrieves and scores candidates for one observation. It never returns an
// error and never panics on any input: an unusable observation produces an
// abstaining Ranking, because the capture loop cannot do anything with a failure
// other than abstain either, and a returned error would just be an abstain with a
// second channel for getting it wrong (docs/spec/12 fuzzes this function for
// NaN/infinite values and for panics).
func (x *Index) Rank(obs Observation, opts RankOptions) Ranking {
	o := opts.sanitize()
	r := &ranker{idx: x, opts: o, counts: make([]int32, len(x.ids))}
	return r.run(obs)
}

// ranker holds the per-query scratch state. It exists so Rank allocates a small
// fixed set of buffers rather than one container per candidate per stage.
type ranker struct {
	idx     *Index
	opts    RankOptions
	obs     Observation
	weights Weights

	// counts is the trigram-accumulator, one entry per message, sized once per
	// query (40 KB for 10k messages). A map keyed by ordinal would cost more and
	// could not be traversed deterministically.
	counts  []int32
	touched []int32

	pool            map[int32]*candidate
	exactN          int
	prefixN         int
	prefixTruncated bool

	// triScratch is the reused per-candidate trigram key buffer. It is the reason a
	// ranking over 40 candidates does not allocate 40 key slices per query.
	triScratch []uint32

	obsKey     string
	obsRunes   []rune
	qKeys      []uint32
	qToks      []uint32
	visibility float64
}

type candidate struct {
	ord       int32
	exact     bool
	prefix    bool
	prefixCov float64
	edgePrior float64
	graph     bool
	fallback  bool
	raw       float64
	prob      float64
	comps     [componentCount]float64
}

func (r *ranker) run(obs Observation) Ranking {
	r.obs = obs
	r.weights = r.opts.Weights

	// Every piece of caller-supplied text is bounded here, once, rather than at
	// each use, so no later stage can be talked into O(input) work per candidate.
	r.obs.Text = truncateBounds(strings.TrimSpace(obs.Text), maxTextRunes)
	r.obs.SceneID = strings.TrimSpace(obs.SceneID)
	r.obs.TextKindHint = strings.TrimSpace(obs.TextKindHint)
	r.obs.Edition = strings.TrimSpace(obs.Edition)
	r.obs.OCRConf = visibilityOf(obs.OCRConf)
	r.visibility = r.obs.OCRConf

	if r.obs.Text == "" || nonSpaceLen(r.obs.Text) == 0 {
		return Ranking{Reason: ReasonAbstained, Abstain: true}
	}

	r.obsKey = r.idx.fold.Normalize(r.obs.Text)
	if r.obsKey == "" {
		// The whole observation was punctuation or an artifact the folder removed.
		return Ranking{Reason: ReasonAbstained, Abstain: true}
	}
	r.obsRunes = []rune(r.obsKey)
	r.qKeys = sortUniqueKeys(trigramKeys(r.obsRunes, make([]uint32, 0, 3*len(r.obsRunes))))
	r.qToks = tokenHashes(r.obsKey, nil)
	r.pool = make(map[int32]*candidate, r.opts.RetrievalK+r.opts.MaxRecent+8)

	r.retrieve()
	if len(r.pool) == 0 {
		return Ranking{Reason: ReasonAbstained, Abstain: true}
	}
	return r.decide()
}

// visibilityOf reads an OCR confidence into the unit range, mapping anything that
// is not a usable number to the neutral 0.5. A caller that has no confidence
// figure (a replay fixture, a human-entered transcript) passes 0 and must not be
// scored as "the OCR was certain the text is wrong".
func visibilityOf(conf float64) float64 {
	if conf == 0 || conf != conf { // 0 and NaN both mean "not reported"
		return 0.5
	}
	return unit(conf)
}

// retrieve executes candidate generation in the order docs/spec/05 "Local
// candidate retrieval" fixes: exact match, unique full prefix, trigram top-K, then
// graph-neighbor expansion, with a global fallback used only when nothing else
// produced a candidate.
func (r *ranker) retrieve() {
	// (1) Exact normalized match. Ordinals are stored ascending, so the first
	// candidate also wins any later tie-break for free.
	if ords, ok := r.idx.exact[r.obsKey]; ok {
		r.exactN = len(ords)
		for _, o := range ords {
			r.add(o).exact = true
		}
	}

	// (2) Full prefix, only when the observation is a typewriter prefix. This is
	// the retrieval stage that makes predictive speech possible at all, and it is
	// deliberately gated on the caller saying the text is still revealing: the
	// same lookup on a complete line would nominate every longer line that happens
	// to contain it.
	if r.obs.IsPrefix {
		ords, _, truncated := r.idx.prefixRange(r.obsKey, r.opts.MaxPrefixScan)
		r.prefixTruncated = truncated
		for _, o := range ords {
			// Byte prefix is sufficient and exact here: both sides are valid UTF-8
			// after normalization, so a byte-level prefix of a valid string cannot
			// cut a rune in half.
			if !strings.HasPrefix(r.idx.keys[o], r.obsKey) {
				continue
			}
			r.prefixN++
			c := r.add(o)
			c.prefix = true
			c.prefixCov = unit(float64(len(r.obsRunes)) / float64(maxInt(int(r.idx.keyLens[o]), 1)))
		}
	}

	// (3) Trigram top-K.
	r.trigramRetrieval()

	// (4) Graph-neighbor expansion. Always included, even when trigram retrieval
	// is empty, because the successor of the last confirmed line is the single
	// most useful thing to show a decision model when the screen is unreadable.
	r.expandGraph()

	// (5) Global fallback — only if nothing else produced anything.
	if len(r.pool) == 0 {
		r.fallbackScan()
	}
}

// trigramRetrieval scores candidates by shared trigram count under two bounds: a
// total posting-visit budget and a per-trigram popularity cut-off. The popularity
// cut-off is the important one: on a real script a trigram like " th" appears in a
// large fraction of messages, and letting it through would make the work budget
// meaningless (the budget would be consumed by the least informative evidence).
func (r *ranker) trigramRetrieval() {
	if len(r.qKeys) == 0 || len(r.idx.postings) == 0 {
		return
	}
	// Trigrams in more than a quarter of the corpus (never fewer than 64 messages,
	// so small profiles are not over-pruned) carry no discriminating power.
	popular := maxInt(len(r.idx.ids)/4, 64)
	budget := r.opts.MaxPostingWork
	for _, h := range r.qKeys { // sorted ascending: the budget is spent identically each run
		list, ok := r.idx.postings[h]
		if !ok {
			continue
		}
		if len(list) > popular {
			continue
		}
		if len(list) > budget {
			list = list[:budget]
		}
		budget -= len(list)
		for _, o := range list {
			r.counts[o]++
			if r.counts[o] == 1 {
				r.touched = append(r.touched, o)
			}
		}
		if budget <= 0 {
			break
		}
	}
	if len(r.touched) == 0 {
		return
	}
	// Bounded top-K selection: keep the best RetrievalK by shared count, ties to
	// the lower ordinal. Insertion into a fixed-size slice costs O(K) per accepted
	// candidate and never sorts the whole touched set.
	best := make([]topEntry, 0, r.opts.RetrievalK)
	for _, o := range r.touched {
		n := int(r.counts[o])
		if len(best) == r.opts.RetrievalK {
			last := best[len(best)-1]
			if n < last.count || (n == last.count && o > last.ord) {
				continue
			}
			best = best[:len(best)-1]
		}
		pos := sort.Search(len(best), func(i int) bool {
			if best[i].count != n {
				return best[i].count < n
			}
			return best[i].ord >= o
		})
		best = append(best, topEntry{})
		copy(best[pos+1:], best[pos:])
		best[pos] = topEntry{ord: o, count: n}
	}
	// The shared count is not carried forward: scoring recomputes it exactly,
	// because a candidate that arrives by prefix or along a graph edge was never
	// seen by this stage and its evidence must not depend on the route it came by.
	for _, e := range best {
		r.add(e.ord)
	}
}

type topEntry struct {
	ord   int32
	count int
}

// expandGraph turns recently confirmed messages into candidate successors and
// records the edge prior that will score them.
func (r *ranker) expandGraph() {
	idx := r.idx
	seenRecent := make(map[int32]bool, len(r.obs.RecentMessageIDs))
	position := 0
	for _, id := range r.obs.RecentMessageIDs {
		if position >= r.opts.MaxRecent {
			break
		}
		o, ok := idx.ords[id]
		if !ok || seenRecent[o] {
			continue
		}
		seenRecent[o] = true
		decay := recencyDecay(position)
		idx.neighbors(o, func(to int32, kind games.EdgeType, conf float64) {
			prior := unit(baseEdgePrior(kind) * conf * decay)
			if prior <= 0 {
				return
			}
			c := r.add(to)
			c.graph = true
			if prior > c.edgePrior {
				c.edgePrior = prior
			}
		})
		position++
	}
}

// recencyDecay discounts graph evidence by how far back the message is: the line
// before this one predicts the next far better than the tenth (docs/spec/05 "Track
// previous N=10 confirmed messages"). 0.6 per step keeps ten messages back at
// about 0.6% — still reachable, never dominant.
func recencyDecay(position int) float64 {
	if position <= 0 {
		return 1
	}
	if position > 20 {
		return 0
	}
	d, w := 1.0, 0.6
	for i := 0; i < position; i++ {
		d *= w
	}
	return d
}

// fallbackScan nominates a bounded set of messages when no retrieval stage found
// anything, so the caller can report *something* to a human or to Clef instead of
// an empty shortlist. Selection is by closeness in length, which is the cheapest
// available proxy for "could this be the line", and the scan itself is bounded by
// the candidate cap rather than by the corpus size.
func (r *ranker) fallbackScan() {
	want := maxInt(r.opts.TopK, 1)
	best := make([]topEntry, 0, want)
	for o := range r.idx.keys {
		diff := len(r.obsRunes) - int(r.idx.keyLens[o])
		if diff < 0 {
			diff = -diff
		}
		if len(best) == want && diff >= best[want-1].count {
			continue // cannot displace the weakest kept candidate
		}
		pos := sort.Search(len(best), func(i int) bool { return best[i].count > diff })
		best = append(best, topEntry{})
		copy(best[pos+1:], best[pos:])
		best[pos] = topEntry{ord: int32(o), count: diff}
		if len(best) > want {
			best = best[:want]
		}
	}
	for _, e := range best {
		c := r.add(e.ord)
		c.fallback = true
	}
}

// add fetches or creates the pool entry for one ordinal. Deduplication across the
// retrieval stages happens here, which is why exact + graph + trigram hits on the
// same message produce one candidate carrying all three signals.
func (r *ranker) add(o int32) *candidate {
	if c, ok := r.pool[o]; ok {
		return c
	}
	c := &candidate{ord: o}
	r.pool[o] = c
	return c
}
