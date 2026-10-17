package recorded

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/openai"
)

// These are the LIVE recorded-provider contract tests: they call a real
// OpenAI-compatible gateway, record what it answered into a temporary cassette,
// and then replay that cassette with no network at all. They are the only proof
// that the cassette format describes *real* gateway traffic and not just the
// fake server used offline (docs/spec/12 "Contract" row: recorded every PR,
// live conditional).
//
// Configuration comes from the environment only — GAME_NARRATOR_GATEWAY_URL plus
// the token variable named by GAME_NARRATOR_GATEWAY_TOKEN_ENV. Nothing here
// prints or stores a URL, host or token, and the cassettes are written to
// t.TempDir(): live-recorded cassettes are never committed (AGENTS.md rule 5).

const liveSkipMessage = "NOT RUN: set " + providers.EnvGatewayURL + " and the token env named by " +
	providers.EnvGatewayTokenEnv + " (default " + providers.EnvGatewayToken + ") to run live recorded-provider tests"

const defaultLiveModel = "qwen3-8-27b"

// liveHarness holds a recording client against the real gateway plus a factory
// for a replay client that reads back what was recorded.
type liveHarness struct {
	client      *openai.Client
	recorder    *Transport
	recordProbe *originProbe

	replayFactory func() (*openai.Client, *originProbe, *neverDials)
	host          string
	dir           string
}

// newLiveHarness resolves the role through the production configuration path, so
// the live tests exercise the same env handling the service uses.
func newLiveHarness(t *testing.T, role providers.Role, timeout time.Duration) *liveHarness {
	t.Helper()
	roles, err := providers.LoadRoles(os.LookupEnv)
	if err != nil {
		// A malformed gateway URL is a configuration fault, not a missing
		// capability: fail loudly rather than skip.
		t.Fatalf("LoadRoles: %v", err)
	}
	cfg := roles[role]
	if strings.TrimSpace(cfg.BaseURL) == "" || strings.TrimSpace(cfg.Token()) == "" {
		t.Skip(liveSkipMessage)
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Host == "" {
		t.Fatalf("%s is not a usable gateway URL", providers.EnvGatewayURL)
	}
	token := cfg.Token()
	model := strings.TrimSpace(os.Getenv("GAME_NARRATOR_LIVE_MODEL"))
	if model == "" {
		model = defaultLiveModel
	}
	dir := t.TempDir()
	recorder, err := New(Options{Role: role, Mode: ModeRecord, Dir: dir, Secrets: []string{token}})
	if err != nil {
		t.Fatalf("New(record): %v", err)
	}
	probe := &originProbe{next: recorder}
	client, err := openai.New(cfg.BaseURL, token, model)
	if err != nil {
		t.Fatalf("openai.New: %v", err)
	}
	client.HTTP = &http.Client{Transport: probe, Timeout: timeout}

	return &liveHarness{
		client:      client,
		recorder:    recorder,
		recordProbe: probe,
		host:        u.Host,
		dir:         dir,
		replayFactory: func() (*openai.Client, *originProbe, *neverDials) {
			probe := &neverDials{t: t}
			replayer, err := New(Options{Role: role, Mode: ModeReplay, Dir: dir, Base: probe, Secrets: []string{token}})
			if err != nil {
				t.Fatalf("New(replay): %v", err)
			}
			origin := &originProbe{next: replayer}
			c, err := openai.New(cfg.BaseURL, token, model)
			if err != nil {
				t.Fatalf("openai.New(replay): %v", err)
			}
			c.HTTP = &http.Client{Transport: origin}
			return c, origin, probe
		},
	}
}

// maxLiveAttempts bounds retries of a *transient* gateway failure while
// recording. Retried attempts are each recorded, so the cassette captures the
// 429 as well as the answer and the replay asserts the same sequence — the
// provider-failure cases in docs/spec/12 become reproducible offline.
const maxLiveAttempts = 3

// chatWithRetry records a chat completion, tolerating a capacity blip, and
// returns the answer together with how many attempts it took.
func (h *liveHarness) chatWithRetry(t *testing.T, ctx context.Context, req openai.ChatRequest, label string) (openai.ChatResponse, int) {
	t.Helper()
	var lastErr error
	for attempt := 1; attempt <= maxLiveAttempts; attempt++ {
		got, err := h.client.Chat(ctx, req)
		if err == nil {
			return got, attempt
		}
		lastErr = err
		var pe *providers.Error
		if errors.As(err, &pe) && pe.Kind == providers.KindUnsupportedCapability {
			// A gateway that refuses the request shape (image parts, json mode)
			// cannot serve this role: record the capability finding rather than
			// pretending the capability exists.
			t.Fatalf("CAPABILITY FINDING: live gateway rejected the %s request (%s); this "+
				"role cannot be recorded against this endpoint", label, pe)
		}
		if !errors.As(err, &pe) || !pe.Retryable || attempt == maxLiveAttempts {
			t.Fatalf("live %s Chat (record): %v", label, err)
		}
		t.Logf("live %s Chat attempt %d failed transiently (%s); retrying while the request is unchanged", label, attempt, pe.Kind)
		select {
		case <-ctx.Done():
			t.Fatalf("live %s Chat: %v (context: %v)", label, lastErr, ctx.Err())
		case <-time.After(time.Duration(attempt) * 3 * time.Second):
		}
	}
	t.Fatalf("live %s Chat: %v", label, lastErr)
	return openai.ChatResponse{}, 0
}

// replayAndCompare replays the recorded attempts offline, in order, and asserts
// the client sees exactly what the gateway answered — including the transient
// failures that preceded the answer — then checks the cassette is portable.
func (h *liveHarness) replayAndCompare(t *testing.T, ctx context.Context, req openai.ChatRequest, want openai.ChatResponse, attempts int) {
	t.Helper()
	replayClient, origins, probe := h.replayFactory()
	for attempt := 1; attempt <= attempts; attempt++ {
		got, err := replayClient.Chat(ctx, req)
		if attempt < attempts {
			if err == nil {
				t.Errorf("replay attempt %d succeeded, but the recorded run failed transiently first", attempt)
			}
			continue
		}
		if err != nil {
			t.Fatalf("replayed Chat: %v", err)
		}
		h.compareAnswers(t, want, got)
	}
	for _, origin := range origins.origins() {
		if origin != OriginRecorded {
			t.Errorf("replay origin = %q, want %q", origin, OriginRecorded)
		}
	}
	if probe.calls.Load() != 0 {
		t.Errorf("replay dialled %d times", probe.calls.Load())
	}
	h.assertCassettePortable(t)
}

// compareAnswers asserts the replayed completion is the recorded one.
func (h *liveHarness) compareAnswers(t *testing.T, want, got openai.ChatResponse) {
	t.Helper()
	if got.Text != want.Text {
		t.Errorf("replayed Text = %q, recorded %q", got.Text, want.Text)
	}
	if got.Usage != want.Usage {
		t.Errorf("replayed Usage = %+v, recorded %+v", got.Usage, want.Usage)
	}
	if got.Model != want.Model || got.FinishReason != want.FinishReason {
		t.Errorf("replayed provenance = model %q finish %q, recorded model %q finish %q",
			got.Model, got.FinishReason, want.Model, want.FinishReason)
	}
}

// assertCassettePortable is the committable-content contract: no endpoint, no
// credential, no image payload, and every line self-labelled recorded.
func (h *liveHarness) assertCassettePortable(t *testing.T) {
	t.Helper()
	cassette := recorderCassette(t, h.dir)
	if cassette == "" {
		t.Fatal("no cassette was written")
	}
	for _, banned := range []string{h.host, "http://", "https://", "Bearer ", "base64,"} {
		if strings.Contains(cassette, banned) {
			t.Errorf("cassette contains %q; it is not committable", h.safeLabel(banned))
		}
	}
	for i, line := range strings.Split(strings.TrimSpace(cassette), "\n") {
		if !strings.Contains(line, `"origin":"recorded"`) {
			t.Errorf("cassette line %d is not labelled recorded", i+1)
		}
		if strings.Contains(line, `"host"`) {
			t.Errorf("cassette line %d stores a host field", i+1)
		}
	}
	if token := gatewayTokenFromEnv(); token != "" && strings.Contains(cassette, token) {
		t.Error("cassette contains the active gateway token")
	}
}

// safeLabel keeps the gateway host out of failure output: naming it would put a
// private endpoint into a log or a CI artefact.
func (h *liveHarness) safeLabel(banned string) string {
	if banned == h.host {
		return "<the configured gateway host>"
	}
	return banned
}

func TestLiveRecordThenReplayTextChat(t *testing.T) {
	harness := newLiveHarness(t, providers.RoleFastReasoning, 3*time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	req := openai.ChatRequest{
		Messages:  []openai.Message{{Role: "user", Text: "Reply with exactly: pong"}},
		MaxTokens: 200,
	}
	want, attempts := harness.chatWithRetry(t, ctx, req, "text")
	if strings.TrimSpace(want.Text) == "" {
		t.Fatal("live gateway returned empty text; nothing meaningful was recorded")
	}
	if want.Usage.Prompt <= 0 || want.Usage.Completion <= 0 {
		t.Errorf("live usage = %+v, want positive token counts to replay", want.Usage)
	}
	for _, origin := range harness.recordProbe.origins() {
		if origin != OriginLive {
			t.Errorf("record-phase origin = %q, want live", origin)
		}
	}

	harness.replayAndCompare(t, ctx, req, want, attempts)
	text := recorderCassette(t, harness.dir)
	t.Logf("live record+replay text chat: role=%s model=%s attempts=%d text=%q usage=%d/%d reasoning_tokens=%d cassette_bytes=%d",
		harness.recorder.Role, want.Model, attempts, truncate(want.Text, 24), want.Usage.Prompt, want.Usage.Completion,
		want.Usage.Reasoning, len(text))
}

func TestLiveRecordThenReplayVisionChat(t *testing.T) {
	harness := newLiveHarness(t, providers.RoleVisionOCRFallback, 4*time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	img := liveRedBluePNG(t, 64, 64)
	req := openai.ChatRequest{
		Messages: []openai.Message{{
			Role:   "user",
			Text:   "What two colors are in this image, left then right? Answer as two lowercase words.",
			Images: [][]byte{img},
		}},
		MaxTokens: 300,
	}
	want, attempts := harness.chatWithRetry(t, ctx, req, "vision")
	harness.replayAndCompare(t, ctx, req, want, attempts)

	// The image must be represented by its digest and nowhere by its bytes.
	cassette := recorderCassette(t, harness.dir)
	if !strings.Contains(cassette, sha256Hex(img)) {
		t.Errorf("cassette does not carry the image digest %s used to match the request", sha256Hex(img))
	}
	t.Logf("live record+replay vision: role=%s image_bytes=%d image_sha256=%s text=%q usage=%d/%d",
		harness.recorder.Role, len(img), sha256Hex(img)[:19], truncate(want.Text, 24), want.Usage.Prompt, want.Usage.Completion)
}

func gatewayTokenFromEnv() string {
	names := []string{providers.EnvGatewayToken}
	if v, ok := os.LookupEnv(providers.EnvGatewayTokenEnv); ok && strings.TrimSpace(v) != "" {
		names = []string{strings.TrimSpace(v), providers.EnvGatewayToken}
	}
	for _, name := range names {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// liveRedBluePNG mirrors the openai adapter's live vision probe: a deterministic
// 64x64 image whose expected answer is unambiguous.
func liveRedBluePNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			if x < width/2 {
				img.Set(x, y, color.RGBA{R: 255, A: 255})
			} else {
				img.Set(x, y, color.RGBA{B: 255, A: 255})
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	return buf.Bytes()
}
