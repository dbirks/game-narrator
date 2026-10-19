package api

// Developer inspection API (docs/spec/08 "APIs": GET /api/v1/dev/events,
// GET /api/v1/dev/traces/:id; ADR-0024 adds GET /api/v1/dev/assets/:sha256
// for the sampled developer-trace evidence).
//
// Every route here is developer-role-only: the event payloads of a live
// session carry raw recognised gameplay text and provider details that a
// viewer must never see (docs/spec/08 "Developer mode can display sensitive
// gameplay text and provider traces; require explicit role"), and the dev-
// trace assets are captured gameplay frames. The scope table grants the whole
// /api/v1/dev prefix to developer; this package adds no exceptions.

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/runtime"
)

// devEventsPage is the REST snapshot behind the developer inspector's event
// list. It is the durable store's After call, not the hub's ring: the ring
// drops, the store does not, and the inspector's job is to read what already
// happened.
type devEventsPage struct {
	Events    []events.Envelope `json:"events"`
	NextSeq   int64             `json:"next_seq"`
	Truncated bool              `json:"truncated"`
}

// handleDevEvents answers GET /api/v1/dev/events with a page of persisted
// event envelopes, filtered by session and/or name and resumed from a seq.
// Query: session_id, name (repeatable), since_seq, limit (default 100, max
// 1000 — the store's own bound).
func (s *Server) handleDevEvents(w http.ResponseWriter, r *http.Request) {
	if s.sessions == nil || s.devEvents == nil {
		s.writeProblem(w, r, http.StatusServiceUnavailable, "dev-inspection-unavailable",
			"Dev Inspection Unavailable",
			"no event store or session runtime is wired in this process; the developer inspector has nothing to read")
		return
	}
	q := r.URL.Query()
	sessionID := strings.TrimSpace(q.Get("session_id"))
	names := q["name"]
	for i := range names {
		names[i] = strings.TrimSpace(names[i])
	}
	since := int64(0)
	if v := q.Get("since_seq"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			s.writeProblem(w, r, http.StatusBadRequest, "invalid-since-seq",
				"Bad Request", "since_seq must be a non-negative integer")
			return
		}
		since = n
	}
	limit := 100
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 || n > 1000 {
			s.writeProblem(w, r, http.StatusBadRequest, "invalid-limit",
				"Bad Request", "limit must be an integer between 1 and 1000")
			return
		}
		limit = n
	}

	envelopes, err := s.devEvents.After(r.Context(), since, events.Filter{
		SessionID: sessionID,
		Names:     names,
	}, limit)
	if err != nil {
		s.writeProblem(w, r, http.StatusInternalServerError, "internal-error",
			"Internal Error", "reading the event store failed; see the server log for this request_id")
		return
	}
	next := since
	if len(envelopes) > 0 {
		next = envelopes[len(envelopes)-1].Seq + 1
	}
	writeJSON(w, http.StatusOK, devEventsPage{
		Events:    envelopes,
		NextSeq:   next,
		Truncated: len(envelopes) == limit,
	})
}

// handleDevTraces answers GET /api/v1/dev/traces/{trace_id}: every persisted
// envelope that carries the named trace, in seq order. The store filters by
// session and name, not by trace, so the handler pages the store and selects
// by trace_id — bounded by the same 1..1000 limit as the event page, with
// truncated=true when a match would have required more scanning.
func (s *Server) handleDevTraces(w http.ResponseWriter, r *http.Request) {
	if s.sessions == nil || s.devEvents == nil {
		s.writeProblem(w, r, http.StatusServiceUnavailable, "dev-inspection-unavailable",
			"Dev Inspection Unavailable",
			"no event store or session runtime is wired in this process; the developer inspector has nothing to read")
		return
	}
	traceID := strings.TrimSpace(r.PathValue("trace_id"))
	if traceID == "" {
		s.writeProblem(w, r, http.StatusBadRequest, "invalid-trace-id",
			"Bad Request", "the trace id in the path is empty")
		return
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 || n > 1000 {
			s.writeProblem(w, r, http.StatusBadRequest, "invalid-limit",
				"Bad Request", "limit must be an integer between 1 and 1000")
			return
		}
		limit = n
	}

	var matched []events.Envelope
	scanSeq := int64(0)
	// scanPages caps the store pages one trace lookup may consume: a trace
	// that spans more than this of the store's history is broken, not slow,
	// and the answer says so instead of holding the appliance.
	const scanPages = 50
	for i := 0; i < scanPages; i++ {
		page, err := s.devEvents.After(r.Context(), scanSeq, events.Filter{}, 1000)
		if err != nil {
			s.writeProblem(w, r, http.StatusInternalServerError, "internal-error",
				"Internal Error", "reading the event store failed; see the server log for this request_id")
			return
		}
		for _, e := range page {
			if e.TraceID == traceID {
				matched = append(matched, e)
			}
		}
		if len(page) < 1000 {
			break // the store is exhausted; the scan is complete
		}
		scanSeq = page[len(page)-1].Seq + 1
	}
	truncated := len(matched) > limit
	if truncated {
		matched = matched[:limit]
	}
	writeJSON(w, http.StatusOK, devEventsPage{
		Events:    matched,
		NextSeq:   scanSeq,
		Truncated: truncated,
	})
}

// devBottlenecksResponse is the GET /api/v1/dev/sessions/{id}/bottlenecks
// body: the session's own per-stage latency summary (docs/spec/16 M7
// "diagnostics expose bottlenecks") — the same rows, shares and named
// slowest stage the support bundle's bottlenecks.json and the developer page
// block read, all from runtime.Bottlenecks on the session's own stats.
type devBottlenecksResponse struct {
	SessionID string `json:"session_id"`
	State     string `json:"state"`
	runtime.BottleneckSummary
}

// handleDevSessionBottlenecks answers GET /api/v1/dev/sessions/{id}/bottlenecks:
// one run's stage latencies (p50/p95/max, share of the end-to-end, the named
// slowest stage) reduced by the shared summary function from the session's own
// pipeline stats. It is the live-session half of the bottleneck artifact:
// the numbers are the run's wall-clock stage intervals (the same class as the
// bench's duration figure, ADR-0064), so the view reports them and names the
// slow stage instead of pretending to grade on them. A live (still running)
// session answers with its stats so far — usually all empty until the run
// settles, which the empty summary says honestly (every stage count 0, no
// stage named).
func (s *Server) handleDevSessionBottlenecks(w http.ResponseWriter, r *http.Request) {
	if s.sessions == nil {
		s.writeProblem(w, r, http.StatusServiceUnavailable, "sessions-unavailable",
			"Sessions Unavailable",
			"no session runtime is wired in this process; the session view has nothing to read")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		s.writeProblem(w, r, http.StatusBadRequest, "invalid-session-id",
			"Bad Request", "the session id in the path is empty")
		return
	}
	info, ok := s.sessions.Get(id)
	if !ok {
		s.writeProblem(w, r, http.StatusNotFound, "session-not-found",
			"Session Not Found", fmt.Sprintf("no session with id %q exists in this process", id))
		return
	}
	summary := runtime.Bottlenecks(info.Stats.Latency)
	writeJSON(w, http.StatusOK, devBottlenecksResponse{
		SessionID:         info.ID,
		State:             info.State,
		BottleneckSummary: summary,
	})
}

// handleDevAsset answers GET /api/v1/dev/assets/{sha256} with one stored
// developer-trace blob (a sampled full-frame JPEG or an ROI crop PNG). The
// bytes travel here and only here: text.observed carries the hash, the hash
// is content addressing, and the developer role gate is the privacy boundary
// (docs/spec/13, ADR-0024).
func (s *Server) handleDevAsset(w http.ResponseWriter, r *http.Request) {
	if s.sessions == nil {
		s.writeProblem(w, r, http.StatusServiceUnavailable, "dev-inspection-unavailable",
			"Dev Inspection Unavailable",
			"no session runtime is wired in this process; the developer inspector has nothing to read")
		return
	}
	sha := strings.ToLower(strings.TrimSpace(r.PathValue("sha256")))
	rc, info, err := s.sessions.OpenDevAsset(r.Context(), sha)
	if err != nil {
		// An unknown hash, an evicted sample and a hash never stored all read
		// as "no such asset": the developer knows the sha came from an event
		// and can judge why it is gone from the event's own session.
		s.writeProblem(w, r, http.StatusNotFound, "asset-not-found",
			"Asset Not Found", fmt.Sprintf("no dev-trace asset with sha256 %q is stored for any session", sha))
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", info.MediaType)
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}
