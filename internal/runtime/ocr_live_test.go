package runtime_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

// Live proof of the VLM OCR fallback at the session level (docs/spec/16 live
// category, ADR-0040): a replay session whose textbox band carries the
// hard-font crop rendered with the same magick recipe the ocr package's VLM
// live test uses (same font pick, same noise and blur), real Tesseract as
// the primary, the real vision_ocr_fallback model behind the session's
// secondary. The assertions:
//
//   - the fallback genuinely engaged on real frames: the session's stats
//     count the secondary's calls and its event stream carries the trace;
//   - the ocr.fallback trace field appears in the session's terminal event
//     with the chosen reading (chosen=secondary when the primary garbles
//     the hard font, the documented value case);
//   - the VLM's own reading of the crop (taken from the session's trace
//     rows, not a second gateway call) is within the documented CER.
//
// NOT RUN (skipped, with setup instructions, never failed) when the gateway
// is not configured, the gateway refuses image input, or magick/tesseract is
// missing — the provider live-test convention (docs/spec/16).
func TestLiveOCRFallbackSession(t *testing.T) {
	const known = "The harbour bell rang twice."
	const frames = 40

	// --- gates. ---
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

	// The capture wrapper records the exact PNG bytes the engine sends (the
	// prepared crop) so a failure can be compared against the bytes a
	// standalone VLM call sends for the same pixels: if the hashes match, the
	// read differs by model sampling, not by wiring.
	capture := &liveCapture{inner: ocr.OpenAIClient{Client: client}}

	// --- render the crop: the ocr live test's recipe verbatim — hard serif
	// face, dim on a dark box, downscaled, heavy Gaussian noise, soft blur.
	// Tuned so the local engine comes back garbled while the VLM resolves
	// the line: the fallback's value case.
	clipDir := t.TempDir()
	crop := filepath.Join(clipDir, "crop.png")
	font := liveHardFont(t)
	args := []string{
		"-size", "1280x240", "xc:#0b1018",
		"-font", font, "-pointsize", "48", "-fill", "#c8d4de",
		"-gravity", "center", "-annotate", "+0+0", known,
		"-resize", "640x120",
		"-seed", "42", "-attenuate", "0.50", "+noise", "Gaussian",
		"-blur", "0x2.6",
		crop,
	}
	if outBytes, err := exec.Command("magick", args...).CombinedOutput(); err != nil {
		t.Fatalf("magick render: %v: %s", err, bytes.TrimSpace(outBytes))
	}

	// --- the clip: a dark screen, the crop in the top band. The seeded
	// profile's own textbox/subtitle regions point at the bottom band,
	// which stays empty, so they read nothing and commit nothing.
	clip := t.TempDir()
	entries := make([]map[string]any, 0, frames)
	for i := 1; i <= frames; i++ {
		name := fmt.Sprintf("frame-%03d.png", i)
		if out, err := exec.Command("magick",
			"-size", "640x480", "xc:#05070a", crop, "-geometry", "+0+48", "-composite",
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
	e := newEnv(t, withOCRFallback(&pipeline.OCRFallback{
		Secondary: ocr.NewVLMEngine(capture),
		// The generous budget is about the session wiring, not the limiter:
		// the replay clip fits a handful of distinct crops, and the rate
		// limit's own behaviour is pinned by the ocr package's clock tests.
		Policy: ocr.FallbackPolicy{RatePerSecond: 10, RateBurst: 30},
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
		RectX:        0.02, RectY: 0.10, RectW: 0.96, RectH: 0.25,
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
		t.Fatalf("no chosen=secondary row over %d annotated rows: the primary resolved the hard font on this host "+
			"(the value case is the hard profile; rerun once the font profile is confirmed defeated)", annotated)
	}
	if bestCER >= 0.10 {
		t.Errorf("the secondary's reading has CER %.3f, want < 0.10 against %q (read %q)",
			bestCER, known, bestRow.OCRFallback.SecondaryText)
	}
	if !strings.Contains(raw, `"ocr_fallback"`) {
		t.Fatal("the ocr.fallback field is missing from the session's terminal event")
	}
	for i, h := range capture.shotList() {
		t.Logf("vlm.sent: call=%d sha256=%s", i+1, h)
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

// liveCapture is a test-local VLMClient wrapper that records the hash of
// each prepared crop the engine sends — the exact bytes that leave the
// appliance — without changing the request. A failure can then be compared
// against a standalone VLM call's bytes for the same pixels: matching hashes
// mean the wiring sends the right crop and any bad reading is the model's,
// not the pipe's.
type liveCapture struct {
	inner ocr.VLMClient
	mu    sync.Mutex
	shots []string
	bytes [][]byte
}

func (c *liveCapture) Chat(ctx context.Context, req ocr.VLMRequest) (string, error) {
	c.mu.Lock()
	sum := sha256.Sum256(req.PNG)
	c.shots = append(c.shots, hex.EncodeToString(sum[:]))
	cp := make([]byte, len(req.PNG))
	copy(cp, req.PNG)
	c.bytes = append(c.bytes, cp)
	c.mu.Unlock()
	return c.inner.Chat(ctx, req)
}

func (c *liveCapture) shotList() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.shots))
	copy(out, c.shots)
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

// liveHardFont picks a serif face that Tesseract's default language model
// recognises less cleanly than its sans default (the ocr live test's pick,
// duplicated here because it is test-local there).
func liveHardFont(t *testing.T) string {
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
