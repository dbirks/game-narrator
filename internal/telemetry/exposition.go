package telemetry

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// WritePrometheus renders the registry in the Prometheus text exposition
// format (https://prometheus.io/docs/instrumenting/exposition_formats/), one
// hand-rolled writer rather than a client dependency (ADR-0106): the format
// this product needs is HELP/TYPE lines plus labelled samples, and the writer
// is the same data the snapshot uses, so the two surfaces cannot drift.
//
// Determinism is part of the contract — a scraper or a test that compares two
// builds' output needs bytes that do not depend on map order: metrics in
// declaration order, and within a metric the series sorted by their label
// values. Histogram buckets are cumulative in the exposition, as the format
// requires (le is the bucket edge, le="+Inf" the total).
func (r *Registry) WritePrometheus(w io.Writer) error {
	var b strings.Builder
	b.Grow(8 * len(r.metricList()))
	for _, m := range r.metricList() {
		fmt.Fprintf(&b, "# HELP %s %s\n", m.name, m.help)
		fmt.Fprintf(&b, "# TYPE %s %s\n", m.name, m.kind)
		if err := m.writeSeries(&b); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// metricList snapshots the declaration-order metric set.
func (r *Registry) metricList() []*metric {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*metric, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.metrics[name])
	}
	return out
}

// labelValues is a label set's values in the metric's declared dimension
// order (the catalog's only two: game, then profile_revision).
func labelValues(dims []string, p labelPair) [2]string {
	var out [2]string
	for i, d := range dims {
		if d == LabelProfileRevision {
			out[i] = p.revision
		} else {
			out[i] = p.game
		}
	}
	return out
}

// labelString renders one series' label set: {game="g",profile_revision="r"}
// in declared order. A series with no declared dimensions (a gauge) renders
// none.
func labelString(dims []string, p labelPair) string {
	if len(dims) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, d := range dims {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s=%q", d, labelValues(dims, p)[i])
	}
	b.WriteByte('}')
	return b.String()
}

// bucketLabelString is a histogram row's label set: the series' labels with
// the bucket edge appended last, the format's convention.
func bucketLabelString(dims []string, p labelPair, le string) string {
	var b strings.Builder
	b.WriteByte('{')
	for i, d := range dims {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s=%q", d, labelValues(dims, p)[i])
	}
	fmt.Fprintf(&b, ",le=%q", le)
	b.WriteByte('}')
	return b.String()
}

type pairLess func(a, b labelPair) bool

func sortByPair[T any](rows []T, pair func(T) labelPair) {
	sort.Slice(rows, func(i, j int) bool {
		a, b := pair(rows[i]), pair(rows[j])
		if a.game != b.game {
			return a.game < b.game
		}
		return a.revision < b.revision
	})
}

// writeSeries appends one metric's samples.
func (m *metric) writeSeries(b *strings.Builder) error {
	switch m.kind {
	case "counter":
		m.mu.RLock()
		type row struct {
			p labelPair
			v uint64
		}
		rows := make([]row, 0, len(m.counters))
		for _, s := range m.counters {
			rows = append(rows, row{s.pair, s.value.Load()})
		}
		m.mu.RUnlock()
		sortByPair(rows, func(r row) labelPair { return r.p })
		for _, r := range rows {
			fmt.Fprintf(b, "%s%s %s\n", m.name, labelString(m.dimensions, r.p), strconv.FormatUint(r.v, 10))
		}
	case "gauge":
		fmt.Fprintf(b, "%s %s\n", m.name, strconv.FormatInt(m.gauge.Load(), 10))
	case "histogram":
		m.mu.RLock()
		snap := make([]histSnapshot, 0, len(m.hists))
		for _, s := range m.hists {
			snap = append(snap, s.snapshot())
		}
		m.mu.RUnlock()
		sortByPair(snap, func(s histSnapshot) labelPair { return s.pair })
		for _, s := range snap {
			// Cumulative over the non-cumulative slots: the count at or
			// below edge i is the sum of slots 0..i, and le="+Inf" is the
			// total — the format's contract, not a choice. The last slot is
			// the overflow (samples past every edge): it joins only the +Inf
			// row, where it belongs.
			var cum uint64
			for i, edge := range s.edges {
				cum += s.buckets[i]
				le := strconv.FormatFloat(edge, 'g', -1, 64)
				fmt.Fprintf(b, "%s_bucket%s %s\n", m.name, bucketLabelString(m.dimensions, s.pair, le), strconv.FormatUint(cum, 10))
			}
			if len(s.buckets) > len(s.edges) {
				cum += s.buckets[len(s.buckets)-1]
			}
			fmt.Fprintf(b, "%s_bucket%s %s\n", m.name, bucketLabelString(m.dimensions, s.pair, "+Inf"), strconv.FormatUint(cum, 10))
			fmt.Fprintf(b, "%s_sum%s %s\n", m.name, labelString(m.dimensions, s.pair), strconv.FormatFloat(s.sumSecs, 'g', -1, 64))
			fmt.Fprintf(b, "%s_count%s %s\n", m.name, labelString(m.dimensions, s.pair), strconv.FormatUint(s.count, 10))
		}
	default:
		return fmt.Errorf("telemetry: metric %s has unknown kind %q", m.name, m.kind)
	}
	return nil
}

// histSnapshot is one histogram series' state under its own lock.
type histSnapshot struct {
	pair    labelPair
	count   uint64
	sumSecs float64
	edges   []float64
	buckets []uint64 // non-cumulative, one slot per edge plus the overflow
}

func (s *histSeries) snapshot() histSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return histSnapshot{
		pair:    s.pair,
		count:   s.count,
		sumSecs: s.sumSecs,
		edges:   s.edges,
		buckets: append([]uint64(nil), s.buckets...),
	}
}

// ── the JSON snapshot (the internal API) ──────────────────────────────────

// Snapshot is the registry's structured view: the same data as the
// Prometheus exposition, shaped for the consumers the appliance has inside
// its own walls — the diagnostics and health surfaces read it through
// GET /api/v1/dev/metrics instead of parsing a text stream.
type Snapshot struct {
	GeneratedAt time.Time        `json:"generated_at"`
	Metrics     []MetricSnapshot `json:"metrics"`
}

// MetricSnapshot is one metric and every series it currently holds.
type MetricSnapshot struct {
	Name   string           `json:"name"`
	Help   string           `json:"help"`
	Type   string           `json:"type"` // counter | gauge | histogram
	Series []SeriesSnapshot `json:"series"`
}

// SeriesSnapshot is one labelled series (or, for a gauge, the gauge's single
// value). Value and the histogram fields are pointers so "absent" encodes as
// absent: a counter row carries no buckets, a histogram row no Value.
type SeriesSnapshot struct {
	Labels map[string]string `json:"labels,omitempty"`
	// Value is the counter total or the gauge reading.
	Value *float64 `json:"value,omitempty"`
	// The histogram half.
	Count *uint64  `json:"count,omitempty"`
	Sum   *float64 `json:"sum_seconds,omitempty"`
	// Buckets is non-cumulative (the exposition's cumulative form is the
	// scraper's contract; a structured consumer wants the slots): one entry
	// per edge plus the overflow slot, whose Le is nil — JSON cannot carry
	// +Inf, and a null edge is the honest spelling of le="+Inf".
	Buckets []Bucket `json:"buckets,omitempty"`
}

// Bucket is one histogram slot.
type Bucket struct {
	Le    *float64 `json:"le"` // nil = +Inf (the overflow slot)
	Count uint64   `json:"count"`
}

// Snapshot returns the registry's current structured view. GeneratedAt is the
// process wall clock — the freshness a page renders, not a metric.
func (r *Registry) Snapshot() Snapshot {
	out := Snapshot{GeneratedAt: time.Now().UTC(), Metrics: make([]MetricSnapshot, 0, len(r.metricList()))}
	for _, m := range r.metricList() {
		ms := MetricSnapshot{Name: m.name, Help: m.help, Type: m.kind, Series: []SeriesSnapshot{}}
		switch m.kind {
		case "counter":
			m.mu.RLock()
			type row struct {
				p labelPair
				v uint64
			}
			rows := make([]row, 0, len(m.counters))
			for _, s := range m.counters {
				rows = append(rows, row{s.pair, s.value.Load()})
			}
			m.mu.RUnlock()
			sortByPair(rows, func(r row) labelPair { return r.p })
			for _, r := range rows {
				f := float64(r.v)
				ms.Series = append(ms.Series, SeriesSnapshot{Labels: labelMap(m.dimensions, r.p), Value: &f})
			}
		case "gauge":
			f := float64(m.gauge.Load())
			ms.Series = append(ms.Series, SeriesSnapshot{Value: &f})
		case "histogram":
			m.mu.RLock()
			snap := make([]histSnapshot, 0, len(m.hists))
			for _, s := range m.hists {
				snap = append(snap, s.snapshot())
			}
			m.mu.RUnlock()
			sortByPair(snap, func(s histSnapshot) labelPair { return s.pair })
			for _, s := range snap {
				buckets := make([]Bucket, 0, len(s.buckets))
				for i, c := range s.buckets {
					var le *float64
					if i < len(s.edges) {
						v := s.edges[i]
						le = &v
					}
					buckets = append(buckets, Bucket{Le: le, Count: c})
				}
				c, sum := s.count, s.sumSecs
				if math.IsInf(sum, 0) || math.IsNaN(sum) {
					// A pathological sum must not poison the JSON (the
					// encoder refuses Inf/NaN): report the count and drop
					// the sum, which is the honest "not measurable".
					ms.Series = append(ms.Series, SeriesSnapshot{
						Labels:  labelMap(m.dimensions, s.pair),
						Count:   &c,
						Buckets: buckets,
					})
					continue
				}
				ms.Series = append(ms.Series, SeriesSnapshot{
					Labels:  labelMap(m.dimensions, s.pair),
					Count:   &c,
					Sum:     &sum,
					Buckets: buckets,
				})
			}
		}
		out.Metrics = append(out.Metrics, ms)
	}
	return out
}

// labelMap renders a label set as the snapshot's labels object.
func labelMap(dims []string, p labelPair) map[string]string {
	if len(dims) == 0 {
		return nil
	}
	m := make(map[string]string, len(dims))
	v := labelValues(dims, p)
	for i, d := range dims {
		m[d] = v[i]
	}
	return m
}
