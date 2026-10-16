package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/dbirks/game-narrator/internal/bootstrap"
	"github.com/dbirks/game-narrator/internal/webassets"
)

// uiBuild is a stand-in Vite output: a hashed asset bundle, a stylesheet, an
// icon and the SPA entry point. Injected through WithWebAssets so the static
// rules are tested against a known bundle instead of whatever happens to be
// compiled into the binary under test (docs/spec/12 "Unit" mode).
func uiBuild() fstest.MapFS {
	return fstest.MapFS{
		"index.html":              {Data: []byte("<!doctype html><title>Game Narrator</title>")},
		"assets/index-abc123.js":  {Data: []byte("console.log('ui');")},
		"assets/index-abc123.css": {Data: []byte("body{margin:0}")},
		"assets/logo-def456.svg":  {Data: []byte("<svg xmlns=\"http://www.w3.org/2000/svg\"/>")},
		"favicon.ico":             {Data: []byte("\x00\x00\x01\x00")},
	}
}

// newUIHandler serves uiBuild() for non-API paths.
func newUIHandler() http.Handler {
	return newHandler(WithWebAssets(uiBuild()))
}

// newNoUIHandler has an index.html-free bundle: every UI path must answer 503.
func newNoUIHandler() http.Handler {
	return newHandler(WithWebAssets(fstest.MapFS{
		"assets/index-abc123.js": {Data: []byte("console.log('ui');")},
	}))
}

func newHandler(opts ...Option) http.Handler {
	build := bootstrap.BuildInfo{
		Version:   "v0.0.1-test",
		Commit:    "deadbee",
		GoVersion: "go1.27.2",
		GOOS:      "linux",
		GOARCH:    "arm64",
	}
	started := time.Date(2026, time.October, 16, 3, 4, 5, 0, time.UTC)
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return New(build, started, log, opts...).Handler()
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

func TestStaticRootServesIndexHTML(t *testing.T) {
	rec := do(t, newUIHandler(), http.MethodGet, "/")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	requireContentType(t, rec, "text/html; charset=utf-8")
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Cache-Control = %q, want no-cache", got)
	}
	if body := rec.Body.String(); !strings.Contains(body, "Game Narrator") {
		t.Fatalf("body = %q, want the SPA entry point", body)
	}
	requireSecurityHeaders(t, rec)
}

// Deep links must reach the SPA, not a 404: the client router owns these paths
// (docs/spec/09 "Embedded binary test" — SPA deep links).
func TestStaticDeepLinksFallBackToIndex(t *testing.T) {
	h := newUIHandler()
	for _, target := range []string{
		"/diagnostics",
		"/games/x/dialogue",
		"/games/demo/publish",
		"/settings/audio",
		"/developer",
	} {
		rec := do(t, h, http.MethodGet, target)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200 (body %s)", target, rec.Code, rec.Body.String())
		}
		requireContentType(t, rec, "text/html; charset=utf-8")
		if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
			t.Fatalf("%s: Cache-Control = %q, want no-cache", target, got)
		}
		if body := rec.Body.String(); !strings.Contains(body, "<title>Game Narrator</title>") {
			t.Fatalf("%s: body = %q, want index.html", target, body)
		}
		requireSecurityHeaders(t, rec)
	}
}

func TestStaticServesAssetsWithCorrectContentType(t *testing.T) {
	h := newUIHandler()
	cases := []struct {
		target, wantContentType, wantBody string
	}{
		{"/assets/index-abc123.js", "text/javascript", "console.log"},
		{"/assets/index-abc123.css", "text/css", "body{margin:0}"},
		{"/assets/logo-def456.svg", "image/svg+xml", "<svg"},
	}
	for _, tc := range cases {
		rec := do(t, h, http.MethodGet, tc.target)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200 (body %s)", tc.target, rec.Code, rec.Body.String())
		}
		requireContentType(t, rec, tc.wantContentType)
		if !strings.Contains(rec.Body.String(), tc.wantBody) {
			t.Fatalf("%s: body = %q, want it to contain %q", tc.target, rec.Body.String(), tc.wantBody)
		}
	}
}

// Hashed Vite output is immutable; other files (index.html, an unhashed
// favicon) must be revalidated.
func TestStaticCacheControl(t *testing.T) {
	h := newUIHandler()
	cases := []struct{ target, want string }{
		{"/assets/index-abc123.js", "public, max-age=31536000, immutable"},
		{"/assets/index-abc123.css", "public, max-age=31536000, immutable"},
		{"/favicon.ico", "no-cache"},
		{"/diagnostics", "no-cache"},
	}
	for _, tc := range cases {
		rec := do(t, h, http.MethodGet, tc.target)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", tc.target, rec.Code)
		}
		if got := rec.Header().Get("Cache-Control"); got != tc.want {
			t.Fatalf("%s: Cache-Control = %q, want %q", tc.target, got, tc.want)
		}
	}
}

// A request that names a file must never be answered with index.html: that
// would hand HTML to a script loader and hide a broken bundle.
func TestStaticMissingFileWithExtensionIsProblem404(t *testing.T) {
	h := newUIHandler()
	for _, target := range []string{
		"/assets/missing.js",
		"/assets/missing-deadbeef.wasm",
		"/favicon.png",
		"/robots.txt",
		"/../outside.html",
	} {
		rec := do(t, h, http.MethodGet, target)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want 404 (body %s)", target, rec.Code, rec.Body.String())
		}
		requireContentType(t, rec, ContentTypeProblem)
		assertProblem(t, rec, http.StatusNotFound, "Not Found", target)
	}
}

// Traversal attempts, encoded or not, are 404s. The SPA fallback must not turn
// them into a 200 and they must never reach outside the embedded filesystem.
func TestStaticRejectsPathTraversal(t *testing.T) {
	h := newUIHandler()
	for _, target := range []string{
		"/%2e%2e/etc/passwd",
		"/%2E%2E/etc/passwd",
		"/assets/%2e%2e/%2e%2e/etc/passwd",
		"/assets/../../etc/passwd",
		"/../etc/passwd",
		"/..%2f..%2fetc/passwd",
		"/%2e%2e%2f%2e%2e%2fetc/passwd",
		"/./../etc/passwd",
	} {
		rec := do(t, h, http.MethodGet, target)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want 404 (body %s)", target, rec.Code, rec.Body.String())
		}
		requireContentType(t, rec, ContentTypeProblem)
		body := decodeJSON(t, rec)
		if body["status"] != float64(http.StatusNotFound) || body["title"] != "Not Found" {
			t.Fatalf("%s: problem body = %v, want a 404 Not Found document", target, body)
		}
		// Nothing outside the bundle is named back or served: the detail is
		// fixed text and the body is not index.html.
		if got := body["detail"]; got != "no embedded asset matches the requested path" {
			t.Fatalf("%s: detail = %v, want the fixed not-found detail", target, got)
		}
	}
}

func TestStaticSecurityHeaders(t *testing.T) {
	h := newUIHandler()
	for _, target := range []string{"/", "/diagnostics", "/assets/index-abc123.js"} {
		rec := do(t, h, http.MethodGet, target)
		requireSecurityHeaders(t, rec)
	}
}

const wantCSP = "default-src 'self'; img-src 'self' data: blob:; media-src 'self' blob:; " +
	"connect-src 'self'; style-src 'self'; script-src 'self'; frame-ancestors 'none'; base-uri 'self'"

func requireSecurityHeaders(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if got := rec.Header().Get("Content-Security-Policy"); got != wantCSP {
		t.Fatalf("Content-Security-Policy = %q, want %q", got, wantCSP)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("Referrer-Policy = %q, want no-referrer", got)
	}
}

// Static content is GET/HEAD only; a wrong method is a problem+json 405 with
// the same shape the API uses.
func TestStaticRejectsOtherMethods(t *testing.T) {
	h := newUIHandler()
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		rec := do(t, h, method, "/diagnostics")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s /diagnostics: status = %d, want 405 (body %s)", method, rec.Code, rec.Body.String())
		}
		requireContentType(t, rec, ContentTypeProblem)
		assertProblem(t, rec, http.StatusMethodNotAllowed, "Method Not Allowed", "/diagnostics")
		if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
			t.Fatalf("%s: Allow = %q, want \"GET, HEAD\"", method, got)
		}
	}
}

func TestStaticHeadMatchesGetHeaders(t *testing.T) {
	h := newUIHandler()
	rec := do(t, h, http.MethodHead, "/assets/index-abc123.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	requireContentType(t, rec, "text/javascript")
	requireSecurityHeaders(t, rec)
	if body := rec.Body.String(); body != "" {
		t.Fatalf("HEAD body = %q, want empty", body)
	}
}

// Without a Vite build in the binary, UI paths are 503 problem+json rather
// than an empty 200 — and API paths are untouched.
func TestStaticNotBuiltReturns503Problem(t *testing.T) {
	h := newNoUIHandler()
	for _, target := range []string{"/", "/diagnostics", "/games/x/dialogue"} {
		rec := do(t, h, http.MethodGet, target)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: status = %d, want 503 (body %s)", target, rec.Code, rec.Body.String())
		}
		requireContentType(t, rec, ContentTypeProblem)
		assertProblem(t, rec, http.StatusServiceUnavailable, "Service Unavailable", target)
		if got := decodeJSON(t, rec)["detail"]; got != NotBuiltDetail {
			t.Fatalf("%s: detail = %v, want %q", target, got, NotBuiltDetail)
		}
		requireSecurityHeaders(t, rec)
	}

	// Missing files still 404 rather than 503 when the build is present; the
	// 503 is specifically "no UI at all".
	rec := do(t, h, http.MethodGet, "/assets/index-abc123.js")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// API behaviour must be unchanged by static serving: /api/* keeps
// problem+json 404/405 and never falls back to index.html.
func TestAPIPathsUnchangedByStaticServing(t *testing.T) {
	h := newUIHandler()

	rec := do(t, h, http.MethodGet, "/api/v1/nope")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	requireContentType(t, rec, ContentTypeProblem)
	assertProblem(t, rec, http.StatusNotFound, "Not Found", "/api/v1/nope")

	rec = do(t, h, http.MethodGet, "/api/index.html")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("/api/index.html: status = %d, want 404 (SPA fallback must not serve inside /api)", rec.Code)
	}
	requireContentType(t, rec, ContentTypeProblem)

	rec = do(t, h, http.MethodPost, "/api/v1/health/live")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	requireContentType(t, rec, ContentTypeProblem)
	if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
		t.Fatalf("Allow = %q, want \"GET, HEAD\"", got)
	}

	rec = do(t, h, http.MethodGet, "/api/v1/health/ready")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	requireContentType(t, rec, ContentTypeJSON)
}

// The default wiring (no WithWebAssets) is the embedded build. Whether a real
// Vite output is compiled in depends on the checkout, so the assertion follows
// webassets.Built() instead of assuming either state.
func TestDefaultHandlerUsesEmbeddedWebAssets(t *testing.T) {
	rec := do(t, newHandler(), http.MethodGet, "/diagnostics")

	if webassets.Built() {
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (embedded build present, body %s)", rec.Code, rec.Body.String())
		}
		requireContentType(t, rec, "text/html; charset=utf-8")
		return
	}

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 for a checkout without a web build (body %s)", rec.Code, rec.Body.String())
	}
	requireContentType(t, rec, ContentTypeProblem)
	if got := decodeJSON(t, rec)["detail"]; got != NotBuiltDetail {
		t.Fatalf("detail = %v, want %q", got, NotBuiltDetail)
	}
}

func TestStaticRequestIDCorrelates(t *testing.T) {
	rec := do(t, newUIHandler(), http.MethodGet, "/diagnostics")
	if id := rec.Header().Get(HeaderRequestID); id == "" {
		t.Fatal("missing X-Request-Id on a static response")
	}
}
