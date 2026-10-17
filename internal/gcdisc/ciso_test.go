package gcdisc

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"testing"
)

const testBlock = int64(0x1000) // small power of two for synthetic images

// rawCISO assembles a synthetic CISO image in memory: magic, block-size
// field, presence map (remaining map bytes stay 0), then dataLen bytes
// of block data starting at 0x8000.
func rawCISO(magic string, bs uint32, mapBytes []byte, dataLen int64) *bytes.Reader {
	buf := make([]byte, cisoHeaderSize)
	copy(buf, magic)
	binary.LittleEndian.PutUint32(buf[4:8], bs)
	copy(buf[cisoMapOffset:cisoMapOffset+cisoMapLen], mapBytes)
	buf = append(buf, make([]byte, dataLen)...)
	return bytes.NewReader(buf)
}

// buildCISO assembles a synthetic CISO with the given presence map and
// per-block data (block i absent or data[i] nil gets deterministic junk
// bytes).
func buildCISO(t *testing.T, bs int64, blocks []byte, data [][]byte) (*bytes.Reader, []byte) {
	t.Helper()
	buf := make([]byte, cisoHeaderSize)
	copy(buf, "CISO")
	binary.LittleEndian.PutUint32(buf[4:8], uint32(bs))
	copy(buf[cisoMapOffset:cisoMapOffset+cisoMapLen], blocks)
	for i, present := range blocks {
		if present != 1 {
			continue
		}
		var d []byte
		if i < len(data) && data[i] != nil {
			d = data[i]
		} else {
			d = make([]byte, bs)
			for j := range d {
				d[j] = byte(i*13 + j%251)
			}
		}
		if int64(len(d)) < bs {
			d = append(d, make([]byte, bs-int64(len(d)))...)
		}
		buf = append(buf, d[:bs]...)
	}
	return bytes.NewReader(buf), buf
}

func TestCISO(t *testing.T) {
	const bs = testBlock
	block0 := make([]byte, bs)
	for i := range block0 {
		block0[i] = byte(i)
	}
	block2 := make([]byte, bs)
	for i := range block2 {
		block2[i] = byte(i*3 + 7)
	}
	// Presence: [present, absent, present].
	r, img := buildCISO(t, bs, []byte{1, 0, 1}, [][]byte{block0, nil, block2})
	c, err := openCISO(r, int64(r.Len()))
	if err != nil {
		t.Fatalf("openCISO: %v", err)
	}
	if c.Size() != 3*bs {
		t.Fatalf("Size() = %d, want %d", c.Size(), 3*bs)
	}

	t.Run("block content", func(t *testing.T) {
		buf := make([]byte, bs)
		n, err := c.ReadAt(buf, 0)
		if n != int(bs) || err != nil {
			t.Fatalf("ReadAt(0): n=%d err=%v", n, err)
		}
		if !bytes.Equal(buf, block0) {
			t.Errorf("block 0 mismatch")
		}
		n, err = c.ReadAt(buf, 2*bs)
		if n != int(bs) || err != nil {
			t.Fatalf("ReadAt(2bs): n=%d err=%v", n, err)
		}
		if !bytes.Equal(buf, block2) {
			t.Errorf("block 2 mismatch")
		}
	})

	t.Run("spanning blocks", func(t *testing.T) {
		// Last 4 bytes of block 0, all of absent block 1, first 8 bytes
		// of block 2, in one read.
		off := bs - 4
		buf := make([]byte, 4+int(bs)+8)
		n, err := c.ReadAt(buf, off)
		if n != len(buf) || err != nil {
			t.Fatalf("ReadAt: n=%d err=%v, want %d, nil", n, err, len(buf))
		}
		if !bytes.Equal(buf[:4], block0[bs-4:]) {
			t.Errorf("block 0 tail mismatch: %x", buf[:4])
		}
		if !allZero(buf[4 : 4+int(bs)]) {
			t.Errorf("absent block 1 is not zero")
		}
		if !bytes.Equal(buf[4+int(bs):], block2[:8]) {
			t.Errorf("block 2 head mismatch: %x", buf[4+int(bs):])
		}
	})

	t.Run("absent block zeros", func(t *testing.T) {
		buf := make([]byte, bs)
		n, err := c.ReadAt(buf, bs)
		if n != int(bs) || err != nil {
			t.Fatalf("ReadAt(bs): n=%d err=%v", n, err)
		}
		if !allZero(buf) {
			t.Errorf("absent block 1 is not zero")
		}
	})

	t.Run("EOF at logical size", func(t *testing.T) {
		buf := make([]byte, 4)
		n, err := c.ReadAt(buf, c.Size())
		if n != 0 || err != io.EOF {
			t.Fatalf("ReadAt(size) = %d, %v; want 0, io.EOF", n, err)
		}
		if _, err := c.ReadAt(buf, c.Size()+16); err == nil {
			t.Fatalf("ReadAt beyond size: want error")
		}
		if _, err := c.ReadAt(buf, -1); err == nil {
			t.Fatalf("ReadAt(-1): want error")
		}
		// Read straddling the logical end: 8 real bytes delivered, the
		// remaining 8 zero-filled, with io.EOF.
		buf16 := make([]byte, 16)
		n, err = c.ReadAt(buf16, c.Size()-8)
		if n != 8 || err != io.EOF {
			t.Fatalf("ReadAt(size-8) = %d, %v; want 8, io.EOF", n, err)
		}
		if !bytes.Equal(buf16[:8], block2[bs-8:]) {
			t.Errorf("tail mismatch: %x", buf16[:8])
		}
		if !allZero(buf16[8:]) {
			t.Errorf("past-size tail is not zero")
		}
	})

	t.Run("trailer ignored", func(t *testing.T) {
		// A few hundred bytes after the last stored block.
		trailer := make([]byte, 333)
		full := append(append([]byte{}, img...), trailer...)
		rc, err := openCISO(bytes.NewReader(full), int64(len(full)))
		if err != nil {
			t.Fatalf("openCISO with trailer: %v", err)
		}
		if rc.Size() != 3*bs {
			t.Fatalf("Size() with trailer = %d, want %d", rc.Size(), 3*bs)
		}
		buf := make([]byte, 8)
		if n, err := rc.ReadAt(buf, 2*bs); n != 8 || err != nil || !bytes.Equal(buf, block2[:8]) {
			t.Fatalf("ReadAt with trailer: n=%d err=%v", n, err)
		}
	})
}

func TestCISOErrors(t *testing.T) {
	const bs = testBlock
	full := int64(2 * bs) // data for two present blocks

	cases := []struct {
		name     string
		magic    string
		bs       uint32
		mapBytes []byte
		dataLen  int64
		want     string
	}{
		{"bad magic", "XISO", uint32(bs), []byte{1, 0, 1}, full, "magic"},
		{"block size zero", "CISO", 0, []byte{1, 0, 1}, full, "block size"},
		{"block size too large", "CISO", 0x8000000, []byte{1}, 0, "block size"},
		{"block size not a power of two", "CISO", 0x1200, []byte{1}, 0, "block size"},
		{"map byte not 0 or 1", "CISO", uint32(bs), []byte{1, 2, 1}, full, "block map"},
		{"file too short for blocks", "CISO", uint32(bs), []byte{1, 0, 1}, bs, "too short"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := rawCISO(tc.magic, tc.bs, tc.mapBytes, tc.dataLen)
			_, err := openCISO(r, int64(r.Len()))
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}

	t.Run("file too short for header", func(t *testing.T) {
		small := make([]byte, 4+4+100)
		copy(small, "CISO")
		_, err := openCISO(bytes.NewReader(small), int64(len(small)))
		if err == nil || !strings.Contains(err.Error(), "too short") {
			t.Fatalf("want too-short error, got %v", err)
		}
	})
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}
