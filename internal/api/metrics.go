package api

import (
	"context"
	"crypto/subtle"
	"net/http"

	"github.com/dbirks/game-narrator/internal/auth"
)

// contentTypePrometheus is the exposition's media type (the Prometheus text
// format, version 0.0.4): a scraper keys on it, and the charset is part of
// the contract because a label value is always ASCII after sanitisation.
const contentTypePrometheus = "text/plain; version=0.0.4; charset=utf-8"

// metricsPath is the scraper endpoint (docs/spec/13 "Metrics"). It sits at
// the root, outside /api/v1, on purpose: it is a monitor-facing scrape, not
// a versioned API route, so it is not in the route table the scope table and
// the OpenAPI contract track, and the standard authorisation middleware
// skips it (non-/api/ paths pass through). Its own gate — admin principal or
// the configured token — is enforced inside the handler, because a gate the
// middleware does not run is a gate that is not run.
const metricsPath = "/metrics"

// metricsRoute reports whether this request is for the /metrics surface, so
// the dispatch handler routes it ahead of the embedded-UI fallback (an
// unclaimed non-API path would otherwise be answered by the SPA's
// index.html, which is the wrong body for a scraper and for the "not
// enabled" 404 alike).
func (s *Server) metricsRoute(r *http.Request) bool {
	return r.URL.Path == metricsPath &&
		(r.Method == http.MethodGet || r.Method == http.MethodHead)
}

// metricsAllowed is the /metrics gate (docs/spec/14): an admin principal —
// the usual session cookie or API token resolved by the same identify the
// /api/ routes use — or the static scraper token when one is configured.
// The token compare is constant-time: a wrong token must not leak a timing
// oracle about its length or contents. A non-admin credential (developer,
// operator, …) does not pass: reading the appliance's raw metrics stream is
// the admin tier, and the developer snapshot endpoint is the lower route for
// the same data inside the normal ACL.
func (s *Server) metricsAllowed(r *http.Request) bool {
	if s.metricsToken != "" {
		if tok := bearerToken(r); tok != "" &&
			subtle.ConstantTimeCompare([]byte(tok), []byte(s.metricsToken)) == 1 {
			return true
		}
	}
	if s.authSvc == nil {
		return false
	}
	id, err := s.identify(r)
	if err != nil {
		return false
	}
	return id.Role.AtLeast(auth.RoleAdmin)
}

// refreshJobQueueDepth books the durable queue's current depth on the gauge
// just before a scrape renders it. The depth is read from the database — the
// authority on job state (internal/jobs) — rather than kept as an in-process
// counter, because crash-recovery handoff moves jobs between states outside
// the enqueue/complete paths and a counter would drift from the table. An
// unreadable queue keeps the last known reading: a scrape that cannot count
// is not zero depth.
func (s *Server) refreshJobQueueDepth(ctx context.Context) {
	if s.metrics == nil || s.jobStore == nil {
		return
	}
	n, err := s.jobStore.QueueDepth(ctx)
	if err != nil {
		return
	}
	s.metrics.SetJobQueueDepth(n)
}

// handleMetrics serves GET /metrics in the Prometheus text format
// (docs/spec/13 "Metrics"). Off by default: a server without a wired catalog
// answers 404, so the absence of a scrape target is visible to the monitor
// instead of the SPA answering for it. With a catalog, the gate is
// admin-or-token (metricsAllowed); an unauthenticated or below-admin caller
// gets a 401 that names both access paths.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if s.metrics == nil {
		s.writeProblem(w, r, http.StatusNotFound, "metrics-disabled",
			"Not Found",
			"the metrics surface is not enabled in this process: wire a telemetry catalog (WithMetrics) to serve it")
		return
	}
	if !s.metricsAllowed(r) {
		s.writeProblem(w, r, http.StatusUnauthorized, "metrics-forbidden",
			"Unauthorized",
			"the metrics surface requires an admin session or API token, or the configured metrics token")
		return
	}
	s.refreshJobQueueDepth(r.Context())
	w.Header().Set("Content-Type", contentTypePrometheus)
	w.Header().Set("Cache-Control", "no-store")
	// The writer streams the whole registry in declaration order; a registry
	// is bounded (docs/spec/13), so the body is bounded with it.
	s.metrics.Registry.WritePrometheus(w)
}

// handleDevMetrics serves GET /api/v1/dev/metrics: the JSON snapshot of the
// same registry, the internal API the diagnostics and health surfaces read
// (docs/spec/13 "Metrics"): structured series with the histogram slots,
// instead of a text stream to parse. It sits under /api/v1/dev, so the
// standard middleware enforces the scope table's developer grant on it, and
// it carries the same data the /metrics scrape does — the difference is the
// reader (the appliance's own UI, authenticated in the normal way) and the
// shape (the snapshot a page renders). A server without a catalog answers
// 503: an empty snapshot would read as "everything is zero".
func (s *Server) handleDevMetrics(w http.ResponseWriter, r *http.Request) {
	if s.metrics == nil {
		s.writeProblem(w, r, http.StatusServiceUnavailable, "metrics-unwired",
			"Service Unavailable",
			"no telemetry catalog is wired into this process; the metrics snapshot has no data to serve")
		return
	}
	s.refreshJobQueueDepth(r.Context())
	writeJSON(w, http.StatusOK, s.metrics.Registry.Snapshot())
}
