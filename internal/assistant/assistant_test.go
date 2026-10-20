package assistant

// Hermetic tests for the assistant agent (ADR-0099): a scripted model drives
// the real job handler over the real store — tool-call parsing, the screen
// context reaching the prompt, the UX-04 non-blocking question, the
// confirmation gate (stage / confirm / cancel / mismatch), the per-tool role
// tiers, the bulk speaker fix, and the exactly-one-follow-up hook. No
// provider, no network: the model boundary is scripted.

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/jobs"
	"github.com/dbirks/game-narrator/internal/providers/openai"
	"github.com/dbirks/game-narrator/internal/store"
)

// ── the scripted model boundary ────────────────────────────────────────────

type scriptedResp struct {
	text      string
	toolCalls []openai.ToolCall
}

type scriptModel struct {
	mu        sync.Mutex
	responses []scriptedResp
	requests  []openai.ChatRequest
	// onCall fires (outside the lock) before the nth response is returned:
	// a test hook for "the answer arrives while the attempt is in flight".
	onCall func(n int)
}

func (m *scriptModel) Chat(ctx context.Context, req openai.ChatRequest) (openai.ChatResponse, error) {
	m.mu.Lock()
	idx := len(m.requests)
	m.requests = append(m.requests, req)
	var r scriptedResp
	if idx < len(m.responses) {
		r = m.responses[idx]
	}
	m.mu.Unlock()
	if m.onCall != nil {
		m.onCall(idx)
	}
	if idx >= len(m.responses) {
		r = scriptedResp{text: "(script exhausted)"}
	}
	fin := "stop"
	if len(r.toolCalls) > 0 {
		fin = "tool_calls"
	}
	return openai.ChatResponse{Text: r.text, ToolCalls: r.toolCalls, FinishReason: fin}, nil
}

func (m *scriptModel) requestTexts(i int) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i >= len(m.requests) {
		return nil
	}
	var out []string
	for _, msg := range m.requests[i].Messages {
		out = append(out, msg.Text)
	}
	return out
}

// ── the fake queue and event log ───────────────────────────────────────────

type fakeQueue struct {
	mu       sync.Mutex
	enqueued []store.EnqueueParams
}

func (f *fakeQueue) Enqueue(ctx context.Context, p store.EnqueueParams) (*store.Job, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enqueued = append(f.enqueued, p)
	return &store.Job{ID: games.NewID(), Kind: p.Kind, Payload: p.Payload}, true, nil
}

func (f *fakeQueue) kinds() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.enqueued))
	for _, p := range f.enqueued {
		out = append(out, p.Kind)
	}
	return out
}

type fakeEvents struct {
	mu    sync.Mutex
	rows  []events.Envelope
	next  int64
	trace string
}

func (f *fakeEvents) Append(ctx context.Context, e events.Envelope) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	e.Seq = f.next
	f.rows = append(f.rows, e)
	return f.next, nil
}

func (f *fakeEvents) After(ctx context.Context, afterSeq int64, flt events.Filter, limit int) ([]events.Envelope, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]events.Envelope, 0)
	for _, e := range f.rows {
		if e.Seq <= afterSeq {
			continue
		}
		out = append(out, e)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// ── the environment ────────────────────────────────────────────────────────

type env struct {
	t         *testing.T
	s         *store.Store
	agent     *store.AgentStore
	threads   *store.ThreadStore
	questions *store.QuestionStore
	hub       *events.Hub
	queue     *fakeQueue
	evlog     *fakeEvents
	model     *scriptModel
	deps      Deps
	runID     string
	threadID  string
	userID    string

	// collected hub envelopes (the card-event assertions).
	evMu   sync.Mutex
	evs    []events.Envelope
	stopEv chan struct{}
}

func newEnv(t *testing.T, model *scriptModel, evlog *fakeEvents) *env {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "gn-test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	hub := events.New()
	q := &fakeQueue{}
	e := &env{
		t: t, s: s, agent: store.NewAgentStore(s),
		threads: store.NewThreadStore(s), questions: store.NewQuestionStore(s),
		hub: hub, queue: q, evlog: evlog, model: model,
		runID: games.NewID(), threadID: games.NewID(), userID: "op-1",
		stopEv: make(chan struct{}),
	}
	e.deps = Deps{
		Store: s, AgentStore: e.agent, Threads: e.threads, Questions: e.questions,
		Hub: hub, Events: evlog, Queue: q,
	}
	sub, err := hub.Subscribe(context.Background(), events.Filter{}, 0)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	go func() {
		defer sub.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		for {
			select {
			case <-e.stopEv:
				return
			default:
			}
			ev, _, err := sub.Next(ctx, 25*time.Millisecond)
			if err != nil {
				return
			}
			e.evMu.Lock()
			e.evs = append(e.evs, ev)
			e.evMu.Unlock()
		}
	}()
	t.Cleanup(func() { close(e.stopEv) })
	if err := e.agent.CreateAgentRun(ctx, store.AgentRun{
		ID: e.runID, State: store.AgentStateQueued,
		PlanJSON: []byte(`{}`), PromptVersion: PromptVersion,
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := e.threads.CreateThread(ctx, store.AssistantThread{
		ID: e.threadID, UserID: e.userID, RunID: e.runID, Title: "test thread",
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	return e
}

// seedProfile creates a game, edition, draft profile and two characters
// (the speaker and a target) for the mutation tests.
func (e *env) seedProfile(speakerID string) (profileID, targetID string) {
	e.t.Helper()
	ctx := e.ctx()
	now := time.Now().UTC().Format(time.RFC3339)
	g := games.Game{ID: games.NewID(), Slug: "gn-test", Title: "Gn Test", Platform: "wii", CreatedAt: now}
	if err := g.Validate(); err != nil {
		e.t.Fatalf("game: %v", err)
	}
	if err := e.s.CreateGame(ctx, g); err != nil {
		e.t.Fatalf("CreateGame: %v", err)
	}
	ed := games.Edition{ID: games.NewID(), GameID: g.ID, Region: "us", Language: "en", Revision: "v1"}
	if err := ed.Validate(); err != nil {
		e.t.Fatalf("edition: %v", err)
	}
	if err := e.s.CreateEdition(ctx, ed); err != nil {
		e.t.Fatalf("CreateEdition: %v", err)
	}
	p, err := e.s.CreateDraftProfile(ctx, ed.ID)
	if err != nil {
		e.t.Fatalf("CreateDraftProfile: %v", err)
	}
	targetID = games.NewID()
	if err := e.s.PutCharacters(ctx, p.ID, []games.Character{
		{ID: speakerID, ProfileID: p.ID, Name: "Speaker", PersonalitySummary: "cautious and quiet", Status: games.CharacterConfirmed, CreatedAt: now},
		{ID: targetID, ProfileID: p.ID, Name: "Target", Status: games.CharacterConfirmed, CreatedAt: now},
	}); err != nil {
		e.t.Fatalf("PutCharacters: %v", err)
	}
	return p.ID, targetID
}

// runTurn executes one assistant.run attempt over the job handler.
func (e *env) runTurn(userRequest, role string) JobResult {
	e.t.Helper()
	handler, err := NewJobHandler(e.model, e.deps, "scripted-model")
	if err != nil {
		e.t.Fatalf("NewJobHandler: %v", err)
	}
	payload, _ := json.Marshal(JobPayload{
		RunID: e.runID, ThreadID: e.threadID, UserID: e.userID,
		CallerRole: role, UserRequest: userRequest,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := handler(ctx, jobs.Job{ID: games.NewID(), Kind: JobKind, Payload: payload}, nil)
	if err != nil {
		e.t.Fatalf("attempt: %v", err)
	}
	var r JobResult
	if err := json.Unmarshal(res, &r); err != nil {
		e.t.Fatalf("result: %v: %s", err, res)
	}
	return r
}

func (e *env) ctx() context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	e.t.Cleanup(cancel)
	return ctx
}

// lastToolResult returns the run's last call row for name.
func (e *env) lastToolResult(name string) (store.AgentToolCall, error) {
	ctx := e.ctx()
	calls, err := e.agent.ListAgentToolCalls(ctx, e.runID)
	if err != nil {
		return store.AgentToolCall{}, err
	}
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].Name == name {
			return calls[i], nil
		}
	}
	return store.AgentToolCall{}, store.ErrNotFound
}

// toolResult is the persisted agent.Result envelope (schema v1).
type toolResult struct {
	Status  string          `json:"status"`
	Results json.RawMessage `json:"results"`
	Error   string          `json:"error"`
}

func (e *env) resultOf(name string) toolResult {
	e.t.Helper()
	tc, err := e.lastToolResult(name)
	if err != nil {
		e.t.Fatalf("no %s call: %v", name, err)
	}
	var r toolResult
	if err := json.Unmarshal(tc.ResultJSON, &r); err != nil {
		e.t.Fatalf("unmarshal %s result: %v", name, err)
	}
	return r
}

// characterBrief reads one character's personality summary from the store.
func (e *env) characterBrief(profileID, charID string) string {
	e.t.Helper()
	chars, err := e.s.ListCharacters(e.ctx(), profileID)
	if err != nil {
		e.t.Fatalf("ListCharacters: %v", err)
	}
	for _, c := range chars {
		if c.ID == charID {
			return c.PersonalitySummary
		}
	}
	e.t.Fatalf("character %s not in profile %s", charID, profileID)
	return ""
}

// appendAnswer simulates the API's answer write: the durable user message
// plus the row's answered state.
func (e *env) appendAnswer(q store.AssistantQuestion, answer string) {
	e.t.Helper()
	content, _ := json.Marshal(map[string]string{
		"role": "user", "content": RenderAnswerMessage(q.Kind, q.ID, q.Prompt, answer),
	})
	seq, err := e.agent.AppendAgentMessage(e.ctx(), e.runID, "user", content)
	if err != nil {
		e.t.Fatalf("append answer: %v", err)
	}
	if err := e.questions.AnswerQuestion(e.ctx(), q.ID, answer, seq); err != nil {
		e.t.Fatalf("answer question: %v", err)
	}
}

// openConfirmation returns the thread's only open confirmation card.
func (e *env) openConfirmation() store.AssistantQuestion {
	e.t.Helper()
	qlist, err := e.questions.ListQuestions(e.ctx(), e.threadID, 10)
	if err != nil {
		e.t.Fatalf("ListQuestions: %v", err)
	}
	for _, q := range qlist {
		if q.Kind == store.AssistantQuestionKindConfirmation && q.Answer == "" {
			return q
		}
	}
	e.t.Fatalf("no open confirmation among %d questions", len(qlist))
	return store.AssistantQuestion{}
}

func (e *env) hasEvent(name string, contains string) bool {
	e.evMu.Lock()
	defer e.evMu.Unlock()
	for _, ev := range e.evs {
		if ev.Name == name && strings.Contains(string(ev.Data), contains) {
			return true
		}
	}
	return false
}

// waitForEvent polls the collector: the run finished, but the collector
// goroutine drains the hub at 25 ms granularity.
func (e *env) waitForEvent(name, contains string) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if e.hasEvent(name, contains) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return e.hasEvent(name, contains)
}

// ── tests ──────────────────────────────────────────────────────────────────

// TestScreenContextReachesThePrompt is the UX-01 half: the rendered context
// sits in the user turn of the first request, and the tool the model called
// actually ran (tool-call parsing over the scripted boundary).
func TestScreenContextReachesThePrompt(t *testing.T) {
	charID := games.NewID()
	model := &scriptModel{responses: []scriptedResp{
		{text: "(never used)"},
		{text: "The character is Speaker."},
	}}
	e := newEnv(t, model, nil)
	profileID, _ := e.seedProfile(charID)
	model.responses[0] = scriptedResp{toolCalls: []openai.ToolCall{{
		ID: "c1", Name: "get_character",
		Arguments: json.RawMessage(`{"profile_id":"` + profileID + `","character_id":"` + charID + `"}`),
	}}}

	ctx := Context{
		Route:     "/profiles/" + profileID + "/characters",
		ProfileID: profileID,
		Selection: &Selection{Type: "character", ID: charID},
	}
	out := e.runTurn(RenderTurn("who is this character?", ctx), "operator")
	if out.State != "succeeded" {
		t.Fatalf("state = %s (reason %s), want succeeded", out.State, out.StopReason)
	}

	// The context reached the prompt in the user turn.
	texts := model.requestTexts(0)
	found := false
	for _, txt := range texts {
		if strings.Contains(txt, "[SCREEN CONTEXT]") &&
			strings.Contains(txt, "selection: character "+charID) &&
			strings.Contains(txt, "route: /profiles/"+profileID+"/characters") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the first request carries no rendered screen context: %v", texts)
	}

	// The tool ran against the real store.
	r := e.resultOf("get_character")
	if r.Status != "succeeded" || !strings.Contains(string(r.Results), "cautious and quiet") {
		t.Fatalf("tool result = %+v; want the character's brief", r)
	}
	// The narrative card was published with the answer.
	if !e.waitForEvent(EventMessage, "The character is Speaker.") {
		t.Fatal("no assistant.message card carrying the answer")
	}
}

// TestQuestionNeverBlocks is UX-04 at the loop level: ask_user answers a
// normal result, the run continues to a terminal success, the question row
// exists, and the card event was published.
func TestQuestionNeverBlocks(t *testing.T) {
	model := &scriptModel{responses: []scriptedResp{
		{toolCalls: []openai.ToolCall{{
			ID: "c1", Name: "ask_user",
			Arguments: json.RawMessage(`{"prompt":"which edition?","options":["us","eu"]}`),
		}}},
		{text: "Asked; continuing with the safe work."},
	}}
	e := newEnv(t, model, nil)
	out := e.runTurn(RenderTurn("onboard this game", Context{}), "operator")
	if out.State != "succeeded" {
		t.Fatalf("state = %s (reason %s); the question must not block the run", out.State, out.StopReason)
	}
	// The question row exists, open, with options.
	list, err := e.questions.ListQuestions(e.ctx(), e.threadID, 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("questions = %d, %v; want 1", len(list), err)
	}
	q := list[0]
	if q.Kind != store.AssistantQuestionKindQuestion || q.Answer != "" || q.Applied {
		t.Fatalf("question = %+v; want open", q)
	}
	// The tool answered a succeeded envelope (not a review/required stop).
	r := e.resultOf("ask_user")
	if r.Status != "succeeded" {
		t.Fatalf("tool status = %s; the question must be a normal result", r.Status)
	}
	if !e.waitForEvent(EventQuestion, "which edition?") {
		t.Fatal("no assistant.question card published")
	}
}

// TestConfirmationGateIsTwoPhase is the ADR-0099 decision 5 cycle: stage
// (no side effect, card emitted) → the operator confirms → the follow-up
// attempt re-invokes with the confirmation id and the write lands. Cancel
// and argument-mismatch are refused without a write.
func TestConfirmationGateIsTwoPhase(t *testing.T) {
	charID := games.NewID()
	e := newEnv(t, &scriptModel{}, nil)
	profileID, _ := e.seedProfile(charID)

	// ── attempt 1: stage ──
	e.model.requests = nil // fresh counter: the hook indexes from zero
	e.model.responses = []scriptedResp{
		{toolCalls: []openai.ToolCall{{
			ID: "c1", Name: "update_voice_brief",
			Arguments: json.RawMessage(`{"profile_id":"` + profileID + `","character_id":"` + charID + `","brief":"braver now"}`),
		}}},
		{text: "Staged the brief change for your confirmation."},
	}
	out := e.runTurn(RenderTurn("make her braver", Context{}), "operator")
	if out.State != "succeeded" {
		t.Fatalf("stage attempt = %s; want succeeded (the card must not block)", out.State)
	}
	// No write happened: the brief is untouched.
	if got := e.characterBrief(profileID, charID); got != "cautious and quiet" {
		t.Fatalf("brief changed before confirmation: %q", got)
	}
	// The staged confirmation row carries the tool and canonical args.
	q := e.openConfirmation()
	var staged stagedAction
	if err := json.Unmarshal(q.ContextRaw, &staged); err != nil || staged.Tool != "update_voice_brief" {
		t.Fatalf("staged = %s (%v); want the tool named", string(q.ContextRaw), err)
	}
	if !e.waitForEvent(EventQuestion, q.ID) {
		t.Fatal("no confirmation card published")
	}

	// The operator confirms.
	e.appendAnswer(q, "confirm")

	// ── attempt 2: apply (resume; the conversation carries the turn) ──
	e.model.requests = nil // fresh counter: the hook indexes from zero
	e.model.responses = []scriptedResp{
		{toolCalls: []openai.ToolCall{{
			ID: "c2", Name: "update_voice_brief",
			Arguments: json.RawMessage(`{"profile_id":"` + profileID + `","character_id":"` + charID + `","brief":"braver now","confirmation":"` + q.ID + `"}`),
		}}},
		{text: "Applied the brief change."},
	}
	out = e.runTurn("", "operator")
	if out.State != "succeeded" {
		t.Fatalf("apply attempt = %s; want succeeded", out.State)
	}
	if got := e.characterBrief(profileID, charID); got != "braver now" {
		t.Fatalf("brief after confirmed apply = %q; want the new brief", got)
	}
	// The answer's seq postdates the attempt's start (it was appended
	// before the attempt), so it was seen by this attempt and no follow-up
	// is owed.
	if out.FollowUp {
		t.Fatal("a follow-up was enqueued although the answer was seen by the attempt")
	}

	// ── cancel: a fresh stage the operator refuses ──
	e.model.requests = nil // fresh counter: the hook indexes from zero
	e.model.responses = []scriptedResp{
		{toolCalls: []openai.ToolCall{{
			ID: "c3", Name: "update_voice_brief",
			Arguments: json.RawMessage(`{"profile_id":"` + profileID + `","character_id":"` + charID + `","brief":"quieter now"}`),
		}}},
		{text: "Staged; awaiting your call."},
	}
	out = e.runTurn("", "operator")
	if out.State != "succeeded" {
		t.Fatalf("re-stage = %s", out.State)
	}
	cancelQ := e.openConfirmation()
	e.appendAnswer(cancelQ, "cancel")
	e.model.requests = nil // fresh counter: the hook indexes from zero
	e.model.responses = []scriptedResp{
		{toolCalls: []openai.ToolCall{{
			ID: "c4", Name: "update_voice_brief",
			Arguments: json.RawMessage(`{"profile_id":"` + profileID + `","character_id":"` + charID + `","brief":"quieter now","confirmation":"` + cancelQ.ID + `"}`),
		}}},
		{text: "Understood — cancelled."},
	}
	out = e.runTurn("", "operator")
	if out.State != "succeeded" {
		t.Fatalf("cancel attempt = %s", out.State)
	}
	if got := e.characterBrief(profileID, charID); got != "braver now" {
		t.Fatalf("brief after a CANCELLED confirmation = %q; the cancel must not write", got)
	}

	// ── mismatch: the confirmation does not name these arguments ──
	e.model.requests = nil // fresh counter: the hook indexes from zero
	e.model.responses = []scriptedResp{
		{toolCalls: []openai.ToolCall{{
			ID: "c5", Name: "update_voice_brief",
			Arguments: json.RawMessage(`{"profile_id":"` + profileID + `","character_id":"` + charID + `","brief":"different now"}`),
		}}},
		{text: "Staged the other brief."},
	}
	_ = e.runTurn("", "operator")
	mismatchQ := e.openConfirmation()
	e.appendAnswer(mismatchQ, "confirm")
	e.model.requests = nil // fresh counter: the hook indexes from zero
	e.model.responses = []scriptedResp{
		{toolCalls: []openai.ToolCall{{
			ID: "c6", Name: "update_voice_brief",
			Arguments: json.RawMessage(`{"profile_id":"` + profileID + `","character_id":"` + charID + `","brief":"TAMPERED","confirmation":"` + mismatchQ.ID + `"}`),
		}}},
		{text: "The arguments no longer match; re-staging."},
	}
	out = e.runTurn("", "operator")
	if out.State != "succeeded" {
		t.Fatalf("mismatch attempt = %s", out.State)
	}
	if got := e.characterBrief(profileID, charID); got != "braver now" {
		t.Fatalf("brief after a mismatched confirmation = %q; the gate must refuse the write", got)
	}
}

// TestOptOutSettingAppliesDirectly: with confirm_mutations off, a mutating
// tool writes on its first call (no card).
func TestOptOutSettingAppliesDirectly(t *testing.T) {
	charID := games.NewID()
	e := newEnv(t, &scriptModel{}, nil)
	profileID, _ := e.seedProfile(charID)
	if err := e.s.SetAssistantConfirmMutations(e.ctx(), e.userID, false); err != nil {
		t.Fatalf("set setting: %v", err)
	}
	e.model.requests = nil // fresh counter: the hook indexes from zero
	e.model.responses = []scriptedResp{
		{toolCalls: []openai.ToolCall{{
			ID: "c1", Name: "update_voice_brief",
			Arguments: json.RawMessage(`{"profile_id":"` + profileID + `","character_id":"` + charID + `","brief":"no-card brief"}`),
		}}},
		{text: "Done."},
	}
	out := e.runTurn(RenderTurn("change her brief", Context{}), "operator")
	if out.State != "succeeded" {
		t.Fatalf("state = %s", out.State)
	}
	if got := e.characterBrief(profileID, charID); got != "no-card brief" {
		t.Fatalf("brief = %q; with confirmations off the first call must apply", got)
	}
	if qlist, _ := e.questions.ListQuestions(e.ctx(), e.threadID, 10); len(qlist) != 0 {
		t.Fatalf("questions = %d; no card may be staged with confirmations off", len(qlist))
	}
}

// TestFollowUpHookEnqueuesExactlyOne is the UX-04 in-flight half: an answer
// durable AFTER the attempt's start seq is not seen by the attempt, so the
// handler enqueues exactly one follow-up attempt to apply it.
func TestFollowUpHookEnqueuesExactlyOne(t *testing.T) {
	e := newEnv(t, &scriptModel{}, nil)
	// Attempt 1: stage a question so the row exists.
	e.model.requests = nil // fresh counter: the hook indexes from zero
	e.model.responses = []scriptedResp{
		{toolCalls: []openai.ToolCall{{
			ID: "c1", Name: "ask_user",
			Arguments: json.RawMessage(`{"prompt":"which tone?","options":["warm","dry"]}`),
		}}},
		{text: "Asked."},
	}
	if out := e.runTurn(RenderTurn("help me pick", Context{}), "operator"); out.State != "succeeded" {
		t.Fatalf("stage = %s", out.State)
	}
	qlist, _ := e.questions.ListQuestions(e.ctx(), e.threadID, 10)
	if len(qlist) != 1 {
		t.Fatalf("questions = %d", len(qlist))
	}
	q := qlist[0]
	e.queue.mu.Lock()
	e.queue.enqueued = nil
	e.queue.mu.Unlock()

	// Attempt 2: the answer arrives WHILE the attempt runs — the onCall hook
	// fires before the final response, i.e. after the attempt's start seq
	// was captured.
	e.model.onCall = func(n int) {
		if n != 1 {
			return
		}
		content, _ := json.Marshal(map[string]string{
			"role": "user", "content": RenderAnswerMessage("question", q.ID, q.Prompt, "warm"),
		})
		seq, err := e.agent.AppendAgentMessage(e.ctx(), e.runID, "user", content)
		if err != nil {
			t.Errorf("append in-flight answer: %v", err)
			return
		}
		if err := e.questions.AnswerQuestion(e.ctx(), q.ID, "warm", seq); err != nil {
			t.Errorf("answer in-flight: %v", err)
		}
	}
	e.model.requests = nil // fresh counter: the hook indexes from zero
	e.model.responses = []scriptedResp{
		{toolCalls: []openai.ToolCall{{
			ID: "c2", Name: "search",
			Arguments: json.RawMessage(`{"query":"tone"}`),
		}}},
		{text: "The answer came in; applying it."},
	}
	out := e.runTurn("", "operator")
	if out.State != "succeeded" {
		t.Fatalf("in-flight attempt = %s", out.State)
	}
	if !out.FollowUp {
		t.Fatal("the in-flight answer was not owed a follow-up")
	}
	kinds := e.queue.kinds()
	if len(kinds) != 1 || kinds[0] != JobKind {
		t.Fatalf("enqueued kinds = %v; want exactly one %s", kinds, JobKind)
	}
}

// TestRoleTiers: get_trace is developer-tier; an operator's attempt gets a
// refusal (the run still succeeds), a developer's gets the trace.
func TestRoleTiers(t *testing.T) {
	trace := "trace-123"
	evlog := &fakeEvents{trace: trace}
	raw, _ := json.Marshal(map[string]any{"frame": "synthetic-frame-1", "text": "synthetic ocr text"})
	if _, err := evlog.Append(context.Background(), events.Envelope{
		Name: "session.frame", TraceID: trace, Data: raw,
	}); err != nil {
		t.Fatalf("seed event: %v", err)
	}

	model := &scriptModel{responses: []scriptedResp{
		{toolCalls: []openai.ToolCall{{
			ID: "c1", Name: "get_trace",
			Arguments: json.RawMessage(`{"trace_id":"` + trace + `"}`),
		}}},
		{text: "Trace read."},
	}}
	e := newEnv(t, model, evlog)
	out := e.runTurn(RenderTurn("why did it misread?", Context{}), "operator")
	if out.State != "succeeded" {
		t.Fatalf("operator attempt = %s; a refusal must not fail the run", out.State)
	}
	tc, err := e.lastToolResult("get_trace")
	if err != nil {
		t.Fatalf("no get_trace call: %v", err)
	}
	var refused toolResult
	if err := json.Unmarshal(tc.ResultJSON, &refused); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if refused.Status != "refused" || !strings.Contains(refused.Error, "developer") {
		t.Fatalf("operator get_trace = %+v; want the refusal naming the tier", refused)
	}

	// A developer's attempt (its own thread/run) may read the trace.
	model2 := &scriptModel{responses: []scriptedResp{
		{toolCalls: []openai.ToolCall{{
			ID: "c1", Name: "get_trace",
			Arguments: json.RawMessage(`{"trace_id":"` + trace + `"}`),
		}}},
		{text: "The frame carried synthetic ocr text."},
	}}
	e2 := newEnv(t, model2, evlog)
	out2 := e2.runTurn(RenderTurn("why did it misread?", Context{}), "developer")
	if out2.State != "succeeded" {
		t.Fatalf("developer attempt = %s", out2.State)
	}
	dev := e2.resultOf("get_trace")
	if dev.Status != "succeeded" || !strings.Contains(string(dev.Results), "synthetic ocr text") {
		t.Fatalf("developer get_trace = %+v; want the trace content", dev)
	}
}

// TestBulkSpeakerFix is UX-05: find_similar_lines names the pattern set, and
// the confirmed apply_speaker_fix re-points exactly the named ids (nothing
// else), in one attempt.
func TestBulkSpeakerFix(t *testing.T) {
	e := newEnv(t, &scriptModel{}, nil)
	ctx := e.ctx()
	now := time.Now().UTC().Format(time.RFC3339)
	g := games.Game{ID: games.NewID(), Slug: "bulk-test", Title: "Bulk Test", Platform: "wii", CreatedAt: now}
	if err := g.Validate(); err != nil {
		t.Fatalf("game: %v", err)
	}
	if err := e.s.CreateGame(ctx, g); err != nil {
		t.Fatalf("CreateGame: %v", err)
	}
	ed := games.Edition{ID: games.NewID(), GameID: g.ID, Region: "us", Language: "en", Revision: "v1"}
	if err := ed.Validate(); err != nil {
		t.Fatalf("edition: %v", err)
	}
	if err := e.s.CreateEdition(ctx, ed); err != nil {
		t.Fatalf("CreateEdition: %v", err)
	}
	p, err := e.s.CreateDraftProfile(ctx, ed.ID)
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	speakerChar := games.NewID()
	targetChar := games.NewID()
	sceneID := games.NewID()
	if err := e.s.PutScenes(ctx, p.ID, []games.Scene{{ID: sceneID, ProfileID: p.ID, Name: "Scene One", CreatedAt: now}}); err != nil {
		t.Fatalf("PutScenes: %v", err)
	}
	if err := e.s.PutCharacters(ctx, p.ID, []games.Character{
		{ID: speakerChar, ProfileID: p.ID, Name: "Speaker", Status: games.CharacterConfirmed, CreatedAt: now},
		{ID: targetChar, ProfileID: p.ID, Name: "Target", Status: games.CharacterConfirmed, CreatedAt: now},
	}); err != nil {
		t.Fatalf("PutCharacters: %v", err)
	}
	mk := func(id, text string) games.Message {
		return games.Message{
			ID: id, ProfileID: p.ID, OriginalText: text,
			NormalizedText: strings.ToLower(text), SpeechText: text,
			Language: "en", TextKind: games.TextDialogue, SceneID: sceneID,
			SpeakerID: speakerChar, AttributionStatus: games.AttributionUnknown,
			Revision: 1, CreatedAt: now, UpdatedAt: now,
		}
	}
	m1, m2, m3 := mk(games.NewID(), "Same line one"), mk(games.NewID(), "Same line two"), mk(games.NewID(), "Different line")
	m1.NormalizedText, m2.NormalizedText = "same line", "same line"
	if err := e.s.PutMessages(ctx, p.ID, []games.Message{m1, m2, m3}, nil); err != nil {
		t.Fatalf("PutMessages: %v", err)
	}
	bulkArgs := func(conf string) string {
		s := `{"profile_id":"` + p.ID + `","message_ids":["` + m1.ID + `","` + m2.ID + `"],"target_character_id":"` + targetChar + `"`
		if conf != "" {
			s += `,"confirmation":"` + conf + `"`
		}
		return s + "}"
	}

	// The read names the pattern set (similar = same normalized text;
	// same_speaker lists the rest of the speaker's lines, including m3).
	e.model.requests = nil // fresh counter: the hook indexes from zero
	e.model.responses = []scriptedResp{
		{toolCalls: []openai.ToolCall{{
			ID: "c1", Name: "find_similar_lines",
			Arguments: json.RawMessage(`{"profile_id":"` + p.ID + `","message_id":"` + m1.ID + `"}`),
		}}},
		{text: "Two lines share the pattern; staging the bulk fix."},
	}
	out := e.runTurn(RenderTurn("fix the speaker for this line and its twins", Context{}), "operator")
	if out.State != "succeeded" {
		t.Fatalf("read attempt = %s", out.State)
	}
	r := e.resultOf("find_similar_lines")
	if r.Status != "succeeded" {
		t.Fatalf("read status = %s: %s", r.Status, r.Error)
	}
	var similar struct {
		AnchorID string `json:"anchor_id"`
		Patterns map[string][]struct {
			ID string `json:"id"`
		} `json:"patterns"`
	}
	if err := json.Unmarshal(r.Results, &similar); err != nil {
		t.Fatalf("unmarshal results: %v", err)
	}
	if similar.AnchorID != m1.ID {
		t.Fatalf("anchor = %s; want %s", similar.AnchorID, m1.ID)
	}
	ids := func(label string) []string {
		var out []string
		for _, h := range similar.Patterns[label] {
			out = append(out, h.ID)
		}
		return out
	}
	sameText := ids("same_text_pattern")
	if !containsID(sameText, m2.ID) {
		t.Fatalf("the twin leaked out of same_text_pattern: %v", sameText)
	}
	if containsID(sameText, m3.ID) {
		t.Fatalf("the dissimilar line leaked into same_text_pattern: %v", sameText)
	}
	speakerPlate := ids("same_speaker_plate")
	if !containsID(speakerPlate, m2.ID) || !containsID(speakerPlate, m3.ID) {
		t.Fatalf("same_speaker_plate missing a line: %v", speakerPlate)
	}

	// Stage the bulk fix (the confirmation card names the exact ids).
	e.model.requests = nil // fresh counter: the hook indexes from zero
	e.model.responses = []scriptedResp{
		{toolCalls: []openai.ToolCall{{
			ID: "c2", Name: "apply_speaker_fix",
			Arguments: json.RawMessage(bulkArgs("")),
		}}},
		{text: "Staged the bulk fix for your confirmation."},
	}
	out = e.runTurn("", "operator")
	if out.State != "succeeded" {
		t.Fatalf("stage = %s", out.State)
	}
	msg, _, _ := e.s.GetMessage(ctx, p.ID, m1.ID)
	if msg.SpeakerID != speakerChar {
		t.Fatal("a line moved before the confirmation")
	}
	fixQ := e.openConfirmation()

	// The operator confirms and the follow-up attempt applies.
	e.appendAnswer(fixQ, "confirm")
	e.model.requests = nil // fresh counter: the hook indexes from zero
	e.model.responses = []scriptedResp{
		{toolCalls: []openai.ToolCall{{
			ID: "c3", Name: "apply_speaker_fix",
			Arguments: json.RawMessage(bulkArgs(fixQ.ID)),
		}}},
		{text: "Bulk fix applied to the confirmed set."},
	}
	out = e.runTurn("", "operator")
	if out.State != "succeeded" {
		t.Fatalf("apply = %s", out.State)
	}
	msg1, _, _ := e.s.GetMessage(ctx, p.ID, m1.ID)
	msg2, _, _ := e.s.GetMessage(ctx, p.ID, m2.ID)
	msg3, _, _ := e.s.GetMessage(ctx, p.ID, m3.ID)
	if msg1.SpeakerID != targetChar || msg2.SpeakerID != targetChar {
		t.Fatalf("twin lines not re-pointed: %s / %s", msg1.SpeakerID, msg2.SpeakerID)
	}
	if msg3.SpeakerID != speakerChar {
		t.Fatal("a line outside the confirmed set was re-pointed")
	}
}

func containsID(ids []string, id string) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

// TestRegenerateVoiceStagesTheDraft: the voice tool creates a draft revision
// and queues the audition job; the previous take stays (the undo). The user
// has confirmations off, so the first call applies.
func TestRegenerateVoiceStagesTheDraft(t *testing.T) {
	charID := games.NewID()
	e := newEnv(t, &scriptModel{}, nil)
	profileID, _ := e.seedProfile(charID)
	if err := e.s.SetAssistantConfirmMutations(e.ctx(), e.userID, false); err != nil {
		t.Fatalf("set setting: %v", err)
	}
	fv := &fakeVoices{t: t}
	e.deps.Voices = fv
	e.model.requests = nil // fresh counter: the hook indexes from zero
	e.model.responses = []scriptedResp{
		{toolCalls: []openai.ToolCall{{
			ID: "c1", Name: "regenerate_voice",
			Arguments: json.RawMessage(`{"profile_id":"` + profileID + `","character_id":"` + charID + `","brief":"a steadier read","count":2}`),
		}}},
		{text: "Queued a new take; the old one is still there."},
	}
	out := e.runTurn(RenderTurn("redo her voice", Context{}), "operator")
	if out.State != "succeeded" {
		t.Fatalf("state = %s", out.State)
	}
	if len(fv.created) != 1 || fv.created[0].Character != charID || fv.created[0].Brief != "a steadier read" || fv.created[0].N != 2 {
		t.Fatalf("drafts = %+v; want one for the character with the brief", fv.created)
	}
	kinds := e.queue.kinds()
	if len(kinds) != 1 || kinds[0] != "voice.auditions" {
		t.Fatalf("enqueued = %v; want the voice.auditions job", kinds)
	}
}

type fakeVoices struct {
	t       *testing.T
	mu      sync.Mutex
	created []fakeDraft
}

type fakeDraft struct {
	Character string
	Brief     string
	N         int
}

func (f *fakeVoices) CreateDraft(ctx context.Context, character, brief string, n int) (store.VoiceProfile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, fakeDraft{Character: character, Brief: brief, N: n})
	return store.VoiceProfile{ID: games.NewID(), Character: character, Revision: 2, State: "draft"}, nil
}
