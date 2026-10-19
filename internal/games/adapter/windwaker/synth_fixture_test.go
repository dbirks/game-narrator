package windwaker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dbirks/game-narrator/internal/games/adapter"
	"github.com/dbirks/game-narrator/internal/gcdisc"
)

// The e2e import directory's disc (ADR-0066's Playwright walk): a synthetic
// Wind Waker disc committed under testdata/disc so the walk can import a disc
// image without a user's real one (AGENTS.md rule 5 — synthetic fixture,
// documented provenance, no user data). The image is deterministic — fixed
// lines, BMP rows derived from the text, sorted RARC nodes — so the committed
// bytes ARE the provenance proof: a fresh synth must equal them byte for
// byte, and the adapter must parse the committed file with exactly its own
// lines through the same path the import job uses.
//
// Regenerate with:
//
//	GN_UPDATE_FIXTURE=1 go test ./internal/games/adapter/windwaker/ -run TestSyntheticDiscFixture
var fixtureLines = []string{
	"Wind Waker disc line 1",
	"Wind Waker disc line 2",
	"Wind Waker disc line 3",
	"Wind Waker disc line 4",
}

func TestSyntheticDiscFixture(t *testing.T) {
	fixture := filepath.Join("..", "..", "..", "..", "testdata", "disc", "synth-windwaker.gcm")
	img, err := SynthDiscImage("GZLE01", fixtureLines)
	if err != nil {
		t.Fatalf("SynthDiscImage: %v", err)
	}
	if os.Getenv("GN_UPDATE_FIXTURE") == "1" {
		if err := os.MkdirAll(filepath.Dir(fixture), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(fixture, img, 0o644); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		t.Logf("fixture written: %s (%d bytes)", fixture, len(img))
		return
	}
	committed, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("the committed fixture is missing (regenerate it with GN_UPDATE_FIXTURE=1): %v", err)
	}
	if len(committed) != len(img) || string(committed) != string(img) {
		t.Fatalf("the committed fixture drifted from a fresh synth: the synth is deterministic, so this means the generator changed — regenerate it and review the diff")
	}

	// And the fixture must parse through the real job path — open the disc,
	// hand it to the adapter — with exactly its own lines: the bytes are not
	// just stable, they are a disc the pipeline can read. ParseDisc is a
	// concrete method (the interface is BMG-first), so it is called through
	// the same assertion the import job uses.
	d, err := gcdisc.Open(fixture)
	if err != nil {
		t.Fatalf("gcdisc.Open on the committed fixture: %v", err)
	}
	a, ok := New().(interface {
		ParseDisc(context.Context, *gcdisc.Disc, adapter.ParseOptions) (*adapter.ParseResult, error)
	})
	if !ok {
		t.Fatal("the windwaker adapter no longer exposes ParseDisc: the disc-import job is broken")
	}
	res, err := a.ParseDisc(context.Background(), d, adapter.ParseOptions{ProfileID: "01ARTESTSYNTH0000000000000"})
	if err != nil {
		t.Fatalf("the committed fixture does not parse through the adapter: %v", err)
	}
	if len(res.Messages) != len(fixtureLines) {
		t.Fatalf("the fixture parsed to %d messages, want %d", len(res.Messages), len(fixtureLines))
	}
	for i, m := range res.Messages {
		if m.OriginalText != fixtureLines[i] {
			t.Errorf("message %d original text = %q, want %q", i+1, m.OriginalText, fixtureLines[i])
		}
	}
}
