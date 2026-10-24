package doctor

import (
	"strings"
	"testing"
)

// --- parsers (issue #61) ---------------------------------------------------

const pwSettingsStock = "update: id:0 key:'default.clock.rate' value:'48000' type:''\n" +
	"update: id:0 key:'default.clock.quantum' value:'1024' type:''\n" +
	"update: id:0 key:'default.clock.min-quantum' value:'1024' type:''\n" +
	"update: id:0 key:'default.clock.max-quantum' value:'2048' type:''\n" +
	"unrelated: id:7 key:'some.setting' value:'garbage' type:''\n"

const pwSettingsBareKeys = "update: id:0 key:'clock.rate' value:'48000' type:''\n" +
	"update: id:0 key:'clock.quantum' value:'1024' type:''\n" +
	"update: id:0 key:'clock.min-quantum' value:'32' type:''\n" +
	"update: id:0 key:'clock.max-quantum' value:'2048' type:''\n"

func TestParsePwMetadataSettings(t *testing.T) {
	got := parsePwMetadataSettings([]byte(pwSettingsStock))
	want := pipewireClock{rate: 48000, quantum: 1024, minQuant: 1024, maxQuant: 2048}
	if got != want {
		t.Fatalf("parsed = %+v, want %+v", got, want)
	}
	got = parsePwMetadataSettings([]byte(pwSettingsBareKeys))
	want = pipewireClock{rate: 48000, quantum: 1024, minQuant: 32, maxQuant: 2048}
	if got != want {
		t.Fatalf("bare-key form parsed = %+v, want %+v", got, want)
	}
	// The default.* form wins when both are present.
	mixed := pwSettingsBareKeys + "update: id:0 key:'default.clock.min-quantum' value:'512' type:''\n"
	got = parsePwMetadataSettings([]byte(mixed))
	if got.minQuant != 512 {
		t.Fatalf("minQuant = %d, want 512 (default.* beats bare clock.*)", got.minQuant)
	}
	if got.rate != 48000 {
		t.Fatalf("rate = %d, want 48000 from bare clock.rate", got.rate)
	}
	// Junk input must not panic and must not invent numbers.
	if c := parsePwMetadataSettings([]byte("not pipewire output\n")); !c.empty() {
		t.Fatalf("junk parsed = %+v, want empty", c)
	}
}

// btSinkBlock is a recorded-shape `pactl list sinks` block for a connected
// Bluetooth sink: all strings are invented, no device names or addresses
// (AGENTS.md rule 5).
const btSinkBlock = "Sink #7\n" +
	"\tState: RUNNING\n" +
	"\tName: bluez_output.fake_bt\n" +
	"\tDescription: Fake Speaker (BT) Audio/Sink sink\n" +
	"\tSample Specification: s16le 2ch 48000Hz\n" +
	"\tChannel Map: front-left,front-right\n" +
	"\tOwner Module: 12\n" +
	"\tMute: no\n" +
	"\tMonitor Source: bluez_output.fake_bt.monitor\n" +
	"\tProperties:\n" +
	"\t\tnode.name = \"bluez_output.fake_bt\"\n" +
	"\t\tmedia.class = \"Audio/Sink\"\n" +
	"\t\tbluez5.path = \"btdevice\"\n" +
	"\t\tbluez5.profile = \"auto\"\n" +
	"\t\tapi.bluez5.codec = \"sbc\"\n" +
	"\tFormats:\n" +
	"\t\tpcm\n"

const wiredSinkBlock = "Sink #3\n" +
	"\tState: RUNNING\n" +
	"\tName: alsa_output.fake_card\n" +
	"\tDescription: Wired Sink\n" +
	"\tSample Specification: s16le 2ch 48000Hz\n" +
	"\tProperties:\n" +
	"\t\tnode.name = \"alsa_output.fake_card\"\n" +
	"\t\tmedia.class = \"Audio/Sink\"\n" +
	"\tFormats:\n" +
	"\t\tpcm\n"

func TestBtSinkDetectsProfileAndCodec(t *testing.T) {
	profile, codec, isBT := btSink(btSinkBlock)
	if !isBT {
		t.Fatalf("btSink(BT block) isBT = false, want true")
	}
	if profile != "auto" || codec != "sbc" {
		t.Fatalf("profile/codec = %q/%q, want auto/sbc", profile, codec)
	}
	if _, _, isBT = btSink(wiredSinkBlock); isBT {
		t.Fatalf("btSink(wired block) isBT = true, want false")
	}
}

func TestBtDefaultSinkPrefersNamedDefault(t *testing.T) {
	blocks := sinkBlocks([]byte(wiredSinkBlock + "\n" + btSinkBlock))
	// Named default is the wired sink: the BT one must not stand in.
	if _, _, isBT := btDefaultSink("alsa_output.fake_card", blocks); isBT {
		t.Fatalf("default sink is wired, btDefaultSink reported BT")
	}
	// Named default is the BT sink.
	profile, codec, isBT := btDefaultSink("bluez_output.fake_bt", blocks)
	if !isBT || profile != "auto" || codec != "sbc" {
		t.Fatalf("got (%q %q %v), want (auto sbc true)", profile, codec, isBT)
	}
	// Unknown default with a BT sink connected: the BT sink stands in.
	profile, codec, isBT = btDefaultSink("no_such_sink", blocks)
	if !isBT || profile != "auto" || codec != "sbc" {
		t.Fatalf("fallback got (%q %q %v), want (auto sbc true)", profile, codec, isBT)
	}
}

// --- the check itself -------------------------------------------------------

func pipewireBT(t *testing.T, minQuantum string) *fakeHost {
	t.Helper()
	h := newFakeHost(t)
	settings := "update: id:0 key:'default.clock.rate' value:'48000' type:''\n" +
		"update: id:0 key:'default.clock.quantum' value:'1024' type:''\n" +
		"update: id:0 key:'default.clock.min-quantum' value:'" + minQuantum + "' type:''\n" +
		"update: id:0 key:'default.clock.max-quantum' value:'2048' type:''\n"
	h.exec("pw-metadata -n settings", settings)
	h.exec("pactl info", "Server String: /run/user/1000/pulse/native\nDefault Sink: bluez_output.fake_bt\nDefault Source: none\n")
	h.exec("pactl list sinks", btSinkBlock+"\n"+wiredSinkBlock+"\n")
	return h
}

func TestPipeWireBtAtStockMinQuantumWarns(t *testing.T) {
	h := pipewireBT(t, "1024")
	c := h.check(h.run(), "audio.pipewire")
	if c.Status != StatusDegraded {
		t.Fatalf("status = %q, want DEGRADED at the stock min-quantum with a BT default sink: %+v", c.Status, c)
	}
	for _, want := range []string{"default.clock.min-quantum", "bluez5.codec_config", "sbc"} {
		if !strings.Contains(c.Remedy+"; "+c.Detail, want) {
			t.Errorf("report must name %q; remedy=%q detail=%q", want, c.Remedy, c.Detail)
		}
	}
}

func TestPipeWireBtAtRaisedMinQuantumIsOK(t *testing.T) {
	h := pipewireBT(t, "32")
	c := h.check(h.run(), "audio.pipewire")
	if c.Status != StatusOK {
		t.Fatalf("status = %q, want OK with a raised min-quantum: %+v", c.Status, c)
	}
	if !strings.Contains(c.Detail, "sbc") || !strings.Contains(c.Detail, "48000") {
		t.Errorf("detail must carry the codec and rate; got %q", c.Detail)
	}
}

func TestPipeWireAbsentIsNotConfigured(t *testing.T) {
	h := newFakeHost(t)
	h.withoutTools("pw-metadata", "pactl")
	c := h.check(h.run(), "audio.pipewire")
	if c.Status != StatusNotConfigured {
		t.Fatalf("status = %q, want NOT CONFIGURED when no PipeWire tooling: %+v", c.Status, c)
	}
}

func TestPipeWireOnlyPactlBtUnverifiableWarns(t *testing.T) {
	// pipewire-pulse without the pw-* CLI: the clock floor is unreadable, so a
	// live BT transport is reported, not verified.
	h := newFakeHost(t)
	h.withoutTools("pw-metadata")
	h.exec("pactl info", "Server String: /run/user/1000/pulse/native\nDefault Sink: bluez_output.fake_bt\n")
	h.exec("pactl list sinks", btSinkBlock+"\n")
	c := h.check(h.run(), "audio.pipewire")
	if c.Status != StatusDegraded {
		t.Fatalf("status = %q, want DEGRADED (BT active, quantum unverifiable): %+v", c.Status, c)
	}
	if !strings.Contains(c.Detail, "min-quantum not readable") {
		t.Errorf("detail must say the min-quantum is unreadable; got %q", c.Detail)
	}
}

func TestPipeWireSettingsProbeFailureDegrades(t *testing.T) {
	h := newFakeHost(t)
	h.execFail("pw-metadata -n settings", "connection refused")
	c := h.check(h.run(), "audio.pipewire")
	if c.Status != StatusDegraded {
		t.Fatalf("status = %q, want DEGRADED when the settings probe fails: %+v", c.Status, c)
	}
}

func TestEmulatorProbeRunsDisplayFree(t *testing.T) {
	// issue #62: the doctor's version probe must reach the binary with the
	// offscreen env, or the installed build core-dumps the check.
	h := newFakeHost(t)
	h.opts.Dev = true
	h.run()
	found := false
	for _, call := range h.calls {
		if call == "dolphin-emu --version env=QT_QPA_PLATFORM=offscreen" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no display-free version probe in calls:\n%v", h.calls)
	}
}
