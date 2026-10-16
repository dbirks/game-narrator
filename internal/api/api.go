// Package api implements the /api/v1 HTTP surface (docs/spec/08-api-and-events.md).
//
// Milestone 0 covers process/system introspection routes, RFC 9457
// problem+json errors and request correlation. Auth, SSE, session, game,
// voice, onboarding, job and dev routes are added by later milestones.
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime"
	"time"

	"github.com/dbirks/game-narrator/internal/bootstrap"
)

// Media types used by this package.
const (
	ContentTypeJSON    = "application/json"
	ContentTypeProblem = "application/problem+json"
)

// HeaderRequestID carries the correlation id returned with every response.
const HeaderRequestID = "X-Request-Id"

type ctxKey int

const requestIDKey ctxKey = iota

// RequestIDFromContext returns the correlation id attached to this request.
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// Server serves the API. Handlers receive explicit dependencies so they can
// be exercised with fakes (docs/spec/12 "Unit" mode).
type Server struct {
	build     bootstrap.BuildInfo
	startedAt time.Time
	log       *slog.Logger
}

// New constructs an API server. A zero BuildInfo or nil logger is tolerated.
func New(build bootstrap.BuildInfo, startedAt time.Time, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{build: build, startedAt: startedAt.UTC(), log: log}
}

// Handler returns the routed HTTP handler, including correlation and
// problem+json fallbacks.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	var routes routeTable
	routes.register(mux, http.MethodGet, "/api/v1/health/live", s.handleLive)
	routes.register(mux, http.MethodGet, "/api/v1/health/ready", s.handleReady)
	routes.register(mux, http.MethodGet, "/api/v1/system/info", s.handleSystemInfo)

	// "/" is the least specific pattern, so it only receives requests that no
	// method+path pattern above matched.
	mux.Handle("/", s.fallback(routes))

	return withRequestID(s.log, mux)
}

// handleLive reports process liveness only: no dependency probing.
func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// healthResponse is the readiness payload. checks stays an (initially empty)
// map so dependency checks can be added without a breaking change.
type healthResponse struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

// handleReady reports dependency readiness. No dependencies are wired yet, so
// the check map is empty, which is reported as ready.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, healthResponse{Status: "ok", Checks: map[string]string{}})
}

// systemInfoResponse exposes build/OS capacity only: no hostnames, paths,
// environment values or secrets.
type systemInfoResponse struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	GoVersion string `json:"go_version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	NumCPU    int    `json:"num_cpu"`
	StartedAt string `json:"started_at"`
}

func (s *Server) handleSystemInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, systemInfoResponse{
		Version:   s.build.Version,
		Commit:    s.build.Commit,
		GoVersion: s.build.GoVersion,
		OS:        s.build.GOOS,
		Arch:      s.build.GOARCH,
		NumCPU:    runtime.NumCPU(),
		StartedAt: s.startedAt.Format(time.RFC3339),
	})
}

// writeProblem emits an RFC 9457 application/problem+json response correlated
// with X-Request-Id.
func (s *Server) writeProblem(w http.ResponseWriter, r *http.Request, status int, slug, title, detail string) {
	p := problem{
		Type:      "urn:game-narrator:error:" + slug,
		Title:     title,
		Status:    status,
		Detail:    detail,
		Instance:  r.URL.Path,
		RequestID: RequestIDFromContext(r.Context()),
	}
	if p.RequestID == "" {
		p.RequestID = newRequestID()
		w.Header().Set(HeaderRequestID, p.RequestID)
	}
	w.Header().Set("Content-Type", ContentTypeProblem)
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(p); err != nil {
		s.log.Error("failed to encode problem response",
			"status", status, "request_id", p.RequestID, "error", err)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", ContentTypeJSON)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
