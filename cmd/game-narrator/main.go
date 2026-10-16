// Command game-narrator runs the Game Narrator service.
//
// Usage:
//
//	game-narrator serve [--listen host:port]
//	game-narrator version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dbirks/game-narrator/internal/api"
	"github.com/dbirks/game-narrator/internal/bootstrap"
)

const usageText = `usage: game-narrator <command> [flags]

commands:
  serve    start the HTTP API server (default listen 127.0.0.1:8080)
  version  print build information and exit
  help     show this message

serve flags:
  --listen addr   TCP listen address; overrides GAME_NARRATOR_LISTEN
                  (default 127.0.0.1:8080)
`

const serveUsageText = `usage: game-narrator serve [--listen host:port]

Start the Game Narrator HTTP API. Endpoints (JSON):
  GET /api/v1/health/live
  GET /api/v1/health/ready
  GET /api/v1/system/info

flags:
  -listen string
    	TCP listen address; overrides GAME_NARRATOR_LISTEN (default "127.0.0.1:8080")
`

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
		fmt.Print(usageText, serveUsageText)
		return 0, nil
	case "serve":
		return runServe(args[1:], log)
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
	if err := fs.Parse(args); err != nil {
		return 2, fmt.Errorf("%w: %v", errUsage, err)
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "game-narrator serve: unexpected argument %q\n", fs.Arg(0))
		return 2, fmt.Errorf("%w: unexpected argument", errUsage)
	}

	cfg, err := bootstrap.LoadConfig(*listen, os.LookupEnv)
	if err != nil {
		return 2, err
	}

	build := bootstrap.Version()
	log.Info("game-narrator starting", build.Values()...)

	// SIGINT/SIGTERM trigger graceful drain (http.Server.Shutdown, 10s budget).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := bootstrap.NewServer(cfg, api.New(build, time.Now(), log).Handler(), log)
	if err := srv.Run(ctx); err != nil {
		return 1, err
	}
	return 0, nil
}
