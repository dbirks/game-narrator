package capture

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- LatestSlot -----------------------------------------------------------

func TestLatestSlotKeepsNewestAndCountsDrops(t *testing.T) {
	// The producer/consumer race in this test is deliberate: 10 000 frames are
	// offered as fast as the loop runs while a consumer takes one every
	// 200 µs, so the slot is overwritten constantly. What must hold, per
	// docs/spec/01's "single-slot latest-frame queue", is that the consumer
	// never sees a frame older than the newest one it saw before, never blocks
	// the producer, and that the drops are *counted* — an uncounted overwrite
	// is the silent frame loss docs/spec/10's dropped-frame-rate check exists
	// to catch.
	const total = 10_000
	s := NewLatestSlot()
	produced := make(chan struct{})
	go func() {
		defer close(produced)
		for i := 1; i <= total; i++ {
			ok := s.Put(Frame{Seq: int64(i), SourceClock: ClockMonotonic, CaptureTimestampNS: int64(i) * 1000})
			if !ok {
				t.Errorf("Put rejected frame %d while the consumer was alive", i)
				return
			}
		}
	}()

	start := time.Now()
	var last int64
	got := 0
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	for {
		f, ok := s.Take(ctx)
		if !ok {
			break
		}
		got++
		if f.Seq <= last && last != 0 {
			t.Fatalf("consumer went backwards: saw %d after %d", f.Seq, last)
		}
		last = f.Seq
		if f.Seq < total {
			select {
			case <-produced:
			case <-time.After(200 * time.Microsecond):
			}
		}
		if f.Seq == total {
			break
		}
	}
	if !waitTimeout(produced, 2*time.Second) {
		t.Fatal("producer never finished: Put blocked")
	}
	if last != total {
		t.Errorf("newest frame delivered = %d, want %d (the consumer must see the latest)", last, total)
	}
	if got >= total/2 {
		t.Errorf("consumer took %d of %d frames with a 200us pace — slot is not coalescing", got, total)
	}
	if drops := s.Drops(); drops != s.Puts()-s.Takes() {
		t.Errorf("drops %d != puts %d - takes %d: conservation broken", drops, s.Puts(), s.Takes())
	}
	el := time.Since(start)
	if el > 5*time.Second {
		t.Errorf("whole exchange took %v; a slow consumer must not stall the producer", el)
	}
	t.Logf("latest-slot: %d puts, %d takes, %d overwritten in %v", s.Puts(), s.Takes(), s.Drops(), el.Round(time.Millisecond))
}

func TestLatestSlotTakeAfterCloseDrainsThenEnds(t *testing.T) {
	s := NewLatestSlot()
	s.Put(Frame{Seq: 7})
	s.Close()
	// A frame published before Close must still be delivered: losing the last
	// frame of a scene is how a dialogue line silently never gets spoken.
	f, ok := s.Take(context.Background())
	if !ok || f.Seq != 7 {
		t.Fatalf("Take after Close = %+v,%v; want seq 7 delivered", f, ok)
	}
	if _, ok := s.Take(context.Background()); ok {
		t.Fatal("Take on a drained closed slot reported a frame")
	}
	// Close is idempotent (docs/spec/10's restart path calls it twice).
	s.Close()
	s.Close()
	if s.Put(Frame{Seq: 8}) {
		t.Error("Put into a closed slot claimed to succeed")
	}
}

func TestLatestSlotTakeHonoursContext(t *testing.T) {
	s := NewLatestSlot()
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, ok := s.Take(ctx); ok {
		t.Fatal("Take returned a frame that was never put")
	}
	if d := time.Since(start); d < 20*time.Millisecond {
		t.Errorf("Take returned in %v, before its deadline: it is spinning, not waiting", d)
	}

	// Cancelled-while-waiting must also return, not deadlock the capture loop.
	ctx2, cancel2 := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel2() }()
	if _, ok := s.Take(ctx2); ok {
		t.Fatal("Take returned a frame after cancellation")
	}
}

func TestLatestSlotConcurrentConsumersAndProducer(t *testing.T) {
	// Race-detector coverage for the mutex/notify protocol, with the
	// conservation identity as the correctness claim.
	s := NewLatestSlot()
	const total = 2000
	var wg sync.WaitGroup
	for c := 0; c < 3; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			for {
				if _, ok := s.Take(ctx); !ok {
					return
				}
			}
		}()
	}
	for i := 1; i <= total; i++ {
		s.Put(Frame{Seq: int64(i)})
	}
	time.Sleep(50 * time.Millisecond)
	s.Close()
	wg.Wait()
	if got := s.Puts(); got != total {
		t.Errorf("puts = %d, want %d", got, total)
	}
	if s.Takes()+s.Drops() < s.Puts() {
		t.Errorf("takes %d + drops %d < puts %d: frames vanished unaccounted",
			s.Takes(), s.Drops(), s.Puts())
	}
}

// --- Lossless -------------------------------------------------------------

func TestLosslessBlocksProducer(t *testing.T) {
	// The recording queue's whole purpose is that it *does* block: a fixture
	// recorder that dropped frames would be documenting a different recording
	// than the one it captured (docs/spec/01).
	l := NewLossless(2)
	ctx := context.Background()
	if !l.Put(ctx, Frame{Seq: 1}) || !l.Put(ctx, Frame{Seq: 2}) {
		t.Fatal("put into a queue with room failed")
	}
	blocked := make(chan bool, 1)
	go func() { blocked <- l.Put(ctx, Frame{Seq: 3}) }()
	select {
	case ok := <-blocked:
		t.Fatalf("third Put returned %v immediately; a full lossless queue must block", ok)
	case <-time.After(60 * time.Millisecond):
	}
	f, ok := l.Take(ctx)
	if !ok || f.Seq != 1 {
		t.Fatalf("Take = %+v,%v; want FIFO frame 1", f, ok)
	}
	select {
	case ok := <-blocked:
		if !ok {
			t.Fatal("unblocked Put reported failure")
		}
	case <-time.After(time.Second):
		t.Fatal("freeing a slot did not unblock the producer")
	}
	if l.Len() != 2 {
		t.Errorf("Len = %d, want 2", l.Len())
	}
	l.Close()
	if l.Put(context.Background(), Frame{Seq: 9}) {
		t.Error("Put into a closed queue claimed success")
	}
}

func TestLosslessDrainsBeforeReportingEnd(t *testing.T) {
	l := NewLossless(4)
	for i := 1; i <= 4; i++ {
		l.Put(context.Background(), Frame{Seq: int64(i)})
	}
	l.Close()
	for i := 1; i <= 4; i++ {
		f, ok := l.Take(context.Background())
		if !ok || f.Seq != int64(i) {
			t.Fatalf("Take %d = %+v,%v; closed lossless queues must drain in order", i, f, ok)
		}
	}
	if _, ok := l.Take(context.Background()); ok {
		t.Fatal("Take on a drained closed queue reported a frame")
	}
	l.Close() // idempotent
}

func TestLosslessPutCancelReturnsFalse(t *testing.T) {
	l := NewLossless(1)
	l.Put(context.Background(), Frame{Seq: 1})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if l.Put(ctx, Frame{Seq: 2}) {
		t.Fatal("Put with an expired context claimed success")
	}
}

// --- hashing --------------------------------------------------------------

func solid(c color.RGBA, w, h int) *image.RGBA {
	i := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i.Set(x, y, c)
		}
	}
	return i
}

func TestLatestSlotReceiveBridgesToChannel(t *testing.T) {
	// Receive exists for consumers that select on frames rather than block in
	// Take (the dev inspector, an SSE fanout). It must close on Close and honour
	// ctx, or a browser tab that goes away would strand a goroutine on the
	// capture loop's mailbox forever.
	s := NewLatestSlot()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := s.Receive(ctx)
	for i := 1; i <= 3; i++ {
		s.Put(Frame{Seq: int64(i), SourceClock: ClockMonotonic, CaptureTimestampNS: int64(i) * 1000})
	}
	s.Close()
	var got []int64
	for f := range ch {
		got = append(got, f.Seq)
	}
	if len(got) == 0 || got[len(got)-1] != 3 {
		t.Errorf("Receive delivered %v; the newest frame must survive the close", got)
	}

	// A consumer whose context ends must not keep the goroutine alive.
	s2 := NewLatestSlot()
	defer s2.Close()
	ctx2, cancel2 := context.WithCancel(context.Background())
	ch2 := s2.Receive(ctx2)
	s2.Put(Frame{Seq: 1})
	<-ch2
	cancel2()
	select {
	case _, ok := <-ch2:
		if ok {
			t.Log("one in-flight frame delivered after cancellation (legal)")
		}
	case <-time.After(time.Second):
		t.Fatal("Receive did not close its channel on cancellation")
	}
}

func TestHashFrameDetectsChangeAndFreeze(t *testing.T) {
	a := solid(color.RGBA{10, 20, 30, 255}, 160, 120)
	b := solid(color.RGBA{10, 20, 30, 255}, 160, 120)
	if HashFrame(a) != HashFrame(b) {
		t.Error("two identical pictures hashed differently: a freeze would look like motion")
	}
	// A textbox appearing: 40x12 block of light pixels inside a dark frame.
	for y := 60; y < 72; y++ {
		for x := 40; x < 80; x++ {
			a.Set(x, y, color.RGBA{240, 240, 240, 255})
		}
	}
	if HashFrame(a) == HashFrame(b) {
		t.Error("a 40x12 bright block did not change the hash: a scene change would look like a freeze")
	}
	// A different size hashes differently even with identical sampled pixels —
	// the framebuffer-size-change case from docs/spec/12.
	c := solid(color.RGBA{10, 20, 30, 255}, 168, 120)
	if HashFrame(c) == HashFrame(b) {
		t.Error("a size change did not change the hash")
	}
	if HashFrame(nil) != "" {
		t.Error("nil image hashed to something")
	}
}

func TestHashFrameStrideDocumented(t *testing.T) {
	// The accuracy/cost claim in HashFrame's doc comment, checked rather than
	// asserted in prose: the 1/8 sample is what makes the hash cheap, and it is
	// the reason a 1-pixel change *can* be missed. If this test ever starts
	// failing because the stride changed, the doc comment and every expectation
	// built on it need re-checking.
	if HashStride != 8 {
		t.Fatalf("HashStride = %d; the downsample ratio is documented as 1/8", HashStride)
	}
	full := solid(color.RGBA{5, 5, 5, 255}, 512, 384)
	one := solid(color.RGBA{5, 5, 5, 255}, 512, 384)
	one.Set(1, 1, color.RGBA{250, 250, 250, 255}) // between sample points
	if HashFrame(full) != HashFrame(one) {
		t.Error("a single off-grid pixel changed the frame hash; stride is no longer 1/8 sampling")
	}
	if DownsampleHash(full, 1) == DownsampleHash(one, 1) {
		t.Error("stride 1 (full sample) missed a changed pixel: the hash itself is broken")
	}
}

func TestDownsampleHashCost(t *testing.T) {
	// The reason for the downsample, measured: a 1080p rgb24 frame is ~6 MB and
	// the hash is computed on every delivered frame. If full-frame hashing ever
	// comes within a few times of the sampled cost, the tradeoff note in
	// HashFrame's comment is wrong and should be revisited.
	img := solid(color.RGBA{9, 9, 9, 255}, 1920, 1080)
	sampled := testing.Benchmark(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = HashFrame(img)
		}
	})
	full := testing.Benchmark(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = DownsampleHash(img, 1)
		}
	})
	ratio := float64(full.NsPerOp()) / float64(sampled.NsPerOp())
	t.Logf("1080p frame hash: sampled %d ns/op, full %d ns/op (%.1fx)", sampled.NsPerOp(), full.NsPerOp(), ratio)
	if ratio < 10 {
		t.Errorf("downsampling only saved %.1fx; the cheap hash is not paying for itself", ratio)
	}
}

// --- Frame / PixFmt -------------------------------------------------------

func TestFrameValidateMirrorsRecognitionContract(t *testing.T) {
	good := Frame{Seq: 1, SourceClock: ClockMonotonic, CaptureTimestampNS: 1, Width: 4, Height: 4, Image: solid(color.RGBA{0, 0, 0, 255}, 4, 4)}
	if err := good.Validate(); err != nil {
		t.Fatalf("valid frame rejected: %v", err)
	}
	bad := []struct {
		name  string
		f     Frame
		wants string
	}{
		{"no pixels", Frame{SourceClock: ClockMonotonic, CaptureTimestampNS: 1, Width: 2, Height: 2}, "no pixels"},
		{"no clock", Frame{Image: good.Image, CaptureTimestampNS: 1, Width: 2, Height: 2}, "source_clock"},
		{"zero ts", Frame{Image: good.Image, SourceClock: ClockMonotonic, Width: 2, Height: 2}, "capture_timestamp_ns"},
		{"negative ts", Frame{Image: good.Image, SourceClock: ClockMonotonic, CaptureTimestampNS: -5, Width: 2, Height: 2}, "capture_timestamp_ns"},
		{"zero size", Frame{Image: good.Image, SourceClock: ClockMonotonic, CaptureTimestampNS: 1}, "size"},
		{"negative seq", Frame{Image: good.Image, SourceClock: ClockMonotonic, CaptureTimestampNS: 1, Width: 2, Height: 2, Seq: -1}, "negative"},
	}
	for _, tc := range bad {
		err := tc.f.Validate()
		if err == nil {
			t.Errorf("%s: accepted", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.wants) {
			t.Errorf("%s: error %q does not mention %q", tc.name, err, tc.wants)
		}
	}
}

func TestFrameBounds(t *testing.T) {
	img := solid(color.RGBA{0, 0, 0, 255}, 8, 6)
	if w, h := (Frame{Width: 640, Height: 480, Image: img}).Bounds(); w != 640 || h != 480 {
		t.Errorf("explicit geometry ignored: %dx%d", w, h)
	}
	if w, h := (Frame{Image: img}).Bounds(); w != 8 || h != 6 {
		t.Errorf("fallback to image bounds gave %dx%d, want 8x6", w, h)
	}
	if w, h := (Frame{}).Bounds(); w != 0 || h != 0 {
		t.Errorf("empty frame bounds = %dx%d, want 0x0 rather than a panic", w, h)
	}
}

func TestPixFmtOf(t *testing.T) {
	if got := PixFmtOf(image.NewRGBA(image.Rect(0, 0, 2, 2))); got != PixRGBA {
		t.Errorf("RGBA = %q, want rgba", got)
	}
	if got := PixFmtOf(image.NewNRGBA(image.Rect(0, 0, 2, 2))); !strings.Contains(string(got), "NRGBA") {
		t.Errorf("NRGBA = %q; an unknown representation must name itself, not lie", got)
	}
	if got := PixFmtOf(nil); got != PixUnknown {
		t.Errorf("nil = %q, want empty", got)
	}
	if got := PixFmtOf(image.NewYCbCr(image.Rect(0, 0, 4, 4), image.YCbCrSubsampleRatio444)); got != PixYCbCr {
		t.Errorf("YCbCr = %q, want yuv420p", got)
	}
}

func TestMonotonicClockIsPositiveAndOrdered(t *testing.T) {
	a := MonotonicNS()
	time.Sleep(2 * time.Millisecond)
	b := MonotonicNS()
	if a <= 0 {
		t.Fatalf("MonotonicNS = %d; recognition rejects a non-positive timestamp", a)
	}
	if b <= a {
		t.Errorf("clock went backwards or stood still: %d then %d", a, b)
	}
	if d := time.Duration(b - a); d < time.Millisecond || d > 2*time.Second {
		t.Errorf("2 ms of sleep measured %v on the capture clock", d)
	}
	// The zero point is process start, so the value must be at least as large as
	// the process's own age — a claim a reader can check against the logs.
	if d := time.Duration(MonotonicNS()); d < 100*time.Microsecond {
		t.Errorf("clock reads %v since process start in a test that has been running longer", d)
	}
}

func TestStatsAlive(t *testing.T) {
	if (Stats{State: StateRunning}).Alive() == false {
		t.Error("running source reported not alive")
	}
	for _, st := range []State{StateStarting, StateReconnecting, StateStopped, StateFailed} {
		if (Stats{State: st}).Alive() {
			t.Errorf("%s reported alive: detection must pause unless frames are arriving", st)
		}
	}
	if !bytes.Contains([]byte("x"), []byte("x")) {
		t.Skip("unused import guard")
	}
}

func waitTimeout(ch <-chan struct{}, d time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(d):
		return false
	}
}
