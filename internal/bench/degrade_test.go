package bench

// degrade_test.go is the unit half of the degraded-render calibration data
// (class b′, ADR-0058 amendment 1): the variants' determinism (the same
// fixture pixels must always yield the same degraded pixels — re-benches
// re-derive the same fit data), their non-identity (a variant that does
// nothing adds no fit material), and the fit sources' shape (the b′ label,
// the variant's frame files, the untouched timestamps).

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/dbirks/game-narrator/internal/dolphin"
)

// subtitleLikeFrame is a 640x480 frame with a bright text-like band in the
// bottom bar — enough structure for the variants to act on (flat frames
// survive every transform unchanged and would not prove the pixels moved).
func subtitleLikeFrame() image.Image {
	img := image.NewNRGBA(image.Rect(0, 0, 640, 480))
	for y := 0; y < 480; y++ {
		for x := 0; x < 640; x++ {
			img.Set(x, y, color.NRGBA{R: 20, G: 25, B: 35, A: 255})
		}
	}
	// Two "lines" of bright glyph-like strokes in the subtitle band.
	for _, y0 := range []int{375, 395} {
		for x := 90; x < 550; x += 7 {
			for y := y0; y < y0+14; y += 3 {
				img.Set(x, y, color.NRGBA{R: 235, G: 230, B: 210, A: 255})
			}
		}
	}
	return img
}

// pixelEqual compares one pixel of each image.
func pixelEqual(a, b image.Image, x, y int) bool {
	r1, g1, b1, a1 := a.At(x, y).RGBA()
	r2, g2, b2, a2 := b.At(x, y).RGBA()
	return r1 == r2 && g1 == g2 && b1 == b2 && a1 == a2
}

// imagesEqual compares two same-bounds images pixel by pixel.
func imagesEqual(a, b image.Image) bool {
	ba, bb := a.Bounds(), b.Bounds()
	if ba != bb {
		return false
	}
	for y := ba.Min.Y; y < ba.Max.Y; y++ {
		for x := ba.Min.X; x < ba.Max.X; x++ {
			if !pixelEqual(a, b, x, y) {
				return false
			}
		}
	}
	return true
}

// countPixelDiff counts the pixels the two images disagree on.
func countPixelDiff(a, b image.Image) int {
	n := 0
	ba := a.Bounds()
	for y := ba.Min.Y; y < ba.Max.Y; y++ {
		for x := ba.Min.X; x < ba.Max.X; x++ {
			if !pixelEqual(a, b, x, y) {
				n++
			}
		}
	}
	return n
}

// TestDegradedVariantsDeterministic verifies the re-derivability the b′
// class claims: every variant is a pure function of its input (two runs on
// the same frame agree pixel for pixel), and every variant actually moves
// pixels (an identity variant would add no fit material at all).
func TestDegradedVariantsDeterministic(t *testing.T) {
	src := subtitleLikeFrame()
	for _, v := range DegradedVariants() {
		a := v.fn(src)
		b := v.fn(src)
		if !imagesEqual(a, b) {
			t.Errorf("variant %s: two runs on the same frame disagree (the degradation is not a pure function — re-benches would re-derive different fit data)", v.Name)
		}
		if n := countPixelDiff(src, a); n == 0 {
			t.Errorf("variant %s: the output is identical to the input (the variant degrades nothing)", v.Name)
		}
		// The degradation must not move the frame's bounds (the pipeline
		// crops the region from a same-size frame).
		if a.Bounds() != src.Bounds() {
			t.Errorf("variant %s: bounds %v -> %v (the pipeline expects same-size frames)", v.Name, src.Bounds(), a.Bounds())
		}
	}
}

// TestDegradedFramesDir verifies the fixture-side half: the variant's
// transform of the fixture's frames lands in a temp dir under the variant's
// names, decodes, and the name map covers exactly the fixture's frames.
func TestDegradedFramesDir(t *testing.T) {
	fxDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(fxDir, "frames"), 0o755); err != nil {
		t.Fatalf("frames dir: %v", err)
	}
	files := []string{"frame-00001.png", "frame-00002.png"}
	src := subtitleLikeFrame()
	for _, f := range files {
		fh, err := os.Create(filepath.Join(fxDir, "frames", f))
		if err != nil {
			t.Fatalf("create %s: %v", f, err)
		}
		if err := png.Encode(fh, src); err != nil {
			t.Fatalf("encode %s: %v", f, err)
		}
		fh.Close()
	}

	v := DegradedVariants()[0]
	dir, names, err := degradedFramesDir(fxDir, v, files)
	if err != nil {
		t.Fatalf("degradedFramesDir: %v", err)
	}
	defer os.RemoveAll(dir)
	for _, f := range files {
		name, ok := names[f]
		if !ok {
			t.Fatalf("name map has no entry for %s: %+v", f, names)
		}
		if filepath.Ext(name) != ".jpg" {
			t.Errorf("degraded frame %s: %q, want the temp JPEG name (the replay source decodes by content)", f, name)
		}
		// The degraded file must decode to the same bounds.
		img, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("open %s: %v", name, err)
		}
		out, _, err := image.Decode(img)
		img.Close()
		if err != nil {
			t.Fatalf("decode %s: %v", name, err)
		}
		if out.Bounds() != src.Bounds() {
			t.Errorf("degraded %s: bounds %v, want %v", name, out.Bounds(), src.Bounds())
		}
	}
}

// TestDegradedFitSourcesShape verifies the fit-source half: one source per
// variant, each labelled class b′ (never clean), replaying the variant's
// frame files over the untouched capture-clock timestamps.
func TestDegradedFitSourcesShape(t *testing.T) {
	fxDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(fxDir, "frames"), 0o755); err != nil {
		t.Fatalf("frames dir: %v", err)
	}
	src := subtitleLikeFrame()
	fh, err := os.Create(filepath.Join(fxDir, "frames", "frame-00001.png"))
	if err != nil {
		t.Fatalf("create frame: %v", err)
	}
	if err := png.Encode(fh, src); err != nil {
		t.Fatalf("encode frame: %v", err)
	}
	fh.Close()

	fx := &CapturedFixture{
		ID: "test-fixture", Dir: fxDir,
		Manifest: dolphin.FixtureManifest{
			ID:     "test-fixture",
			Frames: []dolphin.FixtureFrame{{File: "frame-00001.png"}},
		},
	}
	sample := Sample{
		ID:       "cx0001",
		Frames:   []Frame{{File: "frame-00001.png", TsNS: 1_000_000_000}},
		Scene:    "test-fixture",
		TextKind: "dialogue",
		Split:    SplitTuning,
	}
	variants := DegradedVariants()
	srcs, err := degradedFitSources(fx, []Sample{sample}, nil, nil, nil)
	if err != nil {
		t.Fatalf("degradedFitSources: %v", err)
	}
	defer func() {
		for i := range srcs {
			srcs[i].cleanup()
		}
	}()
	if len(srcs) != len(variants) {
		t.Fatalf("sources = %d, want one per variant (%d)", len(srcs), len(variants))
	}
	for i, s := range srcs {
		if s.class != ClassDegraded {
			t.Errorf("source %d: class %q, want %q (degraded data is never reported as clean capture)", i, s.class, ClassDegraded)
		}
		if s.name == "" || !containsAll(s.name, fx.ID, variants[i].Name, ClassDegraded) {
			t.Errorf("source %d: name %q, want the fixture id, the variant and the b′ label", i, s.name)
		}
		// ADR-0064: the variant's frames are prepared and freed one variant at
		// a time by the fit loop; the source must carry both hooks.
		if s.prepare == nil || s.cleanup == nil {
			t.Fatalf("source %d: prepare/cleanup hooks missing", i)
		}
		if err := s.prepare(); err != nil {
			t.Fatalf("source %d: prepare: %v", i, err)
		}
		dir, tsPath, ok := s.frames(sample.ID)
		if !ok || dir == "" || tsPath == "" {
			t.Fatalf("source %d: frames(%s) not resolved: %q %q", i, sample.ID, dir, tsPath)
		}
		raw, err := os.ReadFile(tsPath)
		if err != nil {
			t.Fatalf("source %d: read ts manifest: %v", i, err)
		}
		if !containsAll(string(raw), variants[i].Name, "1000000000") {
			t.Errorf("source %d: ts manifest %s, want the variant's frame name and the untouched capture timestamp", i, string(raw))
		}
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
