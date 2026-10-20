// Package bt is the Bluetooth (BlueZ) surface of the output-audio product
// (issues #2/#3, ADR-0087): the controller state, the audio-capable device
// inventory and the pairing lifecycle — scan, pair, trust, connect,
// disconnect, forget, and the re-pair repair for the "connected but no
// audio sink" state.
//
// The product rule (AGENTS.md rule 6) is that the appliance never shells out
// to bluetoothctl from a web-triggered path: every operation is a BlueZ
// D-Bus call (org.bluez) made by this package through the system bus.
// Bluetooth stays optional and best-effort (docs/spec/07: USB audio is the
// baseline); the package degrades — never guesses — when the bus or the
// daemon is absent, and the API answers 503 so the page says "not wired"
// instead of "no devices".
//
// The Controller interface is the seam: the real client (client.go) talks
// D-Bus, the fake (fake.go) is the deterministic stand-in for unit tests,
// the API tests and the Playwright walk (the same labelled-synthetic
// pattern as GAME_NARRATOR_AUDIO_FAKE_DEVICES, ADR-0074).
package bt

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// The A2DP service UUIDs (BlueZ reports them lower-case). A remote device
// advertising the A2DP UUID is the sink for our narration: the audio flows
// host → device. The A2DP *Source* UUID means the opposite direction (the
// device would play into us) — reported, but it is not a narration output.
const (
	UUIDA2DP       = "0000110b-0000-1000-8000-002b34566a7e"
	UUIDA2DPSource = "0000110c-0000-1000-8000-002b34566a7e"
)

// agentCapability is the BlueZ agent capability the appliance registers
// with (org.bluez.AgentManager1.RegisterAgent): no input, no output — the
// appliance has no display to show a passkey and no keyboard to type one.
// Most headsets pair just-works; a device that demands a PIN cannot be
// paired from the appliance, and the Pair error is surfaced rather than
// hidden.
const agentCapability = "NoInputNoOutput"

// SinkPrefix is the WirePlumber node-name prefix of the A2DP sink of a
// device: `bluez_output.<ADDRESS>.1` (the ADDRESS is the colon-form MAC,
// the .1 suffix is the codec instance). It is the stable identity the
// audio device selection (ADR-0074) persists, and the pattern the
// auto-reconnect reads back out of a stored selection.
const SinkPrefix = "bluez_output."

var addressRe = regexp.MustCompile(`^([0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}$`)

// NormalizeAddress upper-cases and validates a Bluetooth MAC address. It
// is the single spelling rule for the API's {mac} path segment: an address
// that does not pass is a 400, not a D-Bus call with a typo in it.
func NormalizeAddress(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if !addressRe.MatchString(raw) {
		return "", fmt.Errorf("address %q is not a Bluetooth MAC (want AA:BB:CC:DD:EE:FF)", raw)
	}
	return strings.ToUpper(raw), nil
}

// SinkID is the stable PipeWire identity of one device's A2DP sink (the
// identity the audio selection persists, ADR-0074). address must already
// be colon-form upper-case.
func SinkID(address string) string { return SinkPrefix + address + ".1" }

// ParseSinkID reads a sink identity back to (address, true) when it names
// a BlueZ A2DP sink node, ("", false) otherwise. It is the inverse of
// SinkID used by the auto-reconnect: the selection stores the sink name,
// the reconnect needs the MAC.
func ParseSinkID(sink string) (string, bool) {
	if !strings.HasPrefix(sink, SinkPrefix) {
		return "", false
	}
	rest := strings.TrimPrefix(sink, SinkPrefix)
	// The node name is <ADDRESS>.<n>: the address itself carries colons, so
	// the instance number is the last dot-separated segment.
	i := strings.LastIndex(rest, ".")
	if i < 0 {
		return "", false
	}
	addr, inst := rest[:i], rest[i+1:]
	if inst == "" {
		return "", false
	}
	addr, err := NormalizeAddress(addr)
	if err != nil {
		return "", false
	}
	return addr, true
}

// State is the adapter's (the on-board controller's) condition.
type State struct {
	// Powered is the adapter's Powered property: false means nothing below
	// can work, and the page says so with the exact remediation.
	Powered bool `json:"powered"`
	// Discovering is the adapter's Discovering property: a scan is in
	// progress (ours or another client's).
	Discovering bool `json:"discovering"`
	// Alias is the adapter's friendly name (the system name) — the label
	// the page shows next to the controller badge.
	Alias string `json:"alias,omitempty"`
	// Note carries a controller-level finding ("bluez is not running",
	// "the adapter is powered off") the page can surface verbatim.
	Note string `json:"note,omitempty"`
}

// Device is one remote device row: the stable identity (the MAC address —
// never the object path, which is a per-boot allocation), the pairing
// state BlueZ reports, and the audio capability the UUIDs prove.
type Device struct {
	// Address is the colon-form MAC — the stable identity the selection
	// and the API address.
	Address string `json:"address"`
	// Alias is BlueZ's display name (the remote name when no alias is
	// set); empty while the name has not resolved.
	Alias string `json:"alias,omitempty"`
	// Paired, Trusted and Connected are BlueZ's own Device1 properties —
	// reported, never inferred.
	Paired    bool `json:"paired"`
	Trusted   bool `json:"trusted"`
	Connected bool `json:"connected"`
	// A2DPSink is true when the device advertises the A2DP service UUID:
	// it can receive our audio. That is the audio-capable filter the
	// inventory applies.
	A2DPSink bool `json:"a2dp_sink"`
	// A2DPSource is true when it advertises the A2DP Source UUID (the
	// reverse direction); reported for the "why isn't this a sink"
	// question, not used by the selection.
	A2DPSource bool `json:"a2dp_source"`
	// RSSI is the last measured signal strength (when BlueZ reports it).
	RSSI int16 `json:"rssi,omitempty"`
	// Discovered is true when the device was seen in the most recent scan
	// (whether or not it is paired): a scan that finds only unpaired
	// devices is the "put the headset in pairing mode" evidence.
	Discovered bool `json:"discovered,omitempty"`
}

// HasAudioSink reports whether the device advertises the A2DP service
// (the audio-capable filter of issue #2: "list audio-capable devices").
func (d Device) HasAudioSink() bool { return d.A2DPSink }

// Controller is the seam between the API and BlueZ. The real implementation
// is the D-Bus client (client.go); the fake (fake.go) is the deterministic
// test stand-in. All methods are idempotent where BlueZ's are: Connect on a
// connected device is a no-op answer, Forget on an unknown device is an
// error the caller reports.
type Controller interface {
	// State reports the adapter condition.
	State(ctx context.Context) (State, error)
	// Scan runs a time-bounded discovery (the BlueZ discovery session is
	// held for at most duration, then released) and leaves the inventory
	// reflecting what it found. The caller reads the result with Devices;
	// a scan that finds nothing is not an error — the headset may simply
	// not be in pairing mode, and the page says so.
	Scan(ctx context.Context, duration time.Duration) error
	// Devices lists the known devices: every device BlueZ has a record of
	// (paired or merely discovered), with the pairing state and the UUID
	// evidence.
	Devices(ctx context.Context) ([]Device, error)
	// Pair initiates pairing (with the appliance's NoInputNoOutput agent)
	// and, once paired, sets the device trusted — the proven recipe's
	// `pair` + `trust` in one product operation (issues #2/#3: the
	// hand-verified sequence is pair, trust, connect).
	Pair(ctx context.Context, address string) error
	// Trust sets (or clears) the Trusted property.
	Trust(ctx context.Context, address string, trusted bool) error
	// Connect connects the device's connectable profiles (the A2DP
	// sink appears in the user's PipeWire once this succeeds).
	Connect(ctx context.Context, address string) error
	// Disconnect drops the device's connections.
	Disconnect(ctx context.Context, address string) error
	// Forget removes the device and all its pairing information (BlueZ
	// Adapter1.RemoveDevice) — the first step of the re-pair repair.
	Forget(ctx context.Context, address string) error
	// Repair is the "connected but no audio sink" path of issue #3: the
	// dual-mode headset came back over LE only (GATT reads, no A2DP
	// unit). The repair forgets the device, rescans forcing the classic
	// (BR/EDR) transport, and re-pairs it — the hand-verified fix,
	// productized.
	Repair(ctx context.Context, address string) error
	// Close releases the controller's bus connection (and unregisters its
	// agent). A closed controller answers errors on every later call.
	Close() error
}
