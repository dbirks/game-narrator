package matcher

import (
	"fmt"
	"math"
	"math/rand"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/games"
)

// TestRankingIsDeterministic is the property docs/spec/12 names outright: "rank
// ordering deterministic under same inputs/settings". It is asserted over a mixed
// corpus and a mixed observation set — including ties, which is where a stray map
// iteration or a float accumulated in unstable order actually shows up.
func TestRankingIsDeterministic(t *testing.T) {
	msgs, edges := miniScript()
	big, bigEdges := bigCorpus(2000)
	msgs = append(msgs, big...)
	edges = append(edges, bigEdges...)
	idx := mustBuild(t, msgs, edges)

	rng := rand.New(rand.NewSource(4242))
	obs := noisyObservations(rng, msgs, 200)
	for i, o := range obs {
		first := rankingString(idx.Rank(o, RankOptions{}))
		for k := 0; k < 3; k++ {
			if got := rankingString(idx.Rank(o, RankOptions{})); got != first {
				t.Fatalf("observation %d (%q) run %d disagreed:\n %s\n %s", i, o.Text, k, got, first)
			}
		}
	}
}

// TestPropertyInvariants runs the invariant sweep a fuzz corpus cannot: over
// randomly mutated observations and randomly hostile settings, every returned
// number is finite, in range, and normalized.
func TestPropertyInvariants(t *testing.T) {
	msgs, edges := miniScript()
	idx := mustBuild(t, msgs, edges)
	rng := rand.New(rand.NewSource(90210))
	obs := noisyObservations(rng, msgs, 600)

	// A few settings profiles, including ones a caller could only have produced by
	// accident, which is exactly when the sanitizer has to hold.
	settings := []RankOptions{
		{},
		{TopK: 1},
		{TopK: 50, MinProbability: 0.99, MinMargin: 0.99},
		{Weights: Weights{Text: -1, Scene: math.Inf(1), EdgePrior: math.NaN()}},
		{MinSpecificChars: 1, ExactAmbiguityMax: 1000, MinTextLenForPredict: 1},
		{MinSpecificChars: 1000, ExactAmbiguityMax: 0},
		{RetrievalK: 1, MaxPostingWork: 1, MaxPrefixScan: 1},
		{MaxRecent: 0},
	}
	for i, o := range obs {
		for _, s := range settings {
			r := idx.Rank(o, s)
			label := fmt.Sprintf("obs %d (%q) settings %+v", i, truncate(o.Text, 40), s)
			assertFiniteRanking(t, label, r)
			if len(r.Candidates) > maxInt(s.TopK, 1) && s.TopK != 0 {
				t.Errorf("%s: %d candidates over TopK", label, len(r.Candidates))
			}
			for _, c := range r.Candidates {
				if c.MessageID == "" {
					t.Errorf("%s: candidate with no message ID", label)
				}
			}
			// Candidates must be ordered by probability, ties by ordinal.
			for k := 1; k < len(r.Candidates); k++ {
				if r.Candidates[k].Probability > r.Candidates[k-1].Probability+1e-12 {
					t.Errorf("%s: candidates out of order at %d", label, k)
					break
				}
			}
		}
	}
}

// TestPropertyUniqueLineAlwaysRecoverable is the retrieval-side property: whatever
// OCR noise is applied to a unique line, it stays the top candidate or the ranking
// abstains — it must never silently return a *different* line with confidence.
// That is the property the runtime's safety argument rests on; recall is tunable,
// false confidence is not.
func TestPropertyUniqueLineAlwaysRecoverable(t *testing.T) {
	msgs, edges := miniScript()
	idx := mustBuild(t, msgs, edges)
	rng := rand.New(rand.NewSource(31337))
	const trials = 40
	for _, id := range []string{"m00", "m02", "m08", "m14", "m23", "m28", "m30", "m38", "m39"} {
		base := msgs[ordinalOf(id)].NormalizedText
		for k := 0; k < trials; k++ {
			text := corrupt(rng, base)
			r := idx.Rank(Observation{Text: text, OCRConf: 0.6 + rng.Float64()*0.4,
				SceneID: msgs[ordinalOf(id)].SceneID, RecentMessageIDs: recentOf(msgs, id)}, RankOptions{})
			if r.Abstain {
				continue // honest
			}
			if topID(r) != id {
				t.Errorf("id %s: confidently matched %s for corrupted text %q: %s",
					id, topID(r), text, rankingString(r))
			}
		}
	}
}

// corrupt applies a plausible OCR damage profile: glyph confusions the fold knows
// about, a dropped character, a substituted character, lost punctuation, and
// occasional truncation as if the box were mid-reveal.
func corrupt(rng *rand.Rand, s string) string {
	r := []rune(s)
	n := rng.Intn(4) + 1
	for i := 0; i < n && len(r) > 0; i++ {
		pos := rng.Intn(len(r))
		switch rng.Intn(5) {
		case 0:
			r[pos] = []rune("01rn")[rng.Intn(4)]
		case 1:
			r = append(r[:pos], r[pos+1:]...)
		case 2:
			r[pos] = []rune("aeiounstrlh")[rng.Intn(11)]
		case 3: // duplicate a character, the classic double-exposure artifact
			r = append(r[:pos], append([]rune{r[pos]}, r[pos:]...)...)
		case 4:
			r = r[:pos] // still revealing
		}
	}
	out := string(r)
	if out == "" {
		return " " // total dropout: the OCR returned nothing for a line that was there
	}
	if rng.Intn(3) == 0 {
		out = strings.ToUpper(out[:minInt(len(out), 1)]) + out[1:]
	}
	if rng.Intn(4) == 0 {
		out = out[:maxInt(0, len(out)-1)]
	}
	return out
}

func noisyObservations(rng *rand.Rand, msgs []games.Message, n int) []Observation {
	out := make([]Observation, 0, n)
	for i := 0; i < n; i++ {
		m := msgs[rng.Intn(len(msgs))]
		var text string
		switch rng.Intn(6) {
		case 0:
			text = m.NormalizedText
		case 1: // a revealing prefix
			r := []rune(m.NormalizedText)
			text = string(r[:rng.Intn(len(r))+1])
		case 2:
			text = corrupt(rng, m.NormalizedText)
		case 3:
			text = randomWord(rng, rng.Intn(60))
		case 4:
			text = strings.Repeat(randomWord(rng, 5), rng.Intn(30))
		default:
			text = strings.Join([]string{randomGibberish(rng), randomGibberish(rng)}, " ")
		}
		o := Observation{
			Text:             text,
			OCRConf:          pickConfidence(rng),
			IsPrefix:         rng.Intn(2) == 0,
			SceneID:          m.SceneID,
			RecentMessageIDs: recentOf(msgs, m.ID),
			TextKindHint:     pickKind(rng),
			Edition:          pickEdition(rng),
		}
		if rng.Intn(5) == 0 {
			o.SceneID = "scene.not.in.corpus"
		}
		if rng.Intn(7) == 0 {
			o.Text = strings.Repeat("x", 5000)
		}
		out = append(out, o)
	}
	return out
}

// pickConfidence includes the non-numbers a real capture pipeline produces: the
// zero value from a fixture that has none, a NaN from a division by a word count
// that turned out to be zero, and an over-100% confidence from an engine that
// reports a percentage.
func pickConfidence(rng *rand.Rand) float64 {
	switch rng.Intn(8) {
	case 0:
		return 0
	case 1:
		return math.NaN()
	case 2:
		return math.Inf(1)
	case 3:
		return -1
	case 4:
		return 92.5 // percent, not a fraction
	default:
		return rng.Float64()
	}
}

func pickKind(rng *rand.Rand) string {
	kinds := []string{"", "dialogue", "sign", "item", "menu", "nonsense"}
	return kinds[rng.Intn(len(kinds))]
}

func pickEdition(rng *rand.Rand) string {
	e := []string{"", "demo", "retail", "pal"}
	return e[rng.Intn(len(e))]
}

func randomGibberish(rng *rand.Rand) string {
	// Control characters, combining accents, an astral emoji and a byte order mark:
	// the alphabet a capture pipeline actually emits when it misreads a border.
	chars := []rune("ab.!?\t\n01\u00bb\u00bf\x00\u00e9\U0001F600")
	n := rng.Intn(12)
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteRune(chars[rng.Intn(len(chars))])
	}
	return b.String()
}

// recentOf returns the messages preceding id in the mini script, newest first, so
// property observations exercise the graph path instead of only the text path.
func recentOf(msgs []games.Message, id string) []string {
	o := ordinalOf(id)
	if o == 0 {
		return nil
	}
	start := o - 4
	if start < 0 {
		start = 0
	}
	out := make([]string, 0, o-start)
	for i := o - 1; i >= start; i-- {
		out = append(out, msgs[i].ID)
	}
	return out
}

func ordinalOf(id string) int {
	var n int
	for _, c := range id {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		}
	}
	return n
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
