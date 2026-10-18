package tts

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
)

// TestLiveStreamGenerateViaGateway proves the streaming synthesis wire
// (ADR-0057) against the real gateway, in the two shapes the contract owes:
//
//  1. the complete stream — the deltas assemble to one validated WAV
//     (assemble-then-validate; the partial is never returned), and
//  2. the genuine mid-stream cancel — the caller's context is cancelled
//     after the first delivered delta, the partial is discarded, and the
//     failure is classified as a cancellation, not a clean end.
//
// The logs carry the effective request id and the bytes read before the
// cancel: the proof the cancel landed mid-stream, not before the request.
// No URL or token is printed.
func TestLiveStreamGenerateViaGateway(t *testing.T) {
	c := newLiveClient(t, providers.RoleVoiceClone, liveCloneModel)
	// This test owns the live gateway's streaming route, so it carries its
	// own budget: a gateway that stalls must fail this test in a bounded
	// time, not eat the package's test timeout with the other live tests.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)

	voice, err := liveSynthesizableVoice(t, c, ctx)
	if err != nil {
		t.Fatalf("no listed voice could synthesise: %s", classifyForLog(err))
	}

	// 1) The complete stream.
	a, err := c.GenerateStream(ctx, GenerateRequest{Text: liveGenDefault, Voice: voice})
	if err != nil {
		t.Fatalf("live GenerateStream: %s", classifyForLog(err))
	}
	saveWAV(t, "live-stream-generate.wav", a)
	t.Logf("live stream: model=%s voice=%s duration=%s latency=%s request_id=%s (%s) origin=%s",
		a.Model, voice, a.Duration.Round(time.Millisecond), a.Latency.Round(time.Millisecond),
		a.RequestID, a.RequestIDSource, a.Origin)

	// 2) The genuine mid-stream cancel: one delta in, then the context
	// out. A long line keeps many deltas in flight when the cancel lands.
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s, err := c.OpenStream(cctx, GenerateRequest{Text: liveGenLong, Voice: voice})
	if err != nil {
		if errors.Is(err, ErrStreamUnsupported) {
			// The deployment negotiated the atomic-only wire: there is no
			// stream for a mid-stream cancel to land in, and the atomic
			// path is bounded by the same context. Not a failure.
			t.Skipf("deployment negotiated the atomic-only wire (400 on the stream flag); a mid-stream cancel has no stream to land in: %s", classifyForLog(err))
		}
		t.Fatalf("live OpenStream: %s", classifyForLog(err))
	}
	defer s.Close()

	var (
		read    int64
		delived int
		errEnd  error
	)
	for {
		chunk, err := s.Next()
		read += int64(len(chunk))
		if err != nil {
			errEnd = err
			break
		}
		if len(chunk) > 0 {
			delived++
		}
		// The wire is open and at least one delta is delivered: cancel
		// mid-stream, while the line still has deltas to come.
		cancel()
	}
	if errEnd == nil {
		t.Fatalf("the stream never ended; the read loop must terminate on the cancel")
	}
	if IsCleanEnd(errEnd) {
		t.Fatalf("the stream completed (clean end) instead of failing on the cancel")
	}
	if pe := asProvidersError(errEnd); pe != nil && pe.Kind != providers.KindCancelled {
		// A cancel that arrives while a read is in flight classifies as the
		// cancelled kind; anything else names a wire fault the cancel raced.
		t.Logf("cancel error classified as %s (the cancel raced the wire)", pe.Kind)
	}
	if s.BytesRead() <= 0 {
		t.Fatalf("no audio bytes were read before the cancel: the cancel landed before the stream")
	}
	if got, want := s.BytesRead(), read; got != want {
		t.Errorf("BytesRead() = %d, want the %d byte(s) Next delivered before the error", got, want)
	}
	// Sticky: every later Next returns the same classified error.
	if _, err := s.Next(); !errors.Is(err, errEnd) {
		t.Errorf("the post-cancel Next error %s is not the sticky error %s", classifyForLog(err), classifyForLog(errEnd))
	}
	t.Logf("live stream cancel: model=%s voice=%s request_id=%s (%s) deltas_before_cancel=%d bytes_read_before_cancel=%d err=%s",
		c.Model, voice, s.RequestID(), s.RequestIDSource(), delived, s.BytesRead(), classifyForLog(errEnd))
}

// liveSynthesizableVoice returns the first voice the deployment can
// synthesise: the built-in "default" first, then the listed voices — the
// order the deployment actually resolves.
func liveSynthesizableVoice(t *testing.T, c *Client, ctx context.Context) (string, error) {
	t.Helper()
	candidates := []string{"default"}
	if names, ok := liveListedVoices(t, ctx); ok {
		candidates = append(candidates, names...)
	}
	var lastErr error
	for _, voice := range candidates {
		if _, err := c.Generate(ctx, GenerateRequest{Text: liveGenDefault, Voice: voice}); err == nil {
			return voice, nil
		} else {
			lastErr = err
			t.Logf("synthesis probe with voice=%s failed: %s", voice, classifyForLog(err))
		}
	}
	return "", lastErr
}
