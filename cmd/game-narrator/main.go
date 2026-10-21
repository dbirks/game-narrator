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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dbirks/game-narrator/internal/agent"
	"github.com/dbirks/game-narrator/internal/agent/tools"
	"github.com/dbirks/game-narrator/internal/api"
	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/assistant"
	"github.com/dbirks/game-narrator/internal/audio"
	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/bootstrap"
	"github.com/dbirks/game-narrator/internal/bt"
	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/capture/mode"
	"github.com/dbirks/game-narrator/internal/capture/stack"
	"github.com/dbirks/game-narrator/internal/clips"
	"github.com/dbirks/game-narrator/internal/doctor"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/games/adapter"
	"github.com/dbirks/game-narrator/internal/games/adapter/generic"
	"github.com/dbirks/game-narrator/internal/games/adapter/windwaker"
	"github.com/dbirks/game-narrator/internal/gcdisc"
	"github.com/dbirks/game-narrator/internal/jobs"
	"github.com/dbirks/game-narrator/internal/logbuf"
	"github.com/dbirks/game-narrator/internal/pipeline"
	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/asr"
	"github.com/dbirks/game-narrator/internal/providers/openai"
	"github.com/dbirks/game-narrator/internal/providers/router"
	"github.com/dbirks/game-narrator/internal/providers/tts"
	"github.com/dbirks/game-narrator/internal/restore"
	"github.com/dbirks/game-narrator/internal/runtime"
	"github.com/dbirks/game-narrator/internal/setupboard"
	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/store"
	"github.com/dbirks/game-narrator/internal/telemetry"
	"github.com/dbirks/game-narrator/internal/updates"
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
  ocr      run the product's recognition on one live frame or image (issue #1)
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
  GAME_NARRATOR_READINESS_INTERVAL
    	schedule of the database integrity check behind /health/ready
    	(ADR-0097): a Go duration such as 5m; unset or malformed falls back to
    	the 5m default. The full check never runs per request.
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

// EnvImportDir is the environment variable that names the allowlisted
// directory the API's disc-import surface reads from (ADR-0066). Like the
// game-image path it is operator-local configuration: the API never answers
// with the directory's contents beyond basenames and never logs the path.
const EnvImportDir = "GAME_NARRATOR_IMPORT_DIR"

// EnvReadinessInterval is the schedule of the database integrity check that
// backs /health/ready (ADR-0097): a Go duration ("5m", "30s"); unset or
// malformed falls back to store.DefaultReadinessInterval. The full check
// costs ~3 s on the A53, so the interval is a trade between how stale a
// corruption answer may be and how often the target pays that cost.
const EnvReadinessInterval = "GAME_NARRATOR_READINESS_INTERVAL"

// EnvBluetoothFake is the Bluetooth surface's labelled-synthetic seam
// (ADR-0091): "1" or "true" wires the deterministic fake controller (no
// radio, no bus) instead of BlueZ over D-Bus — the same pattern as
// GAME_NARRATOR_CAPTURE_FAKE and the audio fake devices. UI development,
// contract checks and the Playwright walk run against it; every answer the
// fake serves is labelled synthetic.
const EnvBluetoothFake = "GAME_NARRATOR_BT_FAKE"

// EnvMetricsToken is the static bearer token the /metrics scrape accepts in
// place of an admin session (docs/spec/13 "Metrics", ADR-0106): a monitor that
// does not want to mint an admin user token. Empty (the default) keeps the
// scrape admin-only; the developer snapshot endpoint never uses it.
const EnvMetricsToken = "GAME_NARRATOR_METRICS_TOKEN"

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
	// The process log is JSON to stderr (the journald). ADR-0087 adds a
	// bounded fan-out copy: the in-process ring that /diagnostics' redacted
	// logs section reads. The stderr handler stays first — the operator can
	// always read the real log even if the ring is broken.
	ring := logbuf.New(logbuf.DefaultCapacity)
	log := slog.New(logbuf.Fanout(
		slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}),
		ring.Handler(),
	))

	code, err := run(args, log, ring)
	if err != nil && !errors.Is(err, errUsage) {
		log.Error("fatal", "error", err)
	}
	return code
}

var errUsage = errors.New("usage")

func run(args []string, log *slog.Logger, ring *logbuf.Ring) (int, error) {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usageText, serveUsageText)
		return 2, errUsage
	}

	switch cmd := args[0]; cmd {
	case "version":
		fmt.Println(bootstrap.Version())
		return 0, nil
	case "help", "-h", "--help":
		fmt.Print(usageText, serveUsageText, migrateUsageText, adminUsageText, doctorUsageText, benchUsageText, importDiscUsageText, supportBundleUsageText, ocrUsageText)
		return 0, nil
	case "serve":
		return runServe(args[1:], log, ring)
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
	case "ocr":
		return runOCR(args[1:], log)
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

// logStartup records the "game-narrator starting" line with the resolved
// durable storage: the data directory, the database file and the setting
// that decided it (issue #31: the line has to say where the data lives so
// a run that fell back to the default directory is visible on its very
// first log line, ADR-0126).
func logStartup(log *slog.Logger, build bootstrap.BuildInfo, cfg bootstrap.Config) {
	log.Info("game-narrator starting",
		append(build.Values(),
			"data_dir", cfg.DataDir,
			"data_dir_source", string(cfg.DataDirSource),
			"database", store.DBPath(cfg.DataDir))...)
}

func runServe(args []string, log *slog.Logger, ring *logbuf.Ring) (int, error) {
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
	logStartup(log, build, cfg)

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

	// A pending restore swap (ADR-0090) applies after this build's
	// migrations and before the ready answer: the previous process took the
	// pre-restore copy, wrote the plan and shut down, and this boot is the
	// restart it was counting on. A failed swap recovers the pre-restore
	// snapshot and still serves; the only hard failure is one that cannot
	// even open that. st is reassigned to the swapped store, so the deferred
	// close below releases the store the process actually runs on.
	st, restoreOutcome, err := restore.Apply(ctx, cfg.DataDir, st, log)
	if err != nil {
		return 1, fmt.Errorf("applying the pending restore plan: %w", err)
	}
	if restoreOutcome != nil {
		log.Info("restore plan resolved at boot", "status", restoreOutcome.Status, "error", restoreOutcome.Error)
	}

	schemaVersion, err := st.SchemaVersion(ctx)
	if err != nil {
		return 1, fmt.Errorf("reading schema version: %w", err)
	}
	log.Info("database ready", "applied", len(applied), "schema_version", schemaVersion)

	// Readiness cache (ADR-0097): the full integrity check runs here, before
	// the listener opens, and then on a schedule — never per request, where
	// the 2 s probe budget the A53 cannot meet (issue #9) made every answer
	// a 503. A startup verdict failure (an unrepairable file) does not stop
	// the boot: /health/ready answers 503 from the cached verdict and the
	// operator path is backup/restore.
	rdyInterval := readinessInterval(os.LookupEnv)
	readiness := store.NewReadiness(st, rdyInterval, log)
	readiness.Check(ctx)
	if _, ok, _, _ := readiness.Snapshot(); !ok {
		log.Warn("startup integrity check did not pass: /health/ready answers 503 from the cached verdict until the next scheduled check passes", "interval", rdyInterval.String())
	}
	go readiness.Run(ctx)

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
	// The Wind Waker reference adapter (docs/spec/04): the disc-import surface
	// (ADR-0066) needs its DiscExtractor capability, and linking it also makes
	// raw .bmg file imports work through the API the way the CLI does them.
	if err := windwaker.Register(adapters); err != nil {
		return 1, fmt.Errorf("linking the windwaker disc adapter: %w", err)
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

	// The setup board (ADR-0108, UX-06): the typed SSE stage events the
	// onboarding agent and the voices workflow emit at each real transition,
	// and the snapshot the board route derives from the same durable rows.
	// The emitter degrades toward silence: with the hub or the store unwired
	// it publishes nothing, and nothing here fails the boot over a missing
	// board.
	stageBoard := setupboard.New(hub, st, store.NewAgentStore(st), eventStore, log)

	// Job workers (docs/spec/01: API, jobs and capture in one program on
	// UNO Q). Recovery runs before workers start claiming, so a row left
	// `running` by the last process becomes `recoverable`/`queued` before any
	// live lease exists to race it (docs/spec/02 "Transaction workflows").
	jobsStore := store.NewJobsStore(st)

	// The process's telemetry surface (docs/spec/13 "Metrics", ADR-0106): one
	// catalog the whole process books into — the pipeline's stages, the voice
	// wiring's TTS/playback/prewarm and the session count ride on it, and the
	// two scrape endpoints (GET /metrics, GET /api/v1/dev/metrics) serve it.
	// The scrape token is operator-supplied (a monitor that does not want to
	// mint an admin user token); empty keeps the endpoint admin-only.
	metrics := telemetry.NewProduct()
	metricsToken := strings.TrimSpace(os.Getenv(EnvMetricsToken))
	if metricsToken != "" {
		log.Info("the metrics scrape accepts the configured token (GAME_NARRATOR_METRICS_TOKEN)")
	}
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
	// The disc-import worker (ADR-0066): the API verifies the image and queues
	// this kind, so the queue reports progress and the report. Resumable for
	// the same reason import.file is — the payload names a basename or a
	// content hash, not a temporary file, so a worker that starts after a
	// crash reads the same image and the importer reconciles by content hash.
	if err := runner.Register(api.JobKindImportDisc,
		api.ImportDiscJobHandler(st, assetStore, adapters), jobs.RegisterOpts{
			Concurrency: 1,
			Resumable:   true,
			Timeout:     importJobTimeout,
		}); err != nil {
		return 1, fmt.Errorf("registering %s job kind: %w", api.JobKindImportDisc, err)
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
	agentHandler, agentCloser, err := newAgentWiring(st, assetStore, adapters, hub, stageBoard)
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
	voiceSvc, voiceCloser, err := newVoiceWiring(st, assetStore, log, stageBoard)
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

	// Session runtime (Milestone 2, ADR-0024): the one live pipeline run, its
	// lifecycle events and its developer-trace evidence. Dev-trace storage
	// sits beside the asset store in the durable directory (same reasoning as
	// AssetDir: evidence and catalogue are one body of state), and the replay
	// allowlist is the documented default — GAME_NARRATOR_REPLAY_ROOTS, else
	// testdata/ from a dev checkout, else nothing in production.
	devTraceDir := filepath.Join(cfg.DataDir, "dev-trace")
	// The managed replay clip store (ADR-0073): inside the data directory, so
	// a clip installed by the API lives in the durable state a backup copies
	// (the backup route ships the store's directories alongside the database)
	// and so the runtime always treats it as a replay root — the product path
	// by which a recorded clip reaches a running appliance without an
	// operator editing /etc/game-narrator/env and restarting the service.
	clipsDir := filepath.Join(cfg.DataDir, "replay-clips")
	// A staging directory is what an interrupted upload left behind; it is
	// private state with no reader, so the process that starts answers for
	// it rather than the disk growing one entry per interrupted install.
	if n, perr := clips.PruneStaging(clipsDir); perr != nil {
		log.Warn("pruning the clip store's staging directories", "error", perr)
	} else if n > 0 {
		log.Info("removed stale clip staging directories", "count", n)
	}
	// The session's speech pipeline (docs/spec/07, ADR-0039): wired only when
	// the voice workflow is — a silent appliance keeps answering the audio
	// route 503 rather than applying a control to nothing. The sink is the
	// operator's configuration (filesink under the durable directory by
	// default), and a sink that cannot be built degrades the session to
	// silent, never the run.
	var speechOpts *runtime.SpeechOptions
	if voiceSvc != nil {
		// The session's audio output resolves through the one function the
		// API's audio routes also use (ADR-0074): the environment is the
		// operator's override, the store row is the user's selection, the
		// built-in defaults (filesink, the platform-default sink) come last.
		var stored *audio.Stored
		if as, ok, sErr := st.GetAudioSettings(ctx); sErr != nil {
			return 1, fmt.Errorf("reading the stored audio settings: %w", sErr)
		} else if ok {
			stored = &audio.Stored{
				Device:       as.Device,
				SinkKind:     as.SinkKind,
				MasterVolume: as.MasterVolume,
				Set:          true,
			}
		}
		sel, err := audio.Resolve(os.LookupEnv, stored)
		if err != nil {
			return 1, fmt.Errorf("audio sink configuration: %w", err)
		}
		log.Info("audio output resolved",
			"sink", sel.SinkKind, "sink_source", sel.SinkKindSource,
			"device", sel.Device, "device_source", sel.DeviceSource,
			"master_volume", sel.MasterVolume, "master_source", sel.MasterVolumeSource)
		speechOpts = &runtime.SpeechOptions{
			Voices:         voiceSvc,
			Assets:         assetStore,
			Sink:           sel.SinkKind,
			FileSinkDir:    filepath.Join(cfg.DataDir, "audio"),
			PipeWireTarget: sel.Device,
			MasterVolume:   sel.MasterVolume,
		}
	}
	// The VLM OCR fallback (docs/spec/05, ADR-0040): wired only when the
	// vision_ocr_fallback role is configured — an unconfigured role is the
	// documented "NOT CONFIGURED" degraded mode, and sessions then run the
	// single local engine exactly as before. The secondary is an ocr.VLMEngine
	// over the role's OpenAI-compatible client; the runtime wraps each
	// session's OCR engine with it (per-session rate limit and crop cache).
	ocrFallback, ocrFallbackModel, err := newOCRFallbackWiring(os.LookupEnv)
	if err != nil {
		return 1, fmt.Errorf("wiring the VLM OCR fallback: %w", err)
	}
	// The Clef adjudication (docs/spec/05, ADR-0050): wired only when the
	// decision role is a configured System One role — an unconfigured role
	// is the documented "NOT CONFIGURED" degraded mode, and sessions commit
	// locally exactly as before. The call flows through the provider router
	// (breaker and the role's per-attempt timeout, docs/spec/06 "Routing,
	// reliability and replicas"); the runtime bounds the whole adjudication
	// and keeps at most one in flight per session.
	decisionOpts, decisionModel, err := newDecisionWiring(os.LookupEnv)
	if err != nil {
		return 1, fmt.Errorf("wiring the Clef decision role: %w", err)
	}
	// Capture stack (ADR-0078): the process's one capture layer — the device
	// enumeration, the ADR-0076 mode selection, the persisted selection and
	// the V4L2 source seam. The same instance feeds both the session runtime
	// (a v4l2 session resolves the selection and opens the node) and the API
	// (the device surface, preview and probe), so the selection that starts a
	// session and the one the Settings page shows are the same row. The fake
	// seam (GAME_NARRATOR_CAPTURE_FAKE) is the ADR's labelled-synthetic test
	// path; production wires the real ffmpegpipe V4L2 constructor.
	capStack := stack.DefaultOptions(os.LookupEnv, captureSelectionStore{st: st}, log)
	sessions, err := runtime.New(runtime.Options{
		Store:       st,
		Hub:         hub,
		Jobs:        runner,
		AssetsRoot:  devTraceDir,
		ReplayRoots: runtime.DefaultReplayRoots(os.LookupEnv),
		ClipsDir:    clipsDir,
		Speech:      speechOpts,
		OCRFallback: ocrFallback,
		Decision:    decisionOpts,
		// The telemetry surface (ADR-0106): the session's pipeline and voice
		// stages book their counters and stage latencies here, labelled by the
		// game the profile's edition belongs to and the profile revision.
		Metrics: metrics,
		Capture: &runtime.CaptureOptions{
			Resolve: capStack.Resolve,
			Source: func(ctx context.Context, res stack.Resolved) (capture.Source, error) {
				return capStack.Source(ctx, res.NodePath(), res.Mode)
			},
		},
		Log: log,
	})
	if err != nil {
		return 1, fmt.Errorf("creating the session runtime: %w", err)
	}
	// Settle the process-shared OCR pool at startup, before any session
	// (issue #13, ADR-0100): its warm Tesseract slots and the startup probe
	// spawn in the background, so a session's first frame no longer pays
	// the ~0.6 s per-process engine startup on the critical path — the
	// first read costs the inference only. A configured OCR fallback or a
	// missing tesseract binary never blocks here (the pool's contract).
	sessions.WarmOCR()
	if ocrFallback != nil {
		log.Info("VLM OCR fallback ready", "model", ocrFallbackModel)
	} else {
		log.Info("VLM OCR fallback not configured: sessions run the local OCR engine only (role vision_ocr_fallback)")
	}
	if decisionOpts != nil {
		log.Info("Clef decision role ready", "model", decisionModel)
	} else {
		log.Info("Clef decision role not configured: sessions commit locally (role decision)")
	}
	log.Info("session runtime ready",
		"dev_trace_root", filepath.Base(devTraceDir),
		"clip_store", filepath.Base(clipsDir),
		"replay_roots", len(runtime.DefaultReplayRoots(os.LookupEnv)))

	// The context-aware assistant (ADR-0099): the assistant.run kind executes
	// the same durable loop with the assistant's own allowlist and versioned
	// prompt. The model is the same agent_reasoning role boundary the
	// onboarding run uses (qwen3-8-27b today; that gateway rejects
	// response_format and tool_choice, so the loop's prompt-and-validate is
	// the contract the assistant's tools rely on). A missing role is the same
	// degraded mode as for onboarding: the routes stay up, the attempt blocks
	// with the reason and resumes once configured. The tool backends degrade
	// individually: a nil voice service or session manager leaves the routes
	// serving and the tools that need them answering a failed envelope.
	assistantHandler, err := newAssistantWiring(st, hub, voiceSvc, sessions, eventStore, runner)
	if err != nil {
		return 1, fmt.Errorf("wiring the assistant agent: %w", err)
	}
	if err := runner.Register(assistant.JobKind, assistantHandler, assistant.JobHandlerOpts()); err != nil {
		return 1, fmt.Errorf("registering %s job kind: %w", assistant.JobKind, err)
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

	// The update notification (ADR-0088): the appliance's bounded check
	// against the releases of the repository this build belongs to, with its
	// offline cache in the data directory. The first check runs in the
	// background, so the settings page renders either the cached answer or
	// the honest "first check still running" — an offline appliance never
	// makes the page wait for a network it does not have.
	updateCfg := updates.ConfigFromEnv(os.LookupEnv, filepath.Join(cfg.DataDir, "updates", "latest.json"))
	updatesChecker := updates.New(updateCfg)
	if updateCfg.Enabled {
		log.Info("update check ready", "repo", updateCfg.Repo, "interval", updateCfg.Interval.String())
		go updatesChecker.Start(ctx)
	} else {
		log.Info("update check disabled by configuration")
	}

	// The process-replacement seam behind POST /api/v1/admin/restore
	// (ADR-0090): the handler answers the 202 first, and this closure then
	// runs the process's normal graceful shutdown. Under the documented
	// deployment (the systemd unit, Restart=always) the supervisor brings
	// the replacement up within seconds and its boot applies the pending
	// plan; without a supervisor the plan stays pending and the status
	// route says exactly that — a restore that cannot be applied is never
	// pretended to be.
	restarter := func() error {
		// Flush grace for the 202: the response is written into the
		// connection and the kernel delivers it after the socket closes.
		time.Sleep(300 * time.Millisecond)
		cancelRun()
		return nil
	}

	// The Bluetooth output surface (ADR-0091, issues #2/#3): the labelled-
	// synthetic seam first (GAME_NARRATOR_BT_FAKE, the capture-fake pattern),
	// then the real D-Bus controller; a bus the process cannot reach leaves
	// the surface unwired on purpose (honest degradation over a fake radio).
	var btController bt.Controller
	switch v := os.Getenv(EnvBluetoothFake); {
	case v == "1" || strings.EqualFold(v, "true"):
		btController = bt.DefaultFake()
		log.Warn("the Bluetooth surface is the LABELLED-SYNTHETIC fake (GAME_NARRATOR_BT_FAKE): no radio is touched")
	default:
		c, err := bt.New(ctx)
		if err != nil {
			log.Warn("the Bluetooth surface is unwired (the routes answer 503 and the page hides the section)", "error", err)
		} else {
			btController = c
		}
	}
	if btController != nil {
		// The ADR-0091 auto-reconnect: when the persisted audio selection is
		// a BlueZ A2DP sink (bluez_output.<MAC>.1, the stored row or the
		// operator's environment override) the radio is told to hold that
		// device connected before the first narration is spoken. BlueZ
		// reconnects trusted devices itself; the bounded retry covers the
		// states it does not (the LE-only connection of issue #3, a device
		// still waking from pairing mode). It runs in the background and
		// logs honestly — a narration run is never held up on a speaker.
		var storedDevice string
		if as, ok, sErr := st.GetAudioSettings(ctx); sErr != nil {
			log.Warn("reading the stored audio selection for the Bluetooth auto-reconnect", "error", sErr)
		} else if ok {
			storedDevice = as.Device
		}
		go btAutoReconnect(runCtx, btController, os.LookupEnv, storedDevice, log)
	}

	// The tailnet detection (UX-13, ADR-0110 §4): run it here, at startup
	// — after config, before the listener comes up — so the one bounded
	// `tailscale ip` subprocess (the fallback half) and the interface scan
	// happen outside the request path forever: the value is cached on the
	// server and served for the process's life. A detection failure degrades
	// to the explicit "not configured" state, never a boot failure.
	tailnet := api.DetectTailnet(ctx, api.TailnetDeps{
		Port: api.TailnetPort(cfg.Listen),
	})

	apiOpts := []api.Option{
		api.WithReadiness(storeReadiness(st, readiness, log)),
		api.WithEvents(hub),
		api.WithJobs(jobsStore),
		// The background queue's foreground pause (ADR-0119, issue #27): the
		// status snapshot and the job list show {paused_by, since,
		// queued_count} for as long as a live session holds the budget, so
		// a stalled queue is never a silent one.
		api.WithJobRunner(runner),
		api.WithSessions(sessions),
		// The bound listen address (ADR-0110 §4): the real bind, not the
		// environment fallback — the settings security section and the
		// system-info surface report what the box actually listens on.
		api.WithListen(cfg.Listen),
		// The cached tailnet answer (UX-13): the settings page's tailnet URL
		// and the system-info surface read this value, never a re-detection.
		api.WithTailnet(tailnet),
		api.WithOnboarding(runner, store.NewAgentStore(st), tools.NewSQLiteApprovals(st)),
		// The setup board snapshot (ADR-0108, UX-06): the GET .../board read
		// the setup page's page load rebuilds the board from, derived from
		// the same durable rows the SSE stage events carry.
		api.WithSetupBoard(stageBoard),
		// The context-aware assistant (ADR-0099): the dock's backend. The
		// same store the catalog routes read (the tools work on catalogue
		// rows), the runner the attempts queue on, the hub the reply cards
		// publish to, and the tool backends (voice service, session runtime,
		// durable event log) each at their real seams.
		api.WithAssistant(st, runner, hub, voiceSvc, sessions, eventStore),
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
		api.WithDiscImport(strings.TrimSpace(os.Getenv(EnvImportDir))),
		// The managed replay clip store (ADR-0073): the same asset store the
		// imports commit to (provenance is the archive's content hash) and the
		// managed directory the runtime treats as a replay root. Both halves
		// are wired here, so the API answers the real store rather than 503.
		api.WithClips(assetStore, clipsDir),
		// The capture device surface (ADR-0078): the same stack the runtime
		// resolves through, so the preview and probe read the device the
		// selection row names, and the busy check sees the running session's
		// hold on it.
		api.WithCapture(capStack),
		api.WithTrustedProxy(trustedProxyCIDRs(os.LookupEnv)...),
		// The redacted recent system logs (ADR-0087): the bounded fan-out
		// copy of the process log the /diagnostics page tails. Wired here,
		// so the route reads the real ring rather than 503.
		api.WithLogRing(ring),
		// The restore pair's two process-lifecycle seams (ADR-0090) and the
		// update check the /settings page renders (ADR-0088).
		api.WithRestarter(restarter),
		api.WithUpdates(updatesChecker),
		// The Bluetooth output surface (ADR-0091, issues #2/#3): the real
		// controller talks to BlueZ over D-Bus (the AgentManager's
		// NoInputNoOutput agent plus the adapter and device ObjectManager
		// interfaces); a bus the process cannot reach degrades to unwired —
		// the routes answer 503 and the page hides the section, never an
		// empty 200 that would read as "no devices".
		api.WithBluetoothController(btController),
		// The telemetry surface (docs/spec/13 "Metrics", ADR-0106): the
		// Prometheus scrape at /metrics (admin or the configured token, off
		// until a catalog is wired) and the developer JSON snapshot the
		// diagnostics and health surfaces read. The same catalog the runtime
		// books into, so the scrape and the session's own stages agree.
		api.WithMetrics(metrics, metricsToken),
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
func newAgentWiring(st *store.Store, assetStore *assets.Local, adapters *adapter.Registry, hub *events.Hub, stageBoard *setupboard.Emitter) (jobs.Handler, func(), error) {
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
		// The setup board's stage observer (ADR-0108): the loop calls it
		// after each succeeded tool call, and the emitter maps the
		// stage-relevant tools to the board's transitions.
		SetupStages: stageBoard.ObserveTool,
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

// newAssistantWiring assembles the assistant agent's job handler (ADR-0099):
// the same agent_reasoning model boundary as the onboarding run, the
// assistant's own Deps (store quartet, hub, tool backends, queue) and the
// per-attempt registry the tools close over. The tool backends degrade
// individually — a nil voice service or session manager leaves the routes
// serving and the tools that need them answering a failed envelope — so
// nils are passed straight through and only the required store quartet is a
// wiring fault the handler refuses.
func newAssistantWiring(
	st *store.Store,
	hub *events.Hub,
	voiceSvc *voices.Service,
	sessions *runtime.Manager,
	eventStore events.Store,
	enq assistant.JobEnqueuer,
) (jobs.Handler, error) {
	roles, err := providers.LoadRoles(os.LookupEnv)
	if err != nil {
		return nil, fmt.Errorf("loading provider roles: %w", err)
	}
	roleReg := providers.NewRegistry(roles)
	chatRouter := router.New(roleReg, router.Options{})
	// The same model boundary as the onboarding run: one router call over the
	// agent_reasoning role. The assistant's prompt-and-validate loop is what
	// that role's gateway (qwen3-8-27b today) supports.
	model := agent.RouterCall(roleReg, chatRouter)

	deps := assistant.Deps{
		Store:      st,
		AgentStore: store.NewAgentStore(st),
		Threads:    store.NewThreadStore(st),
		Questions:  store.NewQuestionStore(st),
		Hub:        hub,
		Events:     eventStore,
		Queue:      enq,
	}
	if voiceSvc != nil {
		deps.Voices = voiceSvc
	}
	if sessions != nil {
		deps.Sessions = narrationController{mgr: sessions}
	}
	return assistant.NewJobHandler(model, deps, roles[providers.RoleAgentReasoning].Model)
}

// narrationController adapts *runtime.Manager to the assistant's
// NarrationController: the tool's start/stop maps onto the session
// lifecycle the /api/v1/sessions routes drive, so the dock's "start
// narrating" and the page's start button start the same kind of session
// (one at a time, the runtime's own invariant). The frame-source kind
// defaults to v4l2 — the appliance's live HDMI capture path — and the mode
// defaults to conservative, the same defaults the API's start route applies.
type narrationController struct{ mgr *runtime.Manager }

func (n narrationController) StartSession(ctx context.Context, req assistant.StartNarration) (assistant.SessionRef, error) {
	kind := req.FrameSourceKind
	if kind == "" {
		kind = runtime.SourceKindV4L2
	}
	mode := req.Mode
	if mode == "" {
		mode = runtime.ModeConservative
	}
	info, err := n.mgr.Start(ctx, runtime.StartRequest{
		ProfileID:   req.ProfileID,
		FrameSource: runtime.FrameSource{Kind: kind},
		Mode:        mode,
	})
	if err != nil {
		return assistant.SessionRef{}, err
	}
	return assistant.SessionRef{
		SessionID: info.ID, ProfileID: info.ProfileID,
		Mode: info.Mode, State: info.State,
	}, nil
}

func (n narrationController) StopActiveSession(ctx context.Context) (assistant.SessionRef, error) {
	info, ok := n.mgr.Active()
	if !ok {
		return assistant.SessionRef{}, fmt.Errorf("no session is running")
	}
	stopped, err := n.mgr.Stop(info.ID)
	if err != nil {
		return assistant.SessionRef{}, err
	}
	return assistant.SessionRef{
		SessionID: stopped.ID, ProfileID: stopped.ProfileID,
		Mode: stopped.Mode, State: stopped.State,
	}, nil
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
func newVoiceWiring(st *store.Store, assetStore *assets.Local, log *slog.Logger, stageBoard *setupboard.Emitter) (*voices.Service, func(), error) {
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
		Registry: registry,
		// The speech cache's index is store-backed (migration 0011, ADR-0039
		// amendment): a restart reuses the asset-backed WAVs instead of
		// re-synthesising the corpus. The reference checker is the same store
		// (ADR-0041): eviction may delete a blob only after the store proves
		// no catalogue row still names it — without it, eviction is
		// index-only and every delete is refused.
		Speech:        speech.New(assetStore, store.NewSpeechCacheIndex(st), 8, speech.WithReferenceChecker(st)),
		InstanceID:    voices.DefaultInstanceID,
		BaseModelID:   cloneCfg.Model,
		BackendDigest: previewBackendDigest(cloneCfg.Model),
		// The setup board's take/approval transitions (ADR-0108): the job's
		// fill and the consent-gated approval are the voice workflow's real
		// stage moments, and the emitter resolves the board they belong to.
		StageObserver: stageBoard,
	}
	// ASR QA (FR-10, ADR 0061): the speech_asr role is independent of the
	// voice roles. Configured, the audition read-back and the generated-line
	// QA-on-insert run; unconfigured, both degrade to "not run" — neither
	// playback nor approval ever blocks on ASR.
	if asrCfg := roles[providers.RoleSpeechASR]; asrCfg.Configured() {
		asrClient, err := asr.NewFromRole(asrCfg)
		if err != nil {
			return nil, nil, fmt.Errorf("speech_asr client: %w", err)
		}
		svc.ASR = asrClient
		qa := speech.NewQAEngine(svc.Speech, asrClient)
		qa.Log = func(format string, args ...any) {
			log.Info(format, append([]any{"qa", "speech_asr"}, args...)...)
		}
		svc.QA = qa
		log.Info("speech_asr QA wired", "asr_model", asrCfg.Model, "wer_bound", speech.WERBound())
	} else {
		log.Info("speech_asr QA not wired: the role is not configured; the ASR checks degrade to not-run (ADR 0061)")
	}
	// Auto-cast (ADR-0109, UX-03): the dial and roster the voice job reads
	// come straight off the store the voice service already owns — the same
	// catalogue the onboarding run wrote the characters into.
	svc.CastProfiles = voices.NewStoreCastProfiles(st)
	if err := voices.New(svc); err != nil {
		return nil, nil, fmt.Errorf("voice service: %w", err)
	}
	log.Info("voice workflow ready",
		"design_model", designCfg.Model, "clone_model", cloneCfg.Model, "instance", svc.InstanceID)
	return svc, func() {}, nil
}

// envOCR_FALLBACK_CONFIDENCE is the process default for the VLM OCR
// fallback's confidence threshold (ADR-0115 §4): a primary read at or above
// it is resolved and no secondary call goes out. A session whose regions set
// the knob in ocr_settings wins over it, and a v4l2 (analog HDMI capture)
// session with neither takes the runtime's per-source default (the measured
// floor of correct crop reads on the board feed).
const envOCR_FALLBACK_CONFIDENCE = "GAME_NARRATOR_OCR_FALLBACK_CONFIDENCE"

// newOCRFallbackWiring builds the VLM OCR fallback's secondary engine from
// the vision_ocr_fallback role (docs/spec/05, ADR-0040): the role's
// OpenAI-compatible client, its timeout and reasoning-effort default applied
// by NewFromRole, behind the ocr.VLMClient adapter and the ocr.VLMEngine the
// runtime's sessions wrap their OCR engine with. A role that is not
// configured is the documented degraded mode — nil, not an error — so the
// sessions keep the single local engine and preflight keeps reporting the
// missing configuration. A malformed GAME_NARRATOR_* value, by contrast, fails
// the boot loudly, the same way the other role wirings do.
func newOCRFallbackWiring(lookupEnv func(string) (string, bool)) (*pipeline.OCRFallback, string, error) {
	roles, err := providers.LoadRoles(lookupEnv)
	if err != nil {
		return nil, "", fmt.Errorf("loading provider roles: %w", err)
	}
	cfg := roles[providers.RoleVisionOCRFallback]
	if !cfg.Configured() {
		return nil, "", nil
	}
	client, err := openai.NewFromRole(cfg)
	if err != nil {
		return nil, "", fmt.Errorf("vision_ocr_fallback client: %w", err)
	}
	out := &pipeline.OCRFallback{
		Secondary: ocr.NewVLMEngine(ocr.OpenAIClient{Client: client}),
	}
	if v, ok := lookupEnv(envOCR_FALLBACK_CONFIDENCE); ok && v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f <= 0 || f > 1 {
			return nil, "", fmt.Errorf("%s: %q is not a confidence threshold in (0,1]", envOCR_FALLBACK_CONFIDENCE, v)
		}
		out.Policy.ConfidenceThreshold = f
	}
	return out, cfg.Model, nil
}

// newDecisionWiring builds the Clef adjudication wiring from the decision
// role (docs/spec/05 "Clef adjudication and commit policy", ADR-0050): the
// role's System One endpoint behind the provider router — the breaker and
// the role's per-attempt timeout live there (docs/spec/06 "Routing,
// reliability and replicas"), and the runtime's decision step composes the
// adjudication the way the scenario harness does. A role that is not
// configured, or that does not speak System One, is the documented degraded
// mode — nil, not an error — so the sessions keep committing locally and
// preflight keeps reporting the missing configuration. A malformed
// GAME_NARRATOR_* value, by contrast, fails the boot loudly, the same way
// the other role wirings do.
func newDecisionWiring(lookupEnv func(string) (string, bool)) (*runtime.DecisionOptions, string, error) {
	roles, err := providers.LoadRoles(lookupEnv)
	if err != nil {
		return nil, "", fmt.Errorf("loading provider roles: %w", err)
	}
	cfg := roles[providers.RoleDecision]
	if !cfg.Configured() || cfg.Protocol != providers.ProtocolSystemOne {
		return nil, "", nil
	}
	return &runtime.DecisionOptions{
		Router: router.New(providers.NewRegistry(roles), router.Options{}),
		Role:   cfg,
	}, cfg.Model, nil
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

// captureSelectionStore adapts the store's capture row to the stack's
// selection seam (ADR-0078): the ADR-0074 app_settings JSON row as the
// stack's GetSelection/SetSelection, keeping the row's spelling (device
// stable id, pinned mode) in exactly one place. The stack never imports the
// store; this adapter is the one seam between the two.
type captureSelectionStore struct{ st *store.Store }

func (s captureSelectionStore) GetSelection(ctx context.Context) (stack.Selection, bool, error) {
	cs, ok, err := s.st.GetCaptureSettings(ctx)
	if err != nil || !ok {
		return stack.Selection{}, ok, err
	}
	sel := stack.Selection{DeviceID: cs.DeviceID}
	if cs.Mode != nil {
		m := mode.Mode{W: cs.Mode.W, H: cs.Mode.H, FPS: cs.Mode.FPS, PixFmt: cs.Mode.PixFmt}
		sel.Mode = &m
	}
	return sel, true, nil
}

func (s captureSelectionStore) SetSelection(ctx context.Context, sel stack.Selection) error {
	cs := store.CaptureSettings{DeviceID: sel.DeviceID}
	if sel.Mode != nil {
		cs.Mode = &store.CaptureMode{W: sel.Mode.W, H: sel.Mode.H, FPS: sel.Mode.FPS, PixFmt: sel.Mode.PixFmt}
	}
	return s.st.SetCaptureSettings(ctx, cs)
}

// btAutoReconnect is the ADR-0091 auto-reconnect: the persisted selection's
// BlueZ A2DP sink (the environment override wins over the stored row, the
// one rule the audio selection's Resolve also follows) is held connected
// with a bounded budget — three attempts, five seconds apart — after which
// the state is logged and the walk ends. A missing adapter or a device that
// will not connect is a logged degraded state, never a retry storm: the
// narration pipeline degrades to whatever sink the selection resolves to,
// and the settings page's repair hint is the operator's way in.
func btAutoReconnect(ctx context.Context, c bt.Controller, lookupEnv func(string) (string, bool), storedDevice string, log *slog.Logger) {
	mac, source := btSelectedSinkMAC(lookupEnv, storedDevice)
	if mac == "" {
		return
	}
	log.Info("bluetooth auto-reconnect: the audio selection is a BlueZ A2DP sink", "address", mac, "source", source)
	for attempt := 1; attempt <= 3; attempt++ {
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
		if err := c.Connect(ctx, mac); err != nil {
			log.Warn("bluetooth auto-reconnect attempt failed", "address", mac, "attempt", attempt, "error", err)
			continue
		}
		log.Info("bluetooth auto-reconnect: the A2DP sink is connected", "address", mac, "attempt", attempt)
		return
	}
	log.Warn("bluetooth auto-reconnect gave up: the device is not connected, the selection degrades to the platform default until it is", "address", mac)
}

// btSelectedSinkMAC finds the MAC behind the selection's sink ID, the same
// precedence as audio.Resolve (the operator's environment override first,
// then the stored row). It answers ("", "") when the selection is not a
// BlueZ A2DP sink — auto-reconnect then has nothing to do.
func btSelectedSinkMAC(lookupEnv func(string) (string, bool), storedDevice string) (mac, source string) {
	if v, ok := lookupEnv(audio.EnvDevice); ok {
		if m, parsed := bt.ParseSinkID(strings.TrimSpace(v)); parsed {
			return m, "environment override " + audio.EnvDevice
		}
	}
	if m, parsed := bt.ParseSinkID(strings.TrimSpace(storedDevice)); parsed {
		return m, "the stored audio selection"
	}
	return "", ""
}

// storeReadiness reports database readiness for GET /api/v1/health/ready.
// The per-request half is the schema-version ping (it needs a live connection
// and a readable file); the integrity verdict comes from the ADR-0097 cache —
// the full check runs at startup and on a schedule, never per request, where
// on the A53 it exceeds the probe's 2 s budget (issue #9). The payload
// carries the verdict's age so a stale "ok" is visible. The cache's verdicts
// are fixed strings: store errors name the database file path, and the API
// surface deliberately carries no host paths.
func storeReadiness(st *store.Store, readiness *store.Readiness, log *slog.Logger) api.ReadinessFunc {
	return func(ctx context.Context) (map[string]any, bool) {
		version, err := st.SchemaVersion(ctx)
		if err != nil {
			log.Warn("readiness: database ping failed", "error", err)
			return map[string]any{"database": map[string]any{"status": "fail", "error": store.PingFailedDetail}}, false
		}
		_, ok, detail, age := readiness.Snapshot()
		db := map[string]any{
			"status":                "ok",
			"schema_version":        version,
			"integrity_age_seconds": int(age.Seconds()),
		}
		if !ok {
			db["status"] = "fail"
			db["error"] = detail
			return map[string]any{"database": db}, false
		}
		return map[string]any{"database": db}, true
	}
}

// readinessInterval resolves the integrity-check schedule: the environment
// wins when it parses as a positive duration; anything else (unset, empty,
// malformed, non-positive) falls back to the default rather than refusing to
// boot — a readiness schedule is a tuning knob, not a prerequisite.
func readinessInterval(lookupEnv func(string) (string, bool)) time.Duration {
	v, ok := lookupEnv(EnvReadinessInterval)
	if !ok {
		return store.DefaultReadinessInterval
	}
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil || d <= 0 {
		return store.DefaultReadinessInterval
	}
	return d
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

	report := doctor.Run(ctx, doctor.Options{DataDir: cfg.DataDir, DataDirSource: cfg.DataDirSource, Dev: *dev,
		// The update row shares the serve process's offline cache, so the CLI
		// and the appliance agree on the last good answer (ADR-0088).
		Updates: doctor.ProbeFromEnv(os.LookupEnv, filepath.Join(cfg.DataDir, "updates", "latest.json")),
		// The Bluetooth row (ADR 0091): the same labelled-synthetic seam as
		// the serve process (GAME_NARRATOR_BT_FAKE), so doctor and the UI
		// document the same surface.
		Bluetooth: doctor.BluetoothProbeFromEnv(os.LookupEnv)})

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
