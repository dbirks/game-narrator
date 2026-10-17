package router

// Integration tests: the router is protocol-agnostic, so the real adapters
// (openai, systemone, tts) are driven through Do against httptest fakes, and
// the recorded-cassette transport (ADR-0013) is replayed behind the router.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/openai"
	"github.com/dbirks/game-narrator/internal/providers/recorded"
	"github.com/dbirks/game-narrator/internal/providers/systemone"
	"github.com/dbirks/game-narrator/internal/providers/tts"
)

// statusServer answers every call with the given status and counts requests.
func statusServer(t *testing.T, status int, body string) (*httptest.Server, *int32Counter) {
	t.Helper()
	var mu sync.Mutex
	var count int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		count++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &int32Counter{mu: &mu, n: &count}
}

// int32Counter exposes the request count of a statusServer.
type int32Counter struct {
	mu *sync.Mutex
	n  *int
}

func (c *int32Counter) get() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return *c.n
}

// chatAnswerServer answers the first calls with the scripted statuses and
// then serves a recorded-style chat answer.
func chatAnswerServer(t *testing.T, statuses []int) (*httptest.Server, *int32Counter) {
	t.Helper()
	var mu sync.Mutex
	var count int
	next := func() int {
		mu.Lock()
		defer mu.Unlock()
		defer func() { count++ }()
		if count < len(statuses) {
			return statuses[count]
		}
		return http.StatusOK
	}
	body := `{"id":"chatcmpl-it-1","model":"it-model","choices":[{"index":0,"message":{"role":"assistant","content":"\n\npong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":14,"completion_tokens":9,"total_tokens":23}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		st := next()
		w.Header().Set("Content-Type", "application/json")
		if st != http.StatusOK {
			w.WriteHeader(st)
			io.WriteString(w, `{"error":{"message":"synthetic failure","type":"synthetic"}}`)
			return
		}
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &int32Counter{mu: &mu, n: &count}
}

func roleRegistry(t *testing.T, base string) *providers.Registry {
	t.Helper()
	m := make(map[providers.Role]providers.RoleConfig, len(providers.AllRoles))
	for _, role := range providers.AllRoles {
		cfg := providers.RoleConfig{
			Role:     role,
			Protocol: providers.ProtocolOpenAIChat,
			Timeout:  2 * time.Second,
		}
		switch role {
		case fast:
			cfg.BaseURL = base
			cfg.Model = "it-model"
		case providers.RoleDecision:
			cfg.Protocol = providers.ProtocolSystemOne
			cfg.BaseURL = base
			cfg.Model = "it-decision-model"
		case providers.RoleVoiceClone:
			cfg.Protocol = providers.ProtocolVLLMOmniSpeech
			cfg.BaseURL = base
			cfg.Model = "it-voice-model"
		}
		m[role] = cfg
	}
	return providers.NewRegistry(m)
}

// TestDoDrivesTheOpenAIClient: the real chat adapter behind the router — a
// 429 is retried (idempotent), the recorded-style answer is returned, and the
// router's health reflects one failure and one retry.
func TestDoDrivesTheOpenAIClient(t *testing.T) {
	srv, count := chatAnswerServer(t, []int{http.StatusTooManyRequests})
	reg := roleRegistry(t, srv.URL)
	var sleeps []time.Duration
	r := New(reg, Options{
		Now:   time.Now,
		Sleep: func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil },
	})

	client, err := openai.New(srv.URL, "it-token", "it-model")
	if err != nil {
		t.Fatalf("openai.New: %v", err)
	}
	res, err := Do(context.Background(), r, fast, CallOptions{Idempotent: true}, func(ctx context.Context, _ Target) (openai.ChatResponse, error) {
		return client.Chat(ctx, openai.ChatRequest{
			Messages:  []openai.Message{{Role: "user", Text: "Reply with exactly: pong"}},
			MaxTokens: 32,
		})
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if res.Text != "pong" {
		t.Fatalf("answer = %q, want pong", res.Text)
	}
	if count.get() != 2 {
		t.Fatalf("requests = %d, want 2 (429 then success)", count.get())
	}
	if len(sleeps) != 1 {
		t.Fatalf("backoffs = %d, want 1", len(sleeps))
	}
	ph := r.Health()[fast]
	if ph.Failures != 1 || ph.Retries != 1 || ph.State != StateOK {
		t.Fatalf("health = %+v, want 1 failure, 1 retry, ok", ph)
	}
}

// TestDoDrivesTheSystemOneClient: the real System One adapter behind the
// router — repeated 503s are retried up to the attempt budget and surface as
// unavailable, exactly what the 1500 ms decision budget expects.
func TestDoDrivesTheSystemOneClient(t *testing.T) {
	srv, count := statusServer(t, http.StatusServiceUnavailable, `{"error":{"message":"upstream"}}`)
	reg := roleRegistry(t, srv.URL)
	var sleeps []time.Duration
	r := New(reg, Options{
		Now:   time.Now,
		Sleep: func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil },
	})

	client, err := systemone.New(srv.URL, "it-token", "it-decision-model")
	if err != nil {
		t.Fatalf("systemone.New: %v", err)
	}
	req := systemone.Request{
		State: map[string]any{"scene": "test"},
		Questions: []systemone.Question{{
			ID:           "kind",
			Type:         systemone.TypeChoice,
			Instructions: "classify",
			Criteria:     []systemone.Option{{ID: "dialogue", Description: "speech"}, {ID: "none", Description: "nothing"}},
		}},
	}
	_, err = Do(context.Background(), r, providers.RoleDecision, CallOptions{Idempotent: true},
		func(ctx context.Context, target Target) (systemone.Decision, error) {
			return client.Decide(ctx, req)
		})
	if err == nil {
		t.Fatal("want unavailable after 3 failed attempts")
	}
	isKind(t, err, providers.KindUnavailable)
	if count.get() != 3 {
		t.Fatalf("requests = %d, want 3 (attempt budget)", count.get())
	}
	if len(sleeps) != 2 {
		t.Fatalf("backoffs = %d, want 2", len(sleeps))
	}
}

// TestDoDrivesTheTTSClient: the real TTS adapter behind the router — the
// voice_clone role with a dead endpoint fails with unavailable after the
// attempt budget, so a live session can fall back to cached speech.
func TestDoDrivesTheTTSClient(t *testing.T) {
	srv, count := statusServer(t, http.StatusInternalServerError, `{"error":{"message":"boom"}}`)
	reg := roleRegistry(t, srv.URL)
	var sleeps []time.Duration
	r := New(reg, Options{
		Now:   time.Now,
		Sleep: func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil },
	})

	client, err := tts.New(srv.URL, "it-token", "it-voice-model")
	if err != nil {
		t.Fatalf("tts.New: %v", err)
	}
	_, err = Do(context.Background(), r, providers.RoleVoiceClone, CallOptions{Idempotent: true},
		func(ctx context.Context, _ Target) (tts.Audio, error) {
			return client.Generate(ctx, tts.GenerateRequest{Text: "hello", Voice: "voice-1"})
		})
	if err == nil {
		t.Fatal("want unavailable after 3 failed attempts")
	}
	isKind(t, err, providers.KindUnavailable)
	if count.get() != 3 {
		t.Fatalf("requests = %d, want 3", count.get())
	}
}

// TestRecordedTransportBehindRouter: an openai.Client whose HTTP client uses
// the recorded replay transport (ADR-0013) behind the router returns the
// recorded answer, never dials, and the router labels the role's origin as
// recorded — nothing is labelled live.
func TestRecordedTransportBehindRouter(t *testing.T) {
	transport, err := recorded.New(recorded.Options{
		Role: fast,
		Mode: recorded.ModeReplay,
		Dir:  "../recorded/testdata",
	})
	if err != nil {
		t.Fatalf("recorded.New: %v", err)
	}
	capture := &originCapture{next: transport}
	client, err := openai.New("http://cassette.invalid/v1", "recorded-token", "fake-gateway-model")
	if err != nil {
		t.Fatalf("openai.New: %v", err)
	}
	client.HTTP = &http.Client{Transport: capture}

	reg := roleRegistry(t, "http://cassette.invalid/v1")
	r := New(reg, Options{Now: time.Now})

	var res openai.ChatResponse
	out, err := Do(context.Background(), r, fast, CallOptions{Idempotent: true}, func(ctx context.Context, _ Target) (openai.ChatResponse, error) {
		chat, callErr := client.Chat(ctx, openai.ChatRequest{
			Messages:  []openai.Message{{Role: "user", Text: "Reply with exactly: pong"}},
			MaxTokens: 32,
		})
		if callErr == nil {
			r.ObserveOrigin(fast, capture.get())
		}
		return chat, callErr
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	res = out
	if res.Text != "pong" {
		t.Fatalf("recorded answer = %q, want pong", res.Text)
	}
	if res.RequestID != "fake-chat-1" {
		t.Fatalf("request id = %q, want fake-chat-1", res.RequestID)
	}
	hits, misses, dials := transport.Stats()
	if hits != 1 || misses != 0 || dials != 0 {
		t.Fatalf("transport stats: hits=%d misses=%d dials=%d, want 1/0/0", hits, misses, dials)
	}
	if got := capture.get(); got != recorded.OriginRecorded {
		t.Fatalf("response origin = %q, want %q", got, recorded.OriginRecorded)
	}
	if got := r.Origins()[fast]; got != recorded.OriginRecorded {
		t.Fatalf("router origin label = %q, want %q (nothing may be labelled live)", got, recorded.OriginRecorded)
	}
	if ph := r.Health()[fast]; ph.State != StateOK {
		t.Fatalf("health state = %s, want ok", ph.State)
	}
}

// originCapture is a RoundTripper wrapper that records the
// X-Game-Narrator-Origin header of each response, so the test can hand the
// provenance label to the router the way an app would.
type originCapture struct {
	next http.RoundTripper

	mu     sync.Mutex
	origin string
}

func (o *originCapture) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := o.next.RoundTrip(req)
	if err == nil && resp != nil {
		og := recorded.Origin(resp)
		o.mu.Lock()
		o.origin = og
		o.mu.Unlock()
	}
	return resp, err
}

func (o *originCapture) get() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.origin
}
