// The private-bus integration test: the REAL client (client.go) pointed
// at a fake BlueZ *D-Bus service* on a private dbus-daemon. This is a
// different class of evidence from the unit tests (the fake.go state
// machine): it exercises the actual D-Bus wiring — the ObjectManager
// enumeration, the Properties Get/Set, the AgentManager registration,
// and the exported NoInputNoOutput agent answering the daemon's PIN
// probe — with the godbus marshalling of the real code, no stub.
//
// NOT RUN when dbus-daemon is absent (host-tool-gated like the PipeWire
// null-sink round trip, ADR-0074's evidence standard).
package bt

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// probeName is the well-known name the test client owns on the private
// bus: the fake daemon probes the client's exported agent by it, the
// same way bluez would probe any registered agent by its unique name.
// Owning a test name on a PRIVATE bus cannot collide with anything.
const probeName = "org.game_narrator.bt_test"

// startPrivateBus starts a throwaway session bus with dbus-launch (the
// same d-bus package that ships dbus-daemon) and returns its address and
// the daemon's PID. A private session bus needs no config file and accepts
// the default EXTERNAL auth (the uid), which is all the test's two
// in-process connections need — no ANONYMOUS mechanism, no daemon config
// to keep in sync with the distro's dbus-daemon build.
func startPrivateBus(t *testing.T) (string, int) {
	t.Helper()
	launcher, err := exec.LookPath("dbus-launch")
	if err != nil {
		t.Skip("NOT RUN: dbus-launch is not on PATH (install the d-bus package for the private-bus integration)")
	}
	out, err := exec.Command(launcher, "--sh-syntax").CombinedOutput()
	if err != nil {
		t.Skipf("NOT RUN: dbus-launch could not start a session bus: %v", err)
	}
	var addr string
	var pid int
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(line, "DBUS_SESSION_BUS_ADDRESS='"); ok {
			addr, _, _ = strings.Cut(v, "'")
		}
		if v, ok := strings.CutPrefix(line, "DBUS_SESSION_BUS_PID="); ok {
			if n, err := fmt.Sscanf(v, "%d", &pid); err == nil && n == 1 {
				// keep
			} else {
				t.Fatalf("parsing DBUS_SESSION_BUS_PID from %q: %v", v, err)
			}
		}
	}
	if !strings.Contains(addr, "unix:") || pid == 0 {
		t.Fatalf("dbus-launch output had no bus address/PID: %q", string(out))
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	return addr, pid
}

// fakeBlueZState is the daemon-side state: the object tree the
// ObjectManager reports, and what the daemon observed of the client's
// agent (the registration and the PIN probe answer).
type fakeBlueZState struct {
	mu      sync.Mutex
	agentMu sync.Mutex
	powered bool
	alias   string
	devices map[dbus.ObjectPath]*fakeBtDevice
	agent   agentLog
}

type fakeBtDevice struct {
	addr      string
	alias     string
	uuids     []string
	paired    bool
	trusted   bool
	connected bool
}

// agentLog is what the fake daemon recorded about the client's agent.
type agentLog struct {
	registered    bool
	regCapability string
	regPath       dbus.ObjectPath
	pinProbed     bool
	pinAnswered   string
	pinError      error
}

func (st *fakeBlueZState) addDevice(addr, alias string, uuids []string) {
	p := dbus.ObjectPath(bluezAgentMgr + "/hci0/dev_" + strings.ReplaceAll(addr, ":", "_"))
	st.devices[p] = &fakeBtDevice{addr: addr, alias: alias, uuids: uuids}
}

func (st *fakeBlueZState) adapterProps() map[string]dbus.Variant {
	return map[string]dbus.Variant{
		"Address":     dbus.MakeVariant("00:11:22:33:44:55"),
		"Powered":     dbus.MakeVariant(st.powered),
		"Discovering": dbus.MakeVariant(false),
		"Alias":       dbus.MakeVariant(st.alias),
	}
}

func (st *fakeBlueZState) deviceProps(p dbus.ObjectPath) map[string]dbus.Variant {
	d := st.devices[p]
	return map[string]dbus.Variant{
		"Address":   dbus.MakeVariant(d.addr),
		"Alias":     dbus.MakeVariant(d.alias),
		"UUIDs":     dbus.MakeVariant(d.uuids),
		"Paired":    dbus.MakeVariant(d.paired),
		"Trusted":   dbus.MakeVariant(d.trusted),
		"Connected": dbus.MakeVariant(d.connected),
	}
}

func (st *fakeBlueZState) managed() map[dbus.ObjectPath]map[string]dbus.Variant {
	st.mu.Lock()
	defer st.mu.Unlock()
	// The wire type is a{oa{sv}}: each interface entry is a VARIANT that
	// holds the property dictionary — the client's decode is the two-step
	// map-then-variant, so the fake must build the same shape.
	out := map[dbus.ObjectPath]map[string]dbus.Variant{
		dbus.ObjectPath(bluezAgentMgr + "/hci0"): {ifaceAdapter: dbus.MakeVariant(st.adapterProps())},
	}
	for p := range st.devices {
		out[p] = map[string]dbus.Variant{ifaceDevice: dbus.MakeVariant(st.deviceProps(p))}
	}
	return out
}

// startFakeBlueZ owns org.bluez on the private bus and exports the
// daemon's object tree. deviceMAC is the one device the discovery will
// find (the pairing-mode headset).
func startFakeBlueZ(t *testing.T, addr, deviceMAC string) *fakeBlueZState {
	t.Helper()
	conn, err := dbus.Connect(addr)
	if err != nil {
		t.Fatalf("daemon connecting to the private bus: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := conn.RequestName(bluezService, dbus.NameFlagDoNotQueue); err != nil {
		t.Fatalf("owning org.bluez on the private bus: %v", err)
	}

	st := &fakeBlueZState{
		powered: true,
		alias:   "game-narrator-appliance",
		devices: map[dbus.ObjectPath]*fakeBtDevice{},
	}

	// The ObjectManager (the client's one-call enumeration).
	conn.ExportMethodTable(map[string]any{
		"GetManagedObjects": func() (map[dbus.ObjectPath]map[string]dbus.Variant, *dbus.Error) {
			return st.managed(), nil
		},
	}, dbus.ObjectPath(bluezRoot), ifaceObjectMngr)

	// The AgentManager: records the client's registration.
	conn.ExportMethodTable(map[string]any{
		"RegisterAgent": func(p dbus.ObjectPath, cap string) *dbus.Error {
			st.agentMu.Lock()
			st.agent.registered = true
			st.agent.regCapability = cap
			st.agent.regPath = p
			st.agentMu.Unlock()
			return nil
		},
		"UnregisterAgent": func(p dbus.ObjectPath) *dbus.Error { return nil },
		"RequestDefaultAgent": func(p dbus.ObjectPath) *dbus.Error {
			// The appliance's RequestDefaultAgent is best-effort on the
			// real bus (our own Pair uses our agent by registration); the
			// fake answers a benign error so that path is exercised.
			return &dbus.Error{Name: "org.bluez.Error.DoesNotExist"}
		},
	}, dbus.ObjectPath(bluezAgentMgr), ifaceAgentManager)

	// The adapter (org.bluez.Adapter1 + the Properties interface).
	adapterPath := dbus.ObjectPath(bluezAgentMgr + "/hci0")
	conn.ExportMethodTable(map[string]any{
		"StartDiscovery": func() *dbus.Error {
			// The pairing-mode headset answers the inquiry immediately
			// (deterministic test timing: the client's quiet window ends
			// the scan shortly after).
			st.mu.Lock()
			st.addDevice(deviceMAC, "JBL Tune 770NC", []string{UUIDA2DP})
			st.mu.Unlock()
			return nil
		},
		"StopDiscovery": func() *dbus.Error { return nil },
		"SetDiscoveryFilter": func(m map[string]any) *dbus.Error {
			return nil // the filter is accepted; the fake's scan is transport-agnostic
		},
		"RemoveDevice": func(p dbus.ObjectPath) *dbus.Error {
			st.mu.Lock()
			defer st.mu.Unlock()
			if _, ok := st.devices[p]; !ok {
				return &dbus.Error{Name: "org.bluez.Error.Failed"}
			}
			delete(st.devices, p)
			return nil
		},
	}, adapterPath, ifaceAdapter)
	conn.ExportMethodTable(map[string]any{
		"GetAll": func(iface string) (map[string]dbus.Variant, *dbus.Error) {
			if iface != ifaceAdapter {
				return nil, &dbus.Error{Name: "org.freedesktop.DBus.Error.InvalidArgs"}
			}
			return st.adapterProps(), nil
		},
		"Get": func(iface, prop string) (dbus.Variant, *dbus.Error) {
			props := st.adapterProps()
			v, ok := props[prop]
			if !ok {
				return dbus.MakeVariant(nil), &dbus.Error{Name: "org.freedesktop.DBus.Error.InvalidArgs"}
			}
			return v, nil
		},
	}, adapterPath, "org.freedesktop.DBus.Properties")

	// The device (org.bluez.Device1 + the Properties interface). The
	// Pair handler probes the client's exported agent the way bluez
	// probes a registered agent: a RequestPinCode on the agent object.
	// The probe runs in a goroutine — a handler must not block on a
	// nested call of its own connection (the reply would be handled by
	// the very worker that is stuck in the handler).
	devicePath := dbus.ObjectPath(bluezAgentMgr + "/hci0/dev_" + strings.ReplaceAll(deviceMAC, ":", "_"))
	conn.ExportMethodTable(map[string]any{
		"Pair": func() *dbus.Error {
			go probeClientAgent(conn, st, devicePath)
			st.mu.Lock()
			if d := st.devices[devicePath]; d != nil {
				d.paired = true
			}
			st.mu.Unlock()
			return nil
		},
		"Connect": func() *dbus.Error {
			st.mu.Lock()
			if d := st.devices[devicePath]; d != nil {
				d.connected = true
			}
			st.mu.Unlock()
			return nil
		},
		"Disconnect": func() *dbus.Error {
			st.mu.Lock()
			if d := st.devices[devicePath]; d != nil {
				d.connected = false
			}
			st.mu.Unlock()
			return nil
		},
	}, devicePath, ifaceDevice)
	conn.ExportMethodTable(map[string]any{
		"GetAll": func(iface string) (map[string]dbus.Variant, *dbus.Error) {
			if iface != ifaceDevice {
				return nil, &dbus.Error{Name: "org.freedesktop.DBus.Error.InvalidArgs"}
			}
			st.mu.Lock()
			defer st.mu.Unlock()
			if _, ok := st.devices[devicePath]; !ok {
				return nil, &dbus.Error{Name: "org.freedesktop.DBus.Error.Failed"}
			}
			return st.deviceProps(devicePath), nil
		},
		"Set": func(iface, prop string, v dbus.Variant) *dbus.Error {
			if iface != ifaceDevice || prop != "Trusted" {
				return &dbus.Error{Name: "org.freedesktop.DBus.Error.InvalidArgs"}
			}
			trusted, _ := v.Value().(bool)
			st.mu.Lock()
			defer st.mu.Unlock()
			if d := st.devices[devicePath]; d != nil {
				d.trusted = trusted
			}
			return nil
		},
	}, devicePath, "org.freedesktop.DBus.Properties")

	return st
}

// probeClientAgent asks the client's exported agent for a PIN (the
// pairing's NoInputNoOutput check) and records the answer. The client
// owns probeName on the private bus, which is how the daemon reaches the
// agent object (bluez would use the registering client's unique name).
func probeClientAgent(conn *dbus.Conn, st *fakeBlueZState, devicePath dbus.ObjectPath) {
	st.agentMu.Lock()
	registered, agentPath := st.agent.registered, st.agent.regPath
	st.agentMu.Unlock()
	if !registered || agentPath == "" {
		st.agentMu.Lock()
		st.agent.pinError = fmt.Errorf("no agent was registered before the pair")
		st.agentMu.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var pin string
	err := conn.Object(probeName, agentPath).
		CallWithContext(ctx, ifaceAgent+".RequestPinCode", 0, devicePath).
		Store(&pin)
	st.agentMu.Lock()
	defer st.agentMu.Unlock()
	if err != nil {
		st.agent.pinError = err
		return
	}
	st.agent.pinProbed = true
	st.agent.pinAnswered = pin
}

func TestPrivateBusFakeBlueZ(t *testing.T) {
	addr, _ := startPrivateBus(t)
	const mac = "AA:BB:CC:DD:EE:FF"
	st := startFakeBlueZ(t, addr, mac)

	// The client: the real D-Bus client on the same private bus, owning
	// probeName so the daemon's agent probe has a destination.
	conn, err := dbus.Connect(addr)
	if err != nil {
		t.Fatalf("client connecting to the private bus: %v", err)
	}
	defer conn.Close()
	if _, err := conn.RequestName(probeName, dbus.NameFlagDoNotQueue); err != nil {
		t.Fatalf("owning the probe name: %v", err)
	}
	client, err := NewOnBus(conn)
	if err != nil {
		t.Fatalf("NewOnBus: %v", err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// State: the adapter is powered, with its alias.
	st2, err := client.State(ctx)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if !st2.Powered {
		t.Error("State.Powered = false, want true")
	}
	if st2.Alias != "game-narrator-appliance" {
		t.Errorf("State.Alias = %q, want the adapter's alias", st2.Alias)
	}

	// Scan: the discovery session finds the pairing-mode headset; the
	// quiet window ends the scan early (the fake's device appears on the
	// first poll, so well under the 3 s bound).
	scanStart := time.Now()
	if err := client.Scan(ctx, 3*time.Second); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if elapsed := time.Since(scanStart); elapsed > 2500*time.Millisecond {
		t.Errorf("Scan took %v, want under 2.5 s (first poll 500 ms + the 1.5 s quiet window)", elapsed)
	}
	devs, err := client.Devices(ctx)
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	if len(devs) != 1 {
		t.Fatalf("Devices after scan = %d rows, want 1: %+v", len(devs), devs)
	}
	if devs[0].Address != mac {
		t.Errorf("device address = %q, want %q", devs[0].Address, mac)
	}
	if !devs[0].HasAudioSink() {
		t.Error("the scanned device is not audio-capable (the A2DP UUID)")
	}

	// Pair: the client registers its NoInputNoOutput agent, the daemon
	// probes it for a PIN (the answer must be 000000), and the pair
	// flips paired; the follow-up trust set makes it trusted.
	if err := client.Pair(ctx, mac); err != nil {
		t.Fatalf("Pair: %v", err)
	}
	devs, _ = client.Devices(ctx)
	if len(devs) != 1 || !devs[0].Paired || !devs[0].Trusted {
		t.Errorf("after Pair: %+v, want one device paired and trusted", devs)
	}

	// The agent probe (async on the daemon side; bounded wait).
	probeDeadline := time.Now().Add(5 * time.Second)
	for {
		st.agentMu.Lock()
		probed, pinErr := st.agent.pinProbed, st.agent.pinError
		regCap, registered := st.agent.regCapability, st.agent.registered
		st.agentMu.Unlock()
		if probed {
			break
		}
		if pinErr != nil {
			t.Fatalf("the daemon's agent probe failed (registered=%v cap=%q): %v", registered, regCap, pinErr)
		}
		if time.Now().After(probeDeadline) {
			t.Fatalf("the daemon's agent probe never answered (registered=%v cap=%q)", registered, regCap)
		}
		time.Sleep(50 * time.Millisecond)
	}
	st.agentMu.Lock()
	regCap, regPath, pin := st.agent.regCapability, st.agent.regPath, st.agent.pinAnswered
	st.agentMu.Unlock()
	if regCap != "NoInputNoOutput" {
		t.Errorf("the agent registered with capability %q, want %q", regCap, agentCapability)
	}
	if string(regPath) != string(agentPath) {
		t.Errorf("the agent registered at %q, want %q", regPath, agentPath)
	}
	if pin != "000000" {
		t.Errorf("the NoInputNoOutput agent's PIN answer = %q, want 000000", pin)
	}

	// Connect.
	if err := client.Connect(ctx, mac); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	devs, _ = client.Devices(ctx)
	if !devs[0].Connected {
		t.Error("after Connect: connected = false")
	}

	// Forget: the daemon's RemoveDevice drops the device.
	if err := client.Forget(ctx, mac); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	devs, _ = client.Devices(ctx)
	if len(devs) != 0 {
		t.Errorf("after Forget: %d devices, want 0", len(devs))
	}

	// Operations on an unknown device fail (the API's 404 path).
	if err := client.Connect(ctx, mac); err == nil {
		t.Error("Connect on a forgotten device: want an error")
	}
	if err := client.Pair(ctx, mac); err == nil {
		t.Error("Pair on a forgotten device: want an error")
	}
}
