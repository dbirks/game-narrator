package support

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/store"
)

// TestBuildBundleRedactsConfig proves the config section reduces URLs to
// scheme + redacted host and keeps token values out while keeping the env
// var *names*.
func TestBuildBundleRedactsConfig(t *testing.T) {
	t.Setenv("GAME_NARRATOR_GATEWAY_TOKEN_ENV", "")
	t.Setenv("GAME_NARRATOR_GATEWAY_URL", "https://gw.secret.lan:8443/v1")
	t.Setenv("GAME_NARRATOR_GATEWAY_TOKEN", "gn_0123456789abcdef")
	t.Setenv("GAME_NARRATOR_ROLE_FAST_REASONING_MODEL", "qwen3-8b-instruct")
	t.Setenv("GAME_NARRATOR_ROLE_FAST_REASONING_TIMEOUT", "7s")

	dataDir := t.TempDir()
	ctx := context.Background()
	st, err := store.Open(ctx, store.DBPath(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	st.Close()

	out := filepath.Join(t.TempDir(), "bundle.tar.gz")
	if _, err := Build(ctx, Options{DataDir: dataDir}, out); err != nil {
		t.Fatalf("Build: %v", err)
	}
	entries := extractArchive(t, out)

	var cfg configDoc
	mustJSON(t, entries[ConfigPath], &cfg)

	joined := string(entries[ConfigPath])
	if strings.Contains(joined, "gw.secret.lan") {
		t.Error("config section leaks the gateway hostname")
	}
	if strings.Contains(joined, "gn_0123456789abcdef") {
		t.Error("config section leaks the gateway token")
	}
	if cfg.Gateway.URL != "https://<redacted-host>" {
		t.Errorf("gateway url = %q, want %q", cfg.Gateway.URL, "https://<redacted-host>")
	}
	if !cfg.Gateway.TokenSet {
		t.Error("gateway token_set = false, want true (the env var is set)")
	}
	if cfg.Gateway.TokenEnv != "GAME_NARRATOR_GATEWAY_TOKEN" {
		t.Errorf("gateway token_env = %q, want the variable name", cfg.Gateway.TokenEnv)
	}
	var fast *roleDoc
	for i := range cfg.Roles {
		if cfg.Roles[i].Role == "fast_reasoning" {
			fast = &cfg.Roles[i]
		}
	}
	if fast == nil {
		t.Fatal("fast_reasoning role missing from config section")
	}
	if fast.Model != "qwen3-8b-instruct" {
		t.Errorf("fast_reasoning model = %q, want the (safe) model id", fast.Model)
	}
	if fast.BaseURL != "https://<redacted-host>" {
		t.Errorf("fast_reasoning base_url = %q, want redacted", fast.BaseURL)
	}
	if fast.Timeout != "7s" {
		t.Errorf("fast_reasoning timeout = %q, want 7s", fast.Timeout)
	}
	if !fast.TokenSet {
		t.Error("fast_reasoning token_set = false, want true")
	}
	// Environment variable names survive redaction; values do not.
	var names []string
	for _, e := range cfg.EnvVars {
		names = append(names, e.Name)
	}
	joinedNames := strings.Join(names, " ")
	for _, want := range []string{"GAME_NARRATOR_GATEWAY_TOKEN", "GAME_NARRATOR_ROLE_FAST_REASONING_MODEL", "GAME_NARRATOR_DATA_DIR"} {
		if !strings.Contains(joinedNames, want) {
			t.Errorf("env var names %q missing from %v", want, names)
		}
	}
}

// TestBuildStoreSectionReadsOnly pins the read-only contract: the bundle
// reports counts and the applied migration list, and refuses a data dir with
// no database rather than inventing one.
func TestBuildStoreSectionReadsOnly(t *testing.T) {
	dataDir, _ := seedGuardDataDir(t)
	before, err := os.ReadFile(store.DBPath(dataDir))
	if err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "bundle.tar.gz")
	if _, err := Build(context.Background(), Options{DataDir: dataDir}, out); err != nil {
		t.Fatalf("Build: %v", err)
	}
	after, err := os.ReadFile(store.DBPath(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("the bundle build modified the database file: the store must be read query-only")
	}

	entries := extractArchive(t, out)
	var storeDoc storeDoc
	mustJSON(t, entries[StorePath], &storeDoc)
	if len(storeDoc.Migrations) != len(store.MigrationNames()) {
		t.Errorf("migrations = %d, want %d (every applied migration listed)", len(storeDoc.Migrations), len(store.MigrationNames()))
	}
	for i, m := range storeDoc.Migrations {
		if m.Name != store.MigrationNames()[i] {
			t.Errorf("migration %d = %q, want %q", i, m.Name, store.MigrationNames()[i])
		}
	}

	// A data dir without a database is an actionable error, not an empty
	// bundle.
	empty := t.TempDir()
	if _, err := Build(context.Background(), Options{DataDir: empty}, filepath.Join(t.TempDir(), "x.tar.gz")); err == nil {
		t.Error("Build on a data dir without a database: want an error")
	}
}

// TestBuildDevTraceRespectsSince pins the --since window: a session whose
// blobs are older than the window is absent, a recent one is listed.
func TestBuildDevTraceRespectsSince(t *testing.T) {
	dataDir := t.TempDir()
	ctx := context.Background()
	st, err := store.Open(ctx, store.DBPath(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	st.Close()

	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 1, 7, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 1, 7, 12, 0, 0, 0, time.UTC)

	oldHash := writeHashedBlob(t, filepath.Join(dataDir, "dev-trace", "sess-old", "blobs"), []byte("old frame"))
	if err := os.Chtimes(filepath.Join(dataDir, "dev-trace", "sess-old", "blobs", oldHash[:2], oldHash[2:4], oldHash), old, old); err != nil {
		t.Fatal(err)
	}
	recentHash := writeHashedBlob(t, filepath.Join(dataDir, "dev-trace", "sess-recent", "blobs"), []byte("recent frame"))
	if err := os.Chtimes(filepath.Join(dataDir, "dev-trace", "sess-recent", "blobs", recentHash[:2], recentHash[2:4], recentHash), recent, recent); err != nil {
		t.Fatal(err)
	}

	build := func(since time.Duration) []string {
		out := filepath.Join(t.TempDir(), "b.tar.gz")
		if _, err := Build(ctx, Options{DataDir: dataDir, IncludeDevTrace: true, Since: since, Now: func() time.Time { return now }}, out); err != nil {
			t.Fatalf("Build: %v", err)
		}
		entries := extractArchive(t, out)
		var dt devTraceDoc
		mustJSON(t, entries[DevTracePath], &dt)
		var ids []string
		for _, s := range dt.Sessions {
			ids = append(ids, s.ID)
		}
		return ids
	}

	if ids := build(0); len(ids) != 2 {
		t.Errorf("no window: sessions = %v, want both", ids)
	}
	if ids := build(24 * time.Hour); len(ids) != 1 || ids[0] != "sess-recent" {
		t.Errorf("24h window: sessions = %v, want only sess-recent", ids)
	}
}
