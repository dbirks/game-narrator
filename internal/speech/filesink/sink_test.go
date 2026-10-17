package filesink_test

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/speech/filesink"
)

// filesink is the docs/spec/11 evidence sink: a WAV on disk is NOT proof of
// playback, so the file must contain EXACTLY the samples that would have
// been delivered — the delivered prefix, and a shorter prefix when the
// playback is stopped early.

// TestFileSinkNaturalEndDeliversAllSamples: an un-stopped utterance writes a
// file whose samples equal the source samples (complete delivery).
func TestFileSinkNaturalEndDeliversAllSamples(t *testing.T) {
	dir := t.TempDir()
	sink, err := filesink.New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer sink.Close()

	src := speech.MakeToneWAV(440, 0.15, 22050) // 150 ms tone, real-time paced
	h, err := sink.Play(context.Background(), speech.PlayRequest{
		UtteranceID: "u1", Generation: 3, WAV: src, Gain: 1.0,
	})
	if err != nil {
		t.Fatalf("Play: %v", err)
	}
	res := <-h.Done()
	if res.Err != nil || res.Interrupted {
		t.Fatalf("natural end: %+v", res)
	}

	p := h.(*filesink.Play).Path()
	got, err := readWAV(t, p)
	if err != nil {
		t.Fatalf("reading evidence file: %v", err)
	}
	want, err := speech.DecodeWAV(src)
	if err != nil {
		t.Fatalf("decoding source: %v", err)
	}
	if got.SampleRate != want.SampleRate || got.Channels != want.Channels || got.BitsPerSample != want.BitsPerSample {
		t.Fatalf("format mismatch: got %d/%d/%d want %d/%d/%d",
			got.SampleRate, got.Channels, got.BitsPerSample, want.SampleRate, want.Channels, want.BitsPerSample)
	}
	if !bytes.Equal(got.Data, want.Data) {
		t.Fatalf("natural end delivered %d bytes, source has %d: samples must match exactly", len(got.Data), len(want.Data))
	}
}

// TestFileSinkStopDeliversPrefix: stopping a 600 ms tone at ~150 ms writes a
// file that is (a) strictly shorter than the source and (b) exactly the
// source's leading samples — the evidence is the delivered prefix, no more
// (docs/spec/12 "stale audio after advance": the stop point is what the
// evidence must show).
func TestFileSinkStopDeliversPrefix(t *testing.T) {
	dir := t.TempDir()
	sink, err := filesink.New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer sink.Close()

	src := speech.MakeToneWAV(440, 0.6, 22050) // 600 ms
	srcPCM, err := speech.DecodeWAV(src)
	if err != nil {
		t.Fatalf("decoding source: %v", err)
	}

	h, err := sink.Play(context.Background(), speech.PlayRequest{
		UtteranceID: "u-stop", Generation: 1, WAV: src, Gain: 1.0,
	})
	if err != nil {
		t.Fatalf("Play: %v", err)
	}
	// ~150 ms into the 600 ms tone: well past the start, well before the end.
	time.Sleep(150 * time.Millisecond)
	stopAt := time.Now()
	h.Stop(0) // hard cut, no fade: the prefix must be byte-exact

	res := <-h.Done()
	if !res.Interrupted || res.Err != nil {
		t.Fatalf("stop: %+v", res)
	}
	stopLatency := res.Ended.Sub(stopAt)
	if stopLatency > 200*time.Millisecond {
		t.Fatalf("local stop latency %v exceeds the 200 ms target", stopLatency)
	}

	p := h.(*filesink.Play).Path()
	got, err := readWAV(t, p)
	if err != nil {
		t.Fatalf("reading evidence file: %v", err)
	}
	if len(got.Data) == 0 {
		t.Fatal("stopped playback wrote an empty file")
	}
	if len(got.Data) >= len(srcPCM.Data) {
		t.Fatalf("evidence file (%d bytes) is not shorter than the source (%d bytes): a stop must deliver a prefix", len(got.Data), len(srcPCM.Data))
	}
	if !bytes.Equal(got.Data, srcPCM.Data[:len(got.Data)]) {
		t.Fatalf("delivered samples are NOT the source prefix: the evidence sink must record exactly what would have played")
	}

	// The delivered span should be in the plausible pacing band: the stop
	// landed at ~150 ms, delivery happens in ~10 ms slices (+1 final slice).
	frameBytes := got.Channels * got.BitsPerSample / 8
	gotMs := int64(len(got.Data)/frameBytes) * 1000 / int64(got.SampleRate)
	if gotMs < 100 || gotMs > 250 {
		t.Fatalf("delivered %d ms of a 600 ms tone stopped at 150 ms: pacing implausible", gotMs)
	}
	t.Logf("file sink stop: delivered %d ms (%d bytes) of 600 ms, local stop latency %v (virtual evidence, no acoustic claim)",
		gotMs, len(got.Data), stopLatency)
}

// TestFileSinkGainIsApplied: the evidence is "what would have been heard",
// so a gain of 0.5 must halve the samples in the written file.
func TestFileSinkGainIsApplied(t *testing.T) {
	dir := t.TempDir()
	sink, err := filesink.New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer sink.Close()

	src := speech.MakeToneWAV(440, 0.1, 22050)
	h, err := sink.Play(context.Background(), speech.PlayRequest{
		UtteranceID: "u-gain", Generation: 2, WAV: src, Gain: 0.5,
	})
	if err != nil {
		t.Fatalf("Play: %v", err)
	}
	<-h.Done()

	p := h.(*filesink.Play).Path()
	got, err := readWAV(t, p)
	if err != nil {
		t.Fatalf("reading evidence file: %v", err)
	}
	want, err := speech.DecodeWAV(src)
	if err != nil {
		t.Fatalf("decoding source: %v", err)
	}
	if len(got.Data) != len(want.Data) {
		t.Fatalf("length mismatch: %d vs %d", len(got.Data), len(want.Data))
	}
	// 0.5 gain on a 0.25-amplitude sine → 0.125: check a mid sample.
	i := len(want.Data) / 2
	gv := int16(uint16(got.Data[i]) | uint16(got.Data[i+1])<<8)
	wv := int16(uint16(want.Data[i]) | uint16(want.Data[i+1])<<8)
	if want := int16(float64(wv) * 0.5); int(gv-want) > 2 || int(want-gv) > 2 {
		t.Fatalf("gain not applied at sample %d: got %d want ≈%d", i, gv, want)
	}
}

// readWAV decodes one evidence file.
func readWAV(t *testing.T, path string) (speech.WAVPCM, error) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return speech.WAVPCM{}, err
	}
	return speech.DecodeWAV(b)
}
