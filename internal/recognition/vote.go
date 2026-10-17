package recognition

import (
	"math"
	"strings"

	"github.com/dbirks/game-narrator/internal/games/textnorm"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

// maxLevRunes bounds the text length the edit distance will look at. A textbox
// is a textbox; if a caller hands over 10 KB of garbage (docs/spec/12 fuzzes
// exactly this), the O(n·m) distance would be the most expensive thing in the
// capture loop, so both sides are truncated before comparison and the result
// stays bounded and deterministic rather than stalling the frame rate. Beyond
// maxLevRunes the ratio is computed on the leading slice, which is what matters
// anyway: a change that far into a line does not alter "is this the same text"
// at the resolution the state machine acts on.
const maxLevRunes = 192

// maxWindowRunes bounds the normalized text kept per window entry, so a
// pathological OCR result cannot make the vote window's memory proportional to
// its input.
const maxWindowRunes = 512

// Vote is the temporal-vote answer for one frame.
//
// The rules come straight out of docs/spec/05 "OCR pipeline": "Use temporal
// voting between several adjacent frames" and "A typewriter prefix is not a new
// distinct utterance", which together require a vote that (a) is dominated by
// how many frames said a thing, (b) is nudged but not decided by confidence,
// and (c) prefers the *shorter* of two prefix-related hypotheses, because a
// prefix is what a still-typing line looks like and claiming the extension
// early is what predictive mode exists to make an explicit opt-in.
type Vote struct {
	// Text is the winning normalized text ("" when nothing was readable). It is
	// the *output* normalization — lowercase, whitespace-collapsed, punctuation
	// kept — i.e. the string the session hands to the matcher, which applies the
	// OCR fold itself (internal/matcher IndexOptions). Never render this for a
	// user; it is a retrieval key dressed as a sentence.
	Text string
	// Key is the comparison fold (punctuation stripped, OCR confusions folded)
	// that Text was chosen under. Prefix and contradiction tests run on Key,
	// never on Text, because two frames of one static box routinely differ by
	// exactly one OCR-confusable glyph.
	Key string
	// Confidence is the mean OCR confidence of the frames agreeing with Text, in
	// [0,1].
	Confidence float64
	// Support counts window entries whose text equals Text.
	Support int
	// Total counts window entries considered.
	Total int
	// ContradictionRatio is the vote weight of entries that are not consistent
	// with Text (not a prefix, near-prefix or extension of it) over the total
	// weight. It is what STABILIZING→READY checks.
	ContradictionRatio float64
}

// vote reduces the window to one text. Window entries are (normalized text,
// confidence) pairs, oldest first; empty texts are entries too, because a box
// that reads blank in half the recent frames is not a stable line no matter how
// confident the other half was.
func vote(entries []entry) Vote {
	if len(entries) == 0 {
		return Vote{}
	}
	type agg struct {
		key     string
		weight  float64
		confSum float64
		count   int
		last    int // index of the newest entry in this group
	}
	// Insertion-order grouping: the window is at most Config.VoteWindow (≤ 32)
	// entries, so a slice scan is both cheaper and more deterministic than a
	// map, and no result can ever depend on Go's randomized map iteration
	// (docs/spec/12 requires determinism of the whole recognition path, not
	// only of the matcher's ranking).
	var groups []agg
	for i, e := range entries {
		weight := 0.5 + unit(e.Conf)
		found := false
		for gi := range groups {
			if groups[gi].key == e.Key {
				groups[gi].weight += weight
				groups[gi].confSum += unit(e.Conf)
				groups[gi].count++
				groups[gi].last = i
				found = true
				break
			}
		}
		if !found {
			groups = append(groups, agg{key: e.Key, weight: weight, confSum: unit(e.Conf), count: 1, last: i})
		}
	}

	best := groups[0]
	for _, g := range groups[1:] {
		switch {
		case g.weight > best.weight:
			best = g
		case g.weight == best.weight && g.last > best.last:
			// Deterministic tie-break: with equal accumulated evidence, the newer
			// frame describes the screen now. The opposite preference — keeping the
			// older, shorter hypothesis — would freeze a typewriter on its first few
			// characters: the accepted text would never grow, the stability clock
			// would never reset, and a box still typing would be declared READY.
			// Flicker protection is not this tie-break's job; it is the weight
			// majority (a lone bad frame is outvoted) plus Config.AdvanceRunFrames
			// (a competing text must hold for several frames before it may override
			// the majority).
			best = g
		}
	}

	totalWeight := 0.0
	contraWeight := 0.0
	for _, g := range groups {
		totalWeight += g.weight
		if g.key != best.key && !consistent(best.key, g.key) {
			contraWeight += g.weight
		}
	}
	v := Vote{
		Text:    entries[best.last].Text,
		Key:     best.key,
		Support: best.count,
		Total:   len(entries),
	}
	if best.count > 0 {
		v.Confidence = best.confSum / float64(best.count)
	}
	if totalWeight > 0 {
		v.ContradictionRatio = contraWeight / totalWeight
	}
	return v
}

// confidenceOf reports the mean confidence of the frames whose comparison key is
// key, used when a trailing run overrides the vote and the vote's own confidence
// would describe the wrong hypothesis.
func confidenceOf(entries []entry, key string) float64 {
	sum, n := 0.0, 0
	for _, e := range entries {
		if e.Key == key {
			sum += unit(e.Conf)
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

// newestForKey returns the output-normalized text of the newest entry carrying
// key, so an override agrees with what the engine most recently read rather than
// with whichever spelling happened to enter the window first.
func newestForKey(entries []entry, key string) string {
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Key == key {
			return entries[i].Text
		}
	}
	return key
}

// tailRun returns the comparison key of the trailing run of identical entries
// and its length — "how many consecutive frames have agreed on this". A page
// advance clears a vote window only gradually, so without the run the majority
// of a five-frame window would keep the previous page's text for four more
// frames and the player would hear the old line over the new one
// (docs/spec/12 "20 rapid textbox page advances").
func tailRun(entries []entry) (string, int) {
	if len(entries) == 0 {
		return "", 0
	}
	key := entries[len(entries)-1].Key
	n := 0
	for i := len(entries) - 1; i >= 0 && entries[i].Key == key; i-- {
		n++
	}
	return key, n
}

// changeKind classifies how new text relates to old text.
type changeKind uint8

const (
	// changeSame: identical. The common case, and the reason repeat
	// observation must not create duplicate speech (docs/spec/16 M2).
	changeSame changeKind = iota
	// changeExtension: old is a prefix of new — a typewriter revealing more of
	// the same line. Explicitly *not* a new utterance (docs/spec/05).
	changeExtension
	// changeDropout: new is empty. An unread frame is missing evidence, not a
	// different line, so it never cancels speech.
	changeDropout
	// changeJitter: new is neither identical nor an extension, and differs by
	// less than the material threshold — an OCR read of one or two glyphs.
	changeJitter
	// changeTruncation: new is a strict prefix of old, i.e. the box lost text.
	// Treated as material (docs/spec/05's test is "the new text is not an
	// extension of the old"): a page that disappeared backwards is a page
	// change, and the alternative — waiting for a box to fill in forwards
	// again — would keep narrating a line the game has already left.
	changeTruncation
	// changeMaterial: neither extension nor prefix, and at least
	// Config.ChangeThreshold of the runes differ. This is a new line.
	changeMaterial
)

func (k changeKind) String() string {
	switch k {
	case changeSame:
		return "same"
	case changeExtension:
		return "extension"
	case changeDropout:
		return "dropout"
	case changeJitter:
		return "jitter"
	case changeTruncation:
		return "truncation"
	default:
		return "material"
	}
}

// classify decides whether new is a material change from old under threshold.
// The spec's rule is "normalized edit distance > threshold, or the new text is
// not an extension of the old"; the extra categories exist because
// "not an extension" taken literally turns a single misread comma into a
// cancellation of the player's speech, which is the exact failure
// docs/spec/12 lists under failure injection. The gap is closed by requiring a
// non-extension change to be confirmed over Config.AdvanceRunFrames frames
// (see Tracker.Step) rather than by loosening the distance test.
func classify(old, new string, threshold float64) (changeKind, float64) {
	switch {
	case new == old:
		return changeSame, 0
	case new == "":
		return changeDropout, 1
	case isPrefix(old, new):
		return changeExtension, editRatio(old, new)
	case isPrefix(new, old):
		return changeTruncation, editRatio(old, new)
	}
	r := editRatio(old, new)
	if r > threshold {
		return changeMaterial, r
	}
	return changeJitter, r
}

// isMaterial reports whether new is a material change from old: the two kinds
// that mean "a different line" — a real edit, or a box that lost text — as
// opposed to same / typewriter extension / dropout / glyph jitter, which the
// state machine must ride out rather than cancel the player's speech for.
func isMaterial(old, next string, threshold float64) bool {
	k, _ := classify(old, next, threshold)
	return k == changeMaterial || k == changeTruncation
}

// isExtension reports whether next continues old as a typewriter would: old is a
// prefix of it. It is asked only about a *committed predictive* line, where more
// text is the evidence that the completeness claim was wrong; for every other
// state an extension is deliberately invisible (docs/spec/05 "A typewriter prefix
// is not a new distinct utterance").
func isExtension(old, next string) bool {
	return old != "" && next != "" && isPrefix(old, next)
}

// consistent reports whether two observations can belong to one line as it is
// being typed: one is a prefix of the other, or the shorter sits within the
// first len(shorter) characters of the longer closely enough to be the same
// text with glyph noise. Prefix-comparability is the property that makes a
// typewriter's intermediate frames agree instead of contradicting each other,
// so it — not exact equality — is what contradiction is measured against.
func consistent(a, b string) bool {
	if a == "" || b == "" {
		return true
	}
	if isPrefix(a, b) || isPrefix(b, a) {
		return true
	}
	short, long := a, b
	if len(long) < len(short) {
		short, long = b, a
	}
	head := takeRunes(long, runeLen(short))
	return editRatio(short, head) <= 0.34
}

// editRatio is the Levenshtein distance normalized by the longer input, in
// [0,1]. Normalizing by length (rather than returning a raw count) is what lets
// Config.ChangeThreshold mean the same thing for "OK" and for a 200-character
// page; docs/spec/05 warns that Levenshtein alone is not adequate for short or
// repeated strings, which is why this is a *threshold input* to a decision that
// also weighs prefix structure, and never the decision itself.
// The comparison is bounded so the capture loop\'s cost never depends on how much
// garbage an engine returned: at most two maxLevRunes-wide Levenshtein bands are
// computed, one at the head and one at the tail of the inputs. Within that bound
// the value is the true Levenshtein ratio whenever either text is short enough to
// be compared whole — every real textbox is, which is why maxLevRunes is sized
// around one page of dialogue and the sampling below is a guard, not the
// behaviour. For inputs longer than the bound it is the *observed divergence
// rate* of those two samples, normalized by the sampled span rather than by the
// full length: normalizing a bounded distance by an unbounded length would drive
// every long pair toward 0 and make a 5 KB page change look like OCR jitter, and
// claiming a difference the sample never saw would cancel the player\'s speech for
// a repeated observation of one long page — the stale/duplicate cancellation
// docs/spec/16 M2 forbids. A change confined to the middle of a text longer than
// two samples is therefore invisible here; classify is where that tradeoff is
// written down, and the raw vote window still shows the change to the developer
// inspector.
func editRatio(a, b string) float64 {
	la, lb := runeLen(a), runeLen(b)
	if la == 0 && lb == 0 {
		return 0
	}
	dist := sampleDistance(a, b, la, lb)
	lenDiff := la - lb
	if lenDiff < 0 {
		lenDiff = -lenDiff
	}
	if lenDiff > dist {
		dist = lenDiff // |la-lb| edits are required no matter what the samples show
	}
	n := la
	if lb > n {
		n = lb
	}
	// A denominator that grew past the sampled span would shrink an honest
	// distance toward zero; capping it at the span keeps the ratio a rate over the
	// text that was actually looked at.
	if span := 2 * maxLevRunes; n > span {
		n = span
	}
	if n == 0 {
		return 0
	}
	r := float64(dist) / float64(n)
	if r > 1 {
		return 1
	}
	return r
}

// sampleDistance bounds the O(n·m) work to at most two Levenshtein bands: the
// first maxLevRunes of each input, and, for inputs longer than that, the last
// maxLevRunes. For equal-length inputs the two bands are disjoint, so their sum
// is a lower bound on the true distance; for inputs of different lengths the
// bands can slide past each other, which is why the result is clamped to the
// sampled span and treated as a rate, never as a distance.
func sampleDistance(a, b string, la, lb int) int {
	sum := levenshtein(clampRunes(a, maxLevRunes), clampRunes(b, maxLevRunes))
	if la <= maxLevRunes && lb <= maxLevRunes {
		return sum
	}
	return sum + levenshtein(tailRunes(a, maxLevRunes), tailRunes(b, maxLevRunes))
}

func levenshtein(a, b []rune) int {
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			best := prev[j] + 1
			if cur[j-1]+1 < best {
				best = cur[j-1] + 1
			}
			if prev[j-1]+cost < best {
				best = prev[j-1] + cost
			}
			cur[j] = best
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// rawText assembles the frame's text from an OCR result, dropping the words
// below minConf (docs/spec/05 "Ignore low-confidence artifacts such as textbox
// edge cursors"). It prefers words, falls back to lines, and only then to the
// engine's concatenated text, so an engine that reports no boxes still
// contributes something.
func rawText(res *ocr.Result, minConf float64) (text string, conf float64) {
	if res == nil {
		return "", 0
	}
	if n := len(res.Words); n > 0 {
		var sb strings.Builder
		sum, kept := 0.0, 0
		for _, w := range res.Words {
			c := unit(w.Conf)
			if c < minConf {
				continue
			}
			if sb.Len() > 0 {
				sb.WriteByte(' ')
			}
			sb.WriteString(w.Text)
			sum += c
			kept++
		}
		if kept > 0 {
			return sb.String(), sum / float64(kept)
		}
		// Every word was below the floor: that is a blank box, not a box
		// containing whatever the engine's own mean happened to average.
		return "", meanOr(res.MeanConf, 0)
	}
	if n := len(res.Lines); n > 0 {
		var sb strings.Builder
		sum, kept := 0.0, 0
		for _, l := range res.Lines {
			c := unit(l.Conf)
			if c < minConf || strings.TrimSpace(l.Text) == "" {
				continue
			}
			if sb.Len() > 0 {
				sb.WriteByte(' ')
			}
			sb.WriteString(l.Text)
			sum += c
			kept++
		}
		if kept > 0 {
			return sb.String(), sum / float64(kept)
		}
		return "", meanOr(res.MeanConf, 0)
	}
	return res.Text, meanOr(res.MeanConf, 0)
}

func meanOr(v float64, fallback float64) float64 {
	if v == 0 || math.IsNaN(v) {
		return fallback
	}
	return unit(v)
}

func unit(v float64) float64 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func isPrefix(short, long string) bool { return strings.HasPrefix(long, short) }

func runeLen(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}

func takeRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	seen := 0
	for off := range s {
		if seen == n {
			return s[:off]
		}
		seen++
	}
	return s
}

// tailRunes returns the last n runes of s, for the second sample band.
func tailRunes(s string, n int) []rune {
	r := []rune(s)
	if len(r) > n {
		r = r[len(r)-n:]
	}
	return r
}

func clampRunes(s string, n int) []rune {
	r := []rune(s)
	if len(r) > n {
		r = r[:n]
	}
	return r
}

// normalizeText folds s for comparison with the configured folder.
func normalizeText(f *textnorm.Folder, s string) string {
	if f == nil {
		return strings.TrimSpace(s)
	}
	return f.Normalize(s)
}
