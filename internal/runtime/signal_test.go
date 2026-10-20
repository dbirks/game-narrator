package runtime_test

// The no-signal path through the real session machinery (docs/spec/12
// "video loss/freeze", "repeated identical frames"): a v4l2-kind source whose
// frames the test pushes by hand (the Options.ManualSource seam with a card
// identity) — content, then the card's black no-signal pattern, then content
// again. The assertions are on the session's event stream and commit
// behaviour, which is where the operator and the pipeline see the difference:
//
//   - the loss and the recovery are published as capture.signal events with
//     the reason in the payload;
//   - while the input is lost, the frames that would have committed are held
//     back — matching is paused and nothing commits;
//   - after the recovery the same frames commit: the hold was a pause, not a
//     drop of the pipeline's ability to decide, and the recovery was
//     automatic (no operator stop/start).

import (
	"encoding/json"
	"image"
	"image/color"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/runtime"
)

// cardSource is the manual source behind the v4l2 identity: the same push
// machinery, the ID the Manager's no-signal wrapper keys on.
type cardSource struct {
	*manualSource
}

func (c *cardSource) ID() string { return "v4l2:/dev/video9" }

// blackCardFrame is the card's no-HDMI frame: black, with one pixel that
// differs per frame. The pixel keeps the frame hashes distinct, so the
// monitor classifies the content (Black) rather than the frozen case — a
// perfectly identical stream is the card repeating its last frame, and the
// unit tests in internal/capture/signal pin that path separately.
func blackCardFrame(tsNS int64) capture.Frame {
	img := image.NewRGBA(image.Rect(0, 0, 640, 480))
	mark := int((tsNS / 100_000_000) % 8)
	img.Set(0, 0, color.RGBA{uint8(mark), 0, 0, 255})
	return capture.Frame{
		SourceClock:        capture.ClockMonotonic,
		CaptureTimestampNS: tsNS,
		Width:              640,
		Height:             480,
		PixFmt:             capture.PixRGBA,
		Image:              img,
		Hash:               capture.HashFrame(img),
	}
}

func TestV4L2NoSignalHoldsFramesAndRecovers(t *testing.T) {
	corpusPresent(t)
	card := &cardSource{newManualSource(64)}
	e := newEnv(t, withFakeOCR(truthScript(t), 0), withManualSource(func() (capture.Source, error) {
		return card, nil
	}))
	profileID, _ := seedSyntheticProfile(t, e.st, true)
	info, err := e.mgr.Start(e.ctx, runtime.StartRequest{
		ProfileID:   profileID,
		FrameSource: runtime.FrameSource{Kind: runtime.SourceKindManual},
		Mode:        runtime.ModeConservative,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	sid := info.ID
	defer waitState(t, e.mgr, sid, 60*time.Second)
	defer func() { _ = card.Close() }()

	var ts int64
	pushContent := func(n int) {
		ts += 100_000_000
		card.push(t, stamped(manualClipFrame(t, n), ts))
	}
	pushBlack := func() {
		ts += 100_000_000
		card.push(t, blackCardFrame(ts))
	}

	// Live content: three frames the pipeline receives (pre-reveal, so no
	// commit is on its way yet).
	for n := 1; n <= 3; n++ {
		pushContent(n)
	}

	// The HDMI goes out: the card emits its black pattern. The monitor's gate
	// is three consecutive lost frames; the first two still reach the pipeline
	// (hysteresis is the point — one corrupt frame is not an input loss), and
	// from the third the flow is held.
	pushBlack()
	pushBlack()
	// The gate frame: the loss is declared on this one and it is held.
	pushBlack()
	// While lost, a frame that is on the way to a commit (clip frame 4) is
	// held back with it: matching is paused, nothing commits on held frames.
	pushContent(4)

	e.waitDecisionEvents(t, sid, "capture.signal loss", 60*time.Second, func(evs []events.Envelope) bool {
		n := 0
		for _, ev := range evs {
			if ev.Name == runtime.EventCaptureSignal && signalEventLost(t, ev) {
				n++
			}
		}
		return n >= 1
	})

	// No commit may exist yet: every frame on the way to one was held.
	evs := eventsFrom(t, e, sid)
	if n := countName(evs, events.MessageCommitted); n != 0 {
		t.Fatalf("%d commits before recovery, want 0 (matching must be paused while the input is lost)", n)
	}

	// The plug comes back: two good frames clear the recovery gate, and the
	// same reveal sequence now commits — the hold was a pause, and the
	// recovery was automatic.
	pushContent(5)
	pushContent(6)
	for n := 7; n <= 15; n++ {
		pushContent(n)
	}
	evs = e.waitDecisionEvents(t, sid, "line a committed after recovery", 90*time.Second, func(evs []events.Envelope) bool {
		return countName(evs, events.MessageCommitted) >= 1
	})

	// The event log carries exactly the two transitions, in order: loss first,
	// recovery second.
	losses, recoveries := 0, 0
	for _, ev := range evs {
		if ev.Name != runtime.EventCaptureSignal {
			continue
		}
		if signalEventLost(t, ev) {
			losses++
		} else {
			recoveries++
		}
	}
	if losses != 1 || recoveries != 1 {
		t.Fatalf("capture.signal transitions: %d loss / %d recovery, want 1 / 1 (no flapping)", losses, recoveries)
	}

	// The held frames are visible in the delivered count: fewer frames reached
	// the pipeline than the source produced (the gate frame and the held
	// frame 4 at least; coalescing may absorb more, never less).
	if snap, ok := e.mgr.Get(sid); !ok || snap.Stats.FramesDelivered >= 18 {
		t.Fatalf("frames delivered = %d, want < 18 (the 18 pushed minus the held loss frames)", snap.Stats.FramesDelivered)
	}
}

// stamped stamps a clip frame with the test's running clock: the manual
// source hands out clip content, but the signal test drives its own
// timestamps so the freeze window and the stability window see one clock.
func stamped(f capture.Frame, tsNS int64) capture.Frame {
	f.CaptureTimestampNS = tsNS
	return f
}

// signalEventLost decodes the capture.signal payload and reports its lost
// half.
func signalEventLost(t *testing.T, ev events.Envelope) bool {
	t.Helper()
	var p struct {
		Lost   bool   `json:"lost"`
		Kind   string `json:"kind"`
		Source string `json:"source"`
	}
	if err := json.Unmarshal(ev.Data, &p); err != nil {
		t.Fatalf("decode capture.signal payload %q: %v", string(ev.Data), err)
	}
	if p.Source != "v4l2:/dev/video9" {
		t.Fatalf("capture.signal source = %q, want the card identity the wrapper names", p.Source)
	}
	return p.Lost
}
