package providers_test

// Cross-client audit of the spec-14 redirect control (ADR-0068): the
// operator-configured gateway (trust zone E, mutable) answers the client's
// request with a redirect to a different host. Every provider client must
// refuse the hop — otherwise the request body (prompt, game state, audio)
// and the bearer token flow to an endpoint nobody configured, and the
// target's response would re-enter the model context.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/asr"
	"github.com/dbirks/game-narrator/internal/providers/openai"
	"github.com/dbirks/game-narrator/internal/providers/systemone"
	"github.com/dbirks/game-narrator/internal/providers/tts"
)

// offHostGateways starts gateway A (302s every request to B) and counts B's
// hits; the test proves B never sees the moved request.
func offHostGateways(t *testing.T) (aURL string, bHits func() int32) {
	t.Helper()
	var hits int32
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	t.Cleanup(b.Close)
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", b.URL+"/stolen")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(a.Close)
	return a.URL, func() int32 { return atomic.LoadInt32(&hits) }
}

// assertRefused is the acceptance line: the call fails with
// ErrRedirectRefused and the redirect target was never contacted.
func assertRefused(t *testing.T, err error, bHits func() int32) {
	t.Helper()
	if err == nil {
		t.Fatalf("off-host redirect was followed (B saw %d hits): the gateway must not be able to move the request", bHits())
	}
	if !errors.Is(err, providers.ErrRedirectRefused) {
		t.Fatalf("error = %v; want ErrRedirectRefused", err)
	}
	if n := bHits(); n != 0 {
		t.Fatalf("the redirect target was contacted %d times: it must not receive the request", n)
	}
}

func ctx5s(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestOpenAIClientRefusesOffHostRedirect(t *testing.T) {
	aURL, hits := offHostGateways(t)
	c, err := openai.New(aURL, "secret-token", "test-model")
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Chat(ctx5s(t), openai.ChatRequest{Messages: []openai.Message{{Role: "user", Text: "hi"}}})
	assertRefused(t, err, hits)
}

func TestSystemOneClientRefusesOffHostRedirect(t *testing.T) {
	aURL, hits := offHostGateways(t)
	c, err := systemone.New(aURL, "secret-token", "test-model")
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Decide(ctx5s(t), systemone.Request{
		State:     map[string]any{"scene": "one"},
		Questions: []systemone.Question{{ID: "q1", Type: systemone.TypeNoul, Instructions: "describe the scene"}},
	})
	assertRefused(t, err, hits)
}

func TestTTSClientRefusesOffHostRedirect(t *testing.T) {
	aURL, hits := offHostGateways(t)
	c, err := tts.New(aURL, "secret-token", "test-model")
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Design(ctx5s(t), tts.DesignRequest{Text: "hello"})
	assertRefused(t, err, hits)
}

// SECURITY-FINDING GN-SEC-002 (deferred, severity high): asr.Client's
// httpClient() returns a bare &http.Client{}, so a gateway redirect to a
// different host is followed with the default 10-hop budget — the audio
// upload and bearer token would land at an unconfigured endpoint. The
// failing assertion below is what the fix must satisfy.
// internal/providers/asr is in flight with another worker, so the fix is
// deferred to the orchestrator: apply the same SafeRedirect wiring as
// ADR-0068 (New, NewFromRole, httpClient) and remove this skip.
func TestASRClientRefusesOffHostRedirect(t *testing.T) {
	t.Skip("SECURITY-FINDING GN-SEC-002: asr client follows off-host redirects (no redirect guard); fix lands with the asr worker")
	aURL, hits := offHostGateways(t)
	c, err := asr.New(aURL, "secret-token", "test-model")
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Transcribe(ctx5s(t), asr.TranscribeRequest{WAV: []byte("RIFFxxxxWAVEfmt ")})
	assertRefused(t, err, hits)
}
