package windwaker

// synth.go is the synthetic disc factory (ADR-0054's fixture-factory
// semantics, ADR-0066). It assembles a Wind Waker disc image whose message
// archive holds a BMG of *invented* text, so the disc-import path (API, job
// and UI) can be exercised end to end without game material (AGENTS.md rules
// 3 and 5): the committed fixture testdata/disc/synth-windwaker.gcm is this
// function's output for GZLE01, and a test byte-compares the two.

import (
	"context"
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/dbirks/game-narrator/internal/gcdisc"
)

// align32 pads a BMG section to the 32-byte boundary bmg.Parse expects.
func align32(n int) int { return (n + 31) &^ 31 }

// synthBmgMessageIDs are the BMG message ids the synthetic disc uses: one
// per line, starting at 100 the way the adapter tests' fixture does.
func synthBmgMessageIDs(n int) []uint32 {
	ids := make([]uint32, n)
	for i := range ids {
		ids[i] = 100 + uint32(i)
	}
	return ids
}

// synthBMG assembles a BMG of plain-text messages (no control tags): header
// | INF1 | MID1 | DAT1, 32-byte aligned, every message NUL-terminated in the
// pool — the layout bmg.Parse validates. The INF1 entries mirror the measured
// real-disc layout (ADR-0029, ADR-0070): a u32 text offset plus 20 attribute
// bytes, of which attribute byte 0 is the speaker slot — so the synthetic
// disc carries one distinct speaker per line (values 1..4, the fixture's own
// invented values) exactly as the import's speaker mapping reads it.
func synthBMG(ids []uint32, msgs [][]byte) []byte {
	const infEntrySize = 24 // u32 offset + 20 attribute bytes (attribute 0: speaker)
	infSection := make([]byte, align32(8+8+len(ids)*infEntrySize))
	copy(infSection, "INF1")
	binary.BigEndian.PutUint32(infSection[4:8], uint32(len(infSection)))
	inf := infSection[8:]
	binary.BigEndian.PutUint16(inf[0:2], uint16(len(ids)))
	binary.BigEndian.PutUint16(inf[2:4], infEntrySize)
	binary.BigEndian.PutUint32(inf[4:8], 0)
	offsets := make([]uint32, len(ids))
	pool := []byte{}
	for i, m := range msgs {
		offsets[i] = uint32(len(pool))
		pool = append(pool, m...)
		pool = append(pool, 0) // every message contributes its terminator NUL
	}
	for i, off := range offsets {
		base := 8 + i*infEntrySize
		binary.BigEndian.PutUint32(inf[base:base+4], off)
		inf[base+4] = byte(i) + 1 // the speaker slot: one value per line
	}

	midSection := make([]byte, align32(8+8+4*len(ids)))
	copy(midSection, "MID1")
	binary.BigEndian.PutUint32(midSection[4:8], uint32(len(midSection)))
	mid := midSection[8:]
	binary.BigEndian.PutUint16(mid[0:2], uint16(len(ids)))
	binary.BigEndian.PutUint16(mid[2:4], 0)
	binary.BigEndian.PutUint32(mid[4:8], 0)
	for i, id := range ids {
		binary.BigEndian.PutUint32(mid[8+4*i:12+4*i], id)
	}

	datSection := make([]byte, align32(8+len(pool)))
	copy(datSection, "DAT1")
	binary.BigEndian.PutUint32(datSection[4:8], uint32(len(datSection)))
	copy(datSection[8:], pool)

	out := make([]byte, 0x20+len(infSection)+len(midSection)+len(datSection))
	copy(out, "MESGbmg1")
	binary.BigEndian.PutUint32(out[8:12], uint32(len(out)))
	binary.BigEndian.PutUint32(out[12:16], 3)
	out[16] = 1 // 1-byte text encoding
	copy(out[0x20:], infSection)
	copy(out[0x20+len(infSection):], midSection)
	copy(out[0x20+len(infSection)+len(midSection):], datSection)
	return out
}

// SynthDiscImage assembles a synthetic Wind Waker disc image (plain GCM
// logical format) whose res/Msg/bmgres.arc RARC holds one BMG with one
// plain-text message per line. gameID must start with the disc's game-id
// prefix (GZL); the caller's lines are the fixture's own content and must be
// invented text, never game material (AGENTS.md rule 5).
func SynthDiscImage(gameID string, lines []string) ([]byte, error) {
	if len(gameID) != 6 || !strings.HasPrefix(gameID, DiscGameIDPrefix) {
		return nil, fmt.Errorf("windwaker: synthetic disc game id %q must be six characters starting with %s", gameID, DiscGameIDPrefix)
	}
	if len(lines) == 0 {
		return nil, fmt.Errorf("windwaker: the synthetic disc needs at least one line")
	}
	msgs := make([][]byte, len(lines))
	for i, line := range lines {
		msgs[i] = []byte(line)
	}
	bmg := synthBMG(synthBmgMessageIDs(len(lines)), msgs)
	arc := gcdisc.AssembleRARC(map[string][]byte{
		"bmg/bmg0.bmg": bmg,
	})
	// FST: 0 root, 1 res, 2 Msg, 3 the arc file at 0x3000 — the path
	// ExtractBMG reads is res/Msg/bmgres.arc.
	entries := []gcdisc.SynthEntry{
		{IsDir: true, End: 4},
		{IsDir: true, Name: "res", Parent: 0, End: 4},
		{IsDir: true, Name: "Msg", Parent: 1, End: 4},
		{Name: "bmgres.arc", Off: 0x3000, Size: int64(len(arc))},
	}
	return gcdisc.SynthGCMImage(gameID, 0x2000, entries, map[string][]byte{
		"bmgres.arc": arc,
	}), nil
}

// ExtractDiscSource is the adapter's disc entry point for the durable import
// job (ADR-0066): it verifies the image is this adapter's game — the same
// refusal the CLI makes, in the place the adapter owns — and returns the
// message script's bytes under the name the importer records. The disc is
// read in place and never copied.
func (a Adapter) ExtractDiscSource(ctx context.Context, d *gcdisc.Disc) (script []byte, name, gameID string, err error) {
	if err := ctx.Err(); err != nil {
		return nil, "", "", err
	}
	gameID = d.Header().GameID
	if !strings.HasPrefix(gameID, DiscGameIDPrefix) {
		return nil, "", gameID, fmt.Errorf("disc game id %q does not start with %s: the reference adapter only reads Wind Waker images; a different title needs its own adapter", gameID, DiscGameIDPrefix)
	}
	data, err := ExtractBMG(d)
	if err != nil {
		return nil, "", gameID, err
	}
	return data, "bmgres.bmg", gameID, nil
}
