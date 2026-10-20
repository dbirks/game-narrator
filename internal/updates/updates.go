// Package updates implements the appliance's update notification (docs/spec/09
// /settings "software updates", gap G11): a bounded check against the GitHub
// releases of this repository, a private on-disk cache that keeps the answer
// honest when the network is gone, and a version comparison that tells the
// operator "new version available" without ever installing anything.
//
// The posture is fixed (ADR-0047's "no auto-update" line, docs/release.md):
// the check informs; it never downloads, never replaces the binary, never
// restarts the process. Everything this package reports is renderable in the
// settings page and the doctor table — no hostnames beyond the configured
// endpoint, no credentials (AGENTS.md rule 5).
package updates

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Environment variables (docs/spec/09: the check is configurable and
// disable-able). Names live here so the settings page's variable list, the
// doctor and the serve wiring all read the same names.
const (
	// EnvCheck is "1" (default) or "0"/"false"/"off" to disable the check.
	EnvCheck = "GAME_NARRATOR_UPDATE_CHECK"
	// EnvRepo names the GitHub repository whose releases are checked,
	// "owner/name". The default is this build's own repository.
	EnvRepo = "GAME_NARRATOR_UPDATE_REPO"
	// EnvBaseURL overrides the GitHub API base (self-hosted GitHub Enterprise,
	// a mirror, or the test's fake release server).
	EnvBaseURL = "GAME_NARRATOR_UPDATE_BASE_URL"
)

// DefaultRepo is this product's own release repository.
const DefaultRepo = "dbirks/game-narrator"

// DefaultBaseURL is the public GitHub REST API.
const DefaultBaseURL = "https://api.github.com"

// DefaultTimeout bounds one check. An offline appliance must not make the
// settings page wait: the check runs in the background and this budget is the
// whole of its network exposure.
const DefaultTimeout = 5 * time.Second

// DefaultInterval is how often the background checker refreshes after the
// first check. Updates are a human-timescale event; hourly polling of a public
// endpoint from a LAN appliance is still more than a release cadence needs.
const DefaultInterval = 6 * time.Hour

// cacheFileName inside the data directory's private updates subdirectory.
const cacheFileName = "latest.json"

// userAgent names the client in the endpoint's logs; it carries no version
// secrets (docs/spec/14 redaction posture).
const userAgent = "game-narrator-update-check"

// Config is the check's frozen configuration. Zero fields fall back to the
// documented defaults in ConfigFromEnv; tests set every field explicitly.
type Config struct {
	// Enabled is false when the operator disabled the check (EnvCheck).
	Enabled bool
	// Repo is "owner/name".
	Repo string
	// BaseURL is the API root, without trailing slash.
	BaseURL string
	// Timeout bounds one HTTP check (DefaultTimeout when zero).
	Timeout time.Duration
	// Interval is the background refresh period (DefaultInterval when zero).
	Interval time.Duration
	// CachePath is the file the latest good answer is cached to; "" disables
	// the cache. The file is 0600 and holds no secret (a tag, a URL, a time).
	CachePath string
	// HTTPClient performs the request; zero uses http.DefaultClient.
	HTTPClient http.Client
}

// withDefaults fills the zero fields with the documented defaults.
func (c Config) withDefaults() Config {
	if c.Repo == "" {
		c.Repo = DefaultRepo
	}
	if c.BaseURL == "" {
		c.BaseURL = DefaultBaseURL
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
	}
	if c.Interval <= 0 {
		c.Interval = DefaultInterval
	}
	return c
}

// ConfigFromEnv resolves the check's configuration from the environment:
// enabled by default, disabled by EnvCheck's documented off-values. A repo
// with owner or name missing is refused rather than guessed: a check against
// the wrong repository would announce updates for software this appliance
// does not run.
func ConfigFromEnv(lookupEnv func(string) (string, bool), cachePath string) Config {
	enabled := true
	if v, ok := lookupEnv(EnvCheck); ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "0", "false", "off", "no":
			enabled = false
		}
	}
	repo := DefaultRepo
	if v, ok := lookupEnv(EnvRepo); ok && strings.TrimSpace(v) != "" {
		repo = strings.TrimSpace(v)
	}
	base := DefaultBaseURL
	if v, ok := lookupEnv(EnvBaseURL); ok && strings.TrimSpace(v) != "" {
		base = strings.TrimRight(strings.TrimSpace(v), "/")
	}
	return Config{
		Enabled:   enabled,
		Repo:      repo,
		BaseURL:   base,
		CachePath: cachePath,
	}
}

// Release is the latest release as the endpoint reports it. The fields are the
// subset of GitHub's release object the product surface needs: the version
// tag, the human name, the release-notes page URL and the publication time.
type Release struct {
	Version     string    `json:"tag_name"`
	Name        string    `json:"name,omitempty"`
	URL         string    `json:"html_url"`
	PublishedAt time.Time `json:"published_at"`
}

// Report is the answer of one check. It is complete enough to render in the
// settings page and the doctor row: a release (fresh or from cache), the time
// the answer was determined, whether it came from the offline cache, and the
// reason a fresh check failed — reported, never swallowed.
type Report struct {
	// Release is nil when no release is known (the endpoint has no releases,
	// the check never ran, or it failed with no cache).
	Release *Release `json:"release"`
	// CheckedAt is when Release was determined (the fresh check's answer time,
	// or the cached entry's time when FromCache).
	CheckedAt time.Time `json:"checked_at,omitempty"`
	// FromCache is true when the fresh check failed and the cached answer is
	// what is being reported.
	FromCache bool `json:"from_cache,omitempty"`
	// NoReleases is true when the endpoint answered and has no releases yet —
	// a fact, not a failure.
	NoReleases bool `json:"no_releases,omitempty"`
	// Error is set when the fresh check failed and explains it (one line, no
	// credentials). Empty on a clean answer.
	Error string `json:"error,omitempty"`
	// Disabled is true when the check is turned off by configuration.
	Disabled bool `json:"disabled,omitempty"`
}

// Checker performs and remembers checks. It is safe for concurrent use; the
// settings endpoint reads Last() while the background loop calls Check.
type Checker struct {
	cfg     Config
	client  *http.Client
	mu      sync.Mutex
	last    Report
	lastErr error
}

// New builds a Checker. A disabled check still answers (Report.Disabled) so
// the UI can render "disabled" instead of "unknown". A zero HTTPClient falls
// back to http.DefaultClient semantics (the zero client uses DefaultTransport),
// which is the documented default.
func New(cfg Config) *Checker {
	cfg = cfg.withDefaults()
	return &Checker{cfg: cfg, client: &cfg.HTTPClient}
}

// Config returns the effective (defaulted) configuration.
func (c *Checker) Config() Config { return c.cfg }

// Last returns the most recent answer, zero-value plus no error when the
// checker never ran.
func (c *Checker) Last() (Report, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last, c.lastErr
}

// Check performs one fresh check against the configured endpoint. On a
// failure it falls back to the cache (FromCache) when one exists; with no
// cache it returns the error in the report rather than pretending. A disabled
// check does no I/O at all.
func (c *Checker) Check(ctx context.Context) Report {
	if !c.cfg.Enabled {
		return Report{Disabled: true}
	}

	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.cfg.BaseURL+"/repos/"+c.cfg.Repo+"/releases/latest", nil)
	if err != nil {
		return c.fail(ctx, fmt.Sprintf("building the release request: %v", err))
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", userAgent)

	client := c.client
	resp, err := client.Do(req)
	if err == nil {
		defer resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusOK:
			body, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			if rerr != nil {
				return c.fail(ctx, fmt.Sprintf("reading the release answer: %v", rerr))
			}
			var wire struct {
				TagName   string    `json:"tag_name"`
				Name      string    `json:"name"`
				HTMLURL   string    `json:"html_url"`
				Published time.Time `json:"published_at"`
			}
			if jerr := json.Unmarshal(body, &wire); jerr != nil || wire.TagName == "" {
				return c.fail(ctx, "the release endpoint answered 200 with an unrecognised body; "+
					"the endpoint is not a GitHub-compatible releases API")
			}
			rel := &Release{
				Version:     wire.TagName,
				Name:        wire.Name,
				URL:         wire.HTMLURL,
				PublishedAt: wire.Published,
			}
			now := time.Now().UTC()
			rep := Report{Release: rel, CheckedAt: now}
			c.record(rep)
			if c.cfg.CachePath != "" {
				_ = writeCache(c.cfg.CachePath, cacheEntry{Release: *rel, CheckedAt: now})
			}
			return rep
		case resp.StatusCode == http.StatusNotFound:
			rep := Report{NoReleases: true, CheckedAt: time.Now().UTC()}
			c.record(rep)
			// A 404 means "no releases yet"; remember it briefly via the cache
			// only for the fresh flag — writing nothing keeps the cache a store
			// of release answers, and the next check re-asks cheaply.
			return rep
		default:
			// Drain small bodies so the connection is reusable; then fail.
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			return c.fail(ctx, fmt.Sprintf("the release endpoint answered HTTP %d", resp.StatusCode))
		}
	}
	return c.fail(ctx, fmt.Sprintf("reaching the release endpoint: %v", err))
}

// fail records and returns the error report, with the cached answer attached
// when one exists (FromCache). The cache read is local and cheap; it never
// extends the network budget.
func (c *Checker) fail(ctx context.Context, msg string) Report {
	rep := Report{Error: oneLine(msg, 300)}
	if c.cfg.CachePath != "" {
		if entry, ok := readCache(c.cfg.CachePath); ok {
			rel := entry.Release
			rep.Release = &rel
			rep.CheckedAt = entry.CheckedAt
			rep.FromCache = true
		}
	}
	c.record(rep)
	return rep
}

func (c *Checker) record(rep Report) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last = rep
	if rep.Error != "" {
		c.lastErr = fmt.Errorf("updates: %s", oneLine(rep.Error, 200))
	} else {
		c.lastErr = nil
	}
}

// Start runs the check loop: one check now, then one every Interval, until
// the context is done. It never returns an error (a bad check is a Report);
// the caller runs it in a goroutine. The first check is the only one that
// matters at boot — the settings page renders whatever Last() holds.
func (c *Checker) Start(ctx context.Context) {
	if !c.cfg.Enabled {
		return
	}
	_ = c.Check(ctx)
	ticker := time.NewTicker(c.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = c.Check(context.WithoutCancel(ctx))
		}
	}
}

// ── version comparison ────────────────────────────────────────────────────────

// CompareVersions compares two version strings in argument order (a vs b):
// -1, 0 or 1, each with an optional "v" prefix and trailing pre-release/build
// metadata (v0.2.0-rc1 compares equal to v0.2.0: a pre-release of the same
// number is not a different version). It reports whether both parsed; current
// "dev" or a git-describe fallback ("abc1234") does not parse, and the caller
// renders "dev build" instead of a comparison.
func CompareVersions(current, latest string) (int, bool) {
	c, okC := parseVersion(current)
	l, okL := parseVersion(latest)
	if !okC || !okL {
		return 0, false
	}
	if c.major != l.major {
		return compareInt(c.major, l.major), true
	}
	if c.minor != l.minor {
		return compareInt(c.minor, l.minor), true
	}
	return compareInt(c.patch, l.patch), true
}

type versionParts struct{ major, minor, patch int }

// parseVersion reads "<v?>MAJOR.MINOR.PATCH" with optional trailing metadata.
func parseVersion(s string) (versionParts, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) < 1 || len(parts) > 3 {
		return versionParts{}, false
	}
	for len(parts) < 3 {
		parts = append(parts, "0")
	}
	nums := make([]int, 3)
	for i, p := range parts[:3] {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return versionParts{}, false
		}
		nums[i] = n
	}
	return versionParts{major: nums[0], minor: nums[1], patch: nums[2]}, true
}

func compareInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// Newer reports whether latest is strictly newer than current (i.e. current
// < latest); the second result is false when either side does not parse (a dev
// build), in which case the answer is "unknown", not "no".
func Newer(current, latest string) (bool, bool) {
	n, ok := CompareVersions(current, latest)
	return n == -1, ok
}

// ── cache ─────────────────────────────────────────────────────────────────────

// cacheEntry is the on-disk form of a good answer: the release plus when it
// was fetched. It holds no secret (a tag, a name, a public URL, a time).
type cacheEntry struct {
	Release   Release   `json:"release"`
	CheckedAt time.Time `json:"checked_at"`
}

func writeCache(path string, entry cacheEntry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".updates-cache-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err // the answer is private state; 0600 before it is renamed into place
	}
	return os.Rename(tmpName, path)
}

func readCache(path string) (cacheEntry, bool) {
	var entry cacheEntry
	raw, err := os.ReadFile(path)
	if err != nil {
		return entry, false
	}
	if err := json.Unmarshal(raw, &entry); err != nil || entry.Release.Version == "" {
		return entry, false
	}
	return entry, true
}

// oneLine collapses whitespace and caps length (a URL or error must not
// dominate a table row or a JSON field).
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if max > 0 && len(s) > max {
		return s[:max] + "…"
	}
	return s
}
