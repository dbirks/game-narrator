package pipeline_test

// The active-picture window wiring (ADR-0082 amendment 1): a synthetic
// bordered-HDMI replay — the converter's stretched picture inside black
// borders, a dialogue box and a sign drawn at the profile's regions inside
// the active picture — run through the real pipeline with the session's crop
// options (LetterboxDetect plus one tracker per session). With the tracker the
// crops map through the settled window and both lines commit; without it the
// same frames map the regions onto the whole canvas, the crops land in the
// scene, the reads come back empty and nothing commits. The second subtest is
// the regression proof: it is the exact measurement ADR-0082 took on the real
// RYBOZEN V316B feed, rendered synthetic (AGENTS.md rule 5: generated frames,
// no real content).
//
// Class of this test: (b) captured-frame replay over generated frames — the
// real replay source, the real pipeline, the real matcher; only the OCR engine
// is a scripted stub, and it is a *geometry* stub (it reads a crop only when
// the crop is the panel), which is what makes the mis-placement visible instead
// of hidden behind a lenient fake.

import (
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/capture/replay"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/matcher"
	"github.com/dbirks/game-narrator/internal/pipeline"
	"github.com/dbirks/game-narrator/internal/recognition"
	"github.com/dbirks/game-narrator/internal/vision"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

// The canvas and the window are the ADR's measured 720p numbers: the
// converter stretches the GameCube's 4:3 picture into x 48..1223, y 92..660 of
// a 1280×720 canvas and the borders are true black.
const (
	hdmiW = 1280
	hdmiH = 720

	winMinX = 48
	winMinY = 92
	winMaxX = 1223
	winMaxY = 660

	lineDialogue = "The tide is turning and the harbour bell has not rung."
	lineSign     = "Keep out beyond the broken jetty."

	msgDialogue = "hdmi-harbour"
	msgSign     = "hdmi-jetty"

	panelDialogueLuma = 24 // the dialogue panel's fill
	panelSignLuma     = 34 // the sign panel's fill
	panelLumaSplit    = 29 // the ring mean that tells the panels apart
)

// The profile's regions, in fractions of the active picture (the same
// coordinates a calibrated profile stores).
var (
	dialogueRegion = games.Region{ID: "bottom", RegionType: games.RegionTextbox, RectX: 0.05, RectY: 0.62, RectW: 0.80, RectH: 0.25}
	signRegion     = games.Region{ID: "sign", RegionType: games.RegionSign, RectX: 0.10, RectY: 0.15, RectW: 0.25, RectH: 0.18}
)

func activeWindow() image.Rectangle {
	return image.Rect(winMinX, winMinY, winMaxX, winMaxY)
}

// dialogueBox / signBox are the regions' MapRect of the active window — the
// panels are drawn where the tracked crop lands, which is the whole point of
// the fixture: the regions are calibrated against the active picture, not the
// canvas.
func dialogueBox() image.Rectangle {
	return vision.MapRect(activeWindow(), vision.RectOf(dialogueRegion))
}
func signBox() image.Rectangle { return vision.MapRect(activeWindow(), vision.RectOf(signRegion)) }

// hdmiFrame builds one bordered canvas: black borders, a bright scene inside
// the active window, and the two panels drawn at exactly the regions' MapRect
// of the window. The panels carry different fill luma so the reader can name
// the line it read; the text is bright bands inside each panel, far from any
// crop edge, so an aligned crop's border ring is pure panel fill.
func hdmiFrame() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, hdmiW, hdmiH))
	for y := 0; y < hdmiH; y++ {
		for x := 0; x < hdmiW; x++ {
			var c color.Color = color.Black // the borders
			if x >= winMinX && x < winMaxX && y >= winMinY && y < winMaxY {
				// A bright scene, uniform on purpose: the ring test below
				// needs the scene and the panels to be unambiguous.
				c = color.Gray{Y: 190}
			}
			img.Set(x, y, c)
		}
	}
	fillRect(img, dialogueBox(), color.Gray{Y: panelDialogueLuma})
	fillRect(img, signBox(), color.Gray{Y: panelSignLuma})
	// The text bands (inside the panels, away from the ring).
	fillRect(img, image.Rect(150, 500, 1000, 530), color.Gray{Y: 235})
	fillRect(img, image.Rect(200, 215, 420, 235), color.Gray{Y: 235})
	return img
}

func fillRect(img *image.RGBA, box image.Rectangle, c color.Color) {
	for y := box.Min.Y; y < box.Max.Y; y++ {
		for x := box.Min.X; x < box.Max.X; x++ {
			img.Set(x, y, c)
		}
	}
}

// writeHDMIReplay renders N identical frames with a 100 ms cadence into dir
// (PNG plus the replay's timestamps.json) and returns the dir.
func writeHDMIReplay(t *testing.T, n int) string {
	t.Helper()
	dir := t.TempDir()
	frame := hdmiFrame()
	manifest := make([]replay.Entry, 0, n)
	for i := 1; i <= n; i++ {
		name := "frame-" + itoa(i) + ".png"
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if err := png.Encode(f, frame); err != nil {
			t.Fatalf("encode %s: %v", name, err)
		}
		f.Close()
		manifest = append(manifest, replay.Entry{File: name, TsNS: int64(i) * 100_000_000})
	}
	b, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("timestamps.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "timestamps.json"), b, 0o644); err != nil {
		t.Fatalf("timestamps.json: %v", err)
	}
	return dir
}

// alignedReader is the scripted OCR engine: it reads the raw grayscale crop
// the recipe (Scale: 1) passes through and returns a line only when the
// crop's border ring is panel, not scene — the crop the tracker's window
// produces. A crop mapped onto the whole canvas puts the scene around the
// panel and reads nothing, exactly the mis-placement ADR-0082 measured on
// the real feed. The panel's fill luma names the line (the two panels carry
// different fills). It is a synthetic stub (class (a)); the real Tesseract's
// sensitivity to a shifted crop is what makes the mis-placement a miss there.
type alignedReader struct {
	texts map[string]string
}

func (a *alignedReader) Name() string { return "aligned-fake" }

func (a *alignedReader) Recognize(_ context.Context, img image.Image) (ocr.Result, error) {
	res := ocr.Result{Engine: a.Name()}
	g, ok := img.(*image.Gray)
	if !ok {
		return res, nil
	}
	b := g.Bounds()
	if b.Dx() < 10 || b.Dy() < 10 {
		return res, nil
	}
	// The ring: a 2 px inset border walk. An aligned crop's ring is the panel
	// fill; a mis-mapped crop's ring crosses the bright scene.
	sum, total := 0, 0
	for x := b.Min.X + 2; x < b.Max.X-2; x++ {
		sum += int(g.GrayAt(x, b.Min.Y+2).Y)
		sum += int(g.GrayAt(x, b.Max.Y-3).Y)
		total += 2
	}
	for y := b.Min.Y + 2; y < b.Max.Y-2; y++ {
		sum += int(g.GrayAt(b.Min.X+2, y).Y)
		sum += int(g.GrayAt(b.Max.X-3, y).Y)
		total += 2
	}
	if total == 0 {
		return res, nil
	}
	mean := sum / total
	if mean >= 100 {
		// Scene ring: the crop is not the panel. Reads nothing.
		return res, nil
	}
	// Panel ring: an aligned crop. The fill names the line.
	if mean < panelLumaSplit {
		res.Text = a.texts[msgDialogue]
	} else {
		res.Text = a.texts[msgSign]
	}
	res.MeanConf = 0.9375
	res.Words = []ocr.Word{{Text: res.Text, Conf: 0.9375}}
	res.Lines = []ocr.Line{{Text: res.Text, Conf: 0.9375}}
	return res, nil
}

// hdmiIndex is the candidate corpus: the two lines the frames show plus
// distractors, with the dialogue→sign edge the profile would carry.
func hdmiIndex(t *testing.T) *matcher.Index {
	t.Helper()
	msgs := []games.Message{
		{ID: msgDialogue, ProfileID: "hdmi-profile", OriginalText: lineDialogue, SpeechText: lineDialogue, TextKind: games.TextDialogue},
		{ID: msgSign, ProfileID: "hdmi-profile", OriginalText: lineSign, SpeechText: lineSign, TextKind: games.TextSign},
		{ID: "hdmi-x1", ProfileID: "hdmi-profile", OriginalText: "The tide is turning and the keeper has not rung.", SpeechText: "The tide is turning and the keeper has not rung.", TextKind: games.TextDialogue},
		{ID: "hdmi-x2", ProfileID: "hdmi-profile", OriginalText: "Keep out beyond the broken fence.", SpeechText: "Keep out beyond the broken fence.", TextKind: games.TextSign},
	}
	edges := []games.MessageEdge{{FromID: msgDialogue, ToID: msgSign, EdgeType: games.EdgeNext, Confidence: 0.9}}
	idx, err := matcher.Build(msgs, edges, matcher.IndexOptions{})
	if err != nil {
		t.Fatalf("matcher.Build: %v", err)
	}
	return idx
}

// hdmiRun drives one replay of the bordered clip through the pipeline with
// the given crop options and returns the committed message ids.
func hdmiRun(t *testing.T, crop vision.CropOptions) []string {
	t.Helper()
	dir := writeHDMIReplay(t, 10)
	eng := &alignedReader{texts: map[string]string{msgDialogue: lineDialogue, msgSign: lineSign}}
	cfg := pipeline.Config{
		SessionID:       "hdmi-session",
		ProfileRevision: "1",
		Regions:         []games.Region{dialogueRegion, signRegion},
		OCR:             eng,
		Index:           hdmiIndex(t),
		Preprocess:      vision.Steps{Scale: 1}, // pass the raw crop through
		Mode:            pipeline.ModeReplay,
		Crop:            crop,
		OCRTimeout:      5 * time.Second,
		// The clip samples at 10 Hz, so the recognition tracker is tuned to
		// that cadence: three frames of vote, one frame interval of
		// stability — the same settings the synthetic corpus harness uses.
		Tracker: recognition.Config{VoteWindow: 3, StableWindow: 100 * time.Millisecond},
	}
	runner, err := pipeline.New(cfg)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	mem := events.NewMemStore()
	hub := events.New(events.WithStore(mem))
	defer hub.Close()
	src := &replay.Source{Dir: dir, Speed: 0}
	if err := runner.Run(context.Background(), src, hub); err != nil {
		t.Fatalf("Run: %v", err)
	}
	st := runner.Stats()
	if st.FramesProcessed != 10 {
		t.Fatalf("frames processed = %d, want 10", st.FramesProcessed)
	}
	// Both regions are present on every frame (the panels cover most of even
	// the mis-mapped crops), so the miss in the without-tracker arm must show
	// up as empty reads, not as absent boxes.
	if st.OCRCalls < 2 {
		t.Fatalf("ocr calls = %d, want at least one per region", st.OCRCalls)
	}
	evs, err := mem.After(context.Background(), 0, events.Filter{}, 4096)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	return committed(t, evs)
}

func TestBorderedHDMIReplayCommitsWithTrackedWindow(t *testing.T) {
	got := hdmiRun(t, vision.CropOptions{
		Letterbox: vision.LetterboxDetect,
		Tracker:   vision.NewTracker(vision.TrackerConfig{}),
	})
	if len(got) != 2 || !containsID(got, msgDialogue) || !containsID(got, msgSign) {
		t.Fatalf("committed = %v, want both %q and %q (the crops mapped through the settled window)", got, msgDialogue, msgSign)
	}
}

func TestBorderedHDMIReplayMissesWithoutTracker(t *testing.T) {
	// The same frames, the historic crop options: the regions map onto the
	// whole canvas, the crops land in the scene, and the reader — like the
	// one ADR-0082 measured on the real feed — comes back empty. No line
	// commits.
	got := hdmiRun(t, vision.CropOptions{})
	if len(got) != 0 {
		t.Fatalf("committed = %v, want nothing: without the tracked window the crops are mis-placed and the reads are empty", got)
	}
}

func containsID(ids []string, id string) bool {
	for _, s := range ids {
		if s == id {
			return true
		}
	}
	return false
}
