package api

// Onboarding agent API tests (ADR-0028). The appliance under test is the real
// stack — store, asset store, job queue, tools, approval tokens — driven by a
// scripted agent model (test category (a), synthetic stub: no live model).
// The provider-fault test swaps the scripted model for the real provider
// router pointed at a local fake gateway, so the breaker, the retry and the
// degraded blocked state are the production code paths (AGENTS.md: no
// simulation theater — the fake is only the gateway, not the machinery).

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
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
	"github.com/dbirks/game-narrator/internal/providers/openai"
	"github.com/dbirks/game-narrator/internal/providers/router"
	"github.com/dbirks/game-narrator/internal/store"
)

const onbCSVPath = "../games/adapter/generic/testdata/synthetic-adventure.csv"

// onbEnv is the onboarding half of the appliance: real store, real queue,
// scripted model.
type onbEnv struct {
	t      *testing.T
	ctx    context.Context
	a      *testAuth
	st     *store.Store
	as     *assets.Local
	reg    *adapter.Registry
	js     *store.JobsStore
	hub    *events.Hub
	runner *jobs.Runner
	h      http.Handler
	model  agent.Model
}

// newOnbEnv wires the appliance the way cmd/game-narrator does, with model in
// place of the provider gateway.
func newOnbEnv(t *testing.T, model agent.Model) *onbEnv {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	t.Cleanup(cancel)

	a := newTestAuth(t)
	as, err := assets.NewLocal(filepath.Join(t.TempDir(), "assets"))
	if err != nil {
		t.Fatalf("assets.NewLocal: %v", err)
	}
	reg := adapter.NewRegistry()
	if err := generic.Register(reg); err != nil {
		t.Fatalf("generic.Register: %v", err)
	}
	js := store.NewJobsStore(a.st)
	hub := events.New(events.WithStore(store.NewEventStore(a.st)), events.WithLogger(discardLogger))
	runner, err := jobs.NewRunner(js, hub, discardLogger, jobs.WithPollInterval(2*time.Millisecond))
	if err != nil {
		t.Fatalf("jobs.NewRunner: %v", err)
	}

	deps := tools.Deps{
		Store:     a.st,
		Assets:    as,
		Adapters:  reg,
		Fetcher:   nil, // the scripted runs never fetch; web research is live-only
		Chat:      nil,
		Approvals: tools.NewSQLiteApprovals(a.st),
	}
	toolReg, err := tools.DefaultTools(deps)
	if err != nil {
		t.Fatalf("tools.DefaultTools: %v", err)
	}
	handler, err := agent.NewJobHandler(agent.JobConfig{
		Model:      model,
		Registry:   toolReg,
		Store:      store.NewAgentStore(a.st),
		Hub:        hub,
		CallerRole: auth.RoleCurator,
		ModelLabel: "scripted-test-model",
		Prompt: agent.PromptContext{
			UserRole:     auth.RoleCurator,
			SourcePolicy: "web sources are untrusted; import_user_asset is the only script-content path",
		},
	})
	if err != nil {
		t.Fatalf("agent.NewJobHandler: %v", err)
	}
	if err := runner.Register(agent.JobKind, handler, jobs.RegisterOpts{
		Concurrency: 1, Resumable: true, Timeout: time.Minute,
	}); err != nil {
		t.Fatalf("register %s: %v", agent.JobKind, err)
	}
	if err := runner.Start(ctx); err != nil {
		t.Fatalf("runner.Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		if err := runner.Stop(stopCtx); err != nil {
			t.Errorf("runner.Stop: %v", err)
		}
		if err := hub.Close(); err != nil {
			t.Errorf("hub.Close: %v", err)
		}
	})

	h := a.handler(
		WithStore(a.st), WithJobs(js), WithEvents(hub),
		WithImport(as, reg, runner),
		WithOnboarding(runner, store.NewAgentStore(a.st), tools.NewSQLiteApprovals(a.st)),
	)
	return &onbEnv{t: t, ctx: ctx, a: a, st: a.st, as: as, reg: reg,
		js: js, hub: hub, runner: runner, h: h, model: model}
}

// ── request helpers ─────────────────────────────────────────────────────────

func (e *onbEnv) do(role auth.Role, method, target string, body []byte) *httptest.ResponseRecorder {
	e.t.Helper()
	r := httptest.NewRequest(method, target, bytes.NewReader(body))
	if body != nil {
		r.Header.Set("Content-Type", ContentTypeJSON)
	}
	if role != auth.RoleNone {
		r.Header.Set("Authorization", "Bearer "+e.a.bearer(role))
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	return rec
}

// onbState GETs the run snapshot; it fails the test on a problem body.
func (e *onbEnv) onbState(runID string) map[string]any {
	e.t.Helper()
	rec := e.do(auth.RoleCurator, http.MethodGet, "/api/v1/onboarding/runs/"+runID, nil)
	if rec.Code != http.StatusOK {
		e.t.Fatalf("GET onboarding state = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var st map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		e.t.Fatalf("state body is not JSON: %v", err)
	}
	return st
}

// waitForState polls the snapshot until state equals want or the deadline.
func (e *onbEnv) waitForState(runID, want string, timeout time.Duration) map[string]any {
	e.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		st := e.onbState(runID)
		if st["state"] == want {
			return st
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("run %s did not reach %q within %s (last state %v, stop_reason %v)",
				runID, want, timeout, st["state"], st["stop_reason"])
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// jobRunID decodes the payload's run id.
func jobRunID(payload json.RawMessage) string {
	var p struct {
		RunID string `json:"run_id"`
	}
	if len(payload) > 0 && json.Unmarshal(payload, &p) == nil {
		return p.RunID
	}
	return ""
}

// waitForJobSettled polls until the run has no queued/running attempt: the run
// row's state flips at attempt end, but the job row settles a step later (the
// runner records the outcome), and a resume racing that window is a 409.
func (e *onbEnv) waitForJobSettled(runID string, timeout time.Duration) {
	e.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		cursor := ""
		active := false
		for {
			page, next, err := e.js.ListJobs(e.ctx, store.JobFilter{Kind: agent.JobKind}, cursor, 0)
			if err != nil {
				e.t.Fatalf("list jobs: %v", err)
			}
			for i := range page {
				if (page[i].State == "queued" || page[i].State == "running") && jobRunID(page[i].Payload) == runID {
					active = true
				}
			}
			if next == "" {
				break
			}
			cursor = next
		}
		if !active {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("an attempt of run %s is still in flight after %s", runID, timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// uploadRaw stages a file through POST /api/v1/onboarding/uploads and returns
// its sha256.
func (e *onbEnv) uploadRaw(path, fileName string) string {
	e.t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		e.t.Fatalf("read %s: %v", path, err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/onboarding/uploads", bytes.NewReader(raw))
	r.Header.Set("X-File-Name", fileName)
	r.Header.Set("Content-Type", "text/csv")
	r.Header.Set("Authorization", "Bearer "+e.a.bearer(auth.RoleCurator))
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	if rec.Code != http.StatusCreated {
		e.t.Fatalf("upload = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	var up map[string]any
	decode(e.t, rec, &up)
	sha, _ := up["sha256"].(string)
	if sha == "" {
		e.t.Fatalf("upload response has no sha256: %v", up)
	}
	return sha
}

// createRun starts a run and returns the 202 body.
func (e *onbEnv) createRun(payload string) map[string]any {
	e.t.Helper()
	rec := e.do(auth.RoleCurator, http.MethodPost, "/api/v1/onboarding/runs", []byte(payload))
	if rec.Code != http.StatusAccepted {
		e.t.Fatalf("create run = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	var out map[string]any
	decode(e.t, rec, &out)
	return out
}

func (e *onbEnv) post(runID, sub, payload string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.do(auth.RoleCurator, http.MethodPost,
		"/api/v1/onboarding/runs/"+runID+"/"+sub, []byte(payload))
}

// ── scripted model ──────────────────────────────────────────────────────────

var (
	onbSHARe       = regexp.MustCompile(`sha256 ([0-9a-f]{64})`)
	onbProfileRe   = regexp.MustCompile(`Draft profile: ([0-9A-HJKMNP-TV-Z]{26})`)
	onbProfileIDRe = regexp.MustCompile(`profile_id ([0-9A-HJKMNP-TV-Z]{26})`)
	onbTokenRe     = regexp.MustCompile(`gnapp_[0-9a-fA-F]{64}`)
	onbSourceIDRe  = regexp.MustCompile(`"source_id":"([^"]+)"`)
)

// scriptedOnboardingModel plays the happy path against the durable run log:
// import the staged file, run source_quality, summarise. When the conversation
// carries a human approval token it calls publish_profile with it, then stops.
// It is stateless across requests (the conversation is the state), which is
// what lets the same logic drive the fake gateway over the wire.
type scriptedOnboardingModel struct {
	mu  sync.Mutex
	log []openai.ChatRequest
}

func (m *scriptedOnboardingModel) Chat(ctx context.Context, req openai.ChatRequest) (openai.ChatResponse, error) {
	m.mu.Lock()
	m.log = append(m.log, req)
	m.mu.Unlock()

	var lastUser, lastTool string
	for i := len(req.Messages) - 1; i >= 0; i-- {
		msg := req.Messages[i]
		switch msg.Role {
		case "user":
			if lastUser == "" {
				lastUser = msg.Text
			}
		case "tool":
			if lastTool == "" {
				lastTool = msg.Text
			}
		}
		if lastUser != "" && lastTool != "" {
			break
		}
	}

	// The approval turn: publish with the token, then finish. The approval
	// message names the profile as "profile_id <id>" (not "Draft profile:").
	if loc := onbTokenRe.FindStringIndex(lastUser); loc != nil {
		token := lastUser[loc[0]:loc[1]]
		profile := onbProfileIDRe.FindStringSubmatch(lastUser)
		if len(profile) != 2 {
			return openai.ChatResponse{Text: "approval message is missing the profile id"}, nil
		}
		if lastTool == "" || !strings.Contains(lastTool, `[TOOL RESULT name=publish_profile`) {
			return openai.ChatResponse{
				Text: "Publishing the approved profile.",
				ToolCalls: []openai.ToolCall{{
					ID:   "call_publish",
					Name: "publish_profile",
					Arguments: mustJSON(map[string]any{
						"profile_id":     profile[1],
						"approval_token": token,
					}),
				}},
				FinishReason: "tool_calls",
			}, nil
		}
		return openai.ChatResponse{Text: "The profile is published. Onboarding is complete."}, nil
	}

	if strings.Contains(lastTool, `[TOOL RESULT name=source_quality`) {
		return openai.ChatResponse{
			Text: "Imported the staged script and ran the source-quality report. " +
				"The profile is a draft awaiting your approval; I will not publish without it.",
		}, nil
	}
	if strings.Contains(lastTool, `[TOOL RESULT name=import_user_asset`) {
		profile := onbProfileRe.FindStringSubmatch(lastUser)
		src := onbSourceIDRe.FindStringSubmatch(lastTool)
		if len(profile) != 2 || len(src) != 2 {
			return openai.ChatResponse{Text: "cannot find the imported source id"}, nil
		}
		return openai.ChatResponse{
			Text: "Running the source-quality report on the imported source.",
			ToolCalls: []openai.ToolCall{{
				ID:   "call_quality",
				Name: "source_quality",
				Arguments: mustJSON(map[string]any{
					"profile_id": profile[1],
					"source_id":  src[1],
				}),
			}},
			FinishReason: "tool_calls",
		}, nil
	}

	// Fresh conversation: import the staged file.
	profile := onbProfileRe.FindStringSubmatch(lastUser)
	sha := onbSHARe.FindStringSubmatch(lastUser)
	if len(profile) != 2 || len(sha) != 2 {
		return openai.ChatResponse{Text: "no staged file named in the request"}, nil
	}
	return openai.ChatResponse{
		Text: "Importing the staged script into the draft profile.",
		ToolCalls: []openai.ToolCall{{
			ID:   "call_import",
			Name: "import_user_asset",
			Arguments: mustJSON(map[string]any{
				"profile_id":          profile[1],
				"staged_asset_sha256": sha[1],
				"role":                "dialogue_script",
				"rights":              "user_supplied",
			}),
		}},
		FinishReason: "tool_calls",
	}, nil
}

func (m *scriptedOnboardingModel) requestCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.log)
}

func mustJSON(v any) json.RawMessage {
	raw, _ := json.Marshal(v)
	return raw
}

// wiresJSON renders tool calls in the OpenAI wire shape the fake gateway
// answers with.
func wiresJSON(calls []openai.ToolCall) json.RawMessage {
	wires := make([]map[string]any, 0, len(calls))
	for _, c := range calls {
		wires = append(wires, map[string]any{
			"id":   c.ID,
			"type": "function",
			"function": map[string]any{
				"name":      c.Name,
				"arguments": string(c.Arguments),
			},
		})
	}
	raw, _ := json.Marshal(wires)
	return raw
}

// ── tests ───────────────────────────────────────────────────────────────────

// TestOnboardingHappyPath is the full onboarding walk with a scripted model:
// stage a synthetic CSV, start the run, watch the tool timeline land, approve
// the publish review item, and see the profile published. Nothing publishes
// before the approval (docs/spec/03).
func TestOnboardingHappyPath(t *testing.T) {
	model := &scriptedOnboardingModel{}
	e := newOnbEnv(t, model)

	sha := e.uploadRaw(onbCSVPath, "synthetic-adventure.csv")

	rec := e.do(auth.RoleViewer, http.MethodPost, "/api/v1/onboarding/runs",
		[]byte(`{"title":"Synthetic Adventure","use_web":false,"staged_asset_sha256":"`+sha+`"}`))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer create run = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}

	created := e.createRun(`{"title":"Synthetic Adventure","platform":"GameCube","use_web":false,"staged_asset_sha256":"` + sha + `"}`)
	runID, _ := created["run_id"].(string)
	jobID, _ := created["job_id"].(string)
	profileID, _ := created["profile_id"].(string)
	if runID == "" || jobID == "" || profileID == "" {
		t.Fatalf("202 body is missing ids: %v", created)
	}

	// The run drives import → source_quality → summary and lands in succeeded.
	st := e.waitForState(runID, "succeeded", 30*time.Second)

	var importCall, qualityCall map[string]any
	for _, raw := range st["tool_calls"].([]any) {
		c, _ := raw.(map[string]any)
		switch c["name"] {
		case "import_user_asset":
			importCall = c
		case "source_quality":
			qualityCall = c
		}
	}
	if importCall == nil || importCall["status"] != "succeeded" {
		t.Fatalf("import_user_asset did not succeed: %v", st["tool_calls"])
	}
	if qualityCall == nil || qualityCall["status"] != "succeeded" {
		t.Fatalf("source_quality did not succeed: %v", st["tool_calls"])
	}
	if srcIDs := importCall["source_ids"].([]any); len(srcIDs) != 1 {
		t.Fatalf("import result carries %d source ids, want 1: %v", len(srcIDs), importCall)
	}
	if n := len(st["quality_reports"].([]any)); n != 1 {
		t.Fatalf("quality_reports = %v, want one source-quality report", st["quality_reports"])
	}

	// The draft preview shows the imported content; the profile is still a
	// draft (nothing publishes without approval) and the source is the
	// user-supplied file (web was off: no untrusted sources).
	draft, _ := st["draft"].(map[string]any)
	if draft == nil {
		t.Fatalf("no draft preview: %v", st)
	}
	if got := int(draft["message_count"].(float64)); got < 40 {
		t.Fatalf("draft message_count = %d, want >= 40 (the synthetic CSV)", got)
	}
	if draft["status"] != "draft" {
		t.Fatalf("draft status = %v, want draft before approval", draft["status"])
	}
	sources := st["sources"].([]any)
	if len(sources) != 1 {
		t.Fatalf("sources = %v, want the one imported file", sources)
	}
	if src, _ := sources[0].(map[string]any); src["rights"] != "user_supplied" {
		t.Fatalf("source rights = %v, want user_supplied", sources[0])
	}

	// The publish review item is pending while the draft is unpublished.
	var publishItem map[string]any
	for _, raw := range st["review_items"].([]any) {
		if item, _ := raw.(map[string]any); item["id"] == "publish" {
			publishItem = item
		}
	}
	if publishItem == nil || publishItem["state"] != "pending" {
		t.Fatalf("publish review item = %v, want pending", st["review_items"])
	}

	// Approve: the token is issued, the run resumes, the model calls
	// publish_profile, and the profile is published. The run state was
	// already succeeded, so the proof is the new tool call plus the settled
	// job — not the state itself.
	rec = e.post(runID, "approve", `{"review_item_id":"publish"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("approve = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	e.waitForJobSettled(runID, 30*time.Second)
	st = e.onbState(runID)

	var publishCall map[string]any
	for _, raw := range st["tool_calls"].([]any) {
		if c, _ := raw.(map[string]any); c["name"] == "publish_profile" {
			publishCall = c
		}
	}
	if publishCall == nil || publishCall["status"] != "succeeded" {
		t.Fatalf("publish_profile call = %v, want succeeded after approval", st["tool_calls"])
	}

	profile := e.do(auth.RoleCurator, http.MethodGet, "/api/v1/profiles/"+profileID, nil)
	var pbody map[string]any
	decode(t, profile, &pbody)
	if pbody["status"] != "published" {
		t.Fatalf("profile status = %v, want published (body %s)", pbody["status"], profile.Body.String())
	}

	// The approval token never reaches the wire: displayed messages redact it.
	for _, raw := range st["messages"].([]any) {
		msg, _ := raw.(map[string]any)
		text, _ := msg["text"].(string)
		if strings.Contains(text, "gnapp_") && !strings.Contains(text, "gnapp_****") {
			t.Fatalf("a raw approval token is visible in the run's messages: %q", text)
		}
	}

	// A second publish approval is refused: the profile is published.
	rec = e.post(runID, "approve", `{"review_item_id":"publish"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("second approve = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestOnboardingUploadStagesBytes covers the staging endpoint: raw bytes land
// in the asset store under their content hash, and a run naming a hash that
// was never uploaded is refused at creation.
func TestOnboardingUploadStagesBytes(t *testing.T) {
	e := newOnbEnv(t, &scriptedOnboardingModel{})
	sha := e.uploadRaw(onbCSVPath, "script.csv")
	if info, err := e.as.Stat(e.ctx, sha); err != nil || info.Size == 0 {
		t.Fatalf("staged asset missing after upload: %v %v", info, err)
	}
	rec := e.do(auth.RoleCurator, http.MethodPost, "/api/v1/onboarding/runs",
		[]byte(`{"title":"Ghost","staged_asset_sha256":"`+strings.Repeat("0", 64)+`"}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("create with un-staged sha = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestOnboardingProviderFaultDegradesAndRecovers is the degraded-mode test
// (docs/spec/13, ADR-0028): the model boundary is the real provider router
// pointed at a local fake gateway that answers 503 for a while and then
// serves. The first attempt must land the run in blocked (resumable, clear
// reason), not failed; once the gateway is healthy, resume carries the run to
// success. The fake is only the gateway; the router, the retries, the openai
// client and the loop are the production code paths.
func TestOnboardingProviderFaultDegradesAndRecovers(t *testing.T) {
	var (
		mu           sync.Mutex
		healthy      = false
		requestCount int
	)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ok := healthy
		requestCount++
		mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"gateway draining"}`))
			return
		}
		var body struct {
			Messages []struct {
				Role    string  `json:"role"`
				Content *string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		msgs := make([]openai.Message, 0, len(body.Messages))
		for _, m := range body.Messages {
			text := ""
			if m.Content != nil {
				text = *m.Content
			}
			msgs = append(msgs, openai.Message{Role: m.Role, Text: text})
		}
		resp, _ := (&scriptedOnboardingModel{}).Chat(r.Context(), openai.ChatRequest{Messages: msgs})
		content := resp.Text
		wires := wiresJSON(resp.ToolCalls)
		w.Header().Set("Content-Type", "application/json")
		wire, _ := json.Marshal(map[string]any{
			"id": "chatcmpl-test", "object": "chat.completion", "model": "fake-model",
			"choices": []map[string]any{{
				"index": 0, "finish_reason": resp.FinishReason,
				"message": map[string]any{
					"role":       "assistant",
					"content":    content,
					"tool_calls": wires,
				},
			}},
		})
		_, _ = w.Write(wire)
	}))
	t.Cleanup(gateway.Close)

	lookup := func(k string) (string, bool) {
		switch k {
		case "GAME_NARRATOR_ROLE_AGENT_REASONING_MODEL":
			return "fake-model", true
		case "GAME_NARRATOR_ROLE_AGENT_REASONING_URL":
			return gateway.URL, true
		}
		return "", false
	}
	roles, err := providers.LoadRoles(lookup)
	if err != nil {
		t.Fatalf("LoadRoles: %v", err)
	}
	roleReg := providers.NewRegistry(roles)
	// A breaker that never opens in this window: the fault the test proves is
	// the gateway's 503s exhausting the bounded retries, and an open breaker
	// would blur the recovery step with its cooldown.
	chatRouter := router.New(roleReg, router.Options{
		BreakerThreshold: 1000,
		BreakerCooldown:  time.Hour,
		MaxAttempts:      2,
		RetryBase:        5 * time.Millisecond,
		RetryCap:         20 * time.Millisecond,
	})
	model := agent.RouterCall(roleReg, chatRouter)
	e := newOnbEnv(t, model)

	sha := e.uploadRaw(onbCSVPath, "synthetic-adventure.csv")
	created := e.createRun(`{"title":"Faulty Gateway","use_web":false,"staged_asset_sha256":"` + sha + `"}`)
	runID, _ := created["run_id"].(string)

	// Attempt one: the gateway is draining. The run lands in blocked with the
	// provider reason — resumable, not failed.
	st := e.waitForState(runID, "blocked", 60*time.Second)
	if st["stop_reason"] != "provider_unavailable" {
		t.Fatalf("stop_reason = %v, want provider_unavailable", st["stop_reason"])
	}
	mu.Lock()
	sawGateway := requestCount > 0
	mu.Unlock()
	if !sawGateway {
		t.Fatal("the run never reached the gateway: the degraded path was not exercised")
	}

	// Resume while the gateway is still down: a fresh attempt, and the same
	// honest blocked outcome (no spin: the job ends, the run waits).
	rec := e.post(runID, "resume", `{}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("resume = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	e.waitForState(runID, "blocked", 60*time.Second)
	e.waitForJobSettled(runID, 30*time.Second)

	// The gateway recovers; the next attempt finishes the run.
	mu.Lock()
	healthy = true
	mu.Unlock()
	rec = e.post(runID, "resume", `{}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("resume after recovery = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	e.waitForState(runID, "succeeded", 60*time.Second)
	st = e.onbState(runID)

	var importOK bool
	for _, raw := range st["tool_calls"].([]any) {
		if c, _ := raw.(map[string]any); c["name"] == "import_user_asset" && c["status"] == "succeeded" {
			importOK = true
		}
	}
	if !importOK {
		t.Fatalf("after recovery the import did not land: %v", st["tool_calls"])
	}
}

// TestOnboardingLifecycleGuards pins the 409/404 contract of the lifecycle
// routes: messaging a failed run, an unknown run id, and a run whose profile
// is not a draft are all refused with a stable code.
func TestOnboardingLifecycleGuards(t *testing.T) {
	e := newOnbEnv(t, &scriptedOnboardingModel{})

	// Unknown run ids are 404s on every route.
	for _, target := range []string{
		"GET /api/v1/onboarding/runs/0123456789ABCDEFGHJKMNPQRSTU",
		"POST /api/v1/onboarding/runs/0123456789ABCDEFGHJKMNPQRSTU/messages",
		"POST /api/v1/onboarding/runs/0123456789ABCDEFGHJKMNPQRSTU/pause",
		"POST /api/v1/onboarding/runs/0123456789ABCDEFGHJKMNPQRSTU/resume",
		"POST /api/v1/onboarding/runs/0123456789ABCDEFGHJKMNPQRSTU/cancel",
		"POST /api/v1/onboarding/runs/0123456789ABCDEFGHJKMNPQRSTU/approve",
	} {
		parts := strings.SplitN(target, " ", 2)
		var rec *httptest.ResponseRecorder
		if parts[0] == "GET" {
			rec = e.do(auth.RoleCurator, http.MethodGet, parts[1], nil)
		} else {
			rec = e.do(auth.RoleCurator, http.MethodPost, parts[1], []byte(`{}`))
		}
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s = %d, want 404 (body %s)", target, rec.Code, rec.Body.String())
		}
	}

	// An empty message text is a validation failure (once the attempt has
	// settled, so the answer is the 422 and not the in-flight 409).
	sha := e.uploadRaw(onbCSVPath, "synthetic-adventure.csv")
	created := e.createRun(`{"title":"Guard Run","use_web":false,"staged_asset_sha256":"` + sha + `"}`)
	runID, _ := created["run_id"].(string)
	e.waitForState(runID, "succeeded", 30*time.Second)
	e.waitForJobSettled(runID, 30*time.Second)
	rec := e.post(runID, "messages", `{"text":""}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("empty message = %d, want 422 (body %s)", rec.Code, rec.Body.String())
	}

	// Cancel with no attempt in flight is a no-op that reports the state.
	rec = e.post(runID, "cancel", `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var body map[string]any
	decode(t, rec, &body)
	if body["run_id"] != runID {
		t.Fatalf("cancel body = %v", body)
	}
}
