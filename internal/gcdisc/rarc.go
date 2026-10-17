package gcdisc

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"
)

const (
	rarcHeaderSize = 0x20
	rarcInfoSize   = 0x1C
	rarcMaxNodes   = 65536
	rarcMaxEntries = 65536
	rarcMaxDepth   = 64
)

// rarcHeader mirrors the 0x20-byte RARC header (big-endian).
type rarcHeader struct {
	fileSize        uint32
	hdrSize         uint32
	dataOffset      uint32 // relative to 0x20
	dataLen         uint32
	mram, aram, dvd uint32
}

// rarcInfo mirrors the info block at offset 0x20.
type rarcInfo struct {
	nodeCount  uint32
	nodeOff    uint32 // relative to 0x20
	entryCount uint32
	entryOff   uint32 // relative to 0x20
	strTabLen  uint32
	strTabOff  uint32 // relative to 0x20
	fileCount  uint16
	idsMatch   uint8
}

// rarcNode is a 16-byte directory node.
type rarcNode struct {
	tag        [4]byte
	nameOff    uint32
	nameHash   uint16
	entryCount uint16
	firstEntry uint32
}

// rarcEntry is a 20-byte file entry.
type rarcEntry struct {
	id       uint16 // 0xFFFF = subdirectory
	nameHash uint16
	flags    uint32 // top byte: flags; low 24 bits: name offset
	dataOff  uint32 // file: relative to data section; dir: node index
	dataSize uint32
}

type rarcFile struct {
	path string
	off  int64
	size int64
}

type rarcDir struct {
	path    string
	subdirs []string
	files   []string
}

// rarcFS is a decoded RARC archive.
type rarcFS struct {
	raw   []byte
	base  int64 // absolute data-section start
	files map[string]rarcFile
	dirs  map[string]*rarcDir
}

// OpenRARC opens the RARC archive in b as a read-only fs.FS (also
// fs.ReadDirFS and fs.StatFS). Paths are relative to the root node,
// with the root node's own name stripped.
func OpenRARC(b []byte) (fs.FS, error) {
	if IsYaz0(b) {
		var err error
		b, err = Yaz0Decompress(bytes.NewReader(b), 0)
		if err != nil {
			return nil, err
		}
	}
	f := &rarcFS{raw: b, files: make(map[string]rarcFile), dirs: make(map[string]*rarcDir)}
	if err := f.parse(); err != nil {
		return nil, err
	}
	return f, nil
}

// parse decodes the tables and walks the node tree.
func (f *rarcFS) parse() error {
	b := f.raw
	if len(b) < rarcHeaderSize {
		return errors.New("gcdisc: rarc: truncated header")
	}
	if string(b[:4]) != "RARC" {
		return errors.New("gcdisc: rarc: bad magic")
	}
	hdr := rarcHeader{
		fileSize:   binary.BigEndian.Uint32(b[4:8]),
		hdrSize:    binary.BigEndian.Uint32(b[8:12]),
		dataOffset: binary.BigEndian.Uint32(b[12:16]),
		dataLen:    binary.BigEndian.Uint32(b[16:20]),
		mram:       binary.BigEndian.Uint32(b[20:24]),
		aram:       binary.BigEndian.Uint32(b[24:28]),
		dvd:        binary.BigEndian.Uint32(b[28:32]),
	}
	if hdr.hdrSize < rarcHeaderSize {
		return errors.New("gcdisc: rarc: header size too small")
	}
	f.base = int64(rarcHeaderSize) + int64(hdr.dataOffset)
	if hdr.dataLen > 0 && (f.base < 0 || f.base+int64(hdr.dataLen) > int64(len(b))) {
		return errors.New("gcdisc: rarc: data section out of bounds")
	}
	if len(b) < rarcHeaderSize+rarcInfoSize {
		return errors.New("gcdisc: rarc: truncated info block")
	}
	ib := int64(rarcHeaderSize)
	info := rarcInfo{
		nodeCount:  binary.BigEndian.Uint32(b[ib : ib+4]),
		nodeOff:    binary.BigEndian.Uint32(b[ib+4 : ib+8]),
		entryCount: binary.BigEndian.Uint32(b[ib+8 : ib+12]),
		entryOff:   binary.BigEndian.Uint32(b[ib+12 : ib+16]),
		strTabLen:  binary.BigEndian.Uint32(b[ib+16 : ib+20]),
		strTabOff:  binary.BigEndian.Uint32(b[ib+20 : ib+24]),
		fileCount:  binary.BigEndian.Uint16(b[ib+24 : ib+26]),
		idsMatch:   b[ib+26],
	}
	if info.nodeCount == 0 || info.nodeCount > rarcMaxNodes {
		return fmt.Errorf("gcdisc: rarc: bad node count %d", info.nodeCount)
	}
	if info.entryCount > rarcMaxEntries {
		return fmt.Errorf("gcdisc: rarc: bad entry count %d", info.entryCount)
	}
	nodes := make([]rarcNode, info.nodeCount)
	entries := make([]rarcEntry, info.entryCount)
	ntOff := int64(rarcHeaderSize) + int64(info.nodeOff)
	etOff := int64(rarcHeaderSize) + int64(info.entryOff)
	strOff := int64(rarcHeaderSize) + int64(info.strTabOff)
	if ntOff < 0 || ntOff+int64(len(nodes))*16 > int64(len(b)) {
		return errors.New("gcdisc: rarc: node table out of bounds")
	}
	if etOff < 0 || etOff+int64(len(entries))*20 > int64(len(b)) {
		return errors.New("gcdisc: rarc: entry table out of bounds")
	}
	if info.strTabLen > 0 && (strOff < 0 || strOff+int64(info.strTabLen) > int64(len(b))) {
		return errors.New("gcdisc: rarc: string table out of bounds")
	}
	nameEnd := int64(len(b))
	if info.strTabLen > 0 {
		nameEnd = strOff + int64(info.strTabLen)
	}
	nameAt := func(off uint32) (string, error) {
		s := strOff + int64(off)
		if s < strOff || s > nameEnd {
			return "", errors.New("gcdisc: rarc: name offset out of bounds")
		}
		end := bytes.IndexByte(b[s:], 0)
		if end < 0 || s+int64(end) > nameEnd {
			return "", errors.New("gcdisc: rarc: unterminated name")
		}
		return string(b[s : s+int64(end)]), nil
	}
	validName := func(name string) error {
		if name == "" || name == "." || len(name) > 255 || strings.ContainsAny(name, "/") || strings.Contains(name, "..") {
			return fmt.Errorf("gcdisc: rarc: bad name %q", name)
		}
		return nil
	}
	for i := range nodes {
		o := ntOff + int64(i)*16
		copy(nodes[i].tag[:], b[o:o+4])
		nodes[i].nameOff = binary.BigEndian.Uint32(b[o+4 : o+8])
		nodes[i].nameHash = binary.BigEndian.Uint16(b[o+8 : o+10])
		nodes[i].entryCount = binary.BigEndian.Uint16(b[o+10 : o+12])
		nodes[i].firstEntry = binary.BigEndian.Uint32(b[o+12 : o+16])
	}
	for i := range entries {
		o := etOff + int64(i)*20
		entries[i].id = binary.BigEndian.Uint16(b[o : o+2])
		entries[i].nameHash = binary.BigEndian.Uint16(b[o+2 : o+4])
		entries[i].flags = binary.BigEndian.Uint32(b[o+4 : o+8])
		entries[i].dataOff = binary.BigEndian.Uint32(b[o+8 : o+12])
		entries[i].dataSize = binary.BigEndian.Uint32(b[o+12 : o+16])
	}
	visited := make([]bool, len(nodes))
	var walk func(node int, path string, depth int) error
	walk = func(node int, path string, depth int) error {
		if depth > rarcMaxDepth {
			return errors.New("gcdisc: rarc: directory depth exceeded")
		}
		if node < 0 || node >= len(nodes) || visited[node] {
			return errors.New("gcdisc: rarc: node cycle or bad index")
		}
		visited[node] = true
		n := nodes[node]
		if rname, err := nameAt(n.nameOff); err != nil {
			return err
		} else if err := validName(rname); err != nil {
			return err
		}
		if n.firstEntry > uint32(len(entries)) || uint32(n.entryCount) > uint32(len(entries))-n.firstEntry {
			return errors.New("gcdisc: rarc: node entry range out of bounds")
		}
		d := &rarcDir{}
		f.dirs[path] = d
		seen := make(map[string]bool)
		for i := int(n.firstEntry); i < int(n.firstEntry)+int(n.entryCount); i++ {
			e := entries[i]
			flags := e.flags >> 24
			if flags&0x80 != 0 && flags&0x04 == 0 {
				return errors.New("gcdisc: rarc: yaz0 flag without compressed flag")
			}
			name, err := nameAt(e.flags & 0x00FFFFFF)
			if err != nil {
				return err
			}
			if name == "." || name == ".." {
				continue
			}
			if err := validName(name); err != nil {
				return err
			}
			if seen[name] {
				return fmt.Errorf("gcdisc: rarc: duplicate entry %q", name)
			}
			seen[name] = true
			child := name
			if path != "" {
				child = path + "/" + name
			}
			if e.id == 0xFFFF {
				sub := int(e.dataOff)
				if sub >= len(nodes) {
					return fmt.Errorf("gcdisc: rarc: directory node index %d out of bounds", sub)
				}
				d.subdirs = append(d.subdirs, name)
				if err := walk(sub, child, depth+1); err != nil {
					return err
				}
				continue
			}
			if flags&0x01 == 0 {
				return fmt.Errorf("gcdisc: rarc: entry %q missing file flag", name)
			}
			d.files = append(d.files, name)
			fileOff := int64(e.dataOff)
			if fileOff+int64(e.dataSize) > int64(len(b))-f.base {
				return fmt.Errorf("gcdisc: rarc: file data out of bounds: offset %d size %d", fileOff, e.dataSize)
			}
			f.files[child] = rarcFile{path: child, off: f.base + fileOff, size: int64(e.dataSize)}
		}
		return nil
	}
	return walk(0, "", 0)
}

// Open implements fs.FS. A file whose stored bytes start with the
// Yaz0 magic is decompressed on read (capped at Yaz0MaxOut).
func (f *rarcFS) Open(name string) (fs.File, error) {
	if name == "." {
		name = ""
	}
	if name != "" && !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	if file, ok := f.files[name]; ok {
		raw := f.raw[file.off : file.off+file.size]
		var data []byte
		if IsYaz0(raw) {
			var err error
			data, err = Yaz0Decompress(bytes.NewReader(raw), 0)
			if err != nil {
				return nil, &fs.PathError{Op: "open", Path: name, Err: err}
			}
		} else {
			data = raw
		}
		return &rarcFileObj{name: path.Base(name), rdr: bytes.NewReader(data), size: int64(len(data))}, nil
	}
	if _, ok := f.dirs[name]; ok {
		return &rarcDirObj{name: path.Base(name), f: f, path: name}, nil
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

// ReadDir implements fs.ReadDirFS.
func (f *rarcFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if name == "." {
		name = ""
	}
	d, ok := f.dirs[name]
	if !ok {
		return nil, &fs.PathError{Op: "readdirent", Path: name, Err: fs.ErrNotExist}
	}
	ents := make([]fs.DirEntry, 0, len(d.subdirs)+len(d.files))
	for _, s := range d.subdirs {
		ents = append(ents, rarcDirEntry{name: s, isDir: true})
	}
	for _, s := range d.files {
		ents = append(ents, rarcDirEntry{name: s, isDir: false})
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
	return ents, nil
}

// Stat implements fs.StatFS.
func (f *rarcFS) Stat(name string) (fs.FileInfo, error) {
	if name == "." {
		name = ""
	}
	if file, ok := f.files[name]; ok {
		return rarcFileInfo{name: path.Base(name), size: file.size}, nil
	}
	if _, ok := f.dirs[name]; ok {
		return rarcFileInfo{name: path.Base(name), isDir: true}, nil
	}
	return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
}

// rarcFileObj is a single readable RARC file.
type rarcFileObj struct {
	name string
	rdr  io.Reader
	size int64
}

func (o *rarcFileObj) Read(p []byte) (int, error) { return o.rdr.Read(p) }
func (o *rarcFileObj) Stat() (fs.FileInfo, error) {
	return rarcFileInfo{name: o.name, size: o.size}, nil
}
func (o *rarcFileObj) Close() error { return nil }

// rarcDirObj is a directory; it is also an fs.ReadDirFile.
type rarcDirObj struct {
	name string
	f    *rarcFS
	path string
}

func (d *rarcDirObj) Read(p []byte) (int, error) { return 0, fs.ErrInvalid }
func (d *rarcDirObj) Stat() (fs.FileInfo, error) { return d.f.Stat(d.path) }
func (d *rarcDirObj) Close() error               { return nil }
func (d *rarcDirObj) ReadDir(n int) ([]fs.DirEntry, error) {
	ents, err := d.f.ReadDir(d.path)
	if err != nil {
		return nil, err
	}
	if n > 0 && len(ents) > n {
		ents = ents[:n]
	}
	return ents, nil
}

type rarcDirEntry struct {
	name  string
	isDir bool
}

func (e rarcDirEntry) Name() string { return e.name }
func (e rarcDirEntry) IsDir() bool  { return e.isDir }
func (e rarcDirEntry) Type() fs.FileMode {
	if e.isDir {
		return fs.ModeDir
	}
	return 0
}
func (e rarcDirEntry) Info() (fs.FileInfo, error) {
	return rarcFileInfo{name: e.name, isDir: e.isDir}, nil
}
func (e rarcDirEntry) Size() int64 { return 0 }

// rarcFileInfo is a RARC file or directory info; sizes for compressed
// files are the on-disk (compressed) sizes.
type rarcFileInfo struct {
	name  string
	size  int64
	isDir bool
}

func (i rarcFileInfo) Name() string { return i.name }
func (i rarcFileInfo) Size() int64  { return i.size }
func (i rarcFileInfo) Mode() fs.FileMode {
	if i.isDir {
		return fs.ModeDir
	}
	return 0
}
func (i rarcFileInfo) ModTime() time.Time { return time.Time{} }
func (i rarcFileInfo) IsDir() bool        { return i.isDir }
func (i rarcFileInfo) Sys() any           { return nil }
