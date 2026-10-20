package bt

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestNormalizeAddress(t *testing.T) {
	for _, tc := range []struct {
		raw, want string
		ok        bool
	}{
		{"AA:BB:CC:DD:EE:FF", "AA:BB:CC:DD:EE:FF", true},
		{"aa:bb:cc:dd:ee:ff", "AA:BB:CC:DD:EE:FF", true},
		{" aa:bb:cc:dd:ee:ff ", "AA:BB:CC:DD:EE:FF", true},
		{"AA-BB-CC-DD-EE-FF", "", false},
		{"AA:BB:CC:DD:EE", "", false},
		{"AA:BB:CC:DD:EE:FF:00", "", false},
		{"", "", false},
		{"GG:BB:CC:DD:EE:FF", "", false},
	} {
		got, err := NormalizeAddress(tc.raw)
		if tc.ok && err != nil {
			t.Errorf("NormalizeAddress(%q) = %v, want %q", tc.raw, err, tc.want)
		}
		if !tc.ok && err == nil {
			t.Errorf("NormalizeAddress(%q) = %q, want an error", tc.raw, got)
		}
		if tc.ok && got != tc.want {
			t.Errorf("NormalizeAddress(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestSinkIDRoundTrip(t *testing.T) {
	addr := "AA:BB:CC:DD:EE:FF"
	id := SinkID(addr)
	if id != "bluez_output."+addr+".1" {
		t.Fatalf("SinkID = %q", id)
	}
	got, ok := ParseSinkID(id)
	if !ok || got != addr {
		t.Fatalf("ParseSinkID(%q) = %q, %v", id, got, ok)
	}
	// Non-BlueZ sink names do not parse.
	for _, s := range []string{"default", "auto_null", "alsa_output.platform-sound.HDMI", "bluez_output.", "bluez_output.AA:BB:CC:DD:EE:FF", "usb_dac.1"} {
		if _, ok := ParseSinkID(s); ok {
			t.Errorf("ParseSinkID(%q) parsed, want it refused", s)
		}
	}
}

func TestFakeScanPairConnectLifecycle(t *testing.T) {
	ctx := context.Background()
	const mac = "AA:BB:CC:DD:EE:FF"
	fake := NewFake(FakeDevice{
		Address: mac, Alias: "JBL Tune 770NC",
		UUIDs: []string{UUIDA2DP}, Discoverable: true,
	})
	defer fake.Close()

	// Before the scan: nothing is known (it was not seeded known).
	devs, err := fake.Devices(ctx)
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	if len(devs) != 0 {
		t.Fatalf("Devices before scan = %d, want 0", len(devs))
	}

	// Scan finds it; the row is unpaired and audio-capable.
	if err := fake.Scan(ctx, 20); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	devs, _ = fake.Devices(ctx)
	if len(devs) != 1 {
		t.Fatalf("Devices after scan = %d, want 1", len(devs))
	}
	if !devs[0].HasAudioSink() {
		t.Error("the scanned device is not audio-capable, want the A2DP UUID")
	}

	// Pair: paired+trusted, the ops log records it.
	if err := fake.Pair(ctx, mac); err != nil {
		t.Fatalf("Pair: %v", err)
	}
	devs, _ = fake.Devices(ctx)
	if !devs[0].Paired || !devs[0].Trusted {
		t.Errorf("after Pair: paired=%v trusted=%v, want both true", devs[0].Paired, devs[0].Trusted)
	}

	// Connect: the sink appears (A2DP still advertised).
	if err := fake.Connect(ctx, mac); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	devs, _ = fake.Devices(ctx)
	if !devs[0].Connected || !devs[0].HasAudioSink() {
		t.Errorf("after Connect: connected=%v a2dp=%v, want both true", devs[0].Connected, devs[0].HasAudioSink())
	}

	// Disconnect + forget round trip.
	if err := fake.Disconnect(ctx, mac); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if err := fake.Forget(ctx, mac); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	devs, _ = fake.Devices(ctx)
	if len(devs) != 0 {
		t.Fatalf("after Forget: %d devices, want 0", len(devs))
	}
	// Forget on an unknown device is an error (not a silent no-op): the
	// API's 404 relies on it.
	if err := fake.Forget(ctx, mac); err == nil {
		t.Error("Forget on a forgotten device: want an error")
	}
}

func TestFakeLEOnlyGotchaAndRepair(t *testing.T) {
	ctx := context.Background()
	const mac = "11:22:33:44:55:66"
	fake := NewFake(FakeDevice{
		Address: mac, Alias: "Dual-Mode Headset",
		UUIDs:        []string{UUIDA2DP, "0000110e-0000-1000-8000-002b34566a7e"},
		Discoverable: true, LEOnly: true,
	})
	defer fake.Close()
	if err := fake.Scan(ctx, 20); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	// The regression of issue #3: pair+connect "succeeds", but the A2DP
	// unit never comes up — connected, no audio sink.
	if err := fake.Pair(ctx, mac); err != nil {
		t.Fatalf("Pair: %v", err)
	}
	if err := fake.Connect(ctx, mac); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	devs, _ := fake.Devices(ctx)
	if len(devs) != 1 {
		t.Fatalf("Devices = %d, want 1", len(devs))
	}
	if !devs[0].Connected {
		t.Error("the device is connected=true (bluez reports success)")
	}
	if devs[0].HasAudioSink() {
		t.Error("the device has no A2DP unit in the LE-only state: A2DPSink must be false")
	}

	// Repair restores the classic connection: paired, trusted,
	// connected, and the A2DP sink is back.
	if err := fake.Repair(ctx, mac); err != nil {
		t.Fatalf("Repair: %v", err)
	}
	devs, _ = fake.Devices(ctx)
	if !devs[0].Connected || !devs[0].Paired || !devs[0].HasAudioSink() {
		t.Errorf("after Repair: connected=%v paired=%v a2dp=%v, want all true",
			devs[0].Connected, devs[0].Paired, devs[0].HasAudioSink())
	}

	// Repair on a device that is not in pairing mode fails with the
	// pairing-mode instruction (the human step the appliance cannot do).
	fake2 := NewFake(FakeDevice{
		Address: "77:88:99:AA:BB:CC", Alias: "Asleep Headset",
		UUIDs: []string{UUIDA2DP}, Known: true, // known, but not discoverable
	})
	if err := fake2.Repair(ctx, "77:88:99:AA:BB:CC"); err == nil {
		t.Fatal("Repair on a non-discoverable device: want an error")
	} else if !strings.Contains(err.Error(), "pairing mode") {
		t.Errorf("Repair error %q does not carry the pairing-mode instruction", err)
	}
}

func TestFakePoweredOff(t *testing.T) {
	ctx := context.Background()
	fake := NewFake(FakeDevice{Address: "AA:BB:CC:DD:EE:FF", Discoverable: true})
	fake.SetPowered(false)
	st, err := fake.State(ctx)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.Powered {
		t.Error("State.Powered = true after SetPowered(false)")
	}
	if _, err := fake.Devices(ctx); err == nil {
		t.Error("Devices with the adapter off: want an error")
	}
	if err := fake.Scan(ctx, 20); err == nil {
		t.Error("Scan with the adapter off: want an error")
	}
	if err := fake.Pair(ctx, "AA:BB:CC:DD:EE:FF"); err == nil {
		t.Error("Pair with the adapter off: want an error")
	}
	// Back on.
	fake.SetPowered(true)
	if err := fake.Scan(ctx, 20); err != nil {
		t.Fatalf("Scan after power-on: %v", err)
	}
}

func TestFakeIsAController(t *testing.T) {
	// Compile-time seam check: the fake satisfies the interface the API
	// programs against (a drift between the two is a build error).
	var _ Controller = (*Fake)(nil)
	_ = errors.New
}
