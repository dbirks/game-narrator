// The second game's live TTS leg (gap audit G12, issue #33): the repeatable
// replacement for the one-shot evidence runner (internal/providers/tts/
// secondgame_live_test.go at the 2026-10-19 acceptance audit) that was
// deleted after its run — its recorded evidence is
// artifacts/second-game-live-tts/ (real Design + Generate over the shared
// gateway, a gn_test_* voice registered on the direct Base registry and
// removed after, 4.4 s WAV with SHA-256).
//
// What this test proves, the way the one-shot run did:
//
//  1. The second bench set's (bench-synthetic-002, "Black Water") committed
//     holdout line is read back from the committed fixture — the test
//     synthesises a line the set grades, never an ad-hoc one. The pinned
//     sample is bn0006 / bw-02, the line the one-shot run voiced.
//  2. A narrator voice is designed for the set through the real VoiceDesign
//     checkpoint, registered under the gn_test_ namespace with truthful
//     synthetic consent, spoken, and removed (t.Cleanup is the removal
//     backstop — a failed assertion never leaves a test voice behind).
//  3. The holdout line comes back as a valid WAV with real signal: the
//     bytes parse (RIFF/PCM, the invariants ValidateAudio enforces), the
//     duration is inside a band the line's length admits, and the measured
//     peak/RMS say speech, not silence, not a clipped tone.
//
// Gate and environment (never printed, never logged):
//
//	GN_GATE=test-models (the make test-models marker, ADR-0065) plus the
//	GAME_NARRATOR_GATEWAY_URL and the token env named by
//	GAME_NARRATOR_GATEWAY_TOKEN_ENV (the Design and Generate calls), plus
//	GAME_NARRATOR_TTS_BASE_DIRECT_URL (the Register/List/Remove calls — the
//	registry route is not exposed through the shared gateway). Without any
//	of them the test is NOT RUN, exactly like the other live TTS tests: the
//	unit gate and a host without the gateway stay green offline.
//
// Generated audio is written to t.TempDir() only. Log lines carry model
// ids, durations, latencies, sample rates and (sanitised) voice names —
// nothing in this file may print a URL or a token.
package tts

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
)

const (
	// secondSetDir is the committed second bench set (ADR-0079): the profile
	// and the ground truth the test's holdout line is pinned against.
	secondSetDir = "../../../testdata/bench/bench-synthetic-002"
	// secondHoldoutSample is the committed holdout sample the one-shot
	// evidence run synthesised (artifacts/second-game-live-tts/manifest.json):
	// pinning the same sample keeps this leg's evidence comparable to the
	// recorded one.
	secondHoldoutSample = "bn0006"
	// secondHoldoutMessage is the sample's corpus line.
	secondHoldoutMessage = "bw-02"
	// secondNarratorBrief is the voice brief the one-shot run designed with:
	// a narrator, not a character voice — the leg under test is the TTS
	// route, not a per-character audition.
	secondNarratorBrief = "A calm narrator voice with a steady, unhurried cadence."

	// The signal band the holdout line's synthesis must land in. The line is
	// nine words; the one-shot run measured 4.4 s at 24 kHz (peak 0.828,
	// RMS 0.108). The bounds below are the "real speech" fence, not a
	// stopwatch: wide enough for checkpoint drift, narrow enough that
	// silence, a tone or a truncated payload cannot pass.
	minLineDuration = 1 * time.Second
	maxLineDuration = 30 * time.Second
	minLinePeak     = 0.05 // real speech is orders of magnitude above silence
	maxLineRMS      = 0.5  // sustained half-full-scale is a tone, not a line
)

// secondSetFixture is the committed second-set pair reduced to the fields
// the test pins: the holdout line and its speaker.
type secondSetFixture struct {
	line    string
	speaker string
}

// secondTruthSample is one truth.json row, the fields the pin checks.
type secondTruthSample struct {
	ID             string `json:"id"`
	SourceText     string `json:"source_text"`
	MessageID      string `json:"message_id"`
	Speaker        string `json:"speaker"`
	Split          string `json:"split"`
	IntendedSpeech bool   `json:"intended_speech"`
}

// secondProfileMessage is one profile.json message row.
type secondProfileMessage struct {
	ID      string `json:"id"`
	Text    string `json:"text"`
	Speaker string `json:"speaker"`
}

// loadSecondSetHoldout reads the committed bench-synthetic-002 truth and
// profile and returns the pinned holdout line, verifying the pin against
// both documents. A drift in the committed set fails loudly here instead of
// silently synthesising a line the set no longer grades.
func loadSecondSetHoldout(t *testing.T) secondSetFixture {
	t.Helper()
	truthRaw, err := os.ReadFile(filepath.Join(secondSetDir, "truth.json"))
	if err != nil {
		t.Fatalf("read committed second set truth: %v (the set must be committed under testdata/bench/)", err)
	}
	var truth struct {
		Set     string              `json:"set"`
		Samples []secondTruthSample `json:"samples"`
	}
	if err := json.Unmarshal(truthRaw, &truth); err != nil {
		t.Fatalf("parse second set truth: %v", err)
	}
	if truth.Set != "bench-synthetic-002" {
		t.Fatalf("second set truth names %q, want bench-synthetic-002", truth.Set)
	}
	var sample *secondTruthSample
	for i := range truth.Samples {
		if truth.Samples[i].ID == secondHoldoutSample {
			s := truth.Samples[i]
			sample = &s
			break
		}
	}
	if sample == nil {
		t.Fatalf("committed truth carries no sample %s (the pin moved; re-pin deliberately)", secondHoldoutSample)
	}
	if sample.Split != "holdout" || !sample.IntendedSpeech {
		t.Fatalf("sample %s is split=%q intended_speech=%v: the live TTS leg synthesises a committed intended-speech holdout line",
			secondHoldoutSample, sample.Split, sample.IntendedSpeech)
	}
	if sample.MessageID != secondHoldoutMessage {
		t.Fatalf("sample %s names message %q, want %s (the pin moved; re-pin deliberately)",
			secondHoldoutSample, sample.MessageID, secondHoldoutMessage)
	}

	profileRaw, err := os.ReadFile(filepath.Join(secondSetDir, "profile.json"))
	if err != nil {
		t.Fatalf("read committed second set profile: %v", err)
	}
	var profile struct {
		ID       string                 `json:"id"`
		Origin   string                 `json:"origin"`
		Messages []secondProfileMessage `json:"messages"`
	}
	if err := json.Unmarshal(profileRaw, &profile); err != nil {
		t.Fatalf("parse second set profile: %v", err)
	}
	var line *secondProfileMessage
	for i := range profile.Messages {
		if profile.Messages[i].ID == secondHoldoutMessage {
			m := profile.Messages[i]
			line = &m
			break
		}
	}
	if line == nil {
		t.Fatalf("committed profile carries no message %s (the pin moved; re-pin deliberately)", secondHoldoutMessage)
	}
	if line.Text != sample.SourceText {
		t.Fatalf("truth sample %s text %q and profile message %s text %q disagree: the committed set drifted",
			secondHoldoutSample, sample.SourceText, secondHoldoutMessage, line.Text)
	}
	if strings.TrimSpace(sample.SourceText) == "" {
		t.Fatalf("holdout line is empty: nothing to synthesise")
	}
	return secondSetFixture{line: line.Text, speaker: sample.Speaker}
}

// wavPeakRMS measures the peak and RMS of a WAV's samples, each as a
// fraction of full scale (16-bit s maps to s/32768). The data payload is
// located with the same form-agnostic walk wav.go uses, and every bit depth
// the checkpoints emit (8/16/24/32-bit little-endian PCM) is supported —
// all channels are averaged into the same statistics.
func wavPeakRMS(t *testing.T, wav []byte, bits int) (peak, rms float64) {
	t.Helper()
	data, ok := findChunk(wav, "data")
	if !ok || len(data) < bits/8 {
		t.Fatalf("no data payload to measure in the %d-byte WAV", len(wav))
	}
	n := len(data) / (bits / 8)
	if n == 0 {
		t.Fatalf("the data chunk holds no complete sample")
	}
	var sumSq float64
	for i := 0; i < n; i++ {
		off := i * bits / 8
		var s float64
		switch bits {
		case 8:
			s = (float64(data[off]) - 128) / 128
		case 16:
			s = float64(int16(binary.LittleEndian.Uint16(data[off:]))) / 32768
		case 24:
			v := int32(data[off]) | int32(data[off+1])<<8 | int32(data[off+2])<<16
			if v&0x800000 != 0 {
				v += -0x1000000 // sign-extend
			}
			s = float64(v) / 8388608
		case 32:
			s = float64(int32(binary.LittleEndian.Uint32(data[off:]))) / 2147483648
		default:
			t.Fatalf("unsupported bit depth %d", bits)
		}
		if a := math.Abs(s); a > peak {
			peak = a
		}
		sumSq += s * s
	}
	return peak, math.Sqrt(sumSq / float64(n))
}

// TestLiveSecondGameHoldoutLine is the repeatable G12 TTS leg: the second
// set's committed holdout line, spoken by a narrator voice designed and
// registered for the run (gn_test_ namespace, removed in cleanup), through
// the real gateway and the real Base registry.
func TestLiveSecondGameHoldoutLine(t *testing.T) {
	fixture := loadSecondSetHoldout(t)
	t.Logf("pinned committed holdout line (sample %s, message %s, speaker %s): %q",
		secondHoldoutSample, secondHoldoutMessage, fixture.speaker, fixture.line)

	design := newLiveClient(t, providers.RoleVoiceDesign, liveDesignModel)
	reg, ok := liveRegistry(t)
	if !ok {
		t.Skip("NOT RUN: set GAME_NARRATOR_TTS_BASE_DIRECT_URL to the direct Base service (orchestrator port-forward) to run the second-game live TTS leg")
	}
	ctx := liveCtx(t)

	// 1. Design the narrator voice. The design text is the holdout line
	// itself: the registered voice carries the line it will be graded on,
	// and the ref text the registration declares is the text it was cut
	// from (the consent the registry requires, truthfully).
	da, err := design.Design(ctx, DesignRequest{Text: fixture.line, Instructions: secondNarratorBrief, Language: "English"})
	if err != nil {
		t.Fatalf("live Design of the second-game narrator: %v", classifyForLog(err))
	}
	saveWAV(t, "second-game-narrator-design.wav", da)
	designInfo, err := ValidateAudio(da.WAV)
	if err != nil {
		t.Fatalf("designed narrator audio failed validation: %v", classifyForLog(err))
	}
	t.Logf("narrator designed: model=%s duration=%s latency=%s sample_rate=%d origin=%s",
		da.Model, da.Duration.Round(time.Millisecond), da.Latency.Round(time.Millisecond), designInfo.SampleRate, da.Origin)

	// 2. Register it under the gn_test_ namespace; cleanup removes it even
	// if an assertion below fails (the registry cycle's same backstop).
	name, err := testName()
	if err != nil {
		t.Fatalf("test name: %v", err)
	}
	removed := false
	t.Cleanup(func() {
		if removed {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := reg.Remove(cleanupCtx, name); err != nil && !errors.Is(err, ErrInvalidName) {
			t.Errorf("cleanup Remove(%s): %v", name, classifyForLog(err))
		}
	})
	if _, err := reg.Register(ctx, Voice{
		Name:     name,
		RefAudio: da.WAV,
		RefText:  fixture.line,
		Consent:  "original_synthetic_design (game-narrator automated test, second bench set holdout line)",
	}); err != nil {
		t.Fatalf("live Register(%s): %v", name, classifyForLog(err))
	}
	listed, err := reg.List(ctx)
	if err != nil {
		t.Fatalf("live List: %v", classifyForLog(err))
	}
	found := false
	for _, n := range listed.Voices {
		if n == name {
			found = true
		}
	}
	if !found {
		t.Fatalf("List does not show %s (voices: %v)", name, sanitizeNames(listed.Voices))
	}

	// 3. Synthesise the holdout line with the registered voice.
	generate := newLiveClient(t, providers.RoleVoiceClone, liveCloneModel)
	ga, err := generate.Generate(ctx, GenerateRequest{Text: fixture.line, Voice: name})
	if err != nil {
		t.Fatalf("live Generate(%s): %v", name, classifyForLog(err))
	}
	saveWAV(t, "second-game-holdout-bw-02.wav", ga)
	t.Logf("holdout line spoken: model=%s voice=%s duration=%s latency=%s origin=%s",
		ga.Model, name, ga.Duration.Round(time.Millisecond), ga.Latency.Round(time.Millisecond), ga.Origin)

	// 4. The WAV bytes: a valid RIFF/PCM stream (container, format, the
	// rate/duration/silence/clip invariants ValidateAudio enforces) with a
	// duration the line's length admits.
	info, err := ValidateAudio(ga.WAV)
	if err != nil {
		t.Fatalf("generated holdout WAV failed validation: %v", classifyForLog(err))
	}
	if len(ga.WAV) <= 1024 {
		t.Fatalf("generated holdout WAV is %d bytes: too small to hold a %s line", len(ga.WAV), info.Duration.Round(time.Millisecond))
	}
	if info.Duration < minLineDuration || info.Duration > maxLineDuration {
		t.Fatalf("holdout line duration %s outside [%s, %s]: the line is nine words, not a blip or a loop",
			info.Duration.Round(time.Millisecond), minLineDuration, maxLineDuration)
	}

	// 5. The signal bounds: real speech, not silence and not a tone.
	peak, rms := wavPeakRMS(t, ga.WAV, info.BitsPerSample)
	if peak < minLinePeak {
		t.Fatalf("holdout line peak %.4f below %.4f: the audio is silence, not speech", peak, minLinePeak)
	}
	if rms < minRMSPerFullScale || rms > maxLineRMS {
		t.Fatalf("holdout line RMS %.6f outside [%.6f, %v]: the audio is not speech", rms, minRMSPerFullScale, maxLineRMS)
	}
	t.Logf("signal: bytes=%d sample_rate=%dHz bits=%d duration=%s peak=%.4f rms=%.6f (one-shot reference: peak 0.828, rms 0.108)",
		len(ga.WAV), info.SampleRate, info.BitsPerSample, info.Duration.Round(time.Millisecond), peak, rms)

	// 6. Removal, and the registry no longer shows the voice (the cycle's
	// same terminal assertion).
	if err := reg.Remove(ctx, name); err != nil {
		t.Fatalf("live Remove(%s): %v", name, classifyForLog(err))
	}
	removed = true
	after, err := reg.List(ctx)
	if err != nil {
		t.Fatalf("live List after remove: %v", classifyForLog(err))
	}
	for _, n := range after.Voices {
		if n == name {
			t.Fatalf("List still shows %s after removal", name)
		}
	}
	t.Logf("second-game TTS leg complete: %s designed, registered, spoken the holdout line, removed", name)
}
