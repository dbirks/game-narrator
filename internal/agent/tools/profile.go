package tools

// Profile tools: preview_profile, propose_artwork, publish_profile
// (docs/spec/03 "Tool registry"; "Human-in-the-loop boundaries").
//
// preview_profile is side-effect-free; propose_artwork is a labelled stub
// (candidates only, never fetched or published); publish_profile is the
// approval boundary itself — it refuses without a human-issued, single-use
// token and never mutates a published revision.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/dbirks/game-narrator/internal/agent"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/store"
)

// previewProfile renders the draft's state: sources with rights and
// coverage, message statistics, characters, conflicts, and the voice state.
// It reads nothing outside the profile and writes nothing.
func (d Deps) previewProfile(ctx context.Context, call agent.Call) (agent.Result, error) {
	m, err := parseArgs(call)
	if err != nil {
		return agent.Result{}, err
	}
	profileID, err := require(m, "profile_id")
	if err != nil {
		return agent.Result{}, err
	}
	content, err := d.Store.GetProfileContent(ctx, profileID)
	if err != nil {
		return agent.Result{}, fmt.Errorf("tools: preview_profile: %w", err)
	}
	p := content.Profile

	type sourceView struct {
		ID            string  `json:"id"`
		Type          string  `json:"type"`
		URI           string  `json:"uri"`
		Rights        string  `json:"rights"`
		Trust         float64 `json:"trust"`
		Parser        string  `json:"parser"`
		CoverageNotes string  `json:"coverage_notes"`
	}
	srcViews := make([]sourceView, 0, len(content.Sources))
	for _, s := range content.Sources {
		srcViews = append(srcViews, sourceView{
			ID: s.ID, Type: string(s.SourceType), URI: s.URI,
			Rights: string(s.Rights), Trust: s.Trust,
			Parser: s.Parser, CoverageNotes: s.CoverageNotes,
		})
	}

	msgCount := len(content.Messages)
	var labeled, nonempty int
	langCounts := map[string]int{}
	for _, msg := range content.Messages {
		if msg.SpeakerID != "" {
			labeled++
		}
		if strings.TrimSpace(msg.OriginalText) != "" {
			nonempty++
		}
		lang := msg.Language
		if lang == "" {
			lang = "(unspecified)"
		}
		langCounts[lang]++
	}

	// Conflicts across the whole revision (same source_message_id, different
	// text). The preview reports; only source_quality quarantines.
	bySideID := map[string]map[string]bool{}
	for _, msg := range content.Messages {
		if msg.SourceID == "" || msg.SourceMessageID == "" {
			continue
		}
		if bySideID[msg.SourceMessageID] == nil {
			bySideID[msg.SourceMessageID] = map[string]bool{}
		}
		bySideID[msg.SourceMessageID][textOf(msg)] = true
	}
	conflictIDs := 0
	for _, texts := range bySideID {
		if len(texts) > 1 {
			conflictIDs++
		}
	}

	charViews := make([]map[string]any, 0, len(content.Characters))
	for _, c := range content.Characters {
		charViews = append(charViews, map[string]any{
			"id": c.ID, "name": c.Name, "status": string(c.Status), "role": c.Role,
		})
	}

	out := map[string]any{
		"profile": map[string]any{
			"id":                 p.ID,
			"edition_id":         p.EditionID,
			"revision_num":       p.RevisionNum,
			"status":             string(p.Status),
			"row_version":        content.RowVersion,
			"preferred_voice_id": p.PreferredVoiceID,
		},
		"sources": srcViews,
		"messages": map[string]any{
			"count":                          msgCount,
			"nonempty_text_pct":              pct(msgCount, nonempty),
			"speaker_labeled_pct":            pct(msgCount, labeled),
			"languages":                      langCounts,
			"conflicting_source_message_ids": conflictIDs,
		},
		"characters": charViews,
		"edges":      len(content.Edges),
		"voices": map[string]any{
			"preferred_voice_id": p.PreferredVoiceID,
			"note":               "auditions are draft assets until a human approves one; this preview never changes that",
		},
	}
	if d.Artwork != nil {
		out["artwork"] = d.Artwork.view(profileID)
	}
	return okResult(out)
}

// ---------------------------------------------------------------------------
// propose_artwork — LABELLED STUB (ADR-0026)
// ---------------------------------------------------------------------------
//
// docs/spec/03's propose_artwork returns "candidate thumbnail URLs and
// licenses" and must "not auto-publish remote art". This build implements
// the minimum honest version: candidate URLs are recorded in an in-process
// ledger with license "unknown" and state "candidate_not_published".
// Nothing is fetched, stored in the asset store or published; the ledger
// survives the process only. The full artwork pipeline (fetch under rights
// policy, license verification, asset registration, published-profile
// wiring) is a later task — see ADR-0026 "What is stubbed and why".

// ArtworkCandidate is one recorded candidate.
type ArtworkCandidate struct {
	URL        string `json:"url"`
	License    string `json:"license"` // always "unknown" in this build
	State      string `json:"state"`   // always "candidate_not_published"
	RecordedAt string `json:"recorded_at"`
}

const (
	artworkLicenseUnknown = "unknown"
	artworkStateCandidate = "candidate_not_published"
	artworkStubNote       = "stub: candidate URLs recorded in-process only; no artwork is fetched, stored or published (ADR-0026)"
	maxArtworkCandidates  = 8
	maxArtworkURLLen      = 2000
)

// artworkLedger is the in-process store for propose_artwork candidates.
type artworkLedger struct {
	mu sync.Mutex
	by map[string][]ArtworkCandidate
}

func newArtworkLedger() *artworkLedger {
	return &artworkLedger{by: map[string][]ArtworkCandidate{}}
}

func (l *artworkLedger) set(profileID string, cands []ArtworkCandidate) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.by[profileID] = cands
}

func (l *artworkLedger) view(profileID string) map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	cands := append([]ArtworkCandidate{}, l.by[profileID]...)
	return map[string]any{
		"candidates": cands,
		"note":       artworkStubNote,
	}
}

func (d Deps) proposeArtwork(ctx context.Context, call agent.Call) (agent.Result, error) {
	m, err := parseArgs(call)
	if err != nil {
		return agent.Result{}, err
	}
	profileID, err := require(m, "profile_id")
	if err != nil {
		return agent.Result{}, err
	}
	profile, _, err := d.Store.GetProfile(ctx, profileID)
	if err != nil {
		return agent.Result{}, fmt.Errorf("tools: propose_artwork: %w", err)
	}
	if profile.IsImmutable() {
		return agent.Result{}, fmt.Errorf("profile %s is %s: artwork candidates live on a draft revision", profileID, profile.Status)
	}
	raw, ok := m["urls"].([]any)
	if !ok || len(raw) == 0 {
		return agent.Result{}, fmt.Errorf("argument %q is required (array of 1-%d URL strings)", "urls", maxArtworkCandidates)
	}
	if len(raw) > maxArtworkCandidates {
		return agent.Result{}, fmt.Errorf("argument %q: at most %d candidates per call", "urls", maxArtworkCandidates)
	}
	cands := make([]ArtworkCandidate, 0, len(raw))
	for _, u := range raw {
		s, ok := u.(string)
		if !ok {
			return agent.Result{}, fmt.Errorf("argument %q: entries must be strings", "urls")
		}
		s = strings.TrimSpace(s)
		if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
			return agent.Result{}, fmt.Errorf("argument %q: %q is not an http(s) URL (remote artwork is never fetched or imported)", "urls", s)
		}
		if len(s) > maxArtworkURLLen {
			return agent.Result{}, fmt.Errorf("argument %q: URL exceeds %d chars", "urls", maxArtworkURLLen)
		}
		cands = append(cands, ArtworkCandidate{
			URL: s, License: artworkLicenseUnknown, State: artworkStateCandidate,
			RecordedAt: time.Now().UTC().Format(time.RFC3339),
		})
	}
	d.Artwork.set(profileID, cands)
	return okResult(map[string]any{
		"profile_id": profileID,
		"candidates": cands,
		"note":       artworkStubNote,
	})
}

// ---------------------------------------------------------------------------
// publish_profile — the approval boundary
// ---------------------------------------------------------------------------

func (d Deps) publishProfile(ctx context.Context, call agent.Call) (agent.Result, error) {
	m, err := parseArgs(call)
	if err != nil {
		return agent.Result{}, err
	}
	profileID, err := require(m, "profile_id")
	if err != nil {
		return agent.Result{}, err
	}
	token, err := require(m, "approval_token")
	if err != nil {
		return agent.Result{}, err
	}
	if d.Approvals == nil {
		return agent.Result{}, fmt.Errorf("no approval store configured; publishing requires a human approval (docs/spec/03)")
	}

	// 1. The token gate: unknown, spent and expired tokens all fail here,
	// before the profile is even read. No tool path issues tokens, so a
	// valid token is by construction a human decision.
	approval, err := d.Approvals.Consume(ctx, token)
	if err != nil {
		return agent.Result{}, fmt.Errorf("publish refused: invalid approval token (%v); publishing requires a clear human approval (docs/spec/03)", err)
	}
	if approval.ProfileID != profileID {
		return agent.Result{}, fmt.Errorf("publish refused: approval token was issued for profile %s, not %s", approval.ProfileID, profileID)
	}

	// 2. State gate: publication goes through review, never straight from
	// draft (games.ProfileStatus transition table).
	profile, rowVersion, err := d.Store.GetProfile(ctx, profileID)
	if err != nil {
		return agent.Result{}, fmt.Errorf("tools: publish_profile: %w", err)
	}
	if profile.Status != games.StatusReview {
		return agent.Result{}, fmt.Errorf("publish refused: profile %s is %s; a human must move it to %s before publication (docs/spec/02)", profileID, profile.Status, games.StatusReview)
	}

	// 3. The store's publication gate runs its full validation (messages,
	// referential integrity, rights) inside the publish transaction.
	published, err := d.Store.Publish(ctx, profileID, rowVersion, "agent:publish_profile")
	if err != nil {
		var pe *store.PublishError
		if errors.As(err, &pe) {
			return agent.Result{}, fmt.Errorf("publish refused by validation: %s", strings.Join(pe.Problems, "; "))
		}
		return agent.Result{}, fmt.Errorf("tools: publish_profile: %w", err)
	}
	return okResult(map[string]any{
		"profile_id":      profileID,
		"status":          string(published.Status),
		"published_at":    published.PublishedAt,
		"manifest_digest": published.ManifestDigest,
		"row_version":     rowVersion + 1,
		"approval": map[string]any{
			"granted_by": approval.GrantedBy,
			"issued_at":  approval.CreatedAt.Format(time.RFC3339),
		},
	})
}

// ---------------------------------------------------------------------------
// Schemas
// ---------------------------------------------------------------------------

var (
	previewProfileSchema = json.RawMessage(`{"type":"object","properties":{
		"profile_id":{"type":"string"}
	},"required":["profile_id"]}`)

	proposeArtworkSchema = json.RawMessage(`{"type":"object","properties":{
		"profile_id":{"type":"string"},
		"urls":{"type":"array","items":{"type":"string","maxLength":2000},"maxItems":8}
	},"required":["profile_id","urls"]}`)

	publishProfileSchema = json.RawMessage(`{"type":"object","properties":{
		"profile_id":{"type":"string"},
		"approval_token":{"type":"string"}
	},"required":["profile_id","approval_token"]}`)
)
