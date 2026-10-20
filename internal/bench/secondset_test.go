package bench

import (
	"encoding/json"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// meanLuma returns the mean grayscale level of the rect's pixels (0-255).
func meanLuma(img image.Image, x0, y0, x1, y1 int) float64 {
	b := img.Bounds()
	var sum, n int
	for y := y0; y <= y1 && y < b.Max.Y; y++ {
		for x := x0; x <= x1 && x < b.Max.X; x++ {
			if x < b.Min.X || y < b.Min.Y {
				continue
			}
			r, g, bl, _ := img.At(x, y).RGBA()
			sum += int((r + g + bl) / (3 << 8))
			n++
		}
	}
	if n == 0 {
		return -1
	}
	return float64(sum) / float64(n)
}

func renderOneSample(t *testing.T, ts *TruthSet, layout Layout, dir string) string {
	t.Helper()
	if _, err := exec.LookPath("magick"); err != nil {
		t.Skip("NOT RUN: ImageMagick (magick) is required to render bench frames")
	}
	one := *ts
	one.Samples = ts.Samples[:1]
	dirs, err := RenderSamples(&one, layout, dir)
	if err != nil {
		t.Fatalf("RenderSamples: %v", err)
	}
	if len(dirs) != 1 {
		t.Fatalf("RenderSamples returned %d dirs, want 1", len(dirs))
	}
	for _, d := range dirs {
		return d
	}
	t.Fatal("unreachable")
	return ""
}

func openFrame(t *testing.T, dir, file string) image.Image {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, file))
	if err != nil {
		t.Fatalf("open %s: %v", file, err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatalf("decode %s: %v", file, err)
	}
	return img
}

// TestRenderRespectsSetLayout is the failing-first proof for ADR-0079's core
// assumption: before the Layout data existed, the renderer hard-coded the
// first game's bottom box, so a set whose box sits upper-left could not be
// rendered or graded at all. This test renders the second layout and asserts
// the pixels say where the data says the box is: the upper-left rect is the
// dark panel (and the corner pixel is filled — square corners), while the
// first set's bottom box position shows only the scene's bottom band.
func TestRenderRespectsSetLayout(t *testing.T) {
	ts, prof, err := GenerateSecondGameSamples(0)
	if err != nil {
		t.Fatalf("GenerateSecondGameSamples: %v", err)
	}
	if _, err := exec.LookPath("magick"); err != nil {
		t.Skip("NOT RUN: ImageMagick (magick) is required to render bench frames")
	}
	work := t.TempDir()
	sampleDir := renderOneSample(t, ts, prof.Layout, work)
	// The first text frame of the instant schedule: box present, text on it.
	img := openFrame(t, sampleDir, "text.png")

	// 640×480 base scale (the sample's resolution): the second set's box is
	// (38,77)-(434,172); the first set's box rect is (45,346)-(594,455).
	upper := meanLuma(img, 60, 100, 400, 150) // well inside the second box
	lower := meanLuma(img, 80, 370, 560, 430) // inside the first set's box area
	if !(upper < lower) {
		t.Fatalf("layout not respected: upper box mean luma %v is not darker than the old bottom position %v", upper, lower)
	}
	// Square corners: the box corner pixel is filled dark; the first set's
	// rounded box leaves its corner pixels as the light scene.
	corner := meanLuma(img, 40, 79, 42, 81)
	if corner > 80 {
		t.Fatalf("corner pixel luma %v: the box is not square-cornered (rounded corners leave the corner light)", corner)
	}
}

// TestRenderDefaultLayoutUnchanged pins the compatibility rule: the zero
// layout renders the first set's bottom box, so pre-ADR-0079 frames stay
// byte-stable when the set's profile carries no layout.
func TestRenderDefaultLayoutUnchanged(t *testing.T) {
	ts, _, err := GenerateSecondGameSamples(0)
	if err != nil {
		t.Fatalf("GenerateSecondGameSamples: %v", err)
	}
	if _, err := exec.LookPath("magick"); err != nil {
		t.Skip("NOT RUN: ImageMagick (magick) is required to render bench frames")
	}
	work := t.TempDir()
	sampleDir := renderOneSample(t, ts, Layout{}, work)
	img := openFrame(t, sampleDir, "text.png")
	upper := meanLuma(img, 60, 100, 400, 150)
	lower := meanLuma(img, 80, 370, 560, 430)
	if !(lower < upper) {
		t.Fatalf("zero layout must render the first set's bottom box: lower %v, upper %v", lower, upper)
	}
}

// TestSecondSetShape checks the second set's structural claims (ADR-0079):
// the sample matrix, the instant-reveal schedule, the non-default layout, and
// the non-Latin line.
func TestSecondSetShape(t *testing.T) {
	ts, prof, err := GenerateSecondGameSamples(secondSetSeedBase)
	if err != nil {
		t.Fatalf("GenerateSecondGameSamples: %v", err)
	}
	if ts.Set != secondSetID {
		t.Fatalf("set id = %q, want %q", ts.Set, secondSetID)
	}
	if len(ts.Samples) != 26 { // 6 lines × 3 resolutions + 5 noisy Latin + 3 empty
		t.Fatalf("samples = %d, want 26", len(ts.Samples))
	}
	if len(prof.Messages) != 6 {
		t.Fatalf("messages = %d, want 6", len(prof.Messages))
	}
	holdout, tuning := 0, 0
	for _, s := range ts.Samples {
		switch s.Split {
		case SplitHoldout:
			holdout++
		case SplitTuning:
			tuning++
		}
		// Every sample uses the instant-reveal manifest.
		if len(s.Frames) != 10 || s.Frames[2].File != "text.png" || s.Frames[7].File != "text.png" ||
			s.Frames[8].File != "box.png" || s.Frames[9].File != "absent.png" {
			t.Fatalf("sample %s: frame schedule is not the instant reveal (frames %v)", s.ID, s.Frames)
		}
	}
	if holdout == 0 || tuning == 0 {
		t.Fatalf("split is degenerate: holdout=%d tuning=%d", holdout, tuning)
	}
	if (prof.Layout == Layout{}) || prof.Layout == defaultLayout {
		t.Fatalf("second set must carry a non-default layout, got %+v", prof.Layout)
	}
	if prof.Layout.Rounded {
		t.Fatal("second set's box must have square corners")
	}
	// The non-Latin line is in the script and in the samples, with its font.
	var foreign int
	for _, m := range prof.Messages {
		if m.ID == "bw-05" && !strings.Contains(m.Text, "Вода") {
			t.Fatalf("bw-05 text = %q: the non-Latin line is missing", m.Text)
		}
	}
	for _, s := range ts.Samples {
		if s.MessageID == nil {
			continue
		}
		if *s.MessageID == "bw-05" {
			foreign++
			if s.Variant.Font != secondForeignFont {
				t.Fatalf("bw-05 sample %s font = %q, want %q", s.ID, s.Variant.Font, secondForeignFont)
			}
		}
	}
	if foreign != 3 { // the non-Latin line: 3 resolutions, no noisy variant
		t.Fatalf("foreign-line samples = %d, want 3 (one per resolution)", foreign)
	}
}

// TestSecondSetCommittedMatchesGenerator pins the committed set the same way
// the first set's determinism test pins its: the on-disk truth.json and
// profile.json must be exactly what the generator writes.
func TestSecondSetCommittedMatchesGenerator(t *testing.T) {
	setDir := filepath.Join("..", "..", "testdata", "bench", secondSetID)
	if _, err := os.Stat(filepath.Join(setDir, "truth.json")); err != nil {
		t.Fatalf("committed second set missing (run the generator, see ADR-0079): %v", err)
	}
	committed, err := LoadTruth(setDir)
	if err != nil {
		t.Fatalf("LoadTruth(%s): %v", setDir, err)
	}
	committedProf, err := LoadScriptProfile(setDir)
	if err != nil {
		t.Fatalf("LoadScriptProfile(%s): %v", setDir, err)
	}
	got, gotProf, err := GenerateSecondGameSamples(committed.SeedBase)
	if err != nil {
		t.Fatalf("GenerateSecondGameSamples: %v", err)
	}
	if len(committed.Samples) != len(got.Samples) {
		t.Fatalf("committed samples = %d, generator = %d", len(committed.Samples), len(got.Samples))
	}
	for i := range committed.Samples {
		if !reflect.DeepEqual(committed.Samples[i], got.Samples[i]) {
			t.Fatalf("sample %d differs from the generator:\ncommitted: %+v\ngenerator: %+v", i, committed.Samples[i], got.Samples[i])
		}
	}
	if committedProf.ID != gotProf.ID || committedProf.Layout != gotProf.Layout ||
		len(committedProf.Regions) != len(gotProf.Regions) || committedProf.Regions[0] != gotProf.Regions[0] ||
		len(committedProf.Messages) != len(gotProf.Messages) {
		t.Fatalf("committed profile differs from the generator:\ncommitted: %+v\ngenerator: %+v", committedProf, gotProf)
	}
	for i := range committedProf.Messages {
		if committedProf.Messages[i] != gotProf.Messages[i] {
			t.Fatalf("profile message %d differs: %+v vs %+v", i, committedProf.Messages[i], gotProf.Messages[i])
		}
	}
}

// TestLayoutValidation: a complete in-frame layout passes, a partial or
// out-of-frame layout is a profile error (the renderer derives pixel rects
// from these fractions, so a bad layout is a corrupt profile, not a
// mis-rendered frame).
func TestLayoutValidation(t *testing.T) {
	setDir := filepath.Join("..", "..", "testdata", "bench", "bench-synthetic-001")
	raw, err := os.ReadFile(filepath.Join(setDir, "profile.json"))
	if err != nil {
		t.Fatalf("read set 001 profile: %v", err)
	}
	cases := []struct {
		name    string
		mutate  func(p *ScriptProfile)
		wantErr string
	}{
		{"valid zero layout", func(p *ScriptProfile) { p.Layout = Layout{} }, ""},
		{"valid explicit layout", func(p *ScriptProfile) { p.Layout = secondSetLayout }, ""},
		{"partial layout", func(p *ScriptProfile) { p.Layout = Layout{BoxX: 0.06, BoxW: 0.5} }, "layout"},
		{"out-of-frame layout", func(p *ScriptProfile) { p.Layout = Layout{BoxX: 0.9, BoxY: 0.1, BoxW: 0.2, BoxH: 0.2} }, "layout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var p ScriptProfile
			if err := json.Unmarshal(raw, &p); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			tc.mutate(&p)
			dir := t.TempDir()
			pb, merr := json.Marshal(&p)
			if merr != nil {
				t.Fatalf("marshal: %v", merr)
			}
			if err := os.WriteFile(filepath.Join(dir, "profile.json"), pb, 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := LoadScriptProfile(dir)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("LoadScriptProfile = %v, want ok", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("LoadScriptProfile = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}
