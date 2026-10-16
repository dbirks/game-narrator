package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// ShutdownTimeout bounds graceful drain of in-flight requests.
const ShutdownTimeout = 10 * time.Second

// Server wraps an http.Server with lifecycle logging and graceful shutdown.
type Server struct {
	http *http.Server
	log  *slog.Logger
}

// NewServer builds a Server for cfg serving handler. Timeouts keep a stuck
// client from holding a connection open indefinitely (AGENTS.md: predictable
// runtime); read/write timeouts are intentionally absent until request bodies
// and streaming responses (SSE) need them.
func NewServer(cfg Config, handler http.Handler, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		http: &http.Server{
			Addr:              cfg.Listen,
			Handler:           handler,
			ReadHeaderTimeout: 10 * time.Second,
			// net/http's stdlib logger goes through slog so bind failures and
			// TLS/accept errors land in the same JSON stream.
			ErrorLog: slog.NewLogLogger(log.Handler().WithAttrs(
				[]slog.Attr{slog.String("logger", "net/http")},
			), slog.LevelInfo),
		},
		log: log,
	}
}

// Listen returns the configured bind address.
func (s *Server) Listen() string { return s.http.Addr }

// Run serves until ctx is cancelled, then drains within ShutdownTimeout.
// It returns nil on a clean shutdown.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		s.log.Info("http server starting", "listen", s.http.Addr)
		err := s.http.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("http server on %s failed: %w", s.http.Addr, err)
		}
		return nil
	case <-ctx.Done():
	}

	s.log.Info("shutdown signal received, draining", "timeout", ShutdownTimeout.String())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
	defer cancel()
	if err := s.http.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown failed: %w", err)
	}
	if err := <-errCh; err != nil {
		return err
	}
	s.log.Info("http server stopped cleanly")
	return nil
}
