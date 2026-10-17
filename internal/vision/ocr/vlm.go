// VLMEngine is the docs/spec/05 "optional VLM fallback for difficult fonts":
// a vision-language model that transcribes the textbox crop the local OCR
// engine could not resolve. It implements Engine over the same Result, so the
// recognition stage (and its callers) never branches on which engine produced
// a hypothesis.
//
// Privacy (docs/spec/14) is enforced here, at the only place a frame can leave
// the appliance through this role: the request carries the *crop* — downscaled
// to MaxSide pixels on the long side, re-encoded as PNG — never a full frame.
// Nothing else about the scene, the profile or the session is sent.
package ocr

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
	"strings"
	"time"
)

// VLMRequest is one VLM call: a terse instruction and one PNG. The shape is
// deliberately smaller than the provider's chat request — the provider
// adapter (runtime's job, ADR-0039 §5) fills in the role, the model, the
// reasoning effort and the timeout; this package owns only what the crop and
// the answer contract need.
type VLMRequest struct {
	Instruction string
	PNG         []byte
	// MaxTokens bounds the answer; the adapter must apply it (and must not
	// silently raise it).
	MaxTokens int
}

// VLMClient is the provider seam VLMEngine speaks. The answer is plain text
// (JSON mode is not available on the configured gateway, so the caller
// validates what comes back). Implementations must honor ctx and must not
// return the image bytes in an error: a 1 MB PNG in a log line is a privacy
// incident, not detail.
type VLMClient interface {
	Chat(ctx context.Context, req VLMRequest) (string, error)
}

// VLMEngine recognizes one crop with a vision-language model. Zero-value is
// not usable; NewVLMEngine fills the defaults (MaxSide 1024, MaxTokens 256).
type VLMEngine struct {
	client    VLMClient
	maxSide   int
	maxTokens int
}

// Option customizes a VLMEngine.
type Option func(*VLMEngine)

// WithMaxSide bounds the long side of the sent crop in pixels (default 1024).
// FallbackEngine keys its cache on the prepared crop's bytes, so a fallback
// policy and its VLM engine must share this value.
func WithMaxSide(n int) Option {
	return func(e *VLMEngine) {
		if n >= 32 {
			e.maxSide = n
		}
	}
}

// WithMaxTokens bounds the model's answer in tokens (default 256). A textbox
// line is short; the bound exists so a runaway answer cannot cost the session
// a full minute of latency or the gateway a full context.
func WithMaxTokens(n int) Option {
	return func(e *VLMEngine) {
		if n >= 16 {
			e.maxTokens = n
		}
	}
}

// NewVLMEngine builds a VLMEngine over client with the defaults above.
func NewVLMEngine(client VLMClient, opts ...Option) *VLMEngine {
	e := &VLMEngine{client: client, maxSide: 1024, maxTokens: 256}
	for _, o := range opts {
		o(e)
	}
	return e
}

// vlmInstruction is the whole prompt. It is terse on purpose: the model is
// asked for a transcription, not a commentary, and the output contract ("only
// the text", "NONE" when nothing is readable) is what parseVLMAnswer keys on.
const vlmInstruction = "Transcribe the text in this game dialogue box exactly. " +
	"Output only the text. If there is no readable text, output NONE."

// vlmConfidencePrior is the confidence a VLM reading carries on its own. The
// model has no token logprobs here, so the number is a documented prior, not a
// measurement (ADR-0039 §3): it sits below the Tesseract threshold that
// triggers the fallback, an agreeing primary corroborates it upward, and it is
// never 1.0 — a remote model's assertion is evidence, not certainty.
const vlmConfidencePrior = 0.6

// Name implements Namer.
func (e *VLMEngine) Name() string { return "vlm" }

// MaxSide reports the long-side bound the engine downscales to. FallbackEngine
// uses it to prepare the crop whose bytes it hashes for the cache, so the two
// must agree; reading it back is how a caller proves they do.
func (e *VLMEngine) MaxSide() int { return e.maxSide }

// Recognize implements Engine: downscale the crop, encode it, ask the model.
func (e *VLMEngine) Recognize(ctx context.Context, img image.Image) (Result, error) {
	png, err := encodeCrop(img, e.maxSide)
	if err != nil {
		return Result{}, err
	}
	return e.RecognizePrepared(ctx, png)
}

// RecognizePrepared implements PreparedRecognizer: recognize a crop that is
// already the exact PNG the engine would send. FallbackEngine calls it so the
// cache key is the SHA-256 of the very bytes that leave the appliance, and a
// "sent once per stable textbox" claim is a statement about a hash, not an
// assumption about an encoder.
func (e *VLMEngine) RecognizePrepared(ctx context.Context, pngBytes []byte) (Result, error) {
	start := now()
	if ctx == nil {
		ctx = context.Background()
	}
	text, err := e.client.Chat(ctx, VLMRequest{Instruction: vlmInstruction, PNG: pngBytes, MaxTokens: e.maxTokens})
	if err != nil {
		return Result{}, err
	}
	done := now()
	ans := parseVLMAnswer(text)
	if ans == "" {
		// NONE or empty: the model found no readable text. That is a legitimate
		// reading, not a failure — an empty read is what a caller's
		// "unresolved" decision is computed from.
		return Result{Engine: e.Name(), Duration: done.Sub(start), MeanConf: 0}, nil
	}
	res := Result{
		Text:     ans,
		Engine:   e.Name(),
		Duration: done.Sub(start),
		// A single flat confidence for the whole read: the model returned no
		// per-word boxes, so there is no finer grain to invent. vlmConfidencePrior
		// is the documented heuristic (ADR-0039 §3); FallbackEngine replaces it
		// with the agreement-based value when both readings are on record.
		MeanConf: vlmConfidencePrior,
	}
	res.Lines = []Line{{Text: ans, Conf: vlmConfidencePrior, Box: image.Rectangle{}}}
	for _, w := range strings.Fields(ans) {
		res.Words = append(res.Words, Word{Text: w, Conf: vlmConfidencePrior})
	}
	return res, nil
}

// PreparedRecognizer is the second half of the seam FallbackEngine needs:
// engines that can recognize a pre-encoded PNG directly, so a caller can hash
// exactly the bytes the engine sends (cache key == sent content) instead of
// re-deriving them with a copy of the engine's settings.
type PreparedRecognizer interface {
	RecognizePrepared(ctx context.Context, png []byte) (Result, error)
}

// parseVLMAnswer turns the model's raw answer into reading text. It strips the
// quoting and markdown a chat model loves to add, trims, and maps the NONE
// contract (case-insensitive, alone) to "". Whatever survives is the answer
// verbatim — no rewording: the discrepancy measure is computed on this text,
// and a fallback that edited its own reading would compare itself to the
// primary on unequal ground.
func parseVLMAnswer(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	// Paired quotes, straight or typographic: a chat model that wraps its
	// answer in “quotation marks” has quoted an answer, not written one.
	// Straight quotes open and close the same; curly ones differ by direction.
	if rs := []rune(s); len(rs) >= 2 && isOpeningQuote(rs[0]) && isClosingQuote(rs[len(rs)-1]) {
		s = strings.TrimSpace(string(rs[1 : len(rs)-1]))
	}
	if strings.HasPrefix(s, "```") {
		s = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(s, "```"), "```"))
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = strings.TrimSpace(s[i+1:])
		}
	}
	if i := strings.Index(s, "\n"); i >= 0 {
		// Multi-line answers are not the contract for a single textbox; the
		// first line is the read, the rest is commentary the instruction
		// forbade. Taking line one keeps the reading usable without pretending
		// the model kept the contract.
		s = strings.TrimSpace(s[:i])
	}
	// The NONE contract, with or without the terminal mark a model appends.
	if strings.EqualFold(strings.TrimRight(s, ".!?"), "NONE") || strings.EqualFold(s, "no text") || strings.EqualFold(s, "no readable text") {
		return ""
	}
	return s
}

func isOpeningQuote(r rune) bool {
	switch r {
	case '\'', '"', '\u2018', '\u201c':
		return true
	}
	return false
}

func isClosingQuote(r rune) bool {
	switch r {
	case '\'', '"', '\u2019', '\u201d':
		return true
	}
	return false
}

// encodeCrop downscales img so its long side is at most maxSide pixels and
// returns the PNG. Downsampling (not just truncation) is what keeps the
// request small when a profile crops a 1080p textbox. The recipe is purely
// functional — the same pixels always produce the same bytes — which is what
// makes a hash of them a stable cache key across frames and across restarts
// of one session.
func encodeCrop(img image.Image, maxSide int) ([]byte, error) {
	b := img.Bounds()
	if b.Empty() {
		return nil, fmt.Errorf("ocr: crop is empty")
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
		scaleBilinear(img, b, dst)
	}
	var buf bytes.Buffer
	if err := pngEncode(&buf, dst); err != nil {
		return nil, fmt.Errorf("ocr: encode crop: %w", err)
	}
	return buf.Bytes(), nil
}

// scaleBilinear writes img (bounds b) into dst, scaled to dst's size.
//
// stdlib image/draw scales only 1:1, and the x/image package with its better
// scalers is not a dependency of this product (the CGO-less stdlib-only core
// ADR-0012's supervision design depends on). Bilinear is the right tradeoff
// for a downscale of a text crop: deterministic, allocation-free beyond dst,
// and far better than nearest-neighbour for the glyph edges a VLM reads.
func scaleBilinear(img image.Image, b image.Rectangle, dst *image.RGBA) {
	dw, dh := dst.Bounds().Dx(), dst.Bounds().Dy()
	xs, ys := float64(b.Dx())/float64(dw), float64(b.Dy())/float64(dh)
	for y := 0; y < dh; y++ {
		fy := (float64(y)+0.5)*ys - 0.5
		if fy < 0 {
			fy = 0
		}
		y0 := int(fy)
		ty := fy - float64(y0)
		if y0+1 >= b.Dy() {
			y0, ty = b.Dy()-1, 0
		}
		for x := 0; x < dw; x++ {
			fx := (float64(x)+0.5)*xs - 0.5
			if fx < 0 {
				fx = 0
			}
			x0 := int(fx)
			tx := fx - float64(x0)
			if x0+1 >= b.Dx() {
				x0, tx = b.Dx()-1, 0
			}
			p00 := pixelAt(img, b, x0, y0)
			p10 := pixelAt(img, b, x0+1, y0)
			p01 := pixelAt(img, b, x0, y0+1)
			p11 := pixelAt(img, b, x0+1, y0+1)
			mix := func(i int) byte {
				top := lerp(p00[i], p10[i], tx)
				bot := lerp(p01[i], p11[i], tx)
				return byte(lerp(top, bot, ty) + 0.5)
			}
			idx := y*dst.Stride + x*4
			dst.Pix[idx] = mix(0)
			dst.Pix[idx+1] = mix(1)
			dst.Pix[idx+2] = mix(2)
			dst.Pix[idx+3] = mix(3)
		}
	}
}

// pixelAt returns the pixel at b.Min+(x,y) as [4]float64 (R, G, B, A in
// 0–255, un-premultiplied). RGBA() is the only cross-format accessor that
// needs no type switch, and its premultiplied values are un-premultiplied
// here for the mix — with alpha carried as its own channel so a
// semi-transparent box edge scales like the pixels around it.
func pixelAt(img image.Image, b image.Rectangle, x, y int) [4]float64 {
	r, g, bl, a := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
	return [4]float64{float64(r >> 8), float64(g >> 8), float64(bl >> 8), float64(a >> 8)}
}

func lerp(a, b, t float64) float64 { return a + (b-a)*t }

// pngEncode is a thin seam over encoding/png so a test can observe the bytes
// an engine sends without reaching into a buffer; the encoder itself is the
// stdlib one and nothing here reorders or decorates the output.
var pngEncode = func(buf *bytes.Buffer, img image.Image) error {
	return png.Encode(buf, img)
}

// now is time.Now behind a variable for tests that want to pin the clock.
var now = func() time.Time { return time.Now() }

// cropHash keys the FallbackEngine cache on the exact bytes a VLM request
// sends: the SHA-256 of the prepared PNG. A stable textbox therefore produces
// one key and is sent once; a new page produces a new key.
func cropHash(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
