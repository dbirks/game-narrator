package vision

import (
	"image"
	"image/color"
	"testing"
)

// The tests below exercise the Tracker on synthetic bordered frames only —
// the synthetic-frames rule of AGENTS.md: no captured game frames in the repo.
// can320x180 is a 16:9 canvas (the HDMI case); winA is the "normal" window and
// winB a changed one (a cutscene letterbox clearing at the bottom).
const (
	cw, ch = 320, 180 // canvas, aspect 1.78 > minWideAspect
)

var (
	winA = image.Rect(20, 20, 300, 160) // normal picture window
	winB = image.Rect(20, 20, 300, 120) // letterboxed (shorter) window
)

// borderedFrame builds a cw×ch canvas whose window is flat mid-grey (well
// above BarLuma) and everything outside it pure black (well below).
func borderedFrame(win image.Rectangle) *image.Gray {
	g := image.NewGray(image.Rect(0, 0, cw, ch))
	// Fill the bars with 0, the window with 128.
	for y := 0; y < ch; y++ {
		for x := 0; x < cw; x++ {
			v := uint8(0)
			if x >= win.Min.X && x < win.Max.X && y >= win.Min.Y && y < win.Max.Y {
				v = 128
			}
			g.SetGray(x, y, color.Gray{Y: v})
		}
	}
	return g
}

// blackFrame is a pure-black canvas: every line is a bar, so ActiveArea finds
// no picture and reports not-ok. A fade frame is indistinguishable to the
// bar scan and must be treated the same way.
func blackFrame() *image.Gray {
	return image.NewGray(image.Rect(0, 0, cw, ch))
}

// dimFrame has content, but at luma 5 — below BarLuma, so the whole frame
// reads as black to the bar scan.
func dimFrame() *image.Gray {
	g := image.NewGray(image.Rect(0, 0, cw, ch))
	for y := 0; y < ch; y++ {
		for x := 0; x < cw; x++ {
			g.SetGray(x, y, color.Gray{Y: 5})
		}
	}
	return g
}

// settle feeds the tracker enough identical frames for the default settle
// count to accept win.
func settle(t *testing.T, tr *Tracker, win image.Rectangle) {
	t.Helper()
	for i := 0; i < defaultSettle; i++ {
		got := tr.Update(borderedFrame(win))
		// While settling (and after), the returned window is the whole frame
		// until the very last settle frame, which adopts and returns win.
		_ = got
	}
	if _, ok := tr.Box(); !ok {
		t.Fatal("tracker never settled a window")
	}
}

func TestTrackerSettlesAndHolds(t *testing.T) {
	tr := NewTracker(TrackerConfig{})
	if _, ok := tr.Box(); ok {
		t.Fatal("fresh tracker reports a window before any frame")
	}
	// Before the settle count the tracker maps against the whole frame.
	if got := tr.Update(borderedFrame(winA)); got != image.Rect(0, 0, cw, ch) {
		t.Errorf("first update = %v, want whole frame", got)
	}
	tr.Update(borderedFrame(winA))
	if _, ok := tr.Box(); ok {
		t.Fatal("tracker adopted a window after one observation")
	}
	tr.Update(borderedFrame(winA))
	acc, ok := tr.Box()
	if !ok || acc != winA {
		t.Fatalf("after %d identical frames Box = %v ok=%v, want %v", defaultSettle, acc, ok, winA)
	}

	// A black frame, a fade frame and a second black frame must not move the
	// accepted window, and must each still hand back the window (not the
	// canvas) for the region mapping.
	for i, f := range []*image.Gray{blackFrame(), dimFrame(), blackFrame()} {
		if got := tr.Update(f); got != winA {
			t.Errorf("frame %d (unreliable): Update = %v, want held window %v", i, got, winA)
		}
	}
	acc, ok = tr.Box()
	if !ok || acc != winA {
		t.Fatalf("Box moved on unreliable frames: %v ok=%v", acc, ok)
	}
	// A normal frame keeps the held window exactly (no re-detection drift).
	if got := tr.Update(borderedFrame(winA)); got != winA {
		t.Errorf("normal frame after blacks: Update = %v, want %v", got, winA)
	}
}

func TestTrackerHysteresisKeepsCropStationary(t *testing.T) {
	// A bar edge that jitters by 1-2 pixels per frame is noise, not a new
	// window: the accepted window must be kept exactly, so the crop and the
	// region hashes do not move.
	tr := NewTracker(TrackerConfig{})
	settle(t, tr, winA)
	jitter := winA
	jitter.Min.X += 2 // within the default tolerance
	for i := 0; i < 20; i++ {
		if got := tr.Update(borderedFrame(jitter)); got != winA {
			t.Fatalf("jitter frame %d moved the crop to %v; want %v", i, got, winA)
		}
	}
	if acc, _ := tr.Box(); acc != winA {
		t.Fatalf("Box = %v after jitter, want %v", acc, winA)
	}
}

func TestTrackerRejectsOneFrameGlitch(t *testing.T) {
	tr := NewTracker(TrackerConfig{})
	settle(t, tr, winA)
	// One frame with a different window (a glitch, a dropped frame that
	// decoded wrong, a one-frame converter blip) must not move the crop.
	if got := tr.Update(borderedFrame(winB)); got != winA {
		t.Fatalf("one glitch frame moved the crop to %v", got)
	}
	// And two, even, still must not: the settle count is three.
	tr.Update(borderedFrame(winB))
	if acc, _ := tr.Box(); acc != winA {
		t.Fatalf("two glitch frames moved the crop to %v", acc)
	}
	// Back to normal: the count dies with the frame.
	if got := tr.Update(borderedFrame(winA)); got != winA {
		t.Fatalf("crop did not stay put: %v", got)
	}
	if acc, _ := tr.Box(); acc != winA {
		t.Fatalf("Box = %v after a glitch burst, want %v", acc, winA)
	}
}

func TestTrackerAdoptsSettledChange(t *testing.T) {
	tr := NewTracker(TrackerConfig{})
	settle(t, tr, winA)
	// A real change (a cutscene letterbox clearing) repeats and is adopted
	// after the settle count — a few frames late, by design.
	for i := 0; i < defaultSettle-1; i++ {
		if got := tr.Update(borderedFrame(winB)); got != winA {
			t.Fatalf("frame %d of the change moved early: %v", i, got)
		}
	}
	if got := tr.Update(borderedFrame(winB)); got != winB {
		t.Fatalf("settled change not adopted: %v", got)
	}
	// Hysteresis works in the new direction too: one frame of the old window
	// must not bounce the crop back.
	if got := tr.Update(borderedFrame(winA)); got != winB {
		t.Fatalf("one old-window frame bounced the crop to %v", got)
	}
	if acc, _ := tr.Box(); acc != winB {
		t.Fatalf("Box = %v, want %v", acc, winB)
	}
}

func TestTrackerFreshStartOnBlacks(t *testing.T) {
	tr := NewTracker(TrackerConfig{})
	// A session that starts on black frames invents no window: every update
	// maps against the whole frame until real picture arrives.
	for i := 0; i < defaultSettle+2; i++ {
		if got := tr.Update(blackFrame()); got != image.Rect(0, 0, cw, ch) {
			t.Fatalf("black frame %d: Update = %v, want whole frame", i, got)
		}
	}
	if _, ok := tr.Box(); ok {
		t.Fatal("tracker settled a window out of black frames")
	}
	// Then the picture arrives and settles normally.
	settle(t, tr, winA)
	if acc, ok := tr.Box(); !ok || acc != winA {
		t.Fatalf("Box = %v ok=%v after picture arrived", acc, ok)
	}
}

func TestTrackerReset(t *testing.T) {
	tr := NewTracker(TrackerConfig{})
	settle(t, tr, winA)
	tr.Reset()
	if _, ok := tr.Box(); ok {
		t.Fatal("Reset left a window behind")
	}
	if got := tr.Update(blackFrame()); got != image.Rect(0, 0, cw, ch) {
		t.Fatalf("after Reset a black frame returns %v, want whole frame", got)
	}
}

// TestCropNormalizedWithTracker proves the ROI integration: with a settled
// tracker the region maps against the held window, and a black frame does not
// pull the crop back onto the whole canvas.
func TestCropNormalizedWithTracker(t *testing.T) {
	win := image.Rect(30, 20, 290, 160)
	// A marker in the picture's bottom-left corner, in the same place the real
	// textbox sits in the real window.
	mark := image.Rect(40, 130, 90, 150)
	rect := Rect{
		X: float64(mark.Min.X-win.Min.X) / float64(win.Dx()),
		Y: float64(mark.Min.Y-win.Min.Y) / float64(win.Dy()),
		W: float64(mark.Dx()) / float64(win.Dx()),
		H: float64(mark.Dy()) / float64(win.Dy()),
	}

	tr := NewTracker(TrackerConfig{})
	settle(t, tr, win)
	opts := CropOptions{Letterbox: LetterboxDetect, Tracker: tr}

	frame := borderedFrame(win)
	fillRect(frame, mark, color.Gray{Y: 250})
	crop := CropNormalized(frame, rect, opts)
	if crop.Bounds().Empty() {
		t.Fatal("tracker crop is empty")
	}
	if !containsBright(crop) {
		t.Errorf("tracker crop missed the marker: %v", sample(crop))
	}
	// The crop is the window-mapped box (as a zero-origin copy): its size must
	// match MapRect against the window, not against the whole frame.
	wantWin := MapRect(win, rect)
	wantWhole := MapRect(image.Rect(0, 0, cw, ch), rect)
	if crop.Bounds().Dx() != wantWin.Dx() || crop.Bounds().Dy() != wantWin.Dy() {
		t.Fatalf("crop %v, want window-mapped size %dx%d", crop.Bounds(), wantWin.Dx(), wantWin.Dy())
	}
	if crop.Bounds().Dx() == wantWhole.Dx() && crop.Bounds().Dy() == wantWhole.Dy() {
		t.Errorf("crop %v is the whole-frame mapping %v, not the window mapping", crop.Bounds(), wantWhole)
	}

	// Now the same frame goes black (a fade), tracker still holding the
	// window: the crop must stay the window-sized box at the window position —
	// without the tracker (stateless LetterboxDetect) the same black frame
	// falls back to the whole canvas and the crop jumps.
	dark := blackFrame()
	cropDark := CropNormalized(dark, rect, opts)
	wantDark := MapRect(win, rect)
	if cropDark.Bounds().Dx() != wantDark.Dx() || cropDark.Bounds().Dy() != wantDark.Dy() {
		t.Fatalf("black frame moved the crop to %v, want window-sized %v", cropDark.Bounds(), wantDark)
	}
	// And the stateless path demonstrably does move: same black frame, no
	// tracker, the crop is the whole-frame mapping.
	stateless := CropNormalized(dark, rect, CropOptions{Letterbox: LetterboxDetect})
	whole := MapRect(image.Rect(0, 0, cw, ch), rect)
	if stateless.Bounds().Dx() != whole.Dx() || stateless.Bounds().Dy() != whole.Dy() {
		t.Fatalf("stateless crop on black frame = %v, want whole-frame mapping %v", stateless.Bounds(), whole)
	}
}

// TestTrackerNilKeepsStatelessBehaviour pins the backward-compatibility
// contract: CropOptions without a tracker is exactly the historic path.
func TestTrackerNilKeepsStatelessBehaviour(t *testing.T) {
	frame := borderedFrame(winA)
	withTracker := CropNormalized(frame, Rect{0.1, 0.1, 0.8, 0.8}, CropOptions{Letterbox: LetterboxDetect})
	tr := NewTracker(TrackerConfig{})
	tr.Update(frame) // not settled yet; Box is not ok
	same := CropNormalized(frame, Rect{0.1, 0.1, 0.8, 0.8}, CropOptions{Letterbox: LetterboxDetect, Tracker: tr})
	if withTracker.Bounds() != same.Bounds() {
		t.Fatalf("unsettled tracker changed the crop: %v vs %v", withTracker.Bounds(), same.Bounds())
	}
}

// TestTrackerSettleSingleFrame pins the one-shot seam (ADR-0082 amendment 1):
// a single frame has no stream to hysteresise over, so Settle adopts the
// frame's own window from one observation, and the crop then maps through it
// exactly as the frame loop's three-frame settle would.
func TestTrackerSettleSingleFrame(t *testing.T) {
	tr := NewTracker(TrackerConfig{})
	if _, ok := tr.Box(); ok {
		t.Fatal("a fresh tracker reports a window")
	}
	tr.Settle(borderedFrame(winA))
	got, ok := tr.Box()
	if !ok {
		t.Fatal("Settle left the tracker unsettled on a bordered frame")
	}
	if got != winA {
		t.Fatalf("Settle settled %v, want %v", got, winA)
	}
	// The crop maps through the settled window, the same box the loop's settle
	// would have produced.
	rect := Rect{0.1, 0.1, 0.8, 0.8}
	oneShot := CropNormalized(borderedFrame(winA), rect, CropOptions{Letterbox: LetterboxDetect, Tracker: tr})
	loop := NewTracker(TrackerConfig{})
	settle(t, loop, winA)
	viaLoop := CropNormalized(borderedFrame(winA), rect, CropOptions{Letterbox: LetterboxDetect, Tracker: loop})
	if oneShot.Bounds() != viaLoop.Bounds() {
		t.Fatalf("one-shot window crop = %v, want the loop-settled crop %v", oneShot.Bounds(), viaLoop.Bounds())
	}
}

// TestTrackerSettleBorderlessInventsNothing pins the other side: a frame that
// reports no window — borderless (Dolphin), pure black, or nil — settles
// nothing, so the crop keeps mapping the whole frame.
func TestTrackerSettleBorderlessInventsNothing(t *testing.T) {
	tr := NewTracker(TrackerConfig{})
	flat := image.NewGray(image.Rect(0, 0, 400, 300)) // 4:3: the scan does not run
	for y := 0; y < 300; y++ {
		for x := 0; x < 400; x++ {
			flat.SetGray(x, y, color.Gray{Y: 128})
		}
	}
	tr.Settle(flat)
	if _, ok := tr.Box(); ok {
		t.Fatal("Settle invented a window on a borderless 4:3 frame")
	}
	tr.Settle(blackFrame())
	if _, ok := tr.Box(); ok {
		t.Fatal("Settle invented a window on a black frame")
	}
	tr.Settle(nil) // a no-op, not a panic
	if _, ok := tr.Box(); ok {
		t.Fatal("Settle(nil) invented a window")
	}
}

// TestTrackerSettleThenUpdateKeepsTheWindow pins that Settle is the single-
// frame seam and not a bypass the frame loop can fall into: after a Settle,
// Update keeps the settled window under one changed frame and adopts a real
// change only after the settle count.
func TestTrackerSettleThenUpdateKeepsTheWindow(t *testing.T) {
	tr := NewTracker(TrackerConfig{})
	tr.Settle(borderedFrame(winA))
	if got := tr.Update(borderedFrame(winA)); got != winA {
		t.Fatalf("Update on the settled frame moved the window to %v, want %v", got, winA)
	}
	if got := tr.Update(borderedFrame(winB)); got != winA {
		t.Fatalf("one changed frame moved the settled window to %v, want %v held", got, winA)
	}
	if got := tr.Update(borderedFrame(winA)); got != winA {
		t.Fatalf("the hold restarted wrong: window = %v", got)
	}
	want := winA
	for i := 0; i < defaultSettle; i++ {
		if i == defaultSettle-1 {
			want = winB // the settle frame adopts and returns the new window
		}
		if got := tr.Update(borderedFrame(winB)); got != want {
			t.Fatalf("frame %d: window = %v, want %v", i, got, want)
		}
	}
	if got, _ := tr.Box(); got != winB {
		t.Fatalf("the settled change was not adopted: %v, want %v", got, winB)
	}
}

// TestTrackerBoxIsSafeFromAnotherGoroutine is the -race assertion behind the
// dev-trace wiring (ADR-0082 amendment 1): the capture pump reads Box while
// the ROI stage runs Update, and the mutex keeps every read whole.
func TestTrackerBoxIsSafeFromAnotherGoroutine(t *testing.T) {
	tr := NewTracker(TrackerConfig{})
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				tr.Box()
			}
		}
	}()
	for i := 0; i < 500; i++ {
		tr.Update(borderedFrame(winA))
	}
	close(stop)
	<-done
}
