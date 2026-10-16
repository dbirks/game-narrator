package games_test

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/store"
)

// TestEnumsMatchTheSchemaCHECKs is the anti-drift gate between the two places a
// vocabulary is written down: the CHECK (col IN (…)) list in a migration and the
// enum in this package. Drift in either direction is invisible until a user hits
// it — a SQL value missing from Go means the service rejects a row the database
// would accept, and a Go value missing from SQL means the service accepts input
// that fails mid-transaction with an opaque "CHECK constraint failed".
//
// It lives in the games package and reads the schema through the store's own
// embedded migration list, so it always tests the shipped SQL rather than a
// copied-out snapshot.
func TestEnumsMatchTheSchemaCHECKs(t *testing.T) {
	listPattern := regexp.MustCompile(`(?i)\b([a-z_]+)\s+IN\s*\(([^)]*)\)`)

	inLists := map[string][][]string{}
	migs, err := store.Migrations()
	if err != nil {
		t.Fatalf("store.Migrations: %v", err)
	}
	for _, m := range migs {
		for _, found := range listPattern.FindAllStringSubmatch(m.SQL, -1) {
			col := strings.ToLower(found[1])
			var vals []string
			for _, raw := range strings.Split(found[2], ",") {
				v := strings.TrimSpace(raw)
				v = strings.Trim(v, "'")
				if v != "" {
					vals = append(vals, v)
				}
			}
			inLists[col] = append(inLists[col], vals)
		}
		if !strings.Contains(m.SQL, "IN (") {
			t.Logf("migration %s declares no IN lists", m.Name)
		}
	}

	// Several tables constrain a column with the same name (profiles.status,
	// characters.status and corrections.status are three different vocabularies),
	// so a Go enum passes by matching *one* list on its column exactly.
	cases := []struct {
		name   string
		column string
		values []string
	}{
		{"ProfileStatus", "status", []string{"draft", "review", "published", "archived"}},
		{"SourceType", "source_type", []string{"user_file", "web_page", "repository", "manual", "game_extract"}},
		{"RightsStatus", "rights_status", []string{"user_supplied", "licensed", "unknown"}},
		{"TextKind", "text_kind", []string{"dialogue", "sign", "item", "menu", "tutorial", "system", "other"}},
		{"AttributionStatus", "attribution_status", []string{"unknown", "proposed", "confirmed", "rejected"}},
		{"EdgeType", "edge_type", []string{"next", "branch", "repeat", "contextual", "inferred"}},
		{"CharacterStatus", "status", []string{"proposed", "confirmed", "retired"}},
		{"RegionType", "region_type", []string{"textbox", "speaker_label", "subtitle", "sign", "item", "menu", "tutorial", "hud", "other"}},
		{"DetectMethod", "detect_method", []string{"manual", "auto_discovered", "imported", "inferred"}},
		{"AssertionMethod", "method", []string{"on_screen_label", "script_order", "dump_metadata", "visual_anchor", "model", "manual", "manual_correction"}},
		{"CorrectionKind", "kind", []string{"speaker", "text", "speech_text", "scene", "region", "classification", "edge", "other"}},
		{"CorrectionTarget", "target_type", []string{"message", "message_page", "message_edge", "character", "scene", "region", "other"}},
		{"CorrectionStatus", "status", []string{"proposed", "accepted", "rejected", "superseded"}},
		{"JobState", "state", []string{"queued", "running", "succeeded", "failed", "cancelled", "recoverable", "requires_input"}},
		{"AttemptOutcome", "outcome", []string{"running", "succeeded", "failed", "cancelled", "timed_out", "crashed", "requires_input"}},
	}

	// The literal lists above are the contract; the enum types must agree with
	// them, so a rename has to happen in both places to pass.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			candidates, ok := inLists[tc.column]
			if !ok {
				t.Fatalf("no CHECK list for column %q in the schema", tc.column)
			}
			want := strings.Join(sortedCopy(tc.values), ",")
			var spellings []string
			if err := games.WalkEnum(tc.name, func(value string) { spellings = append(spellings, value) }); err != nil {
				t.Fatalf("enum %s is not registered in Go: %v", tc.name, err)
			}
			if got := strings.Join(sortedCopy(spellings), ","); got != want {
				t.Errorf("Go enum %s = [%s], schema wants [%s]", tc.name, got, want)
			}
			found := false
			for _, candidate := range candidates {
				if strings.Join(sortedCopy(candidate), ",") == want {
					found = true
				}
			}
			if !found {
				t.Errorf("no CHECK(%s IN …) in the schema lists exactly [%s]; closest: %s",
					tc.column, want, renderLists(candidates))
			}
		})
	}
}

// TestOpenVocabularyStaysOpen guards the columns that are deliberately NOT
// enums under docs/spec/04's portability rule: per-game roles, flags,
// conditions and OCR settings. If one ever gains a CHECK IN list, this test
// fails and the decision has to be made consciously (and documented in an ADR).
func TestOpenVocabularyStaysOpen(t *testing.T) {
	migs, err := store.Migrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migs {
		for _, column := range []string{"role", "flags", "condition_expr", "ocr_settings"} {
			// Match only a constraint on that column, not the words in prose.
			pattern := regexp.MustCompile(`(?m)^\s*` + column + `\b.*\bIN\s*\(`)
			if pattern.MatchString(m.SQL) {
				t.Errorf("migration %s closed the open vocabulary %s with an IN list; docs/spec/04 forbids that without an ADR",
					m.Name, column)
			}
		}
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func renderLists(lists [][]string) string {
	var out []string
	for _, l := range lists {
		out = append(out, "["+strings.Join(l, ",")+"]")
	}
	return strings.Join(out, " ")
}
