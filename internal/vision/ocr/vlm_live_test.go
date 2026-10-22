package ocr

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/openai"
)

// TestLiveVLMOCRFallbackHardFontCrop is the live proof of the docs/spec/05
// fallback: a textbox crop rendered in a hard font under heavy noise, where a
// local engine struggles, is sent (crop only, downscaled) to the real
// vision_ocr_fallback model, and the reading's normalised character error rate
// is asserted under 10%. Tesseract's CER on the same crop is logged for the
// comparison the spec asks for.
//
// NOT RUN (skipped, not failed) when: the gateway is not configured, the
// gateway rejects image input, magick or tesseract is missing on this host, or
// a required font is unavailable. The test is sequential (no t.Parallel):
// live gateway calls run one at a time.
func TestLiveVLMOCRFallbackHardFontCrop(t *testing.T) {
	const known = "The harbour bell rang twice."
	const wantMaxCER = 0.10

	// Gate marker (ADR-0065): live tests run only under make test-models;
	// the unit gate (and any manual go test) must never call the gateway.
	if os.Getenv(providers.EnvGate) != "test-models" {
		t.Skipf("live provider test outside the test-models gate: run via make test-models (it sets %s=test-models)", providers.EnvGate)
	}

	// --- configuration gate: skip, with setup instructions, not a failure. ---
	// The same pattern the provider live tests use: the shared gateway's URL
	// and token from the role configuration, with the model resolved role-model
	// → GAME_NARRATOR_LIVE_MODEL → the known-good default. The role's own
	// "NOT CONFIGURED" state (no model) is the app's fallback being off, not
	// the gateway being unavailable — this test probes the gateway capability
	// the role would use.
	roles, err := providers.LoadRoles(os.LookupEnv)
	if err != nil {
		t.Fatalf("LoadRoles: %v", err)
	}
	cfg := roles[providers.RoleVisionOCRFallback]
	if strings.TrimSpace(cfg.BaseURL) == "" || strings.TrimSpace(cfg.Token()) == "" {
		t.Skipf("NOT RUN: set %s and the token env named by %s (default %s) to run the live VLM OCR fallback test",
			providers.EnvGatewayURL, providers.EnvGatewayTokenEnv, providers.EnvGatewayToken)
	}
	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		model = strings.TrimSpace(os.Getenv("GAME_NARRATOR_LIVE_MODEL"))
	}
	if model == "" {
		model = "flint" // the collective replaced qwen3-8-27b with flint on 2026-10-21
	}
	// The vision legs resolve the model through GAME_NARRATOR_LIVE_VISION_MODEL
	// (default: GAME_NARRATOR_LIVE_MODEL); a text-only model cannot serve the
	// VLM, and the refusal is a NOT RUN, never a failure.
	if v := strings.TrimSpace(os.Getenv("GAME_NARRATOR_LIVE_VISION_MODEL")); v != "" {
		model = v
	}
	client, err := openai.New(cfg.BaseURL, cfg.Token(), model)
	if err != nil {
		t.Fatalf("openai.New: %v", err)
	}
	// The role's timeout bounds production calls; the test gives the noisy
	// crop more headroom on a fresh client.
	client.HTTP = &http.Client{Timeout: 2 * time.Minute}
	if _, err := exec.LookPath("magick"); err != nil {
		t.Skip("NOT RUN: ImageMagick (magick) is required to render the crop")
	}
	if _, err := exec.LookPath("tesseract"); err != nil {
		t.Skip("NOT RUN: tesseract is required for the comparison CER")
	}
	font := hardFont(t)

	// --- render the crop: hard serif font, dim on a dark box, downscaled,
	// heavy Gaussian noise and a soft blur. Tuned so the local engine comes
	// back garbled ("The hartunsr bell rang rewe" on the reference host) while
	// the VLM still resolves the line — that gap is the fallback's value case.
	dir := t.TempDir()
	out := filepath.Join(dir, "crop.png")
	args := []string{
		"-size", "1280x240", "xc:#0b1018",
		"-font", font, "-pointsize", "48", "-fill", "#c8d4de",
		"-gravity", "center", "-annotate", "+0+0", known,
		"-resize", "640x120",
		"-seed", "42", "-attenuate", "0.50", "+noise", "Gaussian",
		"-blur", "0x2.6",
		out,
	}
	if outBytes, err := exec.Command("magick", args...).CombinedOutput(); err != nil {
		t.Fatalf("magick render: %v: %s", err, bytes.TrimSpace(outBytes))
	}
	img, err := readImage(out)
	if err != nil {
		t.Fatalf("read crop: %v", err)
	}

	// --- Tesseract's CER on the same crop: the comparison the spec asks for. ---
	// The engine's own deadline is scaled for a race-instrumented binary
	// (ocrTimeoutScale, race_on_test.go) so the comparison read is not killed
	// by runner load and logged as a 1.0 CER it never earned.
	tessRes, tessErr := Tesseract{Timeout: DefaultTimeout * ocrTimeoutScale}.Recognize(context.Background(), img)
	if tessErr != nil {
		t.Logf("tesseract failed on the noisy crop: %v", tessErr)
	}
	tessCER := normalizedDistance(NormaliseForCompare(tessRes.Text), NormaliseForCompare(known))
	t.Logf("tesseract: CER=%.3f read=%q err=%v", tessCER, truncateForLog(tessRes.Text), tessErr)
	if tessCER < 0.01 && tessErr == nil {
		// The local engine already owns this font: the live proof of the
		// fallback's *value* is moot, but the VLM contract is still proven
		// below. Log it and continue.
		t.Logf("note: tesseract read the crop well (CER %.3f); the fallback value case is the hard profile, not this crop", tessCER)
	}

	// --- the VLM, on the exact crop the engine would send. ---
	adapter := liveVLMAdapter{c: client}
	eng := NewVLMEngine(adapter)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	prepared, err := encodeCrop(img, eng.MaxSide())
	if err != nil {
		t.Fatalf("encodeCrop: %v", err)
	}
	vlmRes, err := eng.RecognizePrepared(ctx, prepared)
	if err != nil {
		var pe *providers.Error
		if errors.As(err, &pe) && pe.Kind == providers.KindUnsupportedCapability {
			t.Skipf("NOT RUN: gateway refuses image input: %v", pe)
		}
		t.Fatalf("VLM crop read: %v", err)
	}
	t.Logf("vlm: model=%s sent_bytes=%d (crop is %dx%d, not a full frame)", client.Model, len(prepared), img.Bounds().Dx(), img.Bounds().Dy())
	vlmCER := normalizedDistance(NormaliseForCompare(vlmRes.Text), NormaliseForCompare(known))
	t.Logf("vlm: CER=%.3f read=%q", vlmCER, truncateForLog(vlmRes.Text))
	if vlmCER >= wantMaxCER {
		t.Errorf("VLM normalised CER = %.3f, want < %.2f against %q (read %q)", vlmCER, wantMaxCER, known, vlmRes.Text)
	}
	if vlmCER <= tessCER {
		t.Logf("comparison: vlm CER %.3f <= tesseract CER %.3f on this crop", vlmCER, tessCER)
	} else {
		t.Logf("comparison: vlm CER %.3f > tesseract CER %.3f on this crop (tesseract owned this font; the fallback is for the harder profiles)", vlmCER, tessCER)
	}

	// --- the policy end to end: tesseract primary, VLM secondary. ---
	// DifficultFont is the profile flag for exactly this situation (a font the
	// local engine is known to defeat): the comparison runs even if the
	// primary's confidence is high, so the live proof records both readings
	// and the discrepancy on every run.
	fb := &FallbackEngine{
		Primary:   Tesseract{Timeout: DefaultTimeout * ocrTimeoutScale},
		Secondary: eng,
		Policy: FallbackPolicy{
			ConfidenceThreshold: 0.6,
			MaxSide:             eng.MaxSide(),
			DifficultFont:       true,
			RatePerSecond:       1,
			RateBurst:           3,
		},
	}
	fbRes, fbErr := fb.Recognize(ctx, img)
	if fbErr != nil {
		t.Logf("fallback returned an error (the frame was unread by both): %v", fbErr)
		return
	}
	fbCER := normalizedDistance(NormaliseForCompare(fbRes.Text), NormaliseForCompare(known))
	if fbRes.Fallback == nil {
		t.Fatal("DifficultFont must force the comparison and record the Fallback row")
	}
	t.Logf("fallback: chosen=%s reason=%s discrepancy=%.3f tesseract_conf=%.2f CER=%.3f",
		fbRes.Fallback.Chosen, fbRes.Fallback.Reason, fbRes.Fallback.Discrepancy, fbRes.Fallback.PrimaryConf, fbCER)
	if fbRes.Fallback.Chosen == "secondary" && fbCER >= wantMaxCER {
		t.Errorf("fallback chose the VLM read but its CER = %.3f, want < %.2f (read=%q)", fbCER, wantMaxCER, fbRes.Text)
	}
	if fbRes.Fallback.Chosen == "primary" {
		// The policy kept the local read (a confident primary is not
		// overturned by the VLM's disagreement, ADR-0040 §4): the value case
		// is the VLM-owned frame, so the bound is asserted above on the
		// direct VLM call. Log the comparison either way — docs/spec/05 asks
		// for both readings and the discrepancy on the record.
		t.Logf("note: policy kept the local read (conf %.2f, CER %.3f); the VLM read was %q (CER %.3f)",
			fbRes.Fallback.PrimaryConf, fbCER, truncateForLog(fbRes.Fallback.SecondaryText),
			normalizedDistance(NormaliseForCompare(fbRes.Fallback.SecondaryText), NormaliseForCompare(known)))
	}
}

// hardFont picks a serif face that Tesseract's default language model
// recognises less cleanly than its sans default. The first existing file wins;
// without any of them magick's default font still renders Latin script.
func hardFont(t *testing.T) string {
	t.Helper()
	for _, f := range []string{
		"/usr/share/fonts/liberation/LiberationSerif-Italic.ttf",
		"/usr/share/fonts/TTF/DejaVuSerifCondensed.ttf",
	} {
		if _, err := os.Stat(f); err == nil {
			return f
		}
	}
	t.Log("no preferred hard font found; using magick's default")
	return ""
}

// readImage decodes a PNG into an RGBA (the same contract the pipeline feeds
// its engines: a copy, never a window onto a file buffer).
func readImage(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	src, err := png.Decode(f)
	if err != nil {
		return nil, err
	}
	b := src.Bounds()
	dst := image.NewRGBA(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			dst.Set(x, y, src.At(x, y))
		}
	}
	return dst, nil
}

// liveVLMAdapter is the test-local bridge from the ocr.VLMClient seam to the
// openai client. The production adapter is the runtime's (ADR-0039 §5: the
// runtime owns the provider clients); this one exists only to prove the seam
// against the real gateway. Role defaults apply: reasoning_effort "none" is
// the vision_ocr_fallback role's default, and the timeout is the role's.
type liveVLMAdapter struct{ c *openai.Client }

func (a liveVLMAdapter) Chat(ctx context.Context, req VLMRequest) (string, error) {
	resp, err := a.c.Chat(ctx, openai.ChatRequest{
		Messages: []openai.Message{{
			Role:   "user",
			Text:   req.Instruction,
			Images: [][]byte{req.PNG},
		}},
		MaxTokens:       req.MaxTokens,
		ReasoningEffort: "none",
	})
	if err != nil {
		return "", err
	}
	return resp.Text, nil
}

func truncateForLog(s string) string {
	if len(s) <= 120 {
		return s
	}
	return s[:120] + "…"
}
