package cast

// The --fix leg (ADR-0171): for a MISMATCH, redesign the character's voice
// through the existing voice-design path (voices.Service.CreateAuditions —
// the same composition the auto-cast and manual audition flows use) with a
// brief that states the expected gender/age explicitly, measure the new
// take's median F0, and retry up to FixMaxRetries more times while it still
// reads the wrong class. The winning take stays PENDING approval (its
// revision sits in the auditioning state, voice selection is reviewable —
// AGENTS.md rule 6) unless the caller passes approve, which approves it with
// the profile-default consent attributed to the profile.
//
// Idempotence: the fix brief carries a stable marker sentence. Re-running
// the fix for a character whose current pending revision already carries
// the marker does not open another draft — it re-measures the pending take
// and only designs more when that take is wrong.

import (
	"context"
	"fmt"
	"strings"

	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/store"
	"github.com/dbirks/game-narrator/internal/voices"
)

// FixMaxRetries is how many extra design attempts are allowed after the
// first take still reads the wrong voice class.
const FixMaxRetries = 2

// fixMarker prefixes every fix brief: it is the idempotence key and the
// audit's way of recognising its own drafts on re-runs.
const fixMarker = "cast-audit-fix"

// FixBrief returns the design brief for one fix: the character's existing
// brief (preserved — it carries the creative content) plus the marker and
// an explicit gender/age statement.
func FixBrief(existing, expected string) string {
	vars := ""
	switch expected {
	case ClassMale:
		vars = "This speaker is a LOW-VOICED ADULT MAN (an adult male: deep, low fundamental pitch, around or below the bass bar)."
	case ClassFemale:
		vars = "This speaker is an ADULT WOMAN (an adult female: mid fundamental pitch, clearly female)."
	case ClassChild:
		vars = "This speaker is a YOUNG CHILD (a small child: high fundamental pitch, young-sounding)."
	default:
		vars = "This speaker's gender cannot be established."
	}
	brief := strings.TrimSpace(existing)
	if brief == "" {
		brief = vars
	} else {
		brief = brief + " " + vars
	}
	return fixMarker + ": " + brief
}

// isFixBrief reports whether a stored design instruction carries the marker.
func isFixBrief(instruction string) bool {
	return strings.Contains(instruction, fixMarker)
}

// FixResult is one character's fix report.
type FixResult struct {
	Character  string  `json:"character"`
	BeforeF0Hz float64 `json:"before_f0_hz"`
	NewVoiceID string  `json:"new_voice_id"`
	NewF0Hz    float64 `json:"new_f0_hz"`
	Attempts   int     `json:"attempts"`
	Verdict    string  `json:"verdict"` // the verdict of the final take
	Approved   bool    `json:"approved"`
}

// Fixer redesigns mismatched voices. Svc is the real voice workflow (its
// Design client must be wired to a live provider); Aud measures the takes.
type Fixer struct {
	Aud *Auditor
	Svc *voices.Service
}

// pendingFixRevision finds the character's current non-approved revision
// carrying the fix marker (the idempotent re-measure path).
func (f *Fixer) pendingFixRevision(ctx context.Context, character string) (store.VoiceProfile, error) {
	revs, err := f.Aud.Store.ListVoiceProfiles(ctx, character)
	if err != nil {
		return store.VoiceProfile{}, err
	}
	for _, p := range revs { // revision, descending
		if (p.State == store.VoiceStateDraft || p.State == store.VoiceStateAuditioning) &&
			isFixBrief(p.DesignInstruction) {
			return p, nil
		}
	}
	return store.VoiceProfile{}, nil
}

// FixCharacter redesigns one character's voice until it reads the expected
// class. It returns the outcome of the final take; the take stays pending
// unless approve is true.
func (f *Fixer) FixCharacter(ctx context.Context, profileID, character string, expected string, beforeF0 float64, approve bool) (FixResult, error) {
	if expected == ClassUnknown {
		return FixResult{}, fmt.Errorf("cast fix: no expected class to design towards for %s", character)
	}
	// Idempotent re-run: a pending revision from a previous fix attempt is
	// re-measured first; only a wrong take triggers more design.
	if rev, err := f.pendingFixRevision(ctx, character); err != nil {
		return FixResult{}, err
	} else if rev.ID != "" {
		auds, err := f.Aud.Store.ListAuditions(ctx, rev.ID)
		if err != nil || len(auds) == 0 {
			return FixResult{}, fmt.Errorf("cast fix: no audition on pending revision %s: %v", rev.ID, err)
		}
		f0, err := f.measureAudition(ctx, auds[0].AssetSHA256)
		if err != nil {
			return FixResult{}, err
		}
		res := FixResult{Character: character, BeforeF0Hz: beforeF0,
			NewVoiceID: rev.ID, NewF0Hz: f0, Attempts: 1,
			Verdict: f.verdictOf(f.ClassOfF0Now(f0), expected)}
		if res.Verdict == VerdictOK {
			if approve {
				if err := f.approve(ctx, profileID, rev.ID, auds[0].ID); err != nil {
					return res, err
				}
				res.Approved = true
			}
			// The pending take already reads right: report it, and do not open
			// another draft (idempotence across re-runs).
			return res, nil
		}
		// The pending take is wrong: design more below.
	}

	var best FixResult
	for attempt := 0; attempt <= FixMaxRetries; attempt++ {
		brief := FixBrief("", expected)
		if v, err := f.Aud.AssignedVoiceOf(ctx, character); err == nil && v.VoiceID != "" {
			brief = FixBrief(v.Brief, expected)
		}
		rev, auds, err := f.Svc.CreateAuditions(ctx, character, brief, 1)
		if err != nil {
			return best, fmt.Errorf("cast fix: design take %d for %s: %w", attempt+1, character, err)
		}
		if len(auds) == 0 {
			return best, fmt.Errorf("cast fix: design returned no audition for %s", character)
		}
		f0, err := f.measureAudition(ctx, auds[0].AssetSHA256)
		if err != nil {
			return best, err
		}
		cls := f.ClassOfF0Now(f0)
		res := FixResult{Character: character, BeforeF0Hz: beforeF0,
			NewVoiceID: rev.ID, NewF0Hz: f0, Attempts: attempt + 1,
			Verdict: f.verdictOf(cls, expected)}
		if res.Verdict == VerdictOK {
			if approve {
				if err := f.approve(ctx, profileID, rev.ID, auds[0].ID); err != nil {
					return res, err
				}
				res.Approved = true
			}
			return res, nil
		}
		best = res
	}
	return best, nil // still the wrong class after the retries: pending, reported
}

func (f *Fixer) measureAudition(ctx context.Context, sha string) (float64, error) {
	wav, err := f.Aud.referenceBytes(ctx, sha)
	if err != nil {
		return 0, err
	}
	p, err := speech.MeasurePitch(wav)
	if err != nil {
		return 0, fmt.Errorf("cast fix: measure take: %w", err)
	}
	return p.F0, nil
}

func (f *Fixer) approve(ctx context.Context, profileID, revisionID, auditionID string) error {
	_, err := f.Svc.Approve(ctx, revisionID, auditionID,
		voices.AutoCastConsent(profileID), "cast-audit")
	return err
}

func (f *Fixer) verdictOf(measured, expected string) string {
	if expected == ClassUnknown {
		return VerdictUnknown
	}
	if measured == expected {
		return VerdictOK
	}
	if measured == ClassUnknown {
		return VerdictUnknown
	}
	return VerdictMismatch
}

// ClassOfF0Now aliases ClassOfF0 on the fixer so the fix leg reads through
// one receiver (keeps test seams in one place).
func (f *Fixer) ClassOfF0Now(f0 float64) string { return ClassOfF0(f0) }
