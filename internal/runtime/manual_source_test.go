package runtime_test

// The explicitly-driven frame source behind the runtime's test seam
// (Options.ManualSource, kind "manual", docs/spec/12 "deterministic
// delay-injection suite"). A session test that must assert an ordering
// between a frame and the events it causes (a commit, a speech start, a
// cancellation) pushes the frame, waits on the specific events, then pushes
// the next one: the assertion holds on the event stream, and no wall-clock
// margin — however loaded the machine is — can reorder what the test
// observes. Production wires no ManualSource closure, so the kind is
// unreachable there (the runtime answers ErrSourceUnsupported).

import (
	"context"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/capture"
)

// manualSource is a capture.Source whose frames the test pushes by hand.
// Start owns ctx like every source: when the session's context goes away the
// channel closes and the pipeline sees EOF. Close (the test's end of stream)
// is idempotent and safe before Start.
type manualSource struct {
	mu     sync.Mutex
	ch     chan capture.Frame
	closed bool
	seq    int64
}

func newManualSource(buf int) *manualSource {
	return &manualSource{ch: make(chan capture.Frame, buf)}
}

func (m *manualSource) ID() string { return "manual:test" }

func (m *manualSource) Start(ctx context.Context) (<-chan capture.Frame, error) {
	go func() {
		<-ctx.Done()
		m.Close()
	}()
	return m.ch, nil
}

// push delivers one frame. It blocks (bounded) so a push never silently
// drops, and fails loudly if the pipeline stopped consuming — a hung
// session is a test failure, not a deadlocked one. Seq is owned by the
// source, per the capture contract.
func (m *manualSource) push(t *testing.T, f capture.Frame) {
	t.Helper()
	m.mu.Lock()
	m.seq++
	f.Seq = m.seq
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	select {
	case m.ch <- f:
	case <-ctx.Done():
		t.Fatalf("manual source push of frame %d timed out: the pipeline stopped consuming frames", f.Seq)
	}
}

func (m *manualSource) Stats() capture.Stats { return capture.Stats{} }

func (m *manualSource) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	m.closed = true
	close(m.ch)
	return nil
}

// manualClipFrame decodes frame n of the synthetic clip and stamps it with
// the clip's own recorded clock (100 ms cadence, ClockReplay — the same
// values timestamps.json carries). The tracker measures its stability window
// on the source clock, so an explicitly-driven source must carry the
// recording's cadence: the same pixels pushed at microsecond gaps would
// collapse the window the tracker decides on.
func manualClipFrame(t *testing.T, n int) capture.Frame {
	t.Helper()
	corpusPresent(t)
	path := filepath.Join(syntheticDir, fmt.Sprintf("frame-%03d.png", n))
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open clip frame %d: %v", n, err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatalf("decode clip frame %d: %v", n, err)
	}
	b := img.Bounds()
	return capture.Frame{
		SourceClock:        capture.ClockReplay,
		CaptureTimestampNS: int64(n) * 100_000_000,
		Width:              b.Dx(),
		Height:             b.Dy(),
		PixFmt:             capture.PixFmtOf(img),
		Image:              img,
		Hash:               capture.HashFrame(img),
	}
}
