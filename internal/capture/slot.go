// These are the two queues docs/spec/01 "Parallelism and deadline
// contracts" distinguishes in one sentence: "One capture loop: bounded
// single-slot latest-frame queue for live runtime, unlike lossless recording."
//
// They are opposite policies on purpose. While a player is talking to a game,
// the newest pixels are the only ones that matter: an ROI worker that is 200 ms
// behind should analyse the current frame, not the one it was handed, so the
// capture loop overwrites rather than waits, and the overwrite is counted so the
// staleness is visible instead of silent. While recording a fixture, every
// frame matters or the recording is a lie, so the producer waits instead and the
// bound is what protects memory.

package capture

import (
	"context"
	"sync"
	"sync/atomic"
)

// LatestSlot is the single-slot mailbox for the live capture loop. Put never
// blocks and never waits for a consumer; if the slot is occupied the older
// frame is discarded and the drop counted. Take returns the newest frame
// available, or reports failure on context cancellation or Close.
//
// Single slot, single producer is the documented assumption: docs/spec/01 says
// "one capture loop". Multiple consumers are allowed (each frame goes to at most
// one of them) but that is how a live ROI pool reads the same feed, and a
// consumer that wants every frame must use Lossless.
//
// The zero value is not usable; call NewLatestSlot.
type LatestSlot struct {
	mu      sync.Mutex
	notifyN chan struct{} // capacity 1, signals "a frame is waiting"
	closed  chan struct{} // closed once, by Close
	slot    Frame
	has     bool
	closing bool

	puts   atomic.Uint64
	drops  atomic.Uint64
	takes  atomic.Uint64
	closes sync.Once
}

// NewLatestSlot returns an empty slot.
func NewLatestSlot() *LatestSlot {
	return &LatestSlot{
		notifyN: make(chan struct{}, 1),
		closed:  make(chan struct{}),
	}
}

// Put publishes f, replacing (and discarding) whatever was in the slot. It
// never blocks and never panics: after Close it reports false and counts the
// frame as dropped, which is how a source whose consumer went away accounts for
// the frames it produced into nothing.
func (s *LatestSlot) Put(f Frame) bool {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		s.drops.Add(1)
		return false
	}
	if s.has {
		s.drops.Add(1)
	}
	s.slot = f
	s.has = true
	s.puts.Add(1)
	s.mu.Unlock()

	// Non-blocking signal: a capacity-1 channel already holding a token means
	// some consumer is about to look, which is all the token is for.
	select {
	case s.notifyN <- struct{}{}:
	default:
	}
	return true
}

// Take returns the newest frame, blocking only until a frame appears, ctx is
// cancelled, or the slot is closed. ok is false in the latter two cases; a
// frame published before Close is still delivered first, so a source that
// closes after end-of-stream cannot lose the last frame.
func (s *LatestSlot) Take(ctx context.Context) (f Frame, ok bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		s.mu.Lock()
		if s.has {
			f = s.slot
			s.has = false
			s.takes.Add(1)
			s.mu.Unlock()
			return f, true
		}
		closing := s.closing
		s.mu.Unlock()
		if closing {
			return Frame{}, false
		}
		select {
		case <-s.notifyN:
			// Loop: the token is a hint, the slot is the truth.
		case <-s.closed:
			// One non-blocking re-check so a frame published immediately before
			// Close is still delivered, then the end of the stream. A loop here
			// would spin at 100% CPU for every waiting consumer of a closed
			// source, which is how a shutdown bug turns into a dead appliance.
			s.mu.Lock()
			if s.has {
				f = s.slot
				s.has = false
				s.takes.Add(1)
				s.mu.Unlock()
				return f, true
			}
			s.mu.Unlock()
			return Frame{}, false
		case <-ctx.Done():
			return Frame{}, false
		}
	}
}

// Drops reports frames discarded by a later Put while the slot was occupied,
// plus frames offered after Close.
func (s *LatestSlot) Drops() uint64 { return s.drops.Load() }

// Puts and Takes bound the conservation check every capture test makes:
// puts == takes + drops + (1 if the slot is currently occupied).
func (s *LatestSlot) Puts() uint64 { return s.puts.Load() }

// Takes reports frames handed to consumers.
func (s *LatestSlot) Takes() uint64 { return s.takes.Load() }

// Close wakes every waiting consumer and stops the slot. Idempotent.
func (s *LatestSlot) Close() {
	s.closes.Do(func() {
		s.mu.Lock()
		s.closing = true
		s.mu.Unlock()
		close(s.closed)
	})
}

// Receive bridges the slot to a channel for consumers that want to select on
// frames rather than block in Take (docs/spec/08's SSE fanout, the dev
// inspector). Each frame is still available to a concurrent Take, so this is
// for the single-consumer case it is used in, not a second subscription.
// The returned channel closes when the slot closes and drains, or ctx ends.
func (s *LatestSlot) Receive(ctx context.Context) <-chan Frame {
	out := make(chan Frame)
	go func() {
		defer close(out)
		for {
			f, ok := s.Take(ctx)
			if !ok {
				return
			}
			select {
			case out <- f:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

// Lossless is the bounded queue the recording path uses: nothing is dropped, so
// a slow consumer applies backpressure to the producer instead of silently
// deleting evidence (docs/spec/12 "Frame replay" — a recording whose frame count
// does not match its manifest is not a recording).
//
// Unlike LatestSlot, Put blocks. That is the whole tradeoff and why it is not
// the live path: on a 4-core appliance a stalled recorder must be able to make
// FFmpeg back up and drop frames (with the drop counted) rather than let a pipe
// grow without bound.
//
// The zero value is not usable; call NewLossless.
type Lossless struct {
	mu      sync.Mutex
	ch      chan Frame
	closed  chan struct{}
	once    sync.Once
	closing bool
	count   atomic.Int64 // frames currently queued
}

// NewLossless returns a queue with the given capacity (clamped to ≥ 1). The
// capacity is the memory ceiling: capacity × frame bytes is the worst-case
// footprint, so a caller recording 1080p rgb24 at capacity 60 should understand
// it is reserving ~375 MB (docs/spec/13 "backpressure and memory ceilings").
func NewLossless(capacity int) *Lossless {
	if capacity < 1 {
		capacity = 1
	}
	return &Lossless{
		ch:     make(chan Frame, capacity),
		closed: make(chan struct{}),
	}
}

// Put enqueues f, blocking while the queue is full. It returns false — and
// drops the frame — only when ctx is cancelled or the queue is closed, which
// are the two cases where "we could not record this" is a fact the caller has
// to record rather than a frame to squeeze in.
func (l *Lossless) Put(ctx context.Context, f Frame) bool {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-l.closed:
		return false
	default:
	}
	select {
	case l.ch <- f:
		l.count.Add(1)
		return true
	case <-ctx.Done():
		return false
	case <-l.closed:
		// A Close that raced the enqueue: prefer to drain the frame rather
		// than block forever, since ch's buffer may still have room.
		select {
		case l.ch <- f:
			l.count.Add(1)
			return true
		default:
			return false
		}
	}
}

// Take dequeues the oldest frame (FIFO — unlike LatestSlot, order and
// completeness are the point). ok is false once the queue is closed and drained
// or ctx is cancelled.
func (l *Lossless) Take(ctx context.Context) (f Frame, ok bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case f := <-l.ch:
		l.count.Add(-1)
		return f, true
	default:
	}
	select {
	case f := <-l.ch:
		l.count.Add(-1)
		return f, true
	case <-ctx.Done():
		return Frame{}, false
	case <-l.closed:
		// Drain what was already queued before reporting the end.
		select {
		case f := <-l.ch:
			l.count.Add(-1)
			return f, true
		default:
			return Frame{}, false
		}
	}
}

// Len reports the current queue depth, for the "queue nearly full" warning.
func (l *Lossless) Len() int64 { return l.count.Load() }

// Close stops producers and, once drained, consumers. Idempotent.
func (l *Lossless) Close() {
	l.once.Do(func() {
		l.mu.Lock()
		l.closing = true
		l.mu.Unlock()
		close(l.closed)
	})
}
