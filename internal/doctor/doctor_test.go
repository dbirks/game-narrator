package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/audio"
	"github.com/dbirks/game-narrator/internal/bootstrap"
	"github.com/dbirks/game-narrator/internal/store"
)

// fixedAudioDevices is the enumeration the fake host's AudioDevices seam
// returns by default: a USB DAC the user could pick, a platform-default null
// sink the selection routes to when no device is named, and the capture
// dongle's UAC (USB audio-class) interface — the identity the G5 guard must
// refuse. A UAC-named sink and a USB DAC share a kind, so the guard reads the
// identity string, not the kind: this fixture is what pins that difference.
var fixedAudioDevices = audio.List{
	Backend:   "synthetic",
	Synthetic: true,
	Devices: []audio.Device{
		{ID: "alsa_output.usb-JBL_Flip_6_0011223344", Name: "JBL Flip 6", Kind: audio.KindUSB},
		{ID: "auto_null", Name: "Null Output", Kind: audio.KindNull, PlatformDefault: true},
		{ID: "alsa_output.usb-General_UAC_Device_001", Name: "Generic UAC Device", Kind: audio.KindUSB},
	},
}

// The doctor suite must pass on a host with none of the tools installed, so
// every check is exercised through the injected seams. The two tests that call
// real binaries say why they skip (docs/spec/12 "Unit" mode: never pretend a
// mock was the real thing).

// fakeHost is the whole host surface: tools on PATH, command results, glob
// matches, device permissions, environment and data directory.
type fakeHost struct {
	t    *testing.T
	opts Options

	mu      sync.Mutex
	tools   map[string]bool
	answers map[string]CommandResult
	calls   []string
	devices []string
	env     map[string]string
	denied  bool
}

func newFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	dir := t.TempDir()
	// A working host has a private data directory; t.TempDir is not 0700 by
	// default on every Go release, and the doctor must see the good case here.
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	h := &fakeHost{
		t:       t,
		tools:   map[string]bool{},
		env:     map[string]string{},
		answers: map[string]CommandResult{},
	}
	for _, name := range []string{"ffmpeg", "v4l2-ctl", "tesseract", "pactl", "aplay", "dolphin-emu", "Xvfb"} {
		h.tools[name] = true
	}
	h.exec("ffmpeg", "ffmpeg version n9.0.2 test banner\nlibavutil 59\n")
	h.exec("v4l2-ctl", "v4l2-ctl 1.32.0\n")
	// Keys match the command line by longest prefix, so the version probe and
	// the OCR run of the same binary can answer differently.
	h.exec("tesseract", controlledText+"\n")
	h.exec("tesseract --version", "tesseract 5.5.3\n leptonica-1.87.0\n")
	h.exec("pactl", "0\tsink0\tlibpulse-simple protocol-native.c\t1\n1\tsink1\tlibpulse-simple\t0\n")
	h.exec("aplay", "card 0: Dummy [Dummy], device 0: Dummy HDMI [Dummy HDMI]\n  Subdevices: 1/1\n")
	// The capture check's mode selection runs --list-formats-ext per capable
	// node and reads the USB link from sysfs; both get fixture answers so the
	// suite never depends on the host's real /dev/video* or USB bus
	// (AGENTS.md rule 3). The formats fixture is a labelled-synthetic
	// MS2130-class enumeration (testdata/v4l2-formats-ms2130.txt).
	h.exec("v4l2-ctl -d /dev/video0 --list-formats-ext", v4l2Formats(t, "v4l2-formats-ms2130.txt"))
	h.exec("v4l2-ctl -d /dev/video1 --list-formats-ext", v4l2Formats(t, "v4l2-formats-ms2130.txt"))

	h.opts = Options{
		DataDir: dir,
		// The fake host's budgets, scaled by the race build tag (issue #22):
		// the storage.database check is a real SQLite migration, and an
		// unscaled 3 s ceiling times it out on a saturated, race-instrumented
		// host without the check having failed.
		Timeout:      3 * time.Second * checkTimeoutScale,
		ModelTimeout: 5 * time.Second * checkTimeoutScale,
		LookupEnv:    h.lookupEnv,
		LookPath:     h.lookPath,
		Exec:         h.runCommand,
		Glob:         h.glob,
		DeviceOpen:   h.openDevice,
		// The audio.selected check reads the output-device enumeration through
		// this seam: a fixed, deterministic list (the real enumerator would
		// run pactl/aplay against the host's live sinks and make the suite
		// host-dependent, AGENTS.md rule 3).
		AudioDevices: func(context.Context, func(string) (string, bool)) (audio.List, error) {
			return fixedAudioDevices, nil
		},
		// The USB link behind the fixture capture nodes: a SuperSpeed card on
		// a SuperSpeed port (the healthy case). Tests that want the downgrade
		// fault or an unreadable link re-point this.
		SysfsRoot: fakeSysfs(t, "5000", "5000"),
		Now:       func() time.Time { return time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC) },
	}
	return h
}

func (h *fakeHost) lookupEnv(name string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	v, ok := h.env[name]
	return v, ok
}

func (h *fakeHost) lookPath(name string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.tools[name] {
		return filepath.Join("/fake/bin", name), nil
	}
	return "", fmt.Errorf("%s: not found in $PATH", name)
}

func (h *fakeHost) runCommand(_ context.Context, name string, args ...string) CommandResult {
	h.mu.Lock()
	defer h.mu.Unlock()
	line := strings.Join(append([]string{name}, args...), " ")
	h.calls = append(h.calls, line)
	// Longest registered prefix wins, so "tesseract --version" can differ from
	// the plain "tesseract" answer used by the OCR probe.
	best, found := "", false
	for key := range h.answers {
		if (key == line || strings.HasPrefix(line, key+" ")) && (!found || len(key) > len(best)) {
			best, found = key, true
		}
	}
	if found {
		return h.answers[best]
	}
	return CommandResult{Err: fmt.Errorf("unexpected call: %s", line), ExitCode: -1}
}

func (h *fakeHost) glob(string) ([]string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.devices...), nil
}

func (h *fakeHost) openDevice(string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.denied {
		return os.ErrPermission
	}
	return nil
}

// --- test-side setters -----------------------------------------------------

func (h *fakeHost) exec(name, stdout string) {
	h.execResult(name, CommandResult{Stdout: []byte(stdout)})
}

func (h *fakeHost) execResult(name string, res CommandResult) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.answers[name] = res
}

func (h *fakeHost) execFail(name, msg string) {
	h.execResult(name, CommandResult{Err: errors.New(msg), ExitCode: 1})
}

func (h *fakeHost) withoutTools(names ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, n := range names {
		delete(h.tools, n)
	}
}

func (h *fakeHost) withEnv(name, value string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.env[name] = value
}

func (h *fakeHost) withDevices(nodes ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.devices = nodes
}

func (h *fakeHost) denyDevices() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.denied = true
}

func (h *fakeHost) dataDir() string { return h.opts.DataDir }

func (h *fakeHost) calledWith(prefix string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func (h *fakeHost) invoked() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.calls...)
}

func (h *fakeHost) options() Options { return h.opts }

// run executes the report and returns it.
func (h *fakeHost) run() Report {
	h.t.Helper()
	return Run(context.Background(), h.options())
}

// check returns one row, failing the test if it is missing.
func (h *fakeHost) check(rep Report, id string) Check {
	h.t.Helper()
	for _, c := range rep.Checks {
		if c.ID == id {
			return c
		}
	}
	h.t.Fatalf("report has no %q check: %v", id, ids(rep))
	return Check{}
}

func ids(rep Report) []string {
	out := make([]string, 0, len(rep.Checks))
	for _, c := range rep.Checks {
		out = append(out, c.ID)
	}
	return out
}

// mustStatus asserts the state and prints the whole row when it differs, so a
// failure explains itself without re-running the test.
func mustStatus(t *testing.T, c Check, want string) {
	t.Helper()
	if c.Status != want {
		t.Fatalf("%s status = %s: %s / %s / fix: %s — want %s", c.ID, c.Status, c.Summary, c.Detail, c.Remedy, want)
	}
}

// --- report shape ----------------------------------------------------------

// TestRunReportsEveryComponentExactlyOnce is the docs/spec/16 Milestone 0 gate:
// the diagnostic report lists every configured and missing component, once, in
// a stable order.
func TestRunReportsEveryComponentExactlyOnce(t *testing.T) {
	h := newFakeHost(t)
	rep := h.run()

	want := []string{
		"storage.data_dir", "storage.database",
		"tools.ffmpeg", "tools.v4l2_ctl", "tools.tesseract",
		"ocr.controlled_image", "capture.devices", "audio.sinks", "audio.selected",
		"audio.hdmi_bridge",
		"bluetooth.audio", "updates.check",
		"providers.agent_reasoning", "providers.fast_reasoning",
		"providers.vision_ocr_fallback", "providers.decision",
		"providers.voice_design", "providers.voice_clone", "providers.speech_asr",
	}
	if got := ids(rep); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("check ids =\n%v\nwant\n%v", got, want)
	}
	for _, c := range rep.Checks {
		if c.Group == "" || c.Summary == "" {
			t.Errorf("%s: group=%q summary=%q — both are mandatory", c.ID, c.Group, c.Summary)
		}
		switch c.Status {
		case StatusOK, StatusDegraded, StatusBlocked, StatusNotConfigured, StatusFail:
		default:
			t.Errorf("%s: status %q is outside the docs/spec/13 vocabulary", c.ID, c.Status)
		}
	}
	if rep.Totals.Checks != len(want) {
		t.Fatalf("totals.checks = %d, want %d", rep.Totals.Checks, len(want))
	}
	if rep.Status != StatusOK {
		t.Fatalf("overall status = %q on a fully working host, want OK:\n%+v", rep.Status, rep.Checks)
	}
	if !rep.GeneratedAt.Equal(time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("generated_at = %s, want the injected clock", rep.GeneratedAt)
	}
	for _, c := range rep.Checks {
		if c.Duration <= 0 {
			t.Errorf("%s: duration = %s, every probe must be timed", c.ID, c.Duration)
		}
	}
}

func TestGroupsAreDerivedFromIDs(t *testing.T) {
	h := newFakeHost(t)
	want := map[string]string{
		"storage.data_dir": "Storage", "tools.ffmpeg": "Tools",
		"ocr.controlled_image": "OCR", "capture.devices": "Capture",
		"audio.sinks": "Audio", "audio.selected": "Audio", "audio.hdmi_bridge": "Audio",
		"providers.decision": "Providers",
	}
	rep := h.run()
	for id, group := range want {
		if got := h.check(rep, id).Group; got != group {
			t.Errorf("%s group = %q, want %q", id, got, group)
		}
	}
}

func TestRunAddsDevChecksOnlyWithDev(t *testing.T) {
	h := newFakeHost(t)
	h.opts.Dev = true
	rep := h.run()
	mustStatus(t, h.check(rep, "emulator.dolphin"), StatusOK)
	mustStatus(t, h.check(rep, "game.image"), StatusNotConfigured)
	if got := h.check(rep, "emulator.dolphin").Summary; got != "present (rendering proven by scripts/spikes/dolphin)" {
		t.Fatalf("emulator.dolphin summary = %q, want the documented wording", got)
	}

	// On a production target those rows describe a developer box and would be
	// misleading, so they only exist with --dev.
	plain := newFakeHost(t).run()
	for _, c := range plain.Checks {
		if c.ID == "emulator.dolphin" || c.ID == "game.image" {
			t.Fatalf("%s appeared without --dev", c.ID)
		}
	}
}

func TestMissingEmulatorIsBlocked(t *testing.T) {
	h := newFakeHost(t)
	h.opts.Dev = true
	h.withoutTools("Xvfb")
	mustStatus(t, h.check(h.run(), "emulator.dolphin"), StatusBlocked)
}

// TestGameImageNeverPrintsThePath guards AGENTS.md rule 5: the image is the
// user's private file and only its size is reportable.
func TestGameImageNeverPrintsThePath(t *testing.T) {
	root := filepath.Join(t.TempDir(), "secret-dir")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	image := filepath.Join(root, "private-image.ciso")
	if err := os.WriteFile(image, []byte(strings.Repeat("x", 4096)), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newFakeHost(t)
	h.opts.Dev = true
	h.opts.FixtureRoot = func() string { return t.TempDir() }
	h.withEnv(EnvGameImagePath, image)
	rep := h.run()

	c := h.check(rep, "game.image")
	mustStatus(t, c, StatusOK)
	if !strings.Contains(c.Summary, "set, file exists, 4096 bytes") {
		t.Fatalf("summary = %q, want the documented form", c.Summary)
	}
	var out strings.Builder
	if err := Format(&out, rep); err != nil {
		t.Fatal(err)
	}
	if err := FormatJSON(&out, rep); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), image) || strings.Contains(out.String(), "secret-dir") {
		t.Fatalf("report leaked the game image path:\n%s", out.String())
	}
}

func TestGameImageSetButMissingIsNotConfigured(t *testing.T) {
	h := newFakeHost(t)
	h.opts.Dev = true
	h.opts.FixtureRoot = func() string { return t.TempDir() }
	h.withEnv(EnvGameImagePath, filepath.Join(t.TempDir(), "gone.iso"))
	c := h.check(h.run(), "game.image")
	// A set-but-wrong path means the image is still not configured: the
	// report is NOT CONFIGURED with the actionable steps (acceptance gap 8),
	// not a cryptic FAIL.
	mustStatus(t, c, StatusNotConfigured)
	if strings.Contains(c.Summary+strings.ToLower(c.Detail)+strings.ToLower(c.Remedy), "gone.iso") {
		t.Fatalf("failure text leaked the file name: %+v", c)
	}
	if !strings.Contains(c.Remedy, EnvGameImagePath) {
		t.Fatalf("remedy must name the env var: %+v", c)
	}
}

// --- engine safety ---------------------------------------------------------

func TestPanickingCheckBecomesFailAndNeverCrashes(t *testing.T) {
	r := newRunner(newFakeHost(t).options())
	r.add(context.Background(), "storage.database", time.Second, func(context.Context) Check {
		panic("boom")
	})
	if len(r.checks) != 1 {
		t.Fatalf("got %d checks, want 1", len(r.checks))
	}
	c := r.checks[0]
	if c.Status != StatusFail || !strings.Contains(c.Summary, "boom") {
		t.Fatalf("panic produced %+v, want FAIL naming the panic", c)
	}
	if c.ID != "storage.database" || c.Group != "Storage" {
		t.Fatalf("check lost its identity: %+v", c)
	}
}

func TestCheckThatIgnoresItsDeadlineIsAbandoned(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	r := newRunner(newFakeHost(t).options())
	r.add(context.Background(), "storage.database", 20*time.Millisecond, func(context.Context) Check {
		<-release // deliberately ignores ctx
		return Check{Status: StatusOK, Summary: "too late"}
	})
	c := r.checks[0]
	if c.Status != StatusFail || !strings.Contains(c.Summary, "did not finish") {
		t.Fatalf("ignored deadline produced %+v, want FAIL", c)
	}
	if c.ID != "storage.database" || c.Group != "Storage" {
		t.Fatalf("abandoned check lost its identity: %+v", c)
	}
}

func TestCheckReturningNoStatusIsFail(t *testing.T) {
	r := newRunner(newFakeHost(t).options())
	r.add(context.Background(), "tools.ffmpeg", time.Second, func(context.Context) Check {
		return Check{}
	})
	if c := r.checks[0]; c.Status != StatusFail {
		t.Fatalf("empty check produced %q, want FAIL — an empty row is an unprobed row", c.Status)
	}
}

func TestOverallStatusIsTheWorstStateSeen(t *testing.T) {
	checks := func(statuses ...string) Report {
		out := make([]Check, 0, len(statuses))
		for _, s := range statuses {
			out = append(out, Check{Status: s, Summary: "x"})
		}
		rep := Report{Checks: out}
		rep.Totals = countTotals(out)
		rep.Status = overallStatus(out)
		return rep
	}
	cases := []struct {
		want  string
		input []string
	}{
		{StatusOK, []string{StatusOK, StatusNotConfigured}},
		{StatusDegraded, []string{StatusOK, StatusDegraded, StatusNotConfigured}},
		{StatusBlocked, []string{StatusBlocked, StatusDegraded}},
		{StatusFail, []string{StatusFail, StatusBlocked, StatusDegraded}},
	}
	for _, tc := range cases {
		got := checks(tc.input...)
		if got.Status != tc.want {
			t.Errorf("overall of %v = %q, want %q", tc.input, got.Status, tc.want)
		}
	}
	if !checks(StatusNotConfigured, StatusFail).HasFail() {
		t.Fatal("HasFail missed a FAIL — the exit code contract depends on it")
	}
	if checks(StatusNotConfigured, StatusBlocked).HasFail() {
		t.Fatal("HasFail fired without a FAIL — a missing component must not exit 1")
	}
}

// --- storage ---------------------------------------------------------------

func TestDataDirIsCreatedWithPrivateMode(t *testing.T) {
	h := newFakeHost(t)
	h.opts.DataDir = filepath.Join(t.TempDir(), "narrator")
	mustStatus(t, h.check(h.run(), "storage.data_dir"), StatusOK)

	info, err := os.Stat(h.opts.DataDir)
	if err != nil {
		t.Fatalf("doctor did not create the directory: %v", err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("created mode = 0%o, want 0700", info.Mode().Perm())
	}
}

func TestDataDirWorldReadableIsDegraded(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	h := newFakeHost(t)
	h.opts.DataDir = dir
	c := h.check(h.run(), "storage.data_dir")
	mustStatus(t, c, StatusDegraded)
	if !strings.Contains(c.Remedy, "chmod 700") {
		t.Fatalf("remedy = %q, want an actionable chmod", c.Remedy)
	}
}

// TestDataDirGroupReadableIsDegraded pins the legacy packaging mode: the
// .deb postinst and Arch hooks once installed the data dir at 0750 (fixed by
// ADR 0063), and a pre-fix install leaves exactly this state on the device.
// The check must call it DEGRADED with the documented remedy — and must NOT
// silently self-heal: doctor reports, the admin (or the package upgrade)
// chmods.
func TestDataDirGroupReadableIsDegraded(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o750); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	h := newFakeHost(t)
	h.opts.DataDir = dir
	c := h.check(h.run(), "storage.data_dir")
	mustStatus(t, c, StatusDegraded)
	if !strings.Contains(c.Summary, "readable by group or other") {
		t.Fatalf("summary = %q, want the group/other readability wording", c.Summary)
	}
	if !strings.Contains(c.Remedy, "chmod 700") {
		t.Fatalf("remedy = %q, want the actionable chmod", c.Remedy)
	}
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0o750 {
		t.Fatalf("doctor rewrote the directory (mode %v, stat err %v); it must report, not self-heal", info.Mode().Perm(), err)
	}
}

func TestDataDirUnderAFileFails(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newFakeHost(t)
	h.opts.DataDir = filepath.Join(blocker, "nested")
	mustStatus(t, h.check(h.run(), "storage.data_dir"), StatusFail)
}

func TestEmptyDataDirIsFailWithRemedy(t *testing.T) {
	h := newFakeHost(t)
	h.opts.DataDir = ""
	c := h.check(h.run(), "storage.data_dir")
	mustStatus(t, c, StatusFail)
	if !strings.Contains(c.Remedy, bootstrap.EnvDataDir) {
		t.Fatalf("remedy = %q, want it to name %s", c.Remedy, bootstrap.EnvDataDir)
	}
}

func TestDatabaseCheckAppliesMigrationsOnAFreshDir(t *testing.T) {
	h := newFakeHost(t)
	c := h.check(h.run(), "storage.database")
	mustStatus(t, c, StatusOK)
	// The version is whatever this build's migrations end at, not a number typed
	// into the test: a hardcoded 3 here would make the next migration file report
	// a perfectly healthy database as a doctor failure.
	if want := fmt.Sprintf("schema version %d", store.LatestSchemaVersion()); !strings.Contains(c.Summary, want) {
		t.Fatalf("summary = %q, want it to state %q", c.Summary, want)
	}
	if !strings.Contains(c.Detail, "applied "+store.MigrationNames()[0]) {
		t.Fatalf("detail = %q, want the migration it applied", c.Detail)
	}
	if _, err := os.Stat(filepath.Join(h.dataDir(), "game-narrator.sqlite")); err != nil {
		t.Fatalf("the database file was not created: %v", err)
	}
}

func TestDatabaseCheckIsFailWhenTheStoreCannotOpen(t *testing.T) {
	h := newFakeHost(t)
	// A directory where the database file belongs cannot be opened by SQLite.
	if err := os.Mkdir(filepath.Join(h.dataDir(), "game-narrator.sqlite"), 0o700); err != nil {
		t.Fatal(err)
	}
	c := h.check(h.run(), "storage.database")
	mustStatus(t, c, StatusFail)
	if !strings.Contains(c.Summary, "cannot open") {
		t.Fatalf("summary = %q, want the failure named", c.Summary)
	}
}

// --- tools -----------------------------------------------------------------

func TestMissingToolIsBlockedWithBootstrapRemedy(t *testing.T) {
	h := newFakeHost(t)
	h.withoutTools("ffmpeg")
	c := h.check(h.run(), "tools.ffmpeg")
	mustStatus(t, c, StatusBlocked)
	if !strings.Contains(c.Summary, "ffmpeg is not installed") {
		t.Fatalf("summary = %q", c.Summary)
	}
	if !strings.Contains(c.Remedy, "scripts/bootstrap-arch.sh") || !strings.Contains(c.Remedy, "pacman -S ffmpeg") {
		t.Fatalf("remedy = %q, want the documented bootstrap step", c.Remedy)
	}
}

func TestToolThatCannotRunIsFail(t *testing.T) {
	h := newFakeHost(t)
	h.execFail("ffmpeg", "exit status 1")
	c := h.check(h.run(), "tools.ffmpeg")
	mustStatus(t, c, StatusFail) // configured (on PATH) and broken
	if !strings.Contains(c.Summary, "did not run") {
		t.Fatalf("summary = %q", c.Summary)
	}
}

func TestToolWithoutVersionOutputIsFail(t *testing.T) {
	h := newFakeHost(t)
	h.exec("ffmpeg", "   \n\n")
	mustStatus(t, h.check(h.run(), "tools.ffmpeg"), StatusFail)
}

func TestToolVersionIsTheFirstLine(t *testing.T) {
	h := newFakeHost(t)
	rep := h.run()
	if got := h.check(rep, "tools.ffmpeg").Summary; got != "ffmpeg version n9.0.2 test banner" {
		t.Fatalf("ffmpeg summary = %q, want the first line only", got)
	}
	if got := h.check(rep, "tools.v4l2_ctl").Summary; got != "v4l2-ctl 1.32.0" {
		t.Fatalf("v4l2_ctl summary = %q", got)
	}
	if !h.calledWith("ffmpeg -version") || !h.calledWith("v4l2-ctl --version") || !h.calledWith("tesseract --version") {
		t.Fatalf("version probes not run: %v", h.invoked())
	}
}

// --- OCR -------------------------------------------------------------------

func TestOCRCheckRequiresTheControlledText(t *testing.T) {
	cases := []struct {
		name   string
		result CommandResult
		want   string
	}{
		{"correct read", CommandResult{Stdout: []byte("\nNARRATOR  42\n\n")}, StatusOK},
		{"wrong read", CommandResult{Stdout: []byte("NRARRATOR 4Z")}, StatusFail},
		{"empty read", CommandResult{Stdout: []byte("   \n")}, StatusFail},
		{"engine error", CommandResult{Err: errors.New("exit status 1"), ExitCode: 1}, StatusFail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newFakeHost(t)
			h.execResult("tesseract", tc.result)
			mustStatus(t, h.check(h.run(), "ocr.controlled_image"), tc.want)
		})
	}
}

func TestOCRCheckIsBlockedWithoutTesseract(t *testing.T) {
	h := newFakeHost(t)
	h.withoutTools("tesseract")
	c := h.check(h.run(), "ocr.controlled_image")
	mustStatus(t, c, StatusBlocked)
	if !strings.Contains(c.Remedy, "bootstrap-arch.sh") {
		t.Fatalf("remedy = %q", c.Remedy)
	}
	if h.calledWith("tesseract ") {
		t.Fatal("tesseract was invoked although LookPath said it is missing")
	}
}

// TestControlledImageIsReadableByRealTesseract proves the committed PNG really
// carries the text — the synthetic fixture is only useful if a real engine can
// read it.
func TestControlledImageIsReadableByRealTesseract(t *testing.T) {
	if _, err := exec.LookPath("tesseract"); err != nil {
		t.Skipf("NOT RUN: real tesseract is not installed on this host (%v)", err)
	}
	h := newFakeHost(t)
	// Real LookPath and real exec: this test must run the actual binary.
	h.opts.Exec = nil
	h.opts.LookPath = nil
	h.opts.Timeout = 30 * time.Second
	rep := Run(context.Background(), h.options())
	c := h.check(rep, "ocr.controlled_image")
	mustStatus(t, c, StatusOK)
	if !strings.Contains(c.Summary, controlledText) {
		t.Fatalf("summary = %q, want the recovered text", c.Summary)
	}
}

// --- capture ---------------------------------------------------------------

// v4l2Info reads one of the recorded v4l2-ctl --info fixtures (the venus
// file is the verbatim appliance output, v4l-utils 1.30.1).
func v4l2Info(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return string(b)
}

// v4l2Formats reads a --list-formats-ext fixture (synthetic, labelled as
// such in the file).
func v4l2Formats(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return string(b)
}

// fakeSysfs fabricates the minimal sysfs tree the capture check's USB link
// read walks (internal/capture/usb): class/video4linux/video{0,1}/device
// symlinks into one USB device directory carrying speed and maxportspeed.
func fakeSysfs(t *testing.T, speed, maxport string) string {
	t.Helper()
	root := t.TempDir()
	usbDir := filepath.Join(root, "devices", "platform", "soc", "usb", "usb3", "3-1")
	if err := os.MkdirAll(usbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]string{
		"speed":        speed,
		"busnum":       "3",
		"maxportspeed": maxport,
		"idVendor":     "1bcf",
		"idProduct":    "2d3d",
	} {
		if err := os.WriteFile(filepath.Join(usbDir, name), []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, node := range []string{"video0", "video1"} {
		classDir := filepath.Join(root, "class", "video4linux", node)
		if err := os.MkdirAll(classDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(usbDir, filepath.Join(classDir, "device")); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestCaptureWithoutDevicesIsNotConfigured(t *testing.T) {
	h := newFakeHost(t)
	c := h.check(h.run(), "capture.devices")
	mustStatus(t, c, StatusNotConfigured)
	if c.Summary != "no UVC capture device attached" {
		t.Fatalf("summary = %q, want the documented wording", c.Summary)
	}
}

func TestCaptureWithDevicesAndV4L2IsOK(t *testing.T) {
	h := newFakeHost(t)
	h.withDevices("/dev/video0", "/dev/video1")
	h.exec("v4l2-ctl -d /dev/video0 --info", v4l2Info(t, "v4l2-uvc-info.txt"))
	h.exec("v4l2-ctl -d /dev/video1 --info", v4l2Info(t, "v4l2-uvc-info.txt"))
	c := h.check(h.run(), "capture.devices")
	mustStatus(t, c, StatusOK)
	if !strings.Contains(c.Summary, "2 capture node(s)") {
		t.Fatalf("summary = %q", c.Summary)
	}
	if !h.calledWith("v4l2-ctl -d /dev/video0 --info") || !h.calledWith("v4l2-ctl -d /dev/video1 --info") {
		t.Fatalf("per-node --info was never run: %v", h.invoked())
	}
	// Mode selection (docs/spec/10 preflight): the SuperSpeed card gets the
	// policy's pick with its reason and the rejected candidate count.
	if !strings.Contains(c.Detail, "mode selection") || !strings.Contains(c.Detail, "640x480 YUYV @ 30 fps") {
		t.Fatalf("detail = %q, want the selected mode with the policy's pick", c.Detail)
	}
	if !strings.Contains(c.Detail, "super-speed") {
		t.Fatalf("detail = %q, want the negotiated link class", c.Detail)
	}
}

// TestCaptureIgnoresTheVenusCodecNodesIs the first-device-run bug: the UNO Q
// exposes the Qualcomm venus decoder as /dev/video0 and /dev/video1, and the
// old check counted them as "2 capture node(s)". They are memory-to-memory
// nodes (VIDIOC_QUERYCAP has no VIDEO_CAPTURE), so the check must report no
// capture device and name the driver of the ignored nodes.
func TestCaptureIgnoresTheVenusCodecNodes(t *testing.T) {
	h := newFakeHost(t)
	h.withDevices("/dev/video0", "/dev/video1")
	venus := v4l2Info(t, "v4l2-venus-info.txt")
	h.exec("v4l2-ctl -d /dev/video0 --info", venus)
	h.exec("v4l2-ctl -d /dev/video1 --info", venus)
	c := h.check(h.run(), "capture.devices")
	mustStatus(t, c, StatusNotConfigured)
	if c.Summary != "no UVC capture device attached" {
		t.Fatalf("summary = %q, want the no-capture-device wording", c.Summary)
	}
	for _, node := range []string{"video0", "video1"} {
		if !strings.Contains(c.Detail, node) || !strings.Contains(c.Detail, "Qualcomm Venus video decoder") {
			t.Fatalf("detail = %q, want %s listed as ignored with its driver name", c.Detail, node)
		}
	}
	if !strings.Contains(c.Remedy, "capture card") {
		t.Fatalf("remedy = %q, want the attach instruction", c.Remedy)
	}
}

// TestCaptureMentionsIgnoredNodesAlongsideCapableOnes: a real UVC card and
// a venus codec node side by side — the card is the one node counted, and
// the codec node is recorded as ignored with its driver so the operator can
// see why /dev/video1 is not the capture card.
func TestCaptureMentionsIgnoredNodesAlongsideCapableOnes(t *testing.T) {
	h := newFakeHost(t)
	h.withDevices("/dev/video0", "/dev/video1")
	h.exec("v4l2-ctl -d /dev/video0 --info", v4l2Info(t, "v4l2-uvc-info.txt"))
	h.exec("v4l2-ctl -d /dev/video1 --info", v4l2Info(t, "v4l2-venus-info.txt"))
	c := h.check(h.run(), "capture.devices")
	mustStatus(t, c, StatusOK)
	if c.Summary != "1 capture node(s): video0" {
		t.Fatalf("summary = %q, want only the capture-capable node counted", c.Summary)
	}
	if !strings.Contains(c.Detail, "video1") || !strings.Contains(c.Detail, "Qualcomm Venus video decoder") {
		t.Fatalf("detail = %q, want the ignored node with its driver", c.Detail)
	}
}

// TestCaptureKeepsAnUnreadableNodeInCount: a node whose --info query fails
// (permission, a wedged node) cannot be proven non-capture, so it stays in
// the count and the failure is recorded — hiding a real capture card behind
// a read error is worse than an extra node in the list.
func TestCaptureKeepsAnUnreadableNodeInCount(t *testing.T) {
	h := newFakeHost(t)
	h.withDevices("/dev/video0")
	h.execFail("v4l2-ctl -d /dev/video0 --info", "unable to open device: Permission denied")
	c := h.check(h.run(), "capture.devices")
	mustStatus(t, c, StatusOK)
	if !strings.Contains(c.Summary, "1 capture node(s): video0") {
		t.Fatalf("summary = %q, want the unreadable node kept in the count", c.Summary)
	}
	if !strings.Contains(c.Detail, "unreadable") {
		t.Fatalf("detail = %q, want the read failure recorded", c.Detail)
	}
}

func TestCaptureWithoutV4L2IsDegraded(t *testing.T) {
	h := newFakeHost(t)
	h.withDevices("/dev/video0")
	h.withoutTools("v4l2-ctl")
	c := h.check(h.run(), "capture.devices")
	mustStatus(t, c, StatusDegraded)
	if h.calledWith("v4l2-ctl ") {
		t.Fatal("v4l2-ctl was invoked although it is not on PATH")
	}
}

func TestCaptureDeviceDeniedToThisUserIsDegraded(t *testing.T) {
	h := newFakeHost(t)
	h.withDevices("/dev/video0")
	h.denyDevices()
	c := h.check(h.run(), "capture.devices")
	mustStatus(t, c, StatusDegraded)
	if !strings.Contains(c.Remedy, "video group") {
		t.Fatalf("remedy = %q, want the group-membership fix", c.Remedy)
	}
}

// TestCaptureDowngradedUSB3CardAtUSB2IsDegraded is the silent performance
// fault the link read exists to name (docs/spec/10): a SuperSpeed-class card
// negotiating HighSpeed works, but its raw modes do not fit the link, and
// the doctor must say so with the operator action, not after the first
// flapping session.
func TestCaptureDowngradedUSB3CardAtUSB2IsDegraded(t *testing.T) {
	h := newFakeHost(t)
	h.withDevices("/dev/video0")
	h.exec("v4l2-ctl -d /dev/video0 --info", v4l2Info(t, "v4l2-uvc-info.txt"))
	// Same card, USB 2 port: speed 480, port ceiling SuperSpeed.
	h.opts.SysfsRoot = fakeSysfs(t, "480", "5000")
	c := h.check(h.run(), "capture.devices")
	mustStatus(t, c, StatusDegraded)
	if !strings.Contains(c.Detail, "negotiating USB 2") {
		t.Fatalf("detail = %q, want the downgrade warning", c.Detail)
	}
	if !strings.Contains(c.Remedy, "USB 3") {
		t.Fatalf("remedy = %q, want the re-seat instruction", c.Remedy)
	}
	// The mode itself still fits 480p raw even at HighSpeed: the card is not
	// dead, it is capped — the selection must still report.
	if !strings.Contains(c.Detail, "640x480 YUYV @ 30 fps") {
		t.Fatalf("detail = %q, want the capped mode still selected", c.Detail)
	}
}

// TestCaptureUnreadableFormatsIsDegraded: a capable node whose format
// enumeration fails (wedged node, missing v4l2-ctl on a live card) must not
// be reported as mode-ready.
func TestCaptureUnreadableFormatsIsDegraded(t *testing.T) {
	h := newFakeHost(t)
	h.withDevices("/dev/video0")
	h.exec("v4l2-ctl -d /dev/video0 --info", v4l2Info(t, "v4l2-uvc-info.txt"))
	h.execFail("v4l2-ctl -d /dev/video0 --list-formats-ext", "unable to enumerate formats")
	c := h.check(h.run(), "capture.devices")
	mustStatus(t, c, StatusDegraded)
	if !strings.Contains(c.Remedy, "formats unreadable") {
		t.Fatalf("remedy = %q, want the fault named", c.Remedy)
	}
}

// TestCaptureUnreadableLinkStillSelectsConservatively: sysfs without the card
// (a non-USB-attached node, or a stripped tree) cannot prove the link class,
// so the policy falls back to the HighSpeed budget — a conservative mode,
// never an error and never an invented claim.
func TestCaptureUnreadableLinkStillSelectsConservatively(t *testing.T) {
	h := newFakeHost(t)
	h.withDevices("/dev/video0")
	h.exec("v4l2-ctl -d /dev/video0 --info", v4l2Info(t, "v4l2-uvc-info.txt"))
	h.opts.SysfsRoot = t.TempDir() // no class entry: the link is unreadable
	c := h.check(h.run(), "capture.devices")
	mustStatus(t, c, StatusOK)
	if !strings.Contains(c.Detail, "link unreadable") {
		t.Fatalf("detail = %q, want the unreadable link named", c.Detail)
	}
	if !strings.Contains(c.Detail, "640x480 YUYV @ 30 fps") {
		t.Fatalf("detail = %q, want the conservative HighSpeed pick", c.Detail)
	}
}

// TestParseV4L2Capabilities pins the flag parsing against the recorded
// fixtures: the venus node is M2M-only, the UVC card is plain capture, and
// the identity lines resolve to the card name.
func TestParseV4L2Capabilities(t *testing.T) {
	cases := []struct {
		name           string
		fixture        string
		wantCapture    bool
		wantM2M        bool
		wantDriverPart string
	}{
		{"venus codec node (device recording)", "v4l2-venus-info.txt", false, true, "Qualcomm Venus video decoder"},
		{"uvc capture card", "v4l2-uvc-info.txt", true, false, "HD Webcam"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			capture, m2m, driver := parseV4L2Capabilities([]byte(v4l2Info(t, tc.fixture)))
			if capture != tc.wantCapture || m2m != tc.wantM2M {
				t.Fatalf("capture=%v m2m=%v, want %v/%v", capture, m2m, tc.wantCapture, tc.wantM2M)
			}
			if !strings.Contains(driver, tc.wantDriverPart) {
				t.Fatalf("driver = %q, want the card identity %q", driver, tc.wantDriverPart)
			}
		})
	}

	// A node advertising both a plain capture flag and an M2M flag is not a
	// clean capture source (the finding: VIDEO_CAPTURE and not M2M).
	hybrid := "Capabilities     : 0x84204001\n\tVideo Capture\n\tVideo Memory-to-Memory Multiplanar\n\tStreaming\n"
	if capture, m2m, _ := parseV4L2Capabilities([]byte(hybrid)); !capture || !m2m {
		t.Fatalf("hybrid node: capture=%v m2m=%v, want both flags seen", capture, m2m)
	}
	// No capability list at all: nothing is provable, the caller keeps the
	// node and records the failure.
	if capture, m2m, driver := parseV4L2Capabilities([]byte("")); capture || m2m || driver != "" {
		t.Fatalf("empty output parsed as %v/%v/%q, want all empty", capture, m2m, driver)
	}
}

// --- audio -----------------------------------------------------------------

func TestAudioSinksViaPulseAreOK(t *testing.T) {
	h := newFakeHost(t)
	c := h.check(h.run(), "audio.sinks")
	mustStatus(t, c, StatusOK)
	if !strings.HasPrefix(c.Summary, "2 audio output sink(s) via pactl") {
		t.Fatalf("summary = %q", c.Summary)
	}
}

func TestAudioNoSinksIsDegraded(t *testing.T) {
	h := newFakeHost(t)
	h.exec("pactl", "\n")
	c := h.check(h.run(), "audio.sinks")
	mustStatus(t, c, StatusDegraded)
	if c.Summary != "no audio output sink found" {
		t.Fatalf("summary = %q, want the documented wording", c.Summary)
	}
}

func TestAudioFallsBackToALSAWhenPulseIsNotRunning(t *testing.T) {
	h := newFakeHost(t)
	h.execFail("pactl", "Connection refused")
	c := h.check(h.run(), "audio.sinks")
	mustStatus(t, c, StatusOK)
	if !strings.Contains(c.Summary, "via aplay") {
		t.Fatalf("summary = %q, want the ALSA fallback to answer", c.Summary)
	}
	if !strings.Contains(c.Detail, "earlier attempt failed") {
		t.Fatalf("detail = %q, want the pactl failure recorded", c.Detail)
	}
}

func TestAudioWithoutAnyBackendIsBlocked(t *testing.T) {
	h := newFakeHost(t)
	h.withoutTools("pactl", "aplay")
	mustStatus(t, h.check(h.run(), "audio.sinks"), StatusBlocked)
}

// TestAudioNeverPlaysAnything guards the consent rule in docs/spec/13: the
// probe lists sinks and must not make noise.
func TestAudioNeverPlaysAnything(t *testing.T) {
	h := newFakeHost(t)
	h.withDevices("/dev/video0")
	h.run()
	for _, call := range h.invoked() {
		if strings.Contains(call, "play") && !strings.HasPrefix(call, "aplay -l") {
			t.Fatalf("doctor played audio: %q", call)
		}
	}
}

// --- audio.selected (ADR-0074, gap audit G5) -------------------------------

// TestAudioSelectedPlatformDefaultIsOK pins the default: no env override and
// no stored row, the selection is the platform route, the platform-default
// device (the null sink in the fixture) is found, and the check is OK.
func TestAudioSelectedPlatformDefaultIsOK(t *testing.T) {
	h := newFakeHost(t)
	c := h.check(h.run(), "audio.selected")
	mustStatus(t, c, StatusOK)
	if !strings.Contains(c.Summary, "auto_null") {
		t.Fatalf("summary = %q, want the platform-default device named", c.Summary)
	}
}

// TestAudioSelectedEnvDeviceExistsIsOK: an env override naming an enumerated
// device (the USB DAC) is the user's pick and is sound.
func TestAudioSelectedEnvDeviceExistsIsOK(t *testing.T) {
	h := newFakeHost(t)
	h.withEnv(audio.EnvDevice, "alsa_output.usb-JBL_Flip_6_0011223344")
	c := h.check(h.run(), "audio.selected")
	mustStatus(t, c, StatusOK)
	if !strings.Contains(c.Summary, "JBL_Flip_6") {
		t.Fatalf("summary = %q, want the selected DAC named", c.Summary)
	}
}

// TestAudioSelectedMissingDeviceIsDegraded: a selection naming a device the
// enumeration does not show (pulled the DAC, kept the setting) is DEGRADED
// with the enumerated ids so the operator can see what to pick instead.
func TestAudioSelectedMissingDeviceIsDegraded(t *testing.T) {
	h := newFakeHost(t)
	h.withEnv(audio.EnvDevice, "alsa_output.usb-Pulled_DAC_999")
	c := h.check(h.run(), "audio.selected")
	mustStatus(t, c, StatusDegraded)
	if !strings.Contains(c.Summary, "Pulled_DAC_999") {
		t.Fatalf("summary = %q, want the missing device named", c.Summary)
	}
	if !strings.Contains(c.Detail, "auto_null") || !strings.Contains(c.Remedy, "Audio output") {
		t.Fatalf("detail/remedy must name the enumerated ids and the settings page: %q / %q", c.Detail, c.Remedy)
	}
}

// TestAudioSelectedUACDongleIsRefused is the G5 guard: the selection naming
// the capture dongle's UAC (USB audio-class) interface is DEGRADED — that
// interface is not a speaker — and the remedy points at the DAC. The UVC
// node present on the fixture host sharpens the detail (the dongle is
// attached, so its UAC interface is exactly where a UAC-named sink comes
// from).
func TestAudioSelectedUACDongleIsRefused(t *testing.T) {
	h := newFakeHost(t)
	h.withDevices("/dev/video0")
	h.withEnv(audio.EnvDevice, "alsa_output.usb-General_UAC_Device_001")
	c := h.check(h.run(), "audio.selected")
	mustStatus(t, c, StatusDegraded)
	if !strings.Contains(c.Summary, "UAC") {
		t.Fatalf("summary = %q, want the UAC finding named", c.Summary)
	}
	if !strings.Contains(c.Detail, "video0") {
		t.Fatalf("detail = %q, want the attached UVC node named", c.Detail)
	}
	if !strings.Contains(c.Remedy, "USB DAC") {
		t.Fatalf("remedy = %q, want the DAC pointed at", c.Remedy)
	}
}

// TestAudioSelectedStoredDeviceIsRead: the stored row (the Settings page's
// persisted pick) is read from the real store in the data directory and
// resolves — proof the seam-to-store path, not just env, feeds the check.
func TestAudioSelectedStoredDeviceIsRead(t *testing.T) {
	h := newFakeHost(t)
	ctx := context.Background()
	st, err := store.Open(ctx, store.DBPath(h.dataDir()))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate store: %v", err)
	}
	if err := st.SetAudioSettings(ctx, store.AudioSettings{Device: "alsa_output.usb-JBL_Flip_6_0011223344", SinkKind: "pipewire", MasterVolume: 0.8}); err != nil {
		t.Fatalf("set audio settings: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	c := h.check(h.run(), "audio.selected")
	mustStatus(t, c, StatusOK)
	if !strings.Contains(c.Detail, "(store)") {
		t.Fatalf("detail = %q, want the stored provenance named", c.Detail)
	}
}

// TestAudioSelectedUnresolvableSelectionFails: a stored sink kind this build
// does not accept cannot resolve; the check is FAIL with the selection fix
// named, not a silent default.
func TestAudioSelectedUnresolvableSelectionFails(t *testing.T) {
	h := newFakeHost(t)
	h.withEnv(audio.EnvSink, "jack")
	c := h.check(h.run(), "audio.selected")
	mustStatus(t, c, StatusFail)
	if !strings.Contains(c.Summary, "does not resolve") {
		t.Fatalf("summary = %q", c.Summary)
	}
}
