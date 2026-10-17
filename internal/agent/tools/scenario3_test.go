package tools

// Behavioral acceptance scenarios 7-10 (docs/spec/03 "Behavioral acceptance
// scenarios") plus the approval-token gate for publish_profile.

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/agent"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/providers/openai"
)

// Scenario 7: an interrupted 3,000-line import resumes without duplicates.
// The crash is simulated by Deps.CrashSim panicking after the import's
// writes but before the durable tool-outcome row — the pending-row shape a
// real process death leaves. The resume must execute the pending call
// exactly once and the idempotent import must reconcile, not duplicate.
func TestScenario_7_InterruptedImportResumesWithoutDuplicates(t *testing.T) {
	const lines = 3000
	e := newEnv(t)
	g := e.game("synthetic-tide", "Synthetic Tide")
	p := e.draft(e.edition(g, "US", "en", "v1").ID)
	sha := e.stageBytes([]byte(genCSV(lines)))

	var crashOnce sync.Once
	e.deps.CrashSim = func() {
		crashOnce.Do(func() { panic("simulated process crash after import writes") })
	}
	e.tools(&fakeSearch{}, nil)

	runID := games.NewID()
	model1 := &scriptedModel{responses: []scriptedResponse{
		callScripted("c1", "import_user_asset", map[string]any{
			"profile_id": p.ID, "staged_asset_sha256": sha,
			"role": "dialogue_script", "rights": "user_supplied",
			"file_name": fmt.Sprintf("big-script-%d.csv", lines),
		}),
	}}
	crashed := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				crashed = true
			} else {
				t.Fatal("expected the simulated crash to propagate out of the run")
			}
		}()
		e.runAgent(t, model1, runID, "Import the 3000-line script.", 10)
	}()
	if !crashed {
		t.Fatal("the crash did not fire")
	}

	// Process restart: close and reopen the appliance; rebuild the tools
	// (the crash hook is gone with the old process).
	e.close()
	e.open()
	e.tools(&fakeSearch{}, nil)

	model2 := &scriptedModel{responses: []scriptedResponse{
		{text: fmt.Sprintf("Import complete: %d lines, no duplicates.", lines)},
	}}
	out := e.runAgent(t, model2, runID, "", 10)
	t.Logf("Scenario 7: resume state=%s stop_reason=%s steps=%d", out.State, out.StopReason, out.Steps)
	if out.State != agent.StateSucceeded {
		t.Fatalf("resumed state = %s (reason %s), want succeeded", out.State, out.StopReason)
	}

	// Exactly the 3,000 lines, each with a unique source-side id: no
	// duplicates from the re-executed call.
	msgs, err := e.deps.listMessages(e.ctx, p.ID)
	if err != nil {
		t.Fatalf("listMessages: %v", err)
	}
	if len(msgs) != lines {
		t.Fatalf("messages = %d, want exactly %d (no duplicates from the resume)", len(msgs), lines)
	}
	unique := map[string]bool{}
	for _, m := range msgs {
		if m.SourceMessageID == "" {
			t.Fatalf("message %s has no source-side identity", m.ID)
		}
		unique[m.SourceMessageID] = true
	}
	if len(unique) != lines {
		t.Fatalf("unique source_message_ids = %d, want %d", len(unique), lines)
	}

	// One source row: provenance was reused, not re-registered.
	sources, err := e.store.ListSources(e.ctx, p.ID)
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	if len(sources) != 1 {
		t.Fatalf("sources = %d, want 1 (reused on the re-executed import)", len(sources))
	}

	// The pending call got exactly one durable outcome.
	rows, err := e.agent.ListAgentToolCalls(e.ctx, runID)
	if err != nil {
		t.Fatalf("ListAgentToolCalls: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("tool call rows = %d, want 1", len(rows))
	}
	if rows[0].Status != "succeeded" {
		t.Fatalf("resumed import status = %s, want succeeded", rows[0].Status)
	}
}

// Scenario 8: duplicated source text at two locations -> both occurrences
// are retained with distinct ids.
func TestScenario_8_DuplicateTextKeptDistinct(t *testing.T) {
	e := newEnv(t)
	g := e.game("synthetic-tide", "Synthetic Tide")
	p := e.draft(e.edition(g, "US", "en", "v1").ID)
	sha := e.stageBytes([]byte(dupCSV))
	e.tools(&fakeSearch{}, nil)

	model := &scriptedModel{responses: []scriptedResponse{
		callScripted("c1", "import_user_asset", map[string]any{
			"profile_id": p.ID, "staged_asset_sha256": sha,
			"role": "dialogue_script", "rights": "user_supplied",
			"file_name": "dup-lines.csv",
		}),
		{text: "Imported; the repeated line was kept in both places."},
	}}
	runID := games.NewID()
	out := e.runAgent(t, model, runID, "Import my script.", 6)
	t.Logf("Scenario 8: state=%s stop_reason=%s", out.State, out.StopReason)
	if out.State != agent.StateSucceeded {
		t.Fatalf("state = %s, want succeeded", out.State)
	}

	msgs, err := e.deps.listMessages(e.ctx, p.ID)
	if err != nil {
		t.Fatalf("listMessages: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3 (both duplicates retained)", len(msgs))
	}
	var dupIDs []string
	for _, m := range msgs {
		if strings.TrimSpace(m.OriginalText) == "The same words, in two places." {
			dupIDs = append(dupIDs, m.ID)
		}
	}
	if len(dupIDs) != 2 || dupIDs[0] == dupIDs[1] {
		t.Fatalf("duplicated text occurrences = %v, want two distinct message rows", dupIDs)
	}
}

// Scenario 9: the user supplies their own script and asks for no web -> the
// run completes with zero web_search/fetch_page calls.
func TestScenario_9_UserScriptNoWebCalls(t *testing.T) {
	e := newEnv(t)
	g := e.game("synthetic-tide", "Synthetic Tide")
	p := e.draft(e.edition(g, "US", "en", "v1").ID)
	sha := e.stageBytes([]byte(smallCSV))
	fs := &fakeSearch{}
	e.tools(fs, nil)

	model := &scriptedModel{responses: []scriptedResponse{
		callScripted("c1", "import_user_asset", map[string]any{
			"profile_id": p.ID, "staged_asset_sha256": sha,
			"role": "dialogue_script", "rights": "user_supplied",
			"file_name": "my-own-script.csv",
		}),
	}}
	model.setResponder(func(conv []openai.Message) scriptedResponse {
		if !hasToolMsg(conv, "c2") {
			src := toolFieldByCall(conv, "c1", "source_id")
			return callScripted("c2", "source_quality", map[string]any{"profile_id": p.ID, "source_id": src})
		}
		return scriptedResponse{text: "Indexed your script with no web access."}
	})
	runID := games.NewID()
	out := e.runAgent(t, model, runID, "Use my uploaded script as-is; do NOT use the web.", 8)
	t.Logf("Scenario 9: state=%s stop_reason=%s", out.State, out.StopReason)
	if out.State != agent.StateSucceeded {
		t.Fatalf("state = %s, want succeeded", out.State)
	}
	if fs.queryCount() != 0 {
		t.Fatalf("search provider was called %d time(s); the user asked for no web", fs.queryCount())
	}
	rows, err := e.agent.ListAgentToolCalls(e.ctx, runID)
	if err != nil {
		t.Fatalf("ListAgentToolCalls: %v", err)
	}
	for _, r := range rows {
		if r.Name == "web_search" || r.Name == "fetch_page" {
			t.Fatalf("tool log contains %s: no web calls were requested", r.Name)
		}
	}
}

// Scenario 10: a generated source contradicts a directly extracted message
// id -> the extracted row wins and the conflict is noted (the generated row
// is quarantined, the extracted row is clean).
func TestScenario_10_ExtractedWinsOverGenerated(t *testing.T) {
	e := newEnv(t)
	g := e.game("synthetic-tide", "Synthetic Tide")
	p := e.draft(e.edition(g, "US", "en", "v1").ID)

	// Source A: the directly extracted user file (trust 1.0).
	srcA := e.importDirect(t, p.ID, "script-v1.csv", conflictCSVv1, games.RightsUserSupplied)
	// Source B: a fetched/generated source (trust 0.4) whose m-001 text
	// disagrees. The message row simulates an earlier generation step.
	srcB, err := registerWebSource(e.ctx, e.deps, p.ID, games.SourceWebPage, "https://example/generated-summary", "")
	if err != nil {
		t.Fatalf("registerWebSource: %v", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	msgB := games.Message{
		ID: games.NewID(), ProfileID: p.ID, SourceID: srcB,
		SourceMessageID:   "m-001",
		OriginalText:      "A generated reading that disagrees with the extracted line.",
		NormalizedText:    "A generated reading that disagrees with the extracted line.",
		TextKind:          games.TextDialogue,
		AttributionStatus: games.AttributionUnknown,
		Revision:          1,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if err := msgB.Validate(); err != nil {
		t.Fatalf("fixture message invalid: %v", err)
	}
	if err := e.store.PutMessages(e.ctx, p.ID, []games.Message{msgB}, nil); err != nil {
		t.Fatalf("PutMessages: %v", err)
	}
	e.tools(&fakeSearch{}, nil)

	model := &scriptedModel{responses: []scriptedResponse{
		callScripted("c1", "source_quality", map[string]any{"profile_id": p.ID, "source_id": srcA}),
	}}
	runID := games.NewID()
	out := e.runAgent(t, model, runID, "Check the source quality of my script.", 6)
	t.Logf("Scenario 10: state=%s stop_reason=%s", out.State, out.StopReason)
	if out.State != agent.StateReview {
		t.Fatalf("state = %s (reason %s), want review (conflict noted)", out.State, out.StopReason)
	}

	// The report names the extracted row as preferred.
	rows, err := e.agent.ListAgentToolCalls(e.ctx, runID)
	if err != nil {
		t.Fatalf("ListAgentToolCalls: %v", err)
	}
	var res agent.Result
	for _, r := range rows {
		if r.Name == "source_quality" {
			if err := json.Unmarshal(r.ResultJSON, &res); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
		}
	}
	var rep struct {
		Conflicts []struct {
			SourceMessageID    string `json:"source_message_id"`
			PreferredMessageID string `json:"preferred_message_id"`
			PreferredSourceID  string `json:"preferred_source_id"`
		} `json:"conflicts"`
	}
	if err := json.Unmarshal(res.Results, &rep); err != nil {
		t.Fatalf("unmarshal report: %v", err)
	}
	if len(rep.Conflicts) != 1 || rep.Conflicts[0].SourceMessageID != "m-001" {
		t.Fatalf("conflicts = %+v, want the one m-001 mismatch", rep.Conflicts)
	}
	if rep.Conflicts[0].PreferredSourceID != srcA {
		t.Fatalf("preferred source = %s, want the extracted user file %s", rep.Conflicts[0].PreferredSourceID, srcA)
	}

	// The generated row is quarantined; the extracted one is clean.
	var extracted, generated *games.Message
	msgs, err := e.deps.listMessages(e.ctx, p.ID)
	if err != nil {
		t.Fatalf("listMessages: %v", err)
	}
	for i := range msgs {
		if msgs[i].SourceID == srcA && msgs[i].SourceMessageID == "m-001" {
			extracted = &msgs[i]
		}
		if msgs[i].ID == msgB.ID {
			generated = &msgs[i]
		}
	}
	flagIsQuarantined := func(m games.Message) bool {
		var flags map[string]any
		_ = json.Unmarshal(m.Flags, &flags)
		v, _ := flags["quarantined"].(bool)
		return v
	}
	if extracted == nil || generated == nil {
		t.Fatalf("could not find both conflicting rows (extracted=%v generated=%v)", extracted, generated)
	}
	if flagIsQuarantined(*extracted) {
		t.Fatal("the extracted row was quarantined; it must win (docs/spec/03 scenario 10)")
	}
	if !flagIsQuarantined(*generated) {
		t.Fatal("the generated row is not quarantined")
	}
	if rep.Conflicts[0].PreferredMessageID != extracted.ID {
		t.Fatalf("preferred message = %s, want the extracted row %s", rep.Conflicts[0].PreferredMessageID, extracted.ID)
	}
}

// publishProfile is the approval boundary: refused without a token, refused
// for a spent token, and only then does it publish (through review). The
// published revision is then immutable.
func TestPublishProfileRequiresHumanApprovalToken(t *testing.T) {
	e := newEnv(t)
	g := e.game("synthetic-tide", "Synthetic Tide")
	p := e.draft(e.edition(g, "US", "en", "v1").ID)
	e.importDirect(t, p.ID, "synthetic-adventure.csv", smallCSV, games.RightsUserSupplied)
	e.tools(&fakeSearch{}, nil)

	// A human (this test stands in for the UI approval step) moves the
	// revision to review and issues a token.
	_, rowVersion, err := e.store.GetProfile(e.ctx, p.ID)
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	err = e.store.SetProfileStatus(e.ctx, p.ID, games.StatusReview, rowVersion)
	if err != nil {
		t.Fatalf("SetProfileStatus: %v", err)
	}
	approval, err := e.deps.Approvals.Issue(e.ctx, p.ID, "test-human")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	// Run 1: a bogus token must be refused; the run stops without publishing
	// (publish_profile has one attempt per run — a failed publish is a stop
	// for a human to fix the situation, not a loop for the agent).
	model1 := &scriptedModel{responses: []scriptedResponse{
		callScripted("c1", "publish_profile", map[string]any{"profile_id": p.ID, "approval_token": "gnapp_deadbeef"}),
		{text: "Publishing was refused: the approval token is invalid. A human must approve."},
	}}
	run1 := games.NewID()
	out1 := e.runAgent(t, model1, run1, "Publish the profile now.", 6)
	t.Logf("Publish gate: run1 state=%s stop_reason=%s", out1.State, out1.StopReason)
	if out1.State != agent.StateSucceeded {
		t.Fatalf("run1 state = %s (reason %s), want succeeded (a refused publish ends the run, the store is untouched)", out1.State, out1.StopReason)
	}
	stuck, _, err := e.store.GetProfile(e.ctx, p.ID)
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	if stuck.Status != games.StatusReview {
		t.Fatalf("profile status = %s, want still review after a refused publish", stuck.Status)
	}

	// Run 2: the real token (a fresh run, so the one-attempt budget is clean).
	model2 := &scriptedModel{responses: []scriptedResponse{
		callScripted("p1", "publish_profile", map[string]any{"profile_id": p.ID, "approval_token": approval.Token}),
		{text: "Published."},
	}}
	run2 := games.NewID()
	out2 := e.runAgent(t, model2, run2, "Publish the profile now.", 6)
	t.Logf("Publish gate: run2 state=%s stop_reason=%s", out2.State, out2.StopReason)
	if out2.State != agent.StateSucceeded {
		t.Fatalf("run2 state = %s (reason %s), want succeeded", out2.State, out2.StopReason)
	}

	var refused, published agent.Result
	for _, rid := range []string{run1, run2} {
		rows, err := e.agent.ListAgentToolCalls(e.ctx, rid)
		if err != nil {
			t.Fatalf("ListAgentToolCalls(%s): %v", rid, err)
		}
		for _, r := range rows {
			if r.Name != "publish_profile" {
				continue
			}
			var res agent.Result
			if err := json.Unmarshal(r.ResultJSON, &res); err != nil {
				t.Fatalf("unmarshal publish result: %v", err)
			}
			if res.Status == agent.StatusFailed {
				refused = res
			} else if res.Status == agent.StatusSucceeded {
				published = res
			}
		}
	}
	if refused.Status != agent.StatusFailed {
		t.Fatalf("bogus-token publish was not refused: %+v", refused)
	}
	if !strings.Contains(refused.Error, "invalid approval token") {
		t.Fatalf("refusal reason = %q", refused.Error)
	}
	if published.Status != agent.StatusSucceeded {
		t.Fatalf("valid-token publish did not succeed: %+v", published)
	}
	final, rowVersion2, err := e.store.GetProfile(e.ctx, p.ID)
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	if final.Status != games.StatusPublished {
		t.Fatalf("profile status = %s, want published", final.Status)
	}
	if rowVersion2 != rowVersion+2 {
		t.Fatalf("row version = %d, want %d (review move + publish)", rowVersion2, rowVersion+2)
	}

	// Single-use: the same token cannot be consumed a second time.
	if _, err := e.deps.Approvals.Consume(e.ctx, approval.Token); err == nil {
		t.Fatal("spent token consumed twice")
	}

	// Published revisions are immutable: a new draft revision is the only
	// path back to editing.
	if err := e.store.SetProfileStatus(e.ctx, p.ID, games.StatusDraft, rowVersion2); err == nil {
		t.Fatal("published revision accepted a status move back to draft")
	}
}

// MemoryApprovals negative paths: unknown token, then single-use.
func TestMemoryApprovalsSingleUse(t *testing.T) {
	e := newEnv(t)
	g := e.game("synthetic-tide", "Synthetic Tide")
	p := e.draft(e.edition(g, "US", "en", "v1").ID)
	s := NewMemoryApprovals()
	a, err := s.Issue(e.ctx, p.ID, "tester")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := s.Consume(e.ctx, "gnapp_wrong"); err == nil {
		t.Fatal("unknown token accepted")
	}
	if _, err := s.Consume(e.ctx, a.Token); err != nil {
		t.Fatalf("fresh token not consumable: %v", err)
	}
	if _, err := s.Consume(e.ctx, a.Token); err == nil {
		t.Fatal("token spent twice")
	}
}

// SQLiteApprovals (migration 0008): issue, consume once, then both the
// double-spend and the unknown-token paths fail durably. The raw token is
// never handed to the store — CreateApproval receives only its SHA-256.
func TestSQLiteApprovalsSingleUse(t *testing.T) {
	e := newEnv(t)
	g := e.game("synthetic-tide", "Synthetic Tide")
	p := e.draft(e.edition(g, "US", "en", "v1").ID)
	s := NewSQLiteApprovals(e.store)

	a, err := s.Issue(e.ctx, p.ID, "test-human")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if a.Token == "" {
		t.Fatal("Issue did not return the raw token")
	}

	got, err := s.Consume(e.ctx, a.Token)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if got.ProfileID != p.ID {
		t.Fatalf("consumed approval profile = %s, want %s", got.ProfileID, p.ID)
	}
	if _, err := s.Consume(e.ctx, a.Token); err == nil {
		t.Fatal("token consumed twice from SQLite")
	}
	if _, err := s.Consume(e.ctx, "gnapp_neverissued"); err == nil {
		t.Fatal("unknown token accepted from SQLite")
	}
}
