package audio

import (
	"fmt"
	"strings"
)

// The selection precedence (ADR-0074), documented once here and by the API:
//
//	environment variable (set)  >  persisted store value  >  built-in default
//
// The environment stays the operator's override (ADR-0039 keeps its exact
// names and semantics); the store is the user's selection, made from the
// Settings page; the defaults are filesink and the platform-default sink.
// Master volume has no environment form — it is the store value or 1.0.
// Every resolved part carries its source so the page can say WHY a value
// wins ("set by GAME_NARRATOR_AUDIO_DEVICE", "your saved selection",
// "the built-in default").

// DefaultSinkKind is the built-in sink kind (ADR-0039's boot default: the
// filesink under the durable directory — evidence without a device).
const DefaultSinkKind = SinkFile

// DefaultMasterVolume is unity: the default plays what the voice and the
// session volume decide, with no device-level scaling of its own.
const DefaultMasterVolume = 1.0

// MaxMasterVolume bounds the master volume (0 = silent device, 1 = unity).
const MaxMasterVolume = 1.0

// MaxGainBounds are the per-voice gain bounds (voice_profiles.gain): 0 is a
// silenced voice, and the upper bound keeps a calibration slip from
// hard-clipping the device.
const (
	GainMin = 0.0
	GainMax = 10.0
)

// Source labels for the resolved selection's provenance fields.
const (
	SourceEnv     = "env"
	SourceStore   = "store"
	SourceDefault = "default"
)

// Stored is the persisted selection as the store holds it (one row). Set is
// false when no row exists — the caller must distinguish "the user never
// chose" from "the user chose default".
type Stored struct {
	Device       string
	SinkKind     string
	MasterVolume float64
	Set          bool
}

// Selection is the effective audio output selection.
type Selection struct {
	Device             string  `json:"device"`
	DeviceSource       string  `json:"device_source"`
	SinkKind           string  `json:"sink_kind"`
	SinkKindSource     string  `json:"sink_kind_source"`
	MasterVolume       float64 `json:"master_volume"`
	MasterVolumeSource string  `json:"master_volume_source"`
}

// Resolve applies the precedence. lookupEnv is the environment indirection
// (tests); stored is nil when there is no store (or no row).
func Resolve(lookupEnv func(string) (string, bool), stored *Stored) (Selection, error) {
	sel := Selection{
		Device:             DefaultDevice,
		DeviceSource:       SourceDefault,
		SinkKind:           DefaultSinkKind,
		SinkKindSource:     SourceDefault,
		MasterVolume:       DefaultMasterVolume,
		MasterVolumeSource: SourceDefault,
	}

	// A set-but-blank variable is not a choice: it reads as the default (or
	// the stored value), with the default's source — a blank
	// GAME_NARRATOR_AUDIO_SINK that claimed to be the sink source would be a
	// lie about provenance.
	envSink := ""
	if v, ok := lookupEnv(EnvSink); ok {
		envSink = strings.TrimSpace(v)
	}
	switch {
	case envSink != "":
		if !knownSinkKind(envSink) {
			return sel, fmt.Errorf("unknown sink kind %q (want %s)", envSink, strings.Join(KnownSinkKinds, ", "))
		}
		sel.SinkKind, sel.SinkKindSource = envSink, SourceEnv
	case stored != nil && stored.Set && stored.SinkKind != "":
		if !knownSinkKind(stored.SinkKind) {
			return sel, fmt.Errorf("stored sink kind %q is not one this build accepts (%s)",
				stored.SinkKind, strings.Join(KnownSinkKinds, ", "))
		}
		sel.SinkKind, sel.SinkKindSource = stored.SinkKind, SourceStore
	}

	if v, ok := lookupEnv(EnvDevice); ok {
		v = strings.TrimSpace(v)
		if v != "" {
			sel.Device, sel.DeviceSource = v, SourceEnv
		}
	} else if stored != nil && stored.Set && stored.Device != "" {
		sel.Device, sel.DeviceSource = stored.Device, SourceStore
	}

	if stored != nil && stored.Set && stored.MasterVolume >= 0 && stored.MasterVolume <= MaxMasterVolume {
		// Applied whenever the row exists (Set): a stored 0 is the user's
		// silence calibration, not "never chosen" — the store documents
		// exactly that distinction (AGENTS.md rule 10: the row is the fact).
		sel.MasterVolume, sel.MasterVolumeSource = stored.MasterVolume, SourceStore
	}
	return sel, nil
}

// ValidateDevice checks a user-supplied device against the current
// enumeration (ADR-0074: a selection names a device by its stable ID).
// "default" is always admissible (the platform's own routing); a named
// device must be one the lister just saw. An empty enumeration admits any
// well-formed name: on a backend-less host the user is configuring for the
// hardware they are about to attach, and refusing the save would make the
// selection impossible to make at all.
func ValidateDevice(device string, list List) error {
	device = strings.TrimSpace(device)
	if device == "" {
		return fmt.Errorf("device must not be empty (use %q for the platform default)", DefaultDevice)
	}
	if len(device) > 200 {
		return fmt.Errorf("device name is longer than 200 characters")
	}
	if device == DefaultDevice {
		return nil
	}
	if len(list.Devices) == 0 {
		return nil
	}
	for _, d := range list.Devices {
		if d.ID == device {
			return nil
		}
	}
	return fmt.Errorf("device %q is not among the enumerated devices (%s)", device, deviceIDList(list))
}

// ValidateSinkKind checks a user-supplied sink kind.
func ValidateSinkKind(kind string) error {
	if !knownSinkKind(kind) {
		return fmt.Errorf("sink kind %q is not one this build accepts (%s)", kind, strings.Join(KnownSinkKinds, ", "))
	}
	return nil
}

// ValidateMasterVolume checks the 0..1 bound.
func ValidateMasterVolume(v float64) error {
	if v < 0 || v > MaxMasterVolume {
		return fmt.Errorf("master volume must be in 0..1, got %v", v)
	}
	return nil
}

// ValidateGain checks the per-voice gain bounds.
func ValidateGain(v float64) error {
	if v < GainMin || v > GainMax {
		return fmt.Errorf("gain must be in %g..%g, got %v", GainMin, GainMax, v)
	}
	return nil
}

func knownSinkKind(v string) bool {
	for _, k := range KnownSinkKinds {
		if v == k {
			return true
		}
	}
	return false
}

func deviceIDList(list List) string {
	ids := make([]string, 0, len(list.Devices))
	for _, d := range list.Devices {
		ids = append(ids, d.ID)
	}
	if len(ids) == 0 {
		return "(none)"
	}
	return strings.Join(ids, ", ")
}
