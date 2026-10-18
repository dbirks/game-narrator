package store

// Voice workflow persistence (migration 0009, docs/spec/02 entity catalog
// voice_profiles / voice_registrations, ADR-0005). The store enforces the
// lifecycle at the schema boundary: a row may only move draft -> auditioning
// -> approved -> retired, and an approved or retired row is immutable — the
// canonical reference is frozen the moment a human approves it, and the old
// revision is what old synth cache entries keep attributing to (docs/spec/07
// step 8). The provider registry rows (voice_registrations) are mutable by
// design: reconciliation rewrites them on every pass.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/games"
)

// Voice revision lifecycle states (docs/spec/02 voice_profiles.state).
const (
	VoiceStateDraft       = "draft"
	VoiceStateAuditioning = "auditioning"
	VoiceStateApproved    = "approved"
	VoiceStateRetired     = "retired"
)

// Voice registration states (docs/spec/02 voice_registrations.state).
const (
	VoiceRegPending = "pending"
	VoiceRegReady   = "ready"
	VoiceRegMissing = "missing"
	VoiceRegInvalid = "invalid"
)

// VoiceProfile is one row of voice_profiles — one immutable voice revision.
// ID and the lifecycle fields are owned by the workflow (internal/voices);
// this file only moves them between their legal states.
type VoiceProfile struct {
	ID                string
	Character         string
	Revision          int
	State             string
	DesignInstruction string
	// ReferenceAssetID is the content-addressed id of the canonical reference
	// WAV (ADR-0005); empty until approved.
	ReferenceAssetID  string
	RefTranscript     string
	ModelID           string
	EmbeddingMode     string
	Language          string
	AuditionIDs       []string
	ConsentProvenance string
	ApprovedBy        string
	ApprovedAt        *time.Time
	Fingerprint       string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// VoiceAudition is one design candidate of a revision (voice_auditions row).
// VoiceAuditionQAState mirrors the speech cache's QA states (0 unknown /
// not-run, 1 passed, 2 failed) for the audition approval gate (ADR 0061).
type VoiceAuditionQAState int

const (
	// VoiceAuditionQAUnknown: the clip was not ASR-checked (the role is not
	// configured, or the check could not run) — approval is offered, and the
	// UI says QA was not run.
	VoiceAuditionQAUnknown VoiceAuditionQAState = 0
	// VoiceAuditionQAPassed: the transcript matched the reference within the
	// bound — approval is offered.
	VoiceAuditionQAPassed VoiceAuditionQAState = 1
	// VoiceAuditionQAFailed: the transcript diverged beyond the bound — the
	// audition is flagged (WER + transcript are the review evidence) and the
	// approve action is not offered.
	VoiceAuditionQAFailed VoiceAuditionQAState = 2
)

type VoiceAudition struct {
	ID             string
	VoiceProfileID string
	Index          int
	Brief          string
	ModelID        string
	SampleText     string
	Transcript     string
	AssetSHA256    string
	DurationMS     int64
	SampleRate     int
	Origin         string
	RequestID      string
	LatencyMS      int64
	Seed           *int
	CreatedAt      time.Time
	// ASR quality check of the clip against its reference text (FR-10,
	// ADR 0061); the zero value is the "not run" state.
	AsrModel      string
	AsrState      VoiceAuditionQAState
	AsrWER        float64
	AsrCER        float64
	AsrTranscript string
	AsrRequestID  string
	AsrCheckedAt  *time.Time
}

// VoiceRegistration is one row of voice_registrations: the state of one
// revision's voice on one provider instance.
type VoiceRegistration struct {
	VoiceProfileID string
	InstanceID     string
	RegisteredName string
	RemoteDigest   string
	LastVerifiedAt *time.Time
	State          string
	Err            string
}

const voiceProfileCols = `id, character, revision, state, design_instruction,
		canonical_reference_asset_id, ref_transcript, model_id, embedding_mode,
		language, audition_ids, consent_provenance, approved_by, approved_at,
		fingerprint, created_at, updated_at`

const voiceAuditionCols = `id, voice_profile_id, "index", brief, model_id, sample_text,
		transcript, asset_sha256, duration_ms, sample_rate, origin, request_id,
		latency_ms, seed, created_at,
		asr_model, asr_state, asr_wer, asr_cer, asr_transcript, asr_request_id, asr_checked_at`

const voiceRegistrationCols = `voice_profile_id, provider_instance_id, registered_name,
		remote_digest, last_verified_at, state, error`

// voiceProfileFromRow scans one row in the shared column order.
func voiceProfileFromRow(row interface{ Scan(dest ...any) error }) (VoiceProfile, error) {
	var (
		p          VoiceProfile
		aid        string
		approvedAt sql.NullString
		created    string
		updated    string
	)
	if err := row.Scan(&p.ID, &p.Character, &p.Revision, &p.State, &p.DesignInstruction,
		&p.ReferenceAssetID, &p.RefTranscript, &p.ModelID, &p.EmbeddingMode,
		&p.Language, &aid, &p.ConsentProvenance, &p.ApprovedBy, &approvedAt,
		&p.Fingerprint, &created, &updated); err != nil {
		return p, err
	}
	var err error
	if p.CreatedAt, err = parseStamp(created); err != nil {
		return p, err
	}
	if p.UpdatedAt, err = parseStamp(updated); err != nil {
		return p, err
	}
	if approvedAt.Valid {
		t, err := parseStamp(approvedAt.String)
		if err != nil {
			return p, err
		}
		p.ApprovedAt = &t
	}
	ids, err := parseVoiceStringArray(aid)
	if err != nil {
		return p, fmt.Errorf("store: parse audition_ids of %s: %w", p.ID, err)
	}
	p.AuditionIDs = ids
	return p, nil
}

func parseVoiceStringArray(s string) ([]string, error) {
	if strings.TrimSpace(s) == "" {
		return []string{}, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, err
	}
	return out, nil
}

func marshalVoiceStringArray(ids []string) string {
	if len(ids) == 0 {
		return "[]"
	}
	b, err := json.Marshal(ids)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// voiceStamp renders one nullable RFC 3339 stamp for SQL.
func voiceStamp(t *time.Time) any {
	if t == nil {
		return nil
	}
	return stampMS(*t)
}

// voiceEmbeddingMode keeps the CHECK constraint honest for zero values.
func voiceEmbeddingMode(m string) string {
	if strings.TrimSpace(m) == "" {
		return "registered"
	}
	return m
}

// errFromAffected verifies that a write actually touched the expected number
// of rows: zero rows on an insert is a constraint the caller must surface as
// a conflict, and one the update expected is the only legal outcome here.
func errFromAffected(res sql.Result, op string, want int64) error {
	if res == nil {
		return nil
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != want {
		return notFound(op, "row")
	}
	return nil
}

// voiceTransition reports the legal state moves for a revision row.
func voiceTransition(from, to string) bool {
	switch {
	case from == VoiceStateDraft && to == VoiceStateAuditioning:
		return true
	case from == VoiceStateAuditioning && to == VoiceStateApproved:
		return true
	case from == VoiceStateApproved && to == VoiceStateRetired:
		return true
	}
	return false
}

// CreateVoiceRevision inserts a new revision row. The caller chooses the id
// (games.NewID) so the workflow layer can report it; the UNIQUE
// (character, revision) constraint is the last guard against a numbering race.
func (s *Store) CreateVoiceRevision(ctx context.Context, p VoiceProfile) error {
	const op = "create voice revision"
	if !games.ValidID(p.ID) {
		return fmt.Errorf("store: %s: id is not a ULID", op)
	}
	if strings.TrimSpace(p.Character) == "" {
		return fmt.Errorf("store: %s: character is empty", op)
	}
	if p.Revision < 1 {
		return fmt.Errorf("store: %s: revision must be >= 1", op)
	}
	if p.State == "" {
		p.State = VoiceStateDraft
	}
	return s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		res, err := tx.exec(ctx, op,
			"INSERT INTO voice_profiles ("+voiceProfileCols+") VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
			p.ID, p.Character, p.Revision, p.State, p.DesignInstruction,
			p.ReferenceAssetID, p.RefTranscript, p.ModelID,
			voiceEmbeddingMode(p.EmbeddingMode), p.Language,
			marshalVoiceStringArray(p.AuditionIDs), p.ConsentProvenance,
			p.ApprovedBy, voiceStamp(p.ApprovedAt), p.Fingerprint,
			stampMS(p.CreatedAt), stampMS(p.UpdatedAt))
		if err != nil {
			return mapWriteError(op, err)
		}
		return errFromAffected(res, op, 1)
	})
}

// UpdateVoiceProfile applies one transition to a revision row. The compare
// and the swap are one statement, so two concurrent approvals cannot both
// land on the same row. Anything off the transition table is refused as
// ErrImmutable: an approved reference is frozen (ADR-0005), and re-editing an
// approved row is how a cache key would keep naming audio that no longer
// exists.
func (s *Store) UpdateVoiceProfile(ctx context.Context, p VoiceProfile) error {
	const op = "update voice revision"
	var current string
	return s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		row := tx.queryRow(ctx, op, `SELECT state FROM voice_profiles WHERE id = ?`, p.ID)
		if err := row.Scan(&current); err != nil {
			if err == sql.ErrNoRows {
				return notFound(op, "voice revision "+p.ID)
			}
			return mapReadError(op, err)
		}
		if !voiceTransition(current, p.State) {
			return fmt.Errorf("store: %s: %s cannot move from %s to %s: %w",
				op, p.ID, current, p.State, ErrImmutable)
		}
		res, err := tx.exec(ctx, op,
			`UPDATE voice_profiles SET state=?, design_instruction=?, canonical_reference_asset_id=?,
				ref_transcript=?, model_id=?, embedding_mode=?, language=?, audition_ids=?,
				consent_provenance=?, approved_by=?, approved_at=?, fingerprint=?, updated_at=?
			 WHERE id=?`,
			p.State, p.DesignInstruction, p.ReferenceAssetID, p.RefTranscript,
			p.ModelID, voiceEmbeddingMode(p.EmbeddingMode), p.Language,
			marshalVoiceStringArray(p.AuditionIDs), p.ConsentProvenance,
			p.ApprovedBy, voiceStamp(p.ApprovedAt), p.Fingerprint,
			stampMS(p.UpdatedAt), p.ID)
		if err != nil {
			return mapWriteError(op, err)
		}
		return errFromAffected(res, op, 1)
	})
}

// GetVoiceRevision returns one revision row.
func (s *Store) GetVoiceRevision(ctx context.Context, id string) (VoiceProfile, error) {
	const op = "get voice revision"
	var p VoiceProfile
	err := s.inRead(ctx, func(ctx context.Context, q queryable) error {
		var err2 error
		p, err2 = voiceProfileFromRow(q.queryRow(ctx, op,
			`SELECT `+voiceProfileCols+` FROM voice_profiles WHERE id = ?`, id))
		return err2
	})
	if err != nil && err != sql.ErrNoRows {
		return VoiceProfile{}, mapReadError(op, err)
	}
	if p.ID == "" {
		return VoiceProfile{}, notFound(op, "voice revision "+id)
	}
	return p, nil
}

// ListVoiceProfiles lists revision rows for one character (latest revision
// first), or every character when character is empty.
func (s *Store) ListVoiceProfiles(ctx context.Context, character string) ([]VoiceProfile, error) {
	const op = "list voice profiles"
	var (
		where string
		args  []any
	)
	if strings.TrimSpace(character) != "" {
		where = ` WHERE character = ?`
		args = append(args, character)
	}
	q := `SELECT ` + voiceProfileCols + ` FROM voice_profiles` + where +
		` ORDER BY character, revision DESC, id`
	var out []VoiceProfile
	err := s.inRead(ctx, func(ctx context.Context, qq queryable) error {
		rows, err := qq.query(ctx, op, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			p, err := voiceProfileFromRow(rows)
			if err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// NextVoiceRevision returns the number the next revision of a character must
// carry. The insert's UNIQUE constraint is the real guard; this is the
// expected value for the happy path.
func (s *Store) NextVoiceRevision(ctx context.Context, character string) (int, error) {
	const op = "next voice revision"
	var n int
	err := s.inRead(ctx, func(ctx context.Context, q queryable) error {
		row := q.queryRow(ctx, op,
			`SELECT coalesce(max(revision), 0) + 1 FROM voice_profiles WHERE character = ?`,
			character)
		return row.Scan(&n)
	})
	if err != nil {
		return 0, mapReadError(op, err)
	}
	return n, nil
}

// AddAuditions appends audition rows to a draft revision and moves it to
// auditioning, in one transaction. auditionIDs is the FULL list the revision
// carries after this call (a fresh revision's first batch is the only batch),
// so the caller owns the ordering. The revision must be draft: auditions
// belong to exactly one design attempt, and a non-draft revision is
// frozen-by-workflow, which the store answers with not-found.
func (s *Store) AddAuditions(ctx context.Context, voiceProfileID string, auditionIDs []string, auditions []VoiceAudition) error {
	const op = "add voice auditions"
	if len(auditions) == 0 {
		return fmt.Errorf("store: %s: no auditions", op)
	}
	return s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		for _, a := range auditions {
			if !games.ValidID(a.ID) {
				return fmt.Errorf("store: %s: audition id is not a ULID", op)
			}
			if !games.ValidID(a.VoiceProfileID) {
				return fmt.Errorf("store: %s: voice_profile_id is not a ULID", op)
			}
			if strings.TrimSpace(a.SampleText) == "" || strings.TrimSpace(a.AssetSHA256) == "" {
				return fmt.Errorf("store: %s: audition %s needs sample text and asset", op, a.ID)
			}
			n := strings.Count(voiceAuditionCols, ",") + 1
			pcs := strings.TrimRight(strings.Repeat("?,", n), ",")
			_, err := tx.exec(ctx, op,
				"INSERT INTO voice_auditions ("+voiceAuditionCols+") VALUES ("+pcs+")",
				a.ID, a.VoiceProfileID, a.Index, a.Brief, a.ModelID, a.SampleText,
				a.Transcript, a.AssetSHA256, a.DurationMS, a.SampleRate, a.Origin,
				a.RequestID, a.LatencyMS, a.Seed, stampMS(a.CreatedAt),
				a.AsrModel, int(a.AsrState), a.AsrWER, a.AsrCER, a.AsrTranscript,
				a.AsrRequestID, voiceStamp(a.AsrCheckedAt))
			if err != nil {
				return mapWriteError(op, err)
			}
		}
		res, err := tx.exec(ctx, op,
			`UPDATE voice_profiles SET state = ?, audition_ids = ?, updated_at = ?
			 WHERE id = ? AND state = ?`,
			VoiceStateAuditioning, marshalVoiceStringArray(auditionIDs),
			stampMS(time.Now().UTC()), voiceProfileID, VoiceStateDraft)
		if err != nil {
			return mapWriteError(op, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return mapReadError(op, err)
		}
		if n == 0 {
			return notFound(op, "draft voice revision "+voiceProfileID)
		}
		return nil
	})
}

// scanVoiceAudition pulls one audition row from a scanner.
func scanVoiceAudition(op string, row interface {
	Scan(dest ...any) error
}) (VoiceAudition, error) {
	var (
		a          VoiceAudition
		seed       sql.NullInt64
		created    string
		asrState   int
		asrChecked sql.NullString
	)
	if err := row.Scan(&a.ID, &a.VoiceProfileID, &a.Index, &a.Brief, &a.ModelID,
		&a.SampleText, &a.Transcript, &a.AssetSHA256, &a.DurationMS,
		&a.SampleRate, &a.Origin, &a.RequestID, &a.LatencyMS, &seed, &created,
		&a.AsrModel, &asrState, &a.AsrWER, &a.AsrCER, &a.AsrTranscript,
		&a.AsrRequestID, &asrChecked); err != nil {
		return a, err
	}
	if seed.Valid {
		v := int(seed.Int64)
		a.Seed = &v
	}
	var err error
	if a.CreatedAt, err = parseStamp(created); err != nil {
		return a, mapReadError(op, err)
	}
	a.AsrState = VoiceAuditionQAState(asrState)
	if asrChecked.Valid {
		if t, err := parseStamp(asrChecked.String); err == nil {
			a.AsrCheckedAt = &t
		}
	}
	return a, nil
}

// ListAuditions returns the audition rows of one revision, by index.
func (s *Store) ListAuditions(ctx context.Context, voiceProfileID string) ([]VoiceAudition, error) {
	const op = "list voice auditions"
	var out []VoiceAudition
	err := s.inRead(ctx, func(ctx context.Context, q queryable) error {
		rows, err := q.query(ctx, op,
			`SELECT `+voiceAuditionCols+` FROM voice_auditions
			 WHERE voice_profile_id = ? ORDER BY "index"`, voiceProfileID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			a, err := scanVoiceAudition(op, rows)
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

// GetAudition returns one audition row.
func (s *Store) GetAudition(ctx context.Context, id string) (VoiceAudition, error) {
	const op = "get voice audition"
	var a VoiceAudition
	err := s.inRead(ctx, func(ctx context.Context, q queryable) error {
		row := q.queryRow(ctx, op,
			`SELECT `+voiceAuditionCols+` FROM voice_auditions WHERE id = ?`, id)
		var err2 error
		a, err2 = scanVoiceAudition(op, row)
		return err2
	})
	if err != nil && err != sql.ErrNoRows {
		return VoiceAudition{}, mapReadError(op, err)
	}
	if a.ID == "" {
		return VoiceAudition{}, notFound(op, "audition "+id)
	}
	return a, nil
}

// UpsertVoiceRegistration writes one instance's registration state for a
// revision. Reconciliation rewrites these rows on every pass, so there is no
// immutability here — the immutability is in the reference and the name.
func (s *Store) UpsertVoiceRegistration(ctx context.Context, r VoiceRegistration) error {
	const op = "upsert voice registration"
	if !games.ValidID(r.VoiceProfileID) {
		return fmt.Errorf("store: %s: voice_profile_id is not a ULID", op)
	}
	if strings.TrimSpace(r.InstanceID) == "" || strings.TrimSpace(r.RegisteredName) == "" {
		return fmt.Errorf("store: %s: instance and name are required", op)
	}
	if !voiceRegStateOK(r.State) {
		return fmt.Errorf("store: %s: unknown registration state %q", op, r.State)
	}
	return s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		_, err := tx.exec(ctx, op,
			`INSERT INTO voice_registrations (`+voiceRegistrationCols+`)
			 VALUES (?,?,?,?,?,?,?)
			 ON CONFLICT (voice_profile_id, provider_instance_id) DO UPDATE SET
			    registered_name = excluded.registered_name,
			    remote_digest = excluded.remote_digest,
			    last_verified_at = excluded.last_verified_at,
			    state = excluded.state,
			    error = excluded.error`,
			r.VoiceProfileID, r.InstanceID, r.RegisteredName, r.RemoteDigest,
			voiceStamp(r.LastVerifiedAt), r.State, r.Err)
		if err != nil {
			return mapWriteError(op, err)
		}
		return nil
	})
}

func voiceRegStateOK(state string) bool {
	switch state {
	case VoiceRegPending, VoiceRegReady, VoiceRegMissing, VoiceRegInvalid:
		return true
	}
	return false
}

// scanVoiceRegistration pulls one registration row from a scanner.
func scanVoiceRegistration(row interface{ Scan(dest ...any) error }) (VoiceRegistration, error) {
	var (
		r        VoiceRegistration
		verified sql.NullString
	)
	if err := row.Scan(&r.VoiceProfileID, &r.InstanceID, &r.RegisteredName,
		&r.RemoteDigest, &verified, &r.State, &r.Err); err != nil {
		return r, err
	}
	if verified.Valid {
		t, err := parseStamp(verified.String)
		if err != nil {
			return r, err
		}
		r.LastVerifiedAt = &t
	}
	return r, nil
}

// GetVoiceRegistration returns one instance's registration row.
func (s *Store) GetVoiceRegistration(ctx context.Context, voiceProfileID, instanceID string) (VoiceRegistration, error) {
	const op = "get voice registration"
	var r VoiceRegistration
	err := s.inRead(ctx, func(ctx context.Context, q queryable) error {
		row := q.queryRow(ctx, op,
			`SELECT `+voiceRegistrationCols+` FROM voice_registrations
			 WHERE voice_profile_id = ? AND provider_instance_id = ?`,
			voiceProfileID, instanceID)
		var err2 error
		r, err2 = scanVoiceRegistration(row)
		return err2
	})
	if err != nil && err != sql.ErrNoRows {
		return VoiceRegistration{}, mapReadError(op, err)
	}
	if r.VoiceProfileID == "" {
		return VoiceRegistration{}, notFound(op, "registration "+voiceProfileID+"/"+instanceID)
	}
	return r, nil
}

// ListVoiceRegistrations returns every instance row for one revision.
func (s *Store) ListVoiceRegistrations(ctx context.Context, voiceProfileID string) ([]VoiceRegistration, error) {
	const op = "list voice registrations"
	var out []VoiceRegistration
	err := s.inRead(ctx, func(ctx context.Context, q queryable) error {
		rows, err := q.query(ctx, op,
			`SELECT `+voiceRegistrationCols+` FROM voice_registrations
			 WHERE voice_profile_id = ? ORDER BY provider_instance_id`, voiceProfileID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanVoiceRegistration(rows)
			if err != nil {
				return mapReadError(op, err)
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListApprovedVoiceRevisions lists the approved revisions that carry a
// reference: the set reconciliation must keep registered (docs/spec/06
// "VoiceRegistryManager"). Retired and reference-less rows are not part of
// the required set.
func (s *Store) ListApprovedVoiceRevisions(ctx context.Context) ([]VoiceProfile, error) {
	const op = "list approved voice revisions"
	var out []VoiceProfile
	err := s.inRead(ctx, func(ctx context.Context, q queryable) error {
		rows, err := q.query(ctx, op,
			`SELECT `+voiceProfileCols+` FROM voice_profiles
			 WHERE state = ? AND canonical_reference_asset_id != ''
			 ORDER BY character, revision`, VoiceStateApproved)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			p, err := voiceProfileFromRow(rows)
			if err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
