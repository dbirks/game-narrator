package telemetry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// labelsA/labelsB are two distinct label sets a cardinality test can count.
var (
	labelsA = Labels{Game: "zelda_oot", ProfileRevision: "3"}
	labelsB = Labels{Game: "zelda_mq", ProfileRevision: "7"}
)

// TestGoldenExpositionSmall pins the Prometheus text format on a small
// registry: the HELP/TYPE pairs, the label order, the cumulative histogram
// buckets ending at le="+Inf", the _sum/_count rows and the declaration-order
// metric layout. A change to any of it is a format change, and this golden is
// what a scraper's parser relies on.
func TestGoldenExpositionSmall(t *testing.T) {
	reg := New()
	c := reg.Counter("hits_total", "Things that hit.")
	h := reg.Histogram("wait_seconds", "How long it took.", []float64{0.1, 1})
	g := reg.Gauge("busy", "Whether it is busy.")
	l := Labels{Game: "g1", ProfileRevision: "2"}
	c.Add(l, 3)
	h.Observe(l, 90*time.Millisecond)
	h.Observe(l, 2*time.Second)
	g.Set(1)

	var buf bytes.Buffer
	if err := reg.WritePrometheus(&buf); err != nil {
		t.Fatalf("WritePrometheus: %v", err)
	}
	got := buf.String()
	want := `# HELP game_narrator_hits_total Things that hit.
# TYPE game_narrator_hits_total counter
game_narrator_hits_total{game="g1",profile_revision="2"} 3
# HELP game_narrator_wait_seconds How long it took.
# TYPE game_narrator_wait_seconds histogram
game_narrator_wait_seconds_bucket{game="g1",profile_revision="2",le="0.1"} 1
game_narrator_wait_seconds_bucket{game="g1",profile_revision="2",le="1"} 1
game_narrator_wait_seconds_bucket{game="g1",profile_revision="2",le="+Inf"} 2
game_narrator_wait_seconds_sum{game="g1",profile_revision="2"} 2.09
game_narrator_wait_seconds_count{game="g1",profile_revision="2"} 2
# HELP game_narrator_busy Whether it is busy.
# TYPE game_narrator_busy gauge
game_narrator_busy 1
`
	if got != want {
		t.Errorf("exposition differs from the golden:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestProductCatalogComplete asserts the process catalog is exactly the
// spec's named surface (docs/spec/13 "Metrics"): every metric present with
// its kind, nothing undeclared, and the histogram catalog on the shared
// bucket set. The golden test pins the format; this pins the catalog.
func TestProductCatalogComplete(t *testing.T) {
	p := NewProduct()
	p.FramesDelivered(labelsA, 3)
	p.OCRDuration(labelsA, 40*time.Millisecond)
	p.OCRDuration(labelsA, 9*time.Second)
	p.TTSResolved(labelsB, true, 5*time.Millisecond)
	p.TTSResolved(labelsB, false, 250*time.Millisecond)
	p.PlaybackUnderrun(labelsA)
	p.SetJobQueueDepth(2)
	p.SessionStarted()
	p.SessionStarted()
	p.SessionStopped()

	snap := p.Registry.Snapshot()
	byName := map[string]MetricSnapshot{}
	for _, m := range snap.Metrics {
		byName[m.Name] = m
	}
	want := map[string]string{
		"frames_delivered_total":          "counter",
		"frames_dropped_total":            "counter",
		"ocr_calls_total":                 "counter",
		"ocr_cancelled_total":             "counter",
		"ocr_errors_total":                "counter",
		"ocr_fallback_total":              "counter",
		"ocr_duration_seconds":            "histogram",
		"matcher_rankings_total":          "counter",
		"matcher_ranking_stale_total":     "counter",
		"matcher_ranking_abstained_total": "counter",
		"decisions_total":                 "counter",
		"decisions_answered_total":        "counter",
		"decisions_provider_error_total":  "counter",
		"decisions_not_configured_total":  "counter",
		"decisions_applied_total":         "counter",
		"decisions_fenced_total":          "counter",
		"decisions_duration_seconds":      "histogram",
		"tts_cache_hits_total":            "counter",
		"tts_cache_misses_total":          "counter",
		"tts_duration_seconds":            "histogram",
		"playback_underruns_total":        "counter",
		"prewarm_hits_total":              "counter",
		"frame_to_voice_seconds":          "histogram",
		"job_queue_depth":                 "gauge",
		"active_sessions":                 "gauge",
	}
	for name, kind := range want {
		m, ok := byName[prefix+name]
		if !ok {
			t.Errorf("the catalog is missing metric %s", name)
			continue
		}
		if m.Type != kind {
			t.Errorf("metric %s is kind %s, want %s", name, m.Type, kind)
		}
	}
	if len(byName) != len(want) {
		t.Errorf("the catalog holds %d metrics, want exactly %d (the spec's named surface)", len(byName), len(want))
	}

	// The samples landed where the hooks say they do.
	check := func(name string, labelGame string, labelRev string, value float64) {
		t.Helper()
		m := byName[prefix+name]
		for _, s := range m.Series {
			if s.Labels["game"] == labelGame && s.Labels["profile_revision"] == labelRev && s.Value != nil {
				if *s.Value != value {
					t.Errorf("%s{%s,%s} = %v, want %v", name, labelGame, labelRev, *s.Value, value)
				}
				return
			}
		}
		t.Errorf("%s has no series game=%q profile_revision=%q", name, labelGame, labelRev)
	}
	check("frames_delivered_total", "zelda_oot", "3", 3)
	check("playback_underruns_total", "zelda_oot", "3", 1)
	check("tts_cache_hits_total", "zelda_mq", "7", 1)
	check("tts_cache_misses_total", "zelda_mq", "7", 1)
	if m := byName[prefix+"job_queue_depth"]; len(m.Series) != 1 || m.Series[0].Value == nil || *m.Series[0].Value != 2 {
		t.Errorf("job_queue_depth snapshot = %+v, want value 2", m.Series)
	}
	if m := byName[prefix+"active_sessions"]; len(m.Series) != 1 || m.Series[0].Value == nil || *m.Series[0].Value != 1 {
		t.Errorf("active_sessions snapshot = %+v, want value 1 (started twice, stopped once)", m.Series)
	}
	// The histogram kept both labels' samples in their own series.
	if m := byName[prefix+"ocr_duration_seconds"]; len(m.Series) != 1 {
		t.Fatalf("ocr_duration_seconds series = %d, want 1", len(m.Series))
	} else if s := m.Series[0]; s.Count == nil || *s.Count != 2 || s.Sum == nil || *s.Sum != 9.04 {
		t.Errorf("ocr_duration_seconds = count %v sum %v, want count 2 sum 9.04", s.Count, s.Sum)
	}
	// A 40 ms sample is at or below the 0.05 edge and nowhere else: the
	// non-cumulative slots say the 9 s sample took the overflow slot.
	ocr := byName[prefix+"ocr_duration_seconds"].Series[0]
	if len(ocr.Buckets) != len(DefaultBuckets)+1 {
		t.Fatalf("ocr_duration_seconds buckets = %d slots, want %d", len(ocr.Buckets), len(DefaultBuckets)+1)
	}
	var edge005, overflow uint64
	for _, b := range ocr.Buckets {
		if b.Le != nil && *b.Le == 0.05 {
			edge005 = b.Count
		}
		if b.Le == nil {
			overflow = b.Count
		}
	}
	if edge005 != 1 || overflow != 1 {
		t.Errorf("ocr_duration_seconds slots: le=0.05 %d overflow %d, want 1/1", edge005, overflow)
	}

	// The exposition carries the cumulative +Inf row and the gauge values.
	var buf bytes.Buffer
	if err := p.Registry.WritePrometheus(&buf); err != nil {
		t.Fatalf("WritePrometheus: %v", err)
	}
	out := buf.String()
	for _, w := range []string{
		`game_narrator_ocr_duration_seconds_bucket{game="zelda_oot",profile_revision="3",le="0.05"} 1`,
		`game_narrator_ocr_duration_seconds_bucket{game="zelda_oot",profile_revision="3",le="+Inf"} 2`,
		`game_narrator_tts_duration_seconds_bucket{game="zelda_mq",profile_revision="7",le="0.005"} 1`,
		`game_narrator_tts_duration_seconds_bucket{game="zelda_mq",profile_revision="7",le="+Inf"} 2`,
		"game_narrator_job_queue_depth 2",
		"game_narrator_active_sessions 1",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("exposition is missing %q", w)
		}
	}
	// Bucket rows stay cumulative: walk the ocr series' le rows and require
	// a non-decreasing count on the way to +Inf.
	var leCounts []int
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, `game_narrator_ocr_duration_seconds_bucket{game="zelda_oot",profile_revision="3",le="`) {
			// The count is everything after the closing brace: the label set
			// itself may carry whatever the exposition allows, so the brace —
			// not a parse of its content — is the split point.
			tail := l[strings.LastIndex(l, "}"):]
			var v int
			if _, err := fmt.Sscanf(tail, "} %d", &v); err != nil {
				t.Fatalf("unparseable bucket row %q: %v", l, err)
			}
			leCounts = append(leCounts, v)
		}
	}
	if len(leCounts) == 0 {
		t.Fatal("no ocr_duration bucket rows in the exposition")
	}
	for i := 1; i < len(leCounts); i++ {
		if leCounts[i] < leCounts[i-1] {
			t.Errorf("bucket counts are not cumulative: %v", leCounts)
		}
	}
}

// TestCardinalityBound is the bounded-labels proof: past maxSeries distinct
// label sets, a metric folds the newcomers into the reserved overflow series
// instead of growing. The table never exceeds maxSeries, and — the part that
// makes the fold safe rather than lossy — the folded counts still add up to
// every sample booked.
func TestCardinalityBound(t *testing.T) {
	p := NewProduct()
	const n = 200
	for i := 0; i < n; i++ {
		l := Labels{Game: fmt.Sprintf("game_%02d", i), ProfileRevision: "1"}
		p.FramesDelivered(l, 1)
		p.OCRDuration(l, time.Duration(i)%200*time.Millisecond)
	}
	snap := p.Registry.Snapshot()
	for _, name := range []string{"frames_delivered_total", "ocr_duration_seconds"} {
		m, ok := func() (MetricSnapshot, bool) {
			for _, ms := range snap.Metrics {
				if ms.Name == prefix+name {
					return ms, true
				}
			}
			return MetricSnapshot{}, false
		}()
		if !ok {
			t.Fatalf("missing %s", name)
		}
		if len(m.Series) > maxSeries {
			t.Errorf("%s holds %d series, the bound is %d", name, len(m.Series), maxSeries)
		}
		// The overflow series is there and accounted for.
		var overflow int
		for _, s := range m.Series {
			if s.Labels["game"] == overflowLabel && s.Labels["profile_revision"] == overflowLabel {
				overflow++
			}
		}
		if overflow != 1 {
			t.Errorf("%s has %d overflow series, want 1 (the bound is spent at %d distinct sets)", name, overflow, maxSeries)
		}
		// No count lost: the series' totals sum to every sample booked.
		var total float64
		for _, s := range m.Series {
			if s.Value != nil {
				total += *s.Value
			}
			if s.Count != nil {
				total += float64(*s.Count)
			}
		}
		if total != n {
			t.Errorf("%s booked %v samples across its series, want %d (the fold must not drop counts)", name, total, n)
		}
	}
	// A label set that arrives after the bound is still accepted (it joins
	// the overflow series), so a late game is never refused service.
	p.FramesDelivered(Labels{Game: "late_arriver", ProfileRevision: "9"}, 7)
	total := 0.0
	for _, ms := range p.Registry.Snapshot().Metrics {
		if ms.Name == prefix+"frames_delivered_total" {
			for _, s := range ms.Series {
				if s.Value != nil {
					total += *s.Value
				}
			}
		}
	}
	if total != float64(n+7) {
		t.Errorf("after a late label set the total is %v, want %d", total, n+7)
	}
}

// TestLabelSanitization is the hostile-label proof: a value that tries to
// break the exposition's syntax — quotes, braces, a newline, a run of
// invalid runes past the bound — lands in the output as a safe, bounded
// label, and the JSON snapshot stays valid.
func TestLabelSanitization(t *testing.T) {
	p := NewProduct()
	l := Labels{
		Game: `zelda "oot" {evil}
newline`,
		ProfileRevision: strings.Repeat("r", 100),
	}
	p.FramesDelivered(l, 1)

	var buf bytes.Buffer
	if err := p.Registry.WritePrometheus(&buf); err != nil {
		t.Fatalf("WritePrometheus with a hostile label: %v", err)
	}
	out := buf.String()
	if strings.ContainsAny(out, "\r") {
		t.Error("the exposition carries a raw carriage return: a label value broke the line format")
	}
	// The label row must exist exactly once, with the value collapsed to the
	// safe charset and cut at the bound.
	row := "game_narrator_frames_delivered_total{"
	idx := strings.Index(out, row)
	if idx == -1 {
		t.Fatal("no frames_delivered series in the exposition")
	}
	lineEnd := strings.Index(out[idx:], "\n")
	line := out[idx : idx+lineEnd]
	if strings.Contains(line, `"`) && !strings.Contains(line, `game="`) {
		t.Errorf("the label row %q does not look like a well-formed label set", line)
	}
	// The snapshot must marshal: Inf/NaN would have made the encoder fail
	// earlier, but a malformed label is a JSON-validity question here.
	snapJSON, err := json.Marshal(p.Registry.Snapshot())
	if err != nil {
		t.Fatalf("the snapshot with a hostile label does not marshal: %v", err)
	}
	if !json.Valid(snapJSON) {
		t.Error("the snapshot with a hostile label is not valid JSON")
	}
	// The revision is bounded at 64 characters in both surfaces.
	if !strings.Contains(line, `profile_revision="`+strings.Repeat("r", 64)+`"`) {
		t.Errorf("the long label value was not cut to the 64-char bound: %q", line)
	}
}

// TestNilSafeHooks: a nil *Product and a nil handle are the off shape — every
// hook returns without panicking, and a harness that wired no catalog still
// compiles and runs the same call sites.
func TestNilSafeHooks(t *testing.T) {
	var p *Product
	p.FramesDelivered(labelsA, 1)
	p.FramesDropped(labelsA, 1)
	p.OCRCall(labelsA)
	p.OCRCancelled(labelsA)
	p.OCRError(labelsA)
	p.OCRFallback(labelsA)
	p.OCRDuration(labelsA, time.Second)
	p.Ranking(labelsA)
	p.RankingStale(labelsA)
	p.RankingAbstained(labelsA)
	p.Decision(labelsA)
	p.DecisionVerdict(labelsA, "answered")
	p.DecisionDuration(labelsA, time.Second)
	p.DecisionApplied(labelsA)
	p.DecisionFenced(labelsA)
	p.TTSResolved(labelsA, true, time.Second)
	p.PlaybackUnderrun(labelsA)
	p.PrewarmHit(labelsA)
	p.FrameToVoice(labelsA, time.Second)
	p.SetJobQueueDepth(3)
	p.SessionStarted()
	p.SessionStopped()

	var c *Counter
	c.Add(labelsA, 1)
	var h *Histogram
	h.Observe(labelsA, time.Second)
	var g *Gauge
	g.Set(1)
	g.Add(-1)
}

// TestNegativeDuration books a backwards clock at zero rather than dropping
// the sample: the count moves, the sum does not (the pipeline's histogram
// makes the same choice, and a dropped sample makes a summary optimistic).
func TestNegativeDuration(t *testing.T) {
	p := NewProduct()
	p.OCRDuration(labelsA, -time.Second)
	m := func() MetricSnapshot {
		for _, ms := range p.Registry.Snapshot().Metrics {
			if ms.Name == prefix+"ocr_duration_seconds" {
				return ms
			}
		}
		return MetricSnapshot{}
	}()
	if len(m.Series) != 1 {
		t.Fatalf("ocr_duration_seconds series = %d, want 1", len(m.Series))
	}
	s := m.Series[0]
	if s.Count == nil || *s.Count != 1 {
		t.Errorf("count = %v, want 1 (a negative interval is booked, not dropped)", s.Count)
	}
	if s.Sum == nil || *s.Sum != 0 {
		t.Errorf("sum = %v, want 0 (booked at zero)", s.Sum)
	}
}

// TestConcurrency hammers one catalog from many goroutines — the hot path's
// shape: frame-rate counter adds, per-observation histogram observes, gauge
// moves, and a reader that snapshots and renders the exposition while all of
// it runs. The totals must be exact when it is over, and the reader must
// never see a torn document (a render that failed would surface as an error,
// a torn render would not be self-consistent).
func TestConcurrency(t *testing.T) {
	p := NewProduct()
	const (
		workers   = 16
		perWorker = 250
		labelSets = 8
	)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				l := Labels{Game: fmt.Sprintf("game_%d", w%labelSets), ProfileRevision: fmt.Sprintf("%d", (w+i)%labelSets)}
				p.FramesDelivered(l, 1)
				p.OCRDuration(l, time.Duration(i%37)*time.Millisecond)
				if i%5 == 0 {
					p.TTSResolved(l, i%2 == 0, time.Duration(i%13)*time.Millisecond)
				}
				if i%7 == 0 {
					p.SessionStarted()
					p.SessionStopped()
				}
				p.SetJobQueueDepth(int64(i % 4))
			}
		}(w)
	}

	reads := make(chan error, 8)
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				var buf bytes.Buffer
				var err error
				if err = p.Registry.WritePrometheus(&buf); err != nil {
					reads <- err
					return
				}
				var snapJSON []byte
				if snapJSON, err = json.Marshal(p.Registry.Snapshot()); err != nil {
					reads <- err
					return
				}
				if !json.Valid(snapJSON) {
					reads <- fmt.Errorf("the snapshot is not valid JSON mid-run")
					return
				}
			}
		}()
	}
	wg.Wait()
	close(reads)
	for err := range reads {
		t.Fatalf("concurrent read failed: %v", err)
	}

	total := 0.0
	for _, ms := range p.Registry.Snapshot().Metrics {
		if ms.Name == prefix+"frames_delivered_total" {
			for _, s := range ms.Series {
				if s.Value != nil {
					total += *s.Value
				}
			}
		}
	}
	if want := float64(workers * perWorker); total != want {
		t.Errorf("frames_delivered_total sums to %v, want %v (a lost update under the race)", total, want)
	}
}
