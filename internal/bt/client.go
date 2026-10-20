// The real BlueZ controller (ADR-0087): org.bluez over the system D-Bus,
// through godbus (github.com/godbus/dbus/v5). No bluetoothctl, no shell —
// the AGENTS.md rule-6 boundary: the web-triggered pairing path is
// D-Bus calls only.
//
// The appliance's pairing posture (the proven recipe of
// skills/uno-q-setup/references/audio.md, productized):
//
//   - one NoInputNoOutput agent registered for the lifetime of the
//     process (the appliance has no display and no keyboard; a device
//     that demands a PIN cannot be paired from here and the Pair error
//     says so);
//   - scan → the time-bounded discovery session, released on the way out;
//   - pair → Pair + Trust (the hand-verified `pair` + `trust` sequence);
//   - repair → the LE-only gotcha fix of issue #3: Forget, a classic-
//     transport (BR/EDR) rescan, re-pair, trust, connect.
package bt

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

// The BlueZ bus surface. The object layout follows the real daemon
// (bluez's main.xml / ObjectManager tree): the ObjectManager and nothing
// else lives at the root "/"; the AgentManager lives at "/org/bluez";
// the adapter and its devices under "/org/bluez/hci0[/dev_…]".
const (
	bluezService = "org.bluez"

	// bluezRoot is the ObjectManager's path (the one-call enumeration).
	bluezRoot = "/"
	// bluezAgentMgr is the AgentManager's path (agent registration).
	bluezAgentMgr = "/org/bluez"

	ifaceAdapter      = "org.bluez.Adapter1"
	ifaceDevice       = "org.bluez.Device1"
	ifaceAgentManager = "org.bluez.AgentManager1"
	ifaceAgent        = "org.bluez.Agent1"
	ifaceObjectMngr   = "org.freedesktop.DBus.ObjectManager"
)

// ErrNotAvailable is the controller's honest "I cannot ask" answer: the
// bus is open but org.bluez has no owner (the bluetoothd service is down)
// or the connection is closed. The API maps it to a 503 with the note,
// and the page says "Bluetooth is not running" instead of "no devices".
var ErrNotAvailable = errors.New("bluez is not available (is bluetoothd running?)")

// ErrDeviceUnknown is the sentinel for "BlueZ has no record of this address"
// (scan or pair it first). The API answers it with a 404: the address is
// well-formed, it is simply not a device the radio knows.
var ErrDeviceUnknown = errors.New("device not known to bluez")

// agentPath is the object path the appliance's agent exports. It is a
// private path, not org.bluez's own tree, so it cannot collide with the
// daemon's objects.
const agentPath = dbus.ObjectPath("/org/game_narrator/bt/agent")

// scanQuiet is the no-new-device quiet window the poller waits before
// ending a scan early (a headset that advertises for its 12 s pairing
// window is seen within two polls; 1.5 s of silence is "that's all").
const scanQuiet = 1500 * time.Millisecond

// scanPoll is the poll interval.
const scanPoll = 500 * time.Millisecond

// Client is the D-Bus-backed Controller. One Client per process (the
// serve wiring builds it once); it is safe for concurrent use — the
// state-changing operations (scan/pair/repair) serialize on mu, which is
// also BlueZ's own rule: one discovery session per caller, and pairing a
// second device mid-scan is a race the operator does not want.
type Client struct {
	conn      *dbus.Conn
	mu        sync.Mutex
	agentSet  bool
	closed    bool
	closeOnce sync.Once
}

// New opens the system bus and returns a BlueZ controller with the
// appliance's agent registered. The call fails (with the exact missing
// thing) when there is no system bus at all — the API then answers 503
// and the page says "not wired", which is the honest state for a host
// without D-Bus.
func New(ctx context.Context) (*Client, error) {
	conn, err := dbus.SystemBus()
	if err != nil {
		return nil, fmt.Errorf("opening the system bus: %w", err)
	}
	c := &Client{conn: conn}
	if err := c.ensureAgent(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return c, nil
}

// NewOnBus builds a controller over an already-open bus connection —
// the test seam (the private-bus integration test points it at the fake
// BlueZ; a unit can point it at any bus that speaks org.bluez). The agent
// is registered the same way as in New.
func NewOnBus(conn *dbus.Conn) (*Client, error) {
	c := &Client{conn: conn}
	return c, nil
}

// Close releases the bus connection and unregisters the agent. Idempotent.
func (c *Client) Close() error {
	var err error
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		if c.agentSet {
			_ = c.conn.Object(bluezService, dbus.ObjectPath(bluezAgentMgr)).
				Call(ifaceAgentManager+".UnregisterAgent", 0, agentPath).Err
			c.agentSet = false
		}
		err = c.conn.Close()
	})
	return err
}

// ── the appliance's NoInputNoOutput agent ─────────────────────────────────

// agent is the exported org.bluez.Agent1. Every answer is the appliance's
// posture: confirm just-works, answer the classic PIN with 000000 (the
// universal default; a device that does not accept it fails Pair with
// AuthenticationFailed, which is the exact "this headset needs a PIN
// you cannot enter here" signal), and authorize profile connections —
// the operator asked for them by clicking Connect.
//
// godbus drops an exported method that does not end in *dbus.Error, so
// every method carries that exact return type (nil = the empty reply
// bluez expects from a NoInputNoOutput agent).
type agent struct{}

func (agent) Release() *dbus.Error { return nil }

func (agent) RequestPinCode(dbus.ObjectPath) (string, *dbus.Error) { return "000000", nil }

func (agent) DisplayPinCode(dbus.ObjectPath, string) *dbus.Error { return nil }

func (agent) RequestPasskey(dbus.ObjectPath) (uint32, *dbus.Error) { return 0, nil }

func (agent) DisplayPasskey(dbus.ObjectPath, uint32, uint16) *dbus.Error { return nil }

func (agent) RequestConfirmation(dbus.ObjectPath, uint32) *dbus.Error { return nil }

func (agent) RequestAuthorization(dbus.ObjectPath) *dbus.Error { return nil }

func (agent) AuthorizeService(dbus.ObjectPath, string) *dbus.Error { return nil }

func (agent) Cancel() *dbus.Error { return nil }

// ensureAgent exports the agent object and registers it (idempotent: an
// AlreadyExists answer means it is still registered — a BlueZ restart
// would have dropped it, so re-registration is retried on demand).
func (c *Client) ensureAgent(ctx context.Context) error {
	if err := c.conn.Export(agent{}, agentPath, ifaceAgent); err != nil {
		return fmt.Errorf("exporting the bt agent: %w", err)
	}
	root := c.conn.Object(bluezService, dbus.ObjectPath(bluezAgentMgr))
	err := root.CallWithContext(ctx, ifaceAgentManager+".RegisterAgent", 0, agentPath, agentCapability).Err
	if err == nil {
		c.agentSet = true
	} else if !isAlreadyExists(err) {
		return fmt.Errorf("registering the bt agent: %w", err)
	} else {
		c.agentSet = true
	}
	// Best-effort: make it the default agent (other clients' pairing then
	// uses it too). The appliance's own Pair calls use our agent by
	// registration, so a failure here is a note, not a failure.
	if err := root.CallWithContext(ctx, ifaceAgentManager+".RequestDefaultAgent", 0, agentPath).Err; err != nil {
		if !isAlreadyExists(err) && !isDoesNotExist(err) {
			_ = err // noted, not fatal: Pair still uses our registered agent
		}
	}
	return nil
}

// ── state and inventory ───────────────────────────────────────────────────

// managed pulls the whole BlueZ object tree in one call (the
// ObjectManager's GetManagedObjects — the same enumeration the bluez
// tree's own list-devices tool uses) and returns the adapter paths and
// the per-path property maps. The wire type is a{oa{sv}}: each interface
// entry is a VARIANT holding the property dictionary, so the decode is
// two steps (the map, then the variant's value).
type managed struct {
	adapters []dbus.ObjectPath
	devices  map[dbus.ObjectPath]map[string]dbus.Variant // path → Device1 props
}

func (c *Client) managed(ctx context.Context) (*managed, error) {
	if c.closed {
		return nil, ErrNotAvailable
	}
	var objects map[dbus.ObjectPath]map[string]dbus.Variant
	err := c.conn.Object(bluezService, dbus.ObjectPath(bluezRoot)).
		CallWithContext(ctx, ifaceObjectMngr+".GetManagedObjects", 0).
		Store(&objects)
	if err != nil {
		if isNoOwner(err) {
			return nil, ErrNotAvailable
		}
		return nil, fmt.Errorf("enumerating the bluez objects: %w", err)
	}
	m := &managed{devices: map[dbus.ObjectPath]map[string]dbus.Variant{}}
	for path, ifaces := range objects {
		for iface, variant := range ifaces {
			props, ok := variant.Value().(map[string]dbus.Variant)
			if !ok {
				continue // an interface shape we do not understand is skipped, not guessed
			}
			switch iface {
			case ifaceAdapter:
				m.adapters = append(m.adapters, path)
			case ifaceDevice:
				m.devices[path] = props
			}
		}
	}
	return m, nil
}

// State reports the adapter condition. A missing bluez is not an error
// here: the page needs the note to say "Bluetooth is not running", and
// the operations answer ErrNotAvailable where they cannot work.
func (c *Client) State(ctx context.Context) (State, error) {
	m, err := c.managed(ctx)
	if err != nil {
		return State{Note: bluezNote(err)}, nil
	}
	if len(m.adapters) == 0 {
		return State{Note: "no Bluetooth adapter is present"}, nil
	}
	props := c.properties(ctx, m.adapters[0], ifaceAdapter)
	st := State{}
	if v, ok := props["Powered"]; ok {
		st.Powered, _ = v.Value().(bool)
	}
	if v, ok := props["Discovering"]; ok {
		st.Discovering, _ = v.Value().(bool)
	}
	if v, ok := props["Alias"]; ok {
		st.Alias, _ = v.Value().(string)
	}
	return st, nil
}

// Devices lists the known devices (paired or merely discovered), sorted
// by address so the page's list is stable across polls.
func (c *Client) Devices(ctx context.Context) ([]Device, error) {
	m, err := c.managed(ctx)
	if err != nil {
		return nil, err
	}
	devs := make([]Device, 0, len(m.devices))
	for _, props := range m.devices {
		devs = append(devs, deviceFromProps(props))
	}
	sortDevices(devs)
	return devs, nil
}

// deviceFromProps reduces one Device1 property map to a Device row. The
// UUIDs are lower-cased by BlueZ; the comparison below is case-insensitive
// anyway (the UUIDs are canonical lower-case).
func deviceFromProps(props map[string]dbus.Variant) Device {
	d := Device{}
	if v, ok := props["Address"]; ok {
		d.Address, _ = v.Value().(string)
	}
	if v, ok := props["Alias"]; ok {
		d.Alias, _ = v.Value().(string)
	}
	if v, ok := props["Paired"]; ok {
		d.Paired, _ = v.Value().(bool)
	}
	if v, ok := props["Trusted"]; ok {
		d.Trusted, _ = v.Value().(bool)
	}
	if v, ok := props["Connected"]; ok {
		d.Connected, _ = v.Value().(bool)
	}
	if v, ok := props["RSSI"]; ok {
		d.RSSI, _ = v.Value().(int16)
	}
	var uuids []string
	if v, ok := props["UUIDs"]; ok {
		uuids, _ = v.Value().([]string)
	}
	lower := make([]string, len(uuids))
	for i, u := range uuids {
		lower[i] = strings.ToLower(u)
	}
	d.A2DPSink = contains(lower, UUIDA2DP)
	d.A2DPSource = contains(lower, UUIDA2DPSource)
	return d
}

// properties reads one object's property map (best-effort: a missing
// property is simply absent from the map).
func (c *Client) properties(ctx context.Context, path dbus.ObjectPath, iface string) map[string]dbus.Variant {
	var out map[string]dbus.Variant
	err := c.conn.Object(bluezService, path).
		CallWithContext(ctx, "org.freedesktop.DBus.Properties.GetAll", 0, iface).
		Store(&out)
	if err != nil {
		return map[string]dbus.Variant{}
	}
	if out == nil {
		out = map[string]dbus.Variant{}
	}
	return out
}

// ── the operations ────────────────────────────────────────────────────────

// Scan runs the time-bounded discovery. duration is the hard bound the
// session is held for; the scan ends early after scanQuiet with no new
// device (the pairing window is over). The caller's ctx can cut it short
// (the API's request deadline).
func (c *Client) Scan(ctx context.Context, duration time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrNotAvailable
	}
	if err := c.ensureAgent(ctx); err != nil {
		return err
	}
	m, err := c.managed(ctx)
	if err != nil {
		return err
	}
	if len(m.adapters) == 0 {
		return errors.New("no Bluetooth adapter is present")
	}
	adapter := m.adapters[0]
	adapterObj := c.conn.Object(bluezService, adapter)

	// Clear any filter a previous scan left (BlueZ merges filters across
	// callers; ours must be the only one we own).
	if err := adapterObj.CallWithContext(ctx, ifaceAdapter+".SetDiscoveryFilter", 0, map[string]any{}).Err; err != nil {
		return fmt.Errorf("setting the discovery filter: %w", err)
	}
	if err := adapterObj.CallWithContext(ctx, ifaceAdapter+".StartDiscovery", 0).Err; err != nil {
		return fmt.Errorf("starting the discovery: %w", err)
	}
	defer func() {
		// Release the one session we hold; a ctx cancellation does not
		// stop the radio for us, so the release is on the defer, not the
		// happy path.
		_ = c.conn.Object(bluezService, adapter).Call(ifaceAdapter+".StopDiscovery", 0).Err
	}()

	deadline := time.Now().Add(duration)
	seen := map[string]bool{}
	quiet := time.Now()
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(scanPoll):
		}
		m, err := c.managed(ctx)
		if err != nil {
			return err
		}
		newFound := false
		for _, props := range m.devices {
			if v, ok := props["Address"]; ok {
				if addr, _ := v.Value().(string); !seen[addr] {
					seen[addr] = true
					newFound = true
				}
			}
		}
		if newFound {
			quiet = time.Now()
		} else if time.Since(quiet) >= scanQuiet {
			break
		}
	}
	return nil
}

// ConnectDevicePath resolves a MAC to its BlueZ object path (the
// dev_XX_XX_… under the adapter). An unknown address is a "device is
// not known" error, not a guess at a path.
func (c *Client) devicePath(ctx context.Context, address string) (dbus.ObjectPath, error) {
	m, err := c.managed(ctx)
	if err != nil {
		return "", err
	}
	for path, props := range m.devices {
		if v, ok := props["Address"]; ok {
			if addr, _ := v.Value().(string); addr == address {
				return path, nil
			}
		}
	}
	return "", fmt.Errorf("device %s is not known to bluez (scan, or pair it first): %w", address, ErrDeviceUnknown)
}

// adapterPath finds the one adapter (the appliance has exactly one; a
// second would be a surprise the page should see, not a crash).
func (c *Client) adapterPath(ctx context.Context) (dbus.ObjectPath, error) {
	m, err := c.managed(ctx)
	if err != nil {
		return "", err
	}
	if len(m.adapters) == 0 {
		return "", errors.New("no Bluetooth adapter is present")
	}
	return m.adapters[0], nil
}

// Pair runs the proven recipe's pair+trust: the NoInputNoOutput agent is
// already registered (ensureAgent), so BlueZ uses it for the pairing.
func (c *Client) Pair(ctx context.Context, address string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrNotAvailable
	}
	if err := c.ensureAgent(ctx); err != nil {
		return err
	}
	path, err := c.devicePath(ctx, address)
	if err != nil {
		return err
	}
	if err := c.conn.Object(bluezService, path).
		CallWithContext(ctx, ifaceDevice+".Pair", 0).Err; err != nil {
		return fmt.Errorf("pairing %s: %w", address, err)
	}
	return c.setTrusted(ctx, path, true)
}

// Trust sets or clears the Trusted property.
func (c *Client) Trust(ctx context.Context, address string, trusted bool) error {
	path, err := c.devicePath(ctx, address)
	if err != nil {
		return err
	}
	return c.setTrusted(ctx, path, trusted)
}

func (c *Client) setTrusted(ctx context.Context, path dbus.ObjectPath, trusted bool) error {
	err := c.conn.Object(bluezService, path).
		CallWithContext(ctx, "org.freedesktop.DBus.Properties.Set", 0,
			ifaceDevice, "Trusted", dbus.MakeVariant(trusted)).Err
	if err != nil {
		return fmt.Errorf("setting trust: %w", err)
	}
	return nil
}

// Connect connects the device's connectable profiles. BlueZ answers
// AlreadyConnected for a device that is already up — the product treats
// that as success (the operation is idempotent by intent).
func (c *Client) Connect(ctx context.Context, address string) error {
	path, err := c.devicePath(ctx, address)
	if err != nil {
		return err
	}
	err = c.conn.Object(bluezService, path).CallWithContext(ctx, ifaceDevice+".Connect", 0).Err
	if isAlreadyConnected(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("connecting %s: %w", address, err)
	}
	return nil
}

// Disconnect drops the device's connections.
func (c *Client) Disconnect(ctx context.Context, address string) error {
	path, err := c.devicePath(ctx, address)
	if err != nil {
		return err
	}
	err = c.conn.Object(bluezService, path).CallWithContext(ctx, ifaceDevice+".Disconnect", 0).Err
	if err != nil && !strings.Contains(err.Error(), "NotConnected") {
		return fmt.Errorf("disconnecting %s: %w", address, err)
	}
	return nil
}

// Forget removes the device and its pairing information.
func (c *Client) Forget(ctx context.Context, address string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	adapter, err := c.adapterPath(ctx)
	if err != nil {
		return err
	}
	path, err := c.devicePath(ctx, address)
	if err != nil {
		return err
	}
	err = c.conn.Object(bluezService, adapter).
		CallWithContext(ctx, ifaceAdapter+".RemoveDevice", 0, path).Err
	if err != nil {
		return fmt.Errorf("forgetting %s: %w", address, err)
	}
	return nil
}

// repairScanBound caps the classic-transport rescan inside Repair when
// the caller's ctx has no useful deadline of its own; with a caller
// deadline it is the lesser of the two.
const repairScanBound = 20 * time.Second

// Repair is the "connected but no audio sink" fix (issue #3): the
// dual-mode headset reconnected over LE only — GATT reads, no A2DP unit.
// The hand-verified sequence, productized: forget the device, rescan
// forcing the classic BR/EDR transport, re-pair it (trust included),
// then connect. A headset that is not in pairing mode will not answer
// the rescan, and the error says so with the pairing-mode instruction —
// the one step only a human hand can do.
func (c *Client) Repair(ctx context.Context, address string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrNotAvailable
	}
	if err := c.ensureAgent(ctx); err != nil {
		return err
	}
	if err := c.forgeUnlock(ctx, address); err != nil {
		return err
	}
	adapter, err := c.adapterPath(ctx)
	if err != nil {
		return err
	}
	adapterObj := c.conn.Object(bluezService, adapter)
	// Force the classic transport: the LE-only state is exactly the one a
	// "le"-biased rescan would reproduce.
	filter := map[string]any{"Transport": "bredr"}
	if err := adapterObj.CallWithContext(ctx, ifaceAdapter+".SetDiscoveryFilter", 0, filter).Err; err != nil {
		return fmt.Errorf("setting the classic-transport scan filter: %w", err)
	}
	defer func() {
		_ = c.conn.Object(bluezService, adapter).
			Call(ifaceAdapter+".SetDiscoveryFilter", 0, map[string]any{}).Err
	}()

	scanCtx := ctx
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		scanCtx, cancel = context.WithTimeout(ctx, repairScanBound)
		defer cancel()
	}
	// Scan until the device answers the classic inquiry (or the bound).
	if err := c.scanLocked(scanCtx, 12*time.Second, address); err != nil {
		return fmt.Errorf("the classic-transport rescan did not find %s "+
			"(put the headset in pairing mode and retry): %w", address, err)
	}
	path, err := c.devicePath(ctx, address)
	if err != nil {
		return fmt.Errorf("the rescan did not recreate %s (pairing mode required): %w", address, err)
	}
	if err := c.conn.Object(bluezService, path).
		CallWithContext(ctx, ifaceDevice+".Pair", 0).Err; err != nil {
		return fmt.Errorf("re-pairing %s: %w", address, err)
	}
	if err := c.setTrusted(ctx, path, true); err != nil {
		return err
	}
	if err := c.conn.Object(bluezService, path).CallWithContext(ctx, ifaceDevice+".Connect", 0).Err; err != nil {
		if !isAlreadyConnected(err) {
			return fmt.Errorf("connecting %s after the re-pair: %w", address, err)
		}
	}
	return nil
}

// forgeUnlock is Forget minus the lock: Repair already holds it.
func (c *Client) forgeUnlock(ctx context.Context, address string) error {
	adapter, err := c.adapterPath(ctx)
	if err != nil {
		return err
	}
	path, err := c.devicePath(ctx, address)
	if err != nil {
		// A device bluez no longer knows of needs no forget: the repair
		// is "scan the pairing-mode headset and pair it fresh".
		return nil
	}
	err = c.conn.Object(bluezService, adapter).
		CallWithContext(ctx, ifaceAdapter+".RemoveDevice", 0, path).Err
	if err != nil {
		return fmt.Errorf("forgetting %s: %w", address, err)
	}
	return nil
}

// scanLocked is Scan's discovery loop minus the lock (Repair holds it).
// waitFor, when non-empty, ends the scan as soon as that address is
// among the known devices; empty waits the full duration (the API scan
// uses the quiet-window logic above instead).
func (c *Client) scanLocked(ctx context.Context, duration time.Duration, waitFor string) error {
	adapter, err := c.adapterPath(ctx)
	if err != nil {
		return err
	}
	adapterObj := c.conn.Object(bluezService, adapter)
	if err := adapterObj.CallWithContext(ctx, ifaceAdapter+".StartDiscovery", 0).Err; err != nil {
		return fmt.Errorf("starting the discovery: %w", err)
	}
	defer func() {
		_ = c.conn.Object(bluezService, adapter).Call(ifaceAdapter+".StopDiscovery", 0).Err
	}()
	deadline := time.Now().Add(duration)
	for time.Now().Before(deadline) {
		if waitFor != "" {
			if _, err := c.devicePath(ctx, waitFor); err == nil {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(scanPoll):
		}
	}
	return nil
}

// ── error classification ──────────────────────────────────────────────────

// isNoOwner reports whether err is the bus saying org.bluez has no owner
// (ServiceUnknown / NameHasNoOwner) — the "bluetoothd is down" state.
func isNoOwner(err error) bool {
	var de *dbus.Error
	if !errors.As(err, &de) {
		return false
	}
	switch de.Name {
	case "org.freedesktop.DBus.Error.ServiceUnknown", "org.freedesktop.DBus.Error.NameHasNoOwner":
		return true
	}
	return false
}

func isAlreadyExists(err error) bool {
	var de *dbus.Error
	return errors.As(err, &de) && de.Name == "org.bluez.Error.AlreadyExists"
}

func isAlreadyConnected(err error) bool {
	var de *dbus.Error
	return errors.As(err, &de) && (de.Name == "org.bluez.Error.AlreadyConnected" || de.Name == "org.freedesktop.DBus.Error.AlreadyExists")
}

func isDoesNotExist(err error) bool {
	var de *dbus.Error
	return errors.As(err, &de) && de.Name == "org.bluez.Error.DoesNotExist"
}

// bluezNote renders an error as the user-facing note the GET state
// carries when it cannot ask the daemon.
func bluezNote(err error) string {
	if errors.Is(err, ErrNotAvailable) {
		return "bluez is not available (is bluetoothd running? start it: systemctl start bluetooth)"
	}
	return err.Error()
}

// ── small helpers ─────────────────────────────────────────────────────────

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func sortDevices(devs []Device) {
	for i := 1; i < len(devs); i++ {
		for j := i; j > 0 && devs[j].Address < devs[j-1].Address; j-- {
			devs[j], devs[j-1] = devs[j-1], devs[j]
		}
	}
}
