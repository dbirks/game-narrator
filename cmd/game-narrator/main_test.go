package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/bootstrap"
	"github.com/dbirks/game-narrator/internal/store"
)

// The exit code contract lives here: usage errors are 2, runtime failures are
// 1, success is 0 (AGENTS.md CLI targets).

func TestRunUnknownCommandExitsTwo(t *testing.T) {
	code, err := run([]string{"bogus-command"}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 (err %v)", code, err)
	}
	if err == nil {
		t.Fatal("unknown command returned nil error")
	}
}

func TestRunNoArgsExitsTwo(t *testing.T) {
	code, _ := run(nil, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

func TestRunServeRejectsBadListenAddress(t *testing.T) {
	code, err := run([]string{"serve", "--listen", "not-an-address"},
		slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 (err %v)", code, err)
	}
	if err == nil {
		t.Fatal("invalid listen address returned nil error")
	}
}

func TestRunServeRejectsUnknownFlag(t *testing.T) {
	code, _ := run([]string{"serve", "--nope"}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

// TestRunMigrateEndToEnd exercises the real `game-narrator migrate` path
// against a temp data dir: it must create the database, print one line per
// applied migration plus the schema version, and then be a no-op (docs/spec/02
// "Migration and backup acceptance", docs/spec/16 Milestone 0).
func TestRunMigrateEndToEnd(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	dataDir := filepath.Join(t.TempDir(), "narrator-data")
	dbPath := store.DBPath(dataDir)

	first := captureStdout(t, func() int {
		code, err := run([]string{"migrate", "--data-dir", dataDir}, log)
		if err != nil {
			t.Fatalf("migrate: %v", err)
		}
		return code
	})
	if first.code != 0 {
		t.Fatalf("exit code = %d, want 0", first.code)
	}
	// Derived from the embedded migrations, never typed: the CLI's job is to apply
	// *this build's* migration set, so pinning "three files, version 3" here would
	// turn the next migration into a failing CLI test rather than a schema change.
	want := ""
	for _, name := range store.MigrationNames() {
		want += "applied " + name + "\n"
	}
	want += fmt.Sprintf("schema version %d\n", store.LatestSchemaVersion())
	if first.output != want {
		t.Fatalf("first run output =\n%q\nwant\n%q", first.output, want)
	}

	// The database really exists at the conventional path and really is at the
	// version this build ships: verify through the store, not by parsing our own
	// stdout.
	st, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("Open after migrate: %v", err)
	}
	if v, err := st.SchemaVersion(context.Background()); err != nil || v != store.LatestSchemaVersion() {
		t.Fatalf("SchemaVersion after migrate = %d, %v; want %d", v, err, store.LatestSchemaVersion())
	}
	if err := st.IntegrityCheck(context.Background()); err != nil {
		t.Fatalf("IntegrityCheck after migrate: %v", err)
	}
	st.Close()

	second := captureStdout(t, func() int {
		code, err := run([]string{"migrate", "--data-dir", dataDir}, log)
		if err != nil {
			t.Fatalf("second migrate: %v", err)
		}
		return code
	})
	if second.code != 0 {
		t.Fatalf("second exit code = %d, want 0", second.code)
	}
	if want := fmt.Sprintf("schema version %d\n", store.LatestSchemaVersion()); second.output != want {
		t.Fatalf("second run output =\n%q\nwant\n%q", second.output, want)
	}
}

// TestRunMigrateHonoursDataDirPrecedence proves the env var is used when no
// flag is given (and the flag still wins over it).
func TestRunMigrateHonoursDataDirPrecedence(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	envDir := filepath.Join(t.TempDir(), "from-env")
	t.Setenv(bootstrap.EnvDataDir, envDir)

	captureStdout(t, func() int {
		code, err := run([]string{"migrate"}, log)
		if err != nil {
			t.Fatalf("migrate from env: %v", err)
		}
		if code != 0 {
			t.Fatalf("exit code = %d, want 0", code)
		}
		return code
	})
	if _, err := os.Stat(store.DBPath(envDir)); err != nil {
		t.Fatalf("GAME_NARRATOR_DATA_DIR was not used: %v", err)
	}

	flagDir := filepath.Join(t.TempDir(), "from-flag")
	captureStdout(t, func() int {
		code, _ := run([]string{"migrate", "--data-dir", flagDir}, log)
		return code
	})
	if _, err := os.Stat(store.DBPath(flagDir)); err != nil {
		t.Fatalf("--data-dir was not used: %v", err)
	}
}

func TestRunMigrateUsageAndValidation(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	cases := []struct {
		name string
		args []string
	}{
		{"unknown flag", []string{"migrate", "--nope"}},
		{"unexpected argument", []string{"migrate", "extra"}},
		{"relative data dir", []string{"migrate", "--data-dir", "relative/dir"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if code, _ := run(tc.args, log); code != 2 {
				t.Fatalf("exit code = %d, want 2", code)
			}
		})
	}
}

// capturedStdout holds what a command wrote to stdout plus its exit code.
type capturedStdout struct {
	output string
	code   int
}

// captureStdout redirects the package-level stdout sink so the CLI output can
// be asserted exactly. It restores the original writer when the test ends.
func captureStdout(t *testing.T, fn func() int) capturedStdout {
	t.Helper()
	prev := stdout
	buf := &bytes.Buffer{}
	stdout = buf
	defer func() { stdout = prev }()

	code := fn()
	return capturedStdout{output: buf.String(), code: code}
}

// --- doctor ---------------------------------------------------------------

// clearGatewayEnv makes the doctor CLI tests independent of whatever gateway
// the developer shell happens to export: with these set to empty the roles are
// unconfigured, which is the first-run state under test.
func clearGatewayEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GAME_NARRATOR_GATEWAY_URL", "")
	t.Setenv("GAME_NARRATOR_GATEWAY_TOKEN", "")
	for _, role := range []string{
		"AGENT_REASONING", "FAST_REASONING", "VISION_OCR_FALLBACK", "DECISION",
		"VOICE_DESIGN", "VOICE_CLONE", "SPEECH_ASR",
	} {
		t.Setenv("GAME_NARRATOR_ROLE_"+role+"_MODEL", "")
	}
}

func TestRunDoctorReportsEveryComponentAsJSON(t *testing.T) {
	clearGatewayEnv(t)
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	dataDir := filepath.Join(t.TempDir(), "narrator")

	out := captureStdout(t, func() int {
		code, err := run([]string{"doctor", "--data-dir", dataDir, "--json"}, log)
		if err != nil {
			t.Fatalf("doctor: %v", err)
		}
		return code
	})
	if out.code != 0 {
		t.Fatalf("exit code = %d, want 0 on a host with no gateway:\n%s", out.code, out.output)
	}

	var report struct {
		GeneratedAt time.Time `json:"generated_at"`
		Status      string    `json:"status"`
		Totals      struct {
			OK            int `json:"ok"`
			Degraded      int `json:"degraded"`
			Blocked       int `json:"blocked"`
			NotConfigured int `json:"not_configured"`
			Fail          int `json:"fail"`
			Checks        int `json:"checks"`
		} `json:"totals"`
		Checks []struct {
			ID       string `json:"id"`
			Group    string `json:"group"`
			Status   string `json:"status"`
			Summary  string `json:"summary"`
			Duration int64  `json:"duration_ns"`
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(out.output), &report); err != nil {
		t.Fatalf("--json output is not the documented schema: %v\n%s", err, out.output)
	}
	if report.Totals.Checks != len(report.Checks) || len(report.Checks) < 15 {
		t.Fatalf("report has %d checks (totals %d), want at least the 15 core components",
			len(report.Checks), report.Totals.Checks)
	}
	if report.Status == "" || report.GeneratedAt.IsZero() {
		t.Fatalf("report envelope incomplete: %+v", report)
	}

	// The IDs are the contract downstream tooling reads; the states are not
	// asserted because this host may legitimately lack ffmpeg or a DAC.
	want := map[string]bool{
		"storage.data_dir": true, "storage.database": true,
		"tools.ffmpeg": true, "tools.v4l2_ctl": true, "tools.tesseract": true,
		"ocr.controlled_image": true, "capture.devices": true, "audio.sinks": true,
		"providers.agent_reasoning": true, "providers.fast_reasoning": true,
		"providers.vision_ocr_fallback": true, "providers.decision": true,
		"providers.voice_design": true, "providers.voice_clone": true,
		"providers.speech_asr": true,
	}
	seen := map[string]bool{}
	for _, c := range report.Checks {
		if c.Status == "" || c.Summary == "" || c.Group == "" {
			t.Errorf("check %s is incomplete: %+v", c.ID, c)
		}
		if c.ID == "emulator.dolphin" || c.ID == "game.image" {
			t.Errorf("%s appeared without --dev", c.ID)
		}
		seen[c.ID] = true
	}
	for id := range want {
		if !seen[id] {
			t.Errorf("doctor did not report %s (docs/spec/16 M0: configured and missing components must both be listed)", id)
		}
	}
}

func TestRunDoctorDevAddsEmulatorChecks(t *testing.T) {
	clearGatewayEnv(t)
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))

	out := captureStdout(t, func() int {
		code, _ := run([]string{"doctor", "--data-dir", filepath.Join(t.TempDir(), "d"), "--dev", "--json"}, log)
		return code
	})
	var report struct {
		Checks []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(out.output), &report); err != nil {
		t.Fatalf("--dev --json output: %v\n%s", err, out.output)
	}
	found := map[string]string{}
	for _, c := range report.Checks {
		found[c.ID] = c.Status
	}
	for _, id := range []string{"emulator.dolphin", "game.image"} {
		if _, ok := found[id]; !ok {
			t.Fatalf("%s missing with --dev: %v", id, found)
		}
	}
}

// TestRunDoctorExitsOneWhenAConfiguredComponentIsBroken is the exit-code
// contract scripts rely on: a broken component is 1, a missing optional one is
// not.
func TestRunDoctorExitsOneWhenAConfiguredComponentIsBroken(t *testing.T) {
	clearGatewayEnv(t)
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))

	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() int {
		code, err := run([]string{"doctor", "--data-dir", filepath.Join(blocker, "nested"), "--json"}, log)
		if err != nil {
			t.Fatalf("doctor must not return an error for a failing check, only exit 1: %v", err)
		}
		return code
	})
	if out.code != 1 {
		t.Fatalf("exit code = %d, want 1:\n%s", out.code, out.output)
	}
	if !strings.Contains(out.output, `"id": "storage.data_dir"`) || !strings.Contains(out.output, `"status": "FAIL"`) {
		t.Fatalf("report does not name the failure:\n%s", out.output)
	}
}

func TestRunDoctorHumanOutputIsATable(t *testing.T) {
	clearGatewayEnv(t)
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))

	out := captureStdout(t, func() int {
		code, _ := run([]string{"doctor", "--data-dir", filepath.Join(t.TempDir(), "d")}, log)
		return code
	})
	for _, want := range []string{"storage.data_dir", "providers.fast_reasoning", "Totals:", "OK", "NOT CONFIGURED"} {
		if !strings.Contains(out.output, want) {
			t.Fatalf("human report missing %q:\n%s", want, out.output)
		}
	}
}

func TestRunDoctorUsageErrors(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	cases := []struct {
		name string
		args []string
	}{
		{"unknown flag", []string{"doctor", "--nope"}},
		{"unexpected argument", []string{"doctor", "extra"}},
		{"relative data dir", []string{"doctor", "--data-dir", "relative/dir"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if code, _ := run(tc.args, log); code != 2 {
				t.Fatalf("exit code = %d, want 2", code)
			}
		})
	}
}

// --- serve readiness --------------------------------------------------------

// TestStoreReadinessTracksTheLiveDatabase exercises the exact function `serve`
// wires into /api/v1/health/ready: it must answer from the open store and flip
// to fail as soon as the store stops answering, which is what turns the endpoint
// into a 503 (see internal/api readiness tests).
func TestStoreReadinessTracksTheLiveDatabase(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	ctx := context.Background()
	st, err := store.Open(ctx, store.DBPath(filepath.Join(t.TempDir(), "data")))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	ready := storeReadiness(st, log)
	checks, ok := ready(ctx)
	if !ok {
		t.Fatalf("readiness = not ready with a healthy store: %v", checks)
	}
	database, _ := checks["database"].(map[string]any)
	if want := store.LatestSchemaVersion(); database == nil || database["status"] != "ok" || database["schema_version"] != want {
		t.Fatalf("checks.database = %v, want status ok at schema version %d", checks, want)
	}

	// A closed pool is the cheapest reliable way to make the ping fail; the
	// endpoint must not keep claiming readiness after that.
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	checks, ok = ready(ctx)
	if ok {
		t.Fatalf("readiness = ready after the store was closed: %v", checks)
	}
	database, _ = checks["database"].(map[string]any)
	if database == nil || database["status"] != "fail" {
		t.Fatalf("checks.database = %v, want a fail entry", checks)
	}
	if path := fmt.Sprint(checks); strings.Contains(path, "game-narrator.sqlite") {
		t.Fatalf("readiness payload leaked the database path: %s", path)
	}
}

func TestServeMigratesBeforeListening(t *testing.T) {
	clearGatewayEnv(t)
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	dataDir := filepath.Join(t.TempDir(), "serve-data")
	dbPath := store.DBPath(dataDir)

	if _, err := os.Stat(dbPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the database should not exist yet: %v", err)
	}

	addr := freeListenAddress(t)
	serveDone := make(chan int, 1)
	go func() {
		code, _ := run([]string{"serve", "--listen", addr, "--data-dir", dataDir}, log)
		serveDone <- code
	}()

	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(10 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://" + addr + "/api/v1/health/ready")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				last = string(body)
				break
			}
			last = fmt.Sprintf("HTTP %d %s", resp.StatusCode, body)
		} else {
			last = err.Error()
		}
		time.Sleep(25 * time.Millisecond)
	}
	if want := fmt.Sprintf(`"schema_version":%d`, store.LatestSchemaVersion()); !strings.Contains(last, want) {
		syscall.Kill(os.Getpid(), syscall.SIGTERM)
		t.Fatalf("readiness never reported the migrated database: %s", last)
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("serve did not create the database before listening: %v", err)
	}

	// Drain the way systemd would. The signal is delivered to this process and
	// taken by serve's signal.NotifyContext, so the handler under test is the
	// real startup and shutdown path, not a reimplementation of it.
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("signalling serve: %v", err)
	}
	select {
	case code := <-serveDone:
		if code != 0 {
			t.Fatalf("serve exit code = %d, want 0 after a clean drain", code)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve never shut down after SIGTERM — the store would not be closed either")
	}

	// The store was closed on shutdown, so a reopening read still works but the
	// process that held it is gone: proves defer ordering in runServe.
	st, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("reopening after shutdown: %v", err)
	}
	defer st.Close()
	if v, err := st.SchemaVersion(context.Background()); err != nil || v != store.LatestSchemaVersion() {
		t.Fatalf("schema version after restart = %d (%v), want %d", v, err, store.LatestSchemaVersion())
	}
}

// freeListenAddress reserves a port, releases it and returns it. The tiny race
// that leaves is preferable to a fixed port that collides with a developer's
// running service.
func freeListenAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("releasing the port: %v", err)
	}
	return addr
}
