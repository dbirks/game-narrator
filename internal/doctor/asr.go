package doctor

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/asr"
)

// ASRResult is one transcription the doctor probe got back: the recognised
// text (evidence the endpoint answered at all), the correlation id, and the
// round trip.
type ASRResult struct {
	Text      string
	Model     string
	RequestID string
	Latency   time.Duration
}

// ASRProbe is the slice of the ASR client the doctor needs (the same
// seam discipline as ProviderProbe / VoiceProbe: the production adapter is
// injected as asrProbe, tests inject a scripted fake).
type ASRProbe interface {
	// Transcribe sends one probe WAV through the endpoint the role names.
	Transcribe(ctx context.Context, cfg providers.RoleConfig, wav []byte) (ASRResult, error)
}

// asrProbe is the real ASRProbe: it builds the same client the runtime uses
// (NewFromRole) and sends one real transcription of the probe tone.
type asrProbe struct{}

func (asrProbe) Transcribe(ctx context.Context, cfg providers.RoleConfig, wav []byte) (ASRResult, error) {
	client, err := asr.NewFromRole(cfg)
	if err != nil {
		return ASRResult{}, err
	}
	probeTransport(client)
	res, err := client.Transcribe(ctx, asr.TranscribeRequest{WAV: wav})
	if err != nil {
		return ASRResult{}, err
	}
	return ASRResult{
		Text:      res.Text,
		Model:     res.Model,
		RequestID: res.RequestID,
		Latency:   res.Latency,
	}, nil
}

// probeTransport zeroes the client's fixed timeout so the probe's own context
// budget (roleProbeBudget) is the single deadline — the same contract the TTS
// probe honours: the probe must fail on the budget, not on a hidden
// per-request timeout racing it.
func probeTransport(c *asr.Client) {
	transport := http.Client{}
	if c.HTTP != nil {
		transport = *c.HTTP
	}
	transport.Timeout = 0
	c.HTTP = &transport
}

// checkASRRole runs the real transcription probe for the speech_asr role:
// one request with the probe tone. OK is a WIRE-level verdict — the request
// reached the model named by the role, answered with the transcription
// schema, and the client decoded it — because the probe is a non-speech
// tone and the deployed endpoint (qwen3-asr-1-7b, measured 2026-10-18)
// transcribes it to an empty transcript: an empty `text` on a 200 is the
// endpoint saying "no speech here", not a route failure. A gateway that
// 200s with a non-transcription body still fails (decode_error), and every
// transport/auth/timeout/rate-limit failure fails through the client's
// taxonomy. The content of the transcript is reported either way (the row
// stays honest about what the probe heard — AGENTS.md rule 3); the positive
// proof that the endpoint transcribes speech is the live test
// (TestLiveTranscribeKnownLine, make test-models), not the probe.
func (r *runner) checkASRRole(ctx context.Context, role providers.Role, cfg providers.RoleConfig) Check {
	probe := r.opts.ASRProbe
	if probe == nil {
		probe = asrProbe{}
	}
	res, err := probe.Transcribe(ctx, cfg, asr.ProbeWAV())
	if err != nil {
		return roleFailure(role, cfg, "transcription probe", err)
	}
	summary := fmt.Sprintf("model %s answered the transcription probe in %s",
		cfg.Model, res.Latency.Round(time.Millisecond))
	if res.Text == "" {
		return Check{
			Status:  StatusOK,
			Summary: summary,
			Detail: fmt.Sprintf("request_id %s; the probe tone transcribed to an empty transcript — expected for non-speech audio; the route, protocol and credential are verified",
				res.RequestID),
		}
	}
	return Check{
		Status:  StatusOK,
		Summary: summary,
		Detail:  fmt.Sprintf("request_id %s; the probe tone transcribed to a non-empty transcript (redacted from the row)", res.RequestID),
	}
}
