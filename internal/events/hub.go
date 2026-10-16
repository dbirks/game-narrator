package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/dbirks/game-narrator/internal/games"
)

// Tunables. The defaults come from the architecture contract; every one is a
// HubOption so a test can force the pathological case instead of waiting for it.
const (
	// DefaultRingSize bounds the in-memory replay log: docs/spec/01 "SSE fanout
	// ring buffer is bounded". A ring, not a growing slice — the appliance does
	// OCR in real time on the same process.
	DefaultRingSize = 4096
	// DefaultSubscriberBuffer bounds one slow subscriber's undelivered backlog.
	// Beyond it the drop policy in queue.push decides what that subscriber
	// loses; the publisher never waits.
	DefaultSubscriberBuffer = 256
	// DefaultReplayBudget bounds how many events one Subscribe pulls from the
	// Store: enough to rebuild a browser's view of a session, small enough that a
	// client reconnecting in a loop cannot turn the durable log into an
	// unbounded read on an appliance that is also doing OCR.
	DefaultReplayBudget = 4096
	// DefaultHeartbeat is the SSE keepalive interval. 15 s is below the idle
	// timeout of anything in the deployment topology (docs/spec/10), and it is
	// a property of the stream rather than of one connection, so it lives here.
	DefaultHeartbeat = 15 * time.Second
)

// ErrClosed reports a hub that has been shut down: it no longer accepts
// publishes or subscriptions and existing subscribers are drained.
var ErrClosed = errors.New("events: hub is closed")

// ErrNoMoreEvents reports a subscription whose queue is closed and drained —
// normal end of stream after Hub.Close or a lag-induced resync, not a failure.
var ErrNoMoreEvents = errors.New("events: subscription is closed")

// Hub assigns global sequence numbers, keeps a bounded replay log and fans
// events out to live subscribers.
//
// One mutex covers numbering, the ring append and the fan-out, deliberately: if
// those three steps were not atomic, two publishers handed seq 5 and 6 by the
// store could reach the ring in the opposite order, and the "strictly
// increasing, no gaps" property that SSE resume, the durable log and the
// developer inspector all depend on would be gone. The cost is that publishing
// is serialized — including the Store.Append call, so a slow store serializes
// publishes too. Fan-out itself is non-blocking (queue.push never waits), so
// the critical section stays bounded by a ring write plus one append per
// matching subscriber. If publish volume ever demands more, a commit-ordering
// stage in front of the ring can be added without touching this contract.
type Hub struct {
	mu     sync.Mutex
	closed bool
	seq    int64 // last assigned seq == head of the log

	ring     []Envelope // ringSize slots; entries [0, ringLen) or the full circle
	ringIdx  int        // next write slot, which holds the oldest entry when full
	ringLen  int
	ringSize int

	subs    map[*Subscription]struct{}
	subSeq  int64
	store   Store
	bufSize int
	beat    time.Duration

	// replayBudget caps how many events one Subscribe may pull from the Store.
	replayBudget int
	log          *slog.Logger

	published int64
	dropped   int64
	lagged    int64
}

// HubOption customises a Hub. Production wiring uses WithStore and the default
// sizes; tests shrink the ring and the buffer to reach the resync paths.
type HubOption func(*Hub)

// WithRingSize sets the replay-log depth (default DefaultRingSize). 0 means no
// in-memory replay at all: every cursor becomes a resync, which is the honest
// setting for a memory-constrained replica rather than a silent truncation.
func WithRingSize(n int) HubOption {
	return func(h *Hub) {
		if n < 0 {
			n = 0
		}
		h.ringSize = n
	}
}

// WithSubscriberBuffer sets the per-subscriber backlog bound (default
// DefaultSubscriberBuffer).
func WithSubscriberBuffer(n int) HubOption {
	return func(h *Hub) {
		if n > 0 {
			h.bufSize = n
		}
	}
}

// WithStore attaches the durable log. While it is set, Publish takes its seq
// from Store.Append and Subscribe can replay past the ring instead of telling
// the client to resync (docs/spec/08 "Last-Event-ID replay from bounded event
// log, otherwise send events.resync_required").
func WithStore(s Store) HubOption {
	return func(h *Hub) { h.store = s }
}

// WithReplayBudget sets how many events one Subscribe may pull from the Store
// (default DefaultReplayBudget).
func WithReplayBudget(n int) HubOption {
	return func(h *Hub) {
		if n > 0 {
			h.replayBudget = n
		}
	}
}

// WithLogger receives the hub's degraded-mode warnings: a durable log that
// cannot answer a replay is survivable for a browser (it keeps streaming live
// events and is told about the hole), but it must not be silent.
func WithLogger(l *slog.Logger) HubOption {
	return func(h *Hub) {
		if l != nil {
			h.log = l
		}
	}
}

// WithHeartbeat sets the keepalive interval advertised to stream consumers.
func WithHeartbeat(d time.Duration) HubOption {
	return func(h *Hub) {
		if d > 0 {
			h.beat = d
		}
	}
}

// New builds a Hub.
func New(opts ...HubOption) *Hub {
	h := &Hub{
		subs:         map[*Subscription]struct{}{},
		ringSize:     DefaultRingSize,
		bufSize:      DefaultSubscriberBuffer,
		beat:         DefaultHeartbeat,
		replayBudget: DefaultReplayBudget,
		// Nothing is logged unless the owner wires a logger: a hub is used in
		// tests and in the runtime alike, and a fan-out path must not print on its
		// own account. Degraded-mode notices go to the hub's logger.
		log: slog.New(slog.DiscardHandler),
	}
	for _, opt := range opts {
		opt(h)
	}
	if h.ringSize > 0 {
		h.ring = make([]Envelope, h.ringSize)
	}
	return h
}

// Heartbeat returns the keepalive interval stream consumers should emit on.
func (h *Hub) Heartbeat() time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.beat
}

// Head returns the last assigned seq (0 before the first publish).
func (h *Hub) Head() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.seq
}

// Stats are hub-wide counters, exposed for /metrics and for tests that must
// prove the drop policy fired rather than silently buffered.
type Stats struct {
	// Published counts events accepted by Publish. Synthetic
	// events.resync_required notices are per-subscriber and are not included;
	// they are counted in SubscriptionStats.Delivered of that one stream.
	Published int64
	// Dropped counts events shed across every subscriber's lifetime.
	Dropped int64
	// Subscribers is the number of attached streams.
	Subscribers int
	// Head is the last assigned seq.
	Head int64
	// Lagged counts subscribers retired because their backlog filled with
	// critical events.
	Lagged int64
}

// Stats returns a snapshot of the hub counters.
func (h *Hub) Stats() Stats {
	h.mu.Lock()
	defer h.mu.Unlock()
	return Stats{
		Published:   h.published,
		Dropped:     h.dropped,
		Subscribers: len(h.subs),
		Head:        h.seq,
		Lagged:      h.lagged,
	}
}

// Publish validates e, fills in what the caller left blank, assigns the global
// seq, appends to the replay log and fans out to subscribers. It returns the
// envelope as published — including the assigned Seq, EventID and OccurredAt —
// so a caller can use that as the causation_id of the event it derives next.
//
// Publish never waits on a subscriber: fan-out appends into bounded queues with
// no blocking send, which is how docs/spec/01's "slow browsers cannot stall
// capture" holds. It can still fail — cancelled ctx, rejected envelope, store
// append error, closed hub — and every failure leaves the log and the subscribers
// untouched, because a half-published event is the one thing a browser
// replaying by seq cannot represent.
func (h *Hub) Publish(ctx context.Context, e Envelope) (Envelope, error) {
	if err := ctx.Err(); err != nil {
		return Envelope{}, err
	}
	e = e.withDefaults()
	// The content is checked before anything is allocated or appended: the seq
	// does not exist yet, and Validate requires one (an envelope with no seq is
	// not a complete record). The full Validate runs after numbering.
	if err := e.validateContent(); err != nil {
		return Envelope{}, err
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return Envelope{}, ErrClosed
	}

	if h.store != nil {
		// The durable log owns the sequence: it is what survives a restart, so
		// its seq must be the one browsers see as `id:`. Appended inside the
		// critical section so numbering, ring order and fan-out order stay
		// identical (see the type comment).
		seq, err := h.store.Append(ctx, e)
		if err != nil {
			return Envelope{}, fmt.Errorf("events: append: %w", err)
		}
		if seq < 1 {
			return Envelope{}, fmt.Errorf("events: store returned seq %d", seq)
		}
		e.Seq = seq
		if seq > h.seq {
			// Covers a hub restarted on top of an existing log.
			h.seq = seq
		}
		// e was validated before the append; the only field still unproven is the
		// one the store just chose, checked above. A full re-Validate here would
		// have to run *after* the row exists, so a late rejection would leave the
		// durable log holding an event nobody was fanned out — a permanent gap.
	} else {
		h.seq++
		e.Seq = h.seq
	}

	h.published++
	h.appendRingLocked(e)
	for sub := range h.subs {
		if sub.matches(e) {
			sub.deliverLocked(e)
		}
	}
	return e, nil
}

// withDefaults fills the fields the caller is allowed to omit and takes an
// ownership copy of Data.
func (e Envelope) withDefaults() Envelope {
	if e.SchemaVersion == 0 {
		e.SchemaVersion = SchemaVersion
	}
	if e.EventID == "" {
		e.EventID = games.NewID()
	}
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now().UTC()
	} else {
		e.OccurredAt = e.OccurredAt.UTC()
	}
	if len(e.Data) == 0 {
		e.Data = json.RawMessage("{}")
	} else {
		// Copy: the caller owns that buffer and may reuse it (a reused marshal
		// target, a bytes.Buffer). Every subscriber would otherwise read
		// whatever the publisher wrote next.
		data := make(json.RawMessage, len(e.Data))
		copy(data, e.Data)
		e.Data = data
	}
	return e
}

// appendRingLocked stores e, evicting the oldest entry when the ring is full.
func (h *Hub) appendRingLocked(e Envelope) {
	if h.ringSize == 0 {
		return
	}
	h.ring[h.ringIdx] = e
	if h.ringLen < h.ringSize {
		h.ringLen++
	}
	h.ringIdx = (h.ringIdx + 1) % h.ringSize
}

// oldestSeqLocked returns the smallest seq still buffered.
func (h *Hub) oldestSeqLocked() (int64, bool) {
	if h.ringLen == 0 {
		return 0, false
	}
	if h.ringLen == h.ringSize {
		// ringIdx is the next slot to overwrite, i.e. the oldest entry.
		return h.ring[h.ringIdx].Seq, true
	}
	return h.ring[0].Seq, true
}

// eachRingLocked walks every buffered event in seq order.
func (h *Hub) eachRingLocked(fn func(Envelope)) {
	if h.ringLen == 0 {
		return
	}
	start := 0
	if h.ringLen == h.ringSize {
		start = h.ringIdx
	}
	for i := 0; i < h.ringLen; i++ {
		fn(h.ring[(start+i)%h.ringSize])
	}
}

// Close drains and closes every subscription and refuses further publishes.
// The owner calls it during shutdown so streaming handlers can return instead
// of holding the process open: net/http's Shutdown waits for in-flight
// requests, and an SSE request is in flight until the client goes away.
//
// Close is idempotent, and events already queued stay readable — a client that
// reconnects against a restarted hub finds its cursor, or is told about the gap.
func (h *Hub) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	h.closed = true
	for sub := range h.subs {
		sub.q.close()
	}
	clear(h.subs)
	return nil
}

// Filter selects the events a subscription wants. A zero Filter means
// everything.
type Filter struct {
	// SessionID, when set, restricts the stream to one recognition session.
	// Global events (empty SessionID) deliberately do not match a session
	// filter: a page watching one session should not repaint on unrelated job
	// noise, and the reverse would put another session's gameplay text on this
	// one's stream (docs/spec/14 privacy of partial data).
	SessionID string
	// Names, when non-empty, restricts the stream to those event names.
	Names []string
}

// maxFilterNames bounds a filter's name list: a query string is client-supplied
// and an unbounded set would turn every publish into a long linear scan.
const maxFilterNames = 64

func (f Filter) validate() error {
	if err := ValidateSessionID(f.SessionID); err != nil {
		return err
	}
	if len(f.Names) > maxFilterNames {
		return validationf("a filter may name at most %d events, got %d", maxFilterNames, len(f.Names))
	}
	for _, n := range f.Names {
		if err := ValidateName(n); err != nil {
			return err
		}
	}
	return nil
}

func (f Filter) matches(e Envelope) bool {
	if f.SessionID != "" && e.SessionID != f.SessionID {
		return false
	}
	if len(f.Names) == 0 {
		return true
	}
	return slices.Contains(f.Names, e.Name)
}

// SubscribeOption adjusts one subscription.
type SubscribeOption func(*Subscription)

// WithBuffer overrides the backlog bound for this subscription only. The
// developer inspector legitimately asks for a deeper backlog than a browser
// page does.
func WithBuffer(n int) SubscribeOption {
	return func(s *Subscription) {
		if n > 0 {
			s.q.resize(n)
		}
	}
}

// Subscribe returns a Subscription positioned after afterSeq.
//
// afterSeq is a global cursor (an SSE Last-Event-ID); 0 means "everything still
// retained". Events with Seq > afterSeq matching f are replayed in seq order
// and then live events follow, with no duplicates and no gaps: the replay and
// the registration happen inside one critical section, so an event published
// during the handoff is either in the replay or in the live stream, never both
// and never neither.
//
// When the cursor is older than anything the hub can show — the ring rolled
// over and no Store is attached, or the store has pruned that range — the first
// event on the stream is a synthetic events.resync_required naming the lost
// range, and the client rebuilds from a REST snapshot (docs/spec/08 "Event
// stream reliability").
//
// Subscribe attaches no goroutine; the consumer pulls with Next, so a client
// disconnect costs one Close.
func (h *Hub) Subscribe(ctx context.Context, f Filter, afterSeq int64, opts ...SubscribeOption) (*Subscription, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := f.validate(); err != nil {
		return nil, err
	}
	if afterSeq < 0 {
		return nil, validationf("after_seq %d must be >= 0", afterSeq)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, ErrClosed
	}

	h.subSeq++
	sub := &Subscription{hub: h, id: h.subSeq, filter: f, q: newQueue(h.bufSize)}
	for _, opt := range opts {
		opt(sub)
	}

	if err := sub.replayLocked(ctx, afterSeq); err != nil {
		delete(h.subs, sub)
		sub.q.close()
		return nil, err
	}
	return sub, nil
}

// Subscription is one consumer's view of the stream. It is pull-based — no
// goroutine belongs to it — so a client that stops reading costs one bounded
// queue until the hub sheds it, and a client that goes away costs one Close.
type Subscription struct {
	hub    *Hub
	id     int64
	filter Filter
	q      *queue

	closeOnce sync.Once
}

// ID identifies this subscription in logs and metrics.
func (s *Subscription) ID() int64 { return s.id }

// Filter returns the subscription's filter.
func (s *Subscription) Filter() Filter { return s.filter }

// Stats returns this subscriber's delivery counters.
func (s *Subscription) Stats() SubscriptionStats { return s.q.stats() }

// replayLocked queues history for a new subscription. It is called with h.mu
// held and with the subscription already registered, so live events that arrive
// once it returns land behind the replayed ones.
func (s *Subscription) replayLocked(ctx context.Context, afterSeq int64) error {
	h := s.hub
	h.subs[s] = struct{}{}
	cursor := afterSeq

	// Where does honest replay begin? The ring covers [oldest buffered, head];
	// the store covers its own retained range. The store's low-water mark needs
	// a probe of its own: Store.After applies the filter, and a sparse filtered
	// result cannot prove where coverage begins.
	coverageLow, haveCoverage := h.oldestSeqLocked()
	var storeLow int64
	haveStore := false
	degraded := false
	if h.store != nil {
		rows, err := h.store.After(ctx, 0, Filter{}, 1)
		if err != nil {
			// A durable log that will not answer is degraded, not fatal: the ring
			// still covers recent history and live delivery still works, so the
			// browser keeps watching (AGENTS.md rule 7, graceful degraded modes).
			// What it must not get is a silent claim of continuity.
			degraded = true
			h.log.Warn("event store unavailable for replay, falling back to the in-memory log",
				"subscriber_id", s.id, "error", err)
		} else if len(rows) == 1 {
			storeLow, haveStore = rows[0].Seq, true
			if !haveCoverage || storeLow < coverageLow {
				coverageLow = storeLow
			}
			haveCoverage = true
		}
	}

	if !haveCoverage {
		// Nothing is retained anywhere. A cursor behind the head is a real
		// hole (a restart with no durable log, or ring size 0); a cursor at or
		// ahead of it is a client that is not missing anything showable.
		if cursor < h.seq {
			s.q.push(GapNotice(s.filter.SessionID, "log_reset", cursor+1, h.seq), true)
		}
		return nil
	}
	if cursor+1 < coverageLow {
		// The events between the cursor and the first retained one are gone for
		// good. Say so, then continue with what survives. The notice carries
		// seq == coverageLow-1 so the client's cursor lands exactly where replay
		// resumes: reconnecting with it must not produce a second resync.
		s.q.push(GapNotice(s.filter.SessionID, "gap", cursor+1, coverageLow-1), true)
		cursor = coverageLow - 1
	}

	oldestBuffered, haveBuffered := h.oldestSeqLocked()

	// History the ring has already lost comes from the store, paged under a
	// budget so one reconnect after an outage cannot pull the whole event table
	// into memory.
	if haveStore && (!haveBuffered || cursor+1 < oldestBuffered) {
		remaining := h.replayBudget
		hole := false
		for remaining > 0 {
			limit := min(replayPageSize, remaining)
			rows, err := h.store.After(ctx, cursor, s.filter, limit)
			if err != nil {
				// Same rule as the probe: deliver what survived, then say exactly
				// what could not be proven.
				degraded = true
				h.log.Warn("event store replay failed",
					"subscriber_id", s.id, "after_seq", cursor, "error", err)
				hole = true
				break
			}
			if len(rows) == 0 {
				break
			}
			remaining -= len(rows)
			handedOff := false
			for _, row := range rows {
				if haveBuffered && row.Seq >= oldestBuffered {
					handedOff = true
					break // the ring continues from here, without a duplicate
				}
				if row.Seq <= cursor {
					continue // defensive: a store must return ascending seq
				}
				s.q.push(row, true)
				cursor = row.Seq
			}
			if handedOff {
				break
			}
			if len(rows) < limit {
				break // the store has nothing more in range: no hole
			}
			if remaining <= 0 {
				hole = s.storeHasMore(ctx, cursor, oldestBuffered, haveBuffered)
			}
		}
		if hole {
			// The budget (or the store itself) stopped inside the range that
			// should have been replayable. Announce the hole before the surviving
			// tail, so the client's reducer gets the gap signal before it starts
			// applying state.
			holeEnd := h.seq
			if haveBuffered {
				holeEnd = oldestBuffered - 1
			}
			reason := "replay_truncated"
			if degraded {
				reason = "store_unavailable"
			}
			if cursor < holeEnd {
				s.q.push(GapNotice(s.filter.SessionID, reason, cursor+1, holeEnd), true)
			}
		}
	} else if degraded && haveCoverage && cursor < h.seq && (!haveBuffered || cursor+1 < oldestBuffered) {
		// The store never answered at all, so nothing between the cursor and the
		// ring can be shown or proven. Say so rather than replaying from the ring
		// and letting the client infer the hole from a seq jump.
		holeEnd := h.seq
		if haveBuffered {
			holeEnd = oldestBuffered - 1
		}
		if cursor < holeEnd {
			s.q.push(GapNotice(s.filter.SessionID, "store_unavailable", cursor+1, holeEnd), true)
			cursor = holeEnd
		}
	}

	h.eachRingLocked(func(e Envelope) {
		if e.Seq > cursor && s.filter.matches(e) {
			s.q.push(e, true)
		}
	})
	return nil
}

// replayPageSize bounds one Store.After call in a replay: a browser reconnect
// after an outage must not pull the whole event table in one query.
const replayPageSize = 256

// storeHasMore reports whether the store still holds events the replay was cut
// short of — the difference between "the client is fully caught up to the
// budget" and a silent hole it would have to guess at.
func (s *Subscription) storeHasMore(ctx context.Context, cursor, oldestBuffered int64, haveBuffered bool) bool {
	rows, err := s.hub.store.After(ctx, cursor, Filter{}, 1)
	if err != nil || len(rows) == 0 {
		// An unreadable store cannot be asserted about; reporting no hole lets the
		// replay continue on the data that did arrive, and the client's own
		// snapshot poll is what catches a real inconsistency.
		return false
	}
	return !haveBuffered || rows[0].Seq < oldestBuffered
}

// GapNotice builds the synthetic events.resync_required a client receives when
// its cursor cannot be honoured. It is exported because the SSE writer needs it
// too (a frame it cannot encode is a gap for that one client), and it is not
// published through Publish and never written to the durable log: it describes
// one subscriber's view, not a state change, and persisting it would make a
// notice about missing events look like one of the missing events.
//
// The envelope's Seq is `through`, the last event the client is missing, so the
// cursor the browser stores lands exactly where delivery resumes — a reconnect
// after a resync must not produce a second resync.
func GapNotice(sessionID, reason string, from, through int64) Envelope {
	data, err := json.Marshal(map[string]any{
		"reason":      reason,
		"from_seq":    from,
		"through_seq": through,
		"cursor":      through,
		"snapshot":    "/api/v1/sessions",
	})
	if err != nil {
		// Unreachable: the map holds an int, a validated reason and a validated
		// session id. Keep the notice rather than dropping the signal — a client
		// that is not told about a gap will silently show stale state.
		data = json.RawMessage(`{"reason":"gap"}`)
	}
	return Envelope{
		SchemaVersion: SchemaVersion,
		EventID:       games.NewID(),
		SessionID:     sessionID,
		// Seq names the last event the client is missing, so its cursor lands
		// exactly where live delivery resumes and a reconnect cannot loop.
		Seq:        through,
		Name:       EventsResyncRequired,
		OccurredAt: time.Now().UTC(),
		Data:       data,
	}
}

// deliverLocked fans one event into this subscriber's queue. Called with the
// hub lock held; it never blocks.
func (s *Subscription) deliverLocked(e Envelope) {
	switch s.q.push(e, false) {
	case pushOK:
	case pushDropped, pushEvicted:
		s.hub.dropped++
	case pushLagged:
		// The backlog is entirely critical events and this one does not fit.
		// Nothing may be evicted (docs/spec/08: not state transitions or
		// critical speech events), so the subscriber is retired instead: the
		// queued critical events are still delivered, a final
		// events.resync_required says "you missed state, use REST", no further
		// live events are pushed — and the publisher never waits.
		s.hub.dropped++
		s.hub.lagged++
		s.q.markLagged()
		// Behind the queued critical events, which is why it is pushLast rather
		// than an exempt replay push (those land at the front of the stream).
		s.q.pushLast(GapNotice(s.filter.SessionID, "lagged", e.Seq, s.hub.seq))
		s.q.close()
	case pushClosed:
		s.hub.dropped++
	}
	if s.q.isClosed() {
		delete(s.hub.subs, s)
	}
}

// matches reports whether an event belongs on this stream.
func (s *Subscription) matches(e Envelope) bool { return s.filter.matches(e) }

// Next returns the next event: replayed history first, then live events.
//
// It blocks until an event is available, timeout elapses, ctx is cancelled, or
// the subscription is finished. timeout <= 0 waits indefinitely. A false second
// result with a nil error means "nothing yet, the timeout expired" — what an
// SSE writer turns into a heartbeat comment.
//
// It returns ErrNoMoreEvents once the queue is closed and drained, and
// ctx.Err() on cancellation, so a client's disconnect stays a client-side fact
// rather than an event on the log.
func (s *Subscription) Next(ctx context.Context, timeout time.Duration) (Envelope, bool, error) {
	var deadline <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		deadline = t.C
	}
	for {
		e, ok, closed := s.q.pop()
		if ok {
			return e, true, nil
		}
		if closed {
			return Envelope{}, false, ErrNoMoreEvents
		}
		select {
		case <-ctx.Done():
			return Envelope{}, false, ctx.Err()
		case <-deadline:
			return Envelope{}, false, nil
		case <-s.q.wake:
			// Loop and re-pop: the wake token and the pop above may have raced.
		}
	}
}

// Close detaches the subscription. Safe to call more than once, and safe to
// call while another goroutine is inside Next. It never affects publishers or
// other subscribers.
func (s *Subscription) Close() error {
	s.closeOnce.Do(func() {
		h := s.hub
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(h.subs, s)
		s.q.close()
	})
	return nil
}
