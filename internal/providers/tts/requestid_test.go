package tts

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
)

// These tests pin the client-side correlation-id contract (ADR-0050 follow-up):
// every /audio/speech and registry request carries a client-minted ULID as
// X-Request-Id, and when the deployment answers without a request-id header
// the client id is surfaced as the result's RequestID with the source marked
// "client"; a provider-issued id wins verbatim and is marked "provider".

// recordingServer is an httptest server that records the requests it saw and
// answers /audio/speech with a scripted response. setRequestID controls
// whether the response carries a provider request-id header (the vLLM-Omni
// deployment sets none).
type recordingServer struct {
	t           *testing.T
	srv         *httptest.Server
	requestIDs  []string // the X-Request-Id header sent per request, in order
	setIDHeader bool
	idHeader    string
	body        []byte
	status      int
}

func newRecordingServer(t *testing.T, status int, body []byte, setIDHeader bool, idHeader string) *recordingServer {
	t.Helper()
	s := &recordingServer{t: t, status: status, body: body, setIDHeader: setIDHeader, idHeader: idHeader}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requestIDs = append(s.requestIDs, r.Header.Get("X-Request-Id"))
		if s.setIDHeader {
			w.Header().Set("X-Request-Id", s.idHeader)
		}
		w.Header().Set("Content-Type", "audio/wav")
		w.WriteHeader(s.status)
		_, _ = w.Write(s.body)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *recordingServer) url() string { return s.srv.URL }

// TestDesignSendsClientIDAndSurfacesItWhenProviderHasNone pins the core of
// the contract against the deployment shape we actually run against: no
// request-id response header, so the client id is both sent and surfaced.
func TestDesignSendsClientIDAndSurfacesItWhenProviderHasNone(t *testing.T) {
	f := newRecordingServer(t, http.StatusOK, fixtureWAV(t), false, "")
	c := newTestClient(t, f.url())
	a, err := c.Design(context.Background(), DesignRequest{Text: "The keeper counts three lanterns."})
	if err != nil {
		t.Fatalf("Design: %v", err)
	}
	if len(f.requestIDs) != 1 {
		t.Fatalf("requests = %d, want 1", len(f.requestIDs))
	}
	sent := f.requestIDs[0]
	if !validRequestID(sent) {
		t.Fatalf("X-Request-Id = %q, want a 26-char Crockford ULID", sent)
	}
	if a.RequestID != sent {
		t.Errorf("Audio.RequestID = %q, want the client id that was sent (%q)", a.RequestID, sent)
	}
	if a.RequestIDSource != RequestIDSourceClient {
		t.Errorf("RequestIDSource = %q, want %q", a.RequestIDSource, RequestIDSourceClient)
	}
}

// TestDesignPrefersProviderIDWhenPresent pins the other half: a deployment
// that DOES issue request ids must keep its own, verbatim — the client id
// stays on the wire for correlation but is not the recorded id.
func TestDesignPrefersProviderIDWhenPresent(t *testing.T) {
	f := newRecordingServer(t, http.StatusOK, fixtureWAV(t), true, "prov-9f8e7d")
	c := newTestClient(t, f.url())
	a, err := c.Design(context.Background(), DesignRequest{Text: "The keeper counts three lanterns."})
	if err != nil {
		t.Fatalf("Design: %v", err)
	}
	sent := f.requestIDs[0]
	if !validRequestID(sent) {
		t.Fatalf("X-Request-Id = %q, want a 26-char Crockford ULID", sent)
	}
	if sent == "prov-9f8e7d" {
		t.Fatalf("the client id must be its own ULID, not an echo of the provider id")
	}
	if a.RequestID != "prov-9f8e7d" {
		t.Errorf("Audio.RequestID = %q, want the provider's id verbatim", a.RequestID)
	}
	if a.RequestIDSource != RequestIDSourceProvider {
		t.Errorf("RequestIDSource = %q, want %q", a.RequestIDSource, RequestIDSourceProvider)
	}
}

// TestGenerateCarriesClientIDOnEveryRequest pins that the Base path sends a
// fresh id per request (two calls must not reuse one) and surfaces the
// client id on success.
func TestGenerateCarriesClientIDOnEveryRequest(t *testing.T) {
	f := newRecordingServer(t, http.StatusOK, fixtureWAV(t), false, "")
	c := newTestClient(t, f.url())
	for i := 0; i < 2; i++ {
		a, err := c.Generate(context.Background(), GenerateRequest{Text: "A line.", Voice: "gn_ab12cd34_narrator_v1_0123abcd"})
		if err != nil {
			t.Fatalf("Generate %d: %v", i, err)
		}
		sent := f.requestIDs[i]
		if !validRequestID(sent) {
			t.Fatalf("request %d X-Request-Id = %q, want a 26-char Crockford ULID", i, sent)
		}
		if a.RequestID != sent || a.RequestIDSource != RequestIDSourceClient {
			t.Errorf("request %d: Audio = %q/%q, want the sent client id with source %q", i, a.RequestID, a.RequestIDSource, RequestIDSourceClient)
		}
	}
	if f.requestIDs[0] == f.requestIDs[1] {
		t.Errorf("two requests reused the client id %q", f.requestIDs[0])
	}
}

// TestErrorCarriesTheCorrelationID pins that a classified failure still
// carries the id the provider can be searched by: on this deployment the
// provider echoes nothing back, so the error id is the client id that was
// sent.
func TestErrorCarriesTheCorrelationID(t *testing.T) {
	f := newRecordingServer(t, http.StatusBadRequest, errorBody("input must be non-empty"), false, "")
	c := newTestClient(t, f.url())
	_, err := c.Generate(context.Background(), GenerateRequest{Text: "x", Voice: "v"})
	var pe *providers.Error
	if !errors.As(err, &pe) {
		t.Fatalf("error = %T %v, want *providers.Error", err, err)
	}
	if pe.Kind != providers.KindBadSchema {
		t.Fatalf("kind = %s, want %s", pe.Kind, providers.KindBadSchema)
	}
	sent := f.requestIDs[0]
	if !validRequestID(sent) {
		t.Fatalf("X-Request-Id = %q, want a 26-char Crockford ULID", sent)
	}
	if pe.RequestID != sent {
		t.Errorf("error RequestID = %q, want the client id that was sent (%q)", pe.RequestID, sent)
	}
}

// TestRegistryRequestsCarryCorrelationID pins that the registry routes
// (GET /audio/voices, DELETE /audio/voices/<name>) carry the header too —
// "every request", not just /audio/speech.
func TestRegistryRequestsCarryCorrelationID(t *testing.T) {
	t.Helper()
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("X-Request-Id"))
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte(`{"voices":["default"],"uploaded_voices":[]}`))
		case http.MethodDelete:
			_, _ = w.Write([]byte(`{"success":true}`))
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)

	reg, err := NewRegistry(srv.URL, "test-token-12345")
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	ctx := context.Background()
	if _, err := reg.List(ctx); err != nil {
		t.Fatalf("List: %v", err)
	}
	if err := reg.Remove(ctx, "gn_test_0123abcd"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("requests = %d, want 2", len(got))
	}
	for i, id := range got {
		if !validRequestID(id) {
			t.Errorf("request %d X-Request-Id = %q, want a 26-char Crockford ULID", i, id)
		}
	}
	if got[0] == got[1] {
		t.Errorf("the two registry requests reused the client id %q", got[0])
	}
}

// TestNewRequestIDFormat pins the ULID shape: 26 Crockford characters, a
// leading character in 0-7 (48-bit timestamp field), unique across draws,
// and a timestamp prefix that decodes to the current instant.
func TestNewRequestIDFormat(t *testing.T) {
	seen := make(map[string]bool, 1000)
	var val [256]int8
	for i := range val {
		val[i] = -1
	}
	for v, c := range requestIDAlphabet {
		val[c] = int8(v)
	}
	before := time.Now().UTC()
	for i := 0; i < 1000; i++ {
		id := newRequestID()
		if !validRequestID(id) {
			t.Fatalf("id %q is not a well-formed Crockford ULID", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id after %d draws: %q", i, id)
		}
		seen[id] = true
		var ms uint64
		for j := 0; j < 10; j++ {
			ms = ms<<5 | uint64(val[id[j]])
		}
		// The first 10 characters carry 50 bits: 2 zero padding bits (130
		// bits of Crockford room for 128 of value) above the 48-bit
		// timestamp, so the decoded field is the timestamp shifted left 2.
		ms >>= 2
		at := time.UnixMilli(int64(ms)).UTC()
		// ULID floors to the millisecond, so compare at that granularity:
		// the id's ms is the floor of a moment no earlier than `before`.
		if at.Before(before.Truncate(time.Millisecond)) || at.After(before.Add(5*time.Minute)) {
			t.Fatalf("id %q decodes to %s, want ~now (%s)", id, at, before)
		}
	}
}

// TestRecordRequestID pins the resolution rule in isolation: a provider id
// wins verbatim (whitespace trimmed); anything else falls back to the
// client id with the source flipped.
func TestRecordRequestID(t *testing.T) {
	cases := []struct {
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, source := RecordRequestID(tc.provider, tc.client)
			if id != tc.wantID || source != tc.wantSource {
				t.Errorf("RecordRequestID(%q, %q) = (%q, %q), want (%q, %q)",
					tc.provider, tc.client, id, source, tc.wantID, tc.wantSource)
			}
		})
	}
}

// TestDesignSeedRetryKeepsTheSecondAttemptsID pins that a seed-rejection
// retry records the id of the request that actually succeeded (the second
// one), not the first.
func TestDesignSeedRetryKeepsTheSecondAttemptsID(t *testing.T) {
	// Answer the seed-bearing call with a 400, the seed-less retry with the
	// fixture WAV — the handler decides from the body.
	s := &recordingServer{t: t}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requestIDs = append(s.requestIDs, r.Header.Get("X-Request-Id"))
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["seed"] != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write(errorBody("field 'seed' is not supported by this deployment"))
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write(fixtureWAV(t))
	}))
	t.Cleanup(s.srv.Close)

	c := newTestClient(t, s.url())
	seed := 42
	a, err := c.Design(context.Background(), DesignRequest{Text: "Probe the light.", Seed: &seed})
	if err != nil {
		t.Fatalf("Design: %v", err)
	}
	if len(s.requestIDs) != 2 {
		t.Fatalf("requests = %d, want 2 (with seed, then without)", len(s.requestIDs))
	}
	if a.RequestID != s.requestIDs[1] || a.RequestIDSource != RequestIDSourceClient {
		t.Errorf("Audio = %q/%q, want the second attempt's client id with source %q",
			a.RequestID, a.RequestIDSource, RequestIDSourceClient)
	}
	if a.RequestID == s.requestIDs[0] {
		t.Errorf("the recorded id is the rejected first attempt's, not the successful retry's")
	}
}
