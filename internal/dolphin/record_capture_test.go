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
	"fmt"
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
	// gapFlatFrame (64×48, ~130 B PNG) keeps the loop's worker drain —
	// which StopCapture joins in REAL time — a fraction of a millisecond of
	// CPU per frame, so the test's Finalize never depends on how much CPU a
	// loaded host gives the workers (see the gapFlatFrame comment, the
	// measured 2026-10-19 saturated-host timeout).
	fake := &recFakeBridge{
		imgs: []image.Image{gapFlatFrame(color.Black), gapFlatFrame(color.RGBA{R: 90, G: 90, B: 90, A: 255}), gapFlatFrame(color.RGBA{R: 200, G: 40, B: 40, A: 255})},
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
//
// The assertion rides the capture plan's OWN clock (testVirtualClock, the
// same pattern as TestCaptureMinGap): the test advances the loop's virtual
// clock in 10 ms steps — one step pairs with exactly one loop tick (the
// watermark handshake) — and waits for the loop's OWN stop on the budget
// (the gate clears the on-flag) by EVENT, not by a wall-clock sleep. Every
// assertion is then load-independent: the stop is the gate's own (not
// StopCapture's), the manifest's bytes are >= the budget (the gate closes
// only at >= budget) and within its in-flight bound, and the manifest
// count equals the files on disk and the reserved count (the drain join).
// The old version slept 1.5 s of wall time and asserted the stamped span
// stayed under 0.8 s — an assertion about the scheduler, which fails under
// taskset/-race load with or without the join change (measured at clean
// HEAD: 5/5 runs, span 1.3-1.5 s on one core, 2026-10-19). A span bound in
// virtual time is not assertable either: the virtual clock runs far ahead
// of the workers' real time, so the published-bytes lag is measured in
// worker latency (the loop backpressures at the in-flight bound and the
// window self-paces); the span assertion kept is that it stays inside the
// window the loop ran in.
func TestCapturePlanByteBudget(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	fake := &recFakeBridge{
		imgs: []image.Image{gapFlatFrame(color.Black), gapFlatFrame(color.RGBA{R: 90, G: 90, B: 90, A: 255}), gapFlatFrame(color.RGBA{R: 200, G: 40, B: 40, A: 255})},
	}
	rec, err := NewFixtureRecorder(fake, root, "byte-budget-fixture")
	if err != nil {
		t.Fatalf("NewFixtureRecorder: %v", err)
	}
	clk := newTestVirtualClock(time.Unix(0, 0))
	rec.SetCaptureClock(clk)
	// 64×48 flat frames (measured: ~131 B PNG each): a 200 B budget closes
	// the gate two frames in (262 B published — the gate keys on PUBLISHED
	// bytes, the workers' real-time work), then the loop must stop, whatever
	// the rate says (an unbounded loop would admit a gap-limited frame every
	// 100 ms of the loop's own clock). The small size is deliberate, not a
	// shortcut: every reserved frame is encoded, classified and written by
	// the workers during the drain that StopCapture joins, and that drain
	// runs in REAL time whatever the host is doing — at 640×480 each frame
	// cost ~10 ms of CPU under -race, and on a host saturated by a parallel
	// Dolphin E2E suite the 30× pinned gate's 10 m timeout ran out mid-
	// drain (measured 2026-10-19: all four workers runnable in ScreenSigOf
	// while StopCapture waited on done). At 64×48 the whole drain is a
	// fraction of a millisecond of CPU, so the join completes on any host
	// that gives the process anything at all. The budget is 200 B (not KBs)
	// for the same reason in the other direction: the stop event is the
	// workers' publish — REAL time — while the test's advance loop runs in
	// VIRTUAL time, so the budget must be a couple of frames' worth (not a
	// couple of dozen) for the stop to land inside the step fence below
	// whatever the workers' latency is.
	const budget = 200
	rec.SetCapturePlan(CapturePlan{HighHz: 50, LowHz: 50, MaxBytes: budget})
	if err := rec.Launch(ctx, LaunchSpec{}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := rec.StartCapture(ctx); err != nil {
		t.Fatalf("StartCapture: %v", err)
	}
	// Advance the loop's own clock until it stops itself on the byte budget
	// (the gate clears the on-flag): each step pairs with exactly one loop
	// tick, and the gap admits one frame per 100 ms. The stop is the gate's
	// own — but it fires on the workers' PUBLISHED bytes, which is REAL-time
	// work, so the step fence is generous (3000 steps = 30 s of virtual
	// time): each handshake step also gives the workers more real time, and
	// the loop re-checks the gate on every step, so the stop lands as soon
	// as two frames have published (262 B >= the 200 B budget), no matter
	// how loaded the host is. (A tighter fence raced the workers' latency
	// under -race load: measured 2026-10-19, the loop still running after
	// 300 steps.) done is the loop's join channel, threaded through advance
	// so a later advance can never wait for a watermark the loop will never
	// set (the race: the tick reports a step to the test BEFORE the loop's
	// gate runs on it, so the test's on-flag check can still read the flag
	// as running and advance one more step after the loop has already
	// cleared the flag and exited; the done guard makes that advance return
	// instead of hanging the test to the test-binary timeout — the
	// 2026-10-19 intermittent 10 m hang the full-package -race run hit
	// once).
	rec.mu.Lock()
	done := rec.captureDone
	rec.mu.Unlock()
	const step = 10 * time.Millisecond
	const maxSteps = 3000
	stopped := false
	for i := 0; i < maxSteps; i++ {
		clk.advance(step, done)
		rec.mu.Lock()
		stopped = !rec.captureOn
		rec.mu.Unlock()
		if stopped {
			break
		}
	}
	if !stopped {
		t.Fatalf("the loop is still running after %d steps of its own clock: the %d byte budget did not stop it (unbounded capture)", maxSteps, budget)
	}
	// Join the drain AFTER the self-stop: the loop cleared the on-flag
	// itself, so only the done-channel join waits for the workers — every
	// frame whose number was reserved is written before StopCapture
	// returns (the join contract; the self-stop side of ADR-0071).
	rec.StopCapture()
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
	// The join contract by event: the manifest lists every frame whose file
	// was written — its count equals the files on disk and the reserved
	// count (a mid-drain read would leave the manifest shorter than the
	// disk; an over-read would list frames that never completed).
	files, err := filepath.Glob(filepath.Join(root, "byte-budget-fixture", "frames", "frame-*.png"))
	if err != nil {
		t.Fatalf("glob frames: %v", err)
	}
	rec.mu.Lock()
	reserved := rec.nextFrameNo
	rec.mu.Unlock()
	if summary.Frames != len(files) || summary.Frames != reserved {
		t.Fatalf("manifest frames = %d, files on disk = %d, reserved = %d: the byte-budget self-stop lost a frame in the drain", summary.Frames, len(files), reserved)
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
	// The budget is the assertion, by event and load-independent: the stop
	// observed above is the gate's own (the on-flag cleared under the gate's
	// lock, not by StopCapture), and the gate closes only once the PUBLISHED
	// bytes reach the budget — so after the full drain the manifest records
	// at least the budget's worth of bytes (later frames only add), and the
	// overshoot over it is the bounded in-flight workers, not an unbounded
	// capture. (A fixed span bound is not assertable: the virtual clock runs
	// far ahead of the workers' real time, so the published-bytes lag behind
	// the grabs is measured in worker latency — the loop backpressures at
	// the in-flight bound and the window self-paces, which is exactly why
	// the stop lands well inside the window.)
	if man.Capture.Bytes < int64(budget) {
		t.Errorf("recorded bytes = %d, want >= the %d budget: the loop stopped before consuming the budget (the gate closes only at >= budget)", man.Capture.Bytes, budget)
	}
	if man.Capture.Bytes > int64(budget)+256*1024 {
		t.Errorf("recorded bytes = %d, want within the in-flight bound of the %d budget", man.Capture.Bytes, budget)
	}
	// The span is on the loop's own clock and stays inside the window: the
	// frame stamps are the reservation stamps and the last reservation
	// precedes the stop instant (the loop self-stopped within %d steps of
	// %v, observed above).
	if len(man.Frames) > 1 {
		span := man.Frames[len(man.Frames)-1].TsNS - man.Frames[0].TsNS
		if span >= int64(maxSteps*step) {
			t.Errorf("frame span = %v, want < the %v window: the frames' stamps must stay inside the window the loop ran in", span, maxSteps*step)
		}
	}
	if _, err := VerifyFixture(filepath.Join(root, "byte-budget-fixture")); err != nil {
		t.Fatalf("VerifyFixture: %v", err)
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
//
// done is the capture loop's join channel: advance returns when the loop
// self-stops (budget) instead of blocking on a step the loop will never
// consume. The self-stop lands on the step the loop's gate runs — which is
// AFTER the tick that reports the step to the test — so a test that checks
// the on-flag after a step returned and then advances one more can race the
// loop's gate: the loop clears the flag and exits while the test waits for
// a watermark that never comes. With the done guard that advance returns
// (the loop is gone; the test's next on-flag check sees the self-stop),
// without it the test hangs until the test-binary timeout.
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
// iteration for the new instant (its watermark reaches it) — or returns
// immediately when done (the loop's join channel) closes: the loop stopped
// on its own budget and will never consume the step. The test can never run
// ahead of the loop's own gate, and it never outlives the loop.
func (c *testVirtualClock) advance(d time.Duration, done <-chan struct{}) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	target := c.t
	old := c.w
	c.w = make(chan struct{})
	c.mu.Unlock()
	close(old) // wake the tick (a tick not yet waiting just sees the new instant on its next check)
	for {
		select {
		case <-done:
			return // the loop stopped itself: no more steps will ever be consumed
		default:
		}
		c.mu.Lock()
		if !c.watermark.Before(target) {
			c.mu.Unlock()
			return
		}
		ack := c.ack
		c.mu.Unlock()
		select {
		case <-ack:
			// the tick passed the watermark; re-check (the loop may have
			// stopped in the same breath, and the next wait must not block)
		case <-done:
			return
		}
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

// TestVirtualClockAdvanceSurvivesLoopExit is the deterministic regression
// for the handshake half of the capture-join deadlock (2026-10-19):
// advance must return when the loop it is paired with stops itself, not
// block forever on a watermark the dead loop will never set. The fake
// "loop" consumes exactly one tick (the budget step) and then exits — the
// self-stop shape: the tick reports the step to the test (advance returns)
// and only then does the loop's gate clear the on-flag and leave. The test's
// NEXT advance — the one it issues after its on-flag check has not yet seen
// the self-stop — must return via the done guard. On the old handshake (a
// bare <-ack wait) the second advance blocks forever: it is the 10-minute
// test-timeout hang the full-package -race run hit once.
func TestVirtualClockAdvanceSurvivesLoopExit(t *testing.T) {
	clk := newTestVirtualClock(time.Unix(0, 0))
	done := make(chan struct{})
	tickErr := make(chan error, 1)
	go func() {
		// One consumed step (the budget step the tick reports to the
		// test), then the loop's self-stop: it exits without ever
		// consuming another step.
		if !clk.tick(context.Background(), time.Millisecond) {
			tickErr <- fmt.Errorf("tick returned false: the context is not the stop here")
			return
		}
		close(done)
	}()
	clk.advance(time.Millisecond, done) // pairs with the loop's one tick
	select {
	case err := <-tickErr:
		t.Fatal(err)
	case <-done:
		// the loop self-stopped, as scripted
	case <-time.After(5 * time.Second):
		t.Fatal("the fake loop did not self-stop (its one tick did not pair with the first advance)")
	}
	// The racy advance: the on-flag check the test does after a returned
	// step can still read the flag as running (the loop's gate — the thing
	// that clears it — runs after the tick that returned the step), so the
	// test advances one more step for a loop that is already gone.
	start := time.Now()
	clk.advance(time.Millisecond, done)
	if time.Since(start) > time.Second {
		t.Fatalf("advance after the loop's self-stop took %v: it must return on the done guard, not wait for a step the loop will never consume", time.Since(start))
	}
}

// TestCaptureSelfStopDrainJoin verifies the drain join is independent of WHO
// stopped the loop: when the loop stops itself on its duration budget (the
// gate clears the on-flag itself), a later StopCapture must still cancel
// and JOIN the worker drain — not early-return on the cleared flag and let
// Finalize read the frame list while in-flight frames are still being
// encoded and written (a manifest shorter than the files on disk: the
// fixture-loss shape, masked in production only by wall-clock drain time).
//
// The assertion rides the capture plan's OWN clock (testVirtualClock, the
// fake-capture-clock test hook): the test advances the loop past its 400 ms
// budget in 100 ms steps — one step pairs with exactly one loop tick — so
// by the time the last advance returns the loop has self-stopped on budget
// (its watermark reached the budget instant) while its 2-4 reserved frames
// may still be draining through the workers. The frames are gapFlatFrame
// (64×48, ~130 B PNG, a fraction of a millisecond of worker CPU each) so
// the drain that StopCapture joins in REAL time completes on any host that
// gives the process any CPU at all — the join contract below is asserted by
// EVENT (the files on disk), not by sleep, and the small frames just make
// sure a saturated host cannot stretch the drain past the test timeout.
// The assertions are then by EVENT, not by sleep:
//
//  1. StopCapture (called after the self-stop) returns only after the drain
//     finished: every reserved frame's file exists on disk right after it
//     (no skips on the budget path, so the reserved count IS the frame
//     count — no drive screenshots interleave here).
//  2. Finalize sees every frame whose file was written: the manifest's
//     frame count equals the files on disk (a partial read — the pre-fix
//     shape — leaves the manifest shorter than the disk).
func TestCaptureSelfStopDrainJoin(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	fake := &recFakeBridge{
		imgs: []image.Image{gapFlatFrame(color.RGBA{R: 200, G: 40, B: 40, A: 255})},
	}
	rec, err := NewFixtureRecorder(fake, root, "selfstop-fixture")
	if err != nil {
		t.Fatalf("NewFixtureRecorder: %v", err)
	}
	clk := newTestVirtualClock(time.Unix(0, 0))
	rec.SetCaptureClock(clk)
	// 1000 Hz nominal (1 ms loop interval), 100 ms min-gap: the loop may
	// attempt a grab every 1 ms of its own clock, but the gap admits one
	// frame per 100 ms. The duration budget (400 ms) closes exactly on the
	// last step below. The loop's FIRST tick auto-passes at whatever instant
	// the loop goroutine first runs it — t=0 if the scheduler runs it before
	// the test's first advance, t=100 ms if the first advance wakes it first
	// (the handshake is one tick per advance either way) — so the grabs land
	// at t=0/100/200/300 ms or at t=100/200/300 ms, and the last step's
	// instant (t=400 ms, 400 >= 400) self-stops the loop on its own gate:
	// 2-4 frames reserved (gap-limited grabs at 100 ms spacing inside the
	// 400 ms budget, self-stopped at t=400 ms), the stop the loop's OWN
	// (not StopCapture's). The lower bound is 2, not 3: the gap check reads
	// now(), which can sit one virtual step (10 ms) ahead of the tick's own
	// step, so a grab's stamp can read up to 10 ms past the instant the gap
	// is later checked against — the next step's gap can then measure 90 ms
	// < the 100 ms min-gap and that step skips (at most one consecutive
	// skip: the step after a skip always measures ≥ 190 ms). Either way the
	// last step's instant (t=400 ms, 400 >= 400) self-stops the loop on its
	// own gate.
	rec.SetCapturePlan(CapturePlan{HighHz: 1000, LowHz: 1000, MaxDuration: 400 * time.Millisecond})
	if err := rec.Launch(ctx, LaunchSpec{}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := rec.StartCapture(ctx); err != nil {
		t.Fatalf("StartCapture: %v", err)
	}
	// Advance the loop's own clock past the budget in 100 ms steps: each
	// step pairs with exactly one loop tick (the watermark handshake), so
	// the steps land the grabs 100 ms apart (gap-limited) and the last
	// step's instant (t=400 ms) closes the budget: the loop self-stops on
	// its own gate. done is the loop's join channel: the loop exits on the
	// step it consumes, so no advance here can outlive the loop — the guard
	// is threaded through anyway (the byte-budget test is the one that can
	// race the self-stop between its on-flag check and its next advance).
	rec.mu.Lock()
	done := rec.captureDone
	rec.mu.Unlock()
	for i := 0; i < 4; i++ {
		clk.advance(100*time.Millisecond, done)
	}
	// StopCapture AFTER the self-stop: the loop is gone, the on-flag is
	// already cleared — this call must still join the in-flight drain
	// (return only after close(jobs)+wg.Wait+close(done) in the loop's exit).
	rec.StopCapture()
	// The join contract by event: right after StopCapture returns, every
	// reserved frame's file exists on disk. No drive screenshots here and
	// the budget stop skips no frame, so the reserved count IS the frame
	// count the recording ends with.
	rec.mu.Lock()
	reserved := rec.nextFrameNo
	rec.mu.Unlock()
	if reserved < 2 || reserved > 4 {
		t.Fatalf("reserved frames = %d, want 2-4 (gap-limited grabs at 100 ms spacing inside the 400 ms budget, self-stopped at t=400 ms; the virtual now() can run one step ahead of the tick's step, so a stamp's 10 ms of drift can cost one grab — at most one skip)", reserved)
	}
	framesDir := filepath.Join(root, "selfstop-fixture", "frames")
	for i := 0; i < reserved; i++ {
		p := filepath.Join(framesDir, fmt.Sprintf("frame-%05d.png", i+1))
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("StopCapture returned before the drain finished: %s is not on disk (%v) — a budget self-stop must not let StopCapture skip the worker join", p, err)
		}
	}
	// Finalize after the self-stop: the manifest must list every frame whose
	// file was written — its frame count equals the files on disk.
	summary, err := rec.Finalize(ctx, DriveTrace{})
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	files, err := filepath.Glob(filepath.Join(framesDir, "frame-*.png"))
	if err != nil {
		t.Fatalf("glob frames: %v", err)
	}
	if len(files) != reserved {
		t.Fatalf("frame files on disk = %d, want %d (the reserved count)", len(files), reserved)
	}
	if summary.Frames != len(files) {
		t.Fatalf("manifest frames = %d, want %d (the files on disk: Finalize read the frame list mid-drain)", summary.Frames, len(files))
	}
	// The manifest on disk agrees (the fixture is what ships).
	raw, err := os.ReadFile(filepath.Join(root, "selfstop-fixture", "manifest.json"))
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	var man FixtureManifest
	if err := json.Unmarshal(raw, &man); err != nil {
		t.Fatalf("manifest parse: %v", err)
	}
	if len(man.Frames) != len(files) {
		t.Fatalf("manifest frames = %d, want %d (the files on disk)", len(man.Frames), len(files))
	}
	if _, err := VerifyFixture(filepath.Join(root, "selfstop-fixture")); err != nil {
		t.Fatalf("VerifyFixture: %v", err)
	}
}

// gapFlatFrame is one cheap flat 64×48 frame for the capture unit tests
// (TestCaptureMinGap, TestCapturePlanByteBudget, TestCaptureSelfStopDrain
// Join): the assertions are about the stamps, the budget bookkeeping and
// the join the capture loop does — never about the pixels — and a small
// frame keeps the workers' encode/signature/write pipeline a fraction of a
// millisecond per frame, so the drain that StopCapture joins in REAL time
// never depends on how much CPU the host gives the workers under -race
// load (at 640×480 each frame cost ~10 ms of CPU, and the 30× pinned gate
// timed out mid-drain on a host saturated by a parallel Dolphin E2E suite,
// measured 2026-10-19). Flat (single-colour) also keeps the frame out of
// the text-bearing screen classes, so Finalize's record-time OCR step has
// nothing to read (no Tesseract in these unit tests).
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
// Both stop paths join the drain (ADR-0071): StopCapture keys on the
// loop's done channel, not the on-flag the loop clears itself on a budget
// stop — so the join is correct no matter who stopped the loop. Here the
// plan's duration budget (10 min) far exceeds the virtual window the test
// advances (≤ 10 s), so the loop has NOT self-stopped on budget when the
// test calls StopCapture: StopCapture then cancels and joins (cancel ->
// tick false -> loop returns -> close(jobs) + wg.Wait -> close(done)),
// which guarantees every reserved frame is encoded, written and published
// before Finalize reads the frame list. The self-stop side of the same
// contract (budget closes mid-capture, StopCapture joins the drain anyway)
// is TestCaptureSelfStopDrainJoin. A frame count asserted inside a
// wall-clock sleep window is an assertion about the scheduler, which
// full-suite -race load can starve inside that window.
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
	// ~200 ms of virtual time, ~20 steps) or the bound is spent. done is
	// the loop's join channel (still open for the whole advance loop here:
	// the 10 min budget cannot close inside the ≤ 10 s virtual window, so
	// the loop is alive and joinable at StopCapture — the guard is
	// belt-and-braces).
	rec.mu.Lock()
	done := rec.captureDone
	rec.mu.Unlock()
	const step = 10 * time.Millisecond
	const maxSteps = 1000 // 10 s of virtual time, still << the 10 min budget
	frameCount := func() int {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		return len(rec.frames)
	}
	steps := 0
	for steps < maxSteps && frameCount() < 2 {
		clk.advance(step, done)
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
