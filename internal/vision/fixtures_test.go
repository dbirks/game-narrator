package vision

import (
	"context"
	"encoding/json"
	"image"
	_ "image/png" // decode the PNG fixtures
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/vision/metrics"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

// This file is the milestone's actual claim: the crop + preprocess + Tesseract
// path reads the committed synthetic textbox fixtures (docs/spec/11 "Fixture
// origins" item 3) to the accuracy docs/spec/12 asks CER for. It runs the real
// binary — nothing here is mocked, and if Tesseract is absent the test reports
// NOT RUN rather than passing quietly (AGENTS.md "no simulation theater").

// fixtureDir is the committed corpus. Relative rather than go:embed: these PNGs
// are diagnostic artifacts a human should be able to open, and embedding would
// duplicate every byte into the release binary (docs/spec/15 image size).
const fixtureDir = "../../testdata/synthetic/textbox"

// Truth is one row of truth.json. Unknown keys are ignored on purpose — the
// generator may annotate further without breaking an older runtime — but every
// key this corpus depends on is asserted present below.
type Truth struct {
	File        string `json:"file"`
	Text        string `json:"text"`
	Rect        Rect   `json:"rect"`
	Box         Rect   `json:"box"`
	Size        Coord  `json:"size"`
	Letterboxed bool   `json:"letterboxed"`
	Variant     string `json:"variant"`
	Font        string `json:"font"`
	Origin      string `json:"origin"`
}

// Coord is the pixel size of the generated capture frame.
type Coord struct {
	W int `json:"w"`
	H int `json:"h"`
}

// Thresholds come from the milestone's acceptance criteria, which narrow the
// docs/spec/12 "provisional objectives" to what a local engine on clean synthetic
// frames can be held to today. The noisy variant is excluded from the
// per-fixture bound deliberately: docs/spec/12 requires coverage of
// "analog-converted noisy input", and tuning a ceiling on the hardest case would
// measure the threshold rather than the pipeline.
const (
	meanCERLimit    = 0.05
	perFixtureLimit = 0.10
	emptyConfLimit  = 0.6
	// fixtureSizeLimit keeps the committed corpus honest about its footprint.
	fixtureSizeLimit = 60 << 10
)

func loadTruth(t testing.TB) []Truth {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixtureDir, "truth.json"))
	if err != nil {
		t.Fatalf("read truth.json: %v", err)
	}
	var out []Truth
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode truth.json: %v", err)
	}
	if len(out) < 12 {
		t.Fatalf("truth.json has %d entries; the corpus is 12-16 frames", len(out))
	}
	return out
}

func decodePNG(t testing.TB, path string) image.Image {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	img, format, err := image.Decode(f)
	if err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	if format != "png" {
		t.Fatalf("decode %s: format %q, want png", path, format)
	}
	return img
}

// TestFixtureCorpusBudget checks the corpus itself: provenance labels, size
// budget, and geometry that still matches the committed PNGs. It needs no OCR, so
// it runs everywhere, and it fails on a regenerated frame whose truth row went
// stale — before an OCR test could blame the engine.
func TestFixtureCorpusBudget(t *testing.T) {
	// Invariant both ways: every truth row names a file that exists (checked
	// below), and every PNG on disk has a truth row. An unlabelled frame in the
	// corpus is a fixture nobody scored.
	truth := loadTruth(t)
	named := map[string]bool{}
	for _, f := range truth {
		named[f.File] = true
	}
	pngs, err := filepath.Glob(filepath.Join(fixtureDir, "*.png"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	for _, path := range pngs {
		if base := filepath.Base(path); !named[base] {
			t.Errorf("%s is not described in truth.json (stale fixture? run the generator)", base)
		}
	}

	fonts := map[string]bool{}
	variants := map[string]int{}
	for _, f := range truth {
		path := filepath.Join(fixtureDir, f.File)
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("%s: %v", f.File, err)
			continue
		}
		if info.Size() > fixtureSizeLimit {
			t.Errorf("%s: %d bytes exceeds the %d byte budget", f.File, info.Size(), fixtureSizeLimit)
		}
		if f.Origin != "generated_synthetic" {
			t.Errorf("%s: origin %q, want generated_synthetic (docs/spec/12 provenance labels)", f.File, f.Origin)
		}
		if f.Variant == "" {
			t.Errorf("%s: no variant label", f.File)
		}
		variants[f.Variant]++
		cfg := mustConfig(t, path)
		if cfg.Width != f.Size.W || cfg.Height != f.Size.H {
			t.Errorf("%s: PNG is %dx%d, truth.json says %dx%d", f.File, cfg.Width, cfg.Height, f.Size.W, f.Size.H)
		}
		if f.Rect.X <= 0 || f.Rect.Y <= 0 || f.Rect.W <= 0 || f.Rect.H <= 0 ||
			f.Rect.X+f.Rect.W > 1 || f.Rect.Y+f.Rect.H > 1 {
			t.Errorf("%s: rect %+v is not a region inside the frame", f.File, f.Rect)
		}
		if f.Letterboxed && cfg.Width <= cfg.Height*4/3 {
			t.Errorf("%s: labelled letterboxed but the frame is %dx%d", f.File, cfg.Width, cfg.Height)
		}
		// The region must sit inside the textbox it was derived from, and inset
		// from it: `rect` is `box` with the decorative border masked off.
		if f.Box.W <= 0 || f.Rect.X < f.Box.X || f.Rect.Y < f.Box.Y ||
			f.Rect.X+f.Rect.W > f.Box.X+f.Box.W+1e-9 || f.Rect.Y+f.Rect.H > f.Box.Y+f.Box.H+1e-9 {
			t.Errorf("%s: region %+v is not inset inside box %+v", f.File, f.Rect, f.Box)
		}
		if f.Text != "" {
			if f.Font == "" {
				t.Errorf("%s: text fixture with no font label", f.File)
			} else {
				fonts[f.Font] = true
			}
		}
	}
	// The corpus is only evidence of portability if it actually spans the fonts
	// and layouts docs/spec/12 lists; a corpus that decayed into one font would
	// pass every CER assertion while proving nothing.
	if len(fonts) < 3 {
		t.Errorf("corpus covers %d fonts (%v), want at least 3", len(fonts), keys(fonts))
	}
	for _, want := range []string{"plain", "two_line", "three_line", "punctuation", "noisy_blur", "light_box", "non_ascii", "empty_box"} {
		if variants[want] == 0 {
			t.Errorf("corpus has no %q variant", want)
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func mustConfig(t *testing.T, path string) image.Config {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		t.Fatalf("config %s: %v", path, err)
	}
	return cfg
}

// TestSyntheticFixturesOCR is the real OCR gate: every committed fixture goes
// through the same crop → preprocess → Tesseract path the runtime will use, and
// the result is scored with CER against the corpus truth.
func TestSyntheticFixturesOCR(t *testing.T) {
	if testing.Short() {
		t.Skip("NOT RUN: the real-Tesseract fixture gate is skipped under -short; run without -short for the docs/spec/12 OCR evidence")
	}
	if _, err := exec.LookPath("tesseract"); err != nil {
		t.Skipf("NOT RUN: tesseract not installed (%v); see scripts/bootstrap-arch.sh and docs/adr/0012", err)
	}
	// The 15 s budget is the host-load margin measured on the dev host, scaled
	// for a race-instrumented binary on a 2-core runner (ocrTimeoutScale,
	// race_on_test.go); the per-call context below it is scaled the same way.
	eng := ocr.Tesseract{PSM: ocr.DefaultPSM, Timeout: 15 * time.Second * ocrTimeoutScale}
	steps := DefaultSteps()
	t.Logf("preprocess recipe %s… (invert=%v scale=%d filter=%v otsu=%v median=%v pad=%d)",
		steps.Hash()[:12], steps.AutoInvert, steps.Scale, steps.Filter, steps.Otsu, steps.Median3x3, steps.Pad)

	var sum, measured int
	var worst, worstVariant string
	var worstCER float64
	t.Logf("%-24s %-12s %6s %6s %7s  %s", "fixture", "variant", "CER", "conf", "ms", "hypothesis")
	for _, f := range loadTruth(t) {
		img := decodePNG(t, filepath.Join(fixtureDir, f.File))
		crop := CropNormalized(img, f.Rect, CropOptions{Letterbox: LetterboxDetect})
		if crop.Bounds().Empty() {
			t.Errorf("%s: crop of rect %+v in a %v frame is empty", f.File, f.Rect, img.Bounds())
			continue
		}
		pre := Preprocess(crop, steps)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second*ocrTimeoutScale)
		res, err := eng.Recognize(ctx, pre)
		cancel()
		if err != nil {
			t.Errorf("%s: recognize: %v", f.File, err)
			continue
		}

		cer := metrics.CER(f.Text, res.Text)
		hyp := strings.ReplaceAll(res.Text, "\n", " / ")
		if len(hyp) > 56 {
			hyp = hyp[:56] + "…"
		}
		t.Logf("%-24s %-12s %6.3f %6.2f %7.1f  %q", f.File, f.Variant, cer, res.MeanConf,
			float64(res.Duration.Microseconds())/1000, hyp)

		if f.Variant == "empty_box" {
			// docs/spec/12 "Game running without text": an empty box must not
			// produce a confident reading, or every fade between lines would be
			// narrated and the ≥99 % speech-precision objective would be gone.
			for _, w := range res.Words {
				if w.Conf > emptyConfLimit {
					t.Errorf("%s: empty box yielded confident word %q at conf %.2f (limit %.2f)",
						f.File, w.Text, w.Conf, emptyConfLimit)
				}
			}
			continue
		}

		sum += int(cer * 10000)
		measured++
		if cer > worstCER {
			worst, worstVariant, worstCER = f.File, f.Variant, cer
		}
		if f.Variant != "noisy_blur" && cer > perFixtureLimit {
			t.Errorf("%s (%s): CER %.3f exceeds the per-fixture limit %.3f — truth %q, hypothesis %q",
				f.File, f.Variant, cer, perFixtureLimit, f.Text, res.Text)
		}
	}
	if measured == 0 {
		t.Fatal("no text fixtures were measured")
	}
	mean := float64(sum) / float64(measured) / 10000
	t.Logf("mean CER over %d text fixtures: %.4f (limit %.2f); worst: %s (%s) at %.3f",
		measured, mean, meanCERLimit, worst, worstVariant, worstCER)
	if mean > meanCERLimit {
		t.Errorf("mean CER %.4f exceeds the %.2f limit", mean, meanCERLimit)
	}
}

// BenchmarkPreprocessAndOCR is informational (docs/spec/05 "Timings"): it prices
// one crop + preprocess + subprocess OCR, the per-region cost the frame loop pays
// at 2–5 Hz. Not a release gate — the number that matters is measured on UNO Q
// hardware and recorded in the evidence bundle (AGENTS.md "manifest").
func BenchmarkPreprocessAndOCR(b *testing.B) {
	if _, err := exec.LookPath("tesseract"); err != nil {
		b.Skipf("NOT RUN: tesseract not installed (%v)", err)
	}
	all := loadTruth(b)
	f := all[0]
	img := decodePNG(b, filepath.Join(fixtureDir, f.File))
	eng := ocr.Tesseract{Timeout: 15 * time.Second * ocrTimeoutScale}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pre := Preprocess(CropNormalized(img, f.Rect, CropOptions{Letterbox: LetterboxDetect}), DefaultSteps())
		if _, err := eng.Recognize(ctx, pre); err != nil {
			b.Fatal(err)
		}
	}
}
