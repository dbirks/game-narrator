package store

// Persistent TTS speech-cache index (migration 0011, ADR-0039 amendment,
// docs/spec/07 "Caching"). The cache's WAV bytes are content-addressed blobs
// in the asset store; this table is the key→metadata index in front of them.
// Until now the production index was speech.MemIndex, so every restart lost
// the index and re-synthesised the whole corpus (AGENTS.md restart tests:
// persistent games/voices/cache). SpeechCacheIndex implements speech.Index
// over the speech_cache table so a re-opened process LoadAlls the previous
// run's entries and hits the asset-backed WAVs instead of calling the
// provider again.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/dbirks/game-narrator/internal/speech"
)

// SpeechCacheIndex is the store-backed speech.Index. All methods are safe for
// concurrent use; LoadAll follows the interface's load-once contract (the
// first call reads the table, later calls return nil plus the sticky error),
// mirroring speech.MemIndex.
type SpeechCacheIndex struct {
	st *Store

	mu     sync.Mutex
	loaded bool
	err    error // sticky load error, surfaced by later LoadAll calls
}

// NewSpeechCacheIndex returns the store-backed index over st's speech_cache
// table. The store must already be migrated (migration 0011 creates the
// table); opening one that has not is a wiring error, surfaced on first use
// like any other schema drift.
func NewSpeechCacheIndex(st *Store) *SpeechCacheIndex {
	return &SpeechCacheIndex{st: st}
}

// Pin the satisfaction at compile time: the interface lives in internal/speech
// and the implementation here, so nothing else compiles against a drifted
// method set.
var _ speech.Index = (*SpeechCacheIndex)(nil)

// LoadAll returns the persisted index contents exactly once; later calls
// return nil (the cache must use Lookup for per-key reads, per the
// interface contract).
func (i *SpeechCacheIndex) LoadAll(ctx context.Context) (map[speech.CacheKey]speech.Entry, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.loaded {
		return nil, i.err
	}
	i.loaded = true
	out, err := i.loadLocked(ctx)
	i.err = err
	return out, err
}

// Lookup returns the entry for key, if any. The interface takes no context
// and no error (the in-memory default needs neither), so a read failure
// degrades to a miss: the cache's generation path then re-synthesises and
// re-records the entry, which retries the read on the next write. It is a
// safe degradation, never a stale hit, and a persistent database fault still
// surfaces loudly on LoadAll (open time) and on the next Put.
func (i *SpeechCacheIndex) Lookup(key speech.CacheKey) (speech.Entry, bool) {
	const op = "lookup speech cache entry"
	ctx := context.Background()
	var e speech.Entry
	err := i.st.inRead(ctx, func(_ context.Context, q queryable) error {
		row := q.queryRow(ctx, op,
			`SELECT sha256, size_bytes, duration_ns, sample_rate, qa_state,
			        qa_wer, qa_cer, qa_transcript, qa_model, qa_checked_at, original_message_id
			   FROM speech_cache WHERE key = ?`, string(key))
		return scanSpeechCacheEntry(op, row, &e)
	})
	if err != nil {
		return speech.Entry{}, false
	}
	return e, true
}

// Put upserts one entry: created_at is set on insert and preserved on
// conflict, accessed_at is bumped on every write (the eviction order).
func (i *SpeechCacheIndex) Put(key speech.CacheKey, e speech.Entry) error {
	const op = "upsert speech cache entry"
	return i.st.inWrite(context.Background(), func(_ context.Context, tx *Tx) error {
		_, err := tx.exec(context.Background(), op,
			`INSERT INTO speech_cache (key, sha256, size_bytes, duration_ns, sample_rate, qa_state,
			   qa_wer, qa_cer, qa_transcript, qa_model, qa_checked_at,
			   key_version, original_message_id, created_at, accessed_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT (key) DO UPDATE SET
			     sha256        = excluded.sha256,
			     size_bytes    = excluded.size_bytes,
			     duration_ns   = excluded.duration_ns,
			     sample_rate   = excluded.sample_rate,
			     qa_state      = excluded.qa_state,
			     qa_wer        = excluded.qa_wer,
			     qa_cer        = excluded.qa_cer,
			     qa_transcript = excluded.qa_transcript,
			     qa_model      = excluded.qa_model,
			     qa_checked_at = excluded.qa_checked_at,
			     key_version   = excluded.key_version,
			     accessed_at   = excluded.accessed_at`,
			string(key), e.SHA256, e.Size, e.Duration, e.SampleRate, int(e.QA),
			e.QAWER, e.QACER, e.QATranscript, e.QAModel, e.QACheckedAt,
			speech.KeyVersion(), nullStr(e.OriginalMessageID), stampMS(time.Now().UTC()), stampMS(time.Now().UTC()))
		if err != nil {
			return mapWriteError(op, err)
		}
		return nil
	})
}

// Delete removes one entry (the cache calls it when a QA state is withdrawn
// or an eviction drops the row). A missing key is a no-op, matching
// speech.MemIndex.
func (i *SpeechCacheIndex) Delete(key speech.CacheKey) error {
	const op = "delete speech cache entry"
	return i.st.inWrite(context.Background(), func(_ context.Context, tx *Tx) error {
		if _, err := tx.exec(context.Background(), op,
			`DELETE FROM speech_cache WHERE key = ?`, string(key)); err != nil {
			return mapWriteError(op, err)
		}
		return nil
	})
}

// loadLocked reads the whole table once. The corpus index is bounded by the
// number of distinct utterances ever generated (hundreds to low thousands on
// the target appliance), so a full SELECT is the honest shape; the hot path
// is Lookup, which is a point read on the primary key.
func (i *SpeechCacheIndex) loadLocked(ctx context.Context) (map[speech.CacheKey]speech.Entry, error) {
	const op = "load speech cache index"
	out := make(map[speech.CacheKey]speech.Entry)
	err := i.st.inRead(ctx, func(_ context.Context, q queryable) error {
		rows, err := q.query(ctx, op,
			`SELECT key, sha256, size_bytes, duration_ns, sample_rate, qa_state,
			        qa_wer, qa_cer, qa_transcript, qa_model, qa_checked_at, original_message_id
			   FROM speech_cache`)
		if err != nil {
			return mapReadError(op, err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				e         speech.Entry
				key       string
				qaState   int
				checkedAt sql.NullString
				origMsg   sql.NullString
			)
			if err := rows.Scan(&key, &e.SHA256, &e.Size, &e.Duration, &e.SampleRate, &qaState,
				&e.QAWER, &e.QACER, &e.QATranscript, &e.QAModel, &checkedAt, &origMsg); err != nil {
				return mapReadError(op, fmt.Errorf("scan: %w", err))
			}
			e.QA = speech.QAState(qaState)
			if checkedAt.Valid {
				e.QACheckedAt = checkedAt.String
			}
			if origMsg.Valid {
				e.OriginalMessageID = origMsg.String
			}
			out[speech.CacheKey(key)] = e
		}
		return mapReadError(op, rows.Err())
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// scanSpeechCacheEntry pulls one entry row; sql.ErrNoRows passes through so
// Lookup can report a clean miss.
func scanSpeechCacheEntry(op string, row interface {
	Scan(dest ...any) error
}, e *speech.Entry) error {
	var (
		qaState   int
		checkedAt sql.NullString
		origMsg   sql.NullString
	)
	if err := row.Scan(&e.SHA256, &e.Size, &e.Duration, &e.SampleRate, &qaState,
		&e.QAWER, &e.QACER, &e.QATranscript, &e.QAModel, &checkedAt, &origMsg); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return err
		}
		return mapReadError(op, fmt.Errorf("scan: %w", err))
	}
	e.QA = speech.QAState(qaState)
	if checkedAt.Valid {
		e.QACheckedAt = checkedAt.String
	}
	if origMsg.Valid {
		e.OriginalMessageID = origMsg.String
	}
	return nil
}
