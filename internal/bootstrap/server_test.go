package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"
)

// freePort reserves an ephemeral loopback port, releases it and returns the
// address so the server can bind it. There is a small race window, which is
// acceptable for a loopback test on a dev host.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("release port: %v", err)
	}
	return addr
}

// TestServerRunServesAndShutsDownCleanly drives a real listener: an in-flight
// request must be answered, and Run must return nil after cancellation, which
// is what makes SIGTERM handling verifiable from a unit test.
func TestServerRunServesAndShutsDownCleanly(t *testing.T) {
	addr := freePort(t)
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	srv := NewServer(Config{Listen: addr}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"ok"}`)
	}), log)

	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { done <- srv.Run(ctx) }()

	if !dialWithin(t, addr, 5*time.Second) {
		t.Fatalf("server did not start listening on %s", addr)
	}

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + addr + "/probe")
	if err != nil {
		t.Fatalf("GET /probe: %v", err)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if body["status"] != "ok" || resp.StatusCode != http.StatusOK {
		t.Fatalf("probe response = %d %v", resp.StatusCode, body)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error after cancel: %v", err)
		}
	case <-time.After(ShutdownTimeout + 2*time.Second):
		t.Fatal("Run did not return after context cancellation")
	}

	// The port must be released: a second bind proves Shutdown closed it.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("port %s still held after shutdown: %v", addr, err)
	}
	ln.Close()
}

// TestServerRunReportsBindFailure guards against a silently dead server: an
// occupied port must surface as an error, not a clean nil return.
func TestServerRunReportsBindFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy port: %v", err)
	}
	defer ln.Close()

	srv := NewServer(Config{Listen: ln.Addr().String()}, http.NotFoundHandler(),
		slog.New(slog.NewJSONHandler(io.Discard, nil)))
	err = srv.Run(context.Background())
	if err == nil {
		t.Fatal("Run on an occupied port returned nil")
	}
	if errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("bind failure was swallowed as a clean shutdown: %v", err)
	}
}

// TestServerRejectsInvalidConfig ensures startup fails loudly on a bad bind
// address rather than listening somewhere unintended.
func TestServerRejectsInvalidConfig(t *testing.T) {
	if err := (Config{Listen: "8080"}).Validate(); err == nil {
		t.Fatal("Config{Listen: \"8080\"}.Validate() = nil, want error")
	}
}

func dialWithin(t *testing.T, addr string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
