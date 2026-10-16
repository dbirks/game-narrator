package bootstrap

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

// Defaults for local development. The service binds to loopback unless
// explicitly reconfigured (docs/spec/08: "Default bind to localhost").
const (
	DefaultListen = "127.0.0.1:8080"
	EnvListen     = "GAME_NARRATOR_LISTEN"
)

// Config is the validated process configuration. More settings (data dir,
// gateway roles, etc.) are added by later milestones through ADRs.
type Config struct {
	// Listen is the host:port the HTTP API binds to.
	Listen string
}

// DefaultConfig returns configuration with no overrides applied.
func DefaultConfig() Config {
	return Config{Listen: DefaultListen}
}

// LoadConfig resolves configuration from CLI overrides and the environment.
// Precedence: explicit flag value > environment variable > default.
func LoadConfig(listenFlag string, lookupEnv func(string) (string, bool)) (Config, error) {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	cfg := Config{Listen: ResolveListen(listenFlag, lookupEnv)}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
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
