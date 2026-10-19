// Package doctor implements `game-narrator doctor` — the preflight that has to
// say what is actually there (docs/spec/13 "Diagnostics page and CLI",
// docs/spec/10 "Hardware discovery and preflight").
//
// The governing rule from docs/spec/13 is that a service which has not been
// probed is never reported as working. Every check therefore probes something
// real and answers in one of five states:
//
//	OK             the probe succeeded
//	DEGRADED       present but not fully usable (wider permissions, no sink,
//	               a configured role whose client does not exist yet)
//	BLOCKED        required component is missing; run the bootstrap steps
//	NOT CONFIGURED nothing is configured for this component — not a fault
//	FAIL           a configured component is broken
//
// Nothing here may print a credential, a gateway URL or a game image path:
// reports describe roles, model ids, sizes and statuses (AGENTS.md rule 5).
package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/dbirks/game-narrator/internal/dolphin"
	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/openai"
)

// Report states from docs/spec/13, plus FAIL for a configured component that
// is actually broken — the four spec states cannot distinguish "not set up
// yet" from "set up and broken".
const (
	StatusOK            = "OK"
	StatusDegraded      = "DEGRADED"
	StatusBlocked       = "BLOCKED"
	StatusNotConfigured = "NOT CONFIGURED"
	StatusFail          = "FAIL"
)

// DefaultCheckTimeout bounds one probe; model calls get longer because a cold
// gateway turn legitimately takes seconds (docs/spec/06 "Timeout and
// cancellation defaults" applies to the runtime loop, not to diagnostics).
const (
	DefaultCheckTimeout = 15 * time.Second
	DefaultModelTimeout = 60 * time.Second
)

// checkGrace is how long a check may take to notice its own deadline before
// the engine gives up on it and reports a FAIL. A probe that ignores its
// context must not hang the report.
const checkGrace = 5 * time.Second

// EnvGameImagePath names the user-supplied game image. The value is a private
// path and is never printed; only whether it is set, exists and its size.
const EnvGameImagePath = "GAME_NARRATOR_GAME_IMAGE_PATH"

// Check is the result of one probe.
type Check struct {
	// ID is the stable machine name, e.g. "storage.database" or
	// "providers.fast_reasoning".
	ID string `json:"id"`
	// Group is the human grouping used to lay out the table: Storage, Tools,
	// OCR, Capture, Audio, Providers, Emulator, Game.
	Group   string `json:"group"`
	Status  string `json:"status"`
	Summary string `json:"summary"`
	// Detail carries the evidence: a version string, a mode, an error text
	// that has been checked for credentials.
	Detail string `json:"detail,omitempty"`
	// Remedy is the concrete next action for anything that is not OK.
	Remedy   string        `json:"remedy,omitempty"`
	Duration time.Duration `json:"duration_ns"`
}

// Totals counts a report by state, so "3 missing, 1 broken" is visible without
// reading the table.
type Totals struct {
	OK            int `json:"ok"`
	Degraded      int `json:"degraded"`
	Blocked       int `json:"blocked"`
	NotConfigured int `json:"not_configured"`
	Fail          int `json:"fail"`
	Checks        int `json:"checks"`
}

// Report is a complete diagnostic run.
type Report struct {
	GeneratedAt time.Time `json:"generated_at"`
	// Status is the worst state seen: FAIL > BLOCKED > DEGRADED > OK. NOT
	// CONFIGURED never degrades the whole report, because a first-run host
	// legitimately has no gateway configured.
	Status string  `json:"status"`
	Totals Totals  `json:"totals"`
	Checks []Check `json:"checks"`
}

// HasFail reports whether any check is FAIL, which is the doctor exit-code
// contract: a configured component that is broken must be visible to scripts.
func (r Report) HasFail() bool {
	for _, c := range r.Checks {
		if c.Status == StatusFail {
			return true
		}
	}
	return false
}

// CommandResult is what ExecFunc reports for one attempted command. Stdout and
// Stderr are kept apart because tools such as tesseract print banner noise on
// stderr while the real answer goes to stdout.
type CommandResult struct {
	Stdout   []byte
	Stderr   []byte
	Err      error
	ExitCode int
}

// ExecFunc runs a program with the check's context. Tests inject this so the
// suite never depends on which host tools happen to be installed.
type ExecFunc func(ctx context.Context, name string, args ...string) CommandResult

// ProviderProbe is the slice of a model client the doctor needs. Injecting it
// keeps role checks testable without a gateway; the default implementation
// talks to the configured OpenAI-compatible endpoint for real.
type ProviderProbe interface {
	ListModels(ctx context.Context, cfg providers.RoleConfig) ([]string, error)
	Chat(ctx context.Context, cfg providers.RoleConfig, prompt string, images [][]byte) (ChatResult, error)
	// ProbeCapabilities exercises the role's optional capabilities (JSON mode
	// via response_format and tool_choice=required) and returns a snapshot.
	// It is a diagnostic, not a hard requirement: a capability the app can
	// degrade from (docs/spec/06) is reported, and a probe failure must not
	// fail the role.
	ProbeCapabilities(ctx context.Context, cfg providers.RoleConfig) (openai.Capabilities, error)
}

// ChatResult is one probe answer.
type ChatResult struct {
	Text    string
	Model   string
	Latency time.Duration
}

// Options configures a run. Zero values mean "use the real environment and the
// documented defaults", which is what the CLI passes.
type Options struct {
	// DataDir is the durable data directory to probe.
	DataDir string
	// Dev adds developer-host checks (Dolphin, game image) that have no
	// meaning on a production UNO Q install.
	Dev bool

	// Timeout and ModelTimeout bound individual checks. ModelTimeout is the
	// default bound for the model-role checks; a role whose production
	// per-request timeout implies a longer total probe time gets that larger
	// budget instead, capped at maxRoleProbeBudget (roleProbeBudget in
	// providers.go) — so a slow-but-healthy reasoning role over Wi-Fi is not
	// killed by a budget smaller than the role's own production allowance.
	Timeout      time.Duration
	ModelTimeout time.Duration

	// LookupEnv, LookPath, Exec, Glob and DeviceOpen are the host seams.
	LookupEnv  func(name string) (string, bool)
	LookPath   func(name string) (string, error)
	Exec       ExecFunc
	Glob       func(pattern string) ([]string, error)
	DeviceOpen func(path string) error

	// Roles overrides providers.LoadRoles (tests); nil resolves from
	// LookupEnv.
	Roles map[providers.Role]providers.RoleConfig
	// Probe overrides the model client (tests); nil uses the real OpenAI
	// compatible client for the role.
	Probe ProviderProbe
	// DecisionProbe overrides the System One decision probe (tests); nil
	// builds the real systemone client per role.
	DecisionProbe DecisionProbe
	// VoiceProbe overrides the TTS probe for the voice_design and voice_clone
	// roles (tests); nil uses the real tts client and registry.
	VoiceProbe VoiceProbe
	// ASRProbe overrides the transcription probe for the speech_asr role
	// (tests); nil uses the real asr client.
	ASRProbe ASRProbe

	// OCRImage overrides the embedded controlled image.
	OCRImage []byte

	// FixtureRoot resolves the external fixture root the game.image check
	// scans for manifest-recorded image SHA-256 (games never committed,
	// AGENTS.md rule 5); nil uses dolphin.FixtureDir ($GAME_NARRATOR_FIXTURE_DIR,
	// default artifacts/fixtures).
	FixtureRoot func() string

	// Now stamps the report; nil means time.Now.
	Now func() time.Time
}

// fixtureRoot applies the FixtureRoot seam (tests point it at a temp tree so
// the suite never depends on the host's artifacts/ contents).
func (o *Options) fixtureRoot() string {
	if o.FixtureRoot != nil {
		return o.FixtureRoot()
	}
	return dolphin.FixtureDir()
}

func (o *Options) withDefaults() Options {
	d := *o
	if d.Timeout <= 0 {
		d.Timeout = DefaultCheckTimeout
	}
	if d.ModelTimeout <= 0 {
		d.ModelTimeout = DefaultModelTimeout
	}
	if d.LookupEnv == nil {
		d.LookupEnv = os.LookupEnv
	}
	if d.LookPath == nil {
		d.LookPath = exec.LookPath
	}
	if d.Exec == nil {
		d.Exec = defaultExec
	}
	if d.Glob == nil {
		d.Glob = filepath.Glob
	}
	if d.DeviceOpen == nil {
		d.DeviceOpen = defaultDeviceOpen
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	return d
}

// defaultExec runs a command and captures its streams.
func defaultExec(ctx context.Context, name string, args ...string) CommandResult {
	cmd := exec.CommandContext(ctx, name, args...)
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		code = -1
	}
	return CommandResult{Stdout: []byte(out.String()), Stderr: []byte(errb.String()), Err: err, ExitCode: code}
}

// defaultDeviceOpen opens a capture node the way the runtime will, without
// blocking on a stream that never starts.
func defaultDeviceOpen(path string) error {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	return f.Close()
}

// checkFunc is one probe. It must honour ctx and return a populated Check
// without an ID or Group: the engine fills those in.
type checkFunc func(ctx context.Context) Check

// runner executes checks sequentially and collects their results.
type runner struct {
	opts   Options
	checks []Check
	// rolesErr records why role checks were skipped, so the report says so
	// rather than silently dropping seven rows.
	rolesErr error
}

func newRunner(o Options) *runner {
	return &runner{opts: o.withDefaults()}
}

// Run probes every component in a fixed order and returns the report. It never
// panics and never blocks longer than the sum of its check deadlines: each
// probe gets its own context deadline and, if it ignores that, the engine
// abandons it and records a FAIL.
func Run(ctx context.Context, o Options) Report {
	r := newRunner(o)

	r.add(ctx, "storage.data_dir", r.opts.Timeout, func(ctx context.Context) Check { return r.checkDataDir(ctx) })
	r.add(ctx, "storage.database", r.opts.Timeout, func(ctx context.Context) Check { return r.checkDatabase(ctx) })

	for _, tool := range defaultTools {
		tool := tool
		r.add(ctx, tool.ID, r.opts.Timeout, func(ctx context.Context) Check { return r.checkTool(ctx, tool) })
	}
	r.add(ctx, "ocr.controlled_image", r.opts.Timeout, func(ctx context.Context) Check { return r.checkOCRImage(ctx) })
	r.add(ctx, "capture.devices", r.opts.Timeout, func(ctx context.Context) Check { return r.checkCaptureDevices(ctx) })
	r.add(ctx, "audio.sinks", r.opts.Timeout, func(ctx context.Context) Check { return r.checkAudioSinks(ctx) })

	r.checkRoles(ctx)

	if r.opts.Dev {
		r.add(ctx, "emulator.dolphin", r.opts.Timeout, func(ctx context.Context) Check { return r.checkEmulator(ctx) })
		// The game.image check hashes the whole disc image against the fixture
		// manifests, so it runs on its own budget rather than the 15 s probe
		// deadline (gameImageCheckBudget).
		r.add(ctx, "game.image", gameImageCheckBudget, func(ctx context.Context) Check { return r.checkGameImage(ctx) })
	}

	report := Report{GeneratedAt: r.opts.Now().UTC(), Checks: r.checks}
	report.Totals = countTotals(report.Checks)
	report.Status = overallStatus(report.Checks)
	return report
}

// add runs one check under its own deadline and records the outcome.
func (r *runner) add(ctx context.Context, id string, timeout time.Duration, fn checkFunc) {
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	started := time.Now()
	// A check that ignores its context must not hang the whole report, so the
	// probe runs apart and the engine walks away after deadline+grace. The
	// abandoned goroutine only writes to its own buffered channel.
	done := make(chan Check, 1)
	go func() { done <- guarded(fn, checkCtx) }()

	var c Check
	select {
	case c = <-done:
	case <-time.After(timeout + checkGrace):
		c = Check{
			Status:  StatusFail,
			Summary: fmt.Sprintf("probe did not finish within %s", timeout),
			Detail:  "the check ignored its context deadline; this is a doctor bug, not a host fault",
		}
	}

	c.ID = id
	c.Group = groupFor(id)
	if c.Status == "" {
		c.Status, c.Summary = StatusFail, "probe returned no status"
	}
	if c.Duration == 0 {
		c.Duration = time.Since(started)
	}
	r.checks = append(r.checks, c)
}

// guarded converts a panicking probe into a FAIL row (docs/spec/13: the
// diagnostic report has to be complete even when one probe is broken).
func guarded(fn checkFunc, ctx context.Context) (c Check) {
	defer func() {
		if rec := recover(); rec != nil {
			c = Check{
				Status:  StatusFail,
				Summary: fmt.Sprintf("probe panicked: %v", rec),
				Detail:  oneLine(string(debug.Stack()), 300),
			}
		}
	}()
	return fn(ctx)
}

func countTotals(checks []Check) Totals {
	t := Totals{Checks: len(checks)}
	for _, c := range checks {
		switch c.Status {
		case StatusOK:
			t.OK++
		case StatusDegraded:
			t.Degraded++
		case StatusBlocked:
			t.Blocked++
		case StatusNotConfigured:
			t.NotConfigured++
		default:
			t.Fail++
		}
	}
	return t
}

// overallStatus reports the worst state seen.
func overallStatus(checks []Check) string {
	worst := StatusOK
	rank := map[string]int{StatusOK: 0, StatusNotConfigured: 0, StatusDegraded: 1, StatusBlocked: 2, StatusFail: 3}
	for _, c := range checks {
		if v, ok := rank[c.Status]; ok && v > rank[worst] {
			worst = c.Status
		}
	}
	return worst
}

// groupNames maps the ID namespace to table groups.
var groupNames = map[string]string{
	"storage":   "Storage",
	"tools":     "Tools",
	"ocr":       "OCR",
	"capture":   "Capture",
	"audio":     "Audio",
	"providers": "Providers",
	"emulator":  "Emulator",
	"game":      "Game",
}

func groupFor(id string) string {
	prefix := id
	if i := strings.Index(id, "."); i >= 0 {
		prefix = id[:i]
	}
	if g, ok := groupNames[prefix]; ok {
		return g
	}
	return strings.ToUpper(prefix[:1]) + prefix[1:]
}

// oneLine collapses whitespace and caps length so a stack trace or an OCR page
// cannot dominate a table row.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(strings.ReplaceAll(s, "\x00", " ")), " ")
	if max > 0 && len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// firstLine returns the first non-empty line of a command's output.
func firstLine(b []byte) string {
	for _, line := range strings.Split(string(b), "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

func containsString(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
