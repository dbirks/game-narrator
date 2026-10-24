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

	"github.com/dbirks/game-narrator/internal/audio"
	"github.com/dbirks/game-narrator/internal/bootstrap"
	devicex "github.com/dbirks/game-narrator/internal/capture/devices"
	"github.com/dbirks/game-narrator/internal/capture/mode"
	"github.com/dbirks/game-narrator/internal/capture/usb"
	"github.com/dbirks/game-narrator/internal/dolphin"
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

	// (issue #31) an explicitly configured directory next to a non-empty
	// default one means two databases live on this host: a run that lost its
	// GAME_NARRATOR_DATA_DIR has been writing to the default all along. That
	// is a warning, not a fault — the configured directory is the one the
	// service uses — so the row degrades rather than fails.
	if c, warn := r.staleDefaultDataDir(dir); warn {
		return c
	}

	mode := info.Mode().Perm()
	detail := r.dataDirDetail(dir, fmt.Sprintf("%s, mode 0%03o", dir, mode))
	if created {
		detail += ", created by this run"
	}
	detail += ", write probe created and removed"
	if mode&0o077 != 0 {
		return Check{
			Status:  StatusDegraded,
			Summary: "data directory is readable by group or other",
			Detail:  r.dataDirDetail(dir, fmt.Sprintf("mode is 0%03o, want 0700; %s", mode, dir)),
			Remedy:  fmt.Sprintf("chmod 700 %s", dir),
		}
	}
	return Check{Status: StatusOK, Summary: "data directory is writable and private", Detail: detail}
}

// dataDirDetail appends the resolved storage the row probes — the database
// path and the setting that decided the data directory — to a detail string
// (issue #31: a storage row has to say where the data is, not just that it
// is writable). An empty source (seams that do not record one) leaves the
// detail unchanged.
func (r *runner) dataDirDetail(dir, detail string) string {
	if r.opts.DataDirSource == "" {
		return detail
	}
	return fmt.Sprintf("%s; database %s (source: %s)", detail, store.DBPath(dir), r.opts.DataDirSource)
}

// staleDefaultDataDir reports a DEGRADED check when a non-empty default data
// directory ($XDG_DATA_HOME/game-narrator, else $HOME/.local/share/game-narrator)
// exists alongside a different, explicitly configured one (issue #31: the
// restart opened a stale database from the default after the env file lost
// its GAME_NARRATOR_DATA_DIR). Only flag/env sources qualify: a default-
// sourced or unknown-source directory is the default itself. It never FAILs —
// the default's contents are user data, and the answer is a warning, not a
// fault.
func (r *runner) staleDefaultDataDir(configured string) (Check, bool) {
	switch r.opts.DataDirSource {
	case bootstrap.DataDirSourceFlag, bootstrap.DataDirSourceEnv:
	default:
		return Check{}, false
	}
	defaultDir := bootstrap.ResolveDataDir("", r.opts.LookupEnv)
	if defaultDir == "" || defaultDir == configured {
		return Check{}, false
	}
	entries, err := os.ReadDir(defaultDir)
	if err != nil || len(entries) == 0 {
		return Check{}, false
	}
	return Check{
		Status:  StatusDegraded,
		Summary: "a non-empty default data directory exists alongside the configured one",
		Detail: fmt.Sprintf("%s is in use (source: %s), but %s holds data from a run without %s — two databases on one host; database %s",
			configured, r.opts.DataDirSource, defaultDir, bootstrap.EnvDataDir, store.DBPath(configured)),
		Remedy: fmt.Sprintf("confirm which directory this run must use, name it with %s, and back up both directories before consolidating them", bootstrap.EnvDataDir),
	}, true
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
//
// The enumeration is capability-filtered: the kernel exposes memory-to-memory
// codec nodes as /dev/video* too (the UNO Q's Qualcomm venus decoder owns
// video0/video1), and counting those as capture nodes reports a capture card
// the appliance does not have. Only a node whose VIDIOC_QUERYCAP capabilities
// include VIDEO_CAPTURE and exclude every memory-to-memory flag counts
// (parseV4L2Capabilities); the rest are listed as ignored with their driver
// name so the operator sees why a node is present but not usable.
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

	if len(denied) == len(devices) {
		return Check{
			Status:  StatusDegraded,
			Summary: "capture device(s) exist but this user cannot open them",
			Detail:  strings.Join(denied, ", ") + " denied permission",
			Remedy:  "add this user to the video group (or the configured device ACL) and re-login",
		}
	}
	if _, lookErr := r.opts.LookPath("v4l2-ctl"); lookErr != nil {
		summary := fmt.Sprintf("%d video node(s): %s", len(devices), strings.Join(names, ", "))
		return Check{
			Status:  StatusDegraded,
			Summary: summary + " — capabilities not enumerated because v4l2-ctl is not installed",
			Detail:  lookErr.Error(),
			Remedy:  bootstrapRemedy + " (pacman -S v4l-utils)",
		}
	}

	// Per-node capability query (VIDIOC_QUERYCAP through v4l2-ctl --info):
	// a node that cannot be read is kept in the count — it cannot be proven
	// non-capture — and the failure is recorded in the detail.
	var (
		capable      []string
		capablePaths []string
		ignored      []string
		unread       []string
	)
	for _, d := range devices {
		base := filepath.Base(d)
		res := r.opts.Exec(ctx, "v4l2-ctl", "-d", d, "--info")
		if res.Err != nil {
			capable = append(capable, base)
			capablePaths = append(capablePaths, d)
			unread = append(unread, base)
			continue
		}
		capture, m2m, driver := parseV4L2Capabilities(res.Stdout)
		if capture && !m2m {
			capable = append(capable, base)
			capablePaths = append(capablePaths, d)
			continue
		}
		why := "memory-to-memory codec node"
		if capture {
			why = "video capture but also memory-to-memory"
		}
		ignored = append(ignored, fmt.Sprintf("%s (%s; %s)", base, orDriver(driver), why))
	}

	if len(capable) == 0 {
		return Check{
			Status:  StatusNotConfigured,
			Summary: "no UVC capture device attached",
			Detail:  fmt.Sprintf("%d node(s) present but ignored, not capture-capable: %s", len(ignored), strings.Join(ignored, ", ")) + deniedNote(denied),
			Remedy:  "attach the USB capture card (a UVC HDMI capture dongle, docs/spec/10 physical connection), then re-run doctor; the listed nodes are codec (memory-to-memory) nodes, not capture cards",
		}
	}

	// Mode selection (docs/spec/10 preflight: "record fps, progressive/
	// interlaced status, pixel format", "negotiate … e.g. 720p/1080p"): for
	// each capable node, the formats the card offers, the USB link the card
	// is actually running at, and the mode policy's pick (internal/capture/
	// mode) with the reason — so the doctor answers "which mode will the
	// runtime ask for, and why" before the first session, not after the
	// first flapping one. A SuperSpeed-class card negotiating HighSpeed is
	// the DEGRADED fault the link read exists to name: the card works, its
	// best modes do not.
	var (
		modeNotes  []string
		degraded   bool
		modeFaults []string
	)
	policy := mode.DefaultPolicy()
	for _, p := range capablePaths {
		base := filepath.Base(p)
		// A node whose capabilities were unreadable is kept in the count but
		// cannot be asked for formats either: a mode claim for it would be
		// invented, and the unread note below already says what to do.
		skippable := false
		for _, u := range unread {
			if u == base {
				skippable = true
				break
			}
		}
		if skippable {
			continue
		}
		fmtRes := r.opts.Exec(ctx, "v4l2-ctl", "-d", p, "--list-formats-ext")
		if fmtRes.Err != nil {
			modeFaults = append(modeFaults, fmt.Sprintf("%s: formats unreadable (%s)", base, oneLine(fmtRes.Err.Error(), 120)))
			continue
		}
		fmts := devicex.ParseFormatsExt(string(fmtRes.Stdout))
		speed, linkErr := usb.LinkSpeedFor(p, r.opts.SysfsRoot)
		link := mode.LinkUnknown
		linkName := "link unreadable (assumed HighSpeed)"
		if linkErr == nil {
			link = mode.LinkFromSpeed(speed.SpeedMBps)
			linkName = speed.Class()
			if speed.Downgraded() {
				degraded = true
				modeNotes = append(modeNotes, fmt.Sprintf("WARNING %s: a USB 3 card is negotiating USB 2 (%s) — its raw modes will not fit; re-seat it in a USB 3 port or drop the USB 2 hub", base, linkName))
			}
		}
		sel, selErr := mode.Select(fmts, mode.Policy{Link: link})
		if selErr != nil {
			modeFaults = append(modeFaults, fmt.Sprintf("%s: no mode fits the policy (%s)", base, oneLine(selErr.Error(), 160)))
			continue
		}
		modeNotes = append(modeNotes, fmt.Sprintf("%s: mode %s; %s; link %s; %d offered mode(s) rejected", base, sel.Mode.String(), sel.Reason, linkName, sel.Rejected))
	}

	status := StatusOK
	remedy := ""
	if degraded || len(modeFaults) > 0 {
		status = StatusDegraded
	}
	if degraded {
		remedy = "re-seat the capture card in a USB 3 (SuperSpeed) port or remove the USB 2 hub between it and the box; docs/spec/10 physical connection"
	}
	if len(modeFaults) > 0 {
		remedy = strings.Join(modeFaults, "; ")
	}

	summary := fmt.Sprintf("%d capture node(s): %s", len(capable), strings.Join(capable, ", "))
	detail := "v4l2-ctl --info capabilities: " + strings.Join(capable, ", ") + " capture-capable"
	if len(ignored) > 0 {
		detail += "; ignored as not capture-capable: " + strings.Join(ignored, ", ")
	}
	if len(unread) > 0 {
		detail += "; capabilities unreadable: " + strings.Join(unread, ", ")
	}
	if len(denied) > 0 {
		detail = "not openable by this user: " + strings.Join(denied, ", ") + "; " + detail
	}
	if len(modeNotes) > 0 {
		detail += fmt.Sprintf("; mode selection (policy %dx%d @ %s Hz): %s", policy.GameW, policy.GameH, fmt.Sprintf("%.0f", policy.AnalysisFPS), strings.Join(modeNotes, " | "))
	}
	return Check{Status: status, Summary: summary, Detail: oneLine(detail, 700), Remedy: remedy}
}

// deniedNote appends the not-openable note to a detail line when present.
func deniedNote(denied []string) string {
	if len(denied) == 0 {
		return ""
	}
	return "; not openable by this user: " + strings.Join(denied, ", ")
}

// orDriver renders the driver identity for the ignored-node list: the name
// v4l2-ctl reported, or "unknown driver" when the --info output carried no
// identity line (older v4l-utils versions print the section differently).
func orDriver(driver string) string {
	if strings.TrimSpace(driver) == "" {
		return "unknown driver"
	}
	return driver
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

// checkAudioSelected verifies the appliance's audio output selection (ADR-0074,
// gap audit G5): the device the effective selection names (env override >
// persisted store value > the platform default) must exist in the current
// enumeration, and it must not be the UAC (USB audio-class) interface of the
// capture dongle. On the UNO Q the HDMI capture dongle can expose a USB audio
// interface next to its video capture; that interface is not a speaker —
// speech routed there plays into the wall, so a selection pointing at it is
// reported, not played. The check never plays anything (docs/spec/13: playback
// is a consented action): it reads the selection, enumerates, and reports.
func (r *runner) checkAudioSelected(ctx context.Context) Check {
	lookupEnv := r.opts.LookupEnv
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}

	// The stored selection (the Settings page's persisted choice). A store the
	// check cannot read is reported in the detail, not treated as a selection
	// error — the database check owns the store-failure verdict, and an env or
	// default selection is still verifiable without the row.
	var stored *audio.Stored
	var storeNote string
	if strings.TrimSpace(r.opts.DataDir) != "" {
		st, err := store.Open(ctx, store.DBPath(r.opts.DataDir))
		if err != nil {
			storeNote = "stored selection not read (the database did not open; the database check owns that verdict)"
		} else {
			as, ok, gerr := st.GetAudioSettings(ctx)
			_ = st.Close()
			if gerr != nil {
				storeNote = "stored selection not read (" + gerr.Error() + ")"
			} else if ok {
				stored = &audio.Stored{Device: as.Device, SinkKind: as.SinkKind, MasterVolume: as.MasterVolume, Set: true}
			}
		}
	}

	sel, err := audio.Resolve(lookupEnv, stored)
	if err != nil {
		return Check{
			Status:  StatusFail,
			Summary: "the audio output selection does not resolve",
			Detail:  err.Error(),
			Remedy:  "fix the stored selection or the " + audio.EnvSink + " override in Settings → Audio output, then re-run doctor",
		}
	}

	enumerate := r.opts.audioDevices()
	list, lerr := enumerate(ctx, lookupEnv)
	if lerr != nil {
		return Check{
			Status:  StatusDegraded,
			Summary: "the selected audio device cannot be verified",
			Detail:  lerr.Error(),
			Remedy:  "start the audio service or attach a USB DAC, then re-run doctor",
		}
	}

	// The device the selection actually routes to: the named id, or the
	// platform default when the selection is "default" (the OS's own routing).
	target := sel.Device
	if target == audio.DefaultDevice {
		for _, d := range list.Devices {
			if d.PlatformDefault {
				target = d.ID
				break
			}
		}
		if target == audio.DefaultDevice {
			// No enumeration (or none marked default): the platform routes it.
			// The selection itself is sound — the OS decides — so OK.
			return Check{Status: StatusOK, Summary: "audio output: the platform default route", Detail: selectionDetail(sel, storeNote)}
		}
	}

	var found *audio.Device
	for i := range list.Devices {
		if list.Devices[i].ID == target {
			found = &list.Devices[i]
			break
		}
	}
	if found == nil {
		return Check{
			Status:  StatusDegraded,
			Summary: fmt.Sprintf("selected audio output %q is not among the enumerated devices", target),
			Detail:  "enumerated: " + audioDeviceIDList(list) + selectionDetailNote(storeNote),
			Remedy:  "pick a current device in Settings → Audio output, or use the platform default",
		}
	}

	// The UAC guard (gap audit G5): the identity strings (the sink name and the
	// description the backend saw) carrying the UAC marker name the USB
	// audio-class interface — on the UNO Q the capture dongle's own audio
	// interface, not a speaker.
	identity := strings.ToLower(found.ID + " " + found.Name)
	if strings.Contains(identity, "uac") {
		detail := fmt.Sprintf("%q is classified %s and carries the UAC (USB audio-class) marker: on the UNO Q that is the capture dongle's own audio interface, not a speaker. Speech routed there plays into the wall.", found.ID, found.Kind)
		if nodes, gerr := r.opts.Glob("/dev/video*"); gerr == nil && len(nodes) > 0 {
			detail += " A UVC capture node is attached (" + strings.Join(nodeBasenames(nodes), ", ") + "), which is where the UAC interface usually comes from."
		}
		return Check{
			Status:  StatusDegraded,
			Summary: "the selected audio output appears to be the capture dongle's UAC interface",
			Detail:  detail + selectionDetailNote(storeNote),
			Remedy:  "select the USB DAC or speaker in Settings → Audio output instead",
		}
	}

	summary := fmt.Sprintf("audio output: %q exists and is a %s device", found.ID, found.Kind)
	return Check{Status: StatusOK, Summary: summary, Detail: selectionDetail(sel, storeNote)}
}

// selectionDetail renders the selection's provenance (where each part came
// from) plus the store-read note when one exists, for the check's Detail.
func selectionDetail(sel audio.Selection, storeNote string) string {
	detail := fmt.Sprintf("device %q (%s), sink %q (%s)", sel.Device, sel.DeviceSource, sel.SinkKind, sel.SinkKindSource)
	return detail + selectionDetailNote(storeNote)
}

func selectionDetailNote(storeNote string) string {
	if storeNote == "" {
		return ""
	}
	return "; " + storeNote
}

func audioDeviceIDList(list audio.List) string {
	if len(list.Devices) == 0 {
		return "(none)"
	}
	ids := make([]string, 0, len(list.Devices))
	for _, d := range list.Devices {
		ids = append(ids, d.ID)
	}
	return strings.Join(ids, ", ")
}

func nodeBasenames(nodes []string) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, filepath.Base(n))
	}
	return out
}

// --- developer host --------------------------------------------------------

// checkEmulator is a --dev check. It probes presence plus the build version:
// `dolphin-emu --version` core-dumps without a display server (issue #62,
// build 2609), so the probe runs display-free through the dolphin package's
// argv/env contract (dolphin.ProbeArgs under dolphin.ProbeEnv,
// QT_QPA_PLATFORM=offscreen) rather than a bare launch. The actual headless
// rendering and controller-input proof lives in scripts/spikes/dolphin and
// docs/spikes/dolphin-capability.md.
func (r *runner) checkEmulator(ctx context.Context) Check {
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
	dolphinBin, _ := r.opts.LookPath("dolphin-emu")
	xvfb, _ := r.opts.LookPath("Xvfb")

	version, err := dolphin.HeadlessVersion(ctx, "dolphin-emu", func(ctx context.Context, name string, args []string, env []string) (string, error) {
		res := r.opts.ExecEnv(ctx, name, args, env)
		if res.Err != nil {
			return string(res.Stdout), res.Err
		}
		return string(res.Stdout), nil
	})
	if err != nil {
		return Check{
			Status:  StatusDegraded,
			Summary: "dolphin-emu present, but the display-free version probe failed",
			Detail:  oneLine(fmt.Sprintf("dolphin-emu %s (env %s): %v", strings.Join(dolphin.ProbeArgs(), " "), strings.Join(dolphin.ProbeEnv(), " "), err), 240),
			Remedy:  "probe the binary under a display instead: xvfb-run -a dolphin-emu --version (issue #62)",
		}
	}
	return Check{
		Status:  StatusOK,
		Summary: version + " (rendering proven by scripts/spikes/dolphin)",
		Detail:  fmt.Sprintf("dolphin-emu at %s, Xvfb at %s; display-free probe: %s", dolphinBin, xvfb, version),
	}
}

// checkGameImage reports the state of the user-supplied game image and, when
// it is not usable, exactly what to do: which environment variable names it,
// what file is expected (the user's own disc image, CISO/ISO), how to verify
// its identity (the SHA-256 prefix from the fixture manifests, read at run
// time when they are present) and which gates need it. The path itself is
// private and is never printed, in any output stream, including error text
// from os.Stat (AGENTS.md rule 5); the only reportable facts are existence,
// size and the SHA-256 prefix.
func (r *runner) checkGameImage(ctx context.Context) Check {
	path, ok := r.opts.LookupEnv(EnvGameImagePath)
	path = strings.TrimSpace(path)
	exp := fixtureImageExpectation(r.opts.fixtureRoot())

	if !ok || path == "" {
		return Check{
			Status:  StatusNotConfigured,
			Summary: "no game image is configured",
			Detail: EnvGameImagePath + " is not set. Expect your own legally obtained GameCube disc image (CISO/ISO, e.g. .ciso or .iso). " +
				exp.verifyText() + ". Needed by: " + gameImageGates + ".",
			Remedy: "export " + EnvGameImagePath + "=<path to your disc image>; the path is never recorded. Re-run doctor.",
		}
	}

	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		reason := "the configured file does not exist"
		if err == nil {
			reason = "the configured path points at a directory, not a disc image"
		} else if errors.Is(err, fs.ErrPermission) {
			reason = "the configured file is not readable by this user"
		}
		return Check{
			Status:  StatusNotConfigured,
			Summary: "the configured game image is not usable: " + reason,
			Detail: EnvGameImagePath + " is set, but " + reason + "; the path is not reported. Expect your own legally obtained GameCube disc image (CISO/ISO, e.g. .ciso or .iso). " +
				exp.verifyText() + ". Needed by: " + gameImageGates + ".",
			Remedy: "point " + EnvGameImagePath + " at your disc image, verify its SHA-256 prefix, and re-run doctor; the path is never recorded",
		}
	}

	summary := fmt.Sprintf("%s is set, file exists, %d bytes", EnvGameImagePath, info.Size())
	if len(exp.shas) == 0 {
		return Check{
			Status:  StatusOK,
			Summary: summary,
			Detail:  "no fixture manifest with a recorded image SHA-256 was found, so the identity is not verified against a fixture",
		}
	}

	sum, err := sha256File(ctx, path)
	if err != nil {
		return Check{
			Status:  StatusDegraded,
			Summary: summary + " (identity not verified)",
			Detail:  "the image could not be hashed within the check budget: " + oneLine(err.Error(), 200),
			Remedy:  "re-run doctor; the bench gate re-verifies the image SHA-256 against the fixture manifest",
		}
	}
	for _, want := range exp.shas {
		if strings.EqualFold(sum, want) {
			return Check{
				Status:  StatusOK,
				Summary: summary + " and matches the fixture manifest (SHA-256 prefix " + hashPrefix(sum) + ")",
			}
		}
	}
	want := make([]string, len(exp.shas))
	for i, s := range exp.shas {
		want[i] = hashPrefix(s)
	}
	return Check{
		Status:  StatusFail,
		Summary: "the configured game image does not match the fixtures it was pointed at",
		Detail: "its SHA-256 prefix is " + hashPrefix(sum) + "; the fixture manifest(s) record " +
			strings.Join(want, " or ") + ". The fixtures were recorded against a different image.",
		Remedy: "point " + EnvGameImagePath + " at the image the fixtures were recorded against (verify the SHA-256 prefix above), or re-record the fixtures; the path is never recorded",
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
