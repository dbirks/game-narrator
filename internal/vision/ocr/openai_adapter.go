package ocr

import (
	"context"
	"errors"

	"github.com/dbirks/game-narrator/internal/providers/openai"
)

// OpenAIClient is the production adapter from the ocr.VLMClient seam to the
// OpenAI-compatible gateway client (ADR-0040 §1: the runtime owns the
// role-configured clients; this is the bridge the runtime's wiring hands to
// NewVLMEngine). It forwards the VLMRequest verbatim as one user message —
// the terse fixed instruction plus the prepared crop PNG — because the
// role's configuration already carries the rest: the vision_ocr_fallback
// client is built from the role (providers.NewFromRole), so its transport
// timeout is the role's (10 s floor) and its reasoning-effort default is the
// role's ("none"); leaving ReasoningEffort empty here applies that default
// instead of re-declaring it. MaxTokens passes through from the engine's
// bound (256 by default); the adapter must not raise it.
//
// Privacy (docs/spec/14) is enforced by the request shape, not by this
// adapter: the only bytes that leave the appliance through this role are
// req.PNG — the downscaled crop VLMEngine prepared. The returned error never
// carries the image bytes.
type OpenAIClient struct {
	// Client is the role-configured gateway client. Nil is a construction
	// error the wiring must catch (a VLM fallback without a client is a
	// fallback that can only fail): Chat then refuses the call loudly
	// instead of silently succeeding.
	Client *openai.Client
}

// Chat implements VLMClient. The answer is plain text (JSON mode is not
// available on the configured gateway, ADR-0040) and is returned verbatim:
// parseVLMAnswer owns the "only the text" / "NONE" contract.
func (a OpenAIClient) Chat(ctx context.Context, req VLMRequest) (string, error) {
	if a.Client == nil {
		return "", errors.New("ocr: OpenAIClient has no client: the vision_ocr_fallback role is not wired")
	}
	resp, err := a.Client.Chat(ctx, openai.ChatRequest{
		Messages: []openai.Message{{
			Role:   "user",
			Text:   req.Instruction,
			Images: [][]byte{req.PNG},
		}},
		MaxTokens: req.MaxTokens,
	})
	if err != nil {
		return "", err
	}
	return resp.Text, nil
}

// Pin the seam satisfaction: the wiring constructs NewVLMEngine(
// OpenAIClient{Client: roleClient}) for the vision_ocr_fallback role, and a
// drifted Chat signature must fail here, at compile time.
var _ VLMClient = OpenAIClient{}
