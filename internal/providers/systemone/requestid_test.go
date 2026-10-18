package systemone

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
)

// requestIDModel is the model the correlation-id tests run against; it is a
// plain httptest fake, never the live gateway.
const requestIDModel = "clef-test"

// minimalDecisionRequest is the smallest valid System One request (one choice
// question with two options); State is non-empty, per the wire contract.
func minimalDecisionRequest() Request {
	return Request{
		State: map[string]any{"ocr": "correlation probe"},
		Questions: []Question{{
			ID:           "q1",
			Type:         TypeChoice,
			Instructions: "Pick one.",
			Criteria:     []Option{{ID: "a", Description: "yes"}, {ID: "b", Description: "no"}},
		}},
	}
}

// validDecisionBody renders a schema-valid answer envelope for
// minimalDecisionRequest against model.
func validDecisionBody(model string) []byte {
	return []byte(`{"model":"` + model + `","answers":{"q1":{"type":"choice","choice":"a","confidence":0.9,"probabilities":{"a":0.9,"b":0.1}}},"usage":{"input_tokens":10,"output_tokens":0}}`)
}

// sentIDRecorder collects the X-Request-Id of every request the fake gateway
// serves, so tests can assert on what left the client.
type sentIDRecorder struct {
	mu  sync.Mutex
	ids []string
}

func (r *sentIDRecorder) record(h http.Header) {
	r.mu.Lock()
	r.ids = append(r.ids, h.Get("X-Request-Id"))
	r.mu.Unlock()
}

func (r *sentIDRecorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.ids))
	copy(out, r.ids)
	return out
}

// newRequestIDClient wires the client to a fake gateway that records the
// X-Request-Id of every request it serves.
func newRequestIDClient(t *testing.T, recorder *sentIDRecorder, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder.record(r.Header)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	client, err := New(srv.URL, "requestid-test-token", requestIDModel)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

// TestDecisionSendsClientRequestID is the core guarantee: the gateway answers
// the /systemone route with no request-id header, so the client's own ULID —
// sent as X-Request-Id — must be well-formed and must be what the Decision
// records, flagged source "client".
func TestDecisionSendsClientRequestID(t *testing.T) {
	rec := &sentIDRecorder{}
	client := newRequestIDClient(t, rec, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, string(validDecisionBody(requestIDModel)))
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	d, err := client.Decide(ctx, minimalDecisionRequest())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	sent := rec.all()
	if len(sent) != 1 {
		t.Fatalf("requests seen = %d, want exactly 1", len(sent))
	}
	if !validRequestID(sent[0]) {
		t.Fatalf("X-Request-Id sent = %q, want a well-formed client ULID", sent[0])
	}
	if d.RequestID != sent[0] {
		t.Fatalf("RequestID = %q, want the sent client id %q", d.RequestID, sent[0])
	}
	if d.RequestIDSource != RequestIDSourceClient {
		t.Fatalf("RequestIDSource = %q, want %q (the gateway returned no id of its own)",
			d.RequestIDSource, RequestIDSourceClient)
	}
}

// TestDecisionPrefersProviderRequestID pins the other half of the contract:
// when a deployment does return a request-id header, that id wins verbatim
// and the source flips to "provider".
func TestDecisionPrefersProviderRequestID(t *testing.T) {
	rec := &sentIDRecorder{}
	client := newRequestIDClient(t, rec, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "prov-e2e-42")
		fmt.Fprint(w, string(validDecisionBody(requestIDModel)))
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	d, err := client.Decide(ctx, minimalDecisionRequest())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.RequestID != "prov-e2e-42" {
		t.Fatalf("RequestID = %q, want the provider's id verbatim", d.RequestID)
	}
	if d.RequestIDSource != RequestIDSourceProvider {
		t.Fatalf("RequestIDSource = %q, want %q", d.RequestIDSource, RequestIDSourceProvider)
	}
	sent := rec.all()
	if len(sent) != 1 || !validRequestID(sent[0]) {
		t.Fatalf("sent X-Request-Id = %v, want one well-formed client id (still sent even though the provider answers its own)", sent)
	}
}

// TestDecisionRequestIDUniquePerRequest proves the id is minted per request,
// not per client: a burst of requests carries a distinct, well-formed id
// each, so a log search on one id can never match another attempt.
func TestDecisionRequestIDUniquePerRequest(t *testing.T) {
	rec := &sentIDRecorder{}
	client := newRequestIDClient(t, rec, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, string(validDecisionBody(requestIDModel)))
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const n = 8
	seen := map[string]bool{}
	for i := 0; i < n; i++ {
		if _, err := client.Decide(ctx, minimalDecisionRequest()); err != nil {
			t.Fatalf("request %d: %v", i+1, err)
		}
	}
	sent := rec.all()
	if len(sent) != n {
		t.Fatalf("requests seen = %d, want %d", len(sent), n)
	}
	for i, id := range sent {
		if !validRequestID(id) {
			t.Fatalf("request %d: sent id %q is not a well-formed ULID", i+1, id)
		}
		if seen[id] {
			t.Fatalf("request %d: id %q was already sent; ids must be unique per request", i+1, id)
		}
		seen[id] = true
	}
}

// TestDecisionRequestIDRetryRecordsSuccessfulAttempt pins the retry contract:
// the client does not retry (the model router owns that policy), so when the
// router retries, each attempt carries its own id and the Decision the
// runtime finally records carries the successful attempt's id — while the
// failed attempt's classified error carries its own id, so both attempts stay
// independently searchable in provider logs.
func TestDecisionRequestIDRetryRecordsSuccessfulAttempt(t *testing.T) {
	rec := &sentIDRecorder{}
	var mu sync.Mutex
	attempts := 0
	client := newRequestIDClient(t, rec, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"detail":"transient failure"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, string(validDecisionBody(requestIDModel)))
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Attempt 1 (the router would see this error and retry): classified
	// unavailable, carrying its own client id.
	_, err := client.Decide(ctx, minimalDecisionRequest())
	var pe *providers.Error
	if !errors.As(err, &pe) {
		t.Fatalf("first attempt err = %v (%T), want *providers.Error", err, err)
	}
	if pe.Kind != providers.KindUnavailable {
		t.Fatalf("first attempt Kind = %q, want unavailable", pe.Kind)
	}
	sentAfterFirst := rec.all()
	if len(sentAfterFirst) != 1 || !validRequestID(sentAfterFirst[0]) {
		t.Fatalf("sent ids after first attempt = %v, want one well-formed client id", sentAfterFirst)
	}
	if pe.RequestID != sentAfterFirst[0] {
		t.Fatalf("failed-attempt RequestID = %q, want its own sent id %q", pe.RequestID, sentAfterFirst[0])
	}

	// Attempt 2 (the retry): its id is recorded on the Decision, not the
	// failed attempt's.
	d, err := client.Decide(ctx, minimalDecisionRequest())
	if err != nil {
		t.Fatalf("retry Decide: %v", err)
	}
	sent := rec.all()
	if len(sent) != 2 {
		t.Fatalf("requests seen = %d, want 2 (failed + retried)", len(sent))
	}
	if !validRequestID(sent[1]) {
		t.Fatalf("retry id %q is not a well-formed ULID", sent[1])
	}
	if sent[0] == sent[1] {
		t.Fatalf("both attempts used the same id %q; each attempt must mint its own", sent[0])
	}
	if d.RequestID != sent[1] {
		t.Fatalf("Decision.RequestID = %q, want the successful attempt's id %q", d.RequestID, sent[1])
	}
	if d.RequestIDSource != RequestIDSourceClient {
		t.Fatalf("RequestIDSource = %q, want %q", d.RequestIDSource, RequestIDSourceClient)
	}
}

// TestRecordRequestIDResolution pins the provider-wins / client-fallback rule
// (mirroring the TTS adapter's resolution table).
func TestRecordRequestIDResolution(t *testing.T) {
	for _, tc := range []struct {
		name       string
		provider   string
		client     string
		wantID     string
		wantSource string
	}{
		{"provider present", "prov-1", "c1", "prov-1", RequestIDSourceProvider},
		{"provider whitespace", "   ", "c2", "c2", RequestIDSourceClient},
		{"provider empty", "", "c3", "c3", RequestIDSourceClient},
		{"provider padded", "  prov-2  ", "c4", "prov-2", RequestIDSourceProvider},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, source := RecordRequestID(tc.provider, tc.client)
			if id != tc.wantID || source != tc.wantSource {
				t.Fatalf("RecordRequestID(%q, %q) = (%q, %q), want (%q, %q)",
					tc.provider, tc.client, id, source, tc.wantID, tc.wantSource)
			}
		})
	}
}

// TestNewRequestIDFormatAndUniqueness checks the id shape and that a burst of
// ids (including ids minted in the same millisecond) never collides.
func TestNewRequestIDFormatAndUniqueness(t *testing.T) {
	const n = 1000
	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		id := newRequestID()
		if !validRequestID(id) {
			t.Fatalf("id %d = %q, want 26 Crockford chars with first char 0-7", i+1, id)
		}
		if seen[id] {
			t.Fatalf("id %d = %q collided; ids must be unique", i+1, id)
		}
		seen[id] = true
	}
}
