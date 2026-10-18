// Graph and region-set readers/writers for the curator's graph and region
// pages (docs/spec/09 "/games/:id/graph" and "/games/:id/regions").
//
// Two queries and one write live here rather than in repo_content.go because
// they are page-shaped, not row-shaped:
//
//   - ListGraphNodes projects the list row of the graph page (id, kind, scene,
//     speaker, short normalized text, attribution) with the speaker and scene
//     names joined in, so the page renders "Marina" without a second read. It
//     reuses MessageFilter and the messages id-keyset cursor, so a filtered
//     graph page and a filtered dialogue page page through the same rows in
//     the same order.
//   - ReplaceRegions is the region-set write the region editor saves: the
//     whole set is replaced in one transaction, the set's revision counter is
//     advanced exactly once (docs/spec/09 "edits yield new region-set
//     revision"), and the profile's row_version moves with it so an
//     If-Match-carrying editor on another tab loses its next save with a
//     conflict, not a silent merge.

package store

import (
	"context"
	"database/sql"

	"github.com/dbirks/game-narrator/internal/games"
)

const (
	opListGraphNodes = "list graph nodes"
	opReplaceRegions = "replace regions"
	// graphNodeTable joins the display names in: the graph page renders
	// "Marina" and "Harbour" directly, and a null join (unattributed message,
	// un-scene line) is a legal row, not an error.
	graphNodeTable = "messages m LEFT JOIN scenes sc ON sc.id = m.scene_id LEFT JOIN characters c ON c.id = m.speaker_id"
	// graphNodeColumns is the projection: m.-qualified so the join columns
	// cannot collide (sc.id and m.id are both "id" unqualified).
	graphNodeColumns = "m.id, m.text_kind, m.normalized_text, m.attribution_status, m.attribution_confidence, m.scene_id, sc.name, m.speaker_id, c.name"
)

// GraphNode is one list row of the dialogue graph: the message's identity and
// its two display names, the normalized text (the API cuts it further for the
// row label), and the attribution a "verified vs inferred" label needs.
type GraphNode struct {
	ID                    string
	TextKind              games.TextKind
	NormalizedText        string
	AttributionStatus     games.AttributionStatus
	AttributionConfidence float64
	SceneID               string // empty = un-scene
	SceneName             string
	SpeakerID             string // empty = unattributed
	SpeakerName           string
}

// graphFilter adds the three graph filters — the same predicates the dialogue
// browser applies, so the two pages agree on what a filter selects. An empty
// field adds nothing: the zero filter is the revision's whole graph.
func graphFilter(b *builder, f MessageFilter) {
	if f.TextKind != "" {
		b.more("m.text_kind = ?", string(f.TextKind))
	}
	if f.SceneID != "" {
		b.more("m.scene_id = ?", f.SceneID)
	}
	if f.SpeakerID != "" {
		b.more("m.speaker_id = ?", f.SpeakerID)
	}
}

// ListGraphNodes returns one cursor-paginated page of a revision's graph
// nodes, filtered server-side the way the dialogue browser filters. See
// ListMessages for the keyset-cursor contract: ordered by id, the cursor is
// the last id seen, and an import landing mid-page cannot shift the window.
func (s *Store) ListGraphNodes(ctx context.Context, profileID string, f MessageFilter, cursor string, limit int) ([]GraphNode, string, error) {
	limit = clampLimit(limit, MaxListLimit)
	after, err := decodeCursor(cursor)
	if err != nil {
		return nil, "", err
	}

	var (
		out  []GraphNode
		next string
	)
	q := builder{table: graphNodeTable, columns: graphNodeColumns}
	q.more("m.profile_id = ?", profileID)
	q.more("m.id > ?", after)
	graphFilter(&q, f)

	err = s.inRead(ctx, func(ctx context.Context, r queryable) error {
		rows, err := r.query(ctx, opListGraphNodes, q.sql("m.id"), q.args(limit+1)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for len(out) <= limit && rows.Next() {
			n, err := scanGraphNode(rows)
			if err != nil {
				return err
			}
			out = append(out, n)
		}
		if len(out) > limit {
			next = encodeCursor(out[limit-1].ID)
			out = out[:limit]
		}
		return rows.Err()
	})
	if err != nil {
		return nil, "", err
	}
	return out, next, nil
}

// CountGraphNodes is the filtered node count for the page's "n of m" line:
// the same filter predicates as ListGraphNodes without the page, so the total
// and the rows cannot disagree about what the filters select.
func (s *Store) CountGraphNodes(ctx context.Context, profileID string, f MessageFilter) (int, error) {
	var total int
	cond := "m.profile_id = ?"
	vals := []any{profileID}
	if f.TextKind != "" {
		cond += " AND m.text_kind = ?"
		vals = append(vals, string(f.TextKind))
	}
	if f.SceneID != "" {
		cond += " AND m.scene_id = ?"
		vals = append(vals, f.SceneID)
	}
	if f.SpeakerID != "" {
		cond += " AND m.speaker_id = ?"
		vals = append(vals, f.SpeakerID)
	}
	err := s.inRead(ctx, func(ctx context.Context, r queryable) error {
		return r.queryRow(ctx, opListGraphNodes,
			"SELECT COUNT(*) FROM messages m WHERE "+cond, vals...).Scan(&total)
	})
	if err != nil {
		return 0, mapReadError(opListGraphNodes, err)
	}
	return total, nil
}

func scanGraphNode(r scanner) (GraphNode, error) {
	const op = opListGraphNodes
	var (
		n         GraphNode
		kind      string
		attrib    string
		scene     sql.NullString
		sceneName sql.NullString
		speaker   sql.NullString
		spName    sql.NullString
	)
	dest := []any{&n.ID, &kind, &n.NormalizedText, &attrib, &n.AttributionConfidence,
		&scene, &sceneName, &speaker, &spName}
	if err := r.Scan(dest...); err != nil {
		return n, mapReadError(op, err)
	}
	n.TextKind = games.TextKind(kind)
	n.AttributionStatus = games.AttributionStatus(attrib)
	n.SceneID = scene.String
	n.SceneName = sceneName.String
	n.SpeakerID = speaker.String
	n.SpeakerName = spName.String
	return n, nil
}

// ReplaceRegions replaces a draft revision's whole region set with rows and
// returns the stored rows (timestamps and the set revision server-assigned).
//
// It is the storage half of PUT /profiles/{id}/regions: draft-only through
// loadMutable (a published revision's boxes are frozen, same as its text),
// expectedRowVersion compared against the row the transaction loaded (a stale
// editor is ErrRevisionConflict, the wire 409/412 pair the other draft writes
// answer), and the region-set revision advanced from the stored set's maximum
// so the cached detections keyed on it (docs/spec/04) invalidate precisely.
func (s *Store) ReplaceRegions(ctx context.Context, profileID string, rows []games.Region, expectedRowVersion int64) ([]games.Region, error) {
	var out []games.Region
	err := s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		const op = opReplaceRegions
		p, rev, err := tx.loadMutable(ctx, op, profileID)
		if err != nil {
			return err
		}
		if rev != expectedRowVersion {
			return revisionConflict(op, profileID, expectedRowVersion, rev)
		}
		for i := range rows {
			rows[i].ProfileID = p.ID
		}

		setRevision, err := nextRegionSetRevision(ctx, tx, profileID)
		if err != nil {
			return err
		}
		if _, err := tx.exec(ctx, op, `DELETE FROM regions WHERE profile_id = ?`, profileID); err != nil {
			return err
		}
		now := nowStamp()
		for i := range rows {
			if rows[i].ID == "" {
				rows[i].ID = games.NewID()
			}
			rows[i].Revision = setRevision
			rows[i].UpdatedAt = now
			if err := rows[i].Validate(); err != nil {
				return err
			}
			if err := tx.putRegion(ctx, op, rows[i]); err != nil {
				return err
			}
		}
		if _, err := tx.bumpRowVersion(ctx, op, profileID, rev); err != nil {
			return err
		}
		out, err = loadRegions(ctx, tx, profileID)
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []games.Region{} // an empty set is a valid save; JSON wants [] not null
	}
	return out, nil
}

// nextRegionSetRevision is the set revision the next write stamps: one above
// the stored maximum (0002's `revision` column is per-row but names the set —
// one set, one number — which is what the UI's region-set revision badge shows).
func nextRegionSetRevision(ctx context.Context, tx *Tx, profileID string) (int, error) {
	var max sql.NullInt64
	if err := tx.queryRow(ctx, opReplaceRegions,
		"SELECT MAX(revision) FROM regions WHERE profile_id = ?", profileID).Scan(&max); err != nil {
		return 0, mapReadError(opReplaceRegions, err)
	}
	v := 0
	if max.Valid {
		v = int(max.Int64)
	}
	if v < 1 {
		return 1, nil
	}
	return v + 1, nil
}
