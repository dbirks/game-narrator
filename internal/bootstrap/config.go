package bootstrap

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Defaults for local development. The service binds to loopback unless
// explicitly reconfigured (docs/spec/08: "Default bind to localhost").
const (
	DefaultListen = "127.0.0.1:8080"
	EnvListen     = "GAME_NARRATOR_LISTEN"

	// EnvDataDir overrides the durable data directory that holds the SQLite
	// catalogue and the content-addressed assets (docs/spec/02 "Asset store
	// contract").
	EnvDataDir = "GAME_NARRATOR_DATA_DIR"
	// EnvXDGDataHome and EnvHome are the XDG Base Directory Specification
	// inputs used when neither the flag nor EnvDataDir is set.
	EnvXDGDataHome = "XDG_DATA_HOME"
	EnvHome        = "HOME"
	// AppName is this project's directory segment under the data home.
	AppName = "game-narrator"
)

// Config is the validated process configuration. More settings (gateway
// roles, etc.) are added by later milestones through ADRs.
type Config struct {
	// Listen is the host:port the HTTP API binds to.
	Listen string
	// DataDir is the absolute durable data directory. The database file is
	// <DataDir>/game-narrator.sqlite; store.DBPath builds it.
	DataDir string
}

// DefaultConfig returns configuration with no CLI overrides applied. The data
// directory still comes from the ambient environment, because there is no
// meaningful compile-time default for a durable path.
func DefaultConfig() Config {
	return Config{Listen: DefaultListen, DataDir: ResolveDataDir("", os.LookupEnv)}
}

// LoadConfig resolves configuration from CLI overrides and the environment.
// Precedence: explicit flag value > environment variable > default.
func LoadConfig(listenFlag, dataDirFlag string, lookupEnv func(string) (string, bool)) (Config, error) {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	cfg := Config{
		Listen:  ResolveListen(listenFlag, lookupEnv),
		DataDir: ResolveDataDir(dataDirFlag, lookupEnv),
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// ResolveDataDir applies the flag > GAME_NARRATOR_DATA_DIR >
// $XDG_DATA_HOME/game-narrator > $HOME/.local/share/game-narrator precedence
// (docs/spec/02 "Asset store contract", XDG Base Directory Specification).
// It returns "" when the location cannot be determined at all, which Validate
// turns into an actionable error rather than a surprise relative path.
func ResolveDataDir(dataDirFlag string, lookupEnv func(string) (string, bool)) string {
	if v := strings.TrimSpace(dataDirFlag); v != "" {
		return v
	}
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	if v, ok := lookupEnv(EnvDataDir); ok {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	if v, ok := lookupEnv(EnvXDGDataHome); ok {
		if v = strings.TrimSpace(v); v != "" {
			return filepath.Join(v, AppName)
		}
	}
	if v, ok := lookupEnv(EnvHome); ok {
		if v = strings.TrimSpace(v); v != "" {
			return filepath.Join(v, ".local", "share", AppName)
		}
	}
	return ""
}

// ResolveListen applies the flag > env > default precedence rule.
func ResolveListen(listenFlag string, lookupEnv func(string) (string, bool)) string {
	if v := strings.TrimSpace(listenFlag); v != "" {
		return v
	}
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	if v, ok := lookupEnv(EnvListen); ok {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return DefaultListen
}

// Validate rejects listen addresses that cannot be bound, so startup fails
// loudly instead of silently listening somewhere unexpected.
func (c Config) Validate() error {
	if c.Listen == "" {
		return errors.New("listen address is empty")
	}
	if c.DataDir == "" {
		return errors.New("data directory is unresolved: set --data-dir, GAME_NARRATOR_DATA_DIR, XDG_DATA_HOME or HOME")
	}
	if !filepath.IsAbs(c.DataDir) {
		// A relative data dir would follow the working directory of whoever
		// started the service (systemd, a container, `make dev`), which is how
		// durable user data ends up in the wrong place.
		return fmt.Errorf("invalid data directory %q: must be an absolute path", c.DataDir)
	}
	host, port, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("invalid listen address %q: %w", c.Listen, err)
	}
	if port == "" {
		return fmt.Errorf("invalid listen address %q: missing port", c.Listen)
	}
	// Port 0 asks the kernel for a free port, which is valid but never a
	// deliberate service binding, so the usable range starts at 1.
	if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("invalid listen address %q: port must be 1-65535", c.Listen)
	}
	if host != "" && net.ParseIP(host) == nil {
		// Only literal addresses are accepted at this milestone; the API is
		// loopback/LAN-attached and hostname binding adds no value yet.
		return fmt.Errorf("invalid listen address %q: host must be an IP literal", c.Listen)
	}
	return nil
}
