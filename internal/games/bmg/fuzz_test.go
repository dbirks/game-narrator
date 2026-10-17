package bmg

import (
	"bytes"
	"testing"
)

// FuzzParse must never panic on any input, and whenever Parse
// succeeds every message must satisfy the losslessness property:
// Reconstruct equals Raw exactly, and the tokens tile the raw slice
// without gaps or overlaps.
func FuzzParse(f *testing.F) {
	f.Add(buildSyntheticBMG())
	f.Add([]byte{})
	f.Add([]byte("MESGbmg1"))
	f.Add([]byte("MESGbmg1\x00\x00\x00\x00\x00\x00\x00\x01"))
	// 0x1A-heavy input: tag bytes every 17 positions.
	buf := make([]byte, 128)
	for i := range buf {
		buf[i] = byte(i % 128)
		if i%17 == 0 {
			buf[i] = 0x1A
		}
	}
	f.Add(buf)

	f.Fuzz(func(t *testing.T, b []byte) {
		file, err := Parse(b)
		if err != nil {
			return
		}
		for _, m := range file.Messages {
			if !bytes.Equal(m.Reconstruct(), m.Raw) {
				t.Fatalf("Reconstruct() != Raw for message %d", m.Index)
			}
			pos := 0
			for _, tk := range m.Tokens {
				if tk.Offset != pos || tk.Length != len(tk.Raw) || tk.Length < 0 {
					t.Fatalf("token gap/overlap in message %d at offset %d: %+v", m.Index, pos, tk)
				}
				pos += tk.Length
			}
			if pos != len(m.Raw) {
				t.Fatalf("tokens cover %d of %d bytes in message %d", pos, len(m.Raw), m.Index)
			}
		}
	})
}
