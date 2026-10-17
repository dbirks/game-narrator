// Package gcdisc is a read-only reader for user-supplied GameCube disc
// images (CISO containers and plain GCM logical images).
//
// It implements the container, boot-header, FST, Yaz0 and RARC layers
// needed to lazily read disc files. It never writes extracted data and
// bounds every offset (see ADR 0027).
package gcdisc

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	cisoHeaderSize = 0x8000    // fixed header region: magic, block size, block map
	cisoMapOffset  = 8         // block map starts right after magic + block size
	cisoMapLen     = 0x7FF8    // one presence byte per logical block
	cisoDataOffset = 0x8000    // present blocks are stored contiguously from here
	cisoMaxBlock   = 0x4000000 // 64 MiB maximum block size
	gcmHeaderLen   = 0x500     // GCM boot header length we care about
)

// Header is the parsed GCM boot header (logical offset 0 of the disc).
type Header struct {
	GameID    string
	Maker     string
	Name      string
	DiscNum   uint8
	DiscTotal uint8
	FSTOffset uint32
	FSTSize   uint32
}

// Disc is a read-only logical GameCube disc.
type Disc struct {
	f      *os.File
	readAt logicalReader
	hdr    *Header
}

// logicalReader maps logical disc offsets to physical file offsets.
type logicalReader interface {
	io.ReaderAt
	Size() int64
}

// Open opens a user-owned disc image at path. It detects CISO containers
// by magic and otherwise treats the file as a plain GCM logical image.
func Open(path string) (*Disc, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if info.Size() < gcmHeaderLen {
		f.Close()
		return nil, fmt.Errorf("gcdisc: file too small (%d bytes) to be a disc image", info.Size())
	}
	peek := make([]byte, 4)
	if _, err := f.ReadAt(peek, 0); err != nil {
		f.Close()
		return nil, err
	}
	var lr logicalReader
	if bytes.Equal(peek, []byte("CISO")) {
		c, err := openCISO(f, info.Size())
		if err != nil {
			f.Close()
			return nil, err
		}
		lr = c
	} else {
		// Plain GCM image: logical offset == file offset.
		lr = &plainReader{r: f, size: info.Size()}
	}
	// The GCM boot header sits at logical offset 0; in a CISO image it
	// lives inside the first stored block, not at the start of the file.
	hdrBuf := make([]byte, gcmHeaderLen)
	if _, err := lr.ReadAt(hdrBuf, 0); err != nil {
		f.Close()
		return nil, err
	}
	h, err := parseGCMHeader(hdrBuf)
	if err != nil {
		f.Close()
		return nil, err
	}
	return &Disc{f: f, readAt: lr, hdr: &h}, nil
}

// plainReader is a logical reader for uncompressed GCM images.
type plainReader struct {
	r    io.ReaderAt
	size int64
}

func (p *plainReader) Size() int64 { return p.size }
func (p *plainReader) ReadAt(buf []byte, off int64) (int, error) {
	if off >= p.size {
		if off == p.size {
			return 0, io.EOF
		}
		return 0, fmt.Errorf("gcdisc: offset %d beyond size %d", off, p.size)
	}
	n, err := p.r.ReadAt(buf, off)
	if n < len(buf) && err == nil {
		err = io.EOF
	}
	return n, err
}

// cisoReader maps logical CISO disc offsets to physical file offsets.
//
// CISO layout (verified against the reference image; there is no offset
// shift anywhere in the container):
//
//	0x0000  "CISO" magic
//	0x0004  block size, little-endian u32
//	0x0008  block map, 0x7FF8 bytes, one byte per logical block
//	         (1 = present, 0 = absent)
//	0x8000  present blocks stored contiguously in map order
//
// Logical block i is absent when its map byte is 0 (reads as zeros),
// otherwise it lives at 0x8000 + prefix[i]*bs. A trailer of a few
// hundred bytes after the last stored block is ignored.
type cisoReader struct {
	r       io.ReaderAt
	bs      int64
	mapBits [cisoMapLen]byte // 1 = block present
	prefix  []int64          // prefix[i] = present blocks before block i
	last    int64            // index of last present block (-1 if none)
	size    int64            // (last+1)*bs
}

// openCISO parses the CISO container in f (a file of the given size) and
// returns a logical reader. The CISO magic must be present.
func openCISO(f io.ReaderAt, size int64) (*cisoReader, error) {
	if size < cisoHeaderSize {
		return nil, fmt.Errorf("gcdisc: file too short (%d bytes) for a CISO header", size)
	}
	var hdr [cisoHeaderSize]byte
	if _, err := io.ReadFull(io.NewSectionReader(f, 0, cisoHeaderSize), hdr[:]); err != nil {
		return nil, fmt.Errorf("gcdisc: read CISO header: %w", err)
	}
	if !bytes.Equal(hdr[:4], []byte("CISO")) {
		return nil, fmt.Errorf("gcdisc: bad CISO magic %q", hdr[:4])
	}
	bs := int64(binary.LittleEndian.Uint32(hdr[4:8]))
	if bs == 0 || bs > cisoMaxBlock || bs&(bs-1) != 0 {
		return nil, fmt.Errorf("gcdisc: implausible CISO block size %d", bs)
	}
	prefix := make([]int64, cisoMapLen)
	var present, last int64
	for i := 0; i < cisoMapLen; i++ {
		switch b := hdr[cisoMapOffset+i]; b {
		case 0:
		case 1:
			prefix[i] = present
			present++
			last = int64(i)
		default:
			return nil, fmt.Errorf("gcdisc: CISO block map byte %d is %d, want 0 or 1", i, b)
		}
	}
	if need := cisoDataOffset + present*bs; size < need {
		return nil, fmt.Errorf("gcdisc: file too short (%d bytes) for %d present blocks (%d bytes needed)", size, present, need)
	}
	cr := &cisoReader{
		r:      f,
		bs:     bs,
		prefix: prefix,
		last:   last,
		size:   (last + 1) * bs,
	}
	copy(cr.mapBits[:], hdr[cisoMapOffset:cisoMapOffset+cisoMapLen])
	return cr, nil
}

// Size returns the logical disc size in bytes.
func (c *cisoReader) Size() int64 { return c.size }

// ReadAt reads logical disc bytes. Absent blocks read as zeros, and a
// read that reaches the logical size returns the remaining bytes
// zero-filled with io.EOF; offsets past the logical size return an
// error.
func (c *cisoReader) ReadAt(buf []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("gcdisc: negative offset")
	}
	if off >= c.size {
		if off == c.size {
			return 0, io.EOF
		}
		return 0, fmt.Errorf("gcdisc: offset %d beyond disc size %d", off, c.size)
	}
	n := 0
	for n < len(buf) {
		blk := off / c.bs
		if blk > c.last {
			break // past the logical size: zero-fill the rest
		}
		in := off % c.bs
		take := int64(len(buf) - n)
		if rem := c.bs - in; take > rem {
			take = rem
		}
		if c.mapBits[blk] == 0 {
			clear(buf[n : n+int(take)])
			n += int(take)
			off += take
			continue // absent blocks read as zeros
		}
		m, err := c.r.ReadAt(buf[n:n+int(take)], cisoDataOffset+c.prefix[blk]*c.bs+in)
		if m > 0 {
			n += m
			off += int64(m)
		}
		if err != nil {
			if err != io.EOF {
				return n, err
			}
			break
		}
		if int64(m) < take {
			break
		}
	}
	if n < len(buf) {
		clear(buf[n:])
		return n, io.EOF
	}
	return n, nil
}

// ReadAt returns logical disc bytes. Offsets beyond the disc size read
// as zeros (GameCube discs zero-fill unused regions); offsets past the
// logical size return an error.
func (d *Disc) ReadAt(buf []byte, off int64) (int, error) {
	return d.readAt.ReadAt(buf, off)
}

// Size returns the logical disc size in bytes.
func (d *Disc) Size() int64 { return d.readAt.Size() }

// Header returns the parsed GCM boot header.
func (d *Disc) Header() *Header { return d.hdr }

// Close releases the underlying file.
func (d *Disc) Close() error { return d.f.Close() }

func parseGCMHeader(hdr []byte) (Header, error) {
	if len(hdr) < 0x430 {
		return Header{}, fmt.Errorf("gcdisc: header buffer too short")
	}
	h := Header{
		GameID:    strings.Trim(string(hdr[0:6]), "\x00 "),
		Maker:     strings.Trim(string(hdr[6:8]), "\x00 "),
		FSTOffset: binary.BigEndian.Uint32(hdr[0x424:0x428]),
		FSTSize:   binary.BigEndian.Uint32(hdr[0x428:0x42C]),
		DiscNum:   hdr[0x440],
		DiscTotal: hdr[0x444],
	}
	// The game name sits at 0x10 on most titles; WotW places it at 0x20.
	for _, off := range []int{0x10, 0x20} {
		if s := cstr(hdr[off : off+32]); s != "" {
			h.Name = s
			break
		}
	}
	return h, nil
}

func cstr(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return strings.Trim(string(b), " ")
}
