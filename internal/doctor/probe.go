package doctor

import (
	"context"
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
