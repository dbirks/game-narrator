package profileio

import (
	"fmt"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/store"
)

// TestSearchScaleAtTenThousandMessages is docs/spec/16 M1's "search 10k synthetic
// messages without memory blowup", measured rather than asserted in the abstract.
//
// Two numbers are reported and bounded: the p95 substring-query latency over 200
// queries, and the Go heap growth across the whole search phase. The second is
// the one the acceptance criterion actually names — an appliance with 4 GB and a
// live emulator attached may not answer a search box by copying its index into
// memory — and it is read with a GC in front of it, so what is measured is what
// stays allocated, not what the allocator happened to be holding (AGENTS.md
// "Keep runtime predictable", docs/spec/02 "memory-efficient prefix index").
func TestSearchScaleAtTenThousandMessages(t *testing.T) {
	rows, queryCount, limit := 10_000, 200, 20
	if raceEnabled {
		// Race instrumentation slows the FTS trigger path by roughly 25x, which
		// is a property of the detector and not of the profile store. The race
		// gate keeps a corpus large enough to exercise every concurrent code path
		// and reports the latency/memory benchmark as NOT RUN; see
		// race_on_test.go.
		rows, queryCount = 1_000, 60
		t.Log("NOT RUN under -race: the p95 latency and heap-growth benchmark. Run `go test -count=1 -run Search ./internal/profileio/` for the uninstrumented measurement docs/spec/16 asks for.")
	}
	e := newEnv(t, 15*time.Minute)
	g := e.game("synthetic-tide-scale", "Synthetic Tide")
	p := e.draft(e.edition(g, "US", "en").ID)

	// One registered source of record, so the corpus is a realistic revision
	// (messages with provenance and source-side identities) rather than rows
	// inserted into a vacuum.
	source := games.Source{
		ID: games.NewID(), ProfileID: p.ID, SourceType: games.SourceManual,
		URI: "synthetic://generated-corpus", RetrievedAt: nowStamp(),
		Rights: games.RightsUserSupplied, Trust: 1, Parser: "profileio.test@1.0.0",
		CoverageNotes: fmt.Sprintf("deterministic synthetic corpus, %d messages", rows),
	}
	if err := e.store.PutSources(e.ctx, p.ID, []games.Source{source}); err != nil {
		t.Fatalf("PutSources: %v", err)
	}
	msgs, pages := syntheticCorpus(p.ID, source.ID, rows)
	if !games.ValidID(msgs[0].ID) {
		t.Fatalf("corpus ids must be valid ULIDs, got %q", msgs[0].ID)
	}
	insertStarted := time.Now()
	for start := 0; start < len(msgs); start += ImportBatchSize {
		end := min(start+ImportBatchSize, len(msgs))
		batchPages := pagesFor(msgs[start:end], pages)
		if err := e.store.PutMessages(e.ctx, p.ID, msgs[start:end], batchPages); err != nil {
			t.Fatalf("PutMessages batch %d: %v", start, err)
		}
	}
	t.Logf("inserted %d messages in %s", rows, time.Since(insertStarted).Round(time.Millisecond))
	if got := countMessages(t, e, p.ID); got != rows {
		t.Fatalf("stored %d messages, generated %d", got, rows)
	}

	// 200 queries drawn from the same invented vocabulary the corpus was built
	// from, so every one of them matches something. The mix matters: three-rune
	// queries take the LIKE prefix path (internal/store/repo_search.go explains
	// why the trigram index has nothing below three) and longer ones take FTS5
	// with bm25 ranking, which is the path a 10 000-line profile would otherwise
	// blow up on.
	queryList := searchQueries(queryCount, rows)
	// Warm-up: the first query against a freshly built index pays for pages the
	// cache has never held, which is a storage-warmth measurement rather than a
	// search one. docs/spec/12 reports latency percentiles of the steady state;
	// these runs are discarded, and the phase that is measured starts clean.
	for _, q := range queryList[:min(10, len(queryList))] {
		if _, err := e.store.SearchMessages(e.ctx, p.ID, q, limit); err != nil {
			t.Fatalf("warm-up SearchMessages(%q): %v", q, err)
		}
	}

	results := 0
	latencies := make([]time.Duration, 0, len(queryList))
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	searchStarted := time.Now()
	for _, q := range queryList {
		at := time.Now()
		hits, err := e.store.SearchMessages(e.ctx, p.ID, q, limit)
		if err != nil {
			t.Fatalf("SearchMessages(%q): %v", q, err)
		}
		latencies = append(latencies, time.Since(at))
		results += len(hits)
		if len(hits) == 0 {
			t.Errorf("query %q matched nothing in a corpus built from its own vocabulary", q)
		}
		for _, hit := range hits {
			if hit.Message.ProfileID != p.ID {
				t.Errorf("search leaked a message from another revision: %s", hit.Message.ID)
			}
			if !strings.Contains(strings.ToLower(hit.Message.OriginalText), strings.ToLower(q)) {
				t.Errorf("query %q returned %q, which does not contain it", q, hit.Message.OriginalText)
			}
		}
	}
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	p50, p95 := percentile(latencies, 0.50), percentile(latencies, 0.95)
	heapGrowth := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	t.Logf("searched %d queries over %d messages in %s: %d hits total", len(queryList), rows, time.Since(searchStarted).Round(time.Millisecond), results)
	t.Logf("query latency p50=%s p95=%s max=%s (rows=%d queries=%d)", p50.Round(time.Microsecond), p95.Round(time.Microsecond), maxOf(latencies).Round(time.Microsecond), rows, len(queryList))
	t.Logf("heap growth across the search phase: %s (before %s, after %s); measured p95=%.2fms heap=%.2fMiB",
		mibString(heapGrowth), mibString(int64(before.HeapAlloc)), mibString(int64(after.HeapAlloc)),
		p95.Seconds()*1000, float64(heapGrowth)/(1<<20))

	// The two M1 limits. A margin this large on latency is not timidity: the
	// benchmark is "a search box must feel instant on a 4 GB arm64 box", and this
	// run is the *upper* bound on a dev host, not the release target.
	if !raceEnabled && p95 >= 50*time.Millisecond {
		t.Errorf("p95 substring query latency = %s, must stay below 50ms", p95)
	}
	if raceEnabled && p95 >= 5*time.Second {
		t.Errorf("p95 substring query latency = %s; even instrumented, a search must not take seconds", p95)
	}
	if heapGrowth >= 32<<20 {
		t.Errorf("heap grew by %s during the search phase, must stay below 32 MiB", mibString(heapGrowth))
	}

	// Pagination over the whole corpus: docs/spec/12's "10k messages pagination".
	// Every row exactly once, which a cursor built on a moving window would get
	// wrong in a way no single page could show.
	seen := map[string]int{}
	cursor := ""
	pagesRead := 0
	for {
		page, next, err := e.store.ListMessages(e.ctx, p.ID, store.MessageFilter{}, cursor, 200)
		if err != nil {
			t.Fatalf("ListMessages: %v", err)
		}
		pagesRead++
		if len(page) > 200 {
			t.Fatalf("a page of %d rows exceeded the requested limit", len(page))
		}
		for _, m := range page {
			seen[m.ID]++
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if len(seen) != rows {
		t.Errorf("pagination visited %d distinct messages of %d", len(seen), rows)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("message %s was visited %d times by the cursor", id, n)
			break
		}
	}
	t.Logf("walked %d messages in %d pages of at most 200", rows, pagesRead)
}

// searchQueries builds n deterministic queries out of the corpus itself: real
// substrings of generated lines, at four different lengths and offsets, so every
// query matches and the mix still covers both retrieval paths — three runes
// (internal/store/repo_search.go sends those to the (profile_id, normalized_text)
// LIKE prefix index, because the trigram tokenizer has nothing to look up below
// three) and longer fragments, which take FTS5 with bm25 ranking.
//
// The seeds are the same linear congruential walk the corpus generator uses, so
// asking for a query is asking for a line that is definitely in the revision.
func searchQueries(n, corpusRows int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		row := (i*37 + 11) % corpusRows
		text := strings.ToLower(syntheticMessage(uint64(row) + 1))
		word := inventedVocabulary[(i*7)%len(inventedVocabulary)]
		switch i % 4 {
		case 0:
			// A whole invented word, the shape an OCR fragment usually is.
			out = append(out, word)
		case 1:
			// A cross-word fragment: substring search must survive word
			// boundaries, which is what the trigram index is for.
			at := strings.Index(text, word)
			if at < 0 {
				at = len(text) / 3
			}
			out = append(out, sliceFrom(text, at, 9))
		case 2:
			// The shortest query the FTS path can answer, and the boundary case
			// for the prefix path.
			out = append(out, sliceFrom(text, len(text)/2, 3))
		default:
			// A long fragment spanning several words.
			out = append(out, sliceFrom(text, len(text)/4, 24))
		}
	}
	return out
}

// sliceFrom is a clamped substring: a query that runs off the end of a line is
// still a query that line contains.
func sliceFrom(s string, from, n int) string {
	if from < 0 {
		from = 0
	}
	if from >= len(s) {
		from = len(s) - n - 1
	}
	if from < 0 {
		from = 0
	}
	to := from + n
	if to > len(s) {
		to = len(s)
	}
	return s[from:to]
}

func percentile(d []time.Duration, q float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), d...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	at := int(float64(len(sorted)-1) * q)
	return sorted[at]
}

func maxOf(d []time.Duration) time.Duration {
	var m time.Duration
	for _, v := range d {
		if v > m {
			m = v
		}
	}
	return m
}

func mibString(n int64) string {
	return fmt.Sprintf("%.2f MiB", float64(n)/(1<<20))
}
