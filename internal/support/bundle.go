// Bundle construction for `game-narrator support-bundle` (docs/spec/13
// "Logging/privacy": automated scrubber; "Diagnostics page and CLI").
//
// A bundle is one tar.gz whose every entry is a small JSON document:
//
//	manifest.json          build identity, created_at, the data dir's
//	                       basename only, and a sha256 manifest of every
//	                       other entry (the recipient verifies it before
//	                       sharing; a manifest cannot carry its own hash)
//	doctor.json            the doctor report; network provider probes are
//	                       skipped unless --probe (offline-safe by default)
//	store.json             schema version, applied migrations and per-table
//	                       row COUNTS — never row content (docs/spec/13)
//	config.redacted.json   the effective model-role configuration with every
//	                       URL reduced to scheme + redacted host and every
//	                       token/secret replaced by a sentinel; environment
//	                       variable *names* and their set/unset state survive
//	dev-trace.json         only with --include-dev-trace: session ids, blob
//	                       hashes, sizes and mtimes. Never frames, crops,
//	                       audio or dialogue text.
//
// The app writes no log file under the data dir: structured JSON goes to
// stderr (the service journal), and the manifest records that instead of
// shipping log content.
//
// The section builders live in their own files: doctor_section.go,
// store_section.go, config_section.go, devtrace.go. This file owns the
// options, the manifest and the archive writer.
package support

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/bootstrap"
	"github.com/dbirks/game-narrator/internal/store"
)

// The archive entry names. Fixed, documented and asserted by the tests.
const (
	ManifestPath = "manifest.json"
	DoctorPath   = "doctor.json"
	StorePath    = "store.json"
	ConfigPath   = "config.redacted.json"
	DevTracePath = "dev-trace.json"
)

// Options selects what a bundle contains. Zero values mean "offline, no
// window, no dev-trace metadata".
type Options struct {
	// DataDir is the durable data directory to bundle.
	DataDir string
	// Since limits the dev-trace metadata window (0 = no limit): only blobs
	// modified within [now-Since, now] are listed. There is no log file to
	// window over (see the manifest's logging note), so this flag only has
	// an effect with IncludeDevTrace.
	Since time.Duration
	// IncludeDevTrace adds dev-trace.json (metadata only).
	IncludeDevTrace bool
	// Probe makes the doctor section exercise the configured provider
	// endpoints for real. Off by default: a support bundle is built offline
	// so it can be produced (and tested) with no network.
	Probe bool
	// Now stamps the manifest and the dev-trace window; nil means time.Now.
	Now func() time.Time
}

// ManifestFile is one archive entry with its content hash, so a recipient can
// verify the bundle was not altered in transit before trusting it.
type ManifestFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// Manifest is the bundle's cover page. It carries the data dir's *basename*
// only — the full path would identify the operator's machine layout
// (docs/release.md "Hard rules" bans /home/ paths in shipped artifacts).
type Manifest struct {
	Version    string         `json:"version"`
	Commit     string         `json:"commit"`
	GoVersion  string         `json:"go_version"`
	OS         string         `json:"os"`
	Arch       string         `json:"arch"`
	CreatedAt  time.Time      `json:"created_at"`
	DataDir    string         `json:"data_dir"`
	DoctorMode string         `json:"doctor_mode"`
	Logging    Logging        `json:"logging"`
	DevTrace   *bool          `json:"dev_trace_included"`
	Files      []ManifestFile `json:"files"`
}

// Logging records where structured logs live. The service logs structured
// JSON to stderr only; nothing under the data dir is a log file, so the
// bundle ships this note instead of log content.
type Logging struct {
	File string `json:"file"`
	Note string `json:"note"`
}

const (
	DoctorModeOffline = "offline"
	DoctorModeProbe   = "probe"
)

const loggingNote = "the service logs structured JSON to stderr only; no log file is written under the data directory, so this bundle carries a note instead of log content (see the process journal of 'game-narrator serve')"

// Build assembles the bundle in memory and writes one tar.gz to outPath. It
// never modifies the data dir: the store is opened query-only and no file
// under it is copied into the archive.
func Build(ctx context.Context, o Options, outPath string) (*Manifest, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	info, err := os.Stat(o.DataDir)
	if err != nil {
		return nil, fmt.Errorf("support: data dir: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("support: %s is not a directory", o.DataDir)
	}
	// The database must already exist: a diagnostics command must not create
	// or migrate the operator's data dir behind their back (`serve` and
	// `doctor` both apply pending migrations by design; the bundle must not,
	// so this check runs before the doctor section, whose storage probe
	// would otherwise open and migrate the store).
	if _, err := os.Stat(store.DBPath(o.DataDir)); err != nil {
		return nil, fmt.Errorf("support: no database in the data dir (basename %s): run 'game-narrator migrate --data-dir DIR' first; the bundle build never creates or migrates the database", filepath.Base(o.DataDir))
	}

	files := make(map[string][]byte, 5)

	doctorJSON, mode, err := buildDoctor(ctx, o)
	if err != nil {
		return nil, fmt.Errorf("support: doctor section: %w", err)
	}
	files[DoctorPath] = doctorJSON

	storeJSON, err := buildStore(ctx, o.DataDir)
	if err != nil {
		return nil, fmt.Errorf("support: store section: %w", err)
	}
	files[StorePath] = storeJSON

	configJSON, err := buildConfig()
	if err != nil {
		return nil, fmt.Errorf("support: config section: %w", err)
	}
	files[ConfigPath] = configJSON

	devTraceIncluded := false
	if o.IncludeDevTrace {
		devTraceJSON, err := buildDevTrace(o)
		if err != nil {
			return nil, fmt.Errorf("support: dev-trace section: %w", err)
		}
		files[DevTracePath] = devTraceJSON
		devTraceIncluded = true
	}

	build := bootstrap.Version()
	man := Manifest{
		Version:    build.Version,
		Commit:     build.Commit,
		GoVersion:  build.GoVersion,
		OS:         build.GOOS,
		Arch:       build.GOARCH,
		CreatedAt:  o.Now().UTC(),
		DataDir:    filepath.Base(o.DataDir),
		DoctorMode: mode,
		Logging:    Logging{File: "", Note: loggingNote},
		DevTrace:   &devTraceIncluded,
	}
	for name, data := range files {
		sum := sha256.Sum256(data)
		man.Files = append(man.Files, ManifestFile{
			Path:   name,
			SHA256: hex.EncodeToString(sum[:]),
			Bytes:  int64(len(data)),
		})
	}
	sort.Slice(man.Files, func(i, j int) bool { return man.Files[i].Path < man.Files[j].Path })

	return &man, writeArchive(outPath, files, man)
}

// writeArchive writes the tar.gz: the manifest first (a recipient reads it
// before anything else), then the sections in path order. Entries carry flat
// names only — the archive layout is documented, not nested.
func writeArchive(outPath string, files map[string][]byte, man Manifest) error {
	if outPath == "" {
		return fmt.Errorf("support: no output path")
	}
	f, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("support: create %s: %w", outPath, err)
	}
	defer f.Close()

	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)

	manifestJSON, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return fmt.Errorf("support: encode manifest: %w", err)
	}
	entries := []struct {
		name string
		data []byte
	}{
		{ManifestPath, append(manifestJSON, '\n')},
	}
	for name := range files {
		entries = append(entries, struct {
			name string
			data []byte
		}{name, files[name]})
	}
	sort.Slice(entries[1:], func(i, j int) bool { return entries[i+1].name < entries[j+1].name })

	for _, e := range entries {
		if err := addTarEntry(tw, e.name, e.data); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("support: close archive: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("support: close gzip: %w", err)
	}
	return f.Close()
}

func addTarEntry(tw *tar.Writer, name string, data []byte) error {
	hdr := &tar.Header{
		Name:     name,
		Size:     int64(len(data)),
		Mode:     0o600,
		Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("support: header %s: %w", name, err)
	}
	if _, err := tw.Write(data); err != nil {
		return fmt.Errorf("support: write %s: %w", name, err)
	}
	return nil
}

// marshalDoc renders one bundle section: pretty JSON, deterministic key
// order (the encoding/json package sorts map keys; slices are ordered by
// the builders), trailing newline.
func marshalDoc(v any) ([]byte, error) {
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("support: encode section: %w", err)
	}
	return []byte(buf.String()), nil
}
