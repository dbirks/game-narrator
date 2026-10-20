package api

// /api/v1/assistant API tests (ADR-0099, UX-01/04/05 backend). The appliance
// is the real stack — migrated store, real job queue, scope-table auth — but
// the attempts never execute: no handler is registered for assistant.run, so
// enqueued jobs sit queued, which is exactly the state the 409 busy rule
// guards. (Category (a): synthetic; the model boundary is exercised in
// internal/assistant, the live boundary in assistant_live_test.go.)

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/jobs"
	"github.com/dbirks/game-narrator/internal/store"
)

type assistantEnv struct {
	t         *testing.T
	ctx       context.Context
	a         *testAuth
	st        *store.Store
	js        *store.JobsStore
	agent     *store.AgentStore
	threads   *store.ThreadStore
	questions *store.QuestionStore
	h         http.Handler
}

func newAssistantEnv(t *testing.T) *assistantEnv {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	a := newTestAuth(t)
	js := store.NewJobsStore(a.st)
	hub := events.New(events.WithStore(store.NewEventStore(a.st)), events.WithLogger(discardLogger))
	runner, err := jobs.NewRunner(js, hub, discardLogger, jobs.WithPollInterval(2*time.Millisecond))
	if err != nil {
		t.Fatalf("jobs.NewRunner: %v", err)
	}
	if err := runner.Start(ctx); err != nil {
		t.Fatalf("runner.Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		_ = runner.Stop(stopCtx)
		_ = hub.Close()
	})
	h := a.handler(
		WithStore(a.st), WithJobs(js), WithEvents(hub),
		WithAssistant(a.st, runner, hub, nil, nil, nil),
	)
	return &assistantEnv{
		t: t, ctx: ctx, a: a, st: a.st, js: js,
		agent:     store.NewAgentStore(a.st),
		threads:   store.NewThreadStore(a.st),
		questions: store.NewQuestionStore(a.st),
		h:         h,
	}
}

func (e *assistantEnv) do(role auth.Role, method, target string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	var r *http.Request
	if body == nil {
		r = httptest.NewRequest(method, target, nil)
	} else {
		raw, err := json.Marshal(body)
		if err != nil {
			e.t.Fatalf("marshal body: %v", err)
		}
		r = httptest.NewRequest(method, target, bytes.NewReader(raw))
		r.Header.Set("Content-Type", ContentTypeJSON)
	}
	if role != auth.RoleNone {
		r.Header.Set("Authorization", "Bearer "+e.a.bearer(role))
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	return rec
}

// seedThread creates a run plus the thread row that owns it.
func (e *assistantEnv) seedThread(userID, runState, title string) (store.AssistantThread, string) {
	e.t.Helper()
	runID := games.NewID()
	threadID := games.NewID()
	if err := e.agent.CreateAgentRun(e.ctx, store.AgentRun{
		ID: runID, State: runState, PlanJSON: []byte(`{}`),
		PromptVersion: "assistant-1",
	}); err != nil {
		e.t.Fatalf("create run: %v", err)
	}
	th := store.AssistantThread{ID: threadID, UserID: userID, RunID: runID, Title: title}
	if err := e.threads.CreateThread(e.ctx, th); err != nil {
		e.t.Fatalf("create thread: %v", err)
	}
	return th, runID
}

// operatorID resolves the operator fixture account's user id.
func operatorUser(t *testing.T, a *testAuth) string {
	t.Helper()
	u, err := a.repo.UserByUsername(context.Background(), "operator")
	if err != nil {
		t.Fatalf("operator account: %v", err)
	}
	return u.ID
}

// ── the scope table and identity ───────────────────────────────────────────

// TestAssistantRoutesRoleGate: the scope table keeps viewer and curator out
// of the whole prefix, unauthenticated requests get the 401, and operator or
// above is let through.
func TestAssistantRoutesRoleGate(t *testing.T) {
	e := newAssistantEnv(t)
	body := map[string]any{"text": "hello"}
	for role, want := range map[auth.Role]int{
		auth.RoleViewer:    http.StatusForbidden,
		auth.RoleCurator:   http.StatusForbidden,
		auth.RoleOperator:  http.StatusAccepted,
		auth.RoleDeveloper: http.StatusAccepted,
		auth.RoleAdmin:     http.StatusAccepted,
	} {
		rec := e.do(role, http.MethodPost, "/api/v1/assistant/messages", body)
		if rec.Code != want {
			t.Fatalf("POST /assistant/messages as %s = %d, want %d (body %s)", role, rec.Code, want, rec.Body.String())
		}
	}
	rec := e.do(auth.RoleNone, http.MethodPost, "/api/v1/assistant/messages", body)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST unauthenticated = %d, want 401 (body %s)", rec.Code, rec.Body.String())
	}
	rec = e.do(auth.RoleViewer, http.MethodGet, "/api/v1/assistant/threads", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET /assistant/threads as viewer = %d, want 403", rec.Code)
	}
}

// ── the message flow ───────────────────────────────────────────────────────

// TestAssistantMessageFlow: the first message opens a thread (202 with the
// ids), the thread's list and snapshot carry it, a second message while the
// attempt is queued is a 409, and another user's thread is a 404.
func TestAssistantMessageFlow(t *testing.T) {
	e := newAssistantEnv(t)
	rec := e.do(auth.RoleOperator, http.MethodPost, "/api/v1/assistant/messages", map[string]any{
		"text": "who is this character?",
		"context": map[string]any{
			"route":       "/profiles/p1/characters",
			"selection":   map[string]any{"type": "character", "id": games.NewID()},
			"visible_ids": []string{},
		},
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("first message = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	var first struct {
		ThreadID string `json:"thread_id"`
		RunID    string `json:"run_id"`
		JobID    string `json:"job_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil || first.ThreadID == "" || first.RunID == "" || first.JobID == "" {
		t.Fatalf("202 body = %s; want thread_id, run_id and job_id", rec.Body.String())
	}

	// The thread is listed for the caller, with the run's state.
	rec = e.do(auth.RoleOperator, http.MethodGet, "/api/v1/assistant/threads", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET threads = %d", rec.Code)
	}
	var list struct {
		Threads []struct {
			ID    string `json:"id"`
			State string `json:"state"`
			Title string `json:"title"`
		} `json:"threads"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("threads body: %v", err)
	}
	if len(list.Threads) != 1 || list.Threads[0].ID != first.ThreadID || list.Threads[0].State != "queued" {
		t.Fatalf("thread list = %+v; want the new thread, queued", list.Threads)
	}

	// The attempt is still queued (no handler ran it): a second message is a
	// 409, not a second concurrent attempt.
	rec = e.do(auth.RoleOperator, http.MethodPost, "/api/v1/assistant/messages", map[string]any{
		"thread_id": first.ThreadID, "text": "and the next one?",
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("second message while queued = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}

	// The snapshot before the attempt ran: the turn travels in the queued
	// job's payload (the loop persists it on the fresh run), so the
	// conversation tail is still empty.
	rec = e.do(auth.RoleOperator, http.MethodGet, "/api/v1/assistant/threads/"+first.ThreadID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET thread = %d (body %s)", rec.Code, rec.Body.String())
	}
	var snap struct {
		ID    string `json:"id"`
		State string `json:"state"`
		Turns []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"turns"`
		ToolCards []any `json:"tool_cards"`
		Questions []any `json:"questions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatalf("snapshot body: %v", err)
	}
	if snap.ID != first.ThreadID || snap.State != "queued" || len(snap.Turns) != 0 {
		t.Fatalf("snapshot = id %s state %s turns %+v; want the queued run with no turns yet", snap.ID, snap.State, snap.Turns)
	}
	if len(snap.ToolCards) != 0 || len(snap.Questions) != 0 {
		t.Fatalf("fresh thread cards = %d/%d; want none", len(snap.ToolCards), len(snap.Questions))
	}
	// The queued job's payload carries the rendered first turn.
	job, err := e.js.GetJob(e.ctx, first.JobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	var payload struct {
		UserRequest string `json:"user_request"`
		ThreadID    string `json:"thread_id"`
	}
	if err := json.Unmarshal(job.Payload, &payload); err != nil || payload.ThreadID != first.ThreadID || !strings.Contains(payload.UserRequest, "who is this character?") {
		t.Fatalf("queued payload = %s; want the rendered first turn for the thread", job.Payload)
	}

	// The append path: a thread with no attempt in flight takes the turn
	// durably right away, and the snapshot shows it.
	appendTh, _ := e.seedThread(operatorUser(t, e.a), "succeeded", "append")
	rec = e.do(auth.RoleOperator, http.MethodPost, "/api/v1/assistant/messages", map[string]any{
		"thread_id": appendTh.ID, "text": "the appended turn",
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("append message = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	rec = e.do(auth.RoleOperator, http.MethodGet, "/api/v1/assistant/threads/"+appendTh.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET appended thread = %d", rec.Code)
	}
	var snap2 struct {
		Turns []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"turns"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &snap2); err != nil || len(snap2.Turns) != 1 || snap2.Turns[0].Role != "user" {
		t.Fatalf("appended snapshot turns = %+v; want the one user turn", snap2.Turns)
	}
	if !strings.Contains(snap2.Turns[0].Content, "the appended turn") {
		t.Fatalf("turn content = %q", snap2.Turns[0].Content)
	}

	// A different user's thread is a 404, not a 403 (no id enumeration).
	rec = e.do(auth.RoleDeveloper, http.MethodGet, "/api/v1/assistant/threads/"+first.ThreadID, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign thread GET = %d, want 404", rec.Code)
	}
	rec = e.do(auth.RoleDeveloper, http.MethodPost, "/api/v1/assistant/messages", map[string]any{
		"thread_id": first.ThreadID, "text": "hi",
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign thread POST = %d, want 404", rec.Code)
	}
}

// ── validation and the failed-run conflict ─────────────────────────────────

// TestAssistantMessageValidation: malformed turns are rejected before any row
// is written.
func TestAssistantMessageValidation(t *testing.T) {
	e := newAssistantEnv(t)
	cases := []struct {
		name string
		body map[string]any
		want int
		frag string
	}{
		{"empty text", map[string]any{"text": "   "}, http.StatusUnprocessableEntity, "text is required"},
		{"oversize text", map[string]any{"text": strings.Repeat("a", 4001)}, http.StatusUnprocessableEntity, "4000"},
		{"bad selection type", map[string]any{"text": "hi",
			"context": map[string]any{"selection": map[string]any{
				"type": strings.Repeat("t", 65), "id": games.NewID()}}},
			http.StatusUnprocessableEntity, "context"},
		{"unknown profile", map[string]any{"text": "hi",
			"context": map[string]any{"profile_id": games.NewID()}},
			http.StatusUnprocessableEntity, "profile"},
	}
	for _, c := range cases {
		rec := e.do(auth.RoleOperator, http.MethodPost, "/api/v1/assistant/messages", c.body)
		if rec.Code != c.want {
			t.Fatalf("%s: = %d, want %d (body %s)", c.name, rec.Code, c.want, rec.Body.String())
		}
		if c.frag != "" && !strings.Contains(rec.Body.String(), c.frag) {
			t.Fatalf("%s: body %s lacks %q", c.name, rec.Body.String(), c.frag)
		}
	}
}

// TestAssistantFailedRunConflict: a thread whose run stopped on a fault is not
// messageable; a fresh thread is the path.
func TestAssistantFailedRunConflict(t *testing.T) {
	e := newAssistantEnv(t)
	opID := operatorUser(t, e.a)
	th, _ := e.seedThread(opID, store.AgentStateFailed, "broken")
	rec := e.do(auth.RoleOperator, http.MethodPost, "/api/v1/assistant/messages", map[string]any{
		"thread_id": th.ID, "text": "still here?",
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("message a failed run = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "failed") {
		t.Fatalf("problem body %s should name the failure", rec.Body.String())
	}
}

// ── the answer flow (UX-04) ────────────────────────────────────────────────

// TestAssistantAnswerFlow: the answer is durable (message + row), the
// no-attempt-in-flight half enqueues exactly one follow-up job, the row's
// guarded re-answer is a conflict, and the confirmation answer grammar is
// enforced.
func TestAssistantAnswerFlow(t *testing.T) {
	e := newAssistantEnv(t)
	opID := operatorUser(t, e.a)
	th, runID := e.seedThread(opID, store.AgentStateSucceeded, "answered")

	qid := games.NewID()
	if err := e.questions.CreateQuestion(e.ctx, store.AssistantQuestion{
		ID: qid, ThreadID: th.ID, RunID: runID,
		Kind:   store.AssistantQuestionKindQuestion,
		Prompt: "which edition?", Options: []string{"us", "eu"},
	}); err != nil {
		t.Fatalf("create question: %v", err)
	}

	// A deferred answer (empty) is allowed on a plain question.
	rec := e.do(auth.RoleOperator, http.MethodPost, "/api/v1/assistant/answers", map[string]any{
		"thread_id": th.ID, "question_id": qid, "answer": "",
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("deferred answer = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	var first struct {
		JobID string `json:"job_id"`
		Seq   int64  `json:"seq"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil {
		t.Fatalf("answer body: %v", err)
	}
	// No attempt is in flight: the API owes the follow-up job.
	if first.JobID == "" {
		t.Fatal("no follow-up job_id; with no attempt in flight the API enqueues it")
	}
	// The row is answered (deferred keeps the empty answer, applied).
	q, err := e.questions.GetQuestion(e.ctx, qid)
	if err != nil {
		t.Fatalf("get question: %v", err)
	}
	if q.AnsweredAt == nil || q.Answer != "" {
		t.Fatalf("row = answer %q answered_at %v; want the deferred answer recorded", q.Answer, q.AnsweredAt)
	}
	// The durable user message renders the stored prompt, not the caller's.
	msgs, err := e.agent.ListAgentMessages(e.ctx, runID)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	found := false
	for _, m := range msgs {
		if m.Role == "user" && strings.Contains(string(m.ContentJSON), "deferred the question "+qid) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no durable answer message: %v", msgs)
	}

	// The guarded re-answer: a second answer to the same row is a conflict.
	rec = e.do(auth.RoleOperator, http.MethodPost, "/api/v1/assistant/answers", map[string]any{
		"thread_id": th.ID, "question_id": qid, "answer": "us",
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("re-answer = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}

	// Confirmation grammar: "maybe" is not an answer; "confirm" is.
	cid := games.NewID()
	if err := e.questions.CreateQuestion(e.ctx, store.AssistantQuestion{
		ID: cid, ThreadID: th.ID, RunID: runID,
		Kind:   store.AssistantQuestionKindConfirmation,
		Prompt: "update the voice brief?", Options: []string{"confirm", "cancel"},
		ContextRaw: []byte(`{"tool":"update_voice_brief","args":{}}`),
	}); err != nil {
		t.Fatalf("create confirmation: %v", err)
	}
	rec = e.do(auth.RoleOperator, http.MethodPost, "/api/v1/assistant/answers", map[string]any{
		"thread_id": th.ID, "question_id": cid, "answer": "maybe",
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("confirmation answer \"maybe\" = %d, want 422", rec.Code)
	}
	rec = e.do(auth.RoleOperator, http.MethodPost, "/api/v1/assistant/answers", map[string]any{
		"thread_id": th.ID, "question_id": cid, "answer": "Confirm",
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("confirmation answer \"Confirm\" = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	cq, _ := e.questions.GetQuestion(e.ctx, cid)
	if cq.Answer != "confirm" {
		t.Fatalf("stored confirmation answer = %q; want the canonical \"confirm\"", cq.Answer)
	}
	msgs, _ = e.agent.ListAgentMessages(e.ctx, runID)
	confirmed := false
	for _, m := range msgs {
		if strings.Contains(string(m.ContentJSON), "The user CONFIRMED the staged action "+cid) {
			confirmed = true
		}
	}
	if !confirmed {
		t.Fatal("no CONFIRMED answer message in the conversation")
	}
}

// TestAssistantAnswerUnknownQuestion: a question id the thread does not own
// is a 404, not an answer.
func TestAssistantAnswerUnknownQuestion(t *testing.T) {
	e := newAssistantEnv(t)
	opID := operatorUser(t, e.a)
	th, runID := e.seedThread(opID, store.AgentStateSucceeded, "orphaned")
	other, _ := e.seedThread(opID, store.AgentStateSucceeded, "other")
	foreign := games.NewID()
	if err := e.questions.CreateQuestion(e.ctx, store.AssistantQuestion{
		ID: foreign, ThreadID: other.ID, RunID: other.RunID,
		Kind: store.AssistantQuestionKindQuestion, Prompt: "elsewhere?",
	}); err != nil {
		t.Fatalf("seed foreign question: %v", err)
	}
	rec := e.do(auth.RoleOperator, http.MethodPost, "/api/v1/assistant/answers", map[string]any{
		"thread_id": th.ID, "question_id": foreign, "answer": "x",
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign question answer = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
	rec = e.do(auth.RoleOperator, http.MethodPost, "/api/v1/assistant/answers", map[string]any{
		"thread_id": th.ID, "question_id": games.NewID(), "answer": "x",
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown question answer = %d, want 404", rec.Code)
	}
	_ = runID
}

// TestAssistantThreadSnapshotCarriesCards: after the attempt's work (seeded
// directly: one tool call and one question row), the snapshot rebuilds what
// the dock missed — the tool card with its duration, the open question.
func TestAssistantThreadSnapshotCarriesCards(t *testing.T) {
	e := newAssistantEnv(t)
	opID := operatorUser(t, e.a)
	th, runID := e.seedThread(opID, store.AgentStateSucceeded, "cards")

	start := time.Now().UTC().Add(-2 * time.Second)
	finish := time.Now().UTC()
	callID := games.NewID()
	if _, err := e.agent.InsertAgentToolCall(e.ctx, store.AgentToolCall{
		ID: callID, RunID: runID, Name: "search",
		ArgsJSON: []byte(`{"query":"synthetic"}`),
		Status:   store.AgentToolPending, StartedAt: start,
	}); err != nil {
		t.Fatalf("insert call row: %v", err)
	}
	res, _ := json.Marshal(map[string]any{
		"schema_version": 1, "status": "succeeded",
		"results": map[string]any{"count": 3},
	})
	if err := e.agent.FinishAgentToolCall(e.ctx, callID, store.AgentToolSucceeded, res, finish, ""); err != nil {
		t.Fatalf("finish call row: %v", err)
	}
	qid := games.NewID()
	if err := e.questions.CreateQuestion(e.ctx, store.AssistantQuestion{
		ID: qid, ThreadID: th.ID, RunID: runID,
		Kind: store.AssistantQuestionKindQuestion, Prompt: "which one?",
		Options: []string{"a", "b"},
	}); err != nil {
		t.Fatalf("create question: %v", err)
	}

	rec := e.do(auth.RoleOperator, http.MethodGet, "/api/v1/assistant/threads/"+th.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET thread = %d (body %s)", rec.Code, rec.Body.String())
	}
	var snap struct {
		ToolCards []struct {
			Name       string `json:"name"`
			Status     string `json:"status"`
			DurationMS *int64 `json:"duration_ms"`
			Preview    string `json:"result_preview"`
		} `json:"tool_cards"`
		Questions []struct {
			ID      string   `json:"id"`
			Kind    string   `json:"kind"`
			Prompt  string   `json:"prompt"`
			Options []string `json:"options"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatalf("snapshot body: %v", err)
	}
	if len(snap.ToolCards) != 1 || snap.ToolCards[0].Name != "search" || snap.ToolCards[0].Status != "succeeded" {
		t.Fatalf("tool cards = %+v; want the succeeded search card", snap.ToolCards)
	}
	if snap.ToolCards[0].DurationMS == nil || *snap.ToolCards[0].DurationMS < 1900 {
		t.Fatalf("card duration = %v; want ~2000ms from the stored timestamps", snap.ToolCards[0].DurationMS)
	}
	if len(snap.Questions) != 1 || snap.Questions[0].ID != qid || len(snap.Questions[0].Options) != 2 {
		t.Fatalf("questions = %+v; want the open question with options", snap.Questions)
	}
}
