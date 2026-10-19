package windwaker

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dbirks/game-narrator/internal/games/adapter"
	"github.com/dbirks/game-narrator/internal/gcdisc"
)

// The synthetic disc factory (ADR-0054/0066) is gated by round-trip: the
// image it emits must open through gcdisc, extract through the capability,
// and parse into exactly the lines it was given — the whole disc-import path
// on invented text.

func writeSynthImage(t *testing.T, img []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "synth.gcm")
	if err := os.WriteFile(p, img, 0o644); err != nil {
		t.Fatalf("write synthetic image: %v", err)
	}
	return p
}

func TestSynthDiscImageRoundTrip(t *testing.T) {
	lines := []string{
		"SYNTHETIC-LINE-ONE",
		"SYNTHETIC LINE TWO WITH A SLIGHT WAVE",
		"LINE THREE",
	}
	img, err := SynthDiscImage("GZLE01", lines)
	if err != nil {
		t.Fatalf("SynthDiscImage: %v", err)
	}
	p := writeSynthImage(t, img)

	d, err := gcdisc.Open(p)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	if got := d.Header().GameID; got != "GZLE01" {
		t.Fatalf("game id = %q, want GZLE01", got)
	}

	a := New().(Adapter)
	script, name, gameID, err := a.ExtractDiscSource(context.Background(), d)
	if err != nil {
		t.Fatalf("ExtractDiscSource: %v", err)
	}
	if gameID != "GZLE01" || name != "bmgres.bmg" {
		t.Fatalf("extract reported game id %q name %q, want GZLE01 / bmgres.bmg", gameID, name)
	}
	if len(script) == 0 {
		t.Fatal("no script bytes extracted")
	}

	res, err := a.Parse(context.Background(), bytes.NewReader(script), adapter.ParseOptions{
		ProfileID: "01ARZ3NDEKTSV4RRFFQ69C5BYW",
		SourceID:  "01ARZ3NDEKTSV4RRFFQ69C5BYX",
		Language:  "en",
		MaxBytes:  adapter.DefaultMaxBytes,
		MaxRows:   adapter.DefaultMaxRows,
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if res.Stats.Imported != len(lines) || res.Stats.Skipped != 0 {
		t.Fatalf("stats = %+v, want imported=%d skipped=0", res.Stats, len(lines))
	}
	got := make([]string, 0, len(res.Messages))
	for _, m := range res.Messages {
		got = append(got, m.OriginalText)
	}
	for i, want := range lines {
		if got[i] != want {
			t.Fatalf("line %d = %q, want %q", i, got[i], want)
		}
	}
}

func TestSynthDiscImageRefusals(t *testing.T) {
	// A non-Wind Waker id is refused: the factory only mints GZL images.
	if _, err := SynthDiscImage("RWLE01", []string{"x"}); err == nil {
		t.Error("a non-GZL id was accepted")
	}
	// A mis-length id is refused even with the right prefix letters.
	if _, err := SynthDiscImage("GZL", []string{"x"}); err == nil {
		t.Error("a too-short id was accepted")
	}
	if _, err := SynthDiscImage("GZLE01", nil); err == nil {
		t.Error("an empty disc was accepted")
	}
}

// ExtractDiscSource refuses a mislabelled image in the adapter, the same way
// the CLI refuses one before any parse (the provenance-lie guard).
func TestExtractDiscSourceRefusesForeignDisc(t *testing.T) {
	img, err := SynthDiscImage("GZLE01", []string{"SYNTHETIC"})
	if err != nil {
		t.Fatalf("SynthDiscImage: %v", err)
	}
	// Re-label the boot header's game id with a foreign title.
	copy(img[0:6], []byte("RWLE01"))
	p := writeSynthImage(t, img)
	d, err := gcdisc.Open(p)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	if _, _, _, err := New().(Adapter).ExtractDiscSource(context.Background(), d); err == nil {
		t.Fatal("a foreign disc was extracted: the game-id refusal must happen in the adapter")
	}
}
