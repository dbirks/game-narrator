package doctor

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/openai"
)

// openAIProbe is the real ProviderProbe: it builds the same adapter the runtime
// uses and sends real requests. The doctor must not report a role as working on
// the strength of configuration alone, so this is what the CLI exercises.
type openAIProbe struct{}

func (openAIProbe) ListModels(ctx context.Context, cfg providers.RoleConfig) ([]string, error) {
	client, err := probeClient(cfg)
	if err != nil {
		return nil, err
	}
	return client.ListModels(ctx)
}

func (openAIProbe) Chat(ctx context.Context, cfg providers.RoleConfig, prompt string, images [][]byte) (ChatResult, error) {
	client, err := probeClient(cfg)
	if err != nil {
		return ChatResult{}, err
	}
	resp, err := client.Chat(ctx, openai.ChatRequest{
		Messages: []openai.Message{{Role: "user", Text: prompt, Images: images}},
	})
	if err != nil {
		return ChatResult{}, err
	}
	return ChatResult{Text: resp.Text, Model: resp.Model, Latency: resp.Latency}, nil
}

// probeCapabilitiesProbeJSON and probeCapabilitiesProbeTool are the cheap, fixed
// prompts for the optional-capability probes (docs/spec/06). They are content
// probes, not warm-and-fuzzy pings: the gateway either accepts the feature
// (200) or rejects it (400), which the adapter records as a finding.
const (
	probeCapabilitiesJSONPrompt = `Return only a JSON object with the single boolean key "ok" set to true.`
	probeCapabilitiesToolPrompt = `Use the probe_echo tool to echo the exact text "probe".`
	probeCapabilitiesToolSchema = `{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`
)

// ProbeCapabilities exercises the role's optional capabilities and returns the
// findings the adapter recorded. It sends one JSON-mode request (response_format
// plus the degraded fallback) and one tool_choice=required request (plus the
// auto fallback). Both failures are capability findings, not probe errors, so
// the individual responses are intentionally ignored: only the recorded
// snapshot is returned. A client-construction failure is the only error.
func (openAIProbe) ProbeCapabilities(ctx context.Context, cfg providers.RoleConfig) (openai.Capabilities, error) {
	client, err := probeClient(cfg)
	if err != nil {
		return openai.Capabilities{}, err
	}
	// JSON mode: the fallback is enabled so a rejection degrades instead of
	// aborting, and the finding (rejected/supported) is recorded either way.
	_, _ = client.Chat(ctx, openai.ChatRequest{
		Messages:           []openai.Message{{Role: "user", Text: probeCapabilitiesJSONPrompt}},
		MaxTokens:          64,
		ResponseFormatJSON: true,
		JSONModeFallback:   true,
	})
	// tool_choice=required: the fallback is enabled so a rejection degrades to
	// auto; a missing tool call after degradation is ErrToolCallMissing, which
	// is a capability finding, not a probe error.
	_, _ = client.Chat(ctx, openai.ChatRequest{
		Messages:           []openai.Message{{Role: "user", Text: probeCapabilitiesToolPrompt}},
		MaxTokens:          64,
		Tools:              []openai.ToolDef{{Name: "probe_echo", Description: "Echo the given text back verbatim.", Parameters: json.RawMessage(probeCapabilitiesToolSchema)}},
		ToolChoice:         openai.ToolChoiceRequired,
		ToolChoiceFallback: true,
	})
	return client.Capabilities(), nil
}

// probeClient builds the role's client, then removes the role's runtime latency
// budget as a transport timeout: 10 s is the ceiling for an in-loop observation,
// not for a diagnostic that a user runs deliberately, where a cold model load is
// normal. The check's own context deadline (doctor.Options.ModelTimeout) bounds
// the probe instead, which is why the transport timeout goes to zero.
func probeClient(cfg providers.RoleConfig) (*openai.Client, error) {
	client, err := openai.NewFromRole(cfg)
	if err != nil {
		return nil, err
	}
	transport := http.Client{}
	if client.HTTP != nil {
		transport = *client.HTTP
	}
	transport.Timeout = 0
	client.HTTP = &transport
	return client, nil
}
