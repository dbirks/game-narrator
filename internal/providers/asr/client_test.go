package asr

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
)

// parseForm returns the multipart fields and the file part of a transcription
// request, failing the test on a malformed body.
func parseForm(t *testing.T, r *http.Request) (fields map[string]string, file []byte, fileName string) {
	t.Helper()
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		t.Fatalf("ParseMultipartForm: %v", err)
	}
	fields = map[string]string{}
	for k, v := range r.MultipartForm.Value {
		if len(v) != 1 {
			t.Fatalf("field %q has %d values, want 1", k, len(v))
		}
		fields[k] = v[0]
	}
	fileNames := r.MultipartForm.File["file"]
	if len(fileNames) != 1 {
		t.Fatalf("file part: got %d, want 1", len(fileNames))
	}
	fh, err := fileNames[0].Open()
	if err != nil {
		t.Fatalf("open file part: %v", err)
	}
	defer fh.Close()
	file, err = io.ReadAll(fh)
	if err != nil {
		t.Fatalf("read file part: %v", err)
	}
	fileName = fileNames[0].Filename
	return fields, file, fileName
}

func jsonResponse(t *testing.T, w http.ResponseWriter, status int, payload any, reqID string) {
	t.Helper()
	if reqID != "" {
		w.Header().Set("X-Request-Id", reqID)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		t.Fatalf("encode: %v", err)
	}
}

func TestTranscribeSuccessVerbose(t *testing.T) {
	const text = "The harbour bell rang twice."
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/audio/transcriptions" {
			t.Fatalf("route = %s %s, want POST /audio/transcriptions", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok-1" {
			t.Fatalf("Authorization = %q", got)
		}
		rid := r.Header.Get("X-Request-Id")
		if !validRequestID(rid) {
			t.Fatalf("X-Request-Id = %q, want a client-minted ULID", rid)
		}
		fields, wav, fname := parseForm(t, r)
		if fields["model"] != "qwen3-asr-1-7b" || fields["response_format"] != "verbose_json" || fields["language"] != "en" {
			t.Fatalf("fields = %v, want model/response_format/language", fields)
		}
		if string(wav) != "THE-INPUT-BYTES" || fname != "audio.wav" {
			t.Fatalf("file = %q (%q), want the request bytes as audio.wav", wav, fname)
		}
		jsonResponse(t, w, http.StatusOK, map[string]any{
			"task": "transcribe", "language": "en", "duration": 2.4, "text": text,
		}, "")
	}))
	defer srv.Close()
	c, err := New(srv.URL, "tok-1", "qwen3-asr-1-7b")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := c.Transcribe(context.Background(), TranscribeRequest{WAV: []byte("THE-INPUT-BYTES"), Language: "en"})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if res.Text != text || res.Language != "en" || res.DurationS != 2.4 {
		t.Fatalf("result = %+v, want text/language/duration", res)
	}
	if res.Model != "qwen3-asr-1-7b" {
		t.Fatalf("Model = %q", res.Model)
	}
	if res.RequestIDSource != RequestIDSourceClient || !validRequestID(res.RequestID) {
		t.Fatalf("RequestID=%q source=%q, want the client-minted id", res.RequestID, res.RequestIDSource)
	}
	if res.Latency <= 0 {
		t.Fatalf("Latency = %s, want > 0", res.Latency)
	}
}

// The deployed qwen3-asr-1-7b transcribes the doctor's probe tone to an empty
// transcript (measured 2026-10-18): an empty `text` on a 200 is the endpoint
// saying "no speech here", a VALID result — not a wire failure. The client
// reports it faithfully; the policy (QA flags it, the doctor accepts it)
// belongs to the caller.
func TestTranscribeEmptyTranscriptIsAValidResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(t, w, http.StatusOK, map[string]any{
			"task": "transcribe", "duration": 0.25, "text": "",
		}, "")
	}))
	defer srv.Close()
	c, err := New(srv.URL, "tok-1", "qwen3-asr-1-7b")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := c.Transcribe(context.Background(), TranscribeRequest{WAV: []byte("PROBE-TONE")})
	if err != nil {
		t.Fatalf("Transcribe: an empty transcript is a valid result, not an error: %v", err)
	}
	if res.Text != "" || res.DurationS != 0.25 {
		t.Fatalf("result = %+v, want the empty transcript with its duration", res)
	}
	if res.RequestIDSource != RequestIDSourceClient || !validRequestID(res.RequestID) {
		t.Fatalf("RequestID=%q source=%q, want the client-minted id", res.RequestID, res.RequestIDSource)
	}
}

func TestTranscribeJSONShapeAndProviderIDWins(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fields, _, _ := parseForm(t, r)
		if fields["response_format"] != "json" {
			t.Fatalf("response_format = %q, want json", fields["response_format"])
		}
		if _, ok := fields["language"]; ok {
			t.Fatalf("language field must be omitted when the request has none")
		}
		jsonResponse(t, w, http.StatusOK, map[string]any{"text": "pong"}, "provider-id-123")
	}))
	defer srv.Close()
	c, err := New(srv.URL, "", "m")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := c.Transcribe(context.Background(), TranscribeRequest{WAV: []byte("x"), ResponseFormat: FormatJSON})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if res.Text != "pong" {
		t.Fatalf("Text = %q", res.Text)
	}
	if res.RequestID != "provider-id-123" || res.RequestIDSource != RequestIDSourceProvider {
		t.Fatalf("RequestID=%q source=%q, want the provider id to win", res.RequestID, res.RequestIDSource)
	}
}

func TestTranscribeErrorTaxonomy(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   providers.Kind
		retry  bool
		capab  bool // the body names an unsupported capability
	}{
		{"auth", http.StatusUnauthorized, `{"error":{"message":"bad token"}}`, providers.KindAuth, false, false},
		{"not-found-route", http.StatusNotFound, `{"error":{"message":"no such route"}}`, providers.KindUnsupportedCapability, false, false},
		{"rate-limited", http.StatusTooManyRequests, `{"error":{"message":"slow down"}}`, providers.KindRateLimited, true, false},
		{"server-error", http.StatusServiceUnavailable, `{"error":{"message":"down"}}`, providers.KindUnavailable, true, false},
		{"bad-schema", http.StatusBadRequest, `{"error":{"message":"file part missing"}}`, providers.KindBadSchema, false, false},
		{"unsupported-format", http.StatusBadRequest, `{"error":{"message":"response_format verbose_json is not supported"}}`, providers.KindUnsupportedCapability, false, true},
		{"request-timeout", http.StatusRequestTimeout, `{"error":{"message":"timed out"}}`, providers.KindTimeout, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			c, err := New(srv.URL, "t", "m")
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, err = c.Transcribe(context.Background(), TranscribeRequest{WAV: []byte("x")})
			var pe *providers.Error
			if !errors.As(err, &pe) {
				t.Fatalf("err = %v, want *providers.Error", err)
			}
			if pe.Kind != tc.want {
				t.Fatalf("Kind = %s, want %s", pe.Kind, tc.want)
			}
			if pe.Retryable != tc.retry {
				t.Fatalf("Retryable = %v, want %v", pe.Retryable, tc.retry)
			}
			if !validRequestID(pe.RequestID) {
				t.Fatalf("RequestID = %q, want the client id on the failure too", pe.RequestID)
			}
			if pe.Model != "m" {
				t.Fatalf("Model = %q", pe.Model)
			}
		})
	}
}

func TestTranscribeDecodeErrors(t *testing.T) {
	// Note: a 200 with an empty `text` is NOT a decode error here — the
	// endpoint saying "no speech" is a valid result (TestTranscribeEmpty
	// TranscriptIsAValidResult). A decode error is a body that is not a
	// transcription object at all: malformed JSON, not an object, or an
	// object that omits the `task` field (schema drift — a different service
	// answering).
	bodies := map[string]string{
		"malformed-json": `{not json`,
		"not-an-object":  `[1, 2, 3]`,
		"missing-task":   `{"text":"","duration":0.25}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				io.WriteString(w, body)
			}))
			defer srv.Close()
			c, err := New(srv.URL, "t", "m")
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, err = c.Transcribe(context.Background(), TranscribeRequest{WAV: []byte("x")})
			var pe *providers.Error
			if !errors.As(err, &pe) {
				t.Fatalf("err = %v, want *providers.Error", err)
			}
			if pe.Kind != providers.KindDecodeError {
				t.Fatalf("Kind = %s, want decode_error", pe.Kind)
			}
			if pe.Retryable {
				t.Fatalf("a decode error is not retryable by default")
			}
		})
	}
}

func TestTranscribeClientTimeoutAndCancellation(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		jsonResponse(t, w, http.StatusOK, map[string]any{"text": "late"}, "")
	}))
	defer slow.Close()
	c, err := New(slow.URL, "t", "m")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.HTTP = &http.Client{Timeout: 100 * time.Millisecond}
	if _, err := c.Transcribe(context.Background(), TranscribeRequest{WAV: []byte("x")}); err == nil {
		t.Fatal("want a timeout error")
	} else if !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("err = %v, want timeout", err)
	}

	c2, _ := New(slow.URL, "t", "m")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c2.Transcribe(ctx, TranscribeRequest{WAV: []byte("x")}); err == nil {
		t.Fatal("want a cancellation error")
	}
}

func TestTranscribeRejectsEmptyAudioBeforeAnyRequest(t *testing.T) {
	c, err := New("http://127.0.0.1:1", "t", "m")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Transcribe(context.Background(), TranscribeRequest{}); err == nil {
		t.Fatal("want an error for empty audio")
	}
}

func TestProbeTranscribesTheTone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fields, wav, _ := parseForm(t, r)
		if fields["response_format"] != string(FormatVerboseJSON) {
			t.Fatalf("response_format = %q, want the verbose default", fields["response_format"])
		}
		if len(wav) < 44 || string(wav[0:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
			t.Fatalf("probe WAV malformed (%d bytes)", len(wav))
		}
		jsonResponse(t, w, http.StatusOK, map[string]any{"task": "transcribe", "language": "en", "duration": 0.3, "text": "tone"}, "")
	}))
	defer srv.Close()
	c, err := New(srv.URL, "t", "m")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := c.Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if res.Text != "tone" {
		t.Fatalf("Text = %q", res.Text)
	}
}

func TestNewValidationAndRoleBinding(t *testing.T) {
	if _, err := New("", "t", "m"); err == nil {
		t.Fatal("want an error for an empty base URL")
	}
	if _, err := New("ftp://host", "t", "m"); err == nil {
		t.Fatal("want an error for a non-http scheme")
	}
	if _, err := New("https://user:pass@host/v1", "t", "m"); err == nil {
		t.Fatal("want an error for userinfo in the URL")
	}

	roleCfg := func(role providers.Role, protocol providers.Protocol) providers.RoleConfig {
		return providers.RoleConfig{Role: role, Protocol: protocol, BaseURL: "https://gateway.example.invalid/v1", Model: "m", Timeout: time.Second}
	}
	if _, err := NewFromRole(roleCfg(providers.RoleVoiceClone, providers.ProtocolOpenAITranscriptions)); err == nil {
		t.Fatal("want an error for a non-ASR role")
	}
	if _, err := NewFromRole(providers.RoleConfig{Role: providers.RoleSpeechASR, Protocol: providers.ProtocolOpenAITranscriptions, BaseURL: "https://x.invalid/v1"}); err == nil {
		t.Fatal("want an error for a role with no model")
	}
	if _, err := NewFromRole(roleCfg(providers.RoleSpeechASR, providers.ProtocolSystemOne)); err == nil {
		t.Fatal("want an error for a protocol with no ASR adapter")
	}
	c, err := NewFromRole(roleCfg(providers.RoleSpeechASR, providers.ProtocolOpenAITranscriptions))
	if err != nil {
		t.Fatalf("NewFromRole: %v", err)
	}
	if c.BaseURL != "https://gateway.example.invalid/v1" || c.Model != "m" || c.HTTP == nil || c.HTTP.Timeout != time.Second {
		t.Fatalf("client = %+v", c)
	}

	// Token masking is a property of the client itself: build one directly
	// (the role config's token is unexported, so NewFromRole is the wiring
	// test above and New is the credential-handling test here).
	c2, err := New("https://gateway.example.invalid/v1", "sekrit-token", "m")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, leak := range []string{"sekrit-token", "gateway.example.invalid"} {
		if strings.Contains(c2.String(), leak) {
			t.Fatalf("String leaks %q: %s", leak, c2.String())
		}
	}
	if !strings.Contains(c2.String(), "m") {
		t.Fatalf("String should name the model: %s", c2.String())
	}
	lv, _ := c2.LogValue().(map[string]any)
	if lv["token"] != providers.MaskValue {
		t.Fatalf("LogValue token = %v, want the mask", lv["token"])
	}
}

func TestRecordRequestID(t *testing.T) {
	id, src := RecordRequestID("", "clientid1")
	if id != "clientid1" || src != RequestIDSourceClient {
		t.Fatalf("(id=%q src=%q), want the client id", id, src)
	}
	id, src = RecordRequestID("  provider-id  ", "clientid1")
	if id != "provider-id" || src != RequestIDSourceProvider {
		t.Fatalf("(id=%q src=%q), want the provider id", id, src)
	}
}

func TestNewRequestIDUniqueAndWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := newRequestID()
		if !validRequestID(id) {
			t.Fatalf("id %q is not well formed", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}
