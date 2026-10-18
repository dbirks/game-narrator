package hwsmoke

// TestHWResourceBudget is the executable half of docs/spec/10 "Check UNO Q
// load, temperature/RAM, available persistent storage … and data dir
// writability" and of the "Resource budgets" section ("UNO Q 4 GB must
// remain stable …").
//
// Every host: the RAM/CPU/temperature/storage preflight runs, and the serve
// binary runs for a 60 s replay session (temp data dir, replay roots at the
// repository fixtures when the binary was built from the repo) while its
// RSS is sampled. The binary is the installed one when available
// (GAME_NARRATOR_BINARY, else game-narrator on PATH — the appliance's .deb
// install) and a build from the module root on the dev host; with neither
// the session is NOT RUN-explained. Only on the UNO Q profile — arm64 with
// < 5 GB RAM — is the measured RSS asserted against the documented serve
// budget; on every other host the budget assertion is NOT RUN-explained
// ("not the UNO Q profile") and the numbers are the logged evidence.

import (
	"bytes"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/bootstrap"
)

// sessionDuration is the replay-session length the budget is measured over:
// long enough for caches and page-in to settle, short enough for the gate
// to stay a smoke test.
const sessionDuration = 60 * time.Second

// minFreeMiB is the storage preflight bound: the data dir holds the SQLite
// catalogue plus the content-addressed asset store and speech cache
// (docs/spec/02), so less than 512 MiB free cannot hold a real game.
const minFreeMiB = 512

// sampleInterval paces the RSS sampling across the session.
const sampleInterval = 2 * time.Second

func TestHWResourceBudget(t *testing.T) {
	hwGate(t)
	memTotalKiB := memTotalKiBFromProc()
	ncpu := runtime.NumCPU()
	loads := loadAverage()
	temps := thermalZones()

	t.Logf("host profile: arch=%s mem_total=%s cpu=%d load=%s",
		runtime.GOARCH, miB(memTotalKiB), ncpu, loadString(loads))
	if memTotalKiB == 0 {
		t.Logf("note: /proc/meminfo unreadable — RAM recorded as unknown, UNO Q profile cannot be detected")
	}
	for _, z := range temps {
		t.Logf("thermal: %s", z)
	}
	if len(temps) == 0 {
		t.Logf("thermal: no /sys/class/thermal zones on this host (not the UNO Q)")
	}

	// Data-dir preflight: the dir the app would actually use, writability
	// (the app creates it at boot, so create-if-absent mirrors serve), and
	// the space a game, its assets and the speech cache need.
	dataDir := bootstrap.ResolveDataDir("", os.LookupEnv)
	if dataDir == "" {
		t.Fatalf("cannot resolve the app data dir (set %s or HOME)", bootstrap.EnvDataDir)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("data dir %s not creatable: %v", dataDir, err)
	}
	probe := filepath.Join(dataDir, ".hwsmoke-write-probe")
	if err := os.WriteFile(probe, []byte("hwsmoke\n"), 0o600); err != nil {
		t.Fatalf("data dir %s not writable: %v", dataDir, err)
	}
	_ = os.Remove(probe)
	freeKiB, err := freeSpaceKiB(dataDir)
	if err != nil {
		t.Fatalf("cannot stat the data dir filesystem: %v", err)
	}
	t.Logf("data dir: %s (writable, free space %s; bound %d MiB)", dataDir, miB(freeKiB), minFreeMiB)
	if freeKiB < minFreeMiB*1024 {
		t.Fatalf("data dir free space %s is under the %d MiB bound: free space or move %s",
			miB(freeKiB), minFreeMiB, dataDir)
	}

	// The 60 s replay session: the built binary, a temp data dir (never the
	// operator's real one), replay roots at the repository fixtures so the
	// session's replay input is available exactly as serve would admit it.
	session := runServeSession(t)

	// The documented budget assertion: UNO Q profile only.
	if isUNOQProfile(memTotalKiB) {
		if session.maxRSSMiB > serveRSSBudgetMiB {
			t.Fatalf("serve RSS %d MiB over the 60 s session exceeds the documented %d MiB UNO Q budget "+
				"(docs/manuals/13-uno-q-hardware-smoke.md): the appliance profile is not met",
				session.maxRSSMiB, serveRSSBudgetMiB)
		}
		t.Logf("RSS budget: %d MiB peak over %s (budget %d MiB) — UNO Q profile assertion met",
			session.maxRSSMiB, sessionDuration, serveRSSBudgetMiB)
	} else {
		t.Logf("NOT RUN: serve RSS budget assertion — not the UNO Q profile (arch=%s, RAM=%s); "+
			"measured peak %d MiB over %s is recorded as evidence",
			runtime.GOARCH, miB(memTotalKiB), session.maxRSSMiB, sessionDuration)
	}
}

// serveSession is one timed run of the built serve binary.
type serveSession struct {
	maxRSSMiB int
	duration  time.Duration
	bin       string
}

// runServeSession resolves the serve binary (installed, else built), boots
// it with a temp data dir, samples /proc RSS for sessionDuration and stops
// it gracefully. A process that dies before the window ends is a failure
// with its output tail — a serve that cannot stay up cannot be budgeted.
func runServeSession(t *testing.T) serveSession {
	t.Helper()
	bin, replayRoot, missing := serveBinary(t)
	if missing != "" {
		notRun(t, missing,
			"install the game-narrator .deb (game-narrator on PATH) or set "+EnvBinary+
				" to the installed binary, or run this gate on a dev host with the module checkout")
	}
	t.Logf("budget session: serve binary %s", bin)

	// A free port the session listens on: nothing else may be able to hit
	// it, and the session must not collide with a running dev server.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("cannot reserve a listen port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	dataDir := t.TempDir()
	env := os.Environ()
	// Replay roots only where the repo's fixtures exist with the binary
	// (the dev-host build); the installed binary keeps the production
	// default (none) — see serveBinary.
	if replayRoot != "" {
		env = append(env, "GAME_NARRATOR_REPLAY_ROOTS="+replayRoot)
	}
	cmd := exec.Command(bin, "serve", "--listen", addr, "--data-dir", dataDir)
	cmd.Env = env
	var out bytes.Buffer
	// Bounded capture: a wedged server must not grow the test's heap
	// without bound (the same rule ADR-0012 applies to tool output).
	cmd.Stdout = &boundedWriter{buf: &out, cap: 64 << 10}
	cmd.Stderr = &boundedWriter{buf: &out, cap: 64 << 10}
	if err := cmd.Start(); err != nil {
		t.Fatalf("cannot start the serve subprocess: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	deadline := time.Now().Add(sessionDuration)
	var (
		maxRSS  int
		samples int
	)
	for time.Now().Before(deadline) {
		time.Sleep(sampleInterval)
		rss, ok := processRSSMiB(cmd.Process.Pid)
		if !ok {
			continue // /proc miss: the post-loop liveness check is the assertion
		}
		samples++
		if rss > maxRSS {
			maxRSS = rss
		}
		if samples%5 == 0 {
			t.Logf("budget session: elapsed %s, RSS %d MiB (peak so far)",
				roundDuration(time.Since(deadline.Add(-sessionDuration))), rss)
		}
	}
	if _, alive := processRSSMiB(cmd.Process.Pid); !alive {
		t.Fatalf("the serve process vanished during the session: %s", tail(out.Bytes(), 2000))
	}
	elapsed := time.Since(deadline.Add(-sessionDuration))
	if elapsed < sessionDuration-sampleInterval {
		t.Fatalf("session ended after %s, want the full %s", roundDuration(elapsed), sessionDuration)
	}
	if samples == 0 {
		t.Fatalf("no RSS samples could be read from /proc during the session: RSS sampling unavailable on this host")
	}

	_ = cmd.Process.Signal(syscall.SIGTERM) // serve drains gracefully on SIGTERM
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		t.Logf("budget session: serve stopped gracefully on SIGTERM after %s (peak RSS %d MiB over %d samples)",
			roundDuration(elapsed), maxRSS, samples)
	case <-time.After(15 * time.Second):
		t.Logf("budget session: serve did not stop within 15 s of SIGTERM; killing (peak RSS %d MiB)", maxRSS)
		_ = cmd.Process.Kill()
		<-done
	}
	return serveSession{maxRSSMiB: maxRSS, duration: elapsed, bin: bin}
}
