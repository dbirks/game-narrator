package doctor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
)

// scriptedASRProbe is the synthetic-stub ASRProbe (AGENTS.md rule 3): it
// returns whatever the test queued and records the WAV size, so the check's
// OK/fail/empty decisions are testable without a gateway.
type scriptedASRProbe struct {
	res    ASRResult
	err    error
	gotWAV int
}

func (p *scriptedASRProbe) Transcribe(_ context.Context, _ providers.RoleConfig, wav []byte) (ASRResult, error) {
	p.gotWAV = len(wav)
	if p.err != nil {
		return ASRResult{}, p.err
	}
	return p.res, nil
}

func asrRoleHost(t *testing.T) *fakeHost {
	t.Helper()
	h := newFakeHost(t)
	h.withEnv("GAME_NARRATOR_GATEWAY_URL", "http://gateway.invalid/v1")
	modelEnv, _, _, _ := providers.RoleEnvVars(providers.RoleSpeechASR)
	h.withEnv(modelEnv, "qwen3-asr-1-7b")
	return h
}

func TestASRRoleOKOnATranscribedProbe(t *testing.T) {
	h := asrRoleHost(t)
	h.opts.ASRProbe = &scriptedASRProbe{res: ASRResult{
		Text: "probe tone", Model: "qwen3-asr-1-7b",
		RequestID: "01JPROBE", Latency: 800 * time.Millisecond,
	}}
	rep := h.run()
	c := h.check(rep, "providers.speech_asr")
	mustStatus(t, c, StatusOK)
	if !strings.Contains(c.Summary, "qwen3-asr-1-7b") {
		t.Fatalf("summary = %q, want the model id", c.Summary)
	}
	if !strings.Contains(c.Detail, "01JPROBE") {
		t.Fatalf("detail = %q, want the correlation id", c.Detail)
	}
}

func TestASRRoleOKWhenTheToneIsNotTranscribed(t *testing.T) {
	h := asrRoleHost(t)
	// The deployed endpoint (qwen3-asr-1-7b, measured 2026-10-18) transcribes
	// the probe tone to an empty transcript: no speech in a tone. The route,
	// protocol and credential still verified — that is the wire-level OK,
	// and the row says so rather than hiding it.
	h.opts.ASRProbe = &scriptedASRProbe{res: ASRResult{Text: "", Model: "qwen3-asr-1-7b", RequestID: "01JPROBE", Latency: time.Millisecond}}
	rep := h.run()
	c := h.check(rep, "providers.speech_asr")
	mustStatus(t, c, StatusOK)
	if !strings.Contains(c.Detail, "empty transcript") || !strings.Contains(c.Detail, "expected for non-speech") {
		t.Fatalf("detail = %q, want the honest empty-transcript note", c.Detail)
	}
	if !strings.Contains(c.Detail, "01JPROBE") {
		t.Fatalf("detail = %q, want the correlation id", c.Detail)
	}
}

func TestASRRoleNotConfigured(t *testing.T) {
	h := newFakeHost(t)
	modelEnv, _, _, _ := providers.RoleEnvVars(providers.RoleSpeechASR)
	h.withEnv("GAME_NARRATOR_GATEWAY_URL", "http://gateway.invalid/v1")
	h.opts.ASRProbe = &scriptedASRProbe{}
	rep := h.run()
	c := h.check(rep, "providers.speech_asr")
	mustStatus(t, c, StatusNotConfigured)
	if !strings.Contains(c.Remedy, modelEnv) {
		t.Fatalf("remedy = %q, want it to name %s", c.Remedy, modelEnv)
	}
}
