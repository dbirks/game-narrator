package api

// Live assistant smoke (AGENTS.md rule 3: a live run is a different category
// from the scripted tests — a real decision model, the real total-worklist
// loop, the real tools). Skipped as NOT RUN unless GN_GATE=test-models and
// GAME_NARRATOR_LIVE_MODEL name a reachable model. The run is hermetic in
// content: the profile it is pointed at is seeded with synthetic data, and
// the assertions are that a genuine model exchange took place (durable run
// log, tool calls or a final answer, the narrative card on the hub) — not
// what the model said.
//
//	GN_GATE=test-models GAME_NARRATOR_LIVE_MODEL=<model> go test -run LiveAssistantAPI ./internal/api/
//
// The role's endpoint is selected by the usual env vars (see
// internal/providers roles); this test only stands GAME_NARRATOR_LIVE_MODEL
// in for the role's model name, exactly as the onboarding live smoke does.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/agent"
	"github.com/dbirks/game-narrator/internal/assistant"
	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/jobs"
	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/router"
	"github.com/dbirks/game-narrator/internal/store"
)

func TestLiveAssistantAPI(t *testing.T) {
	if os.Getenv(providers.EnvGate) != "test-models" {
		t.Skipf("live assistant run outside the test-models gate: run via GN_GATE=test-models go test -run LiveAssistantAPI ./internal/api/ (manual, with GAME_NARRATOR_LIVE_MODEL set)")
	}
	model := os.Getenv("GAME_NARRATOR_LIVE_MODEL")
	if model == "" {
		t.Skip("GAME_NARRATOR_LIVE_MODEL is not set; the live assistant run is NOT RUN " +
			"(set it to a reachable model plus the role's URL/gateway env vars)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	t.Cleanup(cancel)

	st, err := store.Open(ctx, store.DBPath(t.TempDir()))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

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

	// The production model wiring (the onboarding live smoke does the same
	// stand-in for the role's model name).
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

	deps := assistant.Deps{
		Store:      st,
		AgentStore: store.NewAgentStore(st),
		Threads:    store.NewThreadStore(st),
		Questions:  store.NewQuestionStore(st),
		Hub:        hub,
		Events:     store.NewEventStore(st),
		Queue:      runner,
	}
	handler, err := assistant.NewJobHandler(agentModel, deps, model)
	if err != nil {
		t.Fatalf("assistant.NewJobHandler: %v", err)
	}
	if err := runner.Register(assistant.JobKind, handler, assistant.JobHandlerOpts()); err != nil {
		t.Fatalf("register assistant.run: %v", err)
	}
	if err := runner.Start(ctx); err != nil {
		t.Fatalf("runner.Start: %v", err)
	}

	a := newTestAuth(t)
	h := a.handler(
		WithStore(st), WithJobs(js), WithEvents(hub),
		WithAssistant(st, runner, hub, nil, nil, nil),
	)
	do := func(role auth.Role, method, target string, body any) *httptest.ResponseRecorder {
		t.Helper()
		var raw []byte
		if body != nil {
			b, err := json.Marshal(body)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			raw = b
		}
		r := httptest.NewRequest(method, target, strings.NewReader(string(raw)))
		if body != nil {
			r.Header.Set("Content-Type", "application/json")
		}
		if role != auth.RoleNone {
			r.Header.Set("Authorization", "Bearer "+a.bearer(role))
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}

	// ── seed the synthetic profile the assistant is pointed at ────────────
	now := time.Now().UTC().Format(time.RFC3339)
	g := games.Game{ID: games.NewID(), Slug: "live-assistant", Title: "Live Assistant", Platform: "wii", CreatedAt: now}
	if err := g.Validate(); err != nil {
		t.Fatalf("game: %v", err)
	}
	if err := st.CreateGame(ctx, g); err != nil {
		t.Fatalf("CreateGame: %v", err)
	}
	ed := games.Edition{ID: games.NewID(), GameID: g.ID, Region: "us", Language: "en", Revision: "v1"}
	if err := ed.Validate(); err != nil {
		t.Fatalf("edition: %v", err)
	}
	if err := st.CreateEdition(ctx, ed); err != nil {
		t.Fatalf("CreateEdition: %v", err)
	}
	p, err := st.CreateDraftProfile(ctx, ed.ID)
	if err != nil {
		t.Fatalf("CreateDraftProfile: %v", err)
	}
	charA := games.NewID()
	charB := games.NewID()
	if err := st.PutCharacters(ctx, p.ID, []games.Character{
		{ID: charA, ProfileID: p.ID, Name: "Alpha", PersonalitySummary: "calm and measured", Status: games.CharacterConfirmed, CreatedAt: now},
		{ID: charB, ProfileID: p.ID, Name: "Beta", Status: games.CharacterConfirmed, CreatedAt: now},
	}); err != nil {
		t.Fatalf("PutCharacters: %v", err)
	}

	// The dock subscribes before the message, so the narrative card (when the
	// attempt publishes one) is not missed.
	sub, err := hub.Subscribe(ctx, events.Filter{Names: []string{assistant.EventMessage}}, 0)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Close()
	cardSeen := make(chan struct{}, 1)
	go func() {
		for {
			ev, _, err := sub.Next(ctx, time.Second)
			if err != nil || ctx.Err() != nil {
				return
			}
			if ev.Name == assistant.EventMessage {
				select {
				case cardSeen <- struct{}{}:
				default:
				}
				return
			}
		}
	}()

	// ── the operator's message, with the screen context ───────────────────
	rec := do(auth.RoleOperator, http.MethodPost, "/api/v1/assistant/messages", map[string]any{
		"text": "Who are the characters on this profile and what does each voice brief say?",
		"context": map[string]any{
			"route":       "/profiles/" + p.ID + "/characters",
			"profile_id":  p.ID,
			"selection":   map[string]any{"type": "character", "id": charA},
			"visible_ids": []string{charA, charB},
		},
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("live message = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	var created struct {
		ThreadID string `json:"thread_id"`
		RunID    string `json:"run_id"`
		JobID    string `json:"job_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.RunID == "" {
		t.Fatalf("202 body = %s", rec.Body.String())
	}
	t.Logf("live assistant run %s (thread %s, model %s)", created.RunID, created.ThreadID, model)

	// ── wait for a terminal state; a provider-fault block is retried once ──
	agentStore := store.NewAgentStore(st)
	runID := created.RunID
	terminal := ""
	blockedRetries := 0
	deadline := time.Now().Add(12 * time.Minute)
	for terminal == "" {
		if time.Now().After(deadline) {
			t.Fatalf("run %s did not reach a terminal state within 12 minutes (last %s/%s)",
				runID, stateOf(t, agentStore, runID), stopOf(t, agentStore, runID))
		}
		time.Sleep(1500 * time.Millisecond)
		run, err := agentStore.GetAgentRun(ctx, runID)
		if err != nil {
			t.Fatalf("GetAgentRun: %v", err)
		}
		switch run.State {
		case store.AgentStateQueued, store.AgentStateRunning:
			continue
		case store.AgentStateBlocked:
			if run.StopReason != "provider_unavailable" || blockedRetries >= 1 || time.Now().After(deadline) {
				terminal = run.State
				continue
			}
			// A transient endpoint fault: the operator's re-message resumes
			// the exact same run (the loop's degraded path).
			blockedRetries++
			t.Logf("attempt blocked on the provider (%s); resuming once", run.StopReason)
			rec := do(auth.RoleOperator, http.MethodPost, "/api/v1/assistant/messages", map[string]any{
				"thread_id": created.ThreadID, "text": "The endpoint came back; please continue.",
			})
			if rec.Code != http.StatusAccepted && rec.Code != http.StatusConflict {
				t.Fatalf("resume message = %d (body %s)", rec.Code, rec.Body.String())
			}
		default: // succeeded | review | failed | cancelled
			terminal = run.State
		}
	}
	t.Logf("live run terminal: %s", terminal)
	if terminal != store.AgentStateSucceeded && terminal != store.AgentStateReview {
		t.Fatalf("live run ended %s; a real assistant smoke must reach succeeded or review", terminal)
	}

	// ── the genuine-exchange assertions ────────────────────────────────────
	run, err := agentStore.GetAgentRun(ctx, runID)
	if err != nil {
		t.Fatalf("final GetAgentRun: %v", err)
	}
	if run.Model != model {
		t.Fatalf("run model = %q; want the live model %q (the exchange must have gone through the role)", run.Model, model)
	}
	msgs, err := agentStore.ListAgentMessages(ctx, runID)
	if err != nil {
		t.Fatalf("ListAgentMessages: %v", err)
	}
	if len(msgs) < 3 {
		t.Fatalf("run log = %d messages; a real exchange leaves at least system, user and an assistant step", len(msgs))
	}
	calls, err := agentStore.ListAgentToolCalls(ctx, runID)
	if err != nil {
		t.Fatalf("ListAgentToolCalls: %v", err)
	}
	var finalAnswer string
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" {
			var body struct {
				Content string `json:"content"`
			}
			if json.Unmarshal(msgs[i].ContentJSON, &body) == nil && body.Content != "" {
				finalAnswer = body.Content
				break
			}
		}
	}
	if terminal == store.AgentStateSucceeded && finalAnswer == "" {
		t.Fatal("a succeeded run must end with a durable final answer")
	}
	// The context reached the prompt: the first user turn carries it.
	userTurnFound := false
	for _, m := range msgs {
		if m.Role == "user" && strings.Contains(string(m.ContentJSON), "[SCREEN CONTEXT]") &&
			strings.Contains(string(m.ContentJSON), "selection: character "+charA) {
			userTurnFound = true
			break
		}
	}
	if !userTurnFound {
		t.Fatal("the screen context never reached the durable conversation (UX-01)")
	}
	// Evidence line (redacted: ids, counts, model — no payloads, AGENTS.md 5).
	t.Logf("LIVE-ASSISTANT run=%s model=%s messages=%d tool_calls=%d terminal=%s answer_chars=%d",
		runID, model, len(msgs), len(calls), terminal, len(finalAnswer))

	// The narrative card: the dock's stream carried the final answer (when
	// the run ended with one).
	if terminal == store.AgentStateSucceeded {
		select {
		case <-cardSeen:
		case <-time.After(5 * time.Second):
			t.Fatal("no assistant.message card on the hub; the dock would show nothing")
		}
	}
	// The snapshot the dock rebuilds from: thread, run state, the turn.
	rec = do(auth.RoleOperator, http.MethodGet, "/api/v1/assistant/threads/"+created.ThreadID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("snapshot = %d (body %s)", rec.Code, rec.Body.String())
	}
}

func stateOf(t *testing.T, es *store.AgentStore, runID string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := es.GetAgentRun(ctx, runID)
	if err != nil {
		return "?"
	}
	return r.State
}

func stopOf(t *testing.T, es *store.AgentStore, runID string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := es.GetAgentRun(ctx, runID)
	if err != nil {
		return "?"
	}
	return r.StopReason
}
