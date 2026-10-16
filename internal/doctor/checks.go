package doctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/dbirks/game-narrator/internal/bootstrap"
	"github.com/dbirks/game-narrator/internal/store"
)

// bootstrapRemedy is the single documented place that installs host
// dependencies (docs/spec/10 "Arch development host").
const bootstrapRemedy = "run scripts/bootstrap-arch.sh"

// --- storage ---------------------------------------------------------------

// checkDataDir proves the data directory exists (or can be created), accepts a
// write, and is private. A world-readable asset root would expose user-curated
// profiles and cache to every account on the box (docs/spec/02 "Asset store
// contract": directory 0700).
func (r *runner) checkDataDir(_ context.Context) Check {
	dir := r.opts.DataDir
	if strings.TrimSpace(dir) == "" {
		return Check{
			Status:  StatusFail,
			Summary: "no data directory is configured",
			Remedy:  fmt.Sprintf("pass --data-dir or set %s", bootstrap.EnvDataDir),
		}
	}

	info, err := os.Stat(dir)
	created := false
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return Check{
				Status:  StatusFail,
				Summary: "cannot create the data directory",
				Detail:  fmt.Sprintf("%s: %s", dir, err),
				Remedy:  "choose a writable location, or fix the ownership of a parent",
			}
		}
		// MkdirAll masks the mode with the umask; 0700 is a requirement.
		if err := os.Chmod(dir, 0o700); err != nil {
			return Check{Status: StatusFail, Summary: "cannot set 0700 on the data directory", Detail: dir + ": " + err.Error()}
		}
		created = true
		info, err = os.Stat(dir)
	}
	if err != nil {
		return Check{Status: StatusFail, Summary: "cannot inspect the data directory", Detail: fmt.Sprintf("%s: %s", dir, err)}
	}
	if !info.IsDir() {
		return Check{Status: StatusFail, Summary: "the data directory path is not a directory", Detail: dir}
	}

	probe, err := os.CreateTemp(dir, "gn-doctor-write-*")
	if err != nil {
		return Check{
			Status:  StatusFail,
			Summary: "data directory is not writable",
			Detail:  fmt.Sprintf("%s: %s", dir, err),
			Remedy:  "check free space, ownership and mount options (a read-only mount fails here)",
		}
	}
	probePath := probe.Name()
	closeErr := probe.Close()
	removeErr := os.Remove(probePath)
	if closeErr != nil || (removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist)) {
		detail := appendError(closeErr, removeErr)
		return Check{Status: StatusFail, Summary: "data directory write probe could not be cleaned up", Detail: fmt.Sprintf("%s: %s", dir, detail)}
	}

	mode := info.Mode().Perm()
	detail := fmt.Sprintf("%s, mode 0%03o", dir, mode)
	if created {
		detail += ", created by this run"
	}
	detail += ", write probe created and removed"
	if mode&0o077 != 0 {
		return Check{
			Status:  StatusDegraded,
			Summary: "data directory is readable by group or other",
			Detail:  fmt.Sprintf("mode is 0%03o, want 0700; %s", mode, dir),
			Remedy:  fmt.Sprintf("chmod 700 %s", dir),
		}
	}
	return Check{Status: StatusOK, Summary: "data directory is writable and private", Detail: detail}
}

// checkDatabase really opens the SQLite catalogue, applies pending migrations
// and runs PRAGMA integrity_check. A store that cannot be opened or that fails
// integrity is the single worst thing this service can arrive to, so it is
// FAIL rather than DEGRADED.
func (r *runner) checkDatabase(ctx context.Context) Check {
	if strings.TrimSpace(r.opts.DataDir) == "" {
		return Check{Status: StatusFail, Summary: "no data directory is configured, so the database cannot be opened"}
	}
	st, err := store.Open(ctx, store.DBPath(r.opts.DataDir))
	if err != nil {
		return Check{
			Status:  StatusFail,
			Summary: "cannot open the SQLite database",
			Detail:  err.Error(),
			Remedy:  "run game-narrator migrate --data-dir " + r.opts.DataDir + " and check file ownership and free space",
		}
	}

	c := probeDatabase(ctx, st)
	// The probe borrows a connection pool; a failed close is a host signal
	// (locked file, dying disk) and must not disappear behind an OK row.
	if cerr := st.Close(); cerr != nil {
		if c.Status == StatusOK {
			c.Status = StatusDegraded
			c.Summary = "database probe passed but the store did not close cleanly"
		}
		c.Detail = oneLine(c.Detail+"; close: "+cerr.Error(), 400)
	}
	return c
}

// probeDatabase runs the read-only part of the storage check against an open
// store so the caller owns the close.
func probeDatabase(ctx context.Context, st *store.Store) Check {
	applied, err := st.Migrate(ctx)
	if err != nil {
		return Check{
			Status:  StatusFail,
			Summary: "migrations did not apply cleanly",
			Detail:  err.Error(),
			Remedy:  "back up the database file, then run game-narrator migrate and read its error (docs/spec/02)",
		}
	}
	version, err := st.SchemaVersion(ctx)
	if err != nil {
		return Check{Status: StatusFail, Summary: "cannot read the schema version", Detail: err.Error()}
	}
	sqliteVersion, err := st.SQLiteVersion(ctx)
	if err != nil {
		return Check{Status: StatusFail, Summary: "cannot read the SQLite version", Detail: err.Error()}
	}
	if err := st.IntegrityCheck(ctx); err != nil {
		return Check{
			Status:  StatusFail,
			Summary: "database integrity check failed",
			Detail:  err.Error(),
			Remedy:  "restore the latest backup before starting a session (docs/spec/02 \"Backup and restore\")",
		}
	}

	detail := fmt.Sprintf("sqlite %s at %s", sqliteVersion, st.Path())
	if len(applied) > 0 {
		detail += ", applied " + strings.Join(applied, ", ")
	}
	return Check{
		Status:  StatusOK,
		Summary: fmt.Sprintf("schema version %d, integrity ok", version),
		Detail:  detail,
	}
}

// --- host tools ------------------------------------------------------------

// tool is one external binary the runtime shells out to. The runtime runs
// these as processes rather than linking them, per docs/adr on FFmpeg/OCI
// strategy and docs/spec/10 ("do not build native OCR into Go").
type tool struct {
	ID   string
	Name string
	Args []string
	Spec string // how to install it
}

var defaultTools = []tool{
	{ID: "tools.ffmpeg", Name: "ffmpeg", Args: []string{"-version"}, Spec: "pacman -S ffmpeg"},
	{ID: "tools.v4l2_ctl", Name: "v4l2-ctl", Args: []string{"--version"}, Spec: "pacman -S v4l-utils"},
	{ID: "tools.tesseract", Name: "tesseract", Args: []string{"--version"}, Spec: "pacman -S tesseract tesseract-data-eng"},
}

// checkTool requires the binary to exist *and* to answer a version query: a
// stale or broken package that cannot start is FAIL, while a missing one is
// BLOCKED because the bootstrap script can fix it.
func (r *runner) checkTool(ctx context.Context, t tool) Check {
	path, err := r.opts.LookPath(t.Name)
	if err != nil {
		return Check{
			Status:  StatusBlocked,
			Summary: fmt.Sprintf("%s is not installed", t.Name),
			Detail:  err.Error(),
			Remedy:  bootstrapRemedy + " (" + t.Spec + ")",
		}
	}
	res := r.opts.Exec(ctx, t.Name, t.Args...)
	if res.Err != nil {
		return Check{
			Status:  StatusFail,
			Summary: fmt.Sprintf("%s is installed but did not run", t.Name),
			Detail:  oneLine(fmt.Sprintf("%s: %v\n%s", path, res.Err, res.Stderr), 300),
			Remedy:  "reinstall the package (" + t.Spec + ") and re-run doctor",
		}
	}
	line := firstLine(res.Stdout)
	if line == "" {
		line = firstLine(res.Stderr)
	}
	if line == "" {
		return Check{
			Status:  StatusFail,
			Summary: fmt.Sprintf("%s ran but reported no version", t.Name),
			Detail:  path,
			Remedy:  "reinstall the package (" + t.Spec + ") and re-run doctor",
		}
	}
	return Check{Status: StatusOK, Summary: line, Detail: path}
}

// --- OCR -------------------------------------------------------------------

// checkOCRImage runs the real tesseract binary over the embedded controlled
// image. This is the docs/spec/13 "OCR on a controlled image" probe: it proves
// the engine, the tessdata pack and the process plumbing, not just that a
// binary is on PATH.
func (r *runner) checkOCRImage(ctx context.Context) Check {
	if _, err := r.opts.LookPath("tesseract"); err != nil {
		return Check{
			Status:  StatusBlocked,
			Summary: "cannot probe OCR: tesseract is not installed",
			Detail:  err.Error(),
			Remedy:  bootstrapRemedy + " (pacman -S tesseract tesseract-data-eng)",
		}
	}

	dir, err := os.MkdirTemp("", "game-narrator-doctor-ocr-")
	if err != nil {
		return Check{Status: StatusFail, Summary: "cannot stage the controlled image", Detail: err.Error()}
	}
	defer func() { _ = os.RemoveAll(dir) }()

	imagePath := filepath.Join(dir, "controlled.png")
	if err := os.WriteFile(imagePath, r.ocrImage(), 0o600); err != nil {
		return Check{Status: StatusFail, Summary: "cannot write the controlled image for tesseract", Detail: err.Error()}
	}

	res := r.opts.Exec(ctx, "tesseract", imagePath, "stdout")
	if res.Err != nil {
		return Check{
			Status:  StatusFail,
			Summary: "tesseract failed on the controlled image",
			Detail:  oneLine(fmt.Sprintf("%v: %s", res.Err, res.Stderr), 300),
			Remedy:  "check the tesseract language pack (tesseract-data-eng) and re-run doctor",
		}
	}
	got := normalizeText(res.Stdout)
	if !strings.Contains(got, controlledText) {
		return Check{
			Status:  StatusFail,
			Summary: fmt.Sprintf("OCR did not recover %q from the controlled image", controlledText),
			Detail:  "read: " + oneLine(got, 200),
			Remedy:  "reinstall the English tessdata pack; if it persists the binary cannot read PNG input",
		}
	}
	return Check{
		Status:  StatusOK,
		Summary: fmt.Sprintf("read %q from the controlled image", controlledText),
		Detail:  "engine output: " + oneLine(got, 120),
	}
}

// normalizeText collapses all whitespace runs so a trailing newline or a
// page-break from the OCR engine does not hide a correct read.
func normalizeText(b []byte) string {
	return strings.Join(strings.Fields(string(b)), " ")
}

// --- capture ---------------------------------------------------------------

// checkCaptureDevices enumerates /dev/video* rather than assuming /dev/video0
// (docs/spec/10 "Hardware discovery and preflight"), and asks v4l2-ctl for the
// names it reports. An absent capture card is a normal state for a dev host, so
// it is NOT CONFIGURED, not a failure.
func (r *runner) checkCaptureDevices(ctx context.Context) Check {
	devices, err := r.opts.Glob("/dev/video*")
	if err != nil {
		return Check{Status: StatusFail, Summary: "cannot list capture devices", Detail: err.Error()}
	}
	if len(devices) == 0 {
		return Check{
			Status:  StatusNotConfigured,
			Summary: "no UVC capture device attached",
			Detail:  "nothing matches /dev/video*",
			Remedy:  "attach the USB capture card, then re-run doctor; see docs/spec/10 for the converter setup",
		}
	}

	names := make([]string, 0, len(devices))
	var denied []string
	for _, d := range devices {
		names = append(names, filepath.Base(d))
		if err := r.opts.DeviceOpen(d); err != nil {
			if errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.EACCES) {
				denied = append(denied, filepath.Base(d))
			}
		}
	}
	summary := fmt.Sprintf("%d capture node(s): %s", len(devices), strings.Join(names, ", "))

	if len(denied) == len(devices) {
		return Check{
			Status:  StatusDegraded,
			Summary: "capture device(s) exist but this user cannot open them",
			Detail:  strings.Join(denied, ", ") + " denied permission",
			Remedy:  "add this user to the video group (or the configured device ACL) and re-login",
		}
	}
	if _, lookErr := r.opts.LookPath("v4l2-ctl"); lookErr != nil {
		return Check{
			Status:  StatusDegraded,
			Summary: summary + " — formats not enumerated because v4l2-ctl is not installed",
			Detail:  lookErr.Error(),
			Remedy:  bootstrapRemedy + " (pacman -S v4l-utils)",
		}
	}

	res := r.opts.Exec(ctx, "v4l2-ctl", "--list-devices")
	if res.Err != nil {
		return Check{
			Status:  StatusDegraded,
			Summary: summary + " — v4l2-ctl could not enumerate devices",
			Detail:  oneLine(fmt.Sprintf("%v: %s", res.Err, res.Stderr), 300),
			Remedy:  "check udev permissions for the video group",
		}
	}
	detail := oneLine(string(res.Stdout), 400)
	if len(denied) > 0 {
		detail = "not openable by this user: " + strings.Join(denied, ", ") + "; " + detail
	}
	return Check{Status: StatusOK, Summary: summary, Detail: "v4l2-ctl --list-devices: " + detail}
}

// --- audio -----------------------------------------------------------------

// checkAudioSinks lists output sinks. It deliberately never plays anything:
// docs/spec/13 makes playback a consented action, and a diagnostic that hoots
// down a living room on every boot is not a diagnostic.
func (r *runner) checkAudioSinks(ctx context.Context) Check {
	type backend struct {
		name string
		args []string
		spec string
	}
	backends := []backend{
		{"pactl", []string{"list", "short", "sinks"}, "pacman -S pipewire-pulse (or pulseaudio)"},
		{"aplay", []string{"-l"}, "pacman -S alsa-utils"},
	}

	var (
		tried    []string
		problems []string
	)
	for _, b := range backends {
		if _, err := r.opts.LookPath(b.name); err != nil {
			continue
		}
		tried = append(tried, b.name)
		res := r.opts.Exec(ctx, b.name, b.args...)
		count := countSinkLines(res.Stdout)
		if res.Err != nil {
			problems = append(problems, fmt.Sprintf("%s %s: %v", b.name, strings.Join(b.args, " "), res.Err))
			continue
		}
		if count == 0 {
			return Check{
				Status:  StatusDegraded,
				Summary: "no audio output sink found",
				Detail:  fmt.Sprintf("%s %s reported no sinks", b.name, strings.Join(b.args, " ")),
				Remedy:  "start the audio service or attach a USB DAC; speech output is unavailable until then",
			}
		}
		detail := fmt.Sprintf("%s %s: %d sink(s)", b.name, strings.Join(b.args, " "), count)
		if len(problems) > 0 {
			detail += "; earlier attempt failed: " + oneLine(strings.Join(problems, "; "), 160)
		}
		return Check{Status: StatusOK, Summary: fmt.Sprintf("%d audio output sink(s) via %s", count, b.name), Detail: detail}
	}

	if len(tried) > 0 {
		return Check{
			Status:  StatusDegraded,
			Summary: "could not enumerate audio sinks",
			Detail:  oneLine(strings.Join(problems, "; "), 300),
			Remedy:  "is the PipeWire/PulseAudio daemon running for this user?",
		}
	}
	return Check{
		Status:  StatusBlocked,
		Summary: "no audio backend tool is installed",
		Detail:  "neither pactl nor aplay is on PATH",
		Remedy:  bootstrapRemedy + " (pacman -S pipewire-pulse alsa-utils)",
	}
}

// countSinkLines counts the device rows in pactl/aplay output. pactl prints one
// sink per line; aplay prints a "card N:" header per card plus "device N:"
// lines. Blank and banner lines must not count, or every host looks ready.
func countSinkLines(out []byte) int {
	count := 0
	for _, line := range strings.Split(string(out), "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		lower := strings.ToLower(t)
		switch {
		case strings.HasPrefix(lower, "card "), strings.HasPrefix(lower, "device "):
			count++
		case strings.Contains(lower, "no soundcards found"), strings.Contains(lower, "0 sink"):
			// Explicit emptiness must never be counted as a device.
		default:
			// pactl's short format is index/tab/name/driver/…: a numeric first
			// field is the only shape that identifies a sink row.
			if fields := strings.Fields(t); len(fields) > 1 && allDigits(fields[0]) {
				count++
			}
		}
	}
	return count
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// --- developer host --------------------------------------------------------

// checkEmulator is a --dev check. It probes presence only: the actual headless
// rendering and controller-input proof lives in scripts/spikes/dolphin and
// docs/spikes/dolphin-capability.md, and `dolphin-emu --version` aborts on this
// host, so launching it here would report a false BLOCKED.
func (r *runner) checkEmulator(_ context.Context) Check {
	var missing []string
	for _, bin := range []string{"dolphin-emu", "Xvfb"} {
		if _, err := r.opts.LookPath(bin); err != nil {
			missing = append(missing, bin)
		}
	}
	if len(missing) > 0 {
		return Check{
			Status:  StatusBlocked,
			Summary: fmt.Sprintf("%s not available for headless Dolphin", strings.Join(missing, " and ")),
			Remedy:  bootstrapRemedy + " (pacman -S dolphin-emu xorg-server-Xvfb), see docs/spikes/dolphin-capability.md",
		}
	}
	dolphin, _ := r.opts.LookPath("dolphin-emu")
	xvfb, _ := r.opts.LookPath("Xvfb")
	return Check{
		Status:  StatusOK,
		Summary: "present (rendering proven by scripts/spikes/dolphin)",
		Detail:  fmt.Sprintf("dolphin-emu at %s, Xvfb at %s", dolphin, xvfb),
	}
}

// checkGameImage reports only whether the user-supplied image is configured and
// readable. The path itself is private and is never printed, in any output
// stream, including error text from os.Stat (AGENTS.md rule 5).
func (r *runner) checkGameImage(_ context.Context) Check {
	path, ok := r.opts.LookupEnv(EnvGameImagePath)
	path = strings.TrimSpace(path)
	if !ok || path == "" {
		return Check{
			Status:  StatusNotConfigured,
			Summary: "no game image is configured",
			Detail:  EnvGameImagePath + " is not set",
			Remedy:  "export " + EnvGameImagePath + "=<path to your own legally obtained image>; the path is never recorded",
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		reason := "the file cannot be read"
		if errors.Is(err, fs.ErrNotExist) {
			reason = "the file does not exist"
		} else if errors.Is(err, fs.ErrPermission) {
			reason = "the file is not readable by this user"
		}
		return Check{
			Status:  StatusFail,
			Summary: EnvGameImagePath + " is set, but " + reason,
			Detail:  "the configured path is not reported",
			Remedy:  "fix the path or permissions; doctor never prints it",
		}
	}
	if info.IsDir() {
		return Check{Status: StatusFail, Summary: EnvGameImagePath + " points at a directory, not a game image"}
	}
	return Check{
		Status:  StatusOK,
		Summary: fmt.Sprintf("%s is set, file exists, %d bytes", EnvGameImagePath, info.Size()),
	}
}

// appendError joins two non-nil errors for a Detail line.
func appendError(a, b error) string {
	joined := errors.Join(a, b)
	if joined == nil {
		return "none"
	}
	return joined.Error()
}
