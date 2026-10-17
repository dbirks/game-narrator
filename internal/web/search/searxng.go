package search

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/bootstrap"
)

// DefaultTimeout bounds a single search request (docs/spec/06: agent
// browsing is 15-30 s per search).
const DefaultTimeout = 20 * time.Second

// DefaultSearchUserAgent identifies agent search traffic.
var DefaultSearchUserAgent = "GameNarrator/" + bootstrap.Version().Version + " (+research)"

// maxSearXNGResponse caps the JSON payload of one SearXNG response
// (defensive; not a documented SearXNG limit).
const maxSearXNGResponse = 1 << 20 // 1 MiB

// SearXNG queries a self-hosted SearXNG instance's JSON API:
//
//	GET {BaseURL}/search?q=<query>&format=json
//
// The base URL is operator configuration, not hostile input; the service
// endpoint itself is assumed trusted (see ADR 0018), but the returned
// results (titles, snippets, URLs) remain untrusted data.
type SearXNG struct {
	// BaseURL is the SearXNG instance base, e.g. "http://searxng.lan:8080".
	// The trailing slash (if any) is stripped.
	BaseURL string

	// Client is optional; default is a bounded http.Client.
	Client *http.Client

	// Timeout bounds each request; 0 means DefaultTimeout.
	Timeout time.Duration

	// UserAgent is optional; 0 means DefaultSearchUserAgent.
	UserAgent string

	// Logger is optional; default slog.Default().
	Logger *slog.Logger
}

// Name implements Provider.
func (s *SearXNG) Name() string { return "searxng" }

type searXNGResponse struct {
	Query   string       `json:"query"`
	Results []searXNGHit `json:"results"`
	Answers []string     `json:"answers"`
}

type searXNGHit struct {
	URL      string `json:"url"`
	Title    string `json:"title"`
	Content  string `json:"content"`
	Category string `json:"category"`
	Engine   string `json:"engine"`
}

// Search implements Provider.
func (s *SearXNG) Search(ctx context.Context, query string, opts Options) ([]Result, error) {
	if s.BaseURL == "" {
		return nil, fmt.Errorf("search: searxng BaseURL not configured")
	}
	endpoint := strings.TrimRight(s.BaseURL, "/") + "/search"
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("search: bad searxng endpoint %q: %w", s.BaseURL, err)
	}
	q := u.Query()
	q.Set("q", query)
	q.Set("format", "json")
	u.RawQuery = q.Encode()

	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: s.timeout()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("search: build request: %w", err)
	}
	ua := s.UserAgent
	if ua == "" {
		ua = DefaultSearchUserAgent
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("search: searxng request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSearXNGResponse+1))
	if err != nil {
		return nil, fmt.Errorf("search: read searxng response: %w", err)
	}
	if len(body) > maxSearXNGResponse {
		return nil, fmt.Errorf("search: searxng response exceeds %d bytes", maxSearXNGResponse)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("search: searxng HTTP %d", resp.StatusCode)
	}

	var parsed searXNGResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("search: decode searxng response: %w", err)
	}
	now := time.Now().UTC()
	results := make([]Result, 0, len(parsed.Results))
	for _, hit := range parsed.Results {
		if !isSafeResultURL(hit.URL) {
			continue
		}
		results = append(results, Result{
			Title:       hit.Title,
			URL:         hit.URL,
			Snippet:     hit.Content,
			Source:      s.Name(),
			RetrievedAt: now,
		})
		if opts.max() > 0 && len(results) >= opts.max() {
			break
		}
	}
	return results, nil
}

func (s *SearXNG) timeout() time.Duration {
	if s.Timeout > 0 {
		return s.Timeout
	}
	return DefaultTimeout
}

// isSafeResultURL keeps only absolute http/https URLs from search results
// (first line of defense; the fetcher policy is authoritative).
func isSafeResultURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	return u.Host != ""
}
