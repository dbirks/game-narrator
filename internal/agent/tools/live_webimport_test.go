package tools

// Live check (ADR-0077, gap audit G4): for a real public title, can the real
// configured model propose a web text source? Real gateway model, real Exa
// search, real SSRF-filtering fetcher, temp appliance. The assertions are
// about the mechanism, not about the page:
//
//   - the model registers a web_page source with its provenance
//     (fetch_page),
//   - propose_web_import stops the run at the human review boundary,
//   - nothing is imported: no token is ever issued by the agent, no import
//     succeeds, the profile stays a draft with zero messages.
//
// NOT RUN outside the test-models gate. Log-safe (AGENTS.md): model id,
// step count, tool names, source counts — never a URL, a token or page
// content (the title is public knowledge; page bytes never reach the repo
// or the logs).
import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
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

func TestLiveWebSourceProposalForPublicTitle(t *testing.T) {
	// Gate marker (ADR-0065): live tests run only under make test-models;
	// the unit gate (and any manual go test) must never call the gateway.
	if os.Getenv(providers.EnvGate) != "test-models" {
		t.Skipf("live provider test outside the test-models gate: run via make test-models (it sets %s=test-models)", providers.EnvGate)
	}
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

	e := newEnv(t)
	// A real public title: the check is whether the machinery can carry the
	// model to a proposed web source, not whether any particular page is
	// importable dialogue.
	g := e.game("the-legend-of-zelda-the-wind-waker", "The Legend of Zelda: The Wind Waker")
	p := e.draft(e.edition(g, "US", "en", "v1").ID)

	exa := &search.ExaMCP{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	budgeted := search.NewBudgeted(exa, 8, 500*time.Millisecond)

	deps := e.deps
	deps.Search = budgeted
	deps.Fetcher = web.NewFetcher(web.Policy{})
	deps.Approvals = NewSQLiteApprovals(e.store)
	reg, err := DefaultTools(deps)
	if err != nil {
		t.Fatalf("DefaultTools: %v", err)
	}

	userRequest := "The user has no disc dump of 'The Legend of Zelda: The Wind Waker' (draft profile " +
		p.ID + "). Use your web tools to find one public web page of text about this game " +
		"(a public article, transcript or wiki page with substantial text), register it with " +
		"fetch_page, and then stop by proposing it for the user's approval with " +
		"propose_web_import. Do not import the source: import_web_source requires the user's " +
		"approval token, and none has been issued. Do not design voices and do not publish."

	runID := games.NewID()
	ctx, cancel := context.WithTimeout(context.Background(), 420*time.Second)
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
		Budget:      agent.BudgetConfig{MaxSteps: 14, MaxToolCalls: 10},
		ModelLabel:  model,
		Prompt:      agent.PromptContext{UserRole: auth.RoleOperator, Edition: "US (real public title, live check)"},
	})
	if err != nil {
		t.Fatalf("live Run: %v", err)
	}
	t.Logf("live web proposal: model=%s steps=%d tool_calls=%d state=%s stop_reason=%s latency=%s",
		model, out.Steps, out.ToolCalls, out.State, out.StopReason, time.Since(start).Round(time.Millisecond))

	rows, err := e.agent.ListAgentToolCalls(e.ctx, runID)
	if err != nil {
		t.Fatalf("ListAgentToolCalls: %v", err)
	}
	var (
		toolNames   []string
		proposeOK   bool
		importOK    bool
		fetchPageOK bool
		proposedIDs = map[string]bool{}
	)
	for _, c := range rows {
		toolNames = append(toolNames, c.Name+"("+c.Status+")")
		var res agent.Result
		if err := json.Unmarshal(c.ResultJSON, &res); err != nil {
			continue
		}
		switch {
		case c.Name == "propose_web_import" && res.Status == agent.StatusSucceeded:
			proposeOK = true
			for _, id := range res.SourceIDs {
				proposedIDs[id] = true
			}
		case c.Name == "import_web_source" && res.Status == agent.StatusSucceeded:
			importOK = true
		case c.Name == "fetch_page" && res.Status == agent.StatusSucceeded:
			fetchPageOK = true
		}
	}
	t.Logf("live web proposal tools=%v search_budget_used=%d", toolNames, budgeted.BudgetUsed())

	if !fetchPageOK {
		t.Fatal("the live run never registered a web page (fetch_page)")
	}
	if !proposeOK {
		t.Fatal("the live run did not propose the web source at the review boundary (propose_web_import)")
	}
	if out.State != agent.StateReview {
		t.Fatalf("state = %s (reason %s), want review: the proposal must stop the run at the human boundary", out.State, out.StopReason)
	}

	// The proposal is provenance, not content: the PROPOSED source row
	// carries a URL and a raw-body hash (never logged here), and nothing was
	// imported. The web_search hits are provenance-only rows by contract (the
	// search provider returns no page body, so those rows legitimately carry
	// no raw-body hash — docs/spec/01: "'' = no addressable payload"), and the
	// model files them on the profile when it searches with the profile id.
	// Pinnability is enforced where it matters, at the propose_web_import
	// boundary, which refuses a hashless source. The deterministic shape of
	// these rows is pinned by
	// TestWebSearchProvenanceRowsStayPinnableAtTheProposeBoundary (the
	// synthetic fixture behind this live assertion).
	sources, err := e.store.ListSources(e.ctx, p.ID)
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	webPages := 0
	for _, s := range sources {
		if s.SourceType != games.SourceWebPage {
			continue
		}
		webPages++
		if s.URI == "" {
			t.Fatalf("web source row %s is missing its URL", s.ID)
		}
		if proposedIDs[s.ID] {
			if s.ContentSHA256 == "" {
				t.Fatalf("the proposed source row %s is missing its raw-body hash: the proposal would be unpinnable", s.ID)
			}
		}
	}
	if webPages == 0 {
		t.Fatal("no web_page source row: the proposed source was not registered")
	}
	foundProposed := false
	for _, s := range sources {
		if proposedIDs[s.ID] {
			foundProposed = true
		}
	}
	if !foundProposed {
		t.Fatal("none of the proposed source ids names a stored web_page row")
	}

	if importOK {
		t.Fatal("import_web_source succeeded without a human approval: the rights gate is broken")
	}
	msgs, err := e.deps.listMessages(e.ctx, p.ID)
	if err != nil {
		t.Fatalf("listMessages: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("%d messages after a proposal-only run: nothing may be imported before the approval", len(msgs))
	}
	final, _, err := e.store.GetProfile(e.ctx, p.ID)
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	if final.Status != games.StatusDraft {
		t.Fatalf("profile status = %s, want draft (the proposal must not publish)", final.Status)
	}
	t.Logf("live web proposal: web_page_sources=%d messages=%d profile_status=%s", webPages, len(msgs), final.Status)
}
