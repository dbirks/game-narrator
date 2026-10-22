// Package e2e holds the real end-to-end onboarding run (task
// e2e-onboarding-live): new game -> the onboarding agent researches the game
// on the live web -> stages the researched characters into the draft profile
// -> research-grounded voice briefs -> real Qwen3-TTS VoiceDesign auditions
// through the voices workflow the UI uses.
//
// This is a live test (AGENTS.md test category (d)/(e) boundary: real model
// providers, real web search). It SKIPs as NOT RUN unless the shared gateway
// is configured, so `go test ./...` stays green offline. Log-safe per
// AGENTS.md rule 5: log lines carry counts, timings, model ids and request
// ids — never page content, dialogue text or tokens. The evidence bundle
// (artifacts/e2e-onboarding/<run-id>/, gitignored) carries the source
// records (id, URL, rights) and the audition/preview WAVs; it does not copy
// fetched page text or game dialogue.
//
//	GAME_NARRATOR_LIVE_MODEL=qwen3-8-27b go test -p 1 -count=1 -timeout 30m -v -run Live ./internal/e2e/

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/agent"
	"github.com/dbirks/game-narrator/internal/agent/tools"
	"github.com/dbirks/game-narrator/internal/api"
	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/bootstrap"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/games/adapter"
	"github.com/dbirks/game-narrator/internal/games/adapter/generic"
	"github.com/dbirks/game-narrator/internal/jobs"
	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/openai"
	"github.com/dbirks/game-narrator/internal/providers/router"
	"github.com/dbirks/game-narrator/internal/providers/tts"
	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/store"
	"github.com/dbirks/game-narrator/internal/voices"
	"github.com/dbirks/game-narrator/internal/web"
	"github.com/dbirks/game-narrator/internal/web/search"
)

const (
	liveTitle    = "The Legend of Zelda: The Wind Waker"
	livePlatform = "GameCube"
	e2ePassword  = "correct horse battery staple"
	// evidenceDir is gitignored (.gitignore: artifacts/).
	evidenceDir = "artifacts/e2e-onboarding"
	// liveConsent is the meaningful consent record approval requires
	// (docs/spec/14): an original, synthetic design — no cloned voice.
	liveConsent = "original_synthetic_design (game-narrator e2e live run)"
)

// e2eEnv gates the run: NOT RUN without the shared gateway. The voice roles
// fall back to the deployment defaults (qwen3-tts-voicedesign / base).
// It also requires the test-models gate marker (ADR-0065): the live
// onboarding e2e calls the real gateway and must never run from make
// test-unit on a host that happens to have the gateway configured.
func e2eEnv(t *testing.T) (providers.RoleConfig, providers.RoleConfig, providers.RoleConfig, bool) {
	t.Helper()
	if os.Getenv(providers.EnvGate) != "test-models" {
		t.Skipf("live onboarding e2e outside the test-models gate: run via make test-models (it sets %s=test-models)", providers.EnvGate)
	}
	model := strings.TrimSpace(os.Getenv("GAME_NARRATOR_LIVE_MODEL"))
	roles, err := providers.LoadRoles(os.LookupEnv)
	if err != nil {
		t.Fatalf("LoadRoles: %v", err)
	}
	agentCfg := roles[providers.RoleAgentReasoning]
	if model == "" {
		model = "flint" // the collective replaced qwen3-8-27b with flint on 2026-10-21
	}
	agentCfg.Model = model
	// Calibrate the agent round-trip deadline for onboarding-scale prompts
	// (the role default of 30 s bounds a single model call; docs/spec/06
	// caps the *search/fetch* work at 15-30 s, the deployment calibrates the
	// model deadline via GAME_NARRATOR_ROLE_AGENT_REASONING_TIMEOUT). The
	// onboarding context (system prompt + staged web results) needs more on
	// a reasoning model than a 30 s deadline; 300 s keeps the per-call bound
	// explicit without letting one slow round-trip eat the run budget.
	agentCfg.Timeout = 300 * time.Second

	designEnv, _, _, _ := providers.RoleEnvVars(providers.RoleVoiceDesign)
	cloneEnv, _, _, _ := providers.RoleEnvVars(providers.RoleVoiceClone)
	design, clone := roles[providers.RoleVoiceDesign], roles[providers.RoleVoiceClone]
	if m := strings.TrimSpace(os.Getenv(designEnv)); m != "" {
		design.Model = m
	} else {
		design.Model = "qwen3-tts-voicedesign"
	}
	if m := strings.TrimSpace(os.Getenv(cloneEnv)); m != "" {
		clone.Model = m
	} else {
		clone.Model = "qwen3-tts-base"
	}
	design.Timeout, clone.Timeout = 180*time.Second, 180*time.Second

	direct := strings.TrimSpace(os.Getenv("GAME_NARRATOR_ROLE_VOICE_CLONE_REGISTRY_URL"))
	if direct == "" {
		direct = strings.TrimSpace(os.Getenv("GAME_NARRATOR_TTS_BASE_DIRECT_URL"))
	}
	if strings.TrimSpace(agentCfg.BaseURL) == "" || strings.TrimSpace(agentCfg.Token()) == "" {
		t.Skip("NOT RUN: set GAME_NARRATOR_GATEWAY_URL and the token env named by " +
			providers.EnvGatewayTokenEnv + " (and GAME_NARRATOR_LIVE_MODEL) to run the live onboarding e2e")
	}
	if strings.TrimSpace(design.BaseURL) == "" || strings.TrimSpace(design.Token()) == "" {
		t.Skip("NOT RUN: the VoiceDesign checkpoint (GAME_NARRATOR_ROLE_VOICE_DESIGN_URL or the gateway) is not configured")
	}
	return agentCfg, design, clone, true
}

// chatRecorder counts model calls and keeps the request ids, in order, so the
// evidence bundle can attribute the brief calls without carrying prompts. Its
// Chat method has the agent.Model signature, so the same recorder sits on the
// agent run's model boundary (JobConfig.Model) and on the analyze_character
// tool's Chat seam: every round-trip in the run and every brief is counted.
type chatRecorder struct {
	fn    tools.ChatFn
	mu    sync.Mutex
	count int
	ids   []string
	model string
}

func (r *chatRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.count, r.ids = 0, nil
}
func (r *chatRecorder) snapshot() (int, []string, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count, append([]string(nil), r.ids...), r.model
}
func (r *chatRecorder) Chat(ctx context.Context, req openai.ChatRequest) (openai.ChatResponse, error) {
	resp, err := r.fn(ctx, req)
	if err != nil {
		return resp, err
	}
	r.mu.Lock()
	r.count++
	r.ids = append(r.ids, resp.RequestID)
	r.model = resp.Model
	r.mu.Unlock()
	return resp, nil
}

func TestLiveOnboarding(t *testing.T) {
	agentCfg, designCfg, cloneCfg, ok := e2eEnv(t)
	if !ok {
		return
	}
	directURL := strings.TrimSpace(os.Getenv("GAME_NARRATOR_ROLE_VOICE_CLONE_REGISTRY_URL"))
	if directURL == "" {
		directURL = strings.TrimSpace(os.Getenv("GAME_NARRATOR_TTS_BASE_DIRECT_URL"))
	}

	// Global wall for the whole test (store, server, job runner). It must
	// exceed the sum of the phase tripwires (research 20m + publish 16m +
	// briefs/auditions tail) so a phase that runs to its own deadline fails
	// with a clear phase error, not a bare context-deadline from here. Kept
	// under the 40m go-test timeout the gate recipe passes.
	ctx, cancel := context.WithTimeout(context.Background(), 37*time.Minute)
	t.Cleanup(cancel)
	startAll := time.Now()

	dataDir := t.TempDir()
	st, err := store.Open(ctx, store.DBPath(dataDir))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	as, err := assets.NewLocal(filepath.Join(dataDir, "assets"))
	if err != nil {
		t.Fatalf("assets.NewLocal: %v", err)
	}
	adapters := adapter.NewRegistry()
	if err := generic.Register(adapters); err != nil {
		t.Fatalf("generic.Register: %v", err)
	}
	disc := slog.New(slog.DiscardHandler)
	js := store.NewJobsStore(st)
	hub := events.New(events.WithStore(store.NewEventStore(st)), events.WithLogger(disc))
	runner, err := jobs.NewRunner(js, hub, disc, jobs.WithPollInterval(50*time.Millisecond))
	if err != nil {
		t.Fatalf("jobs.NewRunner: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		_ = runner.Stop(stopCtx)
		_ = hub.Close()
	})

	// Auth on the same store: one curator token, the role the UI acts with.
	authSvc := auth.NewService(store.NewAuthStore(st), auth.WithPasswordCost(1000), auth.WithLogger(disc))
	if _, err := authSvc.Setup(ctx, "e2e-admin", e2ePassword, "127.0.0.1:1", true); err != nil {
		t.Fatalf("auth setup: %v", err)
	}
	curator, err := authSvc.CreateUser(ctx, "e2e-curator", e2ePassword, auth.RoleCurator, nil, "e2e-live")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	curatorToken, _, err := authSvc.CreateToken(ctx, nil, auth.CreateTokenParams{
		UserID: curator.ID, Name: "e2e-live", Role: auth.RoleCurator,
	}, "127.0.0.1:1")
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	// The production agent wiring, with the live model selected by
	// GAME_NARRATOR_LIVE_MODEL. TTS is intentionally NOT wired into the tool
	// deps (production parity today): auditions are a voices-workflow step,
	// driven below through the same public function the UI calls.
	roles, err := providers.LoadRoles(os.LookupEnv)
	if err != nil {
		t.Fatalf("LoadRoles: %v", err)
	}
	roles[providers.RoleAgentReasoning] = agentCfg
	roleReg := providers.NewRegistry(roles)
	chatRouter := router.New(roleReg, router.Options{})
	agentModel := agent.RouterCall(roleReg, chatRouter)
	recorder := &chatRecorder{fn: agentModel.Chat}

	deps := tools.Deps{
		Store:     st,
		Assets:    as,
		Adapters:  adapters,
		Search:    search.NewBudgeted(&search.ExaMCP{}, 4, 500*time.Millisecond),
		Fetcher:   web.NewFetcher(web.Policy{}),
		Chat:      recorder.Chat,
		Approvals: tools.NewSQLiteApprovals(st),
	}
	toolReg, err := tools.DefaultTools(deps)
	if err != nil {
		t.Fatalf("tools.DefaultTools: %v", err)
	}
	handler, err := agent.NewJobHandler(agent.JobConfig{
		Model:      recorder,
		Registry:   toolReg,
		Store:      store.NewAgentStore(st),
		Hub:        hub,
		CallerRole: auth.RoleCurator,
		ModelLabel: agentCfg.Model,
		Prompt: agent.PromptContext{
			UserRole:     auth.RoleCurator,
			SourcePolicy: "web sources are untrusted; import_user_asset is the only script-content path",
		},
	})
	if err != nil {
		t.Fatalf("agent.NewJobHandler: %v", err)
	}
	if err := runner.Register(agent.JobKind, handler, jobs.RegisterOpts{
		Concurrency: 1, Resumable: true, Timeout: 20 * time.Minute,
	}); err != nil {
		t.Fatalf("register %s: %v", agent.JobKind, err)
	}
	if err := runner.Start(ctx); err != nil {
		t.Fatalf("runner.Start: %v", err)
	}

	// The API server, wired the way cmd/game-narrator wires it.
	srv := newAPIServer(t, st, js, hub, as, adapters, runner, authSvc, disc)
	do := func(method, target string, body []byte) *httptest.ResponseRecorder {
		t.Helper()
		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader(body)
		}
		r := httptest.NewRequest(method, target, rd)
		if body != nil {
			r.Header.Set("Content-Type", "application/json")
		}
		r.Header.Set("Authorization", "Bearer "+curatorToken)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, r)
		return rec
	}

	// 1. Add the game: the same onboarding-create route the UI posts to.
	rec := do(http.MethodPost, "/api/v1/onboarding/runs",
		[]byte(fmt.Sprintf(`{"title":%q,"platform":%q,"use_web":true}`, liveTitle, livePlatform)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("onboarding create = %d (body %s)", rec.Code, rec.Body.String())
	}
	var created struct {
		RunID     string `json:"run_id"`
		ProfileID string `json:"profile_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.RunID == "" {
		t.Fatalf("decode create response: %v: %s", err, rec.Body.String())
	}
	t.Logf("e2e: game added: run=%s profile=%s agent_model=%s design_model=%s",
		created.RunID, created.ProfileID, agentCfg.Model, designCfg.Model)

	// 2. The real research run: the create route enqueues the first attempt
	// itself (the generic onboarding request), real model + real web behind it.
	snap := map[string]any{}
	agentStart := time.Now()
	// waitForRun polls the same state route the UI reads until the run stops.
	// The run row only carries the finished attempt's state (the loop writes
	// it on finish), so a settled run is one whose state is terminal AND whose
	// latest job row is not queued/running — the job field is the queue's
	// answer to "is an attempt in flight" (onboarding_state.go). A stale
	// terminal state while a message/resume attempt is in flight is the race
	// that 409s the next message if the test trusts the run row alone.
	// Transient provider faults (blocked + provider_unavailable) are the
	// ADR-0028 degraded mode: the run is resumable and the resume retries the
	// exact same round-trip, so they are retried with backoff, bounded. Other
	// stop reasons are terminal for the attempt and fail the test.
	waitForRun := func(phase string, deadline time.Duration) {
		t.Helper()
		deadlineAt := time.Now().Add(deadline)
		retries := 0
		for state := ""; ; {
			rec = do(http.MethodGet, "/api/v1/onboarding/runs/"+created.RunID, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: state = %d (body %s)", phase, rec.Code, rec.Body.String())
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &snap)
			state, _ = snap["state"].(string)
			switch state {
			case "review", "succeeded":
				if js := jobStateOf(snap); js == "queued" || js == "running" {
					// The run row shows the previous attempt's terminal state,
					// but a new attempt is in flight: not settled. Keep polling.
					if time.Now().After(deadlineAt) {
						dumpRunLog(t, st, created.RunID)
						t.Fatalf("%s: deadline reached while an attempt was in flight (job %s)", phase, js)
					}
					time.Sleep(5 * time.Second)
					continue
				}
				return
			case "blocked":
				if snap["stop_reason"] != "provider_unavailable" {
					dumpRunLog(t, st, created.RunID)
					t.Fatalf("%s: run blocked (stop_reason %v); a non-transient block is not resumable", phase, snap["stop_reason"])
				}
				if time.Now().After(deadlineAt) {
					dumpRunLog(t, st, created.RunID)
					t.Fatalf("%s: deadline reached while the provider kept failing", phase)
				}
				// The ADR-0028 degraded mode: resume retries the exact same
				// model round-trip. Each cycle is naturally paced by the failed
				// round-trip itself, so the test just re-enqueues and polls.
				retries++
				t.Logf("e2e: %s: provider unavailable (cycle %d); resuming the attempt", phase, retries)
				rec = do(http.MethodPost, "/api/v1/onboarding/runs/"+created.RunID+"/resume", []byte(`{}`))
				switch rec.Code {
				case http.StatusAccepted:
				case http.StatusConflict:
					// An attempt is queued or running: the run row lags the
					// job row, so the state we just read is stale. Keep polling.
				default:
					t.Fatalf("%s: resume = %d (body %s)", phase, rec.Code, rec.Body.String())
				}
				if retries >= 8 {
					dumpRunLog(t, st, created.RunID)
					t.Fatalf("%s: run still blocked after %d provider-unavailable cycles", phase, retries)
				}
			case "failed":
				dumpRunLog(t, st, created.RunID)
				t.Fatalf("%s: live run failed (stop_reason %v)", phase, snap["stop_reason"])
			default:
				if time.Now().After(deadlineAt) {
					dumpRunLog(t, st, created.RunID)
					t.Fatalf("%s: live run did not reach review/succeeded within %s (state %v)", phase, deadline, state)
				}
				time.Sleep(5 * time.Second)
			}
		}
	}
	// The research deadline must not outwait a legitimately in-flight attempt:
	// each agent job attempt carries a 20-minute wall (RegisterOpts below), and
	// the router retries an idempotent slow model turn up to 3×300 s (the
	// agentCfg.Timeout above), so a single pathological round-trip can consume
	// ~15 min of an attempt's tail. 15 min (the old value) expired mid-attempt
	// and failed the run while it was still in flight (observed live 2026-10-19);
	// 20 min is the attempt's own wall, after which the job runner cancels it
	// and the run row lands a terminal state the test can react to.
	waitForRun("research phase", 20*time.Minute)
	researchCalls, _, _ := recorder.snapshot()
	t.Logf("e2e: research phase done: state=%s stop_reason=%v steps=%v tool_calls=%v wall=%s model_calls=%d",
		snap["state"], snap["stop_reason"], snap["steps"], snap["tool_calls"],
		time.Since(agentStart).Round(time.Second), researchCalls)

	// If the first attempt already staged the characters (the system prompt
	// invites the roster step) and registered the research as sources, we are
	// done. Otherwise the UI's conversational path does it: a user message,
	// one resumed attempt. The message covers both gaps (missing sources,
	// missing roster) and is re-sent at most once.
	agentWallStart := time.Now()
	for attempt := 0; ; attempt++ {
		webSrcCount := len(webSourceRows(t, st, created.ProfileID))
		stagedNow := groundedCharacters(t, st, created.ProfileID)
		if webSrcCount >= 2 && len(stagedNow) >= 2 {
			break
		}
		if attempt == 2 {
			dumpRunLog(t, st, created.RunID)
			t.Fatalf("after %d conversational turns the run has %d web sources and %d staged characters; want >= 2 of each",
				attempt, webSrcCount, len(stagedNow))
		}
		msg := fmt.Sprintf(`Continue this web-research-only onboarding. Draft profile: %s. Web research is allowed; pass profile_id %s to every web_search and fetch_page call so the results register as sources.
First, make sure the profile has web sources: if your earlier calls did not register any, run 2-3 narrow web_search queries about the game's main characters and fetch the 1-2 most useful result URLs with fetch_page.
Then call stage_character exactly 3 times - once per character - for the game's main characters (the protagonist plus two other named characters), using the source ids registered on this profile. Each call: profile_id %s; the character's name; a short role; a 1-2 sentence personality summary based only on what you found; and the source_ids that support that character.
Finish with a short summary: the character names staged and the source ids attached to each. Do not design voices and do not publish.`,
			created.ProfileID, created.ProfileID, created.ProfileID)
		rec = do(http.MethodPost, "/api/v1/onboarding/runs/"+created.RunID+"/messages", []byte(`{"text":`+strconv.Quote(msg)+`}`))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("follow-up message = %d (body %s)", rec.Code, rec.Body.String())
		}
		stageStart := time.Now()
		// Same calibration as the research phase: one in-flight attempt has a
		// 20-minute job wall; the phase deadline must not expire mid-attempt.
		waitForRun("follow-up phase", 20*time.Minute)
		t.Logf("e2e: follow-up turn %d done: state=%s stop_reason=%v steps=%v tool_calls=%v wall=%s",
			attempt+1, snap["state"], snap["stop_reason"], snap["steps"], snap["tool_calls"],
			time.Since(stageStart).Round(time.Second))
	}
	agentWall := time.Since(agentWallStart)
	t.Logf("e2e: agent run %s: state=%s stop_reason=%v steps=%v tool_calls=%v total_wall=%s",
		created.RunID, snap["state"], snap["stop_reason"], snap["steps"], snap["tool_calls"],
		agentWall.Round(time.Second))

	// 3. The chain landed: real web sources with recorded rights, and
	// staged characters with source attribution on the draft profile.
	webSources := webSourceRows(t, st, created.ProfileID)
	if len(webSources) < 2 {
		t.Fatalf("only %d web sources registered; the run must have actually fetched web research", len(webSources))
	}
	t.Logf("e2e: sources fetched: %d web sources (rights recorded: %s)",
		len(webSources), rightsSummary(webSources))

	staged := groundedCharacters(t, st, created.ProfileID)
	if len(staged) < 2 {
		dumpRunLog(t, st, created.RunID)
		t.Fatalf("only %d source-grounded characters on the draft profile; want 2-3 staged via stage_character", len(staged))
	}
	if len(staged) > 3 {
		staged = staged[:3]
	}
	t.Logf("e2e: characters staged: %d (%s)", len(staged), names(staged))

	// The run must not have published: the profile is still a draft.
	final, _, err := st.GetProfile(ctx, created.ProfileID)
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	if final.Status != games.StatusDraft && final.Status != games.StatusReview {
		t.Fatalf("profile status = %s, want draft or review (no publication without the approval flow)", final.Status)
	}

	// 4. The same approval boundary the UI uses: POST .../approve with the
	// publish review item. It issues the single-use human token, moves the
	// profile draft->review and resumes the run so the agent attempts
	// publish_profile. The store's publication gate is expected to refuse —
	// a web-research-only profile has no dialogue messages ("nothing to
	// narrate") — and the refusal is recorded in the audit rows. The point of
	// the step is the boundary exercised end-to-end, not the publication.
	rec = do(http.MethodPost, "/api/v1/onboarding/runs/"+created.RunID+"/approve",
		[]byte(`{"review_item_id":"publish"}`))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("approve = %d (body %s)", rec.Code, rec.Body.String())
	}
	var approved struct {
		ApprovalID string `json:"approval_id"`
		JobID      string `json:"job_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &approved); err != nil || approved.ApprovalID == "" {
		t.Fatalf("decode approve response: %v: %s", err, rec.Body.String())
	}
	t.Logf("e2e: approval boundary: human token issued (approval %s…); resume job %s enqueued",
		approved.ApprovalID[:8], approved.JobID)
	// Covers the full retry budget of the publish round-trip (3×300 s +
	// backoff ≈ 15.2 min) so the deadline cannot expire while the router is
	// still inside the attempt's own retry sequence.
	waitForRun("publish attempt", 16*time.Minute)
	published, _, err := st.GetProfile(ctx, created.ProfileID)
	if err != nil {
		t.Fatalf("GetProfile after approval: %v", err)
	}
	if published.Status == games.StatusPublished {
		t.Fatalf("the web-research-only profile was published: the store gate must refuse a revision with no dialogue messages")
	}
	publishCall := lastToolCall(t, st, created.RunID, "publish_profile")
	t.Logf("e2e: publish attempt done: state=%s stop_reason=%v profile_status=%s publish_profile=%s",
		snap["state"], snap["stop_reason"], published.Status, publishCall)

	// 5. Voice briefs: the same tool the agent/UI-driven agent calls, driven
	// through the executor with the real model (research-grounded mode: no
	// script lines exist, the staging is the evidence — ADR-0046).
	briefs, err := briefsForCharacters(t, st, toolReg, recorder, created.ProfileID, staged)
	if err != nil {
		t.Fatalf("briefs: %v", err)
	}

	// 6. Auditions: the voices workflow the UI uses (voices.Service.CreateAuditions
	// is the synchronous body of the voice.auditions job), real VoiceDesign.
	designClient, err := tts.NewFromRole(designCfg)
	if err != nil {
		t.Fatalf("design client: %v", err)
	}
	cloneClient, err := tts.NewFromRole(cloneCfg)
	if err != nil {
		t.Fatalf("clone client: %v", err)
	}
	var registry voices.RegistryClient
	var baseReg *tts.Registry
	if directURL != "" {
		baseReg, err = tts.NewRegistry(directURL, cloneCfg.Token())
		if err != nil {
			t.Fatalf("registry: %v", err)
		}
		registry = baseReg
	}
	voiceSvc := &voices.Service{
		Store: st, Assets: as, Design: designClient, Base: cloneClient,
		Registry: registry, Speech: speech.New(as, speech.NewMemIndex(), 8),
		InstanceID: voices.DefaultInstanceID, BaseModelID: cloneCfg.Model,
		BackendDigest: "e2e-preview-v1", Language: "English",
	}
	if registry != nil {
		if err := voices.New(voiceSvc); err != nil {
			t.Fatalf("voices.New: %v", err)
		}
	}

	var auditionEvidence []map[string]any
	auditionStart := time.Now()
	for _, b := range briefs {
		audStart := time.Now()
		var rev store.VoiceProfile
		var auditions []store.VoiceAudition
		var aerr error
		for attempt := 1; ; attempt++ {
			rev, auditions, aerr = voiceSvc.CreateAuditions(ctx, b.character.Name, b.instruction, len(voices.SyntheticAuditionLines))
			if aerr == nil || attempt >= 3 || !retryableProviderFault(aerr) {
				break
			}
			t.Logf("e2e: auditions for %s: transient provider fault (cycle %d): %s", b.character.Name, attempt, aerr)
			time.Sleep(5 * time.Second * time.Duration(attempt))
		}
		if aerr != nil {
			t.Fatalf("auditions for %s: %v", b.character.Name, aerr)
		}
		wall := time.Since(audStart)
		if len(auditions) != len(voices.SyntheticAuditionLines) {
			t.Fatalf("auditions for %s = %d, want %d", b.character.Name, len(auditions), len(voices.SyntheticAuditionLines))
		}
		var durs []string
		for _, au := range auditions {
			// Real WAV bytes: read the clip back out of the asset store and
			// validate it — a row without its bytes is dead weight.
			wav, err := readAsset(ctx, as, au.AssetSHA256)
			if err != nil {
				t.Fatalf("audition audio for %s: %v", b.character.Name, err)
			}
			info, err := tts.ValidateAudio(wav)
			if err != nil {
				t.Fatalf("audition %s clip %d is not a valid WAV: %v", b.character.Name, au.Index, err)
			}
			if info.Duration <= 500*time.Millisecond {
				t.Fatalf("audition %s clip %d duration %s (validated bytes), want > 0.5s", b.character.Name, au.Index, info.Duration)
			}
			if au.ModelID == "" {
				t.Fatalf("audition %s clip %d missing the model identity", b.character.Name, au.Index)
			}
			durs = append(durs, info.Duration.Round(time.Millisecond).String())
			evidencePath := filepath.Join(evidencePathFor(created.RunID), wavName(b.character.Name, au.Index))
			if err := writeEvidence(evidencePath, wav); err != nil {
				t.Fatalf("write audition evidence %s: %v", evidencePath, err)
			}
			auditionEvidence = append(auditionEvidence, map[string]any{
				"character": b.character.Name, "index": au.Index,
				"duration_ms": au.DurationMS, "wav_duration_ms": int64(info.Duration.Milliseconds()),
				"wav_bytes": len(wav),
				"model":     au.ModelID, "request_id": au.RequestID,
				"sha256_16": au.AssetSHA256[:16],
			})
		}
		t.Logf("e2e: auditions for %s: %d clips [%s] revision=%s model=%s wall=%s",
			b.character.Name, len(auditions), strings.Join(durs, ", "), rev.ID, designCfg.Model, wall.Round(time.Second))
	}
	t.Logf("e2e: auditions done: %d clips total, wall=%s",
		len(auditionEvidence), time.Since(auditionStart).Round(time.Second))

	// Provider request proof: every clip must carry a non-empty request id
	// (ADR-0050 follow-up). The vLLM-Omni deployment issues no request-id
	// response header on /audio/speech (verified live 2026-10-18 — both 200
	// and 400 carry only x-vllm-omni-* token counts and
	// x-llm-collective-model), so the client now mints a ULID per request,
	// sends it as X-Request-Id, and records it when the provider returns none
	// (RequestIDSource: client). Either way the recorded id is the key the
	// provider-side logs can be searched by; an empty id is the regression
	// this assertion catches.
	for _, e := range auditionEvidence {
		rid, _ := e["request_id"].(string)
		if strings.TrimSpace(rid) == "" {
			t.Fatalf("audition %v clip %v: empty request id; every clip must carry provider request proof", e["character"], e["index"])
		}
	}

	// 7. Optional tail (the user's "(and, optionally, …)"): approve one
	// voice, register it on the direct Base instance, preview one line.
	var previewEvidence map[string]any
	if registry == nil {
		t.Logf("e2e: optional approve/register/preview NOT RUN: no direct Base registry URL configured")
	} else {
		first := briefs[0]
		rev, err := latestRevision(ctx, st, first.character.Name)
		if err != nil {
			t.Fatalf("latest revision for %s: %v", first.character.Name, err)
		}
		auds, err := st.ListAuditions(ctx, rev.ID)
		if err != nil || len(auds) == 0 {
			t.Fatalf("auditions of %s: %v (n=%d)", rev.ID, err, len(auds))
		}
		if _, err := voiceSvc.Approve(ctx, rev.ID, auds[0].ID, liveConsent, "e2e-live"); err != nil {
			t.Fatalf("approve %s: %v", rev.ID, err)
		}
		regRow, err := voiceSvc.Register(ctx, rev.ID)
		if err != nil {
			t.Fatalf("register %s: %v", rev.ID, err)
		}
		t.Logf("e2e: voice approved + registered: %s name=%s state=%s",
			rev.ID, regRow.RegisteredName, regRow.State)
		// The run registered a voice on the SHARED Base registry: cleanup
		// must remove the exact name this run created through the product's
		// own removal path (the registry Remove the voice workflow uses) and
		// assert the registry no longer lists it. t.Cleanup runs on failure
		// too, so a failed assertion never leaves a test voice behind on the
		// shared instance (the 2026-10-18 leak that had to be deleted by
		// hand is the regression this pins).
		registeredName := regRow.RegisteredName
		t.Cleanup(func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := baseReg.Remove(cleanupCtx, registeredName); err != nil {
				t.Logf("e2e: cleanup Remove(%s): %v", registeredName, err)
			}
			listed, err := baseReg.List(cleanupCtx)
			if err != nil {
				t.Errorf("e2e: cleanup List: %v", err)
				return
			}
			for _, n := range listed.Voices {
				if n == registeredName {
					t.Errorf("e2e: registry still lists %s after cleanup; the run must not leak registered voices", registeredName)
				}
			}
			t.Logf("e2e: cleanup verified: %s no longer listed on the shared Base registry", registeredName)
		})
		previewLine := "The road ahead was long, and the sea was calm."
		p, err := voiceSvc.Preview(ctx, rev.ID, previewLine)
		if err != nil {
			t.Fatalf("preview %s: %v", rev.ID, err)
		}
		if len(p.WAV) == 0 || p.Duration <= 0 {
			t.Fatalf("preview returned no audio (%d bytes, %s)", len(p.WAV), p.Duration)
		}
		if err := writeEvidence(filepath.Join(evidencePathFor(created.RunID),
			"preview-"+wavName(first.character.Name, 1)+"-preview.wav"), p.WAV); err != nil {
			t.Fatalf("write preview evidence: %v", err)
		}
		previewEvidence = map[string]any{
			"character":           first.character.Name,
			"revision":            rev.ID,
			"registered_name":     regRow.RegisteredName,
			"preview_wav_bytes":   len(p.WAV),
			"preview_duration_ms": int64(p.Duration.Milliseconds()),
			"from_cache":          p.FromCache,
		}
		t.Logf("e2e: preview: %d bytes, duration=%s (from_cache=%v)",
			len(p.WAV), p.Duration, p.FromCache)
	}

	// 8. Evidence bundle: the JSON summary + the WAVs above.
	agentSteps, _ := snap["steps"].(float64)
	agentToolCalls, _ := snap["tool_calls"].(float64)
	modelCalls := map[string]int{
		"agent_run":      int(agentSteps),
		"brief_calls":    len(briefs),
		"audition_calls": len(auditionEvidence),
		"preview_calls":  0,
	}
	if previewEvidence != nil {
		modelCalls["preview_calls"] = 1
	}
	total := 0
	for _, n := range modelCalls {
		total += n
	}
	summary := map[string]any{
		"run_id":             created.RunID,
		"profile_id":         created.ProfileID,
		"title":              liveTitle,
		"platform":           livePlatform,
		"agent_model":        agentCfg.Model,
		"design_model":       designCfg.Model,
		"base_model":         cloneCfg.Model,
		"agent_state":        snap["state"],
		"agent_stop_reason":  snap["stop_reason"],
		"agent_tool_calls":   int(agentToolCalls),
		"started_at":         startAll.UTC().Format(time.RFC3339),
		"finished_at":        time.Now().UTC().Format(time.RFC3339),
		"wall_seconds":       int64(time.Since(startAll).Seconds()),
		"agent_wall_seconds": int64(agentWall.Seconds()),
		"sources":            sourceEvidence(webSources),
		"characters":         characterEvidence(staged),
		"briefs":             briefEvidence(briefs),
		"auditions":          auditionEvidence,
		"optional":           previewEvidence,
		"model_calls":        modelCalls,
		"model_calls_total":  total,
	}
	if err := writeSummary(filepath.Join(evidencePathFor(created.RunID), "summary.json"), summary); err != nil {
		t.Fatalf("write summary: %v", err)
	}
	t.Logf("e2e: SUMMARY: sources=%d characters=%d briefs=%d (citation counts %v) auditions=%d model_calls=%d wall=%s",
		len(webSources), len(staged), len(briefs), citationCounts(briefs),
		len(auditionEvidence), total, time.Since(startAll).Round(time.Second))
}

// ---------------------------------------------------------------------------

type briefResult struct {
	character   games.Character
	instruction string // the design instruction the UI would send (brief JSON)
	briefChars  int    // length of the brief JSON
	citations   int    // grounding source count
	grounding   string
	requestID   string
}

// briefsForCharacters drives analyze_character for each staged character
// through the executor (the same boundary the agent loop uses: schema, role,
// budget, timeout), with the real model behind the tool's Chat seam.
// retryableProviderFault reports whether an error message names a transient
// provider fault (the docs/spec/06 gateway taxonomy): the direct steps the
// test drives itself (briefs, auditions) have no run-resume to fall back on,
// so they retry these a bounded number of times.
func retryableProviderFault(err error) bool {
	s := strings.ToLower(err.Error())
	for _, k := range []string{"unavailable", "timeout", "timed out", "rate_limit", "ratelimit", "502", "503", "504", "connection refused", "connection reset"} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// briefRetryable reports whether a failed analyze_character attempt is one a
// fresh model attempt can fix: a transient provider fault, or the model's
// own output violating the brief contract (an empty creative field or no
// parseable JSON). The tool surfaces both as a failed result; the executor
// (unlike the agent loop) has no conversation to hand the error back into,
// so the test re-rolls the call a bounded number of times.
func briefRetryable(err string) bool {
	if strings.Contains(err, "left the brief field") || strings.Contains(err, "no valid JSON brief") {
		return true
	}
	return retryableProviderFault(fmt.Errorf("%s", err))
}

func briefsForCharacters(t *testing.T, st *store.Store, reg *agent.Registry, rec *chatRecorder,
	profileID string, chars []games.Character) ([]briefResult, error) {
	t.Helper()
	exec := agent.NewExecutor(reg, auth.RoleCurator, 16)
	out := make([]briefResult, 0, len(chars))
	for _, ch := range chars {
		rec.reset()
		args, _ := json.Marshal(map[string]any{
			"profile_id": profileID, "character_id": ch.ID,
		})
		start := time.Now()
		var res agent.Result
		for attempt := 1; ; attempt++ {
			res = exec.Execute(context.Background(), agent.Call{
				ID: fmt.Sprintf("e2e-analyze-%s-%d", ch.ID, attempt), Name: "analyze_character", Arguments: args,
			})
			if res.Status == agent.StatusSucceeded || attempt >= 4 || !briefRetryable(res.Error) {
				break
			}
			t.Logf("e2e: brief for %s: retryable fault (cycle %d): %s", ch.Name, attempt, res.Error)
			time.Sleep(5 * time.Second * time.Duration(attempt))
		}
		if res.Status != agent.StatusSucceeded {
			return nil, fmt.Errorf("analyze_character for %s: %s: %s", ch.Name, res.Status, res.Error)
		}
		var body struct {
			VoiceBrief map[string]struct {
				Value    any  `json:"value"`
				Creative bool `json:"creative"`
			} `json:"voice_brief"`
			CitedLines []map[string]any `json:"cited_lines"`
			Grounding  struct {
				Mode      string   `json:"mode"`
				SourceIDs []string `json:"source_ids"`
			} `json:"grounding"`
		}
		if err := json.Unmarshal(res.Results, &body); err != nil {
			return nil, fmt.Errorf("analyze_character for %s: results are not valid JSON: %v", ch.Name, err)
		}
		if body.Grounding.Mode != "researched_character" {
			return nil, fmt.Errorf("character %s: grounding mode = %q, want researched_character (no script was imported)", ch.Name, body.Grounding.Mode)
		}
		if len(body.Grounding.SourceIDs) == 0 {
			return nil, fmt.Errorf("character %s: the brief cites no sources", ch.Name)
		}
		instr := map[string]any{}
		for f, field := range body.VoiceBrief {
			s, _ := field.Value.(string)
			if field.Creative && strings.TrimSpace(s) == "" {
				return nil, fmt.Errorf("character %s: brief field %s is empty", ch.Name, f)
			}
			instr[f] = s
		}
		instructionJSON, err := json.Marshal(instr)
		if err != nil {
			return nil, err
		}
		n, ids, model := rec.snapshot()
		if n == 0 {
			return nil, fmt.Errorf("character %s: the brief made no model call", ch.Name)
		}
		out = append(out, briefResult{
			character:   ch,
			instruction: string(instructionJSON),
			briefChars:  len(instructionJSON),
			citations:   len(body.Grounding.SourceIDs),
			grounding:   body.Grounding.Mode,
			requestID:   ids[len(ids)-1],
		})
		_ = model
		t.Logf("e2e: brief for %s: %d chars, %d source citations, grounding=%s, wall=%s, request_id=%s",
			ch.Name, len(instructionJSON), len(body.Grounding.SourceIDs), body.Grounding.Mode,
			time.Since(start).Round(time.Second), ids[len(ids)-1])
	}
	return out, nil
}

// newAPIServer assembles the API surface the way cmd/game-narrator does
// (docs/spec/08, ADR-0028): the production options, the e2e credentials.
func newAPIServer(t *testing.T, st *store.Store, js *store.JobsStore, hub *events.Hub,
	as assets.Store, adapters *adapter.Registry, queue jobsQueue, authSvc *auth.Service, log *slog.Logger) http.Handler {
	t.Helper()
	srv := api.New(bootstrap.BuildInfo{Version: "e2e-live"}, time.Now(), log,
		api.WithAuth(authSvc, store.NewAuthStore(st)),
		api.WithStore(st),
		api.WithJobs(js),
		api.WithEvents(hub),
		api.WithImport(as, adapters, queue),
		api.WithOnboarding(queue, store.NewAgentStore(st), tools.NewSQLiteApprovals(st)),
	)
	return srv.Handler()
}

// jobsQueue is the narrow seam the onboarding routes need (*jobs.Runner
// satisfies it, as in production).
type jobsQueue interface {
	Enqueue(ctx context.Context, p store.EnqueueParams) (*store.Job, bool, error)
}

// lastToolCall returns the last recorded outcome of one tool name on the run
// ("succeeded"/"failed"…), for the evidence log.
func lastToolCall(t *testing.T, st *store.Store, runID, tool string) string {
	t.Helper()
	calls, err := store.NewAgentStore(st).ListAgentToolCalls(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListAgentToolCalls: %v", err)
	}
	out := "(no call recorded)"
	for _, c := range calls {
		if c.Name == tool {
			out = c.Status
			if c.Error != "" {
				out += ": " + c.Error
			}
		}
	}
	return out
}

func dumpRunLog(t *testing.T, st *store.Store, runID string) {
	t.Helper()
	agentStore := store.NewAgentStore(st)
	calls, err := agentStore.ListAgentToolCalls(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListAgentToolCalls: %v", err)
	}
	var lines []string
	for _, c := range calls {
		lines = append(lines, fmt.Sprintf("%s %s status=%s error=%s", c.ID, c.Name, c.Status, c.Error))
	}
	t.Log("run tool log:\n" + strings.Join(lines, "\n"))
	// The agent.step events carry the model boundary's error text (clipped by
	// the loop) when a round-trip fails; that is what distinguishes a
	// gateway timeout from a rate limit from an outage in the failure report.
	evs, err := store.NewEventStore(st).After(context.Background(), 0, events.Filter{Names: []string{events.AgentStep}}, 20)
	if err != nil {
		t.Logf("agent step events: %v", err)
		return
	}
	for i := len(evs) - 1; i >= 0; i-- {
		ev := evs[i]
		var p struct {
			RunID string `json:"run_id"`
			Error string `json:"error"`
			Step  int    `json:"step"`
		}
		if json.Unmarshal(ev.Data, &p) == nil && p.RunID == runID {
			t.Logf("agent.step %d: error=%q", p.Step, p.Error)
		}
	}
}

// ---------------------------------------------------------------------------
// evidence helpers
// ---------------------------------------------------------------------------

func evidencePathFor(runID string) string {
	return filepath.Join(evidenceDir, runID)
}

// jobStateOf reads the state route's latest job row (the queue's answer to
// "is an attempt in flight"); "" when the response carries none.
func jobStateOf(snap map[string]any) string {
	j, ok := snap["job"].(map[string]any)
	if !ok {
		return ""
	}
	s, _ := j["state"].(string)
	return s
}

func writeEvidence(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func writeSummary(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func wavName(character string, index int) string {
	slug := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, strings.ToLower(character))
	if slug == "" {
		slug = "character"
	}
	return fmt.Sprintf("%s-audition-%d.wav", slug, index)
}

func rightsSummary(srcs []games.Source) string {
	var seen []string
	m := map[string]bool{}
	for _, s := range srcs {
		k := string(s.Rights)
		if !m[k] {
			m[k] = true
			seen = append(seen, k)
		}
	}
	return strings.Join(seen, ", ")
}

func names(chars []games.Character) string {
	out := make([]string, len(chars))
	for i, c := range chars {
		out[i] = c.Name
	}
	return strings.Join(out, ", ")
}

// webSourceRows lists the profile's web_page source rows with a URI
// (registered by web_search/fetch_page with profile_id).
func webSourceRows(t *testing.T, st *store.Store, profileID string) []games.Source {
	t.Helper()
	sources, err := st.ListSources(context.Background(), profileID)
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	var out []games.Source
	for _, s := range sources {
		if s.SourceType == games.SourceWebPage && s.URI != "" {
			out = append(out, s)
		}
	}
	return out
}

// groundedCharacters lists the profile's roster rows that carry non-empty
// verified source grounding (stage_character's evidence link).
func groundedCharacters(t *testing.T, st *store.Store, profileID string) []games.Character {
	t.Helper()
	chars, err := st.ListCharacters(context.Background(), profileID)
	if err != nil {
		t.Fatalf("ListCharacters: %v", err)
	}
	var out []games.Character
	for _, c := range chars {
		var refs []string
		if len(c.VerifiedSourceRefs) > 0 {
			_ = json.Unmarshal(c.VerifiedSourceRefs, &refs)
		}
		if len(refs) > 0 {
			out = append(out, c)
		}
	}
	return out
}

func sourceEvidence(srcs []games.Source) []map[string]any {
	out := make([]map[string]any, 0, len(srcs))
	for _, s := range srcs {
		out = append(out, map[string]any{
			"id": s.ID, "uri": s.URI, "type": string(s.SourceType),
			"rights": string(s.Rights), "trust": s.Trust, "retrieved_at": s.RetrievedAt,
		})
	}
	return out
}

func characterEvidence(chars []games.Character) []map[string]any {
	out := make([]map[string]any, 0, len(chars))
	for _, c := range chars {
		var refs []string
		if len(c.VerifiedSourceRefs) > 0 {
			_ = json.Unmarshal(c.VerifiedSourceRefs, &refs)
		}
		out = append(out, map[string]any{
			"id": c.ID, "name": c.Name, "role": c.Role, "status": string(c.Status),
			"source_ids": refs,
		})
	}
	return out
}

func briefEvidence(briefs []briefResult) []map[string]any {
	out := make([]map[string]any, 0, len(briefs))
	for _, b := range briefs {
		out = append(out, map[string]any{
			"character":      b.character.Name,
			"brief_chars":    b.briefChars,
			"citation_count": b.citations,
			"grounding_mode": b.grounding,
			"request_id":     b.requestID,
		})
	}
	return out
}

func citationCounts(briefs []briefResult) []int {
	out := make([]int, len(briefs))
	for i, b := range briefs {
		out[i] = b.citations
	}
	return out
}

func readAsset(ctx context.Context, as *assets.Local, sha string) ([]byte, error) {
	r, _, err := as.Get(ctx, sha)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

func latestRevision(ctx context.Context, st *store.Store, character string) (store.VoiceProfile, error) {
	revs, err := st.ListVoiceProfiles(ctx, character)
	if err != nil {
		return store.VoiceProfile{}, err
	}
	var best store.VoiceProfile
	for _, r := range revs {
		if best.ID == "" || r.Revision > best.Revision {
			best = r
		}
	}
	return best, nil
}
