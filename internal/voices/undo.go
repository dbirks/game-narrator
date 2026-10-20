package voices

// One-step undo of a regenerated voice (ADR-0109, docs/design/plum-peach
// UX-03d): regenerating a character's voice creates a new revision that
// becomes current (the runtime speaks the highest approved revision with a
// reference), and the previous approved take is not deleted. Undo restores
// the previous take as current by retiring the new one — the move the state
// machine already allowed (approved -> retired) and nothing else could reach.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/dbirks/game-narrator/internal/store"
)

// ErrNothingToUndo: the named revision is not in the position a one-step undo
// can take — it is not the character's current take, or the character has no
// previous approved take to restore.
var ErrNothingToUndo = errors.New("voices: nothing to undo: the revision is not the current take, or the character has no previous approved take")

// hasReference reports whether the revision carries a frozen reference: the
// pair the runtime requires before it will speak a revision. It is the same
// predicate the session's voice resolution (internal/runtime) applies, so
// "current" means the same thing on both sides.
func hasReference(p store.VoiceProfile) bool {
	return p.State == store.VoiceStateApproved && p.Fingerprint != "" && p.ReferenceAssetID != ""
}

// CurrentRevision returns the character's current voice: the highest approved
// revision carrying a reference (ListVoiceProfiles orders by revision
// descending). ok is false when the character has no current take. The
// predicate is the runtime's, so the API's "current" and the voice the
// session speaks are the same revision.
func (s *Service) CurrentRevision(ctx context.Context, character string) (store.VoiceProfile, bool, error) {
	profiles, err := s.Store.ListVoiceProfiles(ctx, character)
	if err != nil {
		return store.VoiceProfile{}, false, fmt.Errorf("voices: list revisions of %s: %w", character, err)
	}
	for _, p := range profiles {
		if hasReference(p) {
			return p, true, nil
		}
	}
	return store.VoiceProfile{}, false, nil
}

// Undo restores the previous take as the character's current one (UX-03d).
// The named revision must be approved and the character's CURRENT revision —
// undoing a take that is not speaking would change nothing audibly — and the
// character must hold a lower approved revision to restore. The move is the
// named revision's approved -> retired; the previous take becomes current
// again, and both revisions stay in the store with their audio retrievable:
// old cache entries keep attributing to the retired revision (docs/spec/07
// step 8), and nothing here deletes a reference.
//
// One-step by construction: a retired row cannot move back (the state machine
// has no retired -> approved edge), so the undo cannot be repeated and an
// "undo the undo" does not exist.
//
// The check and the move are not one transaction: a regeneration approved in
// between would make the named revision not-current, and retiring it then is
// inert (the runtime was already speaking the newer take). At the appliance's
// single-UI concurrency that interleaving is the worst case, and it corrupts
// nothing — the store's compare-and-swap on the state still guards the row.
func (s *Service) Undo(ctx context.Context, revisionID string) (store.VoiceProfile, error) {
	cur, err := s.Store.GetVoiceRevision(ctx, revisionID)
	if err != nil {
		return store.VoiceProfile{}, err
	}
	if cur.State != store.VoiceStateApproved {
		return store.VoiceProfile{}, fmt.Errorf("%w: state %s, want %s", ErrInvalidState, cur.State, store.VoiceStateApproved)
	}
	highest, ok, err := s.CurrentRevision(ctx, cur.Character)
	if err != nil {
		return store.VoiceProfile{}, err
	}
	if !ok || highest.ID != cur.ID {
		return store.VoiceProfile{}, fmt.Errorf("%w: %s is not the current take of %s", ErrNothingToUndo, revisionID, cur.Character)
	}
	previous, ok, err := s.previousApprovedTake(ctx, cur)
	if err != nil {
		return store.VoiceProfile{}, err
	}
	if !ok {
		return store.VoiceProfile{}, fmt.Errorf("%w: %s has no previous approved take to restore", ErrNothingToUndo, cur.Character)
	}

	updated := cur
	updated.State = store.VoiceStateRetired
	updated.UpdatedAt = time.Now().UTC()
	if err := s.Store.UpdateVoiceProfile(ctx, updated); err != nil {
		return store.VoiceProfile{}, fmt.Errorf("voices: retire %s: %w", revisionID, err)
	}
	fresh, err := s.Store.GetVoiceRevision(ctx, revisionID)
	if err != nil {
		return store.VoiceProfile{}, err
	}
	// The note is the caller's evidence that the undo did what it promised:
	// the restored take is still there and is now the current one.
	if restored, ok2, err := s.CurrentRevision(ctx, cur.Character); err != nil || !ok2 || restored.ID != previous.ID {
		return fresh, fmt.Errorf("%w: %s is not current after the undo (found %s)", ErrNothingToUndo, previous.ID, restored.ID)
	}
	return fresh, nil
}

// previousApprovedTake returns the character's next-lower approved revision
// with a reference — the take an undo would restore.
func (s *Service) previousApprovedTake(ctx context.Context, current store.VoiceProfile) (store.VoiceProfile, bool, error) {
	profiles, err := s.Store.ListVoiceProfiles(ctx, current.Character)
	if err != nil {
		return store.VoiceProfile{}, false, fmt.Errorf("voices: list revisions of %s: %w", current.Character, err)
	}
	for _, p := range profiles { // revision descending
		if p.Revision >= current.Revision {
			continue
		}
		if hasReference(p) {
			return p, true, nil
		}
	}
	return store.VoiceProfile{}, false, nil
}
