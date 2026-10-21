package matcher

import (
	"math"
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestLevenshteinMatchesFullDP is the correctness half of the banded scorer: a
// band is an optimization, and an optimization that quietly reports a different
// number would make every downstream calibration meaningless. Inside the band the
// result must equal a straightforward O(n·m) DP; outside it, 0 is the contract.
func TestLevenshteinMatchesFullDP(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	cases := [][2]string{
		{"", ""}, {"a", ""}, {"", "b"},
		{"same", "same"},
		{"kitten", "sitting"},
		{"morning", "rnorning"},
		{"a line about the harbour bell", "a line about the harbour bel"},
		{"gull", "gulf"},
		{"the tidewatcher counts the boats", "the tidewatcher counts the boats"},
	}
	for i := 0; i < 400; i++ {
		n := rng.Intn(40)
		m := n + rng.Intn(9) - 4
		if m < 0 {
			m = 0
		}
		cases = append(cases, [2]string{randomWord(rng, n), randomWord(rng, m)})
	}
	for _, c := range cases {
		a, b := []rune(c[0]), []rune(c[1])
		got := levenshteinSimilarity(a, b)
		want := referenceLevenshteinSimilarity(a, b)
		if math.IsNaN(got) || math.IsInf(got, 0) || got < 0 || got > 1 {
			t.Fatalf("levenshteinSimilarity(%q, %q) = %v, want [0,1]", c[0], c[1], got)
		}
		d := fullLevenshtein(a, b)
		switch {
		case d <= levBand:
			if math.Abs(got-want) > 1e-9 {
				t.Errorf("within band: %q vs %q: got %v, want %v (d=%d)", c[0], c[1], got, want, d)
			}
		default:
			if got != 0 {
				t.Errorf("beyond band: %q vs %q: got %v, want 0 (d=%d)", c[0], c[1], got, d)
			}
		}
	}
}

// TestLevenshteinCostIsBounded asserts the property the budget depends on: a
// 100 KB pair must not cost more, in cells, than a 128-rune pair. The function
// truncates, and this catches someone removing the truncation.
func TestLevenshteinCostIsBounded(t *testing.T) {
	long := []rune(strings.Repeat("abcdefghij", 20000))
	// Two fixed rows per call, whatever the input length; nothing proportional to
	// the string. A regression here (a full matrix allocation, say) is invisible in
	// the ranking tests and fatal on a 2–5 Hz capture loop.
	allocs := testing.AllocsPerRun(20, func() {
		if got := levenshteinSimilarity(long, long); got != 1 {
			t.Errorf("identical 200k-rune strings: got %v, want 1", got)
		}
	})
	if allocs > 8 {
		t.Errorf("long-input Levenshtein allocated %.0f times per call, want O(1)", allocs)
	}
}

func randomWord(rng *rand.Rand, n int) string {
	const alphabet = "abcdefghij klmnop"
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteByte(alphabet[rng.Intn(len(alphabet))])
	}
	return b.String()
}

// referenceLevenshteinSimilarity and fullLevenshtein are the dumb, obviously
// correct implementations the banded one is checked against. Quadratic, tiny,
// test-only — deliberately not what production runs.
func referenceLevenshteinSimilarity(a, b []rune) float64 {
	d := fullLevenshtein(a, b)
	maxLen := len(a)
	if len(b) > maxLen {
		maxLen = len(b)
	}
	if maxLen == 0 {
		return 1
	}
	if d > levBand {
		return 0
	}
	return unit(1 - float64(d)/float64(maxLen))
}

func fullLevenshtein(a, b []rune) int {
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
			cur[j] = minInt(minInt(prev[j]+1, cur[j-1]+1), prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

func TestTrigramDiceSeparatesLikeLines(t *testing.T) {
	obs := []rune("the tidewatcher counts the boats")
	near := []rune("the tidewatcher counts the boat")
	far := []rune("hold the line and count nothing")

	obsKeys := sortUniqueKeys(trigramKeys(obs, nil))
	nearKeys := sortUniqueKeys(trigramKeys(near, nil))
	farKeys := sortUniqueKeys(trigramKeys(far, nil))

	dNear := dice(intersectCount(obsKeys, nearKeys), len(obsKeys), len(nearKeys))
	dFar := dice(intersectCount(obsKeys, farKeys), len(obsKeys), len(farKeys))
	if !(dNear > dFar) {
		t.Errorf("Dice did not separate similar lines: near=%v far=%v", dNear, dFar)
	}
	if dNear > 1 || dFar < 0 {
		t.Errorf("Dice out of range: %v %v", dNear, dFar)
	}
	// A one- and a two-character string must still produce keys: with no padding
	// they would be unreachable by fuzzy retrieval, which is the wrong default for
	// the shortest lines in a script.
	if len(messageTrigrams("a", nil)) == 0 || len(messageTrigrams("no", nil)) == 0 {
		t.Error("very short keys produced no trigrams; boundary padding is not working")
	}
}

func TestTokenComponentIgnoresOrderButNotContent(t *testing.T) {
	// The token component production computes is Dice over interned token hashes;
	// these are the properties it must keep for docs/spec/05's "weighted
	// character/token similarities" to mean anything.
	diceOf := func(a, b string) float64 {
		ka := tokenHashes(a, nil)
		kb := tokenHashes(b, nil)
		return dice(intersectCount(ka, kb), len(ka), len(kb))
	}
	if got := diceOf("rope and sail", "sail and rope"); got != 1 {
		t.Errorf("permutation: got %v, want 1 (the token set is a set)", got)
	}
	if got := diceOf("rope and sail", "rope and steel"); got >= 1 || got <= 0.5 {
		t.Errorf("one token differing: got %v, want a high but partial score", got)
	}
	// An empty set is no evidence at all, which scores 0 rather than 1: the
	// alternative would make a punctuation-only observation a perfect token match
	// against every candidate. In practice Build never indexes an empty key and
	// Rank returns early on one, so this branch is the belt-and-braces case.
	if got := diceOf("", ""); got != 0 {
		t.Errorf("both empty: got %v, want 0 (no evidence is not agreement)", got)
	}
	if got := diceOf("x", ""); got != 0 {
		t.Errorf("one empty: got %v, want 0", got)
	}
	// A repeated word is one token, not three: otherwise a stammering NPC line
	// would look like three times the evidence and outrank a real match.
	if got := diceOf("no no no", "no"); got != 1 {
		t.Errorf("repeated token: got %v, want 1", got)
	}
	if !sortedUniqueUint32(tokenHashes("the big red fox the", nil)) {
		t.Error("tokenHashes returned duplicates or an unsorted set; the merge intersect would then be wrong")
	}
}

func sortedUniqueUint32(a []uint32) bool {
	for i := 1; i < len(a); i++ {
		if a[i] <= a[i-1] {
			return false
		}
	}
	return true
}

func TestClampTreatsNonFiniteAsNoEvidence(t *testing.T) {
	if got := clamp(math.NaN(), 0, 1); got != 0 {
		t.Errorf("NaN clamped to %v, want the low bound", got)
	}
	if got := clamp(math.Inf(-1), 0, 1); got != 0 {
		t.Errorf("-Inf clamped to %v, want 0", got)
	}
	if got := clamp(math.Inf(1), 0, 1); got != 1 {
		t.Errorf("+Inf clamped to %v, want 1", got)
	}
	if !utf8.ValidString(truncateBounds("héllo wörld", 3)) {
		t.Error("truncateBounds split a rune")
	}
}

// TestTrimEdgeNoise pins the box-edge trim's bounds (ADR-0115 §2): at most two
// short tokens per side, unicode-aware, and the trim must never eat the line.
func TestTrimEdgeNoise(t *testing.T) {
	cases := []struct{ in, want string }{
		// The live noise shapes: border glyphs at the head, a digit at the tail.
		{"4 $o? We sail for the Gull-King's rock on the morning tide.", "We sail for the Gull-King's rock on the morning tide."},
		{"i We sail for the Gull-King's rock. 5", "sail for the Gull-King's rock."},
		// Two short tokens per side is the bound, and the two-rune rule covers
		// real short words too: the raw-versus-trimmed comparison in the
		// scorer is what protects them, not the trim.
		{"a b long word here c d", "long word here"},
		{"ab cd long word here ef", "long word here"},
		// A three-rune all-letter word is not border noise ("$o?" has its
		// letters mixed with marks): nothing is dropped.
		{"The crew argues.", "The crew argues."},
		// A single-token reading has nothing to trim against.
		{"Yes.", "Yes."},
		// The trim must not eat the line: all-noise keeps the original.
		{"a b", "a b"},
		{"4 i", "4 i"},
		// Whitespace is token-boundary only; internal runs fold to single spaces.
		{"4  i\tWe  sail for the tide.", "We sail for the tide."},
		// Empty in, empty out.
		{"", ""},
	}
	for _, c := range cases {
		if got := TrimEdgeNoise(c.in); got != c.want {
			t.Errorf("TrimEdgeNoise(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
