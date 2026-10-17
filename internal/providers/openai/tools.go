package openai

// Tool-calling support for OpenAI-compatible gateways (docs/spec/06,
// docs/spec/03 "typed tool calls"). Verified live against qwen3-8-27b:
// `tools` + `tool_choice:"auto"` is accepted, and a tool response arrives as
// finish_reason "tool_calls" with content null and one or more *parallel*
// tool_calls, each carrying arguments as a JSON *string*.
//
// This file defines the neutral types and their wire projections. client.go
// keeps the request/response plumbing; parsing of an incoming call's
// arguments into valid JSON happens here so a malformed arguments string
// fails locally as bad_schema instead of surfacing as a runtime panic.

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dbirks/game-narrator/internal/providers"
)

// ToolDef declares one callable function for the model. Parameters is a JSON
// Schema object describing the arguments; the agent registry
// (internal/agent) validates it at registration time, and the gateway may
// validate it at request time.
type ToolDef struct {
	Name        string
	Description string
	// Parameters is the JSON Schema for the arguments object. Kept raw:
	// the wire form is passed through verbatim so the model sees exactly
	// what the registry declared.
	Parameters json.RawMessage
}

// ToolChoice mirrors the OpenAI tool_choice field. The zero value means
// "send nothing" (the gateway default, which for the gateways we tested is
// equivalent to "auto" when tools are present).
type ToolChoice string

const (
	// ToolChoiceAuto lets the model decide whether to call a tool.
	ToolChoiceAuto ToolChoice = "auto"
	// ToolChoiceNone forces a plain answer with no tool call.
	ToolChoiceNone ToolChoice = "none"
	// ToolChoiceRequired forces at least one tool call.
	ToolChoiceRequired ToolChoice = "required"
)

// Valid reports whether c is one of the three keyword choices or a concrete
// named tool. A keyword that is also a registered tool name is ambiguous and
// is rejected so a typo cannot silently flip the call to forced-tools.
func (c ToolChoice) Valid() bool {
	switch c {
	case ToolChoiceAuto, ToolChoiceNone, ToolChoiceRequired:
		return true
	}
	return c != "" && c != ToolChoiceAuto && c != ToolChoiceNone && c != ToolChoiceRequired &&
		strings.TrimSpace(string(c)) == string(c)
}

// ToolCall is one function invocation the model asked to execute. Arguments
// is parsed JSON (the gateway sends a JSON string; a string that does not
// parse is a protocol violation and becomes a bad_schema error, not a zero
// value the executor would run on).
type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// ArgumentsObject decodes the call's arguments into a map for the executor.
// It always succeeds on a ToolCall that passed validation.
func (c ToolCall) ArgumentsObject() (map[string]any, error) {
	if len(c.Arguments) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(c.Arguments, &m); err != nil {
		return nil, fmt.Errorf("openai: tool %s arguments are not a JSON object: %w", c.Name, err)
	}
	return m, nil
}

// --- wire forms -----------------------------------------------------------

type wireTool struct {
	Type     string `json:"type"` // always "function"
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type wireToolCall struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Func struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// toWire renders the request-side tool declarations, validating that the
// parameters carry at least a JSON object so the request is not
// self-contradictory before it leaves the process.
func toolsToWire(tools []ToolDef) ([]wireTool, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]wireTool, 0, len(tools))
	for i, t := range tools {
		if strings.TrimSpace(t.Name) == "" {
			return nil, providers.NewError(providers.KindBadSchema, 0, fmt.Sprintf("openai: tool %d has an empty name", i))
		}
		var doc map[string]any
		if err := json.Unmarshal(t.Parameters, &doc); err != nil || doc == nil {
			return nil, providers.NewError(providers.KindBadSchema, 0,
				fmt.Sprintf("openai: tool %q parameters must be a JSON Schema object", t.Name))
		}
		w := wireTool{Type: "function"}
		w.Function.Name = t.Name
		w.Function.Description = t.Description
		w.Function.Parameters = t.Parameters
		out = append(out, w)
	}
	return out, nil
}

func toolCallsToWire(calls []ToolCall) []wireToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]wireToolCall, 0, len(calls))
	for _, c := range calls {
		w := wireToolCall{ID: c.ID, Type: "function"}
		w.Func.Name = c.Name
		if len(c.Arguments) > 0 {
			w.Func.Arguments = string(c.Arguments)
		} else {
			w.Func.Arguments = "{}"
		}
		out = append(out, w)
	}
	return out
}

// choiceToWire renders ToolChoice. Keywords go out as the plain string
// form (the verified gateway shape for qwen3-8-27b); a named tool goes
// out as the object form. Empty renders nothing (no tool_choice field).
func choiceToWire(c ToolChoice) (any, error) {
	if c == "" {
		return nil, nil
	}
	switch c {
	case ToolChoiceAuto, ToolChoiceNone, ToolChoiceRequired:
		return string(c), nil
	default:
		if !c.Valid() {
			return nil, providers.NewError(providers.KindBadSchema, 0, fmt.Sprintf("openai: invalid tool choice %q", c))
		}
		return map[string]any{
			"type":     "function",
			"function": map[string]string{"name": string(c)},
		}, nil
	}
}

// parseToolCalls decodes an assistant message's tool_calls array, validating
// that every arguments string is JSON the executor can safely hand to the
// tool (docs/spec/14: the executor verifies args before action). A gateway
// that echoes an unparseable arguments string is a protocol violation: the
// whole response is rejected as bad_schema so the loop never executes
// garbage.
func parseToolCalls(raw json.RawMessage) ([]ToolCall, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var wires []wireToolCall
	if err := json.Unmarshal(raw, &wires); err != nil {
		return nil, providers.NewError(providers.KindBadSchema, 0, "openai: cannot parse tool_calls array")
	}
	out := make([]ToolCall, 0, len(wires))
	for i, w := range wires {
		if strings.TrimSpace(w.ID) == "" {
			return nil, providers.NewError(providers.KindBadSchema, 0, fmt.Sprintf("openai: tool_call %d has no id", i))
		}
		if strings.TrimSpace(w.Func.Name) == "" {
			return nil, providers.NewError(providers.KindBadSchema, 0, fmt.Sprintf("openai: tool_call %s has no function name", w.ID))
		}
		var args json.RawMessage
		if strings.TrimSpace(w.Func.Arguments) != "" {
			if !json.Valid([]byte(w.Func.Arguments)) {
				return nil, providers.NewError(providers.KindBadSchema, 0,
					fmt.Sprintf("openai: tool_call %s arguments are not valid JSON", w.ID))
			}
			args = json.RawMessage(w.Func.Arguments)
		}
		out = append(out, ToolCall{ID: w.ID, Name: w.Func.Name, Arguments: args})
	}
	return out, nil
}
