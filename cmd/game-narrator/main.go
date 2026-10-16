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
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dbirks/game-narrator/internal/api"
	"github.com/dbirks/game-narrator/internal/bootstrap"
	"github.com/dbirks/game-narrator/internal/doctor"
	"github.com/dbirks/game-narrator/internal/store"
)

const usageText = `usage: game-narrator <command> [flags]

commands:
  serve    start the HTTP API server (default listen 127.0.0.1:8080)
  migrate  apply pending SQLite migrations and report the schema version
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
  GET /api/v1/health/live
  GET /api/v1/health/ready
  GET /api/v1/system/info

flags:
  -data-dir string
    	durable data directory; overrides GAME_NARRATOR_DATA_DIR
  -listen string
    	TCP listen address; overrides GAME_NARRATOR_LISTEN (default "127.0.0.1:8080")
`

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
		fmt.Print(usageText, serveUsageText, migrateUsageText, doctorUsageText)
		return 0, nil
	case "serve":
		return runServe(args[1:], log)
	case "migrate":
		return runMigrate(args[1:], log)
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

	srv := bootstrap.NewServer(cfg, api.New(build, time.Now(), log, api.WithReadiness(storeReadiness(st, log))).Handler(), log)
	if err := srv.Run(ctx); err != nil {
		return 1, err
	}
	return 0, nil
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
