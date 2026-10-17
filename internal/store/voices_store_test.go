package store

import (
	"errors"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/games"
)

// The voice workflow schema (migration 0009): revision lifecycle, the
// immutability of an approved reference, audition bookkeeping and the
// reconciliation-facing registration rows.

func testVoiceRevision(t *testing.T, character string, rev int, state string) VoiceProfile {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)
	return VoiceProfile{
		ID:                games.NewID(),
		Character:         character,
		Revision:          rev,
		State:             state,
		DesignInstruction: "a calm narrator",
		Language:          "en",
		CreatedAt:         now,
		UpdatedAt:         now,
	}
}

func TestVoiceRevisionLifecycleAndImmutability(t *testing.T) {
	ctx := testCtx(t)
	s := migratedStore(t)
	char := "char-a"

	draft := testVoiceRevision(t, char, 1, VoiceStateDraft)
	if err := s.CreateVoiceRevision(ctx, draft); err != nil {
		t.Fatalf("create draft: %v", err)
	}
	got, err := s.GetVoiceRevision(ctx, draft.ID)
	if err != nil || got.State != VoiceStateDraft {
		t.Fatalf("get draft = %v, %v; want draft", got, err)
	}

	// draft -> auditioning is legal only through AddAuditions.
	next, err := s.NextVoiceRevision(ctx, char)
	if err != nil || next != 2 {
		t.Fatalf("next revision = %d, %v; want 2", next, err)
	}
	au := VoiceAudition{
		ID:             games.NewID(),
		VoiceProfileID: draft.ID,
		Index:          1,
		Brief:          draft.DesignInstruction,
		ModelID:        "qwen3-tts-voicedesign",
		SampleText:     "Hello there.",
		Transcript:     "Hello there.",
		AssetSHA256:    "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		DurationMS:     1200,
		SampleRate:     24000,
		Origin:         "voice_design model=test task_type=VoiceDesign",
		LatencyMS:      42,
		CreatedAt:      time.Now().UTC(),
	}
	if err := s.AddAuditions(ctx, draft.ID, []string{au.ID}, []VoiceAudition{au}); err != nil {
		t.Fatalf("add auditions: %v", err)
	}
	got, err = s.GetVoiceRevision(ctx, draft.ID)
	if err != nil {
		t.Fatalf("get after auditions: %v", err)
	}
	if got.State != VoiceStateAuditioning || len(got.AuditionIDs) != 1 || got.AuditionIDs[0] != au.ID {
		t.Fatalf("after AddAuditions: state=%s audition_ids=%v", got.State, got.AuditionIDs)
	}
	list, err := s.ListAuditions(ctx, draft.ID)
	if err != nil || len(list) != 1 || list[0].SampleText != "Hello there." {
		t.Fatalf("list auditions = %v, %v", list, err)
	}
	one, err := s.GetAudition(ctx, au.ID)
	if err != nil || one.SampleRate != 24000 || one.LatencyMS != 42 {
		t.Fatalf("get audition = %v, %v", one, err)
	}

	// auditioning -> approved with the frozen reference.
	approved := got
	approved.State = VoiceStateApproved
	approved.ReferenceAssetID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	approved.RefTranscript = "Hello there."
	approved.ModelID = "qwen3-tts-voicedesign"
	approved.ConsentProvenance = "original_synthetic_design (test)"
	approved.ApprovedBy = "curator-1"
	now := time.Now().UTC().Truncate(time.Millisecond)
	approved.ApprovedAt = &now
	approved.Fingerprint = "f"
	if err := s.UpdateVoiceProfile(ctx, approved); err != nil {
		t.Fatalf("approve: %v", err)
	}
	// A pending registration row records the precomputed name.
	if err := s.UpsertVoiceRegistration(ctx, VoiceRegistration{
		VoiceProfileID: approved.ID, InstanceID: "primary",
		RegisteredName: "gn_deadbeef_char-a_v1_deadbeef", State: VoiceRegPending,
	}); err != nil {
		t.Fatalf("upsert registration: %v", err)
	}

	// The approved row is immutable: any state move or content edit refuses.
	edited := approved
	edited.State = VoiceStateApproved
	edited.RefTranscript = "Hello there. (edited)"
	if err := s.UpdateVoiceProfile(ctx, edited); !errors.Is(err, ErrImmutable) {
		t.Fatalf("editing an approved revision: err = %v, want ErrImmutable", err)
	}
	if _, err := s.GetVoiceRevision(ctx, approved.ID); err != nil {
		t.Fatalf("get approved: %v", err)
	}
	got, _ = s.GetVoiceRevision(ctx, approved.ID)
	if got.RefTranscript != "Hello there." {
		t.Fatalf("approved transcript changed to %q", got.RefTranscript)
	}

	// approved -> retired is the only other legal move.
	retired := got
	retired.State = VoiceStateRetired
	if err := s.UpdateVoiceProfile(ctx, retired); err != nil {
		t.Fatalf("retire: %v", err)
	}
	// And retired is a dead end.
	back := retired
	back.State = VoiceStateApproved
	if err := s.UpdateVoiceProfile(ctx, back); !errors.Is(err, ErrImmutable) {
		t.Fatalf("retired -> approved: err = %v, want ErrImmutable", err)
	}
}

func TestVoiceRegistrationsTrackReconciliation(t *testing.T) {
	ctx := testCtx(t)
	s := migratedStore(t)
	rev := testVoiceRevision(t, "char-b", 1, VoiceStateApproved)
	rev.ReferenceAssetID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if err := s.CreateVoiceRevision(ctx, rev); err != nil {
		t.Fatalf("create approved revision: %v", err)
	}

	name := "gn_deadbeef_char-b_v1_deadbeef"
	verified := time.Now().UTC().Truncate(time.Millisecond)
	reg := VoiceRegistration{
		VoiceProfileID: rev.ID, InstanceID: "primary",
		RegisteredName: name, State: VoiceRegReady, LastVerifiedAt: &verified,
	}
	if err := s.UpsertVoiceRegistration(ctx, reg); err != nil {
		t.Fatalf("upsert ready: %v", err)
	}
	got, err := s.GetVoiceRegistration(ctx, rev.ID, "primary")
	if err != nil || got.State != VoiceRegReady || got.LastVerifiedAt == nil {
		t.Fatalf("get registration = %v, %v", got, err)
	}

	// The provider restarted: the voice is missing, and the next pass says so.
	missing := reg
	missing.State = VoiceRegMissing
	missing.Err = "voice_missing"
	missing.LastVerifiedAt = nil
	if err := s.UpsertVoiceRegistration(ctx, missing); err != nil {
		t.Fatalf("upsert missing: %v", err)
	}
	got, err = s.GetVoiceRegistration(ctx, rev.ID, "primary")
	if err != nil || got.State != VoiceRegMissing || got.Err != "voice_missing" {
		t.Fatalf("after missing = %v, %v", got, err)
	}

	// The reconciliation pass restored it.
	ready := reg
	ready.State = VoiceRegReady
	ready.Err = ""
	if err := s.UpsertVoiceRegistration(ctx, ready); err != nil {
		t.Fatalf("upsert ready again: %v", err)
	}
	rows, err := s.ListVoiceRegistrations(ctx, rev.ID)
	if err != nil || len(rows) != 1 || rows[0].State != VoiceRegReady || rows[0].RegisteredName != name {
		t.Fatalf("list registrations = %v, %v", rows, err)
	}

	// Invalid states are refused at the boundary.
	if err := s.UpsertVoiceRegistration(ctx, VoiceRegistration{
		VoiceProfileID: rev.ID, InstanceID: "primary",
		RegisteredName: name, State: "bogus",
	}); err == nil {
		t.Fatal("bogus state accepted")
	}

	// Approved-with-reference revisions are the reconciliation's required set.
	approved, err := s.ListApprovedVoiceRevisions(ctx)
	if err != nil || len(approved) != 1 || approved[0].ID != rev.ID {
		t.Fatalf("approved revisions = %v, %v", approved, err)
	}
}
