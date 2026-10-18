// Command game-narrator runs the Game Narrator service.
//
// Usage:
//
//	game-narrator serve [--listen host:port] [--data-dir DIR]
//	game-narrator migrate [--data-dir DIR]
//	game-narrator doctor [--data-dir DIR] [--dev] [--json]
//	game-narrator support-bundle --data-dir DIR [--out FILE.tar.gz] [--since 24h] [--include-dev-trace] [--probe]
//	game-narrator version
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/dbirks/game-narrator/internal/agent"
	"github.com/dbirks/game-narrator/internal/agent/tools"
	"github.com/dbirks/game-narrator/internal/api"
	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/bootstrap"
	"github.com/dbirks/game-narrator/internal/doctor"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/games/adapter"
	"github.com/dbirks/game-narrator/internal/games/adapter/generic"
	"github.com/dbirks/game-narrator/internal/games/adapter/windwaker"
	"github.com/dbirks/game-narrator/internal/gcdisc"
	"github.com/dbirks/game-narrator/internal/jobs"
	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/router"
	"github.com/dbirks/game-narrator/internal/providers/tts"
	"github.com/dbirks/game-narrator/internal/runtime"
	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/store"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
	"github.com/dbirks/game-narrator/internal/voices"
	"github.com/dbirks/game-narrator/internal/web"
	"github.com/dbirks/game-narrator/internal/web/search"
)

const usageText = `usage: game-narrator <command> [flags]

commands:
  serve    start the HTTP API server (default listen 127.0.0.1:8080)
  migrate  apply pending SQLite migrations and report the schema version
  admin    create or manage accounts and API tokens
  doctor   probe every configured component and report honestly
  support-bundle  build a redacted diagnostics archive for support (docs/spec/13)
  import-disc import the user's own Wind Waker disc into a local profile
  version  print build information and exit
  help     show this message

common flags:
  --data-dir dir   durable data directory; overrides GAME_NARRATOR_DATA_DIR
                   (default $XDG_DATA_HOME/game-narrator, else
                   $HOME/.local/share/game-narrator)
`

const serveUsageText = `usage: game-narrator serve [--listen host:port] [--data-dir DIR]

Start the Game Narrator HTTP API. Endpoints (JSON):
  POST /api/v1/auth/setup           (first run, loopback only)
  POST /api/v1/auth/login           (session cookie + CSRF token)
  GET  /api/v1/auth/me
  POST /api/v1/auth/logout
  GET  /api/v1/health/live
  GET  /api/v1/health/ready
  GET  /api/v1/system/info
  GET  /api/v1/events                (SSE, Last-Event-ID replay, viewer+)
  POST /api/v1/sessions              (operator+, one active session at a time)
  GET  /api/v1/sessions/{id}         (viewer+)
  POST /api/v1/sessions/{id}/stop    (operator+, idempotent)
  GET  /api/v1/dev/events            (developer, durable event log pages)
  GET  /api/v1/dev/traces/{trace_id} (developer)
  GET  /api/v1/dev/assets/{sha256}   (developer, dev-trace samples)
  GET  /api/v1/jobs[?state=&kind=&cursor=&limit=]
  GET  /api/v1/jobs/{id}
  POST /api/v1/jobs/{id}/cancel      (operator+)
  POST /api/v1/jobs/{id}/retry       (operator+)
  GET/POST /api/v1/users             (admin)
Background job workers start with the server and drain on shutdown.

Every route outside health, system info and the auth endpoints requires a
session cookie or an 'Authorization: Bearer gn_…' token, and the minimum role
for each one is the table in docs/adr/0016-authentication-and-sessions.md.
Until an admin account exists, GET /api/v1/auth/status reports
setup_required and the first admin is created from the local machine (this
endpoint) or with 'game-narrator admin create'.

flags:
  -data-dir string
    	durable data directory; overrides GAME_NARRATOR_DATA_DIR
  -listen string
    	TCP listen address; overrides GAME_NARRATOR_LISTEN (default "127.0.0.1:8080")

environment:
  GAME_NARRATOR_TRUSTED_PROXY_CIDRS
    	comma-separated CIDRs whose X-Forwarded-Proto may mark session cookies
    	Secure (a TLS reverse proxy in front of the appliance). Unset by default:
    	no forwarded header is trusted at all.
`

const adminUsageText = `usage: game-narrator admin <subcommand> [flags] --data-dir DIR

Manage the accounts and API tokens the API authorizes against. These run
directly against the database file and do not need a running server.

subcommands:
  create  add a user with a role, reading the password from stdin
  token   print a new API token once, for an existing user
  list    show every account (never a password hash or a token)

examples:
  printf '%s' 'correct horse battery staple' |
      game-narrator admin create --username admin --role admin
  game-narrator admin token --username admin --name ci-harness --role operator

flags:
  -data-dir string
    	durable data directory; overrides GAME_NARRATOR_DATA_DIR
  -role string
    	one of viewer, curator, operator, developer, admin (default "viewer")
  -username string
    	account name, 3..64 characters
  -name string
    	token label, so two tokens can be told apart before revoking one
  -expires-days int
    	token lifetime in days; 0 (default) means it does not expire
`

// EnvTrustedProxyCIDRS names the comma-separated CIDR list whose
// X-Forwarded-Proto may set the Secure cookie flag. It is read here rather than
// in internal/bootstrap because the config struct that owns Listen and DataDir
// belongs to another task's boundary, and the ADR records that the config route
// absorbs it later.
const EnvTrustedProxyCIDRS = "GAME_NARRATOR_TRUSTED_PROXY_CIDRS"

const migrateUsageText = `usage: game-narrator migrate [--data-dir DIR]

Apply every embedded SQLite migration the database has not seen yet, printing
one line per applied migration followed by the resulting schema version.
Re-running it on an up-to-date database applies nothing.

flags:
  -data-dir string
    	durable data directory; overrides GAME_NARRATOR_DATA_DIR
`

const doctorUsageText = `usage: game-narrator doctor [--data-dir DIR] [--dev] [--json]

Probe each component and report OK, DEGRADED, BLOCKED, NOT CONFIGURED or FAIL.
Nothing is reported as working unless it was actually exercised, and provider
lines never print a URL, a token or a game image path (docs/spec/13).

The store is opened and migrated by this command, exactly as the migrate
command does.

exit status:
  0  no check FAILed (missing optional components are reported, not fatal)
  1  a configured component is broken
  2  bad usage or configuration

flags:
  -data-dir string
    	durable data directory; overrides GAME_NARRATOR_DATA_DIR
  -dev
    	add developer-host checks: Dolphin, Xvfb and the game image
  -json
    	print the report as JSON instead of a table
`

const importDiscUsageText = `usage: game-narrator import-disc [--data-dir DIR] [--image PATH] [--game-slug SLUG] [--publish]
Import the user's own GameCube disc (the Wind Waker reference adapter) into a
local profile. The disc image is read in place and never copied; the extracted
BMG message archive is hashed and stored in the local, content-addressed asset
store only (docs/spec/14, AGENTS.md rule 5) — game text never leaves the
machine, and this command prints counts, never text.

What it does, in order:
  1. resolves the image from --image or GAME_NARRATOR_GAME_IMAGE_PATH,
  2. opens it (gcdisc) and checks the game id starts with GZL,
  3. extracts res/Msg/bmgres.arc -> first .bmg and parses it with the
     windwaker-bmg adapter (byte-faithful original_text, normalized and
     speech renderings, a versioned SpeechRenderingRules revision),
  4. creates or reuses the game (slug), the edition (region from the disc id,
     language en) and a draft profile,
  5. writes one sources row (game_extract / user_supplied, the BMG content
     sha256, parser windwaker-bmg@<version>, and the coverage note)
  6. imports every message, then stores the BMG bytes in the asset store.

It is idempotent by content: a second run against the same data dir finds the
existing source row (same BMG sha256) and the profile's messages and reports
the count instead of duplicating rows.

--publish moves the draft through review and publishes it (the local gate is
the user's own review; nothing is uploaded). It is a deliberate, local act on
the user's own data and is off by default.

exit status: 0 on success (including an idempotent re-run), 1 on failure,
2 on bad usage.

flags:
  -data-dir string
     durable data directory; overrides GAME_NARRATOR_DATA_DIR
  -image string
     path to the disc image (.gcm/.ciso); defaults to GAME_NARRATOR_GAME_IMAGE_PATH
  -game-slug string
     slug of the game to create or reuse (default "wind-waker")
  -publish
     after importing, move the draft through review and publish it
`

// EnvGameImage is the environment variable that names the user's disc image
// for import-disc. It is read here (the command boundary) rather than in an
// internal package because the game-image path is operator-local configuration
// and must never be logged or committed (docs/spec/13, AGENTS.md rule 5).
const EnvGameImage = "GAME_NARRATOR_GAME_IMAGE_PATH"

// importDiscGameIDPrefix is the game-id prefix the reference adapter accepts.
// A disc whose id does not start with it is refused before any parse, so a
// mislabelled image cannot be imported under the Wind Waker adapter's rules.
const importDiscGameIDPrefix = "GZL"

// stdout is where command output goes. It is a variable so tests can assert
// the exact lines the CLI prints without capturing the real process stdout.
var stdout io.Writer = os.Stdout

// stdin is where the admin commands read a password from. A variable for the
// same reason as stdout: a test must be able to feed it without a terminal, and
// reading from a pipe rather than a flag is the point — a password in argv is
// visible in every process listing on the machine (docs/spec/14 "no raw key",
// AGENTS.md "never log passwords").
var stdin io.Reader = os.Stdin

// exitErr carries a process exit code out of run().
type exitErr struct {
	code int
	err  error
}

func (e exitErr) Error() string {
	if e.err == nil {
		return "exit"
	}
	return e.err.Error()
}

func main() {
	os.Exit(mainRun(os.Args[1:]))
}

func mainRun(args []string) int {
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	code, err := run(args, log)
	if err != nil && !errors.Is(err, errUsage) {
		log.Error("fatal", "error", err)
	}
	return code
}

var errUsage = errors.New("usage")

func run(args []string, log *slog.Logger) (int, error) {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usageText, serveUsageText)
		return 2, errUsage
	}

	switch cmd := args[0]; cmd {
	case "version":
		fmt.Println(bootstrap.Version())
		return 0, nil
	case "help", "-h", "--help":
		fmt.Print(usageText, serveUsageText, migrateUsageText, adminUsageText, doctorUsageText, benchUsageText, importDiscUsageText, supportBundleUsageText)
		return 0, nil
	case "serve":
		return runServe(args[1:], log)
	case "migrate":
		return runMigrate(args[1:], log)
	case "admin":
		return runAdmin(args[1:], log)
	case "doctor":
		return runDoctor(args[1:], log)
	case "support-bundle":
		return runSupportBundle(args[1:], log)
	case "bench":
		return runBench(args[1:], log)
	case "import-disc":
		return runImportDisc(args[1:], log)
	default:
		fmt.Fprintf(os.Stderr, "game-narrator: unknown command %q\n\n%s%s", cmd, usageText, serveUsageText)
		return 2, fmt.Errorf("%w: unknown command %q", errUsage, cmd)
	}
}

func newServeFlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, serveUsageText) }
	return fs
}

func runServe(args []string, log *slog.Logger) (int, error) {
	fs := newServeFlagSet()
	listen := fs.String("listen", "",
		"TCP listen address; overrides "+bootstrap.EnvListen+" (default "+bootstrap.DefaultListen+")")
	dataDir := fs.String("data-dir", "",
		"durable data directory; overrides "+bootstrap.EnvDataDir)
	if err := fs.Parse(args); err != nil {
		return 2, fmt.Errorf("%w: %v", errUsage, err)
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "game-narrator serve: unexpected argument %q\n", fs.Arg(0))
		return 2, fmt.Errorf("%w: unexpected argument", errUsage)
	}

	cfg, err := bootstrap.LoadConfig(*listen, *dataDir, os.LookupEnv)
	if err != nil {
		return 2, err
	}

	build := bootstrap.Version()
	log.Info("game-narrator starting", build.Values()...)

	// SIGINT/SIGTERM trigger graceful drain (http.Server.Shutdown, 10s budget).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The database is opened and migrated before the listener starts, so a
	// service that cannot answer /health/ready is never able to claim it is
	// running. `migrate` remains the operator-facing migration command; serve
	// applies pending migrations so a fresh install comes up in one step.
	st, err := store.Open(ctx, store.DBPath(cfg.DataDir))
	if err != nil {
		return 1, fmt.Errorf("opening store in %s: %w", cfg.DataDir, err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			log.Error("closing store", "error", err)
		}
	}()

	applied, err := st.Migrate(ctx)
	if err != nil {
		return 1, fmt.Errorf("migrating store: %w", err)
	}
	for _, name := range applied {
		log.Info("migration applied", "migration", name)
	}
	schemaVersion, err := st.SchemaVersion(ctx)
	if err != nil {
		return 1, fmt.Errorf("reading schema version: %w", err)
	}
	log.Info("database ready", "applied", len(applied), "schema_version", schemaVersion)

	// The content-addressed asset store (ADR-0014) sits beside the database in the
	// same durable directory, because a backup that restores one and not the other
	// restores a catalogue full of pointers to bytes that do not exist. Staging is
	// recovered before the listener starts: an upload interrupted by the last
	// process is not resumable (the client's connection is gone), so its scratch
	// files are discarded rather than left to accumulate.
	assetStore, err := assets.NewLocal(AssetDir(cfg.DataDir), assets.WithMaxSize(maxAssetBlobBytes))
	if err != nil {
		return 1, fmt.Errorf("opening the asset store in %s: %w", AssetDir(cfg.DataDir), err)
	}
	if discarded, err := assetStore.RecoverStaging(ctx); err != nil {
		return 1, fmt.Errorf("recovering the asset staging area: %w", err)
	} else if discarded > 0 {
		log.Warn("discarded upload staging left by the previous process",
			"staging_entries", discarded, "reason", "an interrupted upload is not resumable: the client is gone (ADR-0014)")
	}
	log.Info("asset store ready", "root_basename", filepath.Base(AssetDir(cfg.DataDir)))

	// The source adapters linked into this build decide what an uploaded script
	// can be read as. The generic column-mapping adapter is the one Milestone 1
	// ships with; game-specific extractors join the same registry in later
	// milestones, and POST …/import reports the formats this list can actually
	// read rather than accepting an upload nothing can parse.
	adapters := adapter.NewRegistry()
	if err := generic.Register(adapters); err != nil {
		return 1, fmt.Errorf("linking the generic script adapter: %w", err)
	}
	adapterIDs := make([]string, 0, len(adapters.Adapters()))
	for _, m := range adapters.Adapters() {
		adapterIDs = append(adapterIDs, m.ID)
	}
	log.Info("source adapters linked", "adapters", adapterIDs)

	// Durable event log and fan-out hub (docs/spec/08 "Event stream
	// reliability"): SQLite assigns the global seq; the hub replays a
	// reconnecting browser past its in-memory ring from that log.
	eventStore := store.NewEventStore(st)
	hub := events.New(events.WithStore(eventStore), events.WithLogger(log))
	log.Info("event hub attached to the durable event log")

	// Job workers (docs/spec/01: API, jobs and capture in one program on
	// UNO Q). Recovery runs before workers start claiming, so a row left
	// `running` by the last process becomes `recoverable`/`queued` before any
	// live lease exists to race it (docs/spec/02 "Transaction workflows").
	jobsStore := store.NewJobsStore(st)
	runner, err := jobs.NewRunner(jobsStore, hub, log)
	if err != nil {
		return 1, fmt.Errorf("creating job runner: %w", err)
	}
	if err := runner.Register("system.noop", noopJobHandler, jobs.RegisterOpts{
		Concurrency: 1,
		Resumable:   true,
		Timeout:     time.Minute,
	}); err != nil {
		return 1, fmt.Errorf("registering system.noop job kind: %w", err)
	}
	// The import worker (docs/spec/08 "staged source/profiles", ADR-0017): the API
	// lands the bytes and queues this kind, so the queue is what reports progress
	// and the parse report. Resumable because re-running it is the design — the
	// payload names a content hash, not a temporary file, so a worker that starts
	// after a crash reads the same bytes and profileio reconciles by
	// (source, source_message_id) instead of duplicating rows (docs/spec/02 "Import
	// resume by content hash").
	if err := runner.Register(api.JobKindImportFile,
		api.ImportFileJobHandler(st, assetStore, adapters), jobs.RegisterOpts{
			Concurrency: 1,
			Resumable:   true,
			Timeout:     importJobTimeout,
		}); err != nil {
		return 1, fmt.Errorf("registering %s job kind: %w", api.JobKindImportFile, err)
	}
	// The onboarding agent (ADR-0026/0028): the agent.run kind executes the
	// durable run log in internal/agent. The model is reached through the
	// provider router — the single provider boundary with breakers and
	// retries — and roles come from the environment
	// (GAME_NARRATOR_ROLE_AGENT_REASONING_MODEL / _URL; a _URL points at a
	// deployment, e.g. an Ollama port or an OpenAI-compatible gateway).
	// A missing role is not a boot failure: the run then lands in blocked
	// with that as its reason and resumes once configured (docs/spec/13
	// "degraded modes").
	agentHandler, agentCloser, err := newAgentWiring(st, assetStore, adapters, hub)
	if err != nil {
		return 1, fmt.Errorf("wiring the onboarding agent: %w", err)
	}
	defer agentCloser()
	if err := runner.Register(agent.JobKind, agentHandler, jobs.RegisterOpts{
		Concurrency: 1,
		Resumable:   true,
		Timeout:     agentJobTimeout,
	}); err != nil {
		return 1, fmt.Errorf("registering %s job kind: %w", agent.JobKind, err)
	}
	// The voice workflow (docs/spec/07): audition generation runs as the
	// voice.auditions job, and the service owns the consent-gated approval, the
	// namespaced registry names and the fingerprint-keyed preview cache. A
	// missing speech endpoint is a degraded mode, not a boot failure: the
	// /api/v1/voices routes then answer 503 and doctor names what is missing
	// (docs/spec/13). A malformed GAME_NARRATOR_* value, by contrast, fails the
	// boot loudly, the same way the agent wiring does.
	voiceSvc, voiceCloser, err := newVoiceWiring(st, assetStore, log)
	if err != nil {
		return 1, fmt.Errorf("wiring the voice workflow: %w", err)
	}
	if voiceCloser != nil {
		defer voiceCloser()
	}
	if voiceSvc != nil {
		if err := runner.Register(voices.JobKind, voices.JobHandler(voiceSvc), voices.JobHandlerOpts()); err != nil {
			return 1, fmt.Errorf("registering %s job kind: %w", voices.JobKind, err)
		}
	}
	recovery, err := jobsStore.RecoverOnStartup(ctx, runner.ResumableKinds())
	if err != nil {
		return 1, fmt.Errorf("recovering interrupted jobs: %w", err)
	}
	log.Info("job recovery complete",
		"marked_recoverable", len(recovery.Marked), "requeued", len(recovery.Requeued))
	if err := runner.Start(ctx); err != nil {
		return 1, fmt.Errorf("starting job runner: %w", err)
	}
	log.Info("job runner started")

	// Session runtime (Milestone 2, ADR-0024): the one live pipeline run, its
	// lifecycle events and its developer-trace evidence. Dev-trace storage
	// sits beside the asset store in the durable directory (same reasoning as
	// AssetDir: evidence and catalogue are one body of state), and the replay
	// allowlist is the documented default — GAME_NARRATOR_REPLAY_ROOTS, else
	// testdata/ from a dev checkout, else nothing in production.
	devTraceDir := filepath.Join(cfg.DataDir, "dev-trace")
	// The session's speech pipeline (docs/spec/07, ADR-0039): wired only when
	// the voice workflow is — a silent appliance keeps answering the audio
	// route 503 rather than applying a control to nothing. The sink is the
	// operator's configuration (filesink under the durable directory by
	// default), and a sink that cannot be built degrades the session to
	// silent, never the run.
	var speechOpts *runtime.SpeechOptions
	if voiceSvc != nil {
		sinkKind, device, err := audioSinkFromEnv(os.LookupEnv)
		if err != nil {
			return 1, fmt.Errorf("audio sink configuration: %w", err)
		}
		speechOpts = &runtime.SpeechOptions{
			Voices:         voiceSvc,
			Assets:         assetStore,
			Sink:           sinkKind,
			FileSinkDir:    filepath.Join(cfg.DataDir, "audio"),
			PipeWireTarget: device,
		}
	}
	sessions, err := runtime.New(runtime.Options{
		Store:       st,
		Hub:         hub,
		Jobs:        runner,
		AssetsRoot:  devTraceDir,
		ReplayRoots: runtime.DefaultReplayRoots(os.LookupEnv),
		Speech:      speechOpts,
		Log:         log,
	})
	if err != nil {
		return 1, fmt.Errorf("creating the session runtime: %w", err)
	}
	log.Info("session runtime ready",
		"dev_trace_root", filepath.Base(devTraceDir),
		"replay_roots", len(runtime.DefaultReplayRoots(os.LookupEnv)))

	// Auth store (docs/spec/08 "API version and user authentication", ADR-0016):
	// created before the listener so a process that cannot read its own
	// credential tables never serves, and reported the same way every other
	// component is — an appliance that came up with no admin says so on
	// /api/v1/auth/status rather than failing every request at 401.
	authRepo := store.NewAuthStore(st)
	if hasAdmin, err := authRepo.HasRole(ctx, store.RoleAdmin); err != nil {
		return 1, fmt.Errorf("checking for an admin account: %w", err)
	} else if !hasAdmin {
		log.Warn("no admin account exists: the API will accept no authenticated request. " +
			"Claim this appliance from the local machine with POST /api/v1/auth/setup " +
			"or `game-narrator admin create --username admin --role admin`")
	}

	// Shutdown choreography, in the only order that cannot wedge the
	// process: HTTP drain and job drain race ctx.Done deliberately, because
	// net/http.Shutdown waits for open SSE streams and only closing the hub
	// releases them (docs/spec/01 "slow browsers cannot stall capture" has a
	// shutdown-side twin: a browser must not stall shutdown either). The
	// store closes afterwards via its defer, so nothing writes to a closed
	// database on the way out.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-runCtx.Done()
		drainCtx, drainCancel := context.WithTimeout(context.Background(), jobDrainBudget)
		defer drainCancel()
		if err := runner.Stop(drainCtx); err != nil {
			log.Warn("job runner shutdown incomplete; interrupted jobs are re-queued for the next start",
				"error", err)
		} else {
			log.Info("job runner stopped")
		}
		// The session runtime stops before the hub closes: a run finalising on
		// shutdown still publishes its session.stopped, and a hub closed first
		// would drop the terminal event of the session we asked to stop.
		sessions.Close()
		log.Info("session runtime closed")
		if err := hub.Close(); err != nil {
			log.Error("closing event hub", "error", err)
		}
		log.Info("event hub closed")
	}()

	apiOpts := []api.Option{
		api.WithReadiness(storeReadiness(st, log)),
		api.WithEvents(hub),
		api.WithJobs(jobsStore),
		api.WithSessions(sessions),
		api.WithOnboarding(runner, store.NewAgentStore(st), tools.NewSQLiteApprovals(st)),
		api.WithDevEvents(eventStore),
		api.WithAuth(auth.NewService(authRepo, auth.WithLogger(log)), authRepo),
		api.WithStore(st),
		api.WithImport(assetStore, adapters, runner),
		// The region editor's OCR preview: the same tesseract engine the live
		// pipeline runs (ADR-0012's supervised subprocess), and the configured
		// replay roots as the fixture allowlist (ADR-0024, reused for the
		// preview's test-on-fixture frames).
		api.WithOCRPreview(func(psm int) ocr.Engine {
			return ocr.Tesseract{PSM: psm, Timeout: 15 * time.Second}
		}, runtime.DefaultReplayRoots(os.LookupEnv)),
		api.WithTrustedProxy(trustedProxyCIDRs(os.LookupEnv)...),
	}
	if voiceSvc != nil {
		// The queue is what the 202 audition create enqueues onto; the same
		// runner the voice.auditions job executes on.
		apiOpts = append(apiOpts, api.WithVoices(voiceSvc, runner))
	}
	srv := bootstrap.NewServer(cfg, api.New(build, time.Now(), log,
		apiOpts...,
	).Handler(), log)
	if err := srv.Run(runCtx); err != nil {
		// A bind failure returns before ctx is cancelled; the shutdown
		// goroutine is then waiting on nothing. Release it the same way so
		// both exits leave no goroutine writing to the store we are about to
		// close (AGENTS.md: safe degraded shutdown, not merely a green log).
		cancelRun()
		<-shutdownDone
		return 1, err
	}
	<-shutdownDone
	return 0, nil
}

// audioSinkFromEnv reads the session's audio sink configuration
// (docs/spec/07, ADR-0039): GAME_NARRATOR_AUDIO_SINK picks dummy (the test
// default: paces and records, plays nothing), filesink (the production
// default: one WAV per started playback under the data directory) or
// pipewire (the named sink of GAME_NARRATOR_AUDIO_DEVICE; the presence
// preflight and its file-sink degradation are the wiring's own job).
// An unknown value fails the boot loudly, the same way a malformed role
// env does.
func audioSinkFromEnv(lookupEnv func(string) (string, bool)) (kind, device string, err error) {
	const sinkEnv = "GAME_NARRATOR_AUDIO_SINK"
	const deviceEnv = "GAME_NARRATOR_AUDIO_DEVICE"
	rawSink, _ := lookupEnv(sinkEnv)
	kind = strings.TrimSpace(rawSink)
	if kind == "" {
		kind = runtime.SinkFile
	}
	switch kind {
	case runtime.SinkDummy, runtime.SinkFile, runtime.SinkPipeWire:
	default:
		return "", "", fmt.Errorf("unknown %s %q (want dummy, filesink or pipewire)", sinkEnv, kind)
	}
	rawDevice, _ := lookupEnv(deviceEnv)
	device = strings.TrimSpace(rawDevice)
	if device == "" {
		device = "default"
	}
	return kind, device, nil
}

// AssetDir is the content-addressed asset root of a data directory
// (docs/spec/02 "Asset store contract", ADR-0014). It sits beside the database
// file rather than under a cache directory on purpose: the two are one body of
// durable state, and a restore that brings back the catalogue's pointers without
// the bytes they point at produces a library that cannot be re-parsed or
// re-audited. ADR-0017 records the layout.
func AssetDir(dataDir string) string { return filepath.Join(dataDir, "assets") }

// maxAssetBlobBytes is the per-blob ceiling the asset store enforces, independent
// of the API's own upload cap. It is above any script the docs/spec/16 benchmark
// describes and above a spoken-line audio asset with room to spare, while still
// being small enough that one malformed request cannot fill the appliance's disk
// (docs/spec/14 "Filesystem/import").
const maxAssetBlobBytes = 512 << 20

// importJobTimeout bounds one import.file attempt. A 10 000-message script parses
// and writes in seconds on the target hardware; an hour would hide a wedged
// parser until the lease had already been stolen, and a minute would cut off the
// low-end device doing legitimate work (docs/spec/16's performance budget).
const importJobTimeout = 10 * time.Minute

// agentJobTimeout bounds one agent.run attempt. An onboarding run is a bounded
// research-and-import loop (docs/spec/16 "onboarding agent" budget); 30 minutes
// leaves wide margin for a slow model over the wire while still bounding a
// wedged attempt so its lease is eventually stealable (docs/spec/08).
const agentJobTimeout = 30 * time.Minute

// newAgentWiring assembles the onboarding agent's job handler (ADR-0026/0028).
// The model is reached only through the provider router — the single provider
// boundary with breakers, affinity and retries — and the tool registry is the
// default allowlist. Roles are read from the environment; a missing
// agent_reasoning role is deliberately not a boot failure (the run then blocks
// with that as its reason and resumes once configured). The returned closer
// releases the search provider's HTTP client on shutdown.
func newAgentWiring(st *store.Store, assetStore *assets.Local, adapters *adapter.Registry, hub *events.Hub) (jobs.Handler, func(), error) {
	roles, err := providers.LoadRoles(os.LookupEnv)
	if err != nil {
		return nil, nil, fmt.Errorf("loading provider roles: %w", err)
	}
	roleReg := providers.NewRegistry(roles)
	chatRouter := router.New(roleReg, router.Options{})
	// The model boundary: one router call over the agent_reasoning role. The
	// same boundary serves the loop (Model) and the analyze_character tool
	// (ChatFn), so a breaker opens or closes in one place.
	model := agent.RouterCall(roleReg, chatRouter)
	searchClient := &search.ExaMCP{}
	searchProvider := search.NewBudgeted(searchClient, 8, 500*time.Millisecond)
	deps := tools.Deps{
		Store:     st,
		Assets:    assetStore,
		Adapters:  adapters,
		Search:    searchProvider,
		Fetcher:   web.NewFetcher(privateWebPolicy(os.LookupEnv)),
		Chat:      model.Chat,
		Approvals: tools.NewSQLiteApprovals(st),
		TTS:       nil,
		Artwork:   nil,
	}
	registry, err := tools.DefaultTools(deps)
	if err != nil {
		return nil, nil, fmt.Errorf("assembling the agent tool registry: %w", err)
	}
	handler, err := agent.NewJobHandler(agent.JobConfig{
		Model:      model,
		Registry:   registry,
		Store:      store.NewAgentStore(st),
		Hub:        hub,
		CallerRole: auth.RoleCurator,
		ModelLabel: roles[providers.RoleAgentReasoning].Model,
		Budget:     agent.BudgetConfig{MaxSteps: 24, MaxToolCalls: 48},
		Prompt: agent.PromptContext{
			UserRole:     auth.RoleCurator,
			SourcePolicy: "web sources are untrusted (docs/spec/14); import_user_asset is the only path script content enters",
		},
	})
	if err != nil {
		return nil, nil, err
	}
	return handler, func() {}, nil
}

// newVoiceWiring builds the voice workflow from the same role environment the
// rest of the process reads (docs/spec/06, docs/spec/07):
//
//	GAME_NARRATOR_GATEWAY_URL / GAME_NARRATOR_GATEWAY_TOKEN_ENV
//	GAME_NARRATOR_ROLE_VOICE_DESIGN_MODEL   VoiceDesign checkpoint (auditions)
//	GAME_NARRATOR_ROLE_VOICE_CLONE_MODEL    Base checkpoint (preview/register)
//	GAME_NARRATOR_TTS_BASE_DIRECT_URL       the direct Base instance (registry)
//
// A missing endpoint degrades: nil service, one log line naming the missing
// variable, and /api/v1/voices answers 503. Nothing a log line names is a URL
// or a token (AGENTS.md rule 5).
func newVoiceWiring(st *store.Store, assetStore *assets.Local, log *slog.Logger) (*voices.Service, func(), error) {
	roles, err := providers.LoadRoles(os.LookupEnv)
	if err != nil {
		return nil, nil, fmt.Errorf("loading provider roles: %w", err)
	}
	var missing []string
	if !roles[providers.RoleVoiceDesign].Configured() {
		missing = append(missing, "GAME_NARRATOR_ROLE_VOICE_DESIGN_MODEL")
	}
	if !roles[providers.RoleVoiceClone].Configured() {
		missing = append(missing, "GAME_NARRATOR_ROLE_VOICE_CLONE_MODEL")
	}
	// The registry lives only on the direct Base service (docs/spec/06): in
	// production the role override names it, and the direct-URL variable is the
	// test-harness spelling (an orchestrator port-forward, docs/spec/12). Neither
	// value is ever logged.
	directURL := strings.TrimSpace(os.Getenv("GAME_NARRATOR_ROLE_VOICE_CLONE_REGISTRY_URL"))
	if directURL == "" {
		directURL = strings.TrimSpace(os.Getenv("GAME_NARRATOR_TTS_BASE_DIRECT_URL"))
	}
	if directURL == "" {
		missing = append(missing, "GAME_NARRATOR_TTS_BASE_DIRECT_URL")
	}
	if strings.TrimSpace(roles[providers.RoleVoiceDesign].BaseURL) == "" ||
		strings.TrimSpace(roles[providers.RoleVoiceDesign].Token()) == "" {
		missing = append(missing, "GAME_NARRATOR_GATEWAY_URL or the token env it names")
	}
	if len(missing) > 0 {
		log.Info("voice workflow not wired: the speech endpoints are not configured; /api/v1/voices answers 503",
			"missing", strings.Join(missing, ", "))
		return nil, func() {}, nil
	}

	designCfg, cloneCfg := roles[providers.RoleVoiceDesign], roles[providers.RoleVoiceClone]
	designClient, err := tts.NewFromRole(designCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("design client: %w", err)
	}
	cloneClient, err := tts.NewFromRole(cloneCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("clone client: %w", err)
	}
	registry, err := tts.NewRegistry(directURL, cloneCfg.Token())
	if err != nil {
		return nil, nil, fmt.Errorf("voice registry: %w", err)
	}

	svc := &voices.Service{
		Store: st, Assets: assetStore, Design: designClient, Base: cloneClient,
		Registry:      registry,
		Speech:        speech.New(assetStore, speech.NewMemIndex(), 8),
		InstanceID:    voices.DefaultInstanceID,
		BaseModelID:   cloneCfg.Model,
		BackendDigest: previewBackendDigest(cloneCfg.Model),
	}
	if err := voices.New(svc); err != nil {
		return nil, nil, fmt.Errorf("voice service: %w", err)
	}
	log.Info("voice workflow ready",
		"design_model", designCfg.Model, "clone_model", cloneCfg.Model, "instance", svc.InstanceID)
	return svc, func() {}, nil
}

// previewBackendDigest is the preview cache key's backend_config_digest
// (ADR-0023): it changes when the synthesis backend configuration changes,
// which is the only moment cached previews must stop being reusable.
func previewBackendDigest(model string) string {
	sum := sha256.Sum256([]byte("voice-preview-v1|" + model))
	return hex.EncodeToString(sum[:])[:8]
}

// privateWebPolicy builds the fetcher policy for the agent's web tool from the
// environment (docs/spec/14). Private-network fetches are off by default; the
// operator opts in with GAME_NARRATOR_WEB_ALLOW_PRIVATE and names the exact
// host:port entries that are admitted in GAME_NARRATOR_WEB_PRIVATE_ALLOW
// (comma-separated). That is the only path to a non-public endpoint, and it is
// deliberately narrow so the test harness can reach its fixture pages while a
// production appliance reaches none.
func privateWebPolicy(lookup func(string) (string, bool)) web.Policy {
	policy := web.Policy{}
	if v, ok := lookup("GAME_NARRATOR_WEB_ALLOW_PRIVATE"); ok && (v == "1" || strings.EqualFold(v, "true")) {
		policy.AllowPrivate = true
	}
	if list, ok := lookup("GAME_NARRATOR_WEB_PRIVATE_ALLOW"); ok {
		for _, part := range strings.Split(list, ",") {
			if host := strings.TrimSpace(part); host != "" {
				policy.PrivateAllow = append(policy.PrivateAllow, host)
			}
		}
	}
	return policy
}

// jobDrainBudget bounds graceful in-flight job drain. It stays under
// bootstrap.ShutdownTimeout so a forced job cancellation and its retry
// bookkeeping land before the HTTP drain gives up on a streaming client.
const jobDrainBudget = 7 * time.Second

// noopJobHandler is the built-in diagnostic kind: `system.noop` exists so
// operator tooling (doctor, the evidence bundle, a smoke test of this exact
// binary) can prove the whole queue path — enqueue, claim, lease heartbeat,
// publish, event-stream replay, persistence — with real work rather than a
// mock (AGENTS.md "no simulation theater").
func noopJobHandler(ctx context.Context, job jobs.Job, progress jobs.ProgressFn) (json.RawMessage, error) {
	progress(100, "noop")
	return json.RawMessage(`{"ok":true,"kind":"` + job.Kind + `"}`), nil
}

// storeReadiness reports database readiness for GET /api/v1/health/ready. The
// schema-version query is the ping (it needs a live connection and a readable
// file) and PRAGMA integrity_check is the corruption probe. M0 databases are
// small enough to check on every poll; if that stops being true the probe moves
// behind a cached interval, which is an ADR, not a silent downgrade.
//
// Only fixed strings go into the payload: store errors name the database file
// path, and the API surface deliberately carries no host paths.
func storeReadiness(st *store.Store, log *slog.Logger) api.ReadinessFunc {
	return func(ctx context.Context) (map[string]any, bool) {
		version, err := st.SchemaVersion(ctx)
		if err != nil {
			log.Warn("readiness: database ping failed", "error", err)
			return map[string]any{"database": map[string]any{"status": "fail", "error": "database ping failed"}}, false
		}
		if err := st.IntegrityCheck(ctx); err != nil {
			log.Warn("readiness: database integrity check failed", "error", err)
			return map[string]any{"database": map[string]any{
				"status":         "fail",
				"schema_version": version,
				"error":          "integrity check failed",
			}}, false
		}
		return map[string]any{"database": map[string]any{
			"status":         "ok",
			"schema_version": version,
		}}, true
	}
}

// runMigrate opens the store and applies embedded migrations on request. It is
// the operator-facing path: it prints each applied migration, which `serve`
// logs instead so a restart never blocks on human-readable progress output
// (docs/spec/02 "Migration and backup acceptance").
func runMigrate(args []string, log *slog.Logger) (int, error) {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, migrateUsageText) }
	dataDir := fs.String("data-dir", "",
		"durable data directory; overrides "+bootstrap.EnvDataDir)
	if err := fs.Parse(args); err != nil {
		return 2, fmt.Errorf("%w: %v", errUsage, err)
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "game-narrator migrate: unexpected argument %q\n", fs.Arg(0))
		return 2, fmt.Errorf("%w: unexpected argument", errUsage)
	}

	cfg, err := bootstrap.LoadConfig("", *dataDir, os.LookupEnv)
	if err != nil {
		return 2, err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dbPath := store.DBPath(cfg.DataDir)
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return 1, err
	}
	defer func() {
		if err := st.Close(); err != nil {
			log.Error("closing store", "error", err)
		}
	}()

	applied, err := st.Migrate(ctx)
	if err != nil {
		return 1, err
	}
	for _, name := range applied {
		fmt.Fprintf(stdout, "applied %s\n", name)
	}

	version, err := st.SchemaVersion(ctx)
	if err != nil {
		return 1, err
	}

	sqliteVersion, err := st.SQLiteVersion(ctx)
	if err != nil {
		return 1, err
	}
	log.Info("database migrated",
		"database", dbPath,
		"applied", len(applied),
		"schema_version", version,
		"sqlite_version", sqliteVersion,
	)

	fmt.Fprintf(stdout, "schema version %d\n", version)
	return 0, nil
}

// trustedProxyCIDRs reads the trusted-proxy allowlist from the environment.
// Absent or empty means "trust no forwarded header", which is the safe default
// for a service that binds localhost (docs/spec/10).
func trustedProxyCIDRs(lookupEnv func(string) (string, bool)) []string {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	raw, _ := lookupEnv(EnvTrustedProxyCIDRS)
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// openDataStore opens and migrates the appliance database for a CLI command
// that runs without a server (migrate, admin). It is shared so `admin create`
// cannot be pointed at an unmigrated file and report a constraint error as
// though the username were bad.
func openDataStore(ctx context.Context, dataDir string, log *slog.Logger) (*store.Store, error) {
	st, err := store.Open(ctx, store.DBPath(dataDir))
	if err != nil {
		return nil, fmt.Errorf("opening store in %s: %w", dataDir, err)
	}
	if _, err := st.Migrate(ctx); err != nil {
		st.Close()
		return nil, fmt.Errorf("migrating store: %w", err)
	}
	return st, nil
}

// runAdmin dispatches the account-management subcommands. Everything here is a
// local, deliberate act against the database file: the API's own privileged
// routes need an admin session, which is exactly the chicken-and-egg first run
// exists to break (docs/spec/14 "local-first, set admin on first run").
func runAdmin(args []string, log *slog.Logger) (int, error) {
	if len(args) == 0 {
		os.Stderr.WriteString(adminUsageText)
		return 2, fmt.Errorf("%w: admin requires a subcommand", errUsage)
	}
	switch args[0] {
	case "create":
		return runAdminCreate(args[1:], log)
	case "token":
		return runAdminToken(args[1:], log)
	case "list":
		return runAdminList(args[1:], log)
	default:
		fmt.Fprintf(os.Stderr, "game-narrator admin: unknown subcommand %q\n\n%s", args[0], adminUsageText)
		return 2, fmt.Errorf("%w: unknown admin subcommand %q", errUsage, args[0])
	}
}

// newAdminFlagSet builds the flag set shared by the admin subcommands. The
// password is deliberately not one of them.
func newAdminFlagSet(name string, usage string) (*flag.FlagSet, *string, *string, *string) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	dataDir := fs.String("data-dir", "", "durable data directory; overrides "+bootstrap.EnvDataDir)
	username := fs.String("username", "", "account name (3..64 characters of letters, digits, . _ - @ +)")
	role := fs.String("role", store.RoleViewer, "role: viewer, curator, operator, developer or admin")
	return fs, dataDir, username, role
}

// readPasswordLine reads the password from stdin, to the first newline, without
// echoing it. No terminal echo handling is required (and none is claimed): the
// documented usage is a pipe or a here-string from a secrets manager, which is
// also the only form that works over ssh. The trailing newline is trimmed;
// every other byte, including spaces, is the password — docs/spec/14's password
// policy is length-based, so silently stripping whitespace would be changing
// the credential.
func readPasswordLine() (string, error) {
	scanner := bufio.NewScanner(io.LimitReader(stdin, 1<<20))
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return "", fmt.Errorf("reading the password from stdin: %w", err)
		}
		return "", errors.New(`reading the password from stdin: no input (pipe it instead, e.g. printf '%s' "$SECRET" | game-narrator admin create --username admin --role admin)`)
	}
	return scanner.Text(), nil
}

func runAdminCreate(args []string, log *slog.Logger) (int, error) {
	fs, dataDir, username, role := newAdminFlagSet("admin create", adminUsageText)
	if err := fs.Parse(args); err != nil {
		return 2, fmt.Errorf("%w: %v", errUsage, err)
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "game-narrator admin create: unexpected argument %q\n", fs.Arg(0))
		return 2, fmt.Errorf("%w: unexpected argument", errUsage)
	}
	parsedRole, err := auth.ParseRole(*role)
	if err != nil {
		os.Stderr.WriteString(adminUsageText)
		return 2, fmt.Errorf("%w: %v", errUsage, err)
	}

	cfg, err := bootstrap.LoadConfig("", *dataDir, os.LookupEnv)
	if err != nil {
		return 2, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), cliDBTimeout)
	defer cancel()

	st, err := openDataStore(ctx, cfg.DataDir, log)
	if err != nil {
		return 1, err
	}
	defer closeStore(st, log)

	svc := auth.NewService(store.NewAuthStore(st), auth.WithLogger(log))
	password, err := readPasswordLine()
	if err != nil {
		os.Stderr.WriteString(adminUsageText)
		return 2, fmt.Errorf("%w: %v", errUsage, err)
	}
	// The password is never echoed, never logged, and never taken as a flag: a
	// credential in argv is readable by every process listing on the machine
	// (docs/spec/14, AGENTS.md "never log passwords"). A policy failure is
	// reported by the rule it broke, and auth.ErrWeakPassword states it in full.
	user, err := svc.CreateUser(ctx, *username, password, parsedRole, nil, "cli")
	if err != nil {
		log.Error("could not create the account", "username", *username, "error", err)
		return 1, fmt.Errorf("creating user %q: %w", *username, err)
	}
	fmt.Fprintf(stdout, "created %s with role %s (%s)\n", user.Username, user.Role, user.ID)
	return 0, nil
}

func runAdminToken(args []string, log *slog.Logger) (int, error) {
	fs, dataDir, username, _ := newAdminFlagSet("admin token", adminUsageText)
	name := fs.String("name", "cli", "token label")
	expiresDays := fs.Int("expires-days", 0, "token lifetime in days; 0 means it does not expire")
	if err := fs.Parse(args); err != nil {
		return 2, fmt.Errorf("%w: %v", errUsage, err)
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "game-narrator admin token: unexpected argument %q\n", fs.Arg(0))
		return 2, fmt.Errorf("%w: unexpected argument", errUsage)
	}
	if *expiresDays < 0 || *expiresDays > 3650 {
		os.Stderr.WriteString(adminUsageText)
		return 2, fmt.Errorf("%w: --expires-days must be between 0 and 3650", errUsage)
	}

	cfg, err := bootstrap.LoadConfig("", *dataDir, os.LookupEnv)
	if err != nil {
		return 2, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), cliDBTimeout)
	defer cancel()

	st, err := openDataStore(ctx, cfg.DataDir, log)
	if err != nil {
		return 1, err
	}
	defer closeStore(st, log)

	repo := store.NewAuthStore(st)
	svc := auth.NewService(repo, auth.WithLogger(log))
	owner, err := repo.UserByUsername(ctx, *username)
	if err != nil {
		log.Error("could not find the token's owner", "username", *username, "error", err)
		return 1, fmt.Errorf("no account named %q: create it with `game-narrator admin create`: %w", *username, err)
	}
	ownerRole, err := auth.ParseRole(owner.Role)
	if err != nil {
		return 1, err
	}
	// --role defaults to the owner's own role rather than to viewer: `admin
	// token --username admin` printing a viewer token would surprise an
	// operator, and the ceiling (a token may not exceed its owner) is still
	// enforced by the service.
	// --role defaults to the owner's own role rather than to the flag default of
	// viewer: `admin token --username admin` printing a viewer token would
	// surprise an operator who asked for a token for their admin account. An
	// explicit --role may only go down, never above the owner (checked below).
	requested := ownerRole
	if raw := flagGivenValue(fs, "role"); raw != "" {
		parsed, err := auth.ParseRole(raw)
		if err != nil {
			os.Stderr.WriteString(adminUsageText)
			return 2, fmt.Errorf("%w: %v", errUsage, err)
		}
		requested = parsed
	}
	if requested > ownerRole {
		// The token ceiling rule from docs/spec/08: a credential cannot outgrow
		// the principal that minted it, even when an operator asks for that.
		log.Error("refusing to mint a token above its owner's role",
			"username", owner.Username, "owner_role", ownerRole.String(), "requested", requested.String())
		return 1, fmt.Errorf("account %q holds %s; a token may not exceed it", owner.Username, ownerRole)
	}

	raw, tok, err := svc.CreateToken(ctx, nil, auth.CreateTokenParams{
		UserID: owner.ID,
		Name:   *name,
		Role:   requested,
		TTL:    time.Duration(*expiresDays) * 24 * time.Hour,
	}, "cli")
	if err != nil {
		return 1, fmt.Errorf("creating an api token: %w", err)
	}
	// The token is printed on stdout and nowhere else: not in the log line (the
	// log line names the id), and never again — the database holds its digest.
	fmt.Fprintf(stdout, "%s\n", raw)
	log.Info("api token created", "token_id", tok.ID, "username", owner.Username,
		"role", tok.Role, "name", tok.Name)
	return 0, nil
}

func runAdminList(args []string, log *slog.Logger) (int, error) {
	fs := flag.NewFlagSet("admin list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { os.Stderr.WriteString(adminUsageText) }
	dataDir := fs.String("data-dir", "", "durable data directory; overrides "+bootstrap.EnvDataDir)
	if err := fs.Parse(args); err != nil {
		return 2, fmt.Errorf("%w: %v", errUsage, err)
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "game-narrator admin list: unexpected argument %q\n", fs.Arg(0))
		return 2, fmt.Errorf("%w: unexpected argument", errUsage)
	}
	cfg, err := bootstrap.LoadConfig("", *dataDir, os.LookupEnv)
	if err != nil {
		return 2, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), cliDBTimeout)
	defer cancel()

	st, err := openDataStore(ctx, cfg.DataDir, log)
	if err != nil {
		return 1, err
	}
	defer closeStore(st, log)

	users, err := store.NewAuthStore(st).ListUsers(ctx)
	if err != nil {
		return 1, fmt.Errorf("listing accounts: %w", err)
	}
	if len(users) == 0 {
		fmt.Fprintln(stdout, "no accounts: the API accepts no authenticated request until `game-narrator admin create` makes one")
		return 0, nil
	}
	for _, u := range users {
		// password_hash is never selected into this line: the row type carries it
		// and the CLI has no use for it (AGENTS.md: never log credentials).
		state := ""
		if u.Disabled {
			state = " (disabled)"
		}
		fmt.Fprintf(stdout, "%s\t%s\t%s%s\n", u.Username, u.Role, u.ID, state)
	}
	return 0, nil
}

// flagGivenValue returns a flag's value only if it was set on the command line,
// "" if it is still at its default. flag.Flag has no "was given" accessor, and
// Visit walks exactly the set flags, so this is the documented way to tell
// "the operator chose viewer" from "nobody chose and the default is viewer" —
// which for `admin token` is the difference between a viewer token and the
// owner's own role.
func flagGivenValue(fs *flag.FlagSet, name string) string {
	var out string
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			out = f.Value.String()
		}
	})
	return out
}

// cliDBTimeout bounds a CLI database operation. Long enough for a migration on
// a slow SD card, short enough that a locked database is reported rather than
// hanging an operator's shell (AGENTS.md: bounded retries, escalating
// blockers).
const cliDBTimeout = 60 * time.Second

func closeStore(st *store.Store, log *slog.Logger) {
	if err := st.Close(); err != nil {
		log.Error("closing store", "error", err)
	}
}

// runDoctor probes the host and prints an honest report. It exits 1 only when a
// configured component is actually broken: an unattached capture card or an
// unconfigured role is a state, not an error, and a first-run host must still
// leave with 0 and a list of what to do next (docs/spec/16 Milestone 0
// "diagnostic report lists configured/missing components accurately").
func runDoctor(args []string, log *slog.Logger) (int, error) {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, doctorUsageText) }
	dataDir := fs.String("data-dir", "",
		"durable data directory; overrides "+bootstrap.EnvDataDir)
	dev := fs.Bool("dev", false,
		"add developer-host checks: Dolphin, Xvfb and the game image")
	asJSON := fs.Bool("json", false, "print the report as JSON instead of a table")
	if err := fs.Parse(args); err != nil {
		return 2, fmt.Errorf("%w: %v", errUsage, err)
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "game-narrator doctor: unexpected argument %q\n", fs.Arg(0))
		return 2, fmt.Errorf("%w: unexpected argument", errUsage)
	}

	cfg, err := bootstrap.LoadConfig("", *dataDir, os.LookupEnv)
	if err != nil {
		return 2, err
	}

	// Ctrl-C must not abandon a provider call that is still in flight.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	report := doctor.Run(ctx, doctor.Options{DataDir: cfg.DataDir, Dev: *dev})

	if *asJSON {
		err = doctor.FormatJSON(stdout, report)
	} else {
		err = doctor.Format(stdout, report)
	}
	if err != nil {
		return 1, fmt.Errorf("writing the doctor report: %w", err)
	}

	if report.HasFail() {
		log.Error("doctor: broken components",
			"fail", report.Totals.Fail,
			"degraded", report.Totals.Degraded,
			"blocked", report.Totals.Blocked,
			"not_configured", report.Totals.NotConfigured,
		)
		return 1, nil
	}
	return 0, nil
}

// importDiscTimeout bounds one disc import: the BMG of the reference disc
// parses in well under a second; the bound exists so a wedged read (a disc
// image on a dying network share) is reported instead of hanging the shell.
const importDiscTimeout = 5 * time.Minute

// runImportDisc is the local disc-import command (docs/spec/04 "Wind Waker
// reference adapter"). It is deliberately a thin, honest driver over the
// adapter and the store: the game-specific knowledge (which file, which tags,
// which region letter) lives in the windwaker adapter, not here.
func runImportDisc(args []string, log *slog.Logger) (int, error) {
	fs := flag.NewFlagSet("import-disc", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, importDiscUsageText) }
	dataDir := fs.String("data-dir", "", "durable data directory; overrides "+bootstrap.EnvDataDir)
	image := fs.String("image", "", "disc image path; defaults to "+EnvGameImage)
	gameSlug := fs.String("game-slug", "wind-waker", "slug of the game to create or reuse")
	publish := fs.Bool("publish", false, "move the draft through review and publish it")
	if err := fs.Parse(args); err != nil {
		return 2, fmt.Errorf("%w: %v", errUsage, err)
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "game-narrator import-disc: unexpected argument %q\n", fs.Arg(0))
		return 2, fmt.Errorf("%w: unexpected argument", errUsage)
	}
	if *gameSlug == "" {
		fmt.Fprint(os.Stderr, importDiscUsageText)
		return 2, fmt.Errorf("%w: --game-slug must be non-empty", errUsage)
	}

	// The image path resolves from the flag first, then the environment. The
	// path is never logged and never stored: only the BMG's content hash is.
	imagePath := strings.TrimSpace(*image)
	if imagePath == "" {
		imagePath = strings.TrimSpace(os.Getenv(EnvGameImage))
	}
	if imagePath == "" {
		fmt.Fprint(os.Stderr, importDiscUsageText)
		return 2, fmt.Errorf("%w: no disc image: pass --image or set %s", errUsage, EnvGameImage)
	}
	if _, err := os.Stat(imagePath); err != nil {
		return 1, fmt.Errorf("disc image not readable: %w", err)
	}

	cfg, err := bootstrap.LoadConfig("", *dataDir, os.LookupEnv)
	if err != nil {
		return 2, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), importDiscTimeout)
	defer cancel()

	st, err := openDataStore(ctx, cfg.DataDir, log)
	if err != nil {
		return 1, err
	}
	defer closeStore(st, log)

	assetStore, err := assets.NewLocal(AssetDir(cfg.DataDir), assets.WithMaxSize(maxAssetBlobBytes))
	if err != nil {
		return 1, fmt.Errorf("opening the asset store in %s: %w", AssetDir(cfg.DataDir), err)
	}

	// Open the disc in place. gcdisc never copies the image; it reads the
	// header, the filesystem and the one RARC the adapter needs.
	d, err := gcdisc.Open(imagePath)
	if err != nil {
		return 1, fmt.Errorf("opening the disc image: %w", err)
	}
	defer d.Close()

	gameID := d.Header().GameID
	if !strings.HasPrefix(gameID, importDiscGameIDPrefix) {
		// Refuse a mislabelled image before any parse: the adapter's rules are
		// Wind Waker's, and importing another game's text under them would be a
		// provenance lie the user could not audit later.
		return 1, fmt.Errorf("disc game id %q does not start with %q: the reference adapter only reads Wind Waker images; a different title needs its own adapter", gameID, importDiscGameIDPrefix)
	}
	region := windwaker.DiscRegion(gameID)

	// The adapter is the only place that knows the disc layout and the tag
	// table; the command hands it the open disc and takes canonical messages.
	ww := windwaker.New().(windwaker.Adapter)

	// ── game ────────────────────────────────────────────────────────────────
	game, err := st.GetGameBySlug(ctx, *gameSlug)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			return 1, fmt.Errorf("looking up game %q: %w", *gameSlug, err)
		}
		game = games.Game{
			ID:        games.NewID(),
			Slug:      *gameSlug,
			Title:     "The Legend of Zelda: The Wind Waker",
			Platform:  "gamecube",
			UserTags:  json.RawMessage(`[]`),
			CreatedAt: nowStampRFC3339(),
		}
		if err := st.CreateGame(ctx, game); err != nil {
			return 1, fmt.Errorf("creating game %q: %w", *gameSlug, err)
		}
		log.Info("import-disc: game created", "game", game.ID, "slug", game.Slug)
	}

	// ── edition (region from the disc id, language en) ────────────────────
	editions, err := st.ListEditions(ctx, game.ID)
	if err != nil {
		return 1, fmt.Errorf("listing editions: %w", err)
	}
	var edition *games.Edition
	for i := range editions {
		if editions[i].Region == region && editions[i].Language == "en" {
			edition = &editions[i]
			break
		}
	}
	if edition == nil {
		edition = &games.Edition{
			ID:       games.NewID(),
			GameID:   game.ID,
			Region:   region,
			Language: "en",
			Revision: "1",
		}
		if err := st.CreateEdition(ctx, *edition); err != nil {
			return 1, fmt.Errorf("creating edition: %w", err)
		}
		log.Info("import-disc: edition created", "edition", edition.ID, "region", region)
	}

	// ── draft profile (reuse the existing one so re-runs do not stack) ────
	profiles, err := st.ListProfiles(ctx, edition.ID)
	if err != nil {
		return 1, fmt.Errorf("listing profiles: %w", err)
	}
	profile := games.Profile{}
	for i := range profiles {
		if profiles[i].Status == games.StatusDraft {
			profile = profiles[i]
			break
		}
	}
	if profile.ID == "" {
		profile, err = st.CreateDraftProfile(ctx, edition.ID)
		if err != nil {
			return 1, fmt.Errorf("creating draft profile: %w", err)
		}
		log.Info("import-disc: draft profile created", "profile", profile.ID)
	}

	// ── extract + hash the BMG before anything is written ─────────────────
	bmgBytes, err := windwaker.ExtractBMG(d)
	if err != nil {
		return 1, err
	}
	bmgSHA := sha256.Sum256(bmgBytes)
	bmgHex := hex.EncodeToString(bmgSHA[:])

	// Idempotency by content: if this exact BMG was already imported into this
	// profile, the source row and its messages exist. Report the count and do
	// not write a second copy (a re-run is not a new import).
	sources, err := st.ListSources(ctx, profile.ID)
	if err != nil {
		return 1, fmt.Errorf("listing sources: %w", err)
	}
	sourceID := ""
	for i := range sources {
		if sources[i].ContentSHA256 == bmgHex {
			sourceID = sources[i].ID
			break
		}
	}
	if sourceID != "" {
		existing, _, err := st.ListMessages(ctx, profile.ID, store.MessageFilter{}, "", 1)
		if err != nil {
			return 1, fmt.Errorf("counting existing messages: %w", err)
		}
		_ = existing
		total, err := countProfileMessages(ctx, st, profile.ID)
		if err != nil {
			return 1, err
		}
		fmt.Fprintf(stdout, "already imported: %s @ %s (profile %s, %d messages); nothing written\n",
			windwaker.ID, windwaker.Version, profile.ID, total)
		return 0, nil
	}

	// ── source row ─────────────────────────────────────────────────────────
	// The URI is a descriptive scheme, not a file:// path: it names what the
	// bytes are without committing the operator's absolute path into the store
	// (docs/spec/14), and it sidesteps the file:// + rights-unknown special
	// case by carrying rights user_supplied from the start.
	source := games.Source{
		ID:            games.NewID(),
		ProfileID:     profile.ID,
		SourceType:    games.SourceGameExtract,
		URI:           "gcdisc://" + gameID + "/res/Msg/bmgres.arc",
		RetrievedAt:   nowStampRFC3339(),
		ContentSHA256: bmgHex,
		MimeType:      "application/octet-stream",
		Rights:        games.RightsUserSupplied,
		Trust:         1.0,
		Parser:        windwaker.ID + "@" + windwaker.Version,
		CoverageNotes: "windwaker reference import",
	}
	sourceID = source.ID

	// ── parse ──────────────────────────────────────────────────────────────
	res, err := ww.ParseDisc(ctx, d, adapter.ParseOptions{
		ProfileID: profile.ID,
		SourceID:  sourceID,
		Language:  "en",
	})
	if err != nil {
		return 1, fmt.Errorf("parsing the disc: %w", err)
	}

	// The coverage note is counts and structure only: how many entries the
	// archive held, how many became rows, the rules revision, and the honest
	// graph decision. No message text.
	source.CoverageNotes = fmt.Sprintf(
		"%s@%s: %d entries, %d imported, %d skipped; rules %s; %s",
		windwaker.ID, windwaker.Version, res.Stats.Rows, res.Stats.Imported, res.Stats.Skipped,
		windwaker.RulesRevision, windwaker.GraphNote)

	if err := st.PutSources(ctx, profile.ID, []games.Source{source}); err != nil {
		return 1, fmt.Errorf("writing the source row: %w", err)
	}

	// ── messages ───────────────────────────────────────────────────────────
	// Batch the write: 4 411 rows in one transaction is fine on the target
	// hardware, and a mid-transaction failure rolls the whole import back
	// rather than leaving a half profile (docs/spec/02 "Transaction workflows").
	if err := st.PutMessages(ctx, profile.ID, res.Messages, nil); err != nil {
		return 1, fmt.Errorf("writing messages: %w", err)
	}

	// ── asset store (the BMG bytes, and only the BMG bytes) ───────────────
	staged, info, err := assetStore.PutStaged(ctx, bytes.NewReader(bmgBytes), bmgHex)
	if err != nil {
		return 1, fmt.Errorf("staging the BMG in the asset store: %w", err)
	}
	if _, err := assetStore.Commit(ctx, staged); err != nil {
		assetStore.Abort(ctx, staged)
		return 1, fmt.Errorf("committing the BMG to the asset store: %w", err)
	}
	log.Info("import-disc: BMG stored in the local asset store", "sha256_prefix", info.SHA256[:12], "bytes", len(bmgBytes))

	fmt.Fprintf(stdout, "imported %d messages (%d skipped) from %s into profile %s\n",
		res.Stats.Imported, res.Stats.Skipped, gameID, profile.ID)

	// ── optional publish ───────────────────────────────────────────────────
	if *publish {
		if err := publishDraft(ctx, st, profile.ID, log); err != nil {
			return 1, fmt.Errorf("publishing: %w", err)
		}
		fmt.Fprintf(stdout, "published profile %s\n", profile.ID)
	}
	return 0, nil
}

// publishDraft moves a draft through review and publishes it, re-reading the
// row version at each hop so a concurrent editor is detected, not clobbered
// (docs/spec/12's revision token, applied to the CLI's own two hops).
func publishDraft(ctx context.Context, st *store.Store, profileID string, log *slog.Logger) error {
	p, rev, err := st.GetProfile(ctx, profileID)
	if err != nil {
		return fmt.Errorf("reading the profile: %w", err)
	}
	if err := st.SetProfileStatus(ctx, profileID, games.StatusReview, rev); err != nil {
		return fmt.Errorf("moving %s to review: %w", p.Status, err)
	}
	p, rev, err = st.GetProfile(ctx, profileID)
	if err != nil {
		return fmt.Errorf("re-reading the profile: %w", err)
	}
	if _, err := st.Publish(ctx, profileID, rev, "import-disc"); err != nil {
		return err
	}
	log.Info("import-disc: profile published", "profile", profileID)
	return nil
}

// countProfileMessages counts a profile's messages with one bounded scan.
func countProfileMessages(ctx context.Context, st *store.Store, profileID string) (int, error) {
	total := 0
	cursor := ""
	for {
		msgs, next, err := st.ListMessages(ctx, profileID, store.MessageFilter{}, cursor, 500)
		if err != nil {
			return total, err
		}
		total += len(msgs)
		if next == "" {
			return total, nil
		}
		cursor = next
	}
}

// nowStampRFC3339 is the store's timestamp spelling: UTC RFC3339, so rows this
// command writes sort and compare with rows the server writes.
func nowStampRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }
