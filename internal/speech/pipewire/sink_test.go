// Package pipewire_test is the REAL PipeWire round-trip test. It runs
// against the user's PipeWire session on a DEDICATED null sink
// (module-null-sink, sink_name=gn_test_sink): a 440 Hz tone is played
// through the subprocess sink while pw-record records the sink's monitor
// source, and the recording is asserted to contain non-silent audio near
// 440 Hz (zero-crossing frequency estimate).
//
// docs/spec/11: a WAV file existing is NOT proof it was audible — the
// monitor recording proves samples reached the sink. This is VIRTUAL sink
// evidence only: no acoustic, hardware, or latency-to-ear claim (ADR-0023
// marks the acoustic path NOT VERIFIED without hardware/mic).
package pipewire_test

import (
	"context"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/speech/pipewire"
)

const testSinkName = "gn_test_sink"

// pipewireUp skips with the documented NOT RUN marker when the user
// PipeWire session is unavailable (headless CI, no user bus).
func pipewireUp(t *testing.T) {
	t.Helper()
	if _, err := exec.Command("pactl", "info").CombinedOutput(); err != nil {
		t.Skipf("NOT RUN: no pipewire sink (%v)", err)
	}
}

// loadNullSink loads a dedicated null sink and unloads it (plus any leftover
// module with the same sink name from a crashed prior run) in t.Cleanup.
// It returns once the sink is visible to pactl.
func loadNullSink(t *testing.T) {
	t.Helper()
	pipewireUp(t)

	// Sweep leftovers first so a crashed run cannot leave the module loaded.
	unloadByName(t, testSinkName)

	out, err := exec.Command("pactl", "load-module",
		"module-null-sink", "sink_name="+testSinkName).CombinedOutput()
	if err != nil {
		t.Skipf("NOT RUN: no pipewire sink (load-module failed: %v: %s)", err, strings.TrimSpace(string(out)))
	}
	t.Cleanup(func() { unloadByName(t, testSinkName) })

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if sinkVisible(t) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("null sink %s did not appear within 3s", testSinkName)
}

func unloadByName(t *testing.T, sinkName string) {
	t.Helper()
	out, _ := exec.Command("pactl", "list", "short", "modules").CombinedOutput()
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, sinkName) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if o, err := exec.Command("pactl", "unload-module", fields[0]).CombinedOutput(); err != nil {
			t.Logf("leftover unload %s: %v (%s)", fields[0], err, strings.TrimSpace(string(o)))
		}
	}
}

func sinkVisible(t *testing.T) bool {
	t.Helper()
	out, _ := exec.Command("pactl", "list", "short", "sinks").CombinedOutput()
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, testSinkName) {
			return true
		}
	}
	return false
}

// monitorSerial resolves the monitor source's node serial from its name.
// pw-record --target wants a serial (a plain name is rejected with
// "no target node available"), so the test resolves the numeric id.
func monitorSerial(t *testing.T) int {
	t.Helper()
	out, err := exec.Command("pactl", "list", "short", "sources").CombinedOutput()
	if err != nil {
		t.Fatalf("list sources: %v", err)
	}
	want := testSinkName + ".monitor"
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[1] != want {
			continue
		}
		serial, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		return serial
	}
	t.Fatalf("monitor source %q not found", want)
	return 0
}

// TestPipeWireNullSinkRoundTrip proves samples reach the sink: play a 1 s
// 440 Hz tone into gn_test_sink while parecord records its monitor, then
// assert the recording is non-silent and near 440 Hz.
func TestPipeWireNullSinkRoundTrip(t *testing.T) {
	loadNullSink(t)

	rec := t.TempDir() + "/monitor.wav"
	// pw-record stops itself after 96000 samples (2.0 s at 48 kHz), so the
	// WAV header is finalized by a natural exit (no kill mid-write).
	recCmd := exec.Command("pw-record", "--target="+strconv.Itoa(monitorSerial(t)),
		"--rate=48000", "--channels=1", "--format=s16", "-n", "96000", rec)
	recStart := time.Now()
	if err := recCmd.Start(); err != nil {
		t.Skipf("NOT RUN: no pipewire sink (pw-record spawn: %v)", err)
	}
	time.Sleep(800 * time.Millisecond) // let the recorder lock the stream

	sink, err := pipewire.New(testSinkName)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer sink.Close()

	tone := speech.MakeToneWAV(440, 1.0, 44100)
	playAt := time.Now()
	h, err := sink.Play(context.Background(), speech.PlayRequest{
		UtteranceID: "tone-440", Generation: 1, WAV: tone,
	})
	if err != nil {
		t.Fatalf("Play: %v", err)
	}
	res := <-h.Done()
	if res.Err != nil {
		t.Fatalf("playback errored: %+v", res)
	}
	// pw-record on this build exits 1 when it stops at --sample-count while
	// still writing a complete, finalized file (verified manually). The
	// evidence is the samples (docs/spec/11), so a nonzero exit is tolerated
	// and logged; an invalid/short recording still fails the test.
	recDone := recCmd.Wait()
	if recDone != nil {
		t.Logf("pw-record exited with %v (sample-count stop quirk); validating the file instead", recDone)
	}

	// Analyze the monitor recording — the proof (docs/spec/11).
	pcm, err := speech.DecodeWAV(mustRead(t, rec))
	if err != nil {
		t.Fatalf("decode recording: %v", err)
	}
	const wantFrames = 96000 // 2.0 s @ 48 kHz
	if frames := pcm.Frames(); frames < wantFrames-4000 {
		t.Fatalf("recording has only %d frames, want ≈%d: the monitor stream did not run (pw-record: %v)", frames, wantFrames, recDone)
	}
	freq, rms := toneEstimate(pcm)
	if rms < 500 {
		t.Fatalf("recording is (near-)silent: RMS %d — samples did NOT reach the sink (recorder %v, playback started %v after record start)",
			rms, time.Since(recStart).Round(time.Millisecond), time.Since(playAt).Round(time.Millisecond))
	}
	if freq < 380 || freq > 520 {
		t.Fatalf("recording frequency estimate %d Hz is not near 440 Hz (RMS %d)", freq, rms)
	}
	t.Logf("VIRTUAL SINK EVIDENCE (no acoustic claim): 440 Hz tone recorded via %s.monitor — est %d Hz, RMS %d, whole pipeline %v",
		testSinkName, freq, rms, time.Since(recStart).Round(time.Millisecond))
}

// TestPipeWireStopLatency measures the LOCAL stop latency on a 3 s tone
// stopped at 500 ms: Stop() → subprocess stdin close + SIGTERM → child exit
// (Done result). Device-side buffer (pw-play --latency=50ms) is excluded
// from the number and reported as discarded, not faded (docs/spec/12
// wording, ADR-0023).
func TestPipeWireStopLatency(t *testing.T) {
	loadNullSink(t)
	sink, err := pipewire.New(testSinkName)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer sink.Close()

	tone := speech.MakeToneWAV(440, 3.0, 44100)
	h, err := sink.Play(context.Background(), speech.PlayRequest{
		UtteranceID: "tone-3s", Generation: 2, WAV: tone,
	})
	if err != nil {
		t.Fatalf("Play: %v", err)
	}
	time.Sleep(500 * time.Millisecond) // play half a second of the 3 s tone
	stopAt := time.Now()
	h.Stop(0)
	res := <-h.Done()
	latency := res.Ended.Sub(stopAt)
	if !res.Interrupted {
		t.Fatalf("stop did not interrupt the playback: %+v", res)
	}
	if latency > 1*time.Second {
		t.Fatalf("stop latency %v is unreasonable (subprocess should exit promptly on SIGTERM)", latency)
	}
	t.Logf("measured stop latency (local, 3 s tone stopped at 500 ms, subprocess SIGTERM): %v; docs target 200 ms; device buffer discarded, not faded",
		latency.Round(time.Millisecond))
}

// toneEstimate returns (estimated Hz via zero crossings, RMS) over the
// active (non-silent) span of a mono 16-bit recording. The active span is
// first..last sample above a silence threshold; the estimate counts sign
// changes there: one sine cycle crosses zero twice.
func toneEstimate(pcm speech.WAVPCM) (freq, rms int) {
	if pcm.Channels != 1 || pcm.BitsPerSample != 16 {
		// The test forces s16le mono; anything else is a negotiation surprise.
		return 0, 0
	}
	n := pcm.Frames()
	if n < pcm.SampleRate/4 {
		return 0, 0
	}
	vals := make([]int16, n)
	for i := 0; i < n; i++ {
		off := i * 2
		vals[i] = int16(uint16(pcm.Data[off]) | uint16(pcm.Data[off+1])<<8)
	}
	const thresh = 100
	first, last := -1, -1
	for i := 0; i < n; i++ {
		if v := int(vals[i]); v > thresh || v < -thresh {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 || last-first < 100 {
		return 0, 0
	}
	crossings := 0
	sumSq := int64(0)
	for i := first; i < last; i++ {
		sumSq += int64(vals[i]) * int64(vals[i])
		if i > first && ((vals[i-1] < 0 && vals[i] >= 0) || (vals[i-1] > 0 && vals[i] <= 0)) {
			crossings++
		}
	}
	spanS := float64(last-first) / float64(pcm.SampleRate)
	freq = int(float64(crossings)/(2*spanS) + 0.5)
	rms = int(math.Sqrt(float64(sumSq) / float64(last-first)))
	return freq, rms
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if len(b) < 44 {
		t.Fatalf("%s is %d bytes: recorder wrote no valid WAV", path, len(b))
	}
	return b
}
