package gcdisc

import (
	"bytes"
	"encoding/binary"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- synthetic image builders ----

// synthEntry describes one FST entry for buildSynthDisc.
type synthEntry struct {
	isDir  bool
	name   string
	off    int64 // file: absolute logical disc offset
	size   int64 // file: length
	parent int   // dir: parent entry index
	end    int   // dir: index one past the subtree end
}

// writeImage writes img to a temp file and returns its path.
func writeImage(t *testing.T, img []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "disc.gcm")
	if err := os.WriteFile(p, img, 0o644); err != nil {
		t.Fatalf("write disc image: %v", err)
	}
	return p
}

// buildSynthDisc assembles a plain GCM logical image: boot header with
// the FST at fstOff, the FST entries (root first), the NUL-terminated
// string table right after the entries, and each file's contents at
// its absolute logical offset. It returns the image path.
func buildSynthDisc(t *testing.T, fstOff int64, entries []synthEntry, fileData map[string][]byte) string {
	t.Helper()
	var str bytes.Buffer
	nameOff := make(map[string]int64, len(entries))
	for _, e := range entries {
		if e.name == "" {
			continue
		}
		nameOff[e.name] = int64(str.Len())
		str.WriteString(e.name)
		str.WriteByte(0)
	}
	count := len(entries)
	fst := make([]byte, count*fstEntrySize)
	for i, e := range entries {
		b := fst[i*fstEntrySize : (i+1)*fstEntrySize]
		if e.isDir {
			b[0] = 1
		}
		no := nameOff[e.name]
		b[1], b[2], b[3] = byte(no>>16), byte(no>>8), byte(no)
		switch {
		case i == 0: // root: third field is the total entry count
			binary.BigEndian.PutUint32(b[8:12], uint32(count))
		case e.isDir:
			binary.BigEndian.PutUint32(b[4:8], uint32(e.parent))
			binary.BigEndian.PutUint32(b[8:12], uint32(e.end))
		default:
			binary.BigEndian.PutUint32(b[4:8], uint32(e.off))
			binary.BigEndian.PutUint32(b[8:12], uint32(e.size))
		}
	}
	fstLen := int64(count*fstEntrySize) + int64(str.Len())
	imgSize := fstOff + fstLen
	if imgSize < gcmHeaderLen {
		imgSize = gcmHeaderLen
	}
	fileOff := make(map[string]int64, len(fileData))
	for i, e := range entries {
		if i == 0 || e.isDir {
			continue
		}
		data, ok := fileData[e.name]
		if !ok {
			t.Fatalf("missing contents for file %q", e.name)
		}
		if int64(len(data)) > e.size {
			t.Fatalf("contents of %q exceed declared size %d", e.name, e.size)
		}
		fileOff[e.name] = e.off
		if e.off+e.size > imgSize {
			imgSize = e.off + e.size
		}
	}
	img := make([]byte, imgSize)
	copy(img[0:6], "TEST01")
	binary.BigEndian.PutUint32(img[0x424:0x428], uint32(fstOff))
	binary.BigEndian.PutUint32(img[0x428:0x42C], uint32(fstLen))
	copy(img[fstOff:fstOff+int64(count*fstEntrySize)], fst)
	copy(img[fstOff+int64(count*fstEntrySize):], str.Bytes())
	for name, data := range fileData {
		copy(img[fileOff[name]:], data)
	}
	return writeImage(t, img)
}

// ---- tests ----

func TestFSTSynthetic(t *testing.T) {
	const fstOff = 0x2000
	//  0 root dir    (children 1..8)
	//  1 rootfile    (file, first child of root)
	//  2 dirA        (subtree 3..6)
	//  3 a1          (file in dirA)
	//  4 sub         (nested dir in dirA, subtree 5..5)
	//  5 nested      (file in sub)
	//  6 a2          (file in dirA after the nested subtree)
	//  7 dirB        (subtree 8..8)
	//  8 b1          (file in dirB)
	entries := []synthEntry{
		{isDir: true, end: 9},
		{name: "rootfile", off: 0x3000, size: 5},
		{isDir: true, name: "dirA", parent: 0, end: 7},
		{name: "a1", off: 0x4000, size: 4},
		{isDir: true, name: "sub", parent: 2, end: 6},
		{name: "nested", off: 0x5000, size: 3},
		{name: "a2", off: 0x6000, size: 6},
		{isDir: true, name: "dirB", parent: 0, end: 9},
		{name: "b1", off: 0x7000, size: 2},
	}
	fileData := map[string][]byte{
		"rootfile": []byte("hello"),
		"a1":       []byte("abcd"),
		"nested":   []byte("xyz"),
		"a2":       []byte("123456"),
		"b1":       []byte("ok"),
	}
	path := buildSynthDisc(t, fstOff, entries, fileData)

	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	fsys, err := d.FS()
	if err != nil {
		t.Fatalf("FS: %v", err)
	}
	statFS, ok := fsys.(fs.StatFS)
	if !ok {
		t.Fatalf("FS does not implement fs.StatFS")
	}

	// Root listing: the file first child plus both directories.
	root, err := fs.ReadDir(fsys, ".")
	if err != nil {
		t.Fatalf("ReadDir(.): %v", err)
	}
	var rootNames []string
	for _, e := range root {
		rootNames = append(rootNames, e.Name())
	}
	if got := strings.Join(rootNames, ","); got != "dirA,dirB,rootfile" {
		t.Fatalf("root = %q, want \"dirA,dirB,rootfile\"", got)
	}

	// dirA listing: files before and after the nested subtree.
	da, err := fs.ReadDir(fsys, "dirA")
	if err != nil {
		t.Fatalf("ReadDir(dirA): %v", err)
	}
	var aNames []string
	for _, e := range da {
		aNames = append(aNames, e.Name())
	}
	if got := strings.Join(aNames, ","); got != "a1,a2,sub" {
		t.Fatalf("dirA = %q, want \"a1,a2,sub\"", got)
	}

	// Sizes via Stat.
	for p, size := range map[string]int64{
		"rootfile":        5,
		"dirA/a1":         4,
		"dirA/sub/nested": 3,
		"dirA/a2":         6,
		"dirB/b1":         2,
	} {
		info, err := statFS.Stat(p)
		if err != nil {
			t.Fatalf("Stat(%s): %v", p, err)
		}
		if info.Size() != size {
			t.Errorf("Stat(%s).Size() = %d, want %d", p, info.Size(), size)
		}
		if info.IsDir() {
			t.Errorf("Stat(%s).IsDir() = true, want false", p)
		}
	}
	if info, err := statFS.Stat("dirA/sub"); err != nil || !info.IsDir() {
		t.Errorf("Stat(dirA/sub): err=%v isDir=%v, want dir", err, info.IsDir())
	}

	// Contents read back through the fs.FS.
	for p, want := range map[string]string{
		"rootfile":        "hello",
		"dirA/a1":         "abcd",
		"dirA/sub/nested": "xyz",
		"dirA/a2":         "123456",
		"dirB/b1":         "ok",
	} {
		got, err := fs.ReadFile(fsys, p)
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", p, err)
		}
		if string(got) != want {
			t.Errorf("ReadFile(%s) = %q, want %q", p, got, want)
		}
	}

	// A full walk sees exactly the five files.
	seen := map[string]bool{}
	err = fs.WalkDir(fsys, ".", func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !de.IsDir() {
			seen[p] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}
	if len(seen) != 5 {
		t.Fatalf("walk saw %d files: %v, want 5", len(seen), seen)
	}
}

// rawFST builds a plain GCM image whose FST region (at fstOff, declared
// size fstSize) is exactly the given raw bytes.
func rawFST(t *testing.T, fstOff int64, fstSize uint32, fst []byte) string {
	t.Helper()
	img := make([]byte, 0x4000)
	copy(img[0:6], "TEST01")
	binary.BigEndian.PutUint32(img[0x424:0x428], uint32(fstOff))
	binary.BigEndian.PutUint32(img[0x428:0x42C], fstSize)
	copy(img[fstOff:], fst)
	return writeImage(t, img)
}

func TestFSTHostile(t *testing.T) {
	const fstOff = int64(0x1000)

	t.Run("huge entry count", func(t *testing.T) {
		// Root entry claims 1 Mi entries, far past the 200000 bound.
		fst := make([]byte, fstEntrySize)
		fst[0] = 1
		binary.BigEndian.PutUint32(fst[8:12], 1<<20)
		d, err := Open(rawFST(t, fstOff, 0x1000, fst))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer d.Close()
		if _, err := d.FS(); err == nil || !strings.Contains(err.Error(), "implausible") {
			t.Fatalf("FS() = %v, want implausible count error", err)
		}
	})

	t.Run("name offset outside string table", func(t *testing.T) {
		// Two entries; the string table is 5 bytes ("file\0") but the
		// file's name offset is 0x7FFF.
		fst := make([]byte, 2*fstEntrySize+5)
		fst[0] = 1
		binary.BigEndian.PutUint32(fst[8:12], 2)
		fst[13], fst[14], fst[15] = 0, 0x7F, 0xFF
		copy(fst[2*fstEntrySize:], "file\x00")
		d, err := Open(rawFST(t, fstOff, 2*fstEntrySize+5, fst))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer d.Close()
		if _, err := d.FS(); err == nil || !strings.Contains(err.Error(), "string table") {
			t.Fatalf("FS() = %v, want string-table error", err)
		}
	})

	t.Run("file beyond disc", func(t *testing.T) {
		// One file claiming 0x8000..0xC000 on a 0x4000-byte disc.
		fst := make([]byte, 2*fstEntrySize+5)
		fst[0] = 1
		binary.BigEndian.PutUint32(fst[8:12], 2)
		binary.BigEndian.PutUint32(fst[16:20], 0x8000) // file offset
		binary.BigEndian.PutUint32(fst[20:24], 0x4000) // file size
		copy(fst[2*fstEntrySize:], "file\x00")
		d, err := Open(rawFST(t, fstOff, 2*fstEntrySize+5, fst))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer d.Close()
		if _, err := d.FS(); err == nil || !strings.Contains(err.Error(), "disc size") {
			t.Fatalf("FS() = %v, want beyond-disc-size error", err)
		}
	})
}

func TestFSTRealImage(t *testing.T) {
	path := os.Getenv("GAME_NARRATOR_GAME_IMAGE_PATH")
	if path == "" {
		t.Skip("NOT RUN: GAME_NARRATOR_GAME_IMAGE_PATH not set")
	}
	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	if got := d.Header().GameID; got != "GZLE01" {
		t.Fatalf("game ID = %q, want GZLE01", got)
	}
	fsys, err := d.FS()
	if err != nil {
		t.Fatalf("FS: %v", err)
	}
	statFS, ok := fsys.(fs.StatFS)
	if !ok {
		t.Fatalf("FS does not implement fs.StatFS")
	}
	fileCount := 0
	err = fs.WalkDir(fsys, ".", func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !de.IsDir() {
			fileCount++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}
	if fileCount <= 1000 {
		t.Fatalf("walk found %d files, want more than 1000", fileCount)
	}
	info, err := statFS.Stat("res/Msg/bmgres.arc")
	if err != nil {
		t.Fatalf("Stat(res/Msg/bmgres.arc): %v", err)
	}
	if info.Size() <= 0 {
		t.Fatalf("res/Msg/bmgres.arc size = %d, want > 0", info.Size())
	}
	t.Logf("files=%d bmgres.size=%d", fileCount, info.Size())
}
