package hwsmoke

// TestHWAudioOutput is the executable half of docs/spec/10 "List
// ALSA/PipeWire audio devices; play deterministic test tone; measure output
// sample rate … reselect by stable ID on reboot".
//
// The enumeration (sinks, sample rate) is read-only and always runs when an
// audio backend is present. The audible part — a 1 s 440 Hz tone through
// the app's PipeWire sink (internal/speech/pipewire) — runs only when the
// operator sets GAME_NARRATOR_HW_AUDIO_CONSENT=1: docs/spec/13 makes
// playback a consented action, and the gate convention for an unconsented
// audible check is NOT RUN with the exact export instruction.

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/speech/pipewire"
)

// toneFreq and toneDur are the deterministic test tone docs/spec/10 names:
// 440 Hz (concert A) for 1 s — the same tone internal/speech/pipewire's own
// tests use, at the PipeWire default 48 kHz sample rate so pw-play does no
// rate conversion on the evidence path.
const (
	toneFreq = 440.0
	toneDur  = 1.0
	toneRate = 48000
)

// playedMin/Max bound the observed playback length of a 1 s tone through
// the subprocess sink: well under means the stream died early (device
// error), well over would mean pacing is broken. pw-play's own startup and
// the sink's ~50 ms stream buffer make tight bounds noise, not signal.
const (
	playedMin = 500 * time.Millisecond
	playedMax = 2000 * time.Millisecond
)

func TestHWAudioOutput(t *testing.T) {
	hwGate(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sinks := listPipewireSinks(t)
	cards := listALSAcards(t)
	if len(sinks) == 0 && len(cards) == 0 {
		notRun(t, "no PipeWire sinks and no ALSA cards found",
			"attach a USB DAC/speaker to the hub or start the audio service "+
				"(systemctl --user start wireplumber; docs/spec/10), then re-run")
	}

	name, rate, ok := selectSink(sinks)
	if !ok {
		t.Fatalf("%s is not among the enumerated sinks %s: point it at one of those names "+
			"or unset it to play the first sink", EnvAudioDevice, sinkNames(sinks))
	}
	t.Logf("selected audio sink: %q (sample rate %s Hz; sinks: %s; ALSA cards: %s)",
		name, rate, sinkNames(sinks), cards)

	if os.Getenv(EnvAudioConsent) != "1" {
		notRun(t, "no consent to audible playback (docs/spec/13: playback is a consented action)",
			"export "+EnvAudioConsent+"=1 and re-run make test-hardware — a 1 s 440 Hz tone will play through sink "+
				quote(name))
	}

	if _, err := exec.LookPath("pw-play"); err != nil {
		notRun(t, "pw-play is not on PATH (the app's audio sink binary, internal/speech/pipewire)",
			"install pipewire (pacman -S pipewire / apt install pipewire) and re-run")
	}

	sink, err := pipewire.New(name)
	if err != nil {
		t.Fatalf("pipewire sink construction failed: %v", err)
	}
	defer func() { _ = sink.Close() }()

	h, err := sink.Play(ctx, speech.PlayRequest{
		UtteranceID: "hwsmoke-tone-440",
		Generation:  1,
		WAV:         speech.MakeToneWAV(toneFreq, toneDur, toneRate),
	})
	if err != nil {
		t.Fatalf("tone playback start failed: %v", err)
	}
	select {
	case res := <-h.Done():
		if res.Err != nil {
			t.Fatalf("tone playback failed through sink %q: %v", name, res.Err)
		}
		played := res.Ended.Sub(res.Started)
		if played < playedMin || played > playedMax {
			t.Fatalf("tone played %s; expected within [%s, %s] for a 1 s tone: "+
				"the stream ended early or ran long", played, playedMin, playedMax)
		}
		t.Logf("tone played %s through sink %q at the enumerated %s Hz (interrupted=%v)",
			roundDuration(played), name, rate, res.Interrupted)
	case <-ctx.Done():
		h.Stop(0)
		t.Fatalf("tone playback did not finish within the timeout: the sink is wedged")
	}
}

// pipewireSink is one row of `pactl list short sinks`:
//
//	2	auto_null	PipeWire	float32le	2ch	48000Hz	auto_null	RUNNING
//
// index / name / driver / sample format / channels / rate / name / state.
// The NAME column (not the index) is the stable identity the app plays to —
// "reselect by stable ID on reboot" (docs/spec/10) is what
// GAME_NARRATOR_AUDIO_DEVICE holds.
type pipewireSink struct {
	Name string
	Rate string // e.g. "48000" — kept as the tool printed it; recorded, not parsed
}

// listPipewireSinks runs the same pactl call internal/doctor uses, so the
// gate and the diagnostic agree about what the host has. A missing pactl or
// a failed call is evidence in the log, not an error: the ALSA listing
// below still runs, and "no sinks" is a real, expected host state.
func listPipewireSinks(t *testing.T) []pipewireSink {
	t.Helper()
	if _, err := exec.LookPath("pactl"); err != nil {
		t.Logf("pactl not on PATH: PipeWire sinks not enumerated (pacman -S pipewire-pulse)")
		return nil
	}
	out, err := exec.Command("pactl", "list", "short", "sinks").Output()
	if err != nil {
		t.Logf("pactl list short sinks failed (%v): no PipeWire sinks recorded", err)
		return nil
	}
	var sinks []pipewireSink
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 6 {
			continue // banner/blank: pactl prints nothing else in short mode
		}
		rate := strings.TrimSuffix(f[5], "Hz")
		if _, err := strconv.Atoi(rate); err != nil {
			continue // not a sink row
		}
		sinks = append(sinks, pipewireSink{Name: f[1], Rate: rate})
	}
	return sinks
}

// listALSAcards runs the aplay -l doctor uses: card indices only, so an
// ALSA-only host (no PipeWire) is still visible to the gate.
func listALSAcards(t *testing.T) []string {
	t.Helper()
	if _, err := exec.LookPath("aplay"); err != nil {
		t.Logf("aplay not on PATH: ALSA cards not enumerated (pacman -S alsa-utils)")
		return nil
	}
	out, err := exec.Command("aplay", "-l").Output()
	if err != nil {
		t.Logf("aplay -l failed (%v): no ALSA cards recorded", err)
		return nil
	}
	var cards []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "card ") {
			cards = append(cards, strings.Fields(line)[1])
		}
	}
	return cards
}

// selectSink applies the stable-identity rule: GAME_NARRATOR_AUDIO_DEVICE
// (the app's own env for the named sink) when set and present, else the
// first enumerated sink. A requested-but-missing sink is a misconfiguration
// the operator must fix, so the test fails hard with the found list — not
// a silent fall-through to a different speaker.
func selectSink(sinks []pipewireSink) (name, rate string, ok bool) {
	if len(sinks) == 0 {
		return "(none enumerated — ALSA-only host)", "unrecorded", true
	}
	if want := os.Getenv(EnvAudioDevice); want != "" {
		for _, s := range sinks {
			if s.Name == want {
				return s.Name, s.Rate, true
			}
		}
		return want, "", false
	}
	return sinks[0].Name, sinks[0].Rate, true
}

func sinkNames(sinks []pipewireSink) string {
	if len(sinks) == 0 {
		return "(none)"
	}
	names := make([]string, len(sinks))
	for i, s := range sinks {
		names[i] = s.Name + "@" + s.Rate + "Hz"
	}
	return strings.Join(names, ", ")
}

func quote(s string) string { return `"` + s + `"` }
