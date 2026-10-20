package api

// The diagnostics product surface tests (ADR-0087, docs/spec/09 /diagnostics):
// the public host vitals, the model-role probe runs that execute the doctor's
// own checks through the diagProbe seam, and the redacted log tail. The host
// readers run through function seams so the suite fabricates a kernel
// (AGENTS.md rule 3: never depend on which host runs it), and the probe seam
// keeps the role runs from ever talking to a real gateway in a unit test.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/doctor"
	"github.com/dbirks/game-narrator/internal/logbuf"
	"github.com/dbirks/game-narrator/internal/providers"
)

// hostSeamFacts is the fixture kernel one test fabricates.
type hostSeamFacts struct {
	load1, load5, load15 float64
	haveLoad             bool
	ramTotal, ramAvail   int64
	haveRAM              bool
	zones                []thermalZoneView
	throttleCount        uint64
	haveThrottle         bool
	fsFree, fsTotal      int64
	haveFS               bool
}

// hostSeams points the host readers at the fixture facts for the test's
// duration and restores them after.
func hostSeams(t *testing.T, f hostSeamFacts) {
	t.Helper()
	prevLoad, prevRam, prevThermal, prevThrottle, prevStatfs := hostReadLoadAvg, hostReadMemInfo, hostReadThermal, hostReadThrottle, hostStatfs
	t.Cleanup(func() {
		hostReadLoadAvg, hostReadMemInfo, hostReadThermal, hostReadThrottle, hostStatfs = prevLoad, prevRam, prevThermal, prevThrottle, prevStatfs
	})
	hostReadLoadAvg = func() (float64, float64, float64, bool) { return f.load1, f.load5, f.load15, f.haveLoad }
	hostReadMemInfo = func() (int64, int64, bool) { return f.ramTotal, f.ramAvail, f.haveRAM }
	hostReadThermal = func() []thermalZoneView { return f.zones }
	hostReadThrottle = func() (uint64, bool) { return f.throttleCount, f.haveThrottle }
	hostStatfs = func(dir string) (int64, int64, bool) { return f.fsFree, f.fsTotal, f.haveFS }
}

// scriptedProbe points the run's probe seam at a scripted doctor answer.
func scriptedProbe(t *testing.T, answer doctor.Check) {
	t.Helper()
	prev := diagProbe
	t.Cleanup(func() { diagProbe = prev })
	diagProbe = func(ctx context.Context, o doctor.Options, role providers.Role) (doctor.Check, error) {
		return answer, nil
	}
}

// pollDiagnosticsRun waits for a run to finish (or the deadline) and returns
// its final view.
func pollDiagnosticsRun(t *testing.T, a *testAuth, h http.Handler, role auth.Role, id string) diagnosticsRunView {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		rec := doReq(t, h, a.as(role, getReq("/api/v1/diagnostics/"+id)))
		if rec.Code != http.StatusOK {
			t.Fatalf("run %s answered %d: %s", id, rec.Code, rec.Body.String())
		}
		var view diagnosticsRunView
		if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
			t.Fatalf("unmarshal run view: %v", err)
		}
		if view.Status == string(diagnosticsDone) {
			return view
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("run %s did not reach done within 10 s", id)
	return diagnosticsRunView{}
}

// TestDiagnosticsHostIsPublicSystemInfo pins the scope table: the host vitals
// are a public read (the diagnostics page must work before an account exists),
// and the body carries the fixture facts, the honest unknowns and the derived
// warnings.
func TestDiagnosticsHostIsPublicSystemInfo(t *testing.T) {
	hostSeams(t, hostSeamFacts{
		load1: 1.2, load5: 0.9, load15: 0.7, haveLoad: true,
		ramTotal: 8 << 30, ramAvail: 700 << 20, haveRAM: true,
		zones:         []thermalZoneView{{Name: "cpu-thermal", Celsius: 91.0}},
		throttleCount: 3, haveThrottle: true,
		fsFree: 500 << 20, fsTotal: 16 << 30, haveFS: true,
	})
	a := newTestAuth(t)
	h := a.handler()

	// Public: no credential at all.
	rec := doReq(t, h, getReq("/api/v1/diagnostics/host"))
	if rec.Code != http.StatusOK {
		t.Fatalf("anonymous host = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var view hostReportView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("unmarshal host: %v", err)
	}
	if view.CPU.Load1M == nil || *view.CPU.Load1M != 1.2 {
		t.Fatalf("load = %v, want 1.2", view.CPU.Load1M)
	}
	if view.CPU.ThrottleCount == nil || *view.CPU.ThrottleCount != 3 {
		t.Fatalf("throttle count = %v, want 3", view.CPU.ThrottleCount)
	}
	if view.RAM == nil || view.RAM.TotalBytes != 8<<30 || view.RAM.AvailableBytes != 700<<20 {
		t.Fatalf("ram = %+v", view.RAM)
	}
	if !view.Storage.Known || view.Storage.FreeBytes != 500<<20 || view.Storage.TotalBytes != 16<<30 {
		t.Fatalf("storage = %+v", view.Storage)
	}
	if !view.Thermal.Available || len(view.Thermal.Zones) != 1 || view.Thermal.Zones[0].Celsius != 91.0 {
		t.Fatalf("thermal = %+v", view.Thermal)
	}
	// The warnings: low free space (500 MiB < 1 GiB) and the 91 °C zone.
	var sawLowSpace, sawTemp bool
	for _, w := range view.Warnings {
		if strings.Contains(w, "free") {
			sawLowSpace = true
		}
		if strings.Contains(w, "91") {
			sawTemp = true
		}
	}
	if !sawLowSpace || !sawTemp {
		t.Fatalf("warnings = %v, want the low-space and the thermal lines", view.Warnings)
	}
}

// TestDiagnosticsHostUnknownsAreHonest: nothing readable, nothing invented.
func TestDiagnosticsHostUnknownsAreHonest(t *testing.T) {
	hostSeams(t, hostSeamFacts{})
	a := newTestAuth(t)
	h := a.handler()
	rec := doReq(t, h, getReq("/api/v1/diagnostics/host"))
	if rec.Code != http.StatusOK {
		t.Fatalf("host = %d, want 200", rec.Code)
	}
	var view hostReportView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("unmarshal host: %v", err)
	}
	if view.CPU.Load1M != nil || view.RAM != nil || view.CPU.ThrottleCount != nil {
		t.Fatalf("unreadable facts must stay null: %+v", view)
	}
	if view.Storage.FreeBytes != -1 || view.Storage.TotalBytes != -1 {
		t.Fatalf("unmeasured storage must be -1: %+v", view.Storage)
	}
	if view.Thermal.Available {
		t.Fatal("no zones must not report the thermal section as available")
	}
	if view.Thermal.Note == "" {
		t.Fatal("the unavailable thermal state must say so")
	}
	// The JSON contract: warnings is an array even when nothing crossed a
	// threshold. A nil slice serializes as null, and the page dereferences
	// the field without a null guard — the crash would land exactly when the
	// machine is healthy (the least visible moment to break).
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal raw host: %v", err)
	}
	// A JSON null unmarshals into a nil slice without an error, so the
	// contract check is on the raw bytes.
	if string(raw["warnings"]) == "null" || len(raw["warnings"]) == 0 {
		t.Fatalf("warnings = %s, want a JSON array (possibly empty), not null", raw["warnings"])
	}
	var warnings []string
	if err := json.Unmarshal(raw["warnings"], &warnings); err != nil {
		t.Fatalf("warnings = %s, want a JSON array: %v", raw["warnings"], err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none (no threshold crossed with the seam's facts)", warnings)
	}
}

// TestDiagnosticsRunGrantsAndLifecycle pins the scope table (operator runs,
// viewer reads) and the 202 → running → done lifecycle with the scripted probe.
func TestDiagnosticsRunGrantsAndLifecycle(t *testing.T) {
	scriptedProbe(t, doctor.Check{
		Status:   doctor.StatusOK,
		Summary:  "model fake-model answered the chat probe in 412ms",
		Detail:   "1 model offered",
		Duration: 412 * time.Millisecond,
	})
	a := newTestAuth(t)
	h := a.handler()

	// viewer may not start a run.
	rec := doReq(t, h, a.as(auth.RoleViewer, jsonRequest(http.MethodPost, "/api/v1/diagnostics/run", `{"roles":["fast_reasoning"]}`)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer run = %d, want 403", rec.Code)
	}

	rec = doReq(t, h, a.as(auth.RoleOperator, jsonRequest(http.MethodPost, "/api/v1/diagnostics/run", `{"roles":["fast_reasoning"]}`)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("operator run = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	var ref diagnosticsRunRef
	if err := json.Unmarshal(rec.Body.Bytes(), &ref); err != nil || ref.RunID == "" {
		t.Fatalf("run ref = %q: %v", rec.Body.String(), err)
	}

	// viewer may read the run.
	view := pollDiagnosticsRun(t, a, h, auth.RoleViewer, ref.RunID)
	if len(view.Roles) != 1 || view.Roles[0].Status != doctor.StatusOK {
		t.Fatalf("roles = %+v", view.Roles)
	}
	if view.Roles[0].DurationMS != 412 {
		t.Fatalf("duration = %d ms, want the probe's measured 412", view.Roles[0].DurationMS)
	}
	if view.Host == nil {
		t.Fatal("the run carries the host snapshot")
	}
	if view.FinishedAt == nil {
		t.Fatal("a done run carries its finish time")
	}
}

// TestDiagnosticsRunInvalidRole: the enumeration is the allowlist.
func TestDiagnosticsRunInvalidRole(t *testing.T) {
	a := newTestAuth(t)
	h := a.handler()
	rec := doReq(t, h, a.as(auth.RoleOperator, jsonRequest(http.MethodPost, "/api/v1/diagnostics/run", `{"roles":["bogus"]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bogus role = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestDiagnosticsRunUnknownID: 404, not an empty run.
func TestDiagnosticsRunUnknownID(t *testing.T) {
	a := newTestAuth(t)
	h := a.handler()
	rec := doReq(t, h, a.as(auth.RoleViewer, getReq("/api/v1/diagnostics/never-created")))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown run = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestDiagnosticsRunStoreIsBounded: the bound holds under click-mashing and an
// expired run falls out (the GET answers 404, the honest "expired").
func TestDiagnosticsRunStoreIsBounded(t *testing.T) {
	s := newDiagnosticsRunStore()
	var ids []string
	for i := 0; i < diagRunsMax+10; i++ {
		ids = append(ids, s.create(nil).id)
	}
	// The cap is enforced on eviction: right after a create the store may hold
	// bound+1, and the next access evicts back to the bound.
	if s.get(ids[len(ids)-1]) == nil {
		t.Fatal("the newest run must still be in the store")
	}
	if got := len(s.order); got != diagRunsMax {
		t.Fatalf("store holds %d runs after eviction, want the bound %d", got, diagRunsMax)
	}
	// The newest survives, the oldest was dropped.
	if s.get(ids[0]) != nil {
		t.Fatal("the oldest run should have fallen out of the bound")
	}
	if s.get(ids[len(ids)-1]) == nil {
		t.Fatal("the newest run must still be in the store")
	}
	// An expired finished run falls out of its TTL.
	old := s.create(nil)
	old.finish()
	old.mu.Lock()
	old.finishedAt = time.Now().UTC().Add(-2 * time.Hour)
	old.mu.Unlock()
	if s.get(old.id) != nil {
		t.Fatal("an expired run must be evicted")
	}
}

// TestDiagnosticsLogsAreRedactedAndDeveloper pins the grant (developer) and
// the redaction: a line that carries a gateway URL, a token and a private IP
// leaves the route with every one of them replaced.
func TestDiagnosticsLogsAreRedactedAndDeveloper(t *testing.T) {
	ring := logbuf.New(32)
	log := slog.New(ring.Handler())
	log.Info("calling the gateway",
		"url", "https://gateway.example.internal:8443/v1",
		"token", "sk-live-abcdef123456",
		"peer", "192.168.1.10",
	)
	a := newTestAuth(t)
	h := a.handler(WithLogRing(ring))

	// viewer is refused; developer reads.
	rec := doReq(t, h, a.as(auth.RoleViewer, getReq("/api/v1/diagnostics/logs")))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer logs = %d, want 403", rec.Code)
	}
	rec = doReq(t, h, a.as(auth.RoleDeveloper, getReq("/api/v1/diagnostics/logs")))
	if rec.Code != http.StatusOK {
		t.Fatalf("developer logs = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var view diagnosticsLogsView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("unmarshal logs: %v", err)
	}
	if view.Count != 1 {
		t.Fatalf("count = %d, want 1", view.Count)
	}
	body := rec.Body.String()
	for _, secret := range []string{"gateway.example.internal", "sk-live-abcdef123456", "192.168.1.10"} {
		if strings.Contains(body, secret) {
			t.Fatalf("the logs answer leaked %q:\n%s", secret, body)
		}
	}
	rec0 := view.Records[0]
	if rec0.Attrs["token"] != "<redacted>" || rec0.Attrs["peer"] != "<redacted>" || rec0.Attrs["url"] != "https://<redacted-host>" {
		t.Fatalf("the redacted values must say so: %+v", rec0.Attrs)
	}
}

// TestDiagnosticsLogsUnwiredRingIs503: no ring, no empty tail that reads as
// "no errors".
func TestDiagnosticsLogsUnwiredRingIs503(t *testing.T) {
	a := newTestAuth(t)
	h := a.handler()
	rec := doReq(t, h, a.as(auth.RoleDeveloper, getReq("/api/v1/diagnostics/logs")))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired logs = %d, want 503 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestDiagnosticsLogsLimitValidation: a bad limit is 400, an over-large one is
// capped, never an error.
func TestDiagnosticsLogsLimitValidation(t *testing.T) {
	ring := logbuf.New(16)
	log := slog.New(ring.Handler())
	for i := 0; i < 5; i++ {
		log.Info("line")
	}
	a := newTestAuth(t)
	h := a.handler(WithLogRing(ring))

	rec := doReq(t, h, a.as(auth.RoleDeveloper, getReq("/api/v1/diagnostics/logs?limit=zero")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad limit = %d, want 400", rec.Code)
	}
	rec = doReq(t, h, a.as(auth.RoleDeveloper, getReq("/api/v1/diagnostics/logs?limit=99999")))
	if rec.Code != http.StatusOK {
		t.Fatalf("capped limit = %d, want 200", rec.Code)
	}
	var view diagnosticsLogsView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if view.Count != 5 {
		t.Fatalf("count = %d, want the ring's 5", view.Count)
	}
}
