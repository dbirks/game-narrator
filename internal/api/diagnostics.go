package api

// The diagnostics product surface (ADR-0087, docs/spec/09 /diagnostics): the
// host vitals, the model-role probes that run the doctor's own checks, and
// the redacted tail of the process log.
//
// The governing rule is docs/spec/13's: a service that has not been probed is
// never reported as working. Every section of this file therefore either
// answers from a real probe (the host endpoints read the kernel and sysfs,
// the role probes are the doctor's content probes) or reports the honest
// state — null where nothing was readable, NOT CONFIGURED where no role is
// set, 503 where the wiring is absent — never a number or a status nobody
// measured. The run store is bounded (AGENTS.md rule 7): a probe nobody polls
// is not appliance state, so results live in memory, capped at the newest
// diagRunsMax, evicted after diagRunTTL, with no disk and no goroutine of
// their own beyond the one run execution.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dbirks/game-narrator/internal/doctor"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/support"
)

// ── host vitals (GET /api/v1/diagnostics/host) ──────────────────────────────

// The fixed warning thresholds of the host section (ADR-0087). They are
// constants, not configuration: a threshold the operator cannot change cannot
// be tuned into silence.
const (
	hostLowFreeSpaceBytes = 1 << 30 // 1 GiB of free space is the low-storage line
	hostHighTempCelsius   = 85.0
	hostLowRAMFraction    = 0.10
)

// hostReportView is the GET /api/v1/diagnostics/host body: a system-info-class
// read (docs/spec/08) — host capacity and state, no game content, no host
// paths beyond the data directory's base name, no secrets.
type hostReportView struct {
	GeneratedAt string          `json:"generated_at"`
	CPU         hostCPUView     `json:"cpu"`
	RAM         *hostRAMView    `json:"ram"`
	Storage     hostStorageView `json:"storage"`
	Thermal     hostThermalView `json:"thermal"`
	Warnings    []string        `json:"warnings"`
}

type hostCPUView struct {
	Cores         int      `json:"cores"`
	Load1M        *float64 `json:"load_1m"`
	Load5M        *float64 `json:"load_5m"`
	Load15M       *float64 `json:"load_15m"`
	ThrottleCount *uint64  `json:"throttle_count"`
}

type hostRAMView struct {
	TotalBytes     int64 `json:"total_bytes"`
	AvailableBytes int64 `json:"available_bytes"`
}

type hostStorageView struct {
	DataDir         string `json:"data_dir"`
	Known           bool   `json:"known"`
	FreeBytes       int64  `json:"free_bytes"`
	TotalBytes      int64  `json:"total_bytes"`
	AssetStoreBytes int64  `json:"asset_store_bytes"`
}

type hostThermalView struct {
	Available bool              `json:"available"`
	Note      string            `json:"note,omitempty"`
	Zones     []thermalZoneView `json:"zones"`
}

type thermalZoneView struct {
	Name    string  `json:"name"`
	Celsius float64 `json:"celsius"`
}

// The host readers are function seams so the handler tests fabricate a kernel
// (AGENTS.md rule 3: the suite never depends on which host it runs on); the
// defaults read the real /proc and /sys.
var (
	hostReadLoadAvg  = readLoadAvg
	hostReadMemInfo  = readMemInfo
	hostReadThermal  = readThermalZones
	hostReadThrottle = readThrottleCounts
	hostStatfs       = statfsSpace
)

// handleDiagnosticsHost answers GET /api/v1/diagnostics/host. Public: the
// exposure class is the system-info contract (host capacity, no content, no
// paths, no secrets), and a diagnostics page that cannot read the host before
// login is the page docs/spec/09 says must work before anybody has an account.
func (s *Server) handleDiagnosticsHost(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.hostReport())
}

// hostReport builds the host section from the live kernel and sysfs. It is the
// one spelling the GET handler and the diagnostics run share, so the page and
// a run's snapshot cannot disagree.
func (s *Server) hostReport() hostReportView {
	view := hostReportView{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		CPU: hostCPUView{
			Cores:         runtime.NumCPU(),
			Load1M:        nil,
			Load5M:        nil,
			Load15M:       nil,
			ThrottleCount: nil,
		},
		RAM: nil,
		Storage: hostStorageView{
			FreeBytes:       -1,
			TotalBytes:      -1,
			AssetStoreBytes: -1,
		},
		Thermal:  hostThermalView{Zones: []thermalZoneView{}},
		Warnings: []string{},
	}

	if l1, l5, l15, ok := hostReadLoadAvg(); ok {
		view.CPU.Load1M, view.CPU.Load5M, view.CPU.Load15M = &l1, &l5, &l15
	}
	if total, avail, ok := hostReadMemInfo(); ok {
		view.RAM = &hostRAMView{TotalBytes: total, AvailableBytes: avail}
	}

	dataDir := s.settingsDataDir()
	if dataDir != "" {
		view.Storage.DataDir = filepath.Base(dataDir)
		view.Storage.Known = true
		if free, total, ok := hostStatfs(dataDir); ok {
			view.Storage.FreeBytes = free
			view.Storage.TotalBytes = total
		}
		if size, ok := dirSizeBytes(filepath.Join(dataDir, "assets")); ok {
			view.Storage.AssetStoreBytes = size
		}
	}

	zones := hostReadThermal()
	if len(zones) > 0 {
		view.Thermal.Available = true
		view.Thermal.Zones = zones
	} else {
		view.Thermal.Note = "no thermal zones are exposed by this host's sysfs; the temperature is unknown, not cool"
	}
	if count, ok := hostReadThrottle(); ok {
		view.CPU.ThrottleCount = &count
	}

	view.Warnings = hostWarnings(view)
	return view
}

// hostWarnings derives the warning list from the fixed thresholds. A warning
// is a fact with its number, so the operator can verify it against the same
// section rather than trust an adjective. The slice is never nil: the JSON
// contract carries an array (possibly empty), and the page dereferences it
// without a null check (a nil here would serialize as null and crash the UI
// exactly when everything is healthy — the least visible moment to break).
func hostWarnings(view hostReportView) []string {
	out := []string{}
	if view.Storage.Known && view.Storage.FreeBytes >= 0 && view.Storage.FreeBytes < hostLowFreeSpaceBytes {
		out = append(out, fmt.Sprintf("the data directory's filesystem has %d bytes free (below the 1 GiB line)", view.Storage.FreeBytes))
	}
	if view.CPU.Load1M != nil && *view.CPU.Load1M > float64(view.CPU.Cores) {
		out = append(out, fmt.Sprintf("the 1-minute load average (%.2f) is above the core count (%d)", *view.CPU.Load1M, view.CPU.Cores))
	}
	if view.RAM != nil && view.RAM.TotalBytes > 0 && float64(view.RAM.AvailableBytes) < hostLowRAMFraction*float64(view.RAM.TotalBytes) {
		out = append(out, fmt.Sprintf("available RAM (%d bytes) is below 10%% of the total (%d bytes)", view.RAM.AvailableBytes, view.RAM.TotalBytes))
	}
	for _, z := range view.Thermal.Zones {
		if z.Celsius >= hostHighTempCelsius {
			out = append(out, fmt.Sprintf("thermal zone %s reports %.1f °C (at or above %.0f °C)", z.Name, z.Celsius, hostHighTempCelsius))
		}
	}
	return out
}

// readLoadAvg parses /proc/loadavg (Linux). Non-Linux hosts answer ok=false
// and the section shows the gap.
func readLoadAvg() (float64, float64, float64, bool) {
	if runtime.GOOS != "linux" {
		return 0, 0, 0, false
	}
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, 0, 0, false
	}
	f := strings.Fields(string(b))
	if len(f) < 3 {
		return 0, 0, 0, false
	}
	l1, e1 := strconv.ParseFloat(f[0], 64)
	l5, e2 := strconv.ParseFloat(f[1], 64)
	l15, e3 := strconv.ParseFloat(f[2], 64)
	if e1 != nil || e2 != nil || e3 != nil {
		return 0, 0, 0, false
	}
	return l1, l5, l15, true
}

// readMemInfo parses MemTotal and MemAvailable from /proc/meminfo (kilobytes).
func readMemInfo() (int64, int64, bool) {
	if runtime.GOOS != "linux" {
		return 0, 0, false
	}
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, false
	}
	var total, avail int64
	var haveTotal, haveAvail bool
	for _, line := range strings.Split(string(b), "\n") {
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			if v, ok := parseMeminfoKB(line); ok {
				total, haveTotal = v, true
			}
		case strings.HasPrefix(line, "MemAvailable:"):
			if v, ok := parseMeminfoKB(line); ok {
				avail, haveAvail = v, true
			}
		}
	}
	if !haveTotal || !haveAvail {
		return 0, 0, false
	}
	return total * 1024, avail * 1024, true
}

func parseMeminfoKB(line string) (int64, bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0, false
	}
	v, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// readThermalZones walks /sys/class/thermal (ADR-0087: "from sysfs where
// present"). A zone whose temp or type file is unreadable is skipped, not
// invented: the zone list is what the kernel reports.
func readThermalZones() []thermalZoneView {
	if runtime.GOOS != "linux" {
		return nil
	}
	zones := []thermalZoneView{}
	entries, err := os.ReadDir("/sys/class/thermal")
	if err != nil {
		return zones
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "thermal_zone") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/sys/class/thermal", e.Name(), "temp"))
		if err != nil {
			continue
		}
		millideg, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil {
			continue
		}
		name, _ := os.ReadFile(filepath.Join("/sys/class/thermal", e.Name(), "type"))
		zones = append(zones, thermalZoneView{
			Name:    strings.TrimSpace(string(name)),
			Celsius: float64(millideg) / 1000.0,
		})
	}
	return zones
}

// readThrottleCounts sums the x86 thermal-throttle counters (the sysfs file
// that exists where the kernel exposes it). ARM hosts without the file answer
// ok=false, and the section renders the gap.
func readThrottleCounts() (uint64, bool) {
	if runtime.GOOS != "linux" {
		return 0, false
	}
	entries, err := os.ReadDir("/sys/devices/system/cpu")
	if err != nil {
		return 0, false
	}
	var total uint64
	found := false
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "cpu") || strings.HasSuffix(e.Name(), "cr") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/sys/devices/system/cpu", e.Name(), "thermal_throttle", "throttle_count"))
		if err != nil {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
		if err != nil {
			continue
		}
		total += n
		found = true
	}
	return total, found
}

// statfsSpace asks the kernel for the free and total bytes of the filesystem
// holding dir — the dirFreeSpace rule plus the total. Platforms without a
// stdlib statfs answer ok=false and the section says unknown.
func statfsSpace(dir string) (int64, int64, bool) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return 0, 0, false
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, false
	}
	size := int64(st.Bsize)
	return int64(st.Bavail) * size, int64(st.Blocks) * size, true
}

// ── the diagnostics run (POST …/run, GET …/{id}) ────────────────────────────

// The run store's bounds (AGENTS.md rule 7): the newest diagRunsMax runs, each
// kept diagRunTTL after it finishes. A probe is deliberately not durable state
// — the durable-job queue is for work that must survive a restart, and a
// probe the operator does not poll is re-clicked, not resurrected.
const (
	diagRunsMax = 32
	diagRunTTL  = time.Hour
)

// diagnosticsRunState is the run's lifecycle: running until every requested
// probe has answered (or the outer deadline hit), then done.
type diagnosticsRunState string

const (
	diagnosticsRunning diagnosticsRunState = "running"
	diagnosticsDone    diagnosticsRunState = "done"
)

// diagnosticsRun is one probe run's state. Its mutex guards every field the
// reader (GET …/{id}) and the executor (the run's goroutine) both touch.
type diagnosticsRun struct {
	id         string
	createdAt  time.Time
	roles      []providers.Role
	finishedAt time.Time

	mu        sync.Mutex
	status    diagnosticsRunState
	host      *hostReportView
	roleViews []diagnosticsRoleView
}

func (run *diagnosticsRun) setHost(v hostReportView) {
	run.mu.Lock()
	defer run.mu.Unlock()
	h := v
	run.host = &h
}

func (run *diagnosticsRun) appendRole(v diagnosticsRoleView) {
	run.mu.Lock()
	defer run.mu.Unlock()
	run.roleViews = append(run.roleViews, v)
}

func (run *diagnosticsRun) finish() {
	run.mu.Lock()
	defer run.mu.Unlock()
	run.status = diagnosticsDone
	run.finishedAt = time.Now().UTC()
}

// snapshot answers the GET with whatever has landed: a running run reports
// its host and the role rows answered so far, so the page polls one shape.
func (run *diagnosticsRun) snapshot() diagnosticsRunView {
	run.mu.Lock()
	defer run.mu.Unlock()
	v := diagnosticsRunView{
		ID:        run.id,
		CreatedAt: run.createdAt.UTC().Format(time.RFC3339),
		Status:    string(run.status),
		Host:      run.host,
		Roles:     append([]diagnosticsRoleView{}, run.roleViews...),
		Requested: make([]string, 0, len(run.roles)),
	}
	for _, role := range run.roles {
		v.Requested = append(v.Requested, string(role))
	}
	if !run.finishedAt.IsZero() {
		finished := run.finishedAt.UTC().Format(time.RFC3339)
		v.FinishedAt = &finished
	}
	if v.Roles == nil {
		v.Roles = []diagnosticsRoleView{}
	}
	return v
}

// diagnosticsRunView is the GET /api/v1/diagnostics/{id} body.
type diagnosticsRunView struct {
	ID         string                `json:"id"`
	CreatedAt  string                `json:"created_at"`
	Status     string                `json:"status"`
	Host       *hostReportView       `json:"host"`
	Roles      []diagnosticsRoleView `json:"roles"`
	Requested  []string              `json:"requested"`
	FinishedAt *string               `json:"finished_at,omitempty"`
}

// diagnosticsRoleView is one role's doctor answer: the doctor's status
// (OK / FAIL / NOT CONFIGURED / DEGRADED), its summary and remedy, and the
// probe's measured duration — the latency the spec names, as the measured
// number, never an estimate.
type diagnosticsRoleView struct {
	Role       string `json:"role"`
	Configured bool   `json:"configured"`
	Status     string `json:"status"`
	Summary    string `json:"summary,omitempty"`
	Detail     string `json:"detail,omitempty"`
	Remedy     string `json:"remedy,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

// diagnosticsRunRequest is the POST /api/v1/diagnostics/run body. Zero or
// more role names; empty asks for the host snapshot only.
type diagnosticsRunRequest struct {
	Roles []string `json:"roles"`
}

// diagnosticsRunRef is the 202 answer.
type diagnosticsRunRef struct {
	RunID string `json:"run_id"`
}

// diagnosticsRunStore is the bounded in-memory store of runs.
type diagnosticsRunStore struct {
	mu    sync.Mutex
	runs  map[string]*diagnosticsRun
	order []string // newest first
}

func newDiagnosticsRunStore() *diagnosticsRunStore {
	return &diagnosticsRunStore{runs: map[string]*diagnosticsRun{}}
}

// create adds a new run and evicts to stay within the bound: expired finished
// runs first, then the oldest beyond diagRunsMax (a running run past the cap
// is dropped too — its goroutine keeps writing to a now-unreachable object,
// which is harmless; the GET answers 404, which is the honest "expired").
func (s *diagnosticsRunStore) create(roles []providers.Role) *diagnosticsRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictLocked()
	run := &diagnosticsRun{
		id:        games.NewID(),
		roles:     append([]providers.Role{}, roles...),
		status:    diagnosticsRunning,
		createdAt: time.Now().UTC(),
	}
	s.runs[run.id] = run
	s.order = append([]string{run.id}, s.order...)
	return run
}

// get answers a run by id, evicting expired runs on the way.
func (s *diagnosticsRunStore) get(id string) *diagnosticsRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictLocked()
	return s.runs[id]
}

// roleViewsSnapshot is the run's answered role rows, copied under the
// run's lock. The status rollup (status.go) reads it to map the last
// doctor run onto the health tiles: the probe already happened, and the
// snapshot is what the bar is allowed to touch.
func (run *diagnosticsRun) roleViewsSnapshot() []diagnosticsRoleView {
	run.mu.Lock()
	defer run.mu.Unlock()
	return append([]diagnosticsRoleView{}, run.roleViews...)
}

// isDone reports whether the run has finished, under the run's own lock
// (the finishedAt/status fields move in finish and the snapshot writer).
func (run *diagnosticsRun) isDone() bool {
	run.mu.Lock()
	defer run.mu.Unlock()
	return run.status == diagnosticsDone
}

// doneBefore reports, under the run's own lock, whether the run has finished
// before t. The store's eviction reads this instead of the finishedAt field
// directly: the field is guarded by the run's mutex (finish and the snapshot
// writer hold it), and the store's lock alone would race it.
func (run *diagnosticsRun) doneBefore(t time.Time) bool {
	run.mu.Lock()
	defer run.mu.Unlock()
	return run.status == diagnosticsDone && run.finishedAt.Before(t)
}

func (s *diagnosticsRunStore) evictLocked() {
	cutoff := time.Now().UTC().Add(-diagRunTTL)
	keep := make([]string, 0, len(s.order))
	for _, id := range s.order {
		run := s.runs[id]
		if run.doneBefore(cutoff) {
			delete(s.runs, id)
			continue
		}
		keep = append(keep, id)
	}
	if len(keep) > diagRunsMax {
		// order and keep are newest-first, so the overflow is the tail: the
		// keep[:diagRunsMax] newest stay, keep[diagRunsMax:] fall out.
		drop := keep[diagRunsMax:]
		keep = keep[:diagRunsMax]
		for _, id := range drop {
			delete(s.runs, id)
		}
	}
	s.order = keep
}

// diagProbe is the doctor's door (ADR-0087): the run executes the CLI's own
// providers.<role> check, never a copy. A package seam so the handler tests
// script the probe without touching the process environment (the same
// indirection audioListDevices uses).
var diagProbe = func(ctx context.Context, o doctor.Options, role providers.Role) (doctor.Check, error) {
	return doctor.ProbeRole(ctx, o, role)
}

// handleDiagnosticsRun answers POST /api/v1/diagnostics/run (the scope table's
// reserved operator grant): validate the role names, record the run, answer
// 202, and execute out-of-band — a role probe's budget is minutes, and a
// request that holds that long is a wedged connection in every client.
func (s *Server) handleDiagnosticsRun(w http.ResponseWriter, r *http.Request) {
	var req diagnosticsRunRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&req); err != nil && err.Error() != "EOF" {
		s.writeProblem(w, r, http.StatusBadRequest, "invalid-diagnostics-run",
			"Bad Request", "the run request body is not valid JSON")
		return
	}
	roles, err := parseDiagRoles(req.Roles)
	if err != nil {
		s.writeProblem(w, r, http.StatusBadRequest, "invalid-diagnostics-roles",
			"Bad Request", err.Error())
		return
	}
	run := s.diagRuns.create(roles)
	go s.execDiagnosticsRun(run)
	s.log.Info("diagnostics run started", "run_id", run.id, "roles", len(roles))
	writeJSON(w, http.StatusAccepted, diagnosticsRunRef{RunID: run.id})
}

// execDiagnosticsRun executes one run: the host snapshot first (it is cheap
// and the running run already reports it), then each role's doctor probe.
// The context is the process's, not a request's: the run outlives the POST.
func (s *Server) execDiagnosticsRun(run *diagnosticsRun) {
	ctx := context.Background()
	host := s.hostReport()
	run.setHost(host)
	// The configured? flag is one LoadRoles pass, not seven:
	// the same fact the settings page and the doctor use.
	roleCfgs, rolesErr := providers.LoadRoles(os.LookupEnv)
	for _, role := range run.roles {
		configured := rolesErr == nil && roleCfgs[role].Configured()
		c, err := diagProbe(ctx, doctor.Options{}, role)
		view := diagnosticsRoleView{
			Role:       string(role),
			Configured: configured,
			Status:     string(doctor.StatusFail),
		}
		if err != nil {
			// A probe that cannot even build (an invalid role configuration)
			// is a FAIL row that says so, not a silent skip: the operator
			// re-running doctor sees the same fault.
			view.Summary = safeDiagText(err.Error())
		} else {
			view.Status = c.Status
			view.Summary = c.Summary
			view.Detail = c.Detail
			view.Remedy = c.Remedy
			view.DurationMS = c.Duration.Milliseconds()
		}
		run.appendRole(view)
	}
	run.finish()
}

// parseDiagRoles validates the requested role names against docs/spec/06's
// seven (the enumeration is the allowlist, the capture selection's rule).
func parseDiagRoles(names []string) ([]providers.Role, error) {
	if len(names) > len(providers.AllRoles) {
		return nil, fmt.Errorf("at most %d roles per run (got %d)", len(providers.AllRoles), len(names))
	}
	seen := map[providers.Role]bool{}
	roles := make([]providers.Role, 0, len(names))
	for _, raw := range names {
		role := providers.Role(strings.TrimSpace(raw))
		known := false
		for _, r := range providers.AllRoles {
			if r == role {
				known = true
				break
			}
		}
		if !known {
			return nil, fmt.Errorf("%q is not one of the seven model roles of docs/spec/06", raw)
		}
		if seen[role] {
			continue
		}
		seen[role] = true
		roles = append(roles, role)
	}
	return roles, nil
}

// handleDiagnosticsGet answers GET /api/v1/diagnostics/{id} (the scope
// table's reserved viewer grant): the run's current shape, 404 for an id that
// was never created or already fell out of the bounded store.
func (s *Server) handleDiagnosticsGet(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		s.writeProblem(w, r, http.StatusBadRequest, "empty-run-id",
			"Bad Request", "the run id in the path is empty")
		return
	}
	run := s.diagRuns.get(id)
	if run == nil {
		s.writeCodeProblem(w, r, http.StatusNotFound, "diagnostics-run-unknown",
			"DIAGNOSTICS_RUN_UNKNOWN", "Not Found",
			"no diagnostics run with that id: it was never created, or it fell out of the bounded store")
		return
	}
	writeJSON(w, http.StatusOK, run.snapshot())
}

// safeDiagText is the redaction seam for probe error text that lands in a run
// row: the doctor's own safeText redacts the role's token, but a configuration
// error can carry other values, so the support redactor (ADR-0042's scrubber)
// is the second pass.
var safeDiagText = func(s string) string {
	return support.RedactText(s)
}

// ── the redacted log tail (GET /api/v1/diagnostics/logs) ────────────────────

const (
	diagLogsDefaultLimit = 200
	diagLogsMaxLimit     = 1000
)

// diagnosticsLogsView is the GET /api/v1/diagnostics/logs body. Every string
// has passed the support redactor before it left the process (AGENTS.md rule
// 5): a log line that names a gateway host, a private IP or a token is an
// operational detail, the way the support bundle treats it (ADR-0042).
type diagnosticsLogsView struct {
	Limit   int             `json:"limit"`
	Count   int             `json:"count"`
	Records []logRecordView `json:"records"`
	Note    string          `json:"note"`
}

type logRecordView struct {
	Time  string            `json:"time"`
	Level string            `json:"level"`
	Msg   string            `json:"msg"`
	Attrs map[string]string `json:"attrs"`
}

// handleDiagnosticsLogs answers GET /api/v1/diagnostics/logs (developer): the
// newest records of the process's log ring, redacted. Developer-tier like the
// dev-trace evidence route: system logs can carry provider request detail,
// and docs/spec/14 keeps that class behind the explicit role.
func (s *Server) handleDiagnosticsLogs(w http.ResponseWriter, r *http.Request) {
	if s.logRing == nil {
		s.writeCodeProblem(w, r, http.StatusServiceUnavailable, "logs-unavailable",
			"LOGS_UNAVAILABLE", "Service Unavailable",
			"no log ring is wired into this process")
		return
	}
	limit := diagLogsDefaultLimit
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			s.writeProblem(w, r, http.StatusBadRequest, "invalid-logs-limit",
				"Bad Request", "limit must be a positive integer")
			return
		}
		limit = n
	}
	if limit > diagLogsMaxLimit {
		limit = diagLogsMaxLimit
	}
	records := s.logRing.Tail(limit)
	out := make([]logRecordView, 0, len(records))
	for _, rec := range records {
		attrs := make(map[string]string, len(rec.Attrs))
		for k, v := range rec.Attrs {
			// Keys are the slog attribute names (safe); values are host data.
			attrs[support.RedactText(k)] = support.RedactText(v)
		}
		out = append(out, logRecordView{
			Time:  rec.Time.UTC().Format(time.RFC3339Nano),
			Level: rec.Level.String(),
			Msg:   support.RedactText(rec.Msg),
			Attrs: attrs,
		})
	}
	writeJSON(w, http.StatusOK, diagnosticsLogsView{
		Limit:   limit,
		Count:   len(out),
		Records: out,
		Note:    "every value passed the support-bundle redactor (URLs, tokens and private IPs are replaced); the tail is process-local — a restart clears it, and the durable path is the support bundle",
	})
}
