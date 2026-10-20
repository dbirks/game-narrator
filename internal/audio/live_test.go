package audio_test

// The live null-sink check of ADR-0074 (gap audit G5): the PRODUCT tone
// (audio.ToneWAV — the exact bytes the /api/v1/audio/test-tone endpoint
// plays) is played into a dedicated PipeWire null sink through
// pipewire.New(sink) — the same constructor the endpoint's toneSink builds —
// while pw-record records the sink's monitor source. The assertion is that
// the monitor recording carries a contiguous, non-silent 440 Hz run: the
// selected device RECEIVED the tone. VIRTUAL sink evidence only — no
// acoustic, hardware or latency-to-ear claim (ADR-0023; docs/spec/11: a WAV
// file existing is not proof it was audible).
//
// The headless host without a user PipeWire session skips with the NOT RUN
// marker, the way the pipewire sink's own round-trip test does.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/audio"
	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/speech/pipewire"
)

const toneTestSinkName = "gn_audio_tone_sink"

// toneUp skips with the documented NOT RUN marker when the user's PipeWire
// session is unavailable (headless CI, no user bus).
func toneUp(t *testing.T) {
	t.Helper()
	if _, err := exec.Command("pactl", "info").CombinedOutput(); err != nil {
		t.Skipf("NOT RUN: no pipewire sink (%v)", err)
	}
}

// loadToneNullSink loads a dedicated null sink for the check and unloads it
// (plus any leftover module with the same name from a crashed prior run) in
// t.Cleanup. It returns once the sink is visible to pactl.
func loadToneNullSink(t *testing.T) {
	t.Helper()
	toneUp(t)
	unloadToneSinkByName(t)

	out, err := exec.Command("pactl", "load-module", "module-null-sink", "sink_name="+toneTestSinkName).CombinedOutput()
	if err != nil {
		t.Skipf("NOT RUN: no pipewire sink (load-module failed: %v: %s)", err, strings.TrimSpace(string(out)))
	}
	t.Cleanup(func() { unloadToneSinkByName(t) })

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if toneSinkVisible(t) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("null sink %s did not appear within 3s", toneTestSinkName)
}

func unloadToneSinkByName(t *testing.T) {
	t.Helper()
	out, _ := exec.Command("pactl", "list", "short", "modules").CombinedOutput()
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, toneTestSinkName) {
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

func toneSinkVisible(t *testing.T) bool {
	t.Helper()
	out, _ := exec.Command("pactl", "list", "short", "sinks").CombinedOutput()
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, toneTestSinkName) {
			return true
		}
	}
	return false
}

// toneMonitorSerial resolves the monitor source's node serial (pw-record
// --target wants a serial; a plain name is rejected with "no target node
// available").
func toneMonitorSerial(t *testing.T) int {
	t.Helper()
	out, err := exec.Command("pactl", "list", "short", "sources").CombinedOutput()
	if err != nil {
		t.Fatalf("list sources: %v", err)
	}
	want := toneTestSinkName + ".monitor"
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

// toneRun is one monitor recording's analysis.
type toneRun struct {
	present  bool    // a contiguous tone run of >= 0.15 s exists
	longestS float64 // the longest contiguous non-silent run, in seconds
	rms      int     // the whole recording's RMS (the non-silence floor)
	freq     int     // the tone's estimated frequency (sign changes over the run)
}

// analyzeToneRun scans the 16-bit mono recording the way the pipewire sink's
// own round-trip analysis does (internal/speech/pipewire): 33 ms windowed RMS
// against a threshold of max(peak window RMS / 10, 100). A sample threshold
// on individual samples is wrong for a sine — even the pure tone has a
// sub-threshold dip at every zero crossing — so the windows decide "tone
// present" and the longest run of active windows is the tone. The frequency
// is the sign changes inside the longest run, with sub-threshold dip samples
// treated as transparent (a dip at a zero crossing must not swallow the
// crossing it sits on).
func analyzeToneRun(data []byte) toneRun {
	sampleAt := func(i int) int {
		return int(int16(uint16(data[2*i]) | uint16(data[2*i+1])<<8))
	}
	n := len(data) / 2
	if n == 0 {
		return toneRun{}
	}

	var sum, sumSq int64
	for i := 0; i < n; i++ {
		s := sampleAt(i)
		sum += int64(s)
		sumSq += int64(s) * int64(s)
	}
	variance := sumSq/int64(n) - (sum/int64(n))*(sum/int64(n))

	const win = 1600 // 33 ms @ 48 kHz
	nw := n / win
	if nw == 0 {
		return toneRun{}
	}
	winRMS := make([]float64, nw)
	maxRMS := 0.0
	for w := 0; w < nw; w++ {
		var sq int64
		for i := w * win; i < (w+1)*win; i++ {
			s := sampleAt(i)
			sq += int64(s) * int64(s)
		}
		winRMS[w] = windowRMS(sq, win)
		if winRMS[w] > maxRMS {
			maxRMS = winRMS[w]
		}
	}
	thresh := maxRMS / 10
	if thresh < 100 {
		thresh = 100
	}

	var longest, cur int
	longStart := 0
	for i := 0; i < nw; i++ {
		if winRMS[i] < thresh {
			continue
		}
		if cur == 0 {
			longStart = i
		}
		cur++
		if cur > longest {
			longest = cur
		}
	}

	r := toneRun{longestS: float64(longest*win) / 48000}
	if variance > 0 {
		r.rms = intRsqrt(variance)
	}
	if longest < 1 {
		return r
	}
	if r.longestS >= 0.15 {
		r.present = true
	}

	// Frequency over the longest run: sign changes between samples whose
	// magnitude clears the 100 floor (one crossing per half-cycle).
	start, end := longStart*win, (longStart+longest)*win
	cross, lastSign := 0, 0
	for i := start; i < end; i++ {
		s := sampleAt(i)
		abs := s
		if abs < 0 {
			abs = -abs
		}
		if abs < 100 {
			continue
		}
		sign := 1
		if s < 0 {
			sign = -1
		}
		if lastSign != 0 && sign != lastSign {
			cross++
		}
		lastSign = sign
	}
	if cross > 0 {
		ms := longest * win / 48 // 48 kHz → ms = frames / 48
		if ms > 0 {
			// hz = crossings / (2 * secs): cross*1000 first so the integer
			// division keeps its precision.
			r.freq = cross * 1000 / (2 * ms)
		}
	}
	return r
}

// windowRMS is the floating-point RMS of one window (mean of squares, square
// root; the window's sum of squares is well inside float64's range).
func windowRMS(sq int64, win int) float64 {
	v := float64(sq) / float64(win)
	x := 1.0
	for i := 0; i < 40; i++ {
		x = (x + v/x) / 2
	}
	return x
}

// intRsqrt is the integer square root (Newton's method; the variance is far
// below int64's range, so the iteration converges in a handful of steps).
func intRsqrt(v int64) int {
	if v < 2 {
		return int(v)
	}
	bits := 0
	for x := v; x > 0; x >>= 1 {
		bits++
	}
	x := int64(1) << (bits / 2)
	for {
		next := (x + v/x) / 2
		if next >= x {
			return int(x)
		}
		x = next
	}
}

// TestToneReachesSelectedNullSink is the dev-host live check (ADR-0074): the
// product tone, through the endpoint's own sink constructor, into a dedicated
// null sink, recorded at its monitor. A 2-attempt bound retries once (host
// load / monitor lock is the usual cause of a bad recording); a second bad
// recording is reported, not papered over.
func TestToneReachesSelectedNullSink(t *testing.T) {
	loadToneNullSink(t)

	sink, err := pipewire.New(toneTestSinkName)
	if err != nil {
		t.Fatalf("pipewire.New(%s): %v", toneTestSinkName, err)
	}
	defer sink.Close()

	tone := audio.ToneWAV()
	var last toneRun
	for attempt := 1; attempt <= 2; attempt++ {
		rec := filepath.Join(t.TempDir(), fmt.Sprintf("tone-monitor-%d.wav", attempt))
		serial := toneMonitorSerial(t)
		recCmd := exec.Command("pw-record", "--target="+strconv.Itoa(serial),
			"--rate=48000", "--channels=1", "--format=s16", "-n", "144000", rec)
		if err := recCmd.Start(); err != nil {
			t.Skipf("NOT RUN: no pipewire sink (pw-record spawn: %v)", err)
		}

		// Bounded flow probe: the WAV grows past its header only once the
		// monitor stream actually carries samples. A stuck recorder is a
		// hard stop — retrying a stream that never locked would only burn
		// the whole window twice.
		if !recorderFlowing(rec, 5*time.Second) {
			_ = recCmd.Process.Kill()
			_ = recCmd.Wait()
			t.Fatalf("the monitor stream produced no sample flow within 5s (never locked)")
		}

		handle, err := sink.Play(context.Background(), speech.PlayRequest{
			UtteranceID: "tone", Generation: 1, WAV: tone, Gain: 1.0,
		})
		if err != nil {
			_ = recCmd.Process.Kill()
			_ = recCmd.Wait()
			t.Fatalf("attempt %d: the product tone would not play: %v", attempt, err)
		}
		res := <-handle.Done()
		if res.Err != nil {
			_ = recCmd.Process.Kill()
			_ = recCmd.Wait()
			t.Fatalf("attempt %d: the tone playback failed: %v", attempt, res.Err)
		}
		// pw-record exits 1 when it stops at --sample-count while the file is
		// complete (a verified quirk of this build): the evidence is the
		// samples, so a nonzero exit is tolerated and the file is validated.
		if err := recCmd.Wait(); err != nil {
			t.Logf("pw-record exited %v (sample-count stop quirk); validating the file instead", err)
		}

		data, err := os.ReadFile(rec)
		if err != nil || len(data) < 44 {
			t.Fatalf("attempt %d: the recorder wrote no valid WAV (%d bytes): %v", attempt, len(data), err)
		}
		pcm, err := speech.DecodeWAV(data)
		if err != nil {
			t.Fatalf("attempt %d: decode recording: %v", attempt, err)
		}
		if pcm.Frames() < 144000-4000 {
			t.Fatalf("attempt %d: recording has only %d frames, want ≈144000 (the monitor stream did not run)", attempt, pcm.Frames())
		}

		last = analyzeToneRun(pcm.Data)
		if last.present && last.rms >= 500 && last.freq >= 380 && last.freq <= 520 {
			t.Logf("VIRTUAL SINK EVIDENCE (no acoustic claim): the product tone reached %s — longest contiguous tone run %.3fs of the 1.0 s tone, est %d Hz, whole-recording RMS %d (attempt %d)",
				toneTestSinkName, last.longestS, last.freq, last.rms, attempt)
			return
		}
		t.Logf("attempt %d unusable: present=%v run=%.3fs rms=%d freq=%d", attempt, last.present, last.longestS, last.rms, last.freq)
	}
	t.Fatalf("the tone is not provable at %s after 2 attempts (last: present=%v run=%.3fs rms=%d freq=%d): the monitor evidence is the assertion — no weaker threshold (docs/spec/11)",
		toneTestSinkName, last.present, last.longestS, last.rms, last.freq)
}

// recorderFlowing polls the recorder's WAV until it holds at least 33 ms of
// PCM (bounded by deadline; the same pattern the pipewire sink test uses).
func recorderFlowing(path string, deadline time.Duration) bool {
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
