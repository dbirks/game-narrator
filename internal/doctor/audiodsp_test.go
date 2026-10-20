package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- audio.dsp (issue #24 / ADR 0118) ---------------------------------------
//
// The row reports the UNO Q built-in sound card (ArduinoImolaHPH) read-only:
// OK when the card is in /proc/asound/cards; FAIL when it is absent and
// dmesg carries the lpass deferred-probe signature (the ADSP's APR services
// never registered); NOT CONFIGURED off the UNO Q. The fake host below points
// ProcRoot at a fabricated /proc tree (device-tree compatible + asound/cards)
// and answers the dmesg command with recorded board-shaped output — the suite
// never depends on the host's device tree (AGENTS.md rule 3).

// fakeProcRoot builds a fabricated /proc tree: UNO Q (or not), with or
// without the ArduinoImolaHPH card line.
func fakeProcRoot(t *testing.T, unoQ bool, cardLine string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "device-tree"), 0o755); err != nil {
		t.Fatal(err)
	}
	compat := "qcom,qcm2290\x00qcom,qrb2210\x00"
	if unoQ {
		compat = "arduino,imola\x00" + compat
	}
	if err := os.WriteFile(filepath.Join(root, "device-tree", "compatible"), []byte(compat), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "asound"), 0o755); err != nil {
		t.Fatal(err)
	}
	cards := "  0 [Video          ]: USB-Audio - USB3.0 Video\n"
	if cardLine != "" {
		cards += cardLine + "\n"
	}
	if err := os.WriteFile(filepath.Join(root, "asound", "cards"), []byte(cards), 0o444); err != nil {
		t.Fatal(err)
	}
	return root
}

// dmesgDeferral is the board's real signature line (issue #24), with a
// healthy-boot neighbour line to prove the match is not a bare "clk" hit.
const dmesgDeferral = "[   41.997310] remoteproc remoteproc1: adsp is now up\n" +
	"[   42.020665] platform a7c0000.pinctrl: deferred probe pending: qcom-sm6115-lpass-lpi-pinctrl: Failed to get clk 'audio'\n" +
	"[   42.031102] soundwire qcom-swrm: probe deferral: clock audio not ready\n"

const dmesgHealthy = "[    2.411002] remoteproc remoteproc1: adsp is now up\n" +
	"[    3.401555] glink-edge glink-edge.-16.-16: Adding APR/GPR dev: aprsvc:service:4:1\n"

func TestAudioDSPNotConfiguredOffBoard(t *testing.T) {
	// A non-UNO Q host (no arduino,imola in compatible): NOT CONFIGURED, and
	// the read-only contract — dmesg is never even probed.
	h := newFakeHost(t)
	h.opts.ProcRoot = fakeProcRoot(t, false, "")
	c := h.check(h.run(), "audio.dsp")
	if c.Status != StatusNotConfigured {
		t.Fatalf("status = %q, want NOT CONFIGURED: %+v", c.Status, c)
	}
	if c.Group != "Audio" {
		t.Fatalf("group = %q, want Audio", c.Group)
	}
	if h.calledWith("dmesg") {
		t.Fatal("off-board the row must not probe dmesg (no card to diagnose):", h.invoked())
	}
}

func TestAudioDSPOKWhenCardPresent(t *testing.T) {
	// The healthy board: the card line is in /proc/asound/cards. OK, and the
	// dmesg signature (present even on healthy boots, transiently) must not
	// matter — the card is the evidence.
	h := newFakeHost(t)
	h.opts.ProcRoot = fakeProcRoot(t, true, "  1 [ArduinoImolaHPH]: qcm2290 - Arduino-Imola-HPH-LOUT")
	h.exec("dmesg", dmesgDeferral)
	c := h.check(h.run(), "audio.dsp")
	if c.Status != StatusOK {
		t.Fatalf("status = %q, want OK: %+v", c.Status, c)
	}
	if !strings.Contains(c.Summary, "ArduinoImolaHPH") {
		t.Fatalf("summary = %q, want the card name named", c.Summary)
	}
	if !strings.Contains(c.Detail, "ArduinoImolaHPH") {
		t.Fatalf("detail = %q, want the /proc/asound/cards evidence line", c.Detail)
	}
}

func TestAudioDSPFailOnDeferralSignature(t *testing.T) {
	// The issue #24 fault: cold boot, card absent, the lpass deferral line in
	// dmesg. FAIL with the cause (APR never registered) and the shipped fix
	// (the recover oneshot) named.
	h := newFakeHost(t)
	h.opts.ProcRoot = fakeProcRoot(t, true, "")
	h.exec("dmesg", dmesgDeferral)
	c := h.check(h.run(), "audio.dsp")
	if c.Status != StatusFail {
		t.Fatalf("status = %q, want FAIL: %+v", c.Status, c)
	}
	if !strings.Contains(c.Detail, "Failed to get clk 'audio'") {
		t.Fatalf("detail = %q, want the deferred-probe signature quoted", c.Detail)
	}
	if !strings.Contains(c.Detail, "APR") {
		t.Fatalf("detail = %q, want the cause (APR services never registered)", c.Detail)
	}
	if !strings.Contains(c.Remedy, "game-narrator-audiodsp-recover.service") {
		t.Fatalf("remedy = %q, want the shipped recover oneshot named", c.Remedy)
	}
}

func TestAudioDSPBlockedWithoutSignature(t *testing.T) {
	// Card absent but no signature (a healthy-boot dmesg, or the signature
	// scrolled away): a different fault — BLOCKED, not the APR FAIL, and the
	// remedy steers to dmesg + reboot, not a blind ADSP restart.
	h := newFakeHost(t)
	h.opts.ProcRoot = fakeProcRoot(t, true, "")
	h.exec("dmesg", dmesgHealthy)
	c := h.check(h.run(), "audio.dsp")
	if c.Status != StatusBlocked {
		t.Fatalf("status = %q, want BLOCKED: %+v", c.Status, c)
	}
	if !strings.Contains(c.Remedy, "dmesg") {
		t.Fatalf("remedy = %q, want it to steer to dmesg", c.Remedy)
	}
}

func TestAudioDSPBlockedWhenDmesgUnreadable(t *testing.T) {
	// Card absent and dmesg refuses (kernel.dmesg_restrict, unprivileged
	// user): the cause is unverified — BLOCKED, not a guessed FAIL.
	h := newFakeHost(t)
	h.opts.ProcRoot = fakeProcRoot(t, true, "")
	h.execFail("dmesg", "dmesg: read kernel buffer failed: Operation not permitted")
	c := h.check(h.run(), "audio.dsp")
	if c.Status != StatusBlocked {
		t.Fatalf("status = %q, want BLOCKED: %+v", c.Status, c)
	}
	if !strings.Contains(c.Summary, "dmesg unreadable") {
		t.Fatalf("summary = %q, want the unreadable-dmesg condition named", c.Summary)
	}
}
