package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/auth"
)

// Prompt template tests: the versioned base is embedded and stable, dynamic
// fields render as separate bounded sections, and one huge field cannot
// displace the others (docs/agents/system-prompt-onboarding.md: "render
// dynamic user permissions, tool schemas, edition and source policies into
// bounded separate fields").

func TestPromptRendersBoundedSeparateSections(t *testing.T) {
	reg := testRegistry(t,
		Tool{Name: "web_search", Description: "Search the web.", InputSchema: mustJSON(`{"type":"object"}`), SideEffect: SideEffectExternal, Handler: noopHandler},
	)
	prompt := RenderWithTools(PromptContext{
		UserRole:     auth.RoleOperator,
		Edition:      "US cartridge",
		SourcePolicy: "user-authorized sources only",
		Budgets:      BudgetConfig{MaxSteps: 24, MaxToolCalls: 60},
	}, reg)

	for _, want := range []string{
		"[USER PERMISSIONS]", "[/USER PERMISSIONS]",
		"[ENABLED TOOLS]", "[/ENABLED TOOLS]",
		"[BUDGETS]", "[/BUDGETS]",
		"[EDITION AND SOURCE POLICY]", "[/EDITION AND SOURCE POLICY]",
		"caller_role: operator",
		"web_search: Search the web.",
		"max_model_steps: 24",
		"target_edition: US cartridge",
		"source_policy: user-authorized sources only",
		// The base template body is present and versioned.
		"You are Game Narrator's game setup orchestrator.",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	// The untrusted-data stance is part of the contract, rendered even with
	// no web content present.
	if !strings.Contains(prompt, "DATA ONLY") {
		t.Error("prompt missing the untrusted-data stance")
	}
}

func TestPromptSectionsAreBounded(t *testing.T) {
	reg := NewRegistry()
	huge := strings.Repeat("x", 100000)
	prompt := RenderWithTools(PromptContext{
		UserRole:     auth.RoleViewer,
		SourcePolicy: huge,
		Budgets:      BudgetConfig{MaxSteps: 24, MaxToolCalls: 60},
	}, reg)
	if len(prompt) > len(basePrompt)+4*maxSectionBytes+512 {
		t.Fatalf("prompt is %d bytes; sections are not bounded", len(prompt))
	}
	// A huge policy must not push the tool section out.
	if !strings.Contains(prompt, "[ENABLED TOOLS]") {
		t.Fatal("huge policy displaced the tools section")
	}
	if !strings.Contains(prompt, "…[truncated]") {
		t.Fatal("huge policy was not truncated")
	}
}

func mustJSON(s string) json.RawMessage {
	return json.RawMessage(s)
}
