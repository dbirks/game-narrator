// Package bootstrap owns process-level configuration, build/version info and
// server startup and graceful shutdown.
//
// Router, logging drivers and other cross-cutting dependencies are chosen via
// ADRs later (docs/spec/16 Milestone 0); this package therefore stays on the
// standard library.
package bootstrap

import (
	"fmt"
	"runtime"
)

// Injected at link time, e.g.
//
//	-X github.com/dbirks/game-narrator/internal/bootstrap.version=v0.1.0
//	-X github.com/dbirks/game-narrator/internal/bootstrap.commit=abc1234
var (
	version = "dev"
	commit  = "unknown"
)

// BuildInfo describes the compiled binary. It carries no hostnames, paths or
// environment values and is safe to expose over the API.
type BuildInfo struct {
	Version   string
	Commit    string
	GoVersion string
	GOOS      string
	GOARCH    string
}

// String renders the human-readable one-line form used by
// `game-narrator version`:
//
//	game-narrator <version> (<commit>, <go version>, <GOOS>/<GOARCH>)
func (b BuildInfo) String() string {
	return fmt.Sprintf("game-narrator %s (%s, %s, %s/%s)",
		b.Version, b.Commit, b.GoVersion, b.GOOS, b.GOARCH)
}

// Version returns the build info of the running binary.
func Version() BuildInfo {
	return BuildInfo{
		Version:   version,
		Commit:    commit,
		GoVersion: runtime.Version(),
		GOOS:      runtime.GOOS,
		GOARCH:    runtime.GOARCH,
	}
}

// Values reports the build info as a map for structured logging.
func (b BuildInfo) Values() []any {
	return []any{
		"version", b.Version,
		"commit", b.Commit,
		"go_version", b.GoVersion,
		"os", b.GOOS,
		"arch", b.GOARCH,
	}
}
