package speech

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"sync"
)

// Priority orders waiting generation work. docs/spec/07 "Cache": prioritize
// "current committed" > "likely next" > "batch background". The ordering is
// used only for admission into the bounded generation pool: it never
// preempts work already running (a preemption policy would re-order TTS
// provider calls mid-flight, which the provider contract does not allow).
type Priority int

const (
	// PriorityBackground is batch prewarming of successor messages.
	PriorityBackground Priority = 0
	// PriorityNext is the likely-next utterance.
	PriorityNext Priority = 1
	// PriorityCurrent is the currently committed utterance.
	PriorityCurrent Priority = 2
)

// String names the priority in logs and events.
func (p Priority) String() string {
	switch p {
	case PriorityCurrent:
		return "current"
	case PriorityNext:
		return "next"
	case PriorityBackground:
		return "background"
	default:
		return fmt.Sprintf("priority(%d)", int(p))
	}
}

// ErrLimitClosed reports an acquire on a limiter that has been closed.
var ErrLimitClosed = errors.New("speech: limiter closed")

// genLimiter bounds concurrent TTS generations (docs/spec/07 "bounded
// concurrent generation") and admits waiting callers by Priority, FIFO within
// a priority. It is a semaphore with a priority wait queue, not a queue of
// slots: release wakes the single highest-priority waiter, so a background
// batch can never starve the front of the line, and a background caller can
// never push the current utterance behind another background caller.
//
// Fairness note (ADR-0023): a fresh high-priority arrival does NOT cut in
// front of a same-or-higher-priority waiter already queued (no unbounded
// head-of-line jumping across priorities); it only beats strictly lower
// priority waiters. This keeps the admission order total and testable.
type genLimiter struct {
	mu     sync.Mutex
	free   int
	closed bool
	wait   *waiterQueue // min-heap by (priority, arrival)
	seq    uint64
}

type waiter struct {
	prio    Priority
	arrival uint64
	done    chan struct{}
}

type waiterQueue []*waiter

func (q waiterQueue) Len() int { return len(q) }
func (q waiterQueue) Less(i, j int) bool {
	if q[i].prio != q[j].prio {
		return q[i].prio > q[j].prio // higher priority first
	}
	return q[i].arrival < q[j].arrival
}
func (q waiterQueue) Swap(i, j int) { q[i], q[j] = q[j], q[i] }
func (q *waiterQueue) Push(x any)   { *q = append(*q, x.(*waiter)) }
func (q *waiterQueue) Pop() any     { w := *q; last := w[len(w)-1]; *q = w[:len(w)-1]; return last }

var _ heap.Interface = (*waiterQueue)(nil)

func newGenLimiter(n int) *genLimiter {
	if n < 1 {
		n = 1
	}
	return &genLimiter{free: n, wait: &waiterQueue{}}
}

// acquire takes one permit, waiting (in priority order) until one is free or
// ctx is done / the limiter closes.
func (l *genLimiter) acquire(ctx context.Context, p Priority) error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return ErrLimitClosed
	}
	if l.free > 0 {
		l.free--
		l.mu.Unlock()
		return nil
	}
	w := &waiter{prio: p, arrival: l.seq, done: make(chan struct{})}
	l.seq++
	heap.Push(l.wait, w)
	l.mu.Unlock()

	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		// Wake ourselves up and let the slot go to someone else. The waiter
		// may still be popped and closed later; that is harmless.
		return ctx.Err()
	}
}

// release returns one permit and wakes the highest-priority waiter, if any.
// It must be called exactly once per successful acquire.
func (l *genLimiter) release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		l.free++
		return
	}
	var w *waiter
	if l.wait.Len() > 0 {
		w = heap.Pop(l.wait).(*waiter)
	} else {
		l.free++
	}
	if w != nil {
		close(w.done)
	}
}

// close wakes every waiter with ErrLimitClosed (indirectly: their ctx or the
// done channels) and stops admitting new work. Used by Cache close paths.
func (l *genLimiter) close() {
	l.mu.Lock()
	l.closed = true
	for l.wait.Len() > 0 {
		w := heap.Pop(l.wait).(*waiter)
		close(w.done)
	}
	l.mu.Unlock()
}

// pending reports the number of callers currently queued (diagnostics).
func (l *genLimiter) pending() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.wait.Len()
}
