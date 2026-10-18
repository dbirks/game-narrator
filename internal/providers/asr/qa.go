package asr

import (
	"context"

	"github.com/dbirks/game-narrator/internal/speech"
)

// The QA seam the speech layer owns (docs/spec/07 "ASR loop"): the typed
// client is the only production implementer; tests inject a scripted fake.
var _ speech.ASRClient = (*Client)(nil)

// TranscribeText is the speech.ASRClient seam over Transcribe: one
// transcription of wav (RIFF/WAVE) with the given language hint, returning
// the transcript record the QA loop consumes. The error is the same
// classified *providers.Error Transcribe returns, so the QA engine's bounded
// retry can test its Retryable flag.
func (c *Client) TranscribeText(ctx context.Context, wav []byte, language string) (speech.Transcription, error) {
	res, err := c.Transcribe(ctx, TranscribeRequest{WAV: wav, Language: language})
	if err != nil {
		return speech.Transcription{}, err
	}
	return speech.Transcription{
		Text:            res.Text,
		Model:           res.Model,
		RequestID:       res.RequestID,
		RequestIDSource: res.RequestIDSource,
		Latency:         res.Latency,
	}, nil
}
