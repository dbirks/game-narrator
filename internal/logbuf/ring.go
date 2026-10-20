// Package logbuf keeps a bounded in-process tail of the process's structured
// log records (ADR-0087, docs/spec/09 /diagnostics "system logs (redacted)").
//
// The process's real log goes to stderr (the journald), and it always will —
// this ring is a fanout copy, so a deployment without a journal (a dev
// checkout, a container) still answers the diagnostics page's logs section
// from the one spelling that exists everywhere. The ring is bounded
// (AGENTS.md rule 7): a fixed capacity, written under a mutex, read as a tail.
// It is a tail, not an archive — the support bundle remains the durable,
// redacted log path (ADR-0042).
package logbuf

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// DefaultCapacity is the ring size a production process wires (ADR-0087):
// enough for a tail the operator can read, small enough that a click-happy
// diagnostics page cannot grow the process's memory. Named here so the
// process and the route's "how far back" note share one number.
const DefaultCapacity = 500

// Record is one captured log record. Attrs are the record's attributes with
// their values already rendered to strings; the diagnostics endpoint passes
// them through the redactor before they leave the process.
type Record struct {
	Time  time.Time
	Level slog.Level
	Msg   string
	Attrs map[string]string
}

// Ring is the bounded store. Zero-value Ring is unusable; construct with New.
type Ring struct {
	mu   sync.Mutex
	recs []Record
	cap  int
}

// New returns a ring that keeps at most capacity records (the newest win).
// capacity must be at least 1.
func New(capacity int) *Ring {
	if capacity < 1 {
		capacity = 1
	}
	return &Ring{cap: capacity}
}

// Handler returns the slog.Handler that appends records to the ring. It is
// safe for concurrent use (slog hands the same handler to every goroutine).
func (r *Ring) Handler() slog.Handler {
	return &recHandler{ring: r}
}

func (r *Ring) append(rec Record) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.recs) >= r.cap {
		r.recs = append(r.recs[1:], rec)
	} else {
		r.recs = append(r.recs, rec)
	}
}

// Tail returns the newest up to limit records, oldest first. limit <= 0 asks
// for the whole ring.
func (r *Ring) Tail(limit int) []Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	if limit <= 0 || limit > len(r.recs) {
		limit = len(r.recs)
	}
	out := make([]Record, limit)
	copy(out, r.recs[len(r.recs)-limit:])
	return out
}

// recHandler is the ring's slog.Handler. It captures each record's message and
// attributes and appends the rendered record to the ring; handler options and
// group structure are flattened (the diagnostics view is a flat attrs map).
type recHandler struct {
	ring *Ring
}

func (h *recHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *recHandler) Handle(_ context.Context, rec slog.Record) error {
	attrs := map[string]string{}
	rec.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	h.ring.append(Record{
		Time:  rec.Time,
		Level: rec.Level,
		Msg:   rec.Message,
		Attrs: attrs,
	})
	return nil
}

func (h *recHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recHandler) WithGroup(string) slog.Handler      { return h }

// Fanout composes several handlers into one that hands every record to all of
// them. The process's real destination (stderr) stays first: a ring that is
// down must never shadow the log the operator can always read.
func Fanout(handlers ...slog.Handler) slog.Handler {
	return &fanoutHandler{handlers: handlers}
}

type fanoutHandler struct{ handlers []slog.Handler }

func (f *fanoutHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range f.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (f *fanoutHandler) Handle(ctx context.Context, rec slog.Record) error {
	for _, h := range f.handlers {
		if h.Enabled(ctx, rec.Level) {
			_ = h.Handle(ctx, rec)
		}
	}
	return nil
}

func (f *fanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := make([]slog.Handler, len(f.handlers))
	for i, h := range f.handlers {
		next[i] = h.WithAttrs(attrs)
	}
	return &fanoutHandler{handlers: next}
}

func (f *fanoutHandler) WithGroup(name string) slog.Handler {
	next := make([]slog.Handler, len(f.handlers))
	for i, h := range f.handlers {
		next[i] = h.WithGroup(name)
	}
	return &fanoutHandler{handlers: next}
}
