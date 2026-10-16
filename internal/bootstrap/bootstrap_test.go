package bootstrap

import (
	"regexp"
	"strings"
	"testing"
)

// envMap returns a lookupEnv func over a fixed environment.
func envMap(kv map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := kv[key]
		return v, ok
	}
}

func TestResolveListenPrecedence(t *testing.T) {
	cases := []struct {
		name string
		flag string
		env  map[string]string
		want string
	}{
		{
			name: "flag wins over env",
			flag: "127.0.0.1:9999",
			env:  map[string]string{EnvListen: "127.0.0.1:1234"},
			want: "127.0.0.1:9999",
		},
		{
			name: "env wins over default",
			flag: "",
			env:  map[string]string{EnvListen: "127.0.0.1:18080"},
			want: "127.0.0.1:18080",
		},
		{
			name: "default when neither set",
			flag: "",
			env:  map[string]string{},
			want: DefaultListen,
		},
		{
			name: "blank env value falls back to default",
			flag: "  ",
			env:  map[string]string{EnvListen: "   "},
			want: DefaultListen,
		},
		{
			name: "nil lookup uses default",
			flag: "",
			env:  nil,
			want: DefaultListen,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var lookup func(string) (string, bool)
			if tc.env != nil {
				lookup = envMap(tc.env)
			}
			if got := ResolveListen(tc.flag, lookup); got != tc.want {
				t.Fatalf("ResolveListen(%q) = %q, want %q", tc.flag, got, tc.want)
			}
		})
	}
}

func TestResolveListenTrimsFlag(t *testing.T) {
	got := ResolveListen("  127.0.0.1:18081  ", envMap(nil))
	if got != "127.0.0.1:18081" {
		t.Fatalf("got %q, want trimmed address", got)
	}
}

func TestLoadConfigValidates(t *testing.T) {
	cfg, err := LoadConfig("127.0.0.1:18080", envMap(nil))
	if err != nil {
		t.Fatalf("valid address rejected: %v", err)
	}
	if cfg.Listen != "127.0.0.1:18080" {
		t.Fatalf("Listen = %q", cfg.Listen)
	}

	for _, bad := range []string{"no-port", "127.0.0.1", "not an address", "example.invalid:8080", "127.0.0.1:0", "127.0.0.1:70000"} {
		if _, err := LoadConfig(bad, envMap(nil)); err == nil {
			t.Errorf("LoadConfig(%q) = nil error, want validation failure", bad)
		}
	}

	if err := (Config{Listen: ""}).Validate(); err == nil {
		t.Error("empty listen validated")
	}

	if _, err := LoadConfig("", envMap(map[string]string{EnvListen: ":notaport"})); err == nil {
		t.Error("LoadConfig from invalid env = nil error, want validation failure")
	}
}

func TestDefaultConfig(t *testing.T) {
	if got := DefaultConfig(); got.Listen != "127.0.0.1:8080" {
		t.Fatalf("DefaultConfig().Listen = %q, want 127.0.0.1:8080", got.Listen)
	}
}

func TestBuildInfoStringFormat(t *testing.T) {
	b := BuildInfo{
		Version:   "v0.4.1",
		Commit:    "a1b2c3d",
		GoVersion: "go1.27.2",
		GOOS:      "linux",
		GOARCH:    "arm64",
	}
	want := "game-narrator v0.4.1 (a1b2c3d, go1.27.2, linux/arm64)"
	if got := b.String(); got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
	if got := b.Values(); !strings.Contains(strings.Join(flatten(got), " "), "commit a1b2c3d") {
		t.Fatalf("Values() = %v", got)
	}
}

func flatten(v []any) []string {
	out := make([]string, 0, len(v))
	for _, x := range v {
		out = append(out, toString(x))
	}
	return out
}

func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return "<non-string>"
}

// versionLine matches the documented output shape for `game-narrator version`,
// independent of which ldflags values were injected at build time.
var versionLine = regexp.MustCompile(`^game-narrator [^ ,]+ \([^ ,]+, go[0-9][^ ,]*, [a-z0-9]+/[a-z0-9]+\)$`)

func TestVersionStringOfRunningBinary(t *testing.T) {
	got := Version().String()
	if !versionLine.MatchString(got) {
		t.Fatalf("version string %q does not match %s", got, versionLine)
	}
	if strings.Contains(got, "//") || strings.Contains(got, "  ") {
		t.Fatalf("version string %q has empty fields", got)
	}
}

func TestShutdownTimeoutIsTenSeconds(t *testing.T) {
	if ShutdownTimeout.String() != "10s" {
		t.Fatalf("ShutdownTimeout = %s, want 10s", ShutdownTimeout)
	}
}

func TestVersionDefaultsAreHonest(t *testing.T) {
	// Without ldflags the build must not claim a release identity. Only the
	// defaults are asserted here; a -ldflags build overrides them.
	if version == "" || commit == "" {
		t.Fatalf("version/commit must never be empty: %q %q", version, commit)
	}
	if v := Version(); v.GoVersion == "" || v.GOOS == "" || v.GOARCH == "" {
		t.Fatalf("runtime fields missing: %+v", v)
	}
}
