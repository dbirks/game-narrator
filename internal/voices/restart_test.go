package voices

// Restart persistence (docs/spec/16 M4 acceptance: "cloned voice preserved
// after app/provider restart"): approve and register a voice, close the
// store and the asset store, reopen both, and prove the approved revision,
// its reference bytes and the registry state survive — and that Reconcile
// reports the voice ready again after a provider-side loss.

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/store"
)

func openVoiceWorld(t *testing.T, root string) (*Service, *store.Store, *assets.Local, *fakeRegistry, *fakeTTS) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	st, err := store.Open(ctx, filepath.Join(root, store.FileName))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	assetsStore, err := assets.NewLocal(filepath.Join(root, "assets"))
	if err != nil {
		t.Fatalf("assets.NewLocal: %v", err)
	}
	tt := newFakeTTS()
	reg := newFakeRegistry()
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
	return svc, st, assetsStore, reg, tt
}

func TestVoiceSurvivesAppAndProviderRestart(t *testing.T) {
	root := t.TempDir()
	svc, st, _, _, _ := openVoiceWorld(t, root)
	ctx := ctx(t)

	approved, _ := mustAuditionAndApprove(t, svc, "char-restart", "a brief that must survive")
	row, err := svc.Register(ctx, approved.ID)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if row.State != store.VoiceRegReady {
		t.Fatalf("registered row = %+v, want ready", row)
	}
	firstFingerprint := approved.Fingerprint

	// Close everything (the "app restart"): the store is a process resource;
	// the asset store is file-backed and needs no close — the files ARE the
	// persistence.
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	// Reopen: a new process over the same data directory.
	svc2, st2, _, _, tt2 := openVoiceWorld(t, root)
	defer st2.Close()

	// The approved revision is still there, byte-for-byte the same identity.
	got, err := svc2.Store.GetVoiceRevision(ctx, approved.ID)
	if err != nil {
		t.Fatalf("after restart, GetVoiceRevision: %v", err)
	}
	if got.State != store.VoiceStateApproved || got.Fingerprint != firstFingerprint ||
		got.ReferenceAssetID == "" || got.ConsentProvenance == "" {
		t.Fatalf("approved row changed across restart: %+v", got)
	}

	// The canonical reference is still in the asset store and still valid.
	ref, err := svc2.loadReference(ctx, got.ReferenceAssetID)
	if err != nil {
		t.Fatalf("reference unreadable after restart: %v", err)
	}
	if sha256Hex(ref) != got.ReferenceAssetID {
		t.Fatal("reference bytes no longer match the reference's own content address")
	}

	// The registry row survives too.
	regRow, err := svc2.Store.GetVoiceRegistration(ctx, approved.ID, svc2.InstanceID)
	if err != nil || regRow.RegisteredName != row.RegisteredName {
		t.Fatalf("registration row after restart = %+v, %v", regRow, err)
	}

	// The provider also restarted: world 2's registry is a fresh instance
	// with no voices, which is exactly the lost-replica state, and one
	// Reconcile pass restores the set from the app-owned rows.
	rep, err := svc2.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile after restart: %v", err)
	}
	if len(rep.Registered) != 1 || rep.Registered[0] != row.RegisteredName {
		t.Fatalf("Reconcile after restart = %+v, want %s registered", rep, row.RegisteredName)
	}
	ready, err := svc2.Store.GetVoiceRegistration(ctx, approved.ID, svc2.InstanceID)
	if err != nil || ready.State != store.VoiceRegReady {
		t.Fatalf("post-reconcile state = %+v, %v", ready, err)
	}

	// And a preview works end to end on the restarted world: the first is a
	// miss (fresh in-memory index), the second is a hit.
	res, err := svc2.Preview(ctx, approved.ID, "Still here after the restart.")
	if err != nil {
		t.Fatalf("preview after restart: %v", err)
	}
	if res.FromCache || tt2.baseCalls != 1 {
		t.Fatalf("preview miss after restart: fromCache=%v baseCalls=%d", res.FromCache, tt2.baseCalls)
	}
	res2, err := svc2.Preview(ctx, approved.ID, "Still here after the restart.")
	if err != nil || !res2.FromCache {
		t.Fatalf("preview hit after restart: %v, %v", res2, err)
	}
	if tt2.baseCalls != 1 {
		t.Fatalf("second preview made a provider call: baseCalls=%d", tt2.baseCalls)
	}
}
