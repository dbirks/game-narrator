package windwaker

// Synthetic speaker tests (ADR-0070). A BMG of invented text with invented
// speaker attribute bytes exercises the adapter's mapping — the key set and
// its first-occurrence order, the sidecar lockstep, the proposed evidence
// payload, the provenance warning, and the degrade-to-no-speaker case for an
// entry without the attribute byte. No game text or game-derived name enters
// the fixture or the assertions (AGENTS.md rule 5): the keys are the adapter's
// own function of the byte values.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/games/adapter"
)

// synthBMGWithSpeakerBytes assembles a BMG in the measured real-disc layout
// (ADR-0029): header | INF1 | MID1 | DAT1, 32-byte aligned, every message
// NUL-terminated in the pool. Each INF1 entry is a u32 text offset plus
// attrBytes attribute bytes (the real disc carries 20; 0 is the degrade
// case), and attribute byte 0 — the slot under test — takes speaker[i].
func synthBMGWithSpeakerBytes(ids []uint32, msgs [][]byte, speaker []byte, attrBytes int) []byte {
	entrySize := 4 + attrBytes
	infSection := make([]byte, align32(8+8+len(ids)*entrySize))
	copy(infSection, "INF1")
	binary.BigEndian.PutUint32(infSection[4:8], uint32(len(infSection)))
	inf := infSection[8:]
	binary.BigEndian.PutUint16(inf[0:2], uint16(len(ids)))
	binary.BigEndian.PutUint16(inf[2:4], uint16(entrySize))
	binary.BigEndian.PutUint32(inf[4:8], 0)

	offsets := make([]uint32, len(ids))
	pool := []byte{}
	for i, m := range msgs {
		offsets[i] = uint32(len(pool))
		pool = append(pool, m...)
		pool = append(pool, 0)
	}
	for i, off := range offsets {
		base := 8 + i*entrySize
		binary.BigEndian.PutUint32(inf[base:base+4], off)
		for a := 0; a < attrBytes; a++ {
			v := byte(0)
			if a == 0 && i < len(speaker) {
				v = speaker[i]
			}
			inf[base+4+a] = v
		}
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
	out[16] = 1
	copy(out[0x20:], infSection)
	copy(out[0x20+len(infSection):], midSection)
	copy(out[0x20+len(infSection)+len(midSection):], datSection)
	return out
}

func speakerOpts() adapter.ParseOptions {
	return adapter.ParseOptions{
		ProfileID: "01ARZ3NDEKTSV4RRFFQ69C5BYW",
		SourceID:  "01ARZ3NDEKTSV4RRFFQ69C5BYX",
		Language:  "en",
		MaxBytes:  adapter.DefaultMaxBytes,
		MaxRows:   adapter.DefaultMaxRows,
	}
}

func parseSynthBMG(t *testing.T, raw []byte) *adapter.ParseResult {
	t.Helper()
	res, err := New().(Adapter).Parse(context.Background(), bytes.NewReader(raw), speakerOpts())
	if err != nil {
		t.Fatalf("parse the synthetic BMG: %v", err)
	}
	return res
}

// TestSpeakerSidecarKeysOrderAndEvidence is the mapping itself: the distinct
// opaque keys in first-occurrence order, the per-row sidecar in lockstep, the
// proposed attribution with its source-field evidence, and the provenance
// warning that carries the counts.
func TestSpeakerSidecarKeysOrderAndEvidence(t *testing.T) {
	lines := []string{
		"invented line one",
		"invented line two",
		"invented line three",
		"invented line four",
	}
	var msgs [][]byte
	for _, l := range lines {
		msgs = append(msgs, []byte(l))
	}
	// Speaker slot values in pool order: 7, 3, 7, 12 — the key set is three,
	// and its order is the order the values first appear.
	raw := synthBMGWithSpeakerBytes([]uint32{100, 101, 102, 103}, msgs, []byte{7, 3, 7, 12}, 20)
	res := parseSynthBMG(t, raw)

	if len(res.Messages) != 4 {
		t.Fatalf("imported %d rows, want the four invented lines", len(res.Messages))
	}
	wantKeys := []string{"ww:spk:7", "ww:spk:3", "ww:spk:7", "ww:spk:12"}
	if len(res.SpeakerKeys) != 4 {
		t.Fatalf("sidecar has %d keys, want one per row", len(res.SpeakerKeys))
	}
	for i, want := range wantKeys {
		if res.SpeakerKeys[i] != want {
			t.Errorf("row %d key = %q, want %q (lockstep with the attribute byte)", i, res.SpeakerKeys[i], want)
		}
	}
	wantSpeakers := []struct {
		key string
	}{{SpeakerKey(7)}, {SpeakerKey(3)}, {SpeakerKey(12)}}
	if len(res.Speakers) != len(wantSpeakers) {
		t.Fatalf("%d distinct speakers, want %d in first-occurrence order", len(res.Speakers), len(wantSpeakers))
	}
	for i, want := range wantSpeakers {
		got := res.Speakers[i]
		if got.Key != want.key || got.DisplayName != want.key {
			t.Errorf("speaker %d = %q / %q, want %q with the id as its own label (no name is invented)", i, got.Key, got.DisplayName, want.key)
		}
		if got.SourceNamed {
			t.Errorf("speaker %d claims a source-supplied name; the key is an opaque id", i)
		}
	}
	if res.SpeakerColumn != SpeakerColumn {
		t.Errorf("speaker column = %q, want %q", res.SpeakerColumn, SpeakerColumn)
	}

	// Every row leaves the parse proposed at full confidence with the
	// source-field evidence: the dump's own linkage, nothing the adapter
	// guessed.
	for i, m := range res.Messages {
		if m.AttributionStatus != games.AttributionProposed {
			t.Errorf("row %d status = %s, want proposed (the identity is a curator's call)", i, m.AttributionStatus)
		}
		if m.AttributionConfidence != 1.0 {
			t.Errorf("row %d confidence = %v, want 1.0 (the slot linkage is certain)", i, m.AttributionConfidence)
		}
		var entries []map[string]any
		if err := json.Unmarshal(m.AttributionEvidence, &entries); err != nil {
			t.Fatalf("row %d evidence does not unmarshal: %v", i, err)
		}
		if len(entries) != 1 {
			t.Fatalf("row %d evidence has %d entries, want the one source-field record", i, len(entries))
		}
		e := entries[0]
		if e["method"] != "source_field" || e["field"] != SpeakerColumn {
			t.Errorf("row %d evidence = %v, want the source_field method naming %s", i, e, SpeakerColumn)
		}
		if e["key"] != wantKeys[i] || e["source_id"] != speakerOpts().SourceID || e["posterior"] != 1.0 {
			t.Errorf("row %d evidence = %v, want the key, the source id and posterior 1.0", i, e)
		}
	}

	// The provenance marker: counts only, so the import report can say where
	// the attribution came from without a single line of game text.
	var marker *adapter.Warning
	for i := range res.Warnings {
		if res.Warnings[i].Column == "speakers" {
			marker = &res.Warnings[i]
		}
	}
	if marker == nil {
		t.Fatal("no provenance warning with column speakers")
	}
	for _, want := range []string{SpeakerColumn, "3 distinct", "4 imported"} {
		if !strings.Contains(marker.Msg, want) {
			t.Errorf("marker %q does not carry %q", marker.Msg, want)
		}
	}
}

// TestSpeakerEntryWithoutTheByteDegrades is the bounds case: an INF1 entry
// that carries no attribute byte yields no speaker for its row — degrade to
// "no speaker", never guess.
func TestSpeakerEntryWithoutTheByteDegrades(t *testing.T) {
	lines := []string{"invented line one", "invented line two"}
	var msgs [][]byte
	for _, l := range lines {
		msgs = append(msgs, []byte(l))
	}
	raw := synthBMGWithSpeakerBytes([]uint32{100, 101}, msgs, []byte{7, 8}, 0)
	res := parseSynthBMG(t, raw)

	if len(res.Messages) != 2 {
		t.Fatalf("imported %d rows, want two", len(res.Messages))
	}
	if len(res.Speakers) != 0 || len(res.SpeakerKeys) != 2 || res.SpeakerColumn != "" {
		t.Fatalf("the byte-less BMG reported speakers %v (column %q); want none", res.Speakers, res.SpeakerColumn)
	}
	for i, m := range res.Messages {
		if res.SpeakerKeys[i] != "" {
			t.Errorf("row %d key = %q, want empty (no byte, no speaker)", i, res.SpeakerKeys[i])
		}
		if m.AttributionStatus != games.AttributionUnknown {
			t.Errorf("row %d status = %s, want unknown", i, m.AttributionStatus)
		}
	}
	for _, w := range res.Warnings {
		if w.Column == "speakers" {
			t.Errorf("the byte-less parse emitted the provenance marker: %q", w.Msg)
		}
	}
}

// TestSpeakerSkippedLineRegistersNoSpeaker: an entry whose text normalizes to
// nothing is skipped with a count, and its speaker slot must not become an
// unresolvable roster entry.
func TestSpeakerSkippedLineRegistersNoSpeaker(t *testing.T) {
	msgs := [][]byte{
		[]byte("invented line one"),
		[]byte("invented line two"),
		{}, // empty: the pool holds only the terminator NUL
	}
	raw := synthBMGWithSpeakerBytes([]uint32{100, 101, 102}, msgs, []byte{7, 3, 99}, 20)
	res := parseSynthBMG(t, raw)

	if len(res.Messages) != 2 {
		t.Fatalf("imported %d rows, want the two non-empty lines", len(res.Messages))
	}
	if len(res.Speakers) != 2 {
		t.Fatalf("%d distinct speakers, want the two the non-empty lines carry", len(res.Speakers))
	}
	for _, spk := range res.Speakers {
		if spk.Key == SpeakerKey(99) {
			t.Fatalf("the skipped line's speaker %s became a roster entry", spk.Key)
		}
	}
}
