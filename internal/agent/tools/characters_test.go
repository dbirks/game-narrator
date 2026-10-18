package tools

// Unit tests for the research-grounded character chain (ADR-0046):
// stage_character stages a researched character into the draft roster with
// source grounding, and analyze_character falls back to a research-grounded
// brief when the character has no cited lines. Synthetic names and sources
// only (test category (a) synthetic stubs per AGENTS.md); the live chain has
// its own run in internal/e2e.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/agent"
	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/providers/openai"
)

const researchBrief = `{"vocal_age_impression":"mid-20s","timbre":"clear, bright, slightly husky","cadence":"eager, quick when excited","emotional_baseline":"curious and brave","language_notes":"informal contractions","pronunciation_notes":"clean d and t"}`

// webSource registers one synthetic web_page source row on the profile the
// same way web_search/fetch_page do.
func (e *env) webSource(t *testing.T, profileID, uri string) string {
	t.Helper()
	id, err := registerWebSource(e.ctx, e.deps, profileID, games.SourceWebPage, uri, "")
	if err != nil {
		t.Fatalf("registerWebSource: %v", err)
	}
	return id
}

func TestStageCharacterStagesResearchGroundedCharacter(t *testing.T) {
	e := newEnv(t)
	g := e.game("synthetic-tide", "Synthetic Tide")
	p := e.draft(e.edition(g, "US", "en", "v1").ID)
	s1 := e.webSource(t, p.ID, "https://wiki.example/synthetic-tide/characters")
	s2 := e.webSource(t, p.ID, "https://wiki.example/synthetic-tide/roles")

	e.tools(&fakeSearch{}, nil)
	exec := agent.NewExecutor(e.reg, auth.RoleCurator, 16)
	res := exec.Execute(e.ctx, agent.Call{
		ID:   "stage-1",
		Name: "stage_character",
		Arguments: json.RawMessage(fmt.Sprintf(
			`{"profile_id":%q,"name":"Marla","role":"village captain","personality_summary":"A practical, dry-humoured leader who speaks in short orders.","source_ids":[%q,%q]}`,
			p.ID, s1, s2)),
	})
	if res.Status != agent.StatusSucceeded {
		t.Fatalf("stage_character = %s: %s", res.Status, res.Error)
	}
	var out struct {
		CharacterID string   `json:"character_id"`
		Name        string   `json:"name"`
		Status      string   `json:"status"`
		Updated     bool     `json:"updated"`
		SourceIDs   []string `json:"source_ids"`
	}
	if err := json.Unmarshal(res.Results, &out); err != nil {
		t.Fatalf("results are not valid JSON: %v: %s", err, res.Results)
	}
	if out.Updated {
		t.Error("a first stage was reported as an update")
	}
	if len(res.SourceIDs) != 2 {
		t.Fatalf("envelope source_ids = %v, want the two staged sources", res.SourceIDs)
	}

	chars, err := e.store.ListCharacters(e.ctx, p.ID)
	if err != nil {
		t.Fatalf("ListCharacters: %v", err)
	}
	if len(chars) != 1 {
		t.Fatalf("roster = %d rows, want 1", len(chars))
	}
	ch := chars[0]
	if ch.ID != out.CharacterID || ch.Status != games.CharacterProposed {
		t.Fatalf("row = id %s status %s, want the staged id in the proposed state", ch.ID, ch.Status)
	}
	var refs []string
	if err := json.Unmarshal(ch.VerifiedSourceRefs, &refs); err != nil || len(refs) != 2 {
		t.Fatalf("verified_source_refs = %s (err %v), want the two staged source ids", ch.VerifiedSourceRefs, err)
	}
	if ch.PersonalitySummary == "" {
		t.Error("the researched personality summary was not recorded")
	}
}

func TestStageCharacterRefusesUnregisteredSource(t *testing.T) {
	e := newEnv(t)
	g := e.game("synthetic-tide", "Synthetic Tide")
	p := e.draft(e.edition(g, "US", "en", "v1").ID)
	s1 := e.webSource(t, p.ID, "https://wiki.example/synthetic-tide/characters")
	ghost := games.NewID()

	e.tools(&fakeSearch{}, nil)
	exec := agent.NewExecutor(e.reg, auth.RoleCurator, 16)
	res := exec.Execute(e.ctx, agent.Call{
		ID:   "stage-ghost",
		Name: "stage_character",
		Arguments: json.RawMessage(fmt.Sprintf(
			`{"profile_id":%q,"name":"Marla","source_ids":[%q,%q]}`, p.ID, s1, ghost)),
	})
	if res.Status == agent.StatusSucceeded {
		t.Fatalf("staging with an unregistered source succeeded: %s", res.Results)
	}
	if !strings.Contains(res.Error, "is not registered on profile") {
		t.Fatalf("refusal = %q, want the unregistered-source reason", res.Error)
	}
	chars, err := e.store.ListCharacters(e.ctx, p.ID)
	if err != nil {
		t.Fatalf("ListCharacters: %v", err)
	}
	if len(chars) != 0 {
		t.Fatalf("a refused stage left %d roster rows", len(chars))
	}
}

func TestStageCharacterRestageUpdatesInPlace(t *testing.T) {
	e := newEnv(t)
	g := e.game("synthetic-tide", "Synthetic Tide")
	p := e.draft(e.edition(g, "US", "en", "v1").ID)
	s1 := e.webSource(t, p.ID, "https://wiki.example/synthetic-tide/characters")
	s2 := e.webSource(t, p.ID, "https://wiki.example/synthetic-tide/roles")

	e.tools(&fakeSearch{}, nil)
	exec := agent.NewExecutor(e.reg, auth.RoleCurator, 16)
	stage := func(id, name, summary string, sources ...string) agent.Result {
		b, _ := json.Marshal(map[string]any{
			"profile_id": p.ID, "name": name, "personality_summary": summary, "source_ids": sources,
		})
		return exec.Execute(e.ctx, agent.Call{ID: id, Name: "stage_character", Arguments: b})
	}
	first := stage("stage-a", "Fen", "A quiet cartographer.", s1)
	if first.Status != agent.StatusSucceeded {
		t.Fatalf("first stage: %s: %s", first.Status, first.Error)
	}
	var a struct {
		CharacterID string `json:"character_id"`
		Updated     bool   `json:"updated"`
	}
	_ = json.Unmarshal(first.Results, &a)

	// Same name (different case) with a second source: an update, not a fork.
	second := stage("stage-b", "fen", "A quiet cartographer with a dry sense of humour.", s1, s2)
	if second.Status != agent.StatusSucceeded {
		t.Fatalf("restage: %s: %s", second.Status, second.Error)
	}
	var b struct {
		CharacterID string   `json:"character_id"`
		Updated     bool     `json:"updated"`
		SourceIDs   []string `json:"source_ids"`
	}
	_ = json.Unmarshal(second.Results, &b)
	if !b.Updated {
		t.Error("restaging the same name was not reported as an update")
	}
	if b.CharacterID != a.CharacterID {
		t.Fatalf("restage minted a new id %s (was %s): the roster forked", b.CharacterID, a.CharacterID)
	}
	chars, err := e.store.ListCharacters(e.ctx, p.ID)
	if err != nil {
		t.Fatalf("ListCharacters: %v", err)
	}
	if len(chars) != 1 {
		t.Fatalf("roster = %d rows after a restage, want 1", len(chars))
	}
	if len(b.SourceIDs) != 2 {
		t.Fatalf("restage source_ids = %v, want both sources", b.SourceIDs)
	}
}

// analyzeCharacter research mode: no lines on the profile, staged source
// grounding present -> the brief is built from the researched facts and the
// prompt carries the source metadata, never page content.
func TestAnalyzeCharacterResearchGroundedBrief(t *testing.T) {
	e := newEnv(t)
	g := e.game("synthetic-tide", "Synthetic Tide")
	p := e.draft(e.edition(g, "US", "en", "v1").ID)
	s1 := e.webSource(t, p.ID, "https://wiki.example/synthetic-tide/characters")
	s2 := e.webSource(t, p.ID, "https://wiki.example/synthetic-tide/roles")

	e.tools(&fakeSearch{}, nil)
	exec := agent.NewExecutor(e.reg, auth.RoleCurator, 16)
	stageArgs, _ := json.Marshal(map[string]any{
		"profile_id": p.ID, "name": "Marla", "role": "village captain",
		"personality_summary": "Practical, dry-humoured leader; short orders; rarely raises her voice.",
		"source_ids":          []string{s1, s2},
	})
	stageRes := exec.Execute(e.ctx, agent.Call{ID: "stage-1", Name: "stage_character", Arguments: stageArgs})
	if stageRes.Status != agent.StatusSucceeded {
		t.Fatalf("stage: %s: %s", stageRes.Status, stageRes.Error)
	}
	var staged struct {
		CharacterID string `json:"character_id"`
	}
	if err := json.Unmarshal(stageRes.Results, &staged); err != nil || staged.CharacterID == "" {
		t.Fatalf("stage results: %s (err %v)", stageRes.Results, err)
	}

	var gotReq openai.ChatRequest
	e.deps.Chat = func(ctx context.Context, req openai.ChatRequest) (openai.ChatResponse, error) {
		gotReq = req
		return openai.ChatResponse{Text: "Here is the brief:\n```json\n" + researchBrief + "\n```", FinishReason: "stop"}, nil
	}

	res, err := e.deps.analyzeCharacter(e.ctx, agent.Call{
		ID:        "analyze-1",
		Name:      "analyze_character",
		Arguments: json.RawMessage(fmt.Sprintf(`{"profile_id":%q,"character_id":%q}`, p.ID, staged.CharacterID)),
	})
	if err != nil {
		t.Fatalf("analyze_character: %v", err)
	}
	var out struct {
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
	if err := json.Unmarshal(res.Results, &out); err != nil {
		t.Fatalf("results are not valid JSON: %v: %s", err, res.Results)
	}
	if out.Grounding.Mode != "researched_character" {
		t.Fatalf("grounding.mode = %q, want researched_character", out.Grounding.Mode)
	}
	if len(out.Grounding.SourceIDs) != 2 {
		t.Fatalf("grounding.source_ids = %v, want the two staged sources", out.Grounding.SourceIDs)
	}
	if len(out.CitedLines) != 0 {
		t.Fatalf("cited_lines = %v, want none in research mode", out.CitedLines)
	}
	for _, f := range voiceBriefFields {
		field, ok := out.VoiceBrief[f]
		if !ok {
			t.Fatalf("voice_brief is missing %q", f)
		}
		if !field.Creative {
			t.Errorf("voice_brief.%s is not labelled creative:true", f)
		}
		if s, _ := field.Value.(string); strings.TrimSpace(s) == "" {
			t.Errorf("voice_brief.%s is empty", f)
		}
	}
	if len(res.SourceIDs) != 2 {
		t.Fatalf("envelope source_ids = %v, want the two staged sources", res.SourceIDs)
	}

	// The prompt carries the source metadata as evidence; it must not quote
	// page content (the store never had any: metadata only by construction).
	var userText string
	for _, m := range gotReq.Messages {
		if m.Role == agent.RoleUser {
			userText = m.Text
		}
	}
	if !strings.Contains(userText, "Evidence sources") || !strings.Contains(userText, s1) {
		t.Fatalf("the research prompt does not name the evidence sources: %q", userText)
	}
	if !gotReq.ResponseFormatJSON || !gotReq.JSONModeFallback {
		t.Error("research mode must keep the JSON-mode fallback request flags")
	}
}

// The model answers the brief prompt with the wrapper shape
// {"voice_brief": {...}} — the shape this tool's own result uses — on the
// degraded JSON path (no response_format on the gateway). The parser must
// accept it: a brief that only parses when unwrapped is still the brief.
func TestAnalyzeCharacterAcceptsWrappedBrief(t *testing.T) {
	e := newEnv(t)
	g := e.game("synthetic-tide", "Synthetic Tide")
	p := e.draft(e.edition(g, "US", "en", "v1").ID)
	s1 := e.webSource(t, p.ID, "https://wiki.example/synthetic-tide/characters")
	s2 := e.webSource(t, p.ID, "https://wiki.example/synthetic-tide/roles")

	e.tools(&fakeSearch{}, nil)
	exec := agent.NewExecutor(e.reg, auth.RoleCurator, 16)
	stageArgs, _ := json.Marshal(map[string]any{
		"profile_id": p.ID, "name": "Marla", "role": "village captain",
		"personality_summary": "Practical, dry-humoured leader.",
		"source_ids":          []string{s1, s2},
	})
	stageRes := exec.Execute(e.ctx, agent.Call{ID: "stage-1", Name: "stage_character", Arguments: stageArgs})
	if stageRes.Status != agent.StatusSucceeded {
		t.Fatalf("stage: %s: %s", stageRes.Status, stageRes.Error)
	}
	var staged struct {
		CharacterID string `json:"character_id"`
	}
	if err := json.Unmarshal(stageRes.Results, &staged); err != nil || staged.CharacterID == "" {
		t.Fatalf("stage results: %s (err %v)", stageRes.Results, err)
	}

	wrapped := `{"voice_brief":` + researchBrief + `}`
	e.deps.Chat = func(ctx context.Context, _ openai.ChatRequest) (openai.ChatResponse, error) {
		return openai.ChatResponse{Text: wrapped, FinishReason: "stop"}, nil
	}

	res, err := e.deps.analyzeCharacter(e.ctx, agent.Call{
		ID:        "analyze-wrapped",
		Name:      "analyze_character",
		Arguments: json.RawMessage(fmt.Sprintf(`{"profile_id":%q,"character_id":%q}`, p.ID, staged.CharacterID)),
	})
	if err != nil {
		t.Fatalf("analyze_character: %v", err)
	}
	var out struct {
		VoiceBrief map[string]struct {
			Value any `json:"value"`
		} `json:"voice_brief"`
	}
	if err := json.Unmarshal(res.Results, &out); err != nil {
		t.Fatalf("results are not valid JSON: %v: %s", err, res.Results)
	}
	for _, f := range voiceBriefFields {
		field, ok := out.VoiceBrief[f]
		if !ok {
			t.Fatalf("voice_brief is missing %q after unwrap", f)
		}
		if s, _ := field.Value.(string); strings.TrimSpace(s) == "" {
			t.Errorf("voice_brief.%s is empty after unwrap", f)
		}
	}

	// A top-level object with neither the fields nor a voice_brief wrapper
	// is still a parse failure, not a silent empty brief.
	e.deps.Chat = func(ctx context.Context, _ openai.ChatRequest) (openai.ChatResponse, error) {
		return openai.ChatResponse{Text: `{"summary":"a character"}`, FinishReason: "stop"}, nil
	}
	res2, err := e.deps.analyzeCharacter(e.ctx, agent.Call{
		ID:        "analyze-noshape",
		Name:      "analyze_character",
		Arguments: json.RawMessage(fmt.Sprintf(`{"profile_id":%q,"character_id":%q}`, p.ID, staged.CharacterID)),
	})
	if err == nil && res2.Status == agent.StatusSucceeded {
		t.Fatalf("a brief with none of the expected fields was accepted: %s", res2.Results)
	}
}

// The lines-first rule survives: a character with neither lines nor staging
// grounding still fails with the import-a-script instruction.
func TestAnalyzeCharacterWithoutLinesOrGroundingFails(t *testing.T) {
	e := newEnv(t)
	g := e.game("synthetic-tide", "Synthetic Tide")
	p := e.draft(e.edition(g, "US", "en", "v1").ID)
	ch := games.Character{
		ID: games.NewID(), ProfileID: p.ID, Name: "Ghost",
		Aliases: []byte(`[]`), VisualDescriptors: []byte(`[]`),
		Status:    games.CharacterProposed,
		CreatedAt: nowStamp(),
	}
	if err := e.store.PutCharacters(e.ctx, p.ID, []games.Character{ch}); err != nil {
		t.Fatalf("PutCharacters: %v", err)
	}
	e.tools(&fakeSearch{}, nil)
	// A chat is wired so the check order reaches the evidence rule (the
	// failure under test is "no lines and no grounding", not "no model").
	e.deps.Chat = fakeChat(researchBrief)

	_, err := e.deps.analyzeCharacter(e.ctx, agent.Call{
		ID:        "analyze-ghost",
		Name:      "analyze_character",
		Arguments: json.RawMessage(fmt.Sprintf(`{"profile_id":%q,"character_id":%q}`, p.ID, ch.ID)),
	})
	if err == nil {
		t.Fatal("analyze_character succeeded for a character with no lines and no grounding")
	}
	if !strings.Contains(err.Error(), "import a labelled script first") {
		t.Fatalf("failure = %q, want the lines-first instruction", err.Error())
	}
}

// Regression: a character with cited lines still gets the lines-grounded
// brief, now with the grounding block naming that mode.
func TestAnalyzeCharacterCitedLinesGrounding(t *testing.T) {
	e := newEnv(t)
	g := e.game("synthetic-tide", "Synthetic Tide")
	p := e.draft(e.edition(g, "US", "en", "v1").ID)
	e.importDirect(t, p.ID, "synthetic-adventure.csv", smallCSV, games.RightsUserSupplied)
	chars, err := e.store.ListCharacters(e.ctx, p.ID)
	if err != nil {
		t.Fatalf("ListCharacters: %v", err)
	}
	var marla games.Character
	for _, c := range chars {
		if c.Name == "Marla" {
			marla = c
		}
	}
	if marla.ID == "" {
		t.Fatal("the import did not create the Marla character row")
	}
	e.tools(&fakeSearch{}, nil)
	e.deps.Chat = func(ctx context.Context, req openai.ChatRequest) (openai.ChatResponse, error) {
		return openai.ChatResponse{Text: researchBrief, FinishReason: "stop"}, nil
	}

	res, err := e.deps.analyzeCharacter(e.ctx, agent.Call{
		ID:        "analyze-lines",
		Name:      "analyze_character",
		Arguments: json.RawMessage(fmt.Sprintf(`{"profile_id":%q,"character_id":%q}`, p.ID, marla.ID)),
	})
	if err != nil {
		t.Fatalf("analyze_character: %v", err)
	}
	var out struct {
		CitedLines []map[string]any `json:"cited_lines"`
		Grounding  struct {
			Mode      string   `json:"mode"`
			SourceIDs []string `json:"source_ids"`
		} `json:"grounding"`
	}
	if err := json.Unmarshal(res.Results, &out); err != nil {
		t.Fatalf("results are not valid JSON: %v: %s", err, res.Results)
	}
	if out.Grounding.Mode != "cited_lines" || len(out.CitedLines) == 0 {
		t.Fatalf("grounding = %+v, want cited_lines mode with the character's lines", out.Grounding)
	}
	if len(out.Grounding.SourceIDs) != 0 {
		t.Fatalf("cited-lines mode carries source ids %v, want none (lines are the evidence)", out.Grounding.SourceIDs)
	}
}
