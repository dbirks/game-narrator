package store

// The superseded-run facts read (ADR-0104 amendment): one point read that
// tells the status bar whether a setup run's profile revision is still the
// edition's live work. The fixture walks the real publication lifecycle —
// draft, published, superseded by a second publication, soft-deleted — so
// each clause of the bar's staleness filter is pinned against rows the
// repositories themselves wrote, not hand-edited shapes.

import (
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/games"
)

func TestAgentRunProfileFactsFollowsThePublicationLifecycle(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx

	// A draft revision nobody has published: nothing retired, nothing
	// current. The zero-value facts are the "keep the run" answer.
	facts, err := f.s.AgentRunProfileFacts(ctx, f.profile.ID)
	if err != nil {
		t.Fatalf("AgentRunProfileFacts on the draft: %v", err)
	}
	if facts.Archived || facts.SoftDeleted || facts.IsCurrentPublished {
		t.Errorf("draft facts = %+v, want the zero value (a live draft is not superseded)", facts)
	}
	if !facts.PublishedAt.IsZero() || !facts.CurrentPublishedAt.IsZero() {
		t.Errorf("draft stamps = (%v, %v), want both zero (nothing published yet)",
			facts.PublishedAt, facts.CurrentPublishedAt)
	}

	// Publish revision one. The gate needs a narration: the fixture's
	// bundle is the publishable minimum (a source with publishable rights,
	// a line that resolves inside the revision).
	bundle := f.bundle(2)
	bundle.put(f)
	if err := f.s.SetProfileStatus(ctx, f.profile.ID, games.StatusReview, f.version()); err != nil {
		t.Fatalf("review: %v", err)
	}
	first, err := f.s.Publish(ctx, f.profile.ID, f.version(), "curator@local")
	if err != nil {
		t.Fatalf("first Publish: %v", err)
	}

	// The edition now serves revision one: the pointer names it, and the
	// revision's own stamp is the edition's current stamp.
	facts, err = f.s.AgentRunProfileFacts(ctx, first.ID)
	if err != nil {
		t.Fatalf("AgentRunProfileFacts on the published revision: %v", err)
	}
	if !facts.IsCurrentPublished || facts.Archived || facts.SoftDeleted {
		t.Errorf("published facts = %+v, want current-published and not retired", facts)
	}
	if facts.PublishedAt.IsZero() || facts.CurrentPublishedAt != facts.PublishedAt {
		t.Errorf("stamps = (own %v, current %v), want the current publication's stamp twice",
			facts.PublishedAt, facts.CurrentPublishedAt)
	}

	// A second publication supersedes the first: the predecessor is
	// archived in the same commit the pointer moves, and the facts read
	// must agree with that — archived and no longer the pointer's target.
	second, err := f.s.NewRevision(ctx, first.ID)
	if err != nil {
		t.Fatalf("NewRevision: %v", err)
	}
	if err := f.s.SetProfileStatus(ctx, second.ID, games.StatusReview, mustVersion(t, f.s, ctx, second.ID)); err != nil {
		t.Fatalf("snapshot to review: %v", err)
	}
	published2, err := f.s.Publish(ctx, second.ID, mustVersion(t, f.s, ctx, second.ID), "curator@local")
	if err != nil {
		t.Fatalf("second Publish: %v", err)
	}

	facts, err = f.s.AgentRunProfileFacts(ctx, first.ID)
	if err != nil {
		t.Fatalf("AgentRunProfileFacts on the superseded revision: %v", err)
	}
	if !facts.Archived || facts.IsCurrentPublished {
		t.Errorf("superseded facts = %+v, want archived and not the current pointer", facts)
	}
	if facts.PublishedAt.IsZero() {
		t.Errorf("the superseded revision keeps its own publication stamp, got zero")
	}
	secondStamp, err := time.Parse(time.RFC3339, published2.PublishedAt)
	if err != nil {
		t.Fatalf("second publication stamp %q is not RFC 3339: %v", published2.PublishedAt, err)
	}
	if facts.CurrentPublishedAt != secondStamp {
		t.Errorf("current stamp = %v, want the second publication's stamp", facts.CurrentPublishedAt)
	}

	// The successor's facts: current, stamped with its own publication.
	facts, err = f.s.AgentRunProfileFacts(ctx, published2.ID)
	if err != nil {
		t.Fatalf("AgentRunProfileFacts on the successor: %v", err)
	}
	if facts.Archived || !facts.IsCurrentPublished || facts.CurrentPublishedAt.IsZero() {
		t.Errorf("successor facts = %+v, want current-published with a stamp", facts)
	}

	// A hand archive of the live revision clears the edition pointer in
	// the same write: the facts read reports archived with no current
	// publication at all (the zero stamp, not an error).
	if err := f.s.SetProfileStatus(ctx, published2.ID, games.StatusArchived, mustVersion(t, f.s, ctx, published2.ID)); err != nil {
		t.Fatalf("hand archive: %v", err)
	}
	facts, err = f.s.AgentRunProfileFacts(ctx, published2.ID)
	if err != nil {
		t.Fatalf("AgentRunProfileFacts after the hand archive: %v", err)
	}
	if !facts.Archived || facts.IsCurrentPublished || !facts.CurrentPublishedAt.IsZero() {
		t.Errorf("hand-archived facts = %+v, want archived with no current publication", facts)
	}

	// The ADR-0107 retention mark: only a revision that never shipped may
	// be hidden this way (the audit trail outlives a shipped revision), so
	// the mark lands on a fresh draft — the kind of row an agent run can
	// still name while the library hides it. The row survives on disk;
	// the facts read names the mark.
	draft, err := f.s.CreateDraftProfile(ctx, f.edition.ID)
	if err != nil {
		t.Fatalf("CreateDraftProfile: %v", err)
	}
	if _, err := f.s.SoftDeleteProfile(ctx, draft.ID); err != nil {
		t.Fatalf("SoftDeleteProfile: %v", err)
	}
	facts, err = f.s.AgentRunProfileFacts(ctx, draft.ID)
	if err != nil {
		t.Fatalf("AgentRunProfileFacts on the soft-deleted revision: %v", err)
	}
	if !facts.SoftDeleted || facts.Archived || facts.IsCurrentPublished {
		t.Errorf("soft-deleted facts = %+v, want the retention mark, not supersession", facts)
	}

	// A revision the table never held is an empty answer, not an error
	// (the missing-row rule this file follows): the bar keeps the run.
	facts, err = f.s.AgentRunProfileFacts(ctx, games.NewID())
	if err != nil {
		t.Fatalf("AgentRunProfileFacts on a missing id: %v", err)
	}
	if facts != (AgentRunProfileFacts{}) {
		t.Errorf("missing id facts = %+v, want the zero value", facts)
	}
}
