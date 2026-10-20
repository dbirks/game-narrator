package api

// The GET /api/v1/onboarding/runs/{id} assembler (ADR-0028): the REST
// snapshot of a run. SSE (GET /api/v1/events, names=agent.step,tool.started,
// tool.completed,review.required) carries the live tail; this is what a page
// that reopens the run — or missed the stream — rebuilds from.
//
// Everything here is computed from durable rows (the run row, the tool-call
// log, the conversation log, the profile and its sources), so a snapshot is
// the same no matter which process serves it. One rule of the surface: the
// approval token is appliance-private. It travels inside a conversation
// message (the model needs it to call publish_profile), and this assembler
// redacts it out of every message it hands to the wire (ADR-0028).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/dbirks/game-narrator/internal/agent"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/store"
)

type onbToolCallView struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Status         string   `json:"status"`
	SourceIDs      []string `json:"source_ids"`
	Error          string   `json:"error,omitempty"`
	StartedAt      string   `json:"started_at"`
	FinishedAt     string   `json:"finished_at,omitempty"`
	DurationMs     int64    `json:"duration_ms,omitempty"`
	ReviewRequired bool     `json:"review_required,omitempty"`
}

type onbSourceView struct {
	ID            string  `json:"id"`
	Type          string  `json:"type"`
	URI           string  `json:"uri,omitempty"`
	Rights        string  `json:"rights"`
	Trust         float64 `json:"trust"`
	Parser        string  `json:"parser,omitempty"`
	CoverageNotes string  `json:"coverage_notes,omitempty"`
	Untrusted     bool    `json:"untrusted"` // true for web_page sources (docs/spec/14)
}

type onbMessageView struct {
	Seq       int64  `json:"seq"`
	Role      string `json:"role"`
	Text      string `json:"text"`
	CreatedAt string `json:"created_at"`
}

type onbReviewItemView struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"` // publish | tool
	Tool        string `json:"tool,omitempty"`
	ProfileID   string `json:"profile_id,omitempty"`
	Description string `json:"description"`
	State       string `json:"state"` // pending | resolved
	// Provenance is set for a propose_web_import item (ADR-0077): the exact
	// web source the curator is approving — its URL, the SHA-256 of the raw
	// body the approval pins, when it was retrieved and its rights position.
	Provenance *onbProvenanceView `json:"provenance,omitempty"`
}

// onbProvenanceView is the approved-import provenance block the review item
// renders above its Approve button: approving is the rights assertion, so
// the curator sees the bytes' identity before asserting rights over them.
type onbProvenanceView struct {
	URL         string `json:"url"`
	SHA256      string `json:"sha256"`
	RetrievedAt string `json:"retrieved_at,omitempty"`
	Rights      string `json:"rights"`
}

type onbDraftView struct {
	ProfileID      string `json:"profile_id"`
	Status         string `json:"status"`
	RevisionNum    int    `json:"revision_num"`
	RowVersion     int64  `json:"row_version"`
	MessageCount   int    `json:"message_count"`
	CharacterCount int    `json:"character_count"`
}

type onbStateResponse struct {
	RunID       string              `json:"run_id"`
	State       string              `json:"state"`
	StopReason  string              `json:"stop_reason,omitempty"`
	Model       string              `json:"model,omitempty"`
	ProfileID   string              `json:"profile_id,omitempty"`
	Plan        onbPlan             `json:"plan"`
	Steps       int                 `json:"steps"`
	CreatedAt   string              `json:"created_at"`
	UpdatedAt   string              `json:"updated_at"`
	Job         *jobDTO             `json:"job,omitempty"`
	ToolCalls   []onbToolCallView   `json:"tool_calls"`
	Sources     []onbSourceView     `json:"sources"`
	Quality     []map[string]any    `json:"quality_reports"`
	Draft       *onbDraftView       `json:"draft,omitempty"`
	ReviewItems []onbReviewItemView `json:"review_items"`
	Messages    []onbMessageView    `json:"messages"`
}

// approvalTokenRe redacts the raw approval token from displayed messages.
// The format is tools.mintToken's ("gnapp_" + 64 hex), so the redaction is
// exact rather than a prose guess.
var approvalTokenRe = regexp.MustCompile(`gnapp_[0-9a-fA-F]{64}`)

func redactApprovalTokens(s string) string {
	if s == "" {
		return s
	}
	return approvalTokenRe.ReplaceAllString(s, "gnapp_****")
}

// onbAssembleState builds the snapshot. Every read is ctx-bound; a store
// fault surfaces as the caller's 503, not a partial body.
func (s *Server) onbAssembleState(ctx context.Context, runID string) (onbStateResponse, error) {
	run, err := s.onb.AgentStore.GetAgentRun(ctx, runID)
	if err != nil {
		return onbStateResponse{}, err
	}
	out := onbStateResponse{
		RunID:       run.ID,
		State:       run.State,
		StopReason:  run.StopReason,
		Model:       run.Model,
		ProfileID:   run.ProfileID,
		Plan:        onbPlan{},
		CreatedAt:   fmtTime(run.CreatedAt),
		UpdatedAt:   fmtTime(run.UpdatedAt),
		ToolCalls:   []onbToolCallView{},
		Sources:     []onbSourceView{},
		Quality:     []map[string]any{},
		ReviewItems: []onbReviewItemView{},
		Messages:    []onbMessageView{},
	}
	if len(run.PlanJSON) > 0 {
		_ = json.Unmarshal(run.PlanJSON, &out.Plan)
	}

	calls, err := s.onb.AgentStore.ListAgentToolCalls(ctx, runID)
	if err != nil {
		return onbStateResponse{}, err
	}
	for _, c := range calls {
		view := onbToolCallView{
			ID: c.ID, Name: c.Name, Status: c.Status, Error: c.Error,
			SourceIDs: []string{},
			StartedAt: fmtTime(c.StartedAt),
		}
		var res agent.Result
		if len(c.ResultJSON) > 0 && json.Unmarshal(c.ResultJSON, &res) == nil {
			view.SourceIDs = res.SourceIDs
			view.ReviewRequired = res.ReviewRequired
			// The source-quality report is the run's evidence for coverage
			// (docs/spec/03 "Dialogue source quality"); surface the
			// structured result so the UI renders it without re-parsing.
			if c.Name == "source_quality" && res.Status == agent.StatusSucceeded && len(res.Results) > 0 {
				var report map[string]any
				if json.Unmarshal(res.Results, &report) == nil {
					out.Quality = append(out.Quality, report)
				}
			}
		}
		if c.FinishedAt != nil {
			view.FinishedAt = fmtTime(*c.FinishedAt)
			if d := c.FinishedAt.Sub(c.StartedAt); d >= 0 {
				view.DurationMs = d.Milliseconds()
			}
		}
		out.ToolCalls = append(out.ToolCalls, view)
	}

	// Conversation: user and assistant text only (tool results are the
	// tool timeline's job), with the approval token redacted out.
	msgs, err := s.onb.AgentStore.ListAgentMessages(ctx, runID)
	if err != nil {
		return onbStateResponse{}, err
	}
	steps := 0
	for _, m := range msgs {
		if m.Role == "assistant" {
			steps++
		}
		if m.Role != "user" && m.Role != "assistant" {
			continue
		}
		var pm struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}
		if json.Unmarshal(m.ContentJSON, &pm) != nil {
			continue
		}
		// Assistant turns that only carried tool calls have empty text; the
		// tool timeline is their view, not an empty bubble in the chat.
		if pm.Role == "assistant" && strings.TrimSpace(pm.Content) == "" {
			continue
		}
		out.Messages = append(out.Messages, onbMessageView{
			Seq: m.Seq, Role: m.Role,
			Text:      redactApprovalTokens(pm.Content),
			CreatedAt: fmtTime(m.CreatedAt),
		})
	}
	out.Steps = steps

	// Tool-level review items: the tool calls that asked the run to stop at
	// a human boundary. Pending while the run is in review; resolved once
	// the run moved on (an approval, a message or a new attempt left it).
	for _, c := range calls {
		var res agent.Result
		if len(c.ResultJSON) == 0 || json.Unmarshal(c.ResultJSON, &res) != nil || !res.ReviewRequired {
			continue
		}
		state := "resolved"
		if run.State == "review" {
			state = "pending"
		}
		out.ReviewItems = append(out.ReviewItems, onbReviewItemView{
			ID: c.ID, Kind: "tool", Tool: c.Name,
			ProfileID:   run.ProfileID,
			Description: c.Name + " asked the run to stop for human review; approve it or reply with a message to steer the run.",
			State:       state,
		})
	}

	// The approved web-source import names its provenance on the item itself
	// (ADR-0077): the proposal's structured result carries the URL and the
	// raw-body SHA-256 the approval pins, and the description says what
	// approving commits the curator to.
	for i := range out.ReviewItems {
		item := &out.ReviewItems[i]
		if item.Kind != "tool" || item.Tool != "propose_web_import" {
			continue
		}
		var res agent.Result
		for _, c := range calls {
			if c.ID != item.ID || json.Unmarshal(c.ResultJSON, &res) != nil {
				continue
			}
			var payload struct {
				Provenance *onbProvenanceView `json:"provenance"`
			}
			if json.Unmarshal(res.Results, &payload) == nil && payload.Provenance != nil && payload.Provenance.URL != "" {
				item.Provenance = payload.Provenance
				if len(item.Provenance.SHA256) > 16 {
					item.Description = fmt.Sprintf(
						"Import the web source %s (raw-body sha256 %s\u2026, retrieved %s, rights currently %s) as your approved text source. Approving asserts your rights to this text and releases its bytes to the import exactly once; rejecting leaves the run to choose another path.",
						item.Provenance.URL, item.Provenance.SHA256[:16], item.Provenance.RetrievedAt, item.Provenance.Rights)
				} else {
					item.Description = fmt.Sprintf(
						"Import the web source %s (rights currently %s) as your approved text source. Approving asserts your rights to this text and releases its bytes to the import exactly once; rejecting leaves the run to choose another path.",
						item.Provenance.URL, item.Provenance.Rights)
				}
				break
			}
		}
	}

	// The draft preview and the publish-boundary review item: the item
	// exists while the profile is an unpublished draft/review revision and
	// the run can still act (a failed run's approval is a new run's job).
	if run.ProfileID != "" {
		profile, rowVersion, err := s.store.GetProfile(ctx, run.ProfileID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return onbStateResponse{}, err
		}
		if err == nil {
			content, cerr := s.store.GetProfileContent(ctx, run.ProfileID)
			if cerr == nil {
				out.Draft = &onbDraftView{
					ProfileID:      profile.ID,
					Status:         string(profile.Status),
					RevisionNum:    profile.RevisionNum,
					RowVersion:     rowVersion,
					MessageCount:   len(content.Messages),
					CharacterCount: len(content.Characters),
				}
			} else {
				out.Draft = &onbDraftView{
					ProfileID:   profile.ID,
					Status:      string(profile.Status),
					RevisionNum: profile.RevisionNum,
					RowVersion:  rowVersion,
				}
			}
		}
		switch profile.Status {
		case games.StatusDraft, games.StatusReview:
			if run.State != "failed" {
				out.ReviewItems = append(out.ReviewItems, onbReviewItemView{
					ID:          onbReviewPublishID,
					Kind:        "publish",
					ProfileID:   run.ProfileID,
					Description: "The draft profile awaits human approval to publish. Approving issues the single-use approval token and resumes the run; the agent publishes only through that approval (docs/spec/03).",
					State:       "pending",
				})
			}
		}

		sources, err := s.store.ListSources(ctx, run.ProfileID)
		if err != nil {
			return onbStateResponse{}, err
		}
		for _, src := range sources {
			out.Sources = append(out.Sources, onbSourceView{
				ID: src.ID, Type: string(src.SourceType), URI: src.URI,
				Rights: string(src.Rights), Trust: src.Trust,
				Parser: src.Parser, CoverageNotes: src.CoverageNotes,
				Untrusted: src.SourceType == games.SourceWebPage,
			})
		}
	}

	// The run's latest job row, in whatever state: the queue's answer to
	// "is an attempt in flight, and what did the last one do?".
	if job := s.onbLatestJob(ctx, runID); job != nil {
		dto := toJobDTO(job)
		out.Job = &dto
	}
	return out, nil
}

// onbLatestJob scans the (small) agent.run queue for the run's newest job.
func (s *Server) onbLatestJob(ctx context.Context, runID string) *store.Job {
	var latest *store.Job
	cursor := ""
	for {
		page, next, err := s.jobStore.ListJobs(ctx, store.JobFilter{Kind: agent.JobKind}, cursor, 0)
		if err != nil {
			return latest
		}
		for i := range page {
			if onbJobRunID(page[i].Payload) != runID {
				continue
			}
			if latest == nil || !page[i].CreatedAt.Before(latest.CreatedAt) {
				latest = &page[i]
			}
		}
		if next == "" {
			return latest
		}
		cursor = next
	}
}

func isNotFound(err error) bool { return errors.Is(err, store.ErrNotFound) }
