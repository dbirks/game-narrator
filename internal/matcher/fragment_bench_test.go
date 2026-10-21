package matcher

// Fragment latency benchmarks at the board's corpus scale. The 2026-10-21
// evidence (docs/adr/0151) is a real profile with ~4300 messages, so the
// invented fragment corpus (fragment_test.go) is scaled to 4400 messages
// here. The board CPU is slow, and the task budget is <= 15 ms per query on
// this host; the stricter 5 ms p95 gate over the 10k corpus (scale_test.go)
// is the release gate these numbers must stay under in the noisy-read
// distribution.

import (
	"math/rand"
	"testing"

	"github.com/dbirks/game-narrator/internal/games"
)

// fragBenchQueries invents n fragment-shaped reads: exact 25-40 character
// windows from the middle of corpus lines (the board's read width), plus
// every fourth one with the board's damage profile (a leading "| " and one
// junk trailing character). Deterministic by seed so before/after runs
// measure the same queries.
func fragBenchQueries(idx *Index, msgs []games.Message, n int) []Observation {
	rng := rand.New(rand.NewSource(4400))
	qs := make([]Observation, 0, n)
	for i := 0; i < n; i++ {
		m := msgs[rng.Intn(len(msgs))]
		key := []rune(idx.Fold(m.NormalizedText))
		length := 25 + rng.Intn(16)
		start := rng.Intn(len(key) - length)
		text := string(key[start : start+length])
		if i%4 == 3 {
			text = "| " + text + "t"
		}
		qs = append(qs, Observation{Text: text, OCRConf: 0.9})
	}
	return qs
}

// BenchmarkFragmentRank4400 measures one Rank over a fragment read on the
// scaled corpus: build cost is excluded (ResetTimer after the queries exist),
// and the loop cycles the 32 queries so the reported ns/op is per query.
func BenchmarkFragmentRank4400(b *testing.B) {
	msgs, _ := fragCorpus(4400)
	idx, err := Build(msgs, nil, IndexOptions{})
	if err != nil {
		b.Fatalf("build: %v", err)
	}
	qs := fragBenchQueries(idx, msgs, 32)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := idx.Rank(qs[i%len(qs)], RankOptions{})
		assertFiniteRankingB(b, r)
	}
}

// assertFiniteRankingB is the benchmark twin of assertFiniteRanking: a
// benchmark that panics on a NaN or a negative probability is no faster than
// a test that reports it.
func assertFiniteRankingB(b *testing.B, r Ranking) {
	b.Helper()
	if r.Margin != r.Margin {
		b.Fatalf("NaN margin")
	}
	for i, c := range r.Candidates {
		if c.Probability < 0 || c.Probability > 1 {
			b.Fatalf("candidate %d probability %.3f outside [0,1]", i, c.Probability)
		}
	}
}
