package audio

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The enumeration is the one place this product reads the host's audio
// world, so its parsers are tested against fixtures (AGENTS.md rule 3:
// synthetic inputs, labelled). No test here depends on which sinks or
// cards the host actually has.

// ── synthetic list (the test seam) ─────────────────────────────────────────

func TestParseFakeDevicesParsesTheDocumentedFormat(t *testing.T) {
	devs, err := parseFakeDevices("auto_null:null:48000,USB DAC:usb:48000,HDMI Out:hdmi:44100,Bare")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []struct {
		id, kind, rate string
	}{
		{"auto_null", "null", "48000"},
		{"USB DAC", "usb", "48000"},
		{"HDMI Out", "hdmi", "44100"},
		{"Bare", "builtin", "48000"}, // kind and rate default, name is the ID
	}
	if len(devs) != len(want) {
		t.Fatalf("got %d devices, want %d: %+v", len(devs), len(want), devs)
	}
	for i, w := range want {
		if devs[i].ID != w.id || string(devs[i].Kind) != w.kind || devs[i].SampleRate != w.rate {
			t.Errorf("device %d = %+v, want id %q kind %q rate %q", i, devs[i], w.id, w.kind, w.rate)
		}
	}
}

func TestParseFakeDevicesRejectsMalformedEntries(t *testing.T) {
	cases := map[string]string{
		"unknown kind":     "dac:bluetooth",
		"bad rate":         "dac:usb:forty-eight",
		"too many parts":   "a:b:c:d",
		"empty name":       ":usb",
		"empty list":       " , ",
		"no devices named": "",
	}
	for name, raw := range cases {
		if _, err := parseFakeDevices(raw); err == nil {
			t.Errorf("%s: %q parsed, want an error", name, raw)
		}
	}
}

// TestParseFakeDevicesCarriesTheBluetoothSinkID: the ADR-0091 selection
// persists the A2DP sink id (bluez_output.<MAC>.1), and the seam must carry
// it — the MAC's own five colons included — with the kind and rate parsed
// from the right, and a plain name claiming the bluetooth kind refused
// (the MAC behind a bluetooth sink is never guessed).
func TestParseFakeDevicesCarriesTheBluetoothSinkID(t *testing.T) {
	const sinkID = "bluez_output.AA:BB:CC:DD:EE:FF.1"
	devs, err := parseFakeDevices(sinkID + ":bluetooth:48000,auto_null")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(devs) != 2 || devs[0].ID != sinkID || devs[0].Kind != KindBluetooth || devs[0].SampleRate != "48000" {
		t.Fatalf("sink row = %+v, want id %q kind %q rate 48000", devs[0], sinkID, KindBluetooth)
	}
	if devs[1].ID != "auto_null" || devs[1].Kind != KindBuiltin {
		t.Fatalf("the plain entry after the sink id = %+v, want auto_null/builtin", devs[1])
	}
	// The kind is optional on the sink form (the MAC is the identity).
	devs, err = parseFakeDevices(sinkID + ":48000")
	if err != nil {
		t.Fatalf("parse (kind omitted): %v", err)
	}
	if devs[0].ID != sinkID {
		t.Fatalf("sink row id = %q, want %q", devs[0].ID, sinkID)
	}
	// A plain name with the bluetooth kind is a misconfiguration (the
	// first test's "dac:bluetooth" case pins this at the seam level).
	if _, err := parseFakeDevice("JBL:bluetooth"); err == nil {
		t.Fatal("a plain name with the bluetooth kind must be refused: the MAC is never guessed")
	}
}

func TestListDevicesReturnsTheLabelledSyntheticList(t *testing.T) {
	lookup := func(k string) (string, bool) {
		if k == EnvFakeDevices {
			return "null_sink:null:48000,USB DAC:usb:48000", true
		}
		return "", false
	}
	l, err := ListDevices(context.Background(), lookup)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if l.Backend != "synthetic" || !l.Synthetic {
		t.Fatalf("synthetic seam: backend %q synthetic %v, want synthetic/true", l.Backend, l.Synthetic)
	}
	if len(l.Devices) != 2 {
		t.Fatalf("got %d devices, want 2: %+v", len(l.Devices), l.Devices)
	}
	if !strings.Contains(l.Note, EnvFakeDevices) {
		t.Errorf("the synthetic list must name its seam in the note: %q", l.Note)
	}
	// An empty-but-set seam is "not set": it must not produce a synthetic
	// list of nothing.
	lookup2 := func(k string) (string, bool) {
		if k == EnvFakeDevices {
			return "  ", true
		}
		return "", false
	}
	// It falls through to the real backends (host-dependent), so assert only
	// that it is NOT the synthetic seam.
	l2, err := ListDevices(context.Background(), lookup2)
	if err != nil {
		t.Fatalf("list (blank seam): %v", err)
	}
	if l2.Synthetic {
		t.Errorf("a blank %s must not produce a synthetic list", EnvFakeDevices)
	}
	_ = l2
}

func TestListDevicesMalformedSeamIsAConfigurationError(t *testing.T) {
	lookup := func(k string) (string, bool) {
		if k == EnvFakeDevices {
			return "dac:bluetooth", true
		}
		return "", false
	}
	if _, err := ListDevices(context.Background(), lookup); err == nil {
		t.Fatal("a malformed synthetic seam must fail loudly (the misconfiguration must be loud)")
	}
}

// ── PipeWire parser (the long `pactl list sinks` form) ─────────────────────

const fakePactlSinks = `Sink #0
	State: RUNNING
	Name: alsa_output.pci-0000_00_1f.3.analog-stereo
	Description: Built-in Audio Analog Stereo
	Driver: PipeWire's Anonymous Client
	Specifications:
		Channel Map: front-left,front-right
		Sample Specification: float32le 2ch 48000Hz
		Cookie: 42
	Properties:
		api.alsa.card = "0"
		api.alsa.path = "front:0"
		device.bus = "pci"
		device.name = "alsa_card.pci-0000_00_1f.3"
		device.description = "Built-in Audio"
		factory.name = "spawners:0/alsa_sink"
		clock.rate = "48000"
	State: SUSPENDED
Sink #1
	Name: alsa_output.usb-Generic_USB_Audio-00.analog-stereo
	Description: Generic USB Audio Analog Stereo
	Sample Specification: s16le 2ch 44100Hz
	Properties:
		device.bus = "usb"
		device.name = "alsa_card.usb-Generic_USB_Audio-00"
		device.description = "Generic USB Audio"
		factory.name = "spawners:0/alsa_sink"
Sink #2
	Name: auto_null
	Description: Virtual Null Sink
	Sample Specification: float32le 2ch 48000Hz
	Properties:
		device.description = "Virtual Null Sink"
		factory.name = "libpulse"
`

const fakePactlInfo = `Server Version: 0.3.95
Default Sink: alsa_output.usb-Generic_USB_Audio-00.analog-stereo
Default Source: alsa_input.usb-Generic_USB_Audio-00.analog-stereo
`

// TestListPipeWireParsesBlocksAndKinds runs the real code path with a
// stand-in pactl that prints the fixture: the parser, not the host.
func TestListPipeWireParsesBlocksAndKinds(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "pactl")
	// The stand-in answers `pactl info` and `pactl list sinks` from the
	// fixtures; the branch lives in the script so the Go side only injects
	// the evidence.
	body := "#!/bin/sh\nif [ \"$1\" = \"info\" ]; then\ncat <<'EOF'\n" + fakePactlInfo + "EOF\nelse\ncat <<'EOF'\n" + fakePactlSinks + "EOF\nfi\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatalf("write fake pactl: %v", err)
	}

	l, err := listPipeWire(context.Background(), script)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if l.Backend != "pipewire" {
		t.Fatalf("backend %q, want pipewire", l.Backend)
	}
	if len(l.Devices) != 3 {
		t.Fatalf("got %d devices, want 3: %+v", len(l.Devices), l.Devices)
	}
	byID := map[string]Device{}
	for _, d := range l.Devices {
		byID[d.ID] = d
	}
	builtin := byID["alsa_output.pci-0000_00_1f.3.analog-stereo"]
	if builtin.Kind != KindBuiltin || builtin.Name != "Built-in Audio Analog Stereo" ||
		builtin.SampleRate != "48000" || builtin.Channels != 2 {
		t.Errorf("builtin row wrong: %+v", builtin)
	}
	usb := byID["alsa_output.usb-Generic_USB_Audio-00.analog-stereo"]
	if usb.Kind != KindUSB {
		t.Errorf("usb row kind %q, want usb: %+v", usb.Kind, usb)
	}
	if !usb.PlatformDefault {
		t.Errorf("the platform default sink must be flagged: %+v", usb)
	}
	nu := byID["auto_null"]
	if nu.Kind != KindNull {
		t.Errorf("null sink kind %q, want null: %+v", nu.Kind, nu)
	}
	if nu.SampleRate != "48000" {
		t.Errorf("null sink sample rate %q, want 48000: %+v", nu.SampleRate, nu)
	}
}

func TestListPipeWireFailsClosedOnNoSinks(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "pactl")
	body := "#!/bin/sh\nif [ \"$1\" = \"info\" ]; then echo 'Default Sink: x'; fi\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatalf("write fake pactl: %v", err)
	}
	if _, err := listPipeWire(context.Background(), script); err == nil {
		t.Fatal("no sinks in the listing must be an error, not an empty world")
	}
}

func TestParseDefaultSinkTakesTheNameAfterTheSlash(t *testing.T) {
	cases := map[string]string{
		"Default Sink: usb-dac.analog-stereo\n":   "usb-dac.analog-stereo",
		"Default Sink: 1/usb-dac.analog-stereo\n": "usb-dac.analog-stereo",
		"no default line\n":                       "",
	}
	for in, want := range cases {
		if got := parseDefaultSink(in); got != want {
			t.Errorf("parseDefaultSink(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestInferKindOrderingAndUnknown(t *testing.T) {
	cases := []struct {
		evidence []string
		want     Kind
	}{
		{[]string{"alsa_output.usb-Null_Dongle-00"}, KindNull}, // null beats usb
		{[]string{"alsa_output.hdmi-00"}, KindHDMI},
		{[]string{"dp-1", "DisplayPort"}, KindHDMI},
		{[]string{"usb-USB_DAC-00", "UAC 2.0"}, KindUSB},
		{[]string{"alsa_output.pci-analog"}, KindBuiltin},
		{[]string{"weird-device-xyz"}, KindBuiltin},
	}
	for _, c := range cases {
		if got := inferKind(c.evidence); got != c.want {
			t.Errorf("inferKind(%v) = %q, want %q", c.evidence, got, c.want)
		}
	}
}

func TestParseSampleSpec(t *testing.T) {
	rate, ch, ok := parseSampleSpec("float32le 2ch 48000Hz")
	if !ok || rate != "48000" || ch != 2 {
		t.Errorf("got (%q, %d, %v), want (48000, 2, true)", rate, ch, ok)
	}
	if _, _, ok := parseSampleSpec("garbage"); ok {
		t.Error("a malformed sample spec must not parse")
	}
}

// ── ALSA parser ────────────────────────────────────────────────────────────

func TestSplitALSAline(t *testing.T) {
	name, comment := splitALSAline("hw:0,0    # HDMI output 0")
	if name != "hw:0,0" || comment != "HDMI output 0" {
		t.Errorf("got (%q, %q)", name, comment)
	}
	name, comment = splitALSAline("null")
	if name != "null" || comment != "" {
		t.Errorf("got (%q, %q)", name, comment)
	}
}

func TestInferALSAKind(t *testing.T) {
	if got := inferALSAKind("null", ""); got != KindNull {
		t.Errorf("null = %q, want null", got)
	}
	if got := inferALSAKind("default", ""); got != KindBuiltin {
		t.Errorf("default = %q, want builtin", got)
	}
	if got := inferALSAKind("iec958:0", ""); got != KindHDMI {
		t.Errorf("iec958 = %q, want hdmi", got)
	}
	if got := inferALSAKind("hw:1,0", "USB DAC output"); got != KindUSB {
		t.Errorf("hw with usb comment = %q, want usb", got)
	}
}
