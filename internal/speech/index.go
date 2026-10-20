package speech

import (
	"context"
	"sync"
)

// QAState is the quality state of one cached utterance, as produced by the
// ASR check loop (docs/spec/07 "ASR loop": CER/WR metrics, pronunciation and
// silence checks; docs/spec/12: "ASR transcript diverges: retry bounded
// times, retain diagnostic evidence, mark review").
type QAState int

const (
	// QAUnknown: entry exists but has not been (or could not be) ASR-checked.
	QAUnknown QAState = iota
	// QAPassed: the ASR check accepted the audio against the expected text.
	QAPassed
	// QAFailed: the ASR check rejected the audio. Entries in this state are
	// skipped by default unless an explicit user override is given
	// (docs/spec/07 "Skip QC-failed cache entries by default unless explicit
	// user override").
	QAFailed
)

// String names the state in logs and event data.
func (s QAState) String() string {
	switch s {
	case QAPassed:
		return "pass"
	case QAFailed:
		return "fail"
	default:
		return "unknown"
	}
}

// Entry is the cache index record for one key. It deliberately holds only
// metadata: the bytes live in the asset store, addressed by SHA256.
// Duration and SampleRate are recorded at commit time from the validated WAV
// so a player can reserve time and a QA loop can flag duration outliers
// without re-reading the blob (docs/spec/07 "ASR loop": duration outliers and
// silent audio are QA signals). Size is the committed blob's byte count, so a
// store-backed index (ADR-0039 amendment) can persist and report cache weight
// without reading the blob.
type Entry struct {
	SHA256     string  `json:"sha256"`
	Size       int64   `json:"size_bytes"`
	Duration   int64   `json:"duration_ns"`
	SampleRate int     `json:"sample_rate"`
	QA         QAState `json:"qa_state"`
	// QAWER/QACER are the ASR verdict measured on normalized text (0..1+);
	// QATranscript is the recognised text verbatim; QAModel the ASR model id.
	// Empty/zero when the check did not run (ADR 0061). Persisted by the
	// store-backed index (migration 0012) so a flagged line survives a
	// restart with its evidence.
	QAWER        float64 `json:"qa_wer"`
	QACER        float64 `json:"qa_cer"`
	QATranscript string  `json:"qa_transcript"`
	QAModel      string  `json:"qa_model"`
	QACheckedAt  string  `json:"qa_checked_at"`
	// OriginalMessageID is the id of the message this entry was synthesised
	// for (migration 0017, ADR-0107). The cache key is a hash of its input,
	// so the id is not in the key; the caller that narrates the message
	// carries it through the context (WithKeyInput) and the cache records it
	// here. Empty for rows written before 0017 and for voice-previews and
	// auditions, which belong to no message and age out through eviction.
	// The store-backed index persists it so a profile purge can address
	// "the cache rows of this profile's messages".
	OriginalMessageID string `json:"original_message_id,omitempty"`
	// FromCache is filled by lookups, not persisted.
	FromCache bool `json:"-"`
}

// Index is the persistence hook for the in-memory key→Entry map. The default
// is MemIndex (docs/spec/02 keeps the runtime cache in the local store; a
// SQLite-backed index implementing this interface plugs in without changing
// callers). All methods are safe for concurrent use.
//
// LoadAll is called once at open time and must return the full index of a
// previous run, so a restart reuses the asset-backed WAVs instead of
// re-synthesizing (docs/spec/07 voice lifecycle step 7 + AGENTS.md restart
// tests: persistent cache across restarts).
type Index interface {
	// LoadAll returns the persisted index contents (nil/empty on first run)
	// exactly once per process lifetime (open time); later calls may return
	// nil. Callers must use Lookup for per-key reads.
	LoadAll(ctx context.Context) (map[CacheKey]Entry, error)
	// Lookup returns the current entry for key, if any.
	Lookup(key CacheKey) (Entry, bool)
	// Put upserts one entry.
	Put(key CacheKey, e Entry) error
	// Delete removes one entry (used when a QA state is withdrawn).
	Delete(key CacheKey) error
}

// MemIndex is the in-memory Index. It is the test and single-process
// default; it satisfies the interface so the cache never touches a concrete
// persistence type (the cache stays restart-testable by feeding it a fresh
// MemIndex preloaded from a previous MemIndex).
type MemIndex struct {
	mu     sync.RWMutex
	data   map[CacheKey]Entry
	loaded bool
	err    error // sticky load error, surfaced by Load
}

// NewMemIndex returns an empty in-memory index.
func NewMemIndex() *MemIndex {
	return &MemIndex{data: map[CacheKey]Entry{}}
}

// LoadAll loads the given snapshot (typically from a previous run) exactly
// once. Subsequent LoadAll calls are no-ops, mirroring "load once at open".
func (m *MemIndex) LoadAll(_ context.Context) (map[CacheKey]Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.loaded {
		return nil, m.err
	}
	m.loaded = true
	return m.data, m.err
}

// Put upserts key.
func (m *MemIndex) Put(key CacheKey, e Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = e
	return nil
}

// Delete removes key.
func (m *MemIndex) Delete(key CacheKey) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, key)
	return nil
}

// Snapshot returns a copy of the current map (for persistence backends and
// for restart tests that hand a previous run's snapshot to a new process).
func (m *MemIndex) Snapshot() map[CacheKey]Entry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[CacheKey]Entry, len(m.data))
	for k, v := range m.data {
		out[k] = v
	}
	return out
}

// Lookup returns the entry for key, if any.
func (m *MemIndex) Lookup(key CacheKey) (Entry, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.data[key]
	return e, ok
}

// Len reports the number of indexed keys (diagnostics).
func (m *MemIndex) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.data)
}
