package bench

// degrade.go is the degraded-render calibration data of ADR-0058 amendment
// 1: the calibration fit needs labelled frames where the matcher's top-1
// genuinely errs (the synthetic tuning set is saturated at 100% top-1, and
// the clean captured frames err on nothing), so the dense fixture's frames
// are re-degraded at bench time through deterministic synthetic
// transformations — JPEG re-encode, downscale+upscale, seeded Gaussian
// noise, a fixed crop offset.
//
// Provenance (AGENTS.md rule 3, "No simulation theater"): these are class
// (b′) — captured frames + synthetic degradation. They are NEVER reported
// as clean captures: every degraded source carries the b′ label in the
// calibration's per-source breakdown and in any report, and the degraded
// frames are generated in memory at bench time and written to a temp dir
// only (never stored, never committed, never written into the fixture —
// the fixture stays the clean recording, AGENTS.md rule 5).
//
// The transformations are deterministic functions of the fixture's pixels
// (fixed quality, fixed factor, fixed RNG seed, fixed offset), so a
// re-bench over the same fixture reproduces the same degraded frames and
// the same fit data: the calibration numbers are re-derivable from the
// fixture alone.

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"math/rand"
	"os"
	"path/filepath"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/matcher"
)

// ClassDegraded is the AGENTS.md rule-3 provenance class of the degraded
// sources: captured frames + synthetic degradation. It is reported
// separately from class (b) clean captures and never labelled as one.
const ClassDegraded = "b′"

// DegradedVariant is one deterministic frame transformation.
type DegradedVariant struct {
	Name string // the label carried in the calibration's per-source rows
	fn   func(image.Image) image.Image
}

// DegradedVariants is the transformation set. The set targets the failure
// modes the fit's raw material needs: OCR-degraded readings (jpeg, noise),
// glyph-shape distortion (downscale+upscale blurs strokes, offset shifts
// the line out of the crop's center) — the "blur, shadow, partial reveals"
// the original ADR-0058 named as the unblock plan.
func DegradedVariants() []DegradedVariant {
	return []DegradedVariant{
		{Name: "jpeg-q45", fn: jpegReencode(45)},
		{Name: "downscale-2x", fn: downscaleUpscale(2)},
		{Name: "noise-s7", fn: gaussianNoise(7, 18.0)},
		{Name: "crop-8px", fn: cropOffset(8, 8)},
		// The strong geometric pair (ADR-0058 amendment 1, measured 2026-10-18:
		// the four mild variants above left the OCR recipe's readings intact on
		// 100% of the dense fixture's line frames — 2x-scale + Otsu + 3x3-median
		// re-separates the subtitle glyphs from codec noise, blur and noise —
		// while 3x/4x downscale destroy the glyph strokes: 5/12 and 0/12 line
		// frames still rankable at the truth, the rest retrieval misses. They are
		// the fit's raw material for the degraded frames, and their miss counts
		// are part of the NOT FITTED evidence, so they stay in the set.
		{Name: "downscale-3x", fn: downscaleUpscale(3)},
		{Name: "downscale-4x", fn: downscaleUpscale(4)},
	}
}

// jpegReencode returns the JPEG round-trip at the given quality: the
// blocky artefacts and edge ringing of a low-quality encode on top of the
// captured pixels. Deterministic for identical input bytes (the stdlib
// encoder is stateless).
func jpegReencode(quality int) func(image.Image) image.Image {
	return func(img image.Image) image.Image {
		out, err := jpegRoundTrip(img, quality)
		if err != nil {
			return img // a re-encode failure is a no-op, not an invented frame
		}
		return out
	}
}

// jpegRoundTrip re-encodes img at the quality and decodes it back (the
// pixels, not the file, are what the pipeline sees).
func jpegRoundTrip(img image.Image, quality int) (image.Image, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return jpeg.Decode(&buf)
}

// downscaleUpscale shrinks by the factor (nearest-neighbour) and back:
// glyph strokes lose edges and re-grow blocky — a blur without a kernel.
func downscaleUpscale(factor int) func(image.Image) image.Image {
	return func(img image.Image) image.Image {
		b := img.Bounds()
		w, h := b.Dx()/factor, b.Dy()/factor
		if w < 1 || h < 1 {
			return img
		}
		small := image.NewNRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				small.Set(x, y, img.At(x*factor+b.Min.X, y*factor+b.Min.Y))
			}
		}
		big := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
		for y := 0; y < b.Dy(); y++ {
			for x := 0; x < b.Dx(); x++ {
				big.Set(x, y, small.At(x/factor, y/factor))
			}
		}
		return big
	}
}

// gaussianNoise adds per-pixel zero-mean Gaussian noise (sigma on luma,
// fixed seed) — the analog-noise stand-in for capture jitter. The RNG is a
// private source per frame, seeded identically for every frame, so the
// degradation is a pure function of the fixture (re-benches agree).
func gaussianNoise(seed int64, sigma float64) func(image.Image) image.Image {
	return func(img image.Image) image.Image {
		rng := rand.New(rand.NewSource(seed))
		out := image.NewNRGBA(img.Bounds())
		for y := out.Bounds().Min.Y; y < out.Bounds().Max.Y; y++ {
			for x := out.Bounds().Min.X; x < out.Bounds().Max.X; x++ {
				r, g, b, a := img.At(x, y).RGBA()
				n := func() int { return int(rng.NormFloat64() * sigma) }
				out.Set(x, y, color.NRGBA{
					R: clampUint8(int(r>>8) + n()),
					G: clampUint8(int(g>>8) + n()),
					B: clampUint8(int(b>>8) + n()),
					A: uint8(a >> 8),
				})
			}
		}
		return out
	}
}

// cropOffset shifts the frame by (dx, dy) pixels with a black fill — the
// line moves off the crop's centre, the way a slightly-off window position
// or a resolution change does in real capture.
func cropOffset(dx, dy int) func(image.Image) image.Image {
	return func(img image.Image) image.Image {
		b := img.Bounds()
		out := image.NewNRGBA(b)
		draw.Draw(out, b, image.NewNRGBA(b), b.Min, draw.Over) // black
		dst := b
		src := image.Rect(b.Min.X-dx, b.Min.Y-dy, b.Max.X-dx, b.Max.Y-dy)
		draw.Draw(out, dst, img, src.Min, draw.Over)
		return out
	}
}

// clampUint8 clamps the int arithmetic of the noise addition into the byte
// range (the noise is signed: uint8 addition would wrap, not clamp).
func clampUint8(v int) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}

// degradedFramesDir writes the variant's transformation of the fixture's
// frames into a temp dir (JPEG, quality 95 — the container, not the
// degradation) and returns the dir plus the original-name ->
// degraded-name map. The fixture's frames are decoded read-only and never
// touched; the temp dir is the caller's to remove (defer). The per-frame
// write is one decode + one transform + one encode, so a dense fixture at
// four variants is a few thousand small JPEGs — bounded and temporary.
func degradedFramesDir(fixtureDir string, v DegradedVariant, frameFiles []string) (string, map[string]string, error) {
	dir, err := os.MkdirTemp("", "bench-degraded-")
	if err != nil {
		return "", nil, err
	}
	names := make(map[string]string, len(frameFiles))
	for _, f := range frameFiles {
		img, err := os.Open(filepath.Join(fixtureDir, "frames", f))
		if err != nil {
			os.RemoveAll(dir)
			return "", nil, fmt.Errorf("bench: degraded %s: open frame %s: %v", v.Name, f, err)
		}
		src, _, err := image.Decode(img)
		img.Close()
		if err != nil {
			os.RemoveAll(dir)
			return "", nil, fmt.Errorf("bench: degraded %s: decode frame %s: %v", v.Name, f, err)
		}
		out := v.fn(src)
		name := "d-" + v.Name + "-" + stripExt(f) + ".jpg"
		fh, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			os.RemoveAll(dir)
			return "", nil, fmt.Errorf("bench: degraded %s: create %s: %v", v.Name, name, err)
		}
		if err := jpeg.Encode(fh, out, &jpeg.Options{Quality: 95}); err != nil {
			fh.Close()
			os.RemoveAll(dir)
			return "", nil, fmt.Errorf("bench: degraded %s: encode %s: %v", v.Name, name, err)
		}
		fh.Close()
		names[f] = name
	}
	return dir, names, nil
}

func stripExt(f string) string {
	for i := len(f) - 1; i >= 0; i-- {
		if f[i] == '.' {
			return f[:i]
		}
	}
	return f
}

// degradedFitSources prepares one class (b′) fit source per degradation
// variant: the fixture's samples replayed over the variant's degraded frames
// (generated at bench time in a temp dir, never stored). Each variant is its
// OWN fit source with its own name and the b′ class, so the calibration's
// per-source breakdown — and any refusal's counts — say exactly which
// degradation contributed what (AGENTS.md rule 3: these are never reported
// as clean captures).
//
// The variants' frames are prepared lazily, one at a time by the fit loop
// (each source's prepare renders its frame dir right before the source runs,
// its cleanup frees it right after — ADR-0064): the disk peak is one
// variant's frames, never all variants' (the six dense-fixture variants,
// ~1085 JPEGs each, prepared up front did not fit the shared /tmp). A
// degraded-frame render failure is NOT RUN for the whole fit (the same
// discipline as the synthetic render failure): prepare returns the error and
// the fit refuses.
func degradedFitSources(fx *CapturedFixture, samples []Sample, msgs []games.Message, idx *matcher.Index) ([]fitSource, error) {
	// The union of the samples' frame files (the segments partition the
	// fixture's rows, so this is the fixture's whole frame list in its
	// manifest order).
	seen := map[string]bool{}
	files := make([]string, 0, len(fx.Manifest.Frames))
	for _, s := range samples {
		for _, f := range s.Frames {
			if !seen[f.File] {
				seen[f.File] = true
				files = append(files, f.File)
			}
		}
	}
	prof := CapturedProfile(fx, msgs)
	regions := prof.BuildRegions()
	var srcs []fitSource
	for _, v := range DegradedVariants() {
		var (
			dir   string
			names map[string]string
			work  string
		)
		srcs = append(srcs, fitSource{
			name:    fmt.Sprintf("%s (captured + degraded %s — class b′, %d samples)", fx.ID, v.Name, len(samples)),
			class:   ClassDegraded,
			samples: samples,
			prepare: func() error {
				d, n, err := degradedFramesDir(fx.Dir, v, files)
				if err != nil {
					return err
				}
				w, err := os.MkdirTemp("", "bench-calibrate-degraded-ts-")
				if err != nil {
					os.RemoveAll(d)
					return fmt.Errorf("bench: degraded %s: temp dir: %v", v.Name, err)
				}
				dir, names, work = d, n, w
				return nil
			},
			cleanup: func() {
				os.RemoveAll(dir)
				os.RemoveAll(work)
			},
			frames: func(id string) (string, string, bool) {
				for _, s := range samples {
					if s.ID != id {
						continue
					}
					// Same frames, same timestamps — the degradation changes
					// pixels, never the recording's clock — renamed to the
					// variant's files.
					frames := make([]Frame, len(s.Frames))
					for i, f := range s.Frames {
						frames[i] = f
						if n, ok := names[f.File]; ok {
							frames[i].File = n
						}
					}
					tsPath := filepath.Join(work, "ts-"+v.Name+"-"+id+".json")
					if err := writeManifest(tsPath, frames); err != nil {
						return "", "", false
					}
					return dir, tsPath, true
				}
				return "", "", false
			},
			idx:     idx,
			regions: regions,
			profile: prof,
		})
	}
	return srcs, nil
}
