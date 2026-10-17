package bmg

import (
	"fmt"

	"encoding/binary"
)

const (
	headerSize   = 0x20
	headerMagic  = "MESGbmg1"
	sectionAlign = 32
	maxGapScan   = 31 // bounded: at most up to the next 32-byte boundary
)

// beU16/beU32 decode big-endian integers from b, which must be long
// enough. Callers guarantee bounds before calling.
func beU16(b []byte) uint16 { return binary.BigEndian.Uint16(b) }
func beU32(b []byte) uint32 { return binary.BigEndian.Uint32(b) }

// parseHeader validates the fixed 0x20-byte file header and returns the
// section count and the raw encoding byte. The u32 at 0x08 is treated
// as an opaque/reserved field (it does not always equal the true file
// size); only the magic, section count and encoding byte are validated.
func parseHeader(b []byte) (count int, encoding byte, err error) {
	if len(b) < headerSize {
		return 0, 0, fmt.Errorf("bmg: file too short (%d bytes)", len(b))
	}
	if string(b[0:8]) != headerMagic {
		return 0, 0, fmt.Errorf("%w: bad magic %q", ErrNotABMG, b[0:8])
	}
	n := beU32(b[12:16])
	if n > MaxSections {
		return 0, 0, fmt.Errorf("bmg: section count %d exceeds %d", n, MaxSections)
	}
	return int(n), b[16], nil
}

// printable reports whether the 4-byte candidate is a plausible ASCII
// section magic (all printable characters).
func printable4(b []byte) bool {
	for i := 0; i < 4; i++ {
		if b[i] < 0x20 || b[i] > 0x7E {
			return false
		}
	}
	return true
}

// advanceAligned returns the offset of the next plausible section header
// after a non-aligned section end, scanning up to (and including) the
// next 32-byte boundary plus up to maxGapScan bytes. It never scans
// past end and never returns an offset past end-4.
func advanceAligned(b []byte, off, end int) (int, error) {
	start := off / sectionAlign * sectionAlign
	if start < off {
		start += sectionAlign
	}
	limit := start + maxGapScan
	if limit+4 > end {
		limit = end - 4
	}
	for t := start; t <= limit; t++ {
		if printable4(b[t : t+4]) {
			return t, nil
		}
	}
	return 0, fmt.Errorf("bmg: no plausible section header near offset %d", off)
}

// scanSections walks the section table after the file header,
// validating sizes and retaining every section's raw bytes.
func scanSections(b []byte, count int) ([]Section, int, error) {
	secs := make([]Section, 0, count)
	off := headerSize
	for i := 0; i < count; i++ {
		if off%sectionAlign != 0 {
			var err error
			off, err = advanceAligned(b, off, len(b))
			if err != nil {
				return nil, 0, err
			}
		}
		if off+8 > len(b) {
			return nil, 0, fmt.Errorf("bmg: section %d header past EOF at offset %d", i, off)
		}
		magic := string(b[off : off+4])
		if !printable4(b[off : off+4]) {
			return nil, 0, fmt.Errorf("bmg: section %d has non-printable magic at offset %d", i, off)
		}
		size := int(beU32(b[off+4 : off+8]))
		if size < 8 {
			return nil, 0, fmt.Errorf("bmg: section %d size %d is less than 8", i, size)
		}
		if off+size > len(b) {
			return nil, 0, fmt.Errorf("bmg: section %d (%s) size %d extends past EOF", i, magic, size)
		}
		raw := make([]byte, size)
		copy(raw, b[off:off+size])
		secs = append(secs, Section{Magic: magic, Offset: off, Size: size, Raw: raw})
		off += size
	}
	return secs, off, nil
}
