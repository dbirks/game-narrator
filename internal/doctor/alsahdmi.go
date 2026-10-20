package doctor

import (
	"context"
	"fmt"
	"strings"

	"github.com/dbirks/game-narrator/internal/audio"
)

// checkHDMIBridge is the UNO Q hub-HDMI mixer-route row (issue #5): the
// narrator's hub HDMI output (ANX7625 bridge, the alsahw profile device)
// audibly plays only on the default ALSA route — MultiMedia4 → SEC_MI2S_RX
// → ANX7625 → USB-C DP → hub HDMI. The mixer evidence for that route is two
// switches on the ArduinoImolaHPH card:
//
//	SEC_MI2S_RX Audio Mixer MultiMedia4   must be ON  (the route that works)
//	DISPLAY_PORT_RX Audio Mixer MultiMedia4 must be OFF (the DisplayPort
//	  route is a dead end: no DP backend dai-link in the qcm2290 device
//	  tree, and enabling it gives q6asm_dai_prepare: ret:-22).
//
// The probe is read-only (amixer get): the doctor reports the exact amixer
// command that fixes a misroute, it does not apply it. The row exists to
// catch the one silent-but-present failure mode of the hub HDMI: a board
// whose mixer got re-routed (by an operator or a package) still enumerates
// the card, still answers the tone endpoint's open, and plays nothing.

// checkHDMIBridge probes the hub-HDMI mixer route (see the file's comment
// for the route contract).
func (r *runner) checkHDMIBridge(ctx context.Context) Check {
	prof := audio.UNOQHubHDMI
	card := prof.CardName
	secCtrl := "SEC_MI2S_RX Audio Mixer MultiMedia4"
	dpCtrl := "DISPLAY_PORT_RX Audio Mixer MultiMedia4"

	// 1. Card presence through the same backend the audio.sinks row uses:
	// aplay -l carries the stable card names. No aplay — the route cannot
	// be probed at all (the audio.sinks row names the missing backend).
	if _, err := r.opts.LookPath("aplay"); err != nil {
		return Check{
			Status:  StatusBlocked,
			Summary: "hub HDMI route cannot be probed (no aplay)",
			Detail:  "aplay is not on PATH; the ALSA card list is the card-presence evidence this row needs",
			Remedy:  "pacman -S alsa-utils, then re-run the doctor",
		}
	}
	res := r.opts.Exec(ctx, "aplay", "-l")
	if res.Err != nil {
		return Check{
			Status:  StatusBlocked,
			Summary: "hub HDMI route cannot be probed (aplay -l failed)",
			Detail:  oneLine(fmt.Sprintf("%v", res.Err), 200),
			Remedy:  "check the ALSA installation, then re-run the doctor",
		}
	}
	if !unoQCardPresent(res.Stdout, card) {
		return Check{
			Status:  StatusNotConfigured,
			Summary: "no UNO Q hub HDMI card (ArduinoImolaHPH)",
			Detail:  "this row applies on the UNO Q only: without the card there is no hub HDMI route to check",
		}
	}

	// 2. The mixer route. amixer is the read-only probe; without it the
	// route is present-but-unverifiable — DEGRADED, with the manual command.
	if _, err := r.opts.LookPath("amixer"); err != nil {
		return Check{
			Status:  StatusDegraded,
			Summary: "hub HDMI card present; mixer route unverified (no amixer)",
			Detail:  fmt.Sprintf("card %s is present but amixer is not on PATH; the mixer route cannot be read", card),
			Remedy:  "pacman -S alsa-utils, or check manually: " + amixerGet(card, secCtrl),
		}
	}
	sec, serr := mixerSwitch(ctx, r, card, secCtrl)
	if serr != nil {
		return degradedHDMIRoute(card, secCtrl, serr)
	}
	dp, derr := mixerSwitch(ctx, r, card, dpCtrl)
	if derr != nil {
		// The primary route is the audible one; a missing secondary control
		// (a card revision without the DP switch) degrades rather than
		// fails, with the evidence.
		return Check{
			Status:  StatusOK,
			Summary: "hub HDMI route is set for the ANX7625 bridge (SEC_MI2S_RX on)",
			Detail:  fmt.Sprintf("%s = on; %s could not be read (%v) — the primary route is the audible one", secCtrl, dpCtrl, derr),
		}
	}

	if !sec {
		return Check{
			Status:  StatusFail,
			Summary: "the hub HDMI route is misrouted: SEC_MI2S_RX ← MultiMedia4 is off",
			Detail:  fmt.Sprintf("amixer -c %s get '%s' reports off: the ANX7625 bridge receives nothing and the receiver stays silent (the card still enumerates, so the device row looks healthy)", card, secCtrl),
			Remedy:  amixerSet(card, secCtrl, "on") + "; the DisplayPort route must stay off: " + amixerSet(card, dpCtrl, "off"),
		}
	}
	if dp {
		return Check{
			Status:  StatusFail,
			Summary: "the DisplayPort route is enabled: a dead-end dai-link on the qcm2290",
			Detail:  fmt.Sprintf("amixer -c %s get '%s' reports on: the qcm2290 device tree has no DisplayPort backend dai-link (q6asm_dai_prepare: ret:-22 when it is armed), and the proven audible route is the SEC_MI2S_RX one", card, dpCtrl),
			Remedy:  amixerSet(card, dpCtrl, "off"),
		}
	}
	return Check{
		Status:  StatusOK,
		Summary: "hub HDMI route is set for the ANX7625 bridge (SEC_MI2S_RX on, DISPLAY_PORT_RX off)",
		Detail:  fmt.Sprintf("amixer -c %s: '%s' = on, '%s' = off — the proven MultiMedia4 → SEC_MI2S_RX → ANX7625 → USB-C DP → hub HDMI route", card, secCtrl, dpCtrl),
	}
}

// unoQCardPresent reports whether the aplay -l listing carries the named
// card. The listing is one line per card: "card N: <name> [<description>]"
// (names never contain spaces; the description may).
func unoQCardPresent(out []byte, card string) bool {
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "card ") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line, "card "))
		if i := strings.IndexByte(rest, ':'); i >= 0 {
			rest = strings.TrimSpace(rest[i+1:])
		}
		if rest == card || strings.HasPrefix(rest, card+" [") || strings.HasPrefix(rest, card+" ") {
			return true
		}
	}
	return false
}

// mixerSwitch reads one playback switch through amixer (read-only: `get`).
// It answers (state, nil) when the output carries the switch state, and an
// error naming the condition otherwise (control missing, tool failure,
// unparseable output).
func mixerSwitch(ctx context.Context, r *runner, card, control string) (bool, error) {
	res := r.opts.Exec(ctx, "amixer", "-c", card, "get", control)
	if res.Err != nil {
		// amixer exits 2 when the control does not exist on the card.
		return false, fmt.Errorf("%v", res.Err)
	}
	out := string(res.Stdout)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Playback Switch:") {
			continue
		}
		switch strings.ToLower(switchState(line)) {
		case "on":
			return true, nil
		case "off":
			return false, nil
		}
	}
	return false, fmt.Errorf("no 'Playback Switch' state in the output: %s", oneLine(out, 160))
}

// switchState extracts the bracketed state of a "… Switch: [on]" line.
func switchState(line string) string {
	i := strings.LastIndex(line, "[")
	if i < 0 || !strings.HasSuffix(line, "]") {
		return ""
	}
	return strings.TrimSpace(line[i+1 : len(line)-1])
}

// amixerGet / amixerSet render the exact commands the row names: the
// doctor is read-only, so the remedy is the command the operator runs.
func amixerGet(card, control string) string {
	return fmt.Sprintf("amixer -c %s get '%s'", card, control)
}

func amixerSet(card, control, state string) string {
	return fmt.Sprintf("amixer -c %s set '%s' %s", card, control, state)
}

func degradedHDMIRoute(card, control string, err error) Check {
	return Check{
		Status:  StatusDegraded,
		Summary: "hub HDMI card present; mixer route unverified",
		Detail:  fmt.Sprintf("reading '%s' on card %s failed: %v", control, card, err),
		Remedy:  "check the route manually with " + amixerGet(card, control) + " (it must report on)",
	}
}
