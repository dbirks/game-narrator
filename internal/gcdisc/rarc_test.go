package gcdisc

import (
	"bytes"
	"encoding/binary"
	"io"
	"io/fs"
	"os"
	"strings"
	"testing"
)

// yaz0Literal encodes data as a Yaz0 stream using only literal symbols
// (every control bit set); trailing group bits are zero and never
// decoded because the decoder stops at the declared size.
func yaz0Literal(data []byte) []byte {
	var out bytes.Buffer
	out.WriteString("Yaz0")
	var sz [4]byte
	binary.BigEndian.PutUint32(sz[:], uint32(len(data)))
	out.Write(sz[:])
	out.Write(make([]byte, 8)) // reserved
	i := 0
	for i < len(data) {
		n := len(data) - i
		if n > 8 {
			n = 8
		}
		ctl := byte(0)
		for k := 0; k < n; k++ {
			ctl |= 0x80 >> uint(k)
		}
		out.WriteByte(ctl)
		out.Write(data[i : i+n])
		i += n
	}
	return out.Bytes()
}

// assembleRARC builds a small RARC:
//
//	root/
//	  a.txt   "hello"
//	  sub/
//	    b.txt  "world" (Yaz0-compressed in the data section)
//
// Layout (absolute offsets): header 0x00-0x1F, info 0x20, nodes 0x40,
// entries 0x60, strings 0xA0, data 0xC0.
func assembleRARC() []byte {
	a := []byte("hello")
	bz := yaz0Literal([]byte("world"))
	data := make([]byte, 0, len(a)+len(bz))
	data = append(data, a...)
	data = append(data, bz...)

	strTab := []byte("root\x00a.txt\x00sub\x00b.txt\x00")
	// name offsets within strTab: root=0, a.txt=5, sub=11, b.txt=15
	var buf bytes.Buffer
	u32 := func(v uint32) { var x [4]byte; binary.BigEndian.PutUint32(x[:], v); buf.Write(x[:]) }
	u16 := func(v uint16) { var x [2]byte; binary.BigEndian.PutUint16(x[:], v); buf.Write(x[:]) }
	// header
	buf.WriteString("RARC")
	u32(uint32(0)) // file size (patched at the end)
	u32(0x20)      // header size
	u32(0xA0)      // data offset (relative to 0x20)
	u32(uint32(len(data)))
	u32(0) // mram
	u32(0) // aram
	u32(0) // dvd
	// info block at 0x20
	u32(2) // node count
	u32(0x20)
	u32(3) // entry count
	u32(0x40)
	u32(uint32(len(strTab)))
	u32(0x80)
	u16(2)           // file count
	buf.WriteByte(1) // ids match index
	buf.WriteByte(0) // pad
	buf.Write(make([]byte, 0x40-0x20-28))
	// node 0: tag "root", nameOff 0, hash 0, count 2, first 0
	buf.WriteString("root")
	u32(0)
	u16(0)
	u16(2)
	u32(0)
	// node 1: tag "sub ", nameOff 11, hash 0, count 1, first 2
	buf.WriteString("sub ")
	u32(11)
	u16(0)
	u16(1)
	u32(2)
	// pad to 0x60 (2 nodes = 32 bytes: 0x40..0x5F)
	buf.Write(make([]byte, 0x60-buf.Len()))
	// entry 0: file a.txt, id 1, nameOff 5, flags 0x01, dataOff 0, size 5
	u16(1)
	u16(0)
	u32(0x01000000 | 5)
	u32(0)
	u32(uint32(len(a)))
	u32(0)
	// entry 1: dir sub, id 0xFFFF, nameOff 11, flags 0x02, nodeIdx 1
	u16(0xFFFF)
	u16(0)
	u32(0x02000000 | 11)
	u32(1)
	u32(0)
	u32(0)
	// entry 2: file b.txt, id 2, nameOff 15, flags 0x85 (yaz0+compressed+file), dataOff len(a)
	u16(2)
	u16(0)
	u32(0x85000000 | 15)
	u32(uint32(len(a)))
	u32(uint32(len(bz)))
	u32(0)
	// pad to 0xA0 (3 entries = 60 bytes: 0x60..0x9B)
	buf.Write(make([]byte, 0xA0-buf.Len()))
	buf.Write(strTab)
	// pad to 0xC0
	buf.Write(make([]byte, 0xC0-buf.Len()))
	buf.Write(data)
	// patch file size in header
	out := buf.Bytes()
	binary.BigEndian.PutUint32(out[4:8], uint32(len(out)))
	return out
}

func TestRARCOpen(t *testing.T) {
	arc, err := OpenRARC(assembleRARC())
	if err != nil {
		t.Fatalf("OpenRARC: %v", err)
	}
	got, err := fs.ReadFile(arc, "a.txt")
	if err != nil {
		t.Fatalf("read a.txt: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("a.txt = %q, want %q", got, "hello")
	}
	got, err = fs.ReadFile(arc, "sub/b.txt")
	if err != nil {
		t.Fatalf("read sub/b.txt: %v", err)
	}
	if string(got) != "world" {
		t.Fatalf("sub/b.txt = %q, want %q (decompressed)", got, "world")
	}
	rd, ok := arc.(fs.ReadDirFS)
	if !ok {
		t.Fatal("not a ReadDirFS")
	}
	ents, err := rd.ReadDir("")
	if err != nil {
		t.Fatalf("ReadDir(root): %v", err)
	}
	if len(ents) != 2 || ents[0].Name() != "a.txt" || ents[0].IsDir() || ents[1].Name() != "sub" || !ents[1].IsDir() {
		t.Fatalf("root entries = %v", ents)
	}
	ents, err = rd.ReadDir("sub")
	if err != nil || len(ents) != 1 || ents[0].IsDir() {
		t.Fatalf("ReadDir(sub) = %v, %v", ents, err)
	}
	sf, ok := arc.(fs.StatFS)
	if !ok {
		t.Fatal("not a StatFS")
	}
	st, err := sf.Stat("sub")
	if err != nil || !st.IsDir() {
		t.Fatalf("Stat(sub) = %v, %v", st, err)
	}
	st, err = sf.Stat("a.txt")
	if err != nil || st.Size() != 5 {
		t.Fatalf("Stat(a.txt) = %v, %v", st, err)
	}
	if _, err := arc.Open("nope"); err == nil {
		t.Fatal("Open(nope): want error")
	}
	n := 0
	err = fs.WalkDir(arc, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			t.Fatalf("walk %q: %v", p, err)
		}
		n++
		return nil
	})
	if err != nil || n != 4 {
		t.Fatalf("walk: n=%d err=%v, want 4", n, err)
	}
}

func TestRARCWholeYaz0(t *testing.T) {
	arc, err := OpenRARC(yaz0Literal(assembleRARC()))
	if err != nil {
		t.Fatalf("OpenRARC(yaz0): %v", err)
	}
	if got, err := fs.ReadFile(arc, "a.txt"); err != nil || string(got) != "hello" {
		t.Fatalf("a.txt = %q, %v", got, err)
	}
}

func TestRARCHostile(t *testing.T) {
	good := assembleRARC()
	mustErr := func(name string, b []byte) {
		t.Run(name, func(t *testing.T) {
			if _, err := OpenRARC(b); err == nil {
				t.Fatal("want error, got nil")
			}
		})
	}
	mustErr("truncated", good[:10])
	mustErr("tiny", []byte("RA"))
	bad := append([]byte(nil), good...)
	copy(bad[:4], "XXXX")
	mustErr("bad-magic", bad)

	bad = append([]byte(nil), good...)
	// entry 0 data size (0x6C) past end
	binary.BigEndian.PutUint32(bad[0x6C:0x70], 0x01000000)
	mustErr("offset-past-end", bad)

	bad = append([]byte(nil), good...)
	// entry 1 (dir "sub") node index -> 0 (root) => cycle
	binary.BigEndian.PutUint32(bad[0x7C:0x80], 0)
	mustErr("node-cycle", bad)

	bad = append([]byte(nil), good...)
	// string table at 0xA0: "root\0a.txt\0..." -> "a/t" at 0xA7
	bad[0xA7] = '/'
	mustErr("bad-name", bad)

	bad = append([]byte(nil), good...)
	// node count 100000 (table out of bounds)
	binary.BigEndian.PutUint32(bad[0x20:0x24], 100000)
	mustErr("node-count", bad)

	// entry with Yaz0 flag (0x80) but without compressed flag (0x04)
	bad = append([]byte(nil), good...)
	binary.BigEndian.PutUint32(bad[0x64:0x68], 0x81000000|5)
	mustErr("yaz0-without-compressed", bad)
}

func FuzzRARC(f *testing.F) {
	f.Add(assembleRARC())
	f.Add(assembleRARC()[:5])
	f.Add(yaz0Literal(assembleRARC()))
	f.Fuzz(func(t *testing.T, data []byte) {
		arc, err := OpenRARC(data)
		if err != nil {
			return
		}
		_ = fs.WalkDir(arc, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			fh, err := arc.Open(p)
			if err != nil {
				return nil
			}
			defer fh.Close()
			_, _ = io.Copy(io.Discard, io.LimitReader(fh, 1<<20))
			return nil
		})
	})
}

func TestRARCRealImage(t *testing.T) {
	path := os.Getenv("GAME_NARRATOR_GAME_IMAGE_PATH")
	if path == "" {
		t.Skip("NOT RUN: GAME_NARRATOR_GAME_IMAGE_PATH not set")
	}
	d, err := Open(path)
	if err != nil {
		t.Fatalf("open disc: %v", err)
	}
	defer d.Close()
	dfs, err := d.FS()
	if err != nil {
		t.Fatalf("disc FS: %v", err)
	}
	raw, err := fs.ReadFile(dfs, "res/Msg/bmgres.arc")
	if err != nil {
		t.Fatalf("read bmgres.arc: %v", err)
	}
	arc, err := OpenRARC(raw)
	if err != nil {
		t.Fatalf("OpenRARC(bmgres.arc): %v", err)
	}
	var entries, bmgFiles int
	var bmgTotal int64
	var bmgSizes []int64
	err = fs.WalkDir(arc, ".", func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			t.Logf("walk %q: %v", p, err)
			return nil
		}
		if de.IsDir() {
			return nil
		}
		entries++
		if strings.HasSuffix(p, ".bmg") {
			bmgFiles++
			if st, err := arc.(fs.StatFS).Stat(p); err == nil {
				bmgTotal += st.Size()
				bmgSizes = append(bmgSizes, st.Size())
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if bmgFiles == 0 {
		t.Fatal("no .bmg entries found")
	}
	t.Logf("entries=%d bmg_files=%d bmg_total_bytes=%d", entries, bmgFiles, bmgTotal)
	for i, s := range bmgSizes {
		t.Logf("bmg[%d] size=%d", i, s)
	}
}
