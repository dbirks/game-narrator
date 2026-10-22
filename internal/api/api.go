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
	"github.com/dbirks/game-narrator/internal/bt"
	"github.com/dbirks/game-narrator/internal/capture/stack"
	"github.com/dbirks/game-narrator/internal/dolphin"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games/adapter"
	"github.com/dbirks/game-narrator/internal/logbuf"
	rt "github.com/dbirks/game-narrator/internal/runtime"
	"github.com/dbirks/game-narrator/internal/setupboard"
	"github.com/dbirks/game-narrator/internal/store"
	"github.com/dbirks/game-narrator/internal/telemetry"
	"github.com/dbirks/game-narrator/internal/updates"
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

	// jobRunner is the seam the status snapshot and the job list read the
	// background queue's foreground pause through (ADR-0119, issue #27):
	// *jobs.Runner satisfies it with ForegroundPause, and a test wires a
	// fake. nil (a process whose runner is not handed to the API) shows
	// no pause line — the pause exists only on the process whose runner
	// owns it, and an API that invented the line would be a lie.
	jobRunner jobPauseReporter

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

	// clipsAssets and clipsStoreDir back the managed replay clip store
	// (ADR-0073): /api/v1/replay/clips commits the uploaded archive to
	// clipsAssets for provenance and installs it under clipsStoreDir, which
	// the session runtime always treats as a replay root. An unwired half
	// answers 503: a store that answered "no clips" because it is not
	// configured would hide the configuration fact the operator needs.
	clipsAssets   assets.Store
	clipsStoreDir string

	// sessions owns the session lifecycle API (sessions.go): start, status and
	// stop, plus the dev-trace asset lookup the developer inspector's asset
	// route reads through. nil answers 503 for the same honest-degradation
	// reason as every other unwired subsystem: an empty session list would
	// claim an appliance with no runtime.
	sessions *rt.Manager

	// capture is the UVC HDMI capture device layer (ADR-0078): the device
	// list and selection (GET/PUT /api/v1/capture/devices[/{id}]), the
	// throttled preview snapshot (GET /api/v1/capture/preview) and the capture
	// probe (POST /api/v1/capture/probe). The selection/operation logic lives
	// in internal/capture/stack so the session runtime and the API resolve
	// one spelling of the same device and mode. nil answers 503 for the same
	// honest-degradation reason as every other unwired subsystem: an empty
	// device list would claim an appliance with no capture hardware.
	capture *stack.Stack
	// capturePreviewInFlight and capturePreviewLastNano are the preview's
	// global throttle state (ADR-0078 §3, shared with the signal route's
	// one-shot mode by ADR-0110): one snapshot in flight, a 500 ms minimum
	// interval, shared by every request because the budget is the decode
	// cores and the USB bus, not the caller.
	capturePreviewInFlight int32
	capturePreviewLastNano int64

	// devEvents is the durable event log behind the developer inspector's REST
	// pages (dev.go). It is the same store the hub persists into, so the pages
	// show exactly what the SSE stream would have shown, minus the live tail.
	// nil answers 503 on /api/v1/dev/events and /api/v1/dev/traces.
	devEvents events.Store

	// onb wires the onboarding agent routes (onboarding.go): the durable
	// queue the runs execute as, the run log and the human approval tokens.
	// nil answers 503 under /api/v1/onboarding (ADR-0028).
	onb *onbDeps

	// setupBoard wires the setup board snapshot (onboarding_board.go,
	// ADR-0108, UX-06): the GET .../board read that reconstructs the board a
	// client would assemble from the SSE stage events. nil answers 503 on
	// that one route only; the rest of the onboarding surface and the event
	// stream are unaffected.
	setupBoard *setupboard.Emitter

	// assistantDeps wires the context-aware assistant routes (assistant.go,
	// ADR-0099): the thread→run ownership tables, the durable queue the
	// attempts run as and the tool backends (voices, sessions, event log).
	// nil answers 503 under /api/v1/assistant; a nil tool backend degrades
	// the tool that needs it to a failed envelope, not the whole route.
	assistantDeps *assistantDeps

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

	// tailnet is the cached tailnet detection (UX-13, ADR-0110 §4): detected
	// once at process startup (cmd/game-narrator runs DetectTailnet before the
	// listener comes up and injects the value through WithTailnet), so no
	// request ever touches the network namespace or the `tailscale` CLI. nil
	// means the host did not run the detection — the surfaces report the
	// explicit "not detected by this process" state rather than a blank.
	tailnet *TailnetView

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

	// updatesChecker is the appliance's update notification (ADR-0088): a
	// bounded check against the releases of the repository this build belongs
	// to, with the offline cache in the data directory. nil means this process
	// has no update check wired (test servers): the /settings page keeps its
	// static note and the refresh route answers 503.
	updatesChecker *updates.Checker

	// restarter is the process-replacement seam behind POST /api/v1/admin/restore
	// (ADR-0090): the handler schedules the restore plan and answers, and this
	// function then replaces the process image (same arguments) so the boot
	// applies the plan before the listener comes up. nil means this process
	// cannot restart in place (test servers): the route answers 503.
	restarter func() error

	// diagRuns is the bounded in-memory store of diagnostics runs (ADR-0087).
	// It is internal state, not wiring, so New builds it and it always exists —
	// the route degrades on the probe seams, not on the store.
	diagRuns *diagnosticsRunStore

	// logRing is the process's log tail (ADR-0087, docs/spec/09 /diagnostics
	// "system logs (redacted)"). nil means no ring is wired and the logs route
	// answers 503 — the honest-degradation rule every unwired subsystem follows.
	logRing *logbuf.Ring

	// btController is the Bluetooth surface (ADR-0087, issues #2/#3): the
	// BlueZ D-Bus client on the appliance, the labelled-synthetic fake under
	// GAME_NARRATOR_BT_FAKE, nil in builds without it. nil answers 503 under
	// /api/v1/audio/bluetooth (the section hides itself there). The rate limit
	// is shared by every mutation (the inquiry channel is one radio, not a
	// per-caller budget) and is internal state, like the idempotency store.
	btController bt.Controller
	btLimiter    *auth.Limiter

	// metrics is the process's telemetry surface (docs/spec/13 "Metrics",
	// internal/telemetry): the catalog the hot paths book into and the two
	// scrape endpoints serve. nil (the default) is the off shape — the
	// endpoints answer 404 (the scraper endpoint) and 503 (the developer
	// snapshot) rather than invent numbers, and the hooks the runtime and
	// pipeline call against it are nil-safe. metricsToken is the optional
	// static bearer token the scraper endpoint accepts in place of an admin
	// session, for a monitor that does not want to mint a user token.
	metrics      *telemetry.Product
	metricsToken string

	// The status bar's backend (docs/design/plum-peach "Global chrome").
	// The two caches are internal state like diagRuns — built in New, always
	// present — and the engine is the process's single push loop, started
	// lazily by the stream route (a process whose clients poll by GET pays
	// no push at all). The stream degrades on the hub seam (503, like
	// /events); the GET never does: its seams degrade to "unknown" and "no
	// active session", which are the honest shapes.
	statusMu         sync.Mutex
	statusEng        *statusEngine
	statusEngineOnce sync.Once
	gameTitle        *gameTitleCache
	statusReadiness  *statusReadinessCache
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

// WithJobRunner wires the background queue's foreground pause (ADR-0119,
// issue #27) into the status snapshot and the job list: while a live session
// holds the appliance's CPU/audio budget (docs/spec/01 invariant 1), both
// surfaces carry {paused_by: "live_session", since, queued_count}, so a
// poller can tell "waiting in a normal queue" from "blocked for the
// lifetime of the session". *jobs.Runner satisfies the seam.
func WithJobRunner(r jobPauseReporter) Option {
	return func(s *Server) { s.jobRunner = r }
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

// WithCapture wires the UVC HDMI capture device layer (ADR-0078) behind the
// capture routes: the device list and selection, the throttled preview and
// the capture probe. The stack owns the selection and the source builder, so
// the session runtime and the API agree on which device and mode is opened.
// nil answers 503 under /api/v1/capture for the same honest-degradation
// reason as every other unwired subsystem.
func WithCapture(st *stack.Stack) Option {
	return func(s *Server) { s.capture = st }
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

// WithTailnet injects the startup-cached tailnet detection (UX-13,
// ADR-0110 §4): the value the process detected before the listener came up
// (a 100.64.0.0/10 interface address, or `tailscale ip` when the scan
// found nothing), served from the cache for the process's life — never
// re-detected at request time. The settings security section and the
// system-info surface report it; it crosses no other boundary (the update
// checker, the providers and the support bundle receive no seam that
// carries it, so it is never sent to an external service).
func WithTailnet(v TailnetView) Option {
	return func(s *Server) { s.tailnet = &v }
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

// WithUpdates wires the update check (ADR-0088) behind the /settings page's
// version-and-updates section and the refresh route. A nil checker keeps the
// page's static note and makes the refresh route answer 503.
func WithUpdates(c *updates.Checker) Option {
	return func(s *Server) { s.updatesChecker = c }
}

// WithRestarter wires the process-replacement seam behind
// POST /api/v1/admin/restore (ADR-0090). The handler answers first and calls
// it after; a nil restarter makes the route answer 503.
func WithRestarter(f func() error) Option {
	return func(s *Server) { s.restarter = f }
}

// WithLogRing wires the process's log tail behind GET /api/v1/diagnostics/logs
// (ADR-0087): the redacted recent system logs. A nil ring keeps the route
// answering 503 — a logs section that shows an empty tail because nobody wired
// a ring would read as "no errors", which is the lie docs/spec/13 forbids.
func WithLogRing(ring *logbuf.Ring) Option {
	return func(s *Server) { s.logRing = ring }
}

// WithBluetoothController wires the Bluetooth output surface (ADR-0087,
// issues #2/#3) behind /api/v1/audio/bluetooth: the BlueZ D-Bus client on the
// appliance, the labelled-synthetic fake in the test seams. nil keeps the
// routes answering 503 — the page's section hides itself on available=false.
func WithBluetoothController(c bt.Controller) Option {
	return func(s *Server) { s.btController = c }
}

// WithBluetoothLimiter replaces the shared mutation bucket the Bluetooth
// routes draw on (tests install a small bucket to reach the 429 without
// burning the real window; production keeps the default from New).
func WithBluetoothLimiter(l *auth.Limiter) Option {
	return func(s *Server) { s.btLimiter = l }
}

// WithMetrics wires the telemetry surface (docs/spec/13 "Metrics",
// internal/telemetry) behind the two scrape endpoints: GET /metrics in
// Prometheus text format for a monitor, and GET /api/v1/dev/metrics as the
// JSON snapshot the diagnostics and health surfaces read. Off by default —
// a server that never calls this answers 404 at /metrics and 503 at the
// developer snapshot, so a process that wired no catalog cannot claim to be
// measuring. When wired, /metrics is gated to an admin principal (the usual
// session or API token) or the static token (the second argument, empty to
// require admin only), because a raw metrics stream on a LAN is an
// unauthenticated read of the appliance's internals (docs/spec/14).
func WithMetrics(p *telemetry.Product, token string) Option {
	return func(s *Server) {
		s.metrics = p
		s.metricsToken = strings.TrimSpace(token)
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
		diagRuns:    newDiagnosticsRunStore(),
		// The status bar's caches (status.go): internal state, so New builds
		// them and they always exist.
		gameTitle:       newGameTitleCache(),
		statusReadiness: &statusReadinessCache{},
		// The Bluetooth mutation budget (bt.go): internal state, so it always
		// exists — the route degrades on the controller seam, not on it.
		btLimiter: auth.NewLimiter(btLimiterCapacity, btLimiterWindow, time.Now),
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.webFS == nil {
		s.webFS = emptyFS{}
	}
	// The status bar's event name joins the hub's closed vocabulary the way
	// every other subsystem's extra names do (the jobs runner, the region
	// preview, the runtime's session signals): registered once here, before
	// any handler can publish one, so a publish of status.changed is
	// validated like every other event and an unknown name stays unknown.
	if err := events.Register(statusEventName); err != nil {
		log.Error("status: cannot register the status event name", "error", err)
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
	// The global status bar's backend (docs/design/plum-peach "Global
	// chrome"): the snapshot read and its on-change push (status.changed
	// through the same hub the event stream uses). Both are viewer reads —
	// the bar is on every screen, including the kiosk viewer's.
	routes.register(mux, http.MethodGet, "/api/v1/status", s.handleStatus)
	routes.register(mux, http.MethodGet, "/api/v1/status/stream", s.handleStatusStream)
	// The ⌘K command-palette search (ADR-0098, docs/design/plum-peach UX-02):
	// one viewer read that groups games, characters, lines, traces, settings
	// sections and actions. The route is viewer like the status bar it sits
	// next to; which result kinds a caller actually gets is the handler's
	// business (traces developer-only, settings and verbs role-filtered).
	routes.register(mux, http.MethodGet, "/api/v1/search", s.handleSearch)
	// The diagnostics surface (ADR-0087, docs/spec/09 /diagnostics): the public
	// host vitals, the model-role probe runs (the scope table's two reserved
	// /api/v1/diagnostics grants, the concrete /logs path registered before the
	// {id} template so it wins) and the redacted log tail.
	routes.register(mux, http.MethodGet, "/api/v1/diagnostics/host", s.handleDiagnosticsHost)
	routes.register(mux, http.MethodGet, "/api/v1/diagnostics/logs", s.handleDiagnosticsLogs)
	routes.register(mux, http.MethodPost, "/api/v1/diagnostics/run", s.handleDiagnosticsRun)
	routes.register(mux, http.MethodGet, "/api/v1/diagnostics/{id}", s.handleDiagnosticsGet)
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
	// The output-audio product surface (ADR-0074, gap audit G5): the
	// enumerated devices with the effective selection, and the consent-gated
	// deterministic test tone through the same sinks the session plays.
	routes.register(mux, http.MethodGet, "/api/v1/audio/devices", s.handleAudioDevicesGet)
	routes.register(mux, http.MethodPost, "/api/v1/audio/test-tone", s.handleAudioTestTone)
	// The Bluetooth output surface (ADR-0087, issues #2/#3): the adapter
	// state, the time-bounded scan, the audio-capable inventory and the
	// pairing lifecycle (pair+trust, connect, disconnect, forget, and the
	// re-pair repair for the LE-only "no audio sink" state). Every route sits
	// under the scope table's /api/v1/audio/{rest...} operator grant — the
	// "act on the physical world" tier, like capture — and the selection
	// itself is the operator/admin audio settings route above, which persists
	// the sink identity bluez_output.<MAC>.1.
	routes.register(mux, http.MethodGet, "/api/v1/audio/bluetooth/state", s.handleBtState)
	routes.register(mux, http.MethodPost, "/api/v1/audio/bluetooth/scan", s.handleBtScan)
	routes.register(mux, http.MethodGet, "/api/v1/audio/bluetooth/devices", s.handleBtDevices)
	routes.register(mux, http.MethodPost, "/api/v1/audio/bluetooth/devices/{mac}/pair", s.btDeviceAction("pair", btPairBound, "the device is paired and trusted: connect it to bring up its audio sink", func(c bt.Controller, ctx context.Context, mac string) error { return c.Pair(ctx, mac) }))
	routes.register(mux, http.MethodPost, "/api/v1/audio/bluetooth/devices/{mac}/trust", s.btDeviceAction("trust", btForgetBound, "the device is marked trusted: bluez will connect it without further confirmation", func(c bt.Controller, ctx context.Context, mac string) error { return c.Trust(ctx, mac, true) }))
	routes.register(mux, http.MethodPost, "/api/v1/audio/bluetooth/devices/{mac}/connect", s.btDeviceAction("connect", btConnectBound, "the device is connected: its A2DP sink is now an audio output (bluez_output.<MAC>.1)", func(c bt.Controller, ctx context.Context, mac string) error { return c.Connect(ctx, mac) }))
	routes.register(mux, http.MethodPost, "/api/v1/audio/bluetooth/devices/{mac}/disconnect", s.btDeviceAction("disconnect", btConnectBound, "the device is disconnected", func(c bt.Controller, ctx context.Context, mac string) error { return c.Disconnect(ctx, mac) }))
	routes.register(mux, http.MethodPost, "/api/v1/audio/bluetooth/devices/{mac}/forget", s.btDeviceAction("forget", btForgetBound, "the device and its pairing information were removed: pair it again to use it", func(c bt.Controller, ctx context.Context, mac string) error { return c.Forget(ctx, mac) }))
	routes.register(mux, http.MethodPost, "/api/v1/audio/bluetooth/devices/{mac}/repair", s.btDeviceAction("repair", btRepairBound, "the device was re-paired over the classic transport and reconnected: its A2DP sink should be up (the headset must have been in pairing mode for the rescan)", func(c bt.Controller, ctx context.Context, mac string) error { return c.Repair(ctx, mac) }))
	// The UVC HDMI capture device surface (ADR-0078): the device list with
	// the negotiated link and the chosen mode, the persisted selection, the
	// throttled preview snapshot and the capture probe. The scope table grants
	// /api/v1/capture/{rest...} to operator (a capture preview and a session
	// source are "act on the physical world" actions, docs/spec/10).
	routes.register(mux, http.MethodGet, "/api/v1/capture/devices", s.handleCaptureDevices)
	routes.register(mux, http.MethodPut, "/api/v1/capture/devices/{id}", s.handleCaptureSelect)
	routes.register(mux, http.MethodGet, "/api/v1/capture/preview", s.handleCapturePreview)
	routes.register(mux, http.MethodGet, "/api/v1/capture/signal", s.handleCaptureSignal)
	routes.register(mux, http.MethodPost, "/api/v1/capture/probe", s.handleCaptureProbe)
	// The operator's live controls of docs/spec/08 ("…/pause, …/resume,
	// …/settings"): pause/resume stop and restart the commitment and the
	// speech, and settings changes the narrated classes and the decision mode
	// mid-session. All three are operator (the session-scoped scope grant),
	// and each emits its own session event on the session's chain.
	routes.register(mux, http.MethodPost, "/api/v1/sessions/{id}/pause", s.handleSessionsPause)
	routes.register(mux, http.MethodPost, "/api/v1/sessions/{id}/resume", s.handleSessionsResume)
	routes.register(mux, http.MethodPut, "/api/v1/sessions/{id}/settings", s.handleSessionsSettings)

	// Developer inspection (docs/spec/08 "APIs": dev/events, dev/traces; the
	// asset route is ADR-0024's sampled-evidence read path). All three sit
	// under the developer-only /api/v1/dev prefix of the scope table: the
	// payloads carry raw gameplay text, provider details and captured frames.
	routes.register(mux, http.MethodGet, "/api/v1/dev/events", s.handleDevEvents)
	// The /metrics scrape (docs/spec/13 "Metrics"): registered on the mux but
	// deliberately NOT on the route table — the scope table and the OpenAPI
	// contract track /api/v1 routes and their role grants, and this endpoint
	// is outside both: its gate (admin or the configured token) is the
	// handler's own, and the dispatch special-case below keeps the embedded
	// UI from answering for it. Off by default: with no wired catalog the
	// handler answers 404.
	mux.HandleFunc("GET "+metricsPath, s.handleMetrics)

	// The developer's browsing of the recorded corrections as the re-run inputs
	// (docs/spec/08 "fixture recording and browsing", ADR-0052).
	routes.register(mux, http.MethodGet, "/api/v1/dev/metrics", s.handleDevMetrics)
	routes.register(mux, http.MethodGet, "/api/v1/dev/fixtures", s.handleDevFixturesList)
	routes.register(mux, http.MethodGet, "/api/v1/dev/sessions/{id}/bottlenecks", s.handleDevSessionBottlenecks)
	routes.register(mux, http.MethodGet, "/api/v1/dev/traces/{trace_id}", s.handleDevTraces)
	routes.register(mux, http.MethodGet, "/api/v1/dev/assets/{sha256}", s.handleDevAsset)
	// The developer inspector's per-event actions (ADR-0083): the trace
	// fixture recorded into the external fixture root (the POST half of the
	// fixture pair above), the stored observation's re-run with its diff, the
	// trace's redacted evidence bundle, and one utterance's stored audio.
	routes.register(mux, http.MethodPost, "/api/v1/dev/fixtures", s.handleDevFixtureRecord)
	routes.register(mux, http.MethodPost, "/api/v1/dev/traces/{trace_id}/rerun", s.handleDevTraceRerun)
	routes.register(mux, http.MethodGet, "/api/v1/dev/traces/{trace_id}/evidence", s.handleDevTraceEvidence)
	routes.register(mux, http.MethodGet, "/api/v1/dev/speech/audio", s.handleDevSpeechAudio)

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
	// the scope table's grants below; PUT answers 405 on purpose for the
	// general settings (the one runtime-mutable setting, the audio output,
	// has its own route — settings.go says so with the env names the change
	// would have to go through).
	routes.register(mux, http.MethodGet, "/api/v1/settings", s.handleSettingsGet)
	routes.register(mux, http.MethodPut, "/api/v1/settings", s.handleSettingsPut)
	// The one runtime-mutable setting (ADR-0074): the audio output selection
	// and master volume persist in the store; the env variables keep their
	// override precedence (internal/audio.Resolve documents it).
	routes.register(mux, http.MethodPut, "/api/v1/settings/audio", s.handleAudioSettingsPut)
	routes.register(mux, http.MethodPost, "/api/v1/admin/backup", s.handleAdminBackup)
	// The restore half of the backup/restore pair (ADR-0090): the admin uploads
	// a backup produced by the route above; the handler validates it, takes
	// the automatic pre-restore backup, schedules the plan and restarts the
	// process so the boot applies it before the listener comes up. GET is the
	// post-restart status read (the UI polls it after the restart).
	routes.register(mux, http.MethodPost, "/api/v1/admin/restore", s.handleAdminRestore)
	routes.register(mux, http.MethodGet, "/api/v1/admin/restore", s.handleAdminRestoreStatus)
	// The update notification's manual re-check (ADR-0088): the settings
	// page's "check now" button. Bounded by the checker's own timeout, so a
	// hung endpoint cannot hold the page open.
	routes.register(mux, http.MethodPost, "/api/v1/settings/updates/refresh", s.handleSettingsUpdatesRefresh)

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
	routes.register(mux, http.MethodPost, "/api/v1/onboarding/runs/{id}/abandon", s.handleOnboardingAbandon)
	routes.register(mux, http.MethodPost, "/api/v1/onboarding/runs/{id}/approve", s.handleOnboardingApprove)
	routes.register(mux, http.MethodPost, "/api/v1/onboarding/uploads", s.handleOnboardingUpload)
	// The setup board snapshot (ADR-0108, UX-06): the page-load half of the
	// board the SSE stage events drive live. The scope table grants the whole
	// /api/v1/onboarding prefix, so this GET rides the same grant.
	routes.register(mux, http.MethodGet, "/api/v1/onboarding/runs/{id}/board", s.handleOnboardingBoard)

	// Context-aware assistant (ADR-0099, docs/design/plum-peach UX-01/04/05):
	// the agent dock's backend. One thread is one durable agent run; a
	// message appends a turn and enqueues an assistant.run attempt, and the
	// reply streams over the event hub (the loop's step/tool events plus
	// assistant.message and assistant.question). The scope table grants the
	// whole prefix to operator and up: every mutation the dock can trigger
	// is confirmation-gated or a catalogue draft write, and nothing here
	// publishes.
	routes.register(mux, http.MethodPost, "/api/v1/assistant/messages", s.handleAssistantMessages)
	routes.register(mux, http.MethodPost, "/api/v1/assistant/answers", s.handleAssistantAnswers)
	routes.register(mux, http.MethodGet, "/api/v1/assistant/threads", s.handleAssistantThreads)
	routes.register(mux, http.MethodGet, "/api/v1/assistant/threads/{id}", s.handleAssistantThread)

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
	// The /library screen's per-game read (docs/design/plum-peach 2a,
	// ADR-0140): the actionable revision plus the counts the shelf tiles
	// and list rows render. A viewer read — the shelf is the kiosk's front
	// door — and the scope table grants it before the curator wildcard
	// that owns the rest of the game subtree.
	routes.register(mux, http.MethodGet, "/api/v1/games/{id}/library", s.handleGameLibrary)
	routes.register(mux, http.MethodPost, "/api/v1/games/{id}/editions", s.handleEditionCreate)
	routes.register(mux, http.MethodGet, "/api/v1/editions/{id}/profiles", s.handleProfileListForEdition)
	routes.register(mux, http.MethodPost, "/api/v1/editions/{id}/profiles", s.handleProfileDraftCreate)
	routes.register(mux, http.MethodPost, "/api/v1/editions/{id}/profiles/import-manifest", s.handleImportManifest)
	routes.register(mux, http.MethodGet, "/api/v1/editions/{id}/published", s.handlePublishedProfile)
	routes.register(mux, http.MethodGet, "/api/v1/profiles/{id}", s.handleProfileGet)
	routes.register(mux, http.MethodPost, "/api/v1/profiles/{id}/revisions", s.handleProfileRevision)
	routes.register(mux, http.MethodPost, "/api/v1/profiles/{id}/status", s.handleProfileStatus)
	routes.register(mux, http.MethodPost, "/api/v1/profiles/{id}/narrator-fallback", s.handleNarratorFallback)
	// The per-profile auto-cast dial (ADR-0109, UX-03): the same editorial
	// setting tier as narrator-fallback — a curator write, not content.
	routes.register(mux, http.MethodPost, "/api/v1/profiles/{id}/auto-cast", s.handleAutoCast)
	routes.register(mux, http.MethodPost, "/api/v1/profiles/{id}/publish", s.handleProfilePublish)
	routes.register(mux, http.MethodGet, "/api/v1/profiles/{id}/publish-actions", s.handlePublishActions)
	// Library lifecycle (ADR-0107, gap G14): duplicate and clone a revision,
	// disable and re-enable a game, soft-delete a revision and purge it once
	// the retention window closes. Which role each needs is the scope table's
	// business — the admin-only purge is the explicit grant there, placed
	// before the curator wildcard that would otherwise swallow it.
	routes.register(mux, http.MethodPost, "/api/v1/profiles/{id}/duplicate", s.handleProfileDuplicate)
	routes.register(mux, http.MethodPost, "/api/v1/profiles/{id}/clone", s.handleProfileClone)
	routes.register(mux, http.MethodDelete, "/api/v1/profiles/{id}", s.handleProfileDelete)
	routes.register(mux, http.MethodPost, "/api/v1/profiles/{id}/purge", s.handleProfilePurge)
	routes.register(mux, http.MethodPost, "/api/v1/profiles/{id}/presynthesize", s.handleProfilePresynthesize)
	routes.register(mux, http.MethodPost, "/api/v1/games/{id}/disable", s.handleGameDisableRoute)
	routes.register(mux, http.MethodPost, "/api/v1/games/{id}/enable", s.handleGameEnableRoute)
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

	// The managed replay clip store (ADR-0073): an archive of the replay
	// layout streams into the asset store under its content hash and installs
	// under the managed directory the session runtime always treats as a
	// replay root, so a recorded clip is installable by API — the sustained
	// device script used to get there by editing /etc/game-narrator/env and
	// restarting, the operator footgun this surface removes. The grant is
	// developer in the scope table because a clip is a recording of gameplay
	// frames — the same sensitive-evidence tier as the dev traces (docs/spec/08)
	// — and an admin passes by the role order, which is how the device scripts
	// reach it with the token they already mint.
	routes.register(mux, http.MethodPost, "/api/v1/replay/clips", s.handleClipsUpload)
	routes.register(mux, http.MethodGet, "/api/v1/replay/clips", s.handleClipsList)
	routes.register(mux, http.MethodDelete, "/api/v1/replay/clips/{id}", s.handleClipsDelete)

	// The voice setup workflow (docs/spec/08 "APIs": voice list/auditions/
	// approve/register/preview). The 202 audition job runs as voices.JobKind;
	// which role each route needs is the scope table's business.
	routes.register(mux, http.MethodGet, "/api/v1/voices", s.handleVoiceList)
	routes.register(mux, http.MethodPost, "/api/v1/voices/auditions", s.handleVoiceAuditionsCreate)
	routes.register(mux, http.MethodGet, "/api/v1/voices/{id}", s.handleVoiceGet)
	routes.register(mux, http.MethodGet, "/api/v1/voices/{id}/auditions/{aid}", s.handleVoiceAuditionAudio)
	routes.register(mux, http.MethodPost, "/api/v1/voices/{id}/approve", s.handleVoiceApprove)
	// The one-step "keep old take" after a regeneration (ADR-0109, UX-03d):
	// curation, like approve — it moves a voice revision's state.
	routes.register(mux, http.MethodPost, "/api/v1/voices/{id}/undo", s.handleVoiceUndo)
	routes.register(mux, http.MethodPost, "/api/v1/voices/{id}/register", s.handleVoiceRegister)
	routes.register(mux, http.MethodPost, "/api/v1/voices/{id}/preview", s.handleVoicePreview)
	routes.register(mux, http.MethodGet, "/api/v1/voices/{id}/registrations", s.handleVoiceRegistrations)
	// The per-voice output gain (ADR-0074, docs/spec/07 "per-voice gain"): an
	// output property applied at playback, never by re-synthesis.
	routes.register(mux, http.MethodPost, "/api/v1/voices/{id}/gain", s.handleVoiceGain)

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
		// The /metrics scrape routes ahead of everything: it is not an /api/ route
		// (so the authorisation middleware skips it, and its gate is the
		// handler's own) and it is not an embedded asset (the SPA's index.html
		// is the wrong body for a scraper, and for the not-enabled 404).
		if s.metricsRoute(r) {
			mux.ServeHTTP(w, r)
			return
		}
		// The capture device ids (by-id / by-path) are full device paths, so
		// they contain slashes. The contract is a percent-encoded id in the
		// path (a %2F-escaped segment the mux routes to {id} and decodes); a
		// client that sends the raw id instead arrives here as a multi-segment
		// path the mux would normalize with a 307 before a 404 (issue #10's
		// field observation). Answer the miss with the actionable 400 instead.
		// The test is on the escaped path: an encoded id carries %2F there, a
		// raw id carries literal "/". Only the select route takes an id in the
		// path (the preview and probe carry it as a query/body field).
		if r.Method == http.MethodPut {
			const captureSelectPrefix = "/api/v1/capture/devices/"
			if rest, ok := strings.CutPrefix(r.URL.EscapedPath(), captureSelectPrefix); ok && strings.Contains(rest, "/") {
				s.writeProblem(w, r, http.StatusBadRequest, "device-id-not-encoded",
					"Bad Request", "the capture device id in the path contains raw slashes: by-id and by-path ids are full device paths, so the id must be percent-encoded in the URL (a '/' becomes %2F, as the web client's URL encoder does); GET /api/v1/capture/devices reports the ids")
				return
			}
		}
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

// systemInfoResponse exposes build/OS capacity plus the network reachability
// the settings surface needs (UX-13, ADR-0110 §4): the bound listen
// address(es) and the cached tailnet detection. Still no hostnames, paths,
// environment values or secrets — the tailnet address is a CGNAT-space IP
// the UI renders, not a credential, and it is served from the startup cache,
// never re-detected (and never forwarded) at request time.
type systemInfoResponse struct {
	Version         string       `json:"version"`
	Commit          string       `json:"commit"`
	GoVersion       string       `json:"go_version"`
	OS              string       `json:"os"`
	Arch            string       `json:"arch"`
	NumCPU          int          `json:"num_cpu"`
	StartedAt       string       `json:"started_at"`
	ListenAddresses []string     `json:"listen_addresses"`
	Tailnet         *TailnetView `json:"tailnet"`
}

func (s *Server) handleSystemInfo(w http.ResponseWriter, r *http.Request) {
	listen := s.listenAddr
	if listen == "" {
		listen = bootstrap.ResolveListen("", osLookupEnv)
	}
	writeJSON(w, http.StatusOK, systemInfoResponse{
		Version:         s.build.Version,
		Commit:          s.build.Commit,
		GoVersion:       s.build.GoVersion,
		OS:              s.build.GOOS,
		Arch:            s.build.GOARCH,
		NumCPU:          runtime.NumCPU(),
		StartedAt:       s.startedAt.Format(time.RFC3339),
		ListenAddresses: []string{listen},
		Tailnet:         s.tailnetView(),
	})
}

// tailnetView reports the cached tailnet detection for the surfaces that
// render it (the settings security section, the system info). The host did
// not wire a detection (nil) when the process is not the production host
// (the tests): the answer is the explicit absent state with the reason, so
// a page renders "not detected by this process" instead of a blank field —
// UX-13's "not configured" rule, which the detection itself also applies to
// its own absent answer.
func (s *Server) tailnetView() *TailnetView {
	if s.tailnet != nil {
		return s.tailnet
	}
	return &TailnetView{Present: false, Note: "not detected by this process: the tailnet detection runs at startup and was not wired here"}
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
