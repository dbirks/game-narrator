// Fake is the deterministic test stand-in for the BlueZ controller —
// the labelled-synthetic seam of the API layer (the same pattern as
// GAME_NARRATOR_AUDIO_FAKE_DEVICES, ADR-0074). It exists in three places:
//
//   - the unit and API tests (a known device table, no bus, no timing);
//   - the Playwright walk (GAME_NARRATOR_BT_FAKE seeds the serve binary
//     with one pairing-mode headset, so the UI surface is exercised end
//     to end without a radio — the response's fake flag keeps the page
//     from presenting it as hardware);
//   - the private-bus integration test's counterpart, where the REAL
//     client is pointed at a fake BlueZ *D-Bus service* instead (that is
//     a different class of evidence: the D-Bus wiring, not the state
//     machine).
//
// The state machine mirrors the proven recipe, including the issue-#3
// LE-only gotcha: an LEOnly device that is paired/connected reports no
// A2DP sink (the GATT-only connection), and only Repair restores it.
package bt

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// FakeDevice is one seeded device of the fake controller.
type FakeDevice struct {
	// Address is the colon-form MAC (the stable identity).
	Address string
	// Alias is the display name.
	Alias string
	// UUIDs is the advertised service set (the A2DP UUID makes it a
	// narration output).
	UUIDs []string
	// Paired, Trusted, Connected are the initial states.
	Paired    bool
	Trusted   bool
	Connected bool
	// Discoverable means the device answers a scan (it is powered on /
	// in pairing mode). A non-discoverable device is only reachable if a
	// test seeds it as known.
	Discoverable bool
	// LEOnly simulates the dual-mode regression of issue #3: after a
	// Connect the device is "connected" with no A2DP unit (br-connection-
	// unknown) until a Repair re-pairs it over the classic transport.
	LEOnly bool
	// Known seeds the device into the controller's record even when it is
	// not discoverable (a previously seen device).
	Known bool
}

// Fake is a deterministic, in-memory Controller.
type Fake struct {
	mu       sync.Mutex
	powered  bool
	devices  map[string]*fakeDev
	discover map[string]*fakeDev
	ops      []string
}

type fakeDev struct {
	addr      string
	alias     string
	uuids     []string
	paired    bool
	trusted   bool
	connected bool
	leOnly    bool
	// discovered marks "seen in a scan" (the Device.Discovered row flag):
	// a device that merely advertises is not in the inventory at all —
	// bluez has no record of it until the inquiry finds it.
	discovered bool
}

// NewFake builds the fake controller from seeded devices. A seed is
// "known" (in the controller's record, answering Devices before any scan)
// when it is marked Known, Paired or Connected; a Discoverable-only seed is
// merely advertising — the scan is what puts it in the record, the way the
// real inquiry does.
func NewFake(seed ...FakeDevice) *Fake {
	f := &Fake{
		powered:  true,
		devices:  map[string]*fakeDev{},
		discover: map[string]*fakeDev{},
	}
	for _, d := range seed {
		addr, _ := NormalizeAddress(d.Address)
		fv := &fakeDev{
			addr:      addr,
			alias:     d.Alias,
			uuids:     d.UUIDs,
			paired:    d.Paired,
			trusted:   d.Trusted,
			connected: d.Connected,
			leOnly:    d.LEOnly,
		}
		// A seed already in the GATT-only state (issue #3's regression)
		// carries no A2DP unit: strip it up front, the way a real LE-only
		// connect would have.
		if d.LEOnly && (d.Connected || d.Paired) {
			fv.uuids = stripA2DP(fv.uuids)
		}
		if d.Known || d.Paired || d.Connected {
			f.devices[addr] = fv
		}
		if d.Discoverable {
			f.discover[addr] = fv
		}
	}
	return f
}

// DefaultFakeMAC and DefaultFakeMAC2 are the synthetic inventory's addresses.
// They are obviously-fake on purpose: the public repository never commits
// real hardware addresses (the fixtures rule), and the walk's assertions are
// about the flow, not the address.
const (
	DefaultFakeMAC  = "AA:BB:CC:DD:EE:FF"
	DefaultFakeMAC2 = "11:22:33:44:55:66"
	// DefaultFakeMACLeOnly is the issue-#3 fixture: the dual-mode headset
	// that comes back over the LE transport only (connected, no A2DP unit).
	DefaultFakeMACLeOnly = "99:88:77:66:55:44"
)

// DefaultFake is the process-level seam's labelled-synthetic inventory
// (GAME_NARRATOR_BT_FAKE, the UI-walk counterpart of the capture fake's
// fixed device): one A2DP headset in the full success state (paired,
// trusted, connected, the A2DP unit up), a discoverable-only pairable
// device for the pair/connect/forget walk, and the issue-#3 state (paired,
// connected, LE-only — the "connected but no A2DP sink" the repair button
// exists for). No radio is touched.
func DefaultFake() *Fake {
	return NewFake(
		FakeDevice{
			Address: DefaultFakeMAC, Alias: "Synthetic A2DP Headset",
			UUIDs: []string{UUIDA2DP}, Paired: true, Trusted: true, Connected: true,
		},
		FakeDevice{
			Address: DefaultFakeMAC2, Alias: "Synthetic Pairable Buds",
			UUIDs:        []string{UUIDA2DP},
			Discoverable: true,
		},
		FakeDevice{
			Address:      DefaultFakeMACLeOnly,
			Alias:        "Synthetic Dual-mode Headset",
			UUIDs:        []string{UUIDA2DP},
			Paired:       true,
			Trusted:      true,
			Connected:    true,
			LEOnly:       true,
			Discoverable: true, // the operator's pairing-mode step the repair's rescan needs
		},
	)
}

// SetPowered switches the fake adapter (the "controller is off" test
// state: every operation then answers ErrNotAvailable-ish errors).
func (f *Fake) SetPowered(powered bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.powered = powered
}

// Synthetic reports that this controller is the labelled-synthetic stand-in
// (the ADR-0074 pattern: a fake answer must never be mistaken for hardware).
// The API and the doctor ask for it through a type assertion, so the real
// Client (which has no such method) answers false.
func (f *Fake) Synthetic() bool { return true }

// Ops returns the operation log (for test assertions on the order of the
// state-changing calls).
func (f *Fake) Ops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.ops))
	copy(out, f.ops)
	return out
}

func (f *Fake) record(op string) { f.ops = append(f.ops, op) }

func (f *Fake) checkPowered() error {
	if !f.powered {
		// The same sentinel the real client returns when the bus or the
		// daemon cannot answer, so the API's 503 mapping is one rule.
		return fmt.Errorf("%w: the adapter is powered off", ErrNotAvailable)
	}
	return nil
}

// State reports the fake adapter's condition.
func (f *Fake) State(ctx context.Context) (State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := State{Powered: f.powered}
	if f.powered {
		st.Alias = "game-narrator"
	} else {
		st.Note = "the adapter is powered off"
	}
	return st, nil
}

// Devices lists the known devices.
func (f *Fake) Devices(ctx context.Context) ([]Device, error) {
	if err := f.checkPowered(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	devs := make([]Device, 0, len(f.devices))
	for _, d := range f.devices {
		devs = append(devs, f.deviceRow(d))
	}
	sortDevices(devs)
	return devs, nil
}

// Scan makes the discoverable devices known (and marks them Discovered).
func (f *Fake) Scan(ctx context.Context, duration time.Duration) error {
	if err := f.checkPowered(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("scan")
	for addr, d := range f.discover {
		if _, known := f.devices[addr]; !known {
			f.devices[addr] = d
		}
		// The device answered the inquiry: it was seen, known or not.
		f.devices[addr].discovered = true
	}
	return nil
}

// Pair pairs (and trusts) a known-or-discoverable device.
func (f *Fake) Pair(ctx context.Context, address string) error {
	if err := f.checkPowered(); err != nil {
		return err
	}
	addr, err := NormalizeAddress(address)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.devices[addr]
	if !ok {
		if d2, disc := f.discover[addr]; disc {
			d, ok = d2, true
		}
	}
	if !ok {
		return fmt.Errorf("device %s is not known to bluez (scan, or pair it first): %w", addr, ErrDeviceUnknown)
	}
	f.record("pair " + addr)
	d.paired = true
	d.trusted = true
	d.discovered = true
	// A paired device is in the controller's record from here on (bluez's
	// ObjectManager tree): promote it out of the merely-discoverable set,
	// the way the real daemon does once the pairing lands.
	f.devices[addr] = d
	// The LE-only regression: the pairing "succeeds" over the LE bearer
	// and the A2DP unit never comes up.
	if d.leOnly {
		d.uuids = stripA2DP(d.uuids)
	}
	return nil
}

// Trust sets or clears trust.
func (f *Fake) Trust(ctx context.Context, address string, trusted bool) error {
	if err := f.checkPowered(); err != nil {
		return err
	}
	addr, err := NormalizeAddress(address)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.devices[addr]
	if !ok {
		return fmt.Errorf("device %s is not known to bluez: %w", addr, ErrDeviceUnknown)
	}
	f.record(fmt.Sprintf("trust %s %t", addr, trusted))
	d.trusted = trusted
	return nil
}

// Connect connects the device. An LEOnly device connects with no A2DP
// sink — the "connected but no audio sink" state the UI must detect.
func (f *Fake) Connect(ctx context.Context, address string) error {
	if err := f.checkPowered(); err != nil {
		return err
	}
	addr, err := NormalizeAddress(address)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.devices[addr]
	if !ok {
		return fmt.Errorf("device %s is not known to bluez: %w", addr, ErrDeviceUnknown)
	}
	f.record("connect " + addr)
	d.connected = true
	if d.leOnly {
		d.uuids = stripA2DP(d.uuids)
	}
	return nil
}

// Disconnect drops the connection.
func (f *Fake) Disconnect(ctx context.Context, address string) error {
	if err := f.checkPowered(); err != nil {
		return err
	}
	addr, err := NormalizeAddress(address)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.devices[addr]
	if !ok {
		return fmt.Errorf("device %s is not known to bluez: %w", addr, ErrDeviceUnknown)
	}
	f.record("disconnect " + addr)
	d.connected = false
	return nil
}

// Forget removes the device and its pairing information.
func (f *Fake) Forget(ctx context.Context, address string) error {
	if err := f.checkPowered(); err != nil {
		return err
	}
	addr, err := NormalizeAddress(address)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.devices[addr]; !ok {
		// "Known" means in the controller's record: a device still
		// advertising (discoverable) but not yet scanned in is not
		// forgettable — bluez's RemoveDevice refuses an unknown device.
		return fmt.Errorf("device %s is not known to bluez: %w", addr, ErrDeviceUnknown)
	}
	f.record("forget " + addr)
	delete(f.devices, addr)
	return nil
}

// Repair is the re-pair path: forget, (the rescan finds it only when it
// is discoverable — the pairing-mode requirement), re-pair, trust,
// connect. It clears the LE-only regression.
func (f *Fake) Repair(ctx context.Context, address string) error {
	if err := f.checkPowered(); err != nil {
		return err
	}
	addr, err := NormalizeAddress(address)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	_, wasKnown := f.devices[addr]
	_, discoverable := f.discover[addr]
	if !wasKnown && !discoverable {
		return fmt.Errorf("the classic-transport rescan did not find %s (put the headset in pairing mode and retry)", addr)
	}
	f.record("repair " + addr)
	delete(f.devices, addr)
	if !discoverable {
		return fmt.Errorf("the rescan did not recreate %s (pairing mode required)", addr)
	}
	d := f.discover[addr]
	d.leOnly = false
	d.uuids = restoreA2DP(d.uuids)
	d.paired = true
	d.trusted = true
	d.connected = true
	f.devices[addr] = d
	return nil
}

// Close is a no-op on the fake (nothing to release).
func (f *Fake) Close() error { return nil }

// deviceRow renders one fake device as the public row. The UUID set is
// the single source of truth for the A2DP flags — the LE-only state is
// exactly "connected with the A2DP unit stripped", which stripA2DP has
// already applied, so the row reads the honest "no audio sink".
func (f *Fake) deviceRow(d *fakeDev) Device {
	lower := make([]string, len(d.uuids))
	for i, u := range d.uuids {
		lower[i] = strings.ToLower(u)
	}
	return Device{
		Address:    d.addr,
		Alias:      d.alias,
		Paired:     d.paired,
		Trusted:    d.trusted,
		Connected:  d.connected,
		A2DPSink:   contains(lower, UUIDA2DP),
		A2DPSource: contains(lower, UUIDA2DPSource),
		Discovered: d.discovered,
	}
}

func stripA2DP(uuids []string) []string {
	out := make([]string, 0, len(uuids))
	for _, u := range uuids {
		if strings.EqualFold(u, UUIDA2DP) || strings.EqualFold(u, UUIDA2DPSource) {
			continue
		}
		out = append(out, u)
	}
	return out
}

func restoreA2DP(uuids []string) []string {
	if containsFold(uuids, UUIDA2DP) {
		return uuids
	}
	return append(append([]string{}, uuids...), UUIDA2DP)
}

func containsFold(list []string, want string) bool {
	for _, v := range list {
		if strings.EqualFold(v, want) {
			return true
		}
	}
	return false
}
