package api

// A deliberately small JSON Schema validator for the subset the hand-written
// contract in internal/api/openapi/openapi.json actually uses (Milestone 1,
// ADR-0017, requirement 4c of docs/spec/08's contract test).
//
// Why hand-roll a validator instead of pulling in a JSON Schema library: the
// alternative is a new module dependency (AGENTS.md forbids adding modules to a
// task) and ~2 000 lines of spec — drafts 6/7/2020-12, vocabularies, format
// assertion — to check documents this package writes itself. The subset below is
// everything the embedded contract uses and nothing else, and the honesty of
// that claim is itself tested: TestOpenAPISchemaKeywordsAreAllUnderstood walks
// every schema position in the document and fails on a keyword this validator
// neither enforces nor declares as an unenforced annotation. A contract test
// that silently stopped checking because a keyword was new is worse than no
// contract test, because it reports success.
//
// Enforced: $ref (local pointers), type (including the null-union form
// ["string","null"]), required, properties, items, additionalProperties (false
// and schema forms), propertyNames, enum, const, allOf.
// Not enforced, by declaration: length/range/pattern/format annotations, which
// the handlers and the domain validators in internal/games enforce at write time
// — a response-shape test that re-derived maxLength would be testing the same
// rule twice with two different messages.

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/auth"
)

// enforcedKeywords are the keywords validateSchema implements.
var enforcedKeywords = map[string]bool{
	"$ref":                 true,
	"type":                 true,
	"required":             true,
	"properties":           true,
	"items":                true,
	"additionalProperties": true,
	"propertyNames":        true,
	"enum":                 true,
	"const":                true,
	"allOf":                true,
}

// annotatedKeywords are keywords the contract may use whose constraint is
// enforced somewhere other than here, with the reason for each. Anything not in
// either set fails the audit test rather than being quietly ignored.
var annotatedKeywords = map[string]string{
	"description":           "documentation only",
	"title":                 "documentation only",
	"summary":               "documentation only",
	"format":                "date-time and the like are produced by the writers in internal/games and internal/store, which emit time.RFC3339 and nothing else; re-checking the format here would test fmt, not the contract",
	"default":               "documentation for client generators; the handler applies the same default",
	"example":               "documentation only",
	"examples":              "documentation only",
	"deprecated":            "documentation only",
	"readOnly":              "documentation for generated clients; the routes refuse these fields with DisallowUnknownFields, which is the stronger rule",
	"writeOnly":             "documentation for generated clients",
	"minLength":             "enforced by games.Validate at write time",
	"maxLength":             "enforced by games.Validate at write time",
	"pattern":               "enforced by games.Validate / store CHECK constraints at write time",
	"minimum":               "enforced by games.Validate and the schema's CHECK constraints at write time",
	"maximum":               "enforced by games.Validate and the schema's CHECK constraints at write time",
	"exclusiveMinimum":      "enforced by games.Validate at write time",
	"exclusiveMaximum":      "enforced by games.Validate at write time",
	"minItems":              "enforced by games.Validate at write time",
	"maxItems":              "enforced by the bounded writers in internal/store at write time",
	"uniqueItems":           "enforced by the uniqueness of the Go map/insert path that builds the value",
	"contentEncoding":       "binary payloads (an upload part, a .gnpack) are not JSON and are never validated here",
	"unevaluatedProperties": "asserted true wherever a schema documents an open tail; an absent member is the constraint, so no check is owed",
	"anyOf":                 "used only for nullable members: the writer emits either the referenced object or JSON null (a nil Go pointer), never a partial object, so the branch the writer omits cannot appear on the wire",
}

// schemaValidator validates decoded JSON against schemas taken from one OpenAPI
// document, resolving local $ref pointers within it.
type schemaValidator struct {
	root map[string]any
}

func newSchemaValidator(root map[string]any) *schemaValidator {
	return &schemaValidator{root: root}
}

// resolveSchema follows a local $ref if present and returns the schema object to
// apply. Only `#/...` pointers are supported: an external reference would make
// the contract test depend on the network, and an embedded document has no
// reason to contain one.
func (v *schemaValidator) resolveSchema(s any) (map[string]any, error) {
	sch, ok := s.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("schema is %T, want an object", s)
	}
	ref, ok := sch["$ref"].(string)
	if !ok {
		return sch, nil
	}
	if !strings.HasPrefix(ref, "#/") {
		return nil, fmt.Errorf("$ref %q is not a local pointer", ref)
	}
	node := any(v.root)
	for _, seg := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		seg = strings.ReplaceAll(seg, "~1", "/")
		seg = strings.ReplaceAll(seg, "~0", "~")
		switch cur := node.(type) {
		case map[string]any:
			next, has := cur[seg]
			if !has {
				return nil, fmt.Errorf("$ref %q: no member %q", ref, seg)
			}
			node = next
		case []any:
			idx, err := strconv.Atoi(seg)
			if err != nil || idx < 0 || idx >= len(cur) {
				return nil, fmt.Errorf("$ref %q: index %q is out of range", ref, seg)
			}
			node = cur[idx]
		default:
			return nil, fmt.Errorf("$ref %q: cannot descend into %T", ref, node)
		}
	}
	target, ok := node.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("$ref %q resolves to %T, want an object", ref, node)
	}
	if _, nested := target["$ref"]; nested {
		return nil, fmt.Errorf("$ref %q resolves to another $ref: this validator does not follow chained pointers", ref)
	}
	return target, nil
}

// validate checks value against schema and returns every violation found, each
// prefixed by a JSONPath-ish location so a failure names the member instead of
// just the object. It deliberately does not stop at the first violation: the
// same reason the publish gate lists every finding (docs/spec/03).
func (v *schemaValidator) validate(schema any, value any, at string) []string {
	sch, err := v.resolveSchema(schema)
	if err != nil {
		return []string{at + ": " + err.Error()}
	}
	var errs []string
	addf := func(format string, args ...any) {
		errs = append(errs, fmt.Sprintf("%s: %s", at, fmt.Sprintf(format, args...)))
	}

	// An unread keyword must be reported, not skipped — this is the runtime twin
	// of TestOpenAPISchemaKeywordsAreAllUnderstood, and covers schemas a caller
	// builds inline rather than in the document.
	for _, kw := range sortedKeys(sch) {
		if !enforcedKeywords[kw] && annotatedKeywords[kw] == "" {
			addf("schema uses keyword %q, which this validator does not enforce and does not declare", kw)
		}
	}

	if t, ok := sch["type"]; ok && !schemaTypeMatches(t, value) {
		addf("is %s, schema requires %s", jsonKind(value), describeSchemaType(t))
		return errs // further checks would only add noise to a type mismatch
	}

	if enum, ok := sch["enum"]; ok {
		if !jsonValueIn(enum, value) {
			addf("value %s is not in enum %s", renderJSON(value), renderJSON(enum))
			return errs
		}
	}
	if c, ok := sch["const"]; ok && !jsonEqual(c, value) {
		addf("value %s does not equal const %s", renderJSON(value), renderJSON(c))
	}

	switch typed := value.(type) {
	case map[string]any:
		if req, ok := sch["required"].([]any); ok {
			for _, want := range req {
				name, _ := want.(string)
				if _, has := typed[name]; !has {
					addf("required member %q is absent", name)
				}
			}
		}
		props, _ := sch["properties"].(map[string]any)
		for _, name := range sortedKeys(typed) {
			sub, declared := props[name]
			if declared {
				errs = append(errs, v.validate(sub, typed[name], at+"."+name)...)
				continue
			}
			switch extra := sch["additionalProperties"].(type) {
			case bool:
				if !extra {
					addf("member %q is not declared and additionalProperties is false", name)
				}
			case map[string]any, string:
				errs = append(errs, v.validate(extra, typed[name], at+"."+name)...)
			}
		}
		if names, ok := sch["propertyNames"]; ok {
			for _, name := range sortedKeys(typed) {
				errs = append(errs, v.validate(names, name, at+"{name:"+name+"}")...)
			}
		}
	case []any:
		if items, ok := sch["items"]; ok {
			for i, el := range typed {
				errs = append(errs, v.validate(items, el, fmt.Sprintf("%s[%d]", at, i))...)
			}
		}
	}

	for i, part := range anySlice(sch["allOf"]) {
		errs = append(errs, v.validate(part, value, fmt.Sprintf("%s/allOf[%d]", at, i))...)
	}
	return errs
}

// schemaTypeMatches implements the type keyword, including the 3.1 null-union
// form. "integer" accepts an integral JSON number and rejects 1.5 — the
// distinction matters because Go encodes int and float64 to different text and a
// client typed as an integer would break on a float.
func schemaTypeMatches(t any, value any) bool {
	for _, name := range anySlice(t) {
		s, _ := name.(string)
		switch s {
		case "object":
			if _, ok := value.(map[string]any); ok {
				return true
			}
		case "array":
			if _, ok := value.([]any); ok {
				return true
			}
		case "string":
			if _, ok := value.(string); ok {
				return true
			}
		case "boolean":
			if _, ok := value.(bool); ok {
				return true
			}
		case "null":
			if value == nil {
				return true
			}
		case "number":
			if isJSONNumber(value) {
				return true
			}
		case "integer":
			f, ok := jsonNumber(value)
			if ok && !math.IsInf(f, 0) && f == math.Trunc(f) {
				return true
			}
		}
	}
	return false
}

func describeSchemaType(t any) string {
	if s, ok := t.(string); ok {
		return s
	}
	return renderJSON(t)
}

func jsonKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64, json.Number:
		return "number"
	}
	return fmt.Sprintf("%T", v)
}

func isJSONNumber(v any) bool { _, ok := jsonNumber(v); return ok }

func jsonNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func jsonValueIn(list any, value any) bool {
	for _, el := range anySlice(list) {
		if jsonEqual(el, value) {
			return true
		}
	}
	return false
}

func jsonEqual(a, b any) bool {
	if as, ok := a.(string); ok {
		bs, ok2 := b.(string)
		return ok2 && as == bs
	}
	if af, ok := jsonNumber(a); ok {
		bf, ok2 := jsonNumber(b)
		return ok2 && af == bf
	}
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return renderJSON(a) == renderJSON(b)
}

func renderJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	if len(raw) > 120 {
		return string(raw[:120]) + "…"
	}
	return string(raw)
}

func anySlice(v any) []any {
	switch s := v.(type) {
	case nil:
		return nil
	case []any:
		return s
	}
	return []any{v}
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestSchemaValidatorRejectsWhatItClaimsTo is what keeps the contract test from
// being theatre. A validator that accepted everything would make
// TestOpenAPIDocMatchesTheRouter's response clause pass forever, so each
// enforced keyword gets a passing and a failing case, and the failing cases
// assert they actually report the member they name.
func TestSchemaValidatorRejectsWhatItClaimsTo(t *testing.T) {
	root := map[string]any{"components": map[string]any{"schemas": map[string]any{
		"Thing": map[string]any{
			"type":     "object",
			"required": []any{"id", "count", "note", "tags", "kind"},
			"properties": map[string]any{
				"id":    map[string]any{"type": "string"},
				"count": map[string]any{"type": "integer"},
				"note":  map[string]any{"type": []any{"string", "null"}},
				"tags":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"kind":  map[string]any{"enum": []any{"a", "b"}},
			},
		},
		"Strict": map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"only": map[string]any{"type": "string"}},
			"additionalProperties": false,
		},
		"Names": map[string]any{
			"type":                 "object",
			"propertyNames":        map[string]any{"enum": []any{"x"}},
			"additionalProperties": map[string]any{"type": "string"},
		},
		"Extended": map[string]any{
			"allOf": []any{
				map[string]any{"$ref": "#/components/schemas/Thing"},
				map[string]any{"type": "object", "required": []any{"extra"}},
			},
		},
	}}}
	v := newSchemaValidator(root)
	thing := func() map[string]any {
		return map[string]any{"id": "a", "count": float64(2), "note": nil,
			"tags": []any{"t"}, "kind": "a"}
	}

	cases := []struct {
		name    string
		schema  any
		value   any
		wantErr string // "" = must pass; otherwise a substring of one violation
	}{
		{"valid", ref("#/components/schemas/Thing"), thing(), ""},
		{"missing required", ref("#/components/schemas/Thing"), without(thing(), "kind"), `required member "kind"`},
		{"wrong type", ref("#/components/schemas/Thing"), with(thing(), "id", float64(3)), "$.id: is number, schema requires string"},
		{"integer rejects a fraction", ref("#/components/schemas/Thing"), with(thing(), "count", 1.5), "$.count"},
		{"nullable union accepts null", ref("#/components/schemas/Thing"), with(thing(), "note", nil), ""},
		{"nullable union rejects a number", ref("#/components/schemas/Thing"), with(thing(), "note", float64(1)), "$.note"},
		{"items", ref("#/components/schemas/Thing"), with(thing(), "tags", []any{"ok", 3}), "$.tags[1]"},
		{"enum", ref("#/components/schemas/Thing"), with(thing(), "kind", "c"), "not in enum"},
		{"additionalProperties false", ref("#/components/schemas/Strict"),
			map[string]any{"only": "y", "more": 1}, "additionalProperties is false"},
		{"additionalProperties schema", ref("#/components/schemas/Names"),
			map[string]any{"x": 1}, "requires string"},
		{"propertyNames", ref("#/components/schemas/Names"),
			map[string]any{"nope": "y"}, "not in enum"},
		{"allOf adds the requirement", ref("#/components/schemas/Extended"), thing(), `required member "extra"`},
		{"allOf sees both halves", ref("#/components/schemas/Extended"),
			with(thing(), "count", "nope"), "allOf[0].count"},
		{"const", map[string]any{"type": "string", "const": "x"}, "y", "does not equal const"},
		{"unknown keyword is reported", map[string]any{"type": "string", "dependentRequired": map[string]any{}},
			"y", "does not enforce"},
		{"dangling ref", ref("#/components/schemas/Nope"), thing(), "no member"},
		{"external ref", map[string]any{"$ref": "https://example.test/x.json"}, thing(), "local pointer"},
		{"a string where an object is required", ref("#/components/schemas/Thing"), "not json", "requires object"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := v.validate(tc.schema, tc.value, "$")
			if tc.wantErr == "" {
				if len(got) != 0 {
					t.Fatalf("validate: unexpected violations: %v", got)
				}
				return
			}
			if len(got) == 0 {
				t.Fatalf("validate accepted an invalid value, want a violation containing %q", tc.wantErr)
			}
			for _, line := range got {
				if strings.Contains(line, tc.wantErr) {
					return
				}
			}
			t.Fatalf("violations %q do not contain %q", got, tc.wantErr)
		})
	}
}

func ref(pointer string) map[string]any { return map[string]any{"$ref": pointer} }

func with(m map[string]any, k string, v any) map[string]any {
	out := map[string]any{}
	for key, val := range m {
		out[key] = val
	}
	out[k] = v
	return out
}

func without(m map[string]any, k string) map[string]any {
	out := map[string]any{}
	for key, val := range m {
		if key != k {
			out[key] = val
		}
	}
	return out
}

// TestOpenAPISchemaKeywordsAreAllUnderstood is the audit that keeps the
// validator honest as the document grows: every keyword in a *schema position*
// of the embedded contract must be either enforced by validate or listed in
// annotatedKeywords with a reason. Schema positions are reached structurally
// (components/schemas/*, and the schema/items/properties/... children of a
// schema), so a property that happens to be named "type" is visited as the
// sub-schema it is and never mistaken for a keyword.
func TestOpenAPISchemaKeywordsAreAllUnderstood(t *testing.T) {
	doc := openapiFixture(t)
	unexpected := map[string]int{}
	check := func(sch map[string]any, where string) {
		for kw := range sch {
			if enforcedKeywords[kw] || annotatedKeywords[kw] != "" {
				continue
			}
			unexpected[where+"#"+kw]++
		}
	}
	walkSchemas(doc, check)

	if len(unexpected) > 0 {
		keys := make([]string, 0, len(unexpected))
		for k := range unexpected {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		for _, k := range keys {
			fmt.Fprintf(&b, "\n  %s (%d)", k, unexpected[k])
		}
		t.Fatalf("the contract uses keywords this validator understands neither way:%s\n"+
			"either enforce it in validate (and add a rejecting case to TestSchemaValidatorRejectsWhatItClaimsTo)"+
			" or list it in annotatedKeywords with where the constraint is enforced instead", b.String())
	}
}

// TestOpenAPIDescribesItsSecuritySchemes: docs/spec/09's generated client has to
// be able to authenticate from the document alone, so the cookie + CSRF pair and
// the bearer scheme are asserted by name rather than assumed.
func TestOpenAPIDescribesItsSecuritySchemes(t *testing.T) {
	doc := openapiFixture(t)
	schemes, _ := pathValue(doc, "components", "securitySchemes").(map[string]any)
	if len(schemes) != 3 {
		t.Fatalf("securitySchemes = %v, want gnSession, csrfToken and bearerToken", sortedKeys(schemes))
	}
	session, _ := schemes["gnSession"].(map[string]any)
	if session["type"] != "apiKey" || session["in"] != "cookie" || session["name"] != auth.CookieSession {
		t.Fatalf("gnSession = %v, want an apiKey cookie named %s", session, auth.CookieSession)
	}
	csrf, _ := schemes["csrfToken"].(map[string]any)
	if csrf["type"] != "apiKey" || csrf["in"] != "header" || csrf["name"] != HeaderCSRFToken {
		t.Fatalf("csrfToken = %v, want an apiKey header named %s", csrf, HeaderCSRFToken)
	}
	bearer, _ := schemes["bearerToken"].(map[string]any)
	if bearer["type"] != "http" || bearer["scheme"] != "bearer" {
		t.Fatalf("bearerToken = %v, want http/bearer", bearer)
	}
	if _, ok := doc["security"].([]any); !ok {
		t.Fatal("the document declares no top-level security requirement")
	}
}

// TestOpenAPIProblemDocumentShape checks the shared error schema against a real
// problem this package writes, because the shape is a promise to docs/spec/09's
// recoverable-action UI: type/title/status/detail/instance/request_id, plus the
// code member every handler in this milestone adds.
func TestOpenAPIProblemDocumentShape(t *testing.T) {
	doc := openapiFixture(t)
	v := newSchemaValidator(doc)
	problemSchema := map[string]any{"$ref": "#/components/schemas/Problem"}

	rec := doReq(t, newTestAuth(t).handler(), getReq("/api/v1/nothing-here"))
	var body map[string]any
	decode(t, rec, &body)
	if got := rec.Header().Get("Content-Type"); got != ContentTypeProblem {
		t.Fatalf("content-type = %q, want %s", got, ContentTypeProblem)
	}
	if errs := v.validate(problemSchema, body, "$.problem"); len(errs) != 0 {
		t.Fatalf("the router's 404 problem does not satisfy #/components/schemas/Problem: %v", errs)
	}
	if _, ok := body["code"]; ok {
		t.Fatal("the base problem document carries a code member; the code member belongs to the handlers of ADR-0017 and the contract says which")
	}
}

// TestOpenAPIIsServedPubliclyAndByteStably covers the serving contract: the
// document a client hashes is the document this binary was compiled with, twice.
func TestOpenAPIIsServedPubliclyAndByteStably(t *testing.T) {
	h := newTestAuth(t).handler()
	first := doReq(t, h, getReq(openAPIPath))
	if first.Code != http.StatusOK {
		t.Fatalf("GET %s = %d (unauthenticated), want 200: a client must be able to learn how to authenticate", openAPIPath, first.Code)
	}
	requireContentType(t, first, ContentTypeJSON)
	second := doReq(t, h, getReq(openAPIPath))
	if second.Body.String() != first.Body.String() {
		t.Fatal("the contract is not byte-stable across requests: a client cannot hash a document that changes")
	}
	var doc map[string]any
	decode(t, first, &doc)
	if doc["openapi"] != "3.1.0" {
		t.Fatalf("openapi = %v, want 3.1.0", doc["openapi"])
	}
	if _, err := json.Marshal(doc); err != nil {
		t.Fatalf("the served document does not re-encode: %v", err)
	}
}

// ── schema-position walker ─────────────────────────────────────────────────

// walkSchemas visits every schema object reachable from the two places schemas
// live in an OpenAPI document: components/schemas, and any request/response
// `schema` member under paths. Nested positions (properties, items, allOf,
// additionalProperties, propertyNames) are followed recursively.
func walkSchemas(doc map[string]any, visit func(sch map[string]any, where string)) {
	if schemas, ok := pathValue(doc, "components", "schemas").(map[string]any); ok {
		for name, raw := range schemas {
			if sch, ok := raw.(map[string]any); ok {
				walkOneSchema(sch, "#/components/schemas/"+name, visit)
			}
		}
	}
	paths, _ := pathValue(doc, "paths").(map[string]any)
	for tmpl, item := range paths {
		itemMap, _ := item.(map[string]any)
		for method, raw := range itemMap {
			op, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			where := method + " " + tmpl
			if body, ok := pathValue(op, "requestBody", "content").(map[string]any); ok {
				for media, mediaObj := range body {
					if sch, ok := pathValue(mediaObj, "schema").(map[string]any); ok {
						walkOneSchema(sch, where+" req "+media, visit)
					}
				}
			}
			resp, _ := pathValue(op, "responses").(map[string]any)
			for code, rawResp := range resp {
				respMap, ok := rawResp.(map[string]any)
				if !ok {
					continue
				}
				if content, ok := pathValue(respMap, "content").(map[string]any); ok {
					for media, mediaObj := range content {
						if sch, ok := pathValue(mediaObj, "schema").(map[string]any); ok {
							walkOneSchema(sch, where+" res "+code+" "+media, visit)
						}
					}
				}
				if headers, ok := pathValue(respMap, "headers").(map[string]any); ok {
					for name, rawHdr := range headers {
						if hdr, ok := rawHdr.(map[string]any); ok {
							if sch, ok := hdr["schema"].(map[string]any); ok {
								walkOneSchema(sch, where+" res "+code+" header "+name, visit)
							}
						}
					}
				}
			}
			for _, raw := range anySlice(op["parameters"]) {
				if p, ok := raw.(map[string]any); ok {
					if sch, ok := p["schema"].(map[string]any); ok {
						walkOneSchema(sch, where+" param "+fmt.Sprint(p["name"]), visit)
					}
				}
			}
		}
	}
	if responses, ok := pathValue(doc, "components", "responses").(map[string]any); ok {
		for name, raw := range responses {
			content, _ := pathValue(raw, "content").(map[string]any)
			for media, mediaObj := range content {
				if sch, ok := pathValue(mediaObj, "schema").(map[string]any); ok {
					walkOneSchema(sch, "#/components/responses/"+name+" "+media, visit)
				}
			}
		}
	}
}

// schemaChildKeys are the keywords whose values are themselves schemas. A schema
// under one of these is a schema position; a *property* of the same name is not.
var schemaChildKeys = []string{"items", "additionalProperties", "propertyNames", "not",
	"contains", "if", "then", "else"}

func walkOneSchema(sch map[string]any, where string, visit func(map[string]any, string)) {
	visit(sch, where)
	for _, kw := range schemaChildKeys {
		if sub, ok := sch[kw].(map[string]any); ok {
			walkOneSchema(sub, where+"/"+kw, visit)
		}
	}
	for _, kw := range []string{"allOf", "anyOf", "oneOf", "prefixItems"} {
		for i, raw := range anySlice(sch[kw]) {
			if sub, ok := raw.(map[string]any); ok {
				walkOneSchema(sub, fmt.Sprintf("%s/%s[%d]", where, kw, i), visit)
			}
		}
	}
	if props, ok := sch["properties"].(map[string]any); ok {
		for name, raw := range props {
			if sub, ok := raw.(map[string]any); ok {
				walkOneSchema(sub, where+".properties."+name, visit)
			}
		}
	}
}

func pathValue(v any, keys ...string) any {
	cur := v
	for _, k := range keys {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[k]
	}
	return cur
}
