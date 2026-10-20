package updates

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests run the check against a fake release server (httptest) and the
// real cache file — no network, no repository assumption. The GitHub API
// shape is asserted by the wire struct in Check (tag_name / html_url /
// published_at), which is what the fake serves.

const testTime = "2026-10-19T12:00:00Z"

func fakeReleaseServer(t *testing.T, status int, body string) (*httptest.Server, *[]byte) {
	t.Helper()
	var hits []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, '\n')
		if got, want := r.URL.Path, "/repos/dbirks/game-narrator/releases/latest"; got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
		if got := r.Header.Get("User-Agent"); !strings.Contains(got, "game-narrator") {
			t.Errorf("User-Agent = %q: the endpoint's logs must say who polls it", got)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

const releaseBody = `{"tag_name":"v0.2.0","name":"Release 0.2","html_url":"https://github.com/dbirks/game-narrator/releases/tag/v0.2.0","published_at":"` + testTime + `"}`

func TestCheckAnswersTheLatestRelease(t *testing.T) {
	srv, hits := fakeReleaseServer(t, http.StatusOK, releaseBody)
	cachePath := filepath.Join(t.TempDir(), "updates", "latest.json")
	c := New(Config{
		Enabled: true, Repo: "dbirks/game-narrator", BaseURL: srv.URL,
		Timeout: 2 * time.Second, CachePath: cachePath,
	})
	rep := c.Check(context.Background())
	if rep.Error != "" {
		t.Fatalf("check error: %s", rep.Error)
	}
	if rep.FromCache {
		t.Error("a fresh answer must not be flagged from-cache")
	}
	if rep.Release == nil || rep.Release.Version != "v0.2.0" || rep.Release.URL == "" {
		t.Fatalf("release = %+v, want v0.2.0 with its notes URL", rep.Release)
	}
	if !rep.CheckedAt.After(time.Time{}) || rep.CheckedAt.IsZero() {
		t.Error("a clean answer stamps its check time")
	}
	// The endpoint was asked exactly once and the answer was cached to disk.
	if n := len(*hits); n != 1 {
		t.Errorf("endpoint hits = %d, want 1", n)
	}
	raw, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("the good answer is not cached (offline safety): %v", err)
	}
	var entry cacheEntry
	if err := json.Unmarshal(raw, &entry); err != nil || entry.Release.Version != "v0.2.0" {
		t.Errorf("cache entry = %+v (err %v): a good answer must survive an offline restart", entry, err)
	}
	// And the last answer is what the settings page reads.
	last, _ := c.Last()
	if last.Release == nil || last.Release.Version != "v0.2.0" {
		t.Errorf("Last() = %+v, want the v0.2.0 answer", last)
	}
}

func TestCheckFallsBackToCacheWhenOffline(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "latest.json")
	// A previous good answer, written the way Check writes it.
	if err := writeCache(cachePath, cacheEntry{
		Release:   Release{Version: "v0.1.0", URL: "https://github.com/dbirks/game-narrator/releases/tag/v0.1.0"},
		CheckedAt: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("seeding the cache: %v", err)
	}

	// A server that answers 500, then the offline case (a dead port).
	bad, _ := fakeReleaseServer(t, http.StatusInternalServerError, `{"message":"boom"}`)
	c := New(Config{
		Enabled: true, Repo: "dbirks/game-narrator", BaseURL: bad.URL,
		Timeout: 2 * time.Second, CachePath: cachePath,
	})
	rep := c.Check(context.Background())
	if rep.Error == "" {
		t.Error("a 500 endpoint must report its failure, not silence")
	}
	if !rep.FromCache || rep.Release == nil || rep.Release.Version != "v0.1.0" {
		t.Fatalf("want the cached v0.1.0 answer flagged from-cache, got %+v", rep)
	}
	// A failure must not have clobbered the cached answer.
	entry, ok := readCache(cachePath)
	if !ok || entry.Release.Version != "v0.1.0" {
		t.Error("a failed check overwrote or lost the cached answer")
	}
}

func TestCheckOfflineWithNoCacheReportsTheHonestError(t *testing.T) {
	// A port nothing listens on: the offline-appliance case. The answer is the
	// error, not an invented version.
	c := New(Config{
		Enabled: true, Repo: "dbirks/game-narrator", BaseURL: "http://127.0.0.1:1",
		Timeout: 500 * time.Millisecond,
	})
	rep := c.Check(context.Background())
	if rep.Error == "" {
		t.Fatal("offline with no cache must report the failure")
	}
	if rep.Release != nil || rep.FromCache {
		t.Errorf("no release may be invented from a failed check: %+v", rep)
	}
}

func TestCheckNoReleasesIsAFactNotAFailure(t *testing.T) {
	srv, _ := fakeReleaseServer(t, http.StatusNotFound, `{"message":"Not Found"}`)
	c := New(Config{Enabled: true, Repo: "dbirks/game-narrator", BaseURL: srv.URL, Timeout: 2 * time.Second})
	rep := c.Check(context.Background())
	if rep.Error != "" || !rep.NoReleases {
		t.Fatalf("a 404 releases endpoint means no releases yet: %+v", rep)
	}
}

func TestCheckDisabledDoesNoIO(t *testing.T) {
	srv, hits := fakeReleaseServer(t, http.StatusOK, releaseBody)
	_ = srv
	c := New(Config{Enabled: false, Repo: "dbirks/game-narrator", BaseURL: srv.URL, Timeout: 2 * time.Second})
	rep := c.Check(context.Background())
	if !rep.Disabled {
		t.Fatalf("a disabled check reports itself disabled: %+v", rep)
	}
	if n := len(*hits); n != 0 {
		t.Errorf("a disabled check hit the endpoint %d times", n)
	}
}

func TestConfigFromEnv(t *testing.T) {
	lookup := func(name string) (string, bool) {
		vals := map[string]string{
			"GAME_NARRATOR_UPDATE_REPO": "mirror/repo",
		}
		v, ok := vals[name]
		return v, ok
	}
	cfg := ConfigFromEnv(lookup, "/tmp/x/updates/latest.json")
	if !cfg.Enabled {
		t.Error("the check is enabled by default")
	}
	if cfg.Repo != "mirror/repo" || cfg.BaseURL != DefaultBaseURL || cfg.CachePath != "/tmp/x/updates/latest.json" {
		t.Errorf("cfg = %+v", cfg)
	}
	for _, off := range []string{"0", "false", "OFF", "no"} {
		lookupOff := func(name string) (string, bool) {
			if name == EnvCheck {
				return off, true
			}
			return "", false
		}
		if cfg := ConfigFromEnv(lookupOff, ""); cfg.Enabled {
			t.Errorf("EnvCheck=%q must disable the check", off)
		}
	}
	// The default repo is this product's own.
	cfg = ConfigFromEnv(func(string) (string, bool) { return "", false }, "")
	if cfg.Repo != DefaultRepo {
		t.Errorf("default repo = %q, want %q", cfg.Repo, DefaultRepo)
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		current, latest string
		want            int
		ok              bool
	}{
		{"v0.1.0", "v0.2.0", -1, true},
		{"v0.2.0", "v0.2.0", 0, true},
		{"v0.2.0", "v0.1.9", 1, true},
		{"0.10.0", "0.9.0", 1, true}, // numeric, not lexicographic
		{"1.2.3", "1.10.0", -1, true},
		{"v1.2.3-rc1", "v1.2.3", 0, true}, // pre-release of the same number: equal
		{"v1.2.3+build", "v1.2.3", 0, true},
		{"v1", "v1.1.0", -1, true},
		{"dev", "v0.2.0", 0, false},     // a dev build cannot be compared
		{"abc1234", "v0.2.0", 0, false}, // a git-describe fallback tag
		{"v0.2.0", "not-a-version", 0, false},
	}
	for _, tc := range cases {
		got, ok := CompareVersions(tc.current, tc.latest)
		if got != tc.want || ok != tc.ok {
			t.Errorf("CompareVersions(%q, %q) = %d, %v; want %d, %v",
				tc.current, tc.latest, got, ok, tc.want, tc.ok)
		}
	}
	// Newer's unknown case: a dev build never claims "no update".
	if _, ok := Newer("dev", "v0.2.0"); ok {
		t.Error("Newer must report unknown for a dev current version")
	}
	if newer, ok := Newer("v0.1.0", "v0.2.0"); !ok || !newer {
		t.Error("Newer(v0.1.0, v0.2.0) must be true")
	}
	if newer, _ := Newer("v0.3.0", "v0.2.0"); newer {
		t.Error("Newer(v0.3.0, v0.2.0) must be false: current is ahead")
	}
}

func TestStartStopsOnContextCancel(t *testing.T) {
	srv, _ := fakeReleaseServer(t, http.StatusOK, releaseBody)
	c := New(Config{
		Enabled: true, Repo: "dbirks/game-narrator", BaseURL: srv.URL,
		Timeout: time.Second, Interval: 10 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.Start(ctx)
		close(done)
	}()
	// The first check must have landed before we cancel.
	deadline := time.Now().Add(2 * time.Second)
	for {
		last, _ := c.Last()
		if last.Release != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the initial check never landed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start ignored the cancelled context")
	}
}

func TestCacheFileIsPrivate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "latest.json")
	if err := writeCache(path, cacheEntry{
		Release:   Release{Version: "v9.9.9"},
		CheckedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("writeCache: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("cache permissions = %v, want 0600 (the answer names the operator's update state)", fi.Mode().Perm())
	}
}
