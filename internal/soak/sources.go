// sources.go is the soak's frame source: one replay pass per clip, cycled.
// Passes are the load unit — each pass replays a clip's frames at the
// configured speed (Speed 0: full tilt, the sustained "replay load" of
// docs/spec/12) and the pipeline processes every pass it is handed. The chaos
// drop mask is applied per pass so both layers see the same frames (the
// scenario's keepMask rule).

package soak

import (
	"context"
	"errors"
	"path/filepath"
	"sync"

	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/capture/replay"
)

// cyclingSource delivers the clips' frames, one pass of one clip at a time,
// cycling through the clip list until the context is done or Close. Frame
// sequence numbers are global (they never restart across passes), which keeps
// the region tracker's monotonic-seq contract honest across the whole run.
type cyclingSource struct {
	repoRoot string
	clips    []clip
	speed    float64

	mu       sync.Mutex
	started  bool
	state    capture.State
	lastErr  string
	cancel   context.CancelFunc
	done     chan struct{}
	sequence int64
	dropped  int64
}

func newCyclingSource(repoRoot string, clips []clip, speed float64) *cyclingSource {
	return &cyclingSource{repoRoot: repoRoot, clips: clips, speed: speed, state: capture.StateStarting}
}

// ID implements capture.Source.
func (s *cyclingSource) ID() string {
	return "soak:" + filepath.Base(filepath.Join(s.repoRoot, s.clips[0].dir))
}

// Stats implements capture.Source.
func (s *cyclingSource) Stats() capture.Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return capture.Stats{State: s.state, DroppedSlot: uint64(s.dropped), LastError: s.lastErr}
}

// Start implements capture.Source.
func (s *cyclingSource) Start(ctx context.Context) (<-chan capture.Frame, error) {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return nil, errors.New("soak source: already started")
	}
	s.started = true
	s.state = capture.StateRunning
	out := make(chan capture.Frame)
	s.done = make(chan struct{})
	runCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.mu.Unlock()

	go s.run(runCtx, out)
	return out, nil
}

// Close implements capture.Source.
func (s *cyclingSource) Close() error {
	s.mu.Lock()
	if s.state != capture.StateFailed {
		s.state = capture.StateStopped
	}
	cancel := s.cancel
	done := s.done
	started := s.started
	s.mu.Unlock()
	if !started {
		return nil
	}
	cancel()
	<-done
	return nil
}

// run cycles the passes: clip 0, clip 1, …, wrap. Each pass is one un-looped
// replay.Source (with the chaos dropper on top when the clip carries a mask);
// the pass source's channel close ends the pass and the next one starts.
func (s *cyclingSource) run(ctx context.Context, out chan<- capture.Frame) {
	defer close(out)
	defer close(s.done)
	for {
		for _, c := range s.clips {
			src := &replay.Source{
				Dir:   filepath.Join(s.repoRoot, c.dir),
				Speed: s.speed,
				Loop:  false,
			}
			var pass capture.Source = src
			var d *frameDropper
			if c.dropMask != nil {
				d = &frameDropper{inner: src, rows: len(c.truth), mask: c.dropMask}
				pass = d
			}
			ch, err := pass.Start(ctx)
			if err != nil {
				s.setFailed(err)
				return
			}
			if !s.deliver(ctx, ch, out) {
				// Context done: close the pass source and stop cycling.
				_ = pass.Close()
				if d != nil {
					s.addDrops(d)
				}
				return
			}
			_ = pass.Close()
			if d != nil {
				s.addDrops(d)
			}
			if ctx.Err() != nil {
				return
			}
		}
	}
}

// deliver forwards one pass's frames to the output channel, re-stamping the
// global sequence. It reports whether the pass ran to its natural end.
func (s *cyclingSource) deliver(ctx context.Context, in <-chan capture.Frame, out chan<- capture.Frame) bool {
	for f := range in {
		if ctx.Err() != nil {
			return false
		}
		f.Seq = s.nextSeq()
		select {
		case out <- f:
		case <-ctx.Done():
			return false
		}
	}
	return true
}

func (s *cyclingSource) nextSeq() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sequence++
	return s.sequence
}

func (s *cyclingSource) addDrops(d *frameDropper) {
	s.mu.Lock()
	s.dropped += d.Dropped()
	s.mu.Unlock()
}

func (s *cyclingSource) setFailed(err error) {
	s.mu.Lock()
	if s.state != capture.StateFailed {
		s.state = capture.StateFailed
		s.lastErr = err.Error()
	}
	s.mu.Unlock()
}

// start is the harness's convenience wrapper: the source runs on the run's
// context so the soak's stop path (cancel) is its only stop.
func (s *cyclingSource) start(ctx context.Context) error {
	_, err := s.Start(ctx)
	return err
}
