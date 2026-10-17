package speech_test

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/speech"
)

// recPub is a Publisher that records every envelope for assertions.
type recPub struct {
	mu  sync.Mutex
	evs []events.Envelope
}

func (r *recPub) Publish(_ context.Context, e events.Envelope) (events.Envelope, error) {
	// implements speech.Publisher
	r.mu.Lock()
	r.evs = append(r.evs, e)
	r.mu.Unlock()
	return e, nil
}

// byName returns the recorded events with the given name, in publish order.
func (r *recPub) byName(name string) []events.Envelope {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []events.Envelope
	for _, e := range r.evs {
		if e.Name == name {
			out = append(out, e)
		}
	}
	return out
}

// testFence is a programmable Fence: the test stores the current generation.
type testFence struct{ cur atomic.Int64 }

func (f *testFence) IsCurrent(g int64) bool { return f.cur.Load() == g }

var _ speech.Publisher = (*recPub)(nil)
var _ speech.Fence = (*testFence)(nil)
