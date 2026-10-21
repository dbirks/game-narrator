package doctor

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// freeListenAddress reserves a TCP port and releases it, returning the
// address. The tiny race between release and rebind is accepted: the suite
// already relies on this pattern (cmd/game-narrator's freeListenAddress) and
// a test host's port space is not under adversarial load.
func freeListenAddress(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("cannot reserve a port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("cannot release the port: %v", err)
	}
	return addr
}

// heldListenAddress reserves a TCP port and keeps it bound for the test's
// duration without ever accepting: the "a holder that never answers"
// fixture, which exercises the probe's timeout budget.
func heldListenAddress(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("cannot hold a port: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().String()
}

// rejectingListenerAddress holds a loopback port with a plain TCP listener
// that accepts and immediately closes: a port held by a program that is not
// a game-narrator (no HTTP answer at all).
func rejectingListenerAddress(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("cannot hold a port: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return ln.Addr().String()
}

// narratorHTTPAddress serves /api/v1/system/info on a loopback address the
// way a game-narrator serve process answers (the version the row is
// expected to name). The version string is invented, in the shape the
// deploy stamps (ADR-0150): 0.1.0-<commit>.
func narratorHTTPAddress(t *testing.T, version string) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/system/info", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"` + version + `","commit":"test","listen_addresses":["127.0.0.1:18099"]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

// fakeInstallEvidence is the InstallEvidence seam for tests: the suite
// never shells out to pgrep/systemctl (AGENTS.md rule 3 — a synthetic stub
// is labeled as one).
type fakeInstallEvidence struct {
	processes  int
	processErr error
	restarts   int
	active     bool
	unitErr    error
}

func (f fakeInstallEvidence) GameNarratorProcesses(context.Context) (int, error) {
	return f.processes, f.processErr
}

func (f fakeInstallEvidence) UnitRestartState(context.Context) (int, bool, error) {
	return f.restarts, f.active, f.unitErr
}

// --- network.listen ---------------------------------------------------------

func TestListenCheckOKWhenTheAddressIsFree(t *testing.T) {
	addr := freeListenAddress(t)
	r := newRunner(Options{Listen: addr})

	c := r.checkListen(context.Background())
	mustStatus(t, c, StatusOK)
	if c.Summary != "listen address "+addr+" is free" {
		t.Fatalf("summary = %q, want the exact free-port wording", c.Summary)
	}
}

// The review defect this run fixes: operators run doctor WHILE the narrator
// is serving, so the configured address is normally held by the healthy
// intended install — and that is OK, not a FAIL (ADR-0150 amendment).
// pgrep -x counts the doctor itself too, so the healthy board is 2
// (serve + doctor), and the NRestarts=3 on an ACTIVE unit is restart
// history, not a loop: the active unit's process is the holder, so the row
// must still be OK.
func TestListenCheckOKWhenTheHolderIsTheNarrator(t *testing.T) {
	addr := narratorHTTPAddress(t, "0.1.0-fake1234")
	r := newRunner(Options{
		Listen:          addr,
		InstallEvidence: fakeInstallEvidence{processes: 2, restarts: 3, active: true},
	})

	c := r.checkListen(context.Background())
	mustStatus(t, c, StatusOK)
	want := "held by a running game-narrator 0.1.0-fake1234"
	if c.Summary != want {
		t.Fatalf("summary = %q, want %q (the version comes from the holder's system/info)", c.Summary, want)
	}
	if c.Remedy != "" {
		t.Fatalf("an OK row carries no remedy, got %q", c.Remedy)
	}
}

// A holder that does not answer as a game-narrator keeps the FAIL, with the
// message naming that another program holds the port (not the two-install
// message, which is reserved for the second-install evidence).
func TestListenCheckFailWhenAnotherProgramHoldsTheAddress(t *testing.T) {
	addr := rejectingListenerAddress(t)
	r := newRunner(Options{Listen: addr, InstallEvidence: fakeInstallEvidence{}})

	c := r.checkListen(context.Background())
	mustStatus(t, c, StatusFail)
	want := "another program holds " + addr + ": the holder does not answer as a game-narrator"
	if c.Summary != want {
		t.Fatalf("summary = %q\nwant %q", c.Summary, want)
	}
	if !strings.Contains(c.Remedy, "ss -ltnp") {
		t.Fatalf("a foreign-holder FAIL must name the holder-finding command, got %q", c.Remedy)
	}
}

// A foreign HTTP holder (a 404 from another service's layout) is the same
// foreign-holder FAIL.
func TestListenCheckFailWhenTheHolderIsAForeignHTTPService(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("service-x ok"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	r := newRunner(Options{Listen: srv.Listener.Addr().String(), InstallEvidence: fakeInstallEvidence{}})

	c := r.checkListen(context.Background())
	mustStatus(t, c, StatusFail)
	if !strings.HasPrefix(c.Summary, "another program holds ") {
		t.Fatalf("summary = %q, want the foreign-holder wording", c.Summary)
	}
}

// A game-narrator holder plus two more game-narrator processes (serve +
// second install + the doctor itself = 3 for pgrep -x) is the two-install
// FAIL with the existing message (ADR-0150): the summary and remedy are
// the incident wording, the evidence goes in the detail.
func TestListenCheckFailWhenTwoNarratorProcessesAreRunning(t *testing.T) {
	addr := narratorHTTPAddress(t, "0.1.0-fake1234")
	r := newRunner(Options{
		Listen:          addr,
		InstallEvidence: fakeInstallEvidence{processes: 3, restarts: 0, active: true},
	})

	c := r.checkListen(context.Background())
	mustStatus(t, c, StatusFail)
	want := "another process holds " + addr + "; is a second game-narrator install running? (systemctl status game-narrator / pgrep -x game-narrator)"
	if c.Summary != want {
		t.Fatalf("summary = %q\nwant %q", c.Summary, want)
	}
	if !strings.Contains(c.Detail, "2 game-narrator processes are running besides this doctor") {
		t.Fatalf("the detail must carry the self-excluded pgrep evidence, got %q", c.Detail)
	}
	if !strings.Contains(c.Remedy, "pkill -f") {
		t.Fatalf("the two-install remedy must keep the pkill -f warning, got %q", c.Remedy)
	}
}

// The restart-loop half of the 2026-10-21 incident: the unit has restarted
// and is not stably active, while a (user-level) game-narrator process
// holds the port — two installs, same two-install FAIL. pgrep -x counts 2:
// the user-level holder plus the doctor itself.
func TestListenCheckFailWhenTheUnitRestartLoopsAndANonUnitProcessHoldsThePort(t *testing.T) {
	addr := narratorHTTPAddress(t, "0.1.0-fake1234")
	r := newRunner(Options{
		Listen:          addr,
		InstallEvidence: fakeInstallEvidence{processes: 2, restarts: 503, active: false},
	})

	c := r.checkListen(context.Background())
	mustStatus(t, c, StatusFail)
	if !strings.Contains(c.Summary, "is a second game-narrator install running?") {
		t.Fatalf("summary = %q, want the two-install message", c.Summary)
	}
	if !strings.Contains(c.Detail, "NRestarts=503") {
		t.Fatalf("the detail must carry the unit evidence, got %q", c.Detail)
	}
}

// The holder probe must honour its short budget: a holder that accepts no
// connection (never answers) fails the row in about the budget, not after
// the 15 s check deadline (AGENTS.md rule 7).
func TestListenCheckProbeIsBoundedWhenTheHolderNeverAnswers(t *testing.T) {
	addr := heldListenAddress(t)
	r := newRunner(Options{Listen: addr, InstallEvidence: fakeInstallEvidence{}})

	started := time.Now()
	c := r.checkListen(context.Background())
	elapsed := time.Since(started)
	mustStatus(t, c, StatusFail)
	if elapsed > 5*time.Second {
		t.Fatalf("the holder probe took %s, want it bounded by the %s budget", elapsed, holderProbeBudget)
	}
}

func TestListenCheckNotConfiguredWithoutAnAddress(t *testing.T) {
	r := newRunner(Options{})

	c := r.checkListen(context.Background())
	mustStatus(t, c, StatusNotConfigured)
	if c.Summary != "no listen address is configured for the check" {
		t.Fatalf("summary = %q", c.Summary)
	}
}

func TestListenCheckUnparseableAddressIsFail(t *testing.T) {
	r := newRunner(Options{Listen: "not-a-port"})

	c := r.checkListen(context.Background())
	mustStatus(t, c, StatusFail)
	if c.Summary != "the listen address is invalid" {
		t.Fatalf("summary = %q, want the invalid-address wording", c.Summary)
	}
}

// The row must be present in a full run and, on a host that never named an
// address, must sit in NOT CONFIGURED rather than degrading the report.
func TestListenRowIsNotConfiguredInAPlainRun(t *testing.T) {
	h := newFakeHost(t)
	rep := h.run()
	mustStatus(t, h.check(rep, "network.listen"), StatusNotConfigured)
	if rep.Status != StatusOK {
		t.Fatalf("a NOT CONFIGURED listen row must not degrade the report, got %q", rep.Status)
	}
}

// A port held by a foreign program in a full run is a FAIL, and the report's
// worst state has to say so: that is the deploy incident the row exists for.
func TestListenRowFailsAFullRunWhenAProgramHoldsThePort(t *testing.T) {
	h := newFakeHost(t)
	h.opts.Listen = rejectingListenerAddress(t)
	rep := h.run()

	c := h.check(rep, "network.listen")
	mustStatus(t, c, StatusFail)
	if !rep.HasFail() {
		t.Fatal("a held listen address must make the report HasFail (exit code 1)")
	}
}

// A port held by the serving game-narrator itself in a full run is OK — the
// state operators run doctor in — and must not degrade the report.
func TestListenRowIsOKInAFullRunWhenTheNarratorHoldsThePort(t *testing.T) {
	h := newFakeHost(t)
	h.opts.Listen = narratorHTTPAddress(t, "0.1.0-fake1234")
	h.opts.InstallEvidence = fakeInstallEvidence{processes: 2, restarts: 0, active: true}
	rep := h.run()

	c := h.check(rep, "network.listen")
	mustStatus(t, c, StatusOK)
	if !strings.Contains(c.Summary, "held by a running game-narrator") {
		t.Fatalf("summary = %q, want the narrator-held wording", c.Summary)
	}
	if rep.HasFail() {
		t.Fatalf("a narrator-held port must not make the report HasFail, got %q", rep.Status)
	}
}
