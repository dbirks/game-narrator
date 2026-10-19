package support

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/store"
)

// The distinctive strings the content guard proves do not survive the
// bundle. They are invented (no game text, AGENTS.md rule 5) but distinctive
// enough that a substring of one is as good as the whole line.
const (
	guardDialogueOne = "MANGO LANTERN QUARTZ THE TIDE WHISPERS"
	guardDialogueTwo = "THE LANTERN QUARTZ HUMS A HIDDEN CHORD"
)

// seedStep keeps the seed function compact: one line per store operation.
func seedStep(t *testing.T, op string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", op, err)
	}
}

// seedGuardDataDir creates a data dir the way a used one would look: a
// migrated store holding a synthetic profile with distinctive dialogue, a
// dev-trace session with a JPEG-magic blob, an asset store with a PNG-magic
// and a WAV-magic blob, and a filesink WAV. Returns the data dir and the
// seeded binary content (keyed by kind).
func seedGuardDataDir(t *testing.T) (string, map[string][]byte) {
	t.Helper()
	dataDir := t.TempDir()
	ctx := context.Background()

	st, err := store.Open(ctx, store.DBPath(dataDir))
	seedStep(t, "open store", err)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	game := games.Game{
		ID: games.NewID(), Slug: "synthetic-tide", Title: "Synthetic Tide",
		Platform: "gamecube", UserTags: []byte(`[]`), CreatedAt: now,
	}
	seedStep(t, "CreateGame", st.CreateGame(ctx, game))
	edition := games.Edition{
		ID: games.NewID(), GameID: game.ID, Region: "US", Language: "en",
		Revision: "rev0", Locale: "en_US",
	}
	seedStep(t, "CreateEdition", st.CreateEdition(ctx, edition))
	profile, err := st.CreateDraftProfile(ctx, edition.ID)
	seedStep(t, "CreateDraftProfile", err)
	msg := games.Message{
		ID:                games.NewID(),
		ProfileID:         profile.ID,
		OriginalText:      guardDialogueOne,
		NormalizedText:    strings.ToLower(guardDialogueOne),
		SpeechText:        guardDialogueOne,
		Language:          "en",
		TextKind:          games.TextDialogue,
		AttributionStatus: games.AttributionUnknown,
		Revision:          1,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	pages := []games.MessagePage{{MessageID: msg.ID, Ordinal: 1, PageText: guardDialogueTwo}}
	seedStep(t, "PutMessages", st.PutMessages(ctx, profile.ID, []games.Message{msg}, pages))
	st.Close()

	// Binary content that must never reach the archive: JPEG-magic (dev-trace
	// frame), PNG-magic (crop), WAV-magic (audio), in the layout the runtime
	// actually uses.
	jpeg := append([]byte{0xff, 0xd8, 0xff, 0xe0}, []byte("synthetic frame pixels "+guardDialogueOne)...)
	png := append([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, []byte("synthetic crop "+guardDialogueTwo)...)
	wav := []byte("RIFF\x00\x00\x00\x00WAVEfmt ")
	wav = append(wav, []byte("synthetic voice line "+guardDialogueTwo)...)

	writeHashedBlob(t, filepath.Join(dataDir, "dev-trace", "sess-guard-1", "blobs"), jpeg)
	writeHashedBlob(t, filepath.Join(dataDir, "assets", "blobs"), png)
	writeHashedBlob(t, filepath.Join(dataDir, "assets", "blobs"), wav)
	if err := os.MkdirAll(filepath.Join(dataDir, "audio"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "audio", "line-0001.wav"), wav, 0o600); err != nil {
		t.Fatal(err)
	}
	return dataDir, map[string][]byte{"jpeg": jpeg, "png": png, "wav": wav}
}

// writeHashedBlob stores data at <root>/<h[:2]>/<h[2:4]>/<h>, the layout of
// the content-addressed asset store, so the dev-trace walker really meets the
// production shape.
func writeHashedBlob(t *testing.T, root string, data []byte) string {
	t.Helper()
	sum := sha256.Sum256(data)
	h := hex.EncodeToString(sum[:])
	dir := filepath.Join(root, h[:2], h[2:4])
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, h), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return h
}

// extractArchive reads a tar.gz into a name→bytes map. Every entry must be a
// flat regular file: the documented archive layout has no directories.
func extractArchive(t *testing.T, path string) map[string][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open archive: %v", err)
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
			t.Fatalf("read tar entry: %v", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			t.Fatalf("entry %s is type %c, want a regular file", hdr.Name, hdr.Typeflag)
		}
		if strings.Contains(hdr.Name, "/") {
			t.Fatalf("entry %s is not a flat name", hdr.Name)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read entry %s: %v", hdr.Name, err)
		}
		out[hdr.Name] = data
	}
	return out
}

func mustJSON(t *testing.T, data []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("decode %T: %v", v, err)
	}
}

// containsWAVHeader reports whether data holds a RIFF/WAVE header: "RIFF" +
// a 4-byte size field + "WAVE".
func containsWAVHeader(data []byte) bool {
	for i := 0; i+12 <= len(data); i++ {
		if string(data[i:i+4]) == "RIFF" && string(data[i+8:i+12]) == "WAVE" {
			return true
		}
	}
	return false
}

func entryNames(entries map[string][]byte) []string {
	out := make([]string, 0, len(entries))
	for k := range entries {
		out = append(out, k)
	}
	return out
}

// TestBuildBundleContentGuard is the privacy gate (docs/spec/13, AGENTS.md
// rule 5): a data dir seeded with a synthetic profile containing distinctive
// dialogue, plus frame/crop/audio binaries, must produce a bundle in which
// none of that content — and no WAV/PNG/JPEG magic bytes — appears anywhere
// in the archive.
func TestBuildBundleContentGuard(t *testing.T) {
	dataDir, binaries := seedGuardDataDir(t)
	out := filepath.Join(t.TempDir(), "bundle.tar.gz")

	man, err := Build(context.Background(), Options{
		DataDir:         dataDir,
		Since:           24 * time.Hour,
		IncludeDevTrace: true,
		Now:             func() time.Time { return time.Date(2026, 1, 7, 12, 0, 0, 0, time.UTC) },
	}, out)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	entries := extractArchive(t, out)

	// 1. The archive holds exactly the documented sections and nothing else
	// — no database file, no assets, no dev-trace bytes.
	wantEntries := []string{ManifestPath, DoctorPath, StorePath, BottlenecksPath, ConfigPath, DevTracePath}
	if len(entries) != len(wantEntries) {
		t.Fatalf("archive has %d entries, want %d: %v", len(entries), len(wantEntries), entryNames(entries))
	}
	for _, name := range wantEntries {
		if _, ok := entries[name]; !ok {
			t.Fatalf("archive missing %s (has %v)", name, entryNames(entries))
		}
	}

	// 2. None of the seeded dialogue appears anywhere in the archive.
	var all strings.Builder
	for _, name := range entryNames(entries) {
		all.WriteString(string(entries[name]))
		all.WriteByte(0x1f) // entry separator, never in JSON payloads
	}
	for _, s := range []string{guardDialogueOne, guardDialogueTwo, "synthetic frame pixels", "synthetic crop", "synthetic voice line"} {
		if strings.Contains(all.String(), s) {
			t.Errorf("archive content contains %q: the content guard failed", s)
		}
	}

	// 3. No image or audio magic bytes survive in any entry.
	for _, name := range entryNames(entries) {
		data := entries[name]
		if bytes.Contains(data, []byte{0x89, 'P', 'N', 'G'}) {
			t.Errorf("entry %s contains PNG magic bytes", name)
		}
		if bytes.Contains(data, []byte{0xff, 0xd8, 0xff}) {
			t.Errorf("entry %s contains JPEG magic bytes", name)
		}
		if containsWAVHeader(data) {
			t.Errorf("entry %s contains a WAV header", name)
		}
	}

	// 4. The manifest is honest: basename only, real hashes, real sizes.
	if man.DataDir != filepath.Base(dataDir) || strings.ContainsRune(man.DataDir, '/') {
		t.Errorf("manifest data_dir = %q, want the basename %q (no path)", man.DataDir, filepath.Base(dataDir))
	}
	if man.DoctorMode != DoctorModeOffline {
		t.Errorf("doctor mode = %q, want %q", man.DoctorMode, DoctorModeOffline)
	}
	var manEntry Manifest
	mustJSON(t, entries[ManifestPath], &manEntry)
	if len(manEntry.Files) != len(wantEntries)-1 {
		t.Fatalf("manifest lists %d files, want %d", len(manEntry.Files), len(wantEntries)-1)
	}
	for _, mf := range manEntry.Files {
		data, ok := entries[mf.Path]
		if !ok {
			t.Errorf("manifest lists missing entry %s", mf.Path)
			continue
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != mf.SHA256 {
			t.Errorf("manifest sha256 for %s = %s, actual %s", mf.Path, mf.SHA256, hex.EncodeToString(sum[:]))
		}
		if mf.Bytes != int64(len(data)) {
			t.Errorf("manifest size for %s = %d, actual %d", mf.Path, mf.Bytes, len(data))
		}
	}

	// 5. The store section reports real counts, no content.
	var storeDoc storeDoc
	mustJSON(t, entries[StorePath], &storeDoc)
	if storeDoc.SchemaVersion != store.LatestSchemaVersion() {
		t.Errorf("schema version = %d, want %d", storeDoc.SchemaVersion, store.LatestSchemaVersion())
	}
	if n := storeDoc.TableCounts["messages"]; n < 1 {
		t.Errorf("table_counts[messages] = %d, want >= 1 (seeded row)", n)
	}
	if n := storeDoc.TableCounts["message_pages"]; n < 1 {
		t.Errorf("table_counts[message_pages] = %d, want >= 1 (seeded page)", n)
	}

	// 6. The dev-trace section carries the blob hash and size only.
	var dt devTraceDoc
	mustJSON(t, entries[DevTracePath], &dt)
	if len(dt.Sessions) != 1 || dt.Sessions[0].ID != "sess-guard-1" {
		t.Fatalf("dev-trace sessions = %+v, want one session sess-guard-1", dt.Sessions)
	}
	jpegSHA := sha256.Sum256(binaries["jpeg"])
	for _, b := range dt.Sessions[0].Blobs {
		if b.SHA256 == hex.EncodeToString(jpegSHA[:]) && b.Bytes == int64(len(binaries["jpeg"])) {
			return
		}
	}
	t.Errorf("dev-trace blobs = %+v, want the seeded jpeg hash with its size", dt.Sessions[0].Blobs)
}
