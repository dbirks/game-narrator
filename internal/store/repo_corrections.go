package store

// Correction repository (0002_content.sql, ADR-0052): the user fixes recorded
// as labels. A correction is the regression fixture of its target — the row
// carries what was wrong (before_value), what it should be (after_value, the
// assertion a re-run checks) and who decided (actor), and the target pointer is
// deliberately loose so the label outlives the row it was about.
//
// Writes are inserts, not upserts: a correction is a *record of an act*, and an
// act repeats only when somebody corrects the same target twice — which is two
// labels, not a duplicate. The one duplicate that would matter (the same editor
// save recorded twice) cannot happen: the editor's save records its correction
// inside the message write's own transaction (ADR-0052), and a transaction runs
// once or not at all.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dbirks/game-narrator/internal/games"
)

// correctionColumns is the scanner's column list, in scan order (written out
// rather than SELECT *, the same rule messageColumns follows).
const correctionColumns = "id, profile_id, kind, target_type, target_id, message_id, before_value, after_value, reason, evidence, actor, status, reviewer, created_at, reviewed_at"

// maxCorrectionsList bounds one list read: the endpoint is a bounded queue, not
// an unbounded dump (AGENTS.md "Keep runtime predictable").
const maxCorrectionsList = 500

// defaultCorrectionsList is the page a caller who did not ask for one gets.
const defaultCorrectionsList = 100

// PutCorrection records one correction on the profile it names.
//
// The profile FK is the only referential edge the table has that this method
// enforces: a correction that names a revision this store does not have would
// point at nothing, so it is refused before the write. The target and message
// pointers are not enforced here — 0002 makes them loose on purpose — but the
// API validates the message row a correction names.
func (s *Store) PutCorrection(ctx context.Context, c games.Correction) error {
	if err := c.Validate(); err != nil {
		return fmt.Errorf("store: record correction: %w", err)
	}
	return s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		const op = "record correction"
		var one int
		err := tx.queryRow(ctx, op, "SELECT 1 FROM profiles WHERE id = ?", c.ProfileID).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return notFound(op, "profile "+c.ProfileID)
		}
		if err != nil {
			return fmt.Errorf("store: %s: %w", op, err)
		}
		_, err = tx.exec(ctx, op, `INSERT INTO corrections (
			id, profile_id, kind, target_type, target_id, message_id,
			before_value, after_value, reason, evidence, actor,
			status, reviewer, created_at, reviewed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			c.ID, c.ProfileID, c.Kind, c.TargetType, c.TargetID, nullStr(c.MessageID),
			c.BeforeValue, c.AfterValue, c.Reason, orDefault(c.Evidence, "[]"),
			c.Actor, c.Status, c.Reviewer, c.CreatedAt, nullStr(c.ReviewedAt))
		if err != nil {
			return fmt.Errorf("store: %s: %w", op, err)
		}
		return nil
	})
}

// CorrectionFilter is the read half of ListCorrections: an empty field is no
// filter, limit is clamped to maxCorrectionsList.
type CorrectionFilter struct {
	ProfileID string
	Kind      string
	Limit     int
}

// ListCorrections returns corrections, newest first (created_at, then id so one
// second of edits keeps a stable order), with the optional profile and kind
// filters.
func (s *Store) ListCorrections(ctx context.Context, f CorrectionFilter) ([]games.Correction, error) {
	if f.Limit <= 0 {
		f.Limit = defaultCorrectionsList
	}
	if f.Limit > maxCorrectionsList {
		f.Limit = maxCorrectionsList
	}
	var out []games.Correction
	err := s.inRead(ctx, func(ctx context.Context, q queryable) error {
		const op = "list corrections"
		// A named profile that this store does not have is a 404, the way every
		// other profile-scoped read answers: the filter narrows a revision, it
		// does not invent one.
		if f.ProfileID != "" {
			var one int
			err := q.queryRow(ctx, op, "SELECT 1 FROM profiles WHERE id = ?", f.ProfileID).Scan(&one)
			if errors.Is(err, sql.ErrNoRows) {
				return notFound(op, "profile "+f.ProfileID)
			}
			if err != nil {
				return err
			}
		}
		where := []string{}
		vals := []any{}
		if f.ProfileID != "" {
			where = append(where, "profile_id = ?")
			vals = append(vals, f.ProfileID)
		}
		if f.Kind != "" {
			where = append(where, "kind = ?")
			vals = append(vals, f.Kind)
		}
		query := "SELECT " + correctionColumns + " FROM corrections"
		if len(where) > 0 {
			query += " WHERE " + where[0]
			for _, w := range where[1:] {
				query += " AND " + w
			}
		}
		query += " ORDER BY created_at DESC, id DESC LIMIT ?"
		vals = append(vals, f.Limit)

		rows, err := q.query(ctx, op, query, vals...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			c, err := scanCorrection(rows)
			if err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}

// GetCorrection returns one row by id.
func (s *Store) GetCorrection(ctx context.Context, id string) (games.Correction, error) {
	var out games.Correction
	err := s.inRead(ctx, func(ctx context.Context, q queryable) error {
		const op = "get correction"
		row := q.queryRow(ctx, op,
			"SELECT "+correctionColumns+" FROM corrections WHERE id = ?", id)
		c, err := scanCorrection(row)
		if err != nil {
			return err
		}
		out = c
		return nil
	})
	return out, err
}

func scanCorrection(r scanner) (games.Correction, error) {
	const op = "scan correction"
	var c games.Correction
	var kind, target, status string
	var messageID sql.NullString
	var evidence []byte
	var reviewer sql.NullString
	var reviewedAt sql.NullString
	err := r.Scan(&c.ID, &c.ProfileID, &kind, &target, &c.TargetID, &messageID,
		&c.BeforeValue, &c.AfterValue, &c.Reason, &evidence, &c.Actor,
		&status, &reviewer, &c.CreatedAt, &reviewedAt)
	if err != nil {
		return c, mapReadError(op, err)
	}
	c.Kind = games.CorrectionKind(kind)
	c.TargetType = games.CorrectionTarget(target)
	c.Status = games.CorrectionStatus(status)
	c.MessageID = messageID.String
	c.Reviewer = reviewer.String
	c.ReviewedAt = reviewedAt.String
	c.Evidence = json.RawMessage(append([]byte(nil), evidence...))
	return c, nil
}
