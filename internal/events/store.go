package events

import (
	"context"
	"fmt"
	"sync"
)

// Store is the durable side of the replay log: the piece the in-memory ring
// cannot be, because it survives a restart and answers a browser whose
// Last-Event-ID predates everything in memory (docs/spec/08 "Event stream
// reliability", migration 0003's `events` table).
//
// The interface is deliberately two methods. Anything richer — pruning,
// per-session cursors, trace lookups for /api/v1/dev/traces/:id — belongs to the
// repository that owns the table, not to the fan-out path, and keeping it small
// is what lets the hub be tested without a database.
//
// Contract:
//   - Append assigns and returns the global seq. The caller's Envelope.Seq is
//     ignored on the way in (a row in the events table takes its seq from
//     SQLite's AUTOINCREMENT, so asking the caller for one would put two writers
//     in charge of the same counter) and the returned value must be strictly
//     greater than every value returned before, across restarts.
//   - Append must validate the envelope, since a hub without a Store is the only
//     other gate.
//   - After returns matching events with Seq > afterSeq in ascending seq order,
//     at most limit of them (limit <= 0 means the implementation's default).
//     Rows must survive in the store only as long as the hub may be asked about
//     them; when a store prunes, the hub's low-water probe notices and emits
//     events.resync_required instead of pretending continuity.
type Store interface {
	Append(ctx context.Context, e Envelope) (int64, error)
	After(ctx context.Context, afterSeq int64, f Filter, limit int) ([]Envelope, error)
}

// DefaultStoreLimit bounds a Store.After call made by a caller that passed
// limit <= 0.
const DefaultStoreLimit = 1000

// MemStore is the in-memory Store: the same contract as the SQLite repository
// that replaces it, so the hub's replay paths are exercised without a database
// (docs/spec/12 "Unit" mode). It is safe for concurrent use and never prunes.
//
// It is a test double and a development convenience, not a production log: it
// keeps every event until the process exits, which on an appliance with 4 GB of
// storage is exactly the failure docs/spec/02 "bounded operational store" rules
// out.
type MemStore struct {
	mu      sync.Mutex
	events  []Envelope
	lastSeq int64
}

// NewMemStore returns an empty MemStore.
func NewMemStore() *MemStore {
	return &MemStore{}
}

// Append assigns the next seq and stores a copy of e.
func (m *MemStore) Append(ctx context.Context, e Envelope) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	// Envelopes arrive with Seq 0 from the hub, because the store owns
	// numbering; a caller that appends directly gets the same defaults the hub
	// would apply, and an envelope that already carries a seq is renumbered
	// rather than reused, so a store reused across a restart cannot hand out an
	// aliased cursor.
	m.mu.Lock()
	defer m.mu.Unlock()

	e = e.withDefaults()
	e.Seq = m.lastSeq + 1
	// Validated here, with the seq in place: it is the only gate for a caller
	// that appends directly instead of going through Hub.Publish, and rejecting
	// before lastSeq moves means a rejected event leaves no hole in the log.
	if err := e.Validate(); err != nil {
		return 0, err
	}
	m.lastSeq = e.Seq
	m.events = append(m.events, e)
	return e.Seq, nil
}

// After returns matching events with Seq > afterSeq, oldest first.
func (m *MemStore) After(ctx context.Context, afterSeq int64, f Filter, limit int) ([]Envelope, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > DefaultStoreLimit {
		limit = DefaultStoreLimit
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	// m.events is in append order, which is seq order because Append hands out
	// seq under the same lock it appends under — asserted by
	// TestMemStoreAppendIsSeqOrdered rather than re-sorted on every read.
	out := make([]Envelope, 0, min(limit, len(m.events)))
	for _, e := range m.events {
		if e.Seq <= afterSeq {
			continue
		}
		if !f.matches(e) {
			continue
		}
		out = append(out, e)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// Len reports how many events the store holds. Test helper.
func (m *MemStore) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.events)
}

// PruneBelow drops every event with seq < throughSeq, simulating the retention
// policy the durable log will have (docs/spec/02 "bounded operational store").
// It exists so the resync-on-pruned-history path is testable rather than
// theoretical.
func (m *MemStore) PruneBelow(throughSeq int64) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := m.events[:0]
	dropped := 0
	for _, e := range m.events {
		if e.Seq < throughSeq {
			dropped++
			continue
		}
		kept = append(kept, e)
	}
	n := len(m.events) - len(kept)
	m.events = kept
	return n
}

// compile-time proof that the test double satisfies the contract the SQLite
// repository will implement.
var _ Store = (*MemStore)(nil)

// errStoreFailure is returned by failingStore; declared as a function so the
// error text names the component that failed.
func errStoreFailure(op string) error { return fmt.Errorf("memstore: %s failed", op) }

// FailingStore is a Store whose Append always fails, for the hub test that a
// store failure must leave the log and the subscribers untouched.
type FailingStore struct{ Err error }

// Append always fails.
func (f FailingStore) Append(context.Context, Envelope) (int64, error) {
	if f.Err != nil {
		return 0, f.Err
	}
	return 0, errStoreFailure("append")
}

// After always fails.
func (f FailingStore) After(context.Context, int64, Filter, int) ([]Envelope, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return nil, errStoreFailure("after")
}

var _ Store = FailingStore{}
