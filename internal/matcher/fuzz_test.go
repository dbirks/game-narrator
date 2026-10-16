package matcher

import (
	"strings"
	"sync"
	"testing"
)

// FuzzRank is the docs/spec/12 requirement "candidate ranking's NaN/infinite
// values" plus the general "no panics" bar. The index is built once, outside the
// fuzz loop: what varies is the observation, which is the input an attacker or a
// broken OCR controls.
//
// Invariants asserted per input:
//   - no panic at any length, including inputs the corpus cannot spell by hand
//     (control characters, invalid UTF-8, megabyte strings);
//   - every returned number finite and inside its documented range;
//   - probabilities over the returned set sum to 1;
//   - the ranking is reproducible for the same input, so no call path can depend
//     on map order or on residue from a previous query.
func FuzzRank(f *testing.F) {
	var once sync.Once
	var shared *Index
	ensure := func(t *testing.T) *Index {
		t.Helper()
		once.Do(func() {
			msgs, edges := miniScript()
			big, bigEdges := bigCorpus(1500)
			built, err := Build(append(msgs, big...), append(edges, bigEdges...), IndexOptions{})
			if err != nil {
				panic(err)
			}
			shared = built
		})
		return shared
	}

	seeds := []string{
		"",
		" ",
		"\t\n\v\f\r",
		"Yes.",
		"No.",
		"Thank you.",
		"We sail for the Gull-King's rock on the morning tide.",
		"we sail for the gull-king's rock on the rnorning tide!",
		"\x00\x01\x02\x7f",
		"\xff\xfe\x80",
		strings.Repeat("a", 100000),
		strings.Repeat("é\U0001F600", 20000),
		strings.Repeat("0o1li rn", 3000),
		"{player}",
		strings.Repeat("the ", 5000),
		"the tidewatcher counts the boats until the count stops mattering ton",
	}
	for _, s := range seeds {
		f.Add(s, float64(0.9), true, "scene.isle", "dialogue", "demo")
	}

	f.Fuzz(func(t *testing.T, text string, conf float64, isPrefix bool, scene, kind, edition string) {
		i := ensure(t)
		obs := Observation{
			Text: text, OCRConf: conf, IsPrefix: isPrefix,
			SceneID: scene, TextKindHint: kind, Edition: edition,
			RecentMessageIDs: []string{"m00", "m01", "m02", "m03", "m04", "m05",
				"m06", "m07", "m08", "m09", "m10", "m11", "m12"},
		}
		// Two option profiles: the defaults, and the tightest work bounds the
		// options accept, so a cap cannot be quietly exceeded.
		first := i.Rank(obs, RankOptions{})
		assertFiniteRanking(t, "default options", first)
		if got := rankingString(i.Rank(obs, RankOptions{})); got != rankingString(first) {
			t.Fatalf("non-deterministic ranking for %q:\n %s\n %s", truncate(text, 60), got, rankingString(first))
		}
		second := i.Rank(obs, RankOptions{TopK: 50, RetrievalK: 1, MaxPostingWork: 1, MaxPrefixScan: 1, MaxRecent: 0})
		assertFiniteRanking(t, "tight bounds", second)
		if len(second.Candidates) > 50 {
			t.Fatalf("TopK exceeded: %d", len(second.Candidates))
		}
	})
}
