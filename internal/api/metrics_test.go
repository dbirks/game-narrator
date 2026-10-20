package api

// The metrics surface's HTTP tests (docs/spec/13 "Metrics", ADR-0106): the
// /metrics scraper endpoint's gate (admin principal or the configured token,
// off by default) and the developer JSON snapshot the diagnostics and health
// surfaces read. The exposition format, the label-cardinality bound and the
// concurrency proof are the telemetry package's own tests; these prove the
// route, the gate and the degradation the API adds on top of the registry.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/telemetry"
)

// metricsTestEnv is the metrics half of the appliance: real auth store, a
// wired telemetry catalog (when the test wants one), the server under test.
type metricsTestEnv struct {
	t       *testing.T
	ctx     context.Context
	a       *testAuth
	srv     *httptest.Server
	s       *Server
	metrics *telemetry.Product
}

// newMetricsEnv builds a server with WithMetrics(p, token) when p is non-nil
// (token wired as the test's static token). A nil p is the off shape the
// "not enabled" 404/503 tests exercise.
func newMetricsEnv(t *testing.T, p *telemetry.Product, token string) *metricsTestEnv {
	t.Helper()
	a := newTestAuth(t)
	env := &metricsTestEnv{t: t, ctx: context.Background(), a: a, metrics: p}
	opts := []Option{
		WithWebAssets(uiBuild()),
		WithAuth(a.svc, a.repo),
	}
	if p != nil {
		opts = append(opts, WithMetrics(p, token))
	}
	env.s = New(testBuild, testStarted, discardLogger, opts...)
	env.srv = httptest.NewServer(env.s.Handler())
	t.Cleanup(env.srv.Close)
	return env
}

// get issues a GET with the named role's bearer token ("" for none) and
// returns the response and body.
func (e *metricsTestEnv) get(role auth.Role, token, target string) (*http.Response, []byte) {
	e.t.Helper()
	req, err := http.NewRequestWithContext(e.ctx, http.MethodGet, e.srv.URL+target, nil)
	if err != nil {
		e.t.Fatalf("new request: %v", err)
	}
	if role != auth.RoleNone {
		req.Header.Set("Authorization", "Bearer "+e.a.bearer(role))
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		e.t.Fatalf("GET %s: %v", target, err)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		e.t.Fatalf("read body: %v", err)
	}
	resp.Body.Close()
	return resp, body
}

// TestMetricsScrapeGate is the /metrics access-control proof (docs/spec/14):
// unauthenticated and below-admin callers are refused, an admin principal and
// the configured token are admitted, and an unwired catalog answers 404 rather
// than the SPA answering for the scraper.
func TestMetricsScrapeGate(t *testing.T) {
	const scrapeToken = "scrape-secret"

	t.Run("off by default answers 404", func(t *testing.T) {
		e := newMetricsEnv(t, nil, "")
		resp, body := e.get(auth.RoleAdmin, "", "/metrics")
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET /metrics (unwired) = %d (%s), want 404", resp.StatusCode, body)
		}
		// Even an admin gets the 404: a process that wired no catalog has
		// nothing to scrape, and the SPA's index.html must not answer for it.
		resp, body = e.get(auth.RoleAdmin, "", "/metrics")
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET /metrics (unwired, admin) = %d (%s), want 404", resp.StatusCode, body)
		}
	})

	t.Run("wired but unauthenticated is refused", func(t *testing.T) {
		e := newMetricsEnv(t, telemetry.NewProduct(), scrapeToken)
		resp, _ := e.get(auth.RoleNone, "", "/metrics")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("GET /metrics (unauthenticated) = %d, want 401", resp.StatusCode)
		}
	})

	t.Run("a below-admin role is refused", func(t *testing.T) {
		e := newMetricsEnv(t, telemetry.NewProduct(), scrapeToken)
		resp, _ := e.get(auth.RoleDeveloper, "", "/metrics")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("GET /metrics (developer) = %d, want 401 (the scrape is admin-tier)", resp.StatusCode)
		}
	})

	t.Run("an admin principal is admitted", func(t *testing.T) {
		p := telemetry.NewProduct()
		p.FramesDelivered(telemetry.Labels{Game: "zelda", ProfileRevision: "3"}, 7)
		e := newMetricsEnv(t, p, scrapeToken)
		resp, body := e.get(auth.RoleAdmin, "", "/metrics")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /metrics (admin) = %d (%s), want 200", resp.StatusCode, body)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Fatalf("content-type = %q, want text/plain (the exposition)", ct)
		}
		if !strings.Contains(string(body), "game_narrator_frames_delivered_total{game=\"zelda\",profile_revision=\"3\"} 7") {
			t.Fatalf("the exposition is missing the booked sample:\n%s", body)
		}
	})

	t.Run("the configured token is admitted without a user credential", func(t *testing.T) {
		e := newMetricsEnv(t, telemetry.NewProduct(), scrapeToken)
		resp, _ := e.get(auth.RoleNone, scrapeToken, "/metrics")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /metrics (scrape token) = %d, want 200", resp.StatusCode)
		}
	})

	t.Run("a wrong token is refused", func(t *testing.T) {
		e := newMetricsEnv(t, telemetry.NewProduct(), scrapeToken)
		resp, _ := e.get(auth.RoleNone, "scrape-wrong", "/metrics")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("GET /metrics (wrong token) = %d, want 401", resp.StatusCode)
		}
	})

	t.Run("no token configured keeps the endpoint admin-only", func(t *testing.T) {
		e := newMetricsEnv(t, telemetry.NewProduct(), "")
		// A bearer that is not the (absent) token and not a known user is
		// simply an unknown token: refused.
		resp, _ := e.get(auth.RoleNone, "scrape-secret", "/metrics")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("GET /metrics (bearer, no token configured) = %d, want 401", resp.StatusCode)
		}
	})
}

// TestDevMetricsSnapshot is the internal snapshot API (docs/spec/13 "Metrics"):
// the developer JSON the diagnostics and health surfaces read — the developer
// grant from the scope table, a lower role refused, and an unwired catalog
// answering 503 rather than an empty "everything is zero" body.
func TestDevMetricsSnapshot(t *testing.T) {
	const path = "/api/v1/dev/metrics"

	t.Run("off by default answers 503", func(t *testing.T) {
		e := newMetricsEnv(t, nil, "")
		resp, body := e.get(auth.RoleDeveloper, "", path)
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("GET %s (unwired) = %d (%s), want 503", path, resp.StatusCode, body)
		}
	})

	t.Run("a lower role is refused", func(t *testing.T) {
		e := newMetricsEnv(t, telemetry.NewProduct(), "")
		resp, _ := e.get(auth.RoleViewer, "", path)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("GET %s (viewer) = %d, want 403 (the /api/v1/dev grant is developer)", path, resp.StatusCode)
		}
	})

	t.Run("a developer is admitted and gets the snapshot", func(t *testing.T) {
		p := telemetry.NewProduct()
		p.OCRDuration(telemetry.Labels{Game: "zelda", ProfileRevision: "3"}, 40*time.Millisecond)
		e := newMetricsEnv(t, p, "")
		resp, body := e.get(auth.RoleDeveloper, "", path)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s (developer) = %d (%s), want 200", path, resp.StatusCode, body)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Fatalf("content-type = %q, want application/json", ct)
		}
		var snap telemetry.Snapshot
		if err := json.Unmarshal(body, &snap); err != nil {
			t.Fatalf("body is not the snapshot: %v (%s)", err, body)
		}
		if snap.GeneratedAt.IsZero() {
			t.Fatal("generated_at is zero: the snapshot must carry the reading's freshness")
		}
		// The booked OCR sample is present, labelled by the pair, with its
		// histogram slots.
		var ocr *telemetry.MetricSnapshot
		for i := range snap.Metrics {
			if snap.Metrics[i].Name == "game_narrator_ocr_duration_seconds" {
				ocr = &snap.Metrics[i]
			}
		}
		if ocr == nil {
			t.Fatalf("the snapshot is missing ocr_duration_seconds:\n%s", body)
		}
		if ocr.Type != "histogram" || len(ocr.Series) != 1 {
			t.Fatalf("ocr_duration_seconds = %s with %d series, want histogram with 1", ocr.Type, len(ocr.Series))
		}
		s := ocr.Series[0]
		if s.Labels["game"] != "zelda" || s.Labels["profile_revision"] != "3" {
			t.Fatalf("the series labels = %v, want game=zelda profile_revision=3", s.Labels)
		}
		if s.Count == nil || *s.Count != 1 {
			t.Fatalf("the histogram count = %v, want 1", s.Count)
		}
		if len(s.Buckets) != len(telemetry.DefaultBuckets)+1 {
			t.Fatalf("the histogram has %d slots, want %d (the edges plus the overflow)", len(s.Buckets), len(telemetry.DefaultBuckets)+1)
		}
	})
}
