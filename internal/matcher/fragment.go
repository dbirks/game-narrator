package matcher

// Fragment-aware scoring. The board evidence (2026-10-21, docs/adr/0151): the
// OCR read a real dialogue box but returned only a fragment of it — one line,
// sometimes cut mid-word — and the matcher abstained on every one, because the
// text component scores a fragment on the similarity metrics only:
//
//   - trigram Dice is symmetric, 2|S|/(|Q|+|C|): a 30-character query that is
//     an exact substring of a 105-character message shares all ~27 of its
//     trigrams yet scores 2·27/(27+102) ≈ 0.42;
//   - the banded Levenshtein is exactly 0: the length gap (75 runes) is far
//     past the band of 12, so the term reports "different line" for what is a
//     verbatim window of it;
//   - token Dice punishes the length ratio the same way.
//
// The text component lands at 0.2–0.35, the raw log-linear sum at -9 to -10,
// and the acceptance gate abstains (margins measured 0.11–0.20 against the
// MinMargin = 0.12 floor). The evidence the fragment carries is *containment*,
// not similarity: "every character of what was read is in this line, in this
// order, in one place".
//
// The fix adds one more reading of the shared-trigram evidence: the fraction
// of the observation's interior trigrams that lie in a single contiguous
// window of the candidate (fragmentWindowCoverage), applied as a floor under
// the text component — never a ceiling — and only when the candidate is
// strictly longer than the observation. A full-box read (observation as long
// as the line) therefore scores exactly as before, and a contained unique
// fragment scores as the containment evidence it is, without disturbing any
// similarity case.
//
// The window is what makes this containment rather than a bag of words. The
// read's interior trigrams split into word-internal ones (which name words)
// and cross-word ones (which name word *order*). A set over the whole line
// ignores the second kind: a line that merely contains the read's words, in a
// different order, scores near 1.0 and is counted as a covering line. Measured
// on the invented 200-message corpus, a 26-character read whose five words a
// different line all contains in another order set-covers 0.96 of it — the
// ambiguity count then abstained on a unique read, and the same floor could
// have lifted a wrong line's text score to near-certainty. A window of about
// the read's length only contains cross-word trigrams for words that are
// adjacent there, so order is enforced for free and the best window is also
// the evidence of where in the line the read sits.
//
// Uniqueness is a policy question, not a scoring one: the fraction only says
// "this line explains most of the reading, in order, in one place". When
// several lines explain it, none is the answer (the generic-phrase and
// shared-prefix cases), so the acceptance policy abstains on the count rather
// than on any per-candidate value the ranking would hide.

import (
	"math"
	"sort"
)

// fragmentCoverageFloor is the smallest fraction of the observation's interior
// trigrams a single window of a longer candidate must contain before its
// containment counts as fragment evidence at all.
//
// Measured on the invented 200-message corpus (docs/adr/0151), window
// coverage of the read's own message is:
//
//   - an exact 25-40 character window: 1.000;
//   - a leading "| " (stripped by the fold) and a one-character trailing junk
//     read: 0.964–0.989;
//   - one interior character misread: 0.929; two: 0.857;
//   - a junk-only reading ("a a"): 0.000.
//
// 0.9 sits inside the measured gap, the same way fuzzyTextFloor sits in its
// measured gap: high enough that a junk or half-corrupted reading cannot claim
// containment, low enough that the board's damaged reads still can. It must
// stay below the one-misread number: raising it toward 0.95 silently drops the
// damaged reads the board evidence shows are the common case.

const fragmentCoverageFloor = 0.9

// fragmentFloorScale scales the containment fraction before it floors the
// text component: a contained fragment scores at most 0.9 — far above the
// 0.2–0.35 its similarity metrics give it, below the ~1.0 of a candidate
// whose own text matches the reading.
const fragmentFloorScale = 0.9

// interiorTrigramKeys returns the sorted, duplicate-free hash keys of the
// string's interior character trigrams — every (r[i-2], r[i-1], r[i]) for
// i ≥ 2, without the boundary-pad trigrams trigramKeys adds at both ends.
//
// Interior is what containment means: the pads name the read's own edges, so
// they cannot be present in a line's interior (and a read at a line's start
// matches the line's *prefix* pads, which is the prefix stage's business, not
// containment's). A string of fewer than three runes has no interior trigrams
// and returns the empty slice: a two-character read cannot claim containment
// of anything, and the low-specificity gate is the honest answer for it.
//
// out is reused when it has capacity, on the same terms as trigramKeys:
// callers must not retain the result.
func interiorTrigramKeys(runes []rune, out []uint32) []uint32 {
	out = out[:0]
	for i := 2; i < len(runes); i++ {
		out = append(out, hashTrigram([3]rune{runes[i-2], runes[i-1], runes[i]}))
	}
	return sortUniqueKeys(out)
}

// fragmentWindowCoverage reports the maximum fraction of the observation's
// interior trigrams contained in a single contiguous window of candRunes.
// The caller has checked that the observation has interior trigrams and that
// candRunes is strictly longer than the observation; the value is 0 when no
// window can beat the floor's evidence bar and is NaN-free.
//
// Three window lengths are slid — len(obsRunes)-1, len(obsRunes),
// len(obsRunes)+1 — because the read's length can sit at most a character off
// the window it came from either way: a trailing junk character ("woe" read as
// "woet") makes the read one longer, and a box-edge trim can shave the other
// direction. A window equal to the read that is a perfect window of the line
// contains every interior trigram of the read (each lies wholly inside the
// read, hence wholly inside the window); the ±1 windows keep that true when
// the read is a character off in length.
//
// Cost is three sliding passes of O(len(candRunes)) small steps: a slide
// changes the window by two trigrams (one leaves, one enters), and each is one
// hashTrigram plus one binary search over the observation's sorted interior
// keys, updating the dense winCount/winPresent pair (ranker state). The
// per-candidate set-bound prefilter in scoreOne means this runs only for
// candidates whose bag-of-trigrams already nearly contains the read.
func (r *ranker) fragmentWindowCoverage(candRunes []rune) float64 {
	qLen := len(r.obsRunes)
	qn := len(r.obsIntKeys)
	if qn == 0 || len(candRunes) <= qLen {
		return 0
	}
	best := 0.0
	if len(r.winCount) != qn {
		r.winCount = make([]int16, qn)
	}
	for offset := -1; offset <= 1; offset++ {
		L := qLen + offset
		if L < 3 {
			continue
		}
		r.winPresent = 0
		for i := range r.winCount {
			r.winCount[i] = 0
		}
		for j := 0; j <= L-3; j++ {
			r.winStep(candRunes, j, +1)
		}
		best = maxWindowCoverage(best, r.winPresent, qn)
		last := len(candRunes) - L
		for s := 1; s <= last; s++ {
			r.winStep(candRunes, s-1, -1)
			r.winStep(candRunes, s+L-3, +1)
			if cov := maxWindowCoverage(best, r.winPresent, qn); cov > best {
				best = cov
			}
		}
		if best >= 1.0 {
			return 1.0
		}
	}
	return best
}

// winStep adds (sign +1) or removes (sign -1) the trigram starting at
// candRunes[pos] from the window's count, touching state only when that
// trigram is one of the observation's interior keys.
func (r *ranker) winStep(candRunes []rune, pos int, sign int) {
	t := hashTrigram([3]rune{candRunes[pos], candRunes[pos+1], candRunes[pos+2]})
	i := sort.Search(len(r.obsIntKeys), func(j int) bool { return r.obsIntKeys[j] >= t })
	if i >= len(r.obsIntKeys) || r.obsIntKeys[i] != t {
		return
	}
	if sign > 0 {
		if r.winCount[i] == 0 {
			r.winPresent++
		}
		r.winCount[i]++
		return
	}
	r.winCount[i]--
	if r.winCount[i] == 0 {
		r.winPresent--
	}
}

// maxWindowCoverage folds present/qn into best, keeping the larger.
func maxWindowCoverage(best float64, present, qn int) float64 {
	cov := float64(present) / float64(qn)
	if cov > best {
		return cov
	}
	return best
}

// fragmentFloor reports the observation's interior-trigram coverage over
// candidate: the fraction of the read's interior trigrams the candidate
// contains, with no order constraint. It is the cheap necessary condition for
// fragmentWindowCoverage — every window of the candidate is a subset of the
// candidate, so the set bound is an upper bound on the window metric — and
// scoreOne uses it to skip the sliding passes for candidates that cannot
// clear the floor. The value is meaningful only when the candidate is strictly
// longer than the observation — a line as short as the read is a similarity
// case, not a containment one — which the caller checks; the function is
// total either way and is NaN-free (empty observation set returns 0).
func fragmentFloor(obsInt, candInt []uint32) float64 {
	if len(obsInt) == 0 {
		return 0
	}
	return math.Min(1.0, float64(intersectCount(obsInt, candInt))/float64(len(obsInt)))
}
