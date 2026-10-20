package runtime

import (
	"context"
	"log/slog"
	"strings"
	"sync"

	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/capture/signal"
	"github.com/dbirks/game-narrator/internal/events"
)

// EventCaptureSignal is the session event the no-signal monitor publishes on
// every signal-state transition of a live v4l2 input (docs/spec/12 "video
// loss/freeze"): the data carries the new state and the reason, so the
// developer UI can show "input lost: black frame (no-signal pattern)" rather
// than an unexplained pause of the narration.
const EventCaptureSignal = "capture.signal"

// The event name is registered at init (the events.Register seam docs/spec/01
// gives later milestones; a name is permanent once published) rather than in
// Manager.New, which other work owns.
func init() {
	if err := events.Register(EventCaptureSignal); err != nil {
		panic(err)
	}
}

// signalSource wraps a capture.Source with the no-signal monitor
// (internal/capture/signal). The wrapper is the "pause matching" half of
// docs/spec/12's resilience row: while the monitor judges the input lost —
// the card emitting its black/blue/static no-signal pattern, freezing on the
// last frame, or mid-mode-change — frames are held back from the pipeline,
// so the tracker sees no frames and commits nothing, and the recovery is
// automatic because the monitor re-admits frames once the content is good
// again. The wrapper also publishes the transitions as EventCaptureSignal
// session events.
//
// Manager.Start applies it only to v4l2 sources (ID prefix "v4l2:"): the
// no-signal concept is a property of the HDMI input path, and the fixture
// sources (replay, test pattern, manual) keep their exact frames — a monitor
// on a replay clip would judge the clip's own dark scenes.
type signalSource struct {
	inner capture.Source
	mon   *signal.Monitor
	hub   *events.Hub
	id    string
	log   *slog.Logger

	out  chan capture.Frame
	done chan struct{}
	wg   sync.WaitGroup
}

// newSignalSource wraps src when it is the live card path (ID prefix
// "v4l2:"); every other source passes through unchanged, which is what keeps
// a dev host and the fixture sources byte-for-byte on the old behaviour.
func (m *Manager) newSignalSource(src capture.Source, sessionID string) capture.Source {
	if src == nil || !strings.HasPrefix(src.ID(), "v4l2:") {
		return src
	}
	return &signalSource{
		inner: src,
		mon:   signal.NewMonitor(),
		hub:   m.opts.Hub,
		id:    sessionID,
		log:   m.log,
	}
}

// ID reports the wrapped source's identity: the wrapper adds no transport of
// its own, and the event's source field must name the card, not the wrapper.
func (s *signalSource) ID() string { return s.inner.ID() }

// Stats reports the wrapped source's stats: the wrapper neither delivers
// frames the source did not produce nor hides the source's own counters. The
// frames held back during a loss are the monitor's judgment, visible in the
// capture.signal events, not the source's drop counters.
func (s *signalSource) Stats() capture.Stats { return s.inner.Stats() }

// Start begins the wrapper's hold-back loop over the inner source's frames.
func (s *signalSource) Start(ctx context.Context) (<-chan capture.Frame, error) {
	frames, err := s.inner.Start(ctx)
	if err != nil {
		return nil, err
	}
	s.out = make(chan capture.Frame, 2)
	s.done = make(chan struct{})
	s.wg.Add(1)
	go s.pump(ctx, frames)
	return s.out, nil
}

// Close stops the inner source and waits for the hold-back loop to drain.
func (s *signalSource) Close() error {
	err := s.inner.Close()
	s.wg.Wait()
	<-s.done
	return err
}

// pump feeds every frame the source produced to the monitor and forwards it
// only while the monitor says the input is live. The monitor sees the
// withheld frames too — that is how it notices the input came back — but the
// pipeline never does, which is the paused matching.
func (s *signalSource) pump(ctx context.Context, frames <-chan capture.Frame) {
	defer close(s.out)
	defer close(s.done)
	defer s.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case f, ok := <-frames:
			if !ok {
				return
			}
			lost, changed, ev := s.mon.Observe(f)
			if changed {
				s.publishEvent(ctx, ev)
			}
			if lost {
				// Held back: the pipeline sees a gap in the frame stream
				// (the Seq gap is the evidence), matching is paused, nothing
				// commits.
				continue
			}
			select {
			case s.out <- f:
			case <-ctx.Done():
				return
			}
		}
	}
}

// publishEvent hands one transition to the session's event stream. A publish
// failure is logged, never returned: the signal state must keep moving (and
// recovering) even when the event log is wedged, and the frame flow is
// unaffected either way.
func (s *signalSource) publishEvent(ctx context.Context, ev signal.Event) {
	e := events.Envelope{
		Name:      EventCaptureSignal,
		SessionID: s.id,
		Data: mustJSON(map[string]any{
			"lost":   ev.Lost,
			"kind":   ev.Kind.String(),
			"detail": ev.Detail,
			"seq":    ev.Seq,
			"source": s.ID(),
		}),
	}
	if _, err := s.hub.Publish(ctx, e); err != nil {
		s.log.Warn("publish capture.signal", "session_id", s.id, "lost", ev.Lost, "error", err)
		return
	}
	s.log.Info("capture signal changed", "session_id", s.id, "lost", ev.Lost, "kind", ev.Kind.String())
}
