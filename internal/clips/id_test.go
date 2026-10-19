package clips_test

import (
	"testing"

	"github.com/dbirks/game-narrator/internal/clips"
)

func TestValidID(t *testing.T) {
	for _, id := range []string{
		"a",
		"wind-waker-001",
		"clip.2026.10",
		"abc_123",
		"a2345678901234567890123456789012345678901234567890123456789012ab", // 64
	} {
		if !clips.ValidID(id) {
			t.Errorf("ValidID(%q) = false, want true", id)
		}
	}
	for _, id := range []string{
		"",
		"-leading-dash",
		".leading-dot",
		"..",
		"UPPER",
		"spacey name",
		"with/slash",
		"back\\slash",
		"a2345678901234567890123456789012345678901234567890123456789012abc", // 65
		"\x00nul",
	} {
		if clips.ValidID(id) {
			t.Errorf("ValidID(%q) = true, want false", id)
		}
	}
}

func TestNormalizeName(t *testing.T) {
	for name, want := range map[string]string{
		"Wind Waker 001":    "wind-waker-001",
		"  spaced  out  ":   "spaced-out",
		"underscore_name.1": "underscore-name-1",
		"MiXed.Case-9":      "mixed-case-9",
		"ümlauts-stay":      "mlauts-stay",
		"---":               "",
	} {
		got, err := clips.NormalizeName(name)
		switch {
		case want == "":
			if err == nil {
				t.Errorf("NormalizeName(%q) = %q, want an error", name, got)
			}
		default:
			if err != nil || got != want {
				t.Errorf("NormalizeName(%q) = %q, %v; want %q", name, got, err, want)
			}
		}
	}
	long, err := clips.NormalizeName("a2345678901234567890123456789012345678901234567890123456789012345")
	if err != nil {
		t.Fatalf("NormalizeName(long): %v", err)
	}
	if len(long) > clips.MaxIDLen || !clips.ValidID(long) {
		t.Fatalf("long name = %q (%d), want a valid id of at most %d", long, len(long), clips.MaxIDLen)
	}
}
