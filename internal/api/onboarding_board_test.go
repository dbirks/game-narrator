package api

// The setup board end to end (ADR-0108): a scripted onboarding run drives the
// real tool chain (import → stage two characters → brief the first → design
// the first's voice), the loop's SetupStages hook publishes the typed stage
// events to the durable log, and the board route answers a snapshot that a
// client assembling the board by replaying those events sees equal to (the
// elapsed clock is the only field that keeps running).

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/providers/openai"
	"github.com/dbirks/game-narrator/internal/providers/tts"
	"github.com/dbirks/game-narrator/internal/setupboard"
	"github.com/dbirks/game-narrator/internal/store"
)

// boardScriptedModel plays the board's walk against the durable run log:
// import the staged script, stage Marla and Corvin, brief Marla, design
// Marla's voice (which stops the run at the review boundary — voice
// approval is a human step, so no `done` stage lands in this run). It is
// stateless across requests, the conversation is its state.
type boardScriptedModel struct {
	mu      sync.Mutex
	request int
}

func (m *boardScriptedModel) Chat(_ context.Context, req openai.ChatRequest) (openai.ChatResponse, error) {
	m.mu.Lock()
	m.request++
	m.mu.Unlock()

	// The conversation in chronological order: user text, and each tool
	// result's name + payload (the model is the only stateless reader).
	var userText string
	type toolResult struct {
		name, payload string
	}
	var results []toolResult
	for _, msg := range req.Messages {
		switch msg.Role {
		case "user":
			if userText == "" {
				userText = msg.Text
			}
		case "tool":
			if i := strings.Index(msg.Text, "[TOOL RESULT name="); i >= 0 {
				rest := msg.Text[i+len("[TOOL RESULT name="):]
				if j := strings.IndexByte(rest, ' '); j > 0 {
					results = append(results, toolResult{name: rest[:j], payload: msg.Text})
				}
			}
		}
	}
	var marlaID string
	stages := 0
	for _, r := range results {
		if r.name == "stage_character" {
			stages++
			if stages == 1 {
				marlaID = boardCharID(r.payload)
			}
		}
	}
	last := ""
	if len(results) > 0 {
		last = results[len(results)-1].name
	}

	switch {
	case last == "":
		profile, sha := onbProfileRe.FindStringSubmatch(userText), onbSHARe.FindStringSubmatch(userText)
		if len(profile) != 2 || len(sha) != 2 {
			return openai.ChatResponse{Text: "no staged file named in the request"}, nil
		}
		return boardCall("call_import", "import_user_asset", map[string]any{
			"profile_id":          profile[1],
			"staged_asset_sha256": sha[1],
			"role":                "dialogue_script",
			"rights":              "user_supplied",
		}), nil
	case last == "import_user_asset":
		src := onbSourceIDRe.FindStringSubmatch(results[len(results)-1].payload)
		profile := onbProfileRe.FindStringSubmatch(userText)
		if len(src) != 2 || len(profile) != 2 {
			return openai.ChatResponse{Text: "cannot find the imported source id"}, nil
		}
		return boardCall("call_stage1", "stage_character", map[string]any{
			"profile_id": profile[1],
			"name":       "Marla",
			"source_ids": []string{src[1]},
		}), nil
	case last == "stage_character" && stages < 2:
		profile := onbProfileRe.FindStringSubmatch(userText)
		if len(profile) != 2 {
			return openai.ChatResponse{Text: "cannot find the draft profile id"}, nil
		}
		// The second card: same imported source, a different name.
		var src string
		for i := len(results) - 1; i >= 0; i-- {
			if s := onbSourceIDRe.FindStringSubmatch(results[i].payload); len(s) == 2 {
				src = s[1]
				break
			}
		}
		return boardCall("call_stage2", "stage_character", map[string]any{
			"profile_id": profile[1],
			"name":       "Corvin",
			"source_ids": []string{src},
		}), nil
	case last == "stage_character":
		profile := onbProfileRe.FindStringSubmatch(userText)
		if len(profile) != 2 || marlaID == "" {
			return openai.ChatResponse{Text: "cannot find the draft profile or the staged character"}, nil
		}
		return boardCall("call_analyze", "analyze_character", map[string]any{
			"profile_id":   profile[1],
			"character_id": marlaID,
		}), nil
	case last == "analyze_character":
		profile := onbProfileRe.FindStringSubmatch(userText)
		if len(profile) != 2 || marlaID == "" {
			return openai.ChatResponse{Text: "cannot find the draft profile or the staged character"}, nil
		}
		return boardCall("call_voice", "design_voice", map[string]any{
			"profile_id":   profile[1],
			"character_id": marlaID,
		}), nil
	default:
		// design_voice answered: the run stops at the review boundary
		// (voice approval is a human step), so this is only reached on a
		// resumed attempt.
		return openai.ChatResponse{Text: "The voice audition is ready for review."}, nil
	}
}

// boardCall renders one tool-call response with fresh call ids (ids must be
// unique per conversation; the fixed names would trip the loop's repetition
// guard if the run resumed).
func boardCall(id, name string, args map[string]any) openai.ChatResponse {
	b, _ := json.Marshal(args)
	return openai.ChatResponse{
		Text: "Working.",
		ToolCalls: []openai.ToolCall{{
			ID:        id + "_" + fmt.Sprintf("%d", time.Now().UnixNano()),
			Name:      name,
			Arguments: json.RawMessage(b),
		}},
		FinishReason: "tool_calls",
	}
}

// boardCharID reads the character_id a stage_character result carries.
func boardCharID(payload string) string {
	var out struct {
		CharacterID string `json:"character_id"`
	}
	if json.Unmarshal([]byte(extractJSONPayload(payload)), &out) == nil {
		return out.CharacterID
	}
	return ""
}

// extractJSONPayload strips the [TOOL RESULT ...] envelope to the JSON body
// (first { to the last } before the envelope closes; the envelope may carry
// a trailing sources: line the parser must not see).
func extractJSONPayload(s string) string {
	if j := strings.Index(s, "[/TOOL RESULT]"); j >= 0 {
		s = s[:j]
	}
	i := strings.Index(s, "{")
	j := strings.LastIndex(s, "}")
	if i >= 0 && j > i {
		return s[i : j+1]
	}
	return s
}

// boardBriefChat is the analyze_character chat role: one labelled voice_brief
// JSON object, every creative field non-empty (the tool's contract).
func boardBriefChat(_ context.Context, _ openai.ChatRequest) (openai.ChatResponse, error) {
	return openai.ChatResponse{
		Text: `{"voice_brief":{"vocal_age_impression":"late thirties","timbre":"low and steady, slightly hoarse","cadence":"measured, with pauses before the important word","emotional_baseline":"pragmatic warmth","language_notes":"saltmarsh dockside diction, short sentences","pronunciation_notes":"drops the terminal e on harbour"}}`,
	}, nil
}

// boardTTS is the design provider: a distinct small WAV per call.
type boardTTS struct {
	mu    sync.Mutex
	calls int
}

func (f *boardTTS) Design(_ context.Context, _ tts.DesignRequest) (tts.Audio, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return tts.Audio{
		WAV:           boardWAV(f.calls),
		SampleRate:    8000,
		Channels:      1,
		BitsPerSample: 16,
		Duration:      50 * time.Millisecond,
		Model:         "board-test-design",
		RequestID:     "board-test-request",
		Origin:        "voice_design model=board-test-design voice=marla",
	}, nil
}

// boardWAV builds a valid tiny WAV (the asset store content-addresses bytes;
// the auditions only need to exist).
func boardWAV(seed int) []byte {
	const sampleRate = 8000
	samples := make([]int16, 400)
	for i := range samples {
		samples[i] = int16(8000 * math.Sin(float64(i)*2*3.14159*440/float64(sampleRate)+float64(seed)))
	}
	dataSize := len(samples) * 2
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, []byte("RIFF"))
	binary.Write(buf, binary.LittleEndian, uint32(36+dataSize))
	binary.Write(buf, binary.LittleEndian, []byte("WAVE"))
	binary.Write(buf, binary.LittleEndian, []byte("fmt "))
	binary.Write(buf, binary.LittleEndian, uint32(16))
	binary.Write(buf, binary.LittleEndian, uint16(1))
	binary.Write(buf, binary.LittleEndian, uint16(1))
	binary.Write(buf, binary.LittleEndian, uint32(sampleRate))
	binary.Write(buf, binary.LittleEndian, uint32(sampleRate*2))
	binary.Write(buf, binary.LittleEndian, uint16(2))
	binary.Write(buf, binary.LittleEndian, uint16(16))
	binary.Write(buf, binary.LittleEndian, []byte("data"))
	binary.Write(buf, binary.LittleEndian, uint32(dataSize))
	for _, s := range samples {
		binary.Write(buf, binary.LittleEndian, s)
	}
	return buf.Bytes()
}

// replayedBoard is what a client assembling the board from the SSE events
// (Last-Event-ID replay) would hold: the last stage event per character and
// the last progress event.
type replayedBoard struct {
	characters map[string]setupboard.CharacterStage
	progress   setupboard.Progress
}

// replayBoard rebuilds the board from the durable event log exactly the way a
// reconnecting client does: apply setup.character_stage per character (later
// rows overwrite earlier ones) and keep the newest setup.progress.
func replayBoard(t *testing.T, evStore *store.EventStore, runID string) replayedBoard {
	t.Helper()
	ctx := context.Background()
	envs, err := evStore.After(ctx, 0, events.Filter{
		Names: []string{setupboard.EventCharacterStage, setupboard.EventProgress},
	}, 0)
	if err != nil {
		t.Fatalf("event store replay: %v", err)
	}
	rb := replayedBoard{characters: map[string]setupboard.CharacterStage{}}
	for _, ev := range envs {
		switch ev.Name {
		case setupboard.EventCharacterStage:
			var p setupboard.CharacterStage
			if err := json.Unmarshal(ev.Data, &p); err != nil {
				t.Fatalf("replay: bad character_stage payload: %v", err)
			}
			if p.RunID != runID {
				t.Fatalf("replay: a character_stage event names run %s, want %s", p.RunID, runID)
			}
			rb.characters[p.CharacterID] = p
		case setupboard.EventProgress:
			var p setupboard.Progress
			if err := json.Unmarshal(ev.Data, &p); err != nil {
				t.Fatalf("replay: bad progress payload: %v", err)
			}
			if p.RunID != runID {
				t.Fatalf("replay: a progress event names run %s, want %s", p.RunID, runID)
			}
			rb.progress = p
		}
	}
	return rb
}

// stageSequence returns a character's stages in emission order.
func stageSequence(t *testing.T, evStore *store.EventStore, runID, charID string) []setupboard.Stage {
	t.Helper()
	ctx := context.Background()
	envs, err := evStore.After(ctx, 0, events.Filter{
		Names: []string{setupboard.EventCharacterStage},
	}, 0)
	if err != nil {
		t.Fatalf("event store: %v", err)
	}
	var out []setupboard.Stage
	for _, ev := range envs {
		var p setupboard.CharacterStage
		if err := json.Unmarshal(ev.Data, &p); err != nil || p.RunID != runID || p.CharacterID != charID {
			continue
		}
		out = append(out, p.Stage)
	}
	return out
}

// TestSetupBoardEventsAndSnapshot is the task's event-order + snapshot
// assertion over one scripted run with the real tool chain: the card events
// land in finding → brief → voicing order (a progress event follows each),
// and GET /board equals the board assembled by replaying the durable events.
func TestSetupBoardEventsAndSnapshot(t *testing.T) {
	model := &boardScriptedModel{}
	e := newOnbEnvFull(t, model, onbBoardOpts{board: true, chat: boardBriefChat, tts: &boardTTS{}})

	sha := e.uploadRaw(onbCSVPath, "synthetic-adventure.csv")
	created := e.createRun(`{"title":"Synthetic Adventure","platform":"GameCube","use_web":false,"staged_asset_sha256":"` + sha + `"}`)
	runID, _ := created["run_id"].(string)
	profileID, _ := created["profile_id"].(string)
	if runID == "" || profileID == "" {
		t.Fatalf("202 body is missing ids: %v", created)
	}

	// The walk ends at the review boundary: design_voice stops the run for
	// human voice approval, so the terminal state is review, not succeeded.
	st := e.waitForState(runID, "review", 60*time.Second)
	if stop, _ := st["stop_reason"].(string); stop != "review_required" {
		t.Fatalf("stop_reason = %q, want review_required (the voice review boundary)", stop)
	}

	// ── event order ────────────────────────────────────────────────────────
	// The synthetic fixture names five speakers (Marla, Corvin, Boy, Chef
	// Ondine, Têvance), so the roster — and the board — carries five cards.
	// The run moves only two of them; the rest stay at the baseline finding
	// column, which a replaying client gets from the snapshot, not the
	// stream (events record moves, not the initial board).
	chars, err := e.st.ListCharacters(e.ctx, profileID)
	if err != nil {
		t.Fatalf("ListCharacters: %v", err)
	}
	if len(chars) != 5 {
		t.Fatalf("roster = %d characters, want the fixture's 5 speakers", len(chars))
	}
	byName := map[string]string{}
	for _, c := range chars {
		byName[strings.ToLower(c.Name)] = c.ID
	}
	marlaID, corvinID := byName["marla"], byName["corvin"]
	if marlaID == "" || corvinID == "" {
		t.Fatalf("the run staged Marla and Corvin; the roster shows %v", byName)
	}

	if got := stageSequence(t, e.eventStore, runID, marlaID); len(got) != 3 ||
		got[0] != setupboard.StageFinding || got[1] != setupboard.StageBrief || got[2] != setupboard.StageVoicing {
		t.Fatalf("Marla's stage events = %v, want [finding brief voicing] in order", got)
	}
	if got := stageSequence(t, e.eventStore, runID, corvinID); len(got) != 1 || got[0] != setupboard.StageFinding {
		t.Fatalf("Corvin's stage events = %v, want [finding]", got)
	}

	// Every card move is followed by the run-level progress (the tiles and
	// the rail move with the card), and no stage event repeats: the guard is
	// monotonic even though the run re-observes its own calls on resume.
	ctx := context.Background()
	envs, err := e.eventStore.After(ctx, 0, events.Filter{
		Names: []string{setupboard.EventCharacterStage, setupboard.EventProgress},
	}, 0)
	if err != nil {
		t.Fatalf("event store: %v", err)
	}
	var names []string
	for _, ev := range envs {
		names = append(names, ev.Name)
		if len(names) >= 2 && names[len(names)-2] == setupboard.EventCharacterStage &&
			names[len(names)-1] != setupboard.EventProgress {
			t.Fatalf("a character_stage event is not followed by setup.progress: %v", names)
		}
	}
	if len(names) != 8 {
		t.Fatalf("event stream = %v, want exactly 4 card moves (Marla finding/brief/voicing, Corvin finding) and 4 progress events", names)
	}

	// ── the snapshot ───────────────────────────────────────────────────────
	rec := e.do(auth.RoleCurator, http.MethodGet, "/api/v1/onboarding/runs/"+runID+"/board", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /board = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var board setupboard.Board
	decode(t, rec, &board)
	if board.RunID != runID || board.State != "review" {
		t.Fatalf("board identity = run %s state %s, want %s review", board.RunID, board.State, runID)
	}

	// A non-curator cannot read the board (the onboarding scope is curator).
	if rec := e.do(auth.RoleViewer, http.MethodGet, "/api/v1/onboarding/runs/"+runID+"/board", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer GET /board = %d, want 403", rec.Code)
	}

	// Snapshot ≡ replay: the board a client rebuilds from the events (the
	// Last-Event-ID source) is the board the route answers. A card the
	// stream never moved holds the baseline finding column with no at — the
	// implicit initial column the snapshot states for it.
	rb := replayBoard(t, e.eventStore, runID)
	if len(rb.characters) != 2 {
		t.Fatalf("the replay holds %d cards, want the two the run moved", len(rb.characters))
	}
	if len(board.Characters) != len(chars) {
		t.Fatalf("board shows %d cards, want the roster's %d", len(board.Characters), len(chars))
	}
	for _, bc := range board.Characters {
		rep, moved := rb.characters[bc.CharacterID]
		if !moved {
			if bc.Stage != setupboard.StageFinding || bc.At != "" {
				t.Errorf("board card %s = %s at %q, want the uneventful baseline finding with no at", bc.CharacterID, bc.Stage, bc.At)
			}
			continue
		}
		if bc.Stage != rep.Stage {
			t.Errorf("board card %s stage = %s, replay says %s", bc.CharacterID, bc.Stage, rep.Stage)
		}
		if bc.Lines != rep.Lines {
			t.Errorf("board card %s lines = %d, replay says %d", bc.CharacterID, bc.Lines, rep.Lines)
		}
		if bc.At != rep.At {
			t.Errorf("board card %s at = %q, replay says %q (the card time is its newest stage event)", bc.CharacterID, bc.At, rep.At)
		}
	}

	// The progress snapshot: every field the events carry, equal, except the
	// elapsed clock, which the board reads now (so it is >= the replay's).
	rep := rb.progress
	p := board.Progress
	if rep.RunID == "" {
		t.Fatalf("the replay holds no setup.progress event")
	}
	if p.RunID != rep.RunID || p.LinesRead != rep.LinesRead ||
		p.Characters != rep.Characters || p.Voiced != rep.Voiced || p.Total != rep.Total {
		t.Errorf("board tiles = %+v, replay says %+v", p, rep)
	}
	if p.Elapsed < rep.Elapsed {
		t.Errorf("board elapsed = %d, replay says %d (time only moves forward)", p.Elapsed, rep.Elapsed)
	}
	if len(p.StageRail) != 6 || len(rep.StageRail) != 6 {
		t.Fatalf("rail = %d cells (replay %d), want 6", len(p.StageRail), len(rep.StageRail))
	}
	for i := 0; i < 6; i++ {
		if p.StageRail[i].ID != rep.StageRail[i].ID || p.StageRail[i].State != rep.StageRail[i].State ||
			p.StageRail[i].Value != rep.StageRail[i].Value {
			t.Errorf("rail[%d] = %+v, replay says %+v", i, p.StageRail[i], rep.StageRail[i])
		}
	}
	// The rail's own story: import and roster are done, the briefs cell is
	// the frontier (1/5 briefed), the run has not published.
	if p.StageRail[1].State != setupboard.CellDone || p.StageRail[2].State != setupboard.CellDone {
		t.Errorf("dialogue/characters cells = %s/%s, want done/done", p.StageRail[1].State, p.StageRail[2].State)
	}
	if p.StageRail[3].State != setupboard.CellActive || p.StageRail[3].Value != "1/5 briefs" {
		t.Errorf("briefs cell = %+v, want active with 1/5 briefs", p.StageRail[3])
	}
	if p.StageRail[5].State != setupboard.CellPending {
		t.Errorf("publish cell = %s, want pending (the profile is a draft)", p.StageRail[5].State)
	}

	// The contract: the route is documented and its 200 body validates.
	recordContractResponse(t, http.MethodGet, "/api/v1/onboarding/runs/"+runID+"/board",
		http.StatusOK, "application/json", rec.Body.Bytes())
}

// TestSetupBoardRouteUnwired is the honest-degradation shape: without the
// emitter the route answers 503, so a page cannot believe it watches a board
// the process cannot derive.
func TestSetupBoardRouteUnwired(t *testing.T) {
	model := &boardScriptedModel{}
	e := newOnbEnvFull(t, model, onbBoardOpts{}) // board: false

	sha := e.uploadRaw(onbCSVPath, "synthetic-adventure.csv")
	created := e.createRun(`{"title":"Synthetic Adventure","platform":"GameCube","use_web":false,"staged_asset_sha256":"` + sha + `"}`)
	runID, _ := created["run_id"].(string)

	rec := e.do(auth.RoleCurator, http.MethodGet, "/api/v1/onboarding/runs/"+runID+"/board", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /board without the emitter = %d, want 503 (body %s)", rec.Code, rec.Body.String())
	}
}
