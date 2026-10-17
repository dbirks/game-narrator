package bmg

import (
	"errors"
	"fmt"
)

// infEntry is one parsed INF1 entry: the pool offset plus the raw
// attribute bytes after the offset word (kept opaque per spec).
type infEntry struct {
	TextOffset uint32
	Attributes []byte
}

var (
	errNoINF1 = errors.New("bmg: no INF1 section")
	errNoDAT1 = errors.New("bmg: no DAT1 section")
	errTrunc  = errors.New("bmg: truncated section body")
)

// findSection returns the first section with the given magic, or nil.
func findSection(secs []Section, magic string) *Section {
	for i := range secs {
		if secs[i].Magic == magic {
			return &secs[i]
		}
	}
	return nil
}

// poolBytes returns the DAT1 text pool: everything after the section's
// 8-byte header.
func poolBytes(secs []Section) ([]byte, error) {
	d := findSection(secs, "DAT1")
	if d == nil {
		return nil, errNoDAT1
	}
	return d.Raw[8:], nil
}

// parseINF1 parses the INF1 section body (after the 8-byte section
// header): u16 count | u16 entry size | u32 file id/reserved | entries.
func parseINF1(secs []Section) ([]infEntry, int, error) {
	s := findSection(secs, "INF1")
	if s == nil {
		return nil, 0, errNoINF1
	}
	body := s.Raw[8:]
	if len(body) < 8 {
		return nil, 0, fmt.Errorf("%w: INF1 body %d bytes", errTrunc, len(body))
	}
	count := int(beU16(body[0:2]))
	entrySize := int(beU16(body[2:4]))
	if count > MaxEntries {
		return nil, 0, fmt.Errorf("bmg: INF1 entry count %d exceeds %d", count, MaxEntries)
	}
	if entrySize < MinEntrySize || entrySize > MaxEntrySize {
		return nil, 0, fmt.Errorf("bmg: INF1 entry size %d out of range [%d,%d]", entrySize, MinEntrySize, MaxEntrySize)
	}
	if 8+count*entrySize > len(body) {
		return nil, 0, fmt.Errorf("%w: INF1 holds %d entries of %d bytes but has %d body bytes",
			errTrunc, count, entrySize, len(body)-8)
	}
	entries := make([]infEntry, count)
	for i := 0; i < count; i++ {
		e := body[8+i*entrySize : 8+(i+1)*entrySize]
		entries[i] = infEntry{
			TextOffset: beU32(e[0:4]),
			Attributes: append([]byte(nil), e[MinEntrySize:min(entrySize, len(e))]...),
		}
	}
	return entries, entrySize, nil
}

// parseMID1 parses the optional MID1 section: u16 count | u8 format |
// u8 info | u32 reserved | count x u32 ids. Returns nil (not an error)
// when MID1 is absent or its count does not match the INF1 order, in
// which case callers fall back to index-based ids.
func parseMID1(secs []Section, infCount int) []uint32 {
	s := findSection(secs, "MID1")
	if s == nil {
		return nil
	}
	body := s.Raw[8:]
	if len(body) < 8 {
		return nil
	}
	count := int(beU16(body[0:2]))
	if count > MaxEntries || 8+4*count > len(body) {
		return nil
	}
	if count != infCount {
		return nil // misaligned; keep opaque
	}
	ids := make([]uint32, count)
	for i := 0; i < count; i++ {
		ids[i] = beU32(body[8+4*i : 12+4*i])
	}
	return ids
}
