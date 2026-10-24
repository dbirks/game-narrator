package doctor

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// stockMinQuantum is PipeWire's stock default.clock.min-quantum. With a
// Bluetooth transport in play, the fixed BT latency has to fit inside the
// buffer floor; the stock 1024 is the documented crackle/latency culprit
// (issue #61, pipewire upstream configuration docs).
const stockMinQuantum = 1024

// pipewireClock carries the clock fields the check reports. Zero values mean
// "not reported by the installed tooling".
type pipewireClock struct {
	rate     int
	quantum  int
	minQuant int
	maxQuant int
}

func (c pipewireClock) empty() bool {
	return c.rate == 0 && c.quantum == 0 && c.minQuant == 0 && c.maxQuant == 0
}

// parsePwMetadataSettings parses `pw-metadata -n settings` output (lines of
// the shape `update: id:0 key:'clock.rate' value:'48000' type:”`). Key
// naming varies across PipeWire versions (default.clock.* vs clock.*), so
// both forms are accepted and the default.* form wins. Non-integer or
// non-positive values are dropped, never fatal: a partially reportable clock
// still says something useful.
func parsePwMetadataSettings(out []byte) pipewireClock {
	var c pipewireClock
	put := func(dst *int, value string) {
		n, err := strconv.Atoi(value)
		if err != nil || n <= 0 {
			return
		}
		*dst = n
	}
	for _, line := range strings.Split(string(out), "\n") {
		before, value, ok := strings.Cut(line, " value:'")
		if !ok {
			continue
		}
		_, k, ok := strings.Cut(before, "key:'")
		if !ok {
			continue
		}
		key, _, _ := strings.Cut(k, "'")
		value, _, _ = strings.Cut(value, "'")
		switch key {
		case "default.clock.rate":
			put(&c.rate, value)
		case "clock.rate":
			if c.rate == 0 {
				put(&c.rate, value)
			}
		case "default.clock.quantum":
			put(&c.quantum, value)
		case "clock.quantum":
			if c.quantum == 0 {
				put(&c.quantum, value)
			}
		case "default.clock.min-quantum":
			put(&c.minQuant, value)
		case "clock.min-quantum":
			if c.minQuant == 0 {
				put(&c.minQuant, value)
			}
		case "default.clock.max-quantum":
			put(&c.maxQuant, value)
		case "clock.max-quantum":
			if c.maxQuant == 0 {
				put(&c.maxQuant, value)
			}
		}
	}
	return c
}

// propValue is the right-hand side of a pactl "key = \"value\"" property
// line, quotes stripped.
func propValue(line string) string {
	_, v, _ := strings.Cut(line, "=")
	return strings.Trim(strings.TrimSpace(v), "\"")
}

// btSink reports the Bluetooth profile and negotiated codec of one
// `pactl list sinks` block when its Properties name a BlueZ 5 transport. The
// block is the text after one `Sink #N` header up to the next, so a host with
// a wired and a BT sink only reports the BT one. Property names vary between
// PipeWire versions (bluez5.codec vs api.bluez5.codec); both are accepted.
func btSink(block string) (profile, codec string, isBT bool) {
	props := strings.TrimPrefix(block, "Properties:")
	if !strings.Contains(props, "bluez5.") {
		return "", "", false
	}
	for _, line := range strings.Split(props, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.Contains(line, "bluez5.profile"):
			profile = propValue(line)
		case strings.Contains(line, "api.bluez5.codec") || strings.Contains(line, "bluez5.codec"):
			codec = propValue(line)
		}
	}
	return profile, codec, true
}

// defaultSinkName extracts the default sink name from `pactl info` output
// ("Default Sink: alsa_output.pulse"), which is the transport PipeWire is
// actually routing to — the check's BT verdict must be about that sink, not
// any connected one.
func defaultSinkName(infoOut []byte) string {
	for _, line := range strings.Split(string(infoOut), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "Default Sink:"); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// sinkBlocks splits `pactl list sinks` output into its per-sink blocks.
func sinkBlocks(listOut []byte) map[string]string {
	blocks := map[string]string{}
	var cur, name string
	for _, line := range strings.Split(string(listOut), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "Sink #") {
			if name != "" {
				blocks[name] = cur
			}
			cur, name = "", ""
			continue
		}
		cur += line + "\n"
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "Name:"); ok {
			name = strings.TrimSpace(rest)
		}
	}
	if name != "" {
		blocks[name] = cur
	}
	return blocks
}

// btDefaultSink reports the profile/codec of the default sink when it is a
// Bluetooth transport. The default-sink name comes from `pactl info`; the
// sink's own Properties come from its block in `pactl list sinks`. When the
// name is unknown (no default) the first BT sink found stands in, because a
// host with exactly one BT sink and no other sink is the case the check
// exists for.
func btDefaultSink(defaultName string, blocks map[string]string) (profile, codec string, isBT bool) {
	if defaultName != "" {
		if block, ok := blocks[defaultName]; ok {
			return btSink(block)
		}
	}
	for _, block := range blocks {
		if _, _, isBT := btSink(block); isBT {
			return btSink(block)
		}
	}
	return "", "", false
}

// checkAudioPipeWire reports the live PipeWire clock settings and, when the
// default sink is a Bluetooth transport, its profile and negotiated codec
// (issue #61). It is read-only: settings are read from the daemon and sink
// properties from pactl; nothing is restarted or reconfigured.
//
// Tooling: `pw-metadata -n settings` is the clock source of truth. A host
// with only `pactl` (pipewire-pulse installed, pw-* not on PATH) still gets
// a row: the clock rate comes from the default sink's sample spec and the BT
// verdict from the sink properties, but the min-quantum warning cannot fire
// because the daemon settings are not readable — the detail says so. No
// PipeWire tooling at all is NOT CONFIGURED, not a fault.
func (r *runner) checkAudioPipeWire(ctx context.Context) Check {
	hasTool := func(name string) bool { _, err := r.opts.LookPath(name); return err == nil }

	var clock pipewireClock
	if hasTool("pw-metadata") {
		res := r.opts.Exec(ctx, "pw-metadata", "-n", "settings")
		if res.Err != nil {
			return Check{
				Status:  StatusDegraded,
				Summary: "PipeWire present, but the settings probe failed",
				Detail:  oneLine(fmt.Sprintf("pw-metadata -n settings: %v", res.Err), 240),
				Remedy:  "is the pipewire daemon running for this user? (systemctl --user status pipewire)",
			}
		}
		clock = parsePwMetadataSettings(res.Stdout)
	} else if !hasTool("pactl") {
		return Check{
			Status:  StatusNotConfigured,
			Summary: "PipeWire is not installed on this host",
			Detail:  "neither pw-metadata nor pactl is on PATH",
			Remedy:  "nothing to do unless this host should run PipeWire: pacman -S pipewire pipewire-pulse",
		}
	}

	var profile, codec string
	isBT := false
	if hasTool("pactl") {
		info := r.opts.Exec(ctx, "pactl", "info").Stdout
		lists := r.opts.Exec(ctx, "pactl", "list", "sinks")
		blocks := sinkBlocks(lists.Stdout)
		profile, codec, isBT = btDefaultSink(defaultSinkName(info), blocks)
		if isBT && clock.rate == 0 {
			// Rate-only visibility from the sample spec: the default sink's
			// "Sample Specification:" line is the only rate pactl reports
			// when pw-metadata is not installed.
			if name := defaultSinkName(info); name != "" {
				if line, ok := strings.CutPrefix(sinkSampleRateLine(blocks[name]), "Sample Specification:"); ok {
					if i := strings.LastIndex(line, "Hz"); i > 0 {
						if n, err := strconv.Atoi(line[:i]); err == nil && n > 0 {
							clock.rate = n
						}
					}
				}
			}
		}
	}

	detail := ""
	if clock.rate > 0 {
		detail += fmt.Sprintf("rate %d Hz", clock.rate)
	}
	if clock.quantum > 0 || clock.minQuant > 0 || clock.maxQuant > 0 {
		if detail != "" {
			detail += ", "
		}
		if clock.minQuant > 0 {
			detail += fmt.Sprintf("min-quantum %d", clock.minQuant)
		}
		if clock.quantum > 0 {
			detail += fmt.Sprintf(", quantum %d", clock.quantum)
		}
		if clock.maxQuant > 0 {
			detail += fmt.Sprintf(", max-quantum %d", clock.maxQuant)
		}
	}
	if isBT {
		if detail != "" {
			detail += "; "
		}
		detail += fmt.Sprintf("default sink is Bluetooth (profile %q, codec %q)", profile, codec)
	} else if len(detail) > 0 {
		detail += "; default sink is not Bluetooth"
	}
	if isBT && clock.minQuant == 0 {
		detail += "; min-quantum not readable (pw-metadata absent or empty), the stock-1024 warning is disabled"
	}

	remedy := "raise default.clock.min-quantum above the stock 1024 so the BT transport's fixed latency fits the buffer floor, and pin the codec (avoid SBC fallback) via bluez5.codec_config in the wireplumber/pipewire bluetooth settings; see the setup guide's Bluetooth section"

	if isBT && clock.minQuant == 0 {
		// BT transport active but the quantum cannot be verified: report it
		// and warn, because the stock host we target runs 1024.
		return Check{
			Status:  StatusDegraded,
			Summary: "Bluetooth output active; PipeWire min-quantum unverifiable (stock 1024 is the crackle case)",
			Detail:  detail,
			Remedy:  remedy + " — install pipewire (pacman -S pipewire) for the full clock readout",
		}
	}
	if isBT && clock.minQuant == stockMinQuantum {
		return Check{
			Status:  StatusDegraded,
			Summary: fmt.Sprintf("Bluetooth output at the stock min-quantum %d — the documented crackle/latency case", stockMinQuantum),
			Detail:  detail,
			Remedy:  remedy,
		}
	}
	return Check{
		Status:  StatusOK,
		Summary: "PipeWire clock healthy" + btSuffix(isBT),
		Detail:  detail,
	}
}

func btSuffix(isBT bool) string {
	if isBT {
		return " for the Bluetooth transport"
	}
	return ""
}

// sinkSampleRateLine pulls the "Sample Specification:" line out of a sink
// block.
func sinkSampleRateLine(block string) string {
	for _, line := range strings.Split(block, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "Sample Specification:") {
			return line
		}
	}
	return ""
}
