package bmg

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// be16/be32 write big-endian words into the provided slice.
func be16(b []byte, v uint16) { binary.BigEndian.PutUint16(b, v) }
func be32(b []byte, v uint32) { binary.BigEndian.PutUint32(b, v) }

// buildSyntheticBMG constructs a valid in-memory BMG with 32-byte
// aligned sections:
//
//	header(0x20) | INF1@0x20(64) | MID1@0x60(32) | DAT1@0x80(40)
//
// INF1 has 3 entries of size 8 (u32 pool offset + 4 attribute bytes).
// The DAT1 pool holds three NUL-terminated messages:
//   - 0: plain text, no tags
//   - 1: text with a well-formed 0x1A tag in the middle whose body
//     contains zero bytes (which must not terminate the message)
//   - 2: text with a truncated tag (declared length runs past end)
func buildSyntheticBMG() []byte {
	msg0 := []byte("HELLO")
	tag := []byte{0x1A, 0x06, 0xFF, 0x00, 0x00, 0x01} // L=6, zero bytes in body
	msg1 := append(append([]byte("AB"), tag...), []byte("CD")...)
	msg2 := []byte{'X', 'Y', 0x1A, 0x09, 'Z'} // L=9 but only 3 bytes follow

	offsets := make([]uint32, 3)
	pool := make([]byte, 0, 32)
	for i, m := range [][]byte{msg0, msg1, msg2} {
		offsets[i] = uint32(len(pool))
		pool = append(pool, m...)
		pool = append(pool, 0)
	}

	// INF1 body: u16 count | u16 entry size | u32 file id | entries.
	inf := make([]byte, 8+3*8)
	be16(inf[0:2], 3)
	be16(inf[2:4], 8)
	be32(inf[4:8], 0x1234) // file id / reserved, arbitrary
	for i := 0; i < 3; i++ {
		be32(inf[8+i*8:12+i*8], offsets[i])
		copy(inf[12+i*8:16+i*8], []byte{byte('A' + i), 1, 2, 3})
	}
	infSection := make([]byte, 64)
	copy(infSection, "INF1")
	be32(infSection[4:8], 64)
	copy(infSection[8:], inf)

	// MID1 body: u16 count | u8 format | u8 info | u32 reserved | ids.
	mid := make([]byte, 8+3*4)
	be16(mid[0:2], 3)
	mid[2] = 0x01
	mid[3] = 0x02
	// mid[4:8] reserved (zero)
	be32(mid[8:12], 100)
	be32(mid[12:16], 200)
	be32(mid[16:20], 300)
	midSection := make([]byte, 32)
	copy(midSection, "MID1")
	be32(midSection[4:8], 32)
	copy(midSection[8:], mid)

	// DAT1 section: 8-byte header + pool (23 bytes) + zero padding.
	datSection := make([]byte, 40)
	copy(datSection, "DAT1")
	be32(datSection[4:8], 40)
	copy(datSection[8:], pool)

	// File header.
	out := make([]byte, 0xA8)
	copy(out, "MESGbmg1")
	be32(out[8:12], 0xA8) // opaque reserved field (equals true size here)
	be32(out[12:16], 3)   // section count
	out[16] = 1           // encoding byte (1-byte legacy)
	copy(out[0x20:], infSection)
	copy(out[0x60:], midSection)
	copy(out[0x80:], datSection)
	return out
}

func TestParseSynthetic(t *testing.T) {
	f, err := Parse(buildSyntheticBMG())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if f.Encoding != 1 {
		t.Errorf("Encoding = %d, want 1", f.Encoding)
	}
	wantSecs := []struct {
		magic string
		off   int
		size  int
	}{{"INF1", 0x20, 64}, {"MID1", 0x60, 32}, {"DAT1", 0x80, 40}}
	if len(f.Sections) != len(wantSecs) {
		t.Fatalf("got %d sections, want %d", len(f.Sections), len(wantSecs))
	}
	for i, w := range wantSecs {
		s := f.Sections[i]
		if s.Magic != w.magic || s.Offset != w.off || s.Size != w.size {
			t.Errorf("section %d = {%s %d %d}, want {%s %d %d}", i, s.Magic, s.Offset, s.Size, w.magic, w.off, w.size)
		}
		if len(s.Raw) != s.Size {
			t.Errorf("section %d Raw len = %d, want %d", i, len(s.Raw), s.Size)
		}
	}

	if len(f.Messages) != 3 {
		t.Fatalf("got %d messages, want 3", len(f.Messages))
	}
	m0, m1, m2 := f.Messages[0], f.Messages[1], f.Messages[2]

	// IDs come from MID1.
	if m0.ID != 100 || m1.ID != 200 || m2.ID != 300 {
		t.Errorf("IDs = %d %d %d, want 100 200 300", m0.ID, m1.ID, m2.ID)
	}
	if m0.TextOffset != 0 || m1.TextOffset != 6 || m2.TextOffset != 17 {
		t.Errorf("TextOffsets = %d %d %d, want 0 6 17", m0.TextOffset, m1.TextOffset, m2.TextOffset)
	}
	if !bytes.Equal(m0.Raw, []byte("HELLO")) || !bytes.Equal(m2.Raw, []byte{'X', 'Y', 0x1A, 0x09, 'Z'}) {
		t.Errorf("raw slices wrong: %q / % x", m0.Raw, m2.Raw)
	}
	if len(m1.Raw) != 10 {
		t.Errorf("msg1 Raw len = %d, want 10 (zero bytes inside tag must not terminate the message)", len(m1.Raw))
	}
	// Attributes are the raw 4 bytes after the offset word.
	if !bytes.Equal(m0.Attributes, []byte{'A', 1, 2, 3}) ||
		!bytes.Equal(m2.Attributes, []byte{'C', 1, 2, 3}) {
		t.Errorf("Attributes wrong: % x / % x", m0.Attributes, m2.Attributes)
	}

	// Message 0: one text token.
	if len(m0.Tokens) != 1 || m0.Tokens[0].Kind != TokText ||
		m0.Tokens[0].Offset != 0 || m0.Tokens[0].Length != 5 {
		t.Errorf("msg0 tokens = %+v", m0.Tokens)
	}

	// Message 1: text, tag, text.
	if len(m1.Tokens) != 3 {
		t.Fatalf("msg1 tokens = %d, want 3: %+v", len(m1.Tokens), m1.Tokens)
	}
	tk := m1.Tokens
	if tk[0].Kind != TokText || tk[0].Offset != 0 || tk[0].Length != 2 {
		t.Errorf("msg1 token0 = %+v", tk[0])
	}
	if tk[1].Kind != TokTag || tk[1].Offset != 2 || tk[1].Length != 6 ||
		tk[1].Group != 0xFF || tk[1].Type != 0 {
		t.Errorf("msg1 token1 = %+v", tk[1])
	}
	if !bytes.Equal(tk[1].Args, []byte{0xFF, 0x00, 0x00, 0x01}) {
		t.Errorf("msg1 token1 Args = % x, want ff 00 00 01", tk[1].Args)
	}
	if tk[2].Kind != TokText || tk[2].Offset != 8 || tk[2].Length != 2 {
		t.Errorf("msg1 token2 = %+v", tk[2])
	}

	// Message 2: text, then the truncated tag kept as Invalid.
	if len(m2.Tokens) != 2 {
		t.Fatalf("msg2 tokens = %d, want 2: %+v", len(m2.Tokens), m2.Tokens)
	}
	if m2.Tokens[0].Kind != TokText || m2.Tokens[0].Length != 2 {
		t.Errorf("msg2 token0 = %+v", m2.Tokens[0])
	}
	if m2.Tokens[1].Kind != TokInvalid || m2.Tokens[1].Offset != 2 || m2.Tokens[1].Length != 3 {
		t.Errorf("msg2 token1 = %+v, want Invalid covering the truncated tag", m2.Tokens[1])
	}

	// Losslessness: every message reconstructs exactly.
	for _, m := range f.Messages {
		if !bytes.Equal(m.Reconstruct(), m.Raw) {
			t.Errorf("message %d: Reconstruct() != Raw", m.Index)
		}
	}
}

func TestParseHostile(t *testing.T) {
	base := buildSyntheticBMG()
	cases := []struct {
		name    string
		mutate  func(b []byte) []byte
		wantErr error
	}{
		{"wrong_magic", func(b []byte) []byte { b[0] = 'X'; return b }, ErrNotABMG},
		{"file_too_short", func(b []byte) []byte { return b[:16] }, nil},
		{"section_past_eof", func(b []byte) []byte { be32(b[0x24:0x28], 0x10000000); return b }, nil},
		{"entry_offset_past_dat1", func(b []byte) []byte { be32(b[0x28:0x2C], 0xFFFF); return b }, nil},
		{"section_count_huge", func(b []byte) []byte { be32(b[0x0C:0x10], 1000); return b }, nil},
		{"inf_count_huge", func(b []byte) []byte { be16(b[0x28:0x2A], 65535); return b }, nil},
		{"inf_entry_size_too_small", func(b []byte) []byte { be16(b[0x2A:0x2C], 3); return b }, nil},
		{"inf_entry_size_too_large", func(b []byte) []byte { be16(b[0x2A:0x2C], 65); return b }, nil},
		{"missing_dat1", func(b []byte) []byte { copy(b[0x80:0x84], "XXXX"); return b }, nil},
	}
	for _, tc := range cases {
		b := append([]byte(nil), base...)
		b = tc.mutate(b)
		_, err := Parse(b)
		if err == nil {
			t.Errorf("%s: Parse succeeded, want error", tc.name)
			continue
		}
		if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
			t.Errorf("%s: error %v, want %v", tc.name, err, tc.wantErr)
		}
	}
}
