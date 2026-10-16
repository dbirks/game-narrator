package profileio

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// The manifest schema, embedded so an appliance binary can validate an import
// without a filesystem dependency (docs/spec/16 M1: "JSON-schema and OpenAPI").
//
//go:embed schema/gnprofile-1.0.schema.json
var schemaSource []byte

// SchemaJSON returns the embedded JSON Schema (draft 2020-12) for
// `.gnprofile.json` — the same bytes the validator compiled. Exported for the
// API's schema route and for the parity test that keeps its vocabularies equal
// to the Go enums.
func SchemaJSON() []byte { return schemaSource }

// maxSchemaErrors bounds one validation report. A document wrong in a hundred
// places has one interesting finding per kind of wrongness, and a hundred-
// kilobyte error string is not an error message (docs/spec/13 "bounded").
const maxSchemaErrors = 20

// ValidateDocument checks a raw manifest document against the embedded schema and
// reports every failure it found, each naming the JSON pointer of the value at
// fault.
//
// This validator implements the subset the shipped schema uses — type (including
// unions), enum, const, pattern, required, properties, items,
// additionalProperties, and local $ref — rather than taking a JSON Schema
// dependency: ADR-0008 makes a new module a decision, and a full framework would
// be agreeing to semantics this document never exercises. The tradeoff is stated
// rather than hidden: this is not a general JSON Schema implementation, and a
// schema edited to use any other keyword fails to *compile*, so the failure
// direction — quietly accepting a document the schema forbids — is closed at
// build time rather than at import time.
func ValidateDocument(doc []byte) error {
	root, err := compileSchema(schemaSource)
	if err != nil {
		return fmt.Errorf("profileio: embedded schema is unusable: %w", err)
	}
	var value any
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.UseNumber()
	if err := dec.Decode(&value); err != nil {
		return fmt.Errorf("%w: document is not valid JSON: %v", ErrSchemaValidation, err)
	}
	if err := dec.Decode(new(any)); err == nil {
		return fmt.Errorf("%w: document holds more than one JSON value", ErrSchemaValidation)
	}

	var problems []string
	// "#" is the root of a JSON Pointer fragment, so every reported location
	// reads as a pointer an importer can act on: #/messages/0/kind.
	root.validate(value, "#", &problems)
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	shown, tail := problems, ""
	if len(problems) > maxSchemaErrors {
		shown, tail = problems[:maxSchemaErrors], fmt.Sprintf(" (and %d more)", len(problems)-maxSchemaErrors)
	}
	return fmt.Errorf("%w: %s%s", ErrSchemaValidation, strings.Join(shown, "; "), tail)
}

//
// ── schema compilation ──────────────────────────────────────────────────────
//

// schemaNode is one compiled subschema.
type schemaNode struct {
	types      []string // nil means "any type"
	enum       []any    // permitted literals
	constant   any      // the one permitted literal
	hasConst   bool
	pattern    *regexp.Regexp // strings only
	required   []string
	properties map[string]*schemaNode
	item       *schemaNode // "items" as a schema
	additional *schemaNode // schema for unlisted properties
	closed     bool        // additionalProperties: false
	ref        *schemaNode // resolved $ref
}

// knownKeywords is the whole vocabulary the shipped schema may use. Anything else
// is a compile error rather than an ignored key: skipping `minLength` would make
// the validator accept documents the schema forbids.
var knownKeywords = map[string]bool{
	"$schema": true, "$id": true, "$defs": true, "$ref": true,
	"title": true, "description": true, "$comment": true,
	"type": true, "enum": true, "const": true, "pattern": true,
	"required": true, "properties": true, "items": true, "additionalProperties": true,
}

// annotationKeywords carry no assertion, so they are allowed anywhere, including
// beside a $ref where draft 2020-12 permits them.
var annotationKeywords = map[string]bool{"title": true, "description": true, "$comment": true}

// compiler walks one schema document, resolving local pointers as it goes.
type compiler struct {
	doc   map[string]any
	seen  map[string]bool // $refs on the current stack, against recursion
	depth int
}

// maxSchemaDepth bounds pointer nesting in a schema document. The shipped schema
// is three levels deep; the limit exists so a hand-edited schema cannot be a stack
// overflow (the same reasoning as games.MaxJSONDepth).
const maxSchemaDepth = 32

func compileSchema(raw []byte) (*schemaNode, error) {
	var doc map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("schema document: %w", err)
	}
	c := &compiler{doc: doc, seen: map[string]bool{}}
	return c.compile(doc)
}

func (c *compiler) compile(node map[string]any) (*schemaNode, error) {
	c.depth++
	defer func() { c.depth-- }()
	if c.depth > maxSchemaDepth {
		return nil, fmt.Errorf("schema nests more than %d levels deep", maxSchemaDepth)
	}
	for key := range node {
		if !knownKeywords[key] {
			return nil, fmt.Errorf("unsupported keyword %q: this validator implements type, enum, const, pattern, required, properties, items, additionalProperties and local $ref", key)
		}
	}
	out := &schemaNode{}

	if ref, ok := node["$ref"].(string); ok {
		for key := range node {
			// Draft 2020-12 lets a $ref object carry annotation siblings; anything
			// with assertion power next to a $ref would be silently dropped, which
			// is a validator that accepts more than the schema says.
			if key != "$ref" && !annotationKeywords[key] {
				return nil, fmt.Errorf("keyword %q alongside $ref %q is not applied by this validator", key, ref)
			}
		}
		target, err := c.resolveRef(ref)
		if err != nil {
			return nil, err
		}
		out.ref = target
		return out, nil
	}

	if t, ok := node["type"]; ok {
		switch v := t.(type) {
		case string:
			out.types = []string{v}
		case []any:
			for _, item := range v {
				name, ok := item.(string)
				if !ok {
					return nil, fmt.Errorf("\"type\" array holds %v, not a type name", item)
				}
				out.types = append(out.types, name)
			}
		default:
			return nil, fmt.Errorf("\"type\" is %T, not a string or an array of strings", t)
		}
	}
	if e, ok := node["enum"]; ok {
		list, ok := e.([]any)
		if !ok {
			return nil, fmt.Errorf("\"enum\" is %T, not an array", e)
		}
		out.enum = list
	}
	if cValue, ok := node["const"]; ok {
		out.constant, out.hasConst = cValue, true
	}
	if p, ok := node["pattern"].(string); ok {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("pattern %q: %w", p, err)
		}
		out.pattern = re
	}
	if req, ok := node["required"]; ok {
		list, ok := req.([]any)
		if !ok {
			return nil, fmt.Errorf("\"required\" is %T, not an array", req)
		}
		for _, r := range list {
			name, ok := r.(string)
			if !ok {
				return nil, fmt.Errorf("\"required\" holds %v, not a property name", r)
			}
			out.required = append(out.required, name)
		}
	}
	if props, ok := node["properties"]; ok {
		object, ok := props.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("\"properties\" is %T, not an object", props)
		}
		out.properties = make(map[string]*schemaNode, len(object))
		for name, sub := range object {
			compiled, err := c.subschema(name, sub)
			if err != nil {
				return nil, fmt.Errorf("properties.%s: %w", name, err)
			}
			out.properties[name] = compiled
		}
	}
	if items, ok := node["items"]; ok {
		compiled, err := c.subschema("items", items)
		if err != nil {
			return nil, err
		}
		out.item = compiled
	}
	switch extra := node["additionalProperties"].(type) {
	case bool:
		out.closed = !extra
	case map[string]any:
		compiled, err := c.compile(extra)
		if err != nil {
			return nil, fmt.Errorf("additionalProperties: %w", err)
		}
		out.additional = compiled
	case nil:
	default:
		return nil, fmt.Errorf("additionalProperties is %T, not a boolean or a schema", extra)
	}
	return out, nil
}

func (c *compiler) subschema(where string, node any) (*schemaNode, error) {
	object, ok := node.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s is %T, not a schema object", where, node)
	}
	compiled, err := c.compile(object)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", where, err)
	}
	return compiled, nil
}

// resolveRef resolves one local pointer (`#/$defs/sha256`, `#/properties/edition`).
// Only `#/…` is supported: an external ref would turn schema validation into a
// network operation, which docs/spec/14 forbids for an offline appliance and
// which would make "does this import validate" depend on somebody else's uptime.
func (c *compiler) resolveRef(ref string) (*schemaNode, error) {
	if !strings.HasPrefix(ref, "#/") {
		return nil, fmt.Errorf("$ref %q: only local pointers beginning \"#/\" are supported", ref)
	}
	if c.seen[ref] {
		return nil, fmt.Errorf("$ref %q: recursive reference", ref)
	}
	var current any = c.doc
	for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("$ref %q: path leaves an object", ref)
		}
		current, ok = object[unescapePointer(part)]
		if !ok {
			return nil, fmt.Errorf("$ref %q: no such location", ref)
		}
	}
	target, ok := current.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("$ref %q does not name a schema object", ref)
	}
	c.seen[ref] = true
	defer delete(c.seen, ref)
	return c.compile(target)
}

func unescapePointer(part string) string {
	part = strings.ReplaceAll(part, "~1", "/")
	return strings.ReplaceAll(part, "~0", "~")
}

func escapePointer(name string) string {
	name = strings.ReplaceAll(name, "~", "~0")
	return strings.ReplaceAll(name, "/", "~1")
}

//
// ── validation ──────────────────────────────────────────────────────────────
//

// validate appends failures, each naming the JSON pointer of the value at fault.
// It descends into every branch rather than stopping at the first: an importer
// report that also says "and your seventh message has no kind" is worth more than
// one that reports one defect per attempt.
func (s *schemaNode) validate(v any, ptr string, problems *[]string) {
	if s == nil {
		return
	}
	if s.ref != nil {
		s.ref.validate(v, ptr, problems)
		return
	}
	if s.types != nil && !typeMatches(v, s.types) {
		*problems = append(*problems, fmt.Sprintf("%s: expected %s, found %s", pointerText(ptr), strings.Join(s.types, "/"), jsonKind(v)))
		return
	}
	if s.enum != nil && !inList(v, s.enum) {
		*problems = append(*problems, fmt.Sprintf("%s: %s is not one of %s", pointerText(ptr), render(v), renderList(s.enum)))
		return
	}
	if s.hasConst && !equalJSON(v, s.constant) {
		*problems = append(*problems, fmt.Sprintf("%s: expected %s, found %s", pointerText(ptr), render(s.constant), render(v)))
		return
	}

	switch value := v.(type) {
	case string:
		if s.pattern != nil && !s.pattern.MatchString(value) {
			*problems = append(*problems, fmt.Sprintf("%s: %q does not match %s", pointerText(ptr), clip(value), s.pattern))
		}
	case map[string]any:
		for _, name := range s.required {
			if _, ok := value[name]; !ok {
				*problems = append(*problems, fmt.Sprintf("%s: missing required property %q", pointerText(ptr), name))
			}
		}
		names := make([]string, 0, len(value))
		for name := range value {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			at := ptr + "/" + escapePointer(name)
			sub, ok := s.properties[name]
			if !ok {
				switch {
				case s.closed:
					*problems = append(*problems, fmt.Sprintf("%s: unknown property %q (this document shape is closed)", pointerText(at), name))
				case s.additional != nil:
					s.additional.validate(value[name], at, problems)
				}
				continue
			}
			sub.validate(value[name], at, problems)
		}
	case []any:
		if s.item != nil {
			for i, item := range value {
				s.item.validate(item, fmt.Sprintf("%s/%d", ptr, i), problems)
			}
		}
	}
}

func typeMatches(v any, types []string) bool {
	got := jsonKind(v)
	for _, t := range types {
		switch {
		case t == got:
			return true
		case t == "number" && got == "integer":
			// JSON Schema counts an integer as a number.
			return true
		case t == "integer" && got == "number":
			if n, ok := v.(json.Number); ok {
				if _, err := n.Int64(); err == nil {
					return true
				}
			}
		}
	}
	return false
}

// jsonKind names a decoded value in JSON's own vocabulary.
func jsonKind(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case json.Number:
		if _, err := t.Int64(); err == nil {
			return "integer"
		}
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return fmt.Sprintf("%T", v)
}

func inList(v any, list []any) bool {
	for _, item := range list {
		if equalJSON(v, item) {
			return true
		}
	}
	return false
}

func equalJSON(a, b any) bool {
	switch av := a.(type) {
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case json.Number:
		bv, ok := b.(json.Number)
		return ok && av.String() == bv.String()
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case nil:
		return b == nil
	}
	return false
}

func render(v any) string {
	switch t := v.(type) {
	case string:
		return fmt.Sprintf("%q", clip(t))
	case json.Number:
		return t.String()
	case nil:
		return "null"
	default:
		return fmt.Sprintf("%v", t)
	}
}

func renderList(list []any) string {
	parts := make([]string, 0, len(list))
	for _, item := range list {
		parts = append(parts, render(item))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func pointerText(ptr string) string {
	if ptr == "" || ptr == "#" {
		return "#/"
	}
	return ptr
}

func clip(s string) string {
	if len(s) > 40 {
		return s[:40]
	}
	return s
}
