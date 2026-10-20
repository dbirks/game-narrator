package voices

// The durable-job side of the voice workflow (docs/spec/08 "APIs": voice
// setup is a queue-backed job; docs/spec/04 "jobs: voice setup (design,
// audition, approve, register)"). Audition generation is the slow part —
// n VoiceDesign calls against the provider — so it runs as a job, not
// inline in the HTTP request.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dbirks/game-narrator/internal/jobs"
)

// JobKind is the durable queue kind one audition-generation run executes as.
const JobKind = "voice.auditions"

// JobPayload names the draft revision to fill and the requested audition
// count. The draft row exists before the job is enqueued (the API created it
// synchronously), so a queued job is always addressable and cancellable by
// profile id. Count is the API's `count` (1..len(SyntheticAuditionLines);
// the OpenAPI contract: "How many synthetic lines to audition"): a
// crash-restarted job replays the payload and must reproduce the same fill.
// CastProfileID is the catalogue profile the cast was created for (ADR-0109,
// ADR-0109 auto-cast): empty for a manual run, set when the API's
// profile_id was given, and re-read by the job AFTER the fill to decide
// whether the take auto-approves. It is the catalogue profile (the profiles
// table), not the voice revision ProfileID names.
type JobPayload struct {
	ProfileID     string `json:"profile_id"`
	Count         int    `json:"count"`
	CastProfileID string `json:"cast_profile_id,omitempty"`
}

// JobResult is the durable result the /jobs API and the UI poll for.
type JobResult struct {
	ProfileID     string   `json:"profile_id"`
	State         string   `json:"state"`
	AuditionCount int      `json:"audition_count"`
	AuditionIDs   []string `json:"audition_ids"`
	// Auto-cast outcome (ADR-0109): present only when the payload carried a
	// cast profile. AutoApproved is true when the take auto-approved with the
	// profile consent default; AutoApprovalNote carries the reason it did not
	// (the dial was off at job end, the QA flagged every take, ...) so the UI
	// shows the real state instead of "pending" with no explanation.
	CastProfileID    string `json:"cast_profile_id,omitempty"`
	AutoApproved     bool   `json:"auto_approved,omitempty"`
	AutoApprovalNote string `json:"auto_approval_note,omitempty"`
}

// JobHandler returns the runner handler for JobKind. The fill itself is
// idempotent over crash-restart (CreateAuditionsOnDraft refuses a non-draft
// revision and never stacks a second set of auditions on a half-filled
// draft), so re-queuing after a crash is safe. The auto-cast step after the
// fill (ADR-0109) is idempotent the same way: it re-reads the dial and
// refuses an already-approved revision.
func JobHandler(svc *Service) jobs.Handler {
	return func(ctx context.Context, job jobs.Job, progress jobs.ProgressFn) (json.RawMessage, error) {
		var p JobPayload
		if err := json.Unmarshal(job.Payload, &p); err != nil || p.ProfileID == "" {
			return nil, fmt.Errorf("voice.auditions: unparseable payload: %w", err)
		}
		progress(0.1, "generating audition clips")
		start := time.Now()
		n := p.Count
		if n < 1 || n > len(SyntheticAuditionLines) {
			n = len(SyntheticAuditionLines) // a malformed or legacy payload fills the default set
		}
		auditions, err := svc.CreateAuditionsOnDraft(ctx, p.ProfileID, n)
		if err != nil {
			return nil, fmt.Errorf("voice.auditions: %w", err)
		}
		ids := make([]string, 0, len(auditions))
		for _, a := range auditions {
			ids = append(ids, a.ID)
		}
		rev, err := svc.Store.GetVoiceRevision(ctx, p.ProfileID)
		if err != nil {
			return nil, fmt.Errorf("voice.auditions: load revision after fill: %w", err)
		}
		result := JobResult{ProfileID: rev.ID, State: rev.State, AuditionCount: len(auditions), AuditionIDs: ids}
		// The auto-cast step (ADR-0109): only a run the API tied to a cast
		// profile gets one, and the dial is re-read here — the enqueue-time
		// read only chose the count — so a dial turned off after the run was
		// queued leaves the take pending review, the documented "toggled
		// after the run" outcome.
		if p.CastProfileID != "" {
			progress(0.9, "checking the auto-cast dial")
			result.CastProfileID = p.CastProfileID
			approved, note, aerr := svc.AutoApproveCast(ctx, p.CastProfileID, p.ProfileID)
			if aerr != nil {
				return nil, fmt.Errorf("voice.auditions: auto-cast: %w", aerr)
			}
			result.AutoApproved = approved
			result.AutoApprovalNote = note
			if approved {
				rev, err = svc.Store.GetVoiceRevision(ctx, p.ProfileID)
				if err != nil {
					return nil, fmt.Errorf("voice.auditions: load revision after auto-approval: %w", err)
				}
				result.State = rev.State
			}
		}
		progress(1.0, fmt.Sprintf("%d audition clips ready in %s", len(auditions), time.Since(start).Round(time.Millisecond)))
		return json.Marshal(result)
	}
}

// JobHandlerOpts keeps the register call in main.go explicit.
func JobHandlerOpts() jobs.RegisterOpts {
	// Audition generation is one provider-bound unit; one at a time keeps the
	// gateway rate limit honest. Not resumable: the clips are regenerated,
	// not continued.
	return jobs.RegisterOpts{Concurrency: 1, Resumable: false, Timeout: 10 * time.Minute}
}
