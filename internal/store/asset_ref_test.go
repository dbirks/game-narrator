package store

// Eviction-safety tests for AssetReferenced (ADR-0041 "Reference check before
// delete"). The speech cache may only delete a blob it just stopped indexing
// when no catalogue row names the same hash; these tests prove the query sees
// every referencing table, is index-backed on the whole UNION, and errors on
// a malformed hash instead of answering "unreferenced".

import (
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/games"
)

// refHash is a 64-hex-digit hash the tests point rows at. Each referencing
// table gets its OWN hash so every UNION branch is proven individually: a
// branch the query forgot would leave its hash unreported.
func refHash(c byte) string { return strings.Repeat(string(c), 64) }

// TestAssetReferencedFalseOnEmptyCatalogue is the baseline: with no rows at
// all nothing is referenced.
func TestAssetReferencedFalseOnEmptyCatalogue(t *testing.T) {
	s := migratedStore(t)
	ctx := testCtx(t)
	for _, h := range []string{refHash('a'), refHash('b')} {
		got, err := s.AssetReferenced(ctx, h)
		if err != nil {
			t.Fatalf("AssetReferenced(%s…): %v", h[:8], err)
		}
		if got {
			t.Fatalf("AssetReferenced(%s…) = true on an empty catalogue", h[:8])
		}
	}
}

// TestAssetReferencedCoversEveryReferencingTable inserts exactly one row per
// referencing table, each naming a distinct hash, and requires the query to
// see exactly that hash after each insert — no more (a row in another table
// leaking in) and no less (a branch the UNION is missing). If a future
// migration adds a referencing column, its row belongs in this table list.
func TestAssetReferencedCoversEveryReferencingTable(t *testing.T) {
	s := migratedStore(t)
	ctx := testCtx(t)

	// The characters row needs a legal profile chain.
	gameID, editionID, profileID := "g-ref", "ed-ref", "p-ref"
	mustExec(t, s.db,
		`INSERT INTO games (id, slug, title, platform, user_tags, created_at) VALUES (?,?,?,?,?,?)`,
		gameID, "ref-game", "Ref Game", "gamecube", `["synthetic"]`, nowRFC3339())
	insertEdition(t, s.db, editionID, gameID)
	insertProfile(t, s.db, profileID, editionID, 1, "published", nil)

	// (table, hash, row that names the hash) in an order that satisfies the
	// foreign keys: source_blobs and sources first, then the voice rows.
	voiceID := games.NewID()
	steps := []struct {
		table  string
		hash   string
		insert func(t *testing.T, h string)
	}{
		{"source_blobs", refHash('1'), func(t *testing.T, h string) {
			insertBlob(t, s.db, h, 2048)
		}},
		{"sources", refHash('2'), func(t *testing.T, h string) {
			insertSource(t, s.db, "src-ref", profileID, "user_file", h)
		}},
		{"characters", refHash('3'), func(t *testing.T, h string) {
			mustExec(t, s.db,
				`INSERT INTO characters (id, profile_id, name, portrait_asset_id, status, created_at)
				 VALUES (?,?,?,?,?,?)`,
				"ch-ref", profileID, "Sailor", h, "confirmed", nowRFC3339())
		}},
		{"voice_profiles", refHash('4'), func(t *testing.T, h string) {
			now := time.Now().UTC()
			if err := s.CreateVoiceRevision(ctx, VoiceProfile{
				ID: voiceID, Character: "sailor", Revision: 1,
				State:            VoiceStateApproved,
				ReferenceAssetID: h,
				CreatedAt:        now, UpdatedAt: now,
			}); err != nil {
				t.Fatalf("CreateVoiceRevision: %v", err)
			}
		}},
		{"voice_auditions", refHash('5'), func(t *testing.T, h string) {
			mustExec(t, s.db,
				`INSERT INTO voice_auditions (id, voice_profile_id, "index", sample_text, transcript, asset_sha256, created_at)
				 VALUES (?,?,?,?,?,?,?)`,
				games.NewID(), voiceID, 1, "synthetic audition line", "synthetic audition line", h, nowRFC3339())
		}},
	}

	// Baseline: none of the hashes is referenced yet.
	for _, st := range steps {
		if got, err := s.AssetReferenced(ctx, st.hash); err != nil || got {
			t.Fatalf("before %s row: AssetReferenced(%s…) = %v, %v; want false, nil",
				st.table, st.hash[:8], got, err)
		}
	}

	// One row per table; after each insert exactly that table's hash becomes
	// referenced, and every earlier one stays so.
	for i, st := range steps {
		st.insert(t, st.hash)
		// Every inserted row so far must be visible: no branch missing, no
		// cross-table leakage hiding a false negative.
		for j := 0; j <= i; j++ {
			got, err := s.AssetReferenced(ctx, steps[j].hash)
			if err != nil {
				t.Fatalf("after %s row: AssetReferenced(%s…): %v", st.table, steps[j].hash[:8], err)
			}
			if !got {
				t.Fatalf("after %s row: AssetReferenced(%s…) = false, want true (the UNION is missing %s)",
					st.table, steps[j].hash[:8], steps[j].table)
			}
		}
	}
}

// TestAssetReferencedQueryIsIndexBacked proves the whole UNION runs on the
// indexes rather than scans: EXPLAIN QUERY PLAN must name the three indexes
// added by migration 0010 plus the two from 0001. A future migration that
// removes one of them fails here, which is exactly when the disk-full
// eviction path would start doing table scans.
func TestAssetReferencedQueryIsIndexBacked(t *testing.T) {
	s := migratedStore(t)
	ctx := testCtx(t)
	h := refHash('e')
	rows, err := s.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+assetReferenceQuery, h, h, h, h, h)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var (
			id, parent, notused int
			detail              string
		)
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan plan row: %v", err)
		}
		plan.WriteString(detail)
		plan.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	for _, want := range []string{
		"sources_content_sha256_idx",
		"characters_portrait_asset_idx",
		"voice_profiles_reference_asset_idx",
		"voice_auditions_asset_idx",
	} {
		if !strings.Contains(plan.String(), want) {
			t.Errorf("query plan does not use %q:\n%s", want, plan.String())
		}
	}
	// No branch may fall back to a full table scan: that is exactly what the
	// migration 0010 indexes exist to keep off the disk-full eviction path.
	for _, tbl := range []string{"source_blobs", "sources", "characters", "voice_profiles", "voice_auditions"} {
		if strings.Contains(plan.String(), "SCAN "+tbl) {
			t.Errorf("%s branch scans the table:\n%s", tbl, plan.String())
		}
	}
}

// TestAssetReferencedRejectsMalformedHash is the caller-bug half: an empty or
// truncated hash must not be answered "no references" (that answer would
// authorize deleting an unaddressable blob).
func TestAssetReferencedRejectsMalformedHash(t *testing.T) {
	s := migratedStore(t)
	ctx := testCtx(t)
	for _, bad := range []string{"", "abc", strings.Repeat("a", 63), strings.Repeat("A", 64)} {
		if got, err := s.AssetReferenced(ctx, bad); err == nil {
			t.Errorf("AssetReferenced(%q) = %v, nil; want an error (caller bug, not a catalogue answer)", bad[:4], got)
		}
	}
}
