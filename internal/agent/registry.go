package agent

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/providers/openai"
)

// The registry is the allowlist of docs/spec/14 "Tool executor verifies
// allowlist, args schema, role and cost before action". Only registered tools
// are offered to the model (Definitions) and only they can execute
// (Executor); everything else is refused before any handler runs.

// nameRe keeps tool names safe in prompts, SQL strings and logs.
var nameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{1,99}$`)

// reservedToolNames are refused at registration. The first four are the
// execution names an injection attempt would most plausibly target
// (docs/spec/03 scenario 3, docs/spec/14 "Prompt injection"); the rest are
// the names that would imply host execution if ever added, so registering
// them must be an explicit, reviewed act — which this build does not allow
// at all. The registry has no shell/exec capability, full stop.
var reservedToolNames = map[string]bool{
	"shell":        true,
	"exec":         true,
	"bash":         true,
	"run_command":  true,
	"system":       true,
	"spawn":        true,
	"subprocess":   true,
	"run_script":   true,
	"command_exec": true,
}

// Registry holds the declared tools in registration order.
type Registry struct {
	tools map[string]Tool
	order []string
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{tools: map[string]Tool{}}
}

// Register adds one tool, validating everything that can be validated
// before the model or a handler ever sees it. Registration failure is a
// programming/configuration error: it must fail the process, not degrade.
func (r *Registry) Register(t Tool) error {
	name := strings.TrimSpace(t.Name)
	if name == "" {
		return fmt.Errorf("tool: name is empty")
	}
	if len(name) > 100 {
		return fmt.Errorf("tool %q: name exceeds 100 chars", name)
	}
	if !nameRe.MatchString(name) {
		return fmt.Errorf("tool %q: name must match %s", name, nameRe)
	}
	if reservedToolNames[name] {
		return fmt.Errorf("tool %q: name is reserved and cannot be registered; this agent has no host-execution tools", name)
	}
	if _, dup := r.tools[name]; dup {
		return fmt.Errorf("tool %q: already registered", name)
	}
	if t.Handler == nil {
		return fmt.Errorf("tool %q: handler is nil", name)
	}
	if err := ValidateSchema(t.InputSchema); err != nil {
		return fmt.Errorf("tool %q input schema: %v", name, err)
	}
	if len(t.OutputSchema) > 0 {
		if err := ValidateSchema(t.OutputSchema); err != nil {
			return fmt.Errorf("tool %q output schema: %v", name, err)
		}
	}
	if int(t.MinRole) < 0 || int(t.MinRole) > int(auth.RoleAdmin) {
		return fmt.Errorf("tool %q: min_role %d is outside the role ladder", name, int(t.MinRole))
	}
	switch t.SideEffect {
	case SideEffectNone, SideEffectDraft, SideEffectExternal:
	default:
		return fmt.Errorf("tool %q: side_effect %q is not one of none|draft|external", name, t.SideEffect)
	}
	if t.Budget.MaxCallsPerRun < 0 {
		return fmt.Errorf("tool %q: budget max_calls_per_run %d is negative", name, t.Budget.MaxCallsPerRun)
	}
	if t.Budget.Timeout < 0 {
		return fmt.Errorf("tool %q: budget timeout %s is negative", name, t.Budget.Timeout)
	}
	t.Name = name
	r.tools[name] = t
	r.order = append(r.order, name)
	return nil
}

// Tool returns the registered tool.
func (r *Registry) Tool(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// Names lists the registered tool names in registration order.
func (r *Registry) Names() []string {
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// Definitions renders the allowlist as the model-visible tool declarations.
// The list is built only from the registry, so the model can never be shown a
// tool that the executor would refuse (docs/spec/03 "Use only declared
// tools and outputs").
func (r *Registry) Definitions() []openai.ToolDef {
	out := make([]openai.ToolDef, 0, len(r.order))
	for _, name := range r.order {
		t := r.tools[name]
		out = append(out, openai.ToolDef{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.InputSchema,
		})
	}
	return out
}

// Has reports whether a name is registered (used by the executor and the
// loop to distinguish "unknown tool" from "refused by a guardrail").
func (r *Registry) Has(name string) bool {
	_, ok := r.tools[name]
	return ok
}
