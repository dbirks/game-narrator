package main

// End-to-end test for support-bundle: it builds the real binary, migrates a
// fresh data dir with the real migrate command, runs the real
// support-bundle command and inspects the archive the process wrote. The
// redaction and content-guard assertions live in internal/support (they
// build bundles through the same Build entry point); this test proves the
// command boundary wires them to a usable tar.gz.

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// buildBinary compiles the game-narrator command from this package's
// directory into dir and returns the binary path.
func buildBinary(t *testing.T, dir string) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	pkgDir := filepath.Dir(thisFile)
	binPath := filepath.Join(dir, "game-narrator")
	cmd := exec.Command("go", "build", "-o", binPath, ".")
	cmd.Dir = pkgDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return binPath
}

// runBinary runs the built binary and returns (exitCode, stdout, stderr).
func runBinary(t *testing.T, binPath string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(binPath, args...)
	var outB, errB strings.Builder
	cmd.Stdout = &outB
	cmd.Stderr = &errB
	err := cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if ok := errors.As(err, &ee); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("run %v: %v", args, err)
		}
	}
	return code, outB.String(), errB.String()
}

func TestRunSupportBundleEndToEnd(t *testing.T) {
	binDir := t.TempDir()
	binPath := buildBinary(t, binDir)

	dataDir := t.TempDir()
	code, out, errOut := runBinary(t, binPath, "migrate", "--data-dir", dataDir)
	if code != 0 {
		t.Fatalf("migrate exit = %d, stdout %q, stderr %q; want 0", code, out, errOut)
	}

	outPath := filepath.Join(binDir, "bundle.tar.gz")
	code, out, errOut = runBinary(t, binPath, "support-bundle",
		"--data-dir", dataDir, "--out", outPath, "--include-dev-trace", "--since", "24h")
	if code != 0 {
		t.Fatalf("support-bundle exit = %d, stdout %q, stderr %q; want 0", code, out, errOut)
	}
	if !strings.Contains(out, "support bundle written: "+outPath) {
		t.Errorf("stdout missing the preview line %q:\n%s", "support bundle written: "+outPath, out)
	}
	// The preview lists the section hashes before export (docs/spec/13);
	// the manifest itself is the cover page and lists the other entries.
	for _, name := range []string{"doctor.json", "store.json", "bottlenecks.json", "config.redacted.json", "dev-trace.json"} {
		if !strings.Contains(out, name) {
			t.Errorf("preview missing entry %s:\n%s", name, out)
		}
	}

	// The archive on disk is a real tar.gz with exactly the documented
	// entries, flat names, and a manifest that hashes every other entry.
	entries := readTarGZ(t, outPath)
	for _, name := range []string{"manifest.json", "doctor.json", "store.json", "bottlenecks.json", "config.redacted.json", "dev-trace.json"} {
		if _, ok := entries[name]; !ok {
			t.Errorf("archive missing %s (has %v)", name, namesOf(entries))
		}
	}
	if len(entries) != 6 {
		t.Errorf("archive has %d entries, want 6: %v", len(entries), namesOf(entries))
	}

	var man struct {
		Version    string `json:"version"`
		Commit     string `json:"commit"`
		GoVersion  string `json:"go_version"`
		OS         string `json:"os"`
		Arch       string `json:"arch"`
		CreatedAt  string `json:"created_at"`
		DataDir    string `json:"data_dir"`
		DoctorMode string `json:"doctor_mode"`
		Files      []struct {
			Path   string `json:"path"`
			SHA256 string `json:"sha256"`
			Bytes  int64  `json:"bytes"`
		} `json:"files"`
	}
	if err := json.Unmarshal(entries["manifest.json"], &man); err != nil {
		t.Fatalf("manifest.json does not decode: %v", err)
	}
	if man.Version == "" || man.CreatedAt == "" || man.DoctorMode != "offline" {
		t.Errorf("manifest = %+v, want version/created_at and offline doctor mode", man)
	}
	if man.DataDir != filepath.Base(dataDir) || strings.ContainsRune(man.DataDir, '/') {
		t.Errorf("manifest data_dir = %q, want the basename %q (no path)", man.DataDir, filepath.Base(dataDir))
	}
	if len(man.Files) != 5 {
		t.Fatalf("manifest lists %d files, want 5", len(man.Files))
	}
	for _, f := range man.Files {
		data, ok := entries[f.Path]
		if !ok {
			t.Errorf("manifest lists missing entry %s", f.Path)
			continue
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != f.SHA256 {
			t.Errorf("manifest sha256 for %s = %s, actual %s", f.Path, f.SHA256, hex.EncodeToString(sum[:]))
		}
		if f.Bytes != int64(len(data)) {
			t.Errorf("manifest bytes for %s = %d, actual %d", f.Path, f.Bytes, len(data))
		}
	}

	var storeDoc struct {
		SchemaVersion int              `json:"schema_version"`
		TableCounts   map[string]int64 `json:"table_counts"`
	}
	if err := json.Unmarshal(entries["store.json"], &storeDoc); err != nil {
		t.Fatalf("store.json does not decode: %v", err)
	}
	if storeDoc.SchemaVersion <= 0 || len(storeDoc.TableCounts) == 0 {
		t.Errorf("store.json = %+v, want a schema version and table counts", storeDoc)
	}

	// bottlenecks.json (ADR 0069): valid JSON holding stage names, sample
	// counts and latency figures only (internal/support/guard_test.go is the
	// content guard that proves seeded dialogue never survives; a fresh data
	// dir has no finished sessions, so the rows must be empty). Neither the
	// archive entry nor any field may carry a path: no "/" anywhere outside
	// the note's "p50/p95/max" shorthand, and no data-dir component inside
	// the document.
	bnRaw := string(entries["bottlenecks.json"])
	var bn struct {
		Note     string `json:"note"`
		Sessions []struct {
			SessionID  string `json:"session_id"`
			Event      string `json:"event"`
			OccurredAt string `json:"occurred_at"`
			Stages     []struct {
				Stage string `json:"stage"`
				Count uint64 `json:"count"`
				P50NS int64  `json:"p50_ns"`
				P95NS int64  `json:"p95_ns"`
				MaxNS int64  `json:"max_ns"`
			} `json:"stages"`
			SlowestStage string `json:"slowest_stage"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(entries["bottlenecks.json"], &bn); err != nil {
		t.Fatalf("bottlenecks.json does not decode: %v", err)
	}
	if bn.Note == "" {
		t.Errorf("bottlenecks.json note = %q, want the section's privacy note", bn.Note)
	}
	if len(bn.Sessions) != 0 {
		t.Errorf("bottlenecks.json has %d sessions, want 0 for a fresh data dir", len(bn.Sessions))
	}
	notePathless := strings.ReplaceAll(bnRaw, bn.Note, "")
	if strings.Contains(notePathless, "/") {
		t.Errorf("bottlenecks.json carries a path outside the note: %q", bnRaw)
	}
	for _, s := range []string{filepath.Base(dataDir)} {
		if strings.Contains(notePathless, s) {
			t.Errorf("bottlenecks.json contains the data-dir component %q", s)
		}
	}

	// The default output location is the current directory: run without
	// --out from a scratch cwd and check the file appears there.
	cwd := t.TempDir()
	cmd := exec.Command(binPath, "support-bundle", "--data-dir", dataDir)
	cmd.Dir = cwd
	if err := cmd.Run(); err != nil {
		t.Fatalf("support-bundle without --out: %v", err)
	}
	dirEntries, err := os.ReadDir(cwd)
	if err != nil {
		t.Fatal(err)
	}
	var defaultOut string
	for _, e := range dirEntries {
		if strings.HasPrefix(e.Name(), "game-narrator-support-") && strings.HasSuffix(e.Name(), ".tar.gz") {
			defaultOut = e.Name()
		}
	}
	if defaultOut == "" {
		t.Error("default --out did not land in the current directory")
	}
}

func TestRunSupportBundleUsageAndErrors(t *testing.T) {
	binPath := buildBinary(t, t.TempDir())

	// Unknown flag: usage, exit 2.
	code, _, errOut := runBinary(t, binPath, "support-bundle", "--data-dir", t.TempDir(), "--nope")
	if code != 2 || !strings.Contains(errOut, "support-bundle") {
		t.Errorf("unknown flag: exit = %d, stderr %q; want 2 with usage", code, errOut)
	}

	// No database in the data dir: actionable error, exit 1.
	code, _, errOut = runBinary(t, binPath, "support-bundle", "--data-dir", t.TempDir(), "--out", filepath.Join(t.TempDir(), "x.tar.gz"))
	if code != 1 {
		t.Errorf("missing database: exit = %d, stderr %q; want 1", code, errOut)
	}
	if !strings.Contains(errOut, "migrate") {
		t.Errorf("missing-database error does not point at migrate: %q", errOut)
	}

	// help lists the command.
	code, out, _ := runBinary(t, binPath, "help")
	if code != 0 || !strings.Contains(out, "support-bundle --data-dir DIR") {
		t.Errorf("help does not document support-bundle (exit %d)", code)
	}
}

// readTarGZ decodes a tar.gz into name→bytes; every entry must be a flat
// regular file.
func readTarGZ(t *testing.T, path string) map[string][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	out := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		if hdr.Typeflag != tar.TypeReg || strings.Contains(hdr.Name, "/") {
			t.Fatalf("entry %s is type %c, want a flat regular file", hdr.Name, hdr.Typeflag)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read %s: %v", hdr.Name, err)
		}
		out[hdr.Name] = data
	}
	return out
}

func namesOf(entries map[string][]byte) []string {
	out := make([]string, 0, len(entries))
	for k := range entries {
		out = append(out, k)
	}
	return out
}
