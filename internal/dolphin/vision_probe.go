package dolphin

// vision_probe.go is the dev harness's "eyes" for the interactive real-
// Dolphin runs: one frame (downscaled, PNG) plus a terse question sent to
// the user-configured vision model — the same endpoint the app's OCR
// fallback role uses (docs/spec/05) — and the model's one-word answer back.
//
// Why it exists: the TestReal* drive runs measure the screen with the
// structural classifier (signature.go), which settles the box/dialogue
// family but not a human judgment like "is this the item-get box" or "does
// the A-button label read Speak". Those judgments are what a human operator
// would make by looking at the screenshot after every step; the probe makes
// the same judgment callable from the drive, against the same user-configured
// model the product's vision role talks to.
//
// Privacy (AGENTS.md rule 5; docs/spec/14): the frame goes to the
// user-configured gateway (GAME_NARRATOR_GATEWAY_URL) — the operator's own
// endpoint — downscaled to visionProbeMaxSide, and only the QUESTION and the
// model's ANSWER (plus the frame's sha256, so a log line can be correlated
// with a retained PNG) are ever logged or returned. The pixels never land in
// a log, a committed file or an error. The probe is configured-or-nil: a
// run without the gateway configured must still work (the drive degrades to
// its structural measurements and says so), so Ask reports a typed error the
// caller logs as "vision unavailable" and continues.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"math"
	"os"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/openai"
)

const (
	// visionProbeMaxSide bounds the sent frame's long side (640: the render
	// window's own width — the frame is not enlarged, only downscaled when
	// the window is larger).
	visionProbeMaxSide = 640
	// visionProbeTimeout bounds one ask (the model's terse one-word answer
	// is a single-token generation; the budget covers the gateway's queue).
	visionProbeTimeout = 90 * time.Second
	// visionProbeMaxTokens bounds the answer (one word; the bound exists so
	// a runaway answer cannot cost the session a minute of latency).
	visionProbeMaxTokens = 16
)

// visionProbe asks one configured vision model terse questions about frames.
type visionProbe struct {
	client *openai.Client
	model  string
	calls  int
}

// newVisionProbe resolves the probe from the same environment the provider
// live tests use (the shared gateway URL + the token env the operator named
// + the live model id). It is a capability, not a requirement: when the
// gateway is not configured it returns a typed ErrVisionUnavailable error
// and the drive keeps running on its structural measurements.
func newVisionProbe() (*visionProbe, error) {
	url := strings.TrimSpace(os.Getenv(providers.EnvGatewayURL))
	tokenEnv := os.Getenv(providers.EnvGatewayTokenEnv)
	if tokenEnv == "" {
		tokenEnv = providers.EnvGatewayToken
	}
	token := strings.TrimSpace(os.Getenv(tokenEnv))
	if url == "" || token == "" {
		return nil, fmt.Errorf("vision probe: %s and the %s token are not configured (the probe degrades to structural-only measurement)", providers.EnvGatewayURL, tokenEnv)
	}
	// The probe sends frames, so it needs an image-capable model: the vision
	// model when one is configured (flint takes no images, ADR-0161), else
	// the general live model.
	model := strings.TrimSpace(os.Getenv("GAME_NARRATOR_LIVE_VISION_MODEL"))
	if model == "" {
		model = strings.TrimSpace(os.Getenv("GAME_NARRATOR_LIVE_MODEL"))
	}
	if model == "" {
		model = "flint" // the collective replaced qwen3-8-27b with flint on 2026-10-21
	}
	c, err := openai.New(url, token, model)
	if err != nil {
		return nil, fmt.Errorf("vision probe: %w", err)
	}
	c.HTTP = nil // per-request deadlines own the timeout (the ctx here)
	return &visionProbe{client: c, model: model}, nil
}

// Model reports the model id the probe sends questions to (evidence line).
func (p *visionProbe) Model() string { return p.model }

// Calls reports how many frames the probe has sent (the evidence line's
// budget number).
func (p *visionProbe) Calls() int { return p.calls }

// ErrVisionUnavailable is the probe's typed "not configured" (the drive
// logs it and continues on structural measurement — the probe is an aid,
// not a dependency, of the real-Dolphin run).
var ErrVisionUnavailable = fmt.Errorf("vision probe unavailable (gateway not configured)")

// Ask sends one frame with a terse question and returns the model's answer,
// trimmed and lowercased (the questions expect a one-word answer; the
// caller compares the word). The frame's sha256 is returned too (the
// correlation key into the retained evidence PNGs — the log line never
// carries pixels).
func (p *visionProbe) Ask(ctx context.Context, img image.Image, question string) (string, string, error) {
	if p == nil || p.client == nil {
		return "", "", ErrVisionUnavailable
	}
	pngBytes, err := encodeProbeFrame(img, visionProbeMaxSide)
	if err != nil {
		return "", "", fmt.Errorf("vision probe: %w", err)
	}
	sum := sha256.Sum256(pngBytes)
	frameSHA := hex.EncodeToString(sum[:])
	cctx, cancel := context.WithTimeout(ctx, visionProbeTimeout)
	defer cancel()
	resp, err := p.client.Chat(cctx, openai.ChatRequest{
		Messages: []openai.Message{{
			Role:   "user",
			Text:   question,
			Images: [][]byte{pngBytes},
		}},
		MaxTokens:       visionProbeMaxTokens,
		ReasoningEffort: "none",
	})
	if err != nil {
		return frameSHA, "", fmt.Errorf("vision probe: %w", err)
	}
	p.calls++
	return normalizeProbeAnswer(resp.Text), frameSHA, nil
}

// normalizeProbeAnswer collapses the model's answer to its first token,
// lowercased, punctuation stripped (the one-word contract: "YES." and
// "Yes" both normalize to "yes"; a longer answer keeps its first word and
// the caller's comparison fails it, which is the honest outcome).
func normalizeProbeAnswer(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\n\r"); i >= 0 {
		s = s[:i]
	}
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	return strings.ToLower(strings.Trim(fields[0], ".,;:!?\"'`"))
}

// encodeProbeFrame downscales img to the long-side bound and encodes PNG
// (the request's bytes are exactly what leaves the dev host).
func encodeProbeFrame(img image.Image, maxSide int) ([]byte, error) {
	b := img.Bounds()
	if b.Empty() {
		return nil, fmt.Errorf("empty frame")
	}
	w, h := b.Dx(), b.Dy()
	scale := math.Min(1, float64(maxSide)/float64(max(w, h)))
	nw, nh := int(float64(w)*scale+0.5), int(float64(h)*scale+0.5)
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	if scale == 1 {
		draw.Draw(dst, dst.Bounds(), img, b.Min, draw.Src)
	} else {
		// The probe's downscale is the ocr package's bilinear (the shared,
		// deterministic recipe); reuse it so the probe and the VLM engine
		// send the same pixels for the same frame.
		enlarged := enlarge(img, 1) // copy into one concrete format first (At() on a decoded PNG is slow per-pixel)
		scaleBilinearFrom(enlarged, enlarged.Bounds(), dst)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, dst); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// scaleBilinearFrom is the ocr package's bilinear written over a source of
// type image.Image (the ocr one is unexported and bound to its file).
func scaleBilinearFrom(src image.Image, sb image.Rectangle, dst *image.RGBA) {
	dw, dh := dst.Bounds().Dx(), dst.Bounds().Dy()
	xs, ys := float64(sb.Dx())/float64(dw), float64(sb.Dy())/float64(dh)
	for y := 0; y < dh; y++ {
		fy := (float64(y)+0.5)*ys - 0.5
		if fy < 0 {
			fy = 0
		}
		y0 := int(fy)
		ty := fy - float64(y0)
		if y0+1 >= sb.Dy() {
			y0, ty = sb.Dy()-1, 0
		}
		for x := 0; x < dw; x++ {
			fx := (float64(x)+0.5)*xs - 0.5
			if fx < 0 {
				fx = 0
			}
			x0 := int(fx)
			tx := fx - float64(x0)
			if x0+1 >= sb.Dx() {
				x0, tx = sb.Dx()-1, 0
			}
			p := func(x, y int) [4]float64 {
				r, g, bl, a := src.At(sb.Min.X+x, sb.Min.Y+y).RGBA()
				return [4]float64{float64(r >> 8), float64(g >> 8), float64(bl >> 8), float64(a >> 8)}
			}
			p00, p10, p01, p11 := p(x0, y0), p(x0+1, y0), p(x0, y0+1), p(x0+1, y0+1)
			mix := func(i int) byte {
				top := p00[i] + (p10[i]-p00[i])*tx
				bot := p01[i] + (p11[i]-p01[i])*tx
				return byte(top + (bot-top)*ty + 0.5)
			}
			idx := y*dst.Stride + x*4
			dst.Pix[idx], dst.Pix[idx+1], dst.Pix[idx+2], dst.Pix[idx+3] = mix(0), mix(1), mix(2), mix(3)
		}
	}
}
