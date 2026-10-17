package agent

// A small JSON-Schema validator for the subset the tool registry uses
// (docs/spec/14: "Tool executor verifies allowlist, args schema, role and
// cost before action"). The supported keywords are:
//
//	type        object | string | number | integer | boolean | array
//	required    [names]            (objects only)
//	properties  {name: schema}     (objects only, recursive)
//	items       schema             (arrays only, recursive)
//	enum        [values]           (any type)
//	maxLength   int                (strings; Unicode code points)
//	maxItems    int                (arrays)
//
// Anything outside the subset is not a failure: unknown keywords are ignored
// (a gateway-declared schema with additionalProperties etc. still validates
// the fields we understand), and a missing type means "any type". This keeps
// the validator total over the declared subset and honest about the rest,
// instead of pretending to be a full JSON-Schema engine.

import (
	"encoding/json"
	"fmt"
	"reflect"
	"unicode/utf8"
)

type schemaDoc struct {
	Type       string                     `json:"type"`
	Required   []string                   `json:"required"`
	Properties map[string]json.RawMessage `json:"properties"`
	Items      json.RawMessage            `json:"items"`
	Enum       []json.RawMessage          `json:"enum"`
	MaxItems   *int                       `json:"maxItems"`
	MaxLength  *int                       `json:"maxLength"`
}

var schemaTypes = map[string]bool{
	"object": true, "string": true, "number": true,
	"integer": true, "boolean": true, "array": true,
}

// ValidateSchema checks a schema document at registration time. It must be a
// JSON object, and every supported keyword it carries must be well-formed.
func ValidateSchema(raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return fmt.Errorf("schema is empty")
	}
	var doc schemaDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("schema is not a JSON object: %v", err)
	}
	if doc.Type != "" && !schemaTypes[doc.Type] {
		return fmt.Errorf("schema type %q is not one of %v", doc.Type, sortedTypes())
	}
	for _, r := range doc.Required {
		if r == "" {
			return fmt.Errorf("schema required list contains an empty name")
		}
	}
	for name, p := range doc.Properties {
		if !json.Valid(p) {
			return fmt.Errorf("schema property %q is not valid JSON", name)
		}
		var obj map[string]any
		if err := json.Unmarshal(p, &obj); err != nil {
			return fmt.Errorf("schema property %q must be a JSON object", name)
		}
	}
	if doc.Items != nil {
		var obj map[string]any
		if err := json.Unmarshal(doc.Items, &obj); err != nil {
			return fmt.Errorf("schema items must be a JSON object")
		}
	}
	for i, e := range doc.Enum {
		if !json.Valid(e) {
			return fmt.Errorf("schema enum entry %d is not valid JSON", i)
		}
	}
	return nil
}

// ValidateArgs checks one arguments value against the schema. An empty schema
// accepts anything; empty arguments are treated as the empty object.
func ValidateArgs(schema, args json.RawMessage) error {
	if len(schema) == 0 || string(schema) == "null" {
		return nil
	}
	var doc schemaDoc
	if err := json.Unmarshal(schema, &doc); err != nil {
		// Registration already rejected this; a hand-built call can still
		// reach here with a bad schema, and the executor treats it as a
		// refusal rather than a crash.
		return fmt.Errorf("input schema is malformed")
	}
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	}
	var v any
	if err := json.Unmarshal(args, &v); err != nil {
		return fmt.Errorf("arguments are not valid JSON: %v", err)
	}
	return validateValue(doc, v, "$")
}

func validateValue(doc schemaDoc, v any, path string) error {
	// enum beats everything: it pins the exact accepted set.
	if len(doc.Enum) > 0 {
		for _, e := range doc.Enum {
			var ev any
			if err := json.Unmarshal(e, &ev); err != nil {
				continue
			}
			if reflect.DeepEqual(v, ev) {
				return nil
			}
		}
		return fmt.Errorf("%s: value %v is not one of the allowed enum values", path, clip(v))
	}
	switch doc.Type {
	case "":
		// No type pinned: recurse into whatever shape is declared.
		return shapeOnly(doc, v, path)
	case "object":
		obj, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: expected object, got %s", path, goType(v))
		}
		for _, r := range doc.Required {
			if _, ok := obj[r]; !ok {
				return fmt.Errorf("%s: missing required field %q", path, r)
			}
		}
		for name, pschema := range doc.Properties {
			fv, exists := obj[name]
			if !exists {
				continue
			}
			var pdoc schemaDoc
			if err := json.Unmarshal(pschema, &pdoc); err != nil {
				return fmt.Errorf("%s.%s: property schema malformed", path, name)
			}
			if err := validateValue(pdoc, fv, path+"."+name); err != nil {
				return err
			}
		}
		return shapeOnly(doc, v, path)
	case "string":
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("%s: expected string, got %s", path, goType(v))
		}
		if doc.MaxLength != nil && utf8.RuneCountInString(s) > *doc.MaxLength {
			return fmt.Errorf("%s: string is %d chars, maxLength is %d", path, utf8.RuneCountInString(s), *doc.MaxLength)
		}
		return nil
	case "number":
		if _, ok := v.(float64); !ok {
			return fmt.Errorf("%s: expected number, got %s", path, goType(v))
		}
		return nil
	case "integer":
		f, ok := v.(float64)
		if !ok || f != float64(int64(f)) {
			return fmt.Errorf("%s: expected integer, got %v", path, v)
		}
		return nil
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("%s: expected boolean, got %s", path, goType(v))
		}
		return nil
	case "array":
		arr, ok := v.([]any)
		if !ok {
			return fmt.Errorf("%s: expected array, got %s", path, goType(v))
		}
		if doc.MaxItems != nil && len(arr) > *doc.MaxItems {
			return fmt.Errorf("%s: array has %d items, maxItems is %d", path, len(arr), *doc.MaxItems)
		}
		if doc.Items != nil {
			var idoc schemaDoc
			if err := json.Unmarshal(doc.Items, &idoc); err != nil {
				return fmt.Errorf("%s: items schema malformed", path)
			}
			for i, item := range arr {
				if err := validateValue(idoc, item, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
		return nil
	default:
		// Unknown type keyword: nothing to check (subset boundary).
		return nil
	}
}

// shapeOnly checks constraints that do not depend on the type keyword
// (maxLength on a string-typed value, maxItems on an array-typed value) when
// the caller declared constraints without a type.
func shapeOnly(doc schemaDoc, v any, path string) error {
	if doc.MaxLength != nil {
		if s, ok := v.(string); ok && utf8.RuneCountInString(s) > *doc.MaxLength {
			return fmt.Errorf("%s: string is %d chars, maxLength is %d", path, utf8.RuneCountInString(s), *doc.MaxLength)
		}
	}
	if doc.MaxItems != nil {
		if arr, ok := v.([]any); ok && len(arr) > *doc.MaxItems {
			return fmt.Errorf("%s: array has %d items, maxItems is %d", path, len(arr), *doc.MaxItems)
		}
	}
	return nil
}

func goType(v any) string {
	switch v.(type) {
	case map[string]any:
		return "object"
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	case []any:
		return "array"
	case nil:
		return "null"
	default:
		return "value"
	}
}

func clip(v any) any {
	if s, ok := v.(string); ok && len(s) > 40 {
		return s[:40] + "…"
	}
	return v
}

func sortedTypes() []string {
	return []string{"array", "boolean", "integer", "number", "object", "string"}
}
