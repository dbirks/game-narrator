// Package dummy is the programmable test AudioSink (docs/spec/07 "Audio
// scheduler": the sink abstraction "includes dummy/file/capture for CI and
// tests"). It paces playback in real time using the WAV's own sample count,
// with configurable start and stop latencies, and records every playback so
// tests can assert on what would have been heard: start/stop timestamps,
// interruptions, and at most one active playback.
package dummy

import (
	"context"
	"sync"
	"time"

	"github.com/dbirks/game-narrator/internal/speech"
)

// Record is one completed (or stopped) playback.
type Record struct {
	UtteranceID string
	Generation  int64
	Started     time.Time // zero when it never started
	Ended       time.Time
	Interrupted bool
	Err         error
}

// Sink is the programmable dummy sink.
type Sink struct {
	// StartDelay is how long the sink takes to report "started" after Play.
	// It models device spin-up; the scheduler's fence check happens before
	// Play, so this delay is exactly the window in which a generation can go
	// stale after the last fence check.
	StartDelay time.Duration
	// StopDelay is how long Stop takes to actually stop delivery.
	StopDelay time.Duration

	mu      sync.Mutex
	records []Record
	active  int
	nextSeq int
}

// New returns a dummy sink with the given start delay (0 → 1 ms) and stop
// delay (0 → 5 ms), both deliberately nonzero so tests exercise the fence
// window and measure a real (if tiny) stop latency.
func New(startDelay, stopDelay time.Duration) *Sink {
	if startDelay <= 0 {
		startDelay = time.Millisecond
	}
	if stopDelay <= 0 {
		stopDelay = 5 * time.Millisecond
	}
	return &Sink{StartDelay: startDelay, StopDelay: stopDelay}
}

// Records returns a copy of the completed-playback records in finish order.
func (s *Sink) Records() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Record, len(s.records))
	copy(out, s.records)
	return out
}

// Starts returns how many playbacks actually started (never-started, i.e.
// stopped before first sample, are excluded — they are the "zero audio
// starts for a stale generation" assertion).
func (s *Sink) Starts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.records {
		if !r.Started.IsZero() {
			n++
		}
	}
	return n
}

// ActiveNow reports how many playbacks are in flight (diagnostic; the
// scheduler guarantees ≤ 1, the sink just reports its own view).
func (s *Sink) ActiveNow() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

// Close is a no-op: the dummy sink holds no resources.
func (s *Sink) Close() error { return nil }

type handle struct {
	sink   *Sink
	req    speech.PlayRequest
	done   chan speech.PlayResult
	stopAt chan struct{}
	once   sync.Once
}

func (h *handle) Stop(fade time.Duration) {
	// Non-blocking and idempotent: the sink applies the fade via its
	// StopDelay pacing; a second Stop is a no-op because the first signal is
	// consumed by the playback goroutine (buffer 1).
	select {
	case h.stopAt <- struct{}{}:
	default:
	}
	_ = fade
}

func (h *handle) Done() <-chan speech.PlayResult { return h.done }

// Play implements speech.AudioSink.
func (s *Sink) Play(ctx context.Context, req speech.PlayRequest) (speech.PlayHandle, error) {
	pcm, err := speech.DecodeWAV(req.WAV)
	if err != nil {
		return nil, err
	}
	dur := time.Duration(float64(pcm.Frames()) / float64(pcm.SampleRate) * float64(time.Second))
	if dur <= 0 {
		return nil, context.Canceled
	}
	h := &handle{
		sink:   s,
		req:    req,
		done:   make(chan speech.PlayResult, 1),
		stopAt: make(chan struct{}, 1),
	}
	s.mu.Lock()
	s.active++
	s.nextSeq++
	s.mu.Unlock()

	go s.run(ctx, h, dur)
	return h, nil
}

// run paces one playback in real time.
func (s *Sink) run(ctx context.Context, h *handle, total time.Duration) {
	defer func() {
		s.mu.Lock()
		s.active--
		s.mu.Unlock()
	}()

	started := time.Time{}
	end := time.Time{}
	interrupted := false

	// Spin-up. Stop may arrive during the spin-up window: then nothing ever
	// starts (the "zero audio starts" case).
	select {
	case <-ctx.Done():
		s.finish(h, started, time.Now(), true, ctx.Err())
		return
	case <-time.After(s.StartDelay):
	case <-h.stopAt:
		s.finish(h, started, time.Now(), true, nil)
		return
	}
	started = time.Now()

	// Pace in small slices so Stop is honored promptly.
	elapsed := time.Duration(0)
	tick := 2 * time.Millisecond
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			end = time.Now()
			interrupted = true
			s.finish(h, started, end, interrupted, ctx.Err())
			return
		case <-h.stopAt:
			end = time.Now().Add(s.StopDelay)
			interrupted = true
			time.Sleep(s.StopDelay)
			s.finish(h, started, end, interrupted, nil)
			return
		case <-t.C:
			elapsed += tick
			if elapsed >= total {
				end = time.Now()
				s.finish(h, started, end, false, nil)
				return
			}
		}
	}
}

// finish records and closes exactly once.
func (s *Sink) finish(h *handle, started, ended time.Time, interrupted bool, err error) {
	h.once.Do(func() {
		rec := Record{
			UtteranceID: h.req.UtteranceID,
			Generation:  h.req.Generation,
			Started:     started,
			Ended:       ended,
			Interrupted: interrupted,
			Err:         err,
		}
		s.mu.Lock()
		s.records = append(s.records, rec)
		s.mu.Unlock()
		h.done <- speech.PlayResult{
			Started:     started,
			Ended:       ended,
			Interrupted: interrupted,
			Err:         err,
		}
	})
}
