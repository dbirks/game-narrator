package audio

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// listPipeWire enumerates the PipeWire/PulseAudio sinks through pactl (the
// same tool the doctor and the hwsmoke gate use, so all three surfaces agree
// about what the host has). The stable identity is the sink NAME (docs/spec/
// 10: reselect by stable ID on reboot — the numeric index is exactly what a
// reboot renumbers).
//
// Two calls: `pactl list sinks` (the long form, which carries the
// properties the kind inference needs — factory.name, device.name,
// device.description) and `pactl info` (the platform's default sink, which
// the UI marks rather than the app choosing: choosing is the user's act).
func listPipeWire(ctx context.Context, pactlPath string) (List, error) {
	long, err := runCmd(ctx, pactlPath, "list", "sinks")
	if err != nil {
		return List{}, err
	}
	defaultSink := ""
	if info, ierr := runCmd(ctx, pactlPath, "info"); ierr == nil {
		defaultSink = parseDefaultSink(info)
	}

	var devices []Device
	for _, blk := range splitSinks(long) {
		d, ok := parseSinkBlock(blk)
		if !ok {
			continue
		}
		d.PlatformDefault = defaultSink != "" && d.ID == defaultSink
		devices = append(devices, d)
	}
	if len(devices) == 0 {
		return List{}, fmt.Errorf("no sinks in `pactl list sinks` output (is the audio service running?)")
	}
	return List{Backend: "pipewire", Devices: devices}, nil
}

// splitSinks cuts the long listing into one block per "Sink #N" header.
func splitSinks(out string) []string {
	var blocks []string
	var cur []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "Sink #") {
			if len(cur) > 0 {
				blocks = append(blocks, strings.Join(cur, "\n"))
			}
			cur = []string{line}
			continue
		}
		if len(cur) > 0 {
			cur = append(cur, line)
		}
	}
	if len(cur) > 0 {
		blocks = append(blocks, strings.Join(cur, "\n"))
	}
	return blocks
}

// parseSinkBlock reads one sink block: the Name: line is the ID, the
// Description: line the display name, the Sample Specification line the
// rate/channels, and the Properties: section the kind evidence.
func parseSinkBlock(block string) (Device, bool) {
	var d Device
	kindEvidence := []string{} // factory.name / device.name / device.description values
	inProps := false
	for _, line := range strings.Split(block, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "Name:"):
			d.ID = strings.TrimSpace(strings.TrimPrefix(trimmed, "Name:"))
		case strings.HasPrefix(trimmed, "Description:"):
			d.Name = strings.TrimSpace(strings.TrimPrefix(trimmed, "Description:"))
		case strings.HasPrefix(trimmed, "Sample Specification:"):
			spec := strings.TrimSpace(strings.TrimPrefix(trimmed, "Sample Specification:"))
			if rate, ch, ok := parseSampleSpec(spec); ok {
				d.SampleRate, d.Channels = rate, ch
			}
		case trimmed == "Properties:":
			inProps = true
			continue
		case inProps && (strings.HasPrefix(trimmed, "Formats:") || strings.HasPrefix(trimmed, "State:")):
			// The Properties section ended (or the block shape changed).
			inProps = false
		}
		if inProps {
			for _, key := range []string{"factory.name", "device.name", "device.description"} {
				if v, ok := propValue(trimmed, key); ok {
					kindEvidence = append(kindEvidence, v)
				}
			}
		}
	}
	if d.ID == "" {
		return d, false
	}
	if d.Name == "" {
		d.Name = d.ID
	}
	d.Kind = inferKind(append([]string{d.ID}, kindEvidence...))
	return d, true
}

// propValue extracts `key = "value"` from one property line.
func propValue(line, key string) (string, bool) {
	prefix := key + " ="
	if !strings.HasPrefix(line, prefix) {
		return "", false
	}
	v := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	v = strings.Trim(v, `"`)
	return v, true
}

// parseSampleSpec parses "float32le 2ch 48000Hz" into (rate, channels).
func parseSampleSpec(spec string) (string, int, bool) {
	f := strings.Fields(spec)
	if len(f) < 3 {
		return "", 0, false
	}
	ch := 0
	if n, err := strconv.Atoi(strings.TrimSuffix(f[1], "ch")); err == nil {
		ch = n
	}
	rate := strings.TrimSuffix(f[2], "Hz")
	if _, err := strconv.Atoi(rate); err != nil {
		return "", 0, false
	}
	return rate, ch, true
}

// parseDefaultSink reads the "Default Sink:" line of `pactl info`. The
// value is the sink name, or `index/name` on some builds — the name is the
// stable identity, so the part after the slash wins.
func parseDefaultSink(info string) string {
	for _, line := range strings.Split(info, "\n") {
		trimmed := strings.TrimSpace(line)
		const prefix = "Default Sink:"
		if !strings.HasPrefix(trimmed, prefix) {
			continue
		}
		v := strings.TrimSpace(strings.TrimPrefix(trimmed, prefix))
		if i := strings.LastIndex(v, "/"); i >= 0 {
			v = strings.TrimSpace(v[i+1:])
		}
		return v
	}
	return ""
}

// inferKind classifies one device from its identity strings (ADR-0074):
// the ordering matters — a null sink named "usb_null" is a null sink, not a
// USB one. An unrecognised row is `unknown`, never silently re-classified.
func inferKind(evidence []string) Kind {
	joined := strings.ToLower(strings.Join(evidence, " "))
	switch {
	case strings.Contains(joined, "bluez"), strings.Contains(joined, "bluetooth"),
		strings.Contains(joined, "a2dp"):
		// The PipeWire node of an A2DP sink is bluez_output.<MAC>.<n> and the
		// ALSA card is "bluetooth": one direction of the radio (ADR-0087).
		// Checked before the others: a headset that advertises a USB-style
		// description must still read as its radio, not as a wired DAC.
		return KindBluetooth
	case strings.Contains(joined, "null"):
		return KindNull
	case strings.Contains(joined, "hdmi"), strings.Contains(joined, "displayport"),
		strings.Contains(joined, "optical"), strings.Contains(joined, "iec958"),
		strings.Contains(joined, "dp-"):
		return KindHDMI
	case strings.Contains(joined, "usb"), strings.Contains(joined, "uac"):
		return KindUSB
	default:
		return KindBuiltin
	}
}

// ── ALSA fallback ───────────────────────────────────────────────────────────

// listALSA enumerates through aplay (the ALSA-only host path). `aplay -L`
// prints one output device per line ("name    # comment"); the card list
// (`aplay -l`) feeds the note so the row is self-describing.
func listALSA(ctx context.Context) (List, error) {
	if _, err := exec.LookPath("aplay"); err != nil {
		return List{}, fmt.Errorf("aplay is not on PATH")
	}
	out, err := runCmd(ctx, "aplay", "-L")
	if err != nil {
		return List{}, fmt.Errorf("aplay -L failed: %w", err)
	}
	var devices []Device
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		// The device line is unindented; aplay indents the comment
		// continuation lines, and some builds print standalone "#" comments.
		if strings.HasPrefix(line, " ") || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		name, comment := splitALSAline(line)
		if name == "" {
			continue
		}
		d := Device{
			ID:   name,
			Name: name,
			Kind: inferALSAKind(name, comment),
		}
		devices = append(devices, applyProfile(d))
	}
	if len(devices) == 0 {
		return List{}, fmt.Errorf("no output devices in `aplay -L` output")
	}
	note := ""
	if l, err := runCmd(ctx, "aplay", "-l"); err == nil {
		note = strings.TrimSpace(l)
	}
	l := List{Backend: "alsa", Devices: devices}
	if note != "" {
		l.Note = note
	}
	return l, nil
}

// splitALSAline splits `name    # comment`.
func splitALSAline(line string) (string, string) {
	if i := strings.Index(line, "#"); i >= 0 {
		return strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:])
	}
	return strings.TrimSpace(line), ""
}

// inferALSAKind classifies one aplay -L row. The built-in set is the named
// virtual devices (default/front/surround/center/lfe/monophon…); everything
// else falls through to the name-based evidence.
func inferALSAKind(name, comment string) Kind {
	switch {
	case name == "null":
		return KindNull
	case name == "default", name == "front", name == "rear",
		strings.HasPrefix(name, "surround"), strings.HasPrefix(name, "iec958"):
		// iec958 is the S/PDIF (often HDMI-arc-routed) output: the spec's
		// HDMI class is the display-output class.
		if strings.HasPrefix(name, "iec958") {
			return KindHDMI
		}
		return KindBuiltin
	default:
		return inferKind([]string{name, comment})
	}
}

// applyProfile upgrades a row whose identity is a known profile (issue #5):
// the kind becomes the profile's (the evidence the aplay comment may not
// carry — the UNO Q's aplay -L rows name the subdevice, not the bridge),
// the display name becomes the profile's label, and the proven rate and
// channels fill the row's evidence fields. The ID stays the stable hw:
// identity — selection and the alsahw sink both address it.
func applyProfile(d Device) Device {
	p, ok := ProfileForDevice(d.ID)
	if !ok {
		return d
	}
	d.Kind = KindHDMI
	d.Name = p.Label
	d.SampleRate = strconv.Itoa(p.Rate)
	d.Channels = p.Channels
	return d
}

// runCmd runs one command with the caller's context and returns its stdout.
func runCmd(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return string(out), nil
}
