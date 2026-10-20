package assistant

// The assistant's durable-job side (ADR-0099): one run per thread, executed
// as the `assistant.run` job kind on the docs/spec/08 workflow queue. The
// onboarding run executes the same loop under its own kind with its own
// registry and prompt; the kind split is what keeps the two agents'
// allowlists and budgets independent.
//
// What this handler adds on top of agent.Run (the total-worklist loop,
// ADR-0025):
//
//  1. the assistant's own versioned system prompt (prompt.go), passed to the
//     loop through Options.SystemPrompt so the onboarding render never
//     renders an assistant run;
//  2. the narrative event: a finished attempt with a final answer emits
//     assistant.message on the hub (the dock's text card); the loop's own
//     agent.step / tool.started / tool.completed events (trace id = run id)
//     carry the step and tool-card tail;
//  3. the UX-04 follow-up: before the attempt, the run's last message seq is
//     captured (startSeq); after it, any answer whose durable message
//     postdates startSeq was not seen by this attempt, and exactly one
//     follow-up attempt is enqueued so the answer is applied (the run log is
//     the total record, so the follow-up sees it).

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dbirks/game-narrator/internal/agent"
	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/jobs"
	"github.com/dbirks/game-narrator/internal/store"
)

// JobKind is the durable queue kind one assistant turn executes as.
const JobKind = "assistant.run"

// JobPayload names the thread and run of the attempt. UserRequest is the
// opening turn for a fresh thread (rendered context + text) and must be
// empty on a resume (the conversation, including the appended turn, comes
// from the durable log). CallerRole is the enqueuing operator's role: the
// executor enforces the tool MinRoles against it (ADR-0099 decision 7), so
// a developer's attempt may read traces and an operator's may not.
type JobPayload struct {
	RunID       string `json:"run_id"`
	ThreadID    string `json:"thread_id"`
	UserID      string `json:"user_id"`
	CallerRole  string `json:"caller_role"`
	UserRequest string `json:"user_request,omitempty"`
}

// JobResult is the durable outcome an assistant.run attempt writes on its
// job row (docs/spec/13: the job row answers "why did this attempt stop?").
type JobResult struct {
	RunID      string `json:"run_id"`
	ThreadID   string `json:"thread_id"`
	State      string `json:"state"`
	StopReason string `json:"stop_reason,omitempty"`
	Answer     string `json:"answer,omitempty"`
	Steps      int    `json:"steps"`
	ToolCalls  int    `json:"tool_calls"`
	FollowUp   bool   `json:"follow_up_enqueued,omitempty"`
}

// JobEnqueuer is the slice of the durable queue the assistant needs:
// *jobs.Runner and *store.JobsStore both satisfy it.
type JobEnqueuer interface {
	Enqueue(ctx context.Context, p store.EnqueueParams) (*store.Job, bool, error)
}

// Deps is everything the assistant tools and the job handler need. Store,
// AgentStore, Threads and Questions are required; the rest may be nil, in
// which case the tools that need them answer a failed envelope with a clear
// reason (a misconfigured appliance must not crash the loop).
type Deps struct {
	// Store: catalogue, profile content, corrections, voices, settings.
	Store *store.Store
	// AgentStore is the durable run log (the thread's conversation).
	AgentStore *store.AgentStore
	// Threads and Questions are the ADR-0099 tables (migration 0016).
	Threads   *store.ThreadStore
	Questions *store.QuestionStore
	// Hub receives assistant.question / assistant.message. Nil: no events.
	Hub *events.Hub
	// Voices drives regenerate_voice. Nil: that tool fails gracefully.
	// *voices.Service satisfies it; tests may supply a narrower fake.
	Voices interface {
		CreateDraft(ctx context.Context, character, brief string, n int) (store.VoiceProfile, error)
	}
	// Sessions drives start/stop narration. Nil: those tools fail gracefully.
	// cmd/game-narrator adapts *runtime.Manager to NarrationController; tests
	// fake it — the assistant package therefore never imports internal/runtime.
	Sessions NarrationController
	// Events is the durable event log get_trace scans (developer tier).
	// Nil: that tool fails gracefully.
	Events events.Store
	// Queue queues the voice.auditions job regenerate_voice stages and the
	// follow-up attempt. Nil: staging answers a failed envelope.
	Queue JobEnqueuer
}

// RunContext is the per-attempt identity the tool closures bind: which
// thread, which run, whose settings the confirmation gate reads.
type RunContext struct {
	ThreadID string
	RunID    string
	UserID   string
}

// Budget is the assistant's per-attempt ceilings: an interactive turn is not
// a 30-minute research run (ADR-0099 decision 3).
var Budget = agent.BudgetConfig{MaxSteps: 12, MaxToolCalls: 24, DeadlineSeconds: 300}

// jobTimeout bounds one assistant.run attempt on the queue.
const jobTimeout = 5 * time.Minute

// JobHandlerOpts keeps the register call in cmd/game-narrator explicit.
func JobHandlerOpts() jobs.RegisterOpts {
	// One at a time per thread is the loop's invariant (one attempt owns the
	// run's worklist); the kind concurrency stays 1 on an appliance
	// (background work is one at a time in practice).
	return jobs.RegisterOpts{Concurrency: 1, Resumable: true, Timeout: jobTimeout}
}

// NewJobHandler returns the jobs.Handler for JobKind. model and the Deps'
// store quartet are required; a handler that cannot drive a run is a
// configuration fault the process must not start with.
func NewJobHandler(model agent.Model, deps Deps, modelLabel string) (jobs.Handler, error) {
	if model == nil {
		return nil, fmt.Errorf("assistant: JobHandler: model is required")
	}
	if deps.Store == nil || deps.AgentStore == nil || deps.Threads == nil || deps.Questions == nil {
		return nil, fmt.Errorf("assistant: JobHandler: Store, AgentStore, Threads and Questions are required")
	}
	// The dock's two card events join the vocabulary (idempotent: a second
	// process boot re-registers the same names).
	if err := events.Register(EventMessage, EventQuestion); err != nil {
		return nil, fmt.Errorf("assistant: JobHandler: registering event names: %w", err)
	}
	return func(ctx context.Context, job jobs.Job, _ jobs.ProgressFn) (json.RawMessage, error) {
		var p JobPayload
		if err := json.Unmarshal(job.Payload, &p); err != nil {
			return nil, jobs.Permanent(fmt.Errorf("assistant.run: payload is not valid JSON: %w", err))
		}
		if p.RunID == "" || p.ThreadID == "" {
			return nil, jobs.Permanent(fmt.Errorf("assistant.run: payload needs run_id and thread_id"))
		}
		role, err := parseRole(p.CallerRole)
		if err != nil {
			return nil, jobs.Permanent(fmt.Errorf("assistant.run: %w", err))
		}
		th, err := deps.Threads.GetThread(ctx, p.ThreadID)
		if err != nil {
			return nil, jobs.Permanent(fmt.Errorf("assistant.run: load thread: %w", err))
		}
		if th.RunID != p.RunID {
			return nil, jobs.Permanent(fmt.Errorf("assistant.run: thread %s owns run %s, not %s", p.ThreadID, th.RunID, p.RunID))
		}

		// The attempt's start marker (ADR-0099 decision 4): the last durable
		// message before this attempt. Answers appended after it were not in
		// the conversation this attempt reads.
		startSeq, err := deps.Store.MaxAgentMessageSeq(ctx, p.RunID)
		if err != nil {
			return nil, fmt.Errorf("assistant.run %s: start seq: %w", p.RunID, err)
		}

		reg, err := deps.RegistryFor(RunContext{ThreadID: p.ThreadID, RunID: p.RunID, UserID: p.UserID})
		if err != nil {
			return nil, jobs.Permanent(fmt.Errorf("assistant.run: registry: %w", err))
		}

		pc := PromptContext(role)
		pc.Budgets = Budget
		out, err := agent.Run(ctx, agent.Options{
			RunID:        p.RunID,
			Model:        model,
			Registry:     reg,
			CallerRole:   role,
			Store:        deps.AgentStore,
			Hub:          deps.Hub,
			TraceID:      p.RunID,
			UserRequest:  p.UserRequest,
			Budget:       Budget,
			Prompt:       pc,
			SystemPrompt: Render(pc, reg),
			ModelLabel:   modelLabel,
		})
		// Bookkeeping only (option/store failure): the run row already holds
		// the honest state, and the queue's retry is the fix.
		if err != nil {
			return resultJSON(p, out, false), fmt.Errorf("assistant.run %s: %w", p.RunID, err)
		}

		// The narrative card: the run's final answer (when it ended with one).
		if out.Answer != "" {
			emitCard(deps.Hub, p.RunID, EventMessage, map[string]any{
				"thread_id": p.ThreadID, "run_id": p.RunID, "text": out.Answer,
			})
		}

		// The UX-04 follow-up: an answer durable after this attempt's start
		// was not seen by it. Enqueue exactly one follow-up attempt; its
		// conversation rebuild picks the answer up (the run log is total).
		followUp := false
		unseen, err := deps.Questions.UnseenAnsweredQuestions(ctx, p.ThreadID, startSeq)
		if err != nil {
			return nil, fmt.Errorf("assistant.run %s: follow-up check: %w", p.RunID, err)
		}
		if len(unseen) > 0 && deps.Queue != nil {
			followPayload, err := json.Marshal(JobPayload{
				RunID: p.RunID, ThreadID: p.ThreadID,
				UserID: p.UserID, CallerRole: p.CallerRole,
			})
			if err != nil {
				return nil, fmt.Errorf("assistant.run %s: follow-up payload: %w", p.RunID, err)
			}
			if _, _, err := deps.Queue.Enqueue(ctx, store.EnqueueParams{
				Kind:    JobKind,
				Payload: followPayload,
				TraceID: p.RunID,
			}); err != nil {
				return nil, fmt.Errorf("assistant.run %s: enqueue follow-up: %w", p.RunID, err)
			}
			followUp = true
		}

		return resultJSON(p, out, followUp), nil
	}, nil
}

func resultJSON(p JobPayload, out agent.Outcome, followUp bool) json.RawMessage {
	raw, _ := json.Marshal(JobResult{
		RunID: p.RunID, ThreadID: p.ThreadID,
		State: string(out.State), StopReason: out.StopReason,
		Answer: out.Answer, Steps: out.Steps, ToolCalls: out.ToolCalls,
		FollowUp: followUp,
	})
	return raw
}

func emitCard(hub *events.Hub, traceID, name string, data map[string]any) {
	if hub == nil {
		return
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _ = hub.Publish(ctx, events.Envelope{Name: name, TraceID: traceID, Data: raw})
}

func parseRole(s string) (auth.Role, error) {
	if s == "" {
		return auth.RoleNone, fmt.Errorf("caller role is empty")
	}
	for r := auth.RoleViewer; r <= auth.RoleAdmin; r++ {
		if auth.RoleNames[r] == s {
			return r, nil
		}
	}
	return auth.RoleNone, fmt.Errorf("caller role %q is not a role name", s)
}
