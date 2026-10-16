package vision

import (
	"image"
	"image/color"
	"math/rand"
	"testing"
)

// textSweep draws a stand-in for a textbox: a flat background with a band of
// alternating dark/light columns, i.e. an input with a genuinely bimodal
// histogram, which is what the thresholding tests need a real shape to argue
// about.
func textSweep(bg, ink uint8, w, h int) *image.Gray {
	g := image.NewGray(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := bg
			if x%7 < 2 && y > h/4 && y < 3*h/4 {
				v = ink
			}
			g.Pix[y*g.Stride+x] = v
		}
	}
	return g
}

func TestToGrayModels(t *testing.T) {
	// The same visible grey must convert to the same luma from every model the
	// capture chain produces, or a cache key depends on the transport rather than
	// on the picture (docs/spec/05 "suppress duplicate OCR calls on unchanged
	// region hash").
	red := color.NRGBA{R: 200, G: 100, B: 50, A: 255}
	imgs := map[string]image.Image{
		"nrgba":   uniform(6, 4, red),
		"rgba":    uniformRGBA(6, 4, red),
		"generic": &wrapped{uniform(6, 4, red)},
	}
	var first string
	for name, img := range imgs {
		got := ImageHash(img)
		if first == "" {
			first = got
			continue
		}
		if got != first {
			t.Errorf("%s hashed to %s, want %s — the same picture must have one key", name, got, first)
		}
	}
}

func TestToGrayBT601(t *testing.T) {
	// BT.601 weights: red 0.299, green 0.587, blue 0.114. White stays white and
	// black stays black; green is the dominant contributor, which is the whole
	// reason 601 and 709 differ visibly on a green field.
	for _, tc := range []struct {
		name        string
		c           color.NRGBA
		want, slack uint8
	}{
		{"white", color.NRGBA{255, 255, 255, 255}, 255, 0},
		{"black", color.NRGBA{0, 0, 0, 255}, 0, 0},
		{"red", color.NRGBA{255, 0, 0, 255}, 76, 1},    // 0.299*255
		{"green", color.NRGBA{0, 255, 0, 255}, 150, 1}, // 0.587*255
		{"blue", color.NRGBA{0, 0, 255, 255}, 29, 1},   // 0.114*255
	} {
		got := ToGray(uniform(2, 2, tc.c)).Pix[0]
		if diff := int(got) - int(tc.want); diff > int(tc.slack) || diff < -int(tc.slack) {
			t.Errorf("%s: luma %d, want %d±%d", tc.name, got, tc.want, tc.slack)
		}
	}
}

func TestImageHashCoversDimsAndPixels(t *testing.T) {
	a := textSweep(240, 20, 40, 20)
	b := textSweep(240, 20, 40, 20)
	if ImageHash(a) != ImageHash(b) {
		t.Error("identical images hashed differently — preprocessing would never hit its cache")
	}
	// Same pixels, different dimensions: a 40×20 strip is not a 20×40 one, and a
	// hash that ignored dimensions would call them the same region.
	rotated := textSweep(240, 20, 20, 40)
	if ImageHash(a) == ImageHash(rotated) {
		t.Error("40x20 and 20x40 hashed the same")
	}
	// One luma value changed.
	b.Pix[100] = 3
	if ImageHash(a) == ImageHash(b) {
		t.Error("a changed pixel did not change the hash")
	}
	// A sub-image and a copy of it must agree: this is what lets a crop from a
	// shared ring buffer key the same as a decoded fixture.
	sub := a.SubImage(image.Rect(4, 4, 20, 16))
	cpy := image.NewGray(sub.Bounds())
	for y := 4; y < 16; y++ {
		for x := 4; x < 20; x++ {
			cpy.SetGray(x, y, color.Gray{Y: a.GrayAt(x, y).Y})
		}
	}
	if ImageHash(sub) != ImageHash(cpy) {
		t.Error("sub-image and equal copy hashed differently")
	}
}

func TestPreprocessDeterministic(t *testing.T) {
	src := textSweep(30, 235, 200, 60)
	steps := DefaultSteps()
	h1 := ImageHash(Preprocess(src, steps))
	h2 := ImageHash(Preprocess(src, steps))
	if h1 != h2 {
		t.Fatalf("Preprocess is not deterministic: %s vs %s", h1, h2)
	}
	if got := ImageHash(Preprocess(CropNormalized(src, Rect{0, 0, 1, 1}, CropOptions{}), steps)); got != h1 {
		t.Errorf("a whole-frame crop preprocessed differently: %s vs %s", got, h1)
	}
}

func TestPreprocessOutputIsBinary(t *testing.T) {
	got := Preprocess(textSweep(30, 235, 200, 60), Steps{AutoInvert: true, Scale: 2, Otsu: true})
	bins := Histogram(got)
	if bins[0] == 0 || bins[255] == 0 {
		t.Errorf("thresholded output is not binary: ink=%d paper=%d", bins[0], bins[255])
	}
	for v := 1; v < 255; v++ {
		if bins[v] != 0 {
			t.Fatalf("thresholded output kept grey level %d (%d pixels)", v, bins[v])
		}
	}
	if got.Rect.Dx() != 400 || got.Rect.Dy() != 120 {
		t.Errorf("2x upscale of 200x60 gave %v", got.Bounds())
	}
	if got.Rect.Min != (image.Point{}) {
		t.Errorf("preprocessed image origin is %v, want (0,0)", got.Rect.Min)
	}
}

func TestAutoInvertDecision(t *testing.T) {
	dark := textSweep(30, 235, 100, 40)  // light text on a dark box
	light := textSweep(235, 20, 100, 40) // dark text on a light box
	if !ShouldInvert(dark) {
		t.Error("light-on-dark crop was not marked for inversion")
	}
	if ShouldInvert(light) {
		t.Error("dark-on-light crop was inverted — the engine would read its negative")
	}
	// The observable consequence: both recipes must end with ink darker than
	// paper, because that is the polarity Tesseract is trained on.
	for _, tc := range []struct {
		name string
		in   *image.Gray
	}{{"light-on-dark", dark}, {"dark-on-light", light}} {
		out := Preprocess(tc.in, DefaultSteps())
		bins := Histogram(out)
		if bins[0] >= bins[255] {
			t.Errorf("%s: after preprocessing, ink (%d) is not the minority of paper (%d)", tc.name, bins[0], bins[255])
		}
	}
	// An empty image is not "a dark box": it must not be treated as evidence.
	if ShouldInvert(image.NewGray(image.Rectangle{})) {
		t.Error("empty image was marked for inversion")
	}
}

func TestOtsuRejectsTexture(t *testing.T) {
	// The bimodal case: a textbox with text separates cleanly. The selected level
	// itself is not asserted — with a two-level input every threshold between the
	// modes has the same between-class variance, and the tie goes to the lowest —
	// what matters is that the split it implies is the real one.
	th := Otsu(textSweep(30, 235, 200, 60))
	if !th.Trustworthy() {
		t.Errorf("text crop rejected: %+v", th)
	}
	if th.Separation < 150 {
		t.Errorf("separation %.1f on a crop whose modes are 205 levels apart", th.Separation)
	}

	// The texture case: a noisy, textless crop. Otsu always returns a level, so
	// the judgement has to come from the statistics — this is the guard that stops
	// an empty textbox being read as a sentence (docs/spec/12 "Game running
	// without text"), and the committed empty-box fixture is its end-to-end test.
	noise := image.NewGray(image.Rect(0, 0, 200, 60))
	rnd := rand.New(rand.NewSource(7))
	for i := range noise.Pix {
		noise.Pix[i] = uint8(30 + rnd.Intn(20))
	}
	if th := Otsu(noise); th.Trustworthy() {
		t.Errorf("uniform noise was accepted as a text split: %+v", th)
	}
	// And the pipeline consequence: with the split rejected, preprocessing leaves
	// the crop's own greys alone rather than painting confetti. Checked inside the
	// unpadded area, since the border is legitimately white.
	steps := DefaultSteps()
	steps.Pad = 0
	out := Preprocess(noise, steps)
	if b := Histogram(out); b[0] > 0 || b[255] > 0 {
		t.Errorf("rejected threshold still binarised the crop: black=%d white=%d", b[0], b[255])
	}
}

func TestMedianRemovesSpeckle(t *testing.T) {
	g := image.NewGray(image.Rect(0, 0, 32, 32))
	for i := range g.Pix {
		g.Pix[i] = 255
	}
	// A 2-pixel stem survives, an isolated dot does not.
	for y := 4; y < 20; y++ {
		g.Pix[y*g.Stride+8] = 0
		g.Pix[y*g.Stride+9] = 0
	}
	g.Pix[30*g.Stride+20] = 0
	applyThreshold(g, 128)
	median3x3(g)
	if got := g.GrayAt(12, 30).Y; got == 0 {
		t.Error("median filter kept an isolated speckle")
	}
	if got := g.GrayAt(8, 12).Y; got != 0 {
		t.Errorf("median filter removed a 2px stem (luma %d)", got)
	}
}

func TestUpscaleFilters(t *testing.T) {
	src := image.NewGray(image.Rect(0, 0, 4, 2))
	src.Pix[0] = 0
	src.Pix[1] = 0
	src.Pix[2] = 0
	src.Pix[3] = 200
	src.Pix[4] = 100
	src.Pix[7] = 255

	n := upscaleNearest(src, 3)
	if n.Rect.Dx() != 12 || n.Rect.Dy() != 6 {
		t.Fatalf("nearest upscale size %v", n.Bounds())
	}
	if n.GrayAt(0, 0).Y != 0 || n.GrayAt(2, 0).Y != 0 || n.GrayAt(9, 0).Y != 200 {
		t.Errorf("nearest upscale is not a block copy: row0=%v", rowPix(n, 0))
	}
	if n.GrayAt(0, 3).Y != 100 || n.GrayAt(11, 5).Y != 255 {
		t.Errorf("nearest upscale lost the second row: %v", rowPix(n, 3))
	}

	b := upscaleBilinear(src, 3)
	if b.Rect.Dx() != 12 || b.Rect.Dy() != 6 {
		t.Fatalf("bilinear upscale size %v", b.Bounds())
	}
	// Interpolated values stay inside the source's own range and increase across
	// the row: exact numbers are the point of the fixed-point maths.
	prev := -1
	for x := 0; x < b.Rect.Dx(); x++ {
		v := int(b.GrayAt(x, 0).Y)
		if v < prev {
			t.Fatalf("bilinear row0 decreased at x=%d: %d after %d", x, v, prev)
		}
		if v < 0 || v > 255 {
			t.Fatalf("bilinear produced out-of-range luma %d", v)
		}
		prev = v
	}
	if b.GrayAt(0, 0).Y != 0 {
		t.Errorf("bilinear first pixel %d, want the source value 0", b.GrayAt(0, 0).Y)
	}
	if last := b.GrayAt(11, 5).Y; last != 255 {
		t.Errorf("bilinear last pixel %d, want the clamped source value 255", last)
	}
	// Pixel-centre alignment puts the last source centre at dest x = (3+0.5)*3 -
	// 0.5 = 10; past it the filter clamps rather than inventing values beyond the
	// data, which is what ImageMagick's `-filter point`/`-resize` do too.
	if b.GrayAt(10, 0).Y != 200 || b.GrayAt(11, 0).Y != 200 {
		t.Errorf("bilinear row tail is %v, want 200 from x=10 on", rowPix(b, 0))
	}
	// x=8 maps to source coordinate (8+0.5)/3 - 0.5 = 2.33, i.e. a third of the way
	// from the zero column to the 200 column. Asserting an exact value is the point
	// of doing the interpolation in fixed point: it is the same on every host.
	if mid := b.GrayAt(8, 0).Y; mid < 65 || mid > 69 {
		t.Errorf("bilinear x=8 is %d, want ~67", mid)
	}
}

func TestStepsHash(t *testing.T) {
	a := DefaultSteps()
	if a.Hash() != DefaultSteps().Hash() {
		t.Error("Steps.Hash is not stable across calls")
	}
	cases := map[string]Steps{
		"invert off": {Scale: 2, Otsu: true, Median3x3: true, Pad: 8},
		"scale 3":    {AutoInvert: true, Scale: 3, Otsu: true, Median3x3: true, Pad: 8},
		"bilinear":   {AutoInvert: true, Scale: 2, Filter: FilterBilinear, Otsu: true, Median3x3: true, Pad: 8},
		"no otsu":    {AutoInvert: true, Scale: 2, Median3x3: true, Pad: 8},
		"no median":  {AutoInvert: true, Scale: 2, Otsu: true, Pad: 8},
		"pad 12":     {AutoInvert: true, Scale: 2, Otsu: true, Median3x3: true, Pad: 12},
	}
	base := a.Hash()
	for name, s := range cases {
		if s.Hash() == base {
			t.Errorf("%s: recipe hash collides with the default", name)
		}
	}
	// Clamping is identity-preserving for the hash: "scale 9" means "scale 4" to
	// the pipeline, so it must not create a second cache namespace for the same
	// work.
	if (Steps{AutoInvert: true, Scale: 9, Otsu: true, Median3x3: true, Pad: 8}).Hash() !=
		(Steps{AutoInvert: true, Scale: 4, Otsu: true, Median3x3: true, Pad: 8}).Hash() {
		t.Error("an over-range scale hashed differently from the clamped recipe it runs as")
	}
	if len(a.Hash()) != 64 {
		t.Errorf("hash length %d, want a hex SHA-256", len(a.Hash()))
	}
}

func TestPreprocessEmptyInput(t *testing.T) {
	if got := Preprocess(nil, DefaultSteps()); !got.Bounds().Empty() {
		t.Errorf("Preprocess(nil) = %v, want empty", got.Bounds())
	}
	if got := Preprocess(image.NewGray(image.Rectangle{}), DefaultSteps()); !got.Bounds().Empty() {
		t.Errorf("Preprocess(empty) = %v, want empty", got.Bounds())
	}
	if ImageHash(nil) == "" {
		t.Error("ImageHash(nil) returned the empty string instead of a hash")
	}
}

// uniform and uniformRGBA build the same visible color in two models.
func uniform(w, h int, c color.NRGBA) *image.NRGBA {
	i := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i.SetNRGBA(x, y, c)
		}
	}
	return i
}

func uniformRGBA(w, h int, c color.NRGBA) *image.RGBA {
	i := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i.Set(x, y, c)
		}
	}
	return i
}

// wrapped hides the concrete type so the generic At()-based conversion path is
// exercised, not just the fast paths.
type wrapped struct{ image.Image }
