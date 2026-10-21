package main

// support-bundle — docs/spec/13 "Logging/privacy": "Provide export support
// bundle with automated scrubber and a preview before export".
//
// The implementation lives in internal/support; this file is the command
// boundary (flag parsing, config resolution, output location) that the
// other commands in this file follow. The bundle is the artifact that
// leaves the machine, so the privacy rules are enforced there (redaction,
// counts-only store data, metadata-only dev-trace) and asserted by its
// tests, not here.

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/dbirks/game-narrator/internal/bootstrap"
	"github.com/dbirks/game-narrator/internal/support"
)

const supportBundleUsageText = `usage: game-narrator support-bundle --data-dir DIR [--out FILE.tar.gz] [--since 24h] [--include-dev-trace] [--probe]

Build a redacted diagnostics archive for a support report (docs/spec/13
"Logging/privacy"). The archive is a tar.gz with JSON entries only:
  manifest.json         build identity, created_at, the data dir's basename,
                        a sha256 list of every other entry and the note that
                        the service logs to stderr only (no log file exists
                        under the data dir to ship)
  doctor.json           the doctor report; network provider probes are
                        skipped unless --probe, so the default build is
                        offline-safe
  store.json            schema version, applied migrations and per-table row
                        counts — counts only, never row content; the database
                        file itself never enters the archive
  config.redacted.json  the effective model-role configuration: every URL
                        reduced to scheme + redacted host, every token/secret
                        value replaced by a sentinel, environment variable
                        names (and set/unset) kept
  dev-trace.json        only with --include-dev-trace: session ids, blob
                        hashes, sizes and mtimes — never frames, crops, audio
                        or dialogue text

Nothing is copied out of the data dir: the store is opened strictly
read-only, and the app keeps no copies of the bundle — it lives exactly
where --out puts it (default: the current directory).

Verify before sharing (docs/manuals/11):
  tar -tzf bundle.tar.gz                          # list the entries
  tar -xOf bundle.tar.gz manifest.json | jq .     # identity + entry hashes
  tar -xOf bundle.tar.gz store.json | jq .table_counts

exit status: 0 on success, 1 on failure (no database, unreadable data dir),
2 on bad usage.

flags:
  -data-dir string
    	durable data directory; overrides GAME_NARRATOR_DATA_DIR
  -out string
    	archive path (default: ./game-narrator-support-<UTC stamp>.tar.gz in
    	the current directory)
  -since duration
    	include only dev-trace metadata from the last duration, e.g. 24h
    	(default 0 = no limit; there is no log file to window over, so this
    	affects the dev-trace section only)
  -include-dev-trace
    	include the dev-trace session metadata (ids, blob hashes, sizes and
    	mtimes — never the frames, crops, audio or dialogue they index)
  -probe
    	exercise the configured provider endpoints for the doctor section;
    	off by default, so the bundle is built without any network access
`

func runSupportBundle(args []string, log *slog.Logger) (int, error) {
	fs := flag.NewFlagSet("support-bundle", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, supportBundleUsageText) }
	dataDir := fs.String("data-dir", "",
		"durable data directory; overrides "+bootstrap.EnvDataDir)
	out := fs.String("out", "",
		"archive path (default: ./game-narrator-support-<UTC stamp>.tar.gz in the current directory)")
	since := fs.Duration("since", 0,
		"include only dev-trace metadata from the last duration (e.g. 24h); 0 = no limit")
	includeDevTrace := fs.Bool("include-dev-trace", false,
		"include dev-trace session metadata (ids, blob hashes, sizes and mtimes — never frames, crops, audio or dialogue)")
	probe := fs.Bool("probe", false,
		"exercise the configured provider endpoints for the doctor section; off by default (offline-safe)")
	if err := fs.Parse(args); err != nil {
		return 2, fmt.Errorf("%w: %v", errUsage, err)
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "game-narrator support-bundle: unexpected argument %q\n", fs.Arg(0))
		return 2, fmt.Errorf("%w: unexpected argument", errUsage)
	}

	cfg, err := bootstrap.LoadConfig("", *dataDir, os.LookupEnv)
	if err != nil {
		return 2, err
	}

	outPath := *out
	if outPath == "" {
		// The default lands in the current directory: the bundle is a
		// transient artifact the operator moves on to a support channel, not
		// durable state, so the data dir is deliberately not the default.
		outPath = filepath.Join(".", "game-narrator-support-"+time.Now().UTC().Format("20060102T150405Z")+".tar.gz")
	}

	// The bundle build is short and bounded by the doctor check deadlines;
	// Ctrl-C must not abandon a mid-flight probe when --probe is on.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	man, err := support.Build(ctx, support.Options{
		DataDir: cfg.DataDir,
		// The bundle's doctor section probes the same listen address the
		// service will bind (ADR-0150).
		Listen:          cfg.Listen,
		Since:           *since,
		IncludeDevTrace: *includeDevTrace,
		Probe:           *probe,
	}, outPath)
	if err != nil {
		return 1, fmt.Errorf("support-bundle: %w", err)
	}

	// Preview before export (docs/spec/13): the operator sees exactly what
	// the archive contains and where it lives, and verifies the entry hashes
	// before sending it anywhere.
	fmt.Printf("support bundle written: %s\n", outPath)
	fmt.Printf("  version %s (%s, %s %s/%s), data dir %s, doctor mode %s\n",
		man.Version, man.Commit, man.GoVersion, man.OS, man.Arch, man.DataDir, man.DoctorMode)
	for _, f := range man.Files {
		fmt.Printf("  %s  %s  %d bytes\n", f.SHA256, f.Path, f.Bytes)
	}
	log.Info("support bundle built", "out", outPath, "entries", len(man.Files), "doctor_mode", man.DoctorMode)
	return 0, nil
}
