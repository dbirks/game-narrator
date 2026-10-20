package voices

// The StageObserver seam (ADR-0108): the voices service tells the setup board
// the two moments a take becomes real — the auditions are recorded (a TTS
// take was requested and generated on the queue-backed path) and the
// approval freezes one take as the character's current voice. The board's own
// derivation is tested in internal/setupboard; here the contract is that the
// service calls the observer, with the revision it just moved, on exactly
// those moments — and that a missing observer leaves the workflow intact.

import (
	"context"
	"testing"

	"github.com/dbirks/game-narrator/internal/store"
)

// recObserver is a StageObserver that records the revision ids it is told.
type recObserver struct {
	auditions []string
	approved  []string
}

func (r *recObserver) AuditionsReady(_ context.Context, revisionID string) {
	r.auditions = append(r.auditions, revisionID)
}

func (r *recObserver) VoiceApproved(_ context.Context, revisionID string) {
	r.approved = append(r.approved, revisionID)
}

// TestServiceCallsTheStageObserver proves the service fires the observer at
// the take and the approval, with the revision id it just moved.
func TestServiceCallsTheStageObserver(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	rec := &recObserver{}
	svc.StageObserver = rec

	rev, auditions, err := svc.CreateAuditions(ctx(t), "Marla", "a synthetic brief", 2)
	if err != nil {
		t.Fatalf("CreateAuditions: %v", err)
	}
	if len(rec.auditions) != 1 || rec.auditions[0] != rev.ID {
		t.Fatalf("auditions observed = %v, want exactly one call with %s", rec.auditions, rev.ID)
	}
	if len(rec.approved) != 0 {
		t.Fatalf("approved observed = %v, want none before the approval", rec.approved)
	}

	approved, err := svc.Approve(ctx(t), rev.ID, auditions[0].ID, "synthetic provenance (unit test)", "curator-1")
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if approved.State != store.VoiceStateApproved {
		t.Fatalf("approved state = %s, want approved", approved.State)
	}
	if len(rec.approved) != 1 || rec.approved[0] != rev.ID {
		t.Fatalf("approval observed = %v, want exactly one call with %s", rec.approved, rev.ID)
	}

	// No observer: the workflow runs untouched (a board-less install).
	svc2, _, _, _ := newTestService(t)
	if _, _, err := svc2.CreateAuditions(ctx(t), "Corvin", "a synthetic brief", 2); err != nil {
		t.Fatalf("CreateAuditions without observer: %v", err)
	}
}
