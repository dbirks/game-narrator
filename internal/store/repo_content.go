package store

// Content repository: the rows that make up one profile revision (0002_content.sql).
//
// Two invariants govern everything here, both argued in the migration comments and
// restated in docs/spec/02 "Transaction workflows":
//
//  1. Content belongs to a *draft*. A published or archived revision is frozen;
//     the way to change it is NewRevision, so a session that pinned revision 2
//     keeps hearing revision 2's words (docs/spec/01 "Runtime and setup
//     isolation").
//  2. Every mutation bumps the profile's row_version exactly once per call, so a
//     batch import and a one-field edit are equally visible to a concurrent
//     editor holding an If-Match token.
//
// Writes are upserts keyed by the caller's id rather than insert-then-update,
// because the import path (docs/spec/04 adapters, docs/spec/03 onboarding) re-runs
// after a crash and docs/spec/02 requires that to "reconcile, not duplicate": a
// parser that replays the same 5 000 messages must end with 5 000 rows.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dbirks/game-narrator/internal/games"
)

// Column lists, in scanner order. Written out rather than SELECT * so a later
// migration adding a column cannot silently shift a positional scan.
const (
	sourceColumns    = "id, profile_id, source_type, uri, retrieved_at, content_sha256, mime_type, rights_status, trust, parser, coverage_notes"
	characterColumns = "id, profile_id, name, aliases, visual_descriptors, personality_summary, role, verified_source_refs, portrait_asset_id, status, created_at"
	sceneColumns     = "id, profile_id, name, aliases, region_fingerprint, visual_anchors, progress_json, created_at"
	regionColumns    = "id, profile_id, scene_id, label, region_type, detect_method, rect_x, rect_y, rect_w, rect_h, min_aspect, max_aspect, min_width_px, min_height_px, ocr_settings, mask_asset_ids, reliability, revision, updated_at"
	messageColumns   = "id, profile_id, source_id, source_message_id, original_text, markup_json, normalized_text, speech_text, language, text_kind, scene_id, speaker_id, attribution_status, attribution_confidence, attribution_evidence, flags, revision, created_at, updated_at"
	pageColumns      = "message_id, ordinal, source_page_id, page_text, visible_prefixes, display_controls, timing_json"
	edgeColumns      = "from_id, to_id, edge_type, condition_expr, source_id, evidence, confidence, created_at"
	assertionColumns = "id, profile_id, message_id, character_id, method, asserted_at, evidence, posterior, reviewer"
)

// contentWrite runs one mutation against a draft revision: refuse an immutable
// target, do the work, and record that the revision changed, compare-and-swapping
// the version this transaction loaded. The three steps are in this one helper rather
// than copy-pasted into each Put method because forgetting step 1 is the bug
// docs/spec/12 calls out as a release blocker, and forgetting step 3 silently breaks
// optimistic locking.
func (tx *Tx) contentWrite(ctx context.Context, op, profileID string, fn func() error) error {
	_, rev, err := tx.loadMutable(ctx, op, profileID)
	if err != nil {
		return err
	}
	if err := fn(); err != nil {
		return err
	}
	_, err = tx.bumpRowVersion(ctx, op, profileID, rev)
	return err
}

// sameProfile guards the one caller error that would otherwise be invisible: a
// row that says it belongs to revision A being written into revision B. SQLite
// would accept it (the FK only requires the profile to exist) and the message
// would then be missing from the revision its own profile_id names.
func sameProfile(op, kind, want, have string) error {
	if want != have {
		return fmt.Errorf("store: %s: %s claims profile %s but is being written to profile %s: %w",
			op, kind, have, want, ErrConflict)
	}
	return nil
}

// ── sources ─────────────────────────────────────────────────────────────────

// PutSources registers provenance rows on a draft revision.
//
// Sources are the one content table that is *write-once*: docs/spec/01 key
// invariant 6 makes import provenance immutable after registration, with
// corrections as overlays rather than edits. So a repeat of the identical row is a
// no-op (the idempotent import docs/spec/02 requires) while a repeat that changes
// the URI or rights status of something already registered is ErrConflict — the
// honest answer, since silently rewriting where text came from would invalidate
// every review decision that relied on it.
func (s *Store) PutSources(ctx context.Context, profileID string, rows []games.Source) error {
	return s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		const op = "put sources"
		return tx.contentWrite(ctx, op, profileID, func() error {
			for _, r := range rows {
				if err := sameProfile(op, "source "+r.ID, profileID, r.ProfileID); err != nil {
					return err
				}
				if err := tx.putSource(ctx, op, r); err != nil {
					return err
				}
			}
			return nil
		})
	})
}

// SetSourceRights records the user's rights assertion on one registered
// source of a draft revision (ADR-0077). It is the only operation that may
// change rights_status on a registered row: every other provenance field
// (URI, content hash, retrieval time) is untouched, which is what keeps
// docs/spec/01 key invariant 6 (provenance is immutable after registration)
// intact for the fields that say where the bytes came from. The rights
// position is the field the schema deliberately leaves open — a fetched web
// page registers as `unknown` — and the publication rights gate is only
// satisfiable if a human's assertion can land on the row the content rests
// on; an unrecordable assertion would make the refusal a lock.
//
// The sole production caller is the token-gated approved-import path
// (import_web_source after a consumed human approval), so the assertion
// always has a spent single-use approval behind it. note is the audit
// sentence appended to coverage_notes (who asserted, when); it carries no
// secrets — the approval token itself is never persisted here or anywhere.
func (s *Store) SetSourceRights(ctx context.Context, profileID, sourceID string, rights games.RightsStatus, note string) error {
	if !rights.Publishable() {
		return fmt.Errorf("store: set source rights: %q is not a position a user can assert (user_supplied or licensed); the question stays unknown until a human answers it", rights)
	}
	if len(note) > 1024 {
		return fmt.Errorf("store: set source rights: the audit note is %d bytes, above the 1024-byte bound", len(note))
	}
	return s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		const op = "set source rights"
		return tx.contentWrite(ctx, op, profileID, func() error {
			existing, err := loadSourceByID(ctx, tx, profileID, sourceID)
			if err != nil {
				return err
			}
			newNotes := existing.CoverageNotes
			if note != "" {
				if newNotes != "" {
					newNotes += "; "
				}
				newNotes += note
			}
			_, err = tx.exec(ctx, op,
				`UPDATE sources SET rights_status = ?, coverage_notes = ? WHERE profile_id = ? AND id = ?`,
				string(rights), newNotes, profileID, sourceID)
			return err
		})
	})
}

func (tx *Tx) putSource(ctx context.Context, op string, r games.Source) error {
	if err := r.Validate(); err != nil {
		return err
	}
	res, err := tx.exec(ctx, op,
		`INSERT INTO sources (id, profile_id, source_type, uri, retrieved_at, content_sha256, mime_type, rights_status, trust, parser, coverage_notes)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (id) DO NOTHING`,
		r.ID, r.ProfileID, string(r.SourceType), r.URI, r.RetrievedAt, r.ContentSHA256,
		r.MimeType, string(r.Rights), r.Trust, r.Parser, r.CoverageNotes)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	// The id already exists. Compare before calling it a replay: an id that names
	// different bytes is not the same source, and pretending otherwise would let a
	// second import relabel the provenance of curated text.
	existing, err := loadSourceByID(ctx, tx, r.ProfileID, r.ID)
	if err != nil {
		return err
	}
	if existing == r {
		return nil
	}
	return fmt.Errorf("store: %s: source %s is already registered as %q rights %q from %q and provenance is immutable (docs/spec/01 invariant 6); register a new source id: %w",
		op, r.ID, existing.SourceType, existing.Rights, existing.URI, ErrImmutable)
}

// ListSources returns a revision's provenance rows in id order.
func (s *Store) ListSources(ctx context.Context, profileID string) ([]games.Source, error) {
	var out []games.Source
	err := s.inRead(ctx, func(ctx context.Context, q queryable) error {
		var err error
		out, err = loadSources(ctx, q, profileID)
		return err
	})
	return out, err
}

func loadSources(ctx context.Context, q queryable, profileID string) ([]games.Source, error) {
	const op = "list sources"
	rows, err := q.query(ctx, op, "SELECT "+sourceColumns+" FROM sources WHERE profile_id = ? ORDER BY id", profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []games.Source
	for rows.Next() {
		r, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func loadSourceByID(ctx context.Context, q queryable, profileID, id string) (games.Source, error) {
	const op = "get source"
	var out games.Source
	err := q.queryRow(ctx, op, "SELECT "+sourceColumns+" FROM sources WHERE profile_id = ? AND id = ?", profileID, id).Scan(
		&out.ID, &out.ProfileID, &out.SourceType, &out.URI, &out.RetrievedAt, &out.ContentSHA256,
		&out.MimeType, &out.Rights, &out.Trust, &out.Parser, &out.CoverageNotes)
	if err != nil {
		return out, mapReadError(op, err)
	}
	return out, nil
}

func scanSource(r scanner) (games.Source, error) {
	var (
		s  games.Source
		st string
		rt string
	)
	if err := r.Scan(&s.ID, &s.ProfileID, &st, &s.URI, &s.RetrievedAt, &s.ContentSHA256,
		&s.MimeType, &rt, &s.Trust, &s.Parser, &s.CoverageNotes); err != nil {
		return s, mapReadError("list sources", err)
	}
	s.SourceType = games.SourceType(st)
	s.Rights = games.RightsStatus(rt)
	return s, nil
}

func (tx *Tx) insertSource(ctx context.Context, op string, r games.Source) error {
	return tx.putSource(ctx, op, r)
}

// ── characters ──────────────────────────────────────────────────────────────

// PutCharacters upserts a revision's roster (0002_content.sql `characters`).
func (s *Store) PutCharacters(ctx context.Context, profileID string, rows []games.Character) error {
	return s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		const op = "put characters"
		return tx.contentWrite(ctx, op, profileID, func() error {
			for _, r := range rows {
				if err := sameProfile(op, "character "+r.ID, profileID, r.ProfileID); err != nil {
					return err
				}
				if err := tx.putCharacter(ctx, op, r); err != nil {
					return err
				}
			}
			return nil
		})
	})
}

func (tx *Tx) putCharacter(ctx context.Context, op string, c games.Character) error {
	if err := c.Validate(); err != nil {
		return err
	}
	// PortraitAssetID has no foreign key (assets are content-addressed and shared,
	// 0002_content.sql), so '' and NULL mean the same thing here; write '' as the
	// schema's own default so a read never has to handle both.
	_, err := tx.exec(ctx, op,
		`INSERT INTO characters (id, profile_id, name, aliases, visual_descriptors, personality_summary, role, verified_source_refs, portrait_asset_id, status, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (id) DO UPDATE SET
			name = excluded.name,
			aliases = excluded.aliases,
			visual_descriptors = excluded.visual_descriptors,
			personality_summary = excluded.personality_summary,
			role = excluded.role,
			verified_source_refs = excluded.verified_source_refs,
			portrait_asset_id = excluded.portrait_asset_id,
			status = excluded.status`,
		c.ID, c.ProfileID, c.Name, orDefault(c.Aliases, "[]"), orDefault(c.VisualDescriptors, "[]"),
		c.PersonalitySummary, c.Role, orDefault(c.VerifiedSourceRefs, "[]"), c.PortraitAssetID,
		string(c.Status), c.CreatedAt)
	return err
}

// insertCharacter is the snapshot path: a freshly minted id cannot conflict, so
// the upsert degrades to an insert.
func (tx *Tx) insertCharacter(ctx context.Context, op string, c games.Character) error {
	return tx.putCharacter(ctx, op, c)
}

// ListCharacters returns a revision's roster, by id.
func (s *Store) ListCharacters(ctx context.Context, profileID string) ([]games.Character, error) {
	var out []games.Character
	err := s.inRead(ctx, func(ctx context.Context, q queryable) error {
		var err error
		out, err = loadCharacters(ctx, q, profileID)
		return err
	})
	return out, err
}

// ProfileIDsForCharacter answers "which profile revisions contain a character
// with this exact display name" (ADR-0108): the setup board's voice events
// are keyed by character name — the voice surface's own identity (the
// voice_profiles.character column, /api/v1/voices?character=) — and this is
// the join back to the profile a board belongs to. Distinct profile ids, in
// stable order; an empty slice (not an error) is the honest "no profile has
// that character" answer.
func (s *Store) ProfileIDsForCharacter(ctx context.Context, name string) ([]string, error) {
	const op = "profile ids for character"
	var out []string
	err := s.inRead(ctx, func(ctx context.Context, q queryable) error {
		rows, err := q.query(ctx, op, "SELECT DISTINCT profile_id FROM characters WHERE name = ? ORDER BY profile_id", name)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return mapReadError(op, err)
			}
			out = append(out, id)
		}
		return rows.Err()
	})
	return out, err
}

func loadCharacters(ctx context.Context, q queryable, profileID string) ([]games.Character, error) {
	const op = "list characters"
	rows, err := q.query(ctx, op, "SELECT "+characterColumns+" FROM characters WHERE profile_id = ? ORDER BY id", profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []games.Character
	for rows.Next() {
		c, err := scanCharacter(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func scanCharacter(r scanner) (games.Character, error) {
	var (
		c       games.Character
		aliases []byte
		visual  []byte
		refs    []byte
		status  string
	)
	if err := r.Scan(&c.ID, &c.ProfileID, &c.Name, &aliases, &visual, &c.PersonalitySummary,
		&c.Role, &refs, &c.PortraitAssetID, &status, &c.CreatedAt); err != nil {
		return c, mapReadError("list characters", err)
	}
	c.Aliases = json.RawMessage(append([]byte(nil), aliases...))
	c.VisualDescriptors = json.RawMessage(append([]byte(nil), visual...))
	c.VerifiedSourceRefs = json.RawMessage(append([]byte(nil), refs...))
	c.Status = games.CharacterStatus(status)
	return c, nil
}

// ── scenes ──────────────────────────────────────────────────────────────────

// PutScenes upserts a revision's scenes.
func (s *Store) PutScenes(ctx context.Context, profileID string, rows []games.Scene) error {
	return s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		const op = "put scenes"
		return tx.contentWrite(ctx, op, profileID, func() error {
			for _, r := range rows {
				if err := sameProfile(op, "scene "+r.ID, profileID, r.ProfileID); err != nil {
					return err
				}
				if err := tx.putScene(ctx, op, r); err != nil {
					return err
				}
			}
			return nil
		})
	})
}

func (tx *Tx) putScene(ctx context.Context, op string, sc games.Scene) error {
	if err := sc.Validate(); err != nil {
		return err
	}
	_, err := tx.exec(ctx, op,
		`INSERT INTO scenes (id, profile_id, name, aliases, region_fingerprint, visual_anchors, progress_json, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (id) DO UPDATE SET
			name = excluded.name,
			aliases = excluded.aliases,
			region_fingerprint = excluded.region_fingerprint,
			visual_anchors = excluded.visual_anchors,
			progress_json = excluded.progress_json`,
		sc.ID, sc.ProfileID, sc.Name, orDefault(sc.Aliases, "[]"), sc.RegionFingerprint,
		orDefault(sc.VisualAnchors, "[]"), orDefault(sc.ProgressJSON, "{}"), sc.CreatedAt)
	return err
}

func (tx *Tx) insertScene(ctx context.Context, op string, sc games.Scene) error {
	return tx.putScene(ctx, op, sc)
}

// ListScenes returns a revision's scenes, by id.
func (s *Store) ListScenes(ctx context.Context, profileID string) ([]games.Scene, error) {
	var out []games.Scene
	err := s.inRead(ctx, func(ctx context.Context, q queryable) error {
		var err error
		out, err = loadScenes(ctx, q, profileID)
		return err
	})
	return out, err
}

func loadScenes(ctx context.Context, q queryable, profileID string) ([]games.Scene, error) {
	const op = "list scenes"
	rows, err := q.query(ctx, op, "SELECT "+sceneColumns+" FROM scenes WHERE profile_id = ? ORDER BY id", profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []games.Scene
	for rows.Next() {
		sc, err := scanScene(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

func scanScene(r scanner) (games.Scene, error) {
	var (
		sc       games.Scene
		aliases  []byte
		anchors  []byte
		progress []byte
	)
	if err := r.Scan(&sc.ID, &sc.ProfileID, &sc.Name, &aliases, &sc.RegionFingerprint,
		&anchors, &progress, &sc.CreatedAt); err != nil {
		return sc, mapReadError("list scenes", err)
	}
	sc.Aliases = json.RawMessage(append([]byte(nil), aliases...))
	sc.VisualAnchors = json.RawMessage(append([]byte(nil), anchors...))
	sc.ProgressJSON = json.RawMessage(append([]byte(nil), progress...))
	return sc, nil
}

// ── regions ─────────────────────────────────────────────────────────────────

// PutRegions upserts a revision's screen regions (docs/spec/09's calibration
// output). Region.Revision is preserved as supplied rather than recomputed: it is
// the region-set revision an operator advanced, and docs/spec/09 uses it to
// invalidate cached detections precisely.
func (s *Store) PutRegions(ctx context.Context, profileID string, rows []games.Region) error {
	return s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		const op = "put regions"
		return tx.contentWrite(ctx, op, profileID, func() error {
			for _, r := range rows {
				if err := sameProfile(op, "region "+r.ID, profileID, r.ProfileID); err != nil {
					return err
				}
				if err := tx.putRegion(ctx, op, r); err != nil {
					return err
				}
			}
			return nil
		})
	})
}

func (tx *Tx) putRegion(ctx context.Context, op string, rg games.Region) error {
	if err := rg.Validate(); err != nil {
		return err
	}
	_, err := tx.exec(ctx, op,
		`INSERT INTO regions (id, profile_id, scene_id, label, region_type, detect_method, rect_x, rect_y, rect_w, rect_h, min_aspect, max_aspect, min_width_px, min_height_px, ocr_settings, mask_asset_ids, reliability, revision, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (id) DO UPDATE SET
			scene_id = excluded.scene_id,
			label = excluded.label,
			region_type = excluded.region_type,
			detect_method = excluded.detect_method,
			rect_x = excluded.rect_x,
			rect_y = excluded.rect_y,
			rect_w = excluded.rect_w,
			rect_h = excluded.rect_h,
			min_aspect = excluded.min_aspect,
			max_aspect = excluded.max_aspect,
			min_width_px = excluded.min_width_px,
			min_height_px = excluded.min_height_px,
			ocr_settings = excluded.ocr_settings,
			mask_asset_ids = excluded.mask_asset_ids,
			reliability = excluded.reliability,
			revision = excluded.revision,
			updated_at = excluded.updated_at`,
		rg.ID, rg.ProfileID, nullStr(rg.SceneID), rg.Label, string(rg.RegionType), string(rg.DetectMethod),
		rg.RectX, rg.RectY, rg.RectW, rg.RectH, nullFloat(rg.MinAspect), nullFloat(rg.MaxAspect),
		nullInt(rg.MinWidthPx), nullInt(rg.MinHeightPx), orDefault(rg.OCRSettings, "{}"),
		orDefault(rg.MaskAssetIDs, "[]"), nullFloat(rg.Reliability), rg.Revision, rg.UpdatedAt)
	return err
}

func (tx *Tx) insertRegion(ctx context.Context, op string, rg games.Region) error {
	return tx.putRegion(ctx, op, rg)
}

// ListRegions returns a revision's regions, by id.
func (s *Store) ListRegions(ctx context.Context, profileID string) ([]games.Region, error) {
	var out []games.Region
	err := s.inRead(ctx, func(ctx context.Context, q queryable) error {
		var err error
		out, err = loadRegions(ctx, q, profileID)
		return err
	})
	return out, err
}

func loadRegions(ctx context.Context, q queryable, profileID string) ([]games.Region, error) {
	const op = "list regions"
	rows, err := q.query(ctx, op, "SELECT "+regionColumns+" FROM regions WHERE profile_id = ? ORDER BY id", profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []games.Region
	for rows.Next() {
		rg, err := scanRegion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rg)
	}
	return out, rows.Err()
}

func scanRegion(r scanner) (games.Region, error) {
	var (
		rg        games.Region
		scene     sql.NullString
		rtype     string
		detect    string
		minAspect sql.NullFloat64
		maxAspect sql.NullFloat64
		minW      sql.NullInt64
		minH      sql.NullInt64
		ocr       []byte
		masks     []byte
		rel       sql.NullFloat64
	)
	if err := r.Scan(&rg.ID, &rg.ProfileID, &scene, &rg.Label, &rtype, &detect,
		&rg.RectX, &rg.RectY, &rg.RectW, &rg.RectH, &minAspect, &maxAspect, &minW, &minH,
		&ocr, &masks, &rel, &rg.Revision, &rg.UpdatedAt); err != nil {
		return rg, mapReadError("list regions", err)
	}
	rg.SceneID = scene.String
	rg.RegionType = games.RegionType(rtype)
	rg.DetectMethod = games.DetectMethod(detect)
	rg.MinAspect = optFloat(minAspect)
	rg.MaxAspect = optFloat(maxAspect)
	rg.MinWidthPx = optInt(minW)
	rg.MinHeightPx = optInt(minH)
	rg.OCRSettings = json.RawMessage(append([]byte(nil), ocr...))
	rg.MaskAssetIDs = json.RawMessage(append([]byte(nil), masks...))
	rg.Reliability = optFloat(rel)
	return rg, nil
}

// ── messages and pages ──────────────────────────────────────────────────────

// PutMessages upserts messages and their display pages in one batch.
//
// The batch is a single transaction, and the profile's row version moves once:
// a 5 000-message import that bumped it per row would make every concurrent
// editor's If-Match token expire 5 000 times, and the editor would never be able
// to save.
//
// Message.Revision is server-owned: an insert takes the supplied value (1 for a
// fresh parse) and an update increments what is stored, ignoring whatever the
// caller sent. That is what makes the per-message counter a fact about the row
// rather than an argument.
func (s *Store) PutMessages(ctx context.Context, profileID string, msgs []games.Message, pages []games.MessagePage) error {
	return s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		const op = "put messages"
		return tx.contentWrite(ctx, op, profileID, func() error {
			for _, m := range msgs {
				if err := sameProfile(op, "message "+m.ID, profileID, m.ProfileID); err != nil {
					return err
				}
				if err := tx.putMessage(ctx, op, m); err != nil {
					return err
				}
			}
			for _, p := range pages {
				if err := tx.putPage(ctx, op, p); err != nil {
					return err
				}
			}
			return nil
		})
	})
}

func (tx *Tx) putMessage(ctx context.Context, op string, m games.Message) error {
	if err := m.Validate(); err != nil {
		return err
	}
	_, err := tx.exec(ctx, op,
		`INSERT INTO messages (id, profile_id, source_id, source_message_id, original_text, markup_json, normalized_text, speech_text, language, text_kind, scene_id, speaker_id, attribution_status, attribution_confidence, attribution_evidence, flags, revision, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (id) DO UPDATE SET
			source_id = excluded.source_id,
			source_message_id = excluded.source_message_id,
			original_text = excluded.original_text,
			markup_json = excluded.markup_json,
			normalized_text = excluded.normalized_text,
			speech_text = excluded.speech_text,
			language = excluded.language,
			text_kind = excluded.text_kind,
			scene_id = excluded.scene_id,
			speaker_id = excluded.speaker_id,
			attribution_status = excluded.attribution_status,
			attribution_confidence = excluded.attribution_confidence,
			attribution_evidence = excluded.attribution_evidence,
			flags = excluded.flags,
			revision = messages.revision + 1,
			updated_at = excluded.updated_at`,
		m.ID, m.ProfileID, nullStr(m.SourceID), nullStr(m.SourceMessageID), m.OriginalText,
		orDefault(m.MarkupJSON, "[]"), m.NormalizedText, m.SpeechText, m.Language, string(m.TextKind),
		nullStr(m.SceneID), nullStr(m.SpeakerID), string(m.AttributionStatus), m.AttributionConfidence,
		orDefault(m.AttributionEvidence, "[]"), orDefault(m.Flags, "{}"), m.Revision,
		m.CreatedAt, m.UpdatedAt)
	return err
}

func (tx *Tx) insertMessage(ctx context.Context, op string, m games.Message) error {
	return tx.putMessage(ctx, op, m)
}

func (tx *Tx) putPage(ctx context.Context, op string, p games.MessagePage) error {
	if err := p.Validate(); err != nil {
		return err
	}
	// Pages have no id of their own — (message_id, ordinal) *is* the identity
	// (0002_content.sql) — so the conflict target is composite and a re-import
	// that re-paginates a message leaves no stale page behind at a higher ordinal.
	// Callers that shrink a message's page count delete explicitly with
	// DeleteMessagePagesBelow; docs/spec/02 keeps page ordering meaningful.
	_, err := tx.exec(ctx, op,
		`INSERT INTO message_pages (message_id, ordinal, source_page_id, page_text, visible_prefixes, display_controls, timing_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (message_id, ordinal) DO UPDATE SET
			source_page_id = excluded.source_page_id,
			page_text = excluded.page_text,
			visible_prefixes = excluded.visible_prefixes,
			display_controls = excluded.display_controls,
			timing_json = excluded.timing_json`,
		p.MessageID, p.Ordinal, nullStr(p.SourcePageID), p.PageText,
		orDefault(p.VisiblePrefixes, "[]"), orDefault(p.DisplayControls, "[]"),
		orDefault(p.TimingJSON, "{}"))
	return err
}

func (tx *Tx) insertPage(ctx context.Context, op string, p games.MessagePage) error {
	return tx.putPage(ctx, op, p)
}

// GetMessage loads one message with its display pages. A message with no pages is
// normal (a hand-entered line, a dump row that never needed pagination), so an
// empty slice is a success, not a miss.
func (s *Store) GetMessage(ctx context.Context, profileID, messageID string) (games.Message, []games.MessagePage, error) {
	var (
		m     games.Message
		pages []games.MessagePage
	)
	err := s.inRead(ctx, func(ctx context.Context, q queryable) error {
		var err error
		m, err = scanMessage(q.queryRow(ctx, opGetMessage,
			"SELECT "+messageColumns+" FROM messages WHERE profile_id = ? AND id = ?", profileID, messageID))
		if err != nil {
			return err
		}
		pages, err = loadPages(ctx, q, messageID)
		return err
	})
	return m, pages, err
}

// MessageFilter narrows ListMessages. An empty field means "not filtered"; the
// zero filter is "every message of this revision", which is what the dialogue
// browser's default view is (docs/spec/09).
type MessageFilter struct {
	TextKind  games.TextKind
	SceneID   string
	SpeakerID string
}

// ListMessages returns one cursor-paginated page of a revision's messages.
//
// Ordering by id (a ULID) is what makes the cursor a keyset rather than an offset:
// a page boundary is a row identity, so an import appending messages while a
// curator pages through cannot shift the window and repeat or skip a line
// (docs/spec/12's pagination property). The filters match
// messages_profile_scene_kind_idx and messages_profile_speaker_idx, so filtering
// does not degrade into a scan of the revision.
func (s *Store) ListMessages(ctx context.Context, profileID string, f MessageFilter, cursor string, limit int) ([]games.Message, string, error) {
	limit = clampLimit(limit, MaxListLimit)
	after, err := decodeCursor(cursor)
	if err != nil {
		return nil, "", err
	}

	var (
		out  []games.Message
		next string
	)
	q := builder{table: "messages", columns: messageColumns}
	// The profile predicate comes first and is unconditional: a message list that
	// forgot it would silently merge every revision of every edition into one page,
	// which is both a correctness bug and the leak docs/spec/01's revision pinning
	// exists to prevent. `more` is the only way to add a condition precisely because
	// condition and value are appended together — a value without its placeholder
	// shifts every later bind, and `LIMIT ?` receiving a ULID is a datatype
	// mismatch rather than the obvious error a reviewer would catch.
	q.more("profile_id = ?", profileID)
	q.more("id > ?", after)
	if f.TextKind != "" {
		q.more("text_kind = ?", string(f.TextKind))
	}
	if f.SceneID != "" {
		q.more("scene_id = ?", f.SceneID)
	}
	if f.SpeakerID != "" {
		q.more("speaker_id = ?", f.SpeakerID)
	}

	err = s.inRead(ctx, func(ctx context.Context, r queryable) error {
		rows, err := r.query(ctx, opListMessages, q.sql("id"), q.args(limit+1)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		// limit+1 fetched; see ListGames for why the overflow row is the cursor.
		for len(out) <= limit && rows.Next() {
			m, err := scanMessage(rows)
			if err != nil {
				return err
			}
			out = append(out, m)
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

func loadProfileMessages(ctx context.Context, q queryable, profileID string) ([]games.Message, error) {
	const op = "list profile messages"
	rows, err := q.query(ctx, op, "SELECT "+messageColumns+" FROM messages WHERE profile_id = ? ORDER BY id", profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []games.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// scanMessage reads one message row. When score is non-nil the row is expected to
// carry a search score in its first column, which lets a scored FTS join and a
// plain read share one column mapping instead of maintaining two that can
// disagree about what a message contains.
func scanMessage(r scanner, score ...*float64) (games.Message, error) {
	const op = opGetMessage
	var (
		m           games.Message
		source      sql.NullString
		srcMsgID    sql.NullString
		markup      []byte
		scene       sql.NullString
		speaker     sql.NullString
		kind        string
		attribution string
		evidence    []byte
		flags       []byte
	)
	dest := []any{&m.ID, &m.ProfileID, &source, &srcMsgID, &m.OriginalText, &markup,
		&m.NormalizedText, &m.SpeechText, &m.Language, &kind, &scene, &speaker, &attribution,
		&m.AttributionConfidence, &evidence, &flags, &m.Revision, &m.CreatedAt, &m.UpdatedAt}
	if len(score) > 0 && score[0] != nil {
		dest = append([]any{score[0]}, dest...)
	}
	if err := r.Scan(dest...); err != nil {
		return m, mapReadError(op, err)
	}
	m.SourceID = source.String
	m.SourceMessageID = srcMsgID.String
	m.MarkupJSON = json.RawMessage(append([]byte(nil), markup...))
	m.SceneID = scene.String
	m.SpeakerID = speaker.String
	m.TextKind = games.TextKind(kind)
	m.AttributionStatus = games.AttributionStatus(attribution)
	m.AttributionEvidence = json.RawMessage(append([]byte(nil), evidence...))
	m.Flags = json.RawMessage(append([]byte(nil), flags...))
	return m, nil
}

// loadPages returns one message's display pages in ordinal order.
func loadPages(ctx context.Context, q queryable, messageID string) ([]games.MessagePage, error) {
	const op = opListPages
	rows, err := q.query(ctx, op,
		"SELECT "+pageColumns+" FROM message_pages WHERE message_id = ? ORDER BY ordinal", messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []games.MessagePage
	for rows.Next() {
		p, err := scanPage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func loadProfilePages(ctx context.Context, q queryable, profileID string) ([]games.MessagePage, error) {
	const op = "list profile pages"
	// message_pages has no profile_id (0002_content.sql explains why: the message
	// fixes the revision, and a copied column could drift out of agreement with it),
	// so the revision's pages come through a join rather than a WHERE on this table.
	rows, err := q.query(ctx, op,
		"SELECT "+addPrefix(pageColumns, "p.")+" FROM message_pages p JOIN messages m ON m.id = p.message_id"+
			" WHERE m.profile_id = ? ORDER BY p.message_id, p.ordinal", profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []games.MessagePage
	for rows.Next() {
		p, err := scanPage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func scanPage(r scanner) (games.MessagePage, error) {
	var (
		p        games.MessagePage
		srcPage  sql.NullString
		prefixes []byte
		controls []byte
		timing   []byte
	)
	if err := r.Scan(&p.MessageID, &p.Ordinal, &srcPage, &p.PageText,
		&prefixes, &controls, &timing); err != nil {
		return p, mapReadError(opListPages, err)
	}
	p.SourcePageID = srcPage.String
	p.VisiblePrefixes = json.RawMessage(append([]byte(nil), prefixes...))
	p.DisplayControls = json.RawMessage(append([]byte(nil), controls...))
	p.TimingJSON = json.RawMessage(append([]byte(nil), timing...))
	return p, nil
}

// ── edges ───────────────────────────────────────────────────────────────────

// PutEdges upserts dialogue transitions.
//
// Edges carry no profile_id of their own — their endpoints *are* their revision —
// so both endpoints are checked against the target profile first. Without that
// check a caller could graft one revision's graph onto another's, which
// 0002_content.sql warns would make traversal "walk into nothing": the FK proves
// the messages exist, only this proves they exist in the same revision.
func (s *Store) PutEdges(ctx context.Context, profileID string, rows []games.MessageEdge) error {
	return s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		const op = "put edges"
		return tx.contentWrite(ctx, op, profileID, func() error {
			for _, e := range rows {
				if err := tx.edgeEndpointsInProfile(ctx, op, profileID, e); err != nil {
					return err
				}
				if err := tx.putEdge(ctx, op, e); err != nil {
					return err
				}
			}
			return nil
		})
	})
}

func (tx *Tx) edgeEndpointsInProfile(ctx context.Context, op, profileID string, e games.MessageEdge) error {
	if e.FromID == e.ToID {
		// A repeat edge names one message; counting distinct ids would then be
		// wrong, and the query below is IN (id, id) with a distinct count of 1.
		var n int
		if err := tx.queryRow(ctx, op,
			"SELECT count(*) FROM messages WHERE profile_id = ? AND id = ?", profileID, e.FromID).Scan(&n); err != nil {
			return mapReadError(op, err)
		}
		if n != 1 {
			return notFoundError(op, "message "+e.FromID+" in profile "+profileID)
		}
		return nil
	}
	var n int
	if err := tx.queryRow(ctx, op,
		"SELECT count(DISTINCT id) FROM messages WHERE profile_id = ? AND id IN (?, ?)",
		profileID, e.FromID, e.ToID).Scan(&n); err != nil {
		return mapReadError(op, err)
	}
	if n != 2 {
		return notFoundError(op, fmt.Sprintf("both endpoints of edge %s -> %s in profile %s", e.FromID, e.ToID, profileID))
	}
	return nil
}

func (tx *Tx) putEdge(ctx context.Context, op string, e games.MessageEdge) error {
	if err := e.Validate(); err != nil {
		return err
	}
	if e.CreatedAt == "" {
		// Edges are usually written by a graph builder that has no clock of its
		// own; the schema requires an instant, so the store supplies "now" rather
		// than rejecting a well-formed transition for a missing timestamp.
		e.CreatedAt = nowStamp()
	}
	_, err := tx.exec(ctx, op,
		`INSERT INTO message_edges (from_id, to_id, edge_type, condition_expr, source_id, evidence, confidence, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (from_id, to_id, edge_type) DO UPDATE SET
			condition_expr = excluded.condition_expr,
			source_id = excluded.source_id,
			evidence = excluded.evidence,
			confidence = excluded.confidence`,
		e.FromID, e.ToID, string(e.EdgeType), e.ConditionExpr, nullStr(e.SourceID),
		orDefault(e.Evidence, "[]"), e.Confidence, e.CreatedAt)
	return err
}

func (tx *Tx) insertEdge(ctx context.Context, op string, e games.MessageEdge) error {
	return tx.putEdge(ctx, op, e)
}

// ListEdges returns a revision's graph. Edges are addressed through their
// endpoints, so the query joins messages rather than filtering a column of this
// table (which does not have one).
func (s *Store) ListEdges(ctx context.Context, profileID string) ([]games.MessageEdge, error) {
	var out []games.MessageEdge
	err := s.inRead(ctx, func(ctx context.Context, q queryable) error {
		var err error
		out, err = loadProfileEdges(ctx, q, profileID)
		return err
	})
	return out, err
}

func loadProfileEdges(ctx context.Context, q queryable, profileID string) ([]games.MessageEdge, error) {
	const op = "list profile edges"
	rows, err := q.query(ctx, op,
		"SELECT "+addPrefix(edgeColumns, "e.")+" FROM message_edges e"+
			" JOIN messages m ON m.id = e.from_id WHERE m.profile_id = ? ORDER BY e.from_id, e.to_id, e.edge_type", profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []games.MessageEdge
	for rows.Next() {
		e, err := scanEdge(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func scanEdge(r scanner) (games.MessageEdge, error) {
	var (
		e        games.MessageEdge
		etype    string
		source   sql.NullString
		evidence []byte
	)
	if err := r.Scan(&e.FromID, &e.ToID, &etype, &e.ConditionExpr, &source, &evidence,
		&e.Confidence, &e.CreatedAt); err != nil {
		return e, mapReadError("list edges", err)
	}
	e.EdgeType = games.EdgeType(etype)
	e.SourceID = source.String
	e.Evidence = json.RawMessage(append([]byte(nil), evidence...))
	return e, nil
}

// ── speaker assertions ──────────────────────────────────────────────────────

// PutSpeakerAssertions upserts candidate attributions that are *not* the resolved
// speaker (docs/spec/02 keeps these separate because a name on screen is not
// proof).
func (s *Store) PutSpeakerAssertions(ctx context.Context, profileID string, rows []games.SpeakerAssertion) error {
	return s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		const op = "put speaker assertions"
		return tx.contentWrite(ctx, op, profileID, func() error {
			for _, a := range rows {
				if err := sameProfile(op, "speaker assertion "+a.ID, profileID, a.ProfileID); err != nil {
					return err
				}
				if err := tx.putAssertion(ctx, op, a); err != nil {
					return err
				}
			}
			return nil
		})
	})
}

func (tx *Tx) putAssertion(ctx context.Context, op string, a games.SpeakerAssertion) error {
	if err := a.Validate(); err != nil {
		return err
	}
	_, err := tx.exec(ctx, op,
		`INSERT INTO speaker_assertions (id, profile_id, message_id, character_id, method, asserted_at, evidence, posterior, reviewer)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (id) DO UPDATE SET
			message_id = excluded.message_id,
			character_id = excluded.character_id,
			method = excluded.method,
			asserted_at = excluded.asserted_at,
			evidence = excluded.evidence,
			posterior = excluded.posterior,
			reviewer = excluded.reviewer`,
		a.ID, a.ProfileID, a.MessageID, a.CharacterID, string(a.Method), a.AssertedAt,
		orDefault(a.Evidence, "[]"), a.Posterior, a.Reviewer)
	return err
}

func (tx *Tx) insertAssertion(ctx context.Context, op string, a games.SpeakerAssertion) error {
	return tx.putAssertion(ctx, op, a)
}

// ListSpeakerAssertions returns a revision's candidate attributions, by id.
func (s *Store) ListSpeakerAssertions(ctx context.Context, profileID string) ([]games.SpeakerAssertion, error) {
	var out []games.SpeakerAssertion
	err := s.inRead(ctx, func(ctx context.Context, q queryable) error {
		var err error
		out, err = loadProfileAssertions(ctx, q, profileID)
		return err
	})
	return out, err
}

func loadProfileAssertions(ctx context.Context, q queryable, profileID string) ([]games.SpeakerAssertion, error) {
	const op = "list speaker assertions"
	rows, err := q.query(ctx, op,
		"SELECT "+assertionColumns+" FROM speaker_assertions WHERE profile_id = ? ORDER BY id", profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []games.SpeakerAssertion
	for rows.Next() {
		a, err := scanAssertion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func scanAssertion(r scanner) (games.SpeakerAssertion, error) {
	var (
		a        games.SpeakerAssertion
		method   string
		evidence []byte
	)
	if err := r.Scan(&a.ID, &a.ProfileID, &a.MessageID, &a.CharacterID, &method,
		&a.AssertedAt, &evidence, &a.Posterior, &a.Reviewer); err != nil {
		return a, mapReadError("list speaker assertions", err)
	}
	a.Method = games.AssertionMethod(method)
	a.Evidence = json.RawMessage(append([]byte(nil), evidence...))
	return a, nil
}

// ── speech text ─────────────────────────────────────────────────────────────

// UpdateMessageSpeechText edits the one message field a curator types by hand.
//
// It is the single-field write, and it is the reason the per-message `revision`
// counter exists: docs/spec/02 "Normalization and markup" makes speech_text an
// editable rendering (button icons expanded, redundant on-screen names not
// uttered), and docs/spec/08 requires an optimistic token for the editor so two
// open tabs cannot overwrite each other's wording. The profile's row version is
// the caller's token (it is what GET returns as the ETag), and the message's own
// counter moves with it, so a reader that cached the message can tell its copy is
// stale without re-reading the whole revision.
func (s *Store) UpdateMessageSpeechText(ctx context.Context, profileID, messageID, speechText string, expectedRowVersion int64) (games.Message, error) {
	var out games.Message
	err := s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		const op = "update message speech text"
		_, rev, err := tx.loadMutable(ctx, op, profileID)
		if err != nil {
			return err
		}
		if rev != expectedRowVersion {
			return revisionConflict(op, profileID, expectedRowVersion, rev)
		}

		m, err := scanMessage(tx.queryRow(ctx, op,
			"SELECT "+messageColumns+" FROM messages WHERE profile_id = ? AND id = ?", profileID, messageID))
		if err != nil {
			return err
		}
		m.SpeechText = speechText
		if err := m.Validate(); err != nil {
			return err
		}
		m.UpdatedAt = nowStamp()
		if err := tx.putMessage(ctx, op, m); err != nil {
			return err
		}
		// Re-read: putMessage incremented the per-message counter server-side, and
		// returning the caller's stale value would hand the UI a token that is
		// already rejected.
		out, err = scanMessage(tx.queryRow(ctx, op,
			"SELECT "+messageColumns+" FROM messages WHERE profile_id = ? AND id = ?", profileID, messageID))
		if err != nil {
			return err
		}
		_, err = tx.bumpRowVersion(ctx, op, profileID, rev)
		return err
	})
	return out, err
}

// ── small SQL helpers ───────────────────────────────────────────────────────

// builder assembles a filtered keyset-paginated SELECT. It exists so the queries
// that take an optional filter list (ListMessages today, the review queue later)
// share one code path: every value goes in as a bound parameter, never
// interpolated, which is what keeps a scene id containing a quote from becoming
// syntax.
type builder struct {
	table   string
	columns string
	where   []string
	vals    []any
	limit   string
}

// more adds a WHERE condition and its single placeholder argument together. It is
// the only way to build a condition: appending a value on its own would desync the
// bind list from the placeholders, which SQLite reports as a datatype mismatch on
// whichever placeholder happens to land on the shifted argument.
func (b *builder) more(cond string, value any) *builder {
	b.where = append(b.where, cond)
	b.vals = append(b.vals, value)
	return b
}

// sql renders the statement, ordered by `orderCol` so a cursor can be a keyset.
func (b *builder) sql(orderCol string) string {
	where := "WHERE " + strings.Join(b.where, " AND ")
	return fmt.Sprintf("SELECT %s FROM %s %s ORDER BY %s LIMIT ?", b.columns, b.table, where, orderCol)
}

// args returns the bound values, with the limit placeholder last to match sql().
func (b *builder) args(extra ...any) []any {
	return append(b.vals, extra...)
}

// addPrefix qualifies every comma-separated column of a list, so a join can reuse
// the same column constant as the single-table read (one list to keep in sync with
// the schema instead of two that can disagree).
func addPrefix(cols, prefix string) string {
	parts := strings.Split(cols, ", ")
	for i, p := range parts {
		parts[i] = prefix + p
	}
	return strings.Join(parts, ", ")
}
