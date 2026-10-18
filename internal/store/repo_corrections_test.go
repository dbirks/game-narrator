package store

// The correction repository (0002_content.sql, ADR-0052): user fixes recorded
// as labels, and the regression fixtures they are. Everything here is
// synthetic text typed into the test (AGENTS.md rule 5).

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/games"
)

// acceptedCorrection builds a well-formed label the way the dialogue editor's
// save records one (ADR-0052): the acting curator is both actor and reviewer.
func acceptedCorrection(f *repoFixture, m games.Message, before, after string) games.Correction {
	return games.Correction{
		ID:          games.NewID(),
		ProfileID:   f.profile.ID,
		Kind:        games.CorrectionSpeechText,
		TargetType:  games.TargetMessage,
		TargetID:    m.ID,
		MessageID:   m.ID,
		BeforeValue: before,
		AfterValue:  after,
		Reason:      "speech text corrected in the dialogue editor",
		Evidence:    []byte(`[]`),
		Actor:       "curator-one",
		Status:      games.CorrectionAccepted,
		Reviewer:    "curator-one",
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
		ReviewedAt:  time.Now().UTC().Format(time.RFC3339),
	}
}

func TestPutCorrectionRoundTrip(t *testing.T) {
	f := newRepoFixture(t)
	m := f.message("The lighthouse kept its own weather.")
	if err := f.s.PutMessages(f.ctx, f.profile.ID, []games.Message{m}, nil); err != nil {
		t.Fatalf("PutMessages: %v", err)
	}

	c := acceptedCorrection(f, m, m.SpeechText, "The lighthouse kept its own weather, and so did the harbour.")
	if err := f.s.PutCorrection(f.ctx, c); err != nil {
		t.Fatalf("PutCorrection: %v", err)
	}

	got, err := f.s.GetCorrection(f.ctx, c.ID)
	if err != nil {
		t.Fatalf("GetCorrection: %v", err)
	}
	if got.ID != c.ID || got.ProfileID != c.ProfileID || got.Kind != c.Kind ||
		got.TargetType != c.TargetType || got.TargetID != c.TargetID || got.MessageID != c.MessageID ||
		got.BeforeValue != c.BeforeValue || got.AfterValue != c.AfterValue || got.Reason != c.Reason ||
		got.Actor != c.Actor || got.Status != c.Status || got.Reviewer != c.Reviewer ||
		got.CreatedAt != c.CreatedAt || got.ReviewedAt != c.ReviewedAt {
		t.Errorf("round trip differs:\n got %+v\nwant %+v", got, c)
	}
	if string(got.Evidence) != string(c.Evidence) {
		t.Errorf("evidence differs: %s vs %s", got.Evidence, c.Evidence)
	}

	missing, err := f.s.GetCorrection(f.ctx, games.NewID())
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("GetCorrection for a missing id = %v, want ErrNotFound (row %+v)", err, missing)
	}
}

func TestPutCorrectionRefusesUnknownProfile(t *testing.T) {
	f := newRepoFixture(t)
	c := acceptedCorrection(f, f.message("A line that is not stored."), "x", "y")
	c.ProfileID = games.NewID() // a revision this store does not have
	if err := f.s.PutCorrection(f.ctx, c); !errors.Is(err, ErrNotFound) {
		t.Errorf("PutCorrection against a missing profile = %v, want ErrNotFound", err)
	}
}

func TestPutCorrectionRefusesValidationFaults(t *testing.T) {
	f := newRepoFixture(t)
	m := f.message("A line.")

	// A proposed row must not carry a reviewer (games.Correction's rule).
	c := acceptedCorrection(f, m, "a", "b")
	c.Status = games.CorrectionProposed
	c.Reviewer = "someone"
	if err := f.s.PutCorrection(f.ctx, c); !errors.Is(err, games.ErrValidation) {
		t.Errorf("proposed row with a reviewer = %v, want ErrValidation", err)
	}

	// An unknown kind is refused, not stored.
	c = acceptedCorrection(f, m, "a", "b")
	c.Kind = games.CorrectionKind("not-a-kind")
	if err := f.s.PutCorrection(f.ctx, c); !errors.Is(err, games.ErrValidation) {
		t.Errorf("unknown kind = %v, want ErrValidation", err)
	}
}

func TestListCorrectionsFiltersAndOrder(t *testing.T) {
	f := newRepoFixture(t)
	b := f.bundle(2)
	b.put(f)

	// A second profile on another edition of the same game, so the profile
	// filter has something to exclude.
	other := f.editionWithRegion("JP")
	otherProfile, err := f.s.CreateDraftProfile(f.ctx, other.ID)
	if err != nil {
		t.Fatalf("CreateDraftProfile: %v", err)
	}
	// CreatedAt is explicit and distinct per row: ids carry random entropy
	// (games.NewID), so the created_at column is the only order a test can
	// rely on, and a real second of edits is the tie the id column breaks.
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	at := func(seconds int) string { return t0.Add(time.Duration(seconds) * time.Second).Format(time.RFC3339) }

	first := acceptedCorrection(f, b.Messages[0], "the lighthouse weather", "the lighthouse weather, corrected")
	first.CreatedAt = at(0)
	first.ReviewedAt = at(0)
	if err := f.s.PutCorrection(f.ctx, first); err != nil {
		t.Fatalf("PutCorrection(first): %v", err)
	}
	// A different kind on the same message: the kind filter must separate the
	// two labels.
	speaker := acceptedCorrection(f, b.Messages[0], "Keeper", "Marina")
	speaker.Kind = games.CorrectionSpeaker
	speaker.Reason = "the on-screen label disagrees with the script order"
	speaker.CreatedAt = at(1)
	speaker.ReviewedAt = at(1)
	if err := f.s.PutCorrection(f.ctx, speaker); err != nil {
		t.Fatalf("PutCorrection(speaker): %v", err)
	}
	// And a label on the other profile: the profile filter must drop it.
	otherRow := games.Message{
		ID:                games.NewID(),
		ProfileID:         otherProfile.ID,
		OriginalText:      "A line on the other revision.",
		NormalizedText:    "a line on the other revision",
		SpeechText:        "A line on the other revision.",
		Language:          "en",
		TextKind:          games.TextDialogue,
		AttributionStatus: games.AttributionUnknown,
		Revision:          1,
		CreatedAt:         nowRFC3339(),
		UpdatedAt:         nowRFC3339(),
	}
	if err := f.s.PutMessages(f.ctx, otherProfile.ID, []games.Message{otherRow}, nil); err != nil {
		t.Fatalf("PutMessages(other): %v", err)
	}
	third := acceptedCorrection(f, otherRow, "other", "other, corrected")
	third.ProfileID = otherProfile.ID
	third.CreatedAt = at(2)
	third.ReviewedAt = at(2)
	if err := f.s.PutCorrection(f.ctx, third); err != nil {
		t.Fatalf("PutCorrection(other): %v", err)
	}

	// Unfiltered: all three, newest first (created_at, then id).
	all, err := f.s.ListCorrections(f.ctx, CorrectionFilter{})
	if err != nil {
		t.Fatalf("ListCorrections: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("unfiltered list has %d rows, want 3", len(all))
	}
	if all[0].ID != third.ID || all[1].ID != speaker.ID || all[2].ID != first.ID {
		t.Errorf("newest-first order = %s, %s, %s; want %s, %s, %s",
			all[0].ID, all[1].ID, all[2].ID, third.ID, speaker.ID, first.ID)
	}

	// Profile filter: the other revision's label is dropped.
	byProfile, err := f.s.ListCorrections(f.ctx, CorrectionFilter{ProfileID: f.profile.ID})
	if err != nil {
		t.Fatalf("ListCorrections(profile): %v", err)
	}
	if len(byProfile) != 2 {
		t.Fatalf("profile filter has %d rows, want 2", len(byProfile))
	}
	for _, c := range byProfile {
		if c.ProfileID != f.profile.ID {
			t.Errorf("profile filter returned a row of %s", c.ProfileID)
		}
	}

	// Kind filter: the speech-text labels only.
	byKind, err := f.s.ListCorrections(f.ctx, CorrectionFilter{Kind: string(games.CorrectionSpeechText)})
	if err != nil {
		t.Fatalf("ListCorrections(kind): %v", err)
	}
	if len(byKind) != 2 {
		t.Fatalf("kind filter has %d rows, want 2", len(byKind))
	}
	for _, c := range byKind {
		if c.Kind != games.CorrectionSpeechText {
			t.Errorf("kind filter returned %s", c.Kind)
		}
	}

	// Limit: one row back.
	limited, err := f.s.ListCorrections(f.ctx, CorrectionFilter{Limit: 1})
	if err != nil {
		t.Fatalf("ListCorrections(limit): %v", err)
	}
	if len(limited) != 1 || limited[0].ID != third.ID {
		t.Errorf("limit 1 = %v, want the newest row only", limited)
	}
}

func TestListCorrectionsClampsLimit(t *testing.T) {
	f := newRepoFixture(t)
	b := f.bundle(1)
	b.put(f)
	if err := f.s.PutCorrection(f.ctx, acceptedCorrection(f, b.Messages[0], "a", "b")); err != nil {
		t.Fatalf("PutCorrection: %v", err)
	}
	got, err := f.s.ListCorrections(f.ctx, CorrectionFilter{Limit: 10_000})
	if err != nil {
		t.Fatalf("ListCorrections: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("limit 10000 returned %d rows", len(got))
	}
	// The clamp is the documented ceiling, not a different one.
	if maxCorrectionsList != 500 {
		t.Errorf("maxCorrectionsList = %d, the API and the store must share 500", maxCorrectionsList)
	}
}

// TestListCorrectionsEvidenceAndNullableColumns guards the scan: the evidence
// column defaults to '[]' and the nullable pointers scan back to "" rather than
// SQL noise, so a row written with defaults and a row written with values both
// come back the way the API serves them.
func TestListCorrectionsEvidenceAndNullableColumns(t *testing.T) {
	f := newRepoFixture(t)
	m := f.message("A line.")
	if err := f.s.PutMessages(f.ctx, f.profile.ID, []games.Message{m}, nil); err != nil {
		t.Fatalf("PutMessages: %v", err)
	}

	// A proposal: no reviewer, no reviewed_at, evidence with a hash.
	proposal := acceptedCorrection(f, m, "before", "after")
	proposal.Status = games.CorrectionProposed
	proposal.Reviewer = ""
	proposal.ReviewedAt = ""
	proposal.Evidence = []byte(`["asset:0f1e2d3c4b5a69788796a5b4c3d2e1f0a9b8c7d6e5f40312"]`)
	if err := f.s.PutCorrection(f.ctx, proposal); err != nil {
		t.Fatalf("PutCorrection(proposal): %v", err)
	}

	got, err := f.s.ListCorrections(f.ctx, CorrectionFilter{ProfileID: f.profile.ID})
	if err != nil {
		t.Fatalf("ListCorrections: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("list has %d rows", len(got))
	}
	row := got[0]
	if row.Reviewer != "" || row.ReviewedAt != "" {
		t.Errorf("a proposed row carries reviewer/reviewed time: %q / %q", row.Reviewer, row.ReviewedAt)
	}
	if !strings.Contains(string(row.Evidence), "0f1e2d3c4b5a69788796a5b4c3d2e1f0a9b8c7d6e5f40312") {
		t.Errorf("evidence did not survive the round trip: %s", row.Evidence)
	}
	if row.MessageID != m.ID {
		t.Errorf("message_id = %q, want %q", row.MessageID, m.ID)
	}
}
