package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
)

// These are the LIVE contract tests for the "Contract" row of docs/spec/12
// ("provider schema — recorded every PR, live conditional"). They call a real
// OpenAI-compatible gateway and are the only proof that the recorded fixtures in
// client_test.go still match deployed behaviour.
//
// Configuration comes from the environment only:
//
//	GAME_NARRATOR_GATEWAY_URL          base URL of the compatible gateway
//	GAME_NARRATOR_GATEWAY_TOKEN_ENV    names the variable holding the token
//	                                  (default GAME_NARRATOR_GATEWAY_TOKEN)
//	GAME_NARRATOR_LIVE_MODEL           model id to probe
//	                                  (default qwen3-8-27b)
//
// Nothing here may print a URL, host or token. Log lines carry the model id,
// latency and token counts only.

const defaultLiveModel = "qwen3-8-27b"

const liveSkipMessage = "NOT RUN: set GAME_NARRATOR_GATEWAY_URL and the token env named by " +
	providers.EnvGatewayTokenEnv + " (default " + providers.EnvGatewayToken + ") to run live provider contract tests"

// liveClient loads the shared gateway through the normal role configuration
// path, so the live tests exercise the same resolution code the service uses.
func liveClient(t *testing.T, timeout time.Duration) *Client {
	t.Helper()
	// Gate marker (ADR-0065): live tests run only under make test-models;
	// the unit gate (and any manual go test) must never call the gateway.
	if os.Getenv(providers.EnvGate) != "test-models" {
		t.Skipf("live provider test outside the test-models gate: run via make test-models (it sets %s=test-models)", providers.EnvGate)
	}
	roles, err := providers.LoadRoles(os.LookupEnv)
	if err != nil {
		// A malformed gateway URL is a configuration fault, not a missing
		// capability, so it must fail loudly rather than skip.
		t.Fatalf("LoadRoles: %v", err)
	}
	cfg := roles[providers.RoleFastReasoning]
	if strings.TrimSpace(cfg.BaseURL) == "" {
		t.Skip(liveSkipMessage)
	}
	if strings.TrimSpace(cfg.Token()) == "" {
		t.Skip(liveSkipMessage)
	}
	model := strings.TrimSpace(os.Getenv("GAME_NARRATOR_LIVE_MODEL"))
	if model == "" {
		model = defaultLiveModel
	}
	cfg.Model = model
	cfg.Timeout = timeout
	client, err := NewFromRole(cfg)
	if err != nil {
		t.Fatalf("NewFromRole(%s): %v", model, err)
	}
	return client
}

func TestLiveListModelsReportsTheConfiguredModel(t *testing.T) {
	client := liveClient(t, 30*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	ids, err := client.ListModels(ctx)
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	found := false
	for _, id := range ids {
		if id == client.Model {
			found = true
			break
		}
	}
	if !found {
		// Ids are deployment-scoped labels, not secrets: listing them shows
		// whether the configured role points at something actually served.
		t.Errorf("model %q is not served by the configured gateway; got ids %v", client.Model, ids)
	}
	t.Logf("live ListModels: model=%s present=true count=%d latency=%s", client.Model, len(ids), time.Since(start).Round(time.Millisecond))
}

func TestLiveChatTextAnswersPong(t *testing.T) {
	client := liveClient(t, 90*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	resp, err := client.Chat(ctx, ChatRequest{
		Messages:  []Message{{Role: "user", Text: "Reply with exactly: pong"}},
		MaxTokens: 200,
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if !strings.EqualFold(resp.Text, "pong") {
		t.Errorf("Text = %q, want \"pong\" (case-insensitive, trimmed)", resp.Text)
	}
	if resp.Usage.Prompt <= 0 || resp.Usage.Completion <= 0 {
		t.Errorf("Usage = %+v, want positive prompt and completion token counts", resp.Usage)
	}
	if resp.Latency <= 0 {
		t.Errorf("Latency = %s, want a measured duration", resp.Latency)
	}
	if resp.Model == "" {
		t.Error("Model is empty; provenance must be recorded")
	}
	t.Logf("live Chat text: model=%s finish=%s latency=%s usage=%d/%d reasoning_tokens=%d reasoning_chars=%d request_id=%s",
		resp.Model, resp.FinishReason, resp.Latency.Round(time.Millisecond),
		resp.Usage.Prompt, resp.Usage.Completion, resp.Usage.Reasoning, len(resp.Reasoning), resp.RequestID)
	if resp.Usage.Reasoning > 0 && resp.Usage.Reasoning >= resp.Usage.Completion {
		t.Errorf("usage.reasoning_tokens=%d exceeds completion_tokens=%d; the accounting is inconsistent",
			resp.Usage.Reasoning, resp.Usage.Completion)
	}
}

// TestLiveReasoningEffortNoneYieldsZeroReasoningTokens proves the per-request
// reasoning_effort override against the real gateway: with effort "none" a
// short prompt must come back with zero reasoning tokens, which is what keeps
// the latency-critical roles inside their ceilings. If the gateway does not
// report reasoning token accounting at all, the test skips with a finding
// instead of asserting on a field that is not observable.
func TestLiveReasoningEffortNoneYieldsZeroReasoningTokens(t *testing.T) {
	client := liveClient(t, 90*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	resp, err := client.Chat(ctx, ChatRequest{
		Messages:        []Message{{Role: "user", Text: "Reply with exactly: pong"}},
		MaxTokens:       128,
		ReasoningEffort: "none",
	})
	if err != nil {
		var pe *providers.Error
		if errors.As(err, &pe) && IsReasoningEffortRejection(err) {
			t.Fatalf("live gateway rejects reasoning_effort: %s", pe)
		}
		t.Fatalf("Chat with reasoning_effort=none: %v", err)
	}
	// Observable accounting: the gateway reports reasoning tokens either under
	// usage.reasoning_tokens or usage.completion_tokens_details.reasoning_tokens
	// (both land in Usage.Reasoning). It reports *something* about completion
	// tokens; if reasoning accounting is absent the field is simply zero and
	// indistinguishable from "zero reported". The live probe (qwen3-8-27b,
	// 2026-10-18) confirmed this gateway reports completion_tokens_details.
	if resp.Usage.Completion <= 0 {
		t.Fatalf("Usage.Completion = %d, want a positive completion count (gateway not reporting usage?)", resp.Usage.Completion)
	}
	if resp.Usage.Reasoning != 0 {
		t.Errorf("reasoning_tokens = %d, want 0 for reasoning_effort=none (usage=%+v)", resp.Usage.Reasoning, resp.Usage)
	}
	if resp.Reasoning != "" {
		t.Errorf("reasoning_content = %q, want empty for reasoning_effort=none", truncateForLog(resp.Reasoning))
	}
	if !strings.EqualFold(resp.Text, "pong") {
		t.Errorf("Text = %q, want \"pong\"", resp.Text)
	}
	t.Logf("live reasoning_effort=none: model=%s finish=%s latency=%s usage=%d/%d reasoning_tokens=%d (gateway reports reasoning accounting: verified live 2026-10-18)",
		resp.Model, resp.FinishReason, resp.Latency.Round(time.Millisecond),
		resp.Usage.Prompt, resp.Usage.Completion, resp.Usage.Reasoning)
}

func truncateForLog(s string) string {
	if len(s) <= 120 {
		return s
	}
	return s[:120] + "…"
}

// TestLiveChatVisionNamesBothColors sends a deterministic 64x64 image (left half
// pure red, right half pure blue) as a data URL and requires the model to name
// both colours. This is the vision_ocr_fallback role's real capability probe.
// The gateway's vision capability was cleared 2026-10-17 (docs/status/blocked.md),
// so the test runs with the other live tests: sequential (no t.Parallel) and
// skipped with NOT RUN when the gateway is not configured.
func TestLiveChatVisionNamesBothColors(t *testing.T) {
	client := liveClient(t, 120*time.Second)
	img := redBluePNG(t, 64, 64)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	resp, err := client.Chat(ctx, ChatRequest{
		Messages: []Message{{
			Role:   "user",
			Text:   "What two colors are in this image, left then right? Answer as two lowercase words.",
			Images: [][]byte{img},
		}},
		MaxTokens: 300,
	})
	if err != nil {
		var pe *providers.Error
		if errors.As(err, &pe) && pe.Kind == providers.KindUnsupportedCapability && imageRejected(pe) {
			// Record it rather than pretend the capability exists: a gateway
			// that refuses image parts cannot serve vision_ocr_fallback.
			t.Errorf("live gateway rejects image parts (vision_ocr_fallback unusable): %s", pe)
			return
		}
		t.Fatalf("vision Chat: %v", err)
	}
	lower := strings.ToLower(resp.Text)
	if !strings.Contains(lower, "red") || !strings.Contains(lower, "blue") {
		t.Errorf("Text = %q, want it to name both red and blue", resp.Text)
	}
	if red, blue := strings.Index(lower, "red"), strings.Index(lower, "blue"); red >= 0 && blue >= 0 && red > blue {
		// Left/right order is the semantic content of the probe, but phrasing
		// such as "blue on the right, red on the left" is a correct answer in a
		// different order. Surface it, do not fail on it.
		t.Logf("NOTE: colours named in reverse order (%q); verify phrasing manually", resp.Text)
	}
	t.Logf("live vision Chat: model=%s bytes=%d finish=%s latency=%s usage=%d/%d text=%q",
		resp.Model, len(img), resp.FinishReason, resp.Latency.Round(time.Millisecond),
		resp.Usage.Prompt, resp.Usage.Completion, resp.Text)
}

func TestLiveChatJSONModeReturnsParsableObject(t *testing.T) {
	client := liveClient(t, 90*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	resp, err := client.Chat(ctx, ChatRequest{
		Messages: []Message{{
			Role: "user",
			Text: `Return only a JSON object with the single boolean key "ok" set to true. No prose, no code fences.`,
		}},
		MaxTokens:          300,
		ResponseFormatJSON: true,
	})
	if err != nil {
		var pe *providers.Error
		if errors.As(err, &pe) && IsJSONModeRejection(err) {
			t.Logf("CAPABILITY FINDING: live gateway rejects response_format json_object (%s); "+
				"vision/agent prompts must ask for JSON in text and tolerate prose", pe)
			return
		}
		t.Fatalf("JSON-mode Chat: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(stripCodeFence(resp.Text)), &parsed); err != nil {
		t.Errorf("Text = %q is not parsable JSON even though json_object was accepted: %v", resp.Text, err)
		return
	}
	if ok, isBool := parsed["ok"]; !isBool || ok != true {
		t.Errorf("parsed = %v, want {\"ok\":true}", parsed)
	}
	t.Logf("live JSON mode: model=%s supported=true latency=%s usage=%d/%d",
		resp.Model, resp.Latency.Round(time.Millisecond), resp.Usage.Prompt, resp.Usage.Completion)
}

// TestLiveJSONModeFallbackReturnsObject is the degraded JSON path against the
// real gateway: a JSONModeFallback request must yield one parseable JSON object
// whether the gateway accepts response_format or rejects it (then the client
// retries once without it plus the JSON-only instruction). It logs which path
// was taken from the client's capability snapshot.
func TestLiveJSONModeFallbackReturnsObject(t *testing.T) {
	client := liveClient(t, 90*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	resp, err := client.Chat(ctx, ChatRequest{
		Messages: []Message{{
			Role: "user",
			Text: `Return only a JSON object with the single boolean key "ok" set to true. No prose, no code fences.`,
		}},
		MaxTokens:          300,
		ResponseFormatJSON: true,
		JSONModeFallback:   true,
	})
	if err != nil {
		t.Fatalf("JSON-mode fallback Chat: %v", err)
	}
	// The response must contain one parseable JSON object. In the degraded
	// path the client normalised Text to the extracted object; in the
	// accepted path the gateway guaranteed structured output. extractJSONObject
	// covers both (it also tolerates a stray code fence).
	obj, ok := extractJSONObject(resp.Text)
	if !ok {
		t.Fatalf("Text = %q does not contain a single parseable JSON object", resp.Text)
	}
	var parsed map[string]any
	if err := json.Unmarshal(obj, &parsed); err != nil {
		t.Fatalf("extracted object %s is not a JSON object: %v", obj, err)
	}
	if v, isBool := parsed["ok"]; !isBool || v != true {
		t.Errorf("parsed = %v, want {\"ok\":true}", parsed)
	}
	// Log which path was taken, from the recorded capability finding.
	switch client.Capabilities().JSONMode {
	case CapabilityRejected:
		t.Logf("CAPABILITY FINDING: live gateway rejects response_format; the JSON-mode "+
			"fallback was taken (degraded to plain-text JSON). model=%s latency=%s usage=%d/%d",
			resp.Model, resp.Latency.Round(time.Millisecond), resp.Usage.Prompt, resp.Usage.Completion)
	case CapabilitySupported:
		t.Logf("live JSON-mode fallback NOT needed: gateway accepted response_format. model=%s latency=%s usage=%d/%d",
			resp.Model, resp.Latency.Round(time.Millisecond), resp.Usage.Prompt, resp.Usage.Completion)
	default:
		t.Errorf("Capabilities().JSONMode = %q, want rejected or supported after a JSON-mode request",
			client.Capabilities().JSONMode)
	}
}

// TestLiveChatRejectsUnknownModelLive confirms the 404/model-not-found mapping
// against the real gateway, which is the case that decides whether a role is
// misconfigured or merely overloaded.
func TestLiveChatRejectsUnknownModelLive(t *testing.T) {
	client := liveClient(t, 60*time.Second)
	client.Model = "gn-does-not-exist-" + time.Now().UTC().Format("20060102150405")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err := client.Chat(ctx, ChatRequest{
		Messages:  []Message{{Role: "user", Text: "Reply with exactly: pong"}},
		MaxTokens: 16,
	})
	if err == nil {
		t.Skip("live gateway accepted a nonexistent model id; cannot verify the unsupported_capability mapping here")
	}
	var pe *providers.Error
	if !errors.As(err, &pe) {
		t.Fatalf("error = %v (%T), want *providers.Error", err, err)
	}
	if pe.Kind != providers.KindUnsupportedCapability {
		t.Errorf("Kind = %q, want unsupported_capability for an unknown model (err=%s)", pe.Kind, pe)
	}
	if pe.Retryable {
		t.Error("an unknown model must not be retried")
	}
	t.Logf("live unknown-model probe: kind=%s status=%d retryable=%v request_id=%s", pe.Kind, pe.Status, pe.Retryable, pe.RequestID)
}

// stripCodeFence removes a markdown code fence some chat models wrap JSON in
// even when structured output was requested, so the parse asserts the model's
// answer rather than its formatting habits.
func stripCodeFence(text string) string {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "```") {
		return text
	}
	text = strings.TrimPrefix(text, "```")
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		first := strings.TrimSpace(text[:i])
		if first == "" || strings.EqualFold(first, "json") {
			text = text[i+1:]
		}
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(text), "```"))
}

func imageRejected(pe *providers.Error) bool {
	lower := strings.ToLower(pe.Msg)
	return strings.Contains(lower, "image") || strings.Contains(lower, "content") || strings.Contains(lower, "modalit")
}

// redBluePNG draws a deterministic image with no third-party dependency: left
// half pure red, right half pure blue, so the expected answer is unambiguous.
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
		t.Fatalf("encode PNG: %v", err)
	}
	if buf.Len() > MaxImageBytes {
		t.Fatalf("test fixture is %d bytes, over the %d byte limit", buf.Len(), MaxImageBytes)
	}
	return buf.Bytes()
}
