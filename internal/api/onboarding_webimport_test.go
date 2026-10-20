package api

// ADR-0077: approving the propose_web_import review item is the rights
// assertion. It issues the single-use token for that specific (profile,
// source), records the token in the conversation (the state view redacts it),
// answers with the grant id, and the token — not anything else — is what the
// import_web_source tool will consume. A second approval of the same item is
// refused once the run has left the review state.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/agent"
	"github.com/dbirks/game-narrator/internal/agent/tools"
	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/providers/openai"
	"github.com/dbirks/game-narrator/internal/store"
)

// quietOnboardingModel answers every resumed attempt with a final summary and
// no tool call: the test asserts the approval side effects, not the resume.
type quietOnboardingModel struct{}

func ptrTime(tm time.Time) *time.Time { return &tm }

func (m *quietOnboardingModel) Chat(_ context.Context, _ openai.ChatRequest) (openai.ChatResponse, error) {
	return openai.ChatResponse{Text: "Waiting for your decision on the web source."}, nil
}

func TestOnboardingApproveWebImportIssuesToken(t *testing.T) {
	e := newOnbEnv(t, &quietOnboardingModel{})
	ctx := e.ctx

	// The profile and the web source the proposal named: a draft revision
	// with one web_page source row (the fetch provenance).
	g := games.Game{
		ID: games.NewID(), Slug: "isle-dump", Title: "Isle Dump",
		Platform: "gamecube", UserTags: []byte(`["synthetic"]`),
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := e.st.CreateGame(ctx, g); err != nil {
		t.Fatalf("CreateGame: %v", err)
	}
	ed := games.Edition{ID: games.NewID(), GameID: g.ID, Region: "US",
		Language: "en", Revision: "v1", Locale: "en_US"}
	if err := e.st.CreateEdition(ctx, ed); err != nil {
		t.Fatalf("CreateEdition: %v", err)
	}
	p, err := e.st.CreateDraftProfile(ctx, ed.ID)
	if err != nil {
		t.Fatalf("CreateDraftProfile: %v", err)
	}
	srcSHA := "aa" + strings.Repeat("00", 31)
	src := games.Source{
		ID: games.NewID(), ProfileID: p.ID, SourceType: games.SourceWebPage,
		URI:           "https://wiki.example/synthetic-tide/dump.txt",
		RetrievedAt:   time.Now().UTC().Format(time.RFC3339),
		ContentSHA256: srcSHA, Rights: games.RightsUnknown, Trust: 0.4,
	}
	if err := e.st.PutSources(ctx, p.ID, []games.Source{src}); err != nil {
		t.Fatalf("PutSources: %v", err)
	}

	// The run in review with the proposal's tool call: arguments carry the
	// (profile, source) the approval scopes, the result carries the
	// provenance and the review-required flag.
	runID := games.NewID()
	agentStore := store.NewAgentStore(e.st)
	if err := agentStore.CreateAgentRun(ctx, store.AgentRun{
		ID: runID, ProfileID: p.ID, State: "running",
		PromptVersion: "v1", Model: "quiet",
	}); err != nil {
		t.Fatalf("CreateAgentRun: %v", err)
	}
	if err := agentStore.SetAgentRunState(ctx, runID, "review", "review_required"); err != nil {
		t.Fatalf("SetAgentRunState: %v", err)
	}
	callID := games.NewID()
	args, _ := json.Marshal(map[string]string{"profile_id": p.ID, "source_id": src.ID})
	// The result is the agent.Result envelope (as okResult writes it):
	// the provenance lives in the structured `results` payload where the
	// state view reads it, the review flag at the top level. The envelope
	// must be a struct — marshalling a map base64-encodes the RawMessage.
	results, _ := json.Marshal(map[string]any{
		"profile_id": p.ID,
		"provenance": map[string]any{
			"source_id":    src.ID,
			"url":          src.URI,
			"sha256":       srcSHA,
			"retrieved_at": src.RetrievedAt,
			"rights":       "unknown",
			"trust":        0.4,
		},
		"review_required": true,
		"note":            "stopped at the human review boundary",
	})
	resultJSON, err := json.Marshal(agent.Result{
		SchemaVersion: 1, ToolCallID: callID, Name: "propose_web_import",
		Status:         agent.StatusSucceeded,
		Results:        results,
		SourceIDs:      []string{src.ID},
		ReviewRequired: true,
	})
	if err != nil {
		t.Fatalf("marshal result envelope: %v", err)
	}
	if _, err := agentStore.InsertAgentToolCall(ctx, store.AgentToolCall{
		ID: callID, RunID: runID, Name: "propose_web_import",
		ArgsJSON: args, ResultJSON: resultJSON, Status: "succeeded",
		StartedAt: time.Now().UTC(), FinishedAt: ptrTime(time.Now().UTC()),
	}); err != nil {
		t.Fatalf("InsertAgentToolCall: %v", err)
	}

	// The approval: the curator approves that exact source.
	rec := e.do(auth.RoleCurator, http.MethodPost,
		"/api/v1/onboarding/runs/"+runID+"/approve",
		[]byte(`{"review_item_id":"`+callID+`"}`))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("approve = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("approve body is not JSON: %v", err)
	}
	if resp["tool"] != "propose_web_import" || resp["approval_id"] == "" {
		t.Fatalf("approve response = %v, want the tool name and the grant id", resp)
	}

	// The token is in the conversation (the model's input) and is the
	// consumable, profile-scoped grant the import tool will spend.
	msgs, err := agentStore.ListAgentMessages(ctx, runID)
	if err != nil {
		t.Fatalf("ListAgentMessages: %v", err)
	}
	var token string
	var approvalMsg string
	for _, m := range msgs {
		if m.Role != "user" {
			continue
		}
		var body struct {
			Content string `json:"content"`
		}
		_ = json.Unmarshal(m.ContentJSON, &body)
		if loc := onbTokenRe.FindStringIndex(body.Content); loc != nil {
			token = body.Content[loc[0]:loc[1]]
			approvalMsg = body.Content
		}
	}
	if token == "" {
		t.Fatal("no approval token was recorded in the conversation")
	}
	if !strings.Contains(approvalMsg, src.ID) || !strings.Contains(approvalMsg, p.ID) {
		t.Fatalf("the approval message does not name the approved source/profile: %q", approvalMsg)
	}
	if !strings.Contains(approvalMsg, "import_web_source") {
		t.Fatalf("the approval message does not name the import tool: %q", approvalMsg)
	}

	approvals := tools.NewSQLiteApprovals(e.st)
	got, err := approvals.Consume(ctx, token)
	if err != nil {
		t.Fatalf("the issued token is not consumable: %v", err)
	}
	if got.ProfileID != p.ID {
		t.Fatalf("the token names profile %s, want %s", got.ProfileID, p.ID)
	}
	if _, err := approvals.Consume(ctx, token); err == nil {
		t.Fatal("the token spent twice")
	}

	// The state view: the message is redacted and the review item carries
	// the provenance block.
	e.waitForJobSettled(runID, 10*time.Second)
	st := e.onbState(runID)
	raw, _ := json.Marshal(st["messages"])
	if onbTokenRe.Match(raw) {
		t.Fatalf("the state view leaked the approval token: %s", raw)
	}
	rawItems, ok := st["review_items"].([]any)
	if !ok {
		t.Fatalf("review_items missing from the state view: %v", st)
	}
	for _, r := range rawItems {
		item, _ := r.(map[string]any)
		if item["tool"] != "propose_web_import" {
			continue
		}
		prov, _ := item["provenance"].(map[string]any)
		if prov == nil || prov["url"] != src.URI || prov["sha256"] != srcSHA {
			t.Fatalf("the review item's provenance = %v, want the approved URL and hash", prov)
		}
		if desc, _ := item["description"].(string); !strings.Contains(desc, "asserts your rights") {
			t.Fatalf("the review item description does not name the assertion: %q", desc)
		}
		if item["state"] != "resolved" {
			t.Fatalf("the review item state = %v after the run resumed, want resolved", item["state"])
		}
	}

	// The approval is single-shot: once the run has resumed, approving the
	// same item again is a conflict, not a second grant.
	rec2 := e.do(auth.RoleCurator, http.MethodPost,
		"/api/v1/onboarding/runs/"+runID+"/approve",
		[]byte(`{"review_item_id":"`+callID+`"}`))
	if rec2.Code != http.StatusConflict {
		t.Fatalf("second approve = %d, want 409 (body %s)", rec2.Code, rec2.Body.String())
	}
}
