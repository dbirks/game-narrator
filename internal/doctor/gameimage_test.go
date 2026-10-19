package doctor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The game.image identity tests. The image file is the user's private secret
// (AGENTS.md rule 5): every test asserts the report names the expectation
// (env var, file kind, SHA-256 prefix, the gates that need the image) and
// never leaks the path.

// writeFixtureManifest writes one fixture manifest recording imageSHA as
// game_image_sha256 under root/<id>/manifest.json.
func writeFixtureManifest(t *testing.T, root, id, imageSHA string) {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	man := map[string]any{"schema": 2, "id": id, "game_image_sha256": imageSHA}
	raw, err := json.Marshal(man)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func sha256Of(t *testing.T, b []byte) string {
	t.Helper()
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func gameImageText(c Check) string {
	return c.Summary + "\n" + c.Detail + "\n" + c.Remedy
}

// TestGameImageUnsetIsNotConfigured is the fresh-host contract (acceptance
// gap 8): the report must say exactly what to configure — the env var, the
// expected file kind, how to verify it (the manifest SHA-256 prefix, read at
// run time) and which gates need it.
func TestGameImageUnsetIsNotConfigured(t *testing.T) {
	fixtures := t.TempDir()
	const wantSHA = "677726191ba8cc0829e9baa731c14eeff95ba399e1fe40a4f2473e7b8e0c80ac"
	writeFixtureManifest(t, fixtures, "windwaker-newgame", wantSHA)
	writeFixtureManifest(t, fixtures, "windwaker-legend-dense", wantSHA)

	h := newFakeHost(t)
	h.opts.Dev = true
	h.opts.FixtureRoot = func() string { return fixtures }
	c := h.check(h.run(), "game.image")
	mustStatus(t, c, StatusNotConfigured)
	text := gameImageText(c)
	for _, want := range []string{
		EnvGameImagePath,
		"CISO/ISO",
		"verify the image: its SHA-256 must start with 677726191ba8",
		"2 fixture manifests",
		"make bench",
		"make test-dolphin",
		"import-disc",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("unset report missing %q:\n%s", want, text)
		}
	}
	if !strings.Contains(text, "is not set") {
		t.Fatalf("unset report must say the variable is not set:\n%s", text)
	}
}

// TestGameImageUnsetWithoutManifests is still actionable: the verification
// clause points at the manifests (absent here) rather than inventing a hash.
func TestGameImageUnsetWithoutManifests(t *testing.T) {
	h := newFakeHost(t)
	h.opts.Dev = true
	h.opts.FixtureRoot = func() string { return t.TempDir() }
	c := h.check(h.run(), "game.image")
	mustStatus(t, c, StatusNotConfigured)
	text := gameImageText(c)
	if !strings.Contains(text, "none present here") {
		t.Fatalf("no-manifest report must say none are present:\n%s", text)
	}
	if strings.Contains(text, "SHA-256 must start with") {
		t.Fatalf("no-manifest report must not claim a prefix:\n%s", text)
	}
}

// TestGameImageMissingFileIsNotConfigured: a set-but-wrong path is a NOT
// CONFIGURED with the same actionable steps (the configured identity is
// still missing), not a cryptic FAIL, and never leaks the file name.
func TestGameImageMissingFileIsNotConfigured(t *testing.T) {
	fixtures := t.TempDir()
	writeFixtureManifest(t, fixtures, "windwaker-newgame", strings.Repeat("a", 64))
	h := newFakeHost(t)
	h.opts.Dev = true
	h.opts.FixtureRoot = func() string { return fixtures }
	h.withEnv(EnvGameImagePath, filepath.Join(t.TempDir(), "gone.iso"))
	c := h.check(h.run(), "game.image")
	mustStatus(t, c, StatusNotConfigured)
	text := gameImageText(c)
	for _, want := range []string{"does not exist", EnvGameImagePath, "CISO/ISO", "make test-dolphin"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing-file report missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "gone.iso") || strings.Contains(text, t.TempDir()) {
		t.Fatalf("missing-file report leaked the path:\n%s", text)
	}
}

// TestGameImageDirectoryIsNotConfigured: a directory is not a disc image.
func TestGameImageDirectoryIsNotConfigured(t *testing.T) {
	h := newFakeHost(t)
	h.opts.Dev = true
	h.opts.FixtureRoot = func() string { return t.TempDir() }
	h.withEnv(EnvGameImagePath, t.TempDir())
	c := h.check(h.run(), "game.image")
	mustStatus(t, c, StatusNotConfigured)
	if !strings.Contains(gameImageText(c), "directory") {
		t.Fatalf("directory report must name the fault:\n%s", gameImageText(c))
	}
}

// TestGameImageWrongHashIsFail: the file exists but is not the image the
// fixtures were recorded against — FAIL with both prefixes, never the path.
func TestGameImageWrongHashIsFail(t *testing.T) {
	dir := t.TempDir()
	wrong := filepath.Join(dir, "private-image.ciso")
	if err := os.WriteFile(wrong, []byte("the wrong disc"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixtures := filepath.Join(dir, "fixtures")
	want := sha256Of(t, []byte("the recorded disc"))
	writeFixtureManifest(t, fixtures, "windwaker-newgame", want)

	h := newFakeHost(t)
	h.opts.Dev = true
	h.opts.FixtureRoot = func() string { return fixtures }
	h.withEnv(EnvGameImagePath, wrong)
	c := h.check(h.run(), "game.image")
	mustStatus(t, c, StatusFail)
	text := gameImageText(c)
	got := sha256Of(t, []byte("the wrong disc"))
	if !strings.Contains(text, hashPrefix(want)) {
		t.Fatalf("FAIL report missing the expected prefix %s:\n%s", hashPrefix(want), text)
	}
	if !strings.Contains(text, hashPrefix(got)) {
		t.Fatalf("FAIL report missing the actual prefix %s:\n%s", hashPrefix(got), text)
	}
	// Rule 5: the path (and its parent dir name) must not reach any output.
	if strings.Contains(text, wrong) || strings.Contains(text, dir) {
		t.Fatalf("FAIL report leaked the path:\n%s", text)
	}
	rep := h.run()
	var out strings.Builder
	if err := Format(&out, rep); err != nil {
		t.Fatal(err)
	}
	if err := FormatJSON(&out, rep); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), wrong) {
		t.Fatalf("formatted report leaked the game image path:\n%s", out.String())
	}
}

// TestGameImageMatchingHashIsOK: the configured image is the one the fixtures
// record — OK, with the shared prefix as the evidence.
func TestGameImageMatchingHashIsOK(t *testing.T) {
	dir := t.TempDir()
	image := filepath.Join(dir, "private-image.ciso")
	if err := os.WriteFile(image, []byte("the recorded disc"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixtures := t.TempDir()
	writeFixtureManifest(t, fixtures, "windwaker-newgame", sha256Of(t, []byte("the recorded disc")))
	writeFixtureManifest(t, fixtures, "windwaker-legend-dense", sha256Of(t, []byte("the recorded disc")))

	h := newFakeHost(t)
	h.opts.Dev = true
	h.opts.FixtureRoot = func() string { return fixtures }
	h.withEnv(EnvGameImagePath, image)
	c := h.check(h.run(), "game.image")
	mustStatus(t, c, StatusOK)
	if !strings.Contains(c.Summary, "matches the fixture manifest (SHA-256 prefix "+hashPrefix(sha256Of(t, []byte("the recorded disc")))+")") {
		t.Fatalf("OK summary = %q, want the prefix evidence", c.Summary)
	}
}

// TestGameImageNoManifestsIsOKSizeOnly: with no manifest-recorded identity
// the check still proves existence and size, and says so honestly.
func TestGameImageNoManifestsIsOKSizeOnly(t *testing.T) {
	dir := t.TempDir()
	image := filepath.Join(dir, "private-image.ciso")
	if err := os.WriteFile(image, []byte(strings.Repeat("y", 4096)), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newFakeHost(t)
	h.opts.Dev = true
	h.opts.FixtureRoot = func() string { return t.TempDir() }
	h.withEnv(EnvGameImagePath, image)
	c := h.check(h.run(), "game.image")
	mustStatus(t, c, StatusOK)
	if !strings.Contains(c.Summary, "set, file exists, 4096 bytes") {
		t.Fatalf("summary = %q, want the documented form", c.Summary)
	}
	if !strings.Contains(c.Detail, "not verified against a fixture") {
		t.Fatalf("detail must state the identity is unverified: %q", c.Detail)
	}
}

// TestGameImageHashBudgetExceededIsDegraded: a check whose context is already
// spent (the engine's deadline fired) reports DEGRADED with the reason, never
// OK on an unverified identity and never a hang.
func TestGameImageHashBudgetExceededIsDegraded(t *testing.T) {
	dir := t.TempDir()
	image := filepath.Join(dir, "private-image.ciso")
	if err := os.WriteFile(image, []byte("the recorded disc"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixtures := t.TempDir()
	writeFixtureManifest(t, fixtures, "windwaker-newgame", sha256Of(t, []byte("the recorded disc")))

	h := newFakeHost(t)
	h.opts.Dev = true
	h.opts.FixtureRoot = func() string { return fixtures }
	h.withEnv(EnvGameImagePath, image)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the engine's deadline has fired: the probe context is spent
	defer cancel()
	rep := Run(ctx, h.opts)
	c := h.check(rep, "game.image")
	mustStatus(t, c, StatusDegraded)
	if !strings.Contains(gameImageText(c), "could not be hashed") {
		t.Fatalf("DEGRADED report must name the budget failure:\n%s", gameImageText(c))
	}
}
