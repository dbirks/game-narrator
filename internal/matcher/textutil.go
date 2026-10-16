package matcher

import (
	"math"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// clamp maps v into [lo, hi] and treats a non-finite value as lo. It is the one
// place a NaN can enter a component score, so it does not merely clamp the range:
// a NaN component is read as "no evidence" (0), which is the safe direction for a
// log-linear sum, since log(epsilon + 0) is finite and heavily negative.
func clamp(v, lo, hi float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, -1) {
		return lo
	}
	if math.IsInf(v, 1) {
		return hi
	}
	switch {
	case v < lo:
		return lo
	case v > hi:
		return hi
	default:
		return v
	}
}

func clampInt(v, lo, hi int) int {
	switch {
	case v < lo:
		return lo
	case v > hi:
		return hi
	default:
		return v
	}
}

// unit maps v into [0,1], which is the range every documented component score
// must live in (docs/spec/05 "Component scores in [0,1] for each candidate").
func unit(v float64) float64 { return clamp(v, 0, 1) }

// maxTextRunes is the single bound applied to both sides of every comparison: an
// observation is truncated to it before retrieval, and a stored line is truncated
// to it before indexing. They must be the same number, or a complete observation of
// a long line (docs/spec/12 "very long multi-page utterance") could never match the
// line it came from exactly — the tail would be visible on one side and not the
// other, and the ranking would report a fuzzy near-match on what is in fact a
// verbatim transcript.
//
// 1024 runes is comfortably above any textbox render (the schema allows 8000
// characters per message, which is a multi-page utterance), and it is what caps
// per-candidate scoring work on a hostile 100 KB input.
const maxTextRunes = 1024

// truncateBounds s to at most n runes, cheaply, and is the reason a 100 KB
// observation stays inside the same latency budget as a 100-character one.
func truncateBounds(s string, n int) string {
	if len(s) <= n { // bytes ≥ runes: fast path cannot over-accept
		return s
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

// hashTrigram folds three runes into a 32-bit key. A hash rather than the trigram
// string itself keeps the inverted index small enough for the appliance: a real
// profile's 10k messages produce a few tens of thousands of distinct trigrams, and
// storing them as Go strings would add an allocation and a 16-byte header each
// (docs/spec/16 M1 "without memory blowup"). The collision rate over a 32-bit
// space at that population is on the order of 1e-1 per corpus, i.e. occasionally
// two trigrams share a bucket; that can nudge a retrieval score by one unit and
// cannot change an exact match, which never consults this index.
func hashTrigram(r [3]rune) uint32 {
	var buf [12]byte
	n := 0
	for _, c := range r {
		// EncodeRune substitutes U+FFFD for an invalid rune, so hashing and
		// counting agree on what a malformed observation contains.
		n += utf8.EncodeRune(buf[n:], c)
	}
	return fnv1a(buf[:n])
}

// fnv1a is FNV-1a 32-bit, written out rather than taken from hash/fnv because the
// stdlib type is an interface over a heap-allocated struct: indexing a 10k-message
// profile hashes on the order of a million trigrams, and allocating a hasher per
// one is a megabyte of churn for no reason. Plain integer arithmetic is also what
// makes the keys reproducible across Go releases.
func fnv1a(b []byte) uint32 {
	h := uint32(2166136261)
	for _, c := range b {
		h ^= uint32(c)
		h *= 16777619
	}
	return h
}

// sortUint32 sorts trigram keys. Insertion sort below 128 elements (the common
// case: a one-line observation) beats sort.Slice's reflection; longer runs — a
// stored message indexed up to msgMaxRunes — fall back to sort.Slice, because
// insertion sort on 3k keys is quadratic and build time is a gate in docs/spec/16.
func sortUint32(a []uint32) {
	if len(a) >= 128 {
		slices.Sort(a)
		return
	}
	for i := 1; i < len(a); i++ {
		v := a[i]
		j := i - 1
		for j >= 0 && a[j] > v {
			a[j+1] = a[j]
			j--
		}
		a[j+1] = v
	}
}

// levRow is the fixed-width sliding window of banded Levenshtein columns.
type levRow []uint32

func newLevRow(width int) levRow { return make(levRow, width) }

const levUnknown = math.MaxUint32

// trigramKeys returns the distinct trigram hashes of the rune slice, using
// boundary padding so that a one- or two-character string still yields keys.
// Without padding, "ok" would have no trigrams at all and would be unreachable by
// fuzzy retrieval — the exact case docs/spec/05 says needs *more* evidence rather
// than none.
//
// out is reused when it has capacity, which keeps a 500-query benchmark from
// allocating a slice per candidate comparison; callers must not retain the result.
func trigramKeys(runes []rune, out []uint32) []uint32 {
	out = out[:0]
	if len(runes) == 0 {
		return out
	}
	pad := rune(boundaryPad)
	prev2, prev1 := pad, pad
	push := func(r [3]rune) { out = append(out, hashTrigram(r)) }
	for _, cur := range runes {
		push([3]rune{prev2, prev1, cur})
		prev2, prev1 = prev1, cur
	}
	push([3]rune{prev1, prev2, pad})
	push([3]rune{prev2, pad, pad})
	return out
}

// boundaryPad is the sentinel rune marking string edges for trigrams. It is a
// rune no script text contains after normalization (U+0001 is a control character
// that internal/games/textnorm's sanitizer replaces with U+FFFD), so it cannot
// collide with real content.
const boundaryPad = 0x01

// sortUniqueKeys sorts and de-duplicates in place, returning a prefix of keys.
// Sorting is what makes the posting traversal below deterministic: iterating a Go
// map of query trigrams would make the work cap select a different subset on each
// run, which is precisely the nondeterminism docs/spec/12 forbids.
func sortUniqueKeys(keys []uint32) []uint32 {
	sortUint32(keys)
	out := keys[:0]
	for i, k := range keys {
		if i > 0 && keys[i-1] == k {
			continue
		}
		out = append(out, k)
	}
	return out
}

// intersectCount counts values present in two sorted, duplicate-free key lists.
// Linear merge: with both sides already sorted it beats a binary search per
// element, and it is the hot loop of scoring.
func intersectCount(a, b []uint32) int {
	i, j, n := 0, 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			n++
			i++
			j++
		case a[i] < b[j]:
			i++
		default:
			j++
		}
	}
	return n
}

// dice returns the Sørensen–Dice coefficient over two key sets, given the size of
// their intersection and the size of each set.
func dice(shared, nA, nB int) float64 {
	if nA == 0 || nB == 0 {
		return 0
	}
	return unit(2 * float64(shared) / float64(nA+nB))
}

// hashToken folds a word into a 32-bit key, on the same reasoning as hashTrigram:
// the index stores numbers, not strings, and a collision can only nudge a token
// component by one unit.
func hashToken(s string) uint32 {
	// The string form converts to no []byte: fnv1a ranges over the bytes of a
	// slice header, and Go's compiler keeps a read-only string loop allocation-free.
	return fnv1a([]byte(s))
}

// tokenHashes returns the sorted, duplicate-free token keys of a folded string.
// Duplicate-free matters: a line that repeats "no" three times is not three times
// the evidence of a line that says it once.
func tokenHashes(s string, out []uint32) []uint32 {
	out = out[:0]
	for _, tok := range strings.Fields(s) {
		out = append(out, hashToken(tok))
	}
	return sortUniqueKeys(out)
}

// levBand is how far from the diagonal banded Levenshtein explores. Twelve edits
// is a deliberate ceiling: the term exists to separate "one glyph misread" from
// "different line", and beyond a dozen edits in a ≤512-rune observation the
// similarity term saturates near zero anyway. Cost is O(len·band), not O(len²),
// which is what keeps 40 candidates × 500 queries inside a 5 ms budget.
const levBand = 12

// levMaxRunes bounds the strings handed to the edit-distance term. Longer than
// this and the term reports on a truncated prefix, which is honest because the
// trigram Dice already accounts for the whole string's content.
const levMaxRunes = 128

// levenshteinSimilarity returns 1 - d/maxLen when the edit distance d is within
// levBand, and 0 when it is beyond it.
//
// The band is why this is affordable: a full DP over two 128-rune strings is 16k
// cells per candidate, and the same cost is paid for a candidate that is nowhere
// near the observation. Cells outside the band are never computed; a window that
// cannot reach the target cell already implies d > band, which is reported as 0
// without further work. This is the "bounded Levenshtein" docs/spec/05 asks for
// ("Levenshtein alone is not adequate", and it must not be unbounded either).
func levenshteinSimilarity(a, b []rune) float64 {
	la, lb := len(a), len(b)
	if la == 0 || lb == 0 {
		if la == lb {
			return 1
		}
		return 0
	}
	if la > levMaxRunes {
		a, la = a[:levMaxRunes], levMaxRunes
	}
	if lb > levMaxRunes {
		b, lb = b[:levMaxRunes], levMaxRunes
	}
	band := levBand
	diff := la - lb
	if diff < 0 {
		diff = -diff
	}
	if diff >= band {
		return 0 // the true distance is already past the band
	}
	maxLen := la
	if lb > maxLen {
		maxLen = lb
	}

	const unknown = levUnknown
	prev := newLevRow(2*band + 3)
	cur := newLevRow(2*band + 3)
	get := func(row []uint32, rLo, rHi, j int) uint32 {
		if j < rLo || j > rHi {
			return unknown // outside that row's computed window
		}
		return row[j-rLo]
	}
	maybePlus := func(v uint32, cost uint32) uint32 {
		if v == unknown {
			return unknown
		}
		return v + cost
	}

	// Row 0: D[0][j] = j, only for the columns inside the window.
	loP, hiP := 0, minInt(lb, band)
	for j := 0; j <= hiP; j++ {
		prev[j] = uint32(j)
	}
	for i := 1; i <= la; i++ {
		lo, hi := i-band, i+band
		if lo < 0 {
			lo = 0
		}
		if hi > lb {
			hi = lb
		}
		for j := lo; j <= hi; j++ {
			// Deletion is always available; insertion and substitution need a
			// column j-1 to exist, which it does not for j == 0 (D[i][0] = i).
			best := maybePlus(get(prev, loP, hiP, j), 1)
			if j > 0 {
				cost := uint32(1)
				if a[i-1] == b[j-1] {
					cost = 0
				}
				if v := maybePlus(get(cur, lo, hi, j-1), 1); v != unknown && (best == unknown || v < best) {
					best = v
				}
				if v := maybePlus(get(prev, loP, hiP, j-1), cost); v != unknown && (best == unknown || v < best) {
					best = v
				}
			}
			cur[j-lo] = best
		}
		prev, cur = cur, prev
		loP, hiP = lo, hi
	}
	d := get(prev, loP, hiP, lb)
	if d == unknown || d > uint32(band) {
		return 0
	}
	return unit(1 - float64(d)/float64(maxLen))
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func min32(a, b uint32) uint32 {
	if a < b {
		return a
	}
	return b
}

// nonSpaceLen counts the characters that actually carry information. A 120-space
// OCR artifact has zero, and docs/spec/05's holdoff for "snippets like 'Yes.'" is
// about content, not byte length.
func nonSpaceLen(s string) int {
	n := 0
	for _, r := range s {
		if !unicode.IsSpace(r) {
			n++
		}
	}
	return n
}

// messageTrigrams returns the sorted, duplicate-free trigram keys of one stored
// message. Distinct-per-message is what makes the inverted index's shared count a
// set intersection rather than a common-substring frequency count: a message in
// which "the" appears nine times must not score nine times the evidence of one
// that uses it once, and a posting list per (trigram, message) pair is exactly the
// structure that keeps Dice coefficients in [0,1].
func messageTrigrams(key string, out []uint32) []uint32 {
	return sortUniqueKeys(trigramKeys([]rune(key), out))
}
