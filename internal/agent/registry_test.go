package agent

// Registry and schema-validator tests. The reserved-name cases pin the
// injection defence of docs/spec/03 scenario 3 and docs/spec/14 "Prompt
// injection": the registry can never grow a host-execution tool, because the
// names an injection would target are refused at registration.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/auth"
)

func TestRegistryRefusesReservedNames(t *testing.T) {
	for _, name := range []string{"shell", "exec", "bash", "run_command", "system", "spawn", "subprocess"} {
		reg := NewRegistry()
		err := reg.Register(Tool{
			Name:        name,
			Description: "nope",
			InputSchema: json.RawMessage(`{"type":"object"}`),
			Handler:     func(ctx context.Context, c Call) (Result, error) { return Result{}, nil },
		})
		if err == nil {
			t.Errorf("Register(%q) succeeded, want refusal", name)
		} else if !strings.Contains(err.Error(), "reserved") {
			t.Errorf("Register(%q) = %v, want a reserved-name refusal", name, err)
		}
	}
	// And the definitions offered to the model stay empty.
	reg := NewRegistry()
	if got := len(reg.Definitions()); got != 0 {
		t.Fatalf("definitions = %d, want 0", got)
	}
}

func TestRegistryRejectsBadRegistrations(t *testing.T) {
	cases := []struct {
		name string
		tool Tool
		want string
	}{
		{"empty name", Tool{Name: "", Handler: noopHandler}, "name is empty"},
		{"bad charset", Tool{Name: "Echo-Tool", Handler: noopHandler, InputSchema: json.RawMessage(`{}`)}, "must match"},
		{"duplicate", Tool{Name: "ok_tool", Handler: noopHandler, InputSchema: json.RawMessage(`{}`), SideEffect: SideEffectNone}, ""},
		{"nil handler", Tool{Name: "ok_tool", InputSchema: json.RawMessage(`{}`)}, "handler is nil"},
		{"bad input schema", Tool{Name: "ok_tool", InputSchema: json.RawMessage(`[1]`), Handler: noopHandler}, "input schema"},
		{"bad output schema", Tool{Name: "ok_tool", InputSchema: json.RawMessage(`{}`), OutputSchema: json.RawMessage(`"x"`), Handler: noopHandler}, "output schema"},
		{"bad side effect", Tool{Name: "ok_tool", InputSchema: json.RawMessage(`{}`), SideEffect: "nuclear", Handler: noopHandler}, "side_effect"},
		{"bad role", Tool{Name: "ok_tool", InputSchema: json.RawMessage(`{}`), MinRole: 99, Handler: noopHandler}, "min_role"},
		{"negative budget", Tool{Name: "ok_tool", InputSchema: json.RawMessage(`{}`), Budget: Budget{MaxCallsPerRun: -1}, SideEffect: SideEffectNone, Handler: noopHandler}, "max_calls_per_run"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := NewRegistry()
			if tc.name == "duplicate" {
				if err := reg.Register(tc.tool); err != nil {
					t.Fatalf("first register: %v", err)
				}
			}
			err := reg.Register(tc.tool)
			if err == nil {
				t.Fatalf("Register(%s) succeeded, want failure", tc.name)
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want %q", err, tc.want)
			}
			if tc.name == "duplicate" {
				_ = err
			}
		})
	}
}

func noopHandler(ctx context.Context, c Call) (Result, error) {
	return Result{}, nil
}

func TestRegistryDefinitionsMirrorTheAllowlist(t *testing.T) {
	reg := testRegistry(t,
		Tool{Name: "first_tool", Description: "one", InputSchema: json.RawMessage(`{"type":"object"}`), SideEffect: SideEffectNone, Handler: noopHandler},
		Tool{Name: "second_tool", Description: "two", InputSchema: json.RawMessage(`{"type":"object"}`), SideEffect: SideEffectNone, Handler: noopHandler},
	)
	defs := reg.Definitions()
	if len(defs) != 2 {
		t.Fatalf("definitions = %d, want 2", len(defs))
	}
	if defs[0].Name != "first_tool" || defs[1].Name != "second_tool" {
		t.Fatalf("order = %s, %s", defs[0].Name, defs[1].Name)
	}
	if defs[0].Description != "one" || string(defs[0].Parameters) != `{"type":"object"}` {
		t.Fatalf("def 0 = %+v", defs[0])
	}
	if _, ok := reg.Tool("missing"); ok {
		t.Fatal("Tool(missing) found, want miss")
	}
	if reg.Has("missing") || !reg.Has("first_tool") {
		t.Fatal("Has results wrong")
	}
}

// --- schema validator -------------------------------------------------------

func TestSchemaValidationAcceptsConformingValues(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{
			"name":{"type":"string","maxLength":3},
			"count":{"type":"integer"},
			"ratio":{"type":"number"},
			"ok":{"type":"boolean"},
			"kind":{"enum":["a","b"]},
			"tags":{"type":"array","maxItems":2,"items":{"type":"string"}}
		},
		"required":["name"]
	}`)
	if err := ValidateSchema(schema); err != nil {
		t.Fatalf("ValidateSchema: %v", err)
	}
	good := json.RawMessage(`{"name":"ab","count":2,"ratio":1.5,"ok":true,"kind":"a","tags":["x","y"]}`)
	if err := ValidateArgs(schema, good); err != nil {
		t.Fatalf("ValidateArgs(good) = %v, want nil", err)
	}
	// Empty args against a schema without required fields pass.
	noReq := json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}}}`)
	if err := ValidateArgs(noReq, nil); err != nil {
		t.Fatalf("ValidateArgs(nil, no required) = %v, want nil", err)
	}
}

func TestSchemaValidationRejectsViolations(t *testing.T) {
	cases := []struct {
		name   string
		schema json.RawMessage
		args   json.RawMessage
		want   string
	}{
		{"missing required", json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`), json.RawMessage(`{}`), "required"},
		{"wrong type", json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"}}}`), json.RawMessage(`{"a":3}`), "expected string"},
		{"maxLength", json.RawMessage(`{"type":"object","properties":{"a":{"type":"string","maxLength":2}}}`), json.RawMessage(`{"a":"abc"}`), "maxLength"},
		{"maxItems", json.RawMessage(`{"type":"object","properties":{"a":{"type":"array","maxItems":1}}}`), json.RawMessage(`{"a":[1,2]}`), "maxItems"},
		{"enum", json.RawMessage(`{"type":"object","properties":{"a":{"enum":["x"]}}}`), json.RawMessage(`{"a":"y"}`), "enum"},
		{"integer", json.RawMessage(`{"type":"object","properties":{"a":{"type":"integer"}}}`), json.RawMessage(`{"a":1.5}`), "integer"},
		{"bad json args", json.RawMessage(`{"type":"object"}`), json.RawMessage(`{nope`), "not valid JSON"},
		{"nested", json.RawMessage(`{"type":"object","properties":{"o":{"type":"object","properties":{"s":{"type":"string"}},"required":["s"]}}}`), json.RawMessage(`{"o":{}}`), "missing required field"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateArgs(tc.schema, tc.args)
			if err == nil {
				t.Fatalf("ValidateArgs accepted %s against %s", tc.args, tc.schema)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestValidateSchemaRejectsMalformedSchemas(t *testing.T) {
	cases := []struct {
		name   string
		schema json.RawMessage
	}{
		{"empty", json.RawMessage(``)},
		{"array", json.RawMessage(`[1]`)},
		{"unknown type", json.RawMessage(`{"type":"widget"}`)},
		{"bad property", json.RawMessage(`{"properties":{"a":1}}`)},
		{"empty required", json.RawMessage(`{"required":[""]}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateSchema(tc.schema); err == nil {
				t.Fatalf("ValidateSchema accepted %s", tc.schema)
			}
		})
	}
}

// The executor refuses a call whose arguments fail the schema before the
// handler runs (docs/spec/14 "verifies ... args schema ... before action").
func TestExecutorRefusesSchemaViolationBeforeAction(t *testing.T) {
	ran := false
	reg := testRegistry(t, Tool{
		Name:        "t1",
		Description: "d",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`),
		SideEffect:  SideEffectNone,
		Handler: func(ctx context.Context, c Call) (Result, error) {
			ran = true
			return Result{}, nil
		},
	})
	e := NewExecutor(reg, auth.RoleOperator, 10)
	res := e.Execute(t.Context(), Call{ID: "c1", Name: "t1", Arguments: json.RawMessage(`{}`)})
	if res.Status != StatusRefused {
		t.Fatalf("status = %s, want refused", res.Status)
	}
	if ran {
		t.Fatal("handler ran on a schema violation")
	}
	if res.SchemaVersion != 1 || res.ToolCallID != "c1" || res.Name != "t1" {
		t.Fatalf("envelope = %+v", res)
	}
}
