package clips_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/clips"
)

// writeClipDir lays a minimal clip out on disk for the index tests.
func writeClipDir(t *testing.T, root, id string) {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	files := layoutFiles(t)
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIndexLoadSaveRoundTrip(t *testing.T) {
	root := t.TempDir()
	// No index file is the empty index, not an error.
	idx, err := clips.Load(root)
	if err != nil || len(idx.Records) != 0 {
		t.Fatalf("empty store: %v, %d records; want the empty index", err, len(idx.Records))
	}
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	rec := clips.Record{
		ID: "clip-a", Name: "clip a", SHA256: strings.Repeat("ab", 32),
		Size: 42, Frames: 2, Format: "tar", CreatedAt: now, CreatedBy: "dev",
	}
	if err := clips.Update(root, func(i *clips.Index) error {
		i.Records = append(i.Records, rec)
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	idx, err = clips.Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(idx.Records) != 1 || idx.Records[0].ID != "clip-a" || idx.Records[0].SHA256 != rec.SHA256 {
		t.Fatalf("round trip = %+v", idx.Records)
	}
	got, ok := idx.Find("clip-a")
	if !ok || got.Size != 42 {
		t.Fatalf("Find: %v %d", got, got.Size)
	}
	if _, ok := idx.Find("nope"); ok {
		t.Fatal("Find(nope) = found, want absent")
	}
	// The file is private: an index carrying provenance metadata is appliance
	// state, not world-readable content.
	fi, err := os.Stat(filepath.Join(root, clips.IndexFileName))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("index mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestIndexSortsAndUpdateReplaces(t *testing.T) {
	root := t.TempDir()
	mk := func(id string) clips.Record { return clips.Record{ID: id, CreatedAt: time.Now().UTC()} }
	if err := clips.Update(root, func(i *clips.Index) error {
		i.Records = append(i.Records, mk("zeta"), mk("alpha"))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	idx, _ := clips.Load(root)
	if idx.Records[0].ID != "alpha" || idx.Records[1].ID != "zeta" {
		t.Fatalf("order = %v %v, want sorted", idx.Records[0].ID, idx.Records[1].ID)
	}
	if err := clips.Update(root, func(i *clips.Index) error {
		out := i.Records[:0]
		for _, r := range i.Records {
			if r.ID != "zeta" {
				out = append(out, r)
			}
		}
		i.Records = out
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	idx, _ = clips.Load(root)
	if len(idx.Records) != 1 || idx.Records[0].ID != "alpha" {
		t.Fatalf("after replace = %+v", idx.Records)
	}
}

func TestIndexCorruptIsAError(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, clips.IndexFileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := clips.Load(root)
	if err == nil {
		t.Fatal("Load on a corrupt index = nil, want an error")
	}
	if !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("err = %v, want the corrupt-index signal", err)
	}
}

func TestPruneStagingRemovesOnlyItsOwnPrefix(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{".staging-abc", ".staging-def", "clip-a", clips.IndexFileName, "random.tmp-1"} {
		p := filepath.Join(root, name)
		if name == clips.IndexFileName {
			if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	n, err := clips.PruneStaging(root)
	if err != nil {
		t.Fatalf("PruneStaging: %v", err)
	}
	if n != 2 {
		t.Fatalf("removed = %d, want 2", n)
	}
	if _, err := os.Stat(filepath.Join(root, "clip-a")); err != nil {
		t.Fatalf("clip-a was removed: %v", err)
	}
}

func TestValidateDirRefusals(t *testing.T) {
	mk := func(t *testing.T, files map[string][]byte) string {
		dir := t.TempDir()
		for name, data := range files {
			p := filepath.Join(dir, name)
			if strings.Contains(name, string(filepath.Separator)) {
				if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(p, data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	manifest := `[{"file":"a.png","ts_ns":1}]`

	t.Run("missing manifest", func(t *testing.T) {
		_, err := clips.ValidateDir(mk(t, map[string][]byte{"a.png": framePNG(t)}))
		if err == nil || !strings.Contains(err.Error(), "timestamps.json") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("empty manifest", func(t *testing.T) {
		_, err := clips.ValidateDir(mk(t, map[string][]byte{"timestamps.json": []byte("[]")}))
		if err == nil || !strings.Contains(err.Error(), "no frames") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("traversal entry", func(t *testing.T) {
		_, err := clips.ValidateDir(mk(t, map[string][]byte{"timestamps.json": []byte(`[{"file":"../etc/passwd","ts_ns":1}]`)}))
		if err == nil || !strings.Contains(err.Error(), "unsafe") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("nonpositive timestamp", func(t *testing.T) {
		_, err := clips.ValidateDir(mk(t, map[string][]byte{"timestamps.json": []byte(`[{"file":"a.png","ts_ns":0}]`)}))
		if err == nil || !strings.Contains(err.Error(), "timestamp") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("missing frame", func(t *testing.T) {
		_, err := clips.ValidateDir(mk(t, map[string][]byte{"timestamps.json": []byte(manifest)}))
		if err == nil || !strings.Contains(err.Error(), "not in the clip") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("frame is not an image", func(t *testing.T) {
		_, err := clips.ValidateDir(mk(t, map[string][]byte{
			"timestamps.json": []byte(manifest),
			"a.png":           []byte("definitely not a png"),
		}))
		if err == nil || !strings.Contains(err.Error(), "does not decode") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("good clip", func(t *testing.T) {
		dir := mk(t, layoutFiles(t))
		clip, err := clips.ValidateDir(dir)
		if err != nil {
			t.Fatalf("ValidateDir: %v", err)
		}
		if clip.Frames != 2 {
			t.Fatalf("Frames = %d", clip.Frames)
		}
	})
}
