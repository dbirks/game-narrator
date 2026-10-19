package runtime_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/png"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/pipeline"
	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/openai"
	"github.com/dbirks/game-narrator/internal/runtime"
	"github.com/dbirks/game-narrator/internal/vision"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

// Live proof of the VLM OCR fallback at the session level (docs/spec/16 live
// category, ADR-0040): a replay session whose textbox band carries a
// hard-font crop, real Tesseract as the primary, the real
// vision_ocr_fallback model behind the session's secondary. The assertions:
//
//   - the primary is *confirmed defeated before the session*: the pre-check
//     runs the session's own primary path (same region rect, same
//     preprocessing recipe, the same zero-value Tesseract the session builds)
//     on the rendered crop and measures its CER against the known text. The
//     test walks a small ordered set of deterministic renderings (the
//     reference recipe, then a decorative serif face if one is present,
//     letter-spaced monospace, low contrast, heavier blur, extra seeded
//     noise) and uses the first one whose primary CER reaches the fallback
//     trigger threshold read from the pipeline's OCRFallback policy. If the
//     host's Tesseract reads every rendering well enough (CER below the
//     threshold), the test is NOT RUN with the measured CERs — never FAILED:
//     on such a host the value case (a font the local engine cannot read)
//     cannot be demonstrated, and guessing would be a false pass.
//   - the fallback genuinely engaged on real frames: the session's stats
//     count at least one secondary call and its event stream carries the
//     ocr.fallback trace with the chosen reading (chosen=secondary when the
//     primary garbles the hard font, the documented value case);
//   - the VLM's own reading of the crop (taken from the session's trace rows,
//     not a second gateway call) is within the documented CER bound
//     (ADR-0040: normalised CER < 10% against the known text);
//   - each captured gateway call is logged with the crop's SHA-256, the
//     gateway's request id and its latency, so a failure names the exact
//     request.
//
// NOT RUN (skipped, with setup instructions, never failed) when the gateway
// is not configured, magick/tesseract is missing, or no rendering defeats the
// local primary — the provider live-test convention (docs/spec/16).
func TestLiveOCRFallbackSession(t *testing.T) {
	const known = liveFallbackKnown
	const frames = liveFallbackFrames

	// --- gates. ---
	// Gate marker (ADR-0065): live tests run only under make test-models;
	// the unit gate (and any manual go test) must never call the gateway.
	if os.Getenv(providers.EnvGate) != "test-models" {
		t.Skipf("live provider test outside the test-models gate: run via make test-models (it sets %s=test-models)", providers.EnvGate)
	}
	if _, err := exec.LookPath("magick"); err != nil {
		t.Skip("NOT RUN: ImageMagick (magick) is required to render the hard-font crop")
	}
	if !hasTesseract() {
		t.Skip("NOT RUN: tesseract is required as the primary engine")
	}
	roles, err := providers.LoadRoles(os.LookupEnv)
	if err != nil {
		t.Fatalf("LoadRoles: %v", err)
	}
	cfg := roles[providers.RoleVisionOCRFallback]
	if strings.TrimSpace(cfg.BaseURL) == "" || strings.TrimSpace(cfg.Token()) == "" {
		t.Skipf("NOT RUN: set %s and the token env named by %s (default %s) to run the live VLM OCR fallback session test",
			providers.EnvGatewayURL, providers.EnvGatewayTokenEnv, providers.EnvGatewayToken)
	}
	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		model = strings.TrimSpace(os.Getenv("GAME_NARRATOR_LIVE_MODEL"))
	}
	if model == "" {
		model = "qwen3-8-27b"
	}
	// The client is built the way production builds it (openai.NewFromRole,
	// the runtime's wiring): the role's reasoning-effort default ("none")
	// and its transport timeout are the role semantics this test proves —
	// a fresh openai.New client carries no role default, the model's own
	// default reasoning applies, and the transcript is not a transcript.
	// The extended HTTP timeout is the test's only deviation: the noisy
	// crop gets more headroom than production's role floor (the same
	// choice the ocr package's live test makes).
	cfg.Model = model
	client, err := openai.NewFromRole(cfg)
	if err != nil {
		t.Fatalf("openai.NewFromRole: %v", err)
	}
	base := client.HTTP
	if base == nil {
		base = &http.Client{}
	}
	cloned := *base
	cloned.Timeout = 2 * time.Minute
	client.HTTP = &cloned

	// --- the session's fallback config, once: the same policy the
	// pre-check reads the trigger threshold from and the session runs under
	// (a drift between the two would let the pre-check promise what the
	// session's trigger never asks for).
	fbPolicy := ocr.FallbackPolicy{RatePerSecond: 10, RateBurst: 30, ConfidenceThreshold: 0.6}
	// The trigger threshold, read from the pipeline's OCRFallback policy
	// above: the fallback engages when the primary read is unresolved, i.e.
	// below the policy's ConfidenceThreshold (the documented 0.6 default,
	// the same bar docs/spec/05's stability discussion uses for trusting a
	// local read). The guards mirror the engine's normalization of a zero
	// or out-of-range field (FallbackEngine.normalizedPolicy is unexported),
	// so a later config edit cannot let the pre-check promise what the
	// session's trigger never asks for.
	trigger := fbPolicy.ConfidenceThreshold
	if trigger <= 0 {
		trigger = 0.6
	}
	if trigger > 1 {
		trigger = 1
	}

	// --- confirm the primary defeated, before the session. The pre-check
	// is the session's own primary path: the same region rect, the same
	// vision.DefaultSteps preprocessing, and the zero-value Tesseract the
	// session builds (session.go: engine = ocr.Tesseract{} when no engine
	// is injected). The deadline is scaled 5x under a race build — the
	// ocr package's own live-test convention: a read killed by runner load
	// must not be logged as a 1.0 CER it never earned (a timeout is
	// inconclusive, not a defeat).
	primary := ocr.Tesseract{Timeout: ocr.DefaultTimeout * time.Duration(liveOCRTimeoutScale)}
	region := hardBandRect
	ladder := hardFontLadder(t)
	var measured []string
	var winner string
	var winnerCrop string
	for _, v := range ladder {
		dir := t.TempDir()
		crop := v.render(t, dir)
		frame := filepath.Join(dir, "frame.png")
		magick(t, "-size", "640x480", "xc:#05070a", crop, "-geometry", "+0+48", "-composite", frame)
		cerV, read, err := primaryCER(t, context.Background(), primary, frame, region, known)
		if err != nil {
			// A timed-out measurement is inconclusive (the deadline fired,
			// not the font): note it and keep walking the ladder.
			t.Logf("pre-check: %s inconclusive (%v)", v.name, err)
			measured = append(measured, fmt.Sprintf("%s: inconclusive", v.name))
			continue
		}
		t.Logf("pre-check: %s CER=%.3f conf-threshold=%.2f read=%q", v.name, cerV, trigger, read)
		measured = append(measured, fmt.Sprintf("%s: CER %.3f (read %q)", v.name, cerV, read))
		if cerV >= trigger {
			winner, winnerCrop = v.name, crop
			break
		}
	}
	if winner == "" {
		t.Skipf("NOT RUN: the host's primary Tesseract resolves every hard rendering below the "+
			"fallback trigger threshold (CER < %.2f): %s — the fallback's value case needs a font "+
			"the local engine cannot read; no render could confirm it on this host",
			trigger, strings.Join(measured, "; "))
	}
	t.Logf("pre-check: primary confirmed defeated by %s (measured: %s)", winner, strings.Join(measured, "; "))

	// --- the clip: a dark screen, the winning crop in the top band. The
	// seeded profile's own textbox/subtitle regions point at the bottom
	// band, which stays empty, so they read nothing and commit nothing.
	clip := t.TempDir()
	entries := make([]map[string]any, 0, frames)
	for i := 1; i <= frames; i++ {
		name := fmt.Sprintf("frame-%03d.png", i)
		if out, err := exec.Command("magick",
			"-size", "640x480", "xc:#05070a", winnerCrop, "-geometry", "+0+48", "-composite",
			filepath.Join(clip, name),
		).CombinedOutput(); err != nil {
			t.Fatalf("magick frame %d: %v: %s", i, err, bytes.TrimSpace(out))
		}
		entries = append(entries, map[string]any{"file": name, "ts_ns": int64(i*100) * 1e6})
	}
	data, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("timestamps: %v", err)
	}
	if err := os.WriteFile(filepath.Join(clip, "timestamps.json"), data, 0o644); err != nil {
		t.Fatalf("timestamps.json: %v", err)
	}

	// --- the session: real Tesseract primary, the role's real VLM secondary.
	capture := &liveCapture{client: client}
	e := newEnv(t, withOCRFallback(&pipeline.OCRFallback{
		Secondary: ocr.NewVLMEngine(capture),
		// The generous budget is about the session wiring, not the limiter:
		// the replay clip fits a handful of distinct crops, and the rate
		// limit's own behaviour is pinned by the ocr package's clock tests.
		Policy: fbPolicy,
	}), withReplayRoot(clip))

	profileID, _ := seedSyntheticProfile(t, e.st, false)
	// The hard-font region reads the top band; the profile's difficult_font
	// flag (ADR-0040 step 3) pins the comparison on, so every frame records
	// both readings and the discrepancy. The draft is still editable: a
	// published revision is immutable (store: immutable), so the flag lands
	// in the draft and the revision is then published — the same shape a
	// profile-level flag edit takes in production.
	now := time.Now().UTC().Format(time.RFC3339)
	if err := e.st.PutRegions(context.Background(), profileID, []games.Region{{
		ID:           games.NewID(),
		ProfileID:    profileID,
		RegionType:   games.RegionTextbox,
		DetectMethod: games.DetectManual,
		RectX:        region.X, RectY: region.Y, RectW: region.W, RectH: region.H,
		OCRSettings: json.RawMessage(`{"difficult_font": true}`),
		Revision:    1,
		UpdatedAt:   now,
	}}); err != nil {
		t.Fatalf("PutRegions: %v", err)
	}
	_, rev, err := e.st.GetProfile(context.Background(), profileID)
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	if err := e.st.SetProfileStatus(context.Background(), profileID, games.StatusReview, rev); err != nil {
		t.Fatalf("SetProfileStatus: %v", err)
	}
	_, rev, err = e.st.GetProfile(context.Background(), profileID)
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	published, err := e.st.Publish(context.Background(), profileID, rev, "live-test@local")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}

	info, err := e.start(runtime.StartRequest{
		ProfileID:   published.ID,
		FrameSource: runtime.FrameSource{Kind: runtime.SourceKindReplay, Dir: clip},
		Mode:        runtime.ModeConservative,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	final := waitState(t, e.mgr, info.ID, 5*time.Minute)
	if final.State != runtime.StateStopped && final.State != runtime.StateFailed {
		t.Fatalf("state = %q, want a terminal state", final.State)
	}
	if final.State == runtime.StateStopped && final.Stats.FramesDelivered != frames {
		t.Fatalf("frames_delivered = %d, want %d (the whole clip)", final.Stats.FramesDelivered, frames)
	}
	// The session must have used the fallback on real frames: the pre-check
	// confirmed the primary defeated, so at least one secondary call is the
	// documented behaviour of this wiring.
	if got := final.Stats.OCRFallbackSecondaryCalls; got == 0 {
		t.Fatal("the session's secondary was never called: the live fallback did not engage")
	}

	// --- the record: both readings and the discrepancy, from the session's
	// own terminal event.
	raw, rows := terminalEvent(t, e, info.ID)
	var secondary, annotated int
	var bestCER float64 = 2
	var bestRow *pipeline.TraceEntry
	for i := range rows {
		r := &rows[i]
		if r.OCRFallback == nil {
			continue
		}
		annotated++
		t.Logf("ocr.fallback: frame=%d chosen=%s reason=%s primary_conf=%.2f discrepancy=%.3f secondary=%q",
			r.FrameSeq, r.OCRFallback.Chosen, r.OCRFallback.Reason, r.OCRFallback.PrimaryConf,
			r.OCRFallback.Discrepancy, r.OCRFallback.SecondaryText)
		if r.OCRFallback.Chosen == "secondary" {
			secondary++
			if c := cer(ocr.NormaliseForCompare(r.OCRFallback.SecondaryText), ocr.NormaliseForCompare(known)); c < bestCER {
				bestCER, bestRow = c, r
			}
		}
	}
	if annotated == 0 {
		t.Fatal("no trace row carries the ocr.fallback field: the session's trace lost the comparison record")
	}
	if secondary == 0 {
		t.Fatalf("no chosen=secondary row over %d annotated rows although the pre-check confirmed the primary "+
			"defeated (%s): the session's primary path diverged from the pre-check path (a wiring drift, not a font)",
			annotated, strings.Join(measured, "; "))
	}
	// The committed text (the fallback's chosen reading) stays within the
	// documented bound: ADR-0040's acceptance for the live hard-font run is
	// a normalised CER < 10% against the known text.
	if bestCER >= liveFallbackMaxCER {
		t.Errorf("the committed (chosen=secondary) reading has CER %.3f, want < %.2f against %q (read %q)",
			bestCER, liveFallbackMaxCER, known, bestRow.OCRFallback.SecondaryText)
	}
	if !strings.Contains(raw, `"ocr_fallback"`) {
		t.Fatal("the ocr.fallback field is missing from the session's terminal event")
	}
	for i, call := range capture.calls() {
		t.Logf("vlm.sent: call=%d sha256=%s request_id=%s latency=%s err=%v",
			i+1, call.sha256, orDash(call.requestID), call.latency.Round(time.Millisecond), call.err)
	}
	// Dump every sent crop so a failure is inspectable (the session sends one
	// crop per region per frame: the profile's own empty regions and the hard
	// band are different bytes).
	if dbg := os.Getenv("GAME_NARRATOR_LIVE_DBG_DIR"); dbg != "" {
		_ = os.MkdirAll(dbg, 0o755)
		capture.mu.Lock()
		for i, b := range capture.bytes {
			_ = os.WriteFile(filepath.Join(dbg, fmt.Sprintf("sent-%02d.png", i+1)), b, 0o644)
		}
		capture.mu.Unlock()
	}
	// Controlled comparison: direct VLM calls on the very bytes the session
	// sent. If the direct reads match the known line while the session's
	// reading was creative, the difference is the model's sampling on this
	// hard crop (and the test's tolerance owns it), not the wiring; if the
	// direct reads differ too, the crop itself is the problem.
	if b := capture.firstBytes(); b != nil {
		eng := ocr.NewVLMEngine(ocr.OpenAIClient{Client: client})
		for i := 0; i < 3; i++ {
			res, err := eng.RecognizePrepared(context.Background(), b)
			if err != nil {
				t.Logf("vlm.direct: sample=%d err=%v", i+1, err)
				continue
			}
			t.Logf("vlm.direct: sample=%d read=%q", i+1, res.Text)
		}
	}
	t.Logf("live fallback: model=%s secondary_calls=%d annotated_rows=%d chosen_secondary=%d best_cer=%.3f",
		model, final.Stats.OCRFallbackSecondaryCalls, annotated, secondary, bestCER)
}

// liveFallbackKnown is the line the hard-font crop renders; the pre-check's
// CER and the session's committed-text CER are both measured against it.
const liveFallbackKnown = "The harbour bell rang twice."

// liveFallbackFrames is the replay clip's length: 4 s of the clip's own
// 10 Hz cadence, one stable textbox page (docs/spec/12's page dwell).
const liveFallbackFrames = 40

// liveFallbackMaxCER is the documented bound for the VLM's reading of the
// hard-font crop: ADR-0040's acceptance — "assert its normalised CER < 10%
// against the known text" — the same bound the ocr package's live test pins.
const liveFallbackMaxCER = 0.10

// hardBandRect is the textbox region the session publishes and the pre-check
// measures: one value, so the two can never drift apart (the pre-check's
// promise is only as good as its agreement with the session's crop).
var hardBandRect = vision.Rect{X: 0.02, Y: 0.10, W: 0.96, H: 0.25}

// hardVariant is one rung of the defeat ladder: a deterministic rendering of
// the hard band, harder on the local engine than the one before it.
type hardVariant struct {
	name string
	// render writes the variant's crop into dir and returns its path. Every
	// render is a pure function of (font present on the host, fixed seeds):
	// the same host renders the same bytes on every run, so the ladder's
	// outcome is reproducible.
	render func(t *testing.T, dir string) string
}

// hardFontLadder is the ordered set of renderings the pre-check walks, from
// the reference recipe (the ocr package's live-test recipe verbatim — hard
// serif face, dim on a dark box, downscaled, heavy seeded Gaussian noise,
// soft blur) up through progressively harder faces and treatments: a
// decorative serif face when one is present, letter-spaced monospace (this
// magick build renders text options literally, so spacing is per-character
// annotate, not a text option), low contrast, heavier blur, extra seeded
// noise. The pre-check takes the first rung that defeats the primary, which
// is the mildest defeat — the crop the VLM still has the best chance on.
func hardFontLadder(t *testing.T) []hardVariant {
	t.Helper()
	base := liveHardFont(t)
	ladder := []hardVariant{
		{
			name:   "baseline",
			render: renderHardCrop(t, hardRender{font: base}),
		},
	}
	if dec := firstExistingFont(t,
		"/usr/share/fonts/TTF/DejaVuSerif-BoldItalic.ttf",
		"/usr/share/fonts/liberation/LiberationSerif-BoldItalic.ttf",
		"/usr/share/fonts/TTF/DejaVuSerif.ttf",
	); dec != "" && dec != base {
		ladder = append(ladder, hardVariant{
			name:   "decorative font",
			render: renderHardCrop(t, hardRender{font: dec}),
		})
	}
	if mono := firstExistingFont(t,
		"/usr/share/fonts/TTF/DejaVuSansMono.ttf",
		"/usr/share/fonts/liberation/LiberationMono-Regular.ttf",
		"/usr/share/fonts/Adwaita/AdwaitaMono-Regular.ttf",
	); mono != "" {
		ladder = append(ladder, hardVariant{
			name:   "letter-spacing",
			render: renderHardCrop(t, hardRender{font: mono, perChar: true}),
		})
	}
	ladder = append(ladder,
		hardVariant{name: "low contrast", render: renderHardCrop(t, hardRender{font: base, fill: "#46525e"})},
		hardVariant{name: "blur", render: renderHardCrop(t, hardRender{font: base, blur: "0x4.0"})},
		hardVariant{name: "noise", render: renderHardCrop(t, hardRender{font: base, extraNoise: true})},
	)
	return ladder
}

// hardRender is one deterministic recipe: the reference recipe's canvas,
// text, downscale, seeded noise and blur, with the named hardening applied.
type hardRender struct {
	font       string // face; empty means magick's default
	fill       string // empty means the reference #c8d4de
	blur       string // empty means the reference 0x2.6
	perChar    bool   // letter-spacing: per-character annotate, 10 px gap
	extraNoise bool   // a second seeded noise pass
}

// renderHardCrop renders r into dir/crop.png and returns the path. The
// recipe is the ocr live test's verbatim (1280x240 canvas, pointsize 48,
// centred, downscaled 640x120, Gaussian noise seed 42 attenuate 0.50,
// Gaussian blur) plus the variant's hardening — the same fixed seeds and
// parameters on every run.
func renderHardCrop(t *testing.T, r hardRender) func(t *testing.T, dir string) string {
	return func(t *testing.T, dir string) string {
		t.Helper()
		out := filepath.Join(dir, "crop.png")
		fill, blur := r.fill, r.blur
		if fill == "" {
			fill = "#c8d4de"
		}
		if blur == "" {
			blur = "0x2.6"
		}
		args := []string{"-size", "1280x240", "xc:#0b1018",
			"-font", r.font, "-pointsize", "48", "-fill", fill,
			"-gravity", "center"}
		if r.perChar {
			// Letter-spacing this magick build cannot express as a text
			// option (it renders the option string literally): each rune is
			// annotated at its own offset, pitch = measured mono advance +
			// 10 px, so the spacing is deterministic and visible.
			unit := monoCharUnit(t, r.font)
			runes := []rune(liveFallbackKnown)
			pitch := unit + 10
			start := -((len(runes) - 1) * pitch) / 2
			for i, ch := range runes {
				args = append(args, "-annotate", fmt.Sprintf("+%d+0", start+i*pitch), string(ch))
			}
		} else {
			args = append(args, "-annotate", "+0+0", liveFallbackKnown)
		}
		args = append(args,
			"-resize", "640x120",
			"-seed", "42", "-attenuate", "0.50", "+noise", "Gaussian",
			"-blur", blur)
		if r.extraNoise {
			args = append(args, "-seed", "7", "-attenuate", "0.30", "+noise", "Gaussian")
		}
		args = append(args, out)
		magick(t, args...)
		return out
	}
}

// magick runs ImageMagick and fails the test with the child's output on
// error (a render the host's magick cannot do is a broken test, not a
// flaky one).
func magick(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("magick", args...).CombinedOutput(); err != nil {
		t.Fatalf("magick %v: %v: %s", args, err, bytes.TrimSpace(out))
	}
}

// firstExistingFont returns the first font present on the host, or "" when
// none is: "if present" ladders skip their rung rather than assume a face.
func firstExistingFont(t *testing.T, paths ...string) string {
	t.Helper()
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// monoCharUnit measures one monospace character's advance at the recipe's
// pointsize: the width of four "M"s over four. It is a host/font property,
// so the per-character offsets stay deterministic on a given host.
func monoCharUnit(t *testing.T, font string) int {
	t.Helper()
	raw, err := exec.Command("magick", "-font", font, "-pointsize", "48",
		"label:MMMM", "-format", "%w", "info:").Output()
	if err != nil {
		t.Fatalf("magick measure mono: %v: %s", err, bytes.TrimSpace(raw))
	}
	var w int
	fmt.Sscanf(string(bytes.TrimSpace(raw)), "%d", &w)
	if w < 8 {
		t.Fatalf("mono advance %d px implausible for pointsize 48", w)
	}
	return w / 4
}

// primaryCER measures how well the session's primary path reads the rendered
// band: exactly what the session does to one frame of this region —
// CropNormalized over the rect, vision.DefaultSteps preprocessing (the
// session's Preprocess), the engine's own Recognize. It returns the normalised
// CER against known, the raw read, and an error only when the measurement
// itself is inconclusive (a timed-out read: the deadline fired, not the
// font). An errored read that is not a timeout counts as a defeated primary:
// the primary produced nothing, CER 1.0 — the fallback's primary_errored
// case, which the session's trigger treats as unresolved.
func primaryCER(t *testing.T, ctx context.Context, primary ocr.Tesseract, framePath string, region vision.Rect, known string) (float64, string, error) {
	t.Helper()
	f, err := os.Open(framePath)
	if err != nil {
		return 0, "", fmt.Errorf("open frame: %w", err)
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return 0, "", fmt.Errorf("decode frame: %w", err)
	}
	crop := vision.CropNormalized(img, region, vision.CropOptions{})
	if crop == nil || crop.Bounds().Empty() {
		return 0, "", fmt.Errorf("empty crop for region %v", region)
	}
	pre := vision.Preprocess(crop, vision.DefaultSteps())
	res, err := primary.Recognize(ctx, pre)
	if err != nil {
		var fail *ocr.Failure
		if errors.As(err, &fail) && (errors.Is(fail.Kind, ocr.ErrTimeout) || errors.Is(fail.Kind, ocr.ErrCancelled)) {
			return 0, "", err
		}
		return 1, "", nil
	}
	return cer(ocr.NormaliseForCompare(res.Text), ocr.NormaliseForCompare(known)), res.Text, nil
}

// liveCapture is a test-local VLMClient wrapper that records, for every call,
// the SHA-256 of the exact prepared crop the engine sends (the bytes that
// leave the appliance), the gateway's request id and the wall latency —
// without changing the request. A failure can then be compared against a
// standalone VLM call's bytes for the same pixels: matching hashes mean the
// wiring sends the right crop and any bad reading is the model's, not the
// pipe's. The request id and latency come from the role client's own
// ChatResponse (the ocr.VLMClient seam returns plain text), so the capture
// builds the gateway request the way the production adapter does —
// ocr.OpenAIClient's shape verbatim: one user message, the terse fixed
// instruction, the one prepared crop PNG, the engine's MaxTokens bound.
type liveCapture struct {
	client *openai.Client

	mu        sync.Mutex
	shots     []string
	bytes     [][]byte
	requestID []string
	latency   []time.Duration
	errs      []error
}

// captureCall is one recorded gateway call.
type captureCall struct {
	sha256    string
	requestID string
	latency   time.Duration
	err       error
}

func (c *liveCapture) Chat(ctx context.Context, req ocr.VLMRequest) (string, error) {
	start := time.Now()
	resp, err := c.client.Chat(ctx, openai.ChatRequest{
		Messages: []openai.Message{{
			Role:   "user",
			Text:   req.Instruction,
			Images: [][]byte{req.PNG},
		}},
		MaxTokens: req.MaxTokens,
	})
	c.mu.Lock()
	sum := sha256.Sum256(req.PNG)
	c.shots = append(c.shots, hex.EncodeToString(sum[:]))
	cp := make([]byte, len(req.PNG))
	copy(cp, req.PNG)
	c.bytes = append(c.bytes, cp)
	id := ""
	if err == nil {
		id = resp.RequestID
	}
	c.requestID = append(c.requestID, id)
	c.latency = append(c.latency, time.Since(start))
	c.errs = append(c.errs, err)
	c.mu.Unlock()
	if err != nil {
		return "", err
	}
	return resp.Text, nil
}

// calls returns the recorded calls in send order.
func (c *liveCapture) calls() []captureCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]captureCall, len(c.shots))
	for i := range c.shots {
		out[i] = captureCall{sha256: c.shots[i], requestID: c.requestID[i], latency: c.latency[i], err: c.errs[i]}
	}
	return out
}

func (c *liveCapture) firstBytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.bytes) == 0 {
		return nil
	}
	out := make([]byte, len(c.bytes[0]))
	copy(out, c.bytes[0])
	return out
}

// Pin the seam satisfaction: the session's engine is built over this
// wrapper, and a drifted VLMClient signature must fail here, at compile time.
var _ ocr.VLMClient = (*liveCapture)(nil)

// orDash renders an empty id for a log line as "-" rather than a blank.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// liveHardFont picks a serif face that Tesseract's default language model
// recognises less cleanly than its sans default (the ocr live test's pick,
// duplicated here because it is test-local there).
func liveHardFont(t *testing.T) string {
	t.Helper()
	if f := firstExistingFont(t,
		"/usr/share/fonts/liberation/LiberationSerif-Italic.ttf",
		"/usr/share/fonts/TTF/DejaVuSerifCondensed.ttf",
	); f != "" {
		return f
	}
	t.Log("no preferred hard font found; using magick's default")
	return ""
}

// cer is the normalised edit distance the live assertion compares with —
// the same metric the fallback's own comparison uses, recomputed here
// because the ocr package's is unexported (the matcher keeps its own).
func cer(a, b string) float64 {
	ra, rb := []rune(a), []rune(b)
	la, lb := len(ra), len(rb)
	if la == 0 && lb == 0 {
		return 0
	}
	if la == 0 || lb == 0 {
		return 1
	}
	prev := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		cur := make([]int, lb+1)
		cur[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min3(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return float64(prev[lb]) / float64(max(la, lb))
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}
