package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- disabled + budget ------------------------------------------------------

func TestDisabled(t *testing.T) {
	var p Provider = Disabled{}
	if p.Name() != "disabled" {
		t.Fatalf("Name = %q", p.Name())
	}
	_, err := p.Search(context.Background(), "GameCube", Options{})
	if !errors.Is(err, ErrSearchDisabled) {
		t.Fatalf("err = %v, want ErrSearchDisabled", err)
	}
}

type fakeProvider struct {
	mu   sync.Mutex
	hits int
	err  error
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) Search(ctx context.Context, q string, opts Options) ([]Result, error) {
	f.mu.Lock()
	f.hits++
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return []Result{{Title: q, URL: "https://example.com/", Snippet: "s", Source: "fake", RetrievedAt: time.Now().UTC()}}, nil
}

func TestBudgetExhaustion(t *testing.T) {
	fake := &fakeProvider{}
	b := NewBudgeted(fake, 2, 0)
	for i := 1; i <= 2; i++ {
		if _, err := b.Search(context.Background(), "q", Options{}); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	_, err := b.Search(context.Background(), "q", Options{})
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("third call = %v, want budget exhausted", err)
	}
	if b.BudgetUsed() != 2 {
		t.Fatalf("BudgetUsed = %d, want 2", b.BudgetUsed())
	}
}

func TestBudgetConsumedOnFailure(t *testing.T) {
	boomErr := errors.New("boom")
	fake := &fakeProvider{err: boomErr}
	b := NewBudgeted(fake, 1, 0)
	if _, err := b.Search(context.Background(), "q", Options{}); !errors.Is(err, boomErr) {
		t.Fatalf("first call = %v, want provider error", err)
	}
	_, err := b.Search(context.Background(), "q", Options{})
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("failed call must consume the budget: %v", err)
	}
}

func TestBudgetRateLimit(t *testing.T) {
	b := NewBudgeted(&fakeProvider{}, 10, 150*time.Millisecond)
	start := time.Now()
	if _, err := b.Search(context.Background(), "a", Options{}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Search(context.Background(), "b", Options{}); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 140*time.Millisecond {
		t.Fatalf("two back-to-back calls took %s, want >= ~150ms (rate limit)", elapsed)
	}
	// Canceled context while waiting for the rate limit: the reserved slot
	// must still be waited on, and cancellation surfaces as rate_limited.
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := b.Search(context.Background(), "c", Options{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond) // inside the 150ms window of the last call
	cancel()
	_, err := b.Search(ctx, "d", Options{})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("canceled wait = %v, want rate_limited", err)
	}
}

// ---- SearXNG (recorded) ------------------------------------------------------

func TestSearXNGRecorded(t *testing.T) {
	fixture := loadFixture(t, "searxng-gamecube.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" || r.URL.Query().Get("format") != "json" {
			http.Error(w, "unexpected request "+r.URL.String(), http.StatusBadRequest)
			return
		}
		if got := r.URL.Query().Get("q"); got != "GameCube" {
			http.Error(w, "unexpected query "+got, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(fixture)
	}))
	defer srv.Close()

	p := &SearXNG{BaseURL: srv.URL}
	results, err := p.Search(context.Background(), "GameCube", Options{Max: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// 5 recorded hits, 2 dropped (javascript: scheme, empty URL).
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3: %+v", len(results), results)
	}
	if results[0].URL != "https://en.wikipedia.org/wiki/Nintendo_GameCube" {
		t.Errorf("first URL = %q", results[0].URL)
	}
	for _, r := range results {
		if r.Source != "searxng" || r.Snippet == "" || r.RetrievedAt.IsZero() {
			t.Errorf("result fields incomplete: %+v", r)
		}
	}
	// Max honored.
	results, err = p.Search(context.Background(), "GameCube", Options{Max: 2})
	if err != nil || len(results) != 2 {
		t.Fatalf("Max=2: %v, %d results", err, len(results))
	}
}

func TestSearXNGErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream failed", http.StatusBadGateway)
	}))
	defer srv.Close()
	_, err := (&SearXNG{BaseURL: srv.URL}).Search(context.Background(), "x", Options{})
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("err = %v, want 502 surfaced", err)
	}
}

// ---- Exa MCP (recorded) -------------------------------------------------------

type mcpRecorder struct {
	mu          sync.Mutex
	calls       []string
	argsByName  map[string]map[string]any
	sessionSeen map[string]string
}

func newMCPTestServer(t *testing.T, fixturePath, toolCallTextOverride string, sse bool) (*httptest.Server, *mcpRecorder) {
	t.Helper()
	var fixture struct {
		InitializeResult json.RawMessage  `json:"initializeResult"`
		ToolsListResult  json.RawMessage  `json:"toolsListResult"`
		ToolCallHits     []map[string]any `json:"toolCallHits"`
	}
	loadAndUnmarshal(t, fixturePath, &fixture)
	var toolCallBody json.RawMessage
	if toolCallTextOverride == "" {
		hitsJSON, _ := json.Marshal(fixture.ToolCallHits)
		toolCallBody = json.RawMessage(fmt.Sprintf(`{"content":[{"type":"text","text":%s}],"isError":false}`, marshalString(t, string(hitsJSON))))
	} else {
		toolCallBody = json.RawMessage(fmt.Sprintf(`{"content":[{"type":"text","text":%s}],"isError":false}`, marshalString(t, toolCallTextOverride)))
	}
	rec := &mcpRecorder{argsByName: map[string]map[string]any{}, sessionSeen: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ID     json.Number `json:"id"`
			Method string      `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		rec.mu.Lock()
		rec.calls = append(rec.calls, req.Method)
		if req.Method == "tools/call" {
			rec.argsByName[req.Params.Name] = req.Params.Arguments
		}
		rec.sessionSeen[req.Method] = r.Header.Get("Mcp-Session-Id")
		rec.mu.Unlock()

		reply := func(result json.RawMessage) {
			var payload string
			if sse {
				// Real SSE transports carry the message on a single data
				// line (or multiple data: lines); compact the JSON so the
				// recorded fixture reproduces that shape.
				var compact any
				if json.Unmarshal(result, &compact) == nil {
					if b, e := json.Marshal(compact); e == nil {
						result = b
					}
				}
				w.Header().Set("Content-Type", "text/event-stream")
				payload = fmt.Sprintf("event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":%s}\n\n", req.ID, result)
			} else {
				w.Header().Set("Content-Type", "application/json")
				payload = fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, req.ID, result)
			}
			w.Write([]byte(payload))
		}
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "sess-123")
			reply(fixture.InitializeResult)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			reply(fixture.ToolsListResult)
		case "tools/call":
			reply(toolCallBody)
		default:
			http.Error(w, "unknown method "+req.Method, http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func TestExaMCPRecordedJSON(t *testing.T) {
	srv, rec := newMCPTestServer(t, "testdata/exa-mcp.json", "", false)
	p := &ExaMCP{Endpoint: srv.URL}
	results, err := p.Search(context.Background(), "GameCube", Options{Max: 5})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3 (file:// hit dropped): %+v", len(results), results)
	}
	if results[0].Title != "Nintendo GameCube - Wikipedia" ||
		results[0].URL != "https://en.wikipedia.org/wiki/Nintendo_GameCube" {
		t.Errorf("first result = %+v", results[0])
	}
	if results[0].Snippet == "" || results[0].Source != "exa-mcp" {
		t.Errorf("first result fields = %+v", results[0])
	}
	// Handshake order and session propagation.
	rec.mu.Lock()
	defer rec.mu.Unlock()
	wantCalls := []string{"initialize", "notifications/initialized", "tools/list", "tools/call"}
	if strings.Join(rec.calls, ",") != strings.Join(wantCalls, ",") {
		t.Errorf("MCP call sequence = %v, want %v", rec.calls, wantCalls)
	}
	for _, m := range []string{"tools/list", "tools/call"} {
		if got := rec.sessionSeen[m]; got != "sess-123" {
			t.Errorf("%s session header = %q, want sess-123", m, got)
		}
	}
	args, ok := rec.argsByName["web_search_exa"]
	if !ok {
		t.Fatalf("tools/call did not use web_search_exa; calls = %v", rec.argsByName)
	}
	if args["query"] != "GameCube" || args["numResults"] != float64(5) {
		t.Errorf("arguments = %v, want query=GameCube numResults=5", args)
	}
}

func TestExaMCPRecordedSSE(t *testing.T) {
	srv, rec := newMCPTestServer(t, "testdata/exa-mcp.json", "", true)
	results, err := (&ExaMCP{Endpoint: srv.URL}).Search(context.Background(), "GameCube", Options{})
	if err != nil {
		t.Fatalf("Search (SSE): %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3: %+v", len(results), results)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.calls) != 4 {
		t.Fatalf("expected 4 MCP calls, got %v", rec.calls)
	}
}

func TestExaMCPToolDiscoveryFallback(t *testing.T) {
	// No web_search_exa in tools/list: a single "web_search" tool without
	// a numResults property — discovery must pick it and skip the arg.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.Number `json:"id"`
			Method string      `json:"method"`
			Params struct {
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "s-1")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-06-18","serverInfo":{"name":"alt","version":"0"}}}`, req.ID)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"web_search","description":"search","inputSchema":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}}]}}`, req.ID)
		case "tools/call":
			if q, _ := req.Params.Arguments["query"]; q != "GameCube" {
				t.Errorf("query arg = %v", req.Params.Arguments)
			}
			if _, hasNum := req.Params.Arguments["numResults"]; hasNum {
				t.Error("numResults must not be sent when the schema does not declare it")
			}
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"GameCube wiki at https://en.wikipedia.org/wiki/Nintendo_GameCube"}],"isError":false}}`, req.ID)
		}
	}))
	defer srv.Close()
	results, err := (&ExaMCP{Endpoint: srv.URL, ToolName: "web_search_exa"}).Search(context.Background(), "GameCube", Options{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// Non-JSON tool text falls back to a single raw-text snippet.
	if len(results) != 1 || results[0].Snippet == "" {
		t.Fatalf("fallback result = %+v", results)
	}
	if !strings.Contains(results[0].Snippet, "GameCube") {
		t.Errorf("snippet lost content: %q", results[0].Snippet)
	}
}

// ---- fixture helpers ---------------------------------------------------------

// TestExaTextBlockParsing covers the human-readable text mode Exa returns
// for web_search_exa ("Title: … / URL: … / Published: … / Highlights: …"),
// as recorded live on 2026-10-17 (trimmed to representative values).
func TestExaTextBlockParsing(t *testing.T) {
	text := `Title: GameCube
URL: https://en.wikipedia.org/wiki/GameCube
Published: 2026-10-14T12:09:31.000Z
Author: N/A
Highlights: |
| --- |
| Nintendo |
Title: GCDB
URL: https://gcdb.org/wiki/GameCube
Published: N/A
Summary: Community database of GameCube data.`
	results, ok := parseExaTextBlocks(text)
	if !ok || len(results) != 2 {
		t.Fatalf("parse = ok:%v n:%d, want 2 blocks", ok, len(results))
	}
	if results[0].Title != "GameCube" || results[0].URL != "https://en.wikipedia.org/wiki/GameCube" {
		t.Errorf("block 1 = %+v", results[0])
	}
	if !strings.Contains(results[0].Snippet, "Nintendo") {
		t.Errorf("block 1 snippet lost highlights: %q", results[0].Snippet)
	}
	if strings.Contains(results[0].Snippet, "GCDB") {
		t.Errorf("block 1 snippet leaked block 2 title: %q", results[0].Snippet)
	}
	if results[1].Title != "GCDB" || results[1].URL != "https://gcdb.org/wiki/GameCube" {
		t.Errorf("block 2 = %+v", results[1])
	}
	// Non-URL text falls through to the raw-snippet fallback.
	res, err := parseExaToolText("exa-mcp", "web_search_exa", "just words, no urls", 5, time.Now())
	if err != nil || len(res) != 1 || res[0].URL != "" || res[0].Snippet == "" {
		t.Fatalf("fallback = %+v err=%v", res, err)
	}
}

func TestExaUnstructuredURLFallback(t *testing.T) {
	text := `Here are some pages about GameCube: check
https://en.wikipedia.org/wiki/GameCube for the console and
https://gcdb.org/ for data. More info at https://example-modding.org/x.`
	results, err := parseExaToolText("exa-mcp", "web_search_exa", text, 5, time.Now())
	if err != nil || len(results) != 3 {
		t.Fatalf("results = %d err=%v, want 3", len(results), err)
	}
	if results[0].URL != "https://en.wikipedia.org/wiki/GameCube" {
		t.Errorf("first URL = %q", results[0].URL)
	}
	for _, r := range results {
		if r.Snippet == "" || !strings.Contains(r.Snippet, "GameCube") {
			t.Errorf("snippet lost content: %+v", r)
		}
	}
}

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

func loadAndUnmarshal(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
}

// marshalString JSON-encodes s as a JSON string literal.
func marshalString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal string: %v", err)
	}
	return string(b)
}
