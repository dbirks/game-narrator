package voices

// The auto-cast rules (ADR-0109, UX-03/UX-03d) and the one-step undo, on the
// fake harness: the dial is re-read at job end, the QA gate refuses only a
// failed read-back (ADR 0061), the consent record is attributed to the
// profile default, and undo is one step by construction. No gateway: the
// design clips and the ASR read-back are scripted.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/jobs"
	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/store"
)

// castSeam is the scripted CastProfiles seam: the dial and the roster a test
// fixes before the run, mutable mid-test to simulate a dial turned after the
// job was enqueued.
type castSeam struct {
	profile CastProfile
	missing bool
	broken  error
	calls   int
}

func (c *castSeam) GetCastProfile(_ context.Context, _ string) (CastProfile, error) {
	c.calls++
	if c.broken != nil {
		return CastProfile{}, c.broken
	}
	if c.missing {
		return CastProfile{}, store.ErrNotFound
	}
	return c.profile, nil
}

// newCastService wires the fake harness with a scripted cast seam.
func newCastService(t *testing.T, seam *castSeam) (*Service, *castSeam, *fakeASR) {
	t.Helper()
	svc, _, _, _ := newTestService(t)
	svc.CastProfiles = seam
	asr := &fakeASR{}
	svc.ASR = asr
	return svc, seam, asr
}

// runCastJob runs one audition job end to end the way the runner would: draft
// created, the durable payload (with the cast context) fed to JobHandler.
func runCastJob(t *testing.T, svc *Service, character, brief string, count int, castProfileID string) (store.VoiceProfile, JobResult) {
	t.Helper()
	ctx := ctx(t)
	rev, err := svc.CreateDraft(ctx, character, brief, count)
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	payload, err := json.Marshal(JobPayload{ProfileID: rev.ID, Count: count, CastProfileID: castProfileID})
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	var result JobResult
	raw, err := JobHandler(svc)(ctx, jobs.Job{ID: "job-autocast", Kind: JobKind, Payload: payload}, func(float64, string) {})
	if err != nil {
		t.Fatalf("job handler: %v", err)
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("job result: %v", err)
	}
	fresh, err := svc.Store.GetVoiceRevision(ctx, rev.ID)
	if err != nil {
		t.Fatalf("GetVoiceRevision: %v", err)
	}
	return fresh, result
}

// TestAutoCastApprovesTheTakeWithTheProfileConsent is the product promise:
// dial on, QA passed, one approved take whose consent record is the profile's
// default attributed to the profile — satisfied and audited without a click.
func TestAutoCastApprovesTheTakeWithTheProfileConsent(t *testing.T) {
	seam := &castSeam{profile: CastProfile{AutoCast: true, CharacterNames: []string{"keeper"}}}
	svc, _, asr := newCastService(t, seam)
	// The read-back matches every line the take is filled with.
	asr.results = append(asr.results, passScripts(len(SyntheticAuditionLines))...)

	fresh, result := runCastJob(t, svc, "keeper", "a dry, level keeper", 1, "profile-keeper")
	if !result.AutoApproved || result.State != store.VoiceStateApproved {
		t.Fatalf("result = %+v, want auto-approved and approved", result)
	}
	if fresh.State != store.VoiceStateApproved {
		t.Fatalf("revision = %s, want approved", fresh.State)
	}
	wantConsent := AutoCastConsent("profile-keeper")
	if fresh.ConsentProvenance != wantConsent {
		t.Fatalf("consent provenance = %q, want %q (the profile default attributed to the profile)",
			fresh.ConsentProvenance, wantConsent)
	}
	if !strings.HasPrefix(fresh.ConsentProvenance, ConsentDefault+":profile-default:") {
		t.Fatalf("consent provenance = %q, want the consent default with the profile attribution", fresh.ConsentProvenance)
	}
	if fresh.ApprovedBy != AutoCaster {
		t.Fatalf("approved_by = %q, want %q (the flow, not a click)", fresh.ApprovedBy, AutoCaster)
	}
	if fresh.Fingerprint == "" || fresh.ReferenceAssetID == "" {
		t.Fatalf("approved revision incomplete: %+v", fresh)
	}
	auditions, err := svc.Store.ListAuditions(ctx(t), fresh.ID)
	if err != nil {
		t.Fatalf("ListAuditions: %v", err)
	}
	if len(auditions) != 1 {
		t.Fatalf("the cast run made %d takes, want one (the product promise)", len(auditions))
	}
}

// passScripts returns n transcripts that match the synthetic lines within the
// QA bound (the exact wording is irrelevant: the evaluator compares the
// normalised words).
func passScripts(n int) []speech.Transcription {
	var out []speech.Transcription
	for i, line := range SyntheticAuditionLines[:n] {
		out = append(out, speech.Transcription{Text: line})
		_ = i
	}
	return out
}

// TestAutoCastQAFailedKeepsTheTakePending: a failed read-back (ADR 0061) is
// the only auto-cast refusal — the take stays auditioning for review, the
// evidence stays on the row, and the job result says why.
func TestAutoCastQAFailedKeepsTheTakePending(t *testing.T) {
	seam := &castSeam{profile: CastProfile{AutoCast: true, CharacterNames: []string{"keeper"}}}
	svc, _, asr := newCastService(t, seam)
	line := SyntheticAuditionLines[0]
	asr.results = []speech.Transcription{{Text: "completely different words that share nothing with the line"}}

	fresh, result := runCastJob(t, svc, "keeper", "a dry, level keeper", 1, "profile-keeper")
	if result.AutoApproved || fresh.State != store.VoiceStateAuditioning {
		t.Fatalf("failed QA: result = %+v, revision = %s, want pending (auditioning)", result, fresh.State)
	}
	if !strings.Contains(result.AutoApprovalNote, "QA") {
		t.Fatalf("note = %q, want the QA refusal named", result.AutoApprovalNote)
	}
	auditions, err := svc.Store.ListAuditions(ctx(t), fresh.ID)
	if err != nil || len(auditions) != 1 {
		t.Fatalf("auditions = %d, err = %v, want the flagged take on the row", len(auditions), err)
	}
	if auditions[0].AsrState != store.VoiceAuditionQAFailed || auditions[0].AsrTranscript == "" {
		t.Fatalf("audition asr = state %d transcript %q, want failed with the evidence", auditions[0].AsrState, auditions[0].AsrTranscript)
	}
	_ = line
}

// TestAutoCastNotRunQAStillCasts: the ASR role unconfigured or the endpoint
// down leaves the read-back not-run, and ADR 0061's degraded mode says
// approval never blocks on ASR — the default voice still arrives.
func TestAutoCastNotRunQAStillCasts(t *testing.T) {
	seam := &castSeam{profile: CastProfile{AutoCast: true, CharacterNames: []string{"keeper"}}}
	svc, _, asr := newCastService(t, seam)
	asr.err = errors.New("fakeASR: endpoint down")

	fresh, result := runCastJob(t, svc, "keeper", "a dry, level keeper", 1, "profile-keeper")
	if !result.AutoApproved || fresh.State != store.VoiceStateApproved {
		t.Fatalf("not-run QA: result = %+v, revision = %s, want approved (degraded mode)", result, fresh.State)
	}
}

// TestAutoCastHonoursADialTurnedOffAtJobEnd: the dial the auto-cast step
// re-reads at the end is the one that decides (the enqueue-time read, at the
// API, only chose the count). Off at the step's read leaves the take pending
// review. The API test runs the same seam with the dial turned between the
// enqueue and the job; here the seam simply answers off.
func TestAutoCastHonoursADialTurnedOffAtJobEnd(t *testing.T) {
	seam := &castSeam{profile: CastProfile{AutoCast: false, CharacterNames: []string{"keeper"}}}
	svc, _, asr := newCastService(t, seam)
	asr.results = passScripts(1)

	fresh, result := runCastJob(t, svc, "keeper", "a dry, level keeper", 1, "profile-keeper")
	if result.AutoApproved || fresh.State != store.VoiceStateAuditioning {
		t.Fatalf("dial off at job end: result = %+v, revision = %s, want pending", result, fresh.State)
	}
	if !strings.Contains(strings.ToLower(result.AutoApprovalNote), "auto_cast is off") {
		t.Fatalf("note = %q, want the off dial named", result.AutoApprovalNote)
	}
	// The take is still a normal, human-approvable audition: the dial only
	// decides the auto step, it never flags the evidence.
	if _, err := svc.Approve(ctx(t), fresh.ID, oneAuditionID(t, svc, fresh.ID), "original_synthetic_design (unit test)", "curator-1"); err != nil {
		t.Fatalf("a human approval of the pending take = %v, want success", err)
	}
}

// TestAutoCastIsIdempotentOverAJobRestart: a crash between the approval and
// the job's completion leaves an approved revision the restart must report,
// not re-approve — Approve refuses a non-auditioning revision, so the skip is
// the idempotency.
func TestAutoCastIsIdempotentOverAJobRestart(t *testing.T) {
	seam := &castSeam{profile: CastProfile{AutoCast: true, CharacterNames: []string{"keeper"}}}
	svc, _, asr := newCastService(t, seam)
	asr.results = passScripts(1)

	fresh, first := runCastJob(t, svc, "keeper", "a dry, level keeper", 1, "profile-keeper")
	if !first.AutoApproved {
		t.Fatalf("first run = %+v, want auto-approved", first)
	}
	// The restart replays the same payload (the job runner's contract): the
	// fill refuses a non-draft revision, so the restart's own entry point is
	// the auto-cast step itself.
	approved, note, err := svc.AutoApproveCast(ctx(t), "profile-keeper", fresh.ID)
	if err != nil {
		t.Fatalf("AutoApproveCast on the restart = %v, want the skip, not a fault", err)
	}
	if approved {
		t.Fatalf("the restart re-approved: the work was already done; the skip is the idempotency")
	}
	if !strings.Contains(note, "already approved") {
		t.Fatalf("note = %q, want the restart finding named", note)
	}
}

// TestAutoCastSkipsWhenTheSeamIsUnwired: a payload that names a cast profile
// on a service without the seam (a build without the catalogue) must not loop
// the job — the answer is the skip, not the error.
func TestAutoCastSkipsWhenTheSeamIsUnwired(t *testing.T) {
	svc, _, asr := newCastService(t, nil)
	svc.CastProfiles = nil
	asr.results = passScripts(1)

	fresh, _ := runCastJob(t, svc, "keeper", "a dry, level keeper", 1, "profile-keeper")
	if fresh.State != store.VoiceStateAuditioning {
		t.Fatalf("unwired seam: revision = %s, want pending review (auditioning), not a faulted job", fresh.State)
	}
}

// TestAutoCastAProfileDeletedMidRunDoesNotFailTheJob: the fill already
// happened; the profile deleting under the job is a skip with a note, not a
// failed job the runner would retry against a row that will never come back.
func TestAutoCastAProfileDeletedMidRunDoesNotFailTheJob(t *testing.T) {
	seam := &castSeam{profile: CastProfile{AutoCast: true, CharacterNames: []string{"keeper"}}}
	svc, _, asr := newCastService(t, seam)
	asr.results = passScripts(1)
	_, _ = runCastJob(t, svc, "keeper", "a dry, level keeper", 1, "profile-keeper")

	seam.missing = true
	approved, note, err := svc.AutoApproveCast(ctx(t), "profile-keeper", mustApproved(t, svc, "keeper"))
	if err != nil {
		t.Fatalf("AutoApproveCast after the profile deleted = %v, want the skip", err)
	}
	if approved || !strings.Contains(note, "no longer exists") {
		t.Fatalf("approved=%v note=%q, want the deletion named", approved, note)
	}
}

// TestUndoRestoresThePreviousTake is the one-step undo (UX-03d): the current
// take retires, the previous take becomes current, nothing is deleted, and a
// second undo is refused — there is no undo the undo.
func TestUndoRestoresThePreviousTake(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := ctx(t)
	first, _ := mustAuditionAndApprove(t, svc, "keeper", "a dry, level keeper")
	second, _ := mustAuditionAndApprove(t, svc, "keeper", "the same character, a different take")
	if second.Revision <= first.Revision {
		t.Fatalf("revisions = %d then %d, want the regeneration to be the higher revision", first.Revision, second.Revision)
	}

	restored, err := svc.Undo(ctx, second.ID)
	if err != nil {
		t.Fatalf("Undo: %v", err)
	}
	if restored.State != store.VoiceStateRetired {
		t.Fatalf("the undone take = %s, want retired", restored.State)
	}
	cur, ok, err := svc.CurrentRevision(ctx, "keeper")
	if err != nil || !ok || cur.ID != first.ID {
		t.Fatalf("current after the undo = %s (ok=%v, err=%v), want the previous take %s", cur.ID, ok, err, first.ID)
	}
	// Neither revision was deleted: the old take's audio still resolves.
	old, err := svc.Store.GetVoiceRevision(ctx, first.ID)
	if err != nil || old.State != store.VoiceStateApproved {
		t.Fatalf("the previous take = %s (err=%v), want still approved", old.State, err)
	}
	if old.Fingerprint == "" || old.ReferenceAssetID == "" {
		t.Fatalf("the restored take lost its reference: %+v", old)
	}

	// A second undo has nothing to undo: the retired take is not a take any
	// more, so the domain refuses it — ErrNothingToUndo (it is not current) or
	// ErrInvalidState (it is not approved); the API maps both to the same 409
	// surface, and the state must not have moved.
	if _, err := svc.Undo(ctx, second.ID); !errors.Is(err, ErrNothingToUndo) && !errors.Is(err, ErrInvalidState) {
		t.Fatalf("second undo = %v, want a refusal (one step by construction)", err)
	}
	if fresh, err := svc.Store.GetVoiceRevision(ctx, second.ID); err != nil || fresh.State != store.VoiceStateRetired {
		t.Fatalf("the retired take = %s (err=%v), want still retired (the second undo did nothing)", fresh.State, err)
	}
	// Undoing the restored take is refused too: it has no lower take.
	if _, err := svc.Undo(ctx, first.ID); !errors.Is(err, ErrNothingToUndo) {
		t.Fatalf("undo with no previous take = %v, want ErrNothingToUndo", err)
	}
}

// TestUndoRefusesARevisionThatIsNotCurrent: undoing a take that is not the
// one being spoken changes nothing audibly, so it is refused — the 409
// surface of the API, at the domain level.
func TestUndoRefusesARevisionThatIsNotCurrent(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := ctx(t)
	first, _ := mustAuditionAndApprove(t, svc, "keeper", "a dry, level keeper")
	second, _ := mustAuditionAndApprove(t, svc, "keeper", "the same character, a different take")

	if _, err := svc.Undo(ctx, first.ID); !errors.Is(err, ErrNothingToUndo) {
		t.Fatalf("undo of the older take = %v, want ErrNothingToUndo (it is not current)", err)
	}
	// An auditioning revision is not undoable at all: it has no reference to
	// hand current back over.
	rev, err := svc.CreateDraft(ctx, "keeper", "another try", 1)
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	if _, err := svc.CreateAuditionsOnDraft(ctx, rev.ID, 1); err != nil {
		t.Fatalf("fill: %v", err)
	}
	if _, err := svc.Undo(ctx, rev.ID); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("undo of an auditioning revision = %v, want ErrInvalidState", err)
	}
	_ = second
}

// TestCurrentRevisionIsTheRuntimeRule: "current" is the highest approved
// revision with a reference — the session's voice-resolution rule — so the
// undo the API offers and the voice the game speaks agree.
func TestCurrentRevisionIsTheRuntimeRule(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := ctx(t)
	first, _ := mustAuditionAndApprove(t, svc, "keeper", "a dry, level keeper")

	// Retiring the take by hand (the store allows approved -> retired) makes
	// current fall through: no approved take with a reference remains.
	if _, ok, err := svc.CurrentRevision(ctx, "keeper"); err != nil || !ok {
		t.Fatalf("current = ok=%v err=%v, want the approved take", ok, err)
	}
	updated := first
	updated.State = store.VoiceStateRetired
	if err := svc.Store.UpdateVoiceProfile(ctx, updated); err != nil {
		t.Fatalf("retire the take: %v", err)
	}
	if _, ok, err := svc.CurrentRevision(ctx, "keeper"); err != nil || ok {
		t.Fatalf("current after retiring the only take = ok=%v err=%v, want none", ok, err)
	}
}

// mustApproved returns the id of the character's one approved revision.
func mustApproved(t *testing.T, svc *Service, character string) string {
	t.Helper()
	profiles, err := svc.Store.ListVoiceProfiles(ctx(t), character)
	if err != nil {
		t.Fatalf("ListVoiceProfiles: %v", err)
	}
	for _, p := range profiles {
		if p.State == store.VoiceStateApproved && p.Fingerprint != "" && p.ReferenceAssetID != "" {
			return p.ID
		}
	}
	t.Fatalf("character %s has no approved take", character)
	return ""
}

// oneAuditionID returns the id of the revision's one audition.
func oneAuditionID(t *testing.T, svc *Service, revisionID string) string {
	t.Helper()
	auditions, err := svc.Store.ListAuditions(ctx(t), revisionID)
	if err != nil || len(auditions) != 1 {
		t.Fatalf("auditions = %d (err=%v), want one", len(auditions), err)
	}
	return auditions[0].ID
}
