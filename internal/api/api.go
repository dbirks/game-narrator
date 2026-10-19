// Package api implements the /api/v1 HTTP surface (docs/spec/08-api-and-events.md).
//
// Auth (internal/auth + internal/api/auth.go) sits in front of every non-public
// route: role grants from the scope table, cookie sessions, API tokens and CSRF
// (docs/spec/08 "API version and user authentication", docs/spec/14
// "Authorization", ADR-0016). Health, system info, the auth endpoints
// themselves and the embedded UI are public; everything else under /api/ that
// the scope table does not recognise requires admin, so a route cannot be born
// unauthenticated by omission.
//
// Game, voice, onboarding and dev routes are added by later milestones; the
// runtime that publishes events into the hub arrives with them.
package api

import (
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/bootstrap"
	"github.com/dbirks/game-narrator/internal/dolphin"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games/adapter"
	rt "github.com/dbirks/game-narrator/internal/runtime"
	"github.com/dbirks/game-narrator/internal/store"
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

const (
	requestIDKey ctxKey = iota
	identityKey
)

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

	// readiness reports dependency state for /health/ready. nil means this
	// build has no dependencies wired, which is reported as an empty check
	// map rather than a claim that everything works.
	readiness ReadinessFunc

	// hub fans events out to SSE clients. nil means no event log is wired yet,
	// which GET /api/v1/events reports as 503 rather than pretending to stream.
	hub *events.Hub

	// jobStore backs /api/v1/jobs (docs/spec/08 "durable workflow queue").
	// nil answers 503 there for the same honest-degradation reason as hub:
	// an empty list would claim a working appliance with no queue.
	jobStore *store.JobsStore

	// authSvc verifies credentials and answers the auth routes. nil means this
	// process has no credential store wired at all, which is reported as 503 on
	// every non-public route — an API that simply let requests through because
	// nobody configured auth is the fail-open bug docs/spec/14 is written
	// against, and "localhost by default" is not an access control (ADR-0016).
	authSvc *auth.Service

	// policy is the route→role scope table. It is a field, not a package
	// global, so a test can assert a grant table without rebuilding the server
	// and so a future config route can show the live table.
	policy *auth.Policy

	// trustedProxies gate X-Forwarded-Proto for the cookie Secure attribute.
	// Empty (the default) means no forwarded header is believed, which is the
	// right answer for a localhost-bound appliance and the only answer that
	// cannot be spoofed (docs/spec/10 "reverse proxy with authentication").
	trustedProxies []*net.IPNet

	// routes records what Handler registered, for 405 detection *and* for the
	// test that fails when a route exists without a grant in the scope table.
	routes routeTable

	// authStore backs /api/v1/users and /api/v1/auth/* persistence. It is
	// carried separately from authSvc because the service owns credential rules
	// and never list-and-manage routes; the routes need both.
	users *store.AuthStore

	// store backs the catalog, profile, message, publication and search routes
	// (catalog.go, profiles.go, messages.go, importexport.go). nil answers 503
	// there for the same honest-degradation reason as hub and jobStore: "no
	// games" and "no catalogue wired" are different facts, and an empty list
	// reports the wrong one.
	store *store.Store

	// assets is the content-addressed blob store an upload is committed to
	// before its job is queued (ADR-0014); adapters is the parser registry
	// linked into this build; jobQueue is what makes POST …/import a 202 instead
	// of a request-path parse.
	assets   assets.Store
	adapters *adapter.Registry
	jobQueue JobQueue

	// sessions owns the session lifecycle API (sessions.go): start, status and
	// stop, plus the dev-trace asset lookup the developer inspector's asset
	// route reads through. nil answers 503 for the same honest-degradation
	// reason as every other unwired subsystem: an empty session list would
	// claim an appliance with no runtime.
	sessions *rt.Manager

	// devEvents is the durable event log behind the developer inspector's REST
	// pages (dev.go). It is the same store the hub persists into, so the pages
	// show exactly what the SSE stream would have shown, minus the live tail.
	// nil answers 503 on /api/v1/dev/events and /api/v1/dev/traces.
	devEvents events.Store

	// onb wires the onboarding agent routes (onboarding.go): the durable
	// queue the runs execute as, the run log and the human approval tokens.
	// nil answers 503 under /api/v1/onboarding (ADR-0028).
	onb *onbDeps

	// idempotency remembers the responses of keyed creates so a retried POST
	// cannot create two games (docs/spec/08 "Conventions"). It is per-process by
	// construction and ADR-0017 says so out loud; the durable queue owns the
	// cross-restart case through its own idempotency_key column.
	idempotency *idempotencyStore

	// voices wires the voice setup workflow (voices.go): audition creation is
	// queue-backed (the 202 carries a job_id) and the slow VoiceDesign calls run
	// as the voice.auditions job kind. nil answers 503 under /api/v1/voices for
	// the same honest-degradation reason as every other unwired subsystem.
	voices *voiceDeps

	// dolphinBridge wires the developer-only Dolphin automation bridge behind
	// /api/v1/dev/dolphin (docs/spec/11, ADR-0007). It is a dev-host tool: the
	// routes additionally answer 501 unless GAME_NARRATOR_DEV_DOLPHIN=1, and
	// 503 when nil, so a production process cannot launch an emulator or inject
	// controller input through the API no matter what a developer-role caller
	// asks it to.
	dolphinBridge dolphin.Bridge

	// ocrPreview wires the region editor's OCR preview (regions_preview.go):
	// the per-PSM engine factory and the replay-root fixture allowlist. A nil
	// factory answers 503 on the preview route: empty results would read as
	// "the box is blank" rather than "no OCR engine is wired here".
	ocrPreview ocrPreviewDeps

	// listenAddr is the bound listen address the /settings page's security
	// section reports (WithListen). Empty when the host did not wire it, and
	// the page then falls back to the environment resolution of the same
	// flag > env > default precedence the process itself used.
	listenAddr string

	// harnessRepoRoot is the repository root the /tests page's dev harness
	// reads (WithTestHarness): testdata/scenarios for the catalog and runs,
	// artifacts/gates for the recorded gate results. Empty falls back to the
	// GAME_NARRATOR_REPO_ROOT variable and then the working directory, which
	// is how the e2e binary (started from the checkout) finds its fixtures
	// without any extra wiring.
	harnessRepoRoot string

	// discImportDir is the operator's allowlisted disc-image directory
	// (GAME_NARRATOR_IMPORT_DIR, ADR-0066). Empty means the dir shape answers
	// 503 and only the upload shape works; the store, asset, adapter and
	// queue dependencies it leans on are the Server's own fields.
	discImportDir string

	// workerOnce starts the scenario.run worker at most once per process
	// (tests_worker.go), from the run route after the enqueue.
	workerOnce sync.Once
}

// ReadinessFunc answers GET /api/v1/health/ready. It returns the per-dependency
// check map (keyed by dependency name, e.g. "database") and whether the service
// is ready; ok=false makes the endpoint answer 503 with "status":"fail".
//
// A readiness probe runs on the request path, so it must be cheap and must
// honour ctx. Returned values must not contain paths, URLs or credentials.
type ReadinessFunc func(ctx context.Context) (checks map[string]any, ok bool)

// Option customises a Server. Production wiring needs none of them; tests
// inject an in-memory UI build (docs/spec/12 "Unit" mode).
type Option func(*Server)

// WithWebAssets replaces the filesystem served for non-API paths. A nil fs.FS
// is treated as an empty build, which answers 503 on UI paths.
func WithWebAssets(filesystem fs.FS) Option {
	return func(s *Server) { s.webFS = filesystem }
}

// WithReadiness wires the dependency probe behind /api/v1/health/ready, so the
// process can never answer "ok" about a service it has not exercised. Tests
// inject a failing checker to prove the 503 path.
func WithReadiness(f ReadinessFunc) Option {
	return func(s *Server) { s.readiness = f }
}

// WithEvents wires the event hub behind GET /api/v1/events. Without it the
// endpoint answers 503 problem+json: an API that looked like a working stream
// would let the UI believe it was watching a session it was not (docs/spec/13
// "no generic success on an unprobed service").
func WithEvents(h *events.Hub) Option {
	return func(s *Server) { s.hub = h }
}

// WithJobs wires the durable queue behind /api/v1/jobs (list, get, cancel,
// retry). There is no POST /jobs endpoint yet on purpose — enqueue is a
// server-subsystem action (see internal/api/jobs.go). Like the hub, unconfigured
// is reported as 503, not as an empty queue.
func WithJobs(js *store.JobsStore) Option {
	return func(s *Server) { s.jobStore = js }
}

// WithAuth wires credentials and authorization: the auth service (cookie
// sessions, API tokens, CSRF, first-run setup) and the AuthStore behind the
// user-management routes. Without it every non-public route answers 503
// problem+json — the same rule WithEvents and WithJobs follow, applied to the
// control that decides whether to answer at all.
func WithAuth(svc *auth.Service, users *store.AuthStore) Option {
	return func(s *Server) {
		s.authSvc = svc
		s.users = users
	}
}

// WithAuthPolicy replaces the route→role scope table. Tests inject a table to
// prove the fail-closed default; an operator would not.
func WithAuthPolicy(p *auth.Policy) Option {
	return func(s *Server) {
		if p != nil {
			s.policy = p
		}
	}
}

// WithStore wires the catalogue behind the game, edition, profile, message,
// publication and search routes. As with every other subsystem option, an
// unwired server says 503 rather than serving an empty library.
func WithStore(st *store.Store) Option {
	return func(s *Server) { s.store = st }
}

// WithImport wires the three collaborators an import needs: the asset store the
// upload is committed to, the adapter registry that decides what can be read,
// and the queue the parse is handed to. Passing them together is deliberate —
// an import missing one of the three answers a different 503 sentence, and the
// option makes "partially wired" hard to express at the call site.
func WithImport(as assets.Store, reg *adapter.Registry, queue JobQueue) Option {
	return func(s *Server) {
		s.assets = as
		s.adapters = reg
		s.jobQueue = queue
	}
}

// WithSessions wires the session runtime behind POST /api/v1/sessions, the
// session status routes and the dev-trace asset route (ADR-0024). The manager
// also owns the one-active-session invariant, so the API cannot express
// starting a second one even if a test or a future route tried.
func WithSessions(mgr *rt.Manager) Option {
	return func(s *Server) { s.sessions = mgr }
}

// WithDevEvents wires the durable event log behind the developer inspector's
// REST pages (GET /api/v1/dev/events, /api/v1/dev/traces/{trace_id}). The
// production call passes the same store the hub persists into; a nil store
// answers 503 rather than an empty inspector.
func WithDevEvents(st events.Store) Option {
	return func(s *Server) { s.devEvents = st }
}

// WithOCRPreview wires the region preview's OCR engine factory (one engine per
// page-segmentation mode) and the replay-root allowlist a `fixture` preview
// resolves against (ADR-0024's replay allowlist, reused for the preview's test
// frames). Production passes a real tesseract factory; tests inject a stub.
// Without it POST …/regions/preview answers 503 rather than an empty read.
func WithOCRPreview(factory ocrFactoryFunc, replayRoots []string) Option {
	return func(s *Server) { s.ocrPreview = ocrPreviewDeps{Factory: factory, ReplayRoots: replayRoots} }
}

// WithListen records the bound listen address for the /settings page's
// security section. The bound address — not the environment — is what an
// operator needs to answer "is this box reachable off-loopback?", so it
// outranks the fallback the page uses when the host did not wire it.
func WithListen(addr string) Option {
	return func(s *Server) { s.listenAddr = strings.TrimSpace(addr) }
}

// WithTestHarness wires the /tests page's dev harness to a repository root
// (docs/spec/09 "/tests"): the checkout whose testdata/scenarios the catalog
// lists and whose artifacts/gates the gate section renders. A process without
// it falls back to GAME_NARRATOR_REPO_ROOT and then its working directory;
// with no resolvable root the dev test routes answer 503 with the variable
// named, the same honest-degradation rule the other unwired subsystems follow.
func WithTestHarness(repoRoot string) Option {
	return func(s *Server) { s.harnessRepoRoot = strings.TrimSpace(repoRoot) }
}

// JobQueue is the slice of the durable queue this package uses: enqueue one job,
// learn whether it was new. *jobs.Runner satisfies it (the production wiring,
// because the runner is what publishes job.queued), and so does a bare
// *store.JobsStore for a test that wants the row without a worker.
type JobQueue interface {
	Enqueue(ctx context.Context, p store.EnqueueParams) (*store.Job, bool, error)
}

// WithTrustedProxy declares CIDR (or single-address) strings whose
// X-Forwarded-Proto may set the Secure cookie flag. An unparsable entry is
// logged and skipped rather than silently widening trust: the safe reading of a
// typo in a proxy allowlist is "trust nobody", and the operator sees the line in
// the startup log.
func WithTrustedProxy(cidrs ...string) Option {
	return func(s *Server) {
		for _, raw := range cidrs {
			raw = strings.TrimSpace(raw)
			if raw == "" {
				continue
			}
			if _, block, err := net.ParseCIDR(raw); err == nil {
				s.trustedProxies = append(s.trustedProxies, block)
				continue
			}
			if ip := net.ParseIP(raw); ip != nil {
				// A bare address is a host route: /32 for IPv4, /128 for IPv6.
				bits := "32"
				if strings.ContainsRune(raw, ':') {
					bits = "128"
				}
				if _, block, err := net.ParseCIDR(raw + "/" + bits); err == nil {
					s.trustedProxies = append(s.trustedProxies, block)
				}
				continue
			}
			s.log.Warn("ignoring an unparsable trusted-proxy entry",
				"entry", raw, "error", "not a CIDR or IP address")
		}
	}
}

// New constructs an API server. A zero BuildInfo or nil logger is tolerated.
// By default the UI comes from webassets.FS(), i.e. whatever Vite output was
// compiled into the binary.
func New(build bootstrap.BuildInfo, startedAt time.Time, log *slog.Logger, opts ...Option) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{
		build:       build,
		startedAt:   startedAt.UTC(),
		log:         log,
		webFS:       webassets.FS(),
		policy:      auth.DefaultPolicy(),
		idempotency: newIdempotencyStore(),
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
// authorization, embedded-UI serving and problem+json fallbacks.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	var routes routeTable
	// Public, in the sense of docs/spec/08's health/system contract: no game
	// content, no paths, no secrets. Every other route is granted a role by
	// auth.DefaultPolicy, keyed by these same method+path pairs —
	// TestEveryRegisteredRouteIsCoveredByTheScopeTable is what keeps the two lists from
	// drifting.
	routes.register(mux, http.MethodGet, "/api/v1/health/live", s.handleLive)
	routes.register(mux, http.MethodGet, "/api/v1/health/ready", s.handleReady)
	routes.register(mux, http.MethodGet, "/api/v1/system/info", s.handleSystemInfo)
	routes.register(mux, http.MethodGet, "/api/v1/events", s.developerScopedEvents(s.handleEvents))

	// Authentication and account management (docs/spec/08 "API version and user
	// authentication", docs/spec/14 "Authorization", ADR-0016). The grants live
	// in the scope table, not here, so the whole ACL is reviewable in one file.
	routes.register(mux, http.MethodGet, "/api/v1/auth/status", s.handleAuthStatus)
	routes.register(mux, http.MethodPost, "/api/v1/auth/setup", s.handleAuthSetup)
	routes.register(mux, http.MethodPost, "/api/v1/auth/login", s.handleLogin)
	routes.register(mux, http.MethodPost, "/api/v1/auth/logout", s.handleLogout)
	routes.register(mux, http.MethodGet, "/api/v1/auth/me", s.handleMe)
	routes.register(mux, http.MethodPost, "/api/v1/auth/password", s.handleChangePassword)
	routes.register(mux, http.MethodGet, "/api/v1/users", s.handleUsersList)
	routes.register(mux, http.MethodPost, "/api/v1/users", s.handleUsersCreate)
	routes.register(mux, http.MethodPost, "/api/v1/users/{id}/disable", s.handleUserDisable)
	routes.register(mux, http.MethodPost, "/api/v1/users/{id}/enable", s.handleUserEnable)
	routes.register(mux, http.MethodPost, "/api/v1/users/{id}/password", s.handleUserPasswordReset)
	routes.register(mux, http.MethodGet, "/api/v1/users/{id}/tokens", s.handleTokensList)
	routes.register(mux, http.MethodPost, "/api/v1/users/{id}/tokens", s.handleTokensCreate)
	routes.register(mux, http.MethodPost, "/api/v1/users/{id}/tokens/{tid}/revoke", s.handleTokenRevoke)

	// Durable workflow queue (docs/spec/08). Wildcard patterns match only
	// their method here; a wrong method on a concrete job path falls through
	// to the problem+json fallback as 404 — routeTable cannot expand
	// {id} patterns, a known narrowing noted for the router task.
	routes.register(mux, http.MethodGet, "/api/v1/jobs", s.handleJobsList)
	routes.register(mux, http.MethodGet, "/api/v1/jobs/{id}", s.handleJobGet)
	routes.register(mux, http.MethodPost, "/api/v1/jobs/{id}/cancel", s.handleJobCancel)
	routes.register(mux, http.MethodPost, "/api/v1/jobs/{id}/retry", s.handleJobRetry)

	// Session lifecycle (docs/spec/08 "Example session workflow", ADR-0024).
	// The runtime manager owns the one-active-session invariant, the published-
	// revision rule and the frame-source allowlist; the handlers map its
	// sentinel errors onto documented problem codes.
	routes.register(mux, http.MethodPost, "/api/v1/sessions", s.handleSessionsStart)
	routes.register(mux, http.MethodGet, "/api/v1/sessions/{id}", s.handleSessionsGet)
	routes.register(mux, http.MethodPost, "/api/v1/sessions/{id}/stop", s.handleSessionsStop)
	routes.register(mux, http.MethodPost, "/api/v1/sessions/{id}/audio", s.handleSessionsAudio)

	// Developer inspection (docs/spec/08 "APIs": dev/events, dev/traces; the
	// asset route is ADR-0024's sampled-evidence read path). All three sit
	// under the developer-only /api/v1/dev prefix of the scope table: the
	// payloads carry raw gameplay text, provider details and captured frames.
	routes.register(mux, http.MethodGet, "/api/v1/dev/events", s.handleDevEvents)
	// The developer's browsing of the recorded corrections as the re-run inputs
	// (docs/spec/08 "fixture recording and browsing", ADR-0052).
	routes.register(mux, http.MethodGet, "/api/v1/dev/fixtures", s.handleDevFixturesList)
	routes.register(mux, http.MethodGet, "/api/v1/dev/sessions/{id}/bottlenecks", s.handleDevSessionBottlenecks)
	routes.register(mux, http.MethodGet, "/api/v1/dev/traces/{trace_id}", s.handleDevTraces)
	routes.register(mux, http.MethodGet, "/api/v1/dev/assets/{sha256}", s.handleDevAsset)

	// Developer Dolphin bridge (docs/spec/11, ADR-0007): launch, status,
	// input, screenshot and stop for the isolated, dev-only emulator. The
	// scope table's /api/v1/dev prefix grant makes every one of these
	// developer-only; the opt-in env and the unwired-bridge 503 sit inside the
	// handlers (dolphin.go), not the scope table.
	routes.register(mux, http.MethodPost, "/api/v1/dev/dolphin/launch", s.handleDolphinLaunch)
	routes.register(mux, http.MethodGet, "/api/v1/dev/dolphin", s.handleDolphinStatus)
	routes.register(mux, http.MethodPost, "/api/v1/dev/dolphin/input", s.handleDolphinInput)
	routes.register(mux, http.MethodPost, "/api/v1/dev/dolphin/screenshot", s.handleDolphinScreenshot)
	routes.register(mux, http.MethodPost, "/api/v1/dev/dolphin/stop", s.handleDolphinStop)

	// The /tests page (docs/spec/09): the scenario catalog with run controls,
	// the recorded gate results, Dolphin/provider readiness, and the report
	// viewer's blob route. All under the developer-only /api/v1/dev prefix of
	// the scope table. The run route is a trailing wildcard because a scenario
	// id carries its directory (synthetic/dialogue-advance-001), which a
	// single-segment pattern cannot hold; the handler requires the final
	// segment to be "run".
	routes.register(mux, http.MethodGet, "/api/v1/dev/tests", s.handleDevTests)
	routes.register(mux, http.MethodPost, "/api/v1/dev/tests/scenarios/{rest...}", s.handleDevTestRun)
	routes.register(mux, http.MethodGet, "/api/v1/dev/tests/runs/{job_id}", s.handleDevTestRunStatus)
	routes.register(mux, http.MethodGet, "/api/v1/dev/tests/reports/{sha256}", s.handleDevTestReport)

	// The /settings page (docs/spec/09): the effective configuration, read-only
	// where it comes from the environment, with the backup download. Admin by
	// the scope table's grants below; PUT answers 405 on purpose (no setting
	// of this build is runtime-mutable — settings.go says so with the env
	// names the change would have to go through).
	routes.register(mux, http.MethodGet, "/api/v1/settings", s.handleSettingsGet)
	routes.register(mux, http.MethodPut, "/api/v1/settings", s.handleSettingsPut)
	routes.register(mux, http.MethodPost, "/api/v1/admin/backup", s.handleAdminBackup)

	// Onboarding agent (docs/spec/08 "APIs", ADR-0028): the run lives as a
	// durable agent.run job; this surface starts, chats with, watches and
	// approves it. The scope table grants the whole /api/v1/onboarding prefix
	// to curator and above (ADR-0028), so publication — reachable only
	// through .../approve — is a deliberate, role-gated act.
	routes.register(mux, http.MethodPost, "/api/v1/onboarding/runs", s.handleOnboardingCreate)
	routes.register(mux, http.MethodGet, "/api/v1/onboarding/runs/{id}", s.handleOnboardingState)
	routes.register(mux, http.MethodPost, "/api/v1/onboarding/runs/{id}/messages", s.handleOnboardingMessage)
	routes.register(mux, http.MethodPost, "/api/v1/onboarding/runs/{id}/pause", s.handleOnboardingPause)
	routes.register(mux, http.MethodPost, "/api/v1/onboarding/runs/{id}/resume", s.handleOnboardingResume)
	routes.register(mux, http.MethodPost, "/api/v1/onboarding/runs/{id}/cancel", s.handleOnboardingCancel)
	routes.register(mux, http.MethodPost, "/api/v1/onboarding/runs/{id}/approve", s.handleOnboardingApprove)
	routes.register(mux, http.MethodPost, "/api/v1/onboarding/uploads", s.handleOnboardingUpload)

	// Catalog, profile lifecycle and portability (docs/spec/08 "APIs": games,
	// editions, profiles, messages, characters, regions, import, export,
	// publish). Which role each one needs is the scope table's business, not this
	// file's; TestEveryRegisteredRouteIsCoveredByTheScopeTable and
	// TestOpenAPIDocMatchesTheRouter are what keep the router, the table and the
	// contract from drifting apart (ADR-0017).
	routes.register(mux, http.MethodGet, "/api/v1/games", s.handleGamesList)
	routes.register(mux, http.MethodPost, "/api/v1/games", s.handleGameCreate)
	routes.register(mux, http.MethodGet, "/api/v1/games/{id}", s.handleGameGet)
	routes.register(mux, http.MethodPatch, "/api/v1/games/{id}", s.handleGamePatch)
	routes.register(mux, http.MethodGet, "/api/v1/games/{id}/editions", s.handleEditionList)
	routes.register(mux, http.MethodPost, "/api/v1/games/{id}/editions", s.handleEditionCreate)
	routes.register(mux, http.MethodGet, "/api/v1/editions/{id}/profiles", s.handleProfileListForEdition)
	routes.register(mux, http.MethodPost, "/api/v1/editions/{id}/profiles", s.handleProfileDraftCreate)
	routes.register(mux, http.MethodPost, "/api/v1/editions/{id}/profiles/import-manifest", s.handleImportManifest)
	routes.register(mux, http.MethodGet, "/api/v1/editions/{id}/published", s.handlePublishedProfile)
	routes.register(mux, http.MethodGet, "/api/v1/profiles/{id}", s.handleProfileGet)
	routes.register(mux, http.MethodPost, "/api/v1/profiles/{id}/revisions", s.handleProfileRevision)
	routes.register(mux, http.MethodPost, "/api/v1/profiles/{id}/status", s.handleProfileStatus)
	routes.register(mux, http.MethodPost, "/api/v1/profiles/{id}/publish", s.handleProfilePublish)
	routes.register(mux, http.MethodGet, "/api/v1/profiles/{id}/publish-actions", s.handlePublishActions)
	routes.register(mux, http.MethodGet, "/api/v1/profiles/{id}/messages", s.handleMessagesList)
	routes.register(mux, http.MethodGet, "/api/v1/profiles/{id}/messages/search", s.handleMessageSearch)
	routes.register(mux, http.MethodGet, "/api/v1/profiles/{id}/messages/{mid}", s.handleMessageGet)
	routes.register(mux, http.MethodPatch, "/api/v1/profiles/{id}/messages/{mid}", s.handleMessagePatch)
	// The correction labels: the editor's speech-text save records one inside
	// the message write (ADR-0052), and this surface is where those labels are
	// read back and where explicit corrections are recorded (docs/spec/08
	// "user fixes become labels").
	routes.register(mux, http.MethodGet, "/api/v1/corrections", s.handleCorrectionsList)
	routes.register(mux, http.MethodPost, "/api/v1/corrections", s.handleCreateCorrection)
	routes.register(mux, http.MethodGet, "/api/v1/profiles/{id}/characters", s.handleProfileCharacters)
	// The curator's speaker-link action (ADR-0070): the identification of a
	// placeholder the import created for an opaque source speaker id, with
	// its lines, assertions, alias fold and retirement in one transaction.
	routes.register(mux, http.MethodPost, "/api/v1/profiles/{id}/characters/{cid}/speaker-link", s.handleSpeakerLink)
	routes.register(mux, http.MethodGet, "/api/v1/profiles/{id}/regions", s.handleProfileRegions)
	routes.register(mux, http.MethodPut, "/api/v1/profiles/{id}/regions", s.handleProfileRegionsPut)
	routes.register(mux, http.MethodPost, "/api/v1/profiles/{id}/regions/preview", s.handleProfileRegionsPreview)
	routes.register(mux, http.MethodGet, "/api/v1/profiles/{id}/graph", s.handleProfileGraph)
	routes.register(mux, http.MethodPost, "/api/v1/profiles/{id}/import", s.handleImportFile)
	routes.register(mux, http.MethodGet, "/api/v1/profiles/{id}/export", s.handleProfileExport)
	// The disc-import surface (ADR-0066): the images available in the
	// operator's allowlisted directory, and the 202 enqueue of one
	// import.disc job. The grants live in the scope table: /api/v1/import/*
	// and /api/v1/profiles/* are both curator-tier there.
	routes.register(mux, http.MethodGet, "/api/v1/import/disc/images", s.handleDiscImportImages)
	routes.register(mux, http.MethodPost, "/api/v1/profiles/{id}/import/disc", s.handleDiscImport)

	// The voice setup workflow (docs/spec/08 "APIs": voice list/auditions/
	// approve/register/preview). The 202 audition job runs as voices.JobKind;
	// which role each route needs is the scope table's business.
	routes.register(mux, http.MethodGet, "/api/v1/voices", s.handleVoiceList)
	routes.register(mux, http.MethodPost, "/api/v1/voices/auditions", s.handleVoiceAuditionsCreate)
	routes.register(mux, http.MethodGet, "/api/v1/voices/{id}", s.handleVoiceGet)
	routes.register(mux, http.MethodGet, "/api/v1/voices/{id}/auditions/{aid}", s.handleVoiceAuditionAudio)
	routes.register(mux, http.MethodPost, "/api/v1/voices/{id}/approve", s.handleVoiceApprove)
	routes.register(mux, http.MethodPost, "/api/v1/voices/{id}/register", s.handleVoiceRegister)
	routes.register(mux, http.MethodPost, "/api/v1/voices/{id}/preview", s.handleVoicePreview)
	routes.register(mux, http.MethodGet, "/api/v1/voices/{id}/registrations", s.handleVoiceRegistrations)

	// The machine-readable contract itself. Public: it is how a client learns to
	// authenticate, and it carries no game content and no host paths.
	routes.register(mux, http.MethodGet, "/api/v1/openapi.json", s.handleOpenAPI)

	// "/" is the least specific pattern, so it only receives requests that no
	// method+path pattern above matched.
	mux.Handle("/", s.fallback(routes))
	s.routes = routes

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

	// Order: correlation first (a 401 and a 403 must carry a request_id like
	// any other response), then authorization, then routing. Authorization runs
	// on the whole dispatch handler rather than on the mux alone because the
	// fail-closed default has to cover /api/ paths that match no route at all —
	// an unknown API path is not information to hand an unauthenticated caller.
	return withRequestID(s.log, s.authorize(routes, dispatch))
}

// handleLive reports process liveness only: no dependency probing.
func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// healthResponse is the readiness payload. checks stays a map (initially empty)
// so dependency checks are additive, and each entry is an object rather than a
// bare string so a check can carry evidence such as schema_version.
type healthResponse struct {
	Status string         `json:"status"`
	Checks map[string]any `json:"checks"`
}

// readinessTimeout bounds one readiness probe. A stuck dependency must make the
// endpoint answer "not ready" quickly rather than hold the request open.
const readinessTimeout = 2 * time.Second

// handleReady reports dependency readiness. With no checker wired it reports an
// empty check map, which is the honest "nothing is wired yet" state; with one,
// a failing dependency yields HTTP 503 and "status":"fail" (docs/spec/08,
// docs/spec/13 "no generic success on an unprobed service").
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	checks, ok := map[string]any{}, true
	if s.readiness != nil {
		ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
		defer cancel()
		var body map[string]any
		// A panicking checker must not take the process down: an unreachable
		// dependency is exactly what a readiness probe exists to report.
		body, ok = safeReadiness(ctx, s.readiness, s.log)
		if body != nil {
			checks = body
		}
	}
	status, code := "ok", http.StatusOK
	if !ok {
		status, code = "fail", http.StatusServiceUnavailable
	}
	writeJSON(w, code, healthResponse{Status: status, Checks: checks})
}

func safeReadiness(ctx context.Context, f ReadinessFunc, log *slog.Logger) (checks map[string]any, ok bool) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Error("readiness checker panicked", "panic", rec)
			checks, ok = nil, false
		}
	}()
	return f(ctx)
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
