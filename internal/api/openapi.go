package api

// GET /api/v1/openapi.json — the machine-readable contract (docs/spec/08
// "generated OpenAPI contract"; ADR-0017 records why it is hand-written and
// contract-tested rather than generated from handler code).
//
// The document is embedded at build time, so a binary always serves the contract
// it was compiled with: a schema that drifted from the running handlers is the
// failure mode docs/spec/09's "generate TS clients from OpenAPI" exists to
// prevent, and serving a file read off disk would allow exactly that drift.
//
// What keeps it honest is openapi_contract_test.go, which asserts three things
// against the live router: every registered route is documented with the same
// method and path template and vice versa; every operation's `x-required-role`
// equals the role the scope table actually grants it; and every success body the
// handler tests produced validates against the operation's response schema.

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"net/http"
	"sync"
)

// openAPIPath is where the contract is served.
const openAPIPath = "/api/v1/openapi.json"

//go:embed openapi/openapi.json
var openAPIDocument []byte

var (
	openAPICached     []byte
	openAPICacheOnce  sync.Once
	openAPIParsed     map[string]any
	openAPIParseOnce  sync.Once
	openAPIParseError error
)

// openAPIBytes returns the embedded contract with whitespace collapsed, computed
// once and cached. The bytes come straight from the authored file rather than from
// a decode/re-encode round trip, because Go's map-based JSON encoding would sort
// the keys and turn a hand-ordered document (operations in router order, so a
// reviewer can read it top to bottom against Handler) into an alphabetical one on
// every response. Caching is what makes each response byte-identical, which is what
// lets a client hash the document to tell two builds apart.
func openAPIBytes() []byte {
	openAPICacheOnce.Do(func() {
		var buf bytes.Buffer
		if err := json.Compact(&buf, openAPIDocument); err != nil {
			// An uncompilable document is a build-time bug; the handler reports
			// it as 500 rather than serving bytes that are not JSON.
			openAPIParseError = err
			openAPICached = openAPIDocument
			return
		}
		openAPICached = append(buf.Bytes(), '\n')
	})
	return openAPICached
}

// openAPIRoot parses the embedded document once. Tests use it directly; the
// handler never parses (it writes bytes), so serving costs nothing per request.
func openAPIRoot() (map[string]any, error) {
	openAPIParseOnce.Do(func() {
		if err := json.Unmarshal(openAPIBytes(), &openAPIParsed); err != nil {
			openAPIParseError = err
		}
	})
	return openAPIParsed, openAPIParseError
}

// handleOpenAPI serves the contract. It is public and cache-friendly for a
// session's lifetime: the document describes the build, and a build does not
// change underneath a running process.
func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	raw := openAPIBytes()
	if openAPIParseError != nil {
		s.log.Error("the embedded OpenAPI document is not valid JSON", "error", openAPIParseError)
		s.writeCodeProblem(w, r, http.StatusInternalServerError, "contract-broken", "INTERNAL",
			"Internal Server Error", "the embedded API contract is unreadable in this build")
		return
	}
	w.Header().Set("Content-Type", ContentTypeJSON)
	w.Header().Set("Cache-Control", "private, max-age=60")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}
