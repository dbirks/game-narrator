package voices

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/providers/tts"
	"github.com/dbirks/game-narrator/internal/store"
)

// Register pushes one approved revision's voice to this instance's registry
// (docs/spec/07 step 6) through the reconciliation primitive Ensure: one
// List, register-if-missing, and a conflict report for any name whose stored
// content contradicts the name's digest. The voice_registrations row records
// the outcome per instance (docs/spec/02 voice_registrations).
func (s *Service) Register(ctx context.Context, revisionID string) (store.VoiceRegistration, error) {
	reg, err := s.ensureOne(ctx, revisionID)
	if err != nil {
		return store.VoiceRegistration{}, err
	}
	// A conflict is a state the operator must see, not a success: names are
	// immutable (docs/spec/06), so a collision means a human picked two
	// different contents for one identity and must re-approve a new revision.
	return reg, nil
}

// ensureOne runs Ensure for one revision and records the instance row.
func (s *Service) ensureOne(ctx context.Context, revisionID string) (store.VoiceRegistration, error) {
	rev, err := s.Store.GetVoiceRevision(ctx, revisionID)
	if err != nil {
		return store.VoiceRegistration{}, err
	}
	if rev.State != store.VoiceStateApproved || rev.ReferenceAssetID == "" {
		return store.VoiceRegistration{}, fmt.Errorf("%w: state %s, reference %q", ErrNotApproved, rev.State, rev.ReferenceAssetID)
	}
	reg, err := s.Store.GetVoiceRegistration(ctx, revisionID, s.InstanceID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return store.VoiceRegistration{}, err
	}
	name := reg.RegisteredName
	if name == "" {
		name, err = s.nameFor(ctx, rev)
		if err != nil {
			return store.VoiceRegistration{}, err
		}
	}
	required, err := s.requiredVoice(ctx, rev, name)
	if err != nil {
		return store.VoiceRegistration{}, err
	}
	report, err := s.Registry.Ensure(ctx, []tts.Voice{required})
	if err != nil {
		return store.VoiceRegistration{}, fmt.Errorf("voices: ensure %s: %w", name, err)
	}
	next := s.registrationFromReport(rev, name, report)
	if err := s.Store.UpsertVoiceRegistration(ctx, next); err != nil {
		return store.VoiceRegistration{}, err
	}
	return next, nil
}

// requiredVoice loads the reference bytes and builds the full upload. The
// consent record travels with it (docs/spec/14: the registry's consent field
// must be meaningful — this one is the row's own, frozen at approval).
func (s *Service) requiredVoice(ctx context.Context, rev store.VoiceProfile, name string) (tts.Voice, error) {
	ref, err := s.loadReference(ctx, rev.ReferenceAssetID)
	if err != nil {
		return tts.Voice{}, fmt.Errorf("voices: reference for %s: %w", name, err)
	}
	return tts.Voice{
		Name:     name,
		RefAudio: ref,
		RefText:  rev.RefTranscript,
		Consent:  rev.ConsentProvenance,
	}, nil
}

// nameFor resolves the revision's registry name on this instance: the row's
// recorded name when present (it was precomputed at approval), otherwise a
// recomputation from the reference bytes (a row written by an older build).
func (s *Service) nameFor(ctx context.Context, rev store.VoiceProfile) (string, error) {
	reg, err := s.Store.GetVoiceRegistration(ctx, rev.ID, s.InstanceID)
	if err == nil && reg.RegisteredName != "" {
		return reg.RegisteredName, nil
	}
	ref, err := s.loadReference(ctx, rev.ReferenceAssetID)
	if err != nil {
		return "", err
	}
	name, err := RegistryName(rev.ID, rev.Character, rev.Revision, ref)
	if err != nil {
		return "", err
	}
	return name, nil
}

// registrationFromReport maps one Ensure report onto the instance row.
// RemoteDigest is the content digest embedded in the immutable name itself
// (docs/spec/06): it is the identity the instance can be checked against
// without reading the reference bytes.
func (s *Service) registrationFromReport(rev store.VoiceProfile, name string, report tts.EnsureReport) store.VoiceRegistration {
	now := time.Now().UTC()
	out := store.VoiceRegistration{
		VoiceProfileID: rev.ID, InstanceID: s.InstanceID,
		RegisteredName: name, RemoteDigest: nameDigest8(name),
		LastVerifiedAt: &now,
	}
	for _, c := range report.Conflicts {
		if c.Name == name {
			out.State = store.VoiceRegInvalid
			out.Err = c.Reason
			return out
		}
	}
	switch {
	case contains(report.Present, name):
		out.State = store.VoiceRegReady
	case contains(report.Registered, name):
		out.State = store.VoiceRegReady
	default:
		out.State = store.VoiceRegMissing
		out.Err = "voice_missing: not reported present or registered by the instance"
	}
	return out
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// nameDigest8 extracts the content digest from the end of a namespaced voice
// name (gn_..._<digest8>).
func nameDigest8(name string) string {
	if i := strings.LastIndexByte(name, '_'); i >= 0 && len(name)-i-1 >= 8 {
		return name[len(name)-8:]
	}
	return ""
}

// ReconcileReport is the outcome of one Reconcile pass: what was checked, what
// was (re)registered, what is ready, and what the operator must look at.
type ReconcileReport struct {
	Checked    int
	Present    []string
	Registered []string
	Conflicts  []tts.Conflict
	// Missing are approved revisions whose reference is unreadable or whose
	// name could not be derived; they were skipped, not failed.
	Missing []string
	// Errs are per-revision reconciliation errors (transport, storage).
	Errs []string
}

// Reconcile keeps every approved revision registered on this instance
// (docs/spec/06 "VoiceRegistryManager", docs/spec/07 step 6): list once,
// re-register what is missing, never re-register a conflicting name. It is
// the "voice missing on replica" path: after a provider restart or on a new
// replica, one pass restores the voice set from the app-owned rows and the
// asset store — no user action, no re-audition.
func (s *Service) Reconcile(ctx context.Context) (ReconcileReport, error) {
	var rep ReconcileReport
	approved, err := s.Store.ListApprovedVoiceRevisions(ctx)
	if err != nil {
		return rep, fmt.Errorf("voices: list approved revisions: %w", err)
	}
	rep.Checked = len(approved)

	var required []tts.Voice
	byName := map[string]store.VoiceProfile{}
	for _, rev := range approved {
		name, err := s.nameFor(ctx, rev)
		if err != nil {
			rep.Missing = append(rep.Missing, rev.ID)
			continue
		}
		v, err := s.requiredVoice(ctx, rev, name)
		if err != nil {
			rep.Missing = append(rep.Missing, rev.ID)
			rep.Errs = append(rep.Errs, err.Error())
			continue
		}
		required = append(required, v)
		byName[name] = rev
	}
	if len(required) == 0 {
		return rep, nil
	}
	report, err := s.Registry.Ensure(ctx, required)
	if err != nil {
		return rep, fmt.Errorf("voices: ensure: %w", err)
	}
	rep.Present = report.Present
	rep.Registered = report.Registered
	rep.Conflicts = report.Conflicts

	for _, name := range append(append([]string{}, report.Present...), report.Registered...) {
		rev := byName[name]
		reg := s.registrationFromReport(rev, name, report)
		if err := s.Store.UpsertVoiceRegistration(ctx, reg); err != nil {
			rep.Errs = append(rep.Errs, err.Error())
		}
	}
	for _, c := range report.Conflicts {
		rev, ok := byName[c.Name]
		if !ok {
			continue
		}
		now := time.Now().UTC()
		if err := s.Store.UpsertVoiceRegistration(ctx, store.VoiceRegistration{
			VoiceProfileID: rev.ID, InstanceID: s.InstanceID,
			RegisteredName: c.Name, State: store.VoiceRegInvalid,
			LastVerifiedAt: &now, Err: c.Reason,
		}); err != nil {
			rep.Errs = append(rep.Errs, err.Error())
		}
	}
	return rep, nil
}
