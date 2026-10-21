package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dbirks/game-narrator/internal/bootstrap"
)

// --- network ---------------------------------------------------------------

// checkListen probes the configured listen address by actually binding it.
// The signature it exists for: a board that carries two game-narrator
// installs (the packaged systemd unit and a hand-started user-level
// process, each with its own data directory) and a deploy that installs a
// new binary for one of them while the other still holds the port. The new
// build then never serves — its unit crash-loops on
// "listen tcp <host>:8080: bind: address already in use" — and on a
// CPU-bound appliance that crash loop is the visible incident. A bind that
// fails with EADDRINUSE names the other process, not this service
// (ADR-0150).
//
// A held address is not a fault by itself: operators run doctor WHILE the
// narrator is serving, so the holder is usually the healthy intended
// install. checkHeldListen asks the holder who it is (a short GET to the
// holder's system/info, loopback only) and disambiguates (ADR-0150,
// amendment): a game-narrator answer with no second-install evidence is
// OK, and second-install evidence or a foreign holder is FAIL.
//
// The probe is a real bind and close: an address that cannot be bound for
// any reason is FAIL, because this is the exact address the serve process
// would bind next. The address is configuration the operator set (flag >
// GAME_NARRATOR_LISTEN > default), so the row never invents one: with no
// address to probe it is NOT CONFIGURED.
func (r *runner) checkListen(ctx context.Context) Check {
	addr := strings.TrimSpace(r.opts.Listen)
	if addr == "" {
		return Check{
			Status:  StatusNotConfigured,
			Summary: "no listen address is configured for the check",
			Remedy:  "set " + bootstrap.EnvListen + " or pass --listen so the row can probe the port the service will bind",
		}
	}

	ln, err := net.Listen("tcp", addr)
	if err == nil {
		_ = ln.Close()
		return Check{
			Status:  StatusOK,
			Summary: fmt.Sprintf("listen address %s is free", addr),
			Detail:  "bound and released the address cleanly",
		}
	}

	var addrErr *net.AddrError
	if errors.As(err, &addrErr) {
		return Check{
			Status:  StatusFail,
			Summary: "the listen address is invalid",
			Detail:  oneLine(fmt.Sprintf("%s: %v", addr, err), 200),
			Remedy:  "fix the address in --listen or " + bootstrap.EnvListen,
		}
	}
	if errors.Is(err, syscall.EADDRINUSE) {
		return r.checkHeldListen(ctx, addr, err)
	}
	if errors.Is(err, os.ErrPermission) {
		return Check{
			Status:  StatusFail,
			Summary: fmt.Sprintf("this user cannot bind %s", addr),
			Detail:  oneLine(fmt.Sprintf("bind: %v", err), 200),
			Remedy:  "ports below 1024 need root; raise the port in " + bootstrap.EnvListen + " or run the service as the configured user",
		}
	}
	return Check{
		Status:  StatusFail,
		Summary: fmt.Sprintf("cannot bind the listen address %s", addr),
		Detail:  oneLine(fmt.Sprintf("bind: %v", err), 200),
		Remedy:  "check the address syntax, the firewall and the interface the host exposes; then re-run doctor",
	}
}

// holderProbeBudget bounds the identity probe to a held address: the holder
// is on this host (loopback only), so 2 s is generous and a wedged holder
// must not slow the report (AGENTS.md rule 7).
const holderProbeBudget = 2 * time.Second

// systemInfoProbe is the slice of the holder's /api/v1/system/info answer
// the check needs to name the holder: the version field the game-narrator
// serve process reports (internal/api; the web UI reads the same field). A
// 200 JSON answer with a non-empty version is the "answers as a
// game-narrator" signature. /api/v1/health/live is deliberately not an
// identifying signal: a bare liveness 200 is what any service answers.
type systemInfoProbe struct {
	Version string `json:"version"`
}

// probeHolder asks the process holding addr who it is: a short GET to the
// holder's system/info. Loopback addresses only — the probe never dials
// off-box (a configured LAN or tailnet address is a remote destination and
// this is a local disambiguation, not a network test). It returns
// (info, true) only for a 200 JSON answer with a non-empty version; a 404
// from a foreign service, a non-JSON answer, a timeout or an unreachable
// holder all return (zero, false).
func (r *runner) probeHolder(ctx context.Context, addr string) (systemInfoProbe, bool) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || !holderHostIsLoopback(host) {
		return systemInfoProbe{}, false
	}
	ctx, cancel := context.WithTimeout(ctx, holderProbeBudget)
	defer cancel()
	client := &http.Client{Timeout: holderProbeBudget}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://"+net.JoinHostPort(host, port)+"/api/v1/system/info", nil)
	if err != nil {
		return systemInfoProbe{}, false
	}
	resp, err := client.Do(req)
	if err != nil {
		return systemInfoProbe{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return systemInfoProbe{}, false
	}
	var info systemInfoProbe
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil || info.Version == "" {
		return systemInfoProbe{}, false
	}
	return info, true
}

// holderHostIsLoopback reports whether the host part of a listen address is
// a destination the identity probe may dial: the loopback literals and the
// forms that dial to localhost (the empty host of ":8080"). Anything else
// (a LAN or tailnet IP, a name) is off-box and stays unprobed.
func holderHostIsLoopback(host string) bool {
	if host == "" || host == "localhost" || host == "0.0.0.0" || host == "::" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// portOf is the port part of a listen address, for the remedy's ss grep.
func portOf(addr string) string {
	if _, port, err := net.SplitHostPort(addr); err == nil {
		return port
	}
	return addr
}

// checkHeldListen disambiguates the EADDRINUSE path (ADR-0150 amendment).
// The holder is asked who it is (probeHolder), then:
//
//   - a foreign holder (no game-narrator answer) is FAIL — another program
//     owns the configured port;
//   - a game-narrator holder plus second-install evidence is FAIL with the
//     two-install message. The doctor binary is itself named game-narrator,
//     so pgrep -x always counts it; the evidence uses the self-excluded
//     count — two other processes (the holder plus a second install), or
//     one other process while the unit has restarted and is not stably
//     active (a non-unit process holds the port);
//   - a game-narrator holder with no second-install evidence is OK: the
//     configured install is serving, which is the state operators run
//     doctor in.
func (r *runner) checkHeldListen(ctx context.Context, addr string, bindErr error) Check {
	holder, known := r.probeHolder(ctx, addr)
	if !known {
		return Check{
			Status:  StatusFail,
			Summary: fmt.Sprintf("another program holds %s: the holder does not answer as a game-narrator", addr),
			Detail:  oneLine(fmt.Sprintf("bind: %v; GET /api/v1/system/info on the loopback holder (budget %s) returned no game-narrator version", bindErr, holderProbeBudget), 300),
			Remedy:  fmt.Sprintf("find the holder on the board (`ss -ltnp | grep ':%s'`), stop it if it is not the intended game-narrator install, then re-run doctor", portOf(addr)),
		}
	}

	// The doctor binary is itself named game-narrator, so pgrep -x always
	// counts it: the second-install evidence needs a count of at least two
	// OTHER processes (the holder plus a second install), or one other
	// process plus the restart-looping unit below. Excluding the self PID
	// keeps a healthy board (one serve + this doctor) at the count of 1.
	ev := r.installEvidence()
	n, nerr := ev.GameNarratorProcesses(ctx)
	if nerr == nil && n >= 1+2 {
		return twoInstallCheck(addr, bindErr, fmt.Sprintf("evidence: %d game-narrator processes are running besides this doctor (pgrep -x, self excluded)", n-1))
	}
	// The restart-loop half of the incident, with the self-excluded count:
	// NRestarts > 0 while the unit is not stably active means its process
	// never holds the port, so a game-narrator holder is a non-unit
	// (user-level) process — the exact 2026-10-21 collision. One other
	// process is exactly that holder; zero is the belt-and-braces case.
	// A unit that IS active holds the port itself (the holder answered as
	// game-narrator and only one process can bind it), where NRestarts > 0
	// is restart history, not a loop.
	if nerr == nil && n >= 1 {
		if restarts, active, err := ev.UnitRestartState(ctx); err == nil && restarts > 0 && !active {
			return twoInstallCheck(addr, bindErr, fmt.Sprintf("evidence: the game-narrator unit shows NRestarts=%d and is not stably active while a non-unit process holds the port (systemctl show)", restarts))
		}
	}

	return Check{
		Status:  StatusOK,
		Summary: fmt.Sprintf("held by a running game-narrator %s", holder.Version),
		Detail:  fmt.Sprintf("GET /api/v1/system/info answered with version %q; no second-install evidence (one pgrep -x process besides this doctor, unit not restart-looping)", oneLine(holder.Version, 80)),
	}
}

// twoInstallCheck is the ADR-0150 two-install FAIL: the same summary and
// remedy the held-port row has carried since the incident; the specific
// evidence (a second live process, a restart-looping unit) goes in Detail.
func twoInstallCheck(addr string, bindErr error, evidence string) Check {
	return Check{
		Status:  StatusFail,
		Summary: fmt.Sprintf("another process holds %s; is a second game-narrator install running? (systemctl status game-narrator / pgrep -x game-narrator)", addr),
		Detail:  oneLine(fmt.Sprintf("bind: %v; %s; a second install (systemd unit vs user-level run) holding the port makes a deploy of the other one crash-loop on bind", bindErr, evidence), 300),
		Remedy:  "stop the other install before starting this one: `sudo systemctl stop game-narrator` for the unit, `kill $(pgrep -x game-narrator)` for a hand-started process (never `pkill -f`), then re-run doctor",
	}
}

// InstallEvidence is the seam over the process and systemd probes the
// network.listen row uses for the second-install evidence. The production
// implementation shells out (execInstallEvidence); tests inject a fake
// through Options.InstallEvidence so the suite never shells out
// (AGENTS.md rule 3: a synthetic stub is a stub, and it is labeled as one).
type InstallEvidence interface {
	// GameNarratorProcesses counts the running processes named exactly
	// game-narrator (pgrep -x game-narrator; -x is exact-name only, so the
	// shell that ran doctor is never counted). The count INCLUDES the
	// doctor process itself — its binary is named game-narrator — so the
	// caller reads n-1 "other" processes: a healthy board (one serve +
	// this doctor) counts 2, the two-install board counts 3.
	GameNarratorProcesses(ctx context.Context) (int, error)
	// UnitRestartState reports the game-narrator systemd unit's NRestarts
	// counter and whether the unit's main process is stably active
	// (systemctl show game-narrator -p NRestarts -p SubState; SubState
	// "active"). While the unit is active its process is the stable port
	// holder; while it is not, a game-narrator holding the port is a
	// non-unit process.
	UnitRestartState(ctx context.Context) (restarts int, active bool, err error)
}

// execInstallEvidence is the real InstallEvidence: the pgrep -x count and
// the systemctl show it parses. It runs only through the production check;
// the tests inject fakes.
type execInstallEvidence struct{ exec ExecFunc }

func (e execInstallEvidence) GameNarratorProcesses(ctx context.Context) (int, error) {
	res := e.exec(ctx, "pgrep", "-x", "game-narrator")
	switch res.ExitCode {
	case 0:
		return countNonEmptyLines(res.Stdout), nil
	case 1:
		return 0, nil // pgrep's no-match exit: the count is zero, not an error
	default:
		return 0, fmt.Errorf("pgrep exited %d: %s", res.ExitCode, oneLine(firstLine(res.Stderr), 120))
	}
}

func (e execInstallEvidence) UnitRestartState(ctx context.Context) (int, bool, error) {
	res := e.exec(ctx, "systemctl", "show", "game-narrator", "-p", "NRestarts", "-p", "SubState")
	// No unit or no systemd (dev boxes, containers): the row has no systemd
	// evidence rather than an error; the pgrep half of the evidence still
	// applies.
	if res.ExitCode != 0 {
		return 0, false, nil
	}
	restarts, active := 0, false
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "NRestarts="):
			restarts, _ = strconv.Atoi(strings.TrimPrefix(line, "NRestarts="))
		case strings.HasPrefix(line, "SubState="):
			active = strings.TrimPrefix(line, "SubState=") == "active"
		}
	}
	return restarts, active, nil
}

// installEvidence applies the InstallEvidence seam: tests inject a fake so
// the suite never shells out; nil uses the real pgrep/systemctl probes
// through the runner's Exec seam.
func (r *runner) installEvidence() InstallEvidence {
	if r.opts.InstallEvidence != nil {
		return r.opts.InstallEvidence
	}
	return execInstallEvidence{exec: r.opts.Exec}
}

// countNonEmptyLines counts a command's non-empty stdout lines (pgrep
// prints one PID per line).
func countNonEmptyLines(b []byte) int {
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}
