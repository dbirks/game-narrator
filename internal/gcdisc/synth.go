package gcdisc

// synth.go is the synthetic-image factory (ADR-0054's fixture-factory
// semantics for disc images, ADR-0066). The builders assemble plain GCM
// logical images and RARC archives from in-memory data, so the tests and the
// UI test fixture can exercise the disc-import path with invented content —
// never with game material (AGENTS.md rule 5). What they emit is exactly
// what Open, FS and OpenRARC read back; the round-trip tests are the gate.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"
	"strings"
)

// SynthEntry is one FST entry of a synthetic image. It is the exported form
// of the layout fst_test.go has used privately: the root is entry 0 (a
// directory with no name), directories carry Parent/End, files carry
// Off/Size (absolute logical disc offsets).
type SynthEntry struct {
	IsDir  bool
	Name   string
	Off    int64 // file: absolute logical disc offset
	Size   int64 // file: length
	Parent int   // dir: parent entry index
	End    int   // dir: index one past the subtree end
}

// SynthGCMImage assembles a plain GCM logical image: the boot header with
// gameID, the FST at fstOff (entries first, then the NUL-terminated string
// table), and each file's contents at its absolute logical offset. The image
// is at least gcmHeaderLen bytes and is read back by Open/FS.
func SynthGCMImage(gameID string, fstOff int64, entries []SynthEntry, fileData map[string][]byte) []byte {
	var str bytes.Buffer
	nameOff := make(map[string]int64, len(entries))
	for _, e := range entries {
		if e.Name == "" {
			continue
		}
		nameOff[e.Name] = int64(str.Len())
		str.WriteString(e.Name)
		str.WriteByte(0)
	}
	count := len(entries)
	fst := make([]byte, count*fstEntrySize)
	for i, e := range entries {
		b := fst[i*fstEntrySize : (i+1)*fstEntrySize]
		if e.IsDir {
			b[0] = 1
		}
		no := nameOff[e.Name]
		b[1], b[2], b[3] = byte(no>>16), byte(no>>8), byte(no)
		switch {
		case i == 0: // root: third field is the total entry count
			binary.BigEndian.PutUint32(b[8:12], uint32(count))
		case e.IsDir:
			binary.BigEndian.PutUint32(b[4:8], uint32(e.Parent))
			binary.BigEndian.PutUint32(b[8:12], uint32(e.End))
		default:
			binary.BigEndian.PutUint32(b[4:8], uint32(e.Off))
			binary.BigEndian.PutUint32(b[8:12], uint32(e.Size))
		}
	}
	fstLen := int64(count*fstEntrySize) + int64(str.Len())
	imgSize := fstOff + fstLen
	if imgSize < gcmHeaderLen {
		imgSize = gcmHeaderLen
	}
	fileOff := make(map[string]int64, len(fileData))
	for i, e := range entries {
		if i == 0 || e.IsDir {
			continue
		}
		data, ok := fileData[e.Name]
		if !ok {
			panic(fmt.Sprintf("gcdisc.SynthGCMImage: missing contents for file %q", e.Name))
		}
		if int64(len(data)) > e.Size {
			panic(fmt.Sprintf("gcdisc.SynthGCMImage: contents of %q exceed declared size %d", e.Name, e.Size))
		}
		fileOff[e.Name] = e.Off
		if e.Off+e.Size > imgSize {
			imgSize = e.Off + e.Size
		}
	}
	img := make([]byte, imgSize)
	copy(img[0:6], gameID)
	binary.BigEndian.PutUint32(img[0x424:0x428], uint32(fstOff))
	binary.BigEndian.PutUint32(img[0x428:0x42C], uint32(fstLen))
	copy(img[fstOff:fstOff+int64(count*fstEntrySize)], fst)
	copy(img[fstOff+int64(count*fstEntrySize):], str.Bytes())
	for name, data := range fileData {
		copy(img[fileOff[name]:], data)
	}
	return img
}

// AssembleRARC builds a RARC archive holding the named files (paths may be
// nested, e.g. "bmg/bmg0.bmg"). The layout mirrors what OpenRARC reads:
// header, info, nodes, entries, the NUL-terminated string table, then the
// data section. Files are stored uncompressed with the plain file flag, in
// one pass: node order is root-first over the sorted paths, and each node's
// entry run lists its subdirectories before its files.
func AssembleRARC(files map[string][]byte) []byte {
	if len(files) == 0 {
		panic("gcdisc.AssembleRARC: no files")
	}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	// Directory nodes: the root plus every ancestor of a path, root first so
	// each index is stable while it is assigned.
	dirs := []string{""}
	for _, p := range paths {
		for {
			slash := strings.LastIndex(p, "/")
			if slash < 0 {
				break
			}
			parent := p[:slash]
			if !containsStr(dirs, parent) {
				dirs = append(dirs, parent)
			}
			p = parent
		}
	}
	dirIndex := make(map[string]int, len(dirs))
	for i, d := range dirs {
		dirIndex[d] = i
	}

	// Group the paths per parent directory, keeping the sorted order inside.
	type group struct {
		files []string
		subs  []string
	}
	byDir := make(map[string]*group, len(dirs))
	for _, d := range dirs {
		byDir[d] = &group{}
	}
	for _, p := range paths {
		slash := strings.LastIndex(p, "/")
		name := p
		parent := ""
		if slash >= 0 {
			name = p[slash+1:]
			parent = p[:slash]
		}
		byDir[parent].files = append(byDir[parent].files, name)
	}
	for _, d := range dirs[1:] {
		parent := ""
		leaf := d
		if slash := strings.LastIndex(d, "/"); slash >= 0 {
			parent = d[:slash]
			leaf = d[slash+1:]
		}
		byDir[parent].subs = append(byDir[parent].subs, leaf)
	}

	// String table: "root" plus every directory and file name, in first-use
	// order (nodes walk top-down, entries in the same order).
	var strTab strings.Builder
	nameAt := func(name string) uint32 {
		off := uint32(strTab.Len())
		strTab.WriteString(name)
		strTab.WriteByte(0)
		return off
	}

	// Data section, in entry order (the reader never correlates ids with
	// offsets; it only needs each entry's dataOff to point at its bytes).
	var data bytes.Buffer
	dataOffByPath := make(map[string]uint32, len(paths))
	var entryCount uint32
	addFileEntry := func(d, name string, b *bytes.Buffer) {
		p := name
		if d != "" {
			p = d + "/" + name
		}
		dataOffByPath[p] = uint32(data.Len())
		data.Write(files[p])
		b.Write(u16(uint16(len(dataOffByPath)))) // file id, 1-based over files
		b.Write(u16(0))                          // name hash (unverified)
		b.Write(u32(0x01000000 | nameAt(name)))
		b.Write(u32(dataOffByPath[p]))
		b.Write(u32(uint32(len(files[p]))))
		b.Write(u32(0))
		entryCount++
	}
	var entriesBuf bytes.Buffer
	for _, d := range dirs {
		g := byDir[d]
		for _, sub := range g.subs {
			subPath := sub
			if d != "" {
				subPath = d + "/" + sub
			}
			entriesBuf.Write(u16(0xFFFF)) // directory id
			entriesBuf.Write(u16(0))
			entriesBuf.Write(u32(0x02000000 | nameAt(sub)))
			entriesBuf.Write(u32(uint32(dirIndex[subPath])))
			entriesBuf.Write(u32(0))
			entriesBuf.Write(u32(0))
			entryCount++
		}
		for _, name := range g.files {
			addFileEntry(d, name, &entriesBuf)
		}
	}

	// Nodes, in the same order; firstEntry is the running entry count.
	var nodesBuf bytes.Buffer
	var first uint32
	for _, d := range dirs {
		g := byDir[d]
		name := "root"
		if d != "" {
			name = d[strings.LastIndex(d, "/")+1:]
		}
		nodesBuf.Write(tagBytes(name))
		nodesBuf.Write(u32(nameAt(name)))
		nodesBuf.Write(u16(0))
		nodesBuf.Write(u16(uint16(len(g.files) + len(g.subs))))
		nodesBuf.Write(u32(first))
		first += uint32(len(g.files) + len(g.subs))
	}

	strTabBytes := []byte(strTab.String())
	nodes := nodesBuf.Bytes()
	entries := entriesBuf.Bytes()

	// Assemble: header | info | nodes | entries | strings | data. Every
	// offset the reader decodes is relative to 0x20 (f.base = 0x20 +
	// dataOffset; the info block sits at 0x20 and is 0x1C bytes, so the
	// tables follow it back-to-back with no padding).
	infoSize := int64(rarcInfoSize)
	dataOffset := infoSize + int64(len(nodes)+len(entries)+len(strTabBytes))
	var buf bytes.Buffer
	buf.WriteString("RARC")
	buf.Write(u32(0)) // fileSize, patched at the end
	buf.Write(u32(rarcHeaderSize))
	buf.Write(u32(uint32(dataOffset)))
	buf.Write(u32(uint32(data.Len())))
	buf.Write(u32(0)) // mram
	buf.Write(u32(0)) // aram
	buf.Write(u32(0)) // dvd
	// info at 0x20
	buf.Write(u32(uint32(len(dirs))))
	buf.Write(u32(uint32(infoSize))) // nodeOff, relative to 0x20
	buf.Write(u32(entryCount))
	buf.Write(u32(uint32(infoSize + int64(len(nodes)))))
	buf.Write(u32(uint32(len(strTabBytes))))
	buf.Write(u32(uint32(infoSize + int64(len(nodes)+len(entries)))))
	buf.Write(u16(uint16(len(paths))))
	buf.WriteByte(1) // ids match index
	buf.WriteByte(0) // pad
	buf.Write(nodes)
	buf.Write(entries)
	buf.Write(strTabBytes)
	buf.Write(data.Bytes())
	out := buf.Bytes()
	binary.BigEndian.PutUint32(out[4:8], uint32(len(out)))
	return out
}

func containsStr(dirs []string, d string) bool {
	for _, x := range dirs {
		if x == d {
			return true
		}
	}
	return false
}

// tagBytes pads a name to the 4-byte node tag.
func tagBytes(name string) []byte {
	tag := make([]byte, 4)
	copy(tag, name)
	return tag
}

func u16(v uint16) []byte {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], v)
	return b[:]
}

func u32(v uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return b[:]
}

// RegionOfGameID maps a GameCube game id to its region tag: the fourth
// character is the region slot (E = NTSC-U, J = NTSC-J, P = PAL), as the
// windwaker adapter's DiscRegion reports. An id shorter than four characters
// reports "unknown" rather than guessing.
func RegionOfGameID(gameID string) string {
	if len(gameID) < 4 {
		return "unknown"
	}
	switch gameID[3] {
	case 'E':
		return "ntsc-u"
	case 'J':
		return "ntsc-j"
	case 'P':
		return "pal"
	default:
		return "unknown"
	}
}
