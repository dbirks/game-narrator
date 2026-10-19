package windwaker

// Real-disc tests. These are NOT synthetic: they parse the user's actual
// Wind Waker CISO (ADR-0029) when it is present on this machine and assert
// the properties the import relies on. When the disc is absent the test
// skips — it is an integration check against user data, not a unit test, so
// an absent fixture must not read as a pass.

import (
	"context"
	"os"
	"testing"

	"github.com/dbirks/game-narrator/internal/games/adapter"
	"github.com/dbirks/game-narrator/internal/games/bmg"
	"github.com/dbirks/game-narrator/internal/gcdisc"
)

// realDiscPath returns the configured disc image path or skips the test.
func realDiscPath(t *testing.T) string {
	t.Helper()
	p := os.Getenv("GAME_NARRATOR_GAME_IMAGE_PATH")
	if p == "" {
		t.Skip("NOT RUN: GAME_NARRATOR_GAME_IMAGE_PATH is not set (real-disc check needs the user's Wind Waker image)")
	}
	if _, err := os.Stat(p); err != nil {
		t.Skip("NOT RUN: disc image not found at " + p)
	}
	return p
}

func openRealDisc(t *testing.T) *gcdisc.Disc {
	t.Helper()
	p := realDiscPath(t)
	d, err := gcdisc.Open(p)
	if err != nil {
		t.Fatalf("open disc: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// realOpts returns parse options with a valid profile id (Message.Validate
// requires one) and a stable language for the real-disc checks.
func realOpts() adapter.ParseOptions {
	return adapter.ParseOptions{
		ProfileID: "01ARZ3NDEKTSV4RRFFQ69C5BYW",
		Language:  "en",
		MaxBytes:  adapter.DefaultMaxBytes,
		MaxRows:   adapter.DefaultMaxRows,
	}
}

// TestRealDiscParseDisc asserts the import-level invariants on the user's
// actual archive: a large real message set, byte-faithful original_text for
// every row, and non-empty normalized text for each imported row.
func TestRealDiscParseDisc(t *testing.T) {
	d := openRealDisc(t)
	a := New().(Adapter)
	res, err := a.ParseDisc(context.Background(), d, realOpts())
	if err != nil {
		t.Fatalf("ParseDisc: %v", err)
	}

	// The real Wind Waker message set is large; a parse that silently
	// returned a handful of rows would be a section-walk bug, not a pass.
	if len(res.Messages) < 1000 {
		t.Fatalf("parsed %d messages, want a large real set (>=1000)", len(res.Messages))
	}
	if res.Stats.Skipped > res.Stats.Rows/10 {
		t.Errorf("skipped %d of %d entries (>10%%), mapping may be misfiring", res.Stats.Skipped, res.Stats.Rows)
	}
	t.Logf("real disc: messages=%d skipped=%d", res.Stats.Imported, res.Stats.Skipped)

	// Every original_text re-encodes without error: the escape is a total
	// function on rows the adapter minted, so none can fail at write time.
	for i, m := range res.Messages {
		if _, err := encodeOriginal(m.OriginalText); err != nil {
			t.Fatalf("row %d (source %s) original_text does not round-trip: %v", i, m.SourceMessageID, err)
		}
		if m.NormalizedText == "" {
			t.Fatalf("row %d (source %s) has empty normalized text", i, m.SourceMessageID)
		}
		if m.MarkupJSON == nil || len(m.MarkupJSON) < 2 {
			t.Fatalf("row %d (source %s) has no markup JSON", i, m.SourceMessageID)
		}
	}
}

// TestRealDiscTagCoverage logs the distinct (group,type) population and
// asserts the default rule table covers at least 95% of tag occurrences.
// This is the evidence the table is not a stub: it was built from, and is
// checked against, the real file.
func TestRealDiscTagCoverage(t *testing.T) {
	p := realDiscPath(t)
	d, err := gcdisc.Open(p)
	if err != nil {
		t.Fatalf("open disc: %v", err)
	}
	defer d.Close()
	bmgBytes, err := ExtractBMG(d)
	if err != nil {
		t.Fatalf("extract BMG: %v", err)
	}
	f, err := bmg.Parse(bmgBytes)
	if err != nil {
		t.Fatalf("parse BMG: %v", err)
	}
	rules := NewRules()
	covered, total := tagCoverage(f, rules)
	if total == 0 {
		t.Fatal("no control tags found in the real file; extraction is wrong")
	}
	share := float64(covered) / float64(total)
	t.Logf("real disc: messages=%d tag_occurrences=%d coverage=%d (%.1f%%)",
		len(f.Messages), total, covered, share*100)
	if share < 0.95 {
		t.Errorf("rule table covers only %.1f%% of real tags (<95%%); distinct pairs: %v", share*100, tagPairs(f))
	}
}

// TestRealDiscOriginalRoundTripExact re-derives each message's raw pool
// bytes from the parse and checks the escape is an exact inverse on the real
// data (not just on the synthetic fixture).
func TestRealDiscOriginalRoundTripExact(t *testing.T) {
	d := openRealDisc(t)
	a := New().(Adapter)
	res, err := a.ParseDisc(context.Background(), d, realOpts())
	if err != nil {
		t.Fatalf("ParseDisc: %v", err)
	}
	bmgBytes, err := ExtractBMG(d)
	if err != nil {
		t.Fatalf("extract BMG: %v", err)
	}
	f, err := bmg.Parse(bmgBytes)
	if err != nil {
		t.Fatalf("parse BMG: %v", err)
	}
	byID := make(map[string]int, len(res.Messages))
	for i, m := range res.Messages {
		byID[m.SourceMessageID] = i
	}
	bad := 0
	for _, msg := range f.Messages {
		key := uint32toStr(msg.ID)
		i, ok := byID[key]
		if !ok {
			continue // skipped (empty) entry
		}
		want := append([]byte(nil), msg.Raw...)
		got, err := encodeOriginal(res.Messages[i].OriginalText)
		if err != nil {
			t.Fatalf("message %s: encode: %v", key, err)
		}
		if string(got) != string(want) {
			bad++
			if bad <= 3 {
				t.Errorf("message %s: re-encoded bytes differ from raw pool bytes", key)
			}
		}
	}
	if bad > 3 {
		t.Errorf("%d messages failed exact byte round-trip", bad)
	}
	t.Logf("real disc: roundtrip=ok checked=%d", len(byID))
}

// uint32toStr is a small uint32->string helper for the round-trip loop.
func uint32toStr(v uint32) string {
	if v == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

// TestRealDiscSpeakerCoverage asserts ADR-0070's mapping claim against the
// user's actual archive: the sidecar is lockstep with the rows, every
// imported line carries a speaker key (the attribute byte is present on every
// entry of this layout), the distinct-key count is non-degenerate, and the
// roster agrees with the sidecar. The assertions and the log line carry
// counts and the field name only — no key values, no message text (AGENTS.md
// rule 5).
func TestRealDiscSpeakerCoverage(t *testing.T) {
	d := openRealDisc(t)
	a := New().(Adapter)
	res, err := a.ParseDisc(context.Background(), d, realOpts())
	if err != nil {
		t.Fatalf("parse disc: %v", err)
	}
	if len(res.Messages) == 0 {
		t.Fatal("no imported rows; the coverage assertion would pass vacuously")
	}
	if len(res.SpeakerKeys) != len(res.Messages) {
		t.Fatalf("sidecar carries %d keys for %d rows; the adapter fills it in lockstep", len(res.SpeakerKeys), len(res.Messages))
	}
	if len(res.Speakers) == 0 || res.SpeakerColumn == "" {
		t.Fatal("the parse reported no speakers or no column; the coverage claim is the point of this test")
	}
	covered := 0
	distinct := map[string]bool{}
	for _, k := range res.SpeakerKeys {
		if k != "" {
			covered++
			distinct[k] = true
		}
	}
	if covered != len(res.Messages) {
		t.Errorf("%d of %d rows carry a speaker key, want 100%% coverage (the attribute byte is present on every entry)", covered, len(res.Messages))
	}
	if len(distinct) < 2 {
		t.Errorf("%d distinct speaker keys, want a non-degenerate roster (the measurement rests on many speakers)", len(distinct))
	}
	if len(distinct) != len(res.Speakers) {
		t.Errorf("%d distinct keys in the sidecar but %d reported speakers; the roster and the sidecar must agree", len(distinct), len(res.Speakers))
	}
	t.Logf("real disc: speaker coverage %d/%d rows, %d distinct keys, column %s", covered, len(res.Messages), len(distinct), res.SpeakerColumn)
}
