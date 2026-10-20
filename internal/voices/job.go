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
type JobPayload struct {
	ProfileID string `json:"profile_id"`
	Count     int    `json:"count"`
}

// JobResult is the durable result the /jobs API and the UI poll for.
type JobResult struct {
	ProfileID     string   `json:"profile_id"`
	State         string   `json:"state"`
	AuditionCount int      `json:"audition_count"`
	AuditionIDs   []string `json:"audition_ids"`
}

// JobHandler returns the runner handler for JobKind. The fill itself is
// idempotent over crash-restart (CreateAuditionsOnDraft refuses a non-draft
// revision and never stacks a second set of auditions on a half-filled
// draft), so re-queuing after a crash is safe.
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
		progress(1.0, fmt.Sprintf("%d audition clips ready in %s", len(auditions), time.Since(start).Round(time.Millisecond)))
		return json.Marshal(JobResult{
			ProfileID: rev.ID, State: rev.State,
			AuditionCount: len(auditions), AuditionIDs: ids,
		})
	}
}

// JobHandlerOpts keeps the register call in main.go explicit.
func JobHandlerOpts() jobs.RegisterOpts {
	// Audition generation is one provider-bound unit; one at a time keeps the
	// gateway rate limit honest. Not resumable: the clips are regenerated,
	// not continued.
	return jobs.RegisterOpts{Concurrency: 1, Resumable: false, Timeout: 10 * time.Minute}
}
