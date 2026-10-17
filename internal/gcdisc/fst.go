package gcdisc

import (
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"
	"time"
)

const (
	fstEntrySize = 12
	fstMaxEntry  = 200000 // bound on the declared entry count
	fstMaxName   = 255    // maximum name length in bytes
	fstMaxDepth  = 64     // maximum directory nesting depth
)

// fstFile is a lazily readable file section of the disc.
type fstFile struct {
	name string
	off  int64
	size int64
}

// fstDir is a directory node; Path is "" for the disc root.
type fstDir struct {
	name     string
	path     string
	children []*fstDir
	files    []string // child file names, on-disc order
}

type fstEntry struct {
	isDir      bool
	name       string
	parent     int
	subtreeEnd int
	off, size  int64
}

type fstFS struct {
	d     *Disc
	files map[string]fstFile
	dirs  map[string]*fstDir
}

// FS returns a read-only fs.FS (also fs.ReadDirFS and fs.StatFS) view
// of the disc's FST. Paths are relative to the disc root, e.g.
// "res/Msg/bmgres.arc". File data is read lazily through ReaderAt
// sections; only the FST table is read up front.
func (d *Disc) FS() (fs.FS, error) {
	h := d.Header()
	if h.FSTOffset == 0 {
		return nil, fmt.Errorf("gcdisc: no FST offset in boot header")
	}
	fstOff := int64(h.FSTOffset)
	if fstOff+fstEntrySize > d.Size() {
		return nil, fmt.Errorf("gcdisc: FST offset %x beyond disc size", fstOff)
	}
	first := make([]byte, fstEntrySize)
	if _, err := d.ReadAt(first, fstOff); err != nil {
		return nil, fmt.Errorf("gcdisc: read FST root: %w", err)
	}
	if first[0] != 1 {
		return nil, fmt.Errorf("gcdisc: FST root entry type %d, want dir", first[0])
	}
	count := int(binary.BigEndian.Uint32(first[8:12]))
	if count == 0 || count > fstMaxEntry {
		return nil, fmt.Errorf("gcdisc: implausible FST entry count %d", count)
	}
	tableLen := int64(count) * fstEntrySize
	if fstOff+tableLen > d.Size() {
		return nil, fmt.Errorf("gcdisc: FST entry table of %d entries extends beyond disc size", count)
	}
	// The string table follows the entries and fills the rest of the
	// declared FST region.
	strLen := int64(h.FSTSize) - tableLen
	if strLen < 0 {
		return nil, fmt.Errorf("gcdisc: FST size %d smaller than its %d-byte entry table", h.FSTSize, tableLen)
	}
	tab := make([]byte, tableLen)
	if _, err := d.ReadAt(tab, fstOff); err != nil {
		return nil, fmt.Errorf("gcdisc: read FST table: %w", err)
	}
	f := &fstFS{d: d, files: map[string]fstFile{}, dirs: map[string]*fstDir{}}
	entries, err := f.parseEntries(tab, fstOff+tableLen, strLen)
	if err != nil {
		return nil, err
	}
	root := &fstDir{name: "", path: ""}
	f.dirs[""] = root
	if err := f.addChildren(root, count, 0, 0, entries); err != nil {
		return nil, err
	}
	return f, nil
}

// parseEntries decodes the raw 12-byte FST entries.
//
// Verified layout (big-endian): type(1) | name_offset(3, relative to
// the string table that follows the entries) | u32 a | u32 b.
//   - root entry 0: a=0, b=total entry count
//   - directory i:  a=parent index, b=index just after this subtree
//   - file i:       a=absolute logical disc offset, b=file size
func (f *fstFS) parseEntries(tab []byte, strBase, strLen int64) ([]fstEntry, error) {
	count := len(tab) / fstEntrySize
	entries := make([]fstEntry, count)
	for i := 0; i < count; i++ {
		e := tab[i*fstEntrySize : (i+1)*fstEntrySize]
		switch e[0] {
		case 0, 1:
		default:
			return nil, fmt.Errorf("gcdisc: FST entry %d has unknown type %d", i, e[0])
		}
		ne := fstEntry{isDir: e[0] == 1}
		// The name offset is a 24-bit big-endian value.
		no := int64(e[1])<<16 | int64(e[2])<<8 | int64(e[3])
		a := int64(binary.BigEndian.Uint32(e[4:8]))
		b := int64(binary.BigEndian.Uint32(e[8:12]))
		if i > 0 {
			if ne.isDir {
				ne.parent, ne.subtreeEnd = int(a), int(b)
				if ne.subtreeEnd < i+1 || ne.subtreeEnd > count {
					return nil, fmt.Errorf("gcdisc: FST dir %d has bad subtree end %d", i, ne.subtreeEnd)
				}
			} else {
				// File entries store absolute logical disc offsets.
				ne.off, ne.size = a, b
				if ne.off >= f.d.Size() || ne.off+ne.size > f.d.Size() {
					return nil, fmt.Errorf("gcdisc: FST file %d range [%d, %d) exceeds disc size %d", i, ne.off, ne.off+ne.size, f.d.Size())
				}
			}
		}
		name, err := f.readString(strBase, strLen, no)
		if err != nil {
			return nil, fmt.Errorf("gcdisc: FST entry %d name: %w", i, err)
		}
		ne.name = name
		entries[i] = ne
	}
	return entries, nil
}

// addChildren registers entries [start, end) that belong to dir;
// encloser is the entry index of dir's directory (0 for root) and
// depth is dir's nesting depth (0 for the disc root).
func (f *fstFS) addChildren(dir *fstDir, end, encloser, depth int, entries []fstEntry) error {
	start := 1
	if encloser > 0 {
		start = encloser + 1
	}
	for i := start; i < end; i++ {
		e := entries[i]
		if e.isDir {
			if e.subtreeEnd < i+1 || e.subtreeEnd > end {
				return fmt.Errorf("gcdisc: FST dir %d has bad subtree end %d", i, e.subtreeEnd)
			}
			if e.parent != encloser {
				return fmt.Errorf("gcdisc: FST dir %d has foreign parent %d", i, e.parent)
			}
			if depth+1 > fstMaxDepth {
				return fmt.Errorf("gcdisc: FST directory nesting exceeds depth %d at entry %d", fstMaxDepth, i)
			}
			sub := &fstDir{name: e.name, path: childPath(dir.path, e.name)}
			dir.children = append(dir.children, sub)
			f.dirs[sub.path] = sub
			if err := f.addChildren(sub, e.subtreeEnd, i, depth+1, entries); err != nil {
				return err
			}
			i = e.subtreeEnd - 1 // skip the subtree
		} else {
			name := e.name
			if name == "" || strings.ContainsAny(name, "/\x00") {
				continue
			}
			dir.files = append(dir.files, name)
			f.files[childPath(dir.path, name)] = fstFile{name: name, off: e.off, size: e.size}
		}
	}
	return nil
}

// childPath joins a directory path and a child name, omitting the
// separator at the disc root.
func childPath(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// readString reads the NUL-terminated name at offset off. Offsets are
// relative to the string table, which begins at strBase and is strLen
// bytes long.
func (f *fstFS) readString(strBase, strLen, off int64) (string, error) {
	if off < 0 || off >= strLen {
		return "", fmt.Errorf("string offset %d outside the %d-byte string table", off, strLen)
	}
	max := strLen - off
	if max > fstMaxName+1 {
		max = fstMaxName + 1
	}
	buf := make([]byte, max)
	n, err := f.d.ReadAt(buf, strBase+off)
	if err != nil && err != io.EOF {
		return "", err
	}
	if i := indexByte(buf[:n], 0); i >= 0 {
		if i > fstMaxName {
			return "", fmt.Errorf("name too long (%d)", i)
		}
		return string(buf[:i]), nil
	}
	return "", fmt.Errorf("unterminated name at string offset %d", off)
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

// ---- fs.FS implementation ----

// rootIf maps the fs.FS root "." (or the empty path) to the internal
// root key "".
func (f *fstFS) rootIf(name string) string {
	if name == "" || name == "." {
		return ""
	}
	return name
}

// Open implements fs.FS.
func (f *fstFS) Open(name string) (fs.File, error) {
	name = f.rootIf(name)
	if name != "" && !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	if d, ok := f.dirs[name]; ok {
		return &fstDirFile{dir: d, fs: f}, nil
	}
	if fl, ok := f.files[name]; ok {
		return &fstFileReader{fs: f, fl: fl}, nil
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

// ReadDir implements fs.ReadDirFS.
func (f *fstFS) ReadDir(name string) ([]fs.DirEntry, error) {
	name = f.rootIf(name)
	if name != "" && !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrInvalid}
	}
	d, ok := f.dirs[name]
	if !ok {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
	}
	out := make([]fs.DirEntry, 0, len(d.children)+len(d.files))
	for _, c := range d.children {
		out = append(out, fstDirEntry{name: c.name, dir: true})
	}
	for _, fn := range d.files {
		out = append(out, fstDirEntry{name: fn, dir: false})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

// Stat implements fs.StatFS.
func (f *fstFS) Stat(name string) (fs.FileInfo, error) {
	name = f.rootIf(name)
	if name != "" && !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrInvalid}
	}
	if d, ok := f.dirs[name]; ok {
		return fstInfo{name: d.name, size: 0, dir: true}, nil
	}
	if fl, ok := f.files[name]; ok {
		return fstInfo{name: fl.name, size: fl.size, dir: false}, nil
	}
	return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
}

type fstDirEntry struct {
	name string
	dir  bool
}

func (e fstDirEntry) Name() string { return e.name }
func (e fstDirEntry) Size() int64  { return 0 }
func (e fstDirEntry) Mode() fs.FileMode {
	if e.dir {
		return fs.ModeDir
	}
	return 0
}
func (e fstDirEntry) Info() (fs.FileInfo, error) { return fstInfo{name: e.name, dir: e.dir}, nil }
func (e fstDirEntry) IsDir() bool                { return e.dir }
func (e fstDirEntry) Type() fs.FileMode          { return e.Mode() }

type fstInfo struct {
	name string
	size int64
	dir  bool
}

func (i fstInfo) Name() string { return i.name }
func (i fstInfo) Size() int64  { return i.size }
func (i fstInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir
	}
	return 0
}
func (i fstInfo) ModTime() time.Time { return time.Time{} }
func (i fstInfo) IsDir() bool        { return i.dir }
func (i fstInfo) Sys() any           { return nil }

// fstDirFile is an fs.File for a directory supporting ReadDir.
type fstDirFile struct {
	dir *fstDir
	fs  *fstFS
}

func (df *fstDirFile) Read([]byte) (int, error) {
	return 0, fmt.Errorf("gcdisc: cannot read a directory")
}

func (df *fstDirFile) Stat() (fs.FileInfo, error) {
	return fstInfo{name: df.dir.name, dir: true}, nil
}

func (df *fstDirFile) Close() error { return nil }

func (df *fstDirFile) ReadDir(n int) ([]fs.DirEntry, error) {
	all, err := df.fs.ReadDir(df.dir.path)
	if err != nil {
		return nil, err
	}
	if n > 0 && len(all) > n {
		return all[:n], nil
	}
	return all, nil
}

// fstFileReader lazily serves file bytes from the disc image.
type fstFileReader struct {
	fs  *fstFS
	fl  fstFile
	off int64
}

func (fr *fstFileReader) Read(p []byte) (int, error) {
	// fr.off is relative to the start of the file section.
	rem := fr.fl.size - fr.off
	if rem <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > rem {
		p = p[:rem] // never read past the section
	}
	n, err := fr.fs.d.ReadAt(p, fr.fl.off+fr.off)
	fr.off += int64(n)
	if n < len(p) {
		return n, io.EOF
	}
	if err == io.ErrUnexpectedEOF || err == io.EOF {
		return n, nil
	}
	return n, err
}

func (fr *fstFileReader) Stat() (fs.FileInfo, error) {
	return fstInfo{name: fr.fl.name, size: fr.fl.size}, nil
}

func (fr *fstFileReader) Close() error { return nil }
