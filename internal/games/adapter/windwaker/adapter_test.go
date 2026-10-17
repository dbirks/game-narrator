package windwaker

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/games/adapter"
)

// ── synthetic BMG builder ──────────────────────────────────────────────────
//
// buildTestBMG assembles an in-memory BMG with the layout the bmg package
// validates (header | INF1 | MID1 | DAT1, sections 32-byte aligned):
//
//	id 100: plain text, no tags
//	id 200: text with a player-name tag (0,0), a pause tag (0,7), the A
//	         button (0,10), an emphasis pair (255,0), an unclassified tag
//	         (5,99) and a 0x0A line break inside a text run
//	id 300: empty message (its offset lands on the pool NUL that terminates
//	         it, so the token stream is empty)
//	id 400: tags only, no text
//	id 500: text plus a truncated tag (declared length past the message end)
//
// Every string here is a fixture of single letters — no game text.

func be16(b []byte, v uint16) { binary.BigEndian.PutUint16(b, v) }
func be32(b []byte, v uint32) { binary.BigEndian.PutUint32(b, v) }

func align32(n int) int { return (n + 31) &^ 31 }

// tag builds one control-tag byte sequence: 0x1A, length, group, u16 type,
// extra args.
func tag(group, typ int, extra ...byte) []byte {
	body := []byte{byte(group)}
	if typ > 0 {
		body = append(body, byte(typ>>8), byte(typ))
	} else if len(body) < 3 {
		body = append(body, 0, 0)
	}
	body = append(body, extra...)
	out := []byte{0x1A, byte(2 + len(body))}
	return append(out, body...)
}

func buildTestBMG() (bmgb []byte, raws map[uint32][]byte) {
	raws = map[uint32][]byte{}
	msg100 := []byte("HELLO")
	msg200 := []byte("A")
	for _, part := range [][]byte{
		tag(0, 0),
		[]byte(" B"),
		{0x0A}, // display line break inside a text run
		tag(0, 7, 0x00, 0x0A),
		[]byte("C"),
		tag(0, 10),
		[]byte(" D"),
		tag(255, 0, 0x01),
		[]byte("E"),
		tag(255, 0),
		[]byte("F"),
		tag(5, 99),
		[]byte("G"),
	} {
		msg200 = append(msg200, part...)
	}
	msg400 := []byte{}
	msg400 = append(msg400, tag(0, 1)...)
	msg400 = append(msg400, tag(0, 2)...)
	msg500 := []byte("X")
	msg500 = append(msg500, 0x1A, 0x09, 'Z') // declared L=9, one body byte

	ids := []uint32{100, 200, 300, 400, 500}
	msgs := [][]byte{msg100, msg200, nil, msg400, msg500} // 300 is empty

	// Every message, empty or not, contributes its terminator NUL to the
	// pool, so an empty message's offset lands exactly on a NUL byte.
	offsets := make([]uint32, len(ids))
	pool := []byte{}
	for i, m := range msgs {
		offsets[i] = uint32(len(pool))
		if m != nil {
			pool = append(pool, m...)
			raws[ids[i]] = append([]byte(nil), m...)
		}
		pool = append(pool, 0)
	}

	infSection := make([]byte, align32(8+8+len(ids)*8))
	copy(infSection, "INF1")
	be32(infSection[4:8], uint32(len(infSection)))
	inf := infSection[8:]
	be16(inf[0:2], uint16(len(ids)))
	be16(inf[2:4], 8) // entry size: 4-byte offset + 4-byte attribute word
	be32(inf[4:8], 0)
	for i, off := range offsets {
		be32(inf[8+i*8:12+i*8], off)
		be32(inf[12+i*8:16+i*8], uint32(i)+1) // attributes < 2^16
	}

	midSection := make([]byte, align32(8+8+4*len(ids)))
	copy(midSection, "MID1")
	be32(midSection[4:8], uint32(len(midSection)))
	mid := midSection[8:]
	be16(mid[0:2], uint16(len(ids)))
	be16(mid[2:4], 0)
	be32(mid[4:8], 0)
	for i, id := range ids {
		be32(mid[8+4*i:12+4*i], id)
	}

	datSection := make([]byte, align32(8+len(pool)))
	copy(datSection, "DAT1")
	be32(datSection[4:8], uint32(len(datSection)))
	copy(datSection[8:], pool)

	out := make([]byte, 0x20+len(infSection)+len(midSection)+len(datSection))
	copy(out, "MESGbmg1")
	be32(out[8:12], uint32(len(out)))
	be32(out[12:16], 3)
	out[16] = 1 // 1-byte text encoding
	copy(out[0x20:], infSection)
	copy(out[0x20+len(infSection):], midSection)
	copy(out[0x20+len(infSection)+len(midSection):], datSection)
	return out, raws
}

// ── tests ──────────────────────────────────────────────────────────────────

func parseSynthetic(t *testing.T) (*adapter.ParseResult, map[uint32][]byte) {
	t.Helper()
	data, raws := buildTestBMG()
	a := New().(Adapter)
	opts := adapter.ParseOptions{
		ProfileID: "01ARZ3NDEKTSV4RRFFQ69C5BYW",
		SourceID:  "01ARZ3NDEKTSV4RRFFQ69C5BYX",
		Language:  "en",
		MaxBytes:  adapter.DefaultMaxBytes,
		MaxRows:   adapter.DefaultMaxRows,
	}
	res, err := a.Parse(context.Background(), bytes.NewReader(data), opts)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return res, raws
}

func TestParseSyntheticMapping(t *testing.T) {
	res, _ := parseSynthetic(t)

	if res.Stats.Rows != 5 || res.Stats.Imported != 3 || res.Stats.Skipped != 2 {
		t.Errorf("stats = %+v, want rows=5 imported=3 skipped=2", res.Stats)
	}
	if len(res.Messages) != 3 {
		t.Fatalf("got %d messages, want 3", len(res.Messages))
	}
	byID := map[string]games.Message{}
	for _, m := range res.Messages {
		byID[m.SourceMessageID] = m
	}
	for _, want := range []string{"100", "200", "500"} {
		if _, ok := byID[want]; !ok {
			t.Fatalf("message %s missing; got %v", want, sortedKeys(byID))
		}
	}

	m100 := byID["100"]
	if m100.OriginalText != "HELLO" {
		t.Errorf("100 original = %q, want %q", m100.OriginalText, "HELLO")
	}
	if m100.NormalizedText != "hello" {
		t.Errorf("100 normalized = %q, want %q", m100.NormalizedText, "hello")
	}
	if m100.SpeechText != "HELLO" {
		t.Errorf("100 speech = %q, want %q", m100.SpeechText, "HELLO")
	}
	if m100.TextKind != games.TextDialogue {
		t.Errorf("100 kind = %q, want dialogue", m100.TextKind)
	}
	if !strings.Contains(string(m100.MarkupJSON), `"kind":"text"`) {
		t.Errorf("100 markup lacks text tokens: %s", m100.MarkupJSON)
	}

	m200 := byID["200"]
	wantOriginal := "A{tag g=0 t=0 a=000000} B\\x0a{tag g=0 t=7 a=000007000a}C{tag g=0 t=10 a=00000a} D{tag g=255 t=0 a=ff000001}E{tag g=255 t=0 a=ff0000}F{tag g=5 t=99 a=050063}G"
	if m200.OriginalText != wantOriginal {
		t.Errorf("200 original = %q\nwant          %q", m200.OriginalText, wantOriginal)
	}
	if m200.NormalizedText != "a b c defg" {
		t.Errorf("200 normalized = %q, want %q", m200.NormalizedText, "a b c defg")
	}
	if m200.SpeechText != "A{player} B [pause:250ms]CA button DEFG" {
		t.Errorf("200 speech = %q, want %q", m200.SpeechText, "A{player} B [pause:250ms]CA button DEFG")
	}

	m500 := byID["500"]
	if m500.OriginalText != "X{tag bad r=1a095a}" {
		t.Errorf("500 original = %q, want %q", m500.OriginalText, "X{tag bad r=1a095a}")
	}
	if !strings.Contains(string(m500.MarkupJSON), `"kind":"invalid"`) {
		t.Errorf("500 markup lacks the invalid token: %s", m500.MarkupJSON)
	}
}

func TestOriginalTextRoundTripsToExactBytes(t *testing.T) {
	res, raws := parseSynthetic(t)
	if len(res.Messages) == 0 {
		t.Fatal("no messages parsed")
	}
	for _, m := range res.Messages {
		want := rawBytesFor(res, raws, m)
		got, err := encodeOriginal(m.OriginalText)
		if err != nil {
			t.Fatalf("source %s: encode: %v", m.SourceMessageID, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("source %s: re-encoded bytes != raw pool bytes:\n got % X\nwant % X", m.SourceMessageID, got, want)
		}
	}
}

func rawBytesFor(_ *adapter.ParseResult, raws map[uint32][]byte, m games.Message) []byte {
	var id uint32
	for _, c := range m.SourceMessageID {
		id = id*10 + uint32(c-'0')
	}
	return raws[id]
}

// TestEscapeGrammarIsAnIsomorphism checks the escape on the full byte range
// (text runs, no tags): for every byte value the escape text re-encodes to
// the exact same bytes.
func TestEscapeGrammarIsAnIsomorphism(t *testing.T) {
	all := make([]byte, 256)
	for i := range all {
		all[i] = byte(i)
	}
	for _, chunk := range [][]byte{all, []byte("plain ascii and { } \\ chars"), {0x0A, 0x1A, 0x00, 0x7F, 0x93, 0xA0}} {
		s := escapeTextBytes(chunk)
		back, err := encodeOriginal(s)
		if err != nil {
			t.Fatalf("re-encode of escaped %d-byte text: %v", len(chunk), err)
		}
		if !bytes.Equal(back, chunk) {
			t.Fatalf("escape isomorphism failed at % X", chunk)
		}
	}
}

func TestEncodeOriginalRejectsGarbage(t *testing.T) {
	for _, bad := range []string{
		"trailing backslash \\",
		"\\x1g not hex",
		"\u4e2d cjk has no byte",
		"{tag g=1 t=2 a=zz}",
		"{tag bad r=}",
		"{tag g=xx t=1 a=01}",
	} {
		if _, err := encodeOriginal(bad); err == nil {
			t.Errorf("encode %q: expected an error", bad)
		}
	}
}

// escapeTextBytes escapes raw text-run bytes the way decodeOriginal does for
// text tokens (escape.go's text half, without tags).
func escapeTextBytes(b []byte) string {
	var out strings.Builder
	for _, c := range b {
		switch {
		case c < 0x20:
			fmt.Fprintf(&out, "\\x%02x", c)
		case c == 0x7F:
			out.WriteString("\\x7f")
		case c >= 0x20 && c <= 0x7E:
			switch c {
			case '\\':
				out.WriteString(`\\`)
			case '{':
				out.WriteString(`\{`)
			case '}':
				out.WriteString(`\}`)
			default:
				out.WriteByte(c)
			}
		case c >= 0x80 && c <= 0x9F:
			if r, ok := cp1252[c]; ok {
				out.WriteRune(r)
			} else {
				fmt.Fprintf(&out, "\\x%02x", c)
			}
		default:
			out.WriteRune(rune(c))
		}
	}
	return out.String()
}

func TestDetect(t *testing.T) {
	a := New().(Adapter)
	data, _ := buildTestBMG()

	if c := a.Detect(context.Background(), "whatever.bin", data[:64]); c != adapter.ConfidentExact {
		t.Errorf("BMG head confidence = %v, want ConfidentExact", c)
	}
	magicOnly := make([]byte, 36)
	copy(magicOnly, "MESGbmg1")
	if c := a.Detect(context.Background(), "x", magicOnly); c != adapter.ConfidentFormat {
		t.Errorf("magic-only confidence = %v, want ConfidentFormat", c)
	}
	disc := make([]byte, 0x40)
	copy(disc[0x20:0x26], "GZLE01")
	if c := a.Detect(context.Background(), "disc.bin", disc); c != adapter.ConfidentExact {
		t.Errorf("disc head confidence = %v, want ConfidentExact", c)
	}
	if c := a.Detect(context.Background(), "windwaker.ciso", nil); c != adapter.ConfidentName {
		t.Errorf(".ciso name confidence = %v, want ConfidentName", c)
	}
	if c := a.Detect(context.Background(), "notes.txt", []byte("hello world")); c != adapter.ConfidentNone {
		t.Errorf("unknown source confidence = %v, want ConfidentNone", c)
	}
}

func TestParseResultCarriesRulesRevision(t *testing.T) {
	res, _ := parseSynthetic(t)
	if len(res.Warnings) < 2 {
		t.Fatalf("warnings = %+v, want at least the rules marker and the graph note", res.Warnings)
	}
	if res.Warnings[0].Column != "rules" {
		t.Errorf("first warning column = %q, want \"rules\"", res.Warnings[0].Column)
	}
	if !strings.Contains(res.Warnings[0].Msg, RulesRevision) {
		t.Errorf("rules warning lacks the revision: %q", res.Warnings[0].Msg)
	}
	found := false
	for _, w := range res.Warnings {
		if w.Column == "graph" && w.Msg == GraphNote {
			found = true
		}
	}
	if !found {
		t.Errorf("graph note %q missing from warnings: %+v", GraphNote, res.Warnings)
	}
}

func TestManifestIsValid(t *testing.T) {
	a := New()
	m := a.Manifest()
	if m.ID != ID || m.Version != Version {
		t.Errorf("manifest identity = %s@%s, want %s@%s", m.ID, m.Version, ID, Version)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("manifest invalid: %v", err)
	}
	reg := adapter.NewRegistry()
	if err := Register(reg); err != nil {
		t.Fatalf("register: %v", err)
	}
	got, ok := reg.Get(ID)
	if !ok || got.Manifest().ID != ID {
		t.Fatalf("registry lookup for %s failed", ID)
	}
}

func TestMarkupJSONTilesTheOriginalBytes(t *testing.T) {
	res, _ := parseSynthetic(t)
	for _, m := range res.Messages {
		var toks []map[string]any
		if err := json.Unmarshal(m.MarkupJSON, &toks); err != nil {
			t.Fatalf("source %s: markup is not a JSON array: %v", m.SourceMessageID, err)
		}
		if len(toks) == 0 {
			t.Fatalf("source %s: markup array is empty", m.SourceMessageID)
		}
		var end int
		for i, tk := range toks {
			off, _ := tk["offset"].(float64)
			ln, _ := tk["length"].(float64)
			if int(off) != end {
				t.Fatalf("source %s: token %d offset %d != running %d", m.SourceMessageID, i, int(off), end)
			}
			end += int(ln)
		}
	}
}

func TestParseRejectsOversizedAndWrongInput(t *testing.T) {
	a := New().(Adapter)
	data, _ := buildTestBMG()
	opts := adapter.ParseOptions{
		ProfileID: "01ARZ3NDEKTSV4RRFFQ69C5BYW",
		MaxBytes:  int64(len(data)) - 1,
		MaxRows:   adapter.DefaultMaxRows,
	}
	if _, err := a.Parse(context.Background(), bytes.NewReader(data), opts); err == nil {
		t.Error("oversized input: expected ErrTooLarge")
	}
	opts = adapter.ParseOptions{
		ProfileID: "01ARZ3NDEKTSV4RRFFQ69C5BYW",
		MaxBytes:  adapter.DefaultMaxBytes,
		MaxRows:   2,
	}
	if _, err := a.Parse(context.Background(), bytes.NewReader(data), opts); err == nil {
		t.Error("row limit exceeded: expected ErrTooManyRows")
	}
	bad := []byte("NOTABMG0000000000000000000")
	opts = adapter.ParseOptions{
		ProfileID: "01ARZ3NDEKTSV4RRFFQ69C5BYW",
		MaxBytes:  adapter.DefaultMaxBytes,
		MaxRows:   adapter.DefaultMaxRows,
	}
	if _, err := a.Parse(context.Background(), bytes.NewReader(bad), opts); err == nil {
		t.Error("non-BMG bytes: expected an error")
	}
}

func sortedKeys(m map[string]games.Message) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
