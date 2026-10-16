package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/bootstrap"
)

// newTestHandler builds the real router with a fixed build stamp and a discard
// logger, so tests exercise exactly what bootstrap serves.
func newTestHandler() http.Handler {
	build := bootstrap.BuildInfo{
		Version:   "v0.0.1-test",
		Commit:    "deadbee",
		GoVersion: "go1.27.2",
		GOOS:      "linux",
		GOARCH:    "arm64",
	}
	started := time.Date(2026, time.October, 16, 3, 4, 5, 0, time.UTC)
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return New(build, started, log).Handler()
}

func do(t *testing.T, h http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// requireContentType fails unless the Content-Type header equals want exactly.
func requireContentType(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	got := rec.Header().Get("Content-Type")
	if got != want {
		t.Fatalf("Content-Type = %q, want %q (body %s)", got, want, rec.Body.String())
	}
}

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not valid JSON: %v (body %q)", err, rec.Body.String())
	}
	return body
}

func TestHealthLive(t *testing.T) {
	rec := do(t, newTestHandler(), http.MethodGet, "/api/v1/health/live")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	requireContentType(t, rec, ContentTypeJSON)

	body := decodeJSON(t, rec)
	if len(body) != 1 {
		t.Fatalf("unexpected fields: %v", body)
	}
	if got := body["status"]; got != "ok" {
		t.Fatalf("status = %v, want %q", got, "ok")
	}
}

func TestHealthReadyHasEmptyChecksMap(t *testing.T) {
	rec := do(t, newTestHandler(), http.MethodGet, "/api/v1/health/ready")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	requireContentType(t, rec, ContentTypeJSON)

	// checks must serialize as an object, not null, so later dependency
	// checks are additive.
	if !strings.Contains(rec.Body.String(), `"checks":{}`) {
		t.Fatalf("body = %s, want empty checks object", rec.Body.String())
	}
	var body struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "ok" {
		t.Fatalf("status = %q, want ok", body.Status)
	}
	if body.Checks == nil || len(body.Checks) != 0 {
		t.Fatalf("checks = %v, want empty non-nil map", body.Checks)
	}
}

func TestSystemInfo(t *testing.T) {
	rec := do(t, newTestHandler(), http.MethodGet, "/api/v1/system/info")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	requireContentType(t, rec, ContentTypeJSON)

	body := decodeJSON(t, rec)
	want := map[string]any{
		"version":    "v0.0.1-test",
		"commit":     "deadbee",
		"go_version": "go1.27.2",
		"os":         "linux",
		"arch":       "arm64",
	}
	for k, v := range want {
		if got, ok := body[k]; !ok {
			t.Fatalf("missing field %q in %v", k, body)
		} else if got != v {
			t.Fatalf("%s = %v, want %v", k, got, v)
		}
	}

	if cpu, ok := body["num_cpu"].(float64); !ok || cpu < 1 {
		t.Fatalf("num_cpu = %v, want >= 1", body["num_cpu"])
	}

	startedAt, ok := body["started_at"].(string)
	if !ok {
		t.Fatalf("started_at = %T, want string", body["started_at"])
	}
	parsed, err := time.Parse(time.RFC3339, startedAt)
	if err != nil {
		t.Fatalf("started_at %q is not RFC3339: %v", startedAt, err)
	}
	if parsed.Location() != time.UTC && parsed.Format("-07:00") != "+00:00" {
		t.Fatalf("started_at %q is not UTC", startedAt)
	}
	if !strings.HasPrefix(startedAt, "2026-10-16T03:04:05Z") {
		t.Fatalf("started_at = %q, want the fixed test start time in UTC", startedAt)
	}
}

func TestSystemInfoLeaksNoHostOrEnv(t *testing.T) {
	rec := do(t, newTestHandler(), http.MethodGet, "/api/v1/system/info")
	body := rec.Body.String()

	if host, _ := os.Hostname(); host != "" && strings.Contains(body, host) {
		t.Fatalf("system/info leaks hostname %q", host)
	}
	for _, frag := range []string{"/home/", "/root/", "PATH=", "KEY=", "TOKEN="} {
		if strings.Contains(body, frag) {
			t.Fatalf("system/info leaks %q: %s", frag, body)
		}
	}
}

var hexID = regexp.MustCompile(`^[0-9a-f]{32}$`)

func TestRequestIDHeaderOnEveryResponse(t *testing.T) {
	h := newTestHandler()
	cases := []struct {
		method, target string
	}{
		{http.MethodGet, "/api/v1/health/live"},
		{http.MethodGet, "/api/v1/health/ready"},
		{http.MethodGet, "/api/v1/system/info"},
		{http.MethodGet, "/api/v1/nope"},
		{http.MethodPost, "/api/v1/health/live"},
		{http.MethodGet, "/"},
	}
	seen := map[string]bool{}
	for _, tc := range cases {
		rec := do(t, h, tc.method, tc.target)
		id := rec.Header().Get(HeaderRequestID)
		if id == "" {
			t.Fatalf("%s %s: missing %s header", tc.method, tc.target, HeaderRequestID)
		}
		if !hexID.MatchString(id) {
			t.Fatalf("%s %s: %s = %q, want 32 hex chars", tc.method, tc.target, HeaderRequestID, id)
		}
		if seen[id] {
			t.Fatalf("%s %s: request id %q reused across requests", tc.method, tc.target, id)
		}
		seen[id] = true
	}
}

func TestUnknownAPIPathReturnsProblemJSON(t *testing.T) {
	rec := do(t, newTestHandler(), http.MethodGet, "/api/v1/nope")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
	requireContentType(t, rec, ContentTypeProblem)
	assertProblem(t, rec, http.StatusNotFound, "Not Found", "/api/v1/nope")
}

func TestWrongMethodOnKnownPathReturnsProblemJSON(t *testing.T) {
	rec := do(t, newTestHandler(), http.MethodPost, "/api/v1/health/live")

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405 (body %s)", rec.Code, rec.Body.String())
	}
	requireContentType(t, rec, ContentTypeProblem)
	assertProblem(t, rec, http.StatusMethodNotAllowed, "Method Not Allowed", "/api/v1/health/live")
	if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
		t.Fatalf("Allow = %q, want \"GET, HEAD\"", got)
	}
}

// HEAD is served by GET patterns, so it must not be reported as a method
// mismatch.
func TestHeadOnKnownPathIsNot405(t *testing.T) {
	rec := do(t, newTestHandler(), http.MethodHead, "/api/v1/health/live")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
}

// A deeper unknown path under a known prefix must still produce the problem
// document rather than a redirect or plain-text 404.
func TestUnknownNestedAPIPathReturnsProblemJSON(t *testing.T) {
	for _, target := range []string{"/api/v1/sessions", "/api/v1/", "/api/"} {
		rec := do(t, newTestHandler(), http.MethodGet, target)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want 404 (body %s)", target, rec.Code, rec.Body.String())
		}
		requireContentType(t, rec, ContentTypeProblem)
	}
}

func TestNonAPIPathAlsoProblems(t *testing.T) {
	rec := do(t, newTestHandler(), http.MethodGet, "/")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	requireContentType(t, rec, ContentTypeProblem)
	assertProblem(t, rec, http.StatusNotFound, "Not Found", "/")
}

// assertProblem checks every RFC 9457 field the spec requires, including
// correlation with the X-Request-Id header.
func assertProblem(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantTitle, wantInstance string) {
	t.Helper()

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("problem body is not JSON: %v (%q)", err, rec.Body.String())
	}
	for _, field := range []string{"type", "title", "status", "detail", "instance", "request_id"} {
		v, ok := body[field]
		if !ok {
			t.Fatalf("problem body missing %q: %v", field, body)
		}
		if s, isString := v.(string); isString && s == "" {
			t.Fatalf("problem field %q is empty: %v", field, body)
		}
		if v == nil {
			t.Fatalf("problem field %q is null: %v", field, body)
		}
	}
	if got := body["type"]; got != "urn:game-narrator:error:"+titleSlug(t, wantTitle) {
		t.Fatalf("problem type = %v, want urn tied to %q", got, wantTitle)
	}
	if got := body["title"]; got != wantTitle {
		t.Fatalf("problem title = %v, want %q", got, wantTitle)
	}
	if got := body["status"]; got != float64(wantStatus) {
		t.Fatalf("problem status = %v, want %d", got, wantStatus)
	}
	if got := body["instance"]; got != wantInstance {
		t.Fatalf("problem instance = %v, want %q", got, wantInstance)
	}
	if got, want := body["request_id"], rec.Header().Get(HeaderRequestID); got != want {
		t.Fatalf("problem request_id = %v, want %v (header)", got, want)
	}
	if got := body["detail"]; got.(string) == "" {
		t.Fatal("problem detail is empty")
	}
}

func titleSlug(t *testing.T, title string) string {
	t.Helper()
	switch title {
	case "Not Found":
		return "not-found"
	case "Method Not Allowed":
		return "method-not-allowed"
	}
	t.Fatalf("unknown title %q", title)
	return ""
}
