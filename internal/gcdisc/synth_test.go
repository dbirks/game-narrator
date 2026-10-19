package gcdisc

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// The synthetic builders (ADR-0054/0066) are gated by round-trip: what they
// emit must be what Open/FS/OpenRARC read back, with the exact bytes.

func TestSynthGCMImageRoundTrip(t *testing.T) {
	//  0 root          (children 1..4)
	//  1 rootfile      (file)
	//  2 res           (subtree 3..4)
	//  3 arc           (file in res)
	//  4 nested.txt    (file in res after the arc)
	entries := []SynthEntry{
		{IsDir: true, End: 5},
		{Name: "rootfile", Off: 0x3000, Size: 5},
		{IsDir: true, Name: "res", Parent: 0, End: 5},
		{Name: "arc", Off: 0x4000, Size: 10},
		{Name: "nested.txt", Off: 0x5000, Size: 3},
	}
	arcBytes := []byte("RARC-bytes")
	files := map[string][]byte{
		"rootfile":   []byte("hello"),
		"arc":        arcBytes,
		"nested.txt": []byte("xyz"),
	}
	img := SynthGCMImage("SYNT01", 0x2000, entries, files)

	dir := t.TempDir()
	p := filepath.Join(dir, "disc.gcm")
	if err := os.WriteFile(p, img, 0o644); err != nil {
		t.Fatalf("write image: %v", err)
	}
	d, err := Open(p)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	if got := d.Header().GameID; got != "SYNT01" {
		t.Fatalf("game id = %q, want SYNT01", got)
	}
	fsys, err := d.FS()
	if err != nil {
		t.Fatalf("FS: %v", err)
	}
	for name, want := range map[string]string{
		"rootfile":       "hello",
		"res/arc":        "RARC-bytes",
		"res/nested.txt": "xyz", // the FST names the entry; the data is what was stored
	} {
		got, err := fs.ReadFile(fsys, name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestAssembleRARCRoundTrip(t *testing.T) {
	one := []byte("one-line-of-bytes")
	two := []byte("two")
	deep := []byte("deep content")
	arc := AssembleRARC(map[string][]byte{
		"a.txt":        one,
		"bmg/bmg0.bmg": two,
		"x/y/z/deep":   deep,
	})
	arcFS, err := OpenRARC(arc)
	if err != nil {
		t.Fatalf("OpenRARC: %v", err)
	}
	for name, want := range map[string][]byte{
		"a.txt":        one,
		"bmg/bmg0.bmg": two,
		"x/y/z/deep":   deep,
	} {
		got, err := fs.ReadFile(arcFS, name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != string(want) {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	// The flat entry is readable at the root, and the directory view of the
	// nested tree lists what was stored.
	root, err := fs.ReadDir(arcFS, ".")
	if err != nil {
		t.Fatalf("ReadDir(.): %v", err)
	}
	if len(root) != 3 {
		t.Fatalf("root has %d entries, want 3", len(root))
	}
}

func TestRegionOfGameID(t *testing.T) {
	for id, want := range map[string]string{
		"GZLE01": "ntsc-u",
		"GZLJ01": "ntsc-j",
		"GZLP01": "pal",
		"GZLX01": "unknown",
		"ABC":    "unknown",
		"":       "unknown",
	} {
		if got := RegionOfGameID(id); got != want {
			t.Errorf("RegionOfGameID(%q) = %q, want %q", id, got, want)
		}
	}
}
