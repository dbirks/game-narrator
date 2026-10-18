package runtime

// White-box proof of the region ocr_settings merge (docs/spec/05, ADR-0040
// step 3): the profile's "difficult_font" flag on any region pins the
// fallback comparison on for the session, and malformed settings degrade to
// the default (off) instead of failing the session start.

import (
	"testing"

	"github.com/dbirks/game-narrator/internal/games"
)

func TestDifficultFontInRegions(t *testing.T) {
	cases := []struct {
		name    string
		regions []games.Region
		want    bool
	}{
		{"no regions", nil, false},
		{"empty", []games.Region{}, false},
		{"nil settings", []games.Region{{ID: "r1"}}, false},
		{"default settings", []games.Region{{ID: "r1", OCRSettings: []byte(`{}`)}}, false},
		{"flag true", []games.Region{{ID: "r1", OCRSettings: []byte(`{"difficult_font":true}`)}}, true},
		{"flag false", []games.Region{{ID: "r1", OCRSettings: []byte(`{"difficult_font":false}`)}}, false},
		{"wrong type", []games.Region{{ID: "r1", OCRSettings: []byte(`{"difficult_font":"yes"}`)}}, false},
		{"garbage settings", []games.Region{{ID: "r1", OCRSettings: []byte(`not json`)}}, false},
		{"second region flags it", []games.Region{{ID: "r1"}, {ID: "r2", OCRSettings: []byte(`{"difficult_font": true}`)}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := difficultFontIn(tc.regions); got != tc.want {
				t.Fatalf("difficultFontIn(%d regions) = %v, want %v", len(tc.regions), got, tc.want)
			}
		})
	}
}
