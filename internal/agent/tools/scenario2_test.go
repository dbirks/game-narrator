package tools

// Behavioral acceptance scenarios 4-6 (docs/spec/03 "Behavioral acceptance
// scenarios").

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/dbirks/game-narrator/internal/agent"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/profileio"
	"github.com/dbirks/game-narrator/internal/providers/openai"
)

// importDirect is a test setup step (not a tool): it streams content through
// the real profileio import path so scenarios can start from an imported
// profile without spending the run's scripted steps on it.
func (e *env) importDirect(t *testing.T, profileID, fileName, content string, rights games.RightsStatus) string {
	t.Helper()
	report, err := profileio.ImportFile(e.ctx, e.deps.profileIODeps(), profileio.ImportRequest{
		ProfileID: profileID,
		FileName:  fileName,
		Reader:    stringsReader(content),
		Rights:    rights,
	})
	if err != nil {
		t.Fatalf("setup import: %v", err)
	}
	return report.SourceID
}

// Scenario 4: two sources carry conflicting text for the same
// source_message_id -> the conflict is quarantined (durable flags on both
// rows, no strict winner) and the run ends in review. Nothing is deleted.
func TestScenario_4_ConflictingVersionsQuarantinedReview(t *testing.T) {
	e := newEnv(t)
	g := e.game("synthetic-tide", "Synthetic Tide")
	p := e.draft(e.edition(g, "US", "en", "v1").ID)
	shaV1 := e.stageBytes([]byte(conflictCSVv1))
	shaV2 := e.stageBytes([]byte(conflictCSVv2))
	e.tools(&fakeSearch{}, nil)

	model := &scriptedModel{responses: []scriptedResponse{
		callScripted("c1", "import_user_asset", map[string]any{
			"profile_id": p.ID, "staged_asset_sha256": shaV1,
			"role": "dialogue_script", "rights": "user_supplied",
			"file_name": "script-v1.csv",
		}),
	}}
	model.setResponder(func(conv []openai.Message) scriptedResponse {
		if !hasToolMsg(conv, "c2") {
			return callScripted("c2", "import_user_asset", map[string]any{
				"profile_id": p.ID, "staged_asset_sha256": shaV2,
				"role": "dialogue_script", "rights": "user_supplied",
				"file_name": "script-v2.csv",
			})
		}
		if hasToolMsg(conv, "c3") {
			return scriptedResponse{text: "Both versions imported; the quality report flags a version conflict for review."}
		}
		srcA := toolFieldByCall(conv, "c1", "source_id")
		if srcA == "" {
			return scriptedResponse{err: fmt.Errorf("no source_id for the v1 import")}
		}
		return callScripted("c3", "source_quality", map[string]any{"profile_id": p.ID, "source_id": srcA})
	})
	runID := games.NewID()
	out := e.runAgent(t, model, runID, "Import both script versions and check source quality.", 10)
	t.Logf("Scenario 4: state=%s stop_reason=%s", out.State, out.StopReason)

	if out.State != agent.StateReview {
		t.Fatalf("state = %s (reason %s), want review", out.State, out.StopReason)
	}

	// The quality report names the conflicting id, quarantines every variant
	// (tie: both sources are user files at equal trust), and asks for review.
	rows, err := e.agent.ListAgentToolCalls(e.ctx, runID)
	if err != nil {
		t.Fatalf("ListAgentToolCalls: %v", err)
	}
	var quality agent.Result
	for _, r := range rows {
		if r.Name != "source_quality" {
			continue
		}
		if err := json.Unmarshal(r.ResultJSON, &quality); err != nil {
			t.Fatalf("unmarshal source_quality: %v", err)
		}
	}
	if quality.Status != agent.StatusSucceeded {
		t.Fatalf("source_quality status = %s: %s", quality.Status, quality.Error)
	}
	if !quality.ReviewRequired {
		t.Fatal("source_quality must set review_required on a version mismatch")
	}
	var rep map[string]any
	if err := json.Unmarshal(quality.Results, &rep); err != nil {
		t.Fatalf("unmarshal report: %v", err)
	}
	conflicts, _ := rep["conflicts"].([]any)
	if len(conflicts) != 1 {
		t.Fatalf("conflicts = %d, want the one mismatching id (m-001)", len(conflicts))
	}
	cm, _ := conflicts[0].(map[string]any)
	if cm["source_message_id"] != "m-001" {
		t.Fatalf("conflicting id = %v, want m-001 (m-002 agrees across versions)", cm["source_message_id"])
	}
	if cm["preferred_message_id"] != nil {
		t.Fatalf("preferred_message_id = %v, want none (tie: both rows quarantined)", cm["preferred_message_id"])
	}

	// Durable quarantine: both m-001 rows flagged, the agreeing m-002 rows clean,
	// and all four lines still present (conflict quarantined, nothing deleted).
	msgs, err := e.deps.listMessages(e.ctx, p.ID)
	if err != nil {
		t.Fatalf("listMessages: %v", err)
	}
	if len(msgs) != 4 {
		t.Fatalf("messages = %d, want 4 (both sources kept, nothing deleted)", len(msgs))
	}
	quarantined := 0
	for _, m := range msgs {
		var flags map[string]any
		_ = json.Unmarshal(m.Flags, &flags)
		is, _ := flags["quarantined"].(bool)
		if m.SourceMessageID == "m-001" {
			if !is {
				t.Fatalf("m-001 row %s is not quarantined: %s", m.ID, m.Flags)
			}
			quarantined++
		} else if is {
			t.Fatalf("m-002 row %s wrongly quarantined (the versions agree)", m.ID)
		}
	}
	if quarantined != 2 {
		t.Fatalf("quarantined m-001 rows = %d, want both variants", quarantined)
	}
}

// Scenario 5: "different cover" -> only the artwork candidates change;
// message, voice and asset state are untouched.
func TestScenario_5_ArtworkSwapTouchesOnlyArtwork(t *testing.T) {
	e := newEnv(t)
	g := e.game("synthetic-tide", "Synthetic Tide")
	p := e.draft(e.edition(g, "US", "en", "v1").ID)
	e.importDirect(t, p.ID, "synthetic-adventure.csv", smallCSV, games.RightsUserSupplied)
	e.tools(&fakeSearch{}, nil)

	before := e.profileSnapshot(t, p.ID)
	blobsBefore := blobCount(t, e.assets.Root())

	const newCover = "https://cdn.example/cover-v2.png"
	model := &scriptedModel{responses: []scriptedResponse{
		callScripted("c1", "propose_artwork", map[string]any{
			"profile_id": p.ID, "urls": []string{newCover},
		}),
		{text: "Cover candidates updated; nothing else changed."},
	}}
	runID := games.NewID()
	out := e.runAgent(t, model, runID, "I want a different cover for this game: "+newCover, 8)
	t.Logf("Scenario 5: state=%s stop_reason=%s", out.State, out.StopReason)
	if out.State != agent.StateSucceeded {
		t.Fatalf("state = %s (reason %s), want succeeded", out.State, out.StopReason)
	}

	// Only the artwork candidates changed.
	view := e.deps.Artwork.view(p.ID)
	cands, _ := view["candidates"].([]ArtworkCandidate)
	if len(cands) != 1 || cands[0].URL != newCover {
		t.Fatalf("artwork candidates = %v, want the one new URL", cands)
	}
	if cands[0].State != "candidate_not_published" || cands[0].License != "unknown" {
		t.Fatalf("candidate not labelled: %+v", cands[0])
	}

	// ...and nothing else did: profile content, voice state, asset store.
	after := e.profileSnapshot(t, p.ID)
	if before.msgCount != after.msgCount || before.srcCount != after.srcCount ||
		before.charCount != after.charCount || before.rowVersion != after.rowVersion ||
		before.preferredVoice != after.preferredVoice {
		t.Fatalf("artwork swap changed non-artwork state: before=%+v after=%+v", before, after)
	}
	if got := blobCount(t, e.assets.Root()); got != blobsBefore {
		t.Fatalf("asset store grew (%d -> %d): no artwork bytes may be fetched or stored by the stub", blobsBefore, got)
	}
}

// Scenario 6: "Tetra less stern" on a synthetic character -> only the draft
// voice brief and auditions change across two runs; the approved voice (and
// everything else) remains.
func TestScenario_6_VoiceRedesignKeepsApprovedVoice(t *testing.T) {
	const brief1 = `{"voice_brief":{"vocal_age_impression":"mid-30s","timbre":"stern, low, clipped","cadence":"fast and cutting","emotional_baseline":"cold authority","language_notes":"no contractions","pronunciation_notes":"hard stops"}}`
	const brief2 = `{"voice_brief":{"vocal_age_impression":"mid-30s","timbre":"warm, rounded, steadier","cadence":"measured, unhurried","emotional_baseline":"gentle, dry humour","language_notes":"soft contractions","pronunciation_notes":"relaxed, softer consonants"}}`

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

	before := e.profileSnapshot(t, p.ID)
	blobsBefore := blobCount(t, e.assets.Root())

	// Run 1: brief v1 + 3 auditions. design_voice ends the run at review.
	e.deps.Chat = fakeChat(brief1)
	e.deps.TTS = &fakeTTS{}
	reg1, err := DefaultTools(e.deps)
	if err != nil {
		t.Fatalf("DefaultTools: %v", err)
	}
	e.reg = reg1
	model1 := &scriptedModel{responses: []scriptedResponse{
		callScripted("a1", "analyze_character", map[string]any{"profile_id": p.ID, "character_id": marla.ID}),
		callScripted("a2", "design_voice", map[string]any{"profile_id": p.ID, "character_id": marla.ID, "brief_override": brief1}),
	}}
	run1 := games.NewID()
	out1 := e.runAgent(t, model1, run1, "Design a voice for Marla.", 8)
	t.Logf("Scenario 6: run1 state=%s stop_reason=%s", out1.State, out1.StopReason)
	if out1.State != agent.StateReview {
		t.Fatalf("run1 state = %s, want review (voice approval is a human step)", out1.State)
	}
	aud1 := e.designVoiceAuditions(t, run1)
	if len(aud1) != 3 {
		t.Fatalf("run1 auditions = %d, want 3", len(aud1))
	}

	// Run 2: "less stern" -> brief v2 + 3 new auditions.
	e.deps.Chat = fakeChat(brief2)
	reg2, err := DefaultTools(e.deps)
	if err != nil {
		t.Fatalf("DefaultTools: %v", err)
	}
	e.reg = reg2
	model2 := &scriptedModel{responses: []scriptedResponse{
		callScripted("b1", "analyze_character", map[string]any{"profile_id": p.ID, "character_id": marla.ID}),
		callScripted("b2", "design_voice", map[string]any{"profile_id": p.ID, "character_id": marla.ID, "brief_override": brief2}),
	}}
	run2 := games.NewID()
	out2 := e.runAgent(t, model2, run2, "Make Marla less stern and re-audition.", 8)
	t.Logf("Scenario 6: run2 state=%s stop_reason=%s", out2.State, out2.StopReason)
	if out2.State != agent.StateReview {
		t.Fatalf("run2 state = %s, want review", out2.State)
	}
	aud2 := e.designVoiceAuditions(t, run2)
	if len(aud2) != 3 {
		t.Fatalf("run2 auditions = %d, want 3", len(aud2))
	}
	for _, a1 := range aud1 {
		if _, err := e.assets.Stat(e.ctx, a1); err != nil {
			t.Fatalf("run1 audition %s no longer in the asset store: %v", a1, err)
		}
	}
	for _, a1 := range aud1 {
		for _, a2 := range aud2 {
			if a1 == a2 {
				t.Fatalf("run2 audition %s duplicates a run1 audition: the new brief must produce new audio", a1)
			}
		}
	}
	if got := blobCount(t, e.assets.Root()); got != blobsBefore+6 {
		t.Fatalf("asset store = %d blobs, want %d (6 auditions, no other writes)", got, blobsBefore+6)
	}

	// The approved voice and all other state are untouched.
	after := e.profileSnapshot(t, p.ID)
	if before.msgCount != after.msgCount || before.srcCount != after.srcCount ||
		before.charCount != after.charCount || before.preferredVoice != after.preferredVoice {
		t.Fatalf("voice redesign changed non-voice state: before=%+v after=%+v", before, after)
	}
}
