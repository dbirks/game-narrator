package ocr

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"testing"
	"time"
)

// fakeVLMClient records every request it gets and answers from a script, so a
// test can assert on the very bytes an engine sends (the privacy property) and
// on the parsing the engine does with the answer.
type fakeVLMClient struct {
	answer  string
	err     error
	calls   int
	lastPNG []byte
	lastReq VLMRequest
}

func (c *fakeVLMClient) Chat(_ context.Context, req VLMRequest) (string, error) {
	c.calls++
	c.lastReq = req
	c.lastPNG = req.PNG
	return c.answer, c.err
}

// solidImage is a w×h RGBA of one colour, the cheapest non-empty image.
func solidImage(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 10, G: 200, B: 30, A: 255})
		}
	}
	return img
}

func TestVLMSendsOnlyDownscaledCrop(t *testing.T) {
	// A 2048×1536 "frame" with a 2048-wide crop: the engine must not send the
	// 2048-pixel image, it must downscale to the 1024 long side. The privacy
	// claim (docs/spec/14) is about bytes, so the test decodes the request's
	// PNG and measures it.
	c := &fakeVLMClient{answer: "The keeper nods."}
	e := NewVLMEngine(c)
	res, err := e.Recognize(context.Background(), solidImage(2048, 1536))
	if err != nil {
		t.Fatalf("Recognize: %v", err)
	}
	if res.Text != "The keeper nods." || res.Engine != "vlm" {
		t.Fatalf("result = %+v", res)
	}
	// The privacy claim (docs/spec/14) is about bytes, so the test decodes the
	// recorded request and measures what went out the door.
	img, err := png.Decode(bytes.NewReader(c.lastPNG))
	if err != nil {
		t.Fatalf("request PNG undecodable: %v", err)
	}
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	if w > 1024 || h > 1024 {
		t.Errorf("sent crop is %dx%d, long side must be ≤ 1024", w, h)
	}
	if w == 2048 || h == 1536 {
		t.Errorf("sent crop equals the full frame dimensions: the frame, not the downscale, left the appliance")
	}
	// Aspect is preserved: 2048×1536 → 1024×768 (±1 rounding).
	if w != 1024 || h < 766 || h > 770 {
		t.Errorf("sent crop %dx%d, want ≈1024×768", w, h)
	}
	if c.lastReq.MaxTokens != 256 {
		t.Errorf("MaxTokens = %d, want the 256 bound", c.lastReq.MaxTokens)
	}
}

func TestVLMDownscaleIsBoundedAndConfigurable(t *testing.T) {
	c := &fakeVLMClient{answer: "NONE"}
	e := NewVLMEngine(c, WithMaxSide(256))
	if _, err := e.Recognize(context.Background(), solidImage(512, 128)); err != nil {
		t.Fatalf("Recognize: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(c.lastPNG))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if w, h := img.Bounds().Dx(), img.Bounds().Dy(); w != 256 || h != 64 {
		t.Errorf("crop %dx%d, want 256×64 at MaxSide 256", w, h)
	}
}

func TestVLMNoDownscaleWhenUnderBound(t *testing.T) {
	c := &fakeVLMClient{answer: "ok"}
	e := NewVLMEngine(c) // default 1024
	if _, err := e.Recognize(context.Background(), solidImage(640, 120)); err != nil {
		t.Fatalf("Recognize: %v", err)
	}
	img, _ := png.Decode(bytes.NewReader(c.lastPNG))
	if w, h := img.Bounds().Dx(), img.Bounds().Dy(); w != 640 || h != 120 {
		t.Errorf("crop %dx%d, want unchanged 640×120", w, h)
	}
}

func TestVLMNoneMapsToEmptyRead(t *testing.T) {
	for _, raw := range []string{"NONE", "none", "None.", "No text", "  \n  "} {
		c := &fakeVLMClient{answer: raw}
		res, err := NewVLMEngine(c).Recognize(context.Background(), solidImage(32, 16))
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if res.Text != "" || len(res.Words) != 0 || len(res.Lines) != 0 {
			t.Errorf("%q: read %+v, want empty", raw, res)
		}
	}
}

func TestVLMStripsQuotesAndMarkdown(t *testing.T) {
	cases := map[string]string{
		`"The harbour bell rang twice."`:                         "The harbour bell rang twice.",
		"‘Nobody saw the boat’":                                  "Nobody saw the boat",
		"```\nThe harbour bell rang twice.\n```":                 "The harbour bell rang twice.",
		"The harbour bell rang twice.\nSorry for the extra line": "The harbour bell rang twice.",
		"  The harbour bell rang twice.  ":                       "The harbour bell rang twice.",
	}
	for raw, want := range cases {
		c := &fakeVLMClient{answer: raw}
		res, err := NewVLMEngine(c).Recognize(context.Background(), solidImage(32, 16))
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if res.Text != want {
			t.Errorf("%q → %q, want %q", raw, res.Text, want)
		}
	}
}

func TestVLMCarriesPriorNeverOne(t *testing.T) {
	c := &fakeVLMClient{answer: "The harbour bell rang twice."}
	res, err := NewVLMEngine(c).Recognize(context.Background(), solidImage(64, 16))
	if err != nil {
		t.Fatalf("Recognize: %v", err)
	}
	if res.MeanConf != vlmConfidencePrior {
		t.Errorf("MeanConf = %v, want the documented prior %v", res.MeanConf, vlmConfidencePrior)
	}
	if res.MeanConf >= 1.0 {
		t.Errorf("MeanConf %v claims certainty", res.MeanConf)
	}
}

func TestVLMErrorsPassThrough(t *testing.T) {
	want := &Failure{Kind: ErrFailed, Engine: "fake-vlm", Err: context.DeadlineExceeded}
	c := &fakeVLMClient{err: want}
	if _, err := NewVLMEngine(c).Recognize(context.Background(), solidImage(32, 16)); err == nil {
		t.Fatal("expected the client error to pass through")
	}
}

func TestVLMDurationIsBoundedByClock(t *testing.T) {
	c := &fakeVLMClient{answer: "ok"}
	oldNow := now
	now = func() time.Time { return time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC) }
	defer func() { now = oldNow }()
	// The clock is pinned, so the duration is the delta of two pinned reads:
	// 0. The property under test is that Duration is measured, not invented.
	res, err := NewVLMEngine(c).Recognize(context.Background(), solidImage(32, 16))
	if err != nil {
		t.Fatalf("Recognize: %v", err)
	}
	if res.Duration != 0 {
		t.Errorf("Duration = %v with a pinned clock, want 0", res.Duration)
	}
}
