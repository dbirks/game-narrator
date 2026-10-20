// Package telemetry is the process's metrics surface (docs/spec/13
// "Metrics", docs/spec/01 "internal/telemetry: structured events, metrics,
// tracing"): bounded counters, histograms and gauges that the hot paths book
// into, exposed two ways — the Prometheus text format for scrapers
// (GET /metrics, off by default) and a JSON snapshot for the diagnostics and
// health surfaces.
//
// Cardinality is bounded by construction, which is what makes a metrics
// surface safe on the 4-core appliance this product targets
// (docs/spec/13: "Aggregate by game/profile revisions but avoid
// high-cardinality trace IDs as metric labels"):
//
//   - A metric declares its label dimensions at declaration time. The
//     product catalog (product.go) declares exactly two — the game and the
//     profile revision the session runs against. No per-message, per-region
//     or per-utterance labels exist to declare.
//   - Every label value is sanitised to a bounded safe charset (see
//     sanitize): an untrusted value can widen a label, never break the
//     exposition's syntax or grow a series identity past 64 bytes.
//   - Each metric keeps at most maxSeries distinct series; a label set
//     arriving past the bound is folded into one reserved overflow series
//     instead of being dropped, so the count is never lost and the
//     exposition can never grow unboundedly no matter what a caller passes.
//
// Nothing here is a dependency of anything else in the tree (stdlib only),
// and the hooks a hot path calls are nil-safe: a nil *Product is the
// documented "off" shape, so a harness that wires no catalog keeps the same
// call sites and pays nothing.
package telemetry

import (
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// prefix namespaces every exported metric name.
const prefix = "game_narrator_"

// The two label dimensions the product catalog declares (docs/spec/13
// "aggregate by game/profile revisions"). A metric's declared label set is
// always a subset of these; nothing else may label a series.
const (
	LabelGame            = "game"
	LabelProfileRevision = "profile_revision"
)

// overflowLabel is the reserved value an overflow series carries: the label
// set a metric folds into when a new set arrives past its bound. It is a
// constant, not a label a caller is ever asked to supply, and it names the
// fact in the exposition: "here is what overflowed, aggregated".
const overflowLabel = "__overflow__"

// maxSeries bounds the distinct label sets one metric keeps. A live appliance
// runs one game at a time with a handful of published profile revisions, so
// the bound is far above product reality and far below the memory a
// per-message label would spend.
const maxSeries = 64

// Labels is one set of label values for a series. Zero means "not running
// against a named game/revision" and exposes as the literal value "none" —
// the honest shape, not an empty label that Prometheus readers would parse as
// a missing dimension.
type Labels struct {
	Game            string
	ProfileRevision string
}

// sanitize normalises one label value to the exposition's safe charset and
// bound. Anything outside [A-Za-z0-9._-] becomes '_' (a value that tried to
// inject a quote or a newline cannot, and a value that is just long is cut at
// the bound rather than spent), and the empty value becomes "none".
func sanitize(v string) string {
	if v == "" {
		return "none"
	}
	var b strings.Builder
	b.Grow(len(v))
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if s := b.String(); len(s) > 64 {
		return s[:64]
	}
	return b.String()
}

// labelPair is a label set's normalised, comparable form.
type labelPair struct{ game, revision string }

func (l Labels) pair() labelPair {
	return labelPair{sanitize(l.Game), sanitize(l.ProfileRevision)}
}

// isOverflow reports whether this is the reserved overflow pair.
func (p labelPair) isOverflow() bool {
	return p.game == overflowLabel && p.revision == overflowLabel
}

// metric is one named series family: its help text, its kind, the label
// dimensions it declared, and its bounded table of series.
type metric struct {
	name       string // full, prefixed name
	help       string
	kind       string // "counter" | "gauge" | "histogram"
	dimensions []string

	mu       sync.RWMutex
	counters map[string]*counterSeries
	hists    map[string]*histSeries
	// gauge is set only for kind "gauge": gauges are process-wide (the
	// product catalog declares no label dimensions for them), so one value
	// per metric rather than a table.
	gauge atomic.Int64
	// histogram bucket edges in seconds, ascending; a sample at or below an
	// edge lands in that edge's bucket, and the overflow slot is the last.
	buckets []float64
}

func (m *metric) key(p labelPair) string { return p.game + "\x00" + p.revision }

// counterSeries is one labelled counter.
type counterSeries struct {
	pair  labelPair
	value atomic.Uint64
}

// histSeries is one labelled histogram. The mutex is what lets a snapshot be
// taken mid-observe without stopping the hot path (the pipeline's own stats
// use the same shape, internal/pipeline/stats.go).
type histSeries struct {
	pair    labelPair
	mu      sync.Mutex
	count   uint64
	sumSecs float64
	edges   []float64 // the owning metric's edges, ascending
	buckets []uint64  // one per edge plus the overflow slot
}

// Registry is the collection of declared metrics. New builds one; the
// product catalog (NewProduct) declares the full set at process start so
// the exposition is complete from the first scrape rather than accreting as
// sessions happen to run.
type Registry struct {
	mu      sync.RWMutex
	metrics map[string]*metric
	order   []string
}

// New returns an empty Registry. Declared metrics appear in the exposition
// in declaration order.
func New() *Registry {
	return &Registry{metrics: make(map[string]*metric)}
}

// declare registers one metric and returns its handle. The name is namespaced
// with the prefix; the kind's accessor panics on a name the registry already
// holds under a different kind — a wiring bug at process start, and a panic
// there is the honest failure (a silently dropped metric would read as
// healthy for the life of the process). Re-declaring the same name and kind
// is idempotent and returns the existing handle, so independent packages can
// each declare their own metrics without a shared catalog.
func (r *Registry) declare(name, help, kind string, dims []string, buckets []float64) *metric {
	full := prefix + name
	r.mu.Lock()
	defer r.mu.Unlock()
	if m, ok := r.metrics[full]; ok {
		if m.kind != kind {
			panic("telemetry: metric " + full + " is declared as " + m.kind + ", not " + kind)
		}
		return m
	}
	m := &metric{name: full, help: help, kind: kind, dimensions: dims, buckets: buckets}
	switch kind {
	case "counter":
		m.counters = make(map[string]*counterSeries)
	case "histogram":
		m.hists = make(map[string]*histSeries)
	}
	r.metrics[full] = m
	r.order = append(r.order, full)
	return m
}

// Counter declares (or returns) a labelled counter metric. Label dimensions
// default to the product's two (game, profile_revision); pass none explicitly
// only for a process-wide counter.
func (r *Registry) Counter(name, help string, dims ...string) *Counter {
	if len(dims) == 0 {
		dims = []string{LabelGame, LabelProfileRevision}
	}
	return &Counter{m: r.declare(name, help, "counter", dims, nil)}
}

// Histogram declares (or returns) a labelled histogram in seconds.
// DefaultBuckets is the catalog's edge set (1 ms to 5 s, mirroring the
// pipeline's own latency table, internal/pipeline/stats.go).
func (r *Registry) Histogram(name, help string, buckets []float64, dims ...string) *Histogram {
	if len(buckets) == 0 {
		buckets = DefaultBuckets
	}
	if len(dims) == 0 {
		dims = []string{LabelGame, LabelProfileRevision}
	}
	return &Histogram{m: r.declare(name, help, "histogram", dims, buckets)}
}

// Gauge declares (or returns) a process-wide gauge. The product catalog
// declares no label dimensions for gauges: the queue depth and the session
// count are properties of the process, not of a game.
func (r *Registry) Gauge(name, help string) *Gauge {
	return &Gauge{m: r.declare(name, help, "gauge", nil, nil)}
}

// Counter is the handle for one counter metric.
type Counter struct{ m *metric }

// Add books n on the series for l. A nil handle (a metric a caller never
// declared) is a no-op: the generic accessors are for wiring code, and the
// hot path's nil-safe surface is the Product's hooks.
func (c *Counter) Add(l Labels, n uint64) {
	if c == nil || c.m == nil || n == 0 {
		return
	}
	m := c.m
	p := l.pair()
	m.mu.RLock()
	s, ok := m.counters[m.key(p)]
	if ok {
		m.mu.RUnlock()
		s.value.Add(n)
		return
	}
	m.mu.RUnlock()
	m.mu.Lock()
	if s, ok = m.counters[m.key(p)]; !ok {
		// The overflow series is the table's reserved last slot: once the
		// table holds maxSeries-1 sets, any new set folds into it rather
		// than growing the table past its bound (the count is folded in, not
		// dropped). The overflow pair itself always keeps its own series.
		if len(m.counters) >= maxSeries-1 && !p.isOverflow() {
			p = labelPair{overflowLabel, overflowLabel}
		}
		if s, ok = m.counters[m.key(p)]; !ok {
			s = &counterSeries{pair: p}
			m.counters[m.key(p)] = s
		}
	}
	m.mu.Unlock()
	s.value.Add(n)
}

// Histogram is the handle for one histogram metric.
type Histogram struct{ m *metric }

// Observe books one duration. A negative interval (a clock that stepped
// backwards) is booked at zero rather than dropped — the pipeline's own
// histogram makes the same choice, and a dropped sample makes a latency
// summary optimistic, which is the failure mode the summary exists to catch.
func (h *Histogram) Observe(l Labels, d time.Duration) {
	if h == nil || h.m == nil {
		return
	}
	if d < 0 {
		d = 0
	}
	m := h.m
	p := l.pair()
	m.mu.RLock()
	s, ok := m.hists[m.key(p)]
	if ok {
		m.mu.RUnlock()
		s.observe(d)
		return
	}
	m.mu.RUnlock()
	m.mu.Lock()
	if s, ok = m.hists[m.key(p)]; !ok {
		// The overflow series is the table's reserved last slot (see
		// Counter.Add): at maxSeries-1 sets a new set folds into it.
		if len(m.hists) >= maxSeries-1 && !p.isOverflow() {
			p = labelPair{overflowLabel, overflowLabel}
		}
		if s, ok = m.hists[m.key(p)]; !ok {
			s = &histSeries{pair: p, edges: m.buckets, buckets: make([]uint64, len(m.buckets)+1)}
			m.hists[m.key(p)] = s
		}
	}
	m.mu.Unlock()
	s.observe(d)
}

func (s *histSeries) observe(d time.Duration) {
	sec := d.Seconds()
	s.mu.Lock()
	s.count++
	s.sumSecs += sec
	// A sample lands in the first bucket whose edge is at or above it; past
	// every edge is the overflow slot. (sort.SearchFloat64s returns the first
	// index whose value is >= sec — the last edge at or above the sample.)
	i := sort.SearchFloat64s(s.edges, sec)
	if i >= len(s.buckets) {
		i = len(s.buckets) - 1
	}
	s.buckets[i]++
	s.mu.Unlock()
}

// Gauge is the handle for one process-wide gauge.
type Gauge struct{ m *metric }

// Set stores v as the gauge's current value.
func (g *Gauge) Set(v int64) {
	if g == nil || g.m == nil {
		return
	}
	g.m.gauge.Store(v)
}

// Add moves the gauge by v (positive or negative).
func (g *Gauge) Add(v int64) {
	if g == nil || g.m == nil {
		return
	}
	g.m.gauge.Add(v)
}
