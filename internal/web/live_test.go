package web

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// Live tests run only with GAME_NARRATOR_LIVE_WEB=1 (docs/spec/16 test
// discipline: live tests are marked NOT RUN otherwise and must not be
// presented as passing). They verify the fetcher against the public
// internet with the DEFAULT policy (no test seams).
func liveWebEnabled() bool { return os.Getenv("GAME_NARRATOR_LIVE_WEB") == "1" }

func skipLive(t *testing.T) {
	t.Helper()
	if !liveWebEnabled() {
		t.Skip("NOT RUN: set GAME_NARRATOR_LIVE_WEB=1 to run live web tests")
	}
}

func TestLiveFetchExampleDomain(t *testing.T) {
	skipLive(t)
	f := NewFetcher(Policy{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	doc, err := f.Fetch(ctx, "https://example.com/")
	latency := time.Since(start)
	if err != nil {
		t.Fatalf("live fetch failed: %v", err)
	}
	t.Logf("live fetch: provider=default-fetcher latency=%s status=%d media=%s", latency, doc.Status, doc.MediaType)
	if doc.Title != "Example Domain" {
		t.Fatalf("title = %q, want %q", doc.Title, "Example Domain")
	}
	// The live page body varies; it has always been about the example
	// domain, so assert on that word in the extracted text.
	if !strings.Contains(strings.ToLower(doc.Text), "domain") || len(doc.Text) == 0 {
		t.Fatalf("extracted text looks wrong:\n%s", doc.Text)
	}
	if !doc.Untrusted || doc.SHA256 == "" {
		t.Error("document must be untrusted with a body hash")
	}
	if q := QuoteUntrusted(doc); !strings.Contains(q, "UNTRUSTED WEB CONTENT") || !strings.Contains(q, "domain") {
		t.Error("quote wrapper missing header or content")
	}
}
