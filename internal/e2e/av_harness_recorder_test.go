// Package e2e — the A/V harness's shared-clock recorder (ADR 0143): the
// isolated PipeWire null sink, the FFmpeg X11+Pulse shared-clock capture
// (one .mp4 + one 16 kHz mono .wav from the same wall clock, T0) and the
// per-line clip slicer. Log-safe per AGENTS.md rule 5: paths, ids and
// timings only — the recorded media are gitignored bundle artifacts.
package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// avCreateNullSink loads a dedicated PipeWire null sink (gn_av_<run id>)
// for the run — the narrator's audio output target, isolated from the
// user's real devices (the internal/speech/pipewire sink test's proven
// module-null-sink shape, with the ADR-0143 sink name) — and unloads it in
// t.Cleanup, sweeping any leftover module with the same sink name first so
// a crashed prior run cannot collide. A host without a usable PipeWire
// session reports NOT RUN (a skip, never a pass).
func avCreateNullSink(t *testing.T, runID string) (sinkName string) {
	t.Helper()
	sinkName = "gn_av_" + avSanitizeName(runID)
	if _, err := exec.Command("pactl", "info").CombinedOutput(); err != nil {
		t.Skipf("NOT RUN: no PipeWire session (pactl: %v) — the A/V harness needs a user PipeWire instance for the isolated null sink", err)
	}
	avUnloadSinkModule(t, sinkName)
	out, err := exec.Command("pactl", "load-module",
		"module-null-sink", "sink_name="+sinkName, "sink_properties=device.description=gn_av").CombinedOutput()
	if err != nil {
		t.Skipf("NOT RUN: the PipeWire null sink could not be loaded (%v: %s)", err, strings.TrimSpace(string(out)))
	}
	t.Cleanup(func() { avUnloadSinkModule(t, sinkName) })
	deadline := time.Now().Add(3 * time.Second)
	for !avSinkVisible(sinkName) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if !avSinkVisible(sinkName) {
		t.Fatalf("null sink %s did not appear within 3s of load-module", sinkName)
	}
	t.Logf("av harness: null sink %s loaded (monitor source %s.monitor)", sinkName, sinkName)
	return sinkName
}

// avSanitizeName reduces one id to the characters a PipeWire node name
// accepts (the run id's timestamp punctuation would break the pactl args).
func avSanitizeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "av"
	}
	return b.String()
}

// avUnloadSinkModule unloads every loaded module whose name line mentions
// the sink (the module-null-sink load and its leftovers alike).
func avUnloadSinkModule(t *testing.T, sinkName string) {
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
			t.Logf("av harness: unload-module %s: %v (%s)", fields[0], err, strings.TrimSpace(string(o)))
		}
	}
}

// avSinkVisible reports whether the sink (or its monitor) is listed.
func avSinkVisible(sinkName string) bool {
	out, err := exec.Command("pactl", "list", "short", "sinks").CombinedOutput()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, sinkName) {
			return true
		}
	}
	return false
}

// avRecorder is one shared-clock FFmpeg capture: x11grab of the Dolphin
// Xvfb display at 15 fps plus the null sink's monitor audio, muxed into
// one .mp4 (the agent-inspectable video+audio container) and one 16 kHz
// mono .wav (the RMS-onset track), both stamped from the same wall clock.
type avRecorder struct {
	cmd       *exec.Cmd
	MP4Path   string
	WAVPath   string
	StartedAt time.Time // T0: the wall clock the timeline measures from
}

// avStartRecorder launches the shared-clock capture. display is the
// Dolphin Xvfb display (the bridge's State().Display) and monitorSource
// the null sink's monitor (gn_av_<run>.monitor). T0 is recorded AFTER the
// ffmpeg process has been spawned, so every timeline instant is an offset
// into the recorded media (a frame at T0+offset is at that offset in both
// files). It returns the stop func (SIGINT, then up to 15 s for ffmpeg to
// finalize the MP4 trailer — a hard kill leaves the container unplayable)
// and T0.
func avStartRecorder(t *testing.T, display, monitorSource, mp4Path, wavPath string) (stop func() error, startedAt time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(mp4Path), 0o755); err != nil {
		t.Fatalf("av recorder mkdir %s: %v", filepath.Dir(mp4Path), err)
	}
	args := []string{"-y",
		"-f", "x11grab", "-framerate", "15", "-video_size", "640x480", "-i", display,
		"-f", "pulse", "-i", monitorSource,
		"-map", "0:v:0", "-map", "1:a:0",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "128k", mp4Path,
		"-map", "1:a:0", "-c:a", "pcm_s16le", "-ac", "1", "-ar", "16000", wavPath,
	}
	cmd := exec.Command("ffmpeg", args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("av recorder: ffmpeg start: %v", err)
	}
	rec := &avRecorder{cmd: cmd, MP4Path: mp4Path, WAVPath: wavPath, StartedAt: time.Now().UTC()}
	t.Logf("av harness: shared-clock recording started: display=%s monitor=%s -> %s + %s (pid %d)",
		display, monitorSource, filepath.Base(mp4Path), filepath.Base(wavPath), cmd.Process.Pid)
	stop = func() error {
		if rec.cmd.Process == nil {
			return nil
		}
		// SIGINT: ffmpeg's clean-stop path finalizes the MP4 trailer (an
		// unclosed container is unplayable; the 15 s bound is the
		// trailer-flush budget for a long session recording).
		if err := rec.cmd.Process.Signal(syscall.SIGINT); err != nil {
			// already exited
		}
		done := make(chan error, 1)
		go func() { done <- rec.cmd.Wait() }()
		select {
		case err := <-done:
			return err
		case <-time.After(15 * time.Second):
			_ = rec.cmd.Process.Kill()
			<-done
			t.Errorf("av recorder: ffmpeg did not stop within 15s of SIGINT (killed; the MP4 trailer may be missing)\nlast stderr: %s", strings.TrimSpace(stderr.String()))
			return fmt.Errorf("ffmpeg stop timed out")
		}
	}
	return stop, rec.StartedAt
}

// avSliceClips slices one line's span — [t_box_appear - 200 ms,
// t_audio_end + 300 ms] (the ADR-0143 clip window, the box's context plus
// the whole playback with a tail) — out of the shared-clock session media
// into clips/line-NN.mp4 and line-NN.wav (the agent's direct multimodal
// inspection units, read_file). A line without both stamps (fenced before
// audio) is skipped: its timeline row is the evidence.
func avSliceClips(t *testing.T, sessionMP4, sessionWAV, clipsDir string, lines *[]avLineRecord) {
	t.Helper()
	if err := os.MkdirAll(clipsDir, 0o755); err != nil {
		t.Fatalf("av clips mkdir %s: %v", clipsDir, err)
	}
	for i := range *lines {
		l := &(*lines)[i]
		if l.TBoxAppearMS == 0 || l.TAudioEndMS == 0 {
			continue
		}
		from := l.TBoxAppearMS - 200
		if from < 0 {
			from = 0
		}
		to := l.TAudioEndMS + 300
		dur := to - from
		wavFrom := from
		if l.clipAudioStartMS > 0 && l.clipAudioStartMS-100 > wavFrom && l.clipAudioStartMS < l.TAudioEndMS {
			wavFrom = l.clipAudioStartMS - 100
		}
		wavDur := to - wavFrom
		mp4 := filepath.Join(clipsDir, fmt.Sprintf("line-%02d.mp4", i+1))
		wav := filepath.Join(clipsDir, fmt.Sprintf("line-%02d.wav", i+1))
		// Input-side -ss (the fast seek) + -t (the clip's duration): the clips
		// are inspection media, not bit-exact evidence — the session files
		// are.
		if out, err := exec.Command("ffmpeg", "-y", "-ss", avSec(from),
			"-i", sessionMP4, "-t", avSec(dur), "-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac", mp4).CombinedOutput(); err != nil {
			t.Logf("av harness: line %02d mp4 clip failed: %v: %s", i+1, err, strings.TrimSpace(string(out)))
			continue
		}
		if out, err := exec.Command("ffmpeg", "-y", "-ss", avSec(wavFrom),
			"-i", sessionWAV, "-t", avSec(wavDur), "-c:a", "pcm_s16le", "-ac", "1", "-ar", "16000", wav).CombinedOutput(); err != nil {
			t.Logf("av harness: line %02d wav clip failed: %v: %s", i+1, err, strings.TrimSpace(string(out)))
			continue
		}
		l.ClipMP4 = mp4
		l.ClipWAV = wav
	}
	t.Logf("av harness: per-line clips sliced into %s (%d lines with audio spans)", clipsDir, len(*lines))
}

// avSec renders one ms offset as the ffmpeg -ss/-t seconds operand.
func avSec(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	return strconv.FormatFloat(float64(ms)/1000.0, 'f', 3, 64)
}
