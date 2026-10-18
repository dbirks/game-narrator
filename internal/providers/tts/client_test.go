package tts

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
)

func newTestClient(t *testing.T, url string) *Client {
	t.Helper()
	c, err := New(url, "test-token-12345", "qwen3-tts-test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// TestDesignSendsTheVoiceDesignTaskType pins the wire contract: the
// VoiceDesign checkpoint requires task_type and the default (CustomVoice)
// is rejected by it, so the client must always send it.
func TestDesignSendsTheVoiceDesignTaskType(t *testing.T) {
	f := newFakeSpeechServer(t, func(i int, rec recordedRequest) (int, []byte, string) {
		return 200, fixtureWAV(t), "audio/wav"
	})
	c := newTestClient(t, f.url())
	seed := 42
	a, err := c.Design(context.Background(), DesignRequest{
		Text:         "The keeper counts three lanterns.",
		Instructions: "A calm, weathered female voice.",
		Language:     "English",
		Seed:         &seed,
	})
	if err != nil {
		t.Fatalf("Design: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(f.last().Body, &body); err != nil {
		t.Fatalf("request body is not JSON: %v", err)
	}
	if body["task_type"] != "VoiceDesign" {
		t.Errorf("task_type = %v, want \"VoiceDesign\" (required by the checkpoint)", body["task_type"])
	}
	if body["model"] != "qwen3-tts-test" {
		t.Errorf("model = %v", body["model"])
	}
	if body["input"] != "The keeper counts three lanterns." {
		t.Errorf("input = %v", body["input"])
	}
	if body["instructions"] != "A calm, weathered female voice." {
		t.Errorf("instructions = %v", body["instructions"])
	}
	if body["language"] != "English" {
		t.Errorf("language = %v", body["language"])
	}
	if body["seed"] != float64(42) {
		t.Errorf("seed = %v, want 42", body["seed"])
	}
	if body["response_format"] != "wav" {
		t.Errorf("response_format = %v", body["response_format"])
	}
	if got := f.last().Header.Get("Authorization"); got != "Bearer test-token-12345" {
		t.Errorf("Authorization = %q", got)
	}
	if got := f.last().Header.Get("X-Request-Id"); !validRequestID(got) {
		t.Errorf("X-Request-Id = %q, want a 26-char Crockford ULID", got)
	}
	if f.last().Path != "/audio/speech" {
		t.Errorf("path = %q", f.last().Path)
	}
	// The returned WAV must be validated, not just returned.
	if a.SampleRate != 24000 || a.Channels != 1 || a.BitsPerSample != 16 {
		t.Errorf("parsed WAV = %+v", a)
	}
	if a.Duration < 900*time.Millisecond || a.Duration > 1100*time.Millisecond {
		t.Errorf("duration = %s, want ~1 s", a.Duration)
	}
	if a.Latency <= 0 || a.RequestID == "" {
		t.Errorf("provenance missing: latency=%s request_id=%q", a.Latency, a.RequestID)
	}
	if a.RequestID != "req-1" || a.RequestIDSource != RequestIDSourceProvider {
		t.Errorf("provenance = %q/%q, want the provider's id (req-1) with source %q",
			a.RequestID, a.RequestIDSource, RequestIDSourceProvider)
	}
	if !strings.Contains(a.Origin, "voice_design") || !strings.Contains(a.Origin, "task_type=VoiceDesign") {
		t.Errorf("origin = %q", a.Origin)
	}
	if a.Seed == nil || *a.Seed != 42 || a.SeedUnsupported {
		t.Errorf("seed provenance = %v/%v", a.Seed, a.SeedUnsupported)
	}
	if len(f.requests) != 1 {
		t.Errorf("requests = %d, want 1", len(f.requests))
	}
}

// TestDesignSeedRejectionRetriesWithoutSeedAndRecordsIt pins the ADR-0005
// seed policy: seed is metadata, a rejecting deployment must not lose the
// call, and the rejection must be recorded, not hidden.
func TestDesignSeedRejectionRetriesWithoutSeedAndRecordsIt(t *testing.T) {
	f := newFakeSpeechServer(t, func(i int, rec recordedRequest) (int, []byte, string) {
		var body map[string]any
		_ = json.Unmarshal(rec.Body, &body)
		if body["seed"] != nil {
			return 400, errorBody("field 'seed' is not supported by this deployment"), ""
		}
		return 200, fixtureWAV(t), "audio/wav"
	})
	c := newTestClient(t, f.url())
	seed := 7
	a, err := c.Design(context.Background(), DesignRequest{Text: "Probe the light.", Seed: &seed})
	if err != nil {
		t.Fatalf("Design: %v", err)
	}
	if !a.SeedUnsupported || a.Seed != nil {
		t.Errorf("seed provenance = %v/%v, want dropped and recorded", a.Seed, a.SeedUnsupported)
	}
	if !strings.Contains(a.Origin, "seed_unsupported=true") {
		t.Errorf("origin = %q, want seed_unsupported=true recorded", a.Origin)
	}
	if len(f.requests) != 2 {
		t.Fatalf("requests = %d, want 2 (with seed, then without)", len(f.requests))
	}
	var second map[string]any
	_ = json.Unmarshal(f.nth(1).Body, &second)
	if _, ok := second["seed"]; ok {
		t.Errorf("retry still carries the seed field: %v", second)
	}
	if second["task_type"] != "VoiceDesign" {
		t.Errorf("retry lost task_type: %v", second)
	}
	// The rejection is sticky: the next Design must not pay for the 400 again.
	_, err = c.Design(context.Background(), DesignRequest{Text: "Probe again.", Seed: &seed})
	if err != nil {
		t.Fatalf("second Design: %v", err)
	}
	if len(f.requests) != 3 {
		t.Fatalf("requests = %d, want 3 (the second call must skip the seed)", len(f.requests))
	}
	if !c.SeedUnsupported() {
		t.Error("Client.SeedUnsupported() = false after a recorded rejection")
	}
}

// TestGenerateOmitsTaskTypeByDefault pins the Base contract for registered
// voices: the verified live behaviour is that a registered voice synthesises
// without task_type, so the default request must not carry one.
func TestGenerateOmitsTaskTypeByDefault(t *testing.T) {
	f := newFakeSpeechServer(t, func(i int, rec recordedRequest) (int, []byte, string) {
		return 200, fixtureWAV(t), "audio/wav"
	})
	c := newTestClient(t, f.url())
	_, err := c.Generate(context.Background(), GenerateRequest{
		Text:  "A second line for the keeper.",
		Voice: "gn_ab12cd34_narrator_v1_0123abcd",
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var body map[string]any
	_ = json.Unmarshal(f.last().Body, &body)
	if _, ok := body["task_type"]; ok {
		t.Errorf("default request carries task_type=%v; the checkpoint resolves it for registered voices", body["task_type"])
	}
	if body["voice"] != "gn_ab12cd34_narrator_v1_0123abcd" {
		t.Errorf("voice = %v", body["voice"])
	}
	if body["speed"] != nil || body["language"] != nil {
		t.Errorf("unset optional fields must be omitted: %v", body)
	}
}

// TestGenerateBaseTaskTypeRetry pins the negotiation: a Base checkpoint
// that rejects the server-default task gets exactly one task_type:"Base"
// retry, and the deployment is remembered for the next call.
func TestGenerateBaseTaskTypeRetry(t *testing.T) {
	f := newFakeSpeechServer(t, func(i int, rec recordedRequest) (int, []byte, string) {
		var body map[string]any
		_ = json.Unmarshal(rec.Body, &body)
		if body["task_type"] == "Base" {
			return 200, fixtureWAV(t), "audio/wav"
		}
		return 400, errorBody("Qwen3-TTS Base checkpoint does not support task_type='CustomVoice'. Use task_type='Base'."), ""
	})
	c := newTestClient(t, f.url())
	a, err := c.Generate(context.Background(), GenerateRequest{Text: "Hello keeper.", Voice: "default"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(f.requests) != 2 {
		t.Fatalf("requests = %d, want 2 (default task, then Base)", len(f.requests))
	}
	if !strings.Contains(a.Origin, "task_type=Base") {
		t.Errorf("origin = %q, want the negotiated task type recorded", a.Origin)
	}
	// Sticky: the next call sends task_type from the start.
	_, err = c.Generate(context.Background(), GenerateRequest{Text: "Again.", Voice: "default"})
	if err != nil {
		t.Fatalf("second Generate: %v", err)
	}
	var third map[string]any
	_ = json.Unmarshal(f.nth(2).Body, &third)
	if third["task_type"] != "Base" {
		t.Errorf("third request task_type = %v, want the remembered \"Base\"", third["task_type"])
	}
}

// TestGenerateMapsVoiceMissing pins the docs/spec/06 taxonomy for synthesis:
// a voice the instance cannot resolve is voice_missing, retryable by the
// router (reconcile, then fail over), not a schema accident.
func TestGenerateMapsVoiceMissing(t *testing.T) {
	cases := []struct {
		name string
		msg  string
	}{
		{"unknown voice", "voice 'gn_ab12cd34_narrator_v1_0123abcd' not found"},
		{"no embedding", "Base task requires 'ref_audio' or 'speaker_embedding' for voice cloning"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeSpeechServer(t, func(i int, rec recordedRequest) (int, []byte, string) {
				return 400, errorBody(tc.msg), ""
			})
			c := newTestClient(t, f.url())
			_, err := c.Generate(context.Background(), GenerateRequest{Text: "Hello.", Voice: "gn_ab12cd34_narrator_v1_0123abcd"})
			var pe *providers.Error
			if !errors.As(err, &pe) {
				t.Fatalf("error = %T %v, want *providers.Error", err, err)
			}
			if pe.Kind != providers.KindVoiceMissing {
				t.Errorf("kind = %s, want %s", pe.Kind, providers.KindVoiceMissing)
			}
			if pe.Status != 400 {
				t.Errorf("status = %d", pe.Status)
			}
		})
	}
}

// TestErrorMappingPinsTheTaxonomy covers the non-voice status mappings.
func TestErrorMappingPinsTheTaxonomy(t *testing.T) {
	cases := []struct {
		status int
		body   string
		kind   providers.Kind
		retry  bool
	}{
		{401, `{"error":{"message":"Invalid token"}}`, providers.KindAuth, false},
		{403, `{"error":{"message":"forbidden"}}`, providers.KindAuth, false},
		{404, `{"error":{"message":"unsupported path /audio/voices"}}`, providers.KindUnsupportedCapability, false},
		{413, `{"error":{"message":"too large"}}`, providers.KindPayloadTooLarge, false},
		{429, `{"error":{"message":"slow down"}}`, providers.KindRateLimited, true},
		{408, `{"error":{"message":"timed out"}}`, providers.KindTimeout, true},
		{503, `{"error":{"message":"overloaded"}}`, providers.KindUnavailable, true},
		{400, `{"error":{"message":"instructions is not supported for this model"}}`, providers.KindUnsupportedCapability, false},
		{400, `{"error":{"message":"input must be non-empty"}}`, providers.KindBadSchema, false},
	}
	for _, tc := range cases {
		t.Run(tc.body[:12], func(t *testing.T) {
			f := newFakeSpeechServer(t, func(i int, rec recordedRequest) (int, []byte, string) {
				return tc.status, []byte(tc.body), "application/json"
			})
			c := newTestClient(t, f.url())
			_, err := c.Generate(context.Background(), GenerateRequest{Text: "x", Voice: "v"})
			var pe *providers.Error
			if !errors.As(err, &pe) {
				t.Fatalf("error = %T %v", err, err)
			}
			if pe.Kind != tc.kind {
				t.Errorf("kind = %s, want %s", pe.Kind, tc.kind)
			}
			if pe.Retryable != tc.retry {
				t.Errorf("retryable = %v, want %v", pe.Retryable, tc.retry)
			}
			if strings.Contains(pe.Msg, "test-token-12345") {
				t.Errorf("error message leaked the token: %q", pe.Msg)
			}
		})
	}
}

// TestMalformedWAVIsAudioInvalid pins the docs/spec/07 quarantine rule: a 200
// with a broken file is a classified audio_invalid failure, not an asset.
func TestMalformedWAVIsAudioInvalid(t *testing.T) {
	f := newFakeSpeechServer(t, func(i int, rec recordedRequest) (int, []byte, string) {
		return 200, []byte("definitely-not-a-wav"), "audio/wav"
	})
	c := newTestClient(t, f.url())
	_, err := c.Generate(context.Background(), GenerateRequest{Text: "x", Voice: "v"})
	var pe *providers.Error
	if !errors.As(err, &pe) {
		t.Fatalf("error = %T %v", err, err)
	}
	if pe.Kind != providers.KindAudioInvalid {
		t.Errorf("kind = %s, want %s", pe.Kind, providers.KindAudioInvalid)
	}
	if pe.Retryable {
		t.Error("a malformed artifact is not transient")
	}
}

// TestContextDeadlineIsTimeout pins the cancellation contract: a deadline
// that expires mid-request is timeout (retryable), a caller cancel is
// cancelled.
func TestContextDeadlineIsTimeout(t *testing.T) {
	f := newFakeSpeechServer(t, func(i int, rec recordedRequest) (int, []byte, string) {
		time.Sleep(300 * time.Millisecond)
		return 200, fixtureWAV(t), "audio/wav"
	})
	c := newTestClient(t, f.url())
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := c.Generate(ctx, GenerateRequest{Text: "x", Voice: "v"})
	var pe *providers.Error
	if !errors.As(err, &pe) {
		t.Fatalf("error = %T %v", err, err)
	}
	if pe.Kind != providers.KindTimeout {
		t.Errorf("kind = %s, want %s", pe.Kind, providers.KindTimeout)
	}

	f2 := newFakeSpeechServer(t, func(i int, rec recordedRequest) (int, []byte, string) {
		time.Sleep(300 * time.Millisecond)
		return 200, fixtureWAV(t), "audio/wav"
	})
	c2 := newTestClient(t, f2.url())
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	_, err = c2.Generate(ctx2, GenerateRequest{Text: "x", Voice: "v"})
	if !errors.As(err, &pe) {
		t.Fatalf("error = %T %v", err, err)
	}
	if pe.Kind != providers.KindCancelled {
		t.Errorf("kind = %s, want %s", pe.Kind, providers.KindCancelled)
	}
}

// TestEmptyInputsFailBeforeAnyRequest: no HTTP may go out for an empty text
// or voice; a cold deployment is not a probe target.
func TestEmptyInputsFailBeforeAnyRequest(t *testing.T) {
	f := newFakeSpeechServer(t, func(i int, rec recordedRequest) (int, []byte, string) {
		return 200, fixtureWAV(t), "audio/wav"
	})
	c := newTestClient(t, f.url())
	if _, err := c.Design(context.Background(), DesignRequest{}); err == nil {
		t.Error("Design with empty text must fail")
	}
	if _, err := c.Generate(context.Background(), GenerateRequest{Voice: "v"}); err == nil {
		t.Error("Generate with empty text must fail")
	}
	if _, err := c.Generate(context.Background(), GenerateRequest{Text: "x"}); err == nil {
		t.Error("Generate with empty voice must fail")
	}
	if len(f.requests) != 0 {
		t.Errorf("requests = %d, want 0", len(f.requests))
	}
}

// TestClientLoggingHidesSecretsAndURLs pins the AGENTS.md rule-5 contract: a
// stray %v of the client must never print the token or the gateway URL.
func TestClientLoggingHidesSecretsAndURLs(t *testing.T) {
	f := newFakeSpeechServer(t, func(i int, rec recordedRequest) (int, []byte, string) {
		return 200, fixtureWAV(t), "audio/wav"
	})
	c := newTestClient(t, f.url())
	rendered := c.String()
	if strings.Contains(rendered, "test-token-12345") {
		t.Errorf("String leaked the token: %q", rendered)
	}
	if strings.Contains(rendered, f.url()) {
		t.Errorf("String leaked the base URL: %q", rendered)
	}
	lv, _ := c.LogValue().(map[string]any)
	if lv == nil {
		t.Fatal("LogValue did not render a map")
	}
	if lv["token"] != providers.MaskValue {
		t.Errorf("LogValue token = %v, want the mask", lv["token"])
	}
}

// TestNewRejectsMalformedURLs keeps configuration faults loud at startup.
func TestNewRejectsMalformedURLs(t *testing.T) {
	for _, bad := range []string{"", "   ", "ftp://host/v1", "http://", "http://user:pass@host/v1"} {
		if _, err := New(bad, "t", "m"); err == nil {
			t.Errorf("New(%q) accepted a malformed URL", bad)
		}
	}
	if _, err := New("http://ok.invalid/v1", "t", "m"); err != nil {
		t.Errorf("New rejected a valid URL: %v", err)
	}
}

// TestNewFromRoleEnforcesTheTTSRoles pins which roles this adapter serves.
func TestNewFromRoleEnforcesTheTTSRoles(t *testing.T) {
	valid := providers.RoleConfig{Role: providers.RoleVoiceDesign, Protocol: providers.ProtocolVLLMOmniSpeech, Model: "m", BaseURL: "http://ok.invalid/v1", Timeout: time.Second}
	if _, err := NewFromRole(valid); err != nil {
		t.Errorf("NewFromRole rejected a valid voice_design role: %v", err)
	}
	noURL := providers.RoleConfig{Role: providers.RoleVoiceDesign, Protocol: providers.ProtocolVLLMOmniSpeech, Model: "m"}
	if _, err := NewFromRole(noURL); err == nil {
		t.Error("NewFromRole accepted a voice role with no BaseURL")
	}
	chat := providers.RoleConfig{Role: providers.RoleFastReasoning, Protocol: providers.ProtocolOpenAIChat, Model: "m", BaseURL: "http://ok.invalid/v1"}
	if _, err := NewFromRole(chat); err == nil {
		t.Error("NewFromRole accepted a non-TTS role")
	}
	wrongProto := providers.RoleConfig{Role: providers.RoleVoiceClone, Protocol: providers.ProtocolOpenAIChat, Model: "m", BaseURL: "http://ok.invalid/v1"}
	if _, err := NewFromRole(wrongProto); err == nil {
		t.Error("NewFromRole accepted a voice role on the chat protocol")
	}
	unconfigured := providers.RoleConfig{Role: providers.RoleVoiceClone, Protocol: providers.ProtocolVLLMOmniSpeech, BaseURL: "http://ok.invalid/v1"}
	if _, err := NewFromRole(unconfigured); err == nil {
		t.Error("NewFromRole accepted an unconfigured role")
	}
}
