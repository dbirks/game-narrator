package search

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/web"
)

// Live tests run only with GAME_NARRATOR_LIVE_WEB=1 (docs/spec/16: live
// tests are NOT RUN otherwise and must not be presented as passing).
func liveWebEnabled() bool { return os.Getenv("GAME_NARRATOR_LIVE_WEB") == "1" }

func skipLive(t *testing.T) {
	t.Helper()
	if !liveWebEnabled() {
		t.Skip("NOT RUN: set GAME_NARRATOR_LIVE_WEB=1 to run live web tests")
	}
}

// TestLiveExaMCPGameCube exercises the default provider end to end:
// search "GameCube" via Exa's public MCP endpoint, assert at least one
// https result, then fetch one result URL through the policy-checked
// fetcher. Logs provider, latency and counts (no result text).
func TestLiveExaMCPGameCube(t *testing.T) {
	skipLive(t)
	p := &ExaMCP{} // default endpoint https://mcp.exa.ai/mcp
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	start := time.Now()
	results, err := p.Search(ctx, "GameCube", Options{Max: 5})
	latency := time.Since(start)
	if err != nil {
		t.Fatalf("live exa search failed: %v", err)
	}
	t.Logf("live search: provider=%s query=GameCube latency=%s results=%d", p.Name(), latency, len(results))
	if len(results) < 1 {
		t.Fatalf("live search returned no results")
	}
	var target string
	for _, r := range results {
		if strings.HasPrefix(r.URL, "https://") {
			target = r.URL
			break
		}
	}
	if target == "" {
		t.Fatalf("no https result among %d live results (first: title=%q url=%q snippet=%q)",
			len(results), results[0].Title, results[0].URL, capLog(results[0].Snippet, 300))
	}

	// Every result URL is re-validated by the fetcher policy before fetch.
	f := web.NewFetcher(web.Policy{})
	for _, r := range results {
		if err := f.Policy.ValidURL(r.URL); err != nil {
			t.Errorf("result URL %q failed fetcher policy: %v", r.URL, err)
		}
	}
	fetchStart := time.Now()
	doc, err := f.Fetch(ctx, target)
	fetchLatency := time.Since(fetchStart)
	if err != nil {
		t.Fatalf("live fetch of %s failed: %v", target, err)
	}
	t.Logf("live fetch: url_host=%s latency=%s status=%d text_chars=%d",
		firstHostPart(target), fetchLatency, doc.Status, len(doc.Text))
	if doc.Status != 200 || len(doc.Text) == 0 {
		t.Fatalf("fetched document looks wrong: status=%d text_len=%d", doc.Status, len(doc.Text))
	}
	if !doc.Untrusted {
		t.Error("fetched document must be flagged untrusted")
	}
}

func firstHostPart(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		rest := u[i+3:]
		if j := strings.IndexAny(rest, "/?"); j >= 0 {
			return rest[:j]
		}
		return rest
	}
	return u
}

func capLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
