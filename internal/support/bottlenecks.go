// The bottlenecks section of the support bundle (docs/spec/13 "Diagnostics":
// the support bundle is the diagnostics export; docs/spec/16 M7 "diagnostics
// expose bottlenecks"): the per-session latency summary of every finished
// session, computed by the one shared function (runtime.Bottlenecks) from the
// session's own persisted terminal event — the stats the session.stopped /
// session.failed envelope carries.
//
// Privacy (docs/spec/13, AGENTS.md rule 5): the section carries stage names,
// sample counts and latency figures only. The terminal event's payload also
// holds the run's trace rows (frame/crop hashes); those are never copied into
// the bundle — the latency stats are the diagnostic, and they name no game
// text, no path and no hostname.
//
// The store is opened strictly read-only (the bundle never writes to the
// user's data dir), and the section is bounded to the most recent sessions
// (a bundle for a long-lived appliance must stay small and quick to build).

package support

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/pipeline"
	"github.com/dbirks/game-narrator/internal/runtime"
	"github.com/dbirks/game-narrator/internal/store"
)

// BottlenecksPath is the archive entry name, fixed and asserted by the tests.
const BottlenecksPath = "bottlenecks.json"

// maxBottleneckSessions bounds the section: the newest terminal sessions are
// the ones a support engineer is debugging; the rest are readable through the
// store section's counts and the event log on the box.
const maxBottleneckSessions = 100

// bottlenecksSession is one finished session's row: its terminal event's
// identity and the shared summary of the session's own stage stats.
type bottlenecksSession struct {
	SessionID string `json:"session_id"`
	// Event is the terminal event the stats came from: session.stopped or
	// session.failed.
	Event string `json:"event"`
	// OccurredAt is the terminal event's store stamp (RFC 3339).
	OccurredAt string `json:"occurred_at"`
	// Stages and SlowestStage are the shared summary's rows and its named
	// slowest stage ("" when the run measured no stage).
	Stages       []runtime.StageLatency `json:"stages"`
	SlowestStage string                 `json:"slowest_stage"`
}

// bottlenecksDoc is the section: a note stating exactly what (and what not)
// the rows contain, newest first.
type bottlenecksDoc struct {
	Note     string               `json:"note"`
	Sessions []bottlenecksSession `json:"sessions"`
}

// buildBottlenecks reads the durable event log read-only for the finished
// sessions and reduces each session's own stats with the shared summary
// function. A malformed terminal payload is a store defect the bundle
// reports loudly instead of silently dropping the session: a diagnostics
// artifact that hides one of the very sessions an engineer is debugging
// would mislead the person it was built for.
func buildBottlenecks(ctx context.Context, dataDir string) ([]byte, error) {
	db, err := sql.Open(sqliteDriverName, queryOnlyDSN(store.DBPath(dataDir)))
	if err != nil {
		return nil, fmt.Errorf("support: open store read-only: %w", err)
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx,
		`SELECT session_id, name, occurred_at, data_json
		   FROM events
		  WHERE name IN (?, ?)
		  ORDER BY seq DESC
		  LIMIT `+itoa(maxBottleneckSessions),
		events.SessionStopped, runtime.EventSessionFailed)
	if err != nil {
		return nil, fmt.Errorf("support: read terminal session events: %w", err)
	}
	defer rows.Close()

	sessions := make([]bottlenecksSession, 0, 16)
	for rows.Next() {
		var (
			sessionID, name, occurredAt string
			dataJSON                    string
		)
		if err := rows.Scan(&sessionID, &name, &occurredAt, &dataJSON); err != nil {
			return nil, fmt.Errorf("support: scan terminal session event: %w", err)
		}
		var payload struct {
			Stats pipeline.Stats `json:"stats"`
		}
		if err := json.Unmarshal([]byte(dataJSON), &payload); err != nil {
			return nil, fmt.Errorf("support: parse the terminal event's stats for session %s (event %s): the row cannot be the store wrote it: %w",
				sessionID, name, err)
		}
		summary := runtime.Bottlenecks(payload.Stats.Latency)
		sessions = append(sessions, bottlenecksSession{
			SessionID:    sessionID,
			Event:        name,
			OccurredAt:   occurredAt,
			Stages:       summary.Stages,
			SlowestStage: summary.SlowestStage,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("support: read terminal session events: %w", err)
	}

	doc := bottlenecksDoc{
		Note: "Per-session stage latency (p50/p95/max, share of the end-to-end, named slowest stage) for the most recent " +
			itoa(maxBottleneckSessions) +
			" finished sessions, computed by the shared summary function from each session's own persisted terminal-event stats. Wall-clock diagnostic figures (host-load dependent, ADR-0064): they name the bottleneck, they do not grade the run. Stage names, counts and latencies only — no trace rows, no game text, no paths.",
		Sessions: sessions,
	}
	return marshalDoc(doc)
}

// itoa keeps the SQL limit a constant string without importing strconv into
// a query-literal position (the limit is a fixed bound, not input data).
func itoa(n int) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = digits[n%10]
		n /= 10
	}
	return string(b[i:])
}
