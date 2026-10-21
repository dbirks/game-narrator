package store

// The profile library lifecycle of ADR-0107 (gap G14): the disable switch,
// the soft delete with its retention window, the admin purge, and the clone
// with its provenance link. Duplicate needs no store of its own — it is
// NewRevision under a lifecycle name (the API auditions it separately).
//
// The two rules that shape every method here:
//
//  1. The audit trail outlives its subject. 0004's RESTRICT on
//     publish_actions.profile_id exists so a revision with publication
//     history cannot be deleted out from under its own trail, and the soft
//     delete enforces the same rule with an explicit sentinel instead of
//     leaning on the foreign key's opaque error. A shipped revision retires
//     via archive, not delete.
//  2. A delete is a mark, then a removal. SoftDeleteProfile writes
//     deleted_at + purge_after in one compare-and-swap statement; PurgeProfile
//     refuses until the retention window has elapsed and only then removes
//     the row (the content cascades, the FTS triggers follow the cascade),
//     having first addressed the speech-cache rows keyed to the profile's
//     messages. Between the two, the revision is hidden from every surface a
//     user acts on while its content stays on disk (ADR-0107).

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/speech"
)

// profileDeletionRetention is the window between a soft delete and the moment
// a purge is allowed (ADR-0107). Long enough for the mistake to be noticed on
// the library screen or by support, short enough that content nobody looks at
// twice does not sit on the appliance's disk for good.
const profileDeletionRetention = 72 * time.Hour

// ── disable ────────────────────────────────────────────────────────────────

// SetGameDisabled flips a title family's disable switch (ADR-0107, gap G14).
// A disabled game is hidden from Play and narration — session start refuses
// any of its profiles (docs/spec/01: refuse to start rather than improvise) —
// while its profiles stay editable: the write touches the game row and nothing
// else. A missing game is ErrNotFound, like every other write here.
func (s *Store) SetGameDisabled(ctx context.Context, gameID string, disabled bool) error {
	const op = "set game disabled"
	return s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		res, err := tx.exec(ctx, op,
			"UPDATE games SET disabled = ? WHERE id = ?", boolInt(disabled), gameID)
		if err != nil {
			return mapWriteError(op, err)
		}
		// Zero rows means the id named no game; the reads answer that way, so
		// the write does too.
		return errFromAffected(res, op, 1)
	})
}

// ── soft delete and purge ──────────────────────────────────────────────────

// SoftDeleteProfile marks a revision deleted (ADR-0107): deleted_at and
// purge_after (= deleted_at + profileDeletionRetention) land in one
// compare-and-swap statement — the compare being deleted_at IS NULL — so two
// racing deletes produce one mark. The mark hides the revision from the
// library list, from direct reads and from session start (ADR-0107 documents
// each reader); the row and its content stay on disk until PurgeProfile.
//
// The refusals are 409-class sentinels, each naming its own reason:
//
//   - ErrProfileAlreadyDeleted: the mark is there; the purge is the other
//     half, and the row still answers the store's own reads while it waits.
//   - ErrProfileHasPublicationHistory: a publish_actions row exists. The
//     soft delete obeys 0004's RESTRICT rule instead of leaning on the
//     foreign key's opaque error (see the file comment).
//   - ErrProfileHasDerivedRevisions: another revision descends from this one
//     (the 0001 parent_revision RESTRICT, named).
//
// The active-session refusal lives at the API layer, where the runtime
// manager is: the store does not know session state, and in production this
// method's only caller is the API.
func (s *Store) SoftDeleteProfile(ctx context.Context, profileID string) (games.Profile, error) {
	var out games.Profile
	err := s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		const op = "soft delete profile"
		p, _, err := loadProfile(ctx, tx, op, profileID)
		if err != nil {
			return err
		}
		if p.DeletedAt != "" {
			return fmt.Errorf("store: %s: profile %s was already deleted (purge_after %s); the mark is present and the purge is the other half: %w",
				op, profileID, p.PurgeAfter, ErrProfileAlreadyDeleted)
		}
		if n, err := countPublishActions(ctx, tx, op, profileID); err != nil {
			return err
		} else if n > 0 {
			return fmt.Errorf("store: %s: profile %s has %d publication-history row(s) in publish_actions; the audit trail outlives the revision (0004), so a shipped revision retires via archive, not delete: %w",
				op, profileID, n, ErrProfileHasPublicationHistory)
		}
		if n, err := countDerivedRevisions(ctx, tx, op, p); err != nil {
			return err
		} else if n > 0 {
			return fmt.Errorf("store: %s: profile %s is the parent of %d revision(s); delete the derived revisions first: %w",
				op, profileID, n, ErrProfileHasDerivedRevisions)
		}

		now := time.Now().UTC()
		res, err := tx.exec(ctx, op,
			"UPDATE profiles SET deleted_at = ?, purge_after = ?, row_version = row_version + 1 WHERE id = ? AND deleted_at IS NULL",
			now.Format(time.RFC3339), now.Add(profileDeletionRetention).Format(time.RFC3339), profileID)
		if err != nil {
			return mapWriteError(op, err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			// The compare lost: the row was marked between the read and the
			// write. Report the mark's fact, not a mystery conflict.
			return fmt.Errorf("store: %s: profile %s was deleted by a concurrent write: %w",
				op, profileID, ErrProfileAlreadyDeleted)
		}
		marked, _, err := loadProfile(ctx, tx, op, profileID)
		if err != nil {
			return err
		}
		out = marked
		return nil
	})
	return out, err
}

// PurgeReport is what one purge removed. The asset store is the API layer's
// business (the store has no blob root to delete from), so the report hands
// back the hashes whose index rows the transaction dropped; the API decides
// which of them are now unreferenced and deletes the bytes.
type PurgeReport struct {
	// CacheEntries is the number of speech_cache rows keyed to the profile's
	// messages that were removed.
	CacheEntries int
	// CacheBlobs is the sha256 of each removed cache row's committed WAV.
	CacheBlobs []string
	// ReferencedBlobs is the content hashes the profile's sources and
	// character portraits named (the rows the cascade removed): candidates
	// for blob deletion when nothing else references them.
	ReferencedBlobs []string
}

// PurgeProfile removes a soft-deleted revision for good (ADR-0107). One
// transaction:
//
//  1. re-check the mark and the retention window — a purge before purge_after
//     is ErrRetentionNotElapsed, and on a row that was never marked it is
//     ErrProfileNotDeleted (purging is the second half of a two-step act);
//  2. re-check the two soft-delete guards — the window is long enough for a
//     successor or a publication attempt to appear, and a row hand-written
//     into the schema's blind spot must not get through the back door either;
//  3. delete the profile's speech-cache rows (0017's original_message_id is
//     the address; the key space itself is untouched and stays versioned);
//  4. delete the profile row: the 0002 FK cascades remove the content rows
//     and the FTS5 sync triggers drop the index entries with them
//     (TestDeletingProfileCascadesContentAndFTS is the proof).
//
// The blob bytes the dropped index rows and content rows named are not
// removed here: ADR-0041's reference-before-delete discipline runs at the
// API layer, where the asset store lives, against the post-commit state.
func (s *Store) PurgeProfile(ctx context.Context, profileID string) (PurgeReport, error) {
	var report PurgeReport
	err := s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		const op = "purge profile"
		p, _, err := loadProfile(ctx, tx, op, profileID)
		if err != nil {
			return err
		}
		if p.DeletedAt == "" {
			return fmt.Errorf("store: %s: profile %s is not soft-deleted; purging is the second half of a two-step act and the mark is the first: %w",
				op, profileID, ErrProfileNotDeleted)
		}
		deadline, err := time.Parse(time.RFC3339, p.PurgeAfter)
		if err != nil {
			// Unreachable through the repositories (the CHECKs and Validate
			// refuse the value on the way in); a hand-written row with an
			// unparseable deadline must wait for a human, not a parser.
			return fmt.Errorf("store: %s: profile %s carries a purge_after this build cannot parse (%q): %w",
				op, profileID, p.PurgeAfter, ErrRetentionNotElapsed)
		}
		if time.Now().UTC().Before(deadline) {
			return fmt.Errorf("store: %s: profile %s is inside its retention window (purge_after %s, %s from now): the window exists so the delete can be noticed: %w",
				op, profileID, p.PurgeAfter, deadline.Sub(time.Now().UTC()).Round(time.Second), ErrRetentionNotElapsed)
		}
		if n, err := countPublishActions(ctx, tx, op, profileID); err != nil {
			return err
		} else if n > 0 {
			return fmt.Errorf("store: %s: profile %s gained %d publication-history row(s) since the soft delete; the audit trail outlives the revision (0004): %w",
				op, profileID, n, ErrProfileHasPublicationHistory)
		}
		if n, err := countDerivedRevisions(ctx, tx, op, p); err != nil {
			return err
		} else if n > 0 {
			return fmt.Errorf("store: %s: profile %s is the parent of %d revision(s); delete the derived revisions first: %w",
				op, profileID, n, ErrProfileHasDerivedRevisions)
		}

		// The message ids are the address of the profile's cache entries: the
		// key itself is a hash the purge cannot invert, 0017's column is what
		// makes "the cache rows of this profile's messages" a query.
		var messageIDs []string
		rows, err := tx.query(ctx, op, "SELECT id FROM messages WHERE profile_id = ?", profileID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return mapReadError(op, err)
			}
			messageIDs = append(messageIDs, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return mapReadError(op, err)
		}
		rows.Close()

		// The hashes of the committed WAVs, before the rows go: the report
		// hands them to the blob cleanup, which needs to know what to check.
		if len(messageIDs) > 0 {
			placeholders := strings.TrimSuffix(strings.Repeat("?,", len(messageIDs)), ",")
			blobRows, err := tx.query(ctx, op,
				"SELECT sha256 FROM speech_cache WHERE original_message_id IN ("+placeholders+")",
				toArgs(messageIDs)...)
			if err != nil {
				return err
			}
			var blobs []string
			for blobRows.Next() {
				var sha string
				if err := blobRows.Scan(&sha); err != nil {
					blobRows.Close()
					return mapReadError(op, err)
				}
				blobs = append(blobs, sha)
			}
			if err := blobRows.Err(); err != nil {
				blobRows.Close()
				return mapReadError(op, err)
			}
			blobRows.Close()
			report.CacheEntries = len(blobs)
			report.CacheBlobs = blobs
			if _, err := tx.exec(ctx, op,
				"DELETE FROM speech_cache WHERE original_message_id IN ("+placeholders+")",
				toArgs(messageIDs)...); err != nil {
				return mapWriteError(op, err)
			}

			// The merged fallback (issue #25, ADR-0122): the merged runs the
			// catch-up narration committed under versioned id-lists
			// ("m<version>:id1,id2,…”). The exact-match delete cannot address
			// them — the provenance column holds the list — and the profile's
			// words must not outlive the profile inside a merged blob: the rows
			// that name any of the profile's messages as a whole segment go
			// down with it, their blobs joining the same report.
			if merged, err := s.mergedCacheRowsForMessages(ctx, tx, op, messageIDs); err != nil {
				return err
			} else if len(merged) > 0 {
				mph := strings.TrimSuffix(strings.Repeat("?,", len(merged)), ",")
				margs := make([]any, 0, len(merged))
				for _, r := range merged {
					report.CacheEntries++
					report.CacheBlobs = append(report.CacheBlobs, r.sha256)
					margs = append(margs, r.key)
				}
				if _, err := tx.exec(ctx, op,
					"DELETE FROM speech_cache WHERE key IN ("+mph+")", margs...); err != nil {
					return mapWriteError(op, err)
				}
			}
		}

		// The content hashes the cascade is about to orphan (sources and
		// portraits: the columns that own a blob, as opposed to the JSON
		// evidence pointers 0002 deliberately leaves unindexed).
		sourceRows, err := tx.query(ctx, op,
			"SELECT content_sha256 FROM sources WHERE profile_id = ? AND content_sha256 <> ''", profileID)
		if err != nil {
			return err
		}
		for sourceRows.Next() {
			var sha string
			if err := sourceRows.Scan(&sha); err != nil {
				sourceRows.Close()
				return mapReadError(op, err)
			}
			report.ReferencedBlobs = append(report.ReferencedBlobs, sha)
		}
		if err := sourceRows.Err(); err != nil {
			sourceRows.Close()
			return mapReadError(op, err)
		}
		sourceRows.Close()
		portraitRows, err := tx.query(ctx, op,
			"SELECT portrait_asset_id FROM characters WHERE profile_id = ? AND portrait_asset_id IS NOT NULL", profileID)
		if err != nil {
			return err
		}
		for portraitRows.Next() {
			var sha string
			if err := portraitRows.Scan(&sha); err != nil {
				portraitRows.Close()
				return mapReadError(op, err)
			}
			report.ReferencedBlobs = append(report.ReferencedBlobs, sha)
		}
		if err := portraitRows.Err(); err != nil {
			portraitRows.Close()
			return mapReadError(op, err)
		}
		portraitRows.Close()

		if _, err := tx.exec(ctx, op, "DELETE FROM profiles WHERE id = ?", profileID); err != nil {
			// The parent_revision and published_profile_id RESTRICTs are the
			// backstops behind the explicit guards above; a hand-written
			// database is the only way one of them fires, and the FK error is
			// the honest report for it.
			return mapWriteError(op, err)
		}
		return nil
	})
	return report, err
}

// countPublishActions counts a revision's publication-history rows. The RESTRICT
// on publish_actions.profile_id is the schema's statement that these rows
// outlive their subject; the count is the repositories' statement of the same
// rule in words, before the write that would otherwise meet the FK mid-
// transaction.
func countPublishActions(ctx context.Context, tx *Tx, op, profileID string) (int, error) {
	var n int
	if err := tx.queryRow(ctx, op,
		"SELECT count(*) FROM publish_actions WHERE profile_id = ?", profileID).Scan(&n); err != nil {
		return 0, mapReadError(op, err)
	}
	return n, nil
}

// countDerivedRevisions counts the revisions that descend from p through the
// 0001 parent_revision chain, inside p's own edition (the composite key keeps
// the chain from crossing editions, so edition + parent number is the exact
// child set).
func countDerivedRevisions(ctx context.Context, tx *Tx, op string, p games.Profile) (int, error) {
	var n int
	if err := tx.queryRow(ctx, op,
		"SELECT count(*) FROM profiles WHERE edition_id = ? AND parent_revision = ?",
		p.EditionID, p.RevisionNum).Scan(&n); err != nil {
		return 0, mapReadError(op, err)
	}
	return n, nil
}

// ── speech-cache provenance ────────────────────────────────────────────────

// mergedCacheRowsForMessages selects the speech_cache rows whose
// original_message_id is a merged utterance's versioned id-list form
// (issue #25, ADR-0122: "m<version>:id1,id2,…" — a merged run speaks more
// than one committed line under one cache key) and that names one of
// messageIDs as one of the list's whole segments. The exact-match IN(…) deletes
// elsewhere cannot see these rows — the provenance column holds the *list*,
// not any one of the ids — so the ADR-0107 provenance purge falls back to this
// when the messages must not outlive their rows in a merged blob. The match is
// speech.MergedIDsIn: exact-segment, so an id that is a prefix of another
// segment cannot take a row down with it, and the store never re-spells the
// merged form it does not own. The candidate set is the merged rows of the
// cache (a small fraction), not the whole table; the result is the (key,
// sha256) pairs the caller deletes and hashes for its reference-before-delete
// blob cleanup.
func (s *Store) mergedCacheRowsForMessages(ctx context.Context, tx *Tx, op string, messageIDs []string) ([]struct{ key, sha256 string }, error) {
	rows, err := tx.query(ctx, op,
		"SELECT key, sha256, original_message_id FROM speech_cache WHERE original_message_id LIKE 'm%'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []struct{ key, sha256 string }
	for rows.Next() {
		var k, sha, omid string
		if err := rows.Scan(&k, &sha, &omid); err != nil {
			return nil, mapReadError(op, err)
		}
		if speech.IsMergedOriginalID(omid) && speech.MergedIDsIn(omid, messageIDs) {
			out = append(out, struct{ key, sha256 string }{k, sha})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, mapReadError(op, err)
	}
	return out, nil
}

// DeleteSpeechCacheRowsForMessages removes the speech_cache rows whose
// original_message_id names one of messageIDs (0017's provenance column,
// ADR-0107) and returns their committed-blob hashes, for the caller's
// reference-before-delete blob cleanup. A merged utterance's row (issue #25,
// ADR-0122: original_message_id the "m<version>:id1,id2,…” list) names the
// same messages through the list, so it is addressed by the segment fallback
// and dropped with the rows it is made of. An empty messageID set is a no-op
// with an empty report: the purge of a message-free revision still runs, it
// just addresses nothing.
func (s *Store) DeleteSpeechCacheRowsForMessages(ctx context.Context, messageIDs []string) (int, []string, error) {
	if len(messageIDs) == 0 {
		return 0, nil, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(messageIDs)), ",")
	args := toArgs(messageIDs)

	var blobs []string
	err := s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		const op = "delete speech cache rows"
		rows, err := tx.query(ctx, op,
			"SELECT sha256 FROM speech_cache WHERE original_message_id IN ("+placeholders+")", args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var sha string
			if err := rows.Scan(&sha); err != nil {
				rows.Close()
				return mapReadError(op, err)
			}
			blobs = append(blobs, sha)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return mapReadError(op, err)
		}
		rows.Close()

		if _, err := tx.exec(ctx, op,
			"DELETE FROM speech_cache WHERE original_message_id IN ("+placeholders+")", args...); err != nil {
			return mapWriteError(op, err)
		}

		// The merged fallback (issue #25, ADR-0122): the rows a merged run
		// committed under its versioned id-list, which the exact-match delete
		// above cannot address. They speak the messages' words, so they go
		// down with them — and their blobs join the same reference-before-delete
		// report.
		merged, err := s.mergedCacheRowsForMessages(ctx, tx, op, messageIDs)
		if err != nil {
			return err
		}
		if len(merged) > 0 {
			mph := strings.TrimSuffix(strings.Repeat("?,", len(merged)), ",")
			margs := make([]any, 0, len(merged))
			for _, r := range merged {
				blobs = append(blobs, r.sha256)
				margs = append(margs, r.key)
			}
			if _, err := tx.exec(ctx, op,
				"DELETE FROM speech_cache WHERE key IN ("+mph+")", margs...); err != nil {
				return mapWriteError(op, err)
			}
		}
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return len(blobs), blobs, nil
}

// SpeechCacheRowCountByHash counts the speech_cache rows that committed one
// blob. It is the cache-side half of ADR-0041's reference-before-delete
// discipline: AssetReferenced covers the catalogue tables by design (the cache
// is the eviction path it guards), and a purge that drops the cache rows must
// also check the cache before the blob goes.
func (s *Store) SpeechCacheRowCountByHash(ctx context.Context, sha256 string) (int, error) {
	var n int
	err := s.inRead(ctx, func(ctx context.Context, q queryable) error {
		const op = "count speech cache rows by hash"
		row := q.queryRow(ctx, op, "SELECT count(*) FROM speech_cache WHERE sha256 = ?", sha256)
		return row.Scan(&n)
	})
	if err != nil {
		return 0, mapReadError("count speech cache rows by hash", err)
	}
	return n, nil
}

// ── clone ───────────────────────────────────────────────────────────────────

// CloneResult is what CloneProfile created: the new game, its edition, the
// draft revision holding the source's content, and the provenance link that
// ties the two revisions together (ADR-0107).
type CloneResult struct {
	Game    games.Game
	Edition games.Edition
	Profile games.Profile
	Link    games.CloneLink
}

// CloneProfile copies a revision into a new game with a matching edition and a
// fresh draft revision (ADR-0107, gap G14). One transaction:
//
//  1. the source revision is loaded and must not be soft-deleted — a hidden
//     revision is not a clone source;
//  2. the new game gets the caller's title/slug or the source's: slug ""
//     derives <source-slug>-copy, and a slug already taken gets -2, -3, …
//     appended (bounded by the slug's 80-character limit; past it the clone
//     fails rather than truncating an identity);
//  3. the edition row is the source edition's binding copied under the new
//     game (editions_identity_idx keeps the copy reconcilable on re-import);
//  4. the draft is revision 1 of the new edition with the source's content
//     copied through the same helper NewRevision uses — no row of the clone
//     points at a row of the source (ADR-0107: the snapshot is an independent
//     set of rows); the preferred voice and the narrator fallback follow the
//     content, the source's status/publication fields do not (a clone is a
//     draft by construction);
//  5. the clone_links row records where the clone came from, attributed to the
//     caller, before the commit — a clone without its provenance link is the
//     half-state the transaction exists to prevent.
//
// The source revision is not modified in any way.
func (s *Store) CloneProfile(ctx context.Context, sourceProfileID, title, slug, author string) (CloneResult, error) {
	var out CloneResult
	err := s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		const op = "clone profile"
		source, _, err := loadProfile(ctx, tx, op, sourceProfileID)
		if err != nil {
			return err
		}
		if source.DeletedAt != "" {
			return fmt.Errorf("store: %s: profile %s is deleted (purge_after %s) and is not a clone source: %w",
				op, sourceProfileID, source.PurgeAfter, ErrNotFound)
		}
		srcContent, err := loadProfileContent(ctx, tx, sourceProfileID)
		if err != nil {
			return err
		}
		edition, err := scanEditionRow(tx.queryRow(ctx, op,
			"SELECT "+editionColumns+" FROM editions WHERE id = ?", source.EditionID), op)
		if err != nil {
			return err
		}
		sourceGame, err := scanGameRow(tx.queryRow(ctx, op,
			"SELECT "+gameColumns+" FROM games WHERE id = ?", edition.GameID), op)
		if err != nil {
			return err
		}

		gameTitle := strings.TrimSpace(title)
		if gameTitle == "" {
			gameTitle = sourceGame.Title
		}
		slugBase := strings.TrimSpace(slug)
		if slugBase == "" {
			slugBase = sourceGame.Slug + "-copy"
		}
		slug, err := uniqueCloneSlug(ctx, tx, op, slugBase)
		if err != nil {
			return err
		}

		game := games.Game{
			ID:          games.NewID(),
			Slug:        slug,
			Title:       gameTitle,
			Description: sourceGame.Description,
			Platform:    sourceGame.Platform,
			UserTags:    sourceGame.UserTags,
			CreatedAt:   nowStamp(),
		}
		if err := game.Validate(); err != nil {
			return err
		}
		if err := tx.createGame(ctx, game); err != nil {
			return err
		}

		ed := games.Edition{
			ID:                 games.NewID(),
			GameID:             game.ID,
			Region:             edition.Region,
			Language:           edition.Language,
			Revision:           edition.Revision,
			SourceIdentity:     edition.SourceIdentity,
			DisplayAspect:      edition.DisplayAspect,
			Locale:             edition.Locale,
			CompatibilityNotes: edition.CompatibilityNotes,
		}
		if err := ed.Validate(); err != nil {
			return err
		}
		if _, err := tx.exec(ctx, op,
			`INSERT INTO editions (id, game_id, region, language, revision, source_identity, display_aspect, locale, compatibility_notes)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			ed.ID, ed.GameID, ed.Region, ed.Language, ed.Revision, nullStr(ed.SourceIdentity),
			nullFloat(ed.DisplayAspect), ed.Locale, ed.CompatibilityNotes); err != nil {
			return err
		}

		profile := games.Profile{
			ID:          games.NewID(),
			EditionID:   ed.ID,
			RevisionNum: 1,
			Status:      games.StatusDraft,
			// The curator's voice choice and the narrator fallback follow the
			// content they were made for (the same rule NewRevision applies);
			// the parent link is left NULL: the draft descends from the clone,
			// not from a revision number of its own edition.
			PreferredVoiceID: source.PreferredVoiceID,
			NarratorFallback: source.NarratorFallback,
		}
		if err := profile.Validate(); err != nil {
			return err
		}
		if err := tx.insertProfile(ctx, profile); err != nil {
			return err
		}
		if err := tx.copyProfileContent(ctx, op, srcContent, profile.ID); err != nil {
			return err
		}

		who := strings.TrimSpace(author)
		if who == "" {
			who = unknownActor
		}
		if len([]rune(who)) > 200 {
			who = string([]rune(who)[:200])
		}
		link := games.CloneLink{
			ID:              games.NewID(),
			SourceProfileID: source.ID,
			TargetProfileID: profile.ID,
			SourceEditionID: source.EditionID,
			TargetEditionID: ed.ID,
			SourceGameID:    edition.GameID,
			TargetGameID:    game.ID,
			CreatedBy:       who,
			CreatedAt:       nowStamp(),
		}
		if err := link.Validate(); err != nil {
			return err
		}
		if _, err := tx.exec(ctx, op,
			`INSERT INTO clone_links (id, source_profile_id, target_profile_id, source_edition_id, target_edition_id, source_game_id, target_game_id, created_by, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			link.ID, link.SourceProfileID, link.TargetProfileID, link.SourceEditionID, link.TargetEditionID,
			link.SourceGameID, link.TargetGameID, link.CreatedBy, link.CreatedAt); err != nil {
			return mapWriteError(op, err)
		}

		out = CloneResult{Game: game, Edition: ed, Profile: profile, Link: link}
		return nil
	})
	return out, err
}

// uniqueCloneSlug answers "which spelling of base is free": base itself, then
// base-2, base-3, … A slug is a public identity (it sits in the game's URL),
// so a collision is resolved by suffix, never by truncation — a truncated
// slug would be a new identity pretending to be the one the caller asked for.
// The 80-character limit is the stop condition: past it no spelling of the
// caller's choice fits, and the clone fails rather than inventing an identity.
func uniqueCloneSlug(ctx context.Context, tx *Tx, op, base string) (string, error) {
	for i := 1; ; i++ {
		candidate := base
		if i > 1 {
			candidate = fmt.Sprintf("%s-%d", base, i)
		}
		// The probe runs the real slug rules (pattern and 80-char limit) on
		// the candidate before the collision query: a candidate past the limit
		// is refused, not looped into a -N that only makes it longer. The probe
		// is a minimal well-formed game — Validate is the full game validator,
		// and a probe that fails it on its own omissions would refuse every
		// clone, not just the ones whose slug is too long.
		probe := games.Game{ID: games.NewID(), Slug: candidate, Title: "t", CreatedAt: nowStamp()}
		if err := probe.Validate(); err != nil {
			return "", fmt.Errorf("store: %s: no valid spelling of clone slug %q fits (the suffixed spelling passes the slug limit): %w", op, base, err)
		}
		var n int
		if err := tx.queryRow(ctx, op,
			"SELECT count(*) FROM games WHERE slug = ?", candidate).Scan(&n); err != nil {
			return "", mapReadError(op, err)
		}
		if n == 0 {
			return candidate, nil
		}
		if i >= 50 {
			return "", fmt.Errorf("store: %s: no free spelling of slug %q within 50 suffixes: %w", op, base, ErrConflict)
		}
	}
}

// ── provenance reads ────────────────────────────────────────────────────────

// CloneLinksForGame answers "where did this game come from": the clone_links
// rows whose target is the game, newest first.
func (s *Store) CloneLinksForGame(ctx context.Context, gameID string) ([]games.CloneLink, error) {
	var out []games.CloneLink
	err := s.inRead(ctx, func(ctx context.Context, q queryable) error {
		const op = "list clone links for game"
		rows, err := q.query(ctx, op,
			"SELECT id, source_profile_id, target_profile_id, source_edition_id, target_edition_id, source_game_id, target_game_id, created_by, created_at FROM clone_links WHERE target_game_id = ? ORDER BY created_at DESC, id",
			gameID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c games.CloneLink
			if err := rows.Scan(&c.ID, &c.SourceProfileID, &c.TargetProfileID,
				&c.SourceEditionID, &c.TargetEditionID, &c.SourceGameID, &c.TargetGameID,
				&c.CreatedBy, &c.CreatedAt); err != nil {
				return mapReadError(op, err)
			}
			out = append(out, c)
		}
		return mapReadError(op, rows.Err())
	})
	return out, err
}

// CloneLinksForProfile answers "what was ever cloned out of this revision":
// the clone_links rows it is the source of, newest first.
func (s *Store) CloneLinksForProfile(ctx context.Context, profileID string) ([]games.CloneLink, error) {
	var out []games.CloneLink
	err := s.inRead(ctx, func(ctx context.Context, q queryable) error {
		const op = "list clone links for profile"
		rows, err := q.query(ctx, op,
			"SELECT id, source_profile_id, target_profile_id, source_edition_id, target_edition_id, source_game_id, target_game_id, created_by, created_at FROM clone_links WHERE source_profile_id = ? ORDER BY created_at DESC, id",
			profileID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c games.CloneLink
			if err := rows.Scan(&c.ID, &c.SourceProfileID, &c.TargetProfileID,
				&c.SourceEditionID, &c.TargetEditionID, &c.SourceGameID, &c.TargetGameID,
				&c.CreatedBy, &c.CreatedAt); err != nil {
				return mapReadError(op, err)
			}
			out = append(out, c)
		}
		return mapReadError(op, rows.Err())
	})
	return out, err
}

// toArgs converts a string slice to the slice of any the database/sql
// placeholders expect.
func toArgs(values []string) []any {
	args := make([]any, len(values))
	for i, v := range values {
		args[i] = v
	}
	return args
}
