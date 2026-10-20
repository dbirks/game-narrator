package store

// The assistant thread log over migration 0016 (ADR-0099): the per-user
// ownership mapping to the thread's durable run and the question/confirmation
// rows the run's cards need. The conversation itself lives in the agent run
// log (migration 0007); this file owns exactly the two tables that live
// outside it.
//
// Two invariants are load-bearing and live here:
//
//  1. Ownership. A thread belongs to one user; the API's 404 for a foreign
//     thread is this table's user_id, so a run id guessed by another user's
//     browser is not an information leak.
//  2. The follow-up predicate (UX-04, ADR-0099). An answer is "applied"
//     exactly when its durable user message is in the run's conversation:
//     AnswerQuestion records the answer AND the appended message's seq in
//     one write, and the job handler compares answer_seq against the
//     attempt's start seq to decide whether one follow-up attempt is owed.
//     The schema CHECK makes the open/answered pair closed, so the predicate
//     is enforced by the table, not by caller discipline.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Question kinds: the model's free-form question and a mutating tool's
// confirmation card (ADR-0099 decision 4/5).
const (
	AssistantQuestionKindQuestion     = "question"
	AssistantQuestionKindConfirmation = "confirmation"
)

// AssistantThread is one thread row: the ownership mapping to the run.
type AssistantThread struct {
	ID        string
	UserID    string
	RunID     string
	Title     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// AssistantQuestion is one NEEDS-YOU card or confirmation. AnswerSeq is -1
// while the answer's durable message has not been appended; Applied is 1
// exactly when it has (schema CHECK: answered rows carry both).
type AssistantQuestion struct {
	ID         string
	ThreadID   string
	RunID      string
	Kind       string
	Prompt     string
	Options    []string
	Answer     string
	AnswerSeq  int64
	ContextRaw []byte
	Applied    bool
	CreatedAt  time.Time
	AnsweredAt *time.Time
}

// ThreadStore provides the assistant thread operations.
type ThreadStore struct {
	s *Store
}

// NewThreadStore builds the thread store over an open store.
func NewThreadStore(s *Store) *ThreadStore { return &ThreadStore{s: s} }

// CreateThread inserts a new thread. The caller supplies the ids (ULIDs);
// runID must be a run row the caller created or is creating in the same
// transaction (the FK is checked at the statement, the writer owns the
// ordering — the API creates the run row first, in its own transaction, and
// the thread row's FK then passes).
func (t *ThreadStore) CreateThread(ctx context.Context, th AssistantThread) error {
	const op = "create assistant thread"
	if strings.TrimSpace(th.ID) == "" {
		return fmt.Errorf("store: %s: id is required", op)
	}
	if strings.TrimSpace(th.UserID) == "" {
		return fmt.Errorf("store: %s: user_id is required", op)
	}
	if strings.TrimSpace(th.RunID) == "" {
		return fmt.Errorf("store: %s: run_id is required", op)
	}
	if len(th.Title) < 1 || len(th.Title) > 200 {
		return fmt.Errorf("store: %s: title must be 1..200 chars", op)
	}
	return t.s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		_, err := tx.exec(ctx, op,
			`INSERT INTO assistant_threads (id, user_id, run_id, title, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			th.ID, th.UserID, th.RunID, th.Title, nowStamp(), nowStamp())
		if err != nil {
			return mapWriteError(op, err)
		}
		return nil
	})
}

// GetThread loads one thread.
func (t *ThreadStore) GetThread(ctx context.Context, id string) (AssistantThread, error) {
	const op = "get assistant thread"
	var th AssistantThread
	var created, updated string
	err := t.s.inRead(ctx, func(ctx context.Context, q queryable) error {
		return q.queryRow(ctx, op,
			`SELECT id, user_id, run_id, title, created_at, updated_at
			 FROM assistant_threads WHERE id = ?`, id,
		).Scan(&th.ID, &th.UserID, &th.RunID, &th.Title, &created, &updated)
	})
	if err != nil {
		if err == sql.ErrNoRows {
			return AssistantThread{}, notFound(op, "thread "+id)
		}
		return AssistantThread{}, mapReadError(op, err)
	}
	th.CreatedAt, err = parseStamp(created)
	if err != nil {
		return AssistantThread{}, mapReadError(op, err)
	}
	th.UpdatedAt, err = parseStamp(updated)
	if err != nil {
		return AssistantThread{}, mapReadError(op, err)
	}
	return th, nil
}

// GetThreadByRunID loads the thread that owns a run (the run id is UNIQUE on
// the thread table, so at most one).
func (t *ThreadStore) GetThreadByRunID(ctx context.Context, runID string) (AssistantThread, error) {
	const op = "get assistant thread by run"
	var th AssistantThread
	var created, updated string
	err := t.s.inRead(ctx, func(ctx context.Context, q queryable) error {
		return q.queryRow(ctx, op,
			`SELECT id, user_id, run_id, title, created_at, updated_at
			 FROM assistant_threads WHERE run_id = ?`, runID,
		).Scan(&th.ID, &th.UserID, &th.RunID, &th.Title, &created, &updated)
	})
	if err != nil {
		if err == sql.ErrNoRows {
			return AssistantThread{}, notFound(op, "thread for run "+runID)
		}
		return AssistantThread{}, mapReadError(op, err)
	}
	th.CreatedAt, err = parseStamp(created)
	if err != nil {
		return AssistantThread{}, mapReadError(op, err)
	}
	th.UpdatedAt, err = parseStamp(updated)
	if err != nil {
		return AssistantThread{}, mapReadError(op, err)
	}
	return th, nil
}

// ListThreads returns the user's threads, newest activity first, cursor
// paginated on (updated_at, id) — the keyset shape the catalog lists use
// (docs/spec/12's pagination property).
func (t *ThreadStore) ListThreads(ctx context.Context, userID string, cursor string, limit int) ([]AssistantThread, string, error) {
	const op = "list assistant threads"
	limit = clampLimit(limit, 100)
	var out []AssistantThread
	err := t.s.inRead(ctx, func(ctx context.Context, q queryable) error {
		var rows *sql.Rows
		var err error
		if cursor == "" {
			rows, err = q.query(ctx, op,
				`SELECT id, user_id, run_id, title, created_at, updated_at
				 FROM assistant_threads WHERE user_id = ?
				 ORDER BY updated_at DESC, id DESC LIMIT ?`, userID, limit+1)
		} else {
			var stamp, idAt string
			parts := strings.SplitN(cursor, "|", 2)
			if len(parts) != 2 {
				return fmt.Errorf("store: %s: cursor is not a valid (updated_at|id) pair", op)
			}
			stamp, idAt = parts[0], parts[1]
			rows, err = q.query(ctx, op,
				`SELECT id, user_id, run_id, title, created_at, updated_at
				 FROM assistant_threads WHERE user_id = ?
				 AND (updated_at < ? OR (updated_at = ? AND id < ?))
				 ORDER BY updated_at DESC, id DESC LIMIT ?`,
				userID, stamp, stamp, idAt, limit+1)
		}
		if err != nil {
			return mapReadError(op, err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				th      AssistantThread
				created string
				updated string
			)
			if err := rows.Scan(&th.ID, &th.UserID, &th.RunID, &th.Title, &created, &updated); err != nil {
				return mapReadError(op, err)
			}
			th.CreatedAt, err = parseStamp(created)
			if err != nil {
				return mapReadError(op, err)
			}
			th.UpdatedAt, err = parseStamp(updated)
			if err != nil {
				return mapReadError(op, err)
			}
			out = append(out, th)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, "", err
	}
	if len(out) > limit {
		next := out[limit-1]
		return out[:limit], next.UpdatedAt.UTC().Format(time.RFC3339) + "|" + next.ID, nil
	}
	return out, "", nil
}

// TouchThread moves the thread's updated_at to now (a new message landed).
func (t *ThreadStore) TouchThread(ctx context.Context, id string) error {
	const op = "touch assistant thread"
	return t.s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		res, err := tx.exec(ctx, op,
			`UPDATE assistant_threads SET updated_at = ? WHERE id = ?`, nowStamp(), id)
		if err != nil {
			return mapWriteError(op, err)
		}
		return touchedOrConflict(ctx, tx, op, res, id)
	})
}

// ── questions ────────────────────────────────────────────────────────────────

// QuestionStore provides the assistant question operations.
type QuestionStore struct {
	s *Store
}

// NewQuestionStore builds the question store over an open store.
func NewQuestionStore(s *Store) *QuestionStore { return &QuestionStore{s: s} }

func validQuestionKind(k string) bool {
	return k == AssistantQuestionKindQuestion || k == AssistantQuestionKindConfirmation
}

// CreateQuestion inserts one open card. Options are stored as a JSON array of
// strings; ContextRaw is the captured screen context / staged action (valid
// JSON or nil). The kind, prompt and run/thread binding are validated here —
// the run/thread match is the writer's word, the schema's CHECKs enforce the
// rest.
func (q *QuestionStore) CreateQuestion(ctx context.Context, row AssistantQuestion) error {
	const op = "create assistant question"
	if strings.TrimSpace(row.ID) == "" {
		return fmt.Errorf("store: %s: id is required", op)
	}
	if strings.TrimSpace(row.ThreadID) == "" || strings.TrimSpace(row.RunID) == "" {
		return fmt.Errorf("store: %s: thread_id and run_id are required", op)
	}
	if !validQuestionKind(row.Kind) {
		return fmt.Errorf("store: %s: kind %q is not question|confirmation", op, row.Kind)
	}
	if len(row.Prompt) < 1 || len(row.Prompt) > 2000 {
		return fmt.Errorf("store: %s: prompt must be 1..2000 chars", op)
	}
	options := "[]"
	if len(row.Options) > 0 {
		raw, err := json.Marshal(row.Options)
		if err != nil {
			return fmt.Errorf("store: %s: options: %w", op, err)
		}
		options = string(raw)
	}
	var ctxJSON any
	if len(row.ContextRaw) > 0 {
		if !json.Valid(row.ContextRaw) {
			return fmt.Errorf("store: %s: context_json must be valid JSON", op)
		}
		ctxJSON = string(row.ContextRaw)
	}
	return q.s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		_, err := tx.exec(ctx, op,
			`INSERT INTO assistant_questions (id, thread_id, run_id, kind, prompt,
				options_json, answer, answer_seq, context_json, applied,
				created_at, answered_at)
			 VALUES (?, ?, ?, ?, ?, ?, NULL, -1, ?, 0, ?, NULL)`,
			row.ID, row.ThreadID, row.RunID, row.Kind, row.Prompt, options, ctxJSON, nowStamp())
		if err != nil {
			return mapWriteError(op, err)
		}
		return nil
	})
}

// GetQuestion loads one card.
func (q *QuestionStore) GetQuestion(ctx context.Context, id string) (AssistantQuestion, error) {
	const op = "get assistant question"
	var row AssistantQuestion
	var options, created string
	var ctxRaw sql.NullString
	var answer sql.NullString
	var answered sql.NullString
	var seq int64
	err := q.s.inRead(ctx, func(ctx context.Context, q2 queryable) error {
		return q2.queryRow(ctx, op,
			`SELECT id, thread_id, run_id, kind, prompt, options_json, answer,
				answer_seq, context_json, applied, created_at, answered_at
			 FROM assistant_questions WHERE id = ?`, id,
		).Scan(&row.ID, &row.ThreadID, &row.RunID, &row.Kind, &row.Prompt,
			&options, &answer, &seq, &ctxRaw, &row.Applied, &created, &answered)
	})
	if err != nil {
		if err == sql.ErrNoRows {
			return AssistantQuestion{}, notFound(op, "question "+id)
		}
		return AssistantQuestion{}, mapReadError(op, err)
	}
	if err := json.Unmarshal([]byte(options), &row.Options); err != nil {
		return AssistantQuestion{}, mapReadError(op, fmt.Errorf("%s: options: %w", op, err))
	}
	if ctxRaw.Valid && ctxRaw.String != "" {
		row.ContextRaw = []byte(ctxRaw.String)
	}
	if answer.Valid {
		row.Answer = answer.String
	}
	if answered.Valid {
		t, err := parseStamp(answered.String)
		if err != nil {
			return AssistantQuestion{}, mapReadError(op, err)
		}
		row.AnsweredAt = &t
	}
	row.AnswerSeq = seq
	row.CreatedAt, err = parseStamp(created)
	if err != nil {
		return AssistantQuestion{}, mapReadError(op, err)
	}
	return row, nil
}

// AnswerQuestion records the user's answer on an open card AND the seq of the
// durable user message that carries it (answerSeq, from
// AgentStore.AppendAgentMessage). One guarded UPDATE: the WHERE clause
// refuses an already-answered card (the 409 the API surfaces), so two
// racing answers cannot both land.
func (q *QuestionStore) AnswerQuestion(ctx context.Context, id, answer string, answerSeq int64) error {
	const op = "answer assistant question"
	if answerSeq < 0 {
		return fmt.Errorf("store: %s: answer_seq must be the appended message's seq (>= 0)", op)
	}
	return q.s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		res, err := tx.exec(ctx, op,
			`UPDATE assistant_questions
			 SET answer = ?, answer_seq = ?, applied = 1, answered_at = ?
			 WHERE id = ? AND answer IS NULL`,
			answer, answerSeq, nowStamp(), id)
		if err != nil {
			return mapWriteError(op, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return mapWriteError(op, err)
		}
		if n > 0 {
			return nil
		}
		// The guard did not fire: the row either does not exist or is already
		// answered. Tell the caller which (the re-answer is a conflict the API
		// answers 409 with; the unknown id is a 404).
		var known int
		if err := tx.queryRow(ctx, op,
			`SELECT 1 FROM assistant_questions WHERE id = ?`, id,
		).Scan(&known); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return mapReadError(op, err)
			}
			return mapWriteError(op, err)
		}
		return fmt.Errorf("store: %s: question %s is already answered: %w", op, id, ErrConflict)
	})
}

// ListQuestions returns the thread's cards newest-first, open ones first (an
// open card is the dock's NEEDS-YOU row; answered cards follow, for the
// history). Bounded: the dock shows a screen, not the table.
func (q *QuestionStore) ListQuestions(ctx context.Context, threadID string, limit int) ([]AssistantQuestion, error) {
	const op = "list assistant questions"
	limit = clampLimit(limit, 100)
	var out []AssistantQuestion
	err := q.s.inRead(ctx, func(ctx context.Context, q2 queryable) error {
		rows, err := q2.query(ctx, op,
			`SELECT id, thread_id, run_id, kind, prompt, options_json, answer,
				answer_seq, context_json, applied, created_at, answered_at
			 FROM assistant_questions WHERE thread_id = ?
			 ORDER BY (answer IS NOT NULL) ASC, created_at DESC
			 LIMIT ?`, threadID, limit)
		if err != nil {
			return mapReadError(op, err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				row      AssistantQuestion
				options  string
				ctxRaw   sql.NullString
				created  string
				answer   sql.NullString
				answered sql.NullString
				seq      int64
			)
			if err := rows.Scan(&row.ID, &row.ThreadID, &row.RunID, &row.Kind, &row.Prompt,
				&options, &answer, &seq, &ctxRaw, &row.Applied, &created, &answered); err != nil {
				return mapReadError(op, err)
			}
			if err := json.Unmarshal([]byte(options), &row.Options); err != nil {
				return mapReadError(op, fmt.Errorf("%s: options: %w", op, err))
			}
			if ctxRaw.Valid && ctxRaw.String != "" {
				row.ContextRaw = []byte(ctxRaw.String)
			}
			if answer.Valid {
				row.Answer = answer.String
			}
			if answered.Valid {
				t, err := parseStamp(answered.String)
				if err != nil {
					return mapReadError(op, err)
				}
				row.AnsweredAt = &t
			}
			row.AnswerSeq = seq
			row.CreatedAt, err = parseStamp(created)
			if err != nil {
				return mapReadError(op, err)
			}
			out = append(out, row)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// UnseenAnsweredQuestions returns the thread's answered cards whose durable
// message (answer_seq) is NEWER than the given seq — the post-run follow-up
// predicate (ADR-0099 decision 4): the attempt that started at startSeq did
// not see these answers, so one follow-up attempt is owed.
func (q *QuestionStore) UnseenAnsweredQuestions(ctx context.Context, threadID string, startSeq int64) ([]AssistantQuestion, error) {
	const op = "list unseen answered assistant questions"
	var out []AssistantQuestion
	err := q.s.inRead(ctx, func(ctx context.Context, q2 queryable) error {
		rows, err := q2.query(ctx, op,
			`SELECT id, thread_id, run_id, kind, prompt, options_json, answer,
				answer_seq, context_json, applied, created_at, answered_at
			 FROM assistant_questions
			 WHERE thread_id = ? AND applied = 1 AND answer_seq > ?
			 ORDER BY answer_seq ASC`, threadID, startSeq)
		if err != nil {
			return mapReadError(op, err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				row      AssistantQuestion
				options  string
				ctxRaw   sql.NullString
				created  string
				answer   sql.NullString
				answered sql.NullString
				seq      int64
			)
			if err := rows.Scan(&row.ID, &row.ThreadID, &row.RunID, &row.Kind, &row.Prompt,
				&options, &answer, &seq, &ctxRaw, &row.Applied, &created, &answered); err != nil {
				return mapReadError(op, err)
			}
			if err := json.Unmarshal([]byte(options), &row.Options); err != nil {
				return mapReadError(op, fmt.Errorf("%s: options: %w", op, err))
			}
			if ctxRaw.Valid && ctxRaw.String != "" {
				row.ContextRaw = []byte(ctxRaw.String)
			}
			if answer.Valid {
				row.Answer = answer.String
			}
			if answered.Valid {
				t, err := parseStamp(answered.String)
				if err != nil {
					return mapReadError(op, err)
				}
				row.AnsweredAt = &t
			}
			row.AnswerSeq = seq
			row.CreatedAt, err = parseStamp(created)
			if err != nil {
				return mapReadError(op, err)
			}
			out = append(out, row)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// MaxAgentMessageSeq is the last seq of a run's conversation, or -1 for a run
// with no messages yet: the attempt's start-seq marker (ADR-0099 decision 4).
func (s *Store) MaxAgentMessageSeq(ctx context.Context, runID string) (int64, error) {
	const op = "max agent message seq"
	var seq int64
	err := s.inRead(ctx, func(ctx context.Context, q queryable) error {
		return q.queryRow(ctx, op,
			`SELECT COALESCE(MAX(seq), -1) FROM agent_messages WHERE run_id = ?`, runID,
		).Scan(&seq)
	})
	if err != nil {
		return -1, mapReadError(op, err)
	}
	return seq, nil
}

// ── per-user assistant settings ────────────────────────────────────────────

// assistantSettingsKey is the app_settings key one user's assistant settings
// live under (the key/value home ADR-0074's audio settings established for
// runtime-mutable values; the user id in the key is what makes them per-user).
func assistantSettingsKey(userID string) string { return "assistant." + userID }

// GetAssistantConfirmMutations reads the user's confirm-mutations setting.
// ok is true when a row exists; when no row exists the caller applies the
// default (true: confirm by default). The value is stored as a JSON object
// {"confirm_mutations": bool} so later per-user assistant settings grow into
// the same row without a second key.
func (s *Store) GetAssistantConfirmMutations(ctx context.Context, userID string) (bool, bool, error) {
	const op = "get assistant settings"
	var value string
	err := s.inRead(ctx, func(ctx context.Context, q queryable) error {
		return mapReadError(op, q.queryRow(ctx, op,
			`SELECT value FROM app_settings WHERE key = ?`,
			assistantSettingsKey(userID)).Scan(&value))
	})
	if err != nil {
		if err := errorIsNotFound(err); err != nil {
			return true, false, nil
		}
		return true, false, err
	}
	var row struct {
		ConfirmMutations *bool `json:"confirm_mutations"`
	}
	if err := json.Unmarshal([]byte(value), &row); err != nil {
		return true, false, mapReadError(op, fmt.Errorf("%s: stored value does not decode: %w", op, err))
	}
	if row.ConfirmMutations == nil {
		return true, true, nil
	}
	return *row.ConfirmMutations, true, nil
}

// SetAssistantConfirmMutations writes (or updates) the user's setting.
func (s *Store) SetAssistantConfirmMutations(ctx context.Context, userID string, v bool) error {
	const op = "set assistant settings"
	value, err := json.Marshal(struct {
		ConfirmMutations bool `json:"confirm_mutations"`
	}{v})
	if err != nil {
		return fmt.Errorf("store: %s: %w", op, err)
	}
	return s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		_, err := tx.exec(ctx, op,
			`INSERT INTO app_settings (key, value, updated_at) VALUES (?, ?, ?)
			 ON CONFLICT (key) DO UPDATE SET value = excluded.value,
			 updated_at = excluded.updated_at`,
			assistantSettingsKey(userID), string(value), nowStamp())
		if err != nil {
			return mapWriteError(op, err)
		}
		return nil
	})
}
