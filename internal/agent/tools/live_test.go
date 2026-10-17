package tools

// Live run (the only live test in this package, per the task contract): the
// real configured agent_reasoning model (qwen3-8-27b) drives the full tool
// set onboarding the SYNTHETIC game "Synthetic Adventure" from the generic
// adapter's own synthetic CSV. Real Exa MCP search (wrapped in the budgeted
// provider) and the real SSRF-filtering fetcher are available; the profile,
// assets and run log are a temp appliance.
//
// NOT RUN (skip) when the gateway env is absent. Log-safe: model id, step
// count, tool names and latency only — never a URL or token (AGENTS.md).

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/agent"
	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/router"
	"github.com/dbirks/game-narrator/internal/web"
	"github.com/dbirks/game-narrator/internal/web/search"
)

func TestLiveSyntheticAdventureOnboarding(t *testing.T) {
	roles, err := providers.LoadRoles(os.LookupEnv)
	if err != nil {
		t.Fatalf("LoadRoles: %v", err)
	}
	cfg := roles[providers.RoleAgentReasoning]
	if strings.TrimSpace(cfg.BaseURL) == "" || strings.TrimSpace(cfg.Token()) == "" {
		t.Skip("NOT RUN: set GAME_NARRATOR_GATEWAY_URL and the token env named by GAME_NARRATOR_GATEWAY_TOKEN_ENV")
	}
	model := strings.TrimSpace(os.Getenv("GAME_NARRATOR_LIVE_MODEL"))
	if model == "" {
		model = strings.TrimSpace(cfg.Model)
	}
	if model == "" {
		model = "qwen3-8-27b"
	}
	cfg.Model = model
	roleReg := providers.NewRegistry(map[providers.Role]providers.RoleConfig{
		providers.RoleAgentReasoning: cfg,
	})
	r := router.New(roleReg, router.Options{})
	var chatFn ChatFn = agent.RouterCall(roleReg, r).Chat

	e := newEnv(t)
	g := e.game("synthetic-adventure", "Synthetic Adventure")
	p := e.draft(e.edition(g, "US", "en", "v1").ID)

	// The uploaded synthetic CSV is the generic adapter's own fixture:
	// invented vocabulary, no game text (AGENTS.md rule 5).
	csvPath := filepath.Join("..", "..", "games", "adapter", "generic", "testdata", "synthetic-adventure.csv")
	raw, err := os.ReadFile(csvPath)
	if err != nil {
		t.Fatalf("read synthetic CSV fixture: %v", err)
	}
	sha := e.stageBytes(raw)

	// Real Exa MCP search behind the budgeted provider (docs/spec/14: bound
	// tool budget), real SSRF-filtering fetcher.
	exa := &search.ExaMCP{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	budgeted := search.NewBudgeted(exa, 8, 500*time.Millisecond)

	deps := e.deps
	deps.Search = budgeted
	deps.Fetcher = web.NewFetcher(web.Policy{})
	deps.Chat = chatFn
	deps.Approvals = NewSQLiteApprovals(e.store)
	reg, err := DefaultTools(deps)
	if err != nil {
		t.Fatalf("DefaultTools: %v", err)
	}
	if _, ok := deps.Search.(*search.Budgeted); !ok {
		t.Fatal("web calls must go through the budgeted provider")
	}

	userRequest := "Onboard the game 'Synthetic Adventure' (draft profile " + p.ID +
		"). The script file is already staged in the asset store with SHA-256 " + sha +
		". Import it with import_user_asset (role dialogue_script, rights user_supplied, " +
		"file_name synthetic-adventure.csv), then run source_quality on the imported " +
		"source and summarize the import and the coverage. Do not design voices and do " +
		"not publish. The edition is already identified, so you do not need identify_game."

	runID := games.NewID()
	ctx, cancel := context.WithTimeout(context.Background(), 480*time.Second)
	defer cancel()
	start := time.Now()
	out, err := agent.Run(ctx, agent.Options{
		RunID:       runID,
		ProfileID:   p.ID,
		Model:       agent.RouterCall(roleReg, r),
		Registry:    reg,
		CallerRole:  auth.RoleOperator,
		Store:       e.agent,
		UserRequest: userRequest,
		Budget:      agent.BudgetConfig{MaxSteps: 16, MaxToolCalls: 12},
		ModelLabel:  model,
		Prompt:      agent.PromptContext{UserRole: auth.RoleOperator, Edition: "US (synthetic)"},
	})
	latency := time.Since(start)
	if err != nil {
		t.Fatalf("live Run: %v", err)
	}
	t.Logf("live onboarding: model=%s steps=%d tool_calls=%d state=%s stop_reason=%s latency=%s",
		model, out.Steps, out.ToolCalls, out.State, out.StopReason, latency.Round(time.Millisecond))

	if out.State != agent.StateSucceeded && out.State != agent.StateReview {
		t.Fatalf("state = %s (reason %s), want succeeded or review within the step budget", out.State, out.StopReason)
	}

	// The run imported the script: the profile carries the source and its
	// messages, with a successful import and source-quality call in the log.
	msgs, err := e.deps.listMessages(e.ctx, p.ID)
	if err != nil {
		t.Fatalf("listMessages: %v", err)
	}
	if len(msgs) == 0 {
		t.Fatal("the live run imported nothing")
	}
	sources, err := e.store.ListSources(e.ctx, p.ID)
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	if len(sources) == 0 {
		t.Fatal("no source row: the import did not register provenance")
	}
	rows, err := e.agent.ListAgentToolCalls(e.ctx, runID)
	if err != nil {
		t.Fatalf("ListAgentToolCalls: %v", err)
	}
	var toolNames []string
	qualityOK, importedOK := false, false
	for _, c := range rows {
		toolNames = append(toolNames, c.Name+"("+c.Status+")")
		var res agent.Result
		if err := json.Unmarshal(c.ResultJSON, &res); err == nil {
			if c.Name == "source_quality" && res.Status == agent.StatusSucceeded {
				qualityOK = true
			}
			if c.Name == "import_user_asset" && res.Status == agent.StatusSucceeded {
				importedOK = true
			}
		}
	}
	t.Logf("live onboarding tools=%v search_budget_used=%d", toolNames, budgeted.BudgetUsed())
	if !importedOK {
		t.Fatal("the live run did not record a successful import_user_asset")
	}
	if !qualityOK {
		t.Fatal("the live run did not produce a source-quality report")
	}
	if len(msgs) == 0 || sources[0].ID == "" {
		t.Fatal("import produced no content")
	}

	// Nothing was published: the revision is still a draft, and the approval
	// store holds no consumption.
	final, _, err := e.store.GetProfile(e.ctx, p.ID)
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	if final.Status != games.StatusDraft {
		t.Fatalf("profile status = %s, want draft (the live run must not publish)", final.Status)
	}
}
