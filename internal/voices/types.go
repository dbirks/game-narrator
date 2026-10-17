package voices

import (
	"context"
	"errors"

	"github.com/dbirks/game-narrator/internal/store"
)

// Store is the persistence seam: production passes *store.Store (which
// satisfies it directly), tests pass a fake. It is the slice of the store a
// voice workflow needs — nothing wider, so a test cannot accidentally reach
// the catalogue through it.
type Store interface {
	// --- voice_profiles (one row per revision) ---
	CreateVoiceRevision(ctx context.Context, p store.VoiceProfile) error
	UpdateVoiceProfile(ctx context.Context, p store.VoiceProfile) error
	GetVoiceRevision(ctx context.Context, id string) (store.VoiceProfile, error)
	ListVoiceProfiles(ctx context.Context, character string) ([]store.VoiceProfile, error)
	ListApprovedVoiceRevisions(ctx context.Context) ([]store.VoiceProfile, error)
	NextVoiceRevision(ctx context.Context, character string) (int, error)

	// --- voice_auditions ---
	AddAuditions(ctx context.Context, voiceProfileID string, auditionIDs []string, auditions []store.VoiceAudition) error
	ListAuditions(ctx context.Context, voiceProfileID string) ([]store.VoiceAudition, error)
	GetAudition(ctx context.Context, id string) (store.VoiceAudition, error)

	// --- voice_registrations ---
	UpsertVoiceRegistration(ctx context.Context, r store.VoiceRegistration) error
	GetVoiceRegistration(ctx context.Context, voiceProfileID, instanceID string) (store.VoiceRegistration, error)
	ListVoiceRegistrations(ctx context.Context, voiceProfileID string) ([]store.VoiceRegistration, error)
}

// Sentinel errors. Callers branch with errors.Is; the messages carry no
// secrets (AGENTS.md rule 5).
var (
	// ErrConsentRequired: approval or registration was refused because the
	// consent record is empty (docs/spec/14 "Voice consent").
	ErrConsentRequired = errors.New("voices: consent is required (docs/spec/14): the consent record was empty")
	// ErrInvalidState: the revision is not in the state the operation needs.
	ErrInvalidState = errors.New("voices: revision is not in the required state")
	// ErrNotApproved: the revision has no approved reference.
	ErrNotApproved = errors.New("voices: revision is not approved")
	// ErrAuditionNotFound: the audition does not belong to the revision.
	ErrAuditionNotFound = errors.New("voices: audition not found on this revision")
	// ErrReferenceMissing: the canonical reference asset is gone or corrupt.
	ErrReferenceMissing = errors.New("voices: canonical reference asset is missing or unreadable")
)
