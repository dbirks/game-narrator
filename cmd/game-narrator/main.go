// Command game-narrator runs the Game Narrator service.
//
// Usage:
//
//	game-narrator serve [--listen host:port] [--data-dir DIR]
//	game-narrator migrate [--data-dir DIR]
//	game-narrator doctor [--data-dir DIR] [--dev] [--json]
//	game-narrator version
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/dbirks/game-narrator/internal/api"
	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/bootstrap"
	"github.com/dbirks/game-narrator/internal/doctor"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/jobs"
	"github.com/dbirks/game-narrator/internal/store"
)

const usageText = `usage: game-narrator <command> [flags]

commands:
  serve    start the HTTP API server (default listen 127.0.0.1:8080)
  migrate  apply pending SQLite migrations and report the schema version
  admin    create or manage accounts and API tokens
  doctor   probe every configured component and report honestly
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
		fmt.Print(usageText, serveUsageText, migrateUsageText, adminUsageText, doctorUsageText)
		return 0, nil
	case "serve":
		return runServe(args[1:], log)
	case "migrate":
		return runMigrate(args[1:], log)
	case "admin":
		return runAdmin(args[1:], log)
	case "doctor":
		return runDoctor(args[1:], log)
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
		if err := hub.Close(); err != nil {
			log.Error("closing event hub", "error", err)
		}
		log.Info("event hub closed")
	}()

	srv := bootstrap.NewServer(cfg, api.New(build, time.Now(), log,
		api.WithReadiness(storeReadiness(st, log)),
		api.WithEvents(hub),
		api.WithJobs(jobsStore),
		api.WithAuth(auth.NewService(authRepo, auth.WithLogger(log)), authRepo),
		api.WithTrustedProxy(trustedProxyCIDRs(os.LookupEnv)...),
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
