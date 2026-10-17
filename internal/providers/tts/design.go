package tts

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
)

// Design calls the VoiceDesign checkpoint and returns a validated reference
// WAV. task_type:"VoiceDesign" is always sent: the checkpoints do not
// interoperate and the default (CustomVoice) is rejected by both of them.
func (c *Client) Design(ctx context.Context, req DesignRequest) (Audio, error) {
	text := strings.TrimSpace(req.Text)
	if text == "" {
		return Audio{}, &providers.Error{Kind: providers.KindBadSchema, Msg: "tts: Design.Text is empty", Model: c.Model}
	}
	var (
		seedSent    bool
		seedDropped bool
	)
	payload := map[string]any{
		"model":           c.Model,
		"input":           text,
		"response_format": "wav",
		"task_type":       taskTypeVoiceDesign,
	}
	if strings.TrimSpace(req.Instructions) != "" {
		payload["instructions"] = req.Instructions
	}
	if strings.TrimSpace(req.Language) != "" {
		payload["language"] = req.Language
	}
	if req.Seed != nil {
		if c.SeedUnsupported() {
			// The deployment rejected the seed field on an earlier call; do
			// not pay for the 400 again — record the drop instead.
			seedDropped = true
		} else {
			payload["seed"] = *req.Seed
			seedSent = true
		}
	}
	body, latency, reqID, err := c.do(ctx, payload)
	if err != nil {
		// Seed is metadata (ADR-0005): a deployment that rejects the field
		// must not lose the call, so retry exactly once without it and
		// record the rejection instead of hiding it.
		if req.Seed != nil && isSeedRejection(err) {
			c.markSeedUnsupported()
			delete(payload, "seed")
			body, latency2, reqID2, err2 := c.do(ctx, payload)
			if err2 != nil {
				return Audio{}, err2
			}
			latency, reqID = latency2, reqID2
			return c.finishAudio(body, latency, reqID,
				fmt.Sprintf("voice_design model=%s task_type=%s seed_unsupported=true", c.Model, taskTypeVoiceDesign),
				nil, true)
		}
		return Audio{}, err
	}
	origin := fmt.Sprintf("voice_design model=%s task_type=%s", c.Model, taskTypeVoiceDesign)
	var sentSeed *int
	if seedSent {
		sent := *req.Seed
		sentSeed = &sent
	}
	if seedDropped {
		origin += " seed_unsupported=true"
	}
	return c.finishAudio(body, latency, reqID, origin, sentSeed, seedDropped)
}

// Generate calls the Base checkpoint to speak Text in Voice. The default
// request omits task_type; when the checkpoint's 400 text shows it rejects
// the server-default task, the call is retried once with task_type:"Base"
// and the deployment is recorded as Base-task-type for future calls.
func (c *Client) Generate(ctx context.Context, req GenerateRequest) (Audio, error) {
	text := strings.TrimSpace(req.Text)
	if text == "" {
		return Audio{}, &providers.Error{Kind: providers.KindBadSchema, Msg: "tts: Generate.Text is empty", Model: c.Model}
	}
	if strings.TrimSpace(req.Voice) == "" {
		return Audio{}, &providers.Error{Kind: providers.KindBadSchema, Msg: "tts: Generate.Voice is empty", Model: c.Model}
	}
	withTaskType := c.baseTaskTypeCached()
	payload := c.synthesisPayload(req, withTaskType)
	body, latency, reqID, err := c.do(ctx, payload)
	if err != nil {
		if !withTaskType && isBaseTaskTypeDemand(err) {
			c.markBaseTaskType()
			body, latency, reqID, err = c.do(ctx, c.synthesisPayload(req, true))
		}
		if err != nil {
			return Audio{}, c.classifyGenerateError(req, err)
		}
	}
	origin := fmt.Sprintf("voice_clone model=%s voice=%s", c.Model, req.Voice)
	if withTaskType || c.baseTaskTypeCached() {
		origin += " task_type=" + taskTypeBase
	}
	return c.finishAudio(body, latency, reqID, origin, nil, false)
}

// synthesisPayload builds the Base synthesis body. withTaskType forces
// task_type:"Base"; Speed and Language are only sent when set, so a server
// default is never shadowed by a zero value.
func (c *Client) synthesisPayload(req GenerateRequest, withTaskType bool) map[string]any {
	payload := map[string]any{
		"model":           c.Model,
		"input":           strings.TrimSpace(req.Text),
		"voice":           req.Voice,
		"response_format": "wav",
	}
	if withTaskType {
		payload["task_type"] = taskTypeBase
	}
	if strings.TrimSpace(req.Language) != "" {
		payload["language"] = req.Language
	}
	if req.Speed > 0 {
		payload["speed"] = req.Speed
	}
	return payload
}

// finishAudio validates the raw WAV and assembles the provenance record. A
// server that returned 200 with a malformed file fails as audio_invalid —
// that is the docs/spec/07 "malformed/noisy audio → quarantine" rule.
func (c *Client) finishAudio(body []byte, latency time.Duration, reqID, origin string, seed *int, seedUnsupported bool) (Audio, error) {
	info, err := ValidateAudio(body)
	if err != nil {
		return Audio{}, err
	}
	return Audio{
		WAV:             body,
		SampleRate:      info.SampleRate,
		Channels:        info.Channels,
		BitsPerSample:   info.BitsPerSample,
		Duration:        info.Duration,
		Model:           c.Model,
		Latency:         latency,
		RequestID:       reqID,
		Origin:          origin,
		Seed:            seed,
		SeedUnsupported: seedUnsupported,
	}, nil
}

// baseTaskTypeCached reads the negotiated capability.
func (c *Client) baseTaskTypeCached() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.baseTaskType
}

func (c *Client) markBaseTaskType() {
	c.mu.Lock()
	c.baseTaskType = true
	c.mu.Unlock()
}

func (c *Client) markSeedUnsupported() {
	c.mu.Lock()
	c.seedUnsupported = true
	c.mu.Unlock()
}

// SeedUnsupported reports whether this client has observed the deployment
// rejecting the seed field, so callers can stop paying for the retry.
func (c *Client) SeedUnsupported() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seedUnsupported
}

// classifyGenerateError re-maps synthesis errors whose text shows the voice
// itself is the problem (unknown on this instance, or a built-in name the
// Base checkpoint cannot resolve to an embedding): docs/spec/06 calls that
// voice_missing and the routing layer may reconcile or fail over.
func (c *Client) classifyGenerateError(req GenerateRequest, err error) error {
	pe := asProvidersError(err)
	if pe == nil || pe.Status != http.StatusBadRequest {
		return err
	}
	msg := strings.ToLower(pe.Msg)
	voiceWord := strings.Contains(msg, "voice")
	switch {
	case voiceWord && (strings.Contains(msg, "not found") || strings.Contains(msg, "no such") ||
		strings.Contains(msg, "unknown") || strings.Contains(msg, "does not exist") ||
		strings.Contains(msg, "unrecognized")):
		pe.Kind = providers.KindVoiceMissing
		pe.Retryable = providers.KindVoiceMissing.RetryableByDefault()
		return pe
	case strings.Contains(msg, "requires 'ref_audio'"):
		// The checkpoint has no stored embedding for this voice name on this
		// instance: the voice is missing here, whatever the name looks like.
		pe.Kind = providers.KindVoiceMissing
		pe.Retryable = providers.KindVoiceMissing.RetryableByDefault()
		return pe
	}
	return err
}
