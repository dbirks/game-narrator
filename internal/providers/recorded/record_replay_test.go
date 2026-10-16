package recorded

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/openai"
)

// This file proves the claim that makes recorded providers usable as evidence:
// the same production client, driven through an injected transport, gets
// byte-identical answers from a real (fake-local) gateway when recording and
// from the cassette when replaying with the gateway gone. Nothing in the
// OpenAI adapter knows it is being replayed.

// The fake gateway credentials and model id are placeholders, safe to commit;
// the test asserts they are the only things a cassette could have picked up.
const (
	fakeToken   = "fake-gateway-token-do-not-log"
	fakeModel   = "fake-gateway-model"
	fakeRequest = "Reply with exactly: pong"
)

// committedCassette is the offline example CI replays. It was recorded from the
// httptest fake gateway below — never from a live endpoint — with a fixed clock,
// so regeneration is byte-identical:
//
//	GN_GENERATE_TESTDATA=1 go test -run GenerateCommittedCassette ./internal/providers/recorded/
const committedCassette = "testdata/fast_reasoning.jsonl"

// cassetteClock is the fixed recording time of the committed cassette, so
// regeneration is byte-identical and the fixture does not churn on every run.
var cassetteClock = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

func fakeGateway(t *testing.T) *httptest.Server {
	t.Helper()
	var calls int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		if got := r.Header.Get("Authorization"); got != "Bearer "+fakeToken {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":{"message":"missing bearer token"}}`)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Request-Id", "fake-models-1")
			fmt.Fprintf(w, `{"object":"list","data":[{"id":%q,"object":"model"}]}`, fakeModel)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
			raw, err := readAll(r)
			if err != nil {
				t.Errorf("fake gateway: read body: %v", err)
			}
			body := fakeCompletion(raw)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Request-Id", "fake-chat-1")
			fmt.Fprint(w, body)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":{"message":"no such route"}}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fakeCompletion answers deterministically from the request shape: an image
// part is "red blue", a json_object request is an object, anything else is
// pong. The payload mirrors the recorded gateway shape (leading newlines plus a
// separate reasoning_content) so the client's normalisation is exercised.
func fakeCompletion(request []byte) string {
	var probe struct {
		Model          string `json:"model"`
		ResponseFormat *struct {
			Type string `json:"type"`
		} `json:"response_format"`
		Messages []struct {
			Content any `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(request, &probe)
	content := pongContent
	switch {
	case bytes.Contains(request, []byte("image_url")):
		content = "red blue"
	case probe.ResponseFormat != nil && probe.ResponseFormat.Type == "json_object":
		content = `{\"ok\":true}`
	}
	return fmt.Sprintf(`{"id":"chatcmpl-fake-1","model":%q,"choices":[{"index":0,"message":{"role":"assistant","`+
		`content":"\n\n%s","reasoning_content":"terse answer requested"},"finish_reason":"stop"}],`+
		`"usage":{"prompt_tokens":14,"completion_tokens":9,"total_tokens":23,"reasoning_tokens":7}}`, fakeModel, content)
}

const pongContent = `pong`

func readAll(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r.Body); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// originProbe captures the provenance label of every response that passes
// through it, which is how a caller (the developer UI, a report writer) labels a
// stage without the adapter having to know about cassettes.
type originProbe struct {
	next http.RoundTripper
	mu   sync.Mutex
	seen []string
}

func (p *originProbe) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := p.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.seen = append(p.seen, Origin(resp))
	p.mu.Unlock()
	return resp, nil
}

func (p *originProbe) origins() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.seen...)
}

// providerRound is one provider call used as a record/replay unit.
type providerRound struct {
	name string
	call func(ctx context.Context, c *openai.Client) (string, error)
}

func chatRounds(image []byte) []providerRound {
	return []providerRound{
		{"text-chat", func(ctx context.Context, c *openai.Client) (string, error) {
			resp, err := c.Chat(ctx, openai.ChatRequest{
				Messages:  []openai.Message{{Role: "user", Text: fakeRequest}},
				MaxTokens: 32,
			})
			if err != nil {
				return "", fmt.Errorf("text chat: %w", err)
			}
			return fmt.Sprintf("text=%q reasoning=%q finish=%s model=%s usage=%d/%d/%d",
				resp.Text, resp.Reasoning, resp.FinishReason, resp.Model,
				resp.Usage.Prompt, resp.Usage.Completion, resp.Usage.Reasoning), nil
		}},
		{"vision-chat", func(ctx context.Context, c *openai.Client) (string, error) {
			resp, err := c.Chat(ctx, openai.ChatRequest{
				Messages: []openai.Message{{
					Role:   "user",
					Text:   "What two colors are in this image, left then right? Answer as two lowercase words.",
					Images: [][]byte{image},
				}},
				MaxTokens: 64,
			})
			if err != nil {
				return "", fmt.Errorf("vision chat: %w", err)
			}
			return fmt.Sprintf("text=%q finish=%s model=%s usage=%d/%d/%d",
				resp.Text, resp.FinishReason, resp.Model,
				resp.Usage.Prompt, resp.Usage.Completion, resp.Usage.Reasoning), nil
		}},
		{"list-models", func(ctx context.Context, c *openai.Client) (string, error) {
			ids, err := c.ListModels(ctx)
			if err != nil {
				return "", fmt.Errorf("list models: %w", err)
			}
			return "models=" + strings.Join(ids, ","), nil
		}},
	}
}

func openaiClient(baseURL string, transport http.RoundTripper) *openai.Client {
	return &openai.Client{BaseURL: baseURL + "/v1", Token: fakeToken, Model: fakeModel,
		HTTP: &http.Client{Transport: transport}}
}

// TestRecordAgainstGatewayThenReplayOffline is the end-to-end contract test.
func TestRecordAgainstGatewayThenReplayOffline(t *testing.T) {
	dir := t.TempDir()
	image := redBluePNG(t, 64, 64)
	srv := fakeGateway(t)
	baseURL := srv.URL
	host := strings.TrimPrefix(strings.TrimPrefix(baseURL, "http://"), "https://")

	probe := &originProbe{next: must(New(Options{
		Role: providers.RoleFastReasoning, Mode: ModeRecord, Dir: dir, Secrets: []string{fakeToken},
	}))}
	recordClient := openaiClient(baseURL, probe)

	ctx := context.Background()
	want := make(map[string]string, 3)
	for _, round := range chatRounds(image) {
		got, err := round.call(ctx, recordClient)
		if err != nil {
			t.Fatalf("record %s: %v", round.name, err)
		}
		want[round.name] = got
	}
	for _, origin := range probe.origins() {
		if origin != OriginLive {
			t.Fatalf("record-mode origin = %q, want %q; a recorded run must still label live content live", origin, OriginLive)
		}
	}

	// The cassette must be committable: no endpoint, no credential, no image.
	cassette := recorderCassette(t, dir)
	assertNoLeakage(t, cassette)
	if strings.Contains(cassette, host) || strings.Contains(cassette, fakeToken) {
		t.Error("cassette contains the endpoint host or the fake token")
	}
	if lines := strings.Count(cassette, "\n"); lines != len(chatRounds(image)) {
		t.Errorf("cassette has %d lines, want %d", lines, len(chatRounds(image)))
	}
	assertCanonicalDigests(t, cassette)

	// Shut the gateway down. Everything after this point has no network.
	srv.Close()

	replayBase := &neverDials{t: t}
	replayer := must(New(Options{
		Role: providers.RoleFastReasoning, Mode: ModeReplay, Dir: dir, Base: replayBase, Secrets: []string{fakeToken},
	}))
	replayProbe := &originProbe{next: replayer}
	replayClient := openaiClient(baseURL, replayProbe)
	for _, round := range chatRounds(image) {
		got, err := round.call(ctx, replayClient)
		if err != nil {
			t.Fatalf("replay %s: %v", round.name, err)
		}
		if got != want[round.name] {
			t.Errorf("replayed %s =\n  %s\nrecorded =\n  %s", round.name, got, want[round.name])
		}
	}
	for _, origin := range replayProbe.origins() {
		if origin != OriginRecorded {
			t.Errorf("replay origin = %q, want %q", origin, OriginRecorded)
		}
	}
	if replayBase.calls.Load() != 0 {
		t.Errorf("replay dialled %d times", replayBase.calls.Load())
	}
	hits, misses, dials := replayer.Stats()
	if want := int64(len(chatRounds(image))); hits != want || misses != 0 || dials != 0 {
		t.Errorf("replay stats = hits %d misses %d dials %d, want hits %d with no misses or dials", hits, misses, dials, want)
	}
}

func assertCanonicalDigests(t *testing.T, cassette string) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(cassette), "\n") {
		if line == "" {
			continue
		}
		in, err := parseInteraction([]byte(line))
		if err != nil {
			t.Fatalf("cassette line is not loadable: %v\n%s", err, line)
		}
		if in.Request.BodyDigest != digest(canonicalForDigest(in.Request.Body)) {
			t.Errorf("stored body_digest %s does not match body for %s %s", in.Request.BodyDigest, in.Request.Method, in.Request.Path)
		}
	}
}

func redBluePNG(t *testing.T, width, height int) []byte {
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

func must(t *Transport, err error) *Transport {
	if err != nil {
		panic(err)
	}
	return t
}

func TestGenerateCommittedCassette(t *testing.T) {
	if os.Getenv("GN_GENERATE_TESTDATA") == "" {
		t.Skip("NOT RUN: set GN_GENERATE_TESTDATA=1 to regenerate " + committedCassette + " from the httptest fake gateway")
	}
	dir := t.TempDir()
	srv := fakeGateway(t)
	recorder, err := New(Options{
		Role: providers.RoleFastReasoning, Mode: ModeRecord, Dir: dir,
		Secrets: []string{fakeToken},
		// Pinned clock and pinned latency figure: regenerating the committed
		// cassette produces byte-identical content, so the fixture never churns.
		Now:       func() time.Time { return cassetteClock },
		LatencyMS: func(time.Duration) int64 { return 4 },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	client := openaiClient(srv.URL, recorder)
	for _, round := range chatRounds(redBluePNG(t, 64, 64)) {
		if _, err := round.call(context.Background(), client); err != nil {
			t.Fatalf("record %s: %v", round.name, err)
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, "fast_reasoning.jsonl"))
	if err != nil {
		t.Fatalf("read generated cassette: %v", err)
	}
	assertNoLeakage(t, string(raw))
	if err := os.WriteFile(committedCassette, raw, 0o644); err != nil {
		t.Fatalf("write %s: %v", committedCassette, err)
	}
	t.Logf("wrote %s (%d bytes, %d interactions)", committedCassette, len(raw), strings.Count(string(raw), "\n"))
}

// TestReplayCommittedCassette is the offline CI example: a cassette checked into
// testdata, replayed by the real client with no server and no network, and
// labelled recorded.
func TestReplayCommittedCassette(t *testing.T) {
	replayBase := &neverDials{t: t}
	replayer, err := New(Options{
		Role: providers.RoleFastReasoning, Mode: ModeReplay, Dir: filepath.Dir(committedCassette), Base: replayBase,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := os.Stat(committedCassette); err != nil {
		t.Fatalf("committed cassette missing: %v", err)
	}
	client := openaiClient("http://gateway.invalid", replayer)
	ctx := context.Background()

	resp, err := client.Chat(ctx, openai.ChatRequest{
		Messages:  []openai.Message{{Role: "user", Text: fakeRequest}},
		MaxTokens: 32,
	})
	if err != nil {
		t.Fatalf("replayed text chat: %v", err)
	}
	if resp.Text != "pong" {
		t.Errorf("Text = %q, want the recorded \"pong\"", resp.Text)
	}
	if resp.Usage.Prompt != 14 || resp.Usage.Completion != 9 || resp.Usage.Reasoning != 7 {
		t.Errorf("Usage = %+v, want the recorded 14/9/7", resp.Usage)
	}
	if resp.RequestID != "fake-chat-1" {
		t.Errorf("RequestID = %q, want the recorded correlation id", resp.RequestID)
	}
	ids, err := client.ListModels(ctx)
	if err != nil {
		t.Fatalf("replayed ListModels: %v", err)
	}
	if len(ids) != 1 || ids[0] != fakeModel {
		t.Errorf("ListModels = %v, want [%s]", ids, fakeModel)
	}
	// A request that was never recorded must fail, not answer.
	if _, err := client.Chat(ctx, openai.ChatRequest{
		Messages: []openai.Message{{Role: "user", Text: "This question was never recorded."}},
	}); err == nil {
		t.Fatal("an unrecorded request was answered from the cassette")
	} else {
		var pe *providers.Error
		if !errors.As(err, &pe) {
			t.Fatalf("err = %v (%T), want *providers.Error", err, err)
		}
		if pe.Kind != providers.KindUnsupportedCapability || !strings.Contains(pe.Msg, "cassette_miss") {
			t.Errorf("unrecorded request err = %s, want cassette_miss/unsupported_capability", pe)
		}
		if !IsMiss(err) {
			t.Error("IsMiss did not see through the openai adapter")
		}
	}
	hits, misses, dials := replayer.Stats()
	if hits < 2 || dials != 0 || replayBase.calls.Load() != 0 {
		t.Errorf("stats hits=%d misses=%d dials=%d probe=%d, want >=2 hits and no dials", hits, misses, dials, replayBase.calls.Load())
	}
}
