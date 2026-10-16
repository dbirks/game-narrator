// Package api implements the /api/v1 HTTP surface (docs/spec/08-api-and-events.md).
//
// Milestone 0 covers process/system introspection routes, RFC 9457
// problem+json errors and request correlation. Auth, SSE, session, game,
// voice, onboarding, job and dev routes are added by later milestones.
package api

import (
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/bootstrap"
	"github.com/dbirks/game-narrator/internal/webassets"
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

// Server serves the API and, for every non-API path, the embedded web UI
// (docs/spec/09). Handlers receive explicit dependencies so they can be
// exercised with fakes (docs/spec/12 "Unit" mode).
type Server struct {
	build     bootstrap.BuildInfo
	startedAt time.Time
	log       *slog.Logger

	// webFS holds the built SPA (rooted so index.html is "index.html").
	webFS fs.FS
}

// Option customises a Server. Production wiring needs none of them; tests
// inject an in-memory UI build (docs/spec/12 "Unit" mode).
type Option func(*Server)

// WithWebAssets replaces the filesystem served for non-API paths. A nil fs.FS
// is treated as an empty build, which answers 503 on UI paths.
func WithWebAssets(filesystem fs.FS) Option {
	return func(s *Server) { s.webFS = filesystem }
}

// New constructs an API server. A zero BuildInfo or nil logger is tolerated.
// By default the UI comes from webassets.FS(), i.e. whatever Vite output was
// compiled into the binary.
func New(build bootstrap.BuildInfo, startedAt time.Time, log *slog.Logger, opts ...Option) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{
		build:     build,
		startedAt: startedAt.UTC(),
		log:       log,
		webFS:     webassets.FS(),
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.webFS == nil {
		s.webFS = emptyFS{}
	}
	return s
}

// webBuilt reports whether an actual UI build is embedded. It is a method so
// the injected test filesystem takes part in the same check.
func (s *Server) webBuilt() bool {
	info, err := fs.Stat(s.webFS, indexFile)
	return err == nil && !info.IsDir()
}

// Handler returns the routed HTTP handler, including correlation,
// embedded-UI serving and problem+json fallbacks.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	var routes routeTable
	routes.register(mux, http.MethodGet, "/api/v1/health/live", s.handleLive)
	routes.register(mux, http.MethodGet, "/api/v1/health/ready", s.handleReady)
	routes.register(mux, http.MethodGet, "/api/v1/system/info", s.handleSystemInfo)

	// "/" is the least specific pattern, so it only receives requests that no
	// method+path pattern above matched.
	mux.Handle("/", s.fallback(routes))

	// ServeMux canonicalizes an unclean path with a 307 of its own before it
	// dispatches (net/http findHandler: cleaned path != request path), which
	// would answer a traversal probe such as /../etc/passwd with a redirect
	// instead of the 404 that docs/spec/09 requires. Extensionless UI paths are
	// therefore dispatched here, ahead of the mux; anything a registered route
	// claims still goes through it.
	dispatch := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, apiRoot) && !routes.matches(r.Method, r.URL.Path) {
			s.serveStatic(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})

	return withRequestID(s.log, dispatch)
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
