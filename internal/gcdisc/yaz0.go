package gcdisc

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
)

// Yaz0MaxOut is the hard cap on a single Yaz0 decompression
// (decompression-bomb guard).
const Yaz0MaxOut = 64 << 20 // 64 MiB

// IsYaz0 reports whether b starts with the Yaz0 magic.
func IsYaz0(b []byte) bool {
	return len(b) >= 4 && string(b[:4]) == "Yaz0"
}

// Yaz0Decompress decompresses a Yaz0 stream (magic "Yaz0", big-endian
// decompressed size, 8 reserved bytes, then 8-symbol groups with the
// most significant control bit first: bit set = literal byte, bit
// clear = back-reference of two bytes B1 B2 where N = B1>>4 and
// distance R = (B1&0x0F)<<8|B2; N==0 extends the length with a third
// byte L = B3+0x12, otherwise L = N+2; copy length L at distance R+1).
//
// It refuses declared sizes above Yaz0MaxOut or above maxOut (when
// maxOut > 0), refuses back-references that run past the current
// output, and stops exactly at the declared size.
func Yaz0Decompress(r io.Reader, maxOut int64) ([]byte, error) {
	br := bufio.NewReader(r)
	var hdr [16]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return nil, fmt.Errorf("gcdisc: yaz0 header: %w", err)
	}
	if string(hdr[:4]) != "Yaz0" {
		return nil, fmt.Errorf("gcdisc: bad yaz0 magic %q", hdr[:4])
	}
	outsize := int64(binary.BigEndian.Uint32(hdr[4:8]))
	if outsize > Yaz0MaxOut {
		return nil, fmt.Errorf("gcdisc: yaz0 output size %d exceeds cap %d", outsize, Yaz0MaxOut)
	}
	if maxOut > 0 && outsize > maxOut {
		return nil, fmt.Errorf("gcdisc: yaz0 output size %d exceeds maxOut %d", outsize, maxOut)
	}
	out := make([]byte, 0, outsize)
	for int64(len(out)) < outsize {
		c, err := br.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("gcdisc: yaz0 stream truncated: %w", err)
		}
		for bit := 7; bit >= 0; bit-- {
			if int64(len(out)) >= outsize {
				break // trailing bits in the final group
			}
			if c>>uint(bit)&1 != 0 {
				b, err := br.ReadByte()
				if err != nil {
					return nil, fmt.Errorf("gcdisc: yaz0 literal truncated: %w", err)
				}
				out = append(out, b)
				continue
			}
			b1, err := br.ReadByte()
			if err != nil {
				return nil, fmt.Errorf("gcdisc: yaz0 back-reference truncated: %w", err)
			}
			b2, err := br.ReadByte()
			if err != nil {
				return nil, fmt.Errorf("gcdisc: yaz0 back-reference truncated: %w", err)
			}
			n := b1 >> 4
			var l int
			if n == 0 {
				b3, err := br.ReadByte()
				if err != nil {
					return nil, fmt.Errorf("gcdisc: yaz0 extended length truncated: %w", err)
				}
				l = int(b3) + 0x12
			} else {
				l = int(n) + 2
			}
			dist := int(uint16(b1&0x0F)<<8|uint16(b2)) + 1
			if dist <= 0 || dist > len(out) {
				return nil, fmt.Errorf("gcdisc: yaz0 back-reference distance %d exceeds output %d", dist, len(out))
			}
			for k := 0; k < l && int64(len(out)) < outsize; k++ {
				out = append(out, out[len(out)-dist])
			}
		}
	}
	return out, nil
}
