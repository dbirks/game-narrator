package bench

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// The rendering constants mirror scripts/fixtures/gen-synthetic-textbox.sh so
// the bench frames live in the same visual world as the M2 fixtures (docs/spec/11
// "Fixture origins"): same box geometry fractions, same dark-panel style, same
// quantisation. Pixel values scale with s = H/480 so the three capture
// resolutions render the same proportionate textbox.
const (
	boxX, boxY, boxW, boxH = 0.07, 0.72, 0.86, 0.23
	borderInset            = 17.0 // px at 480p, scales with s
	textInsetX, textInsetY = 22.0, 18.0
	maxPoint               = 40.0
)

// Resolutions is the capture-resolution matrix (docs/spec/12 "capture
// resolutions"): 4:3 direct, 3:2 and 16:9.
var Resolutions = []struct{ W, H int }{{640, 480}, {720, 480}, {1280, 720}}

// Fonts is the font-treatment matrix (docs/spec/12 "multiple font treatments").
// Paths come from fontconfig at render time; no font file is ever committed.
var Fonts = []string{"DejaVu Sans", "Noto Serif", "Liberation Mono"}

// NoiseParams are the analog-noisy variant's parameters (docs/spec/12 "analog-
// converted noisy input"): gaussian blur + noise, a slight chroma shift, and
// scanline darkening every N rows.
var NoiseParams = struct {
	Blur, Attenuate, ChromaR, ChromaB, SecondBlur float64
	ScanlineN                                     int
	ScanlineA                                     string
}{0.6, 0.4, 1.05, 0.96, 1.2, 4, "rgba(0,0,0,0.15)"}

// frameStepNS and nFrames: a ten-frame sample on a 10 Hz capture clock, the
// cadence of the M2 replay clip. The box appears at frame 3, the reading is
// stable over frames 3-8, and frame 9 is an advance (empty box) so the
// stale/duplicate speech paths are exercised.
const (
	frameStepNS = 100_000_000
	nFrames     = 10
)

// SampleSeed derives the deterministic per-sample seed: a function of the set's
// seed base and the sample id only, so a re-render of the same sample is
// byte-identical and never depends on run order.
func SampleSeed(seedBase uint64, id string) uint32 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(fmt.Sprintf("%d:%s", seedBase, id)))
	return uint32(h.Sum64())
}

// sourceProfile is the M2 synthetic profile's JSON shape (the lines the bench
// renders). It is read at generation time, never embedded, so a line change
// regenerates the set instead of silently drifting from it.
type sourceProfile struct {
	ID       string `json:"id"`
	Messages []struct {
		ID           string `json:"id"`
		OriginalText string `json:"original_text"`
	} `json:"messages"`
	Edges []struct {
		From       string  `json:"from"`
		To         string  `json:"to"`
		Type       string  `json:"type"`
		Confidence float64 `json:"confidence"`
	} `json:"edges"`
}

// GenerateSamples builds the sample matrix from the synthetic profile's lines:
// every line in every font at every resolution, plus the analog-noisy variant
// of every line and a few empty boxes (precision coverage: the box is there,
// there is nothing to say). Order is fixed, so ids and hash splits are stable.
func GenerateSamples(srcPath string, seedBase uint64) (*TruthSet, *ScriptProfile, error) {
	raw, err := os.ReadFile(srcPath)
	if err != nil {
		return nil, nil, fmt.Errorf("bench: read source profile: %v", err)
	}
	var src sourceProfile
	if err := json.Unmarshal(raw, &src); err != nil {
		return nil, nil, fmt.Errorf("bench: parse %s: %v", srcPath, err)
	}
	if len(src.Messages) == 0 {
		return nil, nil, fmt.Errorf("bench: %s names no messages", srcPath)
	}
	prof := &ScriptProfile{
		SchemaVersion: SchemaVersion,
		ID:            "bench-synthetic-001",
		Origin:        "generated_synthetic",
		Description:   "Synthetic benchmark script: the docs/spec/11 synthetic profile lines with speaker attribution, for the internal/bench harness (ADR-0038). No real game content.",
		Scene:         SceneID,
		Regions:       []Region{{ID: "bottom", Type: "textbox", X: 0.0975, Y: 0.7565, W: 0.8025, H: 0.1515}},
	}
	for _, m := range src.Messages {
		prof.Messages = append(prof.Messages, ScriptLine{ID: m.ID, Text: m.OriginalText, Speaker: speakerFor(m.ID)})
	}
	for _, e := range src.Edges {
		prof.Edges = append(prof.Edges, Edge{From: e.From, To: e.To, Type: e.Type, Confidence: e.Confidence})
	}

	ts := &TruthSet{
		SchemaVersion: SchemaVersion,
		Set:           prof.ID,
		Origin:        "generated_synthetic",
		SeedBase:      seedBase,
		ProfileFile:   "profile.json",
	}
	n := 0
	add := func(text string, msgID *string, speaker, kind string, intended bool, font string, w, h int, noise float64) {
		n++
		id := fmt.Sprintf("bn%04d", n)
		ts.Samples = append(ts.Samples, Sample{
			ID:             id,
			Frames:         frameSchedule(),
			SourceText:     text,
			MessageID:      msgID,
			Speaker:        speaker,
			TextKind:       kind,
			Scene:          SceneID,
			IntendedSpeech: intended,
			Split:          SplitForID(id),
			Variant:        Variant{Font: font, Resolution: fmt.Sprintf("%dx%d", w, h), Noise: noise},
		})
	}
	str := func(s string) *string { return &s }
	for _, m := range prof.Messages {
		for _, font := range Fonts {
			for _, r := range Resolutions {
				add(m.Text, str(m.ID), m.Speaker, "dialogue", true, font, r.W, r.H, 0)
			}
		}
	}
	for _, m := range prof.Messages { // analog-noisy: the DejaVu treatment at 4:3
		add(m.Text, str(m.ID), m.Speaker, "dialogue", true, Fonts[0], 640, 480, 1)
	}
	for i := 0; i < 4; i++ { // empty boxes: present box, nothing to read
		r := Resolutions[i%len(Resolutions)]
		add("", nil, SpeakerUnknown, "empty", false, "", r.W, r.H, 0)
	}
	if err := ts.Validate(); err != nil {
		return nil, nil, fmt.Errorf("bench: generated set failed validation: %v", err)
	}
	return ts, prof, nil
}

// speakerFor assigns the synthetic script's speakers by message id. The lines
// are invented; a few stay "unknown" so the named-speaker metric has honest
// unknown coverage (unknown cases must not receive a wrong forced speaker).
func speakerFor(id string) string {
	switch id {
	case "demo-page-1":
		return "keeper"
	case "demo-page-2":
		return "harbourmaster"
	case "demo-x01", "demo-x02", "demo-x03", "demo-x04", "demo-x05", "demo-x06":
		return "villager"
	default:
		return SpeakerUnknown
	}
}

// frameSchedule is the ten-frame manifest of one sample.
func frameSchedule() []Frame {
	var out []Frame
	for i := 1; i <= nFrames; i++ {
		name := "absent.png"
		if i >= 3 && i <= 9 {
			name = "text.png"
		}
		out = append(out, Frame{File: name, TsNS: int64(i) * frameStepNS})
	}
	return out
}

// RenderSamples renders every frame of the set's samples into framesDir and
// returns the per-sample frame directories ready for replay. Frames live in a
// temp dir (the committed set carries truth.json only; ADR-0038).
func RenderSamples(ts *TruthSet, framesDir string) (map[string]string, error) {
	if _, err := exec.LookPath("magick"); err != nil {
		return nil, fmt.Errorf("NOT RUN: ImageMagick (magick) is required to render bench frames")
	}
	fonts := map[string]string{}
	need := map[string]bool{}
	for _, s := range ts.Samples {
		if s.Variant.Font != "" {
			need[s.Variant.Font] = true
		}
	}
	for fam := range need {
		file, err := fcMatch(fam)
		if err != nil {
			return nil, fmt.Errorf("NOT RUN: font %q not found on this host (fc-match: %v)", fam, err)
		}
		fonts[fam] = file
	}
	coef := map[string]float64{} // family -> upper-bound px per char per pt
	out := map[string]string{}
	for i := range ts.Samples {
		s := ts.Samples[i]
		dir := filepath.Join(framesDir, s.ID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		if err := renderSampleFrames(&s, fonts, coef, dir, ts.SeedBase); err != nil {
			return nil, fmt.Errorf("render %s: %v", s.ID, err)
		}
		if err := writeManifest(filepath.Join(dir, "timestamps.json"), s.Frames); err != nil {
			return nil, err
		}
		out[s.ID] = dir
	}
	return out, nil
}

func renderSampleFrames(s *Sample, fonts map[string]string, coef map[string]float64, dir string, seedBase uint64) error {
	r := resolutionOf(s.Variant.Resolution)
	scl := float64(r.H) / 480.0
	seed := SampleSeed(seedBase, s.ID)
	bx, by := int(boxX*float64(r.W)+0.5), int(boxY*float64(r.H)+0.5)
	bw, bh := int(boxW*float64(r.W)+0.5), int(boxH*float64(r.H)+0.5)
	lines, pt := []string{}, 0
	if s.TextKind == "dialogue" {
		var err error
		lines, pt, err = layoutText(s, scl, bw, bh, fonts, coef)
		if err != nil {
			return err
		}
	}
	for _, f := range s.Frames {
		kind := "absent"
		if f.File == "text.png" {
			kind = "text"
		}
		if err := renderFrame(s, r, scl, seed, bx, by, bw, bh, lines, pt, fonts, kind, filepath.Join(dir, f.File)); err != nil {
			return err
		}
	}
	return nil
}

// renderFrame is one magick invocation: scene, optional box and text, optional
// analog degradation, quantised PNG. One process per frame keeps the recipe in
// one place and the argument list the whole derivation.
func renderFrame(s *Sample, r struct{ W, H int }, scl float64, seed uint32, bx, by, bw, bh int, lines []string, pt int, fonts map[string]string, kind, out string) error {
	args := []string{"-size", fmt.Sprintf("%dx%d", r.W, r.H), "gradient:#2c4a6e-#9fc0d8",
		"-fill", "rgba(29,43,35,0.5)", "-draw", fmt.Sprintf("stroke none rectangle 0,%d %d,%d", r.H*2/3, r.W, r.H),
		"-seed", strconv.Itoa(int(seed)), "-attenuate", "0.22", "+noise", "Gaussian", "-blur", "0x0.7"}
	if kind != "absent" {
		m := int(borderInset * scl)
		rad, stroke := int(16*scl), int(2*scl)
		args = append(args,
			"-fill", "rgba(11,16,24,0.82)", "-draw", fmt.Sprintf("stroke none roundrectangle %d,%d %d,%d %d,%d", bx, by, bx+bw-1, by+bh-1, rad, rad),
			"-stroke", "rgba(207,224,242,0.9)", "-strokewidth", strconv.Itoa(stroke), "-fill", "none",
			"-draw", fmt.Sprintf("roundrectangle %d,%d %d,%d %d,%d", bx+m/2, by+m/2, bx+bw-1-m/2, by+bh-1-m/2, rad-1, rad-1),
			"-stroke", "none")
	}
	if kind == "text" && len(lines) > 0 {
		lead := pt + int(5*scl)
		x, y := bx+int(textInsetX*scl), by+int(textInsetY*scl)
		args = append(args, "-font", fonts[s.Variant.Font], "-pointsize", strconv.Itoa(pt),
			"-fill", "#f2f6fb", "-stroke", "none", "-gravity", "NorthWest")
		for i, line := range lines {
			args = append(args, "-annotate", fmt.Sprintf("+%d+%d", x, y+i*lead), line)
		}
	}
	if s.Variant.Noise > 0 {
		args = append(args, "-blur", fmt.Sprintf("0x%.1f", NoiseParams.Blur),
			"-seed", strconv.Itoa(int(seed)+100), "-attenuate", fmt.Sprintf("%.1f", NoiseParams.Attenuate), "+noise", "Gaussian",
			"-channel", "R", "-evaluate", "Multiply", strconv.FormatFloat(NoiseParams.ChromaR, 'f', 2, 64),
			"-channel", "B", "-evaluate", "Multiply", strconv.FormatFloat(NoiseParams.ChromaB, 'f', 2, 64),
			"-blur", fmt.Sprintf("0x%.1f", NoiseParams.SecondBlur),
			"-stroke", NoiseParams.ScanlineA, "-strokewidth", "1", "-fill", "none",
			"-draw", scanlineString(r.W, r.H, NoiseParams.ScanlineN))
	}
	args = append(args, "-dither", "none", "-colors", "24",
		"-define", "png:exclude-chunks=date,time", "-depth", "8", "-alpha", "off", out)
	cmd := exec.Command("magick", args...)
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("magick: %v: %s", err, strings.TrimSpace(string(b)))
	}
	return nil
}

// layoutText picks the point size and wraps the text so every line fits the
// box (docs/spec/12: a rendered overflow would make the truth a lie about its
// own pixels). The per-font coefficient is an upper bound measured once per
// font on a wide-glyph string, so fit is guaranteed, not estimated.
func layoutText(s *Sample, scl float64, boxW, boxH int, fonts map[string]string, coef map[string]float64) ([]string, int, error) {
	fam := s.Variant.Font
	c, ok := coef[fam]
	if !ok {
		c, err := measureCoef(fonts[fam])
		if err != nil {
			return nil, 0, err
		}
		coef[fam] = c
	}
	lines := []string{s.SourceText}
	if utf8Len(s.SourceText) > 40 {
		lines = wrapMiddle(s.SourceText)
	}
	longest := 0
	for _, l := range lines {
		if n := utf8Len(l); n > longest {
			longest = n
		}
	}
	avail := float64(boxW) - textInsetX*2*scl
	lineBudget := (float64(boxH) - 36*scl) / float64(len(lines))
	pt := int(avail / (c * float64(longest)))
	if hpt := int(lineBudget - 5*scl); hpt < pt {
		pt = hpt
	}
	if pt > int(maxPoint*scl) {
		pt = int(maxPoint * scl)
	}
	if pt < 10 {
		pt = 10
	}
	return lines, pt, nil
}

// measureCoef renders a 48-wide M string at 100pt and returns the per-char,
// per-pt pixel width: an upper bound, because M is the widest common glyph.
func measureCoef(fontFile string) (float64, error) {
	cmd := exec.Command("magick", "-size", "4000x300", "xc:none", "-font", fontFile,
		"-pointsize", "100", "-fill", "#ffffff", "-gravity", "NorthWest",
		"-annotate", "+0+0", strings.Repeat("M", 48), "-trim", "-format", "%w", "info:")
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("NOT RUN: cannot measure font %s: %v", fontFile, err)
	}
	w, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || w <= 0 {
		return 0, fmt.Errorf("NOT RUN: unreadable font width %d from %s", w, fontFile)
	}
	return float64(w) / (48 * 100), nil
}

// scanlineString is the -draw string that darkens every Nth row: the analog
// capture's line mask, as 1px rectangles in one draw call.
func scanlineString(w, h, n int) string {
	var b strings.Builder
	b.WriteString("stroke-width 1 fill none ")
	for y := 0; y < h; y += n {
		fmt.Fprintf(&b, "rectangle 0,%d %d,%d ", y, w-1, y)
	}
	return strings.TrimRight(b.String(), " ")
}

// fcMatch resolves a font family to the file fontconfig would pick, the same
// way scripts/fixtures/gen-synthetic-textbox.sh does.
func fcMatch(family string) (string, error) {
	cmd := exec.Command("fc-match", "-f", "%{file}", family)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	file := strings.TrimSpace(string(out))
	if file == "" {
		return "", fmt.Errorf("fc-match returned no file for %q", family)
	}
	if st, err := os.Stat(file); err != nil || st.Size() == 0 {
		return "", fmt.Errorf("fc-match gave missing file %q for %q", file, family)
	}
	return file, nil
}

func resolutionOf(s string) struct{ W, H int } {
	var w, h int
	_, _ = fmt.Sscanf(s, "%dx%d", &w, &h)
	return struct{ W, H int }{w, h}
}

func utf8Len(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}

// wrapMiddle splits a line at the space nearest the middle for two-line boxes.
func wrapMiddle(s string) []string {
	mid := len(s) / 2
	pos := -1
	for i := mid; i >= 0; i-- {
		if s[i] == ' ' {
			pos = i
			break
		}
	}
	if pos < 0 {
		pos = strings.Index(s[mid:], " ") + mid
		if pos < 0 || pos >= len(s) {
			return []string{s}
		}
	}
	return []string{s[:pos], strings.TrimLeft(s[pos+1:], " ")}
}
