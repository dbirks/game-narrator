// faults_test.go proves, per fault kind, that the fault fires at the time
// the scenario YAML configures it to (AGENTS.md: "each fault kind has a
// unit test proving it fires at the configured time"). The tests drive the
// same wrapper/gateway code the scenario harness runs, with the real
// provider clients where one exists, so the proof is about the wrapper the
// harness uses, not about a second mock.
package scenario

import (
	"context"
	stderrors "errors"
	"image"
	"image/color"
	"sync"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/systemone"
	"github.com/dbirks/game-narrator/internal/providers/tts"
	"github.com/dbirks/game-narrator/internal/session"
)

// memSource is a capture.Source that delivers prebuilt frames, for the
// drop_frames wrapper test.
type memSource struct {
	frames []capture.Frame
}

func (s *memSource) ID() string { return "mem:test" }

func (s *memSource) Start(ctx context.Context) (<-chan capture.Frame, error) {
	out := make(chan capture.Frame, len(s.frames))
	go func() {
		defer close(out)
		for _, f := range s.frames {
			select {
			case out <- f:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

func (s *memSource) Stats() capture.Stats { return capture.Stats{} }
func (s *memSource) Close() error         { return nil }

// testFrames builds n frames at 100 ms capture spacing (100 ms .. n*100 ms).
func testFrames(n int) []capture.Frame {
	frames := make([]capture.Frame, 0, n)
	for i := 0; i < n; i++ {
		frames = append(frames, capture.Frame{
			Seq:                int64(i + 1),
			SourceClock:        capture.ClockReplay,
			CaptureTimestampNS: int64((i+1)*100) * int64(time.Millisecond),
			Width:              640,
			Height:             480,
			Image:              image.NewUniform(color.Black),
			Hash:               "frame-hash-" + string(rune('a'+i)),
		})
	}
	return frames
}

// TestFaultDropFramesFiresAtConfiguredTime: the wrapper withholds exactly
// the frames whose capture time sits inside [from_ms, to_ms) and reports the
// fault once, at the first withheld frame's capture time.
func TestFaultDropFramesFiresAtConfiguredTime(t *testing.T) {
	inner := &memSource{frames: testFrames(10)} // 100..1000 ms
	var (
		mu    sync.Mutex
		fired []int64
	)
	inj := newFaultInjector(1, func(_ int, kind string, timeMS int64, _, _ string) {
		mu.Lock()
		fired = append(fired, timeMS)
		mu.Unlock()
		if kind != FaultDropFrames {
			t.Errorf("fault kind = %q, want %q", kind, FaultDropFrames)
		}
	})
	src := &DropFrameSource{
		inner:      inner,
		FromNS:     int64(400) * int64(time.Millisecond),
		ToNS:       int64(600) * int64(time.Millisecond),
		FaultIndex: 0,
		Injector:   inj,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ch, err := src.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var got []int64
	for f := range ch {
		got = append(got, f.CaptureTimestampNS/1e6)
	}
	want := []int64{100, 200, 300, 600, 700, 800, 900, 1000}
	if len(got) != len(want) {
		t.Fatalf("delivered %d frames %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("frame %d delivered at %d ms, want %d ms (the window [400,600) covers 400 and 500 ms)", i, got[i], want[i])
		}
	}
	if src.Dropped() != 2 {
		t.Errorf("wrapper withheld %d frames, want 2 (400 and 500 ms)", src.Dropped())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(fired) != 1 {
		t.Fatalf("fault reported %d times, want 1 (first activation only)", len(fired))
	}
	if fired[0] != 400 {
		t.Fatalf("fault fired at %d ms, want 400 ms (the first withheld frame)", fired[0])
	}
}

// decisionFixture returns a fresh decision gateway plus the real client that
// will talk to it, preloaded with the fault windows the test names.
func decisionFixture(t *testing.T, injector *faultInjector, downFrom, downTo, idxDown, delayAfter, delay, idxDelay int) (*decisionGateway, *systemone.Client) {
	t.Helper()
	gw := newDecisionGateway("clef-fake", injector, downFrom, downTo, idxDown, delayAfter, delay, idxDelay)
	t.Cleanup(gw.Close)
	client, err := systemone.New(gw.url(), "test-token", "clef-fake")
	if err != nil {
		t.Fatalf("systemone.New: %v", err)
	}
	return gw, client
}

func decisionRequest(captureMS int64) systemone.Request {
	return systemone.Request{
		Model: "clef-fake",
		State: map[string]any{
			"capture_ts_ms":  captureMS,
			"confirm_choice": "confirmed",
			"text":           "the line",
		},
		Questions: []systemone.Question{{
			ID:           "q1",
			Type:         systemone.TypeChoice,
			Instructions: "confirm the line",
			Criteria: []systemone.Option{
				{ID: "confirmed", Description: "the line"},
				{ID: "unknown", Description: "no match"},
			},
		}},
	}
}

// providersKind reduces one provider failure to its taxonomy kind.
func providersKind(err error) (providers.Kind, bool) {
	var pe *providers.Error
	if stderrors.As(err, &pe) {
		return pe.Kind, true
	}
	return "", false
}

// TestFaultDecisionDelayFiresAtConfiguredTime: a request captured before
// after_ms is answered promptly; one at/after it is held for delay_ms, and
// the fault is reported at the request's capture time, once.
func TestFaultDecisionDelayFiresAtConfiguredTime(t *testing.T) {
	var (
		mu    sync.Mutex
		fired []int64
	)
	inj := newFaultInjector(1, func(_ int, kind string, timeMS int64, _, _ string) {
		mu.Lock()
		fired = append(fired, timeMS)
		mu.Unlock()
		if kind != FaultDecisionDelay {
			t.Errorf("fault kind = %q, want %q", kind, FaultDecisionDelay)
		}
	})
	gw, client := decisionFixture(t, inj, 0, 0, -1, 1300, 300, 0)
	ctx := context.Background()

	// Before the window: no hold, no fault.
	start := time.Now()
	if _, err := client.Decide(ctx, decisionRequest(1000)); err != nil {
		t.Fatalf("decide before the window: %v", err)
	}
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("request at 1000 ms (before after_ms=1300) took %s, want prompt (no delay)", d)
	}

	// At the window: held ~delay_ms, fault reported at the capture time.
	start = time.Now()
	if _, err := client.Decide(ctx, decisionRequest(1300)); err != nil {
		t.Fatalf("decide at the window: %v", err)
	}
	d := time.Since(start)
	if d < 250*time.Millisecond || d > 3*time.Second {
		t.Fatalf("request at 1300 ms (after_ms=1300, delay_ms=300) took %s, want ~300 ms", d)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(fired) != 1 {
		t.Fatalf("fault reported %d times, want 1", len(fired))
	}
	if fired[0] != 1300 {
		t.Fatalf("fault fired at capture %d ms, want 1300 ms (the delayed request)", fired[0])
	}
	if gw.Errors() != 0 {
		t.Fatalf("gateway sent %d 503 answers, want 0 (a delay is not an error)", gw.Errors())
	}
}

// TestFaultEndpointDownFiresAtConfiguredTime: inside the window the decision
// endpoint answers 503 and the real client classifies it as
// providers.KindUnavailable (the abstain path); outside the window the
// answer comes back. The fault is reported once, at the first faulted
// request's capture time.
func TestFaultEndpointDownFiresAtConfiguredTime(t *testing.T) {
	var (
		mu    sync.Mutex
		fired []int64
	)
	inj := newFaultInjector(1, func(_ int, kind string, timeMS int64, role, _ string) {
		mu.Lock()
		fired = append(fired, timeMS)
		mu.Unlock()
		if kind != FaultEndpointDown {
			t.Errorf("fault kind = %q, want %q", kind, FaultEndpointDown)
		}
		if role != RoleDecision {
			t.Errorf("fault role = %q, want %q", role, RoleDecision)
		}
	})
	gw, client := decisionFixture(t, inj, 1200, 2000, 0, 0, 0, -1)
	ctx := context.Background()

	// Inside the window: 503 → unavailable, the abstain path.
	_, err := client.Decide(ctx, decisionRequest(1500))
	if err == nil {
		t.Fatal("decide inside the down window: want an error, got none")
	}
	if kind, ok := providersKind(err); !ok || kind != providers.KindUnavailable {
		t.Fatalf("error kind = %q (ok=%v), want %q (a 503 classifies as unavailable)", kind, ok, providers.KindUnavailable)
	}

	// Outside the window: the answer comes back.
	if _, err := client.Decide(ctx, decisionRequest(2100)); err != nil {
		t.Fatalf("decide after the window (2100 ms): %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(fired) != 1 {
		t.Fatalf("fault reported %d times, want 1", len(fired))
	}
	if fired[0] != 1500 {
		t.Fatalf("fault fired at capture %d ms, want 1500 ms (the first 503)", fired[0])
	}
	if gw.Errors() != 1 {
		t.Fatalf("gateway sent %d 503 answers, want 1", gw.Errors())
	}
}

// TestFaultVoiceMissingFiresAtConfiguredTime: the TTS gateway answers the
// unknown-voice error only for the named voice, and the real client
// classifies it as providers.KindVoiceMissing; another voice synthesizes.
func TestFaultVoiceMissingFiresAtConfiguredTime(t *testing.T) {
	var (
		mu    sync.Mutex
		fired []int64
	)
	inj := newFaultInjector(1, func(_ int, kind string, timeMS int64, _, _ string) {
		mu.Lock()
		fired = append(fired, timeMS)
		mu.Unlock()
		if kind != FaultVoiceMissingReplica {
			t.Errorf("fault kind = %q, want %q", kind, FaultVoiceMissingReplica)
		}
	})
	gw := newTTSGateway("tts-fake", "gone-voice", inj, 0, 0, 0)
	t.Cleanup(gw.Close)
	client, err := tts.New(gw.url(), "test-token", "tts-fake")
	if err != nil {
		t.Fatalf("tts.New: %v", err)
	}
	ctx := context.Background()

	// The named voice: the replica's unknown-voice error, classified.
	_, err = client.Generate(ctx, tts.GenerateRequest{Text: "hello", Voice: "gone-voice"})
	if err == nil {
		t.Fatal("generate the missing voice: want an error, got none")
	}
	if kind, ok := providersKind(err); !ok || kind != providers.KindVoiceMissing {
		t.Fatalf("error kind = %q (ok=%v), want %q (400 unknown-voice classifies as voice_missing)", kind, ok, providers.KindVoiceMissing)
	}

	// A different voice: real WAV bytes back.
	audio, err := client.Generate(ctx, tts.GenerateRequest{Text: "hello", Voice: "other-voice"})
	if err != nil {
		t.Fatalf("generate a present voice: %v", err)
	}
	if len(audio.WAV) == 0 {
		t.Fatal("no WAV bytes back for the present voice")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(fired) != 1 {
		t.Fatalf("fault reported %d times, want 1 (the first unknown-voice answer)", len(fired))
	}
	if gw.Missing() != 1 {
		t.Fatalf("gateway sent %d unknown-voice answers, want 1", gw.Missing())
	}
}

// TestFaultDecisionDuplicateFiresAtConfiguredTime: the same decision reply,
// same sequence, delivered twice — the real session's duplicate-seq fence
// discards the second delivery, and the fault is reported at the commit's
// capture time, once.
func TestFaultDecisionDuplicateFiresAtConfiguredTime(t *testing.T) {
	var (
		mu    sync.Mutex
		fired []int64
	)
	inj := newFaultInjector(1, func(_ int, kind string, timeMS int64, role, _ string) {
		mu.Lock()
		fired = append(fired, timeMS)
		mu.Unlock()
		if kind != FaultDecisionDuplicate {
			t.Errorf("fault kind = %q, want %q", kind, FaultDecisionDuplicate)
		}
		if role != RoleDecision {
			t.Errorf("fault role = %q, want %q", role, RoleDecision)
		}
	})
	pub := &captureOnlyPub{}
	s, err := session.New("fault-dup", "1", pub)
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	commitMS := int64(1500) // the commit's capture time the harness stamps
	dm := session.DecisionMsg{
		Header: session.Header{
			SessionID: "fault-dup", Seq: 1, Generation: s.Generation(), ProfileRevision: "1",
		},
		RegionID:       "bottom",
		ObservationSeq: 1,
		MessageID:      "demo-page-1",
		SpeechEligible: true,
		Accepted:       true,
		Provider:       "systemone",
	}
	ctx := context.Background()
	res1, err := s.Apply(ctx, dm)
	if err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if !res1.Applied {
		t.Fatalf("first delivery not applied: %s", res1.Reason)
	}
	// The gateway redelivers the same reply, same seq.
	first := inj.fire(0, Fault{Type: FaultDecisionDuplicate, Message: dm.MessageID}, commitMS,
		"decision reply for demo-page-1 delivered twice (same seq)")
	if !first {
		t.Fatal("fault injector refused the first activation")
	}
	res2, err := s.Apply(ctx, dm)
	if err != nil {
		t.Fatalf("second delivery: %v", err)
	}
	if res2.Applied {
		t.Fatal("second delivery applied: the duplicate-seq fence must discard it")
	}
	if res2.Reason != "duplicate_seq" {
		t.Fatalf("second delivery reason = %q, want duplicate_seq", res2.Reason)
	}
	if st := s.Stats(); st.Duplicated != 1 {
		t.Fatalf("session Duplicated = %d, want 1", st.Duplicated)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(fired) != 1 || fired[0] != commitMS {
		t.Fatalf("fault fired at %v, want once at %d ms (the commit's capture time)", fired, commitMS)
	}
	// A redelivery with a fresh seq is not a duplicate: it is a new delivery.
	dm2 := dm
	dm2.Header.Seq = 2
	res3, err := s.Apply(ctx, dm2)
	if err != nil {
		t.Fatalf("fresh-seq delivery: %v", err)
	}
	if !res3.Applied {
		t.Fatalf("fresh-seq delivery not applied: %s", res3.Reason)
	}
}

// TestFaultTTSEndpointDownViaWrapper: the tts endpoint_down window rides the
// faultedTTS wrapper (the synthesis payload carries no capture clock), with
// the typed unavailable error a 503 classifies to; outside the window the
// wrapper is transparent.
func TestFaultTTSEndpointDownViaWrapper(t *testing.T) {
	var (
		mu    sync.Mutex
		fired []int64
	)
	inj := newFaultInjector(1, func(_ int, kind string, timeMS int64, role, _ string) {
		mu.Lock()
		fired = append(fired, timeMS)
		mu.Unlock()
		if kind != FaultEndpointDown || role != RoleTTS {
			t.Errorf("fault = %q/%q, want %q/%q", kind, role, FaultEndpointDown, RoleTTS)
		}
	})
	gw := newTTSGateway("tts-fake", "", inj, -1, 0, 0)
	t.Cleanup(gw.Close)
	client, err := tts.New(gw.url(), "test-token", "tts-fake")
	if err != nil {
		t.Fatalf("tts.New: %v", err)
	}
	wrap := &faultedTTS{client: client, fromMS: 1900, toMS: 2100, injector: inj, idx: 0}
	ctx := context.Background()

	// Inside the window: the wrapper refuses before the wire.
	wrap.stampMS = 1950
	_, err = wrap.Generate(ctx, tts.GenerateRequest{Text: "hello", Voice: "v"})
	if err == nil {
		t.Fatal("generate inside the tts down window: want an error, got none")
	}
	if kind, ok := providersKind(err); !ok || kind != providers.KindUnavailable {
		t.Fatalf("error kind = %q (ok=%v), want %q (the 503 classification)", kind, ok, providers.KindUnavailable)
	}
	if gw.Hits() != 0 {
		t.Fatalf("gateway saw %d request(s) inside the window, want 0 (the wrapper refuses before the wire)", gw.Hits())
	}

	// Outside the window: transparent.
	wrap.stampMS = 2200
	if _, err := wrap.Generate(ctx, tts.GenerateRequest{Text: "hello", Voice: "v"}); err != nil {
		t.Fatalf("generate after the window: %v", err)
	}
	if gw.Hits() != 1 {
		t.Fatalf("gateway saw %d request(s), want 1 (the transparent one)", gw.Hits())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(fired) != 1 || fired[0] != 1950 {
		t.Fatalf("fault fired at %v, want once at 1950 ms (the refused request)", fired)
	}
}

// captureOnlyPub is a session.Publisher that keeps events in memory; the
// fence tests care about the reducer's verdicts, not the wire.
type captureOnlyPub struct {
	mu  sync.Mutex
	evs []events.Envelope
}

func (p *captureOnlyPub) Publish(_ context.Context, e events.Envelope) (events.Envelope, error) {
	p.mu.Lock()
	p.evs = append(p.evs, e)
	p.mu.Unlock()
	return e, nil
}
