package hwsmoke

// TestHWOCRControlledImage runs the app's real Tesseract engine
// (internal/vision/ocr.Tesseract — the same struct the pipeline's OCR region
// uses, including its subprocess supervision, TSV parsing and timeout
// bounds) over a known textbox image and bounds the character error rate.
//
// The image is the same controlled asset internal/doctor uses for its
// ocr.controlled_image check: a synthetic white 400x100 canvas with
// "NARRATOR 42" in DejaVu Sans 48 pt, generated with:
//
//	magick -size 400x100 xc:white -font DejaVu-Sans -pointsize 48 -fill black \
//	  -gravity center -annotate +0+0 "NARRATOR 42" testdata/ocr-controlled.png
//
// (the copy in internal/doctor/testdata/ocr-check.png is the original; this
// one is a bytewise duplicate kept under this package so the gate is
// self-contained). Synthetic and copyright-free, so it may live in the
// repository (AGENTS.md rule 5).

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"image"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

//go:embed testdata/ocr-controlled.png
var controlledPNG []byte

// controlledText is the ground truth the image renders — identical to
// internal/doctor's controlledText so both the diagnostic and the gate
// measure the same string.
const controlledText = "NARRATOR 42"

// maxCER is the character-error-rate bound: with this controlled render the
// engine should read every character, so anything under 5 % is the
// "the OCR stack on this host is healthy" answer; a failure here is the
// OCR dependency (binary or language pack) broken on this host, exactly the
// thing docs/spec/10's preflight exists to catch before the device is
// shipped.
const maxCER = 0.05

func TestHWOCRControlledImage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	img, err := decodeControlledImage()
	if err != nil {
		t.Fatalf("cannot decode the controlled image: %v", err)
	}
	t.Logf("controlled image: %dx%d, text %q", img.Bounds().Dx(), img.Bounds().Dy(), controlledText)

	// The real path: the zero-value Tesseract config is the docs/spec/05
	// recommendation (eng, PSM 6, LSTM default) — the same defaults the
	// pipeline runs in production.
	engine := ocr.Tesseract{Lang: "eng"}
	res, err := engine.Recognize(ctx, img)
	if err != nil {
		if errors.Is(err, ocr.ErrEngineUnavailable) {
			notRun(t, "the tesseract binary is unavailable to the real OCR path",
				"install tesseract with the English pack "+
					"(pacman -S tesseract tesseract-data-eng / apt install tesseract-ocr tesseract-ocr-eng) "+
					"and re-run")
		}
		t.Fatalf("real Tesseract path failed: %v", err)
	}

	t.Logf("OCR read: %q (mean conf %.2f, duration %s, engine=%s)",
		strings.TrimSpace(res.Text), res.MeanConf, res.Duration.Round(time.Millisecond), res.Engine)
	got := strings.Join(strings.Fields(res.Text), " ")

	r := cer(got, controlledText)
	if r >= maxCER {
		t.Fatalf("character error rate %.1f%% (read %q, want %q) exceeds the %.0f%% bound: "+
			"the OCR stack on this host is misconfigured",
			r*100, got, controlledText, maxCER*100)
	}
	t.Logf("character error rate: %.1f%% (bound %.0f%%)", r*100, maxCER*100)
}

func decodeControlledImage() (image.Image, error) {
	r, err := png.Decode(bytes.NewReader(controlledPNG))
	if err != nil {
		return nil, err
	}
	return r, nil
}
