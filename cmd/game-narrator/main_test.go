package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/dbirks/game-narrator/internal/bootstrap"
	"github.com/dbirks/game-narrator/internal/store"
)

// The exit code contract lives here: usage errors are 2, runtime failures are
// 1, success is 0 (AGENTS.md CLI targets).

func TestRunUnknownCommandExitsTwo(t *testing.T) {
	code, err := run([]string{"bogus-command"}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 (err %v)", code, err)
	}
	if err == nil {
		t.Fatal("unknown command returned nil error")
	}
}

func TestRunNoArgsExitsTwo(t *testing.T) {
	code, _ := run(nil, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

func TestRunServeRejectsBadListenAddress(t *testing.T) {
	code, err := run([]string{"serve", "--listen", "not-an-address"},
		slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 (err %v)", code, err)
	}
	if err == nil {
		t.Fatal("invalid listen address returned nil error")
	}
}

func TestRunServeRejectsUnknownFlag(t *testing.T) {
	code, _ := run([]string{"serve", "--nope"}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

// TestRunMigrateEndToEnd exercises the real `game-narrator migrate` path
// against a temp data dir: it must create the database, print one line per
// applied migration plus the schema version, and then be a no-op (docs/spec/02
// "Migration and backup acceptance", docs/spec/16 Milestone 0).
func TestRunMigrateEndToEnd(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	dataDir := filepath.Join(t.TempDir(), "narrator-data")
	dbPath := store.DBPath(dataDir)

	first := captureStdout(t, func() int {
		code, err := run([]string{"migrate", "--data-dir", dataDir}, log)
		if err != nil {
			t.Fatalf("migrate: %v", err)
		}
		return code
	})
	if first.code != 0 {
		t.Fatalf("exit code = %d, want 0", first.code)
	}
	if want := "applied 0001_init.sql\nschema version 1\n"; first.output != want {
		t.Fatalf("first run output =\n%q\nwant\n%q", first.output, want)
	}

	// The database really exists at the conventional path and really is at
	// version 1: verify through the store, not by parsing our own stdout.
	st, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("Open after migrate: %v", err)
	}
	if v, err := st.SchemaVersion(context.Background()); err != nil || v != 1 {
		t.Fatalf("SchemaVersion after migrate = %d, %v; want 1", v, err)
	}
	if err := st.IntegrityCheck(context.Background()); err != nil {
		t.Fatalf("IntegrityCheck after migrate: %v", err)
	}
	st.Close()

	second := captureStdout(t, func() int {
		code, err := run([]string{"migrate", "--data-dir", dataDir}, log)
		if err != nil {
			t.Fatalf("second migrate: %v", err)
		}
		return code
	})
	if second.code != 0 {
		t.Fatalf("second exit code = %d, want 0", second.code)
	}
	if want := "schema version 1\n"; second.output != want {
		t.Fatalf("second run output =\n%q\nwant\n%q", second.output, want)
	}
}

// TestRunMigrateHonoursDataDirPrecedence proves the env var is used when no
// flag is given (and the flag still wins over it).
func TestRunMigrateHonoursDataDirPrecedence(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	envDir := filepath.Join(t.TempDir(), "from-env")
	t.Setenv(bootstrap.EnvDataDir, envDir)

	captureStdout(t, func() int {
		code, err := run([]string{"migrate"}, log)
		if err != nil {
			t.Fatalf("migrate from env: %v", err)
		}
		if code != 0 {
			t.Fatalf("exit code = %d, want 0", code)
		}
		return code
	})
	if _, err := os.Stat(store.DBPath(envDir)); err != nil {
		t.Fatalf("GAME_NARRATOR_DATA_DIR was not used: %v", err)
	}

	flagDir := filepath.Join(t.TempDir(), "from-flag")
	captureStdout(t, func() int {
		code, _ := run([]string{"migrate", "--data-dir", flagDir}, log)
		return code
	})
	if _, err := os.Stat(store.DBPath(flagDir)); err != nil {
		t.Fatalf("--data-dir was not used: %v", err)
	}
}

func TestRunMigrateUsageAndValidation(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	cases := []struct {
		name string
		args []string
	}{
		{"unknown flag", []string{"migrate", "--nope"}},
		{"unexpected argument", []string{"migrate", "extra"}},
		{"relative data dir", []string{"migrate", "--data-dir", "relative/dir"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if code, _ := run(tc.args, log); code != 2 {
				t.Fatalf("exit code = %d, want 2", code)
			}
		})
	}
}

// capturedStdout holds what a command wrote to stdout plus its exit code.
type capturedStdout struct {
	output string
	code   int
}

// captureStdout redirects the package-level stdout sink so the CLI output can
// be asserted exactly. It restores the original writer when the test ends.
func captureStdout(t *testing.T, fn func() int) capturedStdout {
	t.Helper()
	prev := stdout
	buf := &bytes.Buffer{}
	stdout = buf
	defer func() { stdout = prev }()

	code := fn()
	return capturedStdout{output: buf.String(), code: code}
}
