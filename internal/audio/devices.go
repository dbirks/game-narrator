// Package audio is the output-audio product surface (ADR-0074): the device
// enumeration the Settings page renders, the deterministic test tone the
// consent-gated endpoint plays, and the selection precedence that decides
// which sink and device actually receive the audio.
//
// Enumeration is read-only and subprocess-based on purpose (ADR-0023's
// subprocess posture, the doctor's own precedent): PipeWire through `pactl`
// when it is on PATH, ALSA through `aplay -L` as the fallback, and a
// labelled-synthetic list when the test environment asks for one. Nothing in
// this package plays audio or mutates system state.
package audio

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// EnvFakeDevices names the environment variable that makes the lister return
// a fixed, labelled-synthetic device list (the Playwright suite has no USB
// DAC). Format: a comma list of `name[:kind[:rate]]` triples, e.g.
// "auto_null:null:48000,USB DAC:usb:48000". The synthetic flag travels into
// every response so the fake list is never mistaken for hardware (ADR-0074).
const EnvFakeDevices = "GAME_NARRATOR_AUDIO_FAKE_DEVICES"

// EnvSink and EnvDevice are the operator's environment override of the audio
// output (ADR-0039). They keep their exact names: the precedence (env beats
// the persisted store value beats the built-in default) is documented
// against these two variables.
const (
	EnvSink   = "GAME_NARRATOR_AUDIO_SINK"
	EnvDevice = "GAME_NARRATOR_AUDIO_DEVICE"
)

// Sink kinds: the same kinds the session runtime accepts (ADR-0039). They
// live here because both the selection logic and the API validate against
// them; the runtime aliases these constants (ADR-0074 keeps one spelling).
const (
	SinkDummy    = "dummy"
	SinkFile     = "filesink"
	SinkPipeWire = "pipewire"
	// SinkALSA writes the device node in-process with the profile's exact
	// hw_params (issue #5 / ADR-0111): the UNO Q narrator runs as a system
	// service without a user PipeWire session, and the hub HDMI bridge
	// audibly plays only at the proven config the profile pins.
	SinkALSA = "alsahw"
)

// KnownSinkKinds is the accepted sink-kind set, for validation and the
// "would have failed to start" note.
var KnownSinkKinds = []string{SinkDummy, SinkFile, SinkPipeWire, SinkALSA}

// DefaultDevice is the platform-default sink identity the app plays to when
// neither the environment nor the store names a device (the platform's own
// default route — the honest "I am not choosing, the OS is").
const DefaultDevice = "default"

// Device kinds (ADR-0074): a USB DAC, an HDMI/arc output, the platform's
// built-in output, a virtual null sink, a Bluetooth A2DP sink (ADR-0087),
// or none of the five (unknown — reported, never re-classified).
type Kind string

const (
	KindUSB       Kind = "usb"
	KindHDMI      Kind = "hdmi"
	KindBuiltin   Kind = "builtin"
	KindNull      Kind = "null"
	KindBluetooth Kind = "bluetooth"
	KindUnknown   Kind = "unknown"
)

// Device is one output device row. ID is the stable identity the app selects
// by across reboots (docs/spec/10): the PipeWire sink NAME or the ALSA
// device name — never a numeric index, which is exactly what a reboot
// renumbers.
type Device struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Kind            Kind   `json:"kind"`
	SampleRate      string `json:"sample_rate,omitempty"`
	Channels        int    `json:"channels,omitempty"`
	PlatformDefault bool   `json:"platform_default"`
}

// List is one enumeration: the backend that answered, the rows, and the
// synthetic flag (true when the list came from EnvFakeDevices — the UI
// label and the doctor check both read it).
type List struct {
	Backend   string   `json:"backend"` // "pipewire" | "alsa" | "synthetic" | "none"
	Synthetic bool     `json:"synthetic"`
	Devices   []Device `json:"devices"`
	// Note carries a backend-level finding the caller can surface ("pactl is
	// on PATH but the server is not running", "aplay -L failed: …"). It is
	// reported, not hidden: an empty list with no note would read as
	// "nothing is attached" when the truth is "I could not ask".
	Note string `json:"note,omitempty"`
}

// ListDevices enumerates the output devices (read-only). lookupEnv is
// indirection so a test can point the synthetic-list decision at a fixture
// environment without mutating the process.
//
// The backend order is the product requirement (docs/spec/10: PipeWire on
// the UNO Q, ALSA as the documented fallback): a synthetic list when the
// test seam is set, then PipeWire when pactl is on PATH and answers, then
// ALSA when aplay is, then an honest "none". A backend that is present but
// fails (the daemon is down) reports the failure in Note and falls through
// to the next backend — the lister degrades, it does not guess.
func ListDevices(ctx context.Context, lookupEnv func(string) (string, bool)) (List, error) {
	if fake, ok := lookupEnv(EnvFakeDevices); ok && strings.TrimSpace(fake) != "" {
		devs, err := parseFakeDevices(fake)
		if err != nil {
			// A malformed test seam is a configuration error, not a
			// degraded backend: fail loudly so the misconfiguration is loud.
			return List{}, fmt.Errorf("audio: %w", err)
		}
		return List{
			Backend:   "synthetic",
			Synthetic: true,
			Devices:   devs,
			Note:      "device list from " + EnvFakeDevices + " (synthetic; no hardware was enumerated)",
		}, nil
	}

	if path, err := exec.LookPath("pactl"); err == nil {
		if l, err := listPipeWire(ctx, path); err == nil {
			return l, nil
		}
		// pactl is on PATH but the server is not (or the call failed):
		// record it and fall through to ALSA rather than reporting an
		// empty world.
		if l, aerr := listALSA(ctx); aerr == nil {
			l.Note = "pactl is on PATH but " + err.Error() + "; ALSA listing shown instead"
			return l, nil
		}
		return List{
			Backend: "none",
			Devices: []Device{},
			Note:    fmt.Sprintf("pactl failed (%v) and aplay did not answer either", err),
		}, nil
	}

	if l, err := listALSA(ctx); err == nil {
		return l, nil
	}
	return List{
		Backend: "none",
		Devices: []Device{},
		Note:    "neither pactl nor aplay is on PATH (pacman -S pipewire-pulse alsa-utils)",
	}, nil
}

// ── synthetic list (the test seam) ──────────────────────────────────────────

// parseFakeDevices parses the EnvFakeDevices format `name[:kind[:rate]]`
// (comma-separated). A kind outside the known set is an error: a typo in the
// test seam must not silently become `unknown` rows the spec cannot name.
func parseFakeDevices(raw string) ([]Device, error) {
	var out []Device
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		dev, err := parseFakeDevice(part)
		if err != nil {
			return nil, err
		}
		out = append(out, dev)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s set but no device named", EnvFakeDevices)
	}
	return out, nil
}

// parseFakeDevice is one seam entry: name[:kind[:rate]]. The Bluetooth A2DP
// sink id (bluez_output.<MAC>.1, ADR-0091) carries the MAC's own five
// colons, so an entry whose name contains bluez_output. is parsed from the
// right — the rate is the last field when it is numeric, the kind the next
// when it is a known kind, the name everything before — while a plain name
// stays strict: more than three fields or an unknown kind is a
// misconfiguration, and the seam fails loudly (the ListDevices contract).
func parseFakeDevice(part string) (Device, error) {
	name := strings.TrimSpace(part)
	var kind Kind
	rate := "48000"
	f := strings.Split(part, ":")
	if strings.Contains(part, "bluez_output.") {
		if len(f) >= 3 {
			if _, err := strconv.Atoi(strings.TrimSpace(f[len(f)-1])); err == nil {
				rate = strings.TrimSpace(f[len(f)-1])
				f = f[:len(f)-1]
			}
		}
		if len(f) >= 2 {
			switch k := Kind(strings.TrimSpace(f[len(f)-1])); k {
			case KindUSB, KindHDMI, KindBuiltin, KindNull, KindBluetooth, KindUnknown:
				kind = k
				f = f[:len(f)-1]
			}
		}
		name = strings.TrimSpace(strings.Join(f, ":"))
	} else if len(f) > 3 {
		return Device{}, fmt.Errorf("malformed %s entry %q (want name[:kind[:rate]])", EnvFakeDevices, part)
	} else if len(f) >= 2 {
		k := Kind(strings.TrimSpace(f[1]))
		switch k {
		case KindBluetooth:
			// The bluetooth kind is reserved for the A2DP sink id (the
			// bluez_output.<MAC>.1 form the ADR-0091 selection persists): a
			// plain name claiming it is a misconfiguration, and the seam
			// fails loudly rather than guessing the MAC behind it.
			return Device{}, fmt.Errorf("malformed %s entry %q: the %q kind needs a bluez_output.<MAC>.1 sink id", EnvFakeDevices, part, k)
		case KindUSB, KindHDMI, KindBuiltin, KindNull, KindUnknown:
		default:
			return Device{}, fmt.Errorf("malformed %s entry %q: unknown kind %q", EnvFakeDevices, part, k)
		}
		kind = k
		name = strings.TrimSpace(f[0])
		if len(f) == 3 {
			rate = strings.TrimSpace(f[2])
			if _, err := strconv.Atoi(rate); err != nil {
				return Device{}, fmt.Errorf("malformed %s entry %q: sample rate %q is not a number", EnvFakeDevices, part, rate)
			}
		}
	}
	if name == "" {
		return Device{}, fmt.Errorf("malformed %s entry %q (empty name)", EnvFakeDevices, part)
	}
	if kind == "" {
		kind = KindBuiltin
	}
	return Device{ID: name, Name: name, Kind: kind, SampleRate: rate}, nil
}
