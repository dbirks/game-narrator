package setupboard

// Unit tests for the setup board's derivation and emitter (ADR-0108), over a
// real SQLite store: the stage derivation is a pure function of durable rows
// (staged characters, succeeded tool calls, voice revisions), the emitter
// moves a card forward and only forward, and the Board snapshot is the same
// derivation a client would get by replaying the events.

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/agent"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/store"
)

// newTestStore opens a real store in a temp dir.
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// seedRun creates one draft profile with two characters, a run row for it and
// the run's opening messages, and returns the run/profile/character ids.
func seedRun(t *testing.T, st *store.Store) (runID, profileID, charA, charB string) {
	t.Helper()
	ctx := context.Background()

	g := games.Game{
		ID: games.NewID(), Slug: "test-game", Title: "Test Game",
		Platform: "arcade", UserTags: json.RawMessage(`[]`), CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := g.Validate(); err != nil {
		t.Fatalf("game invalid: %v", err)
	}
	if err := st.CreateGame(ctx, g); err != nil {
		t.Fatalf("CreateGame: %v", err)
	}
	e := games.Edition{
		ID: games.NewID(), GameID: g.ID,
		Region: "us", Language: "en", Revision: "v1",
	}
	if err := e.Validate(); err != nil {
		t.Fatalf("edition invalid: %v", err)
	}
	if err := st.CreateEdition(ctx, e); err != nil {
		t.Fatalf("CreateEdition: %v", err)
	}
	p, err := st.CreateDraftProfile(ctx, e.ID)
	if err != nil {
		t.Fatalf("CreateDraftProfile: %v", err)
	}
	charA, charB = games.NewID(), games.NewID()
	now := time.Now().UTC().Format(time.RFC3339)
	chars := []games.Character{
		{ID: charA, ProfileID: p.ID, Name: "Marla", Status: games.CharacterProposed,
			Aliases: json.RawMessage(`[]`), VisualDescriptors: json.RawMessage(`[]`),
			VerifiedSourceRefs: json.RawMessage(`[]`), CreatedAt: now},
		{ID: charB, ProfileID: p.ID, Name: "Corvin", Status: games.CharacterProposed,
			Aliases: json.RawMessage(`[]`), VisualDescriptors: json.RawMessage(`[]`),
			VerifiedSourceRefs: json.RawMessage(`[]`), CreatedAt: now},
	}
	if err := st.PutCharacters(ctx, p.ID, chars); err != nil {
		t.Fatalf("PutCharacters: %v", err)
	}
	plan, _ := json.Marshal(map[string]string{"title": "Test Game"})
	runID = games.NewID()
	runs := store.NewAgentStore(st)
	if err := runs.CreateAgentRun(ctx, store.AgentRun{
		ID: runID, ProfileID: p.ID, State: store.AgentStateRunning,
		PlanJSON: plan, PromptVersion: "unit-test",
	}); err != nil {
		t.Fatalf("CreateAgentRun: %v", err)
	}
	return runID, p.ID, charA, charB
}

// insertToolCall records one succeeded tool call on the run's log with the
// result the stage tools answer with (character_id + name).
func insertToolCall(t *testing.T, st *store.Store, runID, tool, charID, name string) {
	t.Helper()
	ctx := context.Background()
	result, _ := json.Marshal(map[string]string{"character_id": charID, "character_name": name})
	runs := store.NewAgentStore(st)
	started := time.Now().UTC()
	finished := started.Add(time.Second)
	tc := store.AgentToolCall{
		ID: games.NewID(), RunID: runID, Name: tool, Status: "succeeded",
		ArgsJSON:   json.RawMessage(`{}`),
		ResultJSON: result,
		StartedAt:  started, FinishedAt: &finished,
	}
	if _, err := runs.InsertAgentToolCall(ctx, tc); err != nil {
		t.Fatalf("InsertAgentToolCall(%s): %v", tool, err)
	}
}

// approvedVoice creates one approved voice revision (with an audition) for a
// character name, the durable fact the done stage reads: draft, auditions
// (which move it to auditioning), then approval — the store's own lifecycle.
func approvedVoice(t *testing.T, st *store.Store, name string, revision int) string {
	t.Helper()
	ctx := context.Background()
	revID := games.NewID()
	now := time.Now().UTC()
	rev := store.VoiceProfile{
		ID: revID, Character: name, Revision: revision,
		State: store.VoiceStateDraft, DesignInstruction: "a test voice brief",
		Language: "en", CreatedAt: now, UpdatedAt: now,
	}
	if err := st.CreateVoiceRevision(ctx, rev); err != nil {
		t.Fatalf("CreateVoiceRevision: %v", err)
	}
	audID := games.NewID()
	aud := store.VoiceAudition{
		ID: audID, VoiceProfileID: revID, Index: 1,
		Brief: "a test voice brief", ModelID: "test-model",
		SampleText: "synthetic line", Transcript: "synthetic line",
		AssetSHA256: "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210",
		CreatedAt:   now,
	}
	if err := st.AddAuditions(ctx, revID, []string{audID}, []store.VoiceAudition{aud}); err != nil {
		t.Fatalf("AddAuditions: %v", err)
	}
	cur, err := st.GetVoiceRevision(ctx, revID)
	if err != nil {
		t.Fatalf("GetVoiceRevision: %v", err)
	}
	cur.State = store.VoiceStateApproved
	cur.AuditionIDs = []string{audID}
	cur.ReferenceAssetID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	cur.RefTranscript = "synthetic line"
	cur.ModelID = "test-model"
	cur.ConsentProvenance = "unit test"
	cur.ApprovedBy = "curator"
	cur.Fingerprint = "fp"
	cur.UpdatedAt = time.Now().UTC()
	cur.ApprovedAt = &cur.UpdatedAt
	if err := st.UpdateVoiceProfile(ctx, cur); err != nil {
		t.Fatalf("UpdateVoiceProfile(approved): %v", err)
	}
	return revID
}

// TestStageDerivation walks the derivation table over durable rows: a staged
// character is finding, a brief call moves it to brief, a take to voicing, an
// approved current voice to done, and a newer take above the approved one
// back to voicing (UX-03d regeneration).
func TestStageDerivation(t *testing.T) {
	st := newTestStore(t)
	runID, profileID, charA, charB := seedRun(t, st)
	em := New(nil, st, store.NewAgentStore(st), nil, nil)

	derive := func() (a, b Stage) {
		f, err := em.loadFacts(context.Background(), runID, profileID)
		if err != nil {
			t.Fatalf("loadFacts: %v", err)
		}
		byID := map[string]games.Character{}
		for _, c := range f.characters {
			byID[c.ID] = c
		}
		return stageFor(f, byID[charA]), stageFor(f, byID[charB])
	}

	a, b := derive()
	if a != StageFinding || b != StageFinding {
		t.Fatalf("fresh roster = %s/%s, want finding/finding", a, b)
	}

	insertToolCall(t, st, runID, "analyze_character", charA, "Marla")
	if a, _ = derive(); a != StageBrief {
		t.Fatalf("after brief call = %s, want brief", a)
	}
	if _, b = derive(); b != StageFinding {
		t.Fatalf("untouched character = %s, want finding", b)
	}

	insertToolCall(t, st, runID, "design_voice", charA, "Marla")
	if a, _ = derive(); a != StageVoicing {
		t.Fatalf("after design call = %s, want voicing", a)
	}

	approvedVoice(t, st, "Marla", 1)
	if a, _ = derive(); a != StageDone {
		t.Fatalf("after approval = %s, want done", a)
	}

	// A regeneration opens a newer draft above the approved revision: the
	// approved take is no longer current, so the card reads voicing again.
	now := time.Now().UTC()
	newer := store.VoiceProfile{
		ID: games.NewID(), Character: "Marla", Revision: 2,
		State: store.VoiceStateDraft, DesignInstruction: "regenerated",
		Language: "en", CreatedAt: now, UpdatedAt: now,
	}
	if err := st.CreateVoiceRevision(context.Background(), newer); err != nil {
		t.Fatalf("newer draft: %v", err)
	}
	if a, _ = derive(); a != StageVoicing {
		t.Fatalf("after regeneration draft = %s, want voicing", a)
	}
}

// waitEvents polls the in-memory log until the filter returns at least want
// events (the emitter publishes on a bounded background context, so the row
// lands a tick after ObserveTool returns).
func waitEvents(t *testing.T, mem *events.MemStore, filter events.Filter, want int) []events.Envelope {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		evs, err := mem.After(context.Background(), 0, filter, 0)
		if err != nil {
			t.Fatalf("mem.After: %v", err)
		}
		if len(evs) >= want {
			return evs
		}
		if time.Now().After(deadline) {
			t.Fatalf("waited 3s for %d events, got %d", want, len(evs))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestEmitterMovesOnlyForward drives the emitter's monotonic guard: the same
// transition twice emits once, a backwards transition emits nothing, and each
// advance emits the card event followed by the run-level progress event.
func TestEmitterMovesOnlyForward(t *testing.T) {
	st := newTestStore(t)
	runID, profileID, charA, charB := seedRun(t, st)
	mem := events.NewMemStore()
	hub := events.New(events.WithStore(mem))
	t.Cleanup(func() { hub.Close() })
	em := New(hub, st, store.NewAgentStore(st), mem, nil)

	result := func() agent.Result {
		raw, _ := json.Marshal(map[string]string{"character_id": charA, "character_name": "Marla"})
		return agent.Result{Status: agent.StatusSucceeded, Results: raw}
	}
	// The loop persists the tool call row before the hook fires (loop.go's
	// finishToolCall precedes SetupStages), so the test does the same: the
	// row lands, then the observer sees the result.
	call := func(tool string) {
		t.Helper()
		insertToolCall(t, st, runID, tool, charA, "Marla")
		em.ObserveTool(runID, profileID, tool, result())
	}

	call("stage_character")
	call("stage_character") // same stage again: silent
	call("analyze_character")
	call("design_voice")
	call("design_voice") // the derived stage already moved there: silent

	// A claim the durable rows do not reflect yet is not emitted: the next
	// observation (or the snapshot) catches up from the rows.
	insertToolCall(t, st, runID, "stage_character", charB, "Corvin")
	em.ObserveTool(runID, profileID, "analyze_character", agent.Result{
		Status:  agent.StatusSucceeded,
		Results: json.RawMessage(`{"character_id":"` + charB + `","name":"Corvin"}`),
	})

	evs := waitEvents(t, mem, events.Filter{
		Names: []string{EventCharacterStage, EventProgress},
	}, 6)
	var got []string
	var stageSeq []Stage
	for _, ev := range evs {
		got = append(got, ev.Name)
		if ev.Name == EventCharacterStage {
			var p CharacterStage
			if err := json.Unmarshal(ev.Data, &p); err != nil {
				t.Fatalf("stage payload: %v", err)
			}
			stageSeq = append(stageSeq, p.Stage)
			if p.RunID != runID || p.CharacterID != charA || p.Name != "Marla" {
				t.Errorf("stage payload identity = %+v, want run/char/name named", p)
			}
			if !p.Stage.Valid() {
				t.Errorf("stage %q is outside the closed vocabulary", p.Stage)
			}
		}
	}
	want := []string{
		EventCharacterStage, EventProgress,
		EventCharacterStage, EventProgress,
		EventCharacterStage, EventProgress,
	}
	if len(got) != len(want) {
		t.Fatalf("event order = %v, want %v (a duplicate or a backwards move leaked through)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event %d = %s, want %s (order %v)", i, got[i], want[i], got)
		}
	}
	wantStages := []Stage{StageFinding, StageBrief, StageVoicing}
	if len(stageSeq) != len(wantStages) {
		t.Fatalf("stages = %v, want %v", stageSeq, wantStages)
	}
	for i := range wantStages {
		if stageSeq[i] != wantStages[i] {
			t.Errorf("stage %d = %s, want %s", i, stageSeq[i], wantStages[i])
		}
	}

	// The progress event after the last move carries the run's tiles and the
	// six-cell rail in board order.
	var last Progress
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Name == EventProgress {
			if err := json.Unmarshal(evs[i].Data, &last); err != nil {
				t.Fatalf("progress payload: %v", err)
			}
			break
		}
	}
	if last.RunID != runID || last.Characters != 2 || last.Total != 2 || last.Voiced != 1 {
		t.Errorf("progress tiles = %+v, want 2 characters, 1 voiced of 2", last)
	}
	if len(last.StageRail) != 6 {
		t.Fatalf("rail = %d cells, want 6", len(last.StageRail))
	}
	if last.StageRail[0].State != CellDone || last.StageRail[0].ID != RailFindGame {
		t.Errorf("rail[0] = %+v, want the find_game cell done", last.StageRail[0])
	}
	if last.StageRail[1].State != CellActive {
		t.Errorf("rail[1] dialogue = %s, want active (no import on this run, find_game already done)", last.StageRail[1].State)
	}
	if last.StageRail[3].ID != RailBriefs || last.StageRail[3].State != CellPending {
		t.Errorf("rail[3] = %+v, want the briefs cell pending (dialogue is not done, so it cannot be active)", last.StageRail[3])
	}
}

// TestBoardIsTheSameDerivation asserts the snapshot the board route serves is
// the same derivation the emitter uses: every card stage, the tiles, and the
// rail agree, and a card's `at` is its newest setup.character_stage row from
// the durable log.
func TestBoardIsTheSameDerivation(t *testing.T) {
	st := newTestStore(t)
	runID, profileID, charA, charB := seedRun(t, st)
	mem := events.NewMemStore()
	hub := events.New(events.WithStore(mem))
	t.Cleanup(func() { hub.Close() })
	em := New(hub, st, store.NewAgentStore(st), mem, nil)

	insertToolCall(t, st, runID, "analyze_character", charA, "Marla")
	approvedVoice(t, st, "Corvin", 1)
	// Corvin has an approved voice but no brief call on the run: the
	// derivation takes the strongest durable fact, so Corvin reads done.
	em.ObserveTool(runID, profileID, "stage_character",
		agent.Result{Status: agent.StatusSucceeded,
			Results: json.RawMessage(`{"character_id":"` + charA + `","name":"Marla"}`)})
	// The emitter publishes on a background context; wait for Marla's stage
	// event to be durable before the snapshot reads it back.
	waitEvents(t, mem, events.Filter{Names: []string{EventCharacterStage}}, 1)

	board, err := em.Board(context.Background(), runID)
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	if board.RunID != runID || board.State != store.AgentStateRunning {
		t.Fatalf("board identity = %+v, want run %s in state running", board, runID)
	}
	byID := map[string]BoardCharacter{}
	for _, c := range board.Characters {
		byID[c.CharacterID] = c
	}
	if len(byID) != 2 {
		t.Fatalf("board shows %d cards, want 2", len(byID))
	}
	if got := byID[charA].Stage; got != StageBrief {
		t.Errorf("Marla = %s, want brief", got)
	}
	if got := byID[charB].Stage; got != StageDone {
		t.Errorf("Corvin = %s, want done (an approved voice is a durable fact the run log has no part in)", got)
	}
	if byID[charA].Name != "Marla" || byID[charB].Name != "Corvin" {
		t.Errorf("card names = %q/%q, want Marla/Corvin", byID[charA].Name, byID[charB].Name)
	}
	// The emitted event carried Marla's move; the snapshot's `at` reads the
	// same row back from the durable log.
	if byID[charA].At == "" {
		t.Errorf("Marla has no at, want the observed time of her newest stage event")
	}
	if byID[charB].At != "" {
		t.Errorf("Corvin has at %q, want none (no stage event was emitted for him)", byID[charB].At)
	}

	if board.Progress.Characters != 2 || board.Progress.Voiced != 1 {
		t.Errorf("tiles = %+v, want 1 of 2 voiced (Corvin's approval)", board.Progress)
	}
	rail := board.Progress.StageRail
	if len(rail) != 6 || rail[5].State != CellPending || rail[5].Value != "draft" {
		t.Errorf("publish cell = %+v, want pending with the draft status", rail[5])
	}
}

// TestEmitterWithoutHubOrStore degrades to silence rather than a guess: with
// no hub nothing is emitted, and with no store the emitter refuses to derive,
// so a mis-wired process cannot publish a board it cannot back up.
func TestEmitterWithoutHubOrStore(t *testing.T) {
	em := New(nil, nil, nil, nil, nil)
	em.ObserveTool("run-1", "profile-1", "stage_character",
		agent.Result{Status: agent.StatusSucceeded,
			Results: json.RawMessage(`{"character_id":"01ARZ3NDEKTSV4RRFFQ69E5WY8","name":"Marla"}`)})
	// No store: loadFacts would fail; the guard answers before it is reached.
	if _, err := em.Board(context.Background(), "run-1"); err == nil {
		t.Fatal("Board with no store wired: expected an error, got a board")
	}
}
