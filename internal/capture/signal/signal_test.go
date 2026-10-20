package signal

import (
	"image"
	"image/color"
	"math/rand"
	"testing"

	"github.com/dbirks/game-narrator/internal/capture"
)

// frame builds a minimal valid capture.Frame for one synthetic picture.
func frame(seq int64, tsNS int64, w, h int, img image.Image, hash string) capture.Frame {
	return capture.Frame{
		Seq:                seq,
		SourceClock:        capture.ClockMonotonic,
		CaptureTimestampNS: tsNS,
		Width:              w,
		Height:             h,
		PixFmt:             capture.PixRGBA,
		Image:              img,
		Hash:               hash,
	}
}

// solid builds a flat picture.
func solid(w, h int, c color.Color) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	return img
}

// contentFrame is a game-like picture: quadrant colours, a white textbox and
// a dark bar — varied luma and structure, nothing flat, nothing noise.
func contentFrame(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	set := func(x0, y0, x1, y1 int, c color.RGBA) {
		for y := y0; y < y1; y++ {
			for x := x0; x < x1; x++ {
				img.Set(x, y, c)
			}
		}
	}
	set(0, 0, w/2, h/2, color.RGBA{120, 60, 40, 255})
	set(w/2, 0, w, h/2, color.RGBA{30, 80, 140, 255})
	set(0, h/2, w/2, h, color.RGBA{90, 120, 50, 255})
	set(w/2, h/2, w, h, color.RGBA{200, 190, 60, 255})
	set(w/8, h*3/4, w*7/8, h*7/8, color.RGBA{250, 250, 250, 255}) // the textbox
	set(0, 0, w, h/16, color.RGBA{10, 10, 12, 255})               // the dark bar
	return img
}

// staticFrame is deterministic per-pixel noise (a fixed-seed LCG, so the
// test is stable): the TV static the card generates with no input.
func staticFrame(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	rng := rand.New(rand.NewSource(42))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := uint8(rng.Intn(256))
			img.Set(x, y, color.RGBA{v, v, v, 255})
		}
	}
	return img
}

func TestClassifyContentPatterns(t *testing.T) {
	const w, h = 320, 240
	cases := []struct {
		name string
		img  image.Image
		kind Kind
	}{
		{"black", solid(w, h, color.RGBA{0, 0, 0, 255}), Black},
		{"blue", solid(w, h, color.RGBA{0, 0, 205, 255}), Blue},
		{"static", staticFrame(w, h), Static},
		{"content", contentFrame(w, h), OK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.img, "a", "b"); got != tc.kind {
				t.Errorf("Classify(%s) = %v, want %v", tc.name, got, tc.kind)
			}
		})
	}
}

func TestClassifyBlueWithNoSignalText(t *testing.T) {
	// The documented card pattern is not a perfectly flat blue: a white
	// "NO SIGNAL" band sits on the field. The band must not defeat the
	// classification — the flatness guards have headroom for it.
	const w, h = 320, 240
	img := solid(w, h, color.RGBA{0, 0, 205, 255})
	for y := h/2 - 8; y < h/2+8; y++ {
		for x := w/2 - 60; x < w/2+60; x++ {
			img.Set(x, y, color.RGBA{255, 255, 255, 255})
		}
	}
	if got := Classify(img, "a", "b"); got != Blue {
		t.Errorf("blue with a text band = %v, want Blue", got)
	}
}

func TestClassifyNearMisses(t *testing.T) {
	const w, h = 320, 240
	// A dark game scene: texture keeps it out of Black.
	dark := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := uint8(4 + ((x*7 + y*13) % 18)) // 4..21, textured
			dark.Set(x, y, color.Gray{Y: v})
		}
	}
	if got := Classify(dark, "a", "b"); got != OK {
		t.Errorf("dark textured scene = %v, want OK (a dark game scene is not no-signal)", got)
	}

	// A game sky: a blue channel with a gradient, not the flat no-signal field.
	sky := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		grad := y * 100 / h
		for x := 0; x < w; x++ {
			sky.Set(x, y, color.RGBA{uint8(60 + grad/2), uint8(90 + grad/2), uint8(180 + grad), 255})
		}
	}
	if got := Classify(sky, "a", "b"); got != OK {
		t.Errorf("gradient sky = %v, want OK (a blue sky is not the no-signal field)", got)
	}

	// An identical hash is frozen regardless of content.
	if got := Classify(contentFrame(w, h), "same", "same"); got != Frozen {
		t.Errorf("identical hash = %v, want Frozen", got)
	}
	if got := Classify(nil, "", ""); got != OK {
		t.Errorf("nil image = %v, want OK (a decode error is not a signal judgment)", got)
	}
}

func TestMonitorDeclaresLostAfterGate(t *testing.T) {
	m := NewMonitor()
	black := solid(640, 480, color.RGBA{0, 0, 0, 255})
	for i := 1; i <= 2; i++ {
		lost, changed, _ := m.Observe(frame(int64(i), int64(i)*100_000_000, 640, 480, black, ""))
		if lost || changed {
			t.Fatalf("frame %d: lost=%v changed=%v, want the gate not reached", i, lost, changed)
		}
	}
	lost, changed, ev := m.Observe(frame(3, 300_000_000, 640, 480, black, ""))
	if !lost || !changed || ev.Kind != Black || !ev.Lost || ev.Seq != 3 {
		t.Fatalf("frame 3: lost=%v changed=%v ev=%+v, want a Black loss on the gate frame", lost, changed, ev)
	}
}

func TestMonitorRecoversAfterGoodGate(t *testing.T) {
	m := NewMonitor()
	black := solid(640, 480, color.RGBA{0, 0, 0, 255})
	good := contentFrame(640, 480)
	// Three blacks declare the loss.
	for i := 1; i <= 3; i++ {
		m.Observe(frame(int64(i), int64(i)*100_000_000, 640, 480, black, ""))
	}
	if !m.State() {
		t.Fatal("monitor is live after three black frames, want lost")
	}
	// One good frame: still held.
	lost, changed, _ := m.Observe(frame(4, 400_000_000, 640, 480, good, "h1"))
	if !lost || changed {
		t.Fatalf("first good frame: lost=%v changed=%v, want still held", lost, changed)
	}
	// The second: recovered.
	lost, changed, ev := m.Observe(frame(5, 500_000_000, 640, 480, good, "h2"))
	if lost || !changed || ev.Kind != OK || ev.Lost {
		t.Fatalf("second good frame: lost=%v changed=%v ev=%+v, want a recovery", lost, changed, ev)
	}
}

func TestMonitorFrozenWindow(t *testing.T) {
	m := NewMonitor()
	pic := contentFrame(640, 480)
	// Frames one second apart, all one hash: the streak starts on the second
	// frame (the first has no predecessor hash) and is judged frozen once it
	// outlives the 3 s window; the gate then needs three frozen frames.
	for i := 1; i <= 5; i++ {
		hash := ""
		if i > 1 {
			hash = "same"
		}
		lost, _, _ := m.Observe(frame(int64(i), int64(i)*1_000_000_000, 640, 480, pic, hash))
		if lost {
			t.Fatalf("frame %d inside the freeze window declared lost", i)
		}
	}
	var lost, changed bool
	for i := 6; i <= 8; i++ {
		lost, changed, _ = m.Observe(frame(int64(i), int64(i)*1_000_000_000, 640, 480, pic, "same"))
	}
	if !lost || !changed {
		t.Fatalf("after the window: lost=%v changed=%v, want the frozen loss declared on frame 8", lost, changed)
	}
}

func TestMonitorFrozenImmediateWithoutClockAdvance(t *testing.T) {
	m := NewMonitor()
	pic := contentFrame(640, 480)
	const ts = 5_000 // a stream that stamps every frame at one instant
	f := frame(1, ts, 640, 480, pic, "")
	m.Observe(f)
	var lost bool
	for i := 2; i <= 5; i++ {
		var changed bool
		lost, changed, _ = m.Observe(frame(int64(i), ts, 640, 480, pic, "h"))
		if i == 5 {
			if !lost || !changed {
				t.Fatalf("frame %d: lost=%v changed=%v, want the immediate frozen loss", i, lost, changed)
			}
		}
	}
}

func TestMonitorSizeModeChange(t *testing.T) {
	m := NewMonitor()
	small := contentFrame(640, 480)
	big := contentFrame(1280, 720)
	for i := 1; i <= 4; i++ {
		m.Observe(frame(int64(i), int64(i)*100_000_000, 640, 480, small, ""))
	}
	if m.State() {
		t.Fatal("steady 480p input judged lost")
	}
	// Two new-size frames: mid-transition, no loss yet.
	for i := 5; i <= 6; i++ {
		lost, changed, _ := m.Observe(frame(int64(i), int64(i)*100_000_000, 1280, 720, big, ""))
		if lost && changed {
			t.Fatalf("frame %d: the size has not settled, want no transition", i)
		}
	}
	lost, changed, ev := m.Observe(frame(7, 700_000_000, 1280, 720, big, ""))
	if !lost || !changed || ev.Kind != SizeChanged {
		t.Fatalf("settled new size: lost=%v changed=%v kind=%v, want the SizeChanged loss", lost, changed, ev.Kind)
	}
	// Recovery: two good frames at the new size re-admit the flow.
	for i := 8; i <= 9; i++ {
		lost, changed, ev = m.Observe(frame(int64(i), int64(i)*100_000_000, 1280, 720, big, ""))
	}
	if lost || !changed || ev.Lost {
		t.Fatalf("recovery at the new size: lost=%v changed=%v, want a recovery event", lost, changed)
	}
}

func TestMonitorSizeGlitchReturns(t *testing.T) {
	m := NewMonitor()
	small := contentFrame(640, 480)
	big := contentFrame(1280, 720)
	m.Observe(frame(1, 100_000_000, 640, 480, small, ""))
	// One new-size frame, then back: a glitch, not a mode change.
	m.Observe(frame(2, 200_000_000, 1280, 720, big, ""))
	lost, changed, _ := m.Observe(frame(3, 300_000_000, 640, 480, small, ""))
	if lost || changed {
		t.Fatalf("after a one-frame size glitch: lost=%v changed=%v, want none", lost, changed)
	}
}
