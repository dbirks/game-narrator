package main

import (
	"io"
	"log/slog"
	"testing"
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
