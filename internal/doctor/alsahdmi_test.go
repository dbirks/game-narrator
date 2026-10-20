package doctor

import (
	"errors"
	"strings"
	"testing"
)

// --- audio.hdmi_bridge (issue #5 / ADR-0111) --------------------------------
//
// The row probes the UNO Q hub-HDMI mixer route read-only: card presence
// (aplay -l) plus the two switches the audible route needs
// (SEC_MI2S_RX ← MultiMedia4 on, DISPLAY_PORT_RX ← MultiMedia4 off). The
// fake amixer below is the board's real output shape (a pswitch control
// reports a single "Playback Switch: [on|off]" line).

// withUNOQCard points the host at a board whose aplay -l carries the
// ArduinoImolaHPH card (card 1, the way the board numbers it) and makes
// amixer available.
func (h *fakeHost) withUNOQCard() {
	h.mu.Lock()
	h.tools["amixer"] = true
	h.answers["aplay -l"] = CommandResult{Stdout: []byte(
		"card 0: Dummy [Dummy], device 0: Dummy HDMI [Dummy HDMI]\n" +
			"  Subdevices: 1/1\n" +
			"card 1: ArduinoImolaHPH [UNO Q], device 0: SEC MI2S TX-1 HiFi sec-mi2s-tx-1-port [SEC MI2S TX-1]\n" +
			"  Subdevices: 1/1\n",
	)}
	h.mu.Unlock()
}

// withUNOQMixers gives the fake amixer answers for the two route switches.
func (h *fakeHost) withUNOQMixers(sec, dp string) {
	const card = "ArduinoImolaHPH"
	h.exec("amixer -c "+card+" get SEC_MI2S_RX Audio Mixer MultiMedia4",
		"Simple mixer control 'SEC_MI2S_RX Audio Mixer MultiMedia4',0\n"+
			"  Capabilities: pswitch\n"+
			"  Playback Channels: [0-0]\n"+
			"  Mono:\n"+
			"  Playback Switch: ["+sec+"]\n")
	h.exec("amixer -c "+card+" get DISPLAY_PORT_RX Audio Mixer MultiMedia4",
		"Simple mixer control 'DISPLAY_PORT_RX Audio Mixer MultiMedia4',0\n"+
			"  Capabilities: pswitch\n"+
			"  Playback Channels: [0-0]\n"+
			"  Mono:\n"+
			"  Playback Switch: ["+dp+"]\n")
}

func TestHDMIBridgeNotConfiguredOffBoard(t *testing.T) {
	// The default host (a Dummy card) has no UNO Q: the row is NOT
	// CONFIGURED, and — the read-only contract — nothing beyond the card
	// listing is probed (no amixer calls at all).
	h := newFakeHost(t)
	c := h.check(h.run(), "audio.hdmi_bridge")
	if c.Status != StatusNotConfigured {
		t.Fatalf("status = %q, want NOT CONFIGURED: %+v", c.Status, c)
	}
	if c.Group != "Audio" {
		t.Fatalf("group = %q, want Audio", c.Group)
	}
	for _, call := range h.calls {
		if strings.HasPrefix(call, "amixer") {
			t.Fatalf("off-board host got an amixer probe: %q", call)
		}
	}
}

func TestHDMIBridgeOKRoute(t *testing.T) {
	h := newFakeHost(t)
	h.withUNOQCard()
	h.withUNOQMixers("on", "off")
	c := h.check(h.run(), "audio.hdmi_bridge")
	if c.Status != StatusOK {
		t.Fatalf("status = %q, want OK: %+v", c.Status, c)
	}
	if c.Remedy != "" {
		t.Fatalf("an OK route carries no remedy: %q", c.Remedy)
	}
}

func TestHDMIBridgeMisrouteSecOff(t *testing.T) {
	h := newFakeHost(t)
	h.withUNOQCard()
	h.withUNOQMixers("off", "off")
	c := h.check(h.run(), "audio.hdmi_bridge")
	if c.Status != StatusFail {
		t.Fatalf("status = %q, want FAIL: %+v", c.Status, c)
	}
	// The remedy is the EXACT amixer command (read-only doctor: it names
	// the fix, it does not apply it).
	want := "amixer -c ArduinoImolaHPH set 'SEC_MI2S_RX Audio Mixer MultiMedia4' on"
	if !strings.Contains(c.Remedy, want) {
		t.Fatalf("remedy %q does not name the exact fix %q", c.Remedy, want)
	}
	if !strings.Contains(c.Remedy, "amixer -c ArduinoImolaHPH set 'DISPLAY_PORT_RX Audio Mixer MultiMedia4' off") {
		t.Fatalf("remedy %q must keep the dead-end DisplayPort route off", c.Remedy)
	}
}

func TestHDMIBridgeMisrouteDPOn(t *testing.T) {
	h := newFakeHost(t)
	h.withUNOQCard()
	h.withUNOQMixers("on", "on")
	c := h.check(h.run(), "audio.hdmi_bridge")
	if c.Status != StatusFail {
		t.Fatalf("status = %q, want FAIL (the DisplayPort route is a dead end): %+v", c.Status, c)
	}
	want := "amixer -c ArduinoImolaHPH set 'DISPLAY_PORT_RX Audio Mixer MultiMedia4' off"
	if !strings.Contains(c.Remedy, want) {
		t.Fatalf("remedy %q does not name the exact fix %q", c.Remedy, want)
	}
}

func TestHDMIBridgeNoAmixer(t *testing.T) {
	h := newFakeHost(t)
	h.mu.Lock()
	h.answers["aplay -l"] = CommandResult{Stdout: []byte(
		"card 1: ArduinoImolaHPH [UNO Q], device 0: ...\n  Subdevices: 1/1\n")}
	h.mu.Unlock() // amixer stays off PATH
	c := h.check(h.run(), "audio.hdmi_bridge")
	if c.Status != StatusDegraded {
		t.Fatalf("status = %q, want DEGRADED (present but unverified): %+v", c.Status, c)
	}
	if !strings.Contains(c.Remedy, "amixer -c ArduinoImolaHPH get 'SEC_MI2S_RX Audio Mixer MultiMedia4'") {
		t.Fatalf("remedy %q must name the manual read command", c.Remedy)
	}
}

func TestHDMIBridgeControlMissing(t *testing.T) {
	// A card revision whose mixer does not carry the control: amixer exits
	// non-zero naming the control. The route is present-but-unverified:
	// DEGRADED, not FAIL (a missing control is not evidence of a misroute).
	h := newFakeHost(t)
	h.withUNOQCard()
	h.execResult("amixer -c ArduinoImolaHPH get SEC_MI2S_RX Audio Mixer MultiMedia4",
		CommandResult{Stderr: []byte("amixer: MIXER_CTRL not found.\n"), Err: errors.New("exit status 2"), ExitCode: 2})
	c := h.check(h.run(), "audio.hdmi_bridge")
	if c.Status != StatusDegraded {
		t.Fatalf("status = %q, want DEGRADED: %+v", c.Status, c)
	}
	if strings.Contains(c.Remedy, "set") {
		t.Fatalf("a missing control must not prescribe a set command: %q", c.Remedy)
	}
}

func TestHDMIBridgeNoAplay(t *testing.T) {
	h := newFakeHost(t)
	h.withoutTools("aplay")
	c := h.check(h.run(), "audio.hdmi_bridge")
	if c.Status != StatusBlocked {
		t.Fatalf("status = %q, want BLOCKED: %+v", c.Status, c)
	}
}
