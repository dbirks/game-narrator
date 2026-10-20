package api

// The OpenAPI contract test (requirement 4 of the Milestone 1 catalog API task,
// ADR-0017). Three clauses, all against the live router rather than a copy of
// it:
//
//  (a) every route registered by Handler() appears in the document with the same
//      method and path template, and every documented operation is registered;
//  (b) every operation's x-required-role equals the role the scope table
//      actually grants that route;
//  (c) every success body the handler tests produced validates against that
//      operation's response schema — see openapi_validate_test.go for the
//      validator and catalog_test.go for the flows whose bodies land here.

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/games"
)

// openapiFixture parses the embedded document. It is the bytes the handler
// serves, not a re-read of the file from disk: a test against the file would
// pass for a binary whose embed directive was broken.
func openapiFixture(t *testing.T) map[string]any {
	t.Helper()
	root, err := openAPIRoot()
	if err != nil {
		t.Fatalf("the embedded OpenAPI document is unreadable: %v", err)
	}
	if root == nil {
		t.Fatal("the embedded OpenAPI document is empty")
	}
	return root
}

// openapiOperation returns the documented operation for a *template* path.
func openapiOperation(t *testing.T, method, template string) (map[string]any, bool) {
	t.Helper()
	doc := openapiFixture(t)
	paths, _ := doc["paths"].(map[string]any)
	item, ok := paths[template].(map[string]any)
	if !ok {
		return nil, false
	}
	op, ok := item[strings.ToLower(method)].(map[string]any)
	return op, ok
}

// openapiOperations flattens the document to "METHOD /template" → operation.
func openapiOperations(t *testing.T) map[string]map[string]any {
	t.Helper()
	doc := openapiFixture(t)
	out := map[string]map[string]any{}
	paths, _ := doc["paths"].(map[string]any)
	for tmpl, raw := range paths {
		item, ok := raw.(map[string]any)
		if !ok {
			t.Errorf("path %q is not a path item object", tmpl)
			continue
		}
		for method, op := range item {
			if !isHTTPMethod(method) {
				continue // parameters, servers, summary — path-item fields
			}
			asMap, ok := op.(map[string]any)
			if !ok {
				t.Errorf("%s %s is not an operation object", strings.ToUpper(method), tmpl)
				continue
			}
			key := strings.ToUpper(method) + " " + tmpl
			if _, dup := out[key]; dup {
				t.Errorf("%s is documented twice", key)
			}
			out[key] = asMap
		}
	}
	return out
}

var httpMethods = map[string]bool{
	"connect": true, "delete": true, "get": true, "head": true, "options": true,
	"patch": true, "post": true, "put": true, "trace": true,
	"CONNECT": true, "DELETE": true, "GET": true, "HEAD": true, "OPTIONS": true,
	"PATCH": true, "POST": true, "PUT": true, "TRACE": true,
}

func isHTTPMethod(s string) bool { return httpMethods[s] }

// templateFor resolves a concrete request path onto its documented template, so
// a test can say "I just got a 200 from /api/v1/profiles/01H…/messages/01H…" and
// have that mean GET /api/v1/profiles/{id}/messages/{mid}. Segments named
// {anything} match one path segment; a literal must match exactly; a trailing
// {rest...} matches one or more.
func templateFor(t *testing.T, method, concrete string) (string, map[string]any, bool) {
	t.Helper()
	// The request target carries its query string; a path template never does.
	concrete, _, _ = strings.Cut(concrete, "?")
	want := splitPath(strings.TrimPrefix(concrete, "/"))

	// The most specific template wins: /profiles/{id}/messages/search and
	// /profiles/{id}/messages/{mid} both "match" the concrete path
	// /profiles/X/messages/search, and a map iteration order must not decide which
	// contract a response is checked against. Specificity here is the number of
	// parameters — a literal segment is always a stronger claim than a wildcard.
	var found string
	var op map[string]any
	best := -1
	for key, candidate := range openapiOperations(t) {
		m, tmpl, _ := strings.Cut(key, " ")
		if m != method {
			continue
		}
		if !templateMatches(tmpl, want) {
			continue
		}
		score := strings.Count(tmpl, "{")
		if best < 0 || score < best {
			found, op, best = tmpl, candidate, score
		}
	}
	if found == "" {
		return "", nil, false
	}
	return found, op, true
}

// TestTemplateResolutionPrefersALiteralSegment is the unit test for that tie-break:
// the two templates above really do both match, and the wrong pick would validate
// a search body against the single-message schema.
func TestTemplateResolutionPrefersALiteralSegment(t *testing.T) {
	e := newCatalogEnv(t)
	game := e.createGame(auth.RoleCurator, "tmpl", "Tmpl", "tmpl")
	edition := e.createEdition(auth.RoleCurator, game["id"].(string), "us", "en", "r1", "tmpl-ed")
	profile, _ := e.createDraft(auth.RoleCurator, edition["id"].(string))
	id := profile["id"].(string)

	for _, tc := range []struct{ method, concrete, want string }{
		{http.MethodGet, "/api/v1/profiles/" + id + "/messages/search?q=x", "/api/v1/profiles/{id}/messages/search"},
		{http.MethodGet, "/api/v1/profiles/" + id + "/messages/" + games.NewID(), "/api/v1/profiles/{id}/messages/{mid}"},
		{http.MethodGet, "/api/v1/profiles/" + id + "/messages", "/api/v1/profiles/{id}/messages"},
		{http.MethodGet, "/api/v1/profiles/" + id, "/api/v1/profiles/{id}"},
		{http.MethodGet, "/api/v1/games/" + games.NewID() + "/editions", "/api/v1/games/{id}/editions"},
		{http.MethodPost, "/api/v1/editions/" + games.NewID() + "/profiles/import-manifest", "/api/v1/editions/{id}/profiles/import-manifest"},
	} {
		got, _, ok := templateFor(t, tc.method, tc.concrete)
		if !ok || got != tc.want {
			t.Errorf("templateFor(%s %s) = %q (%v), want %q", tc.method, tc.concrete, got, ok, tc.want)
		}
	}
	if _, _, ok := templateFor(t, http.MethodGet, "/api/v1/unregistered/thing"); ok {
		t.Error("an undocumented path resolved to a template")
	}
}

func templateMatches(tmpl string, got []string) bool {
	segs := splitPath(strings.TrimPrefix(tmpl, "/"))
	for i, seg := range segs {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			inner := seg[1 : len(seg)-1]
			if strings.HasSuffix(inner, "...") {
				return i < len(got) // greedy: consumes the remainder
			}
			if i >= len(got) {
				return false
			}
			continue
		}
		if i >= len(got) || seg != got[i] {
			return false
		}
	}
	return len(segs) == len(got)
}

func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// ── (a) the route table and the document are the same set ──────────────────

// TestOpenAPIDocMatchesTheRouter is the drift detector: a handler registered
// without being documented is an invisible endpoint, and a documented operation
// with no handler is a client that 404s on a call the contract promised. Both
// directions fail here, in the same test, because they are the same mistake seen
// from opposite ends (ADR-0017 chose hand-written + tested precisely so this
// check is possible at all).
func TestOpenAPIDocMatchesTheRouter(t *testing.T) {
	a := newTestAuth(t)
	s := New(testBuild, testStarted, discardLogger, WithWebAssets(uiBuild()), WithAuth(a.svc, a.repo))
	s.Handler()

	// HEAD is ServeMux's freebie on a GET route and is not a documented
	// operation: the document describes a response body, and a HEAD has none.
	// The router registers no HEAD pattern of its own, so no filtering is needed
	// here — this comment exists so nobody adds one.
	registered := map[string]bool{}
	for _, rt := range s.routes {
		registered[rt.method+" "+rt.path] = true
	}
	if len(registered) == 0 {
		t.Fatal("no routes were recorded, so this check would pass vacuously")
	}

	documented := openapiOperations(t)
	for key := range registered {
		if _, ok := documented[key]; !ok {
			t.Errorf("route %s is registered but not documented in openapi.json", key)
		}
	}
	for key := range documented {
		if !registered[key] {
			t.Errorf("operation %s is documented in openapi.json but no handler is registered for it", key)
		}
	}

	// Path templates in the document must be the router's own patterns, spelled
	// the same way, so the UI can build a URL from a template string.
	for _, rt := range s.routes {
		if !strings.HasPrefix(rt.path, apiRoot) {
			t.Errorf("route %s %s is outside %s", rt.method, rt.path, apiRoot)
		}
	}
}

// ── (b) documented roles are the granted roles ─────────────────────────────

// TestOpenAPIRoleGrantsMatchTheScopeTable: x-required-role is what a generated
// client uses to decide whether to even try a call, and what docs/spec/09's UI
// uses to hide a button. If it disagrees with auth.Policy, the document is
// lying about the access control, which is worse than having none.
func TestOpenAPIRoleGrantsMatchTheScopeTable(t *testing.T) {
	a := newTestAuth(t)
	s := New(testBuild, testStarted, discardLogger, WithWebAssets(uiBuild()), WithAuth(a.svc, a.repo))
	s.Handler()

	for _, rt := range s.routes {
		op, ok := openapiOperation(t, rt.method, rt.path)
		if !ok {
			continue // TestOpenAPIDocMatchesTheRouter already reported it
		}
		access, known := s.policy.Require(rt.method, rt.path)
		if !known {
			t.Errorf("%s %s: the scope table does not know this pattern", rt.method, rt.path)
			continue
		}
		want := "public"
		if !access.IsPublic() {
			want = access.Min.String()
		}
		got, ok := op["x-required-role"].(string)
		if !ok {
			t.Errorf("%s %s: the operation carries no x-required-role string", rt.method, rt.path)
			continue
		}
		if got != want {
			t.Errorf("%s %s: x-required-role = %q, the scope table grants %q", rt.method, rt.path, got, want)
		}
		// The vocabulary is closed: a typo ("curator " or "write") would make the
		// document describe a role that cannot exist.
		if got != "public" {
			if _, err := auth.ParseRole(got); err != nil {
				t.Errorf("%s %s: x-required-role %q is not a role: %v", rt.method, rt.path, got, err)
			}
		}
	}
}

// TestOpenAPIOperationsHaveResponsesAndIDs is the structural half of the
// contract: an operation with no success response cannot be validated by clause
// (c), and an operation without an operationId cannot be turned into a named
// generated client method (docs/spec/09).
func TestOpenAPIOperationsHaveResponsesAndIDs(t *testing.T) {
	seenID := map[string]string{}
	for key, op := range openapiOperations(t) {
		id, _ := op["operationId"].(string)
		if id == "" {
			t.Errorf("%s has no operationId", key)
		} else if other, dup := seenID[id]; dup {
			t.Errorf("%s and %s share operationId %q: a generated client would collide", key, other, id)
		} else {
			seenID[id] = key
		}
		responses, _ := op["responses"].(map[string]any)
		if len(responses) == 0 {
			t.Errorf("%s documents no responses", key)
			continue
		}
		if _, ok := responses["default"]; !ok {
			t.Errorf("%s documents no default response: docs/spec/08's problem+json is the answer to every failure", key)
		}
		hasSuccess := false
		for code := range responses {
			if strings.HasPrefix(code, "2") || strings.EqualFold(code, "default") {
				hasSuccess = true
			}
		}
		if !hasSuccess {
			t.Errorf("%s documents no 2xx or default response", key)
		}
	}
}

// TestOpenAPIErrorResponsesAreProblemJSON checks that every documented failure
// names the shared Problem schema, which is what lets a client branch on `code`
// instead of parsing prose (docs/spec/08 "Error contract examples"). The one
// carve-out is stated as a set rather than a comment: readiness's 503 is a
// *state*, and a load balancer must see the same body for ready and not-ready.
var nonProblemErrors = map[string]map[string]bool{
	"GET /api/v1/health/ready": {"503": true},
	// The tone that the device refused answers with its own body (the tone's
	// constants, the sink and device it was aimed at, played=false and the
	// failure note): a problem document would lose the evidence of where the
	// tone went (ADR-0074).
	"POST /api/v1/audio/test-tone": {"502": true},
}

func TestOpenAPIErrorResponsesAreProblemJSON(t *testing.T) {
	for key, op := range openapiOperations(t) {
		responses, _ := op["responses"].(map[string]any)
		for code, raw := range responses {
			if strings.HasPrefix(code, "2") {
				continue
			}
			body, ok := resolveDocumentedResponse(t, raw)
			if !ok {
				t.Errorf("%s response %s: cannot read the response object", key, code)
				continue
			}
			content, _ := body["content"].(map[string]any)
			if nonProblemErrors[key][code] {
				if _, hasProblem := content[ContentTypeProblem]; hasProblem {
					t.Errorf("%s response %s is the carved-out readiness body but now also documents %s", key, code, ContentTypeProblem)
				}
				if len(content) == 0 {
					t.Errorf("%s response %s documents no content at all", key, code)
				}
				continue
			}
			if _, ok := content[ContentTypeProblem].(map[string]any); !ok {
				t.Errorf("%s response %s does not document %s content", key, code, ContentTypeProblem)
				continue
			}
			if !mentionsProblem(content[ContentTypeProblem]) {
				t.Errorf("%s response %s documents a problem body that is not #/components/schemas/Problem", key, code)
			}
		}
		if _, ok := responses["default"]; !ok {
			t.Errorf("%s documents no default response", key)
		}
	}
}

// resolveDocumentedResponse follows a $ref to components/responses, which is how
// the document states the shared problem shape once instead of 40 times.
func resolveDocumentedResponse(t *testing.T, raw any) (map[string]any, bool) {
	t.Helper()
	doc := openapiFixture(t)
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, false
	}
	if ref, isRef := obj["$ref"].(string); isRef {
		resolved := pathValue(doc, splitPath(strings.TrimPrefix(ref, "#/"))...)
		obj, ok = resolved.(map[string]any)
		if !ok {
			t.Fatalf("$ref %q does not resolve to an object", ref)
		}
	}
	return obj, true
}

func mentionsProblem(mediaObj any) bool {
	raw, err := json.Marshal(mediaObj)
	return err == nil && strings.Contains(string(raw), "#/components/schemas/Problem")
}

// ── (c) bodies the handlers actually wrote ─────────────────────────────────

// contractHit records one validated response for an operation.
type contractHit struct {
	op     string
	status int
}

var (
	contractMu   sync.Mutex
	contractHits []contractHit
)

// recordContractResponse validates one response body against its documented
// schema and files the hit for the coverage test. It is called from the catalog
// test harness, so every 2xx the API produced in this package's tests is checked,
// including the ones a test only glanced at.
//
// Non-JSON bodies (a .gnpack download) are recorded by media type and skipped:
// the contract's job is the JSON shape, and the tar's integrity is profileio's
// tested property.
func recordContractResponse(t *testing.T, method, target string, status int, contentType string, body []byte) {
	t.Helper()
	opKey, op, ok := templateFor(t, method, target)
	if !ok {
		t.Fatalf("%s %s answered %d but no operation documents that path template", method, target, status)
	}
	responses, _ := op["responses"].(map[string]any)
	code := strconv.Itoa(status)
	raw, documented := responses[code]
	if !documented {
		// A 2xx the document never mentions is exactly the drift clause (a)
		// exists for, phrased at the response level.
		var codes []string
		for c := range responses {
			codes = append(codes, c)
		}
		sort.Strings(codes)
		t.Fatalf("%s %s answered %d, which operation %s does not document (documented: %s)",
			method, target, status, opKey, strings.Join(codes, ", "))
	}
	resp, ok := resolveDocumentedResponse(t, raw)
	if !ok {
		t.Fatalf("%s %s: response %s is unreadable", method, target, code)
	}
	content, _ := resp["content"].(map[string]any)
	media, _, _ := strings.Cut(contentType, ";")
	media = strings.TrimSpace(media)
	if media == "" {
		return
	}
	schemaObj, ok := content[media].(map[string]any)
	if !ok {
		t.Fatalf("%s %s answered with %s, which operation %s response %s does not document",
			method, target, media, opKey, code)
	}
	schema, ok := schemaObj["schema"].(map[string]any)
	if !ok {
		t.Fatalf("%s %s response %s documents %s content with no schema", method, target, code, media)
	}
	if media != ContentTypeJSON {
		return // binary payloads: recorded, not validated
	}
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatalf("%s %s answered %d with a body that is not JSON: %v", method, target, status, err)
	}
	if errs := newSchemaValidator(openapiFixture(t)).validate(schema, value, "$"); len(errs) > 0 {
		t.Fatalf("%s %s answered %d with a body that violates the contract for %s response %s:\n  %s",
			method, target, status, opKey, code, strings.Join(errs, "\n  "))
	}
	contractMu.Lock()
	contractHits = append(contractHits, contractHit{op: method + " " + opKey, status: status})
	contractMu.Unlock()
}
