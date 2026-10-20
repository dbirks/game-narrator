package doctor

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// checkAudioDSP is the UNO Q audio-DSP row (issue #24 / ADR 0118): on a cold
// power-on the ADSP sometimes boots ('adsp is now up') but its APR services
// never register, so the deferred-probe chain (lpass pinctrl → soundwire →
// codecs → sound) never completes and the ArduinoImolaHPH card (hub HDMI
// audio, ANX7625 bridge) never appears. The kernel log carries the
// signature: the lpass pinctrl deferral
//
//	qcom-sm6115-lpass-lpi-pinctrl: Failed to get clk 'audio'
//
// (a healthy boot can log the same line transiently — the deferred probe
// then resolves and the card appears — which is why the row keys off the
// card's ABSENCE, with the signature as the cause). The proven fix is
// restarting the ADSP remoteproc once (echo stop/start on its state file);
// the package ships game-narrator-audiodsp-recover.service, which does that
// automatically once per boot (ADR 0118). This row is read-only: it reports
// the state and names the fix, it does not restart anything.

// unoQCardName is the ALSA card name the UNO Q sound card enumerates under
// (the bracketed name in /proc/asound/cards and in aplay -l — verified on
// the board, issue #24).
const unoQCardName = "ArduinoImolaHPH"

// audiodspSignature is the dmesg line of the deferred probe that hangs when
// the ADSP's APR services never register (issue #24).
const audiodspSignature = "Failed to get clk 'audio'"

// audiodspRecoverUnit is the oneshot that performs the bounded ADSP restart
// (ADR 0118): it is the remedy this row names, never something it does.
const audiodspRecoverUnit = "game-narrator-audiodsp-recover.service"

func (r *runner) checkAudioDSP(ctx context.Context) Check {
	procRoot := r.opts.ProcRoot
	if procRoot == "" {
		procRoot = "/proc"
	}

	// 1. UNO Q detection: /proc/device-tree/compatible is NUL-separated and
	// carries "arduino,imola" on the board (verified read-only on the real
	// board, issue #24). Off-board hosts are NOT CONFIGURED, not BLOCKED —
	// the row applies to the appliance only (same contract as
	// audio.hdmi_bridge).
	compat, err := os.ReadFile(procRoot + "/device-tree/compatible")
	if err != nil || !strings.Contains(string(compat), "arduino,imola") {
		return Check{
			Status:  StatusNotConfigured,
			Summary: "not a UNO Q (no arduino,imola in /proc/device-tree/compatible)",
			Detail:  "this row applies on the UNO Q only: there is no built-in sound card to check",
		}
	}

	// 2. Card presence from /proc/asound/cards (the same source the
	// package's recover unit reads, so the doctor and the fix agree on
	// evidence). The card line's second field is the bracketed card name:
	// "  1 [ArduinoImolaHPH]: qcm2290 - Arduino-Imola-HPH-LOUT".
	cards, err := os.ReadFile(procRoot + "/asound/cards")
	if err != nil {
		return Check{
			Status:  StatusBlocked,
			Summary: "UNO Q sound card state unreadable",
			Detail:  fmt.Sprintf("reading %s/asound/cards failed: %v", procRoot, err),
			Remedy:  "check the ALSA install, then re-run the doctor",
		}
	}
	if line, ok := procAsoundCardLine(cards, unoQCardName); ok {
		return Check{
			Status:  StatusOK,
			Summary: fmt.Sprintf("UNO Q sound card %s is present", unoQCardName),
			Detail:  fmt.Sprintf("/proc/asound/cards: %s", strings.TrimSpace(line)),
		}
	}

	// 3. Card absent: look for the deferred-probe signature in dmesg. The
	// read-only contract holds — dmesg is a read; the fix (the ADSP
	// restart) is named, not performed.
	res := r.opts.Exec(ctx, "dmesg")
	if res.Err != nil {
		return Check{
			Status:  StatusBlocked,
			Summary: "UNO Q sound card " + unoQCardName + " is absent; cause unverified (dmesg unreadable)",
			Detail:  fmt.Sprintf("dmesg failed: %v — the deferred-probe signature (%s) could not be checked", oneLine(fmt.Sprintf("%v", res.Err), 160), audiodspSignature),
			Remedy:  "re-run as root (sudo game-narrator doctor), or run " + audiodspRecoverUnit + " and re-check; reboot if it persists",
		}
	}
	if strings.Contains(string(res.Stdout), audiodspSignature) {
		return Check{
			Status:  StatusFail,
			Summary: "UNO Q sound card " + unoQCardName + " is absent: the ADSP's APR services never registered",
			Detail:  fmt.Sprintf("dmesg carries the deferred-probe signature (%s) with no card in /proc/asound/cards: the ADSP booted but its APR services never registered, so the lpass pinctrl → soundwire → codecs → sound probe chain stayed deferred (issue #24). Likely a cold-boot race; healthy boots register the services ~1 s after the ADSP comes up.", audiodspSignature),
			Remedy:  "sudo systemctl start " + audiodspRecoverUnit + " (the package's oneshot restarts the ADSP remoteproc once and waits up to 20 s for the card); manual: echo stop/start on /sys/class/remoteproc/<adsp>/state, then re-run the doctor",
		}
	}
	return Check{
		Status:  StatusBlocked,
		Summary: "UNO Q sound card " + unoQCardName + " is absent (no deferred-probe signature)",
		Detail:  fmt.Sprintf("/proc/asound/cards has no %s card and dmesg has no %s line: a different fault (bridge, kernel, or the ADSP itself) — get a fresh dmesg and reboot before restarting the ADSP again", unoQCardName, audiodspSignature),
		Remedy:  "check dmesg for the lpass deferral chain, then reboot; if the signature appears, run " + audiodspRecoverUnit,
	}
}

// procAsoundCardLine reports the /proc/asound/cards line of the card with the
// bracketed name. Lines look like:
//
//	0 [Video          ]: USB-Audio - USB3.0 Video
//	1 [ArduinoImolaHPH]: qcm2290 - Arduino-Imola-HPH-LOUT
func procAsoundCardLine(out []byte, card string) (string, bool) {
	for _, line := range strings.Split(string(out), "\n") {
		t := strings.TrimSpace(line)
		// The card name is the bracketed field: "N [Name] : ...".
		lo, hi := strings.IndexByte(t, '['), strings.IndexByte(t, ']')
		if lo < 0 || hi <= lo {
			continue
		}
		if strings.TrimSpace(t[lo+1:hi]) == card {
			return t, true
		}
	}
	return "", false
}
