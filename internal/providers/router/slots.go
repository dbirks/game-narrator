package router

import (
	"context"
	"sort"
	"sync"
	"time"
)

// DefaultConcurrency bounds per-role in-flight provider calls (docs/spec/06
// "apply per-role concurrency"). Two is the default: the runtime loop needs
// the role nearly always, and a second slot lets a prefetch run alongside a
// decision without queueing behind it.
const DefaultConcurrency = 2

// waiter is one queued caller. done is closed exactly once, by the slot,
// when the waiter becomes eligible.
type waiter struct {
	prio  Priority
	done  chan struct{}
	woken bool // set by wake under slot.mu so the channel is closed exactly once
}

// slot is a counting semaphore with priority admission: a free slot is
// granted to a waiting Live caller before any waiting Background caller,
// regardless of queue order (docs/spec/06 "background generation receives
// lower priority than runtime"; docs/spec/01 "Runtime and setup isolation").
// Background callers can be paused globally via setPaused: while paused they
// block even if slots are free, and are woken on resume.
type slot struct {
	mu       sync.Mutex
	capacity int
	free     int
	waiters  []*waiter
	bgPaused bool
}

func newSlot(capacity int) *slot {
	if capacity <= 0 {
		capacity = DefaultConcurrency
	}
	return &slot{capacity: capacity, free: capacity}
}

// acquire takes one slot, honouring priority and the background pause, and
// returns ctx.Err() if the context is cancelled while waiting. Each wake
// re-queues with a fresh done channel, so a waiter that loses the race (a
// higher-priority caller arriving in the meantime) simply waits again.
func (s *slot) acquire(ctx context.Context, prio Priority) error {
	for {
		s.mu.Lock()
		if s.free > 0 && (prio == PriorityLive || !s.bgPaused) {
			s.free--
			s.mu.Unlock()
			return nil
		}
		w := &waiter{prio: prio, done: make(chan struct{})}
		s.waiters = append(s.waiters, w)
		s.mu.Unlock()
		select {
		case <-w.done:
		case <-ctx.Done():
			s.removeWaiter(w)
			return ctx.Err()
		}
		s.mu.Lock()
		if s.free > 0 && (prio == PriorityLive || !s.bgPaused) {
			s.free--
			s.removeWaiter(w)
			s.mu.Unlock()
			return nil
		}
		// Woken but no longer eligible: re-queue with a fresh done channel.
		s.removeWaiter(w)
		s.mu.Unlock()
	}
}

// removeWaiter must be called with mu held; it is a no-op when the waiter is
// not queued (already removed or never added).
func (s *slot) removeWaiter(w *waiter) {
	for i, q := range s.waiters {
		if q == w {
			s.waiters = append(s.waiters[:i], s.waiters[i+1:]...)
			return
		}
	}
}

// release returns one slot and wakes the most eligible waiter, if any.
func (s *slot) release() {
	s.mu.Lock()
	s.free++
	if w := s.nextEligible(); w != nil {
		s.wake(w)
	}
	s.mu.Unlock()
}

// wake closes the waiter's done channel exactly once. It must be called with
// mu held: a waiter can be claimed by release and by setPaused concurrently.
func (s *slot) wake(w *waiter) {
	if !w.woken {
		w.woken = true
		close(w.done)
	}
}

// setPaused pauses or resumes Background callers. It re-evaluates the queue:
// on unpause a waiting Background caller may become eligible even though no
// slot was freed (a spurious wake just re-blocks).
func (s *slot) setPaused(paused bool) {
	s.mu.Lock()
	s.bgPaused = paused
	if !paused {
		if w := s.nextEligible(); w != nil {
			s.wake(w)
		}
	}
	s.mu.Unlock()
}

// waiting reports queued waiter counts for tests and diagnostics.
func (s *slot) waiting() (live, background int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, w := range s.waiters {
		switch w.prio {
		case PriorityLive:
			live++
		default:
			background++
		}
	}
	return
}

// nextEligible must be called with mu held. It returns the waiter to wake:
// the earliest Live waiter, or — when no Live waiter is queued and the
// background is not paused — the earliest Background waiter.
func (s *slot) nextEligible() *waiter {
	liveWaiting := false
	for _, w := range s.waiters {
		if w.prio == PriorityLive {
			liveWaiting = true
			break
		}
	}
	for _, w := range s.waiters {
		if w.prio == PriorityBackground && (s.bgPaused || liveWaiting) {
			continue
		}
		return w
	}
	return nil
}

// latRing is a fixed-size ring of attempt latencies (in nanoseconds) used
// for the P50/P95 health figures. It records every attempt — successes and
// failures alike — so a role that keeps timing out shows a rising P95
// instead of a healthy-looking empty ring.
type latRing struct {
	mu  sync.Mutex
	buf []int64
	pos int
	n   int // filled count, at most len(buf)
}

// newLatRing builds a ring holding at most size samples (size <= 0 keeps the
// default 100, the size named in docs/spec/06's health negotiation).
func newLatRing(size int) *latRing {
	if size <= 0 {
		size = DefaultLatencyRingSize
	}
	return &latRing{buf: make([]int64, size)}
}

const DefaultLatencyRingSize = 100

// add records one attempt latency.
func (l *latRing) add(d time.Duration) {
	l.mu.Lock()
	l.buf[l.pos] = int64(d)
	l.pos++
	if l.pos == len(l.buf) {
		l.pos = 0
	}
	if l.n < len(l.buf) {
		l.n++
	}
	l.mu.Unlock()
}

// percentiles returns P50 and P95 over the recorded samples using the
// nearest-rank method (rank = ceil(p * n), 1-based). Zero when empty.
func (l *latRing) percentiles() (p50, p95 time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.n == 0 {
		return 0, 0
	}
	s := make([]int64, l.n)
	// Copy the ring oldest-first.
	start := 0
	if l.n == len(l.buf) {
		start = l.pos // full: oldest is at the write position
	}
	for i := 0; i < l.n; i++ {
		s[i] = l.buf[(start+i)%len(l.buf)]
	}
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return time.Duration(rankAt(s, 0.50)), time.Duration(rankAt(s, 0.95))
}

// rankAt is the nearest-rank percentile on a sorted slice.
func rankAt(s []int64, p float64) int64 {
	n := len(s)
	r := int(p * float64(n))
	if r < 1 {
		r = 1
	}
	if float64(r) < p*float64(n) {
		r++ // ceil
	}
	if r > n {
		r = n
	}
	return s[r-1]
}
