package dolphin

// record_capture_test.go is the unit half of the parallel adaptive capture
// (schema 2, ADR-0054 amendment 2): the capture loop's bookkeeping — its
// own clock (independent of the drive's Screenshot cadence), the min-gap
// between the two streams, the duration/byte budget stops, and the
// manifest's capture block — against the fake bridge. No emulator, no
// Tesseract (the real end-to-end proof is TestRealRecordLegendDense).

import (
	"context"
	"encoding/json"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestCapturePlanParallelStream verifies the loop records frames on its own
// clock (the drive's Screenshot is called exactly once, far sparser than
// the loop's rate), stops at the duration budget, and stamps the manifest's
// capture block (schema 2, the rate the manifest claims).
func TestCapturePlanParallelStream(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	fake := &recFakeBridge{
		imgs: []image.Image{flatFrame(color.Black), flatFrame(color.RGBA{R: 90, G: 90, B: 90, A: 255}), flatFrame(color.RGBA{R: 200, G: 40, B: 40, A: 255})},
	}
	rec, err := NewFixtureRecorder(fake, root, "capture-fixture")
	if err != nil {
		t.Fatalf("NewFixtureRecorder: %v", err)
	}
	// The nominal rates are 100x the capture min-gap (100 ms), so the
	// effective stream is gap-limited to ~10 frames/s: ~600 ms admits ~4-7
	// loop frames — enough to prove the loop runs on its own clock, few
	// enough to prove the budget cut the window.
	rec.SetCapturePlan(CapturePlan{HighHz: 100, LowHz: 50, MaxDuration: 600 * time.Millisecond})
	if err := rec.Launch(ctx, LaunchSpec{}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := rec.StartCapture(ctx); err != nil {
		t.Fatalf("StartCapture: %v", err)
	}
	// The drive's own screenshot, once, well below the loop's rate: the
	// frame sequence interleaves both streams in capture order.
	if _, err := rec.Screenshot(ctx); err != nil {
		t.Fatalf("Screenshot (drive path): %v", err)
	}
	time.Sleep(1200 * time.Millisecond) // well past the 600 ms budget

	summary, err := rec.Finalize(ctx, DriveTrace{})
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if summary.Capture == nil || !summary.Capture.Enabled || summary.Capture.HighHz != 100 || summary.Capture.LowHz != 50 {
		t.Fatalf("manifest capture block = %+v, want enabled with the plan's rates", summary.Capture)
	}
	if summary.Capture.MaxDurationSecs <= 0 {
		t.Errorf("manifest capture block carries no max-duration budget: %+v", summary.Capture)
	}
	// The loop must have run: the drive's single screenshot plus loop frames
	// (gap-limited to ~1/s*10 over 600 ms).
	if summary.Frames < 3 {
		t.Fatalf("frames = %d, want the loop to have recorded (>= 3 incl. the drive's screenshot)", summary.Frames)
	}
	// ... and the budget must have cut the window: the frame timestamps are
	// the record (checked below against the manifest).

	// The manifest on disk: schema 2 with the capture block, verified.
	raw, err := os.ReadFile(filepath.Join(root, "capture-fixture", "manifest.json"))
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	var man FixtureManifest
	if err := json.Unmarshal(raw, &man); err != nil {
		t.Fatalf("manifest parse: %v", err)
	}
	if man.Schema != 2 || man.Capture == nil || !man.Capture.Enabled {
		t.Fatalf("manifest schema/capture = %d %+v, want schema 2 enabled", man.Schema, man.Capture)
	}
	if man.Capture.DurationSecs <= 0 || man.Capture.Bytes == 0 {
		t.Errorf("manifest capture block = %+v, want the measured window and bytes", man.Capture)
	}
	// The budget cut the window: the frames span ~600 ms (the budget), not
	// the 1.2 s the test slept. In-flight frames complete after the stop
	// (their numbers were reserved), so the span is a little longer than
	// the budget — and far shorter than the sleep.
	span := float64(man.Frames[len(man.Frames)-1].TsNS-man.Frames[0].TsNS) / 1e9
	if span < 0.4 || span > 1.0 {
		t.Errorf("frame span = %.3f s, want ~600 ms (the budget cut the loop; the test slept 1.2 s)", span)
	}
	if _, err := VerifyFixture(filepath.Join(root, "capture-fixture")); err != nil {
		t.Fatalf("VerifyFixture: %v", err)
	}
}

// TestCapturePlanByteBudget verifies the byte budget stops the loop (the
// size bound the task asks for): the fixture ends with the budget's worth
// of frames, never much more.
func TestCapturePlanByteBudget(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	fake := &recFakeBridge{
		imgs: []image.Image{flatFrame(color.Black), flatFrame(color.RGBA{R: 90, G: 90, B: 90, A: 255}), flatFrame(color.RGBA{R: 200, G: 40, B: 40, A: 255})},
	}
	rec, err := NewFixtureRecorder(fake, root, "byte-budget-fixture")
	if err != nil {
		t.Fatalf("NewFixtureRecorder: %v", err)
	}
	// Flat-colour frames encode to ~1-2 KB each; a 3 KB budget admits a
	// couple and then the loop must stop, whatever the rate says (over
	// 1.5 s an unbounded loop would admit ~15 gap-limited frames).
	const budget = 3 * 1024
	rec.SetCapturePlan(CapturePlan{HighHz: 50, LowHz: 50, MaxBytes: budget})
	if err := rec.Launch(ctx, LaunchSpec{}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := rec.StartCapture(ctx); err != nil {
		t.Fatalf("StartCapture: %v", err)
	}
	time.Sleep(1500 * time.Millisecond)
	summary, err := rec.Finalize(ctx, DriveTrace{})
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if summary.Capture.MaxBytes != budget {
		t.Errorf("manifest MaxBytes = %d, want the plan's budget %d", summary.Capture.MaxBytes, budget)
	}
	if summary.Frames == 0 {
		t.Fatal("the byte budget stopped the loop before any frame: the budget is a ceiling, not a veto")
	}
	raw, err := os.ReadFile(filepath.Join(root, "byte-budget-fixture", "manifest.json"))
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	var man FixtureManifest
	if err := json.Unmarshal(raw, &man); err != nil {
		t.Fatalf("manifest parse: %v", err)
	}
	if man.Capture.Bytes > int64(budget)+256*1024 {
		t.Errorf("recorded bytes = %d, want within the in-flight bound of the %d budget", man.Capture.Bytes, budget)
	}
	// The budget is the assertion: it stopped the loop long before the 1.5 s
	// sleep ended — the frames' timestamps say where (a fraction of a second
	// span, not 1.5 s).
	if len(man.Frames) > 1 {
		span := float64(man.Frames[len(man.Frames)-1].TsNS-man.Frames[0].TsNS) / 1e9
		if span > 0.8 {
			t.Errorf("frame span = %.3f s, want << 0.8 s (the 3 KB byte budget must have stopped the loop early)", span)
		}
	}
}

// TestCaptureLegacyUnchanged verifies the legacy path: no plan armed, no
// parallel stream — the fixture is exactly the drive's screenshots (the
// existing windwaker-newgame shape), and the manifest's capture block stays
// empty on disk (omitted) so old scenarios' pins are untouched.
func TestCaptureLegacyUnchanged(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	fake := &recFakeBridge{
		imgs: []image.Image{flatFrame(color.Black), flatFrame(color.RGBA{R: 90, G: 90, B: 90, A: 255}), flatFrame(color.RGBA{R: 200, G: 40, B: 40, A: 255})},
	}
	rec, err := NewFixtureRecorder(fake, root, "legacy-fixture")
	if err != nil {
		t.Fatalf("NewFixtureRecorder: %v", err)
	}
	if err := rec.Launch(ctx, LaunchSpec{}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	// StartCapture without a plan is refused (no silent default-on).
	if err := rec.StartCapture(ctx); err == nil {
		t.Fatal("StartCapture passed without a plan: the zero plan is the legacy recording, not a default-on capture")
	}
	for i := 0; i < 3; i++ {
		if _, err := rec.Screenshot(ctx); err != nil {
			t.Fatalf("Screenshot %d: %v", i, err)
		}
	}
	summary, err := rec.Finalize(ctx, DriveTrace{})
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if summary.Frames != 3 {
		t.Fatalf("frames = %d, want exactly the drive's 3 screenshots (no parallel stream)", summary.Frames)
	}
	if summary.Capture != nil {
		t.Errorf("legacy recording claims a capture: %+v", summary.Capture)
	}
	raw, err := os.ReadFile(filepath.Join(root, "legacy-fixture", "manifest.json"))
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if got := string(raw); !strings.Contains(got, `"schema": 2`) {
		t.Errorf("manifest does not carry schema 2")
	}
	if got := string(raw); strings.Contains(got, `"capture"`) {
		t.Errorf("legacy manifest carries a capture block (omitted expected): %s", got)
	}
}

// testVirtualClock is the test's virtual clock for TestCaptureMinGap:
// now only moves when the test advances it, and the test and the capture
// loop interleave one virtual step at a time by handshake — advance blocks
// until the loop has run its iteration for that step (watermark), and the
// loop's tick blocks until the clock passes its watermark. No sleep, and no
// reliance on the scheduler: a cooperative scheduler runs a non-blocking
// test to completion before the loop's first iteration, so a one-sided
// "advance and hope the loop catches up" design lets the test spend the
// whole budget (virtual time) before the loop's first gate — the handshake
// makes that impossible.
type testVirtualClock struct {
	mu        sync.Mutex
	t         time.Time
	watermark time.Time     // the virtual instant the last completed tick saw (zero = none yet)
	w         chan struct{} // the tick's waiter
	ack       chan struct{} // advance's waiter (closed when the tick passes the watermark)
}

func newTestVirtualClock(start time.Time) *testVirtualClock {
	return &testVirtualClock{t: start, w: make(chan struct{}), ack: make(chan struct{})}
}

func (c *testVirtualClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// advance moves the clock and blocks until the capture loop has run its
// iteration for the new instant (its watermark reaches it). The test can
// never run ahead of the loop's own gate.
func (c *testVirtualClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	target := c.t
	old := c.w
	c.w = make(chan struct{})
	c.mu.Unlock()
	close(old) // wake the tick (a tick not yet waiting just sees the new instant on its next check)
	for {
		c.mu.Lock()
		if !c.watermark.Before(target) {
			c.mu.Unlock()
			return
		}
		ack := c.ack
		c.mu.Unlock()
		<-ack
	}
}

// tick blocks until the clock has passed this tick's watermark (or ctx
// finishes), then records the new instant as the watermark. d is the
// production pacing contract (wallClock honours it); the virtual clock
// honours it in the load-independent sense — the loop iterates on every
// advance and the loop's own gate (budget + min-gap) decides whether a
// frame may be grabbed, which is exactly the bookkeeping the test asserts.
func (c *testVirtualClock) tick(ctx context.Context, d time.Duration) bool {
	for {
		c.mu.Lock()
		w := c.w
		// Strictly ahead: the clock must be past the watermark (wm < t); at
		// t == watermark there is no new step, so the loop never spins in
		// place at one instant.
		ready := c.watermark.Before(c.t)
		c.mu.Unlock()
		if ready {
			// Pass the step: the loop body now reads the new instant under
			// its own lock; the waiting advance sees the watermark and
			// returns.
			c.mu.Lock()
			c.watermark = c.t
			ack := c.ack
			c.ack = make(chan struct{})
			c.mu.Unlock()
			close(ack)
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-w:
		}
	}
}

// gapFlatFrame is one cheap flat 64×48 frame for TestCaptureMinGap: the
// min-gap assertion is about the timestamps the capture loop stamps, not
// the pixels, and a small frame keeps the workers' encode/write cost a
// fraction of a millisecond — the test's bounded wait never depends on how
// much CPU the host gives the workers under -race load.
func gapFlatFrame(c color.Color) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, 64, 48))
	for y := 0; y < 48; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, c)
		}
	}
	return img
}

// TestCaptureMinGap verifies the min-gap enforcement: at a 100 ms minimum
// spacing the loop cannot out-record the gap, whatever its (theoretical)
// rate — every pair of consecutive recorded frames is stamped at least
// captureMinGap apart, and ≥ 2 frames were recorded.
//
// The assertion rides the capture plan's OWN clock (testVirtualClock), not
// wall time: the test waits for the recorded frames by EVENT, not by sleep
// — it advances the loop's virtual clock one step at a time (each advance
// pairs with exactly one capture-loop tick, the watermark handshake) and
// polls the recorder's published frame count after each step, in a bounded
// loop. It is the loop's OWN gate (budget + min-gap) that decides whether a
// frame is grabbed at each instant; the test only observes.
//
// The plan's duration budget (10 min) far exceeds the virtual window the
// test advances (≤ 10 s), so the loop has NOT self-stopped on budget when
// the test calls StopCapture: StopCapture then takes the cancel-and-join
// path (cancel -> tick false -> loop returns -> close(jobs) + wg.Wait ->
// close(done)), which guarantees every reserved frame is encoded, written
// and published before Finalize reads the frame list. Running the loop
// PAST its budget and only then stopping would race the join: the budget
// stop clears the loop's on-flag itself, and StopCapture's early return
// would let Finalize read the frame list while the workers are still
// draining the reserved frames — the failure mode of the old wall-window
// test (acceptance gap 12). A frame count asserted inside a wall-clock
// sleep window is an assertion about the scheduler, which full-suite
// -race load can starve inside that window.
func TestCaptureMinGap(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	fake := &recFakeBridge{
		imgs: []image.Image{gapFlatFrame(color.Black)},
	}
	rec, err := NewFixtureRecorder(fake, root, "mingap-fixture")
	if err != nil {
		t.Fatalf("NewFixtureRecorder: %v", err)
	}
	clk := newTestVirtualClock(time.Unix(0, 0))
	rec.SetCaptureClock(clk)
	// 1000 Hz nominal (1 ms loop interval), 100 ms min-gap: the loop may
	// attempt a grab every 1 ms of its own clock, but the gap admits one
	// frame per 100 ms — the gap, not the rate, is the bound. The duration
	// budget is 10 min: it cannot close inside the test's ≤ 10 s virtual
	// window, so the loop is still running (and joinable) at StopCapture.
	plan := CapturePlan{HighHz: 1000, LowHz: 1000, MaxDuration: 10 * time.Minute}
	rec.SetCapturePlan(plan)
	if err := rec.Launch(ctx, LaunchSpec{}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := rec.StartCapture(ctx); err != nil {
		t.Fatalf("StartCapture: %v", err)
	}

	// Wait for the recorded frames by event, on the loop's own clock — no
	// wall-clock sleep, no wall-clock window: advance one step (one loop
	// tick), then check the recorder's published frame count, until ≥ 2
	// frames are recorded (a healthy host publishes the first two after
	// ~200 ms of virtual time, ~20 steps) or the bound is spent.
	const step = 10 * time.Millisecond
	const maxSteps = 1000 // 10 s of virtual time, still << the 10 min budget
	frameCount := func() int {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		return len(rec.frames)
	}
	steps := 0
	for steps < maxSteps && frameCount() < 2 {
		clk.advance(step)
		steps++
	}
	// StopCapture now: the loop is still on its own clock (the budget cannot
	// have closed), so this cancels and JOINS the loop and its workers —
	// every frame reserved by that point is encoded, written and published
	// before this returns, however loaded the host is.
	rec.StopCapture()
	summary, err := rec.Finalize(ctx, DriveTrace{})
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if summary.Frames < 2 {
		t.Fatalf("frames = %d after %d steps of the loop's own clock with the loop joined: the capture loop recorded nothing publishable — need 2 to assert the gap invariant", summary.Frames, steps)
	}
	// The gap invariant itself: every two consecutive recorded frames are
	// stamped at least captureMinGap apart (the manifest's timestamps are
	// the record), whatever the host schedule admitted in the window.
	raw, err := os.ReadFile(filepath.Join(root, "mingap-fixture", "manifest.json"))
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	var man FixtureManifest
	if err := json.Unmarshal(raw, &man); err != nil {
		t.Fatalf("manifest parse: %v", err)
	}
	if len(man.Frames) < 2 {
		t.Fatalf("manifest carries %d frames, want >= 2 to assert the gap invariant", len(man.Frames))
	}
	for i := 1; i < len(man.Frames); i++ {
		gap := man.Frames[i].TsNS - man.Frames[i-1].TsNS
		if gap < int64(captureMinGap) {
			t.Fatalf("frames %d and %d are %v apart: the %v min-gap is not enforced (consecutive stamps %d ns vs %d ns)", i-1, i, gap, captureMinGap, man.Frames[i-1].TsNS, man.Frames[i].TsNS)
		}
	}
	// And the gap, not the rate, is the bound: at 1000 Hz the rate alone
	// would admit ~10 frames per 100 ms of the loop's own clock; the
	// recorded count over the stamped span must be the gap-limited one
	// (span / min-gap + 1 — implied by the pairwise gaps above, stated
	// against the span so a regression that stamps closer frames cannot
	// hide behind a small frame count).
	span := man.Frames[len(man.Frames)-1].TsNS - man.Frames[0].TsNS
	maxByGap := 1 + int(span/int64(captureMinGap))
	if len(man.Frames) > maxByGap {
		t.Errorf("frames = %d over a %d ns span at a %v min-gap: the gap is not the bound (max gap-limited = %d)", len(man.Frames), span, captureMinGap, maxByGap)
	}
}
