package pipeline

import (
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/dbirks/game-narrator/internal/dolphin"
	"github.com/dbirks/game-narrator/internal/vision"
)

// TestPresenceProbeFrames is the local probe for the 2026-10-23 board run
// (issue #44, ADR-0169): a dark translucent dialogue box over open sea. The
// probe isolated the feature that fails: the pipeline's presence gate
// misreads the open-sea/sky band behind and around the box as a box (the sea
// is weakly saturated — under ADR-0137's dark-saturated ceiling — and bright
// at the top), so the gate's verdict on the thin translucent band is
// dominated by the sea and the box reads absent the moment its advance arrow
// (a bright stroke the gate was latching on) disappears.
//
// The four private frames (copyrighted: never copied into the repo,
// referenced only by their names) are read from the directory named by
// GN_PRESENCE_FRAMES when set; the test skips when the variable is unset.
//
// Expected verdicts (the arrow is a marker, not the box; the sea is not a
// box):
//
//	box-with-arrow.jpg       present on both detectors
//	box-no-arrow.jpg         present on both detectors (the arrow is not the box)
//	box-bottom-closeup.jpg   present on both detectors
//	no-box-scene.png         absent on both detectors (the bug: the pipeline read it present)
//
// probeBand is the superset bottom band the gate is replayed on for these
// full frames (the same shape presence_captured_test.go uses for the board
// capture, ADR-0137): the textbox region sits inside it, so any sub-band
// verdict is the verdict the pipeline sees on the box region.
var probeBand = vision.Rect{X: 0.10, Y: 0.62, W: 0.80, H: 0.38}

// cropBand cuts probeBand out of a full frame as a fresh NRGBA image (the
// same normalized cut roi.go's CropNormalized makes, no crop options).
func cropBand(img image.Image) image.Image {
	b := img.Bounds()
	rx := probeBand
	cb := image.NewNRGBA(image.Rect(0, 0, int(float64(b.Dx())*rx.W), int(float64(b.Dy())*rx.H)))
	draw.Draw(cb, cb.Bounds(), img, image.Point{X: int(float64(b.Dx()) * rx.X), Y: int(float64(b.Dy()) * rx.Y)}, draw.Src)
	return cb
}

func TestPresenceProbeFrames(t *testing.T) {
	dir := os.Getenv("GN_PRESENCE_FRAMES")
	if dir == "" {
		// A plain skip, not the gate's NOT RUN convention: this probe is a
		// private-frame diagnostic, not a capability the gate expects on a
		// fixture-less host, so it must not turn test-replay's exit 2.
		t.Skip("GN_PRESENCE_FRAMES unset — the private-frame probe has no frames on this host (set GN_PRESENCE_FRAMES to a frame directory; frames are copyrighted and never committed)")
	}

	want := []struct {
		file        string
		wantPresent bool
	}{
		{"box-with-arrow.jpg", true},
		{"box-no-arrow.jpg", true},
		{"box-bottom-closeup.jpg", true},
		{"no-box-scene.png", false},
	}

	pc := DefaultPresence()
	cp := dolphin.BoxPresent()

	for _, tc := range want {
		t.Run(tc.file, func(t *testing.T) {
			path := filepath.Join(dir, tc.file)
			img, err := decodeFrame(path)
			if err != nil {
				t.Fatalf("decode %s: %v", path, err)
			}

			// Pipeline presence gate on the board-band region crop: the same
			// call roi.go's analyze makes (raw colour crop, the quadruple
			// gate, production defaults).
			pp := vision.PanelPresenceOf(cropBand(img), pc.DarkLuma, vision.DefaultTextLuma, vision.DefaultSatFloor)
			gotPipe := pc.Present(pp)

			// Dolphin's structural checkpoint on the same frame.
			gotDolphin, detail := cp(t.Context(), img, nil)

			t.Logf("%s: pipeline PanelPresence{Dark=%.4f Text=%.6f SaturatedDark=%.4f TopDark=%.3f} -> present=%v; dolphin BoxPresent -> %v (%s)",
				tc.file, pp.Dark, pp.Text, pp.SaturatedDark, pp.TopDark, gotPipe, gotDolphin, detail)

			if gotPipe != tc.wantPresent {
				t.Errorf("pipeline presence = %v, want %v (PanelPresence{Dark=%.4f Text=%.6f SaturatedDark=%.4f TopDark=%.3f})",
					gotPipe, tc.wantPresent, pp.Dark, pp.Text, pp.SaturatedDark, pp.TopDark)
			}
			if gotDolphin != tc.wantPresent {
				t.Errorf("dolphin BoxPresent = %v, want %v: %s", gotDolphin, tc.wantPresent, detail)
			}
		})
	}
}

// decodeFrame reads a probe frame by extension (jpg or png).
func decodeFrame(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if filepath.Ext(path) == ".png" {
		return png.Decode(f)
	}
	return jpeg.Decode(f)
}
