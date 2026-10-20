package store

// Profile-revision repository: drafts, snapshots, and the immutability rule
// (docs/spec/02 "Transaction workflows", docs/spec/01 "Runtime and setup
// isolation", docs/spec/12 "no published edit without valid revision token").
//
// A profile row is one *revision* of one edition, and the unit the whole
// publication story is about: docs/spec/01 says only published revisions are used
// at runtime, and that setup drafts may be edited without affecting an active
// session. That is implemented here as two rules the rest of the package depends
// on:
//
//  1. Every mutation of a revision goes through loadMutable or an explicit
//     version check, so published and archived rows are frozen at the repository
//     boundary rather than in the UI (games.Profile.IsImmutable is the single
//     source of which statuses those are).
//  2. Every mutation bumps `profiles.row_version` exactly once, in the same
//     statement or transaction that made the change, compare-and-swapping the
//     caller's expected value first. That column is the token docs/spec/08 sends
//     as an ETag and compares as If-Match; 0004_publication.sql and ADR-0015 say
//     why it must be a column rather than an event-log row. Without (2), (1) would
//     still let two concurrent editors of the same *draft* overwrite each other
//     silently.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/dbirks/game-narrator/internal/games"
)

// profileColumns are the 0001 columns in domain order. The row version (added by
// 0004) is deliberately not one of them and not a field of games.Profile either:
// it is storage bookkeeping that goes to callers as an ETag, and inventing a place
// for it in the domain type (ManifestDigest is the only integer-shaped column
// nearby) would corrupt the digest every audio cache key derives from. Lists use
// these columns; anything that will be compared against a caller's token uses
// profileRowColumns, which adds the version so the row and its guard are read in
// one statement and cannot come from different instants.
const profileColumns = "id, edition_id, revision_num, parent_revision, status, manifest_digest, published_at, preferred_voice_id, publication_author, narrator_fallback, deleted_at, purge_after"

// profileRowColumns is profileColumns plus the optimistic-locking counter.
const profileRowColumns = profileColumns + ", row_version"

// profileSelect reads one revision by id together with the token that guards it;
// loadProfile is the only caller, so no repository method can act on a status read
// separately from the version read.
const profileSelect = "SELECT " + profileRowColumns + " FROM profiles WHERE id = ?"

// CreateDraftProfile starts revision 1 of an edition's lineage — or the next free
// number if the edition already has revisions, because
// profiles_edition_revision_idx is UNIQUE per edition and a second independent
// draft chain must not collide with the first.
//
// The parent link is left NULL: a fresh draft descends from nothing, and only
// NewRevision records a lineage. That distinction is what makes "which revision
// was this derived from" answerable for a snapshot but honestly empty for the
// first import of an edition.
func (s *Store) CreateDraftProfile(ctx context.Context, editionID string) (games.Profile, error) {
	var out games.Profile
	err := s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		const op = "create draft profile"
		var one int
		if err := tx.queryRow(ctx, op, "SELECT 1 FROM editions WHERE id = ?", editionID).Scan(&one); err != nil {
			return mapReadError(op, err)
		}
		num, err := tx.nextRevisionNum(ctx, editionID)
		if err != nil {
			return err
		}
		p := games.Profile{
			ID:          games.NewID(),
			EditionID:   editionID,
			RevisionNum: num,
			Status:      games.StatusDraft,
			// The flag's default is on: a fresh revision narrates unknown
			// speakers with the narrator voice, the runtime's historical
			// behaviour (ADR-0101), until somebody says otherwise.
			NarratorFallback: true,
		}
		if err := p.Validate(); err != nil {
			return err
		}
		if err := tx.insertProfile(ctx, p); err != nil {
			return err
		}
		out = p
		return nil
	})
	return out, err
}

// GetProfile returns a revision plus its row version (the ETag source).
//
// Both come from one SELECT, so the pair the caller receives was true at one
// instant. A caller acting on it is still racing anybody else's write, which is why
// every mutation re-reads the version inside its own BEGIN IMMEDIATE transaction
// and compare-and-swaps it rather than trusting a token obtained over an earlier
// GetProfile.
func (s *Store) GetProfile(ctx context.Context, id string) (games.Profile, int64, error) {
	var (
		out games.Profile
		rev int64
	)
	err := s.inRead(ctx, func(ctx context.Context, q queryable) error {
		var err error
		out, rev, err = loadProfile(ctx, q, opGetProfile, id)
		return err
	})
	return out, rev, err
}

// loadProfile reads a revision together with its row version. Every repository
// method that compares a caller's token starts here rather than at profileSelect,
// so "I checked the status" can never mean "I checked the token too".
func loadProfile(ctx context.Context, q queryable, op, id string) (games.Profile, int64, error) {
	return scanProfileRow(q.queryRow(ctx, op, profileSelect, id), op)
}

// ── the row version: ETag storage ───────────────────────────────────────────

// rowVersion reads a revision's current counter, for a caller that needs the
// version and nothing else — the failure path of a compare-and-swap, which has to
// report what the row really holds.
func rowVersion(ctx context.Context, q queryable, op, profileID string) (int64, error) {
	var v int64
	if err := q.queryRow(ctx, op, "SELECT row_version FROM profiles WHERE id = ?", profileID).Scan(&v); err != nil {
		return 0, mapReadError(op, err)
	}
	return v, nil
}

// guardedProfileUpdate runs one UPDATE of the revision's own row and moves its
// row_version in the *same statement*: `SET <set>, row_version = row_version + 1
// WHERE id = ? AND row_version = ?`. Zero rows affected is the compare failing, and
// it is reported as itself — ErrRevisionConflict when the row is there with a
// different version, ErrNotFound when it has been deleted underneath us.
//
// Folding the bump into the write (rather than a second statement) is what makes
// the pair (value, version) impossible to split: a mutation can never be observed
// with an unmoved version, in any interleaving — and because the bump is the same
// statement, a rolled-back mutation rolls its bump back with it. The repositories
// all run under BEGIN IMMEDIATE, so the guard is belt-and-braces against a future
// writer that does not; it is not there to resolve races this package's own
// transactions can produce.
func (tx *Tx) guardedProfileUpdate(ctx context.Context, op, profileID string, expected int64, set string, args ...any) (int64, error) {
	query := "UPDATE profiles SET " + set + ", row_version = row_version + 1 WHERE id = ? AND row_version = ?"
	res, err := tx.exec(ctx, op, query, append(args, profileID, expected)...)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return expected + 1, nil
	}
	have, err := rowVersion(ctx, tx, op, profileID)
	if err != nil {
		return 0, err
	}
	return have, revisionConflict(op, profileID, expected, have)
}

// bumpRowVersion records that the revision was written without changing any other
// column (`SET id = id` is the no-op that keeps one code path for every write).
// Callers pass the version they loaded at the start of their transaction, which is
// the value the guard compares against; expected+1 is the answer they return.
//
// A revision that has never been written does not exist here: 0004's DEFAULT 1
// means creating a row *is* version 1, so no caller special-cases a zero.
func (tx *Tx) bumpRowVersion(ctx context.Context, op, profileID string, expected int64) (int64, error) {
	return tx.guardedProfileUpdate(ctx, op, profileID, expected, "id = id")
}

// ListProfiles returns every revision of an edition, lowest revision first.
//
// Deliberately unpaginated: the row count grows by one per human publish click
// (docs/spec/03), so it is bounded by how often a curator ships, not by how much
// text a game has. Forty revisions of a 50 000-message game is the expected shape,
// and forty rows need no cursor.
func (s *Store) ListProfiles(ctx context.Context, editionID string) ([]games.Profile, error) {
	var out []games.Profile
	err := s.inRead(ctx, func(ctx context.Context, q queryable) error {
		const op = "list profiles"
		// Soft-deleted revisions (ADR-0107) are hidden from the library for the
		// whole retention window: the row is still on disk (that is what the
		// window is for), but the list is the surface a user acts on, and a
		// half-deleted revision is not something to act on. Direct reads answer
		// the same way at the API layer (404), and the purge path reads rows
		// through the store without this filter.
		rows, err := q.query(ctx, op,
			"SELECT "+profileRowColumns+" FROM profiles WHERE edition_id = ? AND deleted_at IS NULL ORDER BY revision_num", editionID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			p, err := scanProfile(rows, op)
			if err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	return out, err
}

// nextRevisionNum returns the next dense revision number for an edition.
//
// MAX+1 rather than COUNT+1 so that a revision removed later (an archived
// revision pruned deliberately, audit rows first) cannot make the next revision
// reuse a number some session's history still names. docs/spec/01's "this session
// ran revision 3" has to stay a permanent statement.
func (tx *Tx) nextRevisionNum(ctx context.Context, editionID string) (int, error) {
	const op = "next profile revision"
	var num int
	if err := tx.queryRow(ctx, op,
		"SELECT COALESCE(MAX(revision_num), 0) + 1 FROM profiles WHERE edition_id = ?", editionID).Scan(&num); err != nil {
		return 0, mapReadError(op, err)
	}
	return num, nil
}

// insertProfile writes a fresh revision. row_version is not named: 0004's DEFAULT 1
// is the record that creation was itself the first write, which is what makes row
// version 1 the honest initial ETag instead of a magic number clients and tests
// would have to special-case.
func (tx *Tx) insertProfile(ctx context.Context, p games.Profile) error {
	const op = "insert profile"
	if _, err := tx.exec(ctx, op,
		`INSERT INTO profiles (id, edition_id, revision_num, parent_revision, status, manifest_digest, published_at, preferred_voice_id, publication_author, narrator_fallback, deleted_at, purge_after)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.EditionID, p.RevisionNum, nullIntPtr(p.ParentRevision), string(p.Status),
		p.ManifestDigest, nullStr(p.PublishedAt), nullStr(p.PreferredVoiceID), p.PublicationAuthor,
		boolInt(p.NarratorFallback), nullStr(p.DeletedAt), nullStr(p.PurgeAfter)); err != nil {
		return err
	}
	return nil
}

// loadMutable reads a revision and refuses anything immutable. Every content
// mutation starts here, which is what turns docs/spec/12's "no published edit
// without valid revision token" into a property of the store instead of a
// convention each API handler has to remember.
func (tx *Tx) loadMutable(ctx context.Context, op, profileID string) (games.Profile, int64, error) {
	p, rev, err := loadProfile(ctx, tx, op, profileID)
	if err != nil {
		return p, rev, err
	}
	if p.IsImmutable() {
		return p, rev, immutable(op, p.Status, profileID)
	}
	return p, rev, nil
}

// setProfileStatus moves a revision along games.ProfileStatus.CanTransition,
// compare-and-swapping the caller's row version first.
//
// Two refusals are worth naming:
//
//   - to == published. Publication is Publish's job: it validates the graph,
//     computes the manifest digest, swaps the edition pointer and writes the audit
//     row. Flipping the column here would create a published revision with no
//     digest and no provenance — games.Profile.Validate rejects that pair, and
//     docs/spec/02 "Transaction workflows" forbids shipping it.
//   - a stale expectedRowVersion → ErrRevisionConflict, the storage half of
//     docs/spec/08's PROFILE_REVISION_CONFLICT. The version is checked before the
//     transition table so a loser learns "your token is stale", which is the
//     actionable answer, rather than "that transition is illegal", which it is not.
func (tx *Tx) setProfileStatus(ctx context.Context, op, profileID string, to games.ProfileStatus, expected int64) (games.Profile, error) {
	p, rev, err := loadProfile(ctx, tx, op, profileID)
	if err != nil {
		return p, err
	}
	if rev != expected {
		return p, revisionConflict(op, profileID, expected, rev)
	}
	if !to.Valid() {
		return p, notFoundError(op, "status "+string(to))
	}
	if to == games.StatusPublished {
		return p, publishThroughPublishCall(op)
	}
	// The transition table is the only rule here. An immutable *content* write is
	// refused by loadMutable; a lifecycle move out of published is legal (archiving
	// a shipped revision is how retirement works — docs/spec/02 keeps it
	// addressable), and any other move into or out of published or archived is
	// refused as immutability, which is the actionable answer: derive a revision.
	if !p.Status.CanTransition(to) {
		if p.IsImmutable() {
			return p, immutable(op, p.Status, profileID)
		}
		return p, transitionError(op, profileID, p, to)
	}

	updated := p
	updated.Status = to
	if err := updated.Validate(); err != nil {
		return p, err
	}
	// The status write and the version bump are one statement, so no reader can ever
	// observe the new status under the old token.
	resultVersion, err := tx.guardedProfileUpdate(ctx, op, profileID, rev,
		"status = ?", string(to))
	if err != nil {
		return p, err
	}
	if to == games.StatusArchived {
		if err := tx.retirePublication(ctx, op, p, resultVersion); err != nil {
			return p, err
		}
	}
	return updated, nil
}

// retirePublication takes a revision out of service when it stopped being the one
// an edition serves through a status write rather than through a successor being
// published, and records that it happened.
//
// The edition pointer is cleared *conditionally* (`AND published_profile_id = ?`):
// archiving a revision that is not the live one — an old release retired long ago,
// which docs/spec/02 keeps addressable — must not blank a pointer naming somebody
// else. Both writes run in the transaction that already moved the status, so an
// observer sees either "this revision is live" or "nothing is live", never the
// intermediate that would leave a session pinning a row the profile list calls
// archived (docs/spec/01 "Runtime and setup isolation").
func (tx *Tx) retirePublication(ctx context.Context, op string, p games.Profile, resultVersion int64) error {
	if _, err := tx.exec(ctx, op,
		`UPDATE editions SET published_profile_id = NULL WHERE id = ? AND published_profile_id = ?`,
		p.EditionID, p.ID); err != nil {
		return err
	}
	// The actor is recorded as 'unknown' rather than as the revision's author:
	// SetProfileStatus carries no identity (docs/spec/08 resolves who a caller is
	// at the API layer), and attributing a retirement to whoever shipped the
	// revision would be inventing a decision nobody made. A publication endpoint
	// that knows the curator uses Publish.
	return tx.insertPublishAction(ctx, op, PublishAction{
		ID:               games.NewID(),
		EditionID:        p.EditionID,
		ProfileID:        p.ID,
		Action:           "archive",
		Result:           "succeeded",
		Actor:            unknownActor,
		ResultRowVersion: resultVersion,
		Detail:           `{"via":"SetProfileStatus"}`,
		CreatedAt:        nowStamp(),
	})
}

// SetProfileStatus is the exported form; see the Tx method for the rules.
func (s *Store) SetProfileStatus(ctx context.Context, profileID string, to games.ProfileStatus, expectedRowVersion int64) error {
	return s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		_, err := tx.setProfileStatus(ctx, "set profile status", profileID, to, expectedRowVersion)
		return err
	})
}

// SetProfileNarratorFallback flips the per-profile narrator fallback
// (ADR-0101): when a committed line's speaker has no approved voice, the
// narrator voice speaks the line if and only if this flag is on.
//
// Deliberately not a version-guarded compare-and-swap: the flag is an
// editorial setting, not content — it changes what a line *resolves to*,
// never what the revision contains — and the same reasoning the import
// report gives for the absent guarded write of preferred_voice_id applies:
// moving row_version would 412 a curator mid-edit for a dial nobody is
// racing on, without protecting any content. The write is a plain
// upsert-free UPDATE; a missing profile is ErrNotFound like every read.
func (s *Store) SetProfileNarratorFallback(ctx context.Context, profileID string, enabled bool) error {
	const op = "set profile narrator fallback"
	return s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		res, err := tx.exec(ctx, op,
			`UPDATE profiles SET narrator_fallback = ? WHERE id = ?`, boolInt(enabled), profileID)
		if err != nil {
			return mapWriteError(op, err)
		}
		// Zero rows affected means the id named no profile: the reads answer
		// that way, so the write does too.
		return errFromAffected(res, op, 1)
	})
}

// ── revision snapshots ──────────────────────────────────────────────────────

// ProfileContent is one revision's whole body: sources, characters, scenes,
// regions, messages with their display pages, the dialogue graph and the candidate
// attributions. docs/spec/02 insists a message is not a page, not an occurrence
// and not an utterance, so they stay separate slices joined by id rather than
// being flattened into one structure.
//
// Corrections are absent by design: they are overlays that outlive the rows they
// describe (0002_content.sql argues at length why corrections.target_id carries no
// foreign key), so "what this revision contains" and "what anyone ever said was
// wrong about it" are different questions.
type ProfileContent struct {
	Profile    games.Profile
	RowVersion int64

	Sources    []games.Source
	Characters []games.Character
	Scenes     []games.Scene
	Regions    []games.Region
	Messages   []games.Message
	Pages      []games.MessagePage
	Edges      []games.MessageEdge
	Assertions []games.SpeakerAssertion
}

// loadProfileContent reads a revision and every content row of it.
//
// Everything goes into memory because both callers need everything: the
// portability export path (docs/spec/02 "Profile portability") and NewRevision.
// The bound that makes that safe is that a revision is one game's text — the
// docs/spec/16 benchmark is 10 000 synthetic messages, a few megabytes of Go
// strings — and no caller may use it as a paging substitute, which is what
// ListMessages is for.
func loadProfileContent(ctx context.Context, q queryable, profileID string) (ProfileContent, error) {
	const op = "load profile content"
	var out ProfileContent
	var err error
	if out.Profile, out.RowVersion, err = loadProfile(ctx, q, op, profileID); err != nil {
		return out, err
	}
	if out.Sources, err = loadSources(ctx, q, profileID); err != nil {
		return out, err
	}
	if out.Characters, err = loadCharacters(ctx, q, profileID); err != nil {
		return out, err
	}
	if out.Scenes, err = loadScenes(ctx, q, profileID); err != nil {
		return out, err
	}
	if out.Regions, err = loadRegions(ctx, q, profileID); err != nil {
		return out, err
	}
	if out.Messages, err = loadProfileMessages(ctx, q, profileID); err != nil {
		return out, err
	}
	if out.Pages, err = loadProfilePages(ctx, q, profileID); err != nil {
		return out, err
	}
	if out.Edges, err = loadProfileEdges(ctx, q, profileID); err != nil {
		return out, err
	}
	out.Assertions, err = loadProfileAssertions(ctx, q, profileID)
	return out, err
}

// GetProfileContent is the exported form of loadProfileContent.
func (s *Store) GetProfileContent(ctx context.Context, profileID string) (ProfileContent, error) {
	var out ProfileContent
	err := s.inRead(ctx, func(ctx context.Context, q queryable) error {
		var err error
		out, err = loadProfileContent(ctx, q, profileID)
		return err
	})
	return out, err
}

// NewRevision copies a revision's content into a fresh draft and returns it.
//
// Why a copy and not a pointer. docs/spec/02 "Transaction workflows" sequences
// draft → new revision snapshot → validation → approval → atomic publication, and
// docs/spec/01 pins active sessions to a published revision. Shared rows would
// make "published means immutable" false the moment somebody edited the draft, so
// a snapshot has to be an independent set of rows.
//
// Every row gets a new id, which forces every internal reference to be rewritten:
// message.speaker_id → the new character id, message.scene_id and
// region.scene_id → the new scene/region ids, edge endpoints → the new message
// ids, pages → the new message id, source references → the new source id. One
// old→new map covers all copied rows and is also applied to the JSON reference
// columns, because the property that matters is "no row of the new revision points
// at a row of the old one" — a stale id inside scenes.visual_anchors would satisfy
// a column-by-column check and still make the graph viewer render the parent's
// regions.
//
// Ids are minted for every row *before* the first INSERT, so a reference can never
// observe a half-built map.
//
// Not copied: corrections (overlays) and the parent's publication audit records
// (that history belongs to the revision that earned it), and the parent's status/publication fields — a snapshot
// is a draft by construction. The preferred voice *is* copied: it is a curator
// choice about this edition, and losing it would make deriving a revision a
// request to re-pick a voice.
func (tx *Tx) NewRevision(ctx context.Context, parentProfileID string) (games.Profile, error) {
	const op = "new profile revision"

	parent, _, err := loadProfile(ctx, tx, op, parentProfileID)
	if err != nil {
		return parent, err
	}
	src, err := loadProfileContent(ctx, tx, parentProfileID)
	if err != nil {
		return parent, err
	}

	num, err := tx.nextRevisionNum(ctx, parent.EditionID)
	if err != nil {
		return parent, err
	}
	parentNum := parent.RevisionNum
	fresh := games.Profile{
		ID:               games.NewID(),
		EditionID:        parent.EditionID,
		RevisionNum:      num,
		ParentRevision:   &parentNum,
		Status:           games.StatusDraft,
		PreferredVoiceID: parent.PreferredVoiceID,
		// An editorial decision about how this profile narrates unknown
		// speakers follows the lineage it was made for (ADR-0101).
		NarratorFallback: parent.NarratorFallback,
	}
	if err := fresh.Validate(); err != nil {
		return parent, err
	}
	if err := tx.insertProfile(ctx, fresh); err != nil {
		return parent, err
	}
	if err := tx.copyProfileContent(ctx, op, src, fresh.ID); err != nil {
		return parent, err
	}

	return fresh, nil
}

// copyProfileContent copies one revision's content rows into the profile whose
// id is dstID, minting a fresh id for every row and rewriting every internal
// reference (the work documented on NewRevision). Both NewRevision (the same
// edition, the next revision number) and CloneProfile (a new game and edition,
// revision 1) end their transaction with this helper, so "a snapshot is an
// independent set of rows, with no row of the new revision pointing at a row of
// the old one" has exactly one implementation (ADR-0107 split the caller, not
// the copy).
func (tx *Tx) copyProfileContent(ctx context.Context, op string, src ProfileContent, dstID string) error {
	remap := make(map[string]string,
		len(src.Sources)+len(src.Characters)+len(src.Scenes)+len(src.Regions)+
			len(src.Messages)+len(src.Assertions))
	nextID := func(old string) string {
		if id, ok := remap[old]; ok {
			return id
		}
		id := games.NewID()
		remap[old] = id
		return id
	}
	for _, r := range src.Sources {
		nextID(r.ID)
	}
	for _, r := range src.Characters {
		nextID(r.ID)
	}
	for _, r := range src.Scenes {
		nextID(r.ID)
	}
	for _, r := range src.Regions {
		nextID(r.ID)
	}
	for _, r := range src.Messages {
		nextID(r.ID)
	}
	for _, r := range src.Assertions {
		nextID(r.ID)
	}
	now := nowStamp()

	// Insert in dependency order: the things messages point at first, then
	// messages, then what hangs off messages. SQLite's own FK enforcement is the
	// backstop if this order is ever wrong, and mapWriteError reports it as
	// ErrConflict rather than a mystery.
	for _, r := range src.Sources {
		out := r
		out.ID, out.ProfileID = nextID(r.ID), dstID
		if err := tx.insertSource(ctx, op, out); err != nil {
			return err
		}
	}
	for _, r := range src.Characters {
		out := r
		out.ID, out.ProfileID = nextID(r.ID), dstID
		out.Aliases = remapRefs(r.Aliases, remap)
		out.VisualDescriptors = remapRefs(r.VisualDescriptors, remap)
		out.VerifiedSourceRefs = remapRefs(r.VerifiedSourceRefs, remap)
		if err := tx.insertCharacter(ctx, op, out); err != nil {
			return err
		}
	}
	for _, r := range src.Scenes {
		out := r
		out.ID, out.ProfileID = nextID(r.ID), dstID
		out.Aliases = remapRefs(r.Aliases, remap)
		out.VisualAnchors = remapRefs(r.VisualAnchors, remap)
		out.ProgressJSON = remapRefs(r.ProgressJSON, remap)
		if err := tx.insertScene(ctx, op, out); err != nil {
			return err
		}
	}
	for _, r := range src.Regions {
		out := r
		out.ID, out.ProfileID = nextID(r.ID), dstID
		out.OCRSettings = remapRefs(r.OCRSettings, remap)
		out.MaskAssetIDs = remapRefs(r.MaskAssetIDs, remap)
		sceneID, err := remapRef(op, remap, r.SceneID, "region.scene_id")
		if err != nil {
			return err
		}
		out.SceneID = sceneID
		if err := tx.insertRegion(ctx, op, out); err != nil {
			return err
		}
	}
	for _, r := range src.Messages {
		out := r
		out.ID, out.ProfileID = nextID(r.ID), dstID
		out.MarkupJSON = remapRefs(r.MarkupJSON, remap)
		out.AttributionEvidence = remapRefs(r.AttributionEvidence, remap)
		out.Flags = remapRefs(r.Flags, remap)
		// The per-message counter restarts: it is the If-Match token for edits of
		// *this* row, and this row has never been edited. Carrying the parent's
		// count would let a client that cached "message revision 4" of the old
		// revision overwrite the fourth edit of the new one.
		out.Revision, out.CreatedAt, out.UpdatedAt = 1, now, now
		sourceID, err := remapRef(op, remap, r.SourceID, "message.source_id")
		if err != nil {
			return err
		}
		out.SourceID = sourceID
		sceneID, err := remapRef(op, remap, r.SceneID, "message.scene_id")
		if err != nil {
			return err
		}
		out.SceneID = sceneID
		speakerID, err := remapRef(op, remap, r.SpeakerID, "message.speaker_id")
		if err != nil {
			return err
		}
		out.SpeakerID = speakerID
		if err := tx.insertMessage(ctx, op, out); err != nil {
			return err
		}
	}
	for _, r := range src.Pages {
		out := r
		msgID, ok := remap[r.MessageID]
		if !ok {
			return outsideProfileError(op, "message page", r.MessageID, r.MessageID)
		}
		out.MessageID = msgID
		out.VisiblePrefixes = remapRefs(r.VisiblePrefixes, remap)
		out.DisplayControls = remapRefs(r.DisplayControls, remap)
		out.TimingJSON = remapRefs(r.TimingJSON, remap)
		if err := tx.insertPage(ctx, op, out); err != nil {
			return err
		}
	}
	for _, r := range src.Edges {
		out := r
		from, ok := remap[r.FromID]
		if !ok {
			return outsideProfileError(op, "edge", r.FromID, r.FromID)
		}
		to, ok := remap[r.ToID]
		if !ok {
			return outsideProfileError(op, "edge", r.ToID, r.ToID)
		}
		out.FromID, out.ToID = from, to
		out.Evidence = remapRefs(r.Evidence, remap)
		sourceID, err := remapRef(op, remap, r.SourceID, "edge.source_id")
		if err != nil {
			return err
		}
		out.SourceID = sourceID
		if err := tx.insertEdge(ctx, op, out); err != nil {
			return err
		}
	}
	for _, r := range src.Assertions {
		out := r
		msgID, ok := remap[r.MessageID]
		if !ok {
			return outsideProfileError(op, "speaker assertion", r.ID, r.MessageID)
		}
		charID, ok := remap[r.CharacterID]
		if !ok {
			return outsideProfileError(op, "speaker assertion", r.ID, r.CharacterID)
		}
		out.ID, out.ProfileID = nextID(r.ID), dstID
		out.MessageID, out.CharacterID = msgID, charID
		out.Evidence = remapRefs(r.Evidence, remap)
		if err := tx.insertAssertion(ctx, op, out); err != nil {
			return err
		}
	}
	return nil
}

// NewRevision is the exported form; see the Tx method.
func (s *Store) NewRevision(ctx context.Context, parentProfileID string) (games.Profile, error) {
	var out games.Profile
	err := s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		var err error
		out, err = tx.NewRevision(ctx, parentProfileID)
		return err
	})
	return out, err
}

// remapRef resolves one optional foreign-key column through the snapshot's id map.
// An empty reference stays empty; a reference to a row that is not part of the
// copied revision is a hard error rather than a silent NULL, because quietly
// dropping a message's speaker would make the snapshot claim an attribution nobody
// made (docs/spec/02 gives "we do not know who speaks" a specific meaning).
func remapRef(op string, remap map[string]string, old, what string) (string, error) {
	if old == "" {
		return "", nil
	}
	if id, ok := remap[old]; ok {
		return id, nil
	}
	return "", fmt.Errorf("store: %s: %s references %s, which is not part of the revision being copied: %w",
		op, what, old, ErrConflict)
}

// outsideProfileError reports a row whose parent is not in the revision being
// copied — reachable only through a hand-written insert, since every repository
// write checks its profile first.
func outsideProfileError(op, kind, rowID, referenced string) error {
	return fmt.Errorf("store: %s: %s %s references %s, which is not part of the revision being copied: %w",
		op, kind, rowID, referenced, ErrConflict)
}

// remapRefs rewrites identifier strings inside a JSON reference column.
//
// The whole document is walked rather than an allowlist of keys: docs/spec/02 keeps
// these columns validated-but-uninterpreted (scenes.visual_anchors,
// characters.verified_source_refs, messages.attribution_evidence all name rows in
// other tables), and a key allowlist is exactly the rule that rots when an adapter
// invents a shape nobody listed. Only a string that is *exactly* one of the copied
// rows' ids is rewritten, so text that merely contains an id as a substring is
// untouched.
//
// Numbers survive as json.Number and the original bytes come back when nothing
// matched, so a snapshot never reformats a markup AST or re-renders a float as a
// side effect of copying it.
func remapRefs(raw json.RawMessage, remap map[string]string) json.RawMessage {
	if len(raw) == 0 || len(remap) == 0 {
		return raw
	}
	var v any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		// games.Validate already rejected invalid JSON on the way in, so an
		// unparsable column means a row that never came through here (an import,
		// a hand edit). Preserve the bytes rather than inventing a rewrite.
		return raw
	}
	changed := false
	v = remapValue(v, remap, &changed)
	if !changed {
		return raw
	}
	out, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return out
}

// remapValue walks a decoded JSON document, replacing exact id matches in place.
func remapValue(v any, remap map[string]string, changed *bool) any {
	switch t := v.(type) {
	case string:
		if id, ok := remap[t]; ok {
			*changed = true
			return id
		}
		return t
	case []any:
		for i, item := range t {
			t[i] = remapValue(item, remap, changed)
		}
		return t
	case map[string]any:
		for k, item := range t {
			t[k] = remapValue(item, remap, changed)
		}
		return t
	default:
		return v
	}
}

// ── scanners and error helpers ──────────────────────────────────────────────

// scanProfileRow reads one row of profileRowColumns. It is the only profile
// scanner in the package on purpose: a separate list scanner is how a get and a
// list drift apart and a UI starts showing one status for a row another endpoint
// reads as something else. The version is part of every profile read, so it is part
// of every profile scan — callers that do not need it (ListProfiles) simply drop it.
func scanProfileRow(r scanner, op string) (games.Profile, int64, error) {
	var (
		p          games.Profile
		parent     sql.NullInt64
		status     string
		pubAt      sql.NullString
		voice      sql.NullString
		fallback   sql.NullInt64
		deletedAt  sql.NullString
		purgeAfter sql.NullString
		rev        int64
	)
	if err := r.Scan(&p.ID, &p.EditionID, &p.RevisionNum, &parent, &status,
		&p.ManifestDigest, &pubAt, &voice, &p.PublicationAuthor, &fallback, &deletedAt, &purgeAfter, &rev); err != nil {
		return p, 0, mapReadError(op, err)
	}
	p.Status = games.ProfileStatus(status)
	p.ParentRevision = optInt(parent)
	p.PublishedAt = pubAt.String
	p.PreferredVoiceID = voice.String
	// The column is NOT NULL DEFAULT 1 (0015); a NULL would only reach a
	// pre-migration row, and that reads as the default, on.
	p.NarratorFallback = !fallback.Valid || fallback.Int64 == 1
	// The soft-delete mark (ADR-0107): NULL means not deleted; the pair is
	// written together by the store, so neither column is backfilled.
	p.DeletedAt = deletedAt.String
	p.PurgeAfter = purgeAfter.String
	return p, rev, nil
}

// scanProfile reads the fields a games.Profile carries and discards the row
// version, for the handful of callers that are showing a list rather than holding a
// token to compare.
func scanProfile(r scanner, op string) (games.Profile, error) {
	p, _, err := scanProfileRow(r, op)
	return p, err
}

func nullIntPtr(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

// notFoundError reports a value outside a vocabulary or a missing row.
func notFoundError(op, what string) error {
	return fmt.Errorf("store: %s: %s: %w", op, what, ErrNotFound)
}

// publishThroughPublishCall is the one place that explains why publication is not
// a status write.
func publishThroughPublishCall(op string) error {
	return fmt.Errorf("store: %s: publication is not a status write, call Store.Publish so the revision is validated, digested, pointer-swapped and audited: %w",
		op, ErrInvalidTransition)
}

func transitionError(op, profileID string, from games.Profile, to games.ProfileStatus) error {
	return fmt.Errorf("store: %s: profile %s is %s and cannot become %s (reachable: %s): %w",
		op, profileID, from.Status, to, joinStatuses(from.Status.NextStates()), ErrInvalidTransition)
}

func joinStatuses(states []games.ProfileStatus) string {
	if len(states) == 0 {
		return "none, archived is terminal"
	}
	names := make([]string, len(states))
	for i, s := range states {
		names[i] = string(s)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
