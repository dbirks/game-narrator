package tools

// SQLiteApprovals is the production ApprovalStore: migration 0008's
// agent_approvals table, single-use enforced durably (the consumed_at
// UPDATE is guarded inside one write transaction), expiry by timestamp.
// The raw token is never written to the database — only its SHA-256.

import (
	"context"
	"fmt"
	"time"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/store"
)

// NewSQLiteApprovals wraps the store's approval repository.
func NewSQLiteApprovals(s *store.Store) ApprovalStore {
	return &sqliteApprovals{store: s}
}

type sqliteApprovals struct {
	store *store.Store
}

// Issue implements ApprovalStore.
func (s *sqliteApprovals) Issue(ctx context.Context, profileID, grantedBy string) (Approval, error) {
	if !games.ValidID(profileID) {
		return Approval{}, fmt.Errorf("tools: approval: profile id %q is not a ULID", profileID)
	}
	if grantedBy == "" {
		return Approval{}, fmt.Errorf("tools: approval: granted_by is required")
	}
	token, err := mintToken()
	if err != nil {
		return Approval{}, err
	}
	now := time.Now().UTC()
	a := store.Approval{
		ID:        games.NewID(),
		ProfileID: profileID,
		TokenHash: tokenHash(token),
		GrantedBy: grantedBy,
		CreatedAt: now,
		ExpiresAt: now.Add(ApprovalTTL),
	}
	if err := s.store.CreateApproval(ctx, a); err != nil {
		return Approval{}, fmt.Errorf("tools: approval: issue: %w", err)
	}
	out := Approval{
		ID: a.ID, ProfileID: profileID, GrantedBy: grantedBy,
		CreatedAt: a.CreatedAt, ExpiresAt: a.ExpiresAt, Token: token,
	}
	return out, nil
}

// Consume implements ApprovalStore. Unknown, spent and expired tokens all
// fail; the burn is durable (a crash between read and write cannot
// double-spend: the guarded UPDATE is the single commit point).
func (s *sqliteApprovals) Consume(ctx context.Context, token string) (Approval, error) {
	a, err := s.store.ConsumeApproval(ctx, tokenHash(token), time.Now().UTC())
	if err != nil {
		return Approval{}, fmt.Errorf("tools: approval: consume: %w", err)
	}
	return Approval{
		ID: a.ID, ProfileID: a.ProfileID, GrantedBy: a.GrantedBy,
		CreatedAt: a.CreatedAt, ExpiresAt: a.ExpiresAt,
	}, nil
}
