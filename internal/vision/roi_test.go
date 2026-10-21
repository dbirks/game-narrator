package vision

import (
	"image"
	"image/color"
	"math"
	"testing"
)

// fillRect paints a solid block into a Gray image, for building frames that are
// exactly the geometry a test reasons about.
func fillRect(g *image.Gray, r image.Rectangle, c color.Gray) {
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			g.SetGray(x, y, c)
		}
	}
}

// pillarBoxed builds a width×height frame whose picture is the mid-grey band
// between black bars of the given widths — the synthetic version of a 4:3 capture
// padded to 16:9.
func pillarBoxed(width, height, leftBar, rightBar int) *image.Gray {
	g := image.NewGray(image.Rect(0, 0, width, height))
	fillRect(g, image.Rect(leftBar, 0, width-rightBar, height), color.Gray{Y: 128})
	return g
}

func TestMapRect(t *testing.T) {
	full := image.Rect(0, 0, 640, 480)
	box := Rect{X: 0.07, Y: 0.72, W: 0.86, H: 0.23}
	got := MapRect(full, box)
	want := image.Rect(45, 346, 595, 456)
	if got != want {
		t.Errorf("MapRect(640x480, %+v) = %v, want %v", box, got, want)
	}

	// The same fractions over a 1080p frame scale proportionally: that is the
	// whole point of normalized coordinates (docs/spec/04 "capture resolution").
	got1080 := MapRect(image.Rect(0, 0, 1920, 1080), box)
	if want1080 := image.Rect(134, 778, 1786, 1026); got1080 != want1080 {
		t.Errorf("MapRect(1920x1080) = %v, want %v", got1080, want1080)
	}

	// Inside a pillarboxed frame, the rect maps onto the picture, not the frame.
	active := image.Rect(107, 0, 747, 480)
	gotActive := MapRect(active, box)
	if wantActive := image.Rect(152, 346, 702, 456); gotActive != wantActive {
		t.Errorf("MapRect(active 640x480 @107) = %v, want %v", gotActive, wantActive)
	}

	// A rect that runs off the right edge is clipped to the picture rather than
	// allowed to sample outside it.
	over := MapRect(full, Rect{X: 0.9, Y: 0.5, W: 0.5, H: 0.2})
	if over.Max.X != 640 || over.Dx() != 64 {
		t.Errorf("over-wide rect clipped to %v, want width 64 ending at 640", over)
	}
}

func TestCropNormalizedDegenerate(t *testing.T) {
	img := image.NewGray(image.Rect(0, 0, 64, 48))
	fillRect(img, image.Rect(0, 0, 64, 48), color.Gray{Y: 100})

	for _, tc := range []struct {
		name string
		rect Rect
	}{
		{"zero size", Rect{0.5, 0.5, 0, 0}},
		{"negative size", Rect{0.5, 0.5, -0.2, -0.2}},
		{"beyond frame", Rect{2, 2, 1, 1}},
		{"zero width only", Rect{0.1, 0.1, 0, 0.5}},
		{"NaN", Rect{math.NaN(), 0.1, math.NaN(), 0.5}},
		{"inf", Rect{math.Inf(1), 0.1, math.Inf(-1), 0.5}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The contract is "no panic, and no pixels invented": a region that
			// cannot be interpreted yields an empty image, which callers check.
			got := CropNormalized(img, tc.rect, CropOptions{})
			if !got.Bounds().Empty() && got.Bounds().Dx() < 0 {
				t.Fatalf("returned bounds %+v, which is neither empty nor sane", got.Bounds())
			}
		})
	}

	t.Run("nil image", func(t *testing.T) {
		if got := CropNormalized(nil, Rect{0, 0, 1, 1}, CropOptions{}); !got.Bounds().Empty() {
			t.Errorf("nil image yielded bounds %v, want empty", got.Bounds())
		}
	})
}

func TestCropNormalizedContent(t *testing.T) {
	img := image.NewGray(image.Rect(0, 0, 100, 100))
	fillRect(img, img.Bounds(), color.Gray{Y: 0})
	fillRect(img, image.Rect(50, 25, 75, 75), color.Gray{Y: 200})

	// The bright block occupies x 0.50..0.75, y 0.25..0.75 of the frame.
	got := CropNormalized(img, Rect{X: 0.5, Y: 0.25, W: 0.25, H: 0.5}, CropOptions{})
	if got.Bounds().Dx() != 25 || got.Bounds().Dy() != 50 {
		t.Fatalf("crop is %v, want 25x50", got.Bounds())
	}
	if got.At(0, 0) != (color.Gray{Y: 200}) {
		t.Errorf("crop starts at %v, want the bright block", got.At(0, 0))
	}
	if got.At(24, 49) != (color.Gray{Y: 200}) {
		t.Errorf("crop ends at %v, want the bright block", got.At(24, 49))
	}

	// Cropping must not alias the source buffer: mutating the frame afterwards
	// cannot change an observation already handed upward (docs/spec/05 "immutable
	// scene observations").
	if g, ok := got.(*image.Gray); ok {
		for i := range g.Pix {
			g.Pix[i] = 7
		}
	}
	if img.At(60, 60) != (color.Gray{Y: 200}) {
		t.Fatal("writing through the crop modified the source frame")
	}
}

// TestCropInset cuts the mapped box on every side (ADR-0115 §2): the textbox
// border the OCR would read as noise glyphs stays out of the input, the cut
// degrades to the empty image when the inset eats the whole box, and a
// negative inset is read as 0 rather than allowed to grow the crop.
func TestCropInset(t *testing.T) {
	img := image.NewGray(image.Rect(0, 0, 100, 100))
	fillRect(img, img.Bounds(), color.Gray{Y: 0})
	fillRect(img, image.Rect(50, 25, 75, 75), color.Gray{Y: 200})

	rect := Rect{X: 0.5, Y: 0.25, W: 0.25, H: 0.5}

	// The inset cut is the mapped box shrunk by the inset on every side, and
	// the content starts past the cut (the border pixels are gone).
	got := CropNormalized(img, rect, CropOptions{Inset: 5})
	if got.Bounds().Dx() != 15 || got.Bounds().Dy() != 40 {
		t.Fatalf("inset-5 crop is %v, want 15x40", got.Bounds())
	}
	if got.At(0, 0) != (color.Gray{Y: 200}) {
		t.Errorf("inset crop starts at %v, want the bright block", got.At(0, 0))
	}

	// A zero inset is the historic cut, byte for byte.
	plain := CropNormalized(img, rect, CropOptions{})
	if plain.Bounds().Dx() != 25 || plain.Bounds().Dy() != 50 {
		t.Fatalf("zero-inset crop is %v, want the historic 25x50", plain.Bounds())
	}

	// An inset that eats the whole box degrades to the empty image, not a
	// negative or invented shape.
	big := CropNormalized(img, rect, CropOptions{Inset: 40})
	if !big.Bounds().Empty() {
		t.Fatalf("over-inset crop yielded bounds %v, want empty", big.Bounds())
	}

	// A negative inset reads as 0: the cut must not grow the OCR input.
	neg := CropNormalized(img, rect, CropOptions{Inset: -7})
	if neg.Bounds().Dx() != 25 || neg.Bounds().Dy() != 50 {
		t.Fatalf("negative-inset crop is %v, want the historic 25x50 (read as 0)", neg.Bounds())
	}

	// CropRect agrees with the cut: the box it draws is the insetting one.
	box := CropRect(img, rect, CropOptions{Inset: 5})
	if want := image.Rect(55, 30, 70, 70); box != want {
		t.Errorf("CropRect inset = %v, want %v", box, want)
	}
}

func TestActiveArea(t *testing.T) {
	// 4:3 content in a 16:9 frame is the docs/spec/12 case "4:3 frame letterboxed
	// into 16:9": the picture is 640 wide and the bars are 107 px each side.
	frame := pillarBoxed(854, 480, 107, 107)
	area, ok := ActiveArea(frame)
	if !ok {
		t.Fatal("ActiveArea found no letterbox in an 854x480 pillarboxed frame")
	}
	if want := image.Rect(107, 0, 747, 480); area != want {
		t.Errorf("ActiveArea = %v, want %v", area, want)
	}

	// A native 4:3 frame must be left alone. Scanning it for bars is how a dark
	// game sky becomes a 20-pixel-tall crop.
	native := image.NewGray(image.Rect(0, 0, 640, 480))
	fillRect(native, native.Bounds(), color.Gray{Y: 90})
	if area, ok := ActiveArea(native); ok {
		t.Errorf("ActiveArea trimmed a 4:3 frame to %v", area)
	}

	// A black screen is not a picture with bars: trimming it would crop the OCR
	// input to whatever noise the capture device left in the middle.
	black := image.NewGray(image.Rect(0, 0, 854, 480))
	if _, ok := ActiveArea(black); ok {
		t.Error("ActiveArea reported an active area in an all-black frame")
	}

	// Top and bottom bars are found too, because anamorphic 16:9 content inside a
	// taller frame is the same rule on the other axis.
	letterboxed := image.NewGray(image.Rect(0, 0, 640, 480))
	fillRect(letterboxed, letterboxed.Bounds(), color.Gray{Y: 200})
	fillRect(letterboxed, image.Rect(0, 0, 640, 60), color.Gray{Y: 2})
	// Aspect 640/480 = 1.33 is below the wide-frame gate, so this frame is
	// deliberately not scanned — asserting the gate, not just the maths.
	if _, ok := ActiveArea(letterboxed); ok {
		t.Error("ActiveArea ran on a 4:3 frame; the minWideAspect gate is not applied")
	}
}

func TestCropNormalizedLetterbox(t *testing.T) {
	// The picture carries a marker in its bottom-left corner; bars are black. If
	// the letterbox rule is applied, the normalized rect lands on the marker; if
	// it is not, the same rect lands in the left bar and reads black. That is the
	// observable difference between "adapts to letterboxing" and not.
	frame := pillarBoxed(854, 480, 107, 107)
	fillRect(frame, image.Rect(120, 400, 180, 440), color.Gray{Y: 250})

	rect := Rect{X: 0.02, Y: 0.83, W: 0.11, H: 0.09}
	det := CropNormalized(frame, rect, CropOptions{Letterbox: LetterboxDetect})
	if det.Bounds().Empty() {
		t.Fatal("detect crop is empty")
	}
	if !containsBright(det) {
		t.Errorf("letterbox-aware crop missed the marker in the picture: %v", sample(det))
	}

	naive := CropNormalized(frame, rect, CropOptions{Letterbox: LetterboxNone})
	if containsBright(naive) {
		t.Error("LetterboxNone crop contained the marker, so the two modes are not distinguished")
	}
}

func containsBright(img image.Image) bool {
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if lumaAt(img, x, y) > 200 {
				return true
			}
		}
	}
	return false
}

func sample(img image.Image) []uint8 {
	b := img.Bounds()
	out := make([]uint8, 0, 4)
	for i := 0; i < 4 && i < b.Dx()*b.Dy(); i++ {
		x := b.Min.X + i%b.Dx()
		y := b.Min.Y + i/b.Dx()
		out = append(out, lumaAt(img, x, y))
	}
	return out
}
