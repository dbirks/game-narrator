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
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
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
// assert the recording contains a contiguous 440 Hz tone. The frequency is
// estimated over the STEADY MIDDLE of the longest contiguous tone run
// (start/end transients and any underrun gap are excluded), so host load that
// fragments the recording cannot skew the estimate the way a first..last-span
// count can. A recording that shows a dropout (tone material exists but is
// broken into short runs) or is otherwise unusable is retried ONCE; a second
// dropout is reported explicitly, not papered over.
func TestPipeWireNullSinkRoundTrip(t *testing.T) {
	loadNullSink(t)

	sink, err := pipewire.New(testSinkName)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer sink.Close()

	var last toneAttempt
	for attempt := 1; attempt <= 2; attempt++ {
		last = recordRoundTrip(t, sink, attempt)
		if last.analysis.present && !last.analysis.dropout && last.analysis.freq >= 380 && last.analysis.freq <= 520 && last.analysis.rms >= 500 {
			t.Logf("VIRTUAL SINK EVIDENCE (no acoustic claim): 440 Hz tone recorded via %s.monitor — est %d Hz over the steady middle, RMS %d, longest contiguous tone run %.3fs of the 1.0 s tone, whole pipeline %v (attempt %d)",
				testSinkName, last.analysis.freq, last.analysis.rms, last.analysis.longestRunS, last.pipeline, attempt)
			return
		}
		t.Logf("attempt %d unusable: %s — %s (longest run %.3fs, total tone %.3fs, est %d Hz, RMS %d, pipeline %v)",
			attempt, last.problem, last.analysis.describe(), last.analysis.longestRunS, last.analysis.totalActiveS, last.analysis.freq, last.analysis.rms, last.pipeline)
		if attempt == 1 {
			t.Log("retrying once (host load / underrun is the usual cause)")
		}
	}
	a := last.analysis
	t.Fatalf("round trip not provable after 2 attempts: last attempt %s — %s (longest contiguous tone run %.3fs, total tone %.3fs, est %d Hz over the steady middle, RMS %d, recorder %v, pipeline %v); a mid-tone gap of that shape is a host PipeWire underrun under load, and the monitor evidence is what the assertion is made of — no fallback, no weaker threshold (docs/spec/11)",
		last.problem, a.describe(), a.longestRunS, a.totalActiveS, a.freq, a.rms, last.recErr, last.pipeline)
}

// toneAttempt is one record+play cycle: the analysis plus a human-readable
// problem string when the recording is unusable.
type toneAttempt struct {
	analysis toneAnalysis
	problem  string
	pipeline time.Duration // record start -> recorder stopped (wall, load included)
	recErr   error         // pw-record exit (nonzero is a known stop quirk)
}

// recordRoundTrip runs one monitor-record + tone-play cycle and returns its
// analysis. It never t.Fatals on a bad recording: the caller retries once and
// reports. t.Skip is reserved for "no PipeWire session" (NOT RUN).
func recordRoundTrip(t *testing.T, sink *pipewire.Sink, attempt int) toneAttempt {
	t.Helper()
	at := toneAttempt{}
	rec := filepath.Join(t.TempDir(), fmt.Sprintf("monitor-attempt%d.wav", attempt))
	recCmd := exec.Command("pw-record", "--target="+strconv.Itoa(monitorSerial(t)),
		"--rate=48000", "--channels=1", "--format=s16", "-n", "144000", rec)
	recStart := time.Now()
	if err := recCmd.Start(); err != nil {
		t.Skipf("NOT RUN: no pipewire sink (pw-record spawn: %v)", err)
	}
	// Bounded probe, not a blind sleep: pw-record writes the WAV
	// incrementally, so the file grows past the header only once samples
	// actually flow through the monitor stream. Under load the monitor may
	// take longer to lock than a fixed delay allows; a stuck recorder is
	// reported as an unusable attempt instead of wasting the whole window.
	if !recorderFlowing(t, rec, 5*time.Second) {
		_ = recCmd.Process.Kill()
		at.recErr = recCmd.Wait()
		at.problem = "recorder produced no sample flow within 5s (monitor stream never locked)"
		at.pipeline = time.Since(recStart)
		return at
	}

	// Record window is 3.0 s @ 48 kHz: the flow probe above can consume up to
	// ~1 s of it before the tone starts, so the 1.0 s tone always fits inside
	// the window (the old 2.0 s window did not guarantee that under load).
	tone := speech.MakeToneWAV(440, 1.0, 44100)
	h, err := sink.Play(context.Background(), speech.PlayRequest{
		UtteranceID: fmt.Sprintf("tone-440-a%d", attempt), Generation: int64(attempt), WAV: tone,
	})
	if err != nil {
		_ = recCmd.Process.Kill()
		at.recErr = recCmd.Wait()
		at.problem = fmt.Sprintf("playback spawn failed: %v", err)
		at.pipeline = time.Since(recStart)
		return at
	}
	res := <-h.Done()
	if res.Err != nil {
		_ = recCmd.Process.Kill()
		at.recErr = recCmd.Wait()
		at.problem = fmt.Sprintf("playback errored: %+v", res)
		at.pipeline = time.Since(recStart)
		return at
	}
	// pw-record on this build exits 1 when it stops at --sample-count while
	// still writing a complete, finalized file (verified manually). The
	// evidence is the samples (docs/spec/11), so a nonzero exit is tolerated
	// and logged; an invalid/short recording still fails the attempt.
	at.recErr = recCmd.Wait()
	if at.recErr != nil {
		t.Logf("pw-record exited with %v (sample-count stop quirk); validating the file instead", at.recErr)
	}
	at.pipeline = time.Since(recStart)

	data, err := os.ReadFile(rec)
	if err != nil || len(data) < 44 {
		at.problem = fmt.Sprintf("recorder wrote no valid WAV (%d bytes): %v", len(data), err)
		return at
	}
	pcm, err := speech.DecodeWAV(data)
	if err != nil {
		at.problem = fmt.Sprintf("decode recording: %v", err)
		return at
	}
	const wantFrames = 144000 // 3.0 s @ 48 kHz
	if frames := pcm.Frames(); frames < wantFrames-4000 {
		at.problem = fmt.Sprintf("recording has only %d frames, want ≈%d: the monitor stream did not run (pw-record: %v)", frames, wantFrames, at.recErr)
		return at
	}

	at.analysis = analyzeTone(pcm)
	if !at.analysis.present {
		if at.analysis.rms < 500 {
			at.problem = "recording is (near-)silent: samples did NOT reach the sink"
		} else {
			at.problem = "no contiguous tone run of ≥0.15 s anywhere in the recording (tone fragmented or never played through)"
		}
		return at
	}
	if at.analysis.dropout {
		at.problem = fmt.Sprintf("DROPOUT: tone material exists (%.3fs total) but the longest contiguous run is %.3fs — a mid-tone gap (underrun) is detectable in the recording", at.analysis.totalActiveS, at.analysis.longestRunS)
		return at
	}
	if f := at.analysis.freq; f < 380 || f > 520 {
		at.problem = fmt.Sprintf("steady-middle frequency estimate %d Hz is not near 440 Hz (RMS %d)", f, at.analysis.rms)
		return at
	}
	if at.analysis.rms < 500 {
		at.problem = fmt.Sprintf("tone present but weak: RMS %d < 500", at.analysis.rms)
		return at
	}
	return at
}

// recorderFlowing polls the size of the WAV the recorder is writing until it
// holds at least 33 ms of PCM. pw-record writes incrementally (header first,
// then data as it arrives), so growth past the header is direct evidence the
// monitor stream is live. The 50 ms poll is a bounded condition probe (the
// same pattern as sinkVisible's polling), not a timing assumption: the
// function returns as soon as the condition is true and never past the
// deadline.
func recorderFlowing(t *testing.T, path string, deadline time.Duration) bool {
	t.Helper()
	const minBytes = 44 + 1600*2 // WAV header + 33 ms of 48 kHz s16 mono
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if st, err := os.Stat(path); err == nil && st.Size() >= int64(minBytes) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// toneAnalysis is the steady-middle reading of a monitor recording:
// the frequency is a zero-crossing count over the middle 60% of the LONGEST
// CONTIGUOUS tone run (start/end transients and any underrun gap outside the
// run are excluded, and no gap can sit inside the run by construction), so a
// fragmented recording cannot inflate or deflate the estimate the way a
// first..last-span count can.
type toneAnalysis struct {
	freq         int
	rms          int
	present      bool    // a contiguous tone run of ≥0.15 s (≥ ~60 cycles at 440 Hz)
	dropout      bool    // tone material exists but is fragmented into short runs
	longestRunS  float64 // longest contiguous active run, seconds
	totalActiveS float64 // all active windows, seconds
}

func (a toneAnalysis) describe() string {
	if a.present && a.dropout {
		return "tone present but fragmented (mid-tone gap = underrun)"
	}
	if a.present {
		return "contiguous tone present"
	}
	if a.totalActiveS > 0 {
		return "tone material present but too fragmented to analyze"
	}
	return "no tone material detected"
}

// analyzeTone: window the recording (33 ms RMS per window), mark active
// windows against an adaptive threshold (10% of the loudest window, floor
// 100 — the 440 Hz test tone runs at ~5.7k RMS, silence at ~0), then work
// with the runs of consecutive active windows.
func analyzeTone(pcm speech.WAVPCM) toneAnalysis {
	a := toneAnalysis{}
	if pcm.Channels != 1 || pcm.BitsPerSample != 16 {
		// The test forces s16le mono; anything else is a negotiation surprise.
		return a
	}
	n := pcm.Frames()
	if n < 48000/4 {
		return a
	}
	win := 1600 // 33 ms @ 48 kHz: wide enough for a clean window RMS, narrow enough to localize a gap
	if win <= 0 || n < win {
		return a
	}
	nw := n / win
	winRMS := make([]float64, nw)
	maxRMS := 0.0
	for w := 0; w < nw; w++ {
		var sumSq int64
		for i := w * win; i < (w+1)*win; i++ {
			off := i * 2
			v := int(int16(uint16(pcm.Data[off]) | uint16(pcm.Data[off+1])<<8))
			sumSq += int64(v) * int64(v)
		}
		winRMS[w] = math.Sqrt(float64(sumSq) / float64(win))
		if winRMS[w] > maxRMS {
			maxRMS = winRMS[w]
		}
	}
	const floorRMS = 100.0
	thresh := maxRMS / 10
	if thresh < floorRMS {
		thresh = floorRMS
	}

	// Runs of consecutive active windows.
	var runs [][2]int // [startWindow, endWindow) half-open
	for i := 0; i < nw; {
		if winRMS[i] < thresh {
			i++
			continue
		}
		j := i
		for j < nw && winRMS[j] >= thresh {
			j++
		}
		runs = append(runs, [2]int{i, j})
		i = j
	}
	winS := float64(win) / float64(pcm.SampleRate)
	var totalActive int
	longest := 0
	maxGapS := 0.0 // largest silence gap BETWEEN tone runs (a mid-tone gap)
	var longStart, longEnd int
	for i, r := range runs {
		if l := r[1] - r[0]; l > 0 {
			totalActive += l
			if l > longest {
				longest = l
				longStart, longEnd = r[0], r[1]
			}
			if i > 0 {
				if g := float64(r[0]-runs[i-1][1]) * winS; g > maxGapS {
					maxGapS = g
				}
			}
		}
	}
	a.totalActiveS = float64(totalActive) * winS
	a.longestRunS = float64(longest) * winS
	a.present = a.longestRunS >= 0.15
	// A 1.0 s tone that arrives in pieces: tone material on both sides of a
	// ≥0.1 s internal gap — the recording shows a dropout. A boundary clip or
	// a few-window wobble (a gap shorter than one 33 ms window) cannot trip
	// this, and neither can a late tone start (one contiguous run).
	a.dropout = a.totalActiveS >= 0.3 && maxGapS >= 0.1
	if !a.present {
		return a
	}

	// RMS over the whole longest run (presence strength, whole-tone scale).
	{
		var sumSq int64
		for i := longStart * win; i < longEnd*win; i++ {
			off := i * 2
			v := int(int16(uint16(pcm.Data[off]) | uint16(pcm.Data[off+1])<<8))
			sumSq += int64(v) * int64(v)
		}
		a.rms = int(math.Sqrt(float64(sumSq) / (float64(longEnd-longStart) * float64(win))))
	}

	// Frequency: zero crossings over the STEADY MIDDLE (middle 60%) of the
	// longest run. A sine cycle crosses zero twice; the middle 60% of a
	// 0.6 s run is 0.36 s ≈ 158 cycles — far enough from any boundary
	// transient, and enough cycles that the count is not load-noise.
	midStart := longStart*win + (longEnd-longStart)*win*20/100
	midEnd := longStart*win + (longEnd-longStart)*win*80/100
	if midEnd-midStart < pcm.SampleRate/4 {
		return a // fewer than 0.25 s of steady middle: no defensible estimate
	}
	crossings := 0
	for i := midStart + 1; i < midEnd; i++ {
		off := i * 2
		v := int(int16(uint16(pcm.Data[off]) | uint16(pcm.Data[off+1])<<8))
		pv := int(int16(uint16(pcm.Data[off-2]) | uint16(pcm.Data[off-1])<<8))
		if (pv < 0 && v >= 0) || (pv > 0 && v <= 0) {
			crossings++
		}
	}
	spanS := float64(midEnd-midStart) / float64(pcm.SampleRate)
	a.freq = int(float64(crossings)/(2*spanS) + 0.5)
	return a
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
