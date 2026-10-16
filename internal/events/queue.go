package events

import (
	"sync"
)

// pushResult reports what queue.push did, so the hub can account for drops and
// decide whether to retire a subscriber.
type pushResult int

const (
	// pushOK queued the event.
	pushOK pushResult = iota
	// pushEvicted queued the event after dropping an older droppable one.
	pushEvicted
	// pushDropped refused a droppable event because the backlog is all
	// critical — the newcomer loses, never a queued state transition.
	pushDropped
	// pushLagged means a critical event arrived while the backlog was full of
	// critical events: the only honest answer is to shed the subscriber with a
	// resync notice (docs/spec/08 backpressure policy).
	pushLagged
	// pushClosed means the subscription is finished and the event is discarded.
	pushClosed
)

// queue is the bounded per-subscriber buffer behind a Subscription.
//
// It is a mutex plus a deque rather than a channel because the drop policy has
// to look inside the backlog: "drop the oldest droppable event in its queue" is
// not expressible over a channel without draining it, which would make the
// publisher pay for a slow consumer's backlog.
//
// Two regions with different rules, kept separate so neither can silently
// truncate the other:
//
//   - prefix holds the history queued during Subscribe. It is exempt from max —
//     a client that asked for history must either get it or be told it is gone,
//     and refusing half a replay is the silent gap this package exists to
//     prevent. It is drained first, so ordering is prefix-then-live.
//   - live is the backlog of events fanned out while the subscription is
//     attached, bounded by max.
//
// Both stay in strict seq order: removal happens at the read end, or by taking
// one element out of the middle and shifting the tail down. The shift is why a
// dropped item never leaves a hole: holes would let the write end overtake an
// unread element and deliver it out of order, and "no gaps, strictly increasing
// seq" is the property every consumer's cursor logic is built on. The cost is
// bounded by max, which is exactly what a bound is for.
type queue struct {
	mu     sync.Mutex
	prefix []Envelope
	// phead is how many prefix entries have been handed out.
	phead int

	live []Envelope
	// lhead is how many live entries have been handed out. The deque is
	// live[lhead:]; it is folded back to the front when the read offset gets
	// large enough to matter, which keeps the slice a small multiple of max
	// rather than growing with the lifetime of the connection.
	lhead int

	max int

	closed bool
	lagged bool

	delivered int64
	dropped   int64

	// wake has capacity 1 and is never closed: a non-blocking send signals that
	// there is something to read, and a stale token only costs one extra loop.
	wake chan struct{}
}

func newQueue(max int) *queue {
	if max <= 0 {
		max = DefaultSubscriberBuffer
	}
	return &queue{max: max, wake: make(chan struct{}, 1)}
}

// resize grows the backlog bound. It only takes effect before anything is
// queued: growing an in-flight queue would either lose the backlog or move the
// bound under a subscriber that was already being shed against it.
func (q *queue) resize(n int) {
	if n <= q.max || len(q.prefix)-q.phead+q.pending() > 0 {
		return
	}
	q.max = n
}

// pending counts unread live entries.
func (q *queue) pending() int { return len(q.live) - q.lhead }

func (q *queue) push(e Envelope, exempt bool) pushResult {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return pushClosed
	}
	if exempt {
		q.prefix = append(q.prefix, e)
		q.signal()
		return pushOK
	}
	if q.pending() >= q.max {
		if i := q.oldestDroppable(); i >= 0 {
			if i == q.lhead {
				// The victim is the front: retiring it is a read-offset move
				// rather than a shift of everything behind it. This is the
				// redundant-frame case, and it is why one stalled browser cannot
				// make every publish cost O(buffer).
				q.live[q.lhead] = Envelope{}
				q.lhead++
			} else {
				q.live = append(q.live[:i], q.live[i+1:]...)
			}
			q.dropped++
			q.live = append(q.live, e)
			q.foldLocked()
			q.signal()
			return pushEvicted
		}
		if Droppable(e.Name) {
			// Backlog is entirely critical and this is redundant data: shed it
			// where nobody will notice a gap in state.
			q.dropped++
			return pushDropped
		}
		// A critical event that does not fit an all-critical backlog. The caller
		// retires the subscriber and appends the resync notice, which is exempt
		// and therefore still lands behind the queued events; this event is lost
		// either way, so the accounting starts here.
		q.dropped++
		return pushLagged
	}
	q.live = append(q.live, e)
	q.foldLocked()
	q.signal()
	return pushOK
}

// pushLast queues e at the very end of the live backlog, past max. Exactly one
// caller uses it: the hub's final events.resync_required for a subscriber it is
// retiring. It must land behind the critical events already queued (that is the
// whole point of the notice), and the bound is allowed to be exceeded by that one
// item because a queue that is being closed cannot keep growing.
func (q *queue) pushLast(e Envelope) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.live = append(q.live, e)
	q.signal()
}

// oldestDroppable returns the index into q.live of the oldest evictable event,
// or -1 when everything still queued is critical. It returns the first hit, so
// in the redundant-frame case that motivated it the answer is the front element
// and the scan stops immediately.
func (q *queue) oldestDroppable() int {
	for i := q.lhead; i < len(q.live); i++ {
		if Droppable(q.live[i].Name) {
			return i
		}
	}
	return -1
}

// pop returns the next queued event. The third result reports the queue closed
// and drained, which is the difference between "end of stream" and "wait".
func (q *queue) pop() (Envelope, bool, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.phead < len(q.prefix) {
		e := q.prefix[q.phead]
		q.prefix[q.phead] = Envelope{}
		q.phead++
		if q.phead == len(q.prefix) {
			q.prefix = q.prefix[:0]
			q.phead = 0
		}
		q.delivered++
		return e, true, false
	}

	if q.lhead < len(q.live) {
		e := q.live[q.lhead]
		q.live[q.lhead] = Envelope{}
		q.lhead++
		q.foldLocked()
		q.delivered++
		return e, true, false
	}
	if q.closed {
		return Envelope{}, false, true
	}
	return Envelope{}, false, false
}

// foldLocked reclaims the consumed front of the live deque once the read offset
// has run at least foldThreshold entries past the start and now covers at least
// half the slice. Both sides call it: a subscriber that keeps up folds as it
// reads, and a stalled one folds as it is shed, so neither can grow the buffer
// with the lifetime of the connection. The pass costs one copy of at most two
// buffers per foldThreshold events, i.e. amortised O(1) per event.
func (q *queue) foldLocked() {
	if q.lhead < foldThreshold || q.lhead*2 < len(q.live) {
		return
	}
	n := copy(q.live, q.live[q.lhead:])
	clear(q.live[n:])
	q.live = q.live[:n]
	q.lhead = 0
}

// foldThreshold is how far the read offset may run ahead of the front of live
// before a fold is worth it. Small enough that a subscriber's memory stays near
// its bound, large enough that the copy is rare.
const foldThreshold = 128

// markLagged records that this subscriber was retired for falling behind. It
// takes the queue's own lock rather than relying on the hub lock, because
// Stats is read from whoever is watching metrics, not from the publish path.
func (q *queue) markLagged() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.lagged = true
}

func (q *queue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.closed = true
	q.signal()
}

func (q *queue) isClosed() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.closed
}

func (q *queue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// SubscriptionStats are the per-subscriber delivery counters, exposed for
// metrics and for the tests that must prove the drop policy actually shed
// something rather than silently growing a queue.
type SubscriptionStats struct {
	// Delivered counts events handed to the consumer.
	Delivered int64
	// Dropped counts events shed to keep this subscriber from ever making a
	// publisher wait: droppable events evicted or refused.
	Dropped int64
	// Pending is the backlog still unread, replay prefix included.
	Pending int
	// Lagged reports the subscriber was closed because its backlog filled with
	// critical events — the one case where docs/spec/08 leaves no event to drop,
	// so the stream ends with events.resync_required.
	Lagged bool
	// Closed reports the queue is closed; queued events may still be draining.
	Closed bool
}

func (q *queue) stats() SubscriptionStats {
	q.mu.Lock()
	defer q.mu.Unlock()
	return SubscriptionStats{
		Delivered: q.delivered,
		Dropped:   q.dropped,
		Pending:   len(q.prefix) - q.phead + q.pending(),
		Lagged:    q.lagged,
		Closed:    q.closed,
	}
}
