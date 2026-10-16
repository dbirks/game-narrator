package matcher

import (
	"fmt"
	"math/rand"
	"runtime"
	"sort"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/games"
)

// The gates below are docs/spec/16 M2's ("10k-message index build < 2 s and
// < 64 MiB heap growth, Rank p95 < 5 ms over 500 noisy queries"), run against the
// synthetic 10k corpus rather than a real profile. They are the reason the index is
// built out of int32 ordinals and hashed trigrams instead of the easier
// map[string][]struct layout.
//
// The timing and memory assertions only apply in the uninstrumented build; see
// scale_race_test.go. A device with 4 GB and four cores is the target, and a
// latency budget that the dev host only just meets is a bug on the device, so the
// numbers logged here should be read as an upper bound for UNO Q acceptance rather
// than as the acceptance itself (docs/spec/12 "Benchmark CPU/RSS/temp on UNO Q").

func TestScaleBuildWithinBudget(t *testing.T) {
	skipUnderShort(t)
	msgs, edges := bigCorpus(10000)
	if n := len(msgs); n != 10000 {
		t.Fatalf("corpus is %d messages, want 10000", n)
	}

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	start := time.Now()
	idx, err := Build(msgs, edges, IndexOptions{})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	growth := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	if growth < 0 {
		growth = 0 // a concurrent GC during the build can only make this look smaller
	}
	st := idx.Stats()
	t.Logf("build=%s for %d messages (%d edges); heap growth=%.2f MiB (self-reported index=%.2f MiB)",
		elapsed.Round(time.Millisecond), st.Messages, st.Edges,
		float64(growth)/(1<<20), float64(st.ApproxBytes)/(1<<20))
	t.Logf("index: keys=%d trigrams=%d postings=%d", st.Keys, st.Trigrams, st.Postings)

	if timingAsserted {
		if elapsed > 2*time.Second {
			t.Errorf("build took %s, gate is 2 s", elapsed)
		}
		if growth > 64<<20 {
			t.Errorf("heap growth %.1f MiB, gate is 64 MiB", float64(growth)/(1<<20))
		}
	}
	// Sanity: the postings must be roughly (distinct trigrams × average fan-out),
	// and if they were stored as strings this would be several times the budget.
	if st.Postings < st.Messages {
		t.Errorf("only %d postings for %d messages: the trigram index is not populated",
			st.Postings, st.Messages)
	}
}

func TestScaleRankLatency(t *testing.T) {
	skipUnderShort(t)
	msgs, edges := bigCorpus(10000)
	idx := mustBuild(t, msgs, edges)

	rng := rand.New(rand.NewSource(1729))
	queries := noisyObservations(rng, msgs, 500)
	// A second, harsher slice of the distribution: every query is a corrupted
	// version of a real line, so retrieval always finds rivals and always runs the
	// full scoring pass. The easy case (a unique exact hit short-circuiting work) is
	// not what a p95 budget is for.
	for i := 0; i < 250; i++ {
		m := msgs[rng.Intn(len(msgs))]
		queries = append(queries, Observation{
			Text:             corrupt(rng, m.NormalizedText),
			OCRConf:          0.6 + rng.Float64()*0.39,
			SceneID:          m.SceneID,
			RecentMessageIDs: recentOf(msgs, m.ID),
		})
	}

	samples := make([]time.Duration, 0, len(queries))
	var totalCandidates int
	for i, q := range queries {
		// Recent ids from the big corpus need real IDs; recentOf only understands
		// the mini script's, so synthesize graph context from the chain instead.
		q.RecentMessageIDs = recentBig(msgs, i)
		start := time.Now()
		r := idx.Rank(q, RankOptions{})
		samples = append(samples, time.Since(start))
		totalCandidates += len(r.Candidates)
		assertFiniteRanking(t, fmt.Sprintf("scale query %d", i), r)
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	p50 := samples[len(samples)/2]
	p95 := samples[int(float64(len(samples))*0.95)]
	if int(float64(len(samples))*0.95) >= len(samples) {
		p95 = samples[len(samples)-1]
	}
	worst := samples[len(samples)-1]
	mean := time.Duration(sum(samples) / int64(len(samples)))
	t.Logf("queries=%d p50=%s p95=%s max=%s mean=%s avg candidates=%.1f",
		len(samples), p50.Round(time.Microsecond), p95.Round(time.Microsecond),
		worst.Round(time.Microsecond), mean.Round(time.Microsecond),
		float64(totalCandidates)/float64(len(samples)))
	if !timingAsserted {
		t.Log("p95 gate (5 ms) NOT ASSERTED under -race; run without instrumentation")
		return
	}
	if p95 > 5*time.Millisecond {
		t.Errorf("p95 = %s over %d queries, gate is 5 ms (max %s)", p95, len(samples), worst)
	}
}

// TestScaleNoPerQueryMemoryGrowth catches the failure mode the caps are supposed to
// prevent: a query whose cost scales with the corpus rather than with the bounds.
func TestScaleNoPerQueryMemoryGrowth(t *testing.T) {
	skipUnderShort(t)
	small, smallEdges := bigCorpus(2000)
	big, bigEdges := bigCorpus(10000)
	smallIdx := mustBuild(t, small, smallEdges)
	bigIdx := mustBuild(t, big, bigEdges)

	rng := rand.New(rand.NewSource(5150))
	q := noisyObservations(rng, big, 50)
	for i := range q {
		q[i].RecentMessageIDs = nil
	}

	smallAllocs := testing.AllocsPerRun(50, func() {
		for _, o := range q[:10] {
			smallIdx.Rank(o, RankOptions{})
		}
	})
	bigAllocs := testing.AllocsPerRun(50, func() {
		for _, o := range q[:10] {
			bigIdx.Rank(o, RankOptions{})
		}
	})
	t.Logf("allocs per 10 queries: 2k-corpus=%.0f 10k-corpus=%.0f", smallAllocs, bigAllocs)
	// Per-query work is bounded by the options, so the 5× larger corpus may cost a
	// little more (the accumulator slice is sized by message count) but not 5×.
	if bigAllocs > smallAllocs*1.5+400 {
		t.Errorf("per-query allocations scaled with the corpus: %.0f at 10k vs %.0f at 2k", bigAllocs, smallAllocs)
	}
}

// skipUnderShort keeps `go test -short` a fast loop for a human editing the
// package; the scale gates are then run by name, uninstrumented, which is where
// they mean something (docs/spec/16 M2).
func skipUnderShort(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("scale gate: skipped under -short; run without it (and without -race) to assert the M2 limits")
	}
}

func recentBig(msgs []games.Message, i int) []string {
	if i == 0 {
		return nil
	}
	out := make([]string, 0, 3)
	for k := 1; k <= 3; k++ {
		idx := i - k
		if idx <= 0 || idx >= len(msgs) {
			continue
		}
		out = append(out, msgs[idx].ID)
	}
	return out
}

func sum(d []time.Duration) int64 {
	var n int64
	for _, v := range d {
		n += int64(v)
	}
	return n
}
