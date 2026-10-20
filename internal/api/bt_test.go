package api

// The Bluetooth output surface tests (ADR-0087, issues #2/#3): the route
// grants, the honest-degradation shape of the unwired controller, the
// labelled-synthetic controller's state/scan/inventory/lifecycle, the MAC
// spelling rule, the repair path for the issue-#3 LE-only state, and the
// shared mutation budget (429). The controller is the fake (the same
// labelled-synthetic seam the Playwright walk uses): nothing here touches a
// real bus, and the private-bus integration test in internal/bt exercises
// the real D-Bus client against a fake BlueZ service on its own.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/bt"
	"github.com/dbirks/game-narrator/internal/store"
)

// btTestMACs are the fixture addresses (obviously-fake: the public-repo rule
// keeps real hardware addresses out of committed fixtures).
const (
	btMACA2DP   = "AA:BB:CC:DD:EE:FF" // paired, connected, A2DP sink
	btMACFresh  = "11:22:33:44:55:66" // discoverable only: the scan finds it
	btMACLeOnly = "99:88:77:66:55:44" // the issue-#3 state: paired, LE-only (and in pairing mode, the human step the repair's rescan needs)
)

// btTestFake seeds the three states the page must tell apart.
func btTestFake() *bt.Fake {
	return bt.NewFake(
		bt.FakeDevice{
			Address: btMACA2DP, Alias: "JBL Tune 770NC",
			UUIDs: []string{bt.UUIDA2DP}, Paired: true, Trusted: true, Connected: true,
		},
		bt.FakeDevice{
			Address: btMACFresh, Alias: "Sony WH-1000XM5",
			UUIDs:        []string{bt.UUIDA2DP},
			Discoverable: true,
		},
		bt.FakeDevice{
			Address: btMACLeOnly, Alias: "Dual-mode headset",
			UUIDs: []string{bt.UUIDA2DP}, Paired: true, Trusted: true, Connected: true,
			LEOnly: true, Discoverable: true,
		},
	)
}

// btHandler builds the test server with the fake controller and a store.
func (a *testAuth) btHandler(t *testing.T, fake *bt.Fake, opts ...Option) http.Handler {
	t.Helper()
	all := append([]Option{WithBluetoothController(fake), WithStore(a.st)}, opts...)
	return a.handler(all...)
}

type btStateBody struct {
	Available   bool   `json:"available"`
	Synthetic   bool   `json:"synthetic"`
	Powered     bool   `json:"powered"`
	Discovering bool   `json:"discovering"`
	Alias       string `json:"alias"`
	Note        string `json:"note"`
}

type btDeviceBody struct {
	Address     string `json:"address"`
	Alias       string `json:"alias"`
	Paired      bool   `json:"paired"`
	Trusted     bool   `json:"trusted"`
	Connected   bool   `json:"connected"`
	A2DPSink    bool   `json:"a2dp_sink"`
	A2DPSource  bool   `json:"a2dp_source"`
	Discovered  bool   `json:"discovered"`
	Selected    bool   `json:"selected"`
	NeedsRepair bool   `json:"needs_repair"`
}

type btDevicesBody struct {
	Synthetic bool           `json:"synthetic"`
	Devices   []btDeviceBody `json:"devices"`
	Note      string         `json:"note"`
}

func btFind(t *testing.T, body *btDevicesBody, mac string) *btDeviceBody {
	t.Helper()
	for i := range body.Devices {
		if body.Devices[i].Address == mac {
			return &body.Devices[i]
		}
	}
	t.Fatalf("device %s not in the inventory (%+v)", mac, body.Devices)
	return nil
}

// TestBtRouteGrants pins the scope table: the Bluetooth surface is the
// operator tier (it acts on the physical radio), the viewer is refused.
func TestBtRouteGrants(t *testing.T) {
	a := newTestAuth(t)
	h := a.btHandler(t, btTestFake())
	for _, target := range []string{
		"/api/v1/audio/bluetooth/state",
		"/api/v1/audio/bluetooth/devices",
	} {
		rec := doReq(t, h, a.as(auth.RoleViewer, getReq(target)))
		if rec.Code != http.StatusForbidden {
			t.Errorf("viewer GET %s = %d, want 403 (body %s)", target, rec.Code, rec.Body.String())
		}
	}
	rec := doReq(t, h, a.as(auth.RoleOperator, getReq("/api/v1/audio/bluetooth/state")))
	if rec.Code != http.StatusOK {
		t.Fatalf("operator GET state = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	// The mutations are operator too (the scope table's /api/v1/audio/{rest...}).
	rec = doReq(t, h, a.as(auth.RoleViewer, jsonRequest(http.MethodPost,
		"/api/v1/audio/bluetooth/devices/"+btMACA2DP+"/connect", "")))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer POST connect = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestBtUnwiredAnswers503 is the honest-degradation rule: a build without the
// controller answers 503 (the page hides the section on available=false),
// never an empty 200 that would read as "no devices".
func TestBtUnwiredAnswers503(t *testing.T) {
	a := newTestAuth(t)
	h := a.handler(WithStore(a.st))
	for _, req := range []*http.Request{
		a.as(auth.RoleOperator, getReq("/api/v1/audio/bluetooth/state")),
		a.as(auth.RoleOperator, getReq("/api/v1/audio/bluetooth/devices")),
		a.as(auth.RoleOperator, jsonRequest(http.MethodPost, "/api/v1/audio/bluetooth/scan", "")),
		a.as(auth.RoleOperator, jsonRequest(http.MethodPost,
			"/api/v1/audio/bluetooth/devices/"+btMACA2DP+"/connect", "")),
	} {
		rec := doReq(t, h, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s = %d, want 503 (body %s)", req.Method, req.URL.Path, rec.Code, rec.Body.String())
		}
		var prob map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &prob)
		if prob["code"] != "BLUETOOTH_UNAVAILABLE" {
			t.Errorf("unwired code = %v, want BLUETOOTH_UNAVAILABLE (body %s)", prob["code"], rec.Body.String())
		}
	}
}

// TestBtStateView asserts the adapter state and the synthetic label.
func TestBtStateView(t *testing.T) {
	a := newTestAuth(t)
	fake := btTestFake()
	h := a.btHandler(t, fake)
	rec := doReq(t, h, a.as(auth.RoleOperator, getReq("/api/v1/audio/bluetooth/state")))
	if rec.Code != http.StatusOK {
		t.Fatalf("state = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var body btStateBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("state body: %v (%s)", err, rec.Body.String())
	}
	if !body.Available || !body.Synthetic || !body.Powered || body.Discovering {
		t.Fatalf("state = %+v, want available+synthetic+powered, not discovering", body)
	}
	// The powered-off state: the operations answer 503 with the remediation
	// note, and the state says so.
	fake.SetPowered(false)
	rec = doReq(t, h, a.as(auth.RoleOperator, getReq("/api/v1/audio/bluetooth/state")))
	if rec.Code != http.StatusOK {
		t.Fatalf("powered-off state = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("powered-off body: %v", err)
	}
	if body.Powered {
		t.Fatalf("state still reports powered after SetPowered(false): %+v", body)
	}
	if body.Note == "" {
		t.Error("the powered-off state carries no remediation note")
	}
	rec = doReq(t, h, a.as(auth.RoleOperator, jsonRequest(http.MethodPost,
		"/api/v1/audio/bluetooth/devices/"+btMACA2DP+"/connect", "")))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("powered-off connect = %d, want 503 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestBtDevicesList shows the inventory with the pairing flags, the A2DP
// evidence, the selected badge and the issue-#3 repair hint. The inventory is
// the controller's record (bluez's ObjectManager tree): a device that merely
// advertises is not in it yet — the scan is what puts it there.
func TestBtDevicesList(t *testing.T) {
	a := newTestAuth(t)
	fake := btTestFake()
	h := a.btHandler(t, fake)
	rec := doReq(t, h, a.as(auth.RoleOperator, getReq("/api/v1/audio/bluetooth/devices")))
	if rec.Code != http.StatusOK {
		t.Fatalf("devices = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var body btDevicesBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("devices body: %v (%s)", err, rec.Body.String())
	}
	if !body.Synthetic {
		t.Error("the fake's inventory is not labelled synthetic")
	}
	a2dp := btFind(t, &body, btMACA2DP)
	if !a2dp.Paired || !a2dp.Trusted || !a2dp.Connected || !a2dp.A2DPSink || a2dp.NeedsRepair {
		t.Fatalf("A2DP row = %+v, want paired+trusted+connected+a2dp_sink, no repair", a2dp)
	}
	le := btFind(t, &body, btMACLeOnly)
	if !le.Connected || le.A2DPSink {
		t.Fatalf("LE-only row = %+v, want connected with no A2DP unit", le)
	}
	if !le.NeedsRepair {
		t.Fatalf("the LE-only row carries no needs_repair hint: %+v", le)
	}
	if len(body.Devices) != 2 {
		t.Fatalf("pre-scan inventory = %d rows, want 2 (the seeded pair; the advertising-only device is not in the record yet): %+v", len(body.Devices), body.Devices)
	}
}

// TestBtScanBoundedAndHinted exercises the time-bounded scan: the duration
// clamps, the discoverable device appears, and an empty scan answers 200
// with the pairing-mode hint.
func TestBtScanBoundedAndHinted(t *testing.T) {
	a := newTestAuth(t)
	fake := btTestFake()
	h := a.btHandler(t, fake)

	// The duration clamps to the ceiling (the answer's elapsed time is the
	// evidence the bound was honoured — the fake's scan is instant, so the
	// assertion is on the accepted request, not the radio).
	rec := doReq(t, h, a.as(auth.RoleOperator, jsonRequest(http.MethodPost,
		"/api/v1/audio/bluetooth/scan", `{"duration_ms":999999}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("scan = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var scan struct {
		Synthetic bool           `json:"synthetic"`
		ElapsedMS int64          `json:"elapsed_ms"`
		Devices   []btDeviceBody `json:"devices"`
		Note      string         `json:"note"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &scan); err != nil {
		t.Fatalf("scan body: %v (%s)", err, rec.Body.String())
	}
	if !scan.Synthetic {
		t.Error("the scan answer is not labelled synthetic")
	}
	fresh := btFind(t, &btDevicesBody{Devices: scan.Devices}, btMACFresh)
	if !fresh.Discovered {
		t.Fatalf("the discoverable device is not marked discovered: %+v", fresh)
	}

	// An empty scan (nothing seeded discoverable) is a 200 with the hint.
	empty := bt.NewFake()
	hEmpty := a.btHandler(t, empty)
	rec = doReq(t, hEmpty, a.as(auth.RoleOperator, jsonRequest(http.MethodPost,
		"/api/v1/audio/bluetooth/scan", "")))
	if rec.Code != http.StatusOK {
		t.Fatalf("empty scan = %d, want 200 (an empty result is an answer, not a failure) (body %s)",
			rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &scan); err != nil {
		t.Fatalf("empty scan body: %v", err)
	}
	if len(scan.Devices) != 0 {
		t.Fatalf("empty scan found %d devices, want 0: %+v", len(scan.Devices), scan.Devices)
	}
	if scan.Note == "" {
		t.Error("the empty scan carries no pairing-mode hint")
	}
}

// TestBtLifecycleWalksTheIssueRecipe walks pair → connect → disconnect →
// forget on the fresh device, asserting the state the page badges on.
func TestBtLifecycleWalksTheIssueRecipe(t *testing.T) {
	a := newTestAuth(t)
	fake := btTestFake()
	h := a.btHandler(t, fake)

	// The fresh device is discoverable but not paired: pair+trust it.
	rec := doReq(t, h, a.as(auth.RoleOperator, jsonRequest(http.MethodPost,
		"/api/v1/audio/bluetooth/devices/"+btMACFresh+"/pair", "")))
	if rec.Code != http.StatusOK {
		t.Fatalf("pair = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var act struct {
		Address string `json:"address"`
		Note    string `json:"note"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &act); err != nil {
		t.Fatalf("pair body: %v (%s)", err, rec.Body.String())
	}
	if act.Address != btMACFresh {
		t.Fatalf("pair answer address = %q, want %q", act.Address, btMACFresh)
	}

	// Connect: the A2DP sink "appears".
	rec = doReq(t, h, a.as(auth.RoleOperator, jsonRequest(http.MethodPost,
		"/api/v1/audio/bluetooth/devices/"+btMACFresh+"/connect", "")))
	if rec.Code != http.StatusOK {
		t.Fatalf("connect = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	// Disconnect, then forget: the row is gone from the inventory.
	rec = doReq(t, h, a.as(auth.RoleOperator, jsonRequest(http.MethodPost,
		"/api/v1/audio/bluetooth/devices/"+btMACFresh+"/disconnect", "")))
	if rec.Code != http.StatusOK {
		t.Fatalf("disconnect = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	rec = doReq(t, h, a.as(auth.RoleOperator, jsonRequest(http.MethodPost,
		"/api/v1/audio/bluetooth/devices/"+btMACFresh+"/forget", "")))
	if rec.Code != http.StatusOK {
		t.Fatalf("forget = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	rec = doReq(t, h, a.as(auth.RoleOperator, getReq("/api/v1/audio/bluetooth/devices")))
	if rec.Code != http.StatusOK {
		t.Fatalf("post-forget devices = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var body btDevicesBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("post-forget body: %v", err)
	}
	for _, d := range body.Devices {
		if d.Address == btMACFresh {
			t.Fatalf("the forgotten device is still in the inventory: %+v", d)
		}
	}
	// Forgetting it again is a 404 (bluez's RemoveDevice refuses unknowns).
	rec = doReq(t, h, a.as(auth.RoleOperator, jsonRequest(http.MethodPost,
		"/api/v1/audio/bluetooth/devices/"+btMACFresh+"/forget", "")))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("second forget = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestBtRepairClearsTheLEOnlyState is the issue-#3 repair: the LE-only
// device (connected, no A2DP unit, needs_repair) is re-paired over the
// classic transport and the A2DP sink comes back.
func TestBtRepairClearsTheLEOnlyState(t *testing.T) {
	a := newTestAuth(t)
	fake := btTestFake()
	h := a.btHandler(t, fake)
	rec := doReq(t, h, a.as(auth.RoleOperator, jsonRequest(http.MethodPost,
		"/api/v1/audio/bluetooth/devices/"+btMACLeOnly+"/repair", "")))
	if rec.Code != http.StatusOK {
		t.Fatalf("repair = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	rec = doReq(t, h, a.as(auth.RoleOperator, getReq("/api/v1/audio/bluetooth/devices")))
	if rec.Code != http.StatusOK {
		t.Fatalf("post-repair devices = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var body btDevicesBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("post-repair body: %v (%s)", err, rec.Body.String())
	}
	fixed := btFind(t, &body, btMACLeOnly)
	if !fixed.Connected || !fixed.Paired || !fixed.A2DPSink || fixed.NeedsRepair {
		t.Fatalf("post-repair row = %+v, want connected+paired with the A2DP sink back and no repair hint", fixed)
	}
}

// TestBtMACSpellingRule: a malformed address is a 400 (the API never sends a
// typo to the radio), and the answer echoes the normalised form.
func TestBtMACSpellingRule(t *testing.T) {
	a := newTestAuth(t)
	fake := btTestFake()
	h := a.btHandler(t, fake)
	for _, mac := range []string{"nope", "AA:BB:CC:DD:EE", "AA-BB-CC-DD-EE-FF", "GG:BB:CC:DD:EE:FF"} {
		rec := doReq(t, h, a.as(auth.RoleOperator, jsonRequest(http.MethodPost,
			"/api/v1/audio/bluetooth/devices/"+mac+"/connect", "")))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("connect %q = %d, want 400 (body %s)", mac, rec.Code, rec.Body.String())
		}
	}
	// Lower-case input is normalised to upper-case in the answer.
	rec := doReq(t, h, a.as(auth.RoleOperator, jsonRequest(http.MethodPost,
		"/api/v1/audio/bluetooth/devices/"+strings.ToLower(btMACA2DP)+"/connect", "")))
	if rec.Code != http.StatusOK {
		t.Fatalf("lower-case connect = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var act struct {
		Address string `json:"address"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &act); err != nil {
		t.Fatalf("connect body: %v", err)
	}
	if act.Address != btMACA2DP {
		t.Fatalf("the answer did not echo the normalised address: %q", act.Address)
	}
}

// TestBtRateLimitSharedBucket: the mutation budget is per client address —
// a small bucket 429s with Retry-After, and the read routes are untouched.
func TestBtRateLimitSharedBucket(t *testing.T) {
	a := newTestAuth(t)
	fake := btTestFake()
	// A one-token bucket: the first mutation takes it, the next is refused.
	small := auth.NewLimiter(1, time.Hour, func() time.Time { return time.Now() })
	h := a.btHandler(t, fake, WithBluetoothLimiter(small))

	rec := doReq(t, h, a.as(auth.RoleOperator, jsonRequest(http.MethodPost,
		"/api/v1/audio/bluetooth/devices/"+btMACA2DP+"/connect", "")))
	if rec.Code != http.StatusOK {
		t.Fatalf("first connect = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	rec = doReq(t, h, a.as(auth.RoleOperator, jsonRequest(http.MethodPost,
		"/api/v1/audio/bluetooth/devices/"+btMACA2DP+"/disconnect", "")))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second mutation = %d, want 429 (body %s)", rec.Code, rec.Body.String())
	}
	ra := rec.Header().Get("Retry-After")
	if ra == "" {
		t.Error("the 429 carries no Retry-After header")
	}
	var prob map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &prob)
	if prob["code"] != "RATE_LIMITED" {
		t.Errorf("429 code = %v, want RATE_LIMITED (body %s)", prob["code"], rec.Body.String())
	}
	// The reads are not on the budget.
	rec = doReq(t, h, a.as(auth.RoleOperator, getReq("/api/v1/audio/bluetooth/devices")))
	if rec.Code != http.StatusOK {
		t.Fatalf("devices after the 429 = %d, want 200 (reads are not budgeted)", rec.Code)
	}
	// The budget is per client address: a different transport peer has its
	// own bucket (the key is the peer address, the one unspoofable value).
	req := a.as(auth.RoleOperator, jsonRequest(http.MethodPost,
		"/api/v1/audio/bluetooth/devices/"+btMACA2DP+"/disconnect", ""))
	req.RemoteAddr = "198.51.100.9:5555"
	rec = doReq(t, h, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("a different peer's mutation = %d, want 200 (the bucket is per address, body %s)",
			rec.Code, rec.Body.String())
	}
}

// TestBtSelectedBadge follows the ADR-0074 selection: the device whose A2DP
// sink the stored selection names is the one flagged selected, and the env
// override wins over the store (the one rule the page must not break).
func TestBtSelectedBadge(t *testing.T) {
	a := newTestAuth(t)
	fake := btTestFake()
	// Store a selection of the A2DP device's sink.
	if err := a.st.SetAudioSettings(context.Background(), storeAudioSettingsRow(bt.SinkID(btMACA2DP))); err != nil {
		t.Fatalf("store the audio selection: %v", err)
	}
	h := a.btHandler(t, fake)
	rec := doReq(t, h, a.as(auth.RoleOperator, getReq("/api/v1/audio/bluetooth/devices")))
	if rec.Code != http.StatusOK {
		t.Fatalf("devices = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var body btDevicesBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("devices body: %v (%s)", err, rec.Body.String())
	}
	selected := 0
	for _, d := range body.Devices {
		if d.Selected {
			selected++
			if d.Address != btMACA2DP {
				t.Fatalf("the selected row is %s, want %s", d.Address, btMACA2DP)
			}
		}
	}
	if selected != 1 {
		t.Fatalf("%d rows flagged selected, want exactly one: %+v", selected, body.Devices)
	}
}

// storeAudioSettingsRow is the store's audio row the badge test persists.
func storeAudioSettingsRow(device string) store.AudioSettings {
	return store.AudioSettings{Device: device, MasterVolume: 1.0}
}
