package hwsmoke

// TestHWALSAHubHDMI is the hardware gate of issue #5: the alsahw sink
// (internal/speech/alsahw) plays a 1 s 440 Hz tone into the UNO Q's hub
// HDMI device (hw:ArduinoImolaHPH,3 — the ANX7625 bridge's out) at the
// profile's exact proven hw_params, and the gate asserts the DMA consumed
// it: the hardware pointer (hw_ptr) advanced while the tone played.
//
// The assertion is the ELD question in one measurement: a receiver that is
// not locked (no ELD) does not clock the DMA — the kernel accepts the
// writes into the 520 ms buffer and then the stream stalls, and hw_ptr sits
// still. A pointer that advances in real time is the machine-readable form
// of "the receiver heard it." The hw_params the kernel negotiated are
// recorded from /proc and must match the proven config, byte for byte —
// the same comparison the skill reference (skills/uno-q-setup/references/
// audio.md) runs against aplay's output.
//
// Gate and consent follow the package contract: NOT RUN without
// GAME_NARRATOR_HW_SMOKE=1, and the audible tone only with
// GAME_NARRATOR_HW_AUDIO_CONSENT=1. On a host without the ArduinoImolaHPH
// card the test is NOT RUN with the board instruction — it is the UNO Q's
// row, like the doctor's audio.hdmi_bridge.

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/audio"
	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/speech/alsahw"
)

// hwPtrFloor is the minimum hw_ptr advance a 1 s tone must produce: the DMA
// must have consumed at least this much while the tone played. 0.25 s of
// the stream (12000 frames of 48 kHz) is far below the tone's 48000 frames,
// so a live receiver always clears it, while a stalled stream (no ELD)
// advances little or nothing.
const hwPtrFloor = 12000

// aplayLCards parses `aplay -l` into card name → card index: the /proc
// paths are indexed by the numeric index, the identity by the name.
// Listing line: "card 1: ArduinoImolaHPH [UNO Q], device 0: …" — the
// fields are card / "1:" / name / [desc…].
func aplayLCards(t *testing.T) map[string]string {
	t.Helper()
	out, err := exec.Command("aplay", "-l").Output()
	if err != nil {
		t.Fatalf("aplay -l failed: %v", err)
	}
	byName := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || f[0] != "card" || !strings.HasSuffix(f[1], ":") {
			continue
		}
		byName[f[2]] = strings.TrimSuffix(f[1], ":")
	}
	return byName
}

// procHWPtr reads the subdevice's hardware pointer (frames consumed by the
// DMA) from /proc/asound/card<idx>/pcm<dev>p/sub0/hw_ptr. A missing file
// (the subdevice not yet instantiated) reports (0, false): the caller logs
// it and measures the advance from zero — the after-read still proves
// consumption.
func procHWPtr(t *testing.T, cardIdx, dev string) (int64, bool) {
	t.Helper()
	path := "/proc/asound/card" + cardIdx + "/pcm" + dev + "p/sub0/hw_ptr"
	b, err := os.ReadFile(path)
	if err != nil {
		t.Logf("%s not present before the tone (the subdevice instantiates on first open)", path)
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		t.Fatalf("parsing %s (%q): %v", path, strings.TrimSpace(string(b)), err)
	}
	return n, true
}

// procHWParams reads the negotiated hw_params the kernel reports for the
// subdevice (the same file the skill reference compares against aplay).
func procHWParams(t *testing.T, cardIdx, dev string) string {
	t.Helper()
	path := "/proc/asound/card" + cardIdx + "/pcm" + dev + "p/sub0/hw_params"
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return strings.TrimSpace(string(b))
}

func TestHWALSAHubHDMI(t *testing.T) {
	hwGate(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if _, err := exec.LookPath("aplay"); err != nil {
		notRun(t, "aplay is not on PATH (the card-presence probe)",
			"pacman -S alsa-utils and re-run make test-hardware")
	}
	byName := aplayLCards(t)
	prof := audio.UNOQHubHDMI
	cardIdx, ok := byName[prof.CardName]
	if !ok {
		notRun(t, "the "+prof.CardName+" card is not present (aplay -l)",
			"this check is the UNO Q's hub-HDMI row (hw:ArduinoImolaHPH,3, the ANX7625 bridge): "+
				"run it on the board — scripts/hw/run-on-device.sh — with an AV receiver on the hub HDMI")
	}
	t.Logf("card %s is %s (card index %s); device %d", prof.CardName, prof.DeviceID(), cardIdx, prof.DeviceNumber)

	if os.Getenv(EnvAudioConsent) != "1" {
		notRun(t, "no consent to audible playback (docs/spec/13: playback is a consented action)",
			"export "+EnvAudioConsent+"=1 and re-run make test-hardware — a 1 s 440 Hz tone will play "+
				"into the hub HDMI (receiver volume as usual)")
	}

	node := "/dev/snd/pcmC" + prof.CardName + "D" + strconv.Itoa(prof.DeviceNumber) + "p"
	if _, err := os.Stat(node); err != nil {
		notRun(t, "the device node "+node+" is not present",
			"the card enumerates but subdevice "+strconv.Itoa(prof.DeviceNumber)+" is not open: check the hub HDMI link (receiver powered, ELD)")
	}

	before, _ := procHWPtr(t, cardIdx, strconv.Itoa(prof.DeviceNumber))
	t.Logf("hw_ptr before the tone: %d", before)

	sink, err := alsahw.New(prof.DeviceID(), alsahw.Params{
		Format:      prof.Format,
		Rate:        prof.Rate,
		Channels:    prof.Channels,
		PeriodSize:  uint32(prof.PeriodSize),
		PeriodCount: uint32(prof.PeriodCount),
	}, 0) // no keep-alive: the tone itself is the stream for this check
	if err != nil {
		t.Fatalf("alsahw sink construction failed: %v", err)
	}
	defer func() { _ = sink.Close() }()

	// The tone at the device's own rate (48 kHz): the sink's converter is
	// the 1→2 channel duplicate and a rate no-op, so the evidence stream is
	// exactly the proven hw_params.
	h, err := sink.Play(ctx, speech.PlayRequest{
		UtteranceID: "hwsmoke-hdmi-440",
		Generation:  1,
		WAV:         speech.MakeToneWAV(440, 1.0, prof.Rate),
		Gain:        1.0,
	})
	if err != nil {
		t.Fatalf("tone playback start failed (the hw_params negotiation is the suspect: %v)", err)
	}
	select {
	case res := <-h.Done():
		if res.Err != nil {
			t.Fatalf("tone playback failed: %v (check the mixer route: doctor's audio.hdmi_bridge row)", res.Err)
		}
	case <-ctx.Done():
		h.Stop(0)
		t.Fatal("tone playback did not finish within the timeout: the device is wedged")
	}

	after, ok := procHWPtr(t, cardIdx, strconv.Itoa(prof.DeviceNumber))
	if !ok {
		t.Fatal("hw_ptr is unreadable after the tone: the subdevice did not instantiate (the open must have failed)")
	}
	advanced := after - before
	if advanced < 0 {
		// The pointer is monotonic per stream; a decrease means the stream
		// was re-prepared (an xrun) and the counter restarted — the
		// underrun count carries the evidence.
		t.Fatalf("hw_ptr went backwards (%d → %d): the stream restarted; underruns=%d",
			before, after, sink.Underruns())
	}
	t.Logf("hw_ptr after the tone: %d (advanced %d of the 48000 frames the tone carries; underruns=%d)",
		after, advanced, sink.Underruns())
	if advanced < hwPtrFloor {
		t.Fatalf("hw_ptr advanced only %d frames (< %d): the DMA is not consuming in real time — "+
			"the receiver is not locked (check the hub HDMI link and the receiver's ELD; the tone may have been silent)",
			advanced, hwPtrFloor)
	}

	// The negotiated hw_params must be the proven config (the kernel's own
	// record, compared the way the skill reference compares it).
	hp := procHWParams(t, cardIdx, strconv.Itoa(prof.DeviceNumber))
	t.Logf("negotiated hw_params:\n%s", hp)
	for _, want := range []string{
		"RW_INTERLEAVED", "S16_LE", "2ch", "48000",
		"period_size 6240", "buffer_size 24960",
	} {
		if !strings.Contains(hp, want) {
			t.Fatalf("negotiated hw_params %q, want it to carry %q — the device did not take the proven config:\n%s",
				want, want, hp)
		}
	}
	t.Logf("hub HDMI gate: 1 s 440 Hz tone delivered, hw_ptr advanced %d frames, hw_params match the proven config", advanced)
}
