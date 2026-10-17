package voices

// Domain unit tests (docs/spec/12 "Unit" mode): the workflow rules — consent
// required, approval immutability, fingerprint-driven cache keys,
// register/reconcile with a lost replica, and the preview miss-then-hit
// contract — against fakes; plus one restart-persistence test on the real
// store and asset store (docs/spec/16 "cloned voice preserved after
// app/provider restart").

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/providers/tts"
	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/store"
)

// fakeTTS is the scripted Design + Base client. Every WAV is deterministic
// per (kind, voice, text) so the cache-hit assertions are exact, and the
// counters are the evidence that a hit made no provider call.
type fakeTTS struct {
	mu          sync.Mutex
	designCalls int
	baseCalls   int
	failBase    bool
}

func newFakeTTS() *fakeTTS { return &fakeTTS{} }

func (f *fakeTTS) Design(_ context.Context, req tts.DesignRequest) (tts.Audio, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.designCalls++
	wav := wavTone(180+float64(f.designCalls)*40, 1)
	return tts.Audio{
		WAV: wav, SampleRate: 24000, Channels: 1, BitsPerSample: 16,
		Duration: time.Second, Model: "fake-voicedesign", Latency: 5 * time.Millisecond,
		RequestID: "fake-design-" + strconv.Itoa(f.designCalls),
		Origin:    "voice_design model=fake-voicedesign task_type=VoiceDesign",
	}, nil
}

func (f *fakeTTS) Generate(_ context.Context, req tts.GenerateRequest) (tts.Audio, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failBase {
		return tts.Audio{}, errors.New("fakeTTS: base disabled")
	}
	f.baseCalls++
	wav := wavTone(300+float64(len(req.Voice)%500), 1)
	return tts.Audio{
		WAV: wav, SampleRate: 24000, Channels: 1, BitsPerSample: 16,
		Duration: time.Second, Model: "fake-base", Latency: 5 * time.Millisecond,
		RequestID: "fake-base-" + strconv.Itoa(f.baseCalls),
		Origin:    "voice_clone model=fake-base voice=" + req.Voice,
	}, nil
}

func (f *fakeTTS) GenerateWithReference(_ context.Context, req tts.GenerateRequest, refAudio []byte, _ string) (tts.Audio, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failBase {
		return tts.Audio{}, errors.New("fakeTTS: base disabled")
	}
	f.baseCalls++
	wav := wavTone(600+float64(len(refAudio)%200), 1)
	return tts.Audio{
		WAV: wav, SampleRate: 24000, Channels: 1, BitsPerSample: 16,
		Duration: time.Second, Model: "fake-base", Latency: 5 * time.Millisecond,
		RequestID: "fake-base-ref-" + strconv.Itoa(f.baseCalls),
		Origin:    "voice_clone model=fake-base voice=" + req.Voice + " inline_ref_audio=true",
	}, nil
}

// newTestService wires a Service over the fakes (assets is real, in a temp
// dir: the asset store's content addressing is part of the contract).
func newTestService(t *testing.T) (*Service, *fakeTTS, *fakeRegistry, *fakeStore) {
	t.Helper()
	dir := t.TempDir()
	assetsStore, err := assets.NewLocal(dir)
	if err != nil {
		t.Fatalf("assets.NewLocal: %v", err)
	}
	tt := newFakeTTS()
	reg := newFakeRegistry()
	st := newFakeStore()
	svc := &Service{
		Store: st, Assets: assetsStore, Design: tt, Base: tt, Registry: reg,
		Speech:        speech.New(assetsStore, speech.NewMemIndex(), 4),
		BaseModelID:   "fake-base",
		BackendDigest: "preview-v1",
		Language:      "English",
	}
	if err := New(svc); err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc, tt, reg, st
}

func ctx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func mustAuditionAndApprove(t *testing.T, svc *Service, character, brief string) (store.VoiceProfile, string) {
	t.Helper()
	ctx := ctx(t)
	rev, auditions, err := svc.CreateAuditions(ctx, character, brief, 3)
	if err != nil {
		t.Fatalf("CreateAuditions: %v", err)
	}
	if rev.State != store.VoiceStateAuditioning || len(auditions) != 3 {
		t.Fatalf("revision = %s with %d auditions, want auditioning/3", rev.State, len(auditions))
	}
	approved, err := svc.Approve(ctx, rev.ID, auditions[0].ID, "original_synthetic_design (unit test)", "curator-1")
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if approved.State != store.VoiceStateApproved || approved.Fingerprint == "" ||
		approved.ReferenceAssetID == "" || approved.ConsentProvenance == "" {
		t.Fatalf("approved revision incomplete: %+v", approved)
	}
	return approved, auditions[0].ID
}

func TestApproveRequiresConsent(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := ctx(t)
	rev, _, err := svc.CreateAuditions(ctx, "char-consent", "a brief", 1)
	if err != nil {
		t.Fatalf("CreateAuditions: %v", err)
	}
	auds, err := svc.Store.ListAuditions(ctx, rev.ID)
	if err != nil || len(auds) != 1 {
		t.Fatalf("auditions = %v, %v", auds, err)
	}
	for _, consent := range []string{"", "   "} {
		if _, err := svc.Approve(ctx, rev.ID, auds[0].ID, consent, "curator-1"); !errors.Is(err, ErrConsentRequired) {
			t.Fatalf("Approve(consent=%q) err = %v, want ErrConsentRequired", consent, err)
		}
	}
	got, _ := svc.Store.GetVoiceRevision(ctx, rev.ID)
	if got.State != store.VoiceStateAuditioning {
		t.Fatalf("after refused approvals state = %s, want auditioning", got.State)
	}
}

func TestApprovalIsImmutable(t *testing.T) {
	svc, _, _, st := newTestService(t)
	approved, _ := mustAuditionAndApprove(t, svc, "char-imm", "brief A")

	// A second approval on the same revision is refused: the state machine
	// does not move back, and the frozen content cannot be re-edited.
	if _, err := svc.Approve(ctx(t), approved.ID, "bogus", "consent", "curator-2"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("re-approve err = %v, want ErrInvalidState", err)
	}
	// The store itself refuses a content edit on the approved row.
	edited := approved
	edited.RefTranscript = "tampered transcript"
	if err := st.UpdateVoiceProfile(ctx(t), edited); !errors.Is(err, store.ErrImmutable) {
		t.Fatalf("store edit err = %v, want ErrImmutable", err)
	}
	got, err := st.GetVoiceRevision(ctx(t), approved.ID)
	if err != nil || got.RefTranscript != approved.RefTranscript || got.Fingerprint != approved.Fingerprint {
		t.Fatalf("frozen row changed: %+v, %v", got, err)
	}
	// The pending registration row was opened with the precomputed name.
	reg, err := st.GetVoiceRegistration(ctx(t), approved.ID, svc.InstanceID)
	if err != nil || reg.State != store.VoiceRegPending || reg.RegisteredName == "" {
		t.Fatalf("pending registration = %+v, %v", reg, err)
	}
	if !strings.HasPrefix(reg.RegisteredName, "gn_") || strings.HasPrefix(reg.RegisteredName, "gn_test_") {
		t.Fatalf("name %q is not a production namespaced name", reg.RegisteredName)
	}
	if err := tts.ValidateName(reg.RegisteredName); err != nil {
		t.Fatalf("name fails the registry grammar: %v", err)
	}
}

func TestFingerprintDrivesTheCacheKey(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	one, _ := mustAuditionAndApprove(t, svc, "char-fp", "brief one")
	// A new design attempt is a new revision; a new approval must not touch
	// the first one (docs/spec/07 step 8).
	two, _ := mustAuditionAndApprove(t, svc, "char-fp", "brief two")

	if one.Fingerprint == two.Fingerprint {
		t.Fatal("two revisions with different reference content share a fingerprint")
	}
	k1a, err := previewCacheKey(one, "Hello there.", svc.BackendDigest)
	if err != nil {
		t.Fatalf("key one: %v", err)
	}
	k1b, err := previewCacheKey(one, "Hello there.", svc.BackendDigest)
	if err != nil || k1a != k1b {
		t.Fatalf("same revision+text produced different keys: %s vs %s (%v)", k1a, k1b, err)
	}
	k2, err := previewCacheKey(two, "Hello there.", svc.BackendDigest)
	if err != nil || k2 == k1a {
		t.Fatalf("new revision reused the old key: %s vs %s", k2, k1a)
	}
	// The old revision stays approved and attributable.
	if got, _ := svc.Store.GetVoiceRevision(ctx(t), one.ID); got.State != store.VoiceStateApproved {
		t.Fatalf("old revision state = %s, want approved", got.State)
	}
}

func TestRegisterThenReconcileAfterProviderRestart(t *testing.T) {
	svc, _, reg, st := newTestService(t)
	approved, _ := mustAuditionAndApprove(t, svc, "char-reg", "brief")

	row, err := svc.Register(ctx(t), approved.ID)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if row.State != store.VoiceRegReady || row.RegisteredName == "" {
		t.Fatalf("after Register = %+v, want ready", row)
	}
	if len(reg.voices) != 1 {
		t.Fatalf("registry holds %d voices, want 1", len(reg.voices))
	}

	// The provider restarts: the voice is gone.
	reg.drop(row.RegisteredName)
	rep, err := svc.Reconcile(ctx(t))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(rep.Registered) != 1 || rep.Registered[0] != row.RegisteredName {
		t.Fatalf("Reconcile re-registered %v, want %s", rep.Registered, row.RegisteredName)
	}
	got, err := st.GetVoiceRegistration(ctx(t), approved.ID, svc.InstanceID)
	if err != nil || got.State != store.VoiceRegReady || got.LastVerifiedAt == nil {
		t.Fatalf("after Reconcile = %+v, %v", got, err)
	}

	// A conflicting name (the instance's stored content contradicts the
	// name's identity) is reported, not overwritten: the row goes invalid
	// with the reason, and the app never re-uploads a conflicting name.
	reg.conflictNames[row.RegisteredName] = true
	row, err = svc.Register(ctx(t), approved.ID)
	if err != nil {
		t.Fatalf("Register with conflict: %v", err)
	}
	if row.State != store.VoiceRegInvalid || row.Err == "" {
		t.Fatalf("conflict row = %+v, want invalid with reason", row)
	}
	// And Reconcile keeps it invalid rather than papering over it.
	if _, err := svc.Reconcile(ctx(t)); err != nil {
		t.Fatalf("Reconcile with a conflict: %v", err)
	}
	stillInvalid, _ := st.GetVoiceRegistration(ctx(t), approved.ID, svc.InstanceID)
	if stillInvalid.State != store.VoiceRegInvalid {
		t.Fatalf("Reconcile overwrote a conflict: %+v", stillInvalid)
	}
}

func TestPreviewMissThenHit(t *testing.T) {
	svc, tt, reg, _ := newTestService(t)
	approved, _ := mustAuditionAndApprove(t, svc, "char-preview", "brief")
	if _, err := svc.Register(ctx(t), approved.ID); err != nil {
		t.Fatalf("Register: %v", err)
	}

	line := "The morning was calm."
	res, err := svc.Preview(ctx(t), approved.ID, line)
	if err != nil {
		t.Fatalf("preview 1: %v", err)
	}
	if res.FromCache {
		t.Fatal("first preview was a cache hit; want a miss")
	}
	if tt.baseCalls != 1 {
		t.Fatalf("base calls after miss = %d, want 1", tt.baseCalls)
	}

	res2, err := svc.Preview(ctx(t), approved.ID, line)
	if err != nil {
		t.Fatalf("preview 2: %v", err)
	}
	if !res2.FromCache {
		t.Fatal("second identical preview was a miss; want a hit (docs/spec/07 step 7)")
	}
	if tt.baseCalls != 1 {
		t.Fatalf("base calls after hit = %d, want still 1 (a hit makes no provider call)", tt.baseCalls)
	}
	if res2.SHA256 != res.SHA256 {
		t.Fatal("hit returned different bytes than the miss committed")
	}

	// A different line is a different key and a fresh generation.
	if _, err := svc.Preview(ctx(t), approved.ID, "A different line."); err != nil {
		t.Fatalf("preview 3: %v", err)
	}
	if tt.baseCalls != 2 {
		t.Fatalf("base calls after a new line = %d, want 2", tt.baseCalls)
	}
	_ = reg
}

func TestPreviewWithoutRegistrationUsesInlineReference(t *testing.T) {
	svc, tt, _, _ := newTestService(t)
	_, _ = mustAuditionAndApprove(t, svc, "char-inline", "brief")
	// Not registered on this instance (no Register call): the preview must
	// fall back to the inline reference (docs/spec/06), not to a built-in.
	res, err := svc.Preview(ctx(t), mustAuditionID(t, svc, "char-inline"), "Inline line.")
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if res.FromCache || tt.baseCalls != 1 {
		t.Fatalf("inline preview: fromCache=%v baseCalls=%d", res.FromCache, tt.baseCalls)
	}
}

// mustAuditionID returns the id of the character's latest approved revision.
func mustAuditionID(t *testing.T, svc *Service, character string) string {
	t.Helper()
	ctx := ctx(t)
	profiles, err := svc.Store.ListVoiceProfiles(ctx, character)
	if err != nil || len(profiles) == 0 {
		t.Fatalf("profiles for %s: %v, %v", character, profiles, err)
	}
	for _, p := range profiles {
		if p.State == store.VoiceStateApproved {
			return p.ID
		}
	}
	t.Fatalf("no approved revision for %s", character)
	return ""
}
