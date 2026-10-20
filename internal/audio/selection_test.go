package audio

import (
	"testing"
)

// The precedence (env > stored > default) is the one rule the page, the
// process and the doctor all read, so it is pinned in every ordering: a
// regression here is a page that says one thing and a device that does
// another.

func envLookup(kv map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := kv[k]
		return v, ok
	}
}

func TestResolvePrecedenceMatrix(t *testing.T) {
	noEnv := envLookup(nil)
	store := &Stored{Device: "store-dac", SinkKind: "pipewire", MasterVolume: 0.5, Set: true}
	env := envLookup(map[string]string{EnvDevice: "env-dac", EnvSink: "dummy"})

	cases := []struct {
		name      string
		lookup    func(string) (string, bool)
		stored    *Stored
		device    string
		deviceSrc string
		sink      string
		sinkSrc   string
		volume    float64
		volSrc    string
	}{
		{
			name:   "neither: the built-in defaults",
			lookup: noEnv, stored: nil,
			device: DefaultDevice, deviceSrc: SourceDefault,
			sink: DefaultSinkKind, sinkSrc: SourceDefault,
			volume: DefaultMasterVolume, volSrc: SourceDefault,
		},
		{
			name:   "store only: the user's selection",
			lookup: noEnv, stored: store,
			device: "store-dac", deviceSrc: SourceStore,
			sink: "pipewire", sinkSrc: SourceStore,
			volume: 0.5, volSrc: SourceStore,
		},
		{
			name:   "env only: the operator's override",
			lookup: env, stored: nil,
			device: "env-dac", deviceSrc: SourceEnv,
			sink: "dummy", sinkSrc: SourceEnv,
			volume: DefaultMasterVolume, volSrc: SourceDefault,
		},
		{
			name:   "both: the environment wins",
			lookup: env, stored: store,
			device: "env-dac", deviceSrc: SourceEnv,
			sink: "dummy", sinkSrc: SourceEnv,
			volume: 0.5, volSrc: SourceStore, // the volume has no env form
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sel, err := Resolve(c.lookup, c.stored)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if sel.Device != c.device || sel.DeviceSource != c.deviceSrc {
				t.Errorf("device = %q (%s), want %q (%s)", sel.Device, sel.DeviceSource, c.device, c.deviceSrc)
			}
			if sel.SinkKind != c.sink || sel.SinkKindSource != c.sinkSrc {
				t.Errorf("sink = %q (%s), want %q (%s)", sel.SinkKind, sel.SinkKindSource, c.sink, c.sinkSrc)
			}
			if sel.MasterVolume != c.volume || sel.MasterVolumeSource != c.volSrc {
				t.Errorf("volume = %v (%s), want %v (%s)", sel.MasterVolume, sel.MasterVolumeSource, c.volume, c.volSrc)
			}
		})
	}
}

func TestResolveStoredSilenceIsApplied(t *testing.T) {
	// A stored 0 is the user's silence calibration (the store documents it
	// as a chosen value, not "never chosen"); the resolution must apply it.
	stored := &Stored{Device: "dac", SinkKind: "pipewire", MasterVolume: 0, Set: true}
	sel, err := Resolve(envLookup(nil), stored)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if sel.MasterVolume != 0 || sel.MasterVolumeSource != SourceStore {
		t.Errorf("stored silence: got %v (%s), want 0 (store)", sel.MasterVolume, sel.MasterVolumeSource)
	}
	// …and without a row it is the default, not a silence:
	sel, err = Resolve(envLookup(nil), nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if sel.MasterVolume != DefaultMasterVolume {
		t.Errorf("no row: got %v, want the default %v", sel.MasterVolume, DefaultMasterVolume)
	}
}

func TestResolveRefusesUnknownSinkKinds(t *testing.T) {
	if _, err := Resolve(envLookup(map[string]string{EnvSink: "jack"}), nil); err == nil {
		t.Error("an unknown env sink kind must be refused (the process would fail to build it)")
	}
	if _, err := Resolve(envLookup(nil), &Stored{SinkKind: "jack", Set: true}); err == nil {
		t.Error("an unknown stored sink kind must be refused")
	}
	// An explicitly-empty env value is the default, not an error.
	sel, err := Resolve(envLookup(map[string]string{EnvSink: "  "}), nil)
	if err != nil {
		t.Fatalf("blank env sink: %v", err)
	}
	if sel.SinkKind != DefaultSinkKind || sel.SinkKindSource != SourceDefault {
		t.Errorf("blank env sink = %q (%s), want the default", sel.SinkKind, sel.SinkKindSource)
	}
}

func TestValidateDevice(t *testing.T) {
	list := List{Backend: "synthetic", Devices: []Device{
		{ID: "dac", Name: "dac", Kind: KindUSB},
		{ID: "null", Name: "null", Kind: KindNull},
	}}
	if err := ValidateDevice("dac", list); err != nil {
		t.Errorf("an enumerated device: %v", err)
	}
	if err := ValidateDevice(DefaultDevice, list); err != nil {
		t.Errorf("default: %v", err)
	}
	if err := ValidateDevice("missing", list); err == nil {
		t.Error("an unenumerated device must be refused with the id named")
	}
	// An empty enumeration admits a well-formed name: on a backend-less host
	// the user is configuring for hardware they are about to attach.
	if err := ValidateDevice("future-dac", List{}); err != nil {
		t.Errorf("empty enumeration: %v", err)
	}
	if err := ValidateDevice("   ", list); err == nil {
		t.Error("a blank device must be refused")
	}
}

func TestValidateSinkKind(t *testing.T) {
	for _, k := range KnownSinkKinds {
		if err := ValidateSinkKind(k); err != nil {
			t.Errorf("kind %q: %v", k, err)
		}
	}
	if err := ValidateSinkKind("jack"); err == nil {
		t.Error("an unknown sink kind must be refused")
	}
}

func TestValidateMasterVolume(t *testing.T) {
	for _, v := range []float64{0, 0.25, 1} {
		if err := ValidateMasterVolume(v); err != nil {
			t.Errorf("volume %v: %v", v, err)
		}
	}
	for _, v := range []float64{-0.1, 1.01} {
		if err := ValidateMasterVolume(v); err == nil {
			t.Errorf("volume %v: accepted, want refused", v)
		}
	}
}

func TestValidateGain(t *testing.T) {
	for _, v := range []float64{0, 1, 3.2, 10} {
		if err := ValidateGain(v); err != nil {
			t.Errorf("gain %v: %v", v, err)
		}
	}
	for _, v := range []float64{-0.1, 10.1} {
		if err := ValidateGain(v); err == nil {
			t.Errorf("gain %v: accepted, want refused", v)
		}
	}
}
