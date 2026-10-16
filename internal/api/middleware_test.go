package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// httptestRequest builds a GET request carrying a client-supplied correlation
// id (used verbatim, including the empty case).
func httptestRequest(method, target, requestID string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	if requestID != "" {
		req.Header.Set(HeaderRequestID, requestID)
	}
	return req
}

func serve(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// A client may pin its own correlation id, but only within a bounded safe
// character set: an oversized or punctuation-bearing value is a log-injection
// / header-padding vector, so it must be discarded and replaced.
func TestRequestIDAcceptedFromClient(t *testing.T) {
	h := newUIHandler()
	for _, id := range []string{
		"a",
		"trace-42",
		"web.1",
		"Under_score-9.ok",
		strings.Repeat("x", 64),
	} {
		for _, target := range []string{"/api/v1/health/live", "/diagnostics", "/api/v1/nope"} {
			req := httptestRequest(http.MethodGet, target, id)
			rec := serve(h, req)
			if got := rec.Header().Get(HeaderRequestID); got != id {
				t.Fatalf("%s %s: %s = %q, want the client id %q", req.Method, target, HeaderRequestID, got, id)
			}
		}
	}
}

func TestRequestIDRejectedFromClient(t *testing.T) {
	h := newUIHandler()
	cases := []struct {
		name, id string
	}{
		{"empty", ""},
		{"too long", strings.Repeat("x", 65)},
		{"slash", "a/b"},
		{"space", "a b"},
		{"leading space", " a"},
		{"angle brackets", "<script>"},
		{"non-ascii", "corrélait"},
		{"emoji", "🚀"},
		{"colon and comma", "a:b,c"},
		{"hash", "a#b"},
	}
	for _, tc := range cases {
		for _, target := range []string{"/api/v1/health/live", "/diagnostics"} {
			req := httptestRequest(http.MethodGet, target, tc.id)
			rec := serve(h, req)
			got := rec.Header().Get(HeaderRequestID)
			if got == tc.id && tc.id != "" {
				t.Fatalf("%s: client id %q was echoed for %s", tc.name, tc.id, target)
			}
			if !hexID.MatchString(got) {
				t.Fatalf("%s: %s = %q, want a freshly generated 32 hex char id", tc.name, HeaderRequestID, got)
			}
		}
	}
}

// The generated id must be the one inside the problem document too, so the
// header and body always agree.
func TestRejectedRequestIDStillCorrelatesProblem(t *testing.T) {
	rec := serve(newUIHandler(), httptestRequest(http.MethodGet, "/api/v1/nope", "bad id with spaces"))
	body := decodeJSON(t, rec)
	if got, want := body["request_id"], rec.Header().Get(HeaderRequestID); got != want {
		t.Fatalf("problem request_id = %v, want header %v", got, want)
	}
	if got := rec.Header().Get(HeaderRequestID); !hexID.MatchString(got) {
		t.Fatalf("request id %q is not a generated id", got)
	}
}
