package voices

// Auto-cast (ADR-0109, docs/design/plum-peach UX-03): with a profile's
// auto_cast dial on, a voice audition run created for one of its characters
// ends approved instead of pending — one take, auto-approved, with no
// per-click consent prompt. The existing consent requirement (docs/spec/14,
// ADR 0005) is satisfied by the per-profile consent default: the approval
// records the default position attributed to the profile, not a user click.
//
// The seam (CastProfiles) is the slice of the catalogue a voice workflow
// needs: the dial and the character roster. Production wraps *store.Store
// (NewStoreCastProfiles); tests pass a scripted fake, so the domain's rules
// (consent attribution, the QA gate, the idempotent skip) are testable
// without a gateway.

import (
	"context"
	"errors"
	"fmt"

	"github.com/dbirks/game-narrator/internal/store"
)

// ConsentDefault is the per-profile consent default (docs/spec/14): the
// rights position for voices that are original, newly designed synthetic
// character voices. It is a constant, not user-editable prose — the consent
// wording is a rights statement, and an editable field would let a caller
// fabricate the approval docs/spec/14 forbids. What is per-profile is the
// attribution: AutoCastConsent names the profile whose default consented.
const ConsentDefault = "original_synthetic_design"

// AutoCaster is the approved_by identity of an auto-approved take (ADR-0109):
// the frozen row records the flow that approved it, which is how an audit
// distinguishes a profile-default approval from a human's click.
const AutoCaster = "auto-cast"

// NarratorCharacter is the conventional name of the fallback voice
// (internal/runtime spells the same key for the session's speaker lookup):
// the narrator is not a roster row, so the roster check lets it through.
const NarratorCharacter = "narrator"

// CastProfile is one cast context read: the dial and the roster the check
// needs. CharacterNames is the profile's roster as the store orders it; the
// narrator key always counts, whether or not it appears.
type CastProfile struct {
	AutoCast       bool
	CharacterNames []string
}

// HasCharacter reports whether the voice's character belongs to this cast:
// a roster row by name, or the narrator key (which is not a roster row).
// Roster names are case-sensitive: the voice's character column is the
// roster name verbatim, so a case-folded match would admit a spelling the
// profile never carried.
func (cp CastProfile) HasCharacter(name string) bool {
	if name == NarratorCharacter {
		return true
	}
	for _, n := range cp.CharacterNames {
		if n == name {
			return true
		}
	}
	return false
}

// CastProfiles is the seam to the catalogue profile the auto-cast flow reads
// (ADR-0109). It is the slice of the catalogue a voice workflow needs —
// nothing wider, so a test cannot accidentally reach the catalogue through it.
type CastProfiles interface {
	GetCastProfile(ctx context.Context, profileID string) (CastProfile, error)
}

// StoreCastProfiles adapts *store.Store to the CastProfiles seam (the dial
// from the profile row, migration 0018, and the roster from the character
// rows).
type StoreCastProfiles struct{ st *store.Store }

// NewStoreCastProfiles adapts *store.Store to the CastProfiles seam: the dial
// from the profile row (migration 0018) and the roster from the character
// rows. A missing profile is store.ErrNotFound, which the callers turn into
// their own 404/skip answers.
func NewStoreCastProfiles(st *store.Store) *StoreCastProfiles {
	return &StoreCastProfiles{st: st}
}

var _ CastProfiles = (*StoreCastProfiles)(nil)

func (c *StoreCastProfiles) GetCastProfile(ctx context.Context, profileID string) (CastProfile, error) {
	p, _, err := c.st.GetProfile(ctx, profileID)
	if err != nil {
		return CastProfile{}, err
	}
	chars, err := c.st.ListCharacters(ctx, profileID)
	if err != nil {
		return CastProfile{}, fmt.Errorf("voices: list characters of %s: %w", profileID, err)
	}
	names := make([]string, 0, len(chars))
	for _, ch := range chars {
		names = append(names, ch.Name)
	}
	return CastProfile{AutoCast: p.AutoCast, CharacterNames: names}, nil
}

// AutoCastConsent is the consent record an auto-approved take carries: the
// profile-level consent default attributed to the profile. The frozen row's
// consent_provenance therefore names both the default position and the
// profile it came from, and approved_by (AutoCaster) records that no click
// was involved — docs/spec/14's field is satisfied and audited without a
// per-character prompt.
func AutoCastConsent(profileID string) string {
	return fmt.Sprintf("%s:profile-default:%s", ConsentDefault, profileID)
}

// AutoApproveCast runs the auto-cast approval step of one audition job
// (ADR-0109). It re-reads the cast profile's dial — the enqueue-time read
// only chose the count, so a dial turned off after the run was enqueued is
// honoured here — and, with the dial on, approves the first audition of the
// revision whose ASR QA read-back is not failed.
//
// The returns are (approved, note, err):
//
//   - approved true: the revision moved auditioning -> approved with the
//     profile-default consent.
//   - approved false, err nil: the take stays pending review (auditioning);
//     note says why, and the job result carries it so the UI can show the
//     real reason instead of "stuck".
//   - err non-nil: a wiring or store fault the job must surface (the draft
//     is left in the state it was found in).
//
// The step is idempotent over a crash-restart: a revision that is already
// approved (a restart between Approve and job completion) is reported as
// already approved, not re-approved — Approve itself would refuse it, and
// the note says the restart found the work done.
func (s *Service) AutoApproveCast(ctx context.Context, castProfileID, voiceProfileID string) (bool, string, error) {
	if s.CastProfiles == nil {
		// A payload that names a cast profile on a service without the seam
		// is a wiring bug the enqueue side would normally refuse; a job that
		// already ran past the enqueue (a crash in between) must not loop on
		// it, so the answer is the skip, not the error.
		return false, "the auto-cast wiring is not available in this process; the take stays pending review", nil
	}
	cast, err := s.CastProfiles.GetCastProfile(ctx, castProfileID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// The fill already happened; a profile deleted mid-run must not
			// turn a finished fill into a failed job.
			return false, "the cast profile no longer exists; the take stays pending review", nil
		}
		return false, "", fmt.Errorf("voices: load cast profile %s: %w", castProfileID, err)
	}
	if !cast.AutoCast {
		return false, "auto_cast is off for the cast profile; the take stays pending review", nil
	}
	rev, err := s.Store.GetVoiceRevision(ctx, voiceProfileID)
	if err != nil {
		return false, "", fmt.Errorf("voices: auto-approve: load revision %s: %w", voiceProfileID, err)
	}
	if rev.State != store.VoiceStateAuditioning {
		if rev.State == store.VoiceStateApproved {
			return false, "the revision is already approved (a restart found the approval done); nothing to auto-approve", nil
		}
		return false, fmt.Sprintf("the revision is %s, not auditioning; nothing to auto-approve", rev.State), nil
	}
	auditions, err := s.Store.ListAuditions(ctx, voiceProfileID)
	if err != nil {
		return false, "", fmt.Errorf("voices: auto-approve: list auditions of %s: %w", voiceProfileID, err)
	}
	// ListAuditions orders by index: the first audition whose QA is not
	// failed is the take the cast gets. A failed QA (ADR 0061) is the only
	// auto-cast refusal — the flagged evidence stays on the row for review;
	// a not-run QA (the role unconfigured, the endpoint down) auto-approves,
	// because ADR 0061's degraded mode says neither playback nor approval
	// ever blocks on ASR.
	for _, au := range auditions {
		if au.AsrState == store.VoiceAuditionQAFailed {
			continue
		}
		if _, err := s.Approve(ctx, voiceProfileID, au.ID, AutoCastConsent(castProfileID), AutoCaster); err != nil {
			return false, "", fmt.Errorf("voices: auto-approve %s: %w", voiceProfileID, err)
		}
		return true, "auto-approved with the profile consent default", nil
	}
	return false, "every audition failed its ASR QA read-back; the take stays pending review", nil
}
