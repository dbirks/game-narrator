package dolphin

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
)

// The Dolphin 2609 savestate file header (Source/Core/Core/State.h, the
// 2609 tag — measured against this host's installed dolphin-emu build and
// the fixture states' bytes; all fields little-endian on the x86-64 host):
//
//	off  size  field
//	0    6    game_id (the booted title's ID, "GZLE01" here)
//	6    2    reserved1
//	8    4    lzo_size (0 for new states)
//	12   4    reserved2
//	16   8    time (double, seconds since epoch)
//	24   4    version_cookie (u32)
//	28   4    version_string_length (u32)
//	32   N    version_string ("Dolphin 2609")
//	32+N 2    header_version (u16, EXTENDED_HEADER_VERSION = 1)
//	34+N 2    compression_type (u16: 0 uncompressed, 1 LZ4)
//	36+N 4    payload_offset (u32)
//	40+N 8    uncompressed_size (u64)
//
// Issue #12's failure mode is a state load that Dolphin silently ignores:
// 2609's State.cpp (LoadAsFromCore) falls back to the PRE-LOAD state — the
// fresh boot's copyright/title screen — when the file cannot be opened, the
// header fails, ValidateHeaders rejects the game ID / version cookie, or
// the LZ4 payload does not decompress. None of those paths prints to the
// bridge's stdout log in --batch mode; the on-screen OSD message is a
// 2-second toast the screenshot cadence can miss. The header parse here is
// the harness-side oracle: a saved state whose header does not name the
// SAME game and the SAME Dolphin state version as the boot cannot load,
// and the parse says so before a 20-minute boot is spent on it.
//
// stateVersionCookie is COOKIE_BASE + STATE_VERSION (2609: 0xBAADBABE +
// 192). The cookie, not the revision string, is the compatibility check
// 2609's ValidateHeaders performs (the string is informational; the
// cookie is the version gate), so the harness compares the cookie.
const stateVersionCookie = 0xBAADBABE + 192

// StateHeaderInfo is the parsed savestate header: the identity facts a
// --save_state boot needs (the game the state belongs to and the Dolphin
// state version that wrote it), plus the payload facts a corrupt-file
// diagnosis needs (the compression and the uncompressed size, so a
// truncated or wrongly-sized file is named without decompressing it).
type StateHeaderInfo struct {
	GameID           string
	Time             float64 // seconds since epoch (the save time)
	VersionCookie    uint32
	VersionString    string
	HeaderVersion    uint16
	Compression      uint16 // 0 = uncompressed, 1 = LZ4 (2609's enum)
	UncompressedSize uint64
	FileSize         int64
}

// ParseStateHeader reads and validates the savestate header at path. The
// validation is the harness-side half of 2609's ValidateHeaders: the game
// ID must name wantGameID (the state is only loadable by the SAME game)
// and the version cookie must be stateVersionCookie (the SAME Dolphin
// state version — a mismatch is exactly the silent-fallback failure of
// issue #12's suspect list). The version string is carried for the logs
// (the human-readable "Dolphin 2609"), not compared: 2609 compares the
// cookie. The extended-header fields are parsed for the payload facts
// (truncation diagnosis), not validated against 2609's constants: this
// harness runs against the pinned 2609 install, whose cookie the version
// check already pins.
func ParseStateHeader(path, wantGameID string) (StateHeaderInfo, error) {
	var info StateHeaderInfo
	raw, err := os.ReadFile(path)
	if err != nil {
		return info, fmt.Errorf("dolphin: read state header %s: %w", path, err)
	}
	if len(raw) < 48 {
		return info, fmt.Errorf("dolphin: state %s is %d bytes — shorter than the 48-byte fixed+extended header (truncated file?)", path, len(raw))
	}
	info.FileSize = int64(len(raw))
	if g := string(raw[0:6]); g != wantGameID {
		return info, fmt.Errorf("dolphin: state %s belongs to game %q, want %q (2609 refuses a different game's state with a toast and the fresh-boot fallback — the silent failure of issue #12)", path, g, wantGameID)
	}
	info.GameID = wantGameID
	info.Time = math.Float64frombits(binary.LittleEndian.Uint64(raw[16:24]))
	info.VersionCookie = binary.LittleEndian.Uint32(raw[24:28])
	if info.VersionCookie != stateVersionCookie {
		return info, fmt.Errorf("dolphin: state %s carries version cookie %#010x, want %#010x (the Dolphin state version differs — 2609's ValidateHeaders rejects the load and the boot falls back to the fresh-boot screen)", path, info.VersionCookie, stateVersionCookie)
	}
	n := binary.LittleEndian.Uint32(raw[28:32])
	if uint64(n) > uint64(len(raw)-32) {
		return info, fmt.Errorf("dolphin: state %s claims a %d-byte version string but holds %d header bytes (corrupt header)", path, n, len(raw)-32)
	}
	info.VersionString = string(raw[32 : 32+int(n)])
	ext := 32 + int(n)
	if len(raw) < ext+16 {
		return info, fmt.Errorf("dolphin: state %s is %d bytes — the extended header needs %d (truncated file?)", path, len(raw), ext+16)
	}
	info.HeaderVersion = binary.LittleEndian.Uint16(raw[ext : ext+2])
	info.Compression = binary.LittleEndian.Uint16(raw[ext+2 : ext+4])
	info.UncompressedSize = binary.LittleEndian.Uint64(raw[ext+8 : ext+16])
	if info.FileSize < int64(ext+16)+1 {
		return info, fmt.Errorf("dolphin: state %s carries no payload (header only — truncated file?)", path)
	}
	return info, nil
}
