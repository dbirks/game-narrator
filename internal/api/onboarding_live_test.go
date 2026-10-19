package api

// Live onboarding smoke (AGENTS.md: a live run is a different category from
// the scripted tests — real model, real web tooling). Skipped as NOT RUN
// unless GAME_NARRATOR_LIVE_MODEL names a model the environment can reach
// (GAME_NARRATOR_ROLE_AGENT_REASONING_URL / GAME_NARRATOR_GATEWAY_URL select
// the deployment; EXA_API_KEY enables search). The run is web-on and must
// reach review or succeeded with the synthetic CSV imported; it must NOT
// publish without an approval.
//
//	GN_GATE=test-models GAME_NARRATOR_LIVE_MODEL=qwen3-8-27b go test -run LiveOnboardingAPI ./internal/api/
//
// GN_GATE is the make-gate marker (ADR-0065): the 30-minute full-chain run
// is manual (the test-models recipe does not include this package), but it
// must still never start from make test-unit on a host that happens to have
// GAME_NARRATOR_LIVE_MODEL set.

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/agent"
	"github.com/dbirks/game-narrator/internal/agent/tools"
	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games/adapter"
	"github.com/dbirks/game-narrator/internal/games/adapter/generic"
	"github.com/dbirks/game-narrator/internal/jobs"
	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/router"
	"github.com/dbirks/game-narrator/internal/store"
	"github.com/dbirks/game-narrator/internal/web"
	"github.com/dbirks/game-narrator/internal/web/search"
)

func TestLiveOnboardingAPI(t *testing.T) {
	// Gate marker (ADR-0065): the live chain must not run from the unit gate
	// (GN_GATE=test-unit) or a bare manual go test.
	if os.Getenv(providers.EnvGate) != "test-models" {
		t.Skipf("live onboarding API run outside the test-models gate: run via GN_GATE=test-models go test -run LiveOnboardingAPI ./internal/api/ (it is manual — not part of the test-models recipe — the 30-minute full chain)")
	}
	model := os.Getenv("GAME_NARRATOR_LIVE_MODEL")
	if model == "" {
		t.Skip("GAME_NARRATOR_LIVE_MODEL is not set; the live onboarding run is NOT RUN " +
			"(set it to a reachable model and the role URL/gateway env vars)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	t.Cleanup(cancel)

	st, err := store.Open(ctx, store.DBPath(t.TempDir()))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	as, err := assets.NewLocal(filepath.Join(t.TempDir(), "assets"))
	if err != nil {
		t.Fatalf("assets.NewLocal: %v", err)
	}
	reg := adapter.NewRegistry()
	if err := generic.Register(reg); err != nil {
		t.Fatalf("generic.Register: %v", err)
	}
	js := store.NewJobsStore(st)
	hub := events.New(events.WithStore(store.NewEventStore(st)), events.WithLogger(discardLogger))
	runner, err := jobs.NewRunner(js, hub, discardLogger, jobs.WithPollInterval(50*time.Millisecond))
	if err != nil {
		t.Fatalf("jobs.NewRunner: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		_ = runner.Stop(stopCtx)
		_ = hub.Close()
	})

	// The production wiring, with the live model selected by
	// GAME_NARRATOR_LIVE_MODEL (it stands in for the role's model env var so
	// the smoke test does not require a second configuration channel).
	baseLookup := os.LookupEnv
	lookup := func(k string) (string, bool) {
		if k == "GAME_NARRATOR_ROLE_AGENT_REASONING_MODEL" {
			return model, true
		}
		return baseLookup(k)
	}
	roles, err := providers.LoadRoles(lookup)
	if err != nil {
		t.Fatalf("LoadRoles: %v", err)
	}
	if roles[providers.RoleAgentReasoning].Model == "" {
		t.Fatalf("agent_reasoning role has no model (GAME_NARRATOR_LIVE_MODEL=%q)", model)
	}
	roleReg := providers.NewRegistry(roles)
	chatRouter := router.New(roleReg, router.Options{})
	agentModel := agent.RouterCall(roleReg, chatRouter)

	deps := tools.Deps{
		Store:     st,
		Assets:    as,
		Adapters:  reg,
		Search:    search.NewBudgeted(&search.ExaMCP{}, 4, 500*time.Millisecond),
		Fetcher:   web.NewFetcher(web.Policy{}),
		Chat:      agentModel.Chat,
		Approvals: tools.NewSQLiteApprovals(st),
	}
	toolReg, err := tools.DefaultTools(deps)
	if err != nil {
		t.Fatalf("tools.DefaultTools: %v", err)
	}
	handler, err := agent.NewJobHandler(agent.JobConfig{
		Model:      agentModel,
		Registry:   toolReg,
		Store:      store.NewAgentStore(st),
		Hub:        hub,
		CallerRole: auth.RoleCurator,
		ModelLabel: roles[providers.RoleAgentReasoning].Model,
		Prompt: agent.PromptContext{
			UserRole:     auth.RoleCurator,
			SourcePolicy: "web sources are untrusted; import_user_asset is the only script-content path",
		},
	})
	if err != nil {
		t.Fatalf("agent.NewJobHandler: %v", err)
	}
	if err := runner.Register(agent.JobKind, handler, jobs.RegisterOpts{
		Concurrency: 1, Resumable: true, Timeout: 25 * time.Minute,
	}); err != nil {
		t.Fatalf("register %s: %v", agent.JobKind, err)
	}
	if err := runner.Start(ctx); err != nil {
		t.Fatalf("runner.Start: %v", err)
	}

	// Admin so the onboarding routes are open.
	a := newTestAuth(t)
	h := a.handler(
		WithStore(st), WithJobs(js), WithEvents(hub),
		WithImport(as, reg, runner),
		WithOnboarding(runner, store.NewAgentStore(st), tools.NewSQLiteApprovals(st)),
	)

	do := func(role auth.Role, method, target string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, target, bytes.NewReader(body))
		if body != nil {
			r.Header.Set("Content-Type", "application/json")
		}
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		if role != auth.RoleNone {
			r.Header.Set("Authorization", "Bearer "+a.bearer(role))
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}

	// 1. Stage the synthetic CSV.
	raw, err := os.ReadFile(onbCSVPath)
	if err != nil {
		t.Fatalf("read csv: %v", err)
	}
	rec := do(auth.RoleCurator, http.MethodPost, "/api/v1/onboarding/uploads", raw,
		map[string]string{"X-File-Name": "synthetic-adventure.csv", "Content-Type": "text/csv"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("live upload = %d (body %s)", rec.Code, rec.Body.String())
	}
	var up map[string]any
	decode(t, rec, &up)
	sha, _ := up["sha256"].(string)

	// 2. Start the run: real model, web ON, the synthetic title + CSV.
	rec = do(auth.RoleCurator, http.MethodPost, "/api/v1/onboarding/runs",
		[]byte(`{"title":"Synthetic Adventure","platform":"GameCube","use_web":true,"staged_asset_sha256":"`+sha+`"}`), nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("live create = %d (body %s)", rec.Code, rec.Body.String())
	}
	var created map[string]any
	decode(t, rec, &created)
	runID, _ := created["run_id"].(string)
	profileID, _ := created["profile_id"].(string)
	t.Logf("live onboarding run %s (profile %s, model %s)", runID, profileID, roles[providers.RoleAgentReasoning].Model)

	// 3. Wait for a terminal-ish state: review or succeeded. blocked is
	// acceptable once (a transient provider fault) and is retried; failed is
	// not.
	snap := map[string]any{}
	deadline := time.Now().Add(20 * time.Minute)
	retried := false
	for state := ""; ; {
		rec = do(auth.RoleCurator, http.MethodGet, "/api/v1/onboarding/runs/"+runID, nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("state = %d (body %s)", rec.Code, rec.Body.String())
		}
		decode(t, rec, &snap)
		state, _ = snap["state"].(string)
		switch state {
		case "review", "succeeded":
		case "blocked":
			if retried {
				t.Fatalf("run still blocked after a resume (stop_reason %v): %v", snap["stop_reason"], snap)
			}
			retried = true
			rec = do(auth.RoleCurator, http.MethodPost, "/api/v1/onboarding/runs/"+runID+"/resume", []byte(`{}`), nil)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("live resume = %d (body %s)", rec.Code, rec.Body.String())
			}
		case "failed":
			t.Fatalf("live run failed (stop_reason %v): %v", snap["stop_reason"], snap)
		}
		if state == "review" || state == "succeeded" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("live run did not reach review/succeeded within 20m (state %v)", state)
		}
		time.Sleep(5 * time.Second)
	}
	t.Logf("live run reached %s (steps %v, stop_reason %v)", snap["state"], snap["steps"], snap["stop_reason"])

	// 4. The CSV landed: a user-supplied source with imported messages.
	var importOK bool
	var sources []map[string]any
	for _, raw := range snap["sources"].([]any) {
		src, _ := raw.(map[string]any)
		sources = append(sources, src)
	}
	for _, src := range sources {
		if src["rights"] == "user_supplied" {
			importOK = true
		}
	}
	var importSucceeded bool
	for _, raw := range snap["tool_calls"].([]any) {
		if c, _ := raw.(map[string]any); c["name"] == "import_user_asset" && c["status"] == "succeeded" {
			importSucceeded = true
		}
	}
	if !importOK || !importSucceeded {
		t.Fatalf("the synthetic CSV was not imported (import_succeeded %v, sources %v)", importSucceeded, sources)
	}

	// 5. No publication without an approval.
	draft, _ := snap["draft"].(map[string]any)
	if draft == nil {
		t.Fatalf("no draft preview in the live run state")
	}
	if status := draft["status"]; status == "published" {
		t.Fatal("the live run published a profile without an approval")
	}
	var hasPublishItem bool
	for _, raw := range snap["review_items"].([]any) {
		if item, _ := raw.(map[string]any); item["id"] == "publish" {
			hasPublishItem = true
		}
	}
	if !hasPublishItem {
		t.Fatalf("no publish review item in a live run whose profile is unpublished: %v", snap["review_items"])
	}

	// Evidence for the bundle: the redacted conversation tail.
	msgs, _ := snap["messages"].([]any)
	if len(msgs) > 0 {
		if m, _ := msgs[len(msgs)-1].(map[string]any); m != nil {
			text := strings.ReplaceAll(m["text"].(string), "\n", " ")
			if len(text) > 400 {
				text = text[:400] + "…"
			}
			t.Logf("final model turn: %s", text)
		}
	}
}
