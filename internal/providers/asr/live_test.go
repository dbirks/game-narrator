package asr

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/tts"
	"github.com/dbirks/game-narrator/internal/speech"
)

// LIVE contract test (docs/spec/12 "Contract" row, FR-10, ADR 0061): the only
// proof that the /audio/transcriptions wire matches the deployed qwen3-asr
// endpoint and that a TTS->ASR round trip is transcribable. It runs only when
// the environment names the real endpoints and SKIPs "NOT RUN" otherwise, so
// `go test ./...` stays green offline (the gate reports it NOT RUN, exit 2).
//
// Configuration (never printed, never logged — AGENTS.md rule 5):
//
//	GAME_NARRATOR_GATEWAY_URL              shared gateway for the speech routes
//	GAME_NARRATOR_GATEWAY_TOKEN_ENV        names the variable holding the token
//	GAME_NARRATOR_ROLE_SPEECH_ASR_MODEL    default qwen3-asr-1-7b
//	GAME_NARRATOR_ROLE_VOICE_CLONE_MODEL   default qwen3-tts-base (the TTS half)
//
// The synthesised WAV and the evidence JSON are written to t.TempDir() only.
// Log lines carry model ids, timings and metrics — never a URL or a token.

const (
	liveASRModel   = "qwen3-asr-1-7b"
	liveTTSModel   = "qwen3-tts-base"
	liveASRTimeout = 120 * time.Second
)

// liveASRLine is the known line the round trip speaks: short, single clause,
// no proper nouns — the shape the QA bound is tuned on (ADR 0061).
const liveASRLine = "The road ahead was long and the light held."

func liveASRCfg(t *testing.T, role providers.Role, modelDefault string) (providers.RoleConfig, bool) {
	t.Helper()
	roles, err := providers.LoadRoles(os.LookupEnv)
	if err != nil {
		t.Fatalf("LoadRoles: %v", err)
	}
	c := roles[role]
	modelEnv, _, _, _ := providers.RoleEnvVars(role)
	if m := strings.TrimSpace(os.Getenv(modelEnv)); m != "" {
		c.Model = m
	} else {
		c.Model = modelDefault
	}
	c.Timeout = liveASRTimeout
	if strings.TrimSpace(c.BaseURL) == "" || strings.TrimSpace(c.Token()) == "" {
		return c, false
	}
	return c, true
}

// TestLiveTranscribeKnownLine proves the ASR endpoint end to end: one TTS
// Base synthesis of a known line on the "default" voice, one transcription of
// those bytes, and the round trip's WER/CER scored the way the QA loop scores
// it (normalized text, the deployed bound). The evidence — line, transcript,
// wer, cer, within_bound, the model ids and the request_id correlation — is
// written as asr-live.json next to the WAV for the evidence bundle.
func TestLiveTranscribeKnownLine(t *testing.T) {
	asrCfg, ok := liveASRCfg(t, providers.RoleSpeechASR, liveASRModel)
	asrModelEnv, _, _, _ := providers.RoleEnvVars(providers.RoleSpeechASR)
	if !ok {
		t.Skip("NOT RUN: set GAME_NARRATOR_GATEWAY_URL and the token env named by " +
			providers.EnvGatewayTokenEnv + " (default " + providers.EnvGatewayToken +
			") and " + asrModelEnv + " to run the live ASR tests")
	}
	ttsCfg, ok := liveASRCfg(t, providers.RoleVoiceClone, liveTTSModel)
	if !ok {
		t.Skip("NOT RUN: the TTS half needs the same gateway configuration; " +
			"the ASR endpoint alone cannot be proven without a known line to read back")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	ttsClient, err := tts.NewFromRole(ttsCfg)
	if err != nil {
		t.Fatalf("tts.NewFromRole(%s): %v", ttsCfg.Model, err)
	}
	// The voice is tried the way the deployment actually resolves: the direct
	// Base service's listed voices first (a listed voice always resolves to
	// an embedding), the built-in "default" as the last resort (a Base
	// checkpoint may not resolve a built-in name to an embedding).
	var candidates []string
	if direct := strings.TrimSpace(os.Getenv("GAME_NARRATOR_TTS_BASE_DIRECT_URL")); direct != "" {
		reg, rerr := tts.NewRegistry(direct, ttsCfg.Token())
		if rerr != nil {
			t.Fatalf("tts.NewRegistry: %v", rerr)
		}
		if listed, lerr := reg.List(ctx); lerr == nil {
			for _, u := range listed.Uploaded {
				candidates = append(candidates, u.Name)
			}
			for _, n := range listed.Voices {
				candidates = append(candidates, n)
			}
		} else {
			t.Logf("voice listing failed, continuing with the built-in: %v", lerr)
		}
	}
	candidates = append(candidates, "default")

	var (
		audio   tts.Audio
		lastErr error
	)
	for _, voice := range candidates {
		a, gerr := ttsClient.Generate(ctx, tts.GenerateRequest{Text: liveASRLine, Voice: voice})
		if gerr != nil {
			lastErr = gerr
			t.Logf("live TTS with voice=%s failed: %v", voice, gerr)
			continue
		}
		audio = a
		break
	}
	if audio.WAV == nil {
		t.Fatalf("no voice could synthesise the known line; last error: %v", lastErr)
	}
	wavPath := filepath.Join(t.TempDir(), "live-asr-line.wav")
	if err := os.WriteFile(wavPath, audio.WAV, 0o600); err != nil {
		t.Fatalf("write wav: %v", err)
	}
	t.Logf("live TTS: model=%s duration=%s latency=%s sample_rate=%d request_id=%s",
		audio.Model, audio.Duration.Round(time.Millisecond), audio.Latency.Round(time.Millisecond),
		audio.SampleRate, audio.RequestID)

	asrClient, err := NewFromRole(asrCfg)
	if err != nil {
		t.Fatalf("asr.NewFromRole(%s): %v", asrCfg.Model, err)
	}
	res, err := asrClient.Transcribe(ctx, TranscribeRequest{WAV: audio.WAV, Language: "en"})
	if err != nil {
		t.Fatalf("live Transcribe: %v", err)
	}
	rec := speech.Evaluate(liveASRLine, res.Text)

	evidence := struct {
		Line         string  `json:"line"`
		Transcript   string  `json:"transcript"`
		WER          float64 `json:"wer"`
		CER          float64 `json:"cer"`
		WithinBound  bool    `json:"within_bound"`
		WERBound     float64 `json:"wer_bound"`
		ASRModel     string  `json:"asr_model"`
		TTSModel     string  `json:"tts_model"`
		RequestID    string  `json:"request_id"`
		RequestIDSrc string  `json:"request_id_source"`
		ASRLatencyMS int64   `json:"asr_latency_ms"`
		Language     string  `json:"language,omitempty"`
	}{
		Line:         liveASRLine,
		Transcript:   res.Text,
		WER:          rec.WER,
		CER:          rec.CER,
		WithinBound:  rec.Pass,
		WERBound:     speech.WERBound(),
		ASRModel:     asrCfg.Model,
		TTSModel:     audio.Model,
		RequestID:    res.RequestID,
		RequestIDSrc: res.RequestIDSource,
		ASRLatencyMS: res.Latency.Milliseconds(),
		Language:     res.Language,
	}
	if err := os.WriteFile(filepath.Join(t.TempDir(), "asr-live.json"), mustMarshalEvidence(t, evidence), 0o600); err != nil {
		t.Fatalf("write evidence: %v", err)
	}
	// The gate's evidence collector greps for these tokens in lowercase.
	t.Logf("live ASR: model=%s language=%s latency=%s request_id=%s",
		res.Model, res.Language, res.Latency.Round(time.Millisecond), res.RequestID)
	t.Logf("live ASR verdict: transcript=%q wer=%.3f cer=%.3f within_bound=%v bound=%.2f request_id=%s",
		res.Text, rec.WER, rec.CER, rec.Pass, speech.WERBound(), res.RequestID)

	if strings.TrimSpace(res.Text) == "" {
		t.Fatalf("the endpoint answered with an empty transcript; the route is not the ASR model")
	}
}

func mustMarshalEvidence(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal evidence: %v", err)
	}
	return b
}
