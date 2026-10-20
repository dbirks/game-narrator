package store

// Publication repository: the gate, the atomic swap of what an edition serves, and
// the audit trail (docs/spec/02 "Transaction workflows", docs/spec/01 "Runtime and
// setup isolation", docs/spec/08's PROFILE_REVISION_CONFLICT and
// CONTENT_RIGHTS_REVIEW_REQUIRED).
//
// The three facts this file writes are the ones 0004_publication.sql exists for:
// `editions.published_profile_id` (what an edition serves), `profiles.row_version`
// (the token every writer compares), and `publish_actions` (who decided, under
// which digest, and what the gate found). ADR-0015 records why they are columns and
// a table rather than rows of the prunable event log.
//
// Publication is the one transaction in this package that changes the meaning of
// the data rather than its content: after it returns, live sessions may pin the
// revision, audio is cached under its manifest digest, and the rows become
// immutable. Everything in Publish therefore happens in a single BEGIN IMMEDIATE
// transaction — validate, archive the incumbent, publish, swap the pointer, audit —
// because half of it is worse than none of it: a revision marked published that the
// pointer swap never got to leaves an edition serving its retired predecessor, a
// revision marked published that the archive step never got to leaves an edition
// with two published revisions and no way to say which one a session ran, and a
// refusal that shipped nothing but reported success is how an unreviewed revision
// reaches a player. Either state survives a restart, which is why the swap is one
// commit rather than two requests.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/dbirks/game-narrator/internal/games"
)

// maxListedProblems bounds how many findings one Publish response carries. The
// gate reports *every* problem it can find (docs/spec/08's error contract wants a
// list, not a first-failure) but a revision imported from a bad dump can have
// 20 000 unattributed lines, and 20 000 strings is neither actionable nor
// sendable — it is also unbounded memory in a browser (AGENTS.md "bounded
// queues"). The count of the remainder is always stated.
const maxListedProblems = 20

// unknownActor names a write the store cannot attribute to anybody. It is a
// literal rather than an empty string because an audit record with no actor fails
// PublishAction.Validate, and because an empty actor reads as "the code forgot"
// rather than "the code was not told" — the difference between a diagnostics gap
// and a bug.
const unknownActor = "unknown"

// maxPublicationAuthor matches profiles.publication_author's domain cap in
// games.Profile.Validate; restated here only to name the requirement.
const maxPublicationAuthor = 200

// PublishError is the publication gate's refusal: every finding, in one error.
//
// It wraps ErrUnpublishable so errors.Is keeps working for callers that only care
// "did the gate stop me", while Problems carries what to tell the curator —
// docs/spec/03 requires the review screen to show what has to be fixed before a
// publish is even possible, and docs/spec/08 renders the list into a problem+json
// body.
type PublishError struct {
	ProfileID string
	Problems  []string
}

// Error lists every finding rather than the first, which is the whole point of
// the type.
func (e *PublishError) Error() string {
	return fmt.Sprintf("store: profile %s is not publishable (%d problem(s)): %s",
		e.ProfileID, len(e.Problems), strings.Join(e.Problems, "; "))
}

// Unwrap makes errors.Is(err, ErrUnpublishable) true.
func (e *PublishError) Unwrap() error { return ErrUnpublishable }

// ── the audit trail (docs/spec/02 "Entity catalog": publish_actions) ─────────

// PublishAction is one entry of an edition's publication history: what shipped,
// what it retired, who decided, which manifest digest was validated and what the
// gate found. It is the Go shape of a row of `publish_actions` (0004_publication.sql
// argues the table); the columns that have no column of their own — the gate's
// problem list, the caller's expected token, the superseded revision, the digest —
// travel in `detail_json`, which this type is the only reader of.
//
// The wire/domain vocabulary and the column vocabulary differ on purpose: the
// stored value names the *state the row left behind* ('published', 'archived'), the
// Go value names the *act* ("publish", "archive"), and a reader of either side can
// only be confused if the mapping is in more than one place, so it is in exactly one
// (publishActionColumns / publishActionOf).
type PublishAction struct {
	ID        string
	EditionID string
	ProfileID string
	// SupersededProfileID names the revision this publication retired. Only a
	// publish row has one; an archive record is itself about the revision it
	// retired, so its ProfileID already answers that question.
	SupersededProfileID string
	// Action is "publish", "archive" or "rejected".
	Action string
	// Result is "succeeded" or "rejected", kept distinct from Action because the
	// gate can be asked twice for one revision and an operator must be able to
	// tell "shipped" from "attempted and refused".
	Result string
	Actor  string
	// ManifestDigest is the fingerprint that was validated when this decision was
	// made — the value an audio cache namespace and a rights review are both keyed
	// by, so an audit read must be able to name it without re-deriving it.
	ManifestDigest     string
	ExpectedRowVersion int64
	ResultRowVersion   int64
	Problems           []string
	// Detail is the writer's own context, kept as JSON so a new reason never has to
	// widen the row (e.g. {"via":"SetProfileStatus"}).
	Detail string
	// CreatedAt is the instant the decision committed (the column's occurred_at).
	CreatedAt string
}

// Validate keeps the rows this package writes honest about themselves, for the same
// reason every other entity is validated before a write: a reader must be able to
// trust an audit row's shape without re-deriving it, and the schema's CHECKs guard
// the columns, not the meaning of the detail document.
func (a PublishAction) Validate() error {
	switch a.Action {
	case "publish", "archive", "rejected":
	default:
		return fmt.Errorf("store: publish action %q is not one of publish, archive, rejected", a.Action)
	}
	switch a.Result {
	case "succeeded", "rejected":
	default:
		return fmt.Errorf("store: publish action result %q is not one of succeeded, rejected", a.Result)
	}
	if !games.ValidID(a.ID) {
		return fmt.Errorf("store: publish action id %q is not a ULID", a.ID)
	}
	if !games.ValidID(a.EditionID) || !games.ValidID(a.ProfileID) {
		return fmt.Errorf("store: publish action %s must name a real edition and profile", a.ID)
	}
	if a.Actor == "" || len([]rune(a.Actor)) > maxPublicationAuthor {
		return fmt.Errorf("store: publish action actor must be 1-%d characters", maxPublicationAuthor)
	}
	if a.CreatedAt == "" {
		return fmt.Errorf("store: publish action created_at is required")
	}
	if a.ResultRowVersion < 1 {
		// 0004's CHECK (row_version >= 1) would refuse the row too; refusing it here
		// says which field is wrong instead of reporting a schema constraint.
		return fmt.Errorf("store: publish action %s must name the row version it produced", a.ID)
	}
	if (a.Action == "rejected") != (a.Result == "rejected") {
		// A refusal that claims to have succeeded, or a success that records a
		// refusal, is worse than no record: an auditor would read the wrong history.
		return fmt.Errorf("store: publish action %s is %s with result %s", a.ID, a.Action, a.Result)
	}
	if a.ManifestDigest != "" && len(a.ManifestDigest) != games.MaxSHA256Hex {
		return fmt.Errorf("store: publish action manifest digest must be empty or %d hex characters", games.MaxSHA256Hex)
	}
	return nil
}

// publishActionColumns maps a Go action onto the CHECK-approved column value.
var publishActionColumns = map[string]string{
	"publish":  "published",
	"archive":  "archived",
	"rejected": "rejected",
}

// publishActionOf is its inverse, for reading rows back.
var publishActionOf = map[string]string{
	"published": "publish",
	"archived":  "archive",
	"rejected":  "rejected",
}

// publishActionDetail is the shape of publish_actions.detail_json. It is a struct
// rather than a map so a reader cannot silently mis-name a key, and every field is
// omitempty so an ordinary publication stays the few bytes it costs.
type publishActionDetail struct {
	// Problems is the gate's finding list — the reason a 'rejected' row exists, and
	// the thing docs/spec/03's review screen renders.
	Problems []string `json:"problems,omitempty"`
	// ExpectedRowVersion is the token the caller presented. It is in the detail
	// document rather than a column because it is only interesting beside
	// row_version, which is a column: the pair distinguishes a publication that
	// moved the revision from a refusal that left it where it was.
	ExpectedRowVersion int64 `json:"expected_row_version,omitempty"`
	// SupersededProfileID is the revision a publish retired.
	SupersededProfileID string `json:"superseded_profile_id,omitempty"`
	// ManifestDigest is what was validated under this decision.
	ManifestDigest string `json:"manifest_digest,omitempty"`
	// Detail is the writer's own context, verbatim.
	Detail json.RawMessage `json:"detail,omitempty"`
}

// encode renders the detail document, defaulting to the column's own '{}' so an
// empty audit record is byte-identical whether or not the writer touched it.
func (d publishActionDetail) encode() (string, error) {
	raw, err := json.Marshal(d)
	if err != nil {
		// Every field is a string, an int or a []string: this cannot fail unless a
		// caller put unparsable bytes into Detail, which insertPublishAction rejects
		// before it gets here.
		return "", fmt.Errorf("store: encode publish action detail: %w", err)
	}
	return string(raw), nil
}

// insertPublishAction writes one audit row. It runs inside the caller's transaction,
// so a publication that is rolled back takes its audit row with it — except for a
// gate refusal, which recordRejection commits on its own after the publication
// transaction has been abandoned (a refusal is the answer, and the evidence for it
// must survive the rollback of the attempt that produced it).
func (tx *Tx) insertPublishAction(ctx context.Context, op string, a PublishAction) error {
	if err := a.Validate(); err != nil {
		return err
	}
	action, ok := publishActionColumns[a.Action]
	if !ok { // Validate already refused anything else
		return fmt.Errorf("store: %s: unknown publish action %q", op, a.Action)
	}
	detail := a.Detail
	if detail == "" {
		detail = "{}"
	}
	if !json.Valid([]byte(detail)) {
		return fmt.Errorf("store: %s: publish action detail is not valid JSON", op)
	}
	raw, err := publishActionDetail{
		Problems:            a.Problems,
		ExpectedRowVersion:  a.ExpectedRowVersion,
		SupersededProfileID: a.SupersededProfileID,
		ManifestDigest:      a.ManifestDigest,
		Detail:              json.RawMessage(detail),
	}.encode()
	if err != nil {
		return fmt.Errorf("store: %s: %w", op, err)
	}
	_, err = tx.exec(ctx, op,
		`INSERT INTO publish_actions (id, profile_id, edition_id, action, author, row_version, detail_json, occurred_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.ProfileID, a.EditionID, action, a.Actor, a.ResultRowVersion, raw, a.CreatedAt)
	return err
}

// publishActionColumnsSQL is the SELECT list of the one query that reads the audit
// trail back, in scanPublishAction's order.
const publishActionSelect = `SELECT id, profile_id, edition_id, action, author, row_version, detail_json, occurred_at
	 FROM publish_actions`

// scanPublishAction decodes one row. A detail document this build cannot read is an
// error rather than a silently empty audit record: an auditor is entitled to know
// the evidence is there and unreadable, not to be shown a row that looks clean.
func scanPublishAction(r scanner) (PublishAction, error) {
	const op = "scan publish action"
	var (
		a      PublishAction
		action string
		detail string
		stored int64
	)
	if err := r.Scan(&a.ID, &a.ProfileID, &a.EditionID, &action, &a.Actor, &stored, &detail, &a.CreatedAt); err != nil {
		return a, mapReadError(op, err)
	}
	var ok bool
	if a.Action, ok = publishActionOf[action]; !ok {
		return a, fmt.Errorf("store: %s: publish_actions holds action %q, which this build does not know", op, action)
	}
	var d publishActionDetail
	if err := json.Unmarshal([]byte(detail), &d); err != nil {
		return a, fmt.Errorf("store: %s: audit row %s has a detail document this build cannot read: %w", op, a.ID, err)
	}
	a.Problems = d.Problems
	a.ExpectedRowVersion = d.ExpectedRowVersion
	a.SupersededProfileID = d.SupersededProfileID
	a.ManifestDigest = d.ManifestDigest
	a.ResultRowVersion = stored
	if d.Detail != nil && string(d.Detail) != "{}" {
		a.Detail = string(d.Detail)
	}
	// Result is derived, not stored: it is a function of the action, and a column
	// for it could disagree with the one it describes.
	a.Result = "succeeded"
	if a.Action == "rejected" {
		a.Result = "rejected"
	}
	return a, nil
}

// listPublishActions returns an edition's publication history, newest first.
//
// Ordering is `occurred_at DESC, rowid DESC`: a timestamp at second resolution
// cannot separate the two records one publication writes (the retirement and the
// promotion), and rowid is the insertion order of this table, which is the order the
// decisions were really taken. An id would be wrong here — games.NewID is
// time-ordered only to the millisecond — and the FTS/event-log `seq` does not exist
// for this table, which is the point of giving the audit trail its own rows.
func (s *Store) listPublishActions(ctx context.Context, editionID string, limit int) ([]PublishAction, error) {
	limit = clampLimit(limit, MaxListLimit)
	var out []PublishAction
	err := s.inRead(ctx, func(ctx context.Context, q queryable) error {
		const op = "list publish actions"
		rows, err := q.query(ctx, op,
			publishActionSelect+" WHERE edition_id = ? ORDER BY occurred_at DESC, rowid DESC LIMIT ?", editionID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			a, err := scanPublishAction(rows)
			if err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Publish ships a revision that has been put up for review.
//
// The steps, in the one transaction that makes them atomic:
//
//  1. Compare-and-swap the caller's row version. Stale → ErrRevisionConflict; this
//     is the check that makes two publishers holding the same ETag produce one
//     publication instead of two racing ones. The version is compared before the
//     status, so the loser is told its token is stale rather than being told
//     something it already knew.
//  2. Run the gate (validateForPublish) and collect *every* finding. Anything
//     found → the transaction is rolled back and the refusal is recorded as a
//     rejected audit row in its own transaction (see recordRejection).
//  3. Archive the edition's previously published revision, if any. First, so the
//     one-published-revision invariant (docs/spec/02: a session asks "what is
//     live" and must get one answer, and profiles_one_published_idx enforces it at
//     the schema level too) is never violated, and so the archiving is part of the
//     same commit as the promotion: a crash cannot leave two published revisions or
//     none.
//  4. Set this revision to published with published_at, publication_author and the
//     manifest digest computed from the rows as they now stand — and move its
//     row_version in the same statement, so the frozen revision can never be read
//     under the token that guarded it as a draft.
//  5. Swap `editions.published_profile_id` to this revision. That pointer is what
//     PublishedProfile reads and therefore what a session start pins; the status
//     column is the state, the pointer is the decision, and one commit writes both.
//  6. Append the audit records: this revision published (naming what it superseded)
//     and, in step 3, the predecessor archived.
//
// The previously published revision is archived rather than deleted: its audio
// cache namespace and its recorded occurrences must survive (docs/spec/02 keeps
// archived revisions addressable), and sessions already running it keep running it
// until they stop.
func (s *Store) Publish(ctx context.Context, profileID string, expectedRowVersion int64, author string) (games.Profile, error) {
	var (
		out      games.Profile
		rejected *PublishError
	)
	err := s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		var err error
		out, err = tx.publish(ctx, "publish profile", profileID, expectedRowVersion, author)
		if errors.As(err, &rejected) {
			return rejected // rolls the publication back
		}
		return err
	})
	if rejected != nil {
		// The gate's findings are recorded even though the data did not change —
		// docs/spec/16's evidence bundles have to be able to answer "was this ever
		// offered for publication, and why not" from the database, not from log
		// retention. This is a separate, committed transaction by necessity: the
		// publication transaction is being rolled back, and an audit row inside it
		// would be rolled back with it.
		if auditErr := s.recordRejection(ctx, rejected.ProfileID, author, expectedRowVersion, rejected.Problems); auditErr != nil {
			return out, errors.Join(err, auditErr)
		}
	}
	return out, err
}

func (tx *Tx) publish(ctx context.Context, op, profileID string, expected int64, author string) (games.Profile, error) {
	p, rev, err := loadProfile(ctx, tx, op, profileID)
	if err != nil {
		return p, err
	}
	if rev != expected {
		return p, revisionConflict(op, profileID, expected, rev)
	}
	if strings.TrimSpace(author) == "" {
		return p, fmt.Errorf("store: %s: an author is required, the audit trail must name who shipped this", op)
	}
	if n := len([]rune(author)); n > maxPublicationAuthor {
		return p, fmt.Errorf("store: %s: author is %d characters, the limit is %d", op, n, maxPublicationAuthor)
	}

	problems, err := tx.validateForPublish(ctx, p)
	if err != nil {
		return p, err
	}
	if len(problems) > 0 {
		return p, &PublishError{ProfileID: profileID, Problems: problems}
	}

	now := nowStamp()

	// 3. Retire the incumbent.
	superseded, err := tx.archiveIncumbent(ctx, op, p.EditionID, profileID, author)
	if err != nil {
		return p, err
	}

	// 4+5. Publish this revision and move the pointer. The digest is computed from
	// the rows as they are now, before the status write, so what is stored is a
	// fingerprint of exactly what was validated (docs/spec/02: the digest is what the
	// audio cache key and the publication gate both derive from).
	//
	// Two writes, one commit, and an order that matters: the version bump is folded
	// into the status statement, so there is no instant at which a reader can see
	// 'published' under the pre-publication token (docs/spec/12's "no published edit
	// without valid revision token" is a property of the row, not of the pointer),
	// and the pointer moves only after the row it names says published.
	content, err := loadProfileContent(ctx, tx, profileID)
	if err != nil {
		return p, err
	}
	digest := contentDigest(p, content)

	published := p
	published.Status = games.StatusPublished
	published.PublishedAt = now
	published.PublicationAuthor = author
	published.ManifestDigest = digest
	if err := published.Validate(); err != nil {
		return p, err
	}

	resultVersion, err := tx.guardedProfileUpdate(ctx, op, profileID, rev,
		"status = 'published', published_at = ?, manifest_digest = ?, publication_author = ?",
		now, digest, author)
	if err != nil {
		return p, err
	}

	res, err := tx.exec(ctx, op,
		"UPDATE editions SET published_profile_id = ? WHERE id = ?", profileID, p.EditionID)
	if err != nil {
		return p, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Unreachable while the profile's own edition_id foreign key holds; stated as
		// a missing row rather than a silent no-op because a publication that shipped
		// without moving the pointer would report success and serve the predecessor.
		return p, notFoundError(op, "edition "+p.EditionID)
	}

	// 6. The audit record, part of this commit: it is what makes "who shipped this,
	// under which digest, against which token" answerable after the fact, from the
	// same database the evidence bundle points at.
	action := PublishAction{
		ID:                  games.NewID(),
		EditionID:           p.EditionID,
		ProfileID:           profileID,
		SupersededProfileID: superseded,
		Action:              "publish",
		Result:              "succeeded",
		Actor:               author,
		ManifestDigest:      digest,
		ExpectedRowVersion:  expected,
		ResultRowVersion:    resultVersion,
		Problems:            nil,
		Detail:              "{}",
		CreatedAt:           now,
	}
	if err := tx.insertPublishAction(ctx, op, action); err != nil {
		return p, err
	}
	return published, nil
}

// archiveIncumbent moves the edition's currently published revision (other than
// the one being published) to archived, and returns its id (” when there was
// none). Archiving bumps that revision's own row_version in the same statement:
// an editor holding its token must find out it stopped being published, not read it
// as unchanged.
func (tx *Tx) archiveIncumbent(ctx context.Context, op, editionID, keepID, author string) (string, error) {
	var (
		superseded string
		rev        int64
	)
	err := tx.queryRow(ctx, op,
		`SELECT id, row_version FROM profiles
		 WHERE edition_id = ? AND status = 'published' AND id <> ?
		 ORDER BY revision_num DESC LIMIT 1`,
		editionID, keepID).Scan(&superseded, &rev)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", nil
	case err != nil:
		return "", mapReadError(op, err)
	}
	resultVersion, err := tx.guardedProfileUpdate(ctx, op, superseded, rev, "status = 'archived'")
	if err != nil {
		return "", err
	}
	// Retiring the incumbent is itself a publication-lifecycle act and is audited,
	// so the edition's history reads as a sequence of decisions rather than as an
	// unexplained status change.
	if err := tx.insertPublishAction(ctx, op, PublishAction{
		ID:        games.NewID(),
		EditionID: editionID,
		ProfileID: superseded,
		Action:    "archive",
		Result:    "succeeded",
		// The curator who published the successor is the actor behind the
		// retirement: there was no separate decision, and inventing a second
		// author here would be worse than naming the one that caused it.
		Actor:              author,
		ExpectedRowVersion: rev,
		ResultRowVersion:   resultVersion,
		CreatedAt:          nowStamp(),
	}); err != nil {
		return "", err
	}
	return superseded, nil
}

// validateForPublish is the gate. It deliberately reports every finding rather
// than the first: a curator who has to fix one problem, retry, and be told about
// the next one will not finish this revision this decade (docs/spec/03's review
// screen shows the whole list).
//
// Checks, and why each is here rather than in games.Validate:
//
//   - status is review: publication goes through review and nowhere else
//     (games.ProfileStatus.CanTransition). Checked against the row, so it belongs
//     to the gate rather than the type.
//   - at least one message: a publishable revision narrates something.
//   - every message's speaker_id/scene_id resolves *inside this revision*: the
//     schema's foreign keys prove the row exists, only this proves it is part of
//     the same snapshot — messages.speaker_id → characters has no profile
//     qualification (0002_content.sql explains why the FK is SET NULL and
//     cross-table), so a graph assembled by hand can point at another revision's
//     character and still satisfy SQLite.
//   - every source is publishable: docs/spec/14 forbids shipping derived content
//     from material whose rights position is unknown, and docs/spec/08 has an
//     error code (CONTENT_RIGHTS_REVIEW_REQUIRED) for exactly this refusal.
//   - every message's source, when it names one, is in this revision: same
//     reasoning as the speaker, and provenance that points elsewhere is
//     unverifiable at audit time.
func (tx *Tx) validateForPublish(ctx context.Context, p games.Profile) ([]string, error) {
	const op = "validate for publication"
	var problems []string
	addf := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	if p.Status != games.StatusReview {
		addf("status is %q: a revision must be in %q before it can be published (Store.SetProfileStatus)",
			string(p.Status), string(games.StatusReview))
	}

	// Each query below returns up to maxListedProblems+1 rows, so the "and N more"
	// tail is exact rather than a guess, and a revision with 20 000 findings costs
	// one bounded read instead of materialising 20 000 problems.
	q := func(query string, args ...any) ([][1]string, error) {
		rows, err := tx.query(ctx, op, query, append(args, maxListedProblems+1)...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out [][1]string
		for rows.Next() {
			var v [1]string
			if err := rows.Scan(&v[0]); err != nil {
				return nil, mapReadError(op, err)
			}
			out = append(out, v)
		}
		return out, rows.Err()
	}

	// Truncated list + exact remainder: one COUNT for the total, one bounded list
	// for the examples. The count is cheap because every query below is profile
	// scoped and index-seekable.
	total := func(query string, args ...any) (int, error) {
		var n int
		if err := tx.queryRow(ctx, op, query, args...).Scan(&n); err != nil {
			return 0, mapReadError(op, err)
		}
		return n, nil
	}
	enumerate := func(label, listQuery, countQuery string, args ...any) error {
		rows, err := q(listQuery, args...)
		if err != nil {
			return err
		}
		n, err := total(countQuery, args...)
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		ids := make([]string, 0, len(rows))
		for _, r := range rows[:min(len(rows), maxListedProblems)] {
			ids = append(ids, r[0])
		}
		if n > len(ids) {
			addf("%s: %s and %d more (%d total)", label, strings.Join(ids, ", "), n-len(ids), n)
		} else {
			addf("%s: %s", label, strings.Join(ids, ", "))
		}
		return nil
	}

	var messages int
	if err := tx.queryRow(ctx, op,
		"SELECT count(*) FROM messages WHERE profile_id = ?", p.ID).Scan(&messages); err != nil {
		return problems, mapReadError(op, err)
	}
	if messages == 0 {
		addf("the revision contains no messages, so there is nothing to narrate; add dialogue before publishing: import the disc (disc import), stage a script file you have (the setup upload, imported by the agent with import_user_asset), or approve a web text source (the agent proposes one with propose_web_import; after you approve that exact source it is staged by import_web_source) — or enter lines manually")
	}

	if err := enumerate("message speaker_id does not resolve inside this revision",
		`SELECT m.id FROM messages m
		 LEFT JOIN characters c ON c.id = m.speaker_id
		 WHERE m.profile_id = ? AND m.speaker_id IS NOT NULL AND (c.id IS NULL OR c.profile_id <> m.profile_id)
		 ORDER BY m.id LIMIT ?`,
		`SELECT count(*) FROM messages m
		 LEFT JOIN characters c ON c.id = m.speaker_id
		 WHERE m.profile_id = ? AND m.speaker_id IS NOT NULL AND (c.id IS NULL OR c.profile_id <> m.profile_id)`,
		p.ID); err != nil {
		return problems, err
	}
	if err := enumerate("message scene_id does not resolve inside this revision",
		`SELECT m.id FROM messages m
		 LEFT JOIN scenes s ON s.id = m.scene_id
		 WHERE m.profile_id = ? AND m.scene_id IS NOT NULL AND (s.id IS NULL OR s.profile_id <> m.profile_id)
		 ORDER BY m.id LIMIT ?`,
		`SELECT count(*) FROM messages m
		 LEFT JOIN scenes s ON s.id = m.scene_id
		 WHERE m.profile_id = ? AND m.scene_id IS NOT NULL AND (s.id IS NULL OR s.profile_id <> m.profile_id)`,
		p.ID); err != nil {
		return problems, err
	}
	if err := enumerate("message source_id does not resolve inside this revision",
		`SELECT m.id FROM messages m
		 LEFT JOIN sources so ON so.id = m.source_id
		 WHERE m.profile_id = ? AND m.source_id IS NOT NULL AND (so.id IS NULL OR so.profile_id <> m.profile_id)
		 ORDER BY m.id LIMIT ?`,
		`SELECT count(*) FROM messages m
		 LEFT JOIN sources so ON so.id = m.source_id
		 WHERE m.profile_id = ? AND m.source_id IS NOT NULL AND (so.id IS NULL OR so.profile_id <> m.profile_id)`,
		p.ID); err != nil {
		return problems, err
	}
	// Rights gate. games.RightsStatus.Publishable() is the vocabulary
	// (docs/spec/14: nothing derived from material with an unresolved rights
	// position may ship), and docs/spec/08 has CONTENT_RIGHTS_REVIEW_REQUIRED for
	// the refusal. It is scoped to the sources this revision's *content* rests on —
	// the ones a message or an edge names — rather than to every provenance row of
	// the revision, because provenance is immutable once registered (docs/spec/01
	// invariant 6): an unreferenced row nobody is allowed to edit or delete would
	// make the refusal unfixable, and an unfixable refusal is a lock, not a
	// finding. Unpublished provenance that backs nothing cannot be shipped.
	if err := enumerate(`source has an unpublishable rights_status (CONTENT_RIGHTS_REVIEW_REQUIRED)`,
		`SELECT id FROM sources
		 WHERE profile_id = ? AND rights_status NOT IN ('user_supplied', 'licensed')
		   AND id IN (SELECT source_id FROM messages WHERE profile_id = ? AND source_id IS NOT NULL
		              UNION
		              SELECT e.source_id FROM message_edges e JOIN messages m ON m.id = e.from_id
		                WHERE m.profile_id = ? AND e.source_id IS NOT NULL)
		 ORDER BY id LIMIT ?`,
		`SELECT count(*) FROM sources
		 WHERE profile_id = ? AND rights_status NOT IN ('user_supplied', 'licensed')
		   AND id IN (SELECT source_id FROM messages WHERE profile_id = ? AND source_id IS NOT NULL
		              UNION
		              SELECT e.source_id FROM message_edges e JOIN messages m ON m.id = e.from_id
		                WHERE m.profile_id = ? AND e.source_id IS NOT NULL)`,
		p.ID, p.ID, p.ID); err != nil {
		return problems, err
	}
	// A speaker assertion whose character or message was removed cannot be
	// re-derived later, and docs/spec/02 keeps these as the evidence behind an
	// attribution; SQLite's cascade would have deleted the row already, so this
	// only bites for a profile assembled outside the repositories.
	if err := enumerate("region scene_id does not resolve inside this revision",
		`SELECT r.id FROM regions r
		 LEFT JOIN scenes s ON s.id = r.scene_id
		 WHERE r.profile_id = ? AND r.scene_id IS NOT NULL AND (s.id IS NULL OR s.profile_id <> r.profile_id)
		 ORDER BY r.id LIMIT ?`,
		`SELECT count(*) FROM regions r
		 LEFT JOIN scenes s ON s.id = r.scene_id
		 WHERE r.profile_id = ? AND r.scene_id IS NOT NULL AND (s.id IS NULL OR s.profile_id <> r.profile_id)`,
		p.ID); err != nil {
		return problems, err
	}
	return problems, nil
}

// recordRejection commits the gate's refusal after the publication transaction
// was rolled back.
//
// It writes with its own row-version snapshot and never fails the caller's
// request: the finding list is the answer, and losing the audit row is a
// diagnostics gap rather than a reason to turn a 409 into a 500.
func (s *Store) recordRejection(ctx context.Context, profileID, author string, expected int64, problems []string) error {
	return s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		const op = "record rejected publication"
		p, rev, err := loadProfile(ctx, tx, op, profileID)
		if err != nil {
			return err
		}
		actor := author
		if strings.TrimSpace(actor) == "" {
			// The gate can be reached without a name only by a caller with a bug,
			// and an audit row with an empty actor is worse than one that says so.
			actor = unknownActor
		}
		// A rejection changes nothing, so the stored version is still the one the
		// caller presented; recording both keeps the row comparable with a
		// successful one's expected/result pair.
		return tx.insertPublishAction(ctx, op, PublishAction{
			ID:                 games.NewID(),
			EditionID:          p.EditionID,
			ProfileID:          profileID,
			Action:             "rejected",
			Result:             "rejected",
			Actor:              actor,
			ExpectedRowVersion: expected,
			ResultRowVersion:   rev,
			Problems:           problems,
			CreatedAt:          nowStamp(),
		})
	})
}

// PublishedProfile returns the revision an edition currently serves: the row its
// `published_profile_id` names.
//
// Why a pointer column and not a status query. docs/spec/01 "Runtime and setup
// isolation" requires that a session ask one question — "what is live" — and get
// one answer, and docs/spec/02 "Transaction workflows" names the operation an "atomic
// publication pointer swap". A status query answers "which rows claim to be published",
// which needs a tie-break as soon as anything writes outside these repositories; a
// pointer answers the question as asked, is one indexed lookup, and cannot return two
// revisions. The two representations are kept in agreement by construction rather than
// by a reconciler: Publish writes the status and the pointer in the same BEGIN
// IMMEDIATE transaction, archiving a live revision clears the pointer in the same
// transaction as its status write, and profiles_one_published_idx (0004) makes a second
// published revision of one edition unrepresentable even to raw SQL. There is therefore
// exactly one way for the pointer to name a row whose status is not 'published': a hand
// edit of the file. That state is reported as ErrConflict rather than served, because
// docs/spec/13 wants a session to refuse to start rather than narrate a revision the
// review screen says was retired.
//
// The version that comes back with it is the ETag source for the live revision, read in
// the same statement as the row, so a caller cannot be handed a published profile under
// a token that another write has already spent.
//
// An edition with nothing published is ErrNotFound, which the API renders as a 404
// rather than a 200 with a null body: a session that cannot name a revision must
// refuse to start (docs/spec/13's degraded modes), not improvise.
func (s *Store) PublishedProfile(ctx context.Context, editionID string) (games.Profile, int64, error) {
	const op = "get published profile"
	var (
		out games.Profile
		rev int64
	)
	err := s.inRead(ctx, func(ctx context.Context, q queryable) error {
		p, v, err := scanProfileRow(q.queryRow(ctx, op,
			"SELECT "+profileRowColumns+" FROM profiles WHERE id = "+
				"(SELECT published_profile_id FROM editions WHERE id = ?)", editionID), op)
		if err != nil {
			return err
		}
		if p.Status != games.StatusPublished {
			return fmt.Errorf("store: %s: edition %s points at revision %s, which is %s: the publication pointer and the revision's own status disagree, a state these repositories cannot leave behind: %w",
				op, editionID, p.ID, p.Status, ErrConflict)
		}
		out, rev = p, v
		return nil
	})
	return out, rev, err
}

// PublishActions returns an edition's publication history, newest first, bounded
// so an old appliance with a long history cannot be asked to serialise all of it
// (docs/spec/08's hard upper bounds apply to audit reads too).
func (s *Store) PublishActions(ctx context.Context, editionID string, limit int) ([]PublishAction, error) {
	return s.listPublishActions(ctx, editionID, limit)
}

// ── manifest digest ─────────────────────────────────────────────────────────

// contentDigest fingerprints the exact rows that were validated.
//
// Why a digest at all: docs/spec/02 makes it both the publication gate's output
// and the audio cache key's first component, and docs/spec/01 key invariant 4
// requires cache identity to include the profile revision. A cache key that is a
// *sequence number* would be reused the moment a revision number is recycled and
// start serving the wrong voice over the right game; a content digest cannot.
//
// The serialization is canonical (fields in a fixed order, values joined by a
// separator that cannot appear in a ULID or a JSON document, rows ordered by their
// primary key) and covers every field the narrator can speak or match on: text,
// speech rendering, attribution, scene/speaker identity, graph structure, regions
// and the provenance the rights review hung on.
//
// It is deliberately not a hash of the *tables*: a column added by a later
// migration would change the digest of unmodified content, invalidating the whole
// audio cache for no reason a curator could understand. Fields are listed here, so
// a schema change that does not affect narration leaves the digests alone.
func contentDigest(p games.Profile, c ProfileContent) string {
	h := sha256.New()
	w := func(format string, args ...any) { fmt.Fprintf(h, format, args...) }

	w("gnmanifest/1\n")
	w("edition\t%s\nrevision\t%d\nvoice\t%s\n", p.EditionID, p.RevisionNum, p.PreferredVoiceID)

	for _, r := range c.Sources {
		w("source\t%s\t%s\t%s\t%s\t%s\t%v\n", r.ID, r.SourceType, r.URI, r.ContentSHA256, r.Rights, r.Trust)
	}
	for _, r := range c.Characters {
		w("character\t%s\t%s\t%s\t%s\t%s\n", r.ID, r.Name, r.Status, r.Aliases, r.PortraitAssetID)
	}
	for _, r := range c.Scenes {
		w("scene\t%s\t%s\t%s\t%s\n", r.ID, r.Name, r.Aliases, r.RegionFingerprint)
	}
	for _, r := range c.Regions {
		w("region\t%s\t%s\t%s\t%v\t%v\t%v\t%v\t%s\t%s\n",
			r.ID, r.SceneID, r.RegionType, r.RectX, r.RectY, r.RectW, r.RectH, r.Label, r.OCRSettings)
	}
	for _, r := range c.Messages {
		w("message\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%v\n",
			r.ID, r.TextKind, r.SceneID, r.SpeakerID, r.AttributionStatus,
			r.NormalizedText, r.SpeechText, r.OriginalText, r.Flags)
	}
	for _, r := range c.Pages {
		w("page\t%s\t%d\t%s\t%s\n", r.MessageID, r.Ordinal, r.PageText, r.VisiblePrefixes)
	}
	for _, r := range c.Edges {
		w("edge\t%s\t%s\t%s\t%s\t%v\n", r.FromID, r.ToID, r.EdgeType, r.ConditionExpr, r.Confidence)
	}
	for _, r := range c.Assertions {
		w("assertion\t%s\t%s\t%s\t%s\t%v\n", r.ID, r.MessageID, r.CharacterID, r.Method, r.Posterior)
	}
	return hex.EncodeToString(h.Sum(nil))
}
