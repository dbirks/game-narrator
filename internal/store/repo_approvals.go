package store

// Approval token persistence (migration 0008, ADR-0026). The agent may only
// consume a token (publish_profile); tokens are issued by the human
// approval step outside the agent loop. The raw token is never stored —
// only its SHA-256 — so a database leak does not leak publication authority.

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/dbirks/game-narrator/internal/games"
)

// Approval is one row of agent_approvals. TokenHash is the SHA-256 of the
// raw token; the raw token itself exists only in the human's hands and in
// the tools-layer Approval value returned by Issue.
type Approval struct {
	ID         string
	ProfileID  string
	TokenHash  string
	GrantedBy  string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	ConsumedAt *time.Time
}

// CreateApproval stores one issued token. The caller (the human approval
// step) has already generated the raw token; this stores its hash and the
// audit fields. The raw token is only ever presented to ConsumeApproval.
func (s *Store) CreateApproval(ctx context.Context, a Approval) error {
	const op = "create approval"
	if !games.ValidID(a.ID) {
		return fmt.Errorf("store: %s: id is not a ULID", op)
	}
	if !games.ValidID(a.ProfileID) {
		return fmt.Errorf("store: %s: profile_id %q is not a ULID", op, a.ProfileID)
	}
	if len(a.TokenHash) != 64 {
		return fmt.Errorf("store: %s: token_hash must be 64 hex chars", op)
	}
	if len(a.GrantedBy) < 1 || len(a.GrantedBy) > 200 {
		return fmt.Errorf("store: %s: granted_by must be 1..200 chars", op)
	}
	if a.ExpiresAt.Before(a.CreatedAt) {
		return fmt.Errorf("store: %s: expires_at is before created_at", op)
	}
	// The profile must exist: a token for a nonexistent revision would be
	// unspendable, and unspendable grants are how dead credentials
	// accumulate.
	profile, _, err := s.GetProfile(ctx, a.ProfileID)
	if err != nil {
		return fmt.Errorf("store: %s: %w", op, err)
	}
	if profile.ID == "" {
		return notFound(op, "profile "+a.ProfileID)
	}
	return s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		_, err := tx.exec(ctx, op,
			`INSERT INTO agent_approvals (id, profile_id, token_hash, granted_by, created_at, expires_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			a.ID, a.ProfileID, a.TokenHash, a.GrantedBy,
			stampMS(a.CreatedAt), stampMS(a.ExpiresAt))
		return err
	})
}

// ConsumeApproval validates a token hash and burns it atomically: the
// guarded UPDATE flips consumed_at exactly once, so a crash between read
// and write cannot double-spend the token. It returns the approval (with
// ProfileID) or ErrNotFound for an unknown token, and ErrConflict for an
// already-spent or expired one.
func (s *Store) ConsumeApproval(ctx context.Context, tokenHash string, now time.Time) (Approval, error) {
	const op = "consume approval"
	if len(tokenHash) != 64 {
		return Approval{}, fmt.Errorf("store: %s: token_hash must be 64 hex chars", op)
	}
	nowUTC := now.UTC()
	nowStampVal := nowUTC.Format(time.RFC3339)
	var a Approval
	var created, expires string
	var consumed sql.NullString
	err := s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		err := tx.queryRow(ctx, op,
			`SELECT id, profile_id, granted_by, created_at, expires_at, consumed_at
			 FROM agent_approvals WHERE token_hash = ?`, tokenHash,
		).Scan(&a.ID, &a.ProfileID, &a.GrantedBy, &created, &expires, &consumed)
		if err != nil {
			if err == sql.ErrNoRows {
				return notFound(op, "approval token")
			}
			return mapReadError(op, err)
		}
		a.CreatedAt, err = parseStamp(created)
		if err != nil {
			return mapReadError(op, err)
		}
		a.ExpiresAt, err = parseStamp(expires)
		if err != nil {
			return mapReadError(op, err)
		}
		if consumed.Valid {
			return fmt.Errorf("store: %s: token already used: %w", op, ErrConflict)
		}
		if nowUTC.After(a.ExpiresAt) {
			return fmt.Errorf("store: %s: token expired at %s: %w", op, a.ExpiresAt.Format(time.RFC3339), ErrConflict)
		}
		res, err := tx.exec(ctx, op,
			`UPDATE agent_approvals SET consumed_at = ?
			 WHERE token_hash = ? AND consumed_at IS NULL`,
			nowStampVal, tokenHash)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return mapReadError(op, err)
		}
		if n == 0 {
			// Lost the race: the token was spent between SELECT and UPDATE.
			return fmt.Errorf("store: %s: token already used: %w", op, ErrConflict)
		}
		a.ConsumedAt = &nowUTC
		return nil
	})
	if err != nil {
		return Approval{}, err
	}
	return a, nil
}
