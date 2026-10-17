// Package search implements the onboarding agent's web search providers
// behind one interface (docs/spec/03: "Implement a local search provider
// interface that can be backed by user-configured search services rather
// than hardcoding any one vendor").
//
// Providers: Disabled (explicit "online search disabled" setting),
// SearXNG (self-hosted JSON API), ExaMCP (Exa's keyless public MCP
// endpoint — the default, for parity with the operator's pi setup, see
// ADR 0018). Every provider is wrapped by Budgeted to enforce
// max_searches_per_run and a minimum interval between calls
// (docs/spec/03 tool guardrails "bounded queries"; docs/spec/06
// "15-30 s per search").
//
// Result URLs are hostile data: callers MUST re-validate them with the
// web fetcher's policy before fetching (web.Policy.ValidURL). This
// package never fetches result URLs itself.
package search

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// DefaultMaxResults bounds a single query when Options.Max is unset.
const DefaultMaxResults = 5

// DefaultBudget is the default search budget per agent run
// (examples/config: agent.max_searches_per_run = 40).
const DefaultBudget = 40

// Options tunes a single Search call.
type Options struct {
	// Max is the maximum number of results to return; <=0 means
	// DefaultMaxResults.
	Max int
}

func (o Options) max() int {
	if o.Max <= 0 {
		return DefaultMaxResults
	}
	return o.Max
}

// Result is one search hit. URL/Snippet/Title are untrusted data from a
// third party: validate the URL before fetching, quote the text before it
// enters the agent loop.
type Result struct {
	Title       string
	URL         string
	Snippet     string
	Source      string    // provider name
	RetrievedAt time.Time // query completion (UTC)
}

// Provider is the interface all search backends implement.
type Provider interface {
	// Name identifies the backend in logs and tool traces.
	Name() string
	// Search returns up to opts.max() results for query.
	Search(ctx context.Context, query string, opts Options) ([]Result, error)
}

// ErrSearchDisabled is returned by Disabled; agents must treat it as a
// configuration state, not a failure to retry (docs/spec/12: "online
// search disabled").
var ErrSearchDisabled = errors.New("web search is disabled by configuration")

// Disabled is the provider used when search is explicitly disabled.
type Disabled struct{}

func (Disabled) Name() string { return "disabled" }

func (Disabled) Search(ctx context.Context, query string, opts Options) ([]Result, error) {
	return nil, ErrSearchDisabled
}

// Errors returned by Budgeted.
var (
	// ErrBudgetExhausted: max_searches_per_run reached for this run.
	ErrBudgetExhausted = errors.New("search budget exhausted (max searches per run)")
	// ErrRateLimited: minimum interval between searches not yet elapsed
	// (or the wait was canceled).
	ErrRateLimited = errors.New("search rate limit")
)

// Budgeted enforces per-run call budgets and a minimum interval between
// calls around any Provider (config: agent.max_searches_per_run,
// agent rate limits). A call consumes the budget when attempted, whether
// the provider succeeds or fails. Safe for concurrent use.
type Budgeted struct {
	Provider    Provider
	MaxCalls    int           // <=0 means DefaultBudget
	MinInterval time.Duration // 0 means no rate limiting
	Logger      *slog.Logger  // optional; default slog.Default()

	now func() time.Time

	mu    sync.Mutex
	calls int
	last  time.Time
}

// NewBudgeted wraps p. maxCalls <= 0 means DefaultBudget; minInterval < 0
// is treated as 0.
func NewBudgeted(p Provider, maxCalls int, minInterval time.Duration) *Budgeted {
	if maxCalls <= 0 {
		maxCalls = DefaultBudget
	}
	if minInterval < 0 {
		minInterval = 0
	}
	return &Budgeted{
		Provider:    p,
		MaxCalls:    maxCalls,
		MinInterval: minInterval,
		now:         func() time.Time { return time.Now().UTC() },
	}
}

// Name identifies the wrapped provider.
func (b *Budgeted) Name() string {
	if b.Provider == nil {
		return "budgeted(nil)"
	}
	return "budgeted(" + b.Provider.Name() + ")"
}

func (b *Budgeted) maxCalls() int {
	if b.MaxCalls <= 0 {
		return DefaultBudget
	}
	return b.MaxCalls
}

// BudgetUsed reports the number of calls consumed so far.
func (b *Budgeted) BudgetUsed() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

// Search calls the wrapped provider under the budget and rate limit.
// Query and result counts are logged at debug; full result text is not.
func (b *Budgeted) Search(ctx context.Context, query string, opts Options) ([]Result, error) {
	if b.Provider == nil {
		return nil, errors.New("search: budgeted provider is nil")
	}
	b.mu.Lock()
	if b.calls >= b.maxCalls() {
		used, max := b.calls, b.maxCalls()
		b.mu.Unlock()
		return nil, fmt.Errorf("%w: %d/%d", ErrBudgetExhausted, used, max)
	}
	b.calls++
	var wait time.Duration
	if b.MinInterval > 0 && !b.last.IsZero() {
		wait = b.last.Add(b.MinInterval).Sub(b.now())
		if wait < 0 {
			wait = 0
		}
	}
	b.last = b.now()
	b.mu.Unlock()

	if wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil, fmt.Errorf("%w: %v", ErrRateLimited, ctx.Err())
		}
	}

	log := b.Logger
	if log == nil {
		log = slog.Default()
	}
	log.Debug("web search", "provider", b.Provider.Name(), "query", query, "max_results", opts.max())
	start := time.Now()
	results, err := b.Provider.Search(ctx, query, opts)
	latency := time.Since(start)
	if err != nil {
		log.Debug("web search failed", "provider", b.Provider.Name(), "query", query, "latency", latency.String())
		return nil, err
	}
	var firstURL string
	if len(results) > 0 {
		firstURL = results[0].URL
	}
	log.Debug("web search done", "provider", b.Provider.Name(), "query", query, "results", len(results), "latency", latency.String(), "first_url", firstURL)
	return results, nil
}
