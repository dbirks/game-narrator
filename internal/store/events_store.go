package store

// The durable SQLite side of the event replay log (migration 0003's `events`
// table) — the `events.Store` the hub uses so a browser reconnecting with a
// Last-Event-ID can be replayed past the in-memory ring (docs/spec/08 "Event
// stream reliability"), and so the developer inspector can show the same
// correlated events after the fact.
//
// Append is deliberately a plain INSERT: seq is assigned by SQLite's
// AUTOINCREMENT, which is the single source of global order across every
// writer of this database — asking the caller for a seq would put two writers
// in charge of one counter (the events.Store contract and migration 0003's
// AUTOINCREMENT comment both say so).

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
)

// eventStamp is the fixed-width UTC shape for occurred_at. Same width and
// always 'Z' keeps SQLite's text comparison on occurred_at (used by
// PruneEvents) chronological, satisfies the column's strftime CHECK, and
// matches the envelope wire format (events' timeFormat), so the durable row,
// the JSON a browser saw and the strftime index all agree.
const eventStamp = "2006-01-02T15:04:05.000Z"

// maxEventStoreLimit bounds one After page, matching events.DefaultStoreLimit
// so the hub's paged replay and a direct reader agree on what "the default
// page" costs.
const maxEventStoreLimit = 1000

// EventStore is the events.Store implementation over the `events` table. It
// holds no state of its own; the *Store's bounded connection pool is the only
// concurrency control, and Append runs on one pooled connection because a
// single-row INSERT is already atomic — taking the whole write lock through
// InTx for one append would serialize the log against unrelated writers for no
// additional safety.
type EventStore struct {
	s *Store
}

// NewEventStore wires the durable log onto an open, migrated store.
func NewEventStore(s *Store) *EventStore { return &EventStore{s: s} }

// compile-time proof that the durable log satisfies the hub's contract.
var _ events.Store = (*EventStore)(nil)

// Append validates the envelope (the store is the one gate a direct caller
// cannot skip), inserts the row and returns the AUTOINCREMENT seq. On failure
// nothing is written, so a rejected envelope leaves no hole in the log.
//
// When ctx carries an open Tx the append joins it — a caller publishing an
// event as part of a unit of work then gets all-or-nothing semantics for free
// — and otherwise it runs on one pooled connection, because a single-row
// INSERT is already atomic and taking the write lock through InTx for one
// append would serialize the log against unrelated writers for no extra
// safety.
func (e *EventStore) Append(ctx context.Context, ev events.Envelope) (int64, error) {
	// The caller's Seq is ignored — SQLite's AUTOINCREMENT owns numbering —
	// so defaults are applied here exactly as MemStore does (events' Store
	// contract), and validation runs against a dummy seq of 1: the one field
	// only the insert can decide is the one Validate must not judge on the
	// caller's behalf.
	if ev.SchemaVersion == 0 {
		ev.SchemaVersion = events.SchemaVersion
	}
	if ev.EventID == "" {
		ev.EventID = games.NewID()
	}
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = time.Now().UTC()
	}
	if len(ev.Data) == 0 {
		ev.Data = json.RawMessage("{}")
	}
	ev.Seq = 1
	if err := ev.Validate(); err != nil {
		return 0, err
	}

	const op = "append event"
	var sessionID, causationID any
	if ev.SessionID != "" {
		sessionID = ev.SessionID
	}
	if ev.CausationID != "" {
		causationID = ev.CausationID
	}

	const q = `INSERT INTO events (event_id, session_id, name, trace_id, causation_id,
		occurred_at, schema_version, data_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	args := []any{ev.EventID, sessionID, ev.Name, ev.TraceID, causationID,
		ev.OccurredAt.UTC().Format(eventStamp), ev.SchemaVersion, string(ev.Data)}

	var (
		res sql.Result
		err error
	)
	if tx := TxFrom(ctx); tx != nil {
		res, err = tx.Exec(ctx, q, args...)
	} else {
		res, err = e.s.db.ExecContext(ctx, q, args...)
	}
	if err != nil {
		return 0, mapWriteError(op, err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: %s: read seq: %w", op, err)
	}
	if seq < 1 {
		return 0, fmt.Errorf("store: %s: sqlite returned seq %d", op, seq)
	}
	return seq, nil
}

// After returns matching events with Seq > afterSeq in ascending seq order,
// at most limit (clamped to [1, 1000]). The ordering is the primary-key
// order, so a replay walk is an index range scan (events_session_seq_idx
// covers the filtered variant) rather than a sort.
func (e *EventStore) After(ctx context.Context, afterSeq int64, f events.Filter, limit int) ([]events.Envelope, error) {
	if err := events.ValidateSessionID(f.SessionID); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > maxEventStoreLimit {
		limit = maxEventStoreLimit
	}

	var (
		sb   strings.Builder
		args []any
	)
	sb.WriteString(`SELECT seq, event_id, session_id, name, trace_id, causation_id,
		occurred_at, schema_version, data_json FROM events WHERE seq > ?`)
	args = append(args, afterSeq)
	if f.SessionID != "" {
		sb.WriteString(" AND session_id = ?")
		args = append(args, f.SessionID)
	}
	if len(f.Names) > 0 {
		sb.WriteString(" AND name IN (")
		for i, n := range f.Names {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString("?")
			args = append(args, n)
		}
		sb.WriteString(")")
	}
	fmt.Fprintf(&sb, " ORDER BY seq LIMIT %d", limit) // clamp above; never interpolate caller text

	rows, err := e.s.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, mapReadError("query events after", err)
	}
	defer rows.Close()

	var out []events.Envelope
	for rows.Next() {
		ev, err := scanEventRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, mapReadError("iterate events", rows.Err())
}

// PruneEvents enforces the bounded retention docs/spec/02 demands of the
// operational store: every event older than olderThan is deleted, except that
// the newest keepLast events always survive the age cut. keepLast exists
// because occurred_at is a wall-clock at second-plus width resolution while
// `seq` is the cursor browsers hold — pruning purely by time could delete
// events a reconnecting Last-Event-ID still points into, and the honest answer
// to "what happened after N" is either the events or
// events.resync_required, never silence (the hub's replay probe turns a pruned
// range into exactly that notice).
//
// Returns the number of rows deleted. A negative keepLast is treated as 0.
func (e *EventStore) PruneEvents(ctx context.Context, olderThan time.Time, keepLast int) (int, error) {
	if keepLast < 0 {
		keepLast = 0
	}
	const op = "prune events"
	var deleted int
	err := e.s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		res, err := tx.exec(ctx, op,
			`DELETE FROM events
			 WHERE occurred_at < ?
			   AND seq <= (SELECT COALESCE (MAX (seq), 0) - ? FROM events)`,
			olderThan.UTC().Format(eventStamp), keepLast)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		deleted = int(n)
		return err
	})
	return deleted, err
}

// scanEventRow rebuilds the envelope from its columns. The durable row is the
// authoritative record of what was published; seq is filled from the table,
// which is what makes a replayed frame carry the same `id:` the live frame
// did.
func scanEventRow(s scanner) (events.Envelope, error) {
	var (
		seq                              int64
		eventID, name, traceID, occurred string
		sessionID, causationID           sql.NullString
		schemaVersion                    int
		data                             []byte
	)
	const op = "scan event"
	if err := s.Scan(&seq, &eventID, &sessionID, &name, &traceID, &causationID,
		&occurred, &schemaVersion, &data); err != nil {
		return events.Envelope{}, mapReadError(op, err)
	}
	at, err := time.Parse(time.RFC3339Nano, occurred)
	if err != nil {
		return events.Envelope{}, fmt.Errorf("store: %s: occurred_at %q: %w", op, occurred, err)
	}
	return events.Envelope{
		SchemaVersion: schemaVersion,
		EventID:       eventID,
		SessionID:     nullStringText(sessionID),
		Seq:           seq,
		TraceID:       traceID,
		CausationID:   nullStringText(causationID),
		Name:          name,
		OccurredAt:    at.UTC(),
		Data:          data,
	}, nil
}
