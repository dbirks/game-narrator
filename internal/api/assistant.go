package api

// /api/v1/assistant — the context-aware assistant's API (ADR-0099): the
// agent dock's backend (docs/design/plum-peach UX-01/04/05).
//
// One thread is one durable agent run (agent_runs, migration 0007) owned by
// one user through assistant_threads (migration 0016). A message appends a
// user turn to the run's durable conversation and enqueues an
// `assistant.run` attempt on the workflow queue; the attempt executes the
// total-worklist loop (internal/agent) with the assistant's allowlist and
// versioned prompt (internal/assistant). The reply reaches the dock over
// the existing SSE hub: the loop's agent.step / tool.* events (tool cards)
// plus the assistant.message / assistant.question events this subsystem
// emits (narrative and NEEDS-YOU / confirmation cards).
//
// Questions never block (UX-04): ask_user and the confirmation gate answer
// a normal tool result, the attempt keeps running, and the answer — when it
// arrives — is a durable user message that exactly one follow-up attempt is
// owed (the busy/no-busy split below is what makes the follow-up exactly
// one).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/dbirks/game-narrator/internal/assistant"
	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
	rt "github.com/dbirks/game-narrator/internal/runtime"
	"github.com/dbirks/game-narrator/internal/store"
	"github.com/dbirks/game-narrator/internal/voices"
)

// assistantDeps is the wiring the /api/v1/assistant routes need. Queue runs
// the attempts; the four stores are the ADR-0099 tables plus the run log;
// the rest feed the tools (nil halves answer a failed envelope from the
// tool, a 503 from the route — never a silent no-op).
type assistantDeps struct {
	Store      *store.Store
	AgentStore *store.AgentStore
	Threads    *store.ThreadStore
	Questions  *store.QuestionStore
	Queue      JobQueue
	Hub        *events.Hub
	Voices     *voices.Service
	Sessions   *rt.Manager
	Events     events.Store
}

// WithAssistant wires the assistant routes. All store halves must be
// non-nil (the tables are migrated with the rest of the schema); queue, hub
// and the tool backends may be nil — the routes answer 503 for what they
// need and the tools degrade for the rest.
func WithAssistant(st *store.Store, queue JobQueue, hub *events.Hub, voiceSvc *voices.Service, sessions *rt.Manager, eventStore events.Store) Option {
	return func(s *Server) {
		s.assistantDeps = &assistantDeps{
			Store:      st,
			AgentStore: store.NewAgentStore(st),
			Threads:    store.NewThreadStore(st),
			Questions:  store.NewQuestionStore(st),
			Queue:      queue,
			Hub:        hub,
			Voices:     voiceSvc,
			Sessions:   sessions,
			Events:     eventStore,
		}
	}
}

const (
	assistantMaxText  = 4000
	assistantJobPri   = 20 // interactive work outranks background import/audition jobs
	assistantTitleMax = 80
	assistantListMax  = 50
	assistantConvTail = 40
)

type assistantContextWire = assistant.Context

type assistantMessageRequest struct {
	ThreadID string                `json:"thread_id"`
	Text     string                `json:"text"`
	Context  *assistantContextWire `json:"context"`
}

type assistantMessageResponse struct {
	ThreadID string `json:"thread_id"`
	RunID    string `json:"run_id"`
	JobID    string `json:"job_id"`
	Seq      int64  `json:"seq"`
}

type assistantAnswerRequest struct {
	ThreadID   string `json:"thread_id"`
	QuestionID string `json:"question_id"`
	Answer     string `json:"answer"`
}

// assistantRequired answers 503 when the assistant subsystem is not wired:
// "the API looks like it has an assistant" is the failure this rule exists
// to prevent.
func (s *Server) assistantRequired(w http.ResponseWriter, r *http.Request) bool {
	if s.assistantDeps == nil {
		s.writeProblem(w, r, http.StatusServiceUnavailable, "assistant-unavailable",
			"Assistant Unavailable", "the assistant is not configured in this process")
		return false
	}
	if s.assistantDeps.Store == nil || s.assistantDeps.Queue == nil {
		s.writeProblem(w, r, http.StatusServiceUnavailable, "assistant-unavailable",
			"Assistant Unavailable", "the assistant is partially configured; restart the service")
		return false
	}
	return true
}

// assistantIdentity answers the authenticated caller or a 401. The scope
// table already kept viewers and curators out of the prefix; this is the
// 401 for a request that reached the prefix without a principal (a bearer
// token is a session, not a user).
func (s *Server) assistantIdentity(w http.ResponseWriter, r *http.Request) (*auth.Identity, bool) {
	id := IdentityFromContext(r.Context())
	if id == nil || !id.Authenticated() {
		s.writeProblem(w, r, http.StatusUnauthorized, "unauthenticated", "Unauthorized",
			"an authenticated operator or above is required")
		return nil, false
	}
	return id, true
}

// assistantLoadThread answers the caller's thread or a problem. A thread the
// caller does not own is a 404, not a 403: an id guessed from another user's
// browser must not be an information leak (the ownership invariant in
// internal/store/assistant_store.go).
func (s *Server) assistantLoadThread(w http.ResponseWriter, r *http.Request, id, userID string) (store.AssistantThread, bool) {
	th, err := s.assistantDeps.Threads.GetThread(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.writeProblem(w, r, http.StatusNotFound, "thread-not-found", "Not Found",
				"no assistant thread has that identifier")
		} else {
			s.storeProblem(w, r, err, "load thread", "thread")
		}
		return store.AssistantThread{}, false
	}
	if th.UserID != userID {
		s.writeProblem(w, r, http.StatusNotFound, "thread-not-found", "Not Found",
			"no assistant thread has that identifier")
		return store.AssistantThread{}, false
	}
	return th, true
}

// assistantActiveJob is the queued/running assistant.run job for a run, if
// any — the 409 half of "one attempt owns a run's worklist".
func (s *Server) assistantActiveJob(ctx context.Context, runID string) (*store.Job, error) {
	cursor := ""
	for {
		page, next, err := s.jobStore.ListJobs(ctx, store.JobFilter{Kind: assistant.JobKind}, cursor, 0)
		if err != nil {
			return nil, err
		}
		for i := range page {
			j := &page[i]
			if (j.State == "queued" || j.State == "running") && assistantJobRunID(j.Payload) == runID {
				return j, nil
			}
		}
		if next == "" {
			return nil, nil
		}
		cursor = next
	}
}

func assistantJobRunID(payload json.RawMessage) string {
	var p assistant.JobPayload
	if len(payload) > 0 && json.Unmarshal(payload, &p) == nil {
		return p.RunID
	}
	return ""
}

func assistantEnqueuePayload(p assistant.JobPayload) (json.RawMessage, error) {
	return json.Marshal(p)
}

// assistantEnqueue queues one attempt and answers the queue problems the
// onboarding routes answer (the 202 half of the contract).
func (s *Server) assistantEnqueue(w http.ResponseWriter, r *http.Request, p assistant.JobPayload) string {
	payload, err := assistantEnqueuePayload(p)
	if err != nil {
		s.writeCodeProblem(w, r, http.StatusInternalServerError, "encode-fault", codeInternal,
			"Internal Server Error", "the request could not be prepared for the queue")
		return ""
	}
	job, _, err := s.assistantDeps.Queue.Enqueue(r.Context(), store.EnqueueParams{
		Kind: assistant.JobKind, Payload: payload,
		Priority: assistantJobPri, TraceID: p.RunID,
	})
	if err != nil {
		s.writeCodeProblem(w, r, http.StatusServiceUnavailable, "queue-unavailable", codeInternal,
			"Service Unavailable", "the job queue could not accept the assistant turn")
		return ""
	}
	return job.ID
}

// handleAssistantMessages serves POST /api/v1/assistant/messages: one
// dock message. With a thread_id it appends a turn to that thread's
// conversation; without one it opens the thread (run + thread rows) and
// carries the first turn as the attempt's opening user request. 202: the
// turn is accepted, not answered — the reply streams over the event hub.
func (s *Server) handleAssistantMessages(w http.ResponseWriter, r *http.Request) {
	if !s.assistantRequired(w, r) {
		return
	}
	id, ok := s.assistantIdentity(w, r)
	if !ok {
		return
	}
	var req assistantMessageRequest
	if !s.decodeJSONBody(w, r, &req, maxJSONBodyBytes) {
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" || len(text) > assistantMaxText {
		s.writeCodeProblem(w, r, http.StatusUnprocessableEntity, "invalid-message", codeValidationFailed,
			"Unprocessable Entity", "text is required and must be at most 4000 characters")
		return
	}
	ctx := assistant.Context{}
	if req.Context != nil {
		ctx = *req.Context
	}
	if err := ctx.Validate(); err != nil {
		s.writeCodeProblem(w, r, http.StatusUnprocessableEntity, "invalid-context", codeValidationFailed,
			"Unprocessable Entity", "context is malformed: "+err.Error())
		return
	}

	deps := s.assistantDeps
	userID := id.User.ID

	// ── an existing thread: append the turn ─────────────────────────────
	if req.ThreadID != "" {
		if !games.ValidID(req.ThreadID) {
			s.writeProblem(w, r, http.StatusNotFound, "thread-not-found", "Not Found",
				"no assistant thread has that identifier")
			return
		}
		th, ok := s.assistantLoadThread(w, r, req.ThreadID, userID)
		if !ok {
			return
		}
		run, err := deps.AgentStore.GetAgentRun(r.Context(), th.RunID)
		if err != nil {
			s.storeProblem(w, r, err, "load run", "run")
			return
		}
		if run.State == "failed" {
			s.writeCodeProblem(w, r, http.StatusConflict, "run-failed", codeConflict,
				"Conflict", "the thread's run has stopped on a fault; start a new thread instead of messaging it")
			return
		}
		if active, aerr := s.assistantActiveJob(r.Context(), th.RunID); aerr != nil {
			s.storeProblem(w, r, aerr, "check active job", "job")
			return
		} else if active != nil {
			s.writeCodeProblem(w, r, http.StatusConflict, "run-in-progress", codeConflict,
				"Conflict", fmt.Sprintf("thread %s already has an attempt queued or running; wait for it to finish", th.ID))
			return
		}
		content, err := json.Marshal(map[string]string{"role": "user", "content": assistant.RenderTurn(text, ctx)})
		if err != nil {
			s.writeCodeProblem(w, r, http.StatusInternalServerError, "encode-fault", codeInternal,
				"Internal Server Error", "the message could not be stored")
			return
		}
		seq, err := deps.AgentStore.AppendAgentMessage(r.Context(), th.RunID, "user", content)
		if err != nil {
			s.storeProblem(w, r, err, "append message", "run")
			return
		}
		_ = deps.Threads.TouchThread(r.Context(), th.ID)
		jobID := s.assistantEnqueue(w, r, assistant.JobPayload{
			RunID: th.RunID, ThreadID: th.ID, UserID: userID,
			CallerRole: auth.RoleNames[id.Role],
		})
		if jobID == "" {
			return
		}
		s.auditCatalogWrite(r, "assistant.message", map[string]any{
			"thread_id": th.ID, "run_id": th.RunID, "seq": seq,
		})
		writeJSON(w, http.StatusAccepted, assistantMessageResponse{
			ThreadID: th.ID, RunID: th.RunID, JobID: jobID, Seq: seq,
		})
		return
	}

	// ── no thread: open one ─────────────────────────────────────────────
	runID := games.NewID()
	threadID := games.NewID()
	profileID := ctx.ProfileID
	if profileID != "" {
		if _, _, err := s.store.GetProfile(r.Context(), profileID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				s.writeCodeProblem(w, r, http.StatusUnprocessableEntity, "unknown-profile", codeValidationFailed,
					"Unprocessable Entity", "context.profile_id does not name a profile this appliance has")
				return
			}
			s.storeProblem(w, r, err, "get profile", "profile")
			return
		}
	}
	plan, err := json.Marshal(map[string]any{
		"thread_id":  threadID,
		"game_id":    ctx.GameID,
		"profile_id": profileID,
		"created_at": nowStamp(),
	})
	if err != nil {
		s.writeCodeProblem(w, r, http.StatusInternalServerError, "encode-fault", codeInternal,
			"Internal Server Error", "the run plan could not be frozen")
		return
	}
	if err := deps.AgentStore.CreateAgentRun(r.Context(), store.AgentRun{
		ID:            runID,
		ProfileID:     profileID,
		State:         store.AgentStateQueued,
		PlanJSON:      plan,
		PromptVersion: assistant.PromptVersion,
	}); err != nil {
		s.storeProblem(w, r, err, "create run row", "run")
		return
	}
	title := text
	if len([]rune(title)) > assistantTitleMax {
		title = string([]rune(title)[:assistantTitleMax]) + "…"
	}
	if err := deps.Threads.CreateThread(r.Context(), store.AssistantThread{
		ID: threadID, UserID: userID, RunID: runID, Title: title,
	}); err != nil {
		s.storeProblem(w, r, err, "create thread", "thread")
		return
	}
	jobID := s.assistantEnqueue(w, r, assistant.JobPayload{
		RunID: runID, ThreadID: threadID, UserID: userID,
		CallerRole:  auth.RoleNames[id.Role],
		UserRequest: assistant.RenderTurn(text, ctx),
	})
	if jobID == "" {
		return
	}
	s.auditCatalogWrite(r, "assistant.thread.create", map[string]any{
		"thread_id": threadID, "run_id": runID,
		"profile_id": profileID, "game_id": ctx.GameID,
	})
	writeJSON(w, http.StatusAccepted, assistantMessageResponse{
		ThreadID: threadID, RunID: runID, JobID: jobID,
	})
}

// handleAssistantAnswers serves POST /api/v1/assistant/answers: one
// NEEDS-YOU answer (UX-04). The answer is a durable user message in the
// thread's conversation plus the question row's answered state, written so
// that exactly one follow-up attempt is owed:
//
//   - an attempt in flight: the job handler's post-run hook sees the
//     answer's seq postdate the attempt's start seq and enqueues the
//     follow-up; the API enqueues nothing (a second enqueue would replay
//     the turn);
//   - no attempt in flight: nobody will run the hook, so the API enqueues
//     the follow-up itself — re-checked after the answer is durable, so an
//     attempt that finishes between the two checks cannot drop the follow-up
//     (its hook either saw the answer or did not; the re-check decides who
//     owes it).
func (s *Server) handleAssistantAnswers(w http.ResponseWriter, r *http.Request) {
	if !s.assistantRequired(w, r) {
		return
	}
	id, ok := s.assistantIdentity(w, r)
	if !ok {
		return
	}
	var req assistantAnswerRequest
	if !s.decodeJSONBody(w, r, &req, maxJSONBodyBytes) {
		return
	}
	if !games.ValidID(req.ThreadID) || !games.ValidID(req.QuestionID) {
		s.writeCodeProblem(w, r, http.StatusUnprocessableEntity, "invalid-answer", codeValidationFailed,
			"Unprocessable Entity", "thread_id and question_id must be identifiers")
		return
	}
	answer := strings.TrimSpace(req.Answer)
	if len(answer) > assistantMaxText {
		s.writeCodeProblem(w, r, http.StatusUnprocessableEntity, "invalid-answer", codeValidationFailed,
			"Unprocessable Entity", "answer must be at most 4000 characters")
		return
	}

	deps := s.assistantDeps
	th, ok := s.assistantLoadThread(w, r, req.ThreadID, id.User.ID)
	if !ok {
		return
	}
	q, err := deps.Questions.GetQuestion(r.Context(), req.QuestionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.writeProblem(w, r, http.StatusNotFound, "question-not-found", "Not Found",
				"no question has that identifier")
		} else {
			s.storeProblem(w, r, err, "load question", "question")
		}
		return
	}
	if q.ThreadID != th.ID {
		s.writeProblem(w, r, http.StatusNotFound, "question-not-found", "Not Found",
			"no question has that identifier")
		return
	}
	if q.Kind != store.AssistantQuestionKindQuestion && q.Kind != store.AssistantQuestionKindConfirmation {
		s.writeProblem(w, r, http.StatusNotFound, "question-not-found", "Not Found",
			"no question has that identifier")
		return
	}
	if q.Kind == store.AssistantQuestionKindConfirmation {
		switch {
		case strings.EqualFold(answer, "confirm"), strings.EqualFold(answer, "cancel"):
			// the stored row keeps the canonical spelling
		case answer == "":
			s.writeCodeProblem(w, r, http.StatusUnprocessableEntity, "invalid-answer", codeValidationFailed,
				"Unprocessable Entity", "a confirmation answer is confirm or cancel")
			return
		default:
			s.writeCodeProblem(w, r, http.StatusUnprocessableEntity, "invalid-answer", codeValidationFailed,
				"Unprocessable Entity", "a confirmation answer is confirm or cancel")
			return
		}
	}

	// The durable user message the follow-up reads (rendered from the STORED
	// row's prompt, never from the caller — the prompt text is re-derived so
	// the conversation shows the question the operator actually saw).
	content, err := json.Marshal(map[string]string{
		"role": "user", "content": assistant.RenderAnswerMessage(q.Kind, q.ID, q.Prompt, answer),
	})
	if err != nil {
		s.writeCodeProblem(w, r, http.StatusInternalServerError, "encode-fault", codeInternal,
			"Internal Server Error", "the answer could not be stored")
		return
	}
	seq, err := deps.AgentStore.AppendAgentMessage(r.Context(), th.RunID, "user", content)
	if err != nil {
		s.storeProblem(w, r, err, "append message", "run")
		return
	}
	stored := answer
	if q.Kind == store.AssistantQuestionKindConfirmation {
		stored = strings.ToLower(answer)
	}
	if err := deps.Questions.AnswerQuestion(r.Context(), q.ID, stored, seq); err != nil {
		s.storeProblem(w, r, err, "answer question", "question")
		return
	}
	_ = deps.Threads.TouchThread(r.Context(), th.ID)

	// The exactly-one-follow-up split (documented above).
	var jobID string
	if active, aerr := s.assistantActiveJob(r.Context(), th.RunID); aerr != nil {
		s.storeProblem(w, r, aerr, "check active job", "job")
		return
	} else if active == nil {
		jobID = s.assistantEnqueue(w, r, assistant.JobPayload{
			RunID: th.RunID, ThreadID: th.ID, UserID: id.User.ID,
			CallerRole: auth.RoleNames[id.Role],
		})
		if jobID == "" {
			return
		}
	}
	s.auditCatalogWrite(r, "assistant.answer", map[string]any{
		"thread_id": th.ID, "run_id": th.RunID,
		"question_id": q.ID, "kind": q.Kind, "seq": seq,
	})
	writeJSON(w, http.StatusAccepted, map[string]any{
		"thread_id": th.ID, "run_id": th.RunID,
		"question_id": q.ID, "seq": seq,
		"job_id": jobID, // empty while an attempt in flight owns the follow-up
	})
}

// handleAssistantThreads serves GET /api/v1/assistant/threads: the caller's
// threads, newest first, cursor-paginated.
func (s *Server) handleAssistantThreads(w http.ResponseWriter, r *http.Request) {
	if !s.assistantRequired(w, r) {
		return
	}
	id, ok := s.assistantIdentity(w, r)
	if !ok {
		return
	}
	threads, next, err := s.assistantDeps.Threads.ListThreads(r.Context(), id.User.ID, r.URL.Query().Get("cursor"), assistantListMax)
	if err != nil {
		s.storeProblem(w, r, err, "list threads", "thread")
		return
	}
	rows := make([]map[string]any, 0, len(threads))
	for _, t := range threads {
		run, err := s.assistantDeps.AgentStore.GetAgentRun(r.Context(), t.RunID)
		state := "unknown"
		if err == nil {
			state = run.State
		}
		rows = append(rows, map[string]any{
			"id": t.ID, "run_id": t.RunID, "title": t.Title,
			"state":      state,
			"created_at": t.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
			"updated_at": t.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"threads": rows, "next_cursor": next})
}

// handleAssistantThread serves GET /api/v1/assistant/threads/{id}: the
// snapshot a page that missed the stream rebuilds from — the conversation
// tail (user/assistant turns), the open and recent questions, and the tool
// cards (the run's executed tool calls with their durations).
func (s *Server) handleAssistantThread(w http.ResponseWriter, r *http.Request) {
	if !s.assistantRequired(w, r) {
		return
	}
	id, ok := s.assistantIdentity(w, r)
	if !ok {
		return
	}
	threadID := strings.TrimSpace(r.PathValue("id"))
	if !games.ValidID(threadID) {
		s.writeProblem(w, r, http.StatusNotFound, "thread-not-found", "Not Found",
			"no assistant thread has that identifier")
		return
	}
	deps := s.assistantDeps
	th, ok := s.assistantLoadThread(w, r, threadID, id.User.ID)
	if !ok {
		return
	}
	ctx := r.Context()
	run, err := deps.AgentStore.GetAgentRun(ctx, th.RunID)
	if err != nil {
		s.storeProblem(w, r, err, "load run", "run")
		return
	}
	msgs, err := deps.AgentStore.ListAgentMessages(ctx, th.RunID)
	if err != nil {
		s.storeProblem(w, r, err, "list messages", "run")
		return
	}
	calls, err := deps.AgentStore.ListAgentToolCalls(ctx, th.RunID)
	if err != nil {
		s.storeProblem(w, r, err, "list tool calls", "run")
		return
	}
	questions, err := deps.Questions.ListQuestions(ctx, th.ID, assistantListMax)
	if err != nil {
		s.storeProblem(w, r, err, "list questions", "question")
		return
	}

	// The conversation tail: user/assistant turns only (tool steps live in
	// the tool cards below), newest last.
	type turn struct {
		Seq     int64  `json:"seq"`
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	var turns []turn
	for i := len(msgs) - 1; i >= 0 && len(turns) < assistantConvTail; i-- {
		m := msgs[i]
		if m.Role != "user" && m.Role != "assistant" {
			continue
		}
		var body struct {
			Content string `json:"content"`
		}
		if json.Unmarshal(m.ContentJSON, &body) == nil {
			turns = append(turns, turn{Seq: m.Seq, Role: m.Role, Content: body.Content})
		}
	}
	for i, j := 0, len(turns)-1; i < j; i, j = i+1, j-1 {
		turns[i], turns[j] = turns[j], turns[i]
	}

	cards := make([]map[string]any, 0, len(calls))
	for _, c := range calls {
		card := map[string]any{
			"id": c.ID, "name": c.Name, "state": c.Status,
			"started_at": c.StartedAt.UTC().Format("2006-01-02T15:04:05Z"),
		}
		if c.FinishedAt != nil {
			card["finished_at"] = c.FinishedAt.UTC().Format("2006-01-02T15:04:05Z")
			card["duration_ms"] = c.FinishedAt.Sub(c.StartedAt).Milliseconds()
		}
		if len(c.ResultJSON) > 0 {
			var res struct {
				Status  string          `json:"status"`
				Error   string          `json:"error"`
				Results json.RawMessage `json:"results"`
			}
			if json.Unmarshal(c.ResultJSON, &res) == nil {
				card["status"] = res.Status
				if res.Error != "" {
					card["error"] = res.Error
				}
				if len(res.Results) > 0 {
					preview := string(res.Results)
					if len(preview) > 500 {
						preview = preview[:500]
					}
					card["result_preview"] = preview
				}
			}
		}
		cards = append(cards, card)
	}

	qRows := make([]map[string]any, 0, len(questions))
	for _, q := range questions {
		row := map[string]any{
			"id": q.ID, "kind": q.Kind, "prompt": q.Prompt,
			"options":    q.Options,
			"created_at": q.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		}
		if q.AnsweredAt != nil {
			row["answer"] = q.Answer
			row["answered_at"] = q.AnsweredAt.UTC().Format("2006-01-02T15:04:05Z")
		}
		qRows = append(qRows, row)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id": th.ID, "run_id": th.RunID, "title": th.Title,
		"state": run.State, "stop_reason": run.StopReason,
		"turns":      turns,
		"tool_cards": cards,
		"questions":  qRows,
	})
}
